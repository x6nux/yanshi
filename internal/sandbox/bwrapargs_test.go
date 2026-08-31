//go:build !windows
package sandbox

import (
	"reflect"
	"strings"
	"testing"
)

// This file tests the bubblewrap argument generator. It is platform-neutral
// and runs on every CI leg, which is the point: the generator was written on
// darwin, where no bwrap exists, so if these tests needed Linux the entire
// mount plan would have shipped unexercised.
//
// The existence predicate is injected everywhere so the assertions describe
// the GENERATOR and not the filesystem of whatever host runs them.

// allExist is an existence predicate that accepts everything, for cases where
// path presence is not what is under test.
func allExist(string) bool { return true }

// onlyThese returns an existence predicate accepting exactly the given paths.
func onlyThese(paths ...string) func(string) bool {
	set := map[string]bool{}
	for _, p := range paths {
		set[p] = true
	}
	return func(p string) bool { return set[p] }
}

// argIndex returns the position of the first occurrence of want in args, or -1.
func argIndex(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// hasPair reports whether args contains flag followed by the two operands
// src and dst in consecutive positions.
func hasPair(args []string, flag, src, dst string) bool {
	for i := 0; i+2 < len(args); i++ {
		if args[i] == flag && args[i+1] == src && args[i+2] == dst {
			return true
		}
	}
	return false
}

// hasSingle reports whether args contains flag immediately followed by operand.
func hasSingle(args []string, flag, operand string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == operand {
			return true
		}
	}
	return false
}

// TestBwrapReadOnlyPlan pins the read-only mount plan. Every element is
// asserted because each one prevents a specific escape: the ro-bind is the
// base, the tmpfs keeps a "read-only" command from leaving files in the shared
// /tmp, --dev hides raw block devices the ro-bind would otherwise expose, and
// --unshare-all is what makes namespace isolation the default.
func TestBwrapReadOnlyPlan(t *testing.T) {
	args := BuildBwrapArgs(BwrapInput{Tier: ReadOnly, Exists: allExist})

	if !hasPair(args, "--ro-bind", "/", "/") {
		t.Errorf("read-only plan must bind / read-only, got %v", args)
	}
	if !hasSingle(args, "--tmpfs", "/tmp") {
		t.Errorf("read-only plan must give a private /tmp, got %v", args)
	}
	if !hasSingle(args, "--dev", "/dev") {
		t.Errorf("read-only plan must install a synthetic /dev, got %v", args)
	}
	if !hasSingle(args, "--proc", "/proc") {
		t.Errorf("read-only plan must install a fresh /proc, got %v", args)
	}
	if argIndex(args, "--unshare-all") < 0 {
		t.Errorf("read-only plan must unshare all namespaces, got %v", args)
	}
	// No --bind anywhere: a single writable bind would silently defeat the tier.
	if argIndex(args, "--bind") >= 0 {
		t.Errorf("read-only plan must contain no writable bind, got %v", args)
	}
}

// TestBwrapReadOnlyIgnoresWorkspace is the tier-confusion guard. A caller that
// passes a workspace root while asking for ReadOnly must not get it mounted
// writable -- that would make the tier depend on a field the tier ignores, and
// it is the exact shape of bug where "read-only" quietly means "writable
// because the config had a workspace set".
func TestBwrapReadOnlyIgnoresWorkspace(t *testing.T) {
	args := BuildBwrapArgs(BwrapInput{
		Tier:          ReadOnly,
		WorkspaceRoot: "/home/u/ws",
		ScratchPaths:  []string{"/tmp", "/home/u/.cache"},
		Exists:        allExist,
	})
	for _, a := range args {
		if a == "--bind" {
			t.Fatalf("ReadOnly must ignore WorkspaceRoot and ScratchPaths, got %v", args)
		}
	}
}

