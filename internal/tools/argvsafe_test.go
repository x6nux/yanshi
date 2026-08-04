package tools

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/secproc"
)

// canaryPath returns a file path inside a directory that is deliberately
// OUTSIDE the work root handed to the tool under test. Both git_diff
// (sandbox.ReadOnly) and run_tests (sandbox.WorkspaceWrite) declare tiers that
// forbid writing there, so the file's existence is the whole finding: a tool
// that produced a file on disk outside the workspace it declared.
func canaryPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "PWNED")
}

func mustNotExist(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("%s: %s exists — the value was parsed as an option and wrote a file outside the work root", what, path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("%s: stat %s: %v", what, path, err)
	}
}

// TestGitDiffRefCannotWriteFilesOutsideWorkRoot is the regression test for the
// argument-injection hole in validateGitRef.
//
// The old validator stripped whitespace and NUL and accepted everything else,
// so scope.ref = "--output=<abs path>" reached git's argv as an option rather
// than a revision, and `git show --numstat -z --format= --output=/tmp/x --`
// wrote the diff to /tmp/x. git_diff declares sandbox.ReadOnly, and on the
// platforms where internal/sandbox is still phase0 that declaration is the
// only boundary there is — so the tool was an arbitrary-file-write primitive.
//
// The test proves the fix against the FILESYSTEM, not against an error value.
// It first runs the unguarded git command by hand to confirm the primitive is
// real on this machine's git (a test that only asserts absence would pass
// vacuously on a git that never had --output), then deletes the evidence and
// drives the actual tool, which must leave the canary path empty.
func TestGitDiffRefCannotWriteFilesOutsideWorkRoot(t *testing.T) {
	root := initTempGitRepo(t)
	commitFile(t, root, "a.txt", "one\n")
	commitFile(t, root, "a.txt", "two\n")
	canary := canaryPath(t)

	// Negative control: the raw invocation the tool used to build.
	raw := exec.Command("git", "-c", "core.quotepath=false", "show", "--numstat", "-z",
		"--format=", "--output="+canary, "--")
	raw.Dir = root
	if out, err := raw.CombinedOutput(); err != nil {
		t.Logf("control invocation failed (%v): %s", err, out)
	}
	if _, err := os.Stat(canary); err != nil {
		t.Logf("control did not produce %s on this git build; the fix assertion below still holds", canary)
	} else {
		t.Logf("control confirmed: unguarded git wrote %s", canary)
		if err := os.Remove(canary); err != nil {
			t.Fatal(err)
		}
	}

	ctx := realGitCtx(t, root)
	for _, tc := range []struct {
		name string
		ref  string
	}{
		{"output-flag", "--output=" + canary},
		{"output-indicator", "--output-indicator-new=x"},
		{"order-file", "-O" + canary},
		{"ext-diff", "--ext-diff"},
		{"bare-dash", "-"},
	} {
		for _, kind := range []string{"commit", "base_ref"} {
			payload, err := json.Marshal(map[string]any{
				"scope": map[string]string{"kind": kind, "ref": tc.ref},
			})
			if err != nil {
				t.Fatal(err)
			}
			out, err := runTool(ctx, NewGitTools().Diff, string(payload))
			if err == nil && !strings.Contains(out, "must not start with") {
				t.Fatalf("%s/%s: expected rejection, got %s", kind, tc.name, out)
			}
			mustNotExist(t, canary, kind+"/"+tc.name)
		}
	}
}

// TestGitDiffAcceptsRefsContainingDashes is the false-positive guard for the
// fix above: the rule is "must not START with a dash", and refs carry dashes
// everywhere else. Rejecting these would break the ordinary case (release
// tags, kebab-case branches) in the name of security, which is how shape
// checks get reverted.
func TestGitDiffAcceptsRefsContainingDashes(t *testing.T) {
	root := initTempGitRepo(t)
	commitFile(t, root, "a.txt", "one\n")
	for _, name := range []string{"my-branch", "feature/x-y"} {
		cmd := exec.Command("git", "branch", name)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("branch %s: %v\n%s", name, err, out)
		}
	}
	cmd := exec.Command("git", "tag", "v1.0-rc1")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tag: %v\n%s", err, out)
	}
	commitFile(t, root, "a.txt", "two\n")

	ctx := realGitCtx(t, root)
	for _, ref := range []string{"my-branch", "feature/x-y", "v1.0-rc1"} {
		for _, kind := range []string{"commit", "base_ref"} {
			payload, err := json.Marshal(map[string]any{
				"scope": map[string]string{"kind": kind, "ref": ref},
			})
			if err != nil {
				t.Fatal(err)
			}
			out, err := runTool(ctx, NewGitTools().Diff, string(payload))
			if err != nil {
				t.Fatalf("%s/%s: %v", kind, ref, err)
			}
			if strings.Contains(out, `"error"`) {
				t.Fatalf("%s/%s rejected a legitimate ref: %s", kind, ref, out)
			}
		}
	}
	// validateGitRef is the unit under the integration: assert the same inputs
	// directly so a future refactor of the tool wiring cannot hide a regression.
	for _, ref := range []string{"my-branch", "v1.0-rc1", "feature/x-y", "HEAD~1", "main@{1}"} {
		if err := validateGitRef(ref); err != nil {
			t.Fatalf("validateGitRef(%q) = %v, want nil", ref, err)
		}
	}
}

