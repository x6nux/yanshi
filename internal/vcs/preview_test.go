package vcs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// commitWith writes files into the working copy, records them and commits,
// returning the new commit id. It is the shorthand every test below uses to
// build a history.
func commitWith(t *testing.T, v *VCS, repoID, root, msg string, files map[string]string) string {
	t.Helper()
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		if err := v.RecordEditMain(repoID, "test", abs, []byte(content)); err != nil {
			t.Fatalf("record %s: %v", rel, err)
		}
	}
	id, err := v.CommitMain(repoID, "test", msg)
	if err != nil {
		t.Fatalf("commit %q: %v", msg, err)
	}
	return id
}

// deleteAndCommit removes a tracked path and commits the deletion.
func deleteAndCommit(t *testing.T, v *VCS, repoID, root, msg, rel string) string {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.Remove(abs); err != nil {
		t.Fatalf("remove %s: %v", rel, err)
	}
	if err := v.RecordDeleteMain(repoID, "test", abs); err != nil {
		t.Fatalf("record delete %s: %v", rel, err)
	}
	id, err := v.CommitMain(repoID, "test", msg)
	if err != nil {
		t.Fatalf("commit delete: %v", err)
	}
	return id
}

// findChange returns the plan entry for a path, or fails.
func findChange(t *testing.T, plan *RestorePlan, path string) RestoreChange {
	t.Helper()
	for _, c := range plan.Changes {
		if c.Path == path {
			return c
		}
	}
	t.Fatalf("plan has no entry for %q; entries: %+v", path, plan.Changes)
	return RestoreChange{}
}

// TestPlanRestore_ClassifiesEveryOpAndCountsLines is the table that pins the
// preview's whole vocabulary: a path present in both trees is an overwrite, a
// path only in the target is a create, a path only in the current head is a
// delete, and an identical path is not a change at all.
func TestPlanRestore_ClassifiesEveryOpAndCountsLines(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	target := commitWith(t, v, repoID, root, "target", map[string]string{
		"keep.txt":      "same\n",
		"modified.txt":  "one\ntwo\nthree\n",
		"vanishes.txt":  "gone soon\n",
		"dir/nested.go": "package a\n",
	})
	// Move away from target: modify one, delete one, add one.
	commitWith(t, v, repoID, root, "current", map[string]string{
		"modified.txt": "one\nTWO\nthree\nfour\n",
		"added.txt":    "new file\n",
	})
	deleteAndCommit(t, v, repoID, root, "drop vanishes", "vanishes.txt")

	plan, err := v.PlanRestore(repoID, target, nil)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	cases := []struct {
		path            string
		op              RestoreOp
		added, removed  int
		before, after   int
		wantNotAChange  bool
		wantApproxFalse bool
	}{
		{path: "modified.txt", op: RestoreOverwrite, added: 1, removed: 2, before: 4, after: 3},
		{path: "vanishes.txt", op: RestoreCreate, added: 1, removed: 0, before: 0, after: 1},
		{path: "added.txt", op: RestoreDelete, added: 0, removed: 1, before: 1, after: 0},
		{path: "keep.txt", wantNotAChange: true},
		{path: "dir/nested.go", wantNotAChange: true},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if tc.wantNotAChange {
				for _, c := range plan.Changes {
					if c.Path == tc.path {
						t.Fatalf("%s already matches the target but was planned as %s", tc.path, c.Op)
					}
				}
				return
			}
			got := findChange(t, plan, tc.path)
			if got.Op != tc.op {
				t.Errorf("op = %s, want %s", got.Op, tc.op)
			}
			if got.LinesAdded != tc.added || got.LinesRemoved != tc.removed {
				t.Errorf("lines +%d/-%d, want +%d/-%d",
					got.LinesAdded, got.LinesRemoved, tc.added, tc.removed)
			}
			if got.LinesBefore != tc.before || got.LinesAfter != tc.after {
				t.Errorf("lines before/after = %d/%d, want %d/%d",
					got.LinesBefore, got.LinesAfter, tc.before, tc.after)
			}
			if got.Approx {
				t.Errorf("a %d-line diff must not be approximate", got.LinesBefore)
			}
		})
	}
	// keep.txt, dir/nested.go and the seed repo's a.txt all already hold the
	// target bytes. Counting them (rather than planning them) is what makes a
	// selective restore touch two files instead of the whole tree.
	if plan.Unchanged != 3 {
		t.Errorf("Unchanged = %d, want 3 (keep.txt + dir/nested.go + seeded a.txt)", plan.Unchanged)
	}
	if plan.ConfirmToken == "" {
		t.Error("a plan with changes must carry a confirm token")
	}
}

