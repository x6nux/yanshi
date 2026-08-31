//go:build linux
package sandbox

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
)

// encodeRawForTest base64-encodes an arbitrary payload so the decoder can be
// driven with bytes EncodeLandlockRules would never produce. Going through the
// encoder instead would make the malformed-input tests unable to express the
// cases that matter -- a hostile or corrupted token is by definition not
// something the encoder emits.
func encodeRawForTest(t *testing.T, payload string) string {
	t.Helper()
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// This file tests the Landlock rule model, its tier derivation, and the argv
// encoding/decoding contract. All of it is platform-neutral and runs on every
// CI leg -- which matters more here than for the bwrap generator, because the
// syscall half of this backend can only ever be compiled (not run) on the
// developer machines this was written on, so the portable half is where the
// behaviour has to be pinned.

// contains reports whether xs holds want.
func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestBuildLandlockRulesReadOnly pins the read-only policy: read the whole
// filesystem, write only the device nodes.
//
// The device carve-out is not cosmetic. A strictly read-only ruleset denies
// WRITE_FILE everywhere, and `cmd >/dev/null` is a write -- so without it the
// most common shell idiom in existence fails inside the sandbox, as EACCES on
// /dev/null, which sends whoever reads it looking in entirely the wrong place.
func TestBuildLandlockRulesReadOnly(t *testing.T) {
	r := BuildLandlockRules(BwrapInput{Tier: ReadOnly}, allExist)

	if !reflect.DeepEqual(r.ReadPaths, []string{"/"}) {
		t.Errorf("ReadOnly must grant read on /, got %v", r.ReadPaths)
	}
	if len(r.WritePaths) != 0 {
		t.Errorf("ReadOnly must grant no write paths, got %v", r.WritePaths)
	}
	for _, dev := range []string{"/dev/null", "/dev/zero", "/dev/urandom", "/dev/tty"} {
		if !contains(r.DevWritePaths, dev) {
			t.Errorf("ReadOnly must keep %s writable, got %v", dev, r.DevWritePaths)
		}
	}
}

// TestBuildLandlockRulesIgnoresWorkspaceAtReadOnly is the tier-confusion
// guard: a workspace root supplied alongside ReadOnly must not become writable.
func TestBuildLandlockRulesIgnoresWorkspaceAtReadOnly(t *testing.T) {
	r := BuildLandlockRules(BwrapInput{
		Tier:          ReadOnly,
		WorkspaceRoot: "/home/u/ws",
		ScratchPaths:  []string{"/tmp"},
	}, allExist)
	if len(r.WritePaths) != 0 {
		t.Errorf("ReadOnly must ignore WorkspaceRoot/ScratchPaths, got %v", r.WritePaths)
	}
}

// TestBuildLandlockRulesWorkspaceWrite pins the workspace tier, including that
// the workspace comes first (so a nested scratch path layers predictably) and
// that the device writes survive.
func TestBuildLandlockRulesWorkspaceWrite(t *testing.T) {
	r := BuildLandlockRules(BwrapInput{
		Tier:          WorkspaceWrite,
		WorkspaceRoot: "/home/u/ws",
		ScratchPaths:  []string{"/tmp", "/home/u/.cache"},
	}, allExist)

	if !reflect.DeepEqual(r.ReadPaths, []string{"/"}) {
		t.Errorf("WorkspaceWrite must still grant read on /, got %v", r.ReadPaths)
	}
	want := []string{"/home/u/ws", "/tmp", "/home/u/.cache"}
	if !reflect.DeepEqual(r.WritePaths, want) {
		t.Errorf("WritePaths = %v, want %v", r.WritePaths, want)
	}
	if len(r.DevWritePaths) == 0 {
		t.Error("WorkspaceWrite must still grant the device writes")
	}
}

// TestBuildLandlockRulesFullAccess pins the vacuous tier. It must produce
// WritePaths=["/"] and NOT an empty ruleset -- those are opposites in
// Landlock, where an empty ruleset denies everything.
func TestBuildLandlockRulesFullAccess(t *testing.T) {
	r := BuildLandlockRules(BwrapInput{Tier: FullAccess}, allExist)
	if !reflect.DeepEqual(r.WritePaths, []string{"/"}) {
		t.Errorf("FullAccess must grant full rights on /, got %v", r.WritePaths)
	}
	if len(r.ReadPaths) != 0 {
		t.Errorf("FullAccess needs no separate read grants, got %v", r.ReadPaths)
	}
}

// TestBuildLandlockRulesFiltersMissingAndInvalid pins that absent paths and
// non-absolute paths are dropped before they reach the helper, where any
// add_rule failure is fatal.
func TestBuildLandlockRulesFiltersMissingAndInvalid(t *testing.T) {
	r := BuildLandlockRules(BwrapInput{
		Tier:          WorkspaceWrite,
		WorkspaceRoot: "/home/u/ws",
		ScratchPaths:  []string{"/gone", "relative/path", "", "/tmp"},
	}, onlyThese("/home/u/ws", "/tmp"))

	if !reflect.DeepEqual(r.WritePaths, []string{"/home/u/ws", "/tmp"}) {
		t.Errorf("missing and relative paths must be filtered, got %v", r.WritePaths)
	}
}

// TestBuildLandlockRulesDeduplicates pins that the same directory named twice
// (or in two spellings) produces one rule.
func TestBuildLandlockRulesDeduplicates(t *testing.T) {
	r := BuildLandlockRules(BwrapInput{
		Tier:          WorkspaceWrite,
		WorkspaceRoot: "/tmp/ws",
		ScratchPaths:  []string{"/tmp/ws/", "/tmp/./ws", "/tmp/ws"},
	}, allExist)
	if !reflect.DeepEqual(r.WritePaths, []string{"/tmp/ws"}) {
		t.Errorf("duplicate spellings must collapse to one rule, got %v", r.WritePaths)
	}
}

// TestLandlockRestrictsNothing pins the honesty predicate, including the
// distinction that makes it worth having: a ruleset with NO paths is not
// vacuous, it is maximally restrictive. Conflating the two would report a
// deny-everything sandbox as an absent one.
func TestLandlockRestrictsNothing(t *testing.T) {
	if !LandlockRestrictsNothing(LandlockRules{WritePaths: []string{"/"}}) {
		t.Error("full write on / must be reported as restricting nothing")
	}
	if !LandlockRestrictsNothing(LandlockRules{
		WritePaths: []string{"/home/u/ws", "/"},
	}) {
		t.Error("a / entry anywhere in WritePaths makes the policy vacuous")
	}
	if LandlockRestrictsNothing(LandlockRules{}) {
		t.Error("an EMPTY ruleset denies everything and must not be called vacuous")
	}
	if LandlockRestrictsNothing(LandlockRules{
		ReadPaths: []string{"/"}, WritePaths: []string{"/home/u/ws"},
	}) {
		t.Error("a real workspace policy must not be called vacuous")
	}
	// Read-everything is not write-everything.
	if LandlockRestrictsNothing(LandlockRules{ReadPaths: []string{"/"}}) {
		t.Error("read on / still denies all writes and is not vacuous")
	}
}

// TestLandlockVacuityAgreesWithFullAccessTier cross-checks the predicate
// against what the builder actually produces, so the two cannot drift into a
// state where the disclosure describes a policy nobody runs.
func TestLandlockVacuityAgreesWithFullAccessTier(t *testing.T) {
	if !LandlockRestrictsNothing(BuildLandlockRules(BwrapInput{Tier: FullAccess}, allExist)) {
		t.Error("the FullAccess policy the builder emits must be judged vacuous")
	}
	for _, tier := range []AccessTier{ReadOnly, WorkspaceWrite} {
		r := BuildLandlockRules(BwrapInput{
			Tier: tier, WorkspaceRoot: "/home/u/ws",
		}, allExist)
		if LandlockRestrictsNothing(r) {
			t.Errorf("tier %s must not be judged vacuous, got %+v", tier, r)
		}
	}
}

// TestLandlockRulesRoundTrip pins encode/decode fidelity. A silent field loss
// here means the child confines itself to LESS than the parent computed, while
// both halves look correct in isolation.
func TestLandlockRulesRoundTrip(t *testing.T) {
	// The two boolean fields are here deliberately. They are the parent's
	// DECISION about the syscall filter, and a token that lost them would make
	// the child exec without one while the capability report says the filter is
	// installed — the over-claim that is worse than no sandbox, because it is
	// the line an operator trusts.
	in := LandlockRules{
		ReadPaths:     []string{"/"},
		WritePaths:    []string{"/home/u/ws", "/tmp"},
		DevWritePaths: []string{"/dev/null", "/dev/tty"},
		Seccomp:       true,
		NetDeny:       true,
	}
	token, err := EncodeLandlockRules(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := DecodeLandlockRules(token)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip lost data:\n got=%+v\nwant=%+v", out, in)
	}
}

// TestLandlockTokenIsArgvSafe pins that the token carries no character that
// could be reinterpreted by an argv consumer or a shell. This is why the
// encoding is base64-of-JSON and not a delimiter-joined path list: paths may
// legally contain every byte except NUL and '/', including every separator a
// hand-rolled scheme would pick.
func TestLandlockTokenIsArgvSafe(t *testing.T) {
	hostile := LandlockRules{
		WritePaths: []string{
			"/ws/a:b",
			"/ws/c d",
			"/ws/e|f",
			"/ws/g\nh",
			"/ws/i'j",
			`/ws/k"l`,
			"/ws/m$(whoami)",
			"/ws/n;rm -rf",
			"/ws/o--bind",
		},
	}
	token, err := EncodeLandlockRules(hostile)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for _, bad := range []string{" ", "\n", "\t", "'", `"`, "|", ";", "$", "(", ")", ":", "/", "\\", "*", "&", "`"} {
		if strings.Contains(token, bad) {
			t.Errorf("token contains shell/argv-significant %q: %s", bad, token)
		}
	}
	out, err := DecodeLandlockRules(token)
	if err != nil {
		t.Fatalf("decode hostile paths: %v", err)
	}
	if !reflect.DeepEqual(out.WritePaths, hostile.WritePaths) {
		t.Errorf("hostile paths must survive verbatim:\n got=%v\nwant=%v",
			out.WritePaths, hostile.WritePaths)
	}
}

// TestLandlockSeparatorInjectionCannotForgeARule is the specific attack the
// encoding defeats: a workspace name containing whatever separator a simpler
// scheme used must not be able to smuggle an extra grant into the child's
// policy. Here a path literally containing a newline and a second path is
// decoded as ONE path, not two.
func TestLandlockSeparatorInjectionCannotForgeARule(t *testing.T) {
	smuggled := "/home/u/ws\n/etc"
	token, err := EncodeLandlockRules(LandlockRules{WritePaths: []string{smuggled}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := DecodeLandlockRules(token)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.WritePaths) != 1 || out.WritePaths[0] != smuggled {
		t.Errorf("separator injection forged a rule: got %v", out.WritePaths)
	}
}

// TestDecodeLandlockRulesRejectsMalformed pins the helper's only defence. The
// helper grants itself exactly what the token names, so every malformed shape
// must be an ERROR -- never a partially-applied or empty policy, because an
// empty policy that reached applyLandlock would still be applied and an
// error-swallowing decode would exec the target with the wrong confinement.
func TestDecodeLandlockRulesRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"not base64":         "!!!not-base64!!!",
		"base64 of garbage":  encodeRawForTest(t, "this is not json"),
		"relative path":      encodeRawForTest(t, `{"w":["relative/ws"]}`),
		"empty path":         encodeRawForTest(t, `{"w":[""]}`),
		"uncleaned path":     encodeRawForTest(t, `{"w":["/ws/../etc"]}`),
		"trailing slash":     encodeRawForTest(t, `{"w":["/ws/"]}`),
		"dot segment":        encodeRawForTest(t, `{"w":["/ws/./x"]}`),
		"unknown field":      encodeRawForTest(t, `{"w":["/ws"],"evil":["/etc"]}`),
		"relative read path": encodeRawForTest(t, `{"r":["etc"]}`),
		"relative dev path":  encodeRawForTest(t, `{"d":["dev/null"]}`),
	}
	for name, token := range cases {
		got, err := DecodeLandlockRules(token)
		if err == nil {
			t.Errorf("%s: expected an error, got rules %+v", name, got)
		}
		if len(got.ReadPaths)+len(got.WritePaths)+len(got.DevWritePaths) != 0 {
			t.Errorf("%s: a rejected token must yield NO rules, got %+v", name, got)
		}
	}
}

