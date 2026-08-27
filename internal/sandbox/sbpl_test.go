package sandbox

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildProfileTierShapes is the table over the three access tiers: each
// one's defining rule must be present and each other tier's must be absent.
// It runs on every platform because BuildProfile is a pure function — the
// text is wrong or right regardless of whether a kernel is available to
// consume it.
func TestBuildProfileTierShapes(t *testing.T) {
	ws := "/private/tmp/ws"
	cases := []struct {
		name    string
		input   ProfileInput
		want    []string
		notWant []string
	}{
		{
			name:  "read-only denies all writes and grants no subpath",
			input: ProfileInput{Tier: ReadOnly, WorkspaceRoot: ws, WritablePaths: []string{"/private/tmp/scratch"}},
			want: []string{
				"(version 1)",
				"(deny default)",
				"(allow file-read*)",
				"(deny file-write*)",
				"(allow process-exec*)",
			},
			notWant: []string{
				`(subpath "/private/tmp/ws")`,
				`(subpath "/private/tmp/scratch")`,
				"(allow default)",
			},
		},
		{
			name:  "workspace-write carves out workspace and scratch",
			input: ProfileInput{Tier: WorkspaceWrite, WorkspaceRoot: ws, WritablePaths: []string{"/private/tmp/scratch"}},
			want: []string{
				"(deny default)",
				"(deny file-write*)",
				"(allow file-write*\n",
				`(subpath "/private/tmp/ws")`,
				`(subpath "/private/tmp/scratch")`,
			},
			notWant: []string{"(allow default)"},
		},
		{
			name:  "full-access imposes no filesystem restriction",
			input: ProfileInput{Tier: FullAccess, WorkspaceRoot: ws},
			want:  []string{"(version 1)", "(allow default)"},
			notWant: []string{
				"(deny default)",
				"(deny file-write*)",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildProfile(tc.input)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("profile missing %q\n--- profile ---\n%s", w, got)
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(got, nw) {
					t.Errorf("profile must not contain %q\n--- profile ---\n%s", nw, got)
				}
			}
		})
	}
}

// TestBuildProfileNetworkPosture covers the four combinations of NetworkDeny
// and AllowLoopback. The unix-socket carve-out is asserted alongside the
// denial because (deny network*) also denies AF_UNIX — a fact measured against
// the real sandbox, and the reason a plain denial would break local IPC that
// has nothing to do with egress.
func TestBuildProfileNetworkPosture(t *testing.T) {
	cases := []struct {
		name    string
		input   ProfileInput
		want    []string
		notWant []string
	}{
		{
			name:    "network allowed emits an explicit allow, not silence",
			input:   ProfileInput{Tier: ReadOnly, NetworkDeny: false},
			want:    []string{"(allow network*)"},
			notWant: []string{"(deny network*)", "network-outbound", "localhost"},
		},
		{
			name:    "full-access needs no explicit network allow",
			input:   ProfileInput{Tier: FullAccess, NetworkDeny: false},
			want:    []string{"(allow default)"},
			notWant: []string{"(deny network*)", "(allow network*)"},
		},
		{
			name:  "network denied keeps unix sockets usable",
			input: ProfileInput{Tier: ReadOnly, NetworkDeny: true},
			want: []string{
				"(deny network*)",
				"(allow network-outbound (remote unix-socket))",
				"(allow network-bind (local unix-socket))",
			},
			notWant: []string{"localhost"},
		},
		{
			name:  "loopback carve-out names both local and remote",
			input: ProfileInput{Tier: ReadOnly, NetworkDeny: true, AllowLoopback: true},
			want: []string{
				"(deny network*)",
				`(allow network* (local ip "localhost:*"))`,
				`(allow network* (remote ip "localhost:*"))`,
			},
		},
		{
			name:    "loopback without denial is inert",
			input:   ProfileInput{Tier: ReadOnly, NetworkDeny: false, AllowLoopback: true},
			want:    []string{"(allow network*)"},
			notWant: []string{"localhost", "(deny network*)"},
		},
		{
			name:  "full-access still honours network denial",
			input: ProfileInput{Tier: FullAccess, NetworkDeny: true},
			want:  []string{"(allow default)", "(deny network*)"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildProfile(tc.input)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("profile missing %q\n--- profile ---\n%s", w, got)
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(got, nw) {
					t.Errorf("profile must not contain %q\n--- profile ---\n%s", nw, got)
				}
			}
		})
	}
}