// TestPlanRestore_FlagsUncommittedWorkAsDirty pins the one fact a preview must
// not omit: a file edited on disk but never recorded is lost by the restore and
// no seam can bring it back.
func TestPlanRestore_FlagsUncommittedWorkAsDirty(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	target := commitWith(t, v, repoID, root, "target", map[string]string{
		"tracked.txt": "committed\n",
		"other.txt":   "stable\n",
	})
	commitWith(t, v, repoID, root, "advance", map[string]string{
		"tracked.txt": "advanced\n",
	})
	// Uncommitted edit: bytes on disk that no commit records.
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"),
		[]byte("HAND EDITED, NEVER COMMITTED\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := v.PlanRestore(repoID, target, nil)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}
	dirty := plan.DirtyPaths()
	if len(dirty) != 1 || dirty[0] != "tracked.txt" {
		t.Fatalf("DirtyPaths = %v, want [tracked.txt]", dirty)
	}
	if got := findChange(t, plan, "tracked.txt"); !got.Dirty {
		t.Error("the hand-edited path must be marked dirty")
	}
}

// TestPlanRestore_SelectorsNarrowTheTree is V4: only the matched paths appear,
// and a selector that matches nothing is an error rather than an empty plan.
func TestPlanRestore_SelectorsNarrowTheTree(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	target := commitWith(t, v, repoID, root, "target", map[string]string{
		"src/a.go":  "package a\n",
		"src/b.go":  "package b\n",
		"docs/x.md": "# x\n",
	})
	commitWith(t, v, repoID, root, "break everything", map[string]string{
		"src/a.go":  "BROKEN A\n",
		"src/b.go":  "BROKEN B\n",
		"docs/x.md": "BROKEN X\n",
	})

	tests := []struct {
		name      string
		selectors []string
		want      []string
		wantErr   string
	}{
		{name: "one dir", selectors: []string{"src/*"}, want: []string{"src/a.go", "src/b.go"}},
		{name: "one file", selectors: []string{"src/a.go"}, want: []string{"src/a.go"}},
		{name: "extension across tree", selectors: []string{"**/*.md"}, want: []string{"docs/x.md"}},
		{name: "two selectors", selectors: []string{"src/a.go", "docs/*"},
			want: []string{"docs/x.md", "src/a.go"}},
		{name: "nil means everything", selectors: nil,
			want: []string{"docs/x.md", "src/a.go", "src/b.go"}},
		{name: "typo is rejected", selectors: []string{"srcc/*"}, wantErr: "matched no tracked path"},
		{name: "one good one bad", selectors: []string{"src/*", "nope/*"},
			wantErr: "matched no tracked path"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := v.PlanRestore(repoID, target, tc.selectors)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("PlanRestore: %v", err)
			}
			var got []string
			for _, c := range plan.Changes {
				got = append(got, c.Path)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("planned %v, want %v", got, tc.want)
			}
		})
	}
}

// TestApplyRestore_OnlyTouchesSelectedPaths is the V4 acceptance test: the
// selected file goes back, everything else stays where it was.
func TestApplyRestore_OnlyTouchesSelectedPaths(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	target := commitWith(t, v, repoID, root, "good", map[string]string{
		"src/a.go": "package a // good\n",
		"src/b.go": "package b // good\n",
	})
	commitWith(t, v, repoID, root, "bad", map[string]string{
		"src/a.go": "package a // BROKEN\n",
		"src/b.go": "package b // BROKEN\n",
	})

	plan, err := v.PlanRestore(repoID, target, []string{"src/a.go"})
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}
	if _, err := v.ApplyRestore(repoID, target, []string{"src/a.go"}, plan.ConfirmToken); err != nil {
		t.Fatalf("ApplyRestore: %v", err)
	}
	mustFile(t, filepath.Join(root, "src", "a.go"), "package a // good\n")
	mustFile(t, filepath.Join(root, "src", "b.go"), "package b // BROKEN\n")

	// V4 tracks the restored path so the next commit reflects it. Without this
	// the restore would be invisible to history and the next commit could
	// silently re-apply the broken version.
	if pending := v.Uncommitted("main", repoID); pending["src/a.go"] == "" {
		t.Errorf("selective restore did not enter the pending changeset: %v", pending)
	}
}

// TestApplyRestore_RefusesAStaleToken pins the confirm handshake: a plan
// invalidated by a commit between preview and apply must fail rather than
// silently act on the newer state.
func TestApplyRestore_RefusesAStaleToken(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	target := commitWith(t, v, repoID, root, "target", map[string]string{"f.txt": "v1\n"})
	commitWith(t, v, repoID, root, "advance", map[string]string{"f.txt": "v2\n"})

	plan, err := v.PlanRestore(repoID, target, nil)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}
	// The world moves between the preview and the confirmation.
	commitWith(t, v, repoID, root, "another edit", map[string]string{"g.txt": "added\n"})

	_, err = v.ApplyRestore(repoID, target, nil, plan.ConfirmToken)
	if !errors.Is(err, ErrPlanStale) {
		t.Fatalf("err = %v, want ErrPlanStale", err)
	}
	mustFile(t, filepath.Join(root, "f.txt"), "v2\n")
	mustFile(t, filepath.Join(root, "g.txt"), "added\n")
}

