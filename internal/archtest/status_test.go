// Package archtest — feature status ledger integrity.
//
// docs/feature-status.yaml is S0's single source of truth for "how much of
// the planned surface actually works". A ledger nobody checks is a ledger
// anybody can edit to look good, so these assertions make a terminal verdict
// cost real evidence.
package archtest

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
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
	// Constrained reports that the declaring file carries a build constraint
	// of EITHER shape — an explicit //go:build (or legacy "// +build") line, or
	// an implicit GOOS/GOARCH file-name suffix — so a plain `go test ./...` on
	// an arbitrary machine may never compile it.
	//
	// Both shapes are folded into one flag deliberately. ADR-0011 fixes the
	// verdict at "would the DEFAULT invocation build it", which makes the
	// answer platform-independent: plat_windows_test.go is Constrained when
	// this gate runs on darwin, on linux, and on the windows CI leg alike. A
	// per-platform answer would make governance itself OS-dependent — the same
	// ledger would pass on one leg of the CI matrix and fail on another.
	Constrained bool
	// Constraint is the constraint verbatim (the //go:build line, or a
	// description of the file-name suffix), quoted back in failures so the
	// reader sees what excludes the file.
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

var (
	listedPackagesOnce sync.Once
	listedPackages     map[string]bool
	listedPackagesErr  error
)

// defaultTestPackages returns every module-relative package directory that a
// plain `go test ./...` would build, as reported by `go list` itself.
//
// This exists because the directory layout is NOT the package set, and guessing
// the difference is how the third probe walked through this gate. `go list`
// silently drops directories the toolchain ignores by convention — `testdata/`
// most importantly, but also `_`- and `.`-prefixed directories and any folder
// with no Go files at all. filepath.Glob knows none of that: it happily found
// internal/archtest/zzprobepkg/testdata/probe_test.go and reported a live
// assertion for a file no CI job has ever compiled. Asking the toolchain what
// it builds, instead of re-deriving its conventions here, removes the whole
// class: whatever `go test ./...` skips, this skips too, including rules added
// to Go after this code was written.
//
// `-e` keeps a package that fails to build in the list. The question here is
// membership ("is this directory in ./..."), not health; a genuine compile
// break is CI's finding to report, and letting it also blank out the ledger
// gate would turn one red into two unrelated ones.
func defaultTestPackages(t *testing.T) map[string]bool {
	t.Helper()
	root := moduleRoot(t)
	mp := modulePath(t)
	listedPackagesOnce.Do(func() {
		cmd := exec.Command("go", "list", "-e", "./...")
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			listedPackagesErr = fmt.Errorf("go list -e ./... failed: %w", err)
			return
		}
		set := map[string]bool{}
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || !isModulePkg(line, mp) {
				continue
			}
			set[strings.TrimPrefix(strings.TrimPrefix(line, mp), "/")] = true
		}
		if len(set) == 0 {
			listedPackagesErr = errors.New("go list -e ./... reported no packages in this module")
			return
		}
		listedPackages = set
	})
	if listedPackagesErr != nil {
		t.Fatal(listedPackagesErr)
	}
	return listedPackages
}

var (
	platformTokensOnce sync.Once
	knownGOOS          map[string]bool
	knownGOARCH        map[string]bool
	platformTokensErr  error
)

// platformTokens returns the GOOS and GOARCH names the toolchain recognises,
// read from `go tool dist list` rather than hard-coded.
//
// A frozen copy of the list would rot in the one direction that matters: a new
// port lands, someone writes foo_<newos>_test.go, and the constraint becomes
// invisible to this gate exactly when it is newest and least reviewed.
func platformTokens(t *testing.T) (map[string]bool, map[string]bool) {
	t.Helper()
	platformTokensOnce.Do(func() {
		out, err := exec.Command("go", "tool", "dist", "list").Output()
		if err != nil {
			platformTokensErr = fmt.Errorf("go tool dist list failed: %w", err)
			return
		}
		oses, arches := map[string]bool{}, map[string]bool{}
		for _, pair := range strings.Fields(string(out)) {
			goos, goarch, ok := strings.Cut(pair, "/")
			if !ok {
				continue
			}
			oses[goos] = true
			arches[goarch] = true
		}
		if len(oses) == 0 || len(arches) == 0 {
			platformTokensErr = errors.New("go tool dist list produced no GOOS/GOARCH pairs")
			return
		}
		knownGOOS, knownGOARCH = oses, arches
	})
	if platformTokensErr != nil {
		t.Fatal(platformTokensErr)
	}
	return knownGOOS, knownGOARCH
}

