package sandbox

import (
	"os"
	"path/filepath"
	"strings"
)

// This file holds the bubblewrap argument generator. Like sbpl.go it lives in
// a platform-neutral file on purpose: the argument vector is a pure function
// of a BwrapInput plus an injectable existence predicate, so it compiles and
// is testable on darwin and windows CI legs even though only Linux can hand
// the result to a real bwrap.
//
// # Why an argument vector and not a config file
//
// bubblewrap has no policy-file format. The mount plan IS the command line,
// applied strictly left to right: each --ro-bind / --bind / --tmpfs / --dev /
// --proc mutates the namespace being constructed, and a later operation over
// the same target wins. Every ordering decision below is therefore load-
// bearing, and the tests pin relative order rather than mere membership.
//
// # The existence predicate is not a convenience
//
// bwrap FAILS THE WHOLE LAUNCH when a bind source does not exist:
//
//	bwrap: Can't find source path /home/x/.cache: No such file or directory
//
// exiting 1 before the target program runs. So a generator that emitted a
// static scratch list would turn "this host has no ~/.cache yet" into "every
// sandboxed command fails", which is a denial of service produced by the
// security layer. Every bind emitted here is existence-checked first. The
// predicate is injectable so the generator's tests can drive hosts that do not
// exist on the machine running them -- without that seam a darwin test could
// only ever assert the behaviour of the darwin filesystem it happens to run
// on, and the Linux-shaped cases (/dev/shm, /run/user/N) would be untestable
// where they are actually written.
//
// # Argument quoting is deliberately absent, and that is correct
//
// These strings become argv entries passed to execve, never to a shell. There
// is no metacharacter to escape and adding quotes would create paths with
// literal quote characters in them. The injection surface that DOES exist is
// argument-position confusion, handled by sanitizeBindPath and by the `--`
// terminator; see those comments.

// bwrapProgram is the bubblewrap binary name resolved through PATH.
//
// Unlike darwin's sandbox-exec -- which is hard-coded to /usr/bin/sandbox-exec
// because it sits on the read-only signed system volume -- bubblewrap has no
// single canonical location across distributions: Debian and Fedora ship
// /usr/bin/bwrap, NixOS ships a /nix/store path, and Arch's is /usr/bin/bwrap
// but a user install may land in /usr/local/bin. Hard-coding one path would
// report "no sandbox" on hosts that have a perfectly good one.
//
// The cost is real and is stated rather than hidden: whoever controls PATH
// controls which binary plays the enforcement role. That is mitigated, not
// eliminated, by the fact that the probe (probeBwrapAt) does not merely check
// for the file -- it requires the resolved binary to actually DENY a write
// that must fail. A pass-through shim planted on PATH fails that probe and the
// backend degrades honestly instead of reporting os-isolated.
const bwrapProgram = "bwrap"

// BwrapInput is the complete description of one bubblewrap invocation's mount
// plan. It carries no platform types so the generator stays portable.
type BwrapInput struct {
	// Tier selects the mount plan shape. See BuildBwrapArgs.
	Tier AccessTier
	// WorkspaceRoot is the directory made writable at WorkspaceWrite. Ignored
	// at ReadOnly (nothing is writable) and at FullAccess (everything is).
	WorkspaceRoot string
	// ScratchPaths are the additional writable directories a WorkspaceWrite
	// child needs so that compilers and package managers can function. See
	// BwrapScratchPaths for how the list is derived and what it costs.
	ScratchPaths []string
	// NetworkDeny removes the child's network namespace access entirely.
	NetworkDeny bool
	// Chdir is the working directory to enter inside the namespace. Emitted
	// only when it exists, because bwrap fails the launch on a missing chdir
	// target the same way it does on a missing bind source.
	Chdir string
	// Exists reports whether a path is present and usable as a bind source.
	// nil selects the real filesystem. Injected by tests.
	Exists func(string) bool
}

// exists applies the input's predicate, defaulting to a real stat.
func (in BwrapInput) exists(p string) bool {
	if p == "" {
		return false
	}
	if in.Exists != nil {
		return in.Exists(p)
	}
	_, err := os.Stat(p)
	return err == nil
}

