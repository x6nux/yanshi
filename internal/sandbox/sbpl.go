package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// This file holds the SBPL (Sandbox Profile Language) generator. It lives in a
// platform-neutral file on purpose: profile text is a pure function of a
// ProfileInput, so it compiles and is testable on Linux and Windows CI legs
// even though only darwin can hand the result to sandbox-exec. Nothing here
// imports a darwin-only package, and nothing here touches a process.
//
// SBPL is a Scheme dialect the macOS kernel's Sandbox (TrustedBSD MAC) module
// consumes. The grammar relevant to us:
//
//	(version 1)                      ; mandatory first form
//	(deny default)                   ; default-deny; everything below is an
//	                                 ; explicit carve-out
//	(allow file-read*)               ; unfiltered: applies to every path
//	(allow file-write* (subpath "/x")) ; filtered by a path predicate
//	(deny  network*)
//
// Filters used here and why they and not others:
//
//   - (subpath "P") matches P and everything beneath it. It is NOT a string
//     prefix: a sibling directory "/tmp/wsEVIL" does not match (subpath
//     "/tmp/ws"). Verified empirically before this file was written, because
//     if it HAD been a prefix match, every workspace whose name is a prefix of
//     a sibling would have been silently writable.
//   - (literal "P") matches exactly P. Used for the device nodes.
//   - (regex #"...") matches the resolved path. Used only for the ttys
//     range, where enumerating literals is impossible.
//
// Two SBPL rule-resolution facts are load-bearing for the shape below, both
// measured against /usr/bin/sandbox-exec on macOS 15.7.7 (24G720) rather than
// taken from documentation, which does not state them:
//
//  1. Resolution is NOT last-match-wins, and it is not first-match-wins
//     either: a (deny X (filter)) beats a broader (allow X) regardless of
//     which is written first. `(allow file-read*)` followed by `(deny
//     file-read* (subpath "~/.ssh"))` and the same two lines in the opposite
//     order both refuse the read. That is why DenyPaths can be appended at the
//     end of the profile without any ordering ceremony.
//
//  2. The converse also holds and is the reason WorkspaceWrite works at all:
//     a filtered (allow file-write* (subpath WS)) beats the blanket (deny
//     file-write*) in either order. So "deny everything, then carve out the
//     workspace" is expressible, which is the whole tier.
//
// The combination means a nested deny inside an allowed subpath wins (a
// DenyPath under the workspace root is genuinely unwritable), which is what
// makes DenyPaths usable for credential directories that happen to live inside
// a workspace.

// sbplVersion is the profile dialect header every profile must open with.
// sandbox-exec rejects a profile without it before looking at any rule.
const sbplVersion = "(version 1)"

// ProfileInput is everything the SBPL generator needs. It is deliberately a
// plain data struct with no methods that touch the filesystem or the process:
// BuildProfile is a pure function of this value, which is what lets the golden
// tests run on every platform and lets an operator diff two profiles by
// diffing two inputs.
//
// WorkspaceRoot and the paths in WritablePaths/DenyPaths MUST already be
// symlink-resolved (see ResolvePath). Seatbelt matches the RESOLVED path, so
// an unresolved /var/folders/... workspace on macOS produces a profile whose
// workspace rule can never match — the tier silently degrades to ReadOnly and
// the operator sees "Operation not permitted" writing into their own workspace.
type ProfileInput struct {
	// Tier selects the write posture: ReadOnly denies all writes,
	// WorkspaceWrite carves out WorkspaceRoot plus WritablePaths, FullAccess
	// imposes no write restriction.
	Tier AccessTier
	// WorkspaceRoot is the resolved directory the WorkspaceWrite tier grants
	// write access to. Ignored at the other two tiers. Empty at
	// WorkspaceWrite means no workspace carve-out (the tier then differs from
	// ReadOnly only by the scratch directories).
	WorkspaceRoot string
	// WritablePaths are additional resolved directories writable at
	// WorkspaceWrite. This is where the scratch/cache set computed by
	// ScratchPaths lands.
	WritablePaths []string
	// DenyPaths are resolved directories denied for both read and write at
	// every tier, including FullAccess. Credential stores go here.
	DenyPaths []string
	// NetworkDeny denies all IP and unix-domain networking except the
	// unix-socket carve-out described at buildNetwork.
	NetworkDeny bool
	// AllowLoopback re-permits 127.0.0.1/::1 while NetworkDeny is in force,
	// for children that must talk to a local proxy or test server. It has no
	// effect when NetworkDeny is false.
	AllowLoopback bool
}