// TestBwrapWorkspaceWritePlan checks the workspace and scratch binds are
// emitted, ordered after the base ro-bind so they layer on top of it. Order is
// asserted rather than mere membership because bwrap applies operations left
// to right: a --bind emitted BEFORE the --ro-bind / / would be overwritten by
// it and the workspace would be silently read-only.
func TestBwrapWorkspaceWritePlan(t *testing.T) {
	args := BuildBwrapArgs(BwrapInput{
		Tier:          WorkspaceWrite,
		WorkspaceRoot: "/home/u/ws",
		ScratchPaths:  []string{"/tmp", "/home/u/.cache"},
		Exists:        allExist,
	})

	if !hasPair(args, "--bind", "/home/u/ws", "/home/u/ws") {
		t.Errorf("workspace must be bound read-write, got %v", args)
	}
	if !hasPair(args, "--bind", "/home/u/.cache", "/home/u/.cache") {
		t.Errorf("scratch path must be bound read-write, got %v", args)
	}

	roIdx := argIndex(args, "--ro-bind")
	bindIdx := argIndex(args, "--bind")
	if roIdx < 0 || bindIdx < 0 || roIdx > bindIdx {
		t.Errorf("writable binds must come after the base --ro-bind (ro=%d bind=%d): %v",
			roIdx, bindIdx, args)
	}

	// The workspace bind must precede the scratch binds so a scratch entry
	// nested inside the workspace layers over it rather than the reverse.
	wsIdx := -1
	cacheIdx := -1
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--bind" && args[i+1] == "/home/u/ws" {
			wsIdx = i
		}
		if args[i] == "--bind" && args[i+1] == "/home/u/.cache" {
			cacheIdx = i
		}
	}
	if wsIdx < 0 || cacheIdx < 0 || wsIdx > cacheIdx {
		t.Errorf("workspace bind must precede scratch binds (ws=%d cache=%d): %v",
			wsIdx, cacheIdx, args)
	}
}

// TestBwrapSkipsMissingBindSources is the denial-of-service guard, and it is
// the reason the existence predicate exists at all. bwrap exits 1 with "Can't
// find source path" when a bind source is absent, so emitting an unconditional
// scratch list would turn "this host has no ~/.cache" into "every sandboxed
// command fails" -- an outage produced by the security layer.
func TestBwrapSkipsMissingBindSources(t *testing.T) {
	args := BuildBwrapArgs(BwrapInput{
		Tier:          WorkspaceWrite,
		WorkspaceRoot: "/home/u/ws",
		ScratchPaths:  []string{"/tmp", "/does/not/exist", "/dev/shm"},
		Exists:        onlyThese("/home/u/ws", "/tmp"),
	})
	for _, missing := range []string{"/does/not/exist", "/dev/shm"} {
		if argIndex(args, missing) >= 0 {
			t.Errorf("must not bind missing path %q, got %v", missing, args)
		}
	}
	if !hasPair(args, "--bind", "/tmp", "/tmp") {
		t.Errorf("existing scratch path must still be bound, got %v", args)
	}
}

// TestBwrapDeduplicatesBinds guards against a workspace that also appears in
// the scratch list. A duplicated --bind is not a security hole but it does
// make the argv unreadable, and dedup keeps the plan a function of the SET of
// paths rather than of how many times a caller happened to list one.
func TestBwrapDeduplicatesBinds(t *testing.T) {
	args := BuildBwrapArgs(BwrapInput{
		Tier:          WorkspaceWrite,
		WorkspaceRoot: "/tmp/ws",
		ScratchPaths:  []string{"/tmp/ws", "/tmp/ws/", "/tmp/./ws"},
		Exists:        allExist,
	})
	n := 0
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--bind" && args[i+1] == "/tmp/ws" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected exactly one bind of /tmp/ws, got %d: %v", n, args)
	}
}

// TestBwrapNetworkPolicy pins both directions of the network decision. Both
// are asserted because the deny path is expressed by the ABSENCE of
// --share-net, and an absence is exactly the kind of assertion that silently
// stops holding when the flag name changes.
func TestBwrapNetworkPolicy(t *testing.T) {
	denied := BuildBwrapArgs(BwrapInput{Tier: ReadOnly, NetworkDeny: true, Exists: allExist})
	if argIndex(denied, "--share-net") >= 0 {
		t.Errorf("NetworkDeny must not share the net namespace, got %v", denied)
	}
	if argIndex(denied, "--unshare-all") < 0 {
		t.Errorf("NetworkDeny still requires --unshare-all, got %v", denied)
	}

	allowed := BuildBwrapArgs(BwrapInput{Tier: ReadOnly, NetworkDeny: false, Exists: allExist})
	if argIndex(allowed, "--share-net") < 0 {
		t.Errorf("network-permitted plan must re-share the net namespace, got %v", allowed)
	}
	// --share-net must come after --unshare-all or it is undone by it.
	if argIndex(allowed, "--unshare-all") > argIndex(allowed, "--share-net") {
		t.Errorf("--share-net must follow --unshare-all, got %v", allowed)
	}
}