// TestBuildProfileDenyPathsApplyAtEveryTier pins that DenyPaths override even
// FullAccess's (allow default). A credential deny-list that evaporated at the
// most permissive tier would be worse than none, because the operator who
// configured it would believe it applied everywhere.
func TestBuildProfileDenyPathsApplyAtEveryTier(t *testing.T) {
	secret := "/Users/someone/.ssh"
	for _, tier := range []AccessTier{ReadOnly, WorkspaceWrite, FullAccess} {
		t.Run(tier.String(), func(t *testing.T) {
			got := BuildProfile(ProfileInput{Tier: tier, DenyPaths: []string{secret}})
			for _, want := range []string{"(deny file-read*\n", "(deny file-write*\n", `(subpath "/Users/someone/.ssh")`} {
				if !strings.Contains(got, want) {
					t.Errorf("tier %s: profile missing %q\n--- profile ---\n%s", tier, want, got)
				}
			}
		})
	}
}

// TestQuoteSBPLEscapesInjection is the attacking test for the profile's only
// injection surface. Each payload is a path that, interpolated raw, changes
// the MEANING of the profile rather than merely breaking its syntax.
//
// The `")) (allow file-write*) ;` shape is not hypothetical: rendered raw into
// a workspace rule and handed to the real /usr/bin/sandbox-exec, it produced a
// profile that compiled and a subsequent write outside the workspace that
// succeeded with exit 0. The trailing `;` comments out the wreckage, which is
// what makes it an escape rather than a parse error.
func TestQuoteSBPLEscapesInjection(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		want    string
		comment string
	}{
		{
			name: "plain path is unchanged",
			path: "/private/tmp/ws",
			want: `"/private/tmp/ws"`,
		},
		{
			name: "space needs no escape but must stay inside the literal",
			path: "/Users/a b/My Project",
			want: `"/Users/a b/My Project"`,
		},
		{
			name: "double quote is escaped",
			path: `/tmp/a"b`,
			want: `"/tmp/a\"b"`,
		},
		{
			name: "backslash is escaped before the quote pass doubles it",
			path: `/tmp/a\b`,
			want: `"/tmp/a\\b"`,
		},
		{
			name:    "quote-paren-rule escape attempt is neutralised",
			path:    `/tmp/ws")) (allow file-write*) ;`,
			want:    `"/tmp/ws\")) (allow file-write*) ;"`,
			comment: "verified to be a real escape against sandbox-exec when unescaped",
		},
		{
			name: "allow-default escape attempt is neutralised",
			path: `/tmp/ws") (allow default) (deny nothing "`,
			want: `"/tmp/ws\") (allow default) (deny nothing \""`,
		},
		{
			name: "backslash-quote pair cannot smuggle a terminator",
			path: `/tmp/ws\") (allow default) ;`,
			want: `"/tmp/ws\\\") (allow default) ;"`,
		},
		{
			name: "newline is replaced wholesale, not escaped",
			path: "/tmp/ws\n(allow default)",
			want: `"` + unmatchablePath + `"`,
		},
		{
			name: "carriage return is replaced wholesale",
			path: "/tmp/ws\r(allow default)",
			want: `"` + unmatchablePath + `"`,
		},
		{
			name: "NUL is replaced wholesale",
			path: "/tmp/ws\x00(allow default)",
			want: `"` + unmatchablePath + `"`,
		},
		{
			name: "tab is replaced wholesale",
			path: "/tmp/ws\t(allow default)",
			want: `"` + unmatchablePath + `"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := quoteSBPL(tc.path); got != tc.want {
				t.Fatalf("quoteSBPL(%q) = %s, want %s (%s)", tc.path, got, tc.want, tc.comment)
			}
		})
	}
}

// TestBuildProfileNeverEmitsUnbalancedQuotes is the structural counterpart to
// the escaping table: rather than checking one payload's rendering, it counts
// unescaped quotes across a whole generated profile.
//
// Balance is the invariant that matters. Any injection that changes the
// profile's meaning must first escape a string literal, and escaping a literal
// necessarily leaves an odd number of unescaped quotes somewhere. This catches
// a future rule added to the generator that interpolates a path with %q or
// with raw concatenation instead of going through quoteSBPL.
func TestBuildProfileNeverEmitsUnbalancedQuotes(t *testing.T) {
	nasty := []string{
		`/tmp/ws") (allow default) ("`,
		`/tmp/ws\`,
		`/tmp/a"b"c`,
		`/tmp/ws")) (allow file-write*) ;`,
	}
	for _, tier := range []AccessTier{ReadOnly, WorkspaceWrite, FullAccess} {
		profile := BuildProfile(ProfileInput{
			Tier:          tier,
			WorkspaceRoot: nasty[0],
			WritablePaths: nasty[1:],
			DenyPaths:     nasty,
			NetworkDeny:   true,
			AllowLoopback: true,
		})
		for i, line := range strings.Split(profile, "\n") {
			if countUnescapedQuotes(line)%2 != 0 {
				t.Errorf("tier %s line %d has unbalanced quotes: %s", tier, i+1, line)
			}
		}
		// A meaning-changing injection would land one of these tokens at the
		// start of a line, outside any literal. Inside a literal they are inert.
		for _, forbidden := range []string{"\n(allow default)", "\n(allow file-write*)\n"} {
			if strings.Contains(profile, forbidden) && tier != FullAccess {
				t.Errorf("tier %s: injected rule %q escaped into the profile:\n%s", tier, forbidden, profile)
			}
		}
	}
}