// readOnlyDevices are the device nodes a process must be able to WRITE to even
// under the ReadOnly tier, because "read-only" is a statement about the
// filesystem, not about the process's ability to emit output.
//
// Omitting /dev/null alone breaks `cmd > /dev/null`, which appears in a large
// fraction of real shell commands; omitting /dev/tty breaks any program that
// opens the controlling terminal directly rather than using fd 1 (git's pager
// probe, ssh's askpass, sudo). /dev/dtracehelper is opened by the dyld startup
// path for some binaries. They are granted file-write-data rather than
// file-write* deliberately: write-data permits writing bytes into an existing
// node but not creating, unlinking, chmod'ing or otherwise mutating the
// namespace, so a compromised child cannot replace /dev/null with a regular
// file that captures everything later written to it.
var readOnlyDevices = []string{
	"/dev/null",
	"/dev/zero",
	"/dev/random",
	"/dev/urandom",
	"/dev/tty",
	"/dev/dtracehelper",
	"/dev/stdout",
	"/dev/stderr",
	"/dev/fd",
}

// ttyDeviceRegex matches the pseudo-terminal slave nodes (/dev/ttys003 and
// friends). A PTY-backed child writes to whichever slave the kernel handed it,
// and the number is not knowable when the profile is generated, so this is the
// one place a regex is unavoidable. /dev/ptmx (the multiplexer) is granted
// separately as a literal.
//
// The anchors matter: an unanchored ttys[0-9]+ would also match a path a child
// created named /workspace/dev/ttys1, and file-write* on an attacker-chosen
// path is exactly what this file exists to prevent.
const ttyDeviceRegex = `^/dev/ttys[0-9]+$`

// BuildProfile renders the SBPL text for input.
//
// The output is deterministic — same input, same bytes — so it can be golden
// tested and so an operator comparing two runs sees a real difference rather
// than map-iteration noise. Paths are deduplicated and sorted for the same
// reason.
//
// Every path is rendered through quoteSBPL. That is not stylistic: an
// unescaped double quote in a path closes the string literal early and the
// remainder of the path is parsed as SBPL source. A workspace root named
//
//	/tmp/ws")) (allow file-write*) ;
//
// renders, unescaped, to a profile containing a blanket `(allow file-write*)`
// — a complete escape from the tier, verified against the real sandbox-exec
// before this comment was written. It is not merely a syntax error: the
// trailing `;` comments out the wreckage so the profile compiles and runs.
func BuildProfile(input ProfileInput) string {
	var b strings.Builder
	b.WriteString(sbplVersion)
	b.WriteString("\n")

	if input.Tier == FullAccess {
		buildFullAccess(&b, input)
		return b.String()
	}

	b.WriteString("\n; Default-deny: every capability below is an explicit carve-out.\n")
	b.WriteString("(deny default)\n")

	buildProcessRules(&b)
	buildReadRules(&b)
	buildWriteRules(&b, input)
	buildNetwork(&b, input)
	buildDenyPaths(&b, input.DenyPaths)

	return b.String()
}

