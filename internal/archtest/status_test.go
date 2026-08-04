// Package archtest — feature status ledger integrity.
//
// docs/feature-status.yaml is S0's single source of truth for "how much of
// the planned surface actually works". A ledger nobody checks is a ledger
// anybody can edit to look good, so these assertions make a terminal verdict
// cost real evidence.
package archtest

import (
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

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
	// Evidence is a plain string for non-terminal entries and a clause-index
	// mapping for terminal ones — see evidenceField and the GOV8 gate in
	// status_evidence_test.go, which is where terminal evidence is audited
	// clause by clause.
	Evidence evidenceField `yaml:"evidence"`
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

// evidenceScanRoots are the module-relative top-level directories an evidence
// reference may name.
//
// The forward pass (checkTerminalEvidence) and the backward scan
// (TestLedgerMarkersAreLive) MUST agree on this set, or the handshake is only
// half a handshake. The backward walk visits exactly these directories, so a
// citation pointing anywhere else — sdk/, third_party/, docs/ — would have its
// marker verified on the way in and then never be re-checked for staleness:
// withdraw the citation and the claim rots in place, unreachable by the very
// scan that exists to find it. Sharing one list makes that impossible by
// construction rather than merely unlikely.
var evidenceScanRoots = []string{"internal", "cmd"}

// testRefInfo is the resolution of one "pkg/path::Name" evidence reference.
type testRefInfo struct {
	// Problem is "" when the reference names a function `go test` would
	// actually run, and otherwise says why it does not.
	Problem string
	// Constrained reports that the declaring file carries an explicit
	// //go:build (or legacy "// +build") line, so the default toolchain
	// invocation never compiles it.
	Constrained bool
	// Constraint is the constraint line verbatim, quoted back in failures so
	// the reader sees which tag is missing.
	Constraint string
}

// isTestFuncName mirrors `go test`'s own naming rule: a test is named Test
// followed by nothing or by a non-lower-case rune. "TestingHelper" is a helper
// — go test never runs it — and neither is "newTestStore".
func isTestFuncName(name string) bool {
	if !strings.HasPrefix(name, "Test") {
		return false
	}
	rest := name[len("Test"):]
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return !unicode.IsLower(r)
}

// isTestSignature reports whether fd has the signature `go test` requires of a
// test function: exactly one parameter of type *testing.T and no results.
//
// An unnamed parameter (func TestX(*testing.T)) is legal Go and is run, so it
// is accepted here too.
func isTestSignature(fd *ast.FuncDecl) bool {
	if fd.Type.Results != nil && len(fd.Type.Results.List) > 0 {
		return false
	}
	params := fd.Type.Params
	if params == nil || len(params.List) != 1 || len(params.List[0].Names) > 1 {
		return false
	}
	star, ok := params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "testing" && sel.Sel.Name == "T"
}

// buildConstraintOf returns the //go:build or "// +build" line governing f, or
// "" when the file is compiled unconditionally.
//
// Only comment groups that END before the package clause are considered, which
// is where the toolchain looks too; a "//go:build"-shaped string further down
// the file is just text.
func buildConstraintOf(fset *token.FileSet, f *ast.File) string {
	pkgLine := fset.Position(f.Package).Line
	for _, cg := range f.Comments {
		if fset.Position(cg.End()).Line >= pkgLine {
			break
		}
		for _, c := range cg.List {
			if constraint.IsGoBuild(c.Text) || constraint.IsPlusBuild(c.Text) {
				return strings.TrimSpace(c.Text)
			}
		}
	}
	return ""
}

// resolveTestRef resolves a "pkg/path::Name" evidence reference and reports
// every way that resolution falls short of an assertion `go test` executes.
//
// The existence of a same-named top-level func in some *_test.go — all the
// first version of this check asked for — is NOT enough, and review drove three
// probes straight through it: a plain fixture helper (newTestStore) with no
// Test prefix, a name whose next rune is lower-case (which go test skips), and
// a test behind //go:build e2e_real that CI never compiles. All three satisfied
// the property that was implemented ("a func by this name lives in a test
// file") while satisfying none of the property ADR-0011 declares inviolable
// ("only an executable assertion can carry a terminal claim"). A gate whose
// implemented predicate is weaker than its advertised one is worse than no
// gate: it launders the weaker check as the stronger one.
//
// This replaces `go test -list` (spec §5.3): same question, no compile. The one
// thing it cannot answer that `go test -list` could is whether the package
// still builds — a citation surviving a compile break is caught by CI itself.
func resolveTestRef(t *testing.T, root, pkg, name string) testRefInfo {
	t.Helper()

	head, _, _ := strings.Cut(pkg, "/")
	if !slices.Contains(evidenceScanRoots, head) {
		return testRefInfo{Problem: "evidence package " + pkg + " is outside " +
			strings.Join(evidenceScanRoots, "/ and ") + "/ — the backward staleness " +
			"scan only walks those roots, so a marker there could never be re-checked"}
	}

	pkgDir := filepath.Join(root, filepath.FromSlash(pkg))
	if st, err := os.Stat(pkgDir); err != nil || !st.IsDir() {
		return testRefInfo{Problem: "package dir not found: " + pkg +
			" (test references use a PACKAGE path, not a file path)"}
	}
	if !isTestFuncName(name) {
		return testRefInfo{Problem: name + " is not a test function name — `go test` " +
			"only runs top-level funcs named Test followed by a non-lower-case rune, so " +
			"citing a helper claims coverage from code that never runs"}
	}

	matches, err := filepath.Glob(filepath.Join(pkgDir, "*_test.go"))
	if err != nil {
		return testRefInfo{Problem: "cannot list test files in " + pkg + ": " + err.Error()}
	}
	fset := token.NewFileSet()
	for _, path := range matches {
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			continue
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || fd.Name.Name != name {
				continue
			}
			if !isTestSignature(fd) {
				return testRefInfo{Problem: pkg + "::" + name + " is not func(*testing.T) — " +
					"`go test` will not run it whatever it is named"}
			}
			cons := buildConstraintOf(fset, f)
			return testRefInfo{Constrained: cons != "", Constraint: cons}
		}
	}
	return testRefInfo{Problem: "no test named " + name + " in package " + pkg}
}