// TestBwrapFullAccessIsProcessIsolationOnly pins that FullAccess restricts no
// files but still unshares pid. The --bind / / assertion is the load-bearing
// one: bubblewrap starts from an EMPTY namespace, so omitting it would leave
// the tier named "full-access" as the only one that cannot exec anything.
func TestBwrapFullAccessIsProcessIsolationOnly(t *testing.T) {
	args := BuildBwrapArgs(BwrapInput{Tier: FullAccess, Exists: allExist})

	if !hasPair(args, "--bind", "/", "/") {
		t.Errorf("FullAccess must bind / read-write, got %v", args)
	}
	if argIndex(args, "--ro-bind") >= 0 {
		t.Errorf("FullAccess must not bind anything read-only, got %v", args)
	}
	if argIndex(args, "--tmpfs") >= 0 {
		t.Errorf("FullAccess must not shadow /tmp, got %v", args)
	}
	if argIndex(args, "--dev") >= 0 {
		t.Errorf("FullAccess must not narrow /dev, got %v", args)
	}
	if argIndex(args, "--unshare-pid") < 0 {
		t.Errorf("FullAccess must still isolate pids, got %v", args)
	}
	if argIndex(args, "--unshare-all") >= 0 {
		t.Errorf("FullAccess must not unshare everything, got %v", args)
	}
}

// TestBwrapFullAccessNetworkDeny pins that the network policy is honoured even
// at the tier that restricts no files -- NetworkDeny is orthogonal to the file
// tier and a reader could reasonably assume full-access means full network.
func TestBwrapFullAccessNetworkDeny(t *testing.T) {
	args := BuildBwrapArgs(BwrapInput{Tier: FullAccess, NetworkDeny: true, Exists: allExist})
	if argIndex(args, "--unshare-net") < 0 {
		t.Errorf("FullAccess with NetworkDeny must unshare net, got %v", args)
	}
	open := BuildBwrapArgs(BwrapInput{Tier: FullAccess, NetworkDeny: false, Exists: allExist})
	if argIndex(open, "--unshare-net") >= 0 {
		t.Errorf("FullAccess without NetworkDeny must keep the net namespace, got %v", open)
	}
}

// TestBwrapAlwaysHardensSession pins --new-session and --die-with-parent at
// every tier. --new-session closes the TIOCSTI terminal-injection class, in
// which a sandboxed child pushes characters into the operator's terminal input
// queue and the shell runs them after the sandbox exits -- an escape that
// needs no filesystem access at all, which is why it must hold even at
// FullAccess.
func TestBwrapAlwaysHardensSession(t *testing.T) {
	for _, tier := range []AccessTier{ReadOnly, WorkspaceWrite, FullAccess} {
		args := BuildBwrapArgs(BwrapInput{Tier: tier, Exists: allExist})
		if argIndex(args, "--new-session") != 0 {
			t.Errorf("tier %s: --new-session must lead the plan, got %v", tier, args)
		}
		if argIndex(args, "--die-with-parent") < 0 {
			t.Errorf("tier %s: --die-with-parent missing, got %v", tier, args)
		}
	}
}

// TestBwrapRejectsRelativeAndEmptyPaths is the injection guard. bubblewrap
// consumes a fixed operand count per option, so a leading dash cannot become a
// flag -- but an EMPTY operand makes bwrap resolve the current directory,
// binding somewhere nobody named, and a relative one resolves against whatever
// directory yanshi happens to be in.
func TestBwrapRejectsRelativeAndEmptyPaths(t *testing.T) {
	args := BuildBwrapArgs(BwrapInput{
		Tier:          WorkspaceWrite,
		WorkspaceRoot: "relative/ws",
		ScratchPaths:  []string{"", "   ", "./also-relative", "../escape"},
		Exists:        allExist,
	})
	if argIndex(args, "--bind") >= 0 {
		t.Errorf("no relative or empty path may become a bind, got %v", args)
	}
}