// ProfileRestrictsNothing reports whether the profile BuildProfile would render
// for input imposes no restriction whatsoever — i.e. it is exactly
// `(allow default)` with no filtered deny after it.
//
// This exists because a backend must not report OS isolation it is not
// delivering, and FullAccess is the one tier where "the sandbox is working
// perfectly" and "the sandbox is denying nothing" are the same state. At
// FullAccess the profile opens with (allow default); the only rules that can
// follow are the network denial and DenyPaths. With NetworkDeny false and
// DenyPaths empty, the rendered profile has no deny in it at all, so the child
// runs with the full authority of the user — and a caller reading
// Effective=os-isolated would be entitled to believe otherwise.
//
// It is a predicate over the INPUT rather than a scan of the rendered text on
// purpose: matching on generated strings would silently stop being true the
// first time a rule is reworded, and the failure mode of that staleness is a
// report that over-claims, which is the exact thing this guards against.
//
// The other two tiers can never satisfy this. Both render (deny default), so
// they always restrict something regardless of the network and DenyPaths
// settings — TestProfileRestrictsNothingOnlyAtFullAccess pins that.
//
// DenyPaths is filtered through dedupeSorted, the SAME function buildDenyPaths
// uses to decide whether to emit anything. Re-implementing the "is this path
// list effectively empty?" rule here would let the two answers diverge, and the
// direction they would diverge in is the dangerous one: a list this function
// judged non-empty but the builder dropped would suppress the disclosure while
// the profile really did restrict nothing.
func ProfileRestrictsNothing(input ProfileInput) bool {
	return input.Tier == FullAccess && !input.NetworkDeny && len(dedupeSorted(input.DenyPaths)) == 0
}

// buildFullAccess renders the FullAccess tier: no filesystem restriction, with
// the network and credential-deny carve-outs still applied.
//
// FullAccess is (allow default) rather than an enumeration because the tier's
// contract is "the sandbox imposes no ACCESS-TIER restriction" — the value it
// still provides is the network denial and the DenyPaths, both of which
// override the blanket allow per the rule-resolution facts documented at the
// top of this file.
//
// When neither of those is configured the result restricts nothing at all.
// That is a legitimate operator choice, but it must not be REPORTED as OS
// isolation; see ProfileRestrictsNothing and the darwin backend's Reason.
func buildFullAccess(b *strings.Builder, input ProfileInput) {
	b.WriteString("\n; FullAccess: no filesystem restriction. Network and\n")
	b.WriteString("; credential denials below still apply — a filtered deny\n")
	b.WriteString("; overrides the blanket allow.\n")
	b.WriteString("(allow default)\n")
	buildNetwork(b, input)
	buildDenyPaths(b, input.DenyPaths)
}

// buildProcessRules emits the capabilities any non-trivial program needs
// before it can run at all under (deny default).
//
// Each line here was added because its absence produced a concrete failure,
// not out of caution:
//
//   - process-exec*: without it sandbox-exec cannot even exec the target.
//     `(version 1)(deny default)` alone yields "execvp() of '/usr/bin/true'
//     failed: Operation not permitted" — the sandbox refuses before the
//     program's first instruction.
//   - process-fork: any shell, any build tool, anything that spawns.
//   - signal (target same-sandbox): a shell must be able to signal the jobs it
//     started. `(allow signal (target self))` is NOT sufficient and this is
//     counter-intuitive: measured, "self" does not include the process's own
//     children, so `sleep 30 & kill $!` fails with EPERM under it. A bare
//     `(allow signal)` does work but is far too wide — measured, it lets the
//     sandboxed child kill an arbitrary UNRELATED pid on the host, which is
//     precisely the "cross-session kill" risk the guard's auto-approval prompt
//     names. same-sandbox is the setting that permits the first and refuses
//     the second.
//   - sysctl-read / mach-lookup / ipc-posix-shm / ipc-posix-sem: the dyld and
//     libSystem startup path consults all of these. Without mach-lookup most
//     dynamically linked binaries fail before main().
func buildProcessRules(b *strings.Builder) {
	b.WriteString("\n; Process lifecycle. signal is scoped to same-sandbox:\n")
	b.WriteString("; (target self) cannot signal own children; a bare (allow\n")
	b.WriteString("; signal) can kill unrelated host processes.\n")
	b.WriteString("(allow process-exec*)\n")
	b.WriteString("(allow process-fork)\n")
	b.WriteString("(allow signal (target same-sandbox))\n")
	b.WriteString("\n; Runtime startup: dyld/libSystem consult all of these.\n")
	b.WriteString("(allow sysctl-read)\n")
	b.WriteString("(allow mach-lookup)\n")
	b.WriteString("(allow ipc-posix-shm)\n")
	b.WriteString("(allow ipc-posix-sem)\n")
}

