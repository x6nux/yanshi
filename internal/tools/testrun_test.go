package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
)

// ledger: B3/DT4#1 至少 Go 解析正确
func TestParseGoJSONCountsPassFailSkip(t *testing.T) {
	raw := `{"Action":"pass","Package":"p","Test":"TestA"}` + "\n" +
		`{"Action":"fail","Package":"p","Test":"TestB"}` + "\n" +
		`{"Action":"skip","Package":"p","Test":"TestC"}` + "\n"
	got := parseGoJSON(raw)
	if got.Framework != "go" || got.Passed != 1 || got.Failed != 1 || got.Skipped != 1 {
		t.Fatalf("got=%+v", got)
	}
	if len(got.Failures) != 1 || got.Failures[0].Test != "TestB" {
		t.Fatalf("failures=%+v", got.Failures)
	}
}

func TestParseCargoOutputSummarizes(t *testing.T) {
	raw := "test result: ok. 3 passed; 1 failed; 0 ignored; ...\nrunning test::other ...\n\nfailures:\n    case_x\n"
	got := parseCargoOutput(raw)
	if got.Framework != "cargo" || got.Passed != 3 || got.Failed != 1 {
		t.Fatalf("got=%+v", got)
	}
	if len(got.Failures) != 1 || got.Failures[0].Test != "case_x" {
		t.Fatalf("failures=%+v", got.Failures)
	}
}

func TestParseNPMOutputAggregates(t *testing.T) {
	raw := `{"stats":{"passes":4,"failures":2,"pending":1},"tests":[{"name":"t1","err":"bad"}],"passes":[{"name":"t1"}],"failures":[{"name":"t1"}]}`
	got := parseNPMOutput(raw)
	if got.Framework != "npm" || got.Passed != 4 || got.Failed != 2 || got.Skipped != 1 {
		t.Fatalf("got=%+v", got)
	}
}

// TestParseTestResultIgnoresStderr pins the stdout-only contract of
// parseTestResult for all three frameworks. Before this, cargo and npm were
// handed res.Stdout+res.Stderr, which turned ordinary runner chatter into
// fabricated verdicts: one "npm WARN" line made a fully passing suite report
// status:"error", and any stderr line ending in "failures:" made cargo harvest
// the following stderr lines as failed test names.
func TestParseTestResultIgnoresStderr(t *testing.T) {
	t.Run("npm warning does not break a passing suite", func(t *testing.T) {
		res := commandResult{
			Stdout: `{"stats":{"passes":12,"failures":0,"pending":1},"failures":[]}`,
			Stderr: "npm WARN config production Use `--omit=dev` instead.\n",
		}
		got := parseTestResult("npm", res)
		if got.Status != "pass" || got.Passed != 12 || got.Failed != 0 {
			t.Fatalf("npm stderr poisoned the result: %+v", got)
		}
	})
	t.Run("cargo stderr cannot fabricate failures", func(t *testing.T) {
		res := commandResult{
			Stdout: "running 3 tests\ntest result: ok. 3 passed; 0 failed; 0 ignored\n",
			Stderr: "note: previous failures:\nphantom_case\n",
		}
		got := parseTestResult("cargo", res)
		if got.Status != "pass" || got.Failed != 0 {
			t.Fatalf("cargo stderr poisoned the result: %+v", got)
		}
		if len(got.Failures) != 0 {
			t.Fatalf("cargo harvested phantom failures from stderr: %+v", got.Failures)
		}
	})
	t.Run("go already stdout-only", func(t *testing.T) {
		res := commandResult{
			Stdout: `{"Action":"pass","Package":"p","Test":"TestA"}` + "\n",
			Stderr: `{"Action":"fail","Package":"p","Test":"TestGhost"}` + "\n",
		}
		got := parseTestResult("go", res)
		if got.Passed != 1 || got.Failed != 0 {
			t.Fatalf("go stderr poisoned the result: %+v", got)
		}
	})
}

func TestDetectRunnerPriority(t *testing.T) {
	cases := []struct {
		files map[string]bool
		want  string
	}{
		{map[string]bool{"go.mod": true}, "go"},
		{map[string]bool{"Cargo.toml": true}, "cargo"},
		{map[string]bool{"package.json": true}, "npm"},
		{map[string]bool{"go.mod": true, "Cargo.toml": true}, "go"},
		{map[string]bool{}, ""},
	}
	for _, tc := range cases {
		if got := detectRunner(tc.files); got != tc.want {
			t.Fatalf("files=%v got=%s want=%s", tc.files, got, tc.want)
		}
	}
}