// filenameConstraintOf returns a description of the implicit build constraint a
// file NAME imposes, or "" when the name constrains nothing.
//
// Go gives `foo_windows_test.go` exactly the force of `//go:build windows`,
// with no comment anywhere in the file. buildConstraintOf reads comments only,
// so before this function existed a GOOS-suffixed test resolved as
// Constrained=false — advertised as an assertion the default toolchain runs,
// when on any other GOOS it is not compiled at all. Every GOOS-suffixed test
// file in the repo happened to ALSO carry a redundant //go:build line when this
// was written, so the hole was latent rather than exploited; the file-name form
// alone is idiomatic Go and would have opened it the first time someone left
// the comment off.
//
// The matching rules mirror (*go/build.Context).goodOSArchFile, including its
// two easy-to-miss clauses: everything before the FIRST underscore is discarded
// (so a package-level `windows.go` is not tagged, per Go 1.4), and a trailing
// `_test` element is stripped before the platform elements are examined.
func filenameConstraintOf(t *testing.T, name string) string {
	t.Helper()
	goos, goarch := platformTokens(t)
	stem, _, _ := strings.Cut(name, ".")
	i := strings.Index(stem, "_")
	if i < 0 {
		return ""
	}
	parts := strings.Split(stem[i:], "_") // leading element is "" by construction
	if n := len(parts); n > 0 && parts[n-1] == "test" {
		parts = parts[:n-1]
	}
	n := len(parts)
	if n >= 2 && goos[parts[n-2]] && goarch[parts[n-1]] {
		return "file name suffix _" + parts[n-2] + "_" + parts[n-1]
	}
	if n >= 1 && (goos[parts[n-1]] || goarch[parts[n-1]]) {
		return "file name suffix _" + parts[n-1]
	}
	return ""
}

// nonAssertingTestCalls are *testing.T methods that may precede an
// unconditional Skip without changing the verdict: none of them can fail a
// test, so a body made only of these and a Skip proves nothing either way.
var nonAssertingTestCalls = map[string]bool{
	"Helper": true, "Parallel": true, "Log": true, "Logf": true,
}