// TestGitDiffPassesEndOfOptionsBeforeRef pins the second defence layer. git's
// own --end-of-options marker must sit after every real option and immediately
// before the revision in all four ref-carrying invocations, so that a future
// call site added without validateGitRef still cannot smuggle a flag through.
func TestGitDiffPassesEndOfOptionsBeforeRef(t *testing.T) {
	for _, tc := range []struct {
		kind    string
		wantRef string
	}{
		{"base_ref", "v1.0-rc1...HEAD"},
		{"commit", "v1.0-rc1"},
	} {
		args := gitDiffArgs{}
		args.Scope.Kind = tc.kind
		args.Scope.Ref = "v1.0-rc1"
		numstat, patch, err := gitDiffCommands(args)
		if err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		for label, argv := range map[string][]string{
			"numstat": numstat.Args,
			"patch":   patch("a.txt").Args,
		} {
			marker := -1
			for i, a := range argv {
				if a == gitEndOfOptions {
					marker = i
					break
				}
			}
			if marker < 0 {
				t.Fatalf("%s/%s: argv=%v missing %s", tc.kind, label, argv, gitEndOfOptions)
			}
			if marker+1 >= len(argv) || argv[marker+1] != tc.wantRef {
				t.Fatalf("%s/%s: argv=%v, %s must be immediately followed by %q", tc.kind, label, argv, gitEndOfOptions, tc.wantRef)
			}
			for _, a := range argv[:marker] {
				if a == "--" {
					t.Fatalf("%s/%s: argv=%v has -- before %s, which ends revision parsing early", tc.kind, label, argv, gitEndOfOptions)
				}
			}
		}
	}
}

// TestGitDiffPathOperandsFollowSeparator records why the pathspec operands
// need no validation of their own: every path this file emits sits after the
// `--` separator, where git has already stopped looking for options. If a
// future edit moves a path in front of `--`, this test is the one that fails.
func TestGitDiffPathOperandsFollowSeparator(t *testing.T) {
	for _, kind := range []string{"working_tree", "base_ref", "commit"} {
		args := gitDiffArgs{}
		args.Scope.Kind = kind
		args.Scope.Ref = "HEAD"
		_, patch, err := gitDiffCommands(args)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		argv := patch("-weird-name.txt").Args
		sep := -1
		for i, a := range argv {
			if a == "--" {
				sep = i
				break
			}
		}
		if sep < 0 {
			t.Fatalf("%s: argv=%v has no -- separator", kind, argv)
		}
		for _, a := range argv[:sep] {
			if a == "-weird-name.txt" {
				t.Fatalf("%s: argv=%v puts the path before --", kind, argv)
			}
		}
		if argv[len(argv)-1] != "-weird-name.txt" {
			t.Fatalf("%s: argv=%v does not end with the path operand", kind, argv)
		}
	}
}

