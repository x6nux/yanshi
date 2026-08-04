package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/secproc"
)

func initTempGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return root
}

func commitFile(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", "--", path)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "commit", "--quiet", "-m", "add "+path)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
}

// realGitFactory runs git through the OS — used for status/diff integration tests.
type realGitFactory struct{}

func (realGitFactory) Start(ctx context.Context, spec secproc.SecureProcessSpec) (*secproc.StartedProcess, error) {
	cmd := exec.CommandContext(ctx, spec.Program, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = append(os.Environ(), spec.Env...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &secproc.StartedProcess{Wait: cmd.Wait, PID: cmd.Process.Pid, Stdout: stdout, Stderr: stderr}, nil
}

func realGitCtx(t *testing.T, root string) context.Context {
	return WithWorkRoot(secureTestContext(t, realGitFactory{}), root)
}

func TestGitStatusParsesPorcelainV2ZWithHostileNames(t *testing.T) {
	root := initTempGitRepo(t)
	names := []string{"a b.txt", "你好.go"}
	if runtime.GOOS != "windows" {
		names = append(names, "tab\tname.txt")
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := runTool(realGitCtx(t, root), NewGitTools().Status, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		needle := strings.ReplaceAll(strings.ReplaceAll(name, `"`, `\"`), "\t", "\\t")
		if !strings.Contains(out, needle) {
			t.Fatalf("out=%s missing %q", out, name)
		}
	}
}

func TestGitDiffReturnsOneRecordPerFileWithBinaryMarker(t *testing.T) {
	root := initTempGitRepo(t)
	commitFile(t, root, "a b.txt", "old\n")
	commitFile(t, root, "bin.dat", string([]byte{0, 1, 2}))
	if err := os.WriteFile(filepath.Join(root, "a b.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin.dat"), []byte{0, 9, 2}, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runTool(realGitCtx(t, root), NewGitTools().Diff, `{"scope":{"kind":"working_tree"}}`)
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Files []struct {
			Path   string `json:"path"`
			Binary bool   `json:"binary"`
			Patch  string `json:"patch"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("out=%q: %v", out, err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("files=%+v", res.Files)
	}
	byPath := map[string]int{}
	for i, f := range res.Files {
		byPath[f.Path] = i
	}
	idxText, ok1 := byPath["a b.txt"]
	idxBin, ok2 := byPath["bin.dat"]
	if !ok1 || !ok2 {
		t.Fatalf("files=%+v", res.Files)
	}
	if res.Files[idxText].Binary || res.Files[idxText].Patch == "" {
		t.Fatalf("text file missing patch: %+v", res.Files[idxText])
	}
	if !res.Files[idxBin].Binary || res.Files[idxBin].Patch != "" {
		t.Fatalf("binary file should not include patch: %+v", res.Files[idxBin])
	}
}

func TestGitDiffWorkingTreeIncludesStagedUnstagedUntracked(t *testing.T) {
	root := initTempGitRepo(t)
	commitFile(t, root, "committed.go", "package p\n")
	if err := os.WriteFile(filepath.Join(root, "staged.go"), []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "add", "--", "staged.go").CombinedOutput(); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(root, "committed.go"), []byte("package p // edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "untracked.go"), []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runTool(realGitCtx(t, root), NewGitTools().Diff, `{"scope":{"kind":"working_tree"}}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"staged.go", "committed.go", "untracked.go"} {
		if !strings.Contains(out, want) {
			t.Fatalf("working_tree missing %s\n%s", want, out)
		}
	}
}

func TestGitDiffEmptyReturnsNoFiles(t *testing.T) {
	root := initTempGitRepo(t)
	out, err := runTool(realGitCtx(t, root), NewGitTools().Diff, `{"scope":{"kind":"working_tree"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"files":[]`) {
		t.Fatalf("out=%s", out)
	}
}

func TestGitDiffRejectsPathEscape(t *testing.T) {
	root := initTempGitRepo(t)
	// runGitDiff returns ("", fmt.Errorf("path ... outside work root")); SyncStream
	// packages that as a ToolChunk with Err set, and InvokableRun converts non-
	// circuit-breaker errors into the result string (err==nil). Assert on `out`.
	out, err := runTool(realGitCtx(t, root), NewGitTools().Diff, `{"scope":{"kind":"working_tree"},"paths":["../escape"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "outside work root") {
		t.Fatalf("out=%s", out)
	}
}

func TestGitDiffScopesBaseRefAndCommit(t *testing.T) {
	root := initTempGitRepo(t)
	commitFile(t, root, "a.go", "package a\n")
	commitFile(t, root, "a.go", "package a // v2\n")
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a // v2 edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, args string }{
		{"working_tree", `{"scope":{"kind":"working_tree"}}`},
		{"base_ref", `{"scope":{"kind":"base_ref","ref":"HEAD~1"}}`},
		{"commit", `{"scope":{"kind":"commit","ref":"HEAD"}}`},
	} {
		out, err := runTool(realGitCtx(t, root), NewGitTools().Diff, tc.args)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !strings.Contains(out, `"path":"a.go"`) {
			t.Fatalf("%s: out=%s", tc.name, out)
		}
	}
}

func TestGitToolsDoNotWriteGitConfig(t *testing.T) {
	home := t.TempDir()
	global := filepath.Join(home, "global.gitconfig")
	original := []byte("[user]\n\tname = sentinel\n")
	if err := os.WriteFile(global, original, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	root := initTempGitRepo(t)
	commitFile(t, root, "a.go", "package a\n")
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a // edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _ = runTool(realGitCtx(t, root), NewGitTools().Status, `{}`)
	_, _ = runTool(realGitCtx(t, root), NewGitTools().Diff, `{"scope":{"kind":"working_tree"}}`)
	got, err := os.ReadFile(global)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("global config mutated:\nwant=%q\ngot=%q", original, got)
	}
}

// gitCallKind classifies a git invocation into one of the call sites git.go
// makes, so a scripted factory can fail exactly one of them and let the rest
// succeed. Both numstat and the patch builder go through `git show` in commit
// scope, so --numstat is what separates them.
func gitCallKind(spec secproc.SecureProcessSpec) string {
	has := func(tok string) bool {
		for _, a := range spec.Args {
			if a == tok {
				return true
			}
		}
		return false
	}
	switch {
	case has("status"):
		return "status"
	case has("ls-files"):
		return "ls-files"
	case has("--numstat") && has("--cached"):
		return "diff-cached-numstat"
	case has("--numstat") && has("show"):
		return "show-numstat"
	case has("--numstat"):
		return "diff-numstat"
	case has("show"):
		return "show-patch"
	default:
		return "diff-patch"
	}
}

// TestGitToolsReportFailureInsteadOfEmptyResult is the regression test for
// git.go reading only res.Stdout and never res.ExitCode.
//
// Every failure git can hit — not a repository, corrupt index, unreadable
// object, ambiguous ref, permission denied — writes its reason to stderr,
// exits non-zero and leaves stdout EMPTY. The parsers happily turn an empty
// stdout into zero entries, so git_status answered {"entries":null}: a clean
// working tree for a repository it could not read at all, and git_diff
// answered {"files":[]}. The model then proceeds as if there were no changes.
//
// Each of git.go's call sites gets its own case, because each one had to be
// fixed separately and each one alone is enough to fabricate an empty answer.
func TestGitToolsReportFailureInsteadOfEmptyResult(t *testing.T) {
	// Canned success output for the call sites that are NOT under test in a
	// given case, so the run reaches the one that is.
	stdoutFor := map[string]string{
		"status":              "",
		"diff-numstat":        "1\t0\tfile.go\x00",
		"diff-cached-numstat": "",
		"ls-files":            "",
		"show-numstat":        "1\t0\tfile.go\x00",
		"diff-patch":          "diff --git a/file.go b/file.go\n+x\n",
		"show-patch":          "diff --git a/file.go b/file.go\n+x\n",
	}
	for _, tc := range []struct {
		failing string
		tool    string
		args    string
		stderr  string
	}{
		{"status", "git_status", `{}`, "fatal: not a git repository (or any of the parent directories): .git"},
		{"diff-numstat", "git_diff", `{"scope":{"kind":"working_tree"}}`, "error: bad index file sha1 signature"},
		{"diff-cached-numstat", "git_diff", `{"scope":{"kind":"working_tree"}}`, "fatal: unable to read index file"},
		{"ls-files", "git_diff", `{"scope":{"kind":"working_tree"}}`, "fatal: could not open directory: Permission denied"},
		{"diff-patch", "git_diff", `{"scope":{"kind":"working_tree"}}`, "error: unable to read sha1 file of file.go"},
		{"show-numstat", "git_diff", `{"scope":{"kind":"commit","ref":"deadbeef"}}`, "fatal: ambiguous argument 'deadbeef': unknown revision"},
		{"show-patch", "git_diff", `{"scope":{"kind":"commit","ref":"HEAD"}}`, "fatal: unable to read tree object"},
	} {
		t.Run(tc.failing, func(t *testing.T) {
			factory := newScriptedFactory(t, func(spec secproc.SecureProcessSpec) cannedResult {
				kind := gitCallKind(spec)
				if kind == tc.failing {
					return cannedResult{Stderr: tc.stderr, ExitCode: 128}
				}
				return cannedResult{Stdout: stdoutFor[kind]}
			})
			ctx := WithWorkRoot(secureTestContext(t, factory), t.TempDir())
			tl := NewGitTools().Status
			if tc.tool == "git_diff" {
				tl = NewGitTools().Diff
			}
			out, err := runTool(ctx, tl, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(out, "✗ ") {
				t.Fatalf("%s exited 128 but %s returned a success result: %s", tc.failing, tc.tool, out)
			}
			if !strings.Contains(out, tc.stderr) {
				t.Fatalf("result %q must carry git's own failure text %q — stderr is the only place the reason exists", out, tc.stderr)
			}
		})
	}
}

// TestGitDiffSucceedsWhenGitExitsZeroWithNoOutput guards the other direction:
// an empty diff is a legitimate exit-0 answer (nothing changed), and the exit
// code check must not turn it into an error.
func TestGitDiffSucceedsWhenGitExitsZeroWithNoOutput(t *testing.T) {
	factory := newScriptedFactory(t, func(secproc.SecureProcessSpec) cannedResult {
		return cannedResult{}
	})
	ctx := WithWorkRoot(secureTestContext(t, factory), t.TempDir())
	out, err := runTool(ctx, NewGitTools().Diff, `{"scope":{"kind":"working_tree"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"files":[]`) {
		t.Fatalf("clean tree must stay a success, got %s", out)
	}
}