// BuildBwrapArgs renders the bubblewrap argument vector for one invocation,
// excluding the bwrap program name itself and excluding the trailing
// `-- <program> <args>`, both of which the platform adapter appends.
//
// # The three tiers
//
// ReadOnly binds the entire filesystem read-only and gives the child a private
// tmpfs at /tmp, a synthetic /dev, a fresh /proc, and no namespaces it does
// not need. Nothing on the host is writable. The private /tmp matters: a
// read-only tier that still shared the host /tmp would let a "read-only"
// command drop a file another process reads back, which is a write by any
// useful definition.
//
// WorkspaceWrite is ReadOnly plus a read-write bind of WorkspaceRoot and of
// each existing ScratchPaths entry. The scratch binds intentionally come after
// the --tmpfs /tmp so a scratch entry naming /tmp replaces the private tmpfs
// with the shared host one; see BwrapScratchPaths for why that trade is made
// and TestBwrapScratchGrantIsDirectoryWide for the boundary it creates.
//
// FullAccess restricts no files at all: it binds / read-WRITE. It is not a
// no-op, because it still unshares the PID namespace, which is the one
// guarantee this tier does make -- the child cannot see or signal host
// processes, and its own descendants are reaped with it. The filesystem is
// exactly the host's.
//
// # Why FullAccess uses --bind / / rather than passing no mount arguments
//
// bubblewrap does not start from the host root and subtract. It always creates
// a fresh mount namespace with an empty root and applies exactly what it is
// told. Given no filesystem arguments the child sees an empty directory and
// cannot even exec /bin/sh. So "do not restrict the filesystem" has to be
// spelled --bind / /, and omitting it would produce the most confusing
// possible failure: the tier named full-access being the only one that cannot
// run anything.
func BuildBwrapArgs(in BwrapInput) []string {
	args := make([]string, 0, 32)

	// --new-session allocates a new session keyring and detaches the child
	// from the controlling terminal, which closes the TIOCSTI/TIOCLINUX
	// terminal-injection class: without it a sandboxed child can push
	// characters into the parent's terminal input queue and have the operator's
	// shell execute them after the sandbox exits. --die-with-parent makes the
	// kernel SIGKILL the sandbox when yanshi goes away, so a killed turn cannot
	// leave an orphaned sandboxed process running.
	args = append(args, "--new-session", "--die-with-parent")

	if in.Tier == FullAccess {
		return append(args, buildBwrapFullAccess(in)...)
	}

	// Base: the whole host, read-only. Emitted first so every writable carve-
	// out below is layered over it.
	args = append(args, "--ro-bind", "/", "/")
	// A private, writable, discarded /tmp. Also shadows anything the host has
	// at /tmp, so a leftover world-writable file there is not visible.
	args = append(args, "--tmpfs", "/tmp")
	// A synthetic /dev with only null/zero/full/random/urandom/tty. This is
	// narrower than the host /dev the --ro-bind above would otherwise expose:
	// no raw block devices, no /dev/mem, no /dev/kmsg.
	args = append(args, "--dev", "/dev")
	// A fresh /proc matching the new PID namespace. Required for correctness,
	// not just isolation: without it the child sees the HOST /proc while
	// holding host-invalid pids, and ps, pgrep and anything reading
	// /proc/self/... misbehave.
	args = append(args, "--proc", "/proc")

	if in.Tier == WorkspaceWrite {
		args = append(args, buildBwrapWritable(in)...)
	}

	// --unshare-all covers user, ipc, pid, net, uts and cgroup namespaces.
	// Network is re-shared explicitly when the policy permits it, which keeps
	// deny the default: a future bwrap that grows another namespace is
	// unshared automatically rather than needing this list updated.
	args = append(args, "--unshare-all")
	if !in.NetworkDeny {
		args = append(args, "--share-net")
	}

	return append(args, buildBwrapChdir(in)...)
}

// buildBwrapFullAccess renders the FullAccess plan: process isolation only.
//
// No --dev and no --tmpfs here. Both would NARROW the filesystem, and this
// tier's contract is that it does not. --proc /proc stays because it is what
// makes the unshared PID namespace coherent rather than an isolation measure
// in its own right.
func buildBwrapFullAccess(in BwrapInput) []string {
	args := []string{"--bind", "/", "/", "--proc", "/proc", "--unshare-pid"}
	if in.NetworkDeny {
		args = append(args, "--unshare-net")
	}
	return append(args, buildBwrapChdir(in)...)
}

