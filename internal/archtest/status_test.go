// Package archtest — feature status ledger integrity.
//
// docs/feature-status.yaml is S0's single source of truth for "how much of
// the planned surface actually works". A ledger nobody checks is a ledger
// anybody can edit to look good, so these assertions make a terminal verdict
// cost real evidence.
package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ledgerSize is the fixed number of entries in the S0 ledger: the audit's 64
// items minus A1/S08, which moved to the S1 sub-project (spec §4.1). A
// changed count means someone added or dropped scope without updating the
// spec.
const ledgerSize = 63

type ledgerEntry struct {
	ID         string `yaml:"id"`
	Package    string `yaml:"package"`
	Verdict    string `yaml:"verdict"`
	Title      string `yaml:"title"`
	Acceptance string `yaml:"acceptance"`
	Evidence   string `yaml:"evidence"`
}

var (
	validVerdicts = map[string]bool{
		"partial": true, "missing": true, "divergent": true,
		"done": true, "removed": true,
	}
	terminalVerdicts = map[string]bool{"done": true, "removed": true}
	validPackages    = map[string]bool{
		"W1": true, "W2": true, "W3": true, "W4": true, "W5": true,
		"W6": true, "W7": true, "W8": true, "W9": true, "W10": true,
		"-": true, // O12, closed by removal
	}
)

func loadLedger(t *testing.T) []ledgerEntry {
	t.Helper()
	path := filepath.Join(moduleRoot(t), "docs", "feature-status.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ledger unreadable: %v", err)
	}
	var entries []ledgerEntry
	if err := yaml.Unmarshal(data, &entries); err != nil {
		t.Fatalf("ledger is not valid YAML: %v", err)
	}
	return entries
}

// testExistsInPkg reports whether pkgDir contains a *_test.go declaring a
// top-level func named testName.
//
// This replaces `go test -list` (spec §5.3): same question, no compile.
func testExistsInPkg(t *testing.T, pkgDir, testName string) bool {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(pkgDir, "*_test.go"))
	if err != nil || len(matches) == 0 {
		return false
	}
	fset := token.NewFileSet()
	for _, path := range matches {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			continue
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if ok && fd.Recv == nil && fd.Name.Name == testName {
				return true
			}
		}
	}
	return false
}

// checkEvidence validates one evidence string and returns "" when valid or a
// human-readable reason when not.
//
// Two legal forms:
//
//	internal/foo/bar.go:123   file reference — path must exist; the line
//	                          number is NOT checked (it drifts with any
//	                          unrelated edit, and a gate that reddens for no
//	                          reason gets nolint'd away)
//	internal/foo::TestName    test reference — PACKAGE path, not file path
func checkEvidence(t *testing.T, root, ev string) string {
	t.Helper()
	if strings.Contains(ev, "::") {
		parts := strings.SplitN(ev, "::", 2)
		pkgDir := filepath.Join(root, filepath.FromSlash(parts[0]))
		if st, err := os.Stat(pkgDir); err != nil || !st.IsDir() {
			return "package dir not found: " + parts[0] +
				" (test references use a PACKAGE path, not a file path)"
		}
		if !testExistsInPkg(t, pkgDir, parts[1]) {
			return "no test named " + parts[1] + " in package " + parts[0]
		}
		return ""
	}
	idx := strings.LastIndex(ev, ":")
	if idx <= 0 {
		return "malformed: expected \"path/to/file.go:LINE\" or \"pkg/path::TestName\""
	}
	rel := ev[:idx]
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		return "file not found: " + rel
	}
	return ""
}

// TestFeatureStatusLedgerIntegrity guards the ledger against the one failure
// mode that would make every other governance assertion pointless: flipping
// a verdict to a terminal state without doing the work.
func TestFeatureStatusLedgerIntegrity(t *testing.T) {
	root := moduleRoot(t)
	entries := loadLedger(t)

	if len(entries) != ledgerSize {
		t.Fatalf("ledger has %d entries, expected %d — scope changed without a spec "+
			"update (see spec §4.1)", len(entries), ledgerSize)
	}

	seen := make(map[string]bool, len(entries))
	var problems []string
	for _, e := range entries {
		if e.ID == "" {
			problems = append(problems, "an entry has an empty id")
			continue
		}
		if seen[e.ID] {
			problems = append(problems, e.ID+": duplicate id")
		}
		seen[e.ID] = true

		if !validVerdicts[e.Verdict] {
			problems = append(problems, e.ID+": invalid verdict "+e.Verdict)
		}
		if !validPackages[e.Package] {
			problems = append(problems, e.ID+": invalid package "+e.Package)
		}
		if e.Acceptance == "" {
			problems = append(problems, e.ID+": empty acceptance criteria")
		}

		if !terminalVerdicts[e.Verdict] {
			continue
		}
		if e.Evidence == "" {
			problems = append(problems, e.ID+": verdict "+e.Verdict+
				" requires non-empty evidence")
			continue
		}
		if reason := checkEvidence(t, root, e.Evidence); reason != "" {
			problems = append(problems, e.ID+": bad evidence — "+reason)
		}
		if e.Verdict == "removed" && !strings.Contains(e.Evidence, "::") {
			problems = append(problems, e.ID+": verdict removed requires a TEST "+
				"reference (pkg::TestName) — it must assert the thing is gone, and a "+
				"file reference cannot do that")
		}
	}

	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("docs/feature-status.yaml has %d problem(s):\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// TestFeatureStatusLedgerProgress prints the current tally. It never fails —
// it exists so CI logs carry the number without anyone running a tool.
func TestFeatureStatusLedgerProgress(t *testing.T) {
	entries := loadLedger(t)
	counts := make(map[string]int)
	for _, e := range entries {
		counts[e.Verdict]++
	}
	done := counts["done"] + counts["removed"]
	t.Logf("S0 progress: %d/%d terminal (done=%d removed=%d) | "+
		"remaining: partial=%d missing=%d divergent=%d",
		done, len(entries), counts["done"], counts["removed"],
		counts["partial"], counts["missing"], counts["divergent"])
}