// buildReadRules grants unfiltered read.
//
// This backend is a WRITE and NETWORK boundary, not a read boundary, and
// saying so plainly is more useful than a read allowlist that would be wrong.
// An allowlist tight enough to be meaningful would have to enumerate every
// path a toolchain touches — SDK roots, dyld shared caches, locale databases,
// certificate stores, per-language module caches — and the failure mode of a
// missing entry is a build that dies with an unattributable "Operation not
// permitted" deep inside a compiler. QwenPaw's macOS backend reached the same
// conclusion: its default (allow_read_all=True) is deny-list, not allow-list.
//
// The read boundary that does exist is DenyPaths, which is where credential
// directories belong. See buildDenyPaths.
func buildReadRules(b *strings.Builder) {
	b.WriteString("\n; Unfiltered read. This backend is a WRITE and NETWORK\n")
	b.WriteString("; boundary; the read boundary is the DenyPaths list below.\n")
	b.WriteString("(allow file-read*)\n")
}

// buildWriteRules emits the write posture for the ReadOnly and WorkspaceWrite
// tiers (FullAccess never reaches here).
//
// Both tiers start from a blanket (deny file-write*) and carve back. The
// device grants are identical at both tiers because they are about producing
// output, not about the access tier -- see readOnlyDevices.
//
// # The blanket deny is redundant, and saying so is the point
//
// Measured against the real sandbox: with (deny file-write*) removed, ReadOnly
// still refuses `echo x > /tmp/f`, and WorkspaceWrite still refuses a write
// outside the workspace while permitting one inside. (deny default) already
// covers it -- an unlisted operation is denied, and file-write* is unlisted.
//
// It is kept anyway, for two reasons that are worth more than the line it
// costs. It states the posture legibly to anyone auditing a generated profile,
// who should not have to know that the absence of a rule is the rule. And it
// is insurance: a future edit that adds a broad (allow file-write* ...) for
// some new purpose would silently widen both tiers, where with the blanket
// deny present the widening at least has to be written deliberately.
//
// What this comment must NOT become is a claim that the line is load-bearing.
// A mutation probe that deleted it found no test failed at the process level,
// only the text-shape assertion -- and a redundant line documented as
// essential is how the next person ends up building a mental model of SBPL
// that is wrong.
func buildWriteRules(b *strings.Builder, input ProfileInput) {
	b.WriteString("\n; Blanket write denial. REDUNDANT under (deny default) --\n")
	b.WriteString("; measured, removing it changes no observable behaviour at\n")
	b.WriteString("; either tier. Kept as a legible statement of intent and as\n")
	b.WriteString("; insurance if a future rule widens the default. See\n")
	b.WriteString("; buildWriteRules for why that is stated rather than hidden.\n")
	b.WriteString("(deny file-write*)\n")

	b.WriteString("\n; Output devices: write-data only, so the child can emit\n")
	b.WriteString("; bytes but cannot replace the node with a capture file.\n")
	b.WriteString("(allow file-write-data\n")
	for _, dev := range readOnlyDevices {
		fmt.Fprintf(b, "  (literal %s)\n", quoteSBPL(dev))
	}
	fmt.Fprintf(b, "  (literal %s)\n", quoteSBPL("/dev/ptmx"))
	fmt.Fprintf(b, "  (regex #%s))\n", quoteSBPL(ttyDeviceRegex))

	b.WriteString("\n; Terminal control: ioctl on the tty/pty pair, so a child\n")
	b.WriteString("; that queries its window size does not die on EPERM.\n")
	b.WriteString("(allow file-ioctl\n")
	fmt.Fprintf(b, "  (literal %s)\n", quoteSBPL("/dev/tty"))
	fmt.Fprintf(b, "  (literal %s)\n", quoteSBPL("/dev/ptmx"))
	fmt.Fprintf(b, "  (literal %s)\n", quoteSBPL("/dev/dtracehelper"))
	fmt.Fprintf(b, "  (regex #%s))\n", quoteSBPL(ttyDeviceRegex))

	if input.Tier != WorkspaceWrite {
		return
	}

	writable := dedupeSorted(append([]string{input.WorkspaceRoot}, input.WritablePaths...))
	if len(writable) == 0 {
		return
	}
	b.WriteString("\n; WorkspaceWrite carve-outs (workspace + scratch/caches).\n")
	b.WriteString("(allow file-write*\n")
	for _, p := range writable {
		fmt.Fprintf(b, "  (subpath %s)\n", quoteSBPL(p))
	}
	b.WriteString(")\n")
}