// TestDecodeLandlockRulesRejectsTraversal is called out separately from the
// table because it is the one that would grant rights somewhere other than
// where the token reads as granting them: "/home/u/ws/../../etc" looks like a
// workspace grant to anyone auditing the process list and is /etc to the
// kernel. Rejecting rather than cleaning is deliberate -- cleaning here would
// let the parent's computed policy and the child's applied policy differ while
// both look right on their own.
func TestDecodeLandlockRulesRejectsTraversal(t *testing.T) {
	token := encodeRawForTest(t, `{"w":["/home/u/ws/../../etc"]}`)
	if _, err := DecodeLandlockRules(token); err == nil {
		t.Fatal("a traversal path must be rejected, not silently cleaned")
	}
	// And the cleaned form is accepted, so the rejection is about the FORM and
	// not about /etc being special.
	ok := encodeRawForTest(t, `{"w":["/etc"]}`)
	if _, err := DecodeLandlockRules(ok); err != nil {
		t.Fatalf("the cleaned equivalent must decode: %v", err)
	}
}

// TestSplitLandlockHelperArgs pins the argv grammar shared by the parent that
// builds it and the child that parses it.
func TestSplitLandlockHelperArgs(t *testing.T) {
	token, program, args, err := SplitLandlockHelperArgs([]string{
		"/usr/bin/yanshi", landlockHelperArg, "TOKEN", "--", "/bin/echo", "hi", "there",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "TOKEN" {
		t.Errorf("token = %q", token)
	}
	if program != "/bin/echo" {
		t.Errorf("program = %q", program)
	}
	if !reflect.DeepEqual(args, []string{"/bin/echo", "hi", "there"}) {
		t.Errorf("args = %v", args)
	}
}

// TestSplitLandlockHelperArgsPreservesInnerSeparators pins that only the FIRST
// `--` is the boundary. A target program legitimately taking its own `--`
// (git, go test, sh -c) must receive it verbatim.
func TestSplitLandlockHelperArgsPreservesInnerSeparators(t *testing.T) {
	_, program, args, err := SplitLandlockHelperArgs([]string{
		"/usr/bin/yanshi", landlockHelperArg, "T", "--", "/usr/bin/git", "diff", "--", "path",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if program != "/usr/bin/git" {
		t.Errorf("program = %q", program)
	}
	if !reflect.DeepEqual(args, []string{"/usr/bin/git", "diff", "--", "path"}) {
		t.Errorf("inner -- must survive, got %v", args)
	}
}

// TestSplitLandlockHelperArgsRejectsMalformed pins that every malformed argv
// is an error. The helper must never fall through to an exec on a shape it did
// not fully understand -- that would run the target unconfined while the
// parent's report says os-isolated.
func TestSplitLandlockHelperArgsRejectsMalformed(t *testing.T) {
	cases := map[string][]string{
		"empty":               {},
		"no subcommand":       {"/usr/bin/yanshi"},
		"wrong subcommand":    {"/usr/bin/yanshi", "serve", "T", "--", "/bin/true"},
		"no token":            {"/usr/bin/yanshi", landlockHelperArg},
		"no separator":        {"/usr/bin/yanshi", landlockHelperArg, "T", "/bin/true"},
		"no program after --": {"/usr/bin/yanshi", landlockHelperArg, "T", "--"},
	}
	for name, argv := range cases {
		if _, _, _, err := SplitLandlockHelperArgs(argv); err == nil {
			t.Errorf("%s: expected an error for argv %v", name, argv)
		}
	}
}

// TestLandlockHelperArgIsHidden pins the two properties that keep the helper
// out of the operator-facing surface.
//
// cmd/gendocs' TestSubcommandListMatchesDispatch scans main.go for
// `case "<lowercase-name>":` and requires every match to be a documented
// subcommand. A token starting with an underscore cannot match that regex, so
// dispatching on it neither breaks that test nor enlarges the documented
// surface -- and the underscore also marks it, in a process listing, as an
// internal re-exec rather than something to type.
func TestLandlockHelperArgIsHidden(t *testing.T) {
	arg := LandlockHelperArg()
	if arg != landlockHelperArg {
		t.Fatalf("exported accessor disagrees with the constant: %q vs %q", arg, landlockHelperArg)
	}
	if !strings.HasPrefix(arg, "_") {
		t.Errorf("helper arg %q must start with _ so gendocs' `case \"[a-z]...\"` "+
			"regex cannot match it and it reads as internal", arg)
	}
	if strings.ContainsAny(arg, " \t\n\"'") {
		t.Errorf("helper arg %q must be a single plain token", arg)
	}
}

// TestValidateLandlockPath pins the absolute-and-clean invariant directly.
func TestValidateLandlockPath(t *testing.T) {
	good := []string{"/", "/tmp", "/home/u/ws", "/dev/null", "/a b/c"}
	for _, p := range good {
		if err := validateLandlockPath(p); err != nil {
			t.Errorf("validateLandlockPath(%q) = %v, want nil", p, err)
		}
	}
	bad := []string{"", "rel", "./rel", "../up", "/tmp/", "/tmp/./x", "/tmp/../x", "  "}
	for _, p := range bad {
		if err := validateLandlockPath(p); err == nil {
			t.Errorf("validateLandlockPath(%q) = nil, want an error", p)
		}
	}
}
