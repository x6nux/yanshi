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

// containsAny reports whether any element of args equals flag or starts with
// flag+"=". The "=" form covers `-tags=e2e_real` when the rule lists
// `-tags=e2e_real` as the flag (the canonical form). A nil/empty flags list
// never matches.
func containsAny(args, flags []string) bool {
	if len(flags) == 0 {
		return false
	}
	for _, arg := range args {
		for _, flag := range flags {
			if arg == flag || strings.HasPrefix(arg, flag+"=") {
				return true
			}
		}
	}
	return false
}