// TestRunTestsPackagePatternCannotWriteFilesOutsideWorkRoot is the run_tests
// twin of the git hole, and it was live for the same reason: args.Packages
// went into `go test -json <packages…>` as bare positional operands with no
// validation, so packages: ["-o", "<abs path>", "./..."] made the Go toolchain
// LINK THE TEST BINARY to an arbitrary absolute path. run_tests declares
// sandbox.WorkspaceWrite; this writes a 4 MB executable outside it.
//
// As above, the control runs the unguarded command first so the absence
// assertion cannot pass vacuously.
func TestRunTestsPackagePatternCannotWriteFilesOutsideWorkRoot(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module canary\n\ngo 1.21\n")
	write("x.go", "package canary\n")
	write("x_test.go", "package canary\n\nimport \"testing\"\n\nfunc TestNoop(t *testing.T) {}\n")
	canary := canaryPath(t)

	control := exec.Command("go", "test", "-json", "-o", canary, "./...")
	control.Dir = root
	control.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := control.CombinedOutput(); err != nil {
		t.Logf("control invocation failed (%v): %s", err, out)
	}
	if _, err := os.Stat(canary); err != nil {
		t.Logf("control did not produce %s on this toolchain; the fix assertion below still holds", canary)
	} else {
		t.Logf("control confirmed: unguarded `go test -o` wrote %s", canary)
		if err := os.Remove(canary); err != nil {
			t.Fatal(err)
		}
	}

	launched := 0
	factory := newScriptedFactory(t, func(secproc.SecureProcessSpec) cannedResult {
		launched++
		return cannedResult{}
	})
	ctx := WithWorkRoot(secureTestContext(t, factory), root)
	for _, pkgs := range [][]string{
		{"-o", canary, "./..."},
		{"-exec", "/bin/sh"},
		{"-toolexec=/bin/sh"},
	} {
		payload, err := json.Marshal(map[string]any{"framework": "go", "packages": pkgs})
		if err != nil {
			t.Fatal(err)
		}
		out, err := runTool(ctx, NewTestRunTool(), string(payload))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "must not start with") {
			t.Fatalf("packages=%v: expected rejection, got %s", pkgs, out)
		}
		mustNotExist(t, canary, "run_tests packages")
	}
	if launched != 0 {
		t.Fatalf("rejected argv still reached the process factory %d time(s)", launched)
	}
}

// TestRunTestsAcceptsPackagePatternsContainingDashes is the false-positive
// guard: Go import paths are full of dashes and must survive the check.
func TestRunTestsAcceptsPackagePatternsContainingDashes(t *testing.T) {
	var last secproc.SecureProcessSpec
	factory := newScriptedFactory(t, func(s secproc.SecureProcessSpec) cannedResult {
		last = s
		return cannedResult{Stdout: `{"Action":"pass","Package":"p","Test":"T"}` + "\n"}
	})
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := WithWorkRoot(secureTestContext(t, factory), root)
	pkgs := []string{"./cmd/foo-bar", "example.com/x-y/z...", "./..."}
	payload, err := json.Marshal(map[string]any{"framework": "go", "packages": pkgs})
	if err != nil {
		t.Fatal(err)
	}
	out, err := runTool(ctx, NewTestRunTool(), string(payload))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "must not start with") {
		t.Fatalf("legitimate package patterns rejected: %s", out)
	}
	for _, pkg := range pkgs {
		found := false
		for _, a := range last.Args {
			if a == pkg {
				found = true
			}
		}
		if !found {
			t.Fatalf("argv=%v dropped %q", last.Args, pkg)
		}
	}
}