// buildNetwork emits the network posture.
//
// The NetworkDeny=false branch must emit an explicit (allow network*) at the
// restricted tiers and this is easy to get wrong: under (deny default) the
// network is already denied, so "not denying it" is not the same as allowing
// it. Omitting the allow produced a ReadOnly sandbox with no network at all
// regardless of configuration — caught by the control arm of the egress test,
// which could not reach a loopback server it was supposed to be permitted to
// reach. FullAccess does not reach here for this branch because its
// (allow default) already covers the network.
//
// (deny network*) denies unix-domain sockets too, which is a real and
// surprising consequence: it is not an IP-level switch. Measured, a child
// under a bare (deny network*) cannot bind() an AF_UNIX socket in its own
// temp directory. That breaks anything using a local socket for IPC that has
// nothing to do with egress. So the unix-socket family is carved back in
// explicitly, and the IP family stays denied — verified in the same run that
// the carve-out did NOT restore outbound TCP.
//
// AllowLoopback exists because the netpolicy proxy is a loopback listener: a
// child denied all IP cannot reach the very proxy that would have mediated its
// egress. The rule must name BOTH local and remote with localhost filters; a
// filtered (allow network-bind (local ip)) without the address filter was
// measured NOT to restore loopback bind.
//
// What this cannot do: Seatbelt has no domain- or port-level filtering. A
// per-host allowlist is not expressible here, which is why the netpolicy proxy
// remains the only place host allowlists are enforced. QwenPaw's backend
// carries the same limitation and the same comment.
func buildNetwork(b *strings.Builder, input ProfileInput) {
	if !input.NetworkDeny {
		if input.Tier != FullAccess {
			b.WriteString("\n; Network permitted. The explicit allow is required:\n")
			b.WriteString("; under (deny default), not denying is not allowing.\n")
			b.WriteString("(allow network*)\n")
		}
		return
	}
	b.WriteString("\n; Network denial. Note this also denies AF_UNIX, so the\n")
	b.WriteString("; unix-socket family is carved back in — measured NOT to\n")
	b.WriteString("; restore outbound IP. Seatbelt has no host/port filter:\n")
	b.WriteString("; per-host allowlists live in the netpolicy proxy.\n")
	b.WriteString("(deny network*)\n")
	b.WriteString("(allow network-outbound (remote unix-socket))\n")
	b.WriteString("(allow network-bind (local unix-socket))\n")
	if input.AllowLoopback {
		b.WriteString("\n; Loopback re-permitted so the child can reach the\n")
		b.WriteString("; local egress proxy. Both filters are required.\n")
		b.WriteString("(allow network* (local ip \"localhost:*\"))\n")
		b.WriteString("(allow network* (remote ip \"localhost:*\"))\n")
	}
}

// buildDenyPaths emits read+write denials that override every allow above,
// including FullAccess's (allow default).
//
// Read and write are denied separately rather than via a single rule because
// SBPL has no combined operation covering both, and the read half is the one
// that matters for a credential store: a child that cannot read ~/.ssh cannot
// exfiltrate the key regardless of whether it could have written there.
func buildDenyPaths(b *strings.Builder, paths []string) {
	paths = dedupeSorted(paths)
	if len(paths) == 0 {
		return
	}
	b.WriteString("\n; Denied outright at every tier. A filtered deny overrides\n")
	b.WriteString("; both (allow file-read*) and FullAccess's (allow default).\n")
	b.WriteString("(deny file-read*\n")
	for _, p := range paths {
		fmt.Fprintf(b, "  (subpath %s)\n", quoteSBPL(p))
	}
	b.WriteString(")\n")
	b.WriteString("(deny file-write*\n")
	for _, p := range paths {
		fmt.Fprintf(b, "  (subpath %s)\n", quoteSBPL(p))
	}
	b.WriteString(")\n")
}