// TestBwrapPathTraversalIsNormalised pins that a traversal sequence is cleaned
// to the path it actually denotes before becoming an operand. Without this,
// the argv would read "--bind /home/u/ws/../../etc" -- an operator auditing
// the process list would see a workspace bind while the kernel bound /etc.
func TestBwrapPathTraversalIsNormalised(t *testing.T) {
	args := BuildBwrapArgs(BwrapInput{
		Tier:          WorkspaceWrite,
		WorkspaceRoot: "/home/u/ws/../../../etc",
		Exists:        allExist,
	})
	if !hasPair(args, "--bind", "/etc", "/etc") {
		t.Errorf("traversal must be cleaned to its real target, got %v", args)
	}
	for _, a := range args {
		if strings.Contains(a, "..") {
			t.Errorf("no operand may retain a traversal sequence, got %v", args)
		}
	}
}

// TestBwrapChdir pins the working-directory handling, including the
// /tmp exception. A --chdir into the private tmpfs would fail the entire
// launch, because bwrap enters the directory after installing the tmpfs that
// emptied it -- and the resulting error names the directory, not the tmpfs, so
// nobody would connect the failure to the sandbox's own /tmp policy.
func TestBwrapChdir(t *testing.T) {
	args := BuildBwrapArgs(BwrapInput{
		Tier: ReadOnly, Chdir: "/home/u/ws", Exists: allExist,
	})
	if !hasSingle(args, "--chdir", "/home/u/ws") {
		t.Errorf("existing chdir must be emitted, got %v", args)
	}

	missing := BuildBwrapArgs(BwrapInput{
		Tier: ReadOnly, Chdir: "/nope", Exists: onlyThese(),
	})
	if argIndex(missing, "--chdir") >= 0 {
		t.Errorf("missing chdir must be skipped rather than fail the launch, got %v", missing)
	}

	underTmp := BuildBwrapArgs(BwrapInput{
		Tier: WorkspaceWrite, Chdir: "/tmp/build", Exists: allExist,
	})
	if argIndex(underTmp, "--chdir") >= 0 {
		t.Errorf("chdir under the private tmpfs must be skipped, got %v", underTmp)
	}

	// FullAccess has no private tmpfs, so /tmp is the host's and chdir is fine.
	fullTmp := BuildBwrapArgs(BwrapInput{
		Tier: FullAccess, Chdir: "/tmp/build", Exists: allExist,
	})
	if !hasSingle(fullTmp, "--chdir", "/tmp/build") {
		t.Errorf("FullAccess chdir under /tmp must be emitted, got %v", fullTmp)
	}
}

// TestUnderPrivateTmpIsComponentWise pins that /tmp containment is a path
// component test and not a string prefix. "/tmpfoo" begins with "/tmp" but is
// a sibling, and treating it as contained would silently drop a legitimate
// --chdir -- the same distinction sbpl.go documents for (subpath ...).
func TestUnderPrivateTmpIsComponentWise(t *testing.T) {
	cases := map[string]bool{
		"/tmp":         true,
		"/tmp/x":       true,
		"/tmp/x/y":     true,
		"/tmpfoo":      false,
		"/tmpfoo/bar":  false,
		"/var/tmp":     false,
		"/home/u/tmp":  false,
		"/":            false,
		"/tmpfoo/tmp":  false,
		"/var/tmp/tmp": false,
	}
	for path, want := range cases {
		if got := underPrivateTmp(path); got != want {
			t.Errorf("underPrivateTmp(%q) = %t, want %t", path, got, want)
		}
	}
}