// countUnescapedQuotes counts double quotes that actually open or close a
// string literal, skipping any preceded by an odd run of backslashes.
func countUnescapedQuotes(line string) int {
	n := 0
	for i := 0; i < len(line); i++ {
		if line[i] != '"' {
			continue
		}
		backslashes := 0
		for j := i - 1; j >= 0 && line[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			n++
		}
	}
	return n
}

// TestBuildProfileIsDeterministic pins that the same input yields byte-identical
// output and that path order does not leak into the result. Without the sort a
// caller building its path list from a map would produce a different profile on
// every run, which makes the golden tests flaky and makes two runs impossible
// for an operator to diff.
func TestBuildProfileIsDeterministic(t *testing.T) {
	a := ProfileInput{
		Tier:          WorkspaceWrite,
		WorkspaceRoot: "/private/tmp/ws",
		WritablePaths: []string{"/private/tmp/c", "/private/tmp/a", "/private/tmp/b"},
		DenyPaths:     []string{"/z", "/y"},
	}
	b := a
	b.WritablePaths = []string{"/private/tmp/b", "/private/tmp/c", "/private/tmp/a"}
	b.DenyPaths = []string{"/y", "/z"}

	first := BuildProfile(a)
	if second := BuildProfile(a); first != second {
		t.Fatal("BuildProfile is not deterministic for identical input")
	}
	if reordered := BuildProfile(b); first != reordered {
		t.Fatalf("path order leaked into the profile:\n--- a ---\n%s\n--- b ---\n%s", first, reordered)
	}
}

// TestBuildProfileDropsEmptyPaths pins that an unset WorkspaceRoot does not
// render as (subpath ""). An empty subpath rule matches nothing useful and
// reads to anyone auditing the profile like a bug rather than an absent value.
func TestBuildProfileDropsEmptyPaths(t *testing.T) {
	got := BuildProfile(ProfileInput{
		Tier:          WorkspaceWrite,
		WorkspaceRoot: "",
		WritablePaths: []string{"", "/private/tmp/real", ""},
		DenyPaths:     []string{""},
	})
	if strings.Contains(got, `(subpath "")`) {
		t.Errorf("empty path rendered as a rule:\n%s", got)
	}
	if !strings.Contains(got, `(subpath "/private/tmp/real")`) {
		t.Errorf("real path was dropped along with the empties:\n%s", got)
	}
	if strings.Contains(got, "(deny file-read*\n") {
		t.Errorf("an all-empty DenyPaths list produced a deny block:\n%s", got)
	}
}