// quoteSBPL renders s as an SBPL string literal, including the surrounding
// double quotes.
//
// This is the injection boundary of the whole backend. SBPL string literals
// are C-like: backslash escapes backslash and double quote. Escaping order is
// load-bearing — backslash first, then quote, or the backslash pass would
// double the backslash this pass just introduced.
//
// Control characters cannot be escaped into a literal reliably, and a raw
// newline inside one terminates the literal in the parser. Rather than fail
// (Prepare has no good error channel to the operator for a path the model
// merely mentioned) they are rendered as an unmatchable placeholder: the
// resulting rule is syntactically valid and matches nothing, so a path with a
// newline in it is denied rather than granted. That is the fail-closed
// direction. See sanitizeSBPLPath.
//
// A worked example of what happens without this, measured against the real
// sandbox-exec: the workspace root
//
//	/tmp/ws")) (allow file-write*) ;
//
// interpolated raw into `(allow file-write* (subpath "<root>"))` yields a
// profile with a blanket `(allow file-write*)` and a comment swallowing the
// leftovers. It compiles. A subsequent `echo pwn > /tmp/PWNED` succeeded with
// exit 0 and the file existed. With this function the same root produces a
// single harmless subpath rule and the write is refused.
func quoteSBPL(s string) string {
	s = sanitizeSBPLPath(s)
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' || c == '"' {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	b.WriteByte('"')
	return b.String()
}

// unmatchablePath is substituted for any path carrying a character that cannot
// survive an SBPL string literal. It is an absolute path under a reserved
// name, so the rule it produces is well-formed and matches no real file.
//
// Substituting rather than erroring is the fail-closed choice for the two
// directions this can occur in: as a WRITE carve-out the rule grants nothing
// (correct — a path we cannot express must not be writable), and as a DENY
// entry it denies nothing, but a deny entry can only ever have come from
// yanshi's own configuration, never from model-controlled text.
const unmatchablePath = "/dev/null/yanshi-unrepresentable-path"

// sanitizeSBPLPath replaces any string containing a character that cannot be
// represented in an SBPL string literal.
//
// The rejected set is control characters (< 0x20) plus DEL. Newline and
// carriage return are the dangerous ones — they terminate the literal and turn
// the remainder into source text — but no legitimate filesystem path contains
// any C0 control character either, so the whole range goes. Tab is included in
// the rejection despite being nominally printable-ish: it cannot be escaped
// into an SBPL literal and a path containing one is overwhelmingly more likely
// to be an injection attempt than a real directory.
func sanitizeSBPLPath(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return unmatchablePath
		}
	}
	return s
}