// TestBwrapRestrictsNothingVacuity pins the honesty check, and the
// workspace-root-of-"/" case is the one that matters. That plan renders
// `--ro-bind / /` immediately followed by `--bind / /`; the later bind wins,
// so every path on the host is writable while the argv looks unremarkable and
// the tier still reads "workspace-write".
func TestBwrapRestrictsNothingVacuity(t *testing.T) {
	if !BwrapRestrictsNothing(BwrapInput{Tier: FullAccess, Exists: allExist}) {
		t.Error("FullAccess must be reported as restricting nothing")
	}
	if !BwrapRestrictsNothing(BwrapInput{
		Tier: WorkspaceWrite, WorkspaceRoot: "/", Exists: allExist,
	}) {
		t.Error("WorkspaceWrite rooted at / must be reported as restricting nothing")
	}
	// The traversal form cleans to "/" and must be caught the same way.
	if !BwrapRestrictsNothing(BwrapInput{
		Tier: WorkspaceWrite, WorkspaceRoot: "/home/../..", Exists: allExist,
	}) {
		t.Error("a workspace root that CLEANS to / must be reported as vacuous")
	}
	if BwrapRestrictsNothing(BwrapInput{
		Tier: WorkspaceWrite, WorkspaceRoot: "/home/u/ws", Exists: allExist,
	}) {
		t.Error("a real workspace root must not be reported as vacuous")
	}
	if BwrapRestrictsNothing(BwrapInput{Tier: ReadOnly, Exists: allExist}) {
		t.Error("ReadOnly restricts plenty and must not be reported as vacuous")
	}
	// A "/" workspace that does not exist cannot be bound, so the plan is not
	// actually vacuous -- the ro-bind stands.
	if BwrapRestrictsNothing(BwrapInput{
		Tier: WorkspaceWrite, WorkspaceRoot: "/", Exists: onlyThese(),
	}) {
		t.Error("a non-existent / workspace cannot be bound and is not vacuous")
	}
}

// TestBwrapVacuousPlanReallyIsVacuous is the cross-check that the vacuity
// PREDICATE agrees with the ARGUMENTS the generator emits. Without it the two
// could drift: BwrapRestrictsNothing could keep returning true for a shape the
// generator no longer renders that way, and the disclosure in the capability
// Reason would describe a plan nobody runs.
func TestBwrapVacuousPlanReallyIsVacuous(t *testing.T) {
	in := BwrapInput{Tier: WorkspaceWrite, WorkspaceRoot: "/", Exists: allExist}
	if !BwrapRestrictsNothing(in) {
		t.Fatal("precondition: this input must be judged vacuous")
	}
	args := BuildBwrapArgs(in)
	if !hasPair(args, "--bind", "/", "/") {
		t.Fatalf("a plan judged vacuous must actually bind / read-write, got %v", args)
	}
	ro := argIndex(args, "--ro-bind")
	rw := -1
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--bind" && args[i+1] == "/" && args[i+2] == "/" {
			rw = i
		}
	}
	if ro < 0 || rw < 0 || rw < ro {
		t.Errorf("the read-write bind of / must come last to actually win (ro=%d rw=%d): %v",
			ro, rw, args)
	}
}

// TestBwrapScratchPathsFromDerivation drives the scratch list through its
// injected seams. Testing the exported BwrapScratchPaths directly would only
// assert what the running host contains -- and no darwin host has /dev/shm, so
// the entry most likely to be dropped by a refactor is the one a
// darwin-only test could never see.
func TestBwrapScratchPathsFromDerivation(t *testing.T) {
	env := map[string]string{"TMPDIR": "/custom/tmp"}
	got := bwrapScratchPathsFrom(
		func(k string) string { return env[k] },
		func() (string, error) { return "/home/u", nil },
		allExist,
	)
	want := []string{"/custom/tmp", "/dev/shm", "/home/u/.cache", "/tmp", "/var/tmp"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scratch list = %v, want %v", got, want)
	}
}

// TestBwrapScratchPathsPrefersXDG pins that XDG_CACHE_HOME wins over ~/.cache
// and that the home fallback is not ALSO added -- granting both would widen
// the sandbox past what the operator's environment asked for.
func TestBwrapScratchPathsPrefersXDG(t *testing.T) {
	env := map[string]string{"XDG_CACHE_HOME": "/xdg/cache"}
	got := bwrapScratchPathsFrom(
		func(k string) string { return env[k] },
		func() (string, error) { return "/home/u", nil },
		allExist,
	)
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "/xdg/cache") {
		t.Errorf("XDG_CACHE_HOME must be granted, got %v", got)
	}
	if strings.Contains(joined, "/home/u/.cache") {
		t.Errorf("~/.cache must not be granted when XDG_CACHE_HOME is set, got %v", got)
	}
}