// TestReadOnlyStillGrantsOutputDevices pins that "read-only" is a statement
// about the filesystem, not about the process's ability to emit output.
// Dropping /dev/null alone breaks `cmd > /dev/null`, which appears in a large
// fraction of real shell commands, and the failure would look to the model
// like the command itself was broken.
func TestReadOnlyStillGrantsOutputDevices(t *testing.T) {
	got := BuildProfile(ProfileInput{Tier: ReadOnly})
	for _, dev := range readOnlyDevices {
		if !strings.Contains(got, `(literal "`+dev+`")`) {
			t.Errorf("read-only profile does not grant %s:\n%s", dev, got)
		}
	}
	// write-data, not write*: the child may put bytes into the node but must
	// not be able to unlink /dev/null and replace it with a capture file.
	if !strings.Contains(got, "(allow file-write-data\n") {
		t.Errorf("device grant must be file-write-data, not file-write*:\n%s", got)
	}
}

// TestSignalScopeIsSameSandbox pins the one process rule with a security
// consequence. Measured against the real sandbox: (target self) does NOT cover
// the process's own children, so a shell cannot signal the jobs it started; a
// bare (allow signal) DOES let the sandboxed child kill an unrelated host pid.
// same-sandbox is the only setting that permits the first and refuses the
// second, so a future edit widening it must fail here.
func TestSignalScopeIsSameSandbox(t *testing.T) {
	for _, tier := range []AccessTier{ReadOnly, WorkspaceWrite} {
		got := BuildProfile(ProfileInput{Tier: tier})
		if !strings.Contains(got, "(allow signal (target same-sandbox))") {
			t.Errorf("tier %s: signal is not scoped to same-sandbox:\n%s", tier, got)
		}
		if strings.Contains(got, "(allow signal)\n") {
			t.Errorf("tier %s: bare (allow signal) permits killing unrelated host processes:\n%s", tier, got)
		}
	}
}

// TestScratchPathsAreAbsoluteAndDeduped pins the shape of the scratch set. The
// entries themselves are host-dependent (TMPDIR is per-user and per-boot), so
// this asserts the properties every entry must have rather than the values.
func TestScratchPathsAreAbsoluteAndDeduped(t *testing.T) {
	paths := ScratchPaths()
	if len(paths) == 0 {
		t.Fatal("ScratchPaths returned nothing; a WorkspaceWrite child would have no temp directory")
	}
	seen := map[string]bool{}
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			t.Errorf("scratch path %q is not absolute; Seatbelt subpath rules require absolute paths", p)
		}
		if seen[p] {
			t.Errorf("scratch path %q appears twice", p)
		}
		seen[p] = true
	}
}

// TestDedupeSortedDropsEmpties is the unit test for the helper that makes
// BuildProfile deterministic.
func TestDedupeSortedDropsEmpties(t *testing.T) {
	got := dedupeSorted([]string{"b", "", "a", "b", "", "c"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("dedupeSorted = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dedupeSorted = %v, want %v", got, want)
		}
	}
}

// TestResolvePathHandlesMissingAndEmpty pins the two non-obvious branches of
// ResolvePath: an empty input stays empty (so it can be dropped rather than
// rendered as a rule), and a path that does not exist yet is still returned
// absolute rather than dropped — a workspace the child will create still needs
// its rule.
func TestResolvePathHandlesMissingAndEmpty(t *testing.T) {
	if got := ResolvePath(""); got != "" {
		t.Errorf("ResolvePath(\"\") = %q, want empty", got)
	}
	missing := filepath.Join(t.TempDir(), "not-created-yet", "deeper")
	got := ResolvePath(missing)
	if got == "" {
		t.Fatal("ResolvePath dropped a path that does not exist yet")
	}
	if !filepath.IsAbs(got) {
		t.Errorf("ResolvePath(%q) = %q, which is not absolute", missing, got)
	}
}

// TestResolvePathFollowsSymlinks is the regression test for the mistake that
// silently disables the WorkspaceWrite tier on macOS.
//
// Seatbelt matches the RESOLVED path. On macOS the per-user temp directory
// t.TempDir() hands out lives under /var/folders/..., and /var is a symlink to
// /private/var — so a profile granting the unresolved path never matches, the
// workspace is silently read-only, and a test that only asserted the
// outside-workspace denial would have passed for entirely the wrong reason.
// This was observed exactly that way while building this backend.
func TestResolvePathFollowsSymlinks(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := mkdirAllForTest(real); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := symlinkForTest(real, link); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}
	resolvedLink := ResolvePath(link)
	resolvedReal := ResolvePath(real)
	if resolvedLink != resolvedReal {
		t.Fatalf("ResolvePath did not follow the symlink:\n  link -> %q\n  real -> %q", resolvedLink, resolvedReal)
	}
}

