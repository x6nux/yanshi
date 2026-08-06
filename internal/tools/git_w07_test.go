package tools

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRun(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestGitStatusHandlesTrackedSpacesAndRenames drives the porcelain-v2 branches
// the existing hostile-names fixture never reached.
//
// That fixture wrote only UNTRACKED files, so every record took the "? "
// branch. The tracked branches (types 1 and 2) had two defects nothing could
// see: a tracked path containing spaces was truncated to its last field, and a
// rename's original path — a SEPARATE NUL record, not a status record — was
// parsed as a phantom entry with a status code no file has.
//
// Counting entries is what catches the phantom: asserting only that the real
// paths appear passes with an extra bogus record sitting alongside them.
//
// ledger: B3/W07#1 status/diff 结构化
func TestGitStatusHandlesTrackedSpacesAndRenames(t *testing.T) {
	root := initTempGitRepo(t)
	commitFile(t, root, "a b.txt", "one\n")
	commitFile(t, root, "old name.txt", "two\n")

	// A tracked, space-bearing path with an uncommitted edit.
	if err := os.WriteFile(filepath.Join(root, "a b.txt"), []byte("one edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A rename whose ORIGINAL path also contains spaces.
	gitRun(t, root, "mv", "old name.txt", "new name.txt")

	out, err := runTool(realGitCtx(t, root), NewGitTools().Status, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Entries []struct {
			Path string `json:"path"`
			XY   string `json:"xy"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("status output %q: %v", out, err)
	}

	got := map[string]string{}
	for _, e := range res.Entries {
		got[e.Path] = e.XY
	}
	if _, ok := got["a b.txt"]; !ok {
		t.Errorf("a tracked path with spaces was mangled; entries=%+v", res.Entries)
	}
	if _, ok := got["new name.txt"]; !ok {
		t.Errorf("the rename destination is missing; entries=%+v", res.Entries)
	}
	// git reports exactly two changes here. Any extra entry is a phantom
	// synthesised from the rename's original-path record.
	if len(res.Entries) != 2 {
		t.Errorf("got %d entries for 2 changes: %+v", len(res.Entries), res.Entries)
	}
	if _, ok := got["old name.txt"]; ok {
		t.Error("the rename's ORIGINAL path was emitted as its own status entry")
	}
}

// TestGitToolsIsolateUserConfigObservably strengthens the isolation guard.
//
// TestGitToolsDoNotWriteGitConfig asserts the global config file is unchanged
// — true, and mutation-blind: `git status` never writes config, so deleting
// gitEnvIsolation from every spec leaves it green. Isolation is about not
// READING the operator's config either, so this plants a setting that would
// change the output and asserts it does not.
//
// The setting has to be one the command line does not already override.
// status.showUntrackedFiles was the first choice and could not fail: every
// spec passes --untracked-files=all explicitly, which wins over config no
// matter what isolation does. core.excludesFile has no such flag.
//
// It is planted in ~/.gitconfig rather than under XDG_CONFIG_HOME because that
// is the file the operator actually has, and because git reads XDG only when
// ~/.gitconfig is ABSENT — a probe that writes the XDG copy alone would be
// checking a path no real machine takes.
//
// ledger: B3/W07#2 不修改用户配置
func TestGitToolsIsolateUserConfigObservably(t *testing.T) {
	root := initTempGitRepo(t)
	commitFile(t, root, "tracked.go", "package p\n")
	if err := os.WriteFile(filepath.Join(root, "untracked.go"), []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A global ignore rule that hides the untracked file. If the tool reads
	// the operator's config, the entry below disappears.
	home := t.TempDir()
	excludes := filepath.Join(home, "ignore")
	if err := os.WriteFile(excludes, []byte("*.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"),
		[]byte("[core]\n\texcludesFile = "+filepath.ToSlash(excludes)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))

	// Positive control: the planted config really does change git's answer.
	// Without it this test cannot distinguish "isolation works" from "the
	// fixture never had any effect" — the shape the first version was.
	leaked := exec.Command("git", "-c", "core.quotepath=false", "status",
		"--porcelain=v2", "-z", "--untracked-files=all")
	leaked.Dir = root
	leaked.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home)
	raw, err := leaked.CombinedOutput()
	if err != nil {
		t.Fatalf("control git status: %v\n%s", err, raw)
	}
	if strings.Contains(string(raw), "untracked.go") {
		t.Fatalf("the fixture has no effect on git itself, so this test cannot fail: %q", raw)
	}

	out, err := runTool(realGitCtx(t, root), NewGitTools().Status, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "untracked.go") {
		t.Errorf("the operator's core.excludesFile changed the tool's output: %s\n"+
			"  git_status must not read ~/.gitconfig", out)
	}
}

// TestGitDiffSpillsLargePatchToAnArtifact is the third clause.
//
// The implementation calls writeArtifactOrSpill, and the only tests around it
// exercise the generic helper with "git-diff" as a string label — nothing goes
// through the git tool, and no fixture in git_test.go exceeds 64 KiB. A patch
// larger than a model's context is exactly what this tool produces on a real
// branch.
//
// ledger: B3/W07#3 大 diff 成 artifact
func TestGitDiffSpillsLargePatchToAnArtifact(t *testing.T) {
	root := initTempGitRepo(t)
	commitFile(t, root, "big.txt", "seed\n")
	big := strings.Repeat("a line of text that is long enough to add up quickly\n", 2000)
	if len(big) <= SpillThreshold {
		t.Fatalf("fixture is %d bytes, under the %d-byte threshold", len(big), SpillThreshold)
	}
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runTool(realGitCtx(t, root), NewGitTools().Diff, `{"scope":{"kind":"working_tree"}}`)
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Files []struct {
			Path        string `json:"path"`
			Patch       string `json:"patch"`
			ArtifactRef string `json:"artifact_ref"`
			Degraded    bool   `json:"degraded"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, f := range res.Files {
		if f.Path != "big.txt" {
			continue
		}
		found = true
		if len(f.Patch) >= len(big) {
			t.Errorf("the whole %d-byte patch was inlined; it must become a reference",
				len(f.Patch))
		}
		if f.ArtifactRef == "" && !f.Degraded {
			t.Error("a patch above SpillThreshold produced neither an artifact " +
				"reference nor a degraded marker")
		}
	}
	if !found {
		t.Fatalf("big.txt is not in the diff: %s", out)
	}

	// Negative control: a small patch stays inline, or "spills when large" is
	// satisfied by a tool that spills everything.
	small := initTempGitRepo(t)
	commitFile(t, small, "small.txt", "one\n")
	if err := os.WriteFile(filepath.Join(small, "small.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sout, err := runTool(realGitCtx(t, small), NewGitTools().Diff, `{"scope":{"kind":"working_tree"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sout, `"artifact_ref"`) {
		t.Errorf("a small patch was spilled: %s", sout)
	}
}

// TestGitDiffScopesSeeDifferentChanges replaces an assertion that could not
// distinguish the three scopes.
//
// TestGitDiffScopesBaseRefAndCommit checks `"path":"a.go"` for all three, and
// under its fixture all three answers are identical — collapsing base_ref and
// commit into working_tree leaves it green. Each scope here sees something the
// others do not.
//
// ledger: B3/W07#4 边界清晰
func TestGitDiffScopesSeeDifferentChanges(t *testing.T) {
	root := initTempGitRepo(t)
	commitFile(t, root, "base.go", "package p\n")
	first := gitRevParse(t, root, "HEAD")
	commitFile(t, root, "second.go", "package p\n")
	second := gitRevParse(t, root, "HEAD")
	if err := os.WriteFile(filepath.Join(root, "base.go"),
		[]byte("package p // uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := realGitCtx(t, root)
	cases := []struct {
		name, args, want, absent string
	}{
		{"working_tree", `{"scope":{"kind":"working_tree"}}`, "base.go", "second.go"},
		{"commit", `{"scope":{"kind":"commit","ref":"` + second + `"}}`, "second.go", "base.go"},
		{"base_ref", `{"scope":{"kind":"base_ref","ref":"` + first + `"}}`, "second.go", "base.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runTool(ctx, NewGitTools().Diff, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, `"`+tc.want+`"`) {
				t.Errorf("scope %s did not report %s: %s", tc.name, tc.want, out)
			}
			if strings.Contains(out, `"`+tc.absent+`"`) {
				t.Errorf("scope %s reported %s, which belongs to another scope — "+
					"the scopes are not distinct: %s", tc.name, tc.absent, out)
			}
		})
	}
}