// TestBwrapScratchPathsFiltersMissing pins that a host lacking /dev/shm or a
// cache directory yields a shorter list rather than a plan bwrap refuses.
func TestBwrapScratchPathsFiltersMissing(t *testing.T) {
	got := bwrapScratchPathsFrom(
		func(string) string { return "" },
		func() (string, error) { return "/home/u", nil },
		onlyThese("/tmp"),
	)
	if !reflect.DeepEqual(got, []string{"/tmp"}) {
		t.Errorf("only existing paths may be listed, got %v", got)
	}
}

// TestBwrapScratchPathsExcludesPackageRoots pins the supply-chain boundary. A
// tier whose contract is "writes stay in the workspace" must not also grant
// write access to the directories the NEXT build resolves its dependencies
// from -- that is a supply-chain write, not a scratch write, and it is the
// difference between a poisoned build cache (recoverable, local) and a
// poisoned module cache (a dependency substituted for every later build).
func TestBwrapScratchPathsExcludesPackageRoots(t *testing.T) {
	got := bwrapScratchPathsFrom(
		func(string) string { return "" },
		func() (string, error) { return "/home/u", nil },
		allExist,
	)
	forbidden := []string{
		"/home/u/.cargo", "/home/u/.npm", "/home/u/go/pkg/mod",
		"/home/u/.rustup", "/home/u/.m2", "/usr/local/lib",
	}
	for _, f := range forbidden {
		for _, g := range got {
			if g == f {
				t.Errorf("package root %q must not be granted, got %v", f, got)
			}
		}
	}
}

// TestBwrapScratchGrantIsDirectoryWide is the boundary pin, and it asserts a
// LIMITATION rather than a protection -- deliberately, because this is the
// property most likely to be quietly assumed away.
//
// The scratch grants are whole directories shared with the host. A sandboxed
// child can write anywhere under /tmp, /var/tmp, /dev/shm and ~/.cache,
// INCLUDING over a build cache that a later UNSANDBOXED build reads back. So a
// sandboxed `go build` can leave an object in ~/.cache/go-build that a
// subsequent host build links into a binary, and two concurrent sandboxes are
// not isolated from each other's temp files.
//
// This test exists so that boundary cannot move in either direction without
// someone saying so: narrowing it to per-sandbox private directories is a
// legitimate change, but it must be a deliberate one with its own tests, not
// a silent side effect.
func TestBwrapScratchGrantIsDirectoryWide(t *testing.T) {
	scratch := bwrapScratchPathsFrom(
		func(string) string { return "" },
		func() (string, error) { return "/home/u", nil },
		allExist,
	)
	args := BuildBwrapArgs(BwrapInput{
		Tier:          WorkspaceWrite,
		WorkspaceRoot: "/home/u/ws",
		ScratchPaths:  scratch,
		Exists:        allExist,
	})

	// Each scratch entry is bound as a whole directory, not as a subpath of a
	// per-sandbox private root.
	for _, p := range scratch {
		if !hasPair(args, "--bind", p, p) {
			t.Errorf("scratch %q must be bound whole (current, documented behaviour), got %v", p, args)
		}
	}

	// The concrete consequence, pinned: a shared cache directory is writable,
	// so a path a later host build reads is reachable by the sandboxed child.
	if !hasPair(args, "--bind", "/home/u/.cache", "/home/u/.cache") {
		t.Error("the shared cache root is writable; this is the documented boundary " +
			"(a sandboxed child can poison a cache a later unsandboxed build reads)")
	}
	// And nothing in the plan redirects the child to a private cache.
	for _, a := range args {
		if strings.Contains(a, "yanshi-sandbox-private") {
			t.Error("plan appears to mint a private scratch root; the documented " +
				"boundary changed and BwrapScratchPaths' comment plus this test must be updated")
		}
	}
}