// TestProfileRestrictsNothingMatchesTheRenderedProfile is the cross-check that
// keeps the honesty predicate honest.
//
// ProfileRestrictsNothing is a predicate over the INPUT, chosen so it cannot
// rot when a rule is reworded. The risk that trade accepts is that the
// predicate stops describing what BuildProfile actually emits. So this test
// derives the answer the other way — by looking for any deny in the RENDERED
// text — and requires the two to agree on every combination.
//
// "Restricts nothing" is defined here as "the profile contains no (deny ...)
// form at all". That is deliberately the weakest possible reading: a profile
// with even one deny is restricting something, so any disagreement this test
// reports is the predicate over-claiming vacuity, never under-claiming it.
func TestProfileRestrictsNothingMatchesTheRenderedProfile(t *testing.T) {
	tiers := []AccessTier{ReadOnly, WorkspaceWrite, FullAccess}
	denySets := [][]string{nil, {}, {""}, {"/Users/x/.ssh"}}
	for _, tier := range tiers {
		for _, netDeny := range []bool{true, false} {
			for _, deny := range denySets {
				in := ProfileInput{
					Tier:          tier,
					WorkspaceRoot: "/ws",
					WritablePaths: []string{"/tmp"},
					NetworkDeny:   netDeny,
					DenyPaths:     deny,
				}
				profile := BuildProfile(in)
				renderedVacuous := !strings.Contains(profile, "(deny ")
				if got := ProfileRestrictsNothing(in); got != renderedVacuous {
					t.Errorf("tier=%s netDeny=%t denyPaths=%q: "+
						"ProfileRestrictsNothing=%t but the rendered profile %s contain a deny.\n"+
						"The predicate no longer describes what BuildProfile emits, so the "+
						"capability report's vacuity disclosure is wrong.\nprofile:\n%s",
						tier, netDeny, deny, got,
						map[bool]string{true: "does NOT", false: "DOES"}[renderedVacuous], profile)
				}
			}
		}
	}
}

// TestProfileRestrictsNothingOnlyAtFullAccess pins the claim made in
// ProfileRestrictsNothing's own doc comment: the other two tiers can never be
// vacuous, because both open with (deny default).
//
// Asserted separately from the cross-check above because it is a different
// statement. The cross-check says "the predicate matches the renderer"; this
// says "only one tier can ever reach the vacuous state at all". If a future
// edit made ReadOnly render (allow default), the cross-check would still pass
// (both sides would flip together) and this test is what would catch it.
func TestProfileRestrictsNothingOnlyAtFullAccess(t *testing.T) {
	for _, tier := range []AccessTier{ReadOnly, WorkspaceWrite} {
		for _, netDeny := range []bool{true, false} {
			in := ProfileInput{Tier: tier, WorkspaceRoot: "/ws", NetworkDeny: netDeny}
			if ProfileRestrictsNothing(in) {
				t.Errorf("tier %s with netDeny=%t was judged to restrict nothing; "+
					"a non-FullAccess tier must always render (deny default)", tier, netDeny)
			}
			if !strings.Contains(BuildProfile(in), "(deny default)") {
				t.Errorf("tier %s no longer renders (deny default): the whole "+
					"write boundary is gone", tier)
			}
		}
	}

	vacuous := ProfileInput{Tier: FullAccess, WorkspaceRoot: "/ws", NetworkDeny: false}
	if !ProfileRestrictsNothing(vacuous) {
		t.Fatal("FullAccess with no network denial and no DenyPaths must be judged " +
			"vacuous: the rendered profile is (version 1)(allow default)")
	}
	// Non-vacuity guard: if this input somehow stopped being reachable the test
	// above would pass without ever exercising the true branch.
	for _, saved := range []ProfileInput{
		{Tier: FullAccess, WorkspaceRoot: "/ws", NetworkDeny: true},
		{Tier: FullAccess, WorkspaceRoot: "/ws", DenyPaths: []string{"/Users/x/.aws"}},
	} {
		if ProfileRestrictsNothing(saved) {
			t.Errorf("FullAccess input %+v restricts something (network or DenyPaths) "+
				"but was judged vacuous", saved)
		}
	}
}