// buildBwrapWritable renders the read-write carve-outs for WorkspaceWrite.
//
// The workspace is emitted before the scratch paths so that a scratch entry
// nested inside the workspace (an operator whose project lives under /tmp, say)
// layers on top rather than being shadowed. Both are --bind, so the relative
// order only matters when one contains the other, but leaving it unspecified
// would make the resulting namespace depend on map iteration order somewhere
// upstream.
//
// Every path is sanitized and existence-checked. A workspace root that does
// not exist is SKIPPED rather than created: this generator must not have a
// filesystem side effect, and a bwrap launch that fails outright is worse than
// one where the child gets a clear EROFS on its first write.
func buildBwrapWritable(in BwrapInput) []string {
	var args []string
	seen := map[string]bool{}
	for _, p := range append([]string{in.WorkspaceRoot}, in.ScratchPaths...) {
		clean, ok := sanitizeBindPath(p)
		if !ok || seen[clean] || !in.exists(clean) {
			continue
		}
		seen[clean] = true
		args = append(args, "--bind", clean, clean)
	}
	return args
}

// buildBwrapChdir emits --chdir when the directory exists inside the sandbox.
//
// Existence is checked against the HOST because at ReadOnly and WorkspaceWrite
// the host root is bound in, so host presence implies sandbox presence. The
// one case where that reasoning fails is a chdir target under /tmp, which the
// private tmpfs empties: bwrap would then fail the launch. Emitting nothing
// leaves the child in the namespace root, which runs.
func buildBwrapChdir(in BwrapInput) []string {
	clean, ok := sanitizeBindPath(in.Chdir)
	if !ok || !in.exists(clean) {
		return nil
	}
	if in.Tier != FullAccess && underPrivateTmp(clean) {
		return nil
	}
	return []string{"--chdir", clean}
}

// underPrivateTmp reports whether p would be hidden by the --tmpfs /tmp that
// the ReadOnly and WorkspaceWrite plans install.
//
// Path-component comparison, not a string prefix: "/tmpfoo" is not under
// "/tmp" and treating it as such would drop a legitimate --chdir. This is the
// same distinction sbpl.go documents for (subpath ...).
func underPrivateTmp(p string) bool {
	return p == "/tmp" || strings.HasPrefix(p, "/tmp/")
}

// sanitizeBindPath normalises a path for use as a bwrap operand and reports
// whether it is usable at all.
//
// It rejects the empty string and anything not absolute after cleaning. Both
// rejections are about argument-position confusion rather than about the
// filesystem. bubblewrap's parser consumes the N operands following an option
// unconditionally, so a leading dash in a path cannot be reinterpreted as a
// flag -- but an EMPTY operand can: `--bind "" ""` makes bwrap resolve the
// current directory, silently binding somewhere nobody named. A relative path
// has the same defect with a different mechanism, resolving against whatever
// directory yanshi happens to be in.
//
// It deliberately does NOT call EvalSymlinks. Symlink resolution happens once,
// up front, in the adapter (via ResolvePath) so that the generator stays a
// pure function; resolving here would make the same input produce different
// argument vectors on different hosts and make the generator's tests depend on
// the filesystem they run on.
func sanitizeBindPath(p string) (string, bool) {
	if strings.TrimSpace(p) == "" {
		return "", false
	}
	clean := filepath.Clean(p)
	// filepath on Windows would accept "C:\x" as absolute; this generator's
	// output is only ever consumed by a Linux bwrap, so the check is explicit
	// about the leading slash rather than delegating to filepath.IsAbs.
	if !strings.HasPrefix(clean, "/") {
		return "", false
	}
	return clean, true
}

// BwrapRestrictsNothing reports whether the rendered plan leaves the
// filesystem entirely unrestricted, so the capability Reason can say so.
//
// Two shapes qualify, and the second is the one worth having a function for:
//
//  1. FullAccess, whose contract is exactly that.
//
//  2. WorkspaceWrite whose WorkspaceRoot resolves to "/". The plan then reads
//     `--ro-bind / /` immediately followed by `--bind / /`, and the later bind
//     wins: every path on the host is writable. Nothing about the argument
//     vector looks alarming, the backend would report os-isolated, and an
//     operator reading "workspace-write" would have precisely the opposite
//     belief from the truth. A workspace root of "/" is not hypothetical --
//     it is what an empty or mis-substituted config value cleans to once a
//     Join is involved.
//
// Note the asymmetry with darwin's ProfileRestrictsNothing: this returns false
// for a network-denied FullAccess plan's PROCESS isolation, because PID
// namespace isolation is a real restriction that survives at every tier. The
// question this answers is specifically about the filesystem.
func BwrapRestrictsNothing(in BwrapInput) bool {
	switch in.Tier {
	case FullAccess:
		return true
	case WorkspaceWrite:
		clean, ok := sanitizeBindPath(in.WorkspaceRoot)
		return ok && clean == "/" && in.exists(clean)
	default:
		return false
	}
}