// TestRunTestsRejectsOptionLikeCargoFilter covers the third operand slot:
// `cargo test <TESTNAME>` is positional, so a dash-leading filter becomes a
// cargo option (--target-dir=… writes, -Z… changes behaviour). cargo is not
// assumed to be installed, so the assertion is structural — the runner must
// refuse before any process is started at all.
func TestRunTestsRejectsOptionLikeCargoFilter(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Cargo.toml"), []byte("[package]\nname=\"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	launched := 0
	factory := newScriptedFactory(t, func(secproc.SecureProcessSpec) cannedResult {
		launched++
		return cannedResult{}
	})
	ctx := WithWorkRoot(secureTestContext(t, factory), root)
	out, err := runTool(ctx, NewTestRunTool(), `{"framework":"cargo","filter":"--target-dir=/tmp/pwn"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "must not start with") {
		t.Fatalf("expected rejection, got %s", out)
	}
	if launched != 0 {
		t.Fatalf("rejected argv still reached the process factory %d time(s)", launched)
	}
	// A legitimate cargo filter with interior dashes must still run.
	out, err = runTool(ctx, NewTestRunTool(), `{"framework":"cargo","filter":"tests::my-case"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "must not start with") {
		t.Fatalf("legitimate cargo filter rejected: %s", out)
	}
}

// TestRunTestsFlagSlotFiltersAreNotOverRejected documents the audit's negative
// result: `go test -run <filter>` and `npm test -- --testNamePattern <filter>`
// place the value in a flag slot whose parser consumes the next argv element
// unconditionally, so no injection is possible there and no validation is
// applied. A jest pattern may legitimately start with anything; over-rejecting
// would be a behaviour regression sold as a security fix.
func TestRunTestsFlagSlotFiltersAreNotOverRejected(t *testing.T) {
	for _, tc := range []struct {
		framework string
		filter    string
		wantFlag  string
	}{
		{"go", "-not-a-flag", "-run"},
		{"npm", "-dash leading name", "--testNamePattern"},
	} {
		spec, err := testSpec(tc.framework, runTestsArgs{Filter: tc.filter}, "/root")
		if err != nil {
			t.Fatalf("%s: %v", tc.framework, err)
		}
		idx := -1
		for i, a := range spec.Args {
			if a == tc.wantFlag {
				idx = i
			}
		}
		if idx < 0 || idx+1 >= len(spec.Args) || spec.Args[idx+1] != tc.filter {
			t.Fatalf("%s: argv=%v must place the filter directly after %s", tc.framework, spec.Args, tc.wantFlag)
		}
	}
}

// TestGitHubArgvPlacesUserValuesInFlagSlotsOnly records the github.go half of
// the audit. `repo` and `body` are model-controlled but always emitted as the
// value of --repo / --body, which cobra/pflag consumes unconditionally; the
// only positional operand is the PR number, which is an int. `method` is
// concatenated into "--"+method and IS therefore injection-shaped, but the
// enum switch in runGitHubMerge rejects anything outside merge|squash|rebase
// before the concatenation happens. This test pins all three properties so a
// refactor that moves repo into a positional slot fails here.
func TestGitHubArgvPlacesUserValuesInFlagSlotsOnly(t *testing.T) {
	var specs []secproc.SecureProcessSpec
	factory := newScriptedFactory(t, func(s secproc.SecureProcessSpec) cannedResult {
		specs = append(specs, s)
		return cannedResult{Stdout: "{}"}
	})
	ctx := WithPermissionCallback(secureTestContext(t, factory), func(PermissionRequest) PermissionDecision {
		return PermissionAllow
	})
	hostile := `--output=/tmp/pwn`
	gt := NewGitHubTools(nil)
	for _, call := range []struct {
		tool *GuardedTool
		args string
	}{
		{gt.PRContext, `{"repo":"` + hostile + `","number":7}`},
		{gt.Comment, `{"repo":"` + hostile + `","number":7,"body":"` + hostile + `"}`},
		{gt.Approve, `{"repo":"` + hostile + `","number":7,"body":"` + hostile + `"}`},
		{gt.Merge, `{"repo":"` + hostile + `","number":7,"method":"squash"}`},
	} {
		if _, err := runTool(ctx, call.tool, call.args); err != nil {
			t.Fatal(err)
		}
	}
	if len(specs) != 4 {
		t.Fatalf("expected 4 gh invocations, got %d", len(specs))
	}
	for _, spec := range specs {
		for i, a := range spec.Args {
			if a != hostile {
				continue
			}
			if i == 0 {
				t.Fatalf("argv=%v starts with a model-controlled value", spec.Args)
			}
			switch spec.Args[i-1] {
			case "--repo", "--body":
			default:
				t.Fatalf("argv=%v places a model-controlled value in a positional slot (preceded by %q)", spec.Args, spec.Args[i-1])
			}
		}
	}
	// The enum guard on `method` is what keeps "--"+method safe.
	out, err := runTool(ctx, gt.Merge, `{"repo":"owner/repo","number":7,"method":"output=/tmp/pwn"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "unknown merge method") {
		t.Fatalf("merge method enum did not reject an injected value: %s", out)
	}
}

// TestValidateArgvOperandRejectsOnlyOptionShapes is the unit-level contract
// for the shared helper: leading dash and emptiness out, dashes everywhere
// else in.
func TestValidateArgvOperandRejectsOnlyOptionShapes(t *testing.T) {
	for _, bad := range []string{"", "-", "--", "-o", "--output=/tmp/x", "-Ofile", "--ext-diff", "a\x00b"} {
		if err := validateArgvOperand("operand", bad); err == nil {
			t.Fatalf("validateArgvOperand(%q) = nil, want error", bad)
		}
	}
	for _, good := range []string{"my-branch", "v1.0-rc1", "feature/x-y", "./cmd/foo-bar", "a-", "x--y", "HEAD~1", "a b"} {
		if err := validateArgvOperand("operand", good); err != nil {
			t.Fatalf("validateArgvOperand(%q) = %v, want nil", good, err)
		}
	}
	// The message must name the reason; the model reads it and retries.
	err := validateArgvOperand("git ref", "--output=/tmp/x")
	if err == nil || !strings.Contains(err.Error(), "must not start with") {
		t.Fatalf("unhelpful error: %v", err)
	}
}
