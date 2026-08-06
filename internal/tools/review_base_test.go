package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestReviewResolvesAllThreeBases is the first acceptance clause.
//
// reviewInput used to be {diff, task_id, repo, number} — the caller had to
// produce the diff text itself, so "supports three bases" was false even
// though git_diff had all three scopes the whole time. Reviewing a branch was
// a two-tool dance the model had to get right.
//
// The assertion is on the CONTENT each base yields, not just that a call
// succeeded: the three scopes run three different git commands, and a wiring
// that quietly resolved every base to the working tree would pass any check
// that only looked for a non-empty diff. Each base here sees a file the
// others do not.
//
// ledger: B3/V13#1 支持三种 base
func TestReviewResolvesAllThreeBases(t *testing.T) {
	root := initTempGitRepo(t)
	commitFile(t, root, "base.go", "package p\n")
	firstCommit := gitRevParse(t, root, "HEAD")

	commitFile(t, root, "second.go", "package p\n")
	secondCommit := gitRevParse(t, root, "HEAD")

	// An uncommitted edit to a TRACKED file: only the working tree sees it,
	// and unlike a new file it produces real patch text. A staged addition
	// yields an empty patch from `git diff -- path`, which would make this
	// test fail for a reason that has nothing to do with base selection.
	if err := os.WriteFile(filepath.Join(root, "base.go"),
		[]byte("package p // uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := realGitCtx(t, root)
	cases := []struct {
		name     string
		in       reviewInput
		wantFile string
		absent   string
	}{
		// Each base sees a file (or an edit) the others do not.
		{"working_tree", reviewInput{Base: "working_tree"}, "uncommitted", "second.go"},
		{"commit", reviewInput{Base: "commit", Ref: secondCommit}, "second.go", "uncommitted"},
		{"base_ref", reviewInput{Base: "base_ref", Ref: firstCommit}, "second.go", "uncommitted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diff, err := resolveReviewDiff(ctx, tc.in)
			if err != nil {
				t.Fatalf("resolve %s: %v", tc.name, err)
			}
			if !strings.Contains(diff, tc.wantFile) {
				t.Errorf("base %s did not surface %s:\n%s", tc.name, tc.wantFile, diff)
			}
			if tc.absent != "" && strings.Contains(diff, tc.absent) {
				t.Errorf("base %s surfaced %s, which belongs to a different base — "+
					"the bases are not actually distinct", tc.name, tc.absent)
			}
		})
	}
}

// TestReviewRejectsAnUnusableBase covers the argument contract.
//
// A base that needs a ref and did not get one, or a base nobody implements,
// has to fail loudly. Falling back to the working tree would review something
// the caller did not ask about and report it as the thing they did.
func TestReviewRejectsAnUnusableBase(t *testing.T) {
	root := initTempGitRepo(t)
	commitFile(t, root, "a.go", "package p\n")
	ctx := realGitCtx(t, root)

	for _, tc := range []struct {
		name string
		in   reviewInput
	}{
		{"base_ref without ref", reviewInput{Base: "base_ref"}},
		{"commit without ref", reviewInput{Base: "commit"}},
		{"unknown base", reviewInput{Base: "staged"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := resolveReviewDiff(ctx, tc.in); err == nil {
				t.Error("an unusable base was accepted; review would silently examine " +
					"something the caller did not ask for")
			}
		})
	}
}

// TestReviewPrefersAnExplicitDiff pins precedence.
//
// A caller holding a diff — a PR payload, say — is not asking for a working
// tree to be read, and reading one would be both wrong and a filesystem access
// they did not request.
//
// ledger: B3/V13#4 只读不改
func TestReviewPrefersAnExplicitDiff(t *testing.T) {
	in := reviewInput{Diff: "--- a/x\n+++ b/x\n", Base: "working_tree"}
	// No work root bound: if base were consulted, resolveReviewDiff would fail
	// and the error would surface. streamReview needs a runner too, so this
	// asserts the base branch is not taken rather than the whole pipeline.
	out, err := streamReview(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "no work root bound") {
		t.Error("an explicit diff was ignored in favour of reading the working tree")
	}
}

// gitRevParse returns the resolved sha for a rev in root.
func gitRevParse(t *testing.T, root, rev string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "rev-parse", rev).Output()
	if err != nil {
		t.Fatalf("rev-parse %s: %v", rev, err)
	}
	return strings.TrimSpace(string(out))
}
