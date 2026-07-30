// Command testchanged runs go test only on packages with changed .go files.
//
// Usage:
//
//	go run ./cmd/testchanged            # test changed packages
//	go run ./cmd/testchanged -v          # with verbose output
//	go run ./cmd/testchanged -run Foo    # with test filter
//
// "Changed" means either modified (tracked, staged or unstaged) or untracked
// .go files since HEAD. If there are no commits yet, it falls back to all
// untracked Go files.
//
// For the full suite (all packages, caching applies), use: go test ./...
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// findChanged is the changed-file detector, a var so tests can inject a fixed
// file list (the real detector shells out to git and reflects the live working
// tree, which is not deterministic).
var findChanged = findChangedGoFiles

// runGoTest executes `go test` with args, streaming to stdout/stderr. It is a
// var so tests can inject a no-op runner instead of actually running the suite.
var runGoTest = func(args []string, stdout, stderr io.Writer) error {
	cmd := exec.Command("go", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// run is the testable core of testchanged: it finds changed .go files, derives
// the changed packages, and runs `go test` on them. Returns the exit code
// (0 ok / 1 go-test failure). Split from main so every branch (no-changed,
// no-packages, run) is unit-testable via the findChanged/runGoTest seams.
func run(extraArgs []string, stdout, stderr io.Writer) int {
	changed := findChanged()
	if len(changed) == 0 {
		fmt.Fprintln(stdout, "No changed Go files found.")
		fmt.Fprintln(stdout, "Use 'go test ./...' for the full suite (caching applies).")
		return 0
	}

	pkgs := uniqueDirs(changed)
	if len(pkgs) == 0 {
		fmt.Fprintln(stdout, "No changed Go packages found.")
		return 0
	}

	args := []string{"test"}
	args = append(args, extraArgs...)
	args = append(args, pkgs...)

	fmt.Fprintf(stdout, "Testing changed packages: %s\n\n", strings.Join(pkgs, " "))

	if err := runGoTest(args, stdout, stderr); err != nil {
		return 1
	}
	return 0
}

// findChangedGoFiles returns all .go files that are modified or untracked.
func findChangedGoFiles() []string {
	// git diff --name-only HEAD for tracked files (staged+unstaged)
	files := runGit("diff", "--name-only", "HEAD")
	// Also include untracked Go files that aren't gitignored
	untracked := runGit("ls-files", "--others", "--exclude-standard")
	files = append(files, untracked...)

	var gos []string
	for _, f := range files {
		f = strings.TrimSpace(f)
		if strings.HasSuffix(f, ".go") && f != "" {
			gos = append(gos, f)
		}
	}
	return gos
}

func runGit(args ...string) []string {
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// uniqueDirs extracts unique directory paths from a list of file paths and
// returns them as Go package references (./dir/subdir). Dirs that are not
// valid Go packages (e.g. test_scratch with stray .go files) are excluded.
func uniqueDirs(files []string) []string {
	set := map[string]bool{}
	for _, f := range files {
		dir := filepath.ToSlash(filepath.Dir(f))
		set[dir] = true
	}

	// Validate each dir with go list so we skip non-package directories.
	dirs := make([]string, 0, len(set))
	for d := range set {
		pkg := "./" + d
		if isGoPackage(pkg) {
			dirs = append(dirs, pkg)
		}
	}
	sort.Strings(dirs)
	return dirs
}

// isGoPackage reports whether the given Go package reference exists and is
// a valid package (not a main module without package files).
func isGoPackage(pkg string) bool {
	cmd := exec.Command("go", "list", pkg)
	cmd.Stderr = nil // swallow errors for non-package dirs
	if err := cmd.Run(); err != nil {
		return false
	}
	// go list -f '{{.Name}}' could check for non-empty, but plain go list
	// succeeds only for valid packages, so a nil err is sufficient.
	return true
}
