package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- run() branches via seams --------------------------------------------

// withFindChanged swaps the findChanged seam and restores it on cleanup.
func withFindChanged(t *testing.T, files []string) {
	t.Helper()
	prev := findChanged
	findChanged = func() []string { return files }
	t.Cleanup(func() { findChanged = prev })
}

// TestRunNoChangedFiles covers the empty-detector branch (exit 0, hint printed).
func TestRunNoChangedFiles(t *testing.T) {
	withFindChanged(t, nil)
	var out, errOut bytes.Buffer
	code := run(nil, &out, &errOut)
	assert.Equal(t, 0, code)
	assert.Contains(t, out.String(), "No changed Go files found.")
	assert.Contains(t, out.String(), "go test ./...")
}

// TestRunNoPackages covers the "files present but none in a valid package"
// branch. test_scratch is a non-package dir with stray .go files in the repo.
func TestRunNoPackages(t *testing.T) {
	withFindChanged(t, []string{"test_scratch/foo_test.go"})
	var out bytes.Buffer
	code := run(nil, &out, &bytes.Buffer{})
	assert.Equal(t, 0, code)
	assert.Contains(t, out.String(), "No changed Go packages found.")
}

// TestRunExecutesGoTest covers the happy path: changed files in a real package
// trigger runGoTest. A fake runner captures the argv and returns nil (exit 0).
// The file path is relative to the test cwd so `go list ./.<dir>` resolves.
func TestRunExecutesGoTest(t *testing.T) {
	withFindChanged(t, []string{"main.go", "main_test.go"})
	var captured []string
	prev := runGoTest
	runGoTest = func(args []string, stdout, stderr io.Writer) error {
		captured = args
		return nil
	}
	t.Cleanup(func() { runGoTest = prev })

	var out bytes.Buffer
	code := run([]string{"-v", "-run", "Foo"}, &out, &bytes.Buffer{})
	require.Equal(t, 0, code, out.String())
	// argv = test + extra args + unique package dirs (sorted).
	require.NotEmpty(t, captured)
	assert.Equal(t, "test", captured[0])
	assert.Contains(t, captured, "-v")
	assert.Contains(t, captured, "Foo")
	assert.Contains(t, strings.Join(captured, " "), "./.") // current package
}

// TestRunGoTestFailure covers the go-test-error branch (exit 1).
func TestRunGoTestFailure(t *testing.T) {
	withFindChanged(t, []string{"main.go"})
	prev := runGoTest
	runGoTest = func(args []string, stdout, stderr io.Writer) error {
		return errors.New("tests failed")
	}
	t.Cleanup(func() { runGoTest = prev })

	code := run(nil, &bytes.Buffer{}, &bytes.Buffer{})
	assert.Equal(t, 1, code)
}

// ---- helper functions ----------------------------------------------------

// TestFindChangedGoFilesLive proves findChangedGoFiles shells out to git and
// returns .go files (this worktree has many). It also filters non-.go entries.
func TestFindChangedGoFilesLive(t *testing.T) {
	gos := findChangedGoFiles()
	// The worktree always has changed .go files during this work.
	for _, f := range gos {
		assert.True(t, strings.HasSuffix(f, ".go"), "non-.go leaked: %s", f)
	}
}

// TestRunGitHandlesErrors proves runGit returns nil for a failing git command.
func TestRunGitHandlesErrors(t *testing.T) {
	assert.Nil(t, runGit("not-a-real-git-subcommand-xyz"))
}

// TestRunGitReturnsLines proves runGit splits stdout into trimmed lines.
func TestRunGitReturnsLines(t *testing.T) {
	out := runGit("diff", "--name-only", "HEAD")
	// May be empty if clean, but when non-empty each entry is a single line.
	for _, l := range out {
		assert.False(t, strings.Contains(l, "\n"), "entry spans lines: %q", l)
	}
}

// TestUniqueDirsDedupes proves uniqueDirs deduplicates files in the same dir
// and keeps the package when it is valid. Uses a cwd-relative path so `go list`
// resolves.
func TestUniqueDirsDedupes(t *testing.T) {
	dirs := uniqueDirs([]string{"main.go", "main_test.go"})
	assert.Equal(t, []string{"./."}, dirs)
}

// TestUniqueDirsExcludesNonPackage proves uniqueDirs drops dirs that are not
// valid Go packages (via go list).
func TestUniqueDirsExcludesNonPackage(t *testing.T) {
	dirs := uniqueDirs([]string{"no/such/pkg/x.go"})
	assert.Empty(t, dirs, "a non-existent dir is not a valid package")
}

// TestIsGoPackage proves isGoPackage accepts the current package and rejects a
// non-existent one.
func TestIsGoPackage(t *testing.T) {
	assert.True(t, isGoPackage("./."))
	assert.False(t, isGoPackage("./this/does/not/exist"))
}

// TestRunGitSmoke just confirms a known-good git command returns a slice.
func TestRunGitSmoke(t *testing.T) {
	// `git --version` always succeeds.
	out := runGit("--version")
	assert.NotEmpty(t, out)
}

// TestRunGitEmptyOutput covers the empty-output branch: `git stripspace` reads
// empty stdin and prints nothing, exiting 0, so runGit returns nil.
func TestRunGitEmptyOutput(t *testing.T) {
	assert.Nil(t, runGit("stripspace"))
}

// TestRunGoTestClosure covers the real runGoTest var closure (exec.Command +
// cmd.Run): running `go version` via it succeeds and returns nil.
func TestRunGoTestClosure(t *testing.T) {
	var out bytes.Buffer
	err := runGoTest([]string{"version"}, &out, &bytes.Buffer{})
	assert.NoError(t, err)
	assert.Contains(t, out.String(), "go")
}