// unconditionalSkip returns the name of a Skip method that EVERY execution of
// fd reaches, or "" when no such call dominates the body.
//
// Rule 4 bans clauses that rest solely on build-constrained tests because the
// assertion has never executed. A test whose body opens with a bare t.Skip has
// the same defect and is worse camouflaged: `go test` compiles it, runs it, and
// prints it as a pass, so every signal a reader has says the clause is covered.
//
// The predicate is deliberately narrow, and its narrowness is a documented
// boundary of GOV8 (ADR-0011). It fires only when the skip provably dominates
// the whole body: every statement is a call on the test's own *testing.T, the
// ones before the Skip are drawn from nonAssertingTestCalls, and anything else
// at all — a plain `if`, a helper call, an assignment — abandons the analysis
// and returns "". Skips nested inside a condition, a subtest closure, or a
// helper are NOT detected, and cannot be without a reachability analysis this
// gate has no business carrying. Under-detecting leaves a hole; over-detecting
// would redden honest platform-gated tests, and a gate that reddens for no
// reason gets deleted, which is the larger hole.
func unconditionalSkip(fd *ast.FuncDecl) string {
	if fd.Body == nil {
		return ""
	}
	// isTestSignature has already established exactly one parameter; an unnamed
	// or blank one leaves no way to recognise calls on it.
	names := fd.Type.Params.List[0].Names
	if len(names) == 0 || names[0].Name == "_" {
		return ""
	}
	recv := names[0].Name
	for _, stmt := range fd.Body.List {
		es, ok := stmt.(*ast.ExprStmt)
		if !ok {
			return ""
		}
		call, ok := es.X.(*ast.CallExpr)
		if !ok {
			return ""
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return ""
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || id.Name != recv {
			return ""
		}
		switch sel.Sel.Name {
		case "Skip", "Skipf", "SkipNow":
			return recv + "." + sel.Sel.Name
		}
		if !nonAssertingTestCalls[sel.Sel.Name] {
			return ""
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
// A second review round drove three MORE probes through what was left, and all
// three have the same shape as the first three: the check answered a question
// about the FILESYSTEM where the advertised question is about the TOOLCHAIN.
//
//	testdata/           `go list ./...` does not list it and `go test ./...`
//	                    never compiles it, but filepath.Glob finds *_test.go
//	                    there like anywhere else
//	plat_windows_test.go a GOOS file-name suffix is a build constraint with no
//	                    comment to read, so a comment-only scan called it
//	                    unconstrained
//	body starts t.Skip  compiled, run, reported as a pass, asserts nothing
//
// The first two are closed here by asking `go list` which packages exist and by
// reading the constraint out of the file name; the third by unconditionalSkip,
// whose limits are a stated boundary rather than a claim.
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
	if !defaultTestPackages(t)[pkg] {
		return testRefInfo{Problem: pkg + " is a directory but not a package `go list ./...` " +
			"reports, so `go test ./...` never builds it — testdata/, \"_\"- and \".\"-prefixed " +
			"directories are invisible to the toolchain by convention, and a test the toolchain " +
			"cannot see has never asserted anything"}
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
			if skip := unconditionalSkip(fd); skip != "" {
				return testRefInfo{Problem: pkg + "::" + name + " opens with an unconditional " +
					skip + "(...) — `go test` runs it and it asserts nothing, so it reports a " +
					"pass while proving exactly as much as a build-constrained test that never " +
					"compiled (rule 4), with none of the visible warning"}
			}
			cons := buildConstraintOf(fset, f)
			if cons == "" {
				cons = filenameConstraintOf(t, filepath.Base(path))
			}
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

// TestResolveTestRefExecutabilityPredicates pins the three predicates that
// decide whether a citation names an assertion `go test` really executes.
//
// They are unit-tested rather than left to the ledger's own citations because
// the ledger only exercises the PASSING side: every reference in
// docs/feature-status.yaml resolves, so a regression that made these predicates
// answer "fine" to everything would leave the whole gate green. Each earlier
// hole in resolveTestRef had exactly that signature — permanently green, and
// only visible to someone who built a probe by hand.
func TestResolveTestRefExecutabilityPredicates(t *testing.T) {
	t.Run("file name constraints", func(t *testing.T) {
		cases := []struct{ file, want string }{
			{"alive_windows.go", "file name suffix _windows"},
			{"plat_windows_test.go", "file name suffix _windows"},
			{"cpu_linux_amd64_test.go", "file name suffix _linux_amd64"},
			{"tty_darwin.go", "file name suffix _darwin"},
			{"asm_arm64_test.go", "file name suffix _arm64"},
			// Go 1.4 onward, a name with nothing before the first underscore
			// carries no implicit tag — "windows.go" is ordinary source.
			{"windows.go", ""},
			{"windows_test.go", ""},
			{"status_test.go", ""},
			{"manager.go", ""},
			// Only the trailing elements count: an OS name in the middle is
			// part of the identifier, not a constraint.
			{"windows_helper_test.go", ""},
		}
		for _, c := range cases {
			if got := filenameConstraintOf(t, c.file); got != c.want {
				t.Errorf("filenameConstraintOf(%q) = %q, want %q", c.file, got, c.want)
			}
		}
	})

	t.Run("unconditional skips", func(t *testing.T) {
		cases := []struct{ name, body, want string }{
			{"bare skip", `t.Skip("nope")`, "t.Skip"},
			{"skipf", `t.Skipf("nope %d", 1)`, "t.Skipf"},
			{"skipnow", `t.SkipNow()`, "t.SkipNow"},
			{"after parallel and log", "t.Parallel()\n\tt.Log(\"x\")\n\tt.Skip(\"nope\")", "t.Skip"},
			{"renamed receiver", `tt.Skip("nope")`, "tt.Skip"},
			// Conditional skips are the legitimate, overwhelmingly common form
			// and must never be flagged.
			{"guarded skip", "if os.Getenv(\"X\") == \"\" {\n\t\tt.Skip(\"nope\")\n\t}\n\tt.Fatal(\"x\")", ""},
			{"skip after work", "run(t)\n\tt.Skip(\"nope\")", ""},
			{"no skip", `t.Fatal("boom")`, ""},
			{"empty body", "", ""},
		}
		for _, c := range cases {
			recv := "t"
			if strings.Contains(c.body, "tt.") {
				recv = "tt"
			}
			src := "package p\n\nfunc TestX(" + recv + " *testing.T) {\n\t" + c.body + "\n}\n"
			f, err := parser.ParseFile(token.NewFileSet(), "x_test.go", src, parser.ParseComments)
			if err != nil {
				t.Fatalf("%s: fixture does not parse: %v", c.name, err)
			}
			fd := f.Decls[0].(*ast.FuncDecl)
			if got := unconditionalSkip(fd); got != c.want {
				t.Errorf("%s: unconditionalSkip = %q, want %q", c.name, got, c.want)
			}
		}
	})

	t.Run("package set excludes toolchain-invisible dirs", func(t *testing.T) {
		pkgs := defaultTestPackages(t)
		if !pkgs["internal/archtest"] {
			t.Fatal("go list ./... does not report internal/archtest — the package set is wrong")
		}
		for pkg := range pkgs {
			for _, elem := range strings.Split(pkg, "/") {
				if elem == "testdata" || strings.HasPrefix(elem, "_") || strings.HasPrefix(elem, ".") {
					t.Errorf("go list ./... reported %q, which the toolchain should ignore", pkg)
				}
			}
		}
	})
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