// dedupeSorted returns the unique non-empty entries of in, sorted.
//
// Sorting is what makes BuildProfile deterministic for a caller that builds
// its path list from a map or from environment iteration. Dropping empties is
// what keeps an unset WorkspaceRoot from rendering `(subpath "")`, which
// matches nothing useful and reads, to anyone auditing the profile, like a
// bug rather than an absent value.
func dedupeSorted(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// ResolvePath returns the symlink-resolved absolute form of p, or "" when p is
// empty.
//
// This is not a cosmetic normalization, it is a correctness requirement of
// every Seatbelt path rule, and getting it wrong is the single most likely way
// to ship a profile that looks right and enforces nothing useful. Seatbelt
// matches the RESOLVED path. On macOS the per-user temp directory that
// t.TempDir() and mktemp hand out is /var/folders/..., and /var is a symlink to
// /private/var. A profile granting (subpath "/var/folders/…/ws") therefore
// never matches the kernel's /private/var/folders/…/ws and the workspace is
// silently read-only — measured exactly this way while developing this file,
// where the inside-workspace write failed with EPERM identically to the
// outside-workspace write, i.e. the test would have "passed" for the wrong
// reason had it only asserted the outside case.
//
// On failure (path does not exist yet, or a permission error walking it) the
// input is returned made-absolute rather than dropped: a workspace directory
// that will be created by the child still needs its rule, and an absolute
// unresolved path is a strictly better guess than nothing. The caller cannot
// distinguish, which is acceptable because the failure mode is a rule that
// does not match — fail-closed — not one that matches too much.
func ResolvePath(p string) string {
	if p == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		if abs, err := filepath.Abs(resolved); err == nil {
			return abs
		}
		return resolved
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// ScratchPaths returns the directories a WorkspaceWrite child must be able to
// write to besides its workspace, resolved and deduplicated.
//
// This list is the difference between a usable sandbox and one that "cannot
// run the compiler", and every entry was established by running a real
// toolchain under a real profile rather than by reasoning about it. Measured
// with `go build` under a workspace-only profile:
//
//	workspace only         → go: creating work dir: mkdir /var/folders/…/T/
//	                         go-build…: operation not permitted
//	+ TMPDIR               → failed to initialize build cache at
//	                         ~/Library/Caches/go-build…: operation not permitted
//	+ TMPDIR + ~/Library/Caches → builds, links, and the binary runs
//
// Each subsequent failure was only visible after the previous one was fixed,
// which is precisely why an invented list would be wrong: it would look
// complete right up until a compiler failed in a way nobody could attribute.
//
// The entries:
//
//   - TMPDIR: on macOS this is the per-user /var/folders/…/T, not /tmp. Taken
//     from the environment because it is per-user and per-boot.
//   - /tmp and /var/tmp: the POSIX locations, still used by tools that ignore
//     TMPDIR. Listed by their /private/… resolved forms via ResolvePath.
//   - ~/Library/Caches: the macOS convention, and where GOCACHE, the Swift
//     module cache, and npm's cache actually live.
//   - ~/.cache: the XDG convention, used on macOS by cargo, pip and anything
//     ported from Linux.
//
// Deliberately NOT included: ~/.npm, ~/.cargo, ~/go/pkg/mod and other
// per-ecosystem package roots. Those are download destinations for code
// fetched from the network, and a tier whose point is "writes stay in the
// workspace" should not silently also grant write access to the place the next
// build resolves its dependencies from — that is a supply-chain write, not a
// scratch write. A workspace that needs them should set the ecosystem's own
// cache variable to a path inside the workspace, which is what the sandboxed
// go build in this package's own test does with GOCACHE.
//
// # The honest boundary
//
// These grants are DIRECTORY-WIDE, not per-sandbox. A WorkspaceWrite child can
// write anywhere under TMPDIR, /tmp, /var/tmp, ~/Library/Caches and ~/.cache —
// including over another process's temp files and over build caches that later
// builds will read back. So "workspace-write" means "writes are confined to
// the workspace plus shared scratch", not "writes are confined to the
// workspace". Two consequences worth stating plainly rather than discovering:
// a sandboxed child can poison a build cache that a LATER unsandboxed build
// consumes, and two concurrent sandboxes are not isolated from each other's
// temp files.
//
// This is the same tradeoff QwenPaw's macOS backend makes (it grants /tmp and
// /private/tmp unconditionally), and the alternative — minting a private temp
// directory per sandbox and overriding TMPDIR for the child — was not taken
// here because it silently changes an environment variable the caller set,
// and only covers tools that honour TMPDIR in the first place. Narrowing it
// belongs in its own change with its own tests, not smuggled into the grant
// list. TestScratchGrantIsDirectoryWide pins the current behaviour so the
// boundary cannot quietly move in either direction.
func ScratchPaths() []string {
	candidates := []string{
		os.TempDir(),
		"/tmp",
		"/var/tmp",
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates,
			filepath.Join(home, "Library", "Caches"),
			filepath.Join(home, ".cache"),
		)
	}
	resolved := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if r := ResolvePath(c); r != "" {
			resolved = append(resolved, r)
		}
	}
	return dedupeSorted(resolved)
}