// BwrapScratchPaths returns the writable directories a WorkspaceWrite child
// needs besides its workspace, on Linux, resolved, existence-filtered and
// deduplicated.
//
// This is the Linux counterpart of ScratchPaths (which is tuned for macOS and
// grants ~/Library/Caches). It is a separate function rather than an extra
// branch in that one because the two lists disagree on almost every entry and
// a combined list would emit binds for paths that cannot exist on the host it
// runs on -- which, for bwrap, is a launch failure rather than a dead rule.
//
// The entries and the failure each one prevents:
//
//   - TMPDIR: honoured by the Go toolchain, cc, and most build systems.
//     Read from the environment because a caller may have already pointed it
//     somewhere specific for this run.
//   - /tmp and /var/tmp: the POSIX locations, used by tools that ignore
//     TMPDIR. /tmp additionally REPLACES the private tmpfs from the base plan
//     when it is granted -- see the boundary note below.
//   - /dev/shm: POSIX shared memory. Its absence is not a compiler failure but
//     a crash: Python multiprocessing, Chromium and anything using
//     sem_open aborts rather than degrading.
//   - XDG_CACHE_HOME, else ~/.cache: where GOCACHE, pip, cargo's registry
//     index and npm's _cacache live on Linux. Without it `go build` fails at
//     "failed to initialize build cache".
//
// Deliberately NOT included, for the same reason ScratchPaths excludes them:
// ~/.cargo, ~/.npm, ~/go/pkg/mod and other package roots are download
// destinations for code fetched from the network. Granting them would make a
// tier whose point is "writes stay in the workspace" also grant write access
// to the place the NEXT build resolves its dependencies from, which is a
// supply-chain write and not a scratch write.
//
// # The honest boundary
//
// These grants are DIRECTORY-WIDE and shared with the host. A WorkspaceWrite
// child can write anywhere under /tmp, /var/tmp, /dev/shm and ~/.cache,
// including over build caches that a LATER UNSANDBOXED build reads back. A
// sandboxed `go build` can therefore leave an object in ~/.cache/go-build that
// a subsequent host build links into a binary. Two concurrent sandboxes are
// likewise not isolated from each other's temp files.
//
// This is a deliberate trade, not an oversight: the alternative -- minting a
// private cache directory per sandbox and overriding GOCACHE/XDG_CACHE_HOME
// for the child -- silently rewrites environment variables the caller set and
// only covers tools that honour them, while costing a cold cache on every
// invocation. TestBwrapScratchGrantIsDirectoryWide pins the current behaviour
// so the boundary cannot move in either direction without someone saying so.
func BwrapScratchPaths() []string {
	return bwrapScratchPathsFrom(os.Getenv, os.UserHomeDir, func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	})
}

// bwrapScratchPathsFrom is the injectable core of BwrapScratchPaths.
//
// Split out so the list can be tested on a machine that has none of these
// paths (every developer machine running this suite is one: no darwin host has
// /dev/shm). Testing the exported function directly would only ever assert
// what the running host happens to contain, which is how a scratch list ends
// up silently empty on the platform it was written for.
func bwrapScratchPathsFrom(
	getenv func(string) string,
	home func() (string, error),
	exists func(string) bool,
) []string {
	candidates := []string{
		getenv("TMPDIR"),
		"/tmp",
		"/var/tmp",
		"/dev/shm",
	}
	if xdg := getenv("XDG_CACHE_HOME"); xdg != "" {
		candidates = append(candidates, xdg)
	} else if h, err := home(); err == nil && h != "" {
		candidates = append(candidates, filepath.Join(h, ".cache"))
	}

	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		clean, ok := sanitizeBindPath(c)
		if !ok || !exists(clean) {
			continue
		}
		out = append(out, clean)
	}
	return dedupeSorted(out)
}
