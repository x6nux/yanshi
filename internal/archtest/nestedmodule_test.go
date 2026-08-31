// nestedmodule_test.go — RE-26: a nested module's tests are invisible to the
// root module's `go test ./...`, so they need their own CI step or they never
// run at all.
//
// third_party/bubbletea carries its own go.mod (the root go.mod points at it
// with a replace directive). That makes it a SEPARATE module: `go list ./...`
// in the root does not mention it, `go test ./...` cannot reach it, and
// `go test ./third_party/bubbletea` fails outright with "main module does not
// contain package". Every test in that fork had therefore never executed in
// CI — including the sole automated guard on the one behaviour CLAUDE.md names
// as the fork's reason to exist (telling Ctrl+Enter from Enter on Windows).
//
// The failure mode is silent in the worst way: the tests exist, they pass when
// run by hand, and a reviewer counting "does this have a test?" gets yes. What
// no one gets is a signal when they break. This gate turns that into a
// verdict — the CI step that runs the nested module is now load-bearing, and
// deleting it fails here rather than in six months on a Windows user's
// keyboard.
package archtest

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// nestedModulesNotTestedInCI records nested modules that deliberately have no
// CI test step, one reason each. This is a debt-table shape and both
// directions fail: an entry whose go.mod is gone is dead and must be deleted,
// and an entry that DOES get a CI step is no longer an exemption.
var nestedModulesNotTestedInCI = map[string]string{
	"test_scratch": "Deliberately not a compilable package — it is the fixture " +
		"cmd/testchanged's own test uses to prove `go list` filtering drops " +
		"non-package directories (its foo_test.go has escaped quotes and does " +
		"not build). Running `go test` there would fail by construction.",
}

// ciWorkflowPath is the workflow whose steps this gate reads. The nested-module
// step lives on the existing `test` matrix job rather than a job of its own,
// because the fork's load-bearing divergence from upstream is Windows-only and
// needs the same ubuntu/windows/macos legs the main suite already has.
const ciWorkflowPath = ".github/workflows/ci.yml"

// nestedGoModDirs returns every directory below the module root (excluding the
// root itself) that declares its own go.mod, slash-separated and relative.
func nestedGoModDirs(t *testing.T) []string {
	t.Helper()
	root := moduleRoot(t)
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := short(path, root)
		if d.IsDir() {
			if path != root && isNestedCheckoutDir(rel, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() != "go.mod" || rel == "go.mod" {
			return nil
		}
		out = append(out, strings.TrimSuffix(rel, "/go.mod"))
		return nil
	})
	if err != nil {
		t.Fatalf("walk for nested go.mod: %v", err)
	}
	return out
}

// ciRunCommands parses the workflow and returns the `run:` script of every
// step of every job — the commands CI actually executes, and nothing else.
//
// This is parsed rather than string-matched against the file, and that choice
// is the whole point of the helper. A `#` is how a YAML step gets disabled;
// it is the first thing anyone reaches for to skip a step "just for this one
// push". A gate that reads the workflow as one string and asks whether the
// command appears in it answers yes for a commented-out step — it protects
// against DELETING the step while ignoring the far more likely way the step
// stops running. Measured on the real file: deleting the line failed the gate,
// prefixing the same line with `#` passed it.
//
// The reverse direction matters too and comes free here: prose in a comment
// that merely quotes the command (an explanation, a TODO, this very sentence
// in some future edit) can no longer satisfy the gate, because a comment is
// not a step and never reaches this slice.
//
// This exact defect — a gate reading source as text, counting an occurrence,
// and passing on a commented-out occurrence — was already found and fixed once
// in this repo, on the gate guarding composition-root wiring. Reintroducing it
// in the gate whose entire job is "these tests must actually run" would be the
// same mistake twice.
func ciRunCommands(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(abs(ciWorkflowPath))
	if err != nil {
		t.Fatalf("read %s: %v", ciWorkflowPath, err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", ciWorkflowPath, err)
	}
	var out []string
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			if strings.TrimSpace(step.Run) != "" {
				out = append(out, step.Run)
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s parsed to zero `run:` steps — the gate below would pass "+
			"vacuously for every nested module; the workflow's shape changed",
			ciWorkflowPath)
	}
	return out
}

// TestNestedModulesRunInCI requires every nested module to be either driven by
// a `go test -C <dir>` step in the CI workflow or listed, with a reason, in
// nestedModulesNotTestedInCI.
func TestNestedModulesRunInCI(t *testing.T) {
	runs := ciRunCommands(t)

	dirs := nestedGoModDirs(t)
	if len(dirs) == 0 {
		t.Fatalf("no nested go.mod found — this gate would pass vacuously; " +
			"if the last nested module really is gone, delete this file and its " +
			"CI step together")
	}

	seen := map[string]bool{}
	for _, dir := range dirs {
		seen[dir] = true
		// `go test -C <dir> ./...` is the only invocation form that reaches a
		// nested module from the repo root; `go test ./<dir>/...` is the one
		// that fails with "main module does not contain package", so matching
		// the -C form specifically is the point rather than an accident.
		step := regexp.MustCompile(`go test\s+-C\s+` + regexp.QuoteMeta(dir) + `\b`)
		inCI := slices.ContainsFunc(runs, step.MatchString)
		reason, exempt := nestedModulesNotTestedInCI[dir]

		switch {
		case inCI && exempt:
			t.Errorf("%s/go.mod: has a `go test -C %s` step in %s AND an exemption "+
				"(%q) — delete the exemption, it is no longer true",
				dir, dir, ciWorkflowPath, reason)
		case !inCI && !exempt:
			t.Errorf("%s/go.mod is a separate module: the root `go test ./...` cannot "+
				"reach it, so every test in it runs zero times in CI. Add a "+
				"`go test -C %s ./...` step to %s, or record why it must not run in "+
				"nestedModulesNotTestedInCI", dir, dir, ciWorkflowPath)
		case exempt && strings.TrimSpace(reason) == "":
			t.Errorf("%s: exemption has an empty reason", dir)
		}
	}

	for dir := range nestedModulesNotTestedInCI {
		if !seen[dir] {
			t.Errorf("nestedModulesNotTestedInCI has a dead entry %q — no %s/go.mod "+
				"exists any more; delete it", dir, dir)
		}
	}
}
