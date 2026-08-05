package execpolicy

import "strings"

// Rule is one entry in an execpolicy rule table. A command matches a Rule
// when normalizeProgram(Rule.Program) == Segment.Program AND Segment.Args
// begins with Rule.Prefix (an exact, case-sensitive prefix match).
//
// Decision semantics:
//   - "allow"  — the segment is permitted.
//   - "prompt" — the segment is conditionally permitted; surface to the
//     interactive approval layer (Promptable).
//   - "deny"   — DENY *only* when one of DenyFlags is present as a stand-alone
//     argument or as `flag=…` form. A "deny" rule without matching DenyFlags
//     is inert: it does NOT block, so a later "allow"/"prompt" rule can admit
//     the ordinary form. This is the fix for the original "no-real-e2e"
//     regression where `deny{test, deny_flags=[-tags=e2e_real]}` was wrongly
//     blocking all `go test`.
//
// Unknown Decision values are hard_deny at evaluation time so a typo in
// config.yaml cannot silently widen policy.
type Rule struct {
	ID            string   `yaml:"id" json:"id"`
	Program       string   `yaml:"program" json:"program"`
	Prefix        []string `yaml:"prefix" json:"prefix"`
	Decision      string   `yaml:"decision" json:"decision"`
	DenyFlags     []string `yaml:"deny_flags" json:"deny_flags"`
	Justification string   `yaml:"justification" json:"justification"`
}

// Result is the policy outcome for a parsed Command. RuleID/Justification are
// populated even for hard_deny so the TUI / WS layer can surface the exact
// rule that fired (e.g. "no-real-e2e — real E2E requires explicit operator
// approval") instead of a generic denial.
type Result struct {
	Verdict       string
	RuleID        string
	Justification string
	MatchedPrefix []string
	Reason        string
}

// Evaluate applies rules to cmd and returns a Result. The resolution order
// per segment is:
//  1. A "deny" rule with DenyFlags that matches a flag in Args → hard_deny
//     (returns immediately; deny is fail-closed).
//  2. A "deny" rule with DenyFlags that does NOT match → inert, continues.
//  3. "allow"/"prompt" rules → candidate; "prompt" wins over "allow" so a
//     more cautious rule cannot be hidden by an earlier permissive one.
//  4. No matching rule at all → hard_deny with RuleID="unmatched-segment".
//
// Across segments, the most cautious verdict wins (prompt > allow). If any
// segment hard_denies, that's the overall result.
//
// Control tokens (&&/||) are parsed by the lexer/parser but Evaluate refuses
// to execute them: the Result is hard_deny("control-token"). This is the
// explanation layer for the guard's existing metacharacter HardDeny.
func Evaluate(cmd Command, rules []Rule) Result {
	if len(cmd.Segments) == 0 {
		return hard("empty-command", "no executable segment", "")
	}
	if cmd.Control == AndIf || cmd.Control == OrIf {
		return hard("control-token", "&&/|| are parsed but not executable in A1", "")
	}
	var overall Result
	for _, seg := range cmd.Segments {
		matched := false
		var best Result
		for _, rule := range rules {
			if normalizeProgram(rule.Program) != seg.Program || !hasPrefix(seg.Args, rule.Prefix) {
				continue
			}
			decision := strings.ToLower(rule.Decision)
			if decision == "deny" {
				// Critical semantic: a deny rule with DenyFlags is conditional.
				// If no flag matches, CONTINUE so a later allow rule can admit
				// ordinary `go test` while `-tags=e2e_real` is denied.
				if !containsAny(seg.Args, rule.DenyFlags) {
					continue
				}
				return hard(rule.ID, "deny flag matched", rule.Justification)
			}
			matched = true
			candidate := Result{
				Verdict:       decision,
				RuleID:        rule.ID,
				Justification: rule.Justification,
				MatchedPrefix: append([]string{seg.Program}, rule.Prefix...),
				Reason:        rule.Justification,
			}
			switch decision {
			case "allow", "prompt":
			default:
				return hard(rule.ID, "unknown execpolicy verdict", rule.Justification)
			}
			if best.RuleID == "" || (candidate.Verdict == "prompt" && best.Verdict == "allow") {
				best = candidate
			}
		}
		if !matched {
			return hard("unmatched-segment", "an executable segment has no matching allow/prompt rule", "")
		}
		if overall.RuleID == "" || (best.Verdict == "prompt" && overall.Verdict == "allow") {
			overall = best
		}
	}
	return overall
}

// hard is the single constructor for hard_deny Result values. Centralizing
// it keeps the Verdict string consistent so callers compare against a single
// literal ("hard_deny") rather than re-spelling it.
func hard(ruleID, reason, justification string) Result {
	return Result{Verdict: "hard_deny", RuleID: ruleID, Reason: reason, Justification: justification}
}

// hasPrefix reports whether args begins with the exact, case-sensitive prefix.
// A nil/empty prefix matches any args list.
func hasPrefix(args, prefix []string) bool {
	if len(prefix) > len(args) {
		return false
	}
	for i := range prefix {
		if args[i] != prefix[i] {
			return false
		}
	}
	return true
}

// containsAny reports whether args carry any of flags, in any spelling a
// shell user would reach for.
//
// It used to compare the raw token against flag and flag+"=", which meant a
// deny rule listing `-tags=e2e_real` caught exactly that string and nothing
// else. `-tags e2e_real`, `--tags=e2e_real` and `-tags=e2e_real,foo` all
// walked past it into whatever permissive rule sat behind -- including the
// no-real-e2e rule this package's header holds up as its worked example.
//
// Normalisation is deliberately confined to this function. Widening
// hasPrefix or normalizeProgram would let rules match commands they were not
// written for, turning a tightening into a loosening; here every added form
// can only make a deny rule fire more often.
func containsAny(args, flags []string) bool {
	if len(flags) == 0 {
		return false
	}
	for _, flag := range flags {
		name, want, hasValue := strings.Cut(flag, "=")
		for i, arg := range args {
			// `--tags` and `-tags` are the same flag to Go's flag package and
			// to most CLIs, so a rule written either way must catch both.
			argName, argVal, argHasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
			if argName != strings.TrimLeft(name, "-") {
				continue
			}
			if !hasValue {
				return true // the rule denies the flag regardless of its value
			}
			if !argHasValue {
				// `-tags e2e_real`: the value is the next argument.
				if i+1 < len(args) {
					argVal = args[i+1]
				} else {
					continue
				}
			}
			// A comma list carries the denied value if any element matches;
			// a plain value is a one-element list, so one path covers both.
			for _, part := range strings.Split(argVal, ",") {
				if strings.TrimSpace(part) == want {
					return true
				}
			}
		}
	}
	return false
}
