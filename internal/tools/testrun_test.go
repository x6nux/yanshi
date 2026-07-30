package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
)

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