// ledger: B3/DT4#1 至少 Go 解析正确
func TestRunTestsExecutesGoTestJSONWithWorkspaceWriteTier(t *testing.T) {
	var last secproc.SecureProcessSpec
	factory := newScriptedFactory(t, func(s secproc.SecureProcessSpec) cannedResult {
		last = s
		return cannedResult{Stdout: `{"Action":"pass","Package":"p","Test":"T"}` + "\n"}
	})
	root := t.TempDir()
	// Create a go.mod so detectRunner("auto") picks "go".
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := WithWorkRoot(secureTestContext(t, factory), root)
	out, err := runTool(ctx, NewTestRunTool(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if last.Program != "go" || last.Args[0] != "test" || last.Args[1] != "-json" {
		t.Fatalf("argv=%+v", last.Args)
	}
	if last.UseSandboxTier != sandbox.WorkspaceWrite {
		t.Fatalf("tier=%v", last.UseSandboxTier)
	}
	var res testResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.Framework != "go" || res.Passed != 1 || res.Status != "pass" {
		t.Fatalf("res=%+v", res)
	}
}

// TestRunTestsReportsRunnerFailureInsteadOfPass is the regression test for
// run_tests answering {"status":"pass","passed":0,"failed":0} for a `go test`
// that never ran a single test.
//
// The parsers only see per-test events, so a runner that dies before the first
// test (missing module cache, build error, unknown package, missing toolchain)
// parses as "0 passed, 0 failed" and every parser then declares "pass". The
// exit code is the only signal present in all of those cases, and testrun.go
// did not read it anywhere. Reported as green, the model proceeds as if the
// suite passed — the single worst failure mode a test tool can have.
//
// Each framework is exercised because each has its own parser and each one
// reached the same wrong conclusion independently.
//
// ledger: B3/DT4#2 结构化计数+失败列表
func TestRunTestsReportsRunnerFailureInsteadOfPass(t *testing.T) {
	for _, tc := range []struct {
		marker string
		stderr string
	}{
		{"go.mod", "go: module cache not found: neither GOMODCACHE nor GOPATH is set"},
		{"Cargo.toml", "error: could not find `Cargo.toml`"},
		{"package.json", "npm ERR! missing script: test"},
	} {
		t.Run(tc.marker, func(t *testing.T) {
			factory := newScriptedFactory(t, func(secproc.SecureProcessSpec) cannedResult {
				return cannedResult{Stderr: tc.stderr, ExitCode: 1}
			})
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, tc.marker), []byte("{}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			ctx := WithWorkRoot(secureTestContext(t, factory), root)
			out, err := runTool(ctx, NewTestRunTool(), `{}`)
			if err != nil {
				t.Fatal(err)
			}
			var res testResult
			if err := json.Unmarshal([]byte(out), &res); err != nil {
				t.Fatalf("unmarshal %q: %v", out, err)
			}
			if res.Status == "pass" {
				t.Fatalf("runner exited 1 with no test events but run_tests reported %q — "+
					"a false green (result: %s)", res.Status, out)
			}
			if !strings.Contains(res.Summary, tc.stderr) {
				t.Fatalf("summary %q must carry the runner's own failure text %q — "+
					"it is the only place the reason exists", res.Summary, tc.stderr)
			}
		})
	}
}

// TestRunTestsKeepsFailWhenTestsActuallyFailed pins the other half of the
// exit-code reconciliation: a non-zero exit with parsed failures must stay
// "fail" (the parser already knows what broke) and must not be flattened into
// the coarser "error" bucket.
//
// ledger: B3/DT4#2 结构化计数+失败列表
func TestRunTestsKeepsFailWhenTestsActuallyFailed(t *testing.T) {
	factory := newScriptedFactory(t, func(secproc.SecureProcessSpec) cannedResult {
		return cannedResult{
			Stdout:   `{"Action":"fail","Package":"p","Test":"TBroken"}` + "\n",
			ExitCode: 1,
		}
	})
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := WithWorkRoot(secureTestContext(t, factory), root)
	out, err := runTool(ctx, NewTestRunTool(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var res testResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "fail" || res.Failed != 1 {
		t.Fatalf("res=%+v, want status=fail failed=1", res)
	}
}

// TestRunTestsStaysPassOnCleanRun guards against the reconciliation
// over-reaching: exit 0 must remain "pass" no matter what the parser saw.
func TestRunTestsStaysPassOnCleanRun(t *testing.T) {
	factory := newScriptedFactory(t, func(secproc.SecureProcessSpec) cannedResult {
		return cannedResult{Stdout: `{"Action":"pass","Package":"p","Test":"T"}` + "\n"}
	})
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := WithWorkRoot(secureTestContext(t, factory), root)
	out, err := runTool(ctx, NewTestRunTool(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var res testResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "pass" || res.Passed != 1 {
		t.Fatalf("res=%+v, want status=pass passed=1", res)
	}
}