// TestApplyRestore_RejectsAFabricatedToken proves the token is not decorative:
// an apply cannot be driven without a matching preview.
func TestApplyRestore_RejectsAFabricatedToken(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	target := commitWith(t, v, repoID, root, "target", map[string]string{"f.txt": "v1\n"})
	commitWith(t, v, repoID, root, "advance", map[string]string{"f.txt": "v2\n"})

	for _, token := range []string{"", "deadbeef", strings.Repeat("0", 64)} {
		if _, err := v.ApplyRestore(repoID, target, nil, token); !errors.Is(err, ErrPlanStale) {
			t.Errorf("token %q: err = %v, want ErrPlanStale", token, err)
		}
	}
	mustFile(t, filepath.Join(root, "f.txt"), "v2\n")
}

// TestPreviewAndRevertAgreeOnEveryPath is the anti-drift test for V1: the
// preview and the revert are required to be the SAME computation, so the set of
// paths the preview names must equal the set the revert actually changes.
func TestPreviewAndRevertAgreeOnEveryPath(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	commitWith(t, v, repoID, root, "target", map[string]string{
		"a.txt":     "A1\n",
		"b.txt":     "B1\n",
		"gone.txt":  "will be deleted later\n",
		"deep/c.md": "C1\n",
	})
	seamID, err := v.SealMainTurnSeam(repoID, "s1", 0, 0, SeamPreTurn, "pre")
	if err != nil {
		t.Fatalf("SealMainTurnSeam: %v", err)
	}
	commitWith(t, v, repoID, root, "diverge", map[string]string{
		"a.txt":     "A2\n",
		"deep/c.md": "C2\n",
		"new.txt":   "did not exist at the seam\n",
	})
	deleteAndCommit(t, v, repoID, root, "delete gone", "gone.txt")

	plan, err := v.PlanRevertToSeam(repoID, seamID)
	if err != nil {
		t.Fatalf("PlanRevertToSeam: %v", err)
	}
	predicted := map[string]RestoreOp{}
	for _, c := range plan.Changes {
		predicted[c.Path] = c.Op
	}

	before := snapshotTree(t, root)
	if _, err := v.RevertToSeam(repoID, seamID, "test", 0, 0, nil); err != nil {
		t.Fatalf("RevertToSeam: %v", err)
	}
	after := snapshotTree(t, root)

	observed := map[string]RestoreOp{}
	for path, content := range before {
		switch newContent, still := after[path]; {
		case !still:
			observed[path] = RestoreDelete
		case newContent != content:
			observed[path] = RestoreOverwrite
		}
	}
	for path := range after {
		if _, existed := before[path]; !existed {
			observed[path] = RestoreCreate
		}
	}
	if len(predicted) != len(observed) {
		t.Fatalf("preview named %d paths, revert changed %d\npreview: %v\nactual: %v",
			len(predicted), len(observed), predicted, observed)
	}
	for path, op := range observed {
		if predicted[path] != op {
			t.Errorf("%s: preview said %q, revert did %q", path, predicted[path], op)
		}
	}
}

// TestRevertDoesNotRewriteFilesThatAlreadyMatch is the anti-drift test with
// teeth.
//
// TestPreviewAndRevertAgreeOnEveryPath compares final CONTENT, and content is
// the one thing the old tree-walking materializer and the shared plan already
// agreed on — so it passes even when RevertToSeam is put back on its own
// walker, which is precisely the regression it was meant to catch. (Measured:
// swapping applyRestorePlanLocked for materializeMainLocked leaves it green.)
//
// The plan's distinguishing behaviour is what it does NOT do. planOnePath drops
// a path whose on-disk bytes already equal the target, so those files are never
// opened. The old walker resolved every blob in the target tree and rewrote
// every one of them, unconditionally. Modification time makes that difference
// observable — and it is not a cosmetic difference: rewriting an entire tree on
// every rollback invalidates every mtime-based build cache in the project,
// which for a goal loop that rolls back often is most of its wall-clock cost.
func TestRevertDoesNotRewriteFilesThatAlreadyMatch(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	commitWith(t, v, repoID, root, "target", map[string]string{
		"changes.txt":   "before\n",
		"unchanged.txt": "constant across the whole test\n",
		"deep/also.txt": "also constant\n",
	})
	seamID, err := v.SealMainTurnSeam(repoID, "s1", 0, 0, SeamPreTurn, "pre")
	if err != nil {
		t.Fatal(err)
	}
	commitWith(t, v, repoID, root, "diverge", map[string]string{
		"changes.txt": "after\n",
	})

	// Backdate the untouched files well past filesystem timestamp granularity,
	// so "was it rewritten?" is a reliable question.
	old := time.Now().Add(-2 * time.Hour)
	untouched := []string{"unchanged.txt", "deep/also.txt"}
	for _, rel := range untouched {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.Chtimes(abs, old, old); err != nil {
			t.Fatalf("chtimes %s: %v", rel, err)
		}
	}

	if _, err := v.RevertToSeam(repoID, seamID, "test", 0, 0, nil); err != nil {
		t.Fatalf("RevertToSeam: %v", err)
	}

	mustFile(t, filepath.Join(root, "changes.txt"), "before\n")
	for _, rel := range untouched {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Stat(abs)
		if err != nil {
			t.Fatalf("stat %s: %v", rel, err)
		}
		if info.ModTime().After(old.Add(time.Minute)) {
			t.Errorf("%s already held the target bytes but was rewritten "+
				"(mtime moved to %v); the revert is not running the preview's plan",
				rel, info.ModTime())
		}
	}
}