// checkEvidence validates an evidence string and returns "" when valid or a
// human-readable reason when not.
//
// A clause may cite SEVERAL references separated by ";" — one clause can
// legitimately take more than one test to prove. Which clause a reference
// belongs to, and whether every clause has one, is decided one level up in
// checkTerminalEvidence (GOV8); this function only answers "does this
// reference resolve".
//
// Each reference takes one of two legal forms:
//
//	internal/foo/bar.go:123   file reference — path must exist; the line
//	                          number is NOT checked (it drifts with any
//	                          unrelated edit, and a gate that reddens for no
//	                          reason gets nolint'd away)
//	internal/foo::TestName    test reference — PACKAGE path, not file path;
//	                          resolved by resolveTestRef, so the name must be
//	                          a func(*testing.T) that `go test` would run
//
// Build constraints are reported but not rejected here: a constrained test is
// still a real assertion, and whether a CLAUSE may rest solely on one is a
// terminal-verdict question, answered in checkTerminalEvidence.
func checkEvidence(t *testing.T, root, ev string) string {
	t.Helper()
	refs := strings.Split(ev, ";")
	if len(refs) > 1 {
		for _, ref := range refs {
			ref = strings.TrimSpace(ref)
			if ref == "" {
				return "empty reference in a \";\"-separated evidence list"
			}
			if reason := checkEvidence(t, root, ref); reason != "" {
				return reason
			}
		}
		return ""
	}
	ev = strings.TrimSpace(ev)
	if pkg, name, ok := strings.Cut(ev, "::"); ok {
		return resolveTestRef(t, root, pkg, name).Problem
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
	entries := loadLedger(t)
	root := moduleRoot(t)

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
			// Non-terminal evidence is OPTIONAL, but it is no longer
			// UNCHECKED. The ledger header says non-terminal entries write "";
			// five of them carry real references anyway. The documented shape
			// and the actual shape had drifted, and the gate agreed with
			// neither — it `continue`d before checkEvidence could run, which
			// made those references the only strings in the file that nothing
			// verifies. A dangling lead is worse than no lead: it reads as
			// corroboration. Validating them costs nothing and makes the
			// permissive rule ("leads are welcome, they just have to resolve")
			// the one that is actually enforced.
			switch {
			case e.Evidence.Mapped:
				problems = append(problems, e.ID+": verdict "+e.Verdict+" uses a "+
					"clause mapping — per-clause accounting is a TERMINAL shape "+
					"(a non-terminal entry has nothing to have proven yet). Use a "+
					"plain string of leads, or flip the verdict and face GOV8")
			case strings.TrimSpace(e.Evidence.Scalar) != "":
				if reason := checkEvidence(t, root, e.Evidence.Scalar); reason != "" {
					problems = append(problems, e.ID+": evidence lead does not resolve — "+reason)
				}
			}
			continue
		}
		if e.Evidence.Empty() {
			problems = append(problems, e.ID+": verdict "+e.Verdict+
				" requires non-empty evidence")
		}
		// Clause-by-clause auditing (reference validity, per-clause coverage,
		// and the test-side handshake) is TestLedgerEvidenceIsClauseComplete's
		// job and is deliberately not repeated here: this test asks "is the
		// ledger well-formed", GOV8 asks "is the evidence sufficient". Calling
		// checkTerminalEvidence from both printed every clause-level finding
		// twice, which reads like two independent failures and doubles the
		// output a reader has to reconcile.
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