// TestRevertDoesNotLeavePendingEdits pins the track=false decision in
// applyRestorePlanLocked: after a revert the working copy already matches the
// new head, so a pending changeset would make the next commit a self-referential
// no-op delta.
func TestRevertDoesNotLeavePendingEdits(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	commitWith(t, v, repoID, root, "target", map[string]string{"f.txt": "v1\n"})
	seamID, err := v.SealMainTurnSeam(repoID, "s1", 0, 0, SeamPreTurn, "pre")
	if err != nil {
		t.Fatal(err)
	}
	commitWith(t, v, repoID, root, "advance", map[string]string{"f.txt": "v2\n"})

	if _, err := v.RevertToSeam(repoID, seamID, "test", 0, 0, nil); err != nil {
		t.Fatalf("RevertToSeam: %v", err)
	}
	if pending := v.Uncommitted("main", repoID); len(pending) != 0 {
		t.Errorf("revert left pending edits: %v", pending)
	}
	mustFile(t, filepath.Join(root, "f.txt"), "v1\n")
}

// TestDiffLineCounts is the unit table for the line arithmetic. It exists
// separately from the plan tests because the counts are what an operator reads
// to decide, and an off-by-one here is invisible in an end-to-end assertion.
func TestDiffLineCounts(t *testing.T) {
	tests := []struct {
		name           string
		before, after  string
		added, removed int
		approx         bool
	}{
		{name: "identical", before: "a\nb\n", after: "a\nb\n"},
		{name: "append one", before: "a\n", after: "a\nb\n", added: 1},
		{name: "remove one", before: "a\nb\n", after: "a\n", removed: 1},
		{name: "replace middle", before: "a\nb\nc\n", after: "a\nB\nc\n", added: 1, removed: 1},
		{name: "empty to content", before: "", after: "a\nb\n", added: 2},
		{name: "content to empty", before: "a\nb\n", after: "", removed: 2},
		{name: "no trailing newline", before: "a\nb", after: "a\nb\nc", added: 1},
		{name: "reorder is add+remove", before: "a\nb\n", after: "b\na\n", added: 1, removed: 1},
		{
			name:   "huge unrelated files degrade to approximate",
			before: strings.Repeat("old line\n", maxExactDiffLines+1),
			after:  strings.Repeat("new line\n", maxExactDiffLines+1),
			added:  maxExactDiffLines + 1, removed: maxExactDiffLines + 1, approx: true,
		},
		{
			// The trim is what keeps the DP small: a one-line edit inside a huge
			// file must still be exact.
			name:   "one line changed inside a huge file stays exact",
			before: strings.Repeat("same\n", maxExactDiffLines*2) + "old\n",
			after:  strings.Repeat("same\n", maxExactDiffLines*2) + "new\n",
			added:  1, removed: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			added, removed, approx := diffLineCounts([]byte(tc.before), []byte(tc.after))
			if added != tc.added || removed != tc.removed || approx != tc.approx {
				t.Errorf("got +%d/-%d approx=%v, want +%d/-%d approx=%v",
					added, removed, approx, tc.added, tc.removed, tc.approx)
			}
		})
	}
}

// TestCountLines pins the editor-style counting the preview reports.
func TestCountLines(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{in: "", want: 0},
		{in: "\n", want: 1},
		{in: "a", want: 1},
		{in: "a\n", want: 1},
		{in: "a\nb", want: 2},
		{in: "a\nb\n", want: 2},
		{in: "a\n\nb\n", want: 3},
	}
	for _, tc := range tests {
		if got := countLines([]byte(tc.in)); got != tc.want {
			t.Errorf("countLines(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// mustFile asserts a file's exact content.
func mustFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
}

// snapshotTree reads every regular file under root into a repo-relative map.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !info.Mode().IsRegular() {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}
