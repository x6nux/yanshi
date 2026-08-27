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
	"go/build"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io"
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

// ledgerSize is the fixed number of entries in the ledger: the original S0
// audit's 64 items minus A1/S08, which moved to the S1 sub-project (spec
// §4.1), plus entries added by later, separately-scoped work packages. A
// changed count means someone added or dropped scope without updating this
// constant and saying why here.
//
// 2026-08-27: +1 for A2/W-A-01, package "W-A" — a P0 fix from the
// codex/QwenPaw capability audit (docs/superpowers/notes/2026-08-27-capability-audit.md
// P0-1), not part of the original S0 scope. This is new scope, deliberately
// added, not a silent drift.
//
// 2026-08-27: +1 for A2/W-A-02, same package and audit, P0-2 (tool output
// reached the model provider unredacted).
//
// 2026-08-27: +1 for A2/W-A-03, same package and audit, P0-3 merged with
// P0-11 (CJK queries returned zero hits in history_search / SearchMemory /
// memory_autorecall — the FTS5 tokenizer does not segment Chinese).
//
// 2026-08-28: +1 for A2/W-A-04, same package and audit, F9 (the WS restore
// loop mapped only Role + Content and split role into just user/assistant,
// dropping every stored ToolCallID/ToolName/ToolArgs and misclassifying
// tool messages as user).
//
// 2026-08-28: +1 for A2/W-A-05, same package and audit — the "written but
// zero readers" pattern recurring a ninth time: tools.DistillMemories +
// store.ApplyDistillation shipped complete with no caller. Wires /distill
// (interactive) plus an optional post-turn pass behind
// memory_distill_after_turn (default off).
//
// 2026-08-28: +1 for A2/W-A-08, same package — agent_dag/agent_batch's
// concurrently-dispatched sub-agents shared one work root and silently
// overwrote each other's edits (data loss, not a missing feature). W-A-07
// (sandbox path expansion, audit F4) was reviewed and rejected — no
// corresponding code path exists — so W-A's item count goes 06 -> 08, not
// 06 -> 07; this is the deliberate ID gap, not a skipped entry.
const ledgerSize = 70

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
		"W-A": true, // immediate fixes from the 2026-08-27 capability audit, outside the W1-W10 sequence
		"-":   true, // O12, closed by removal
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

// testFilesIn returns the *_test.go files of pkgDir split by whether the
// toolchain can see them at all.
//
// filepath.Glob's "*" is NOT the shell's: it matches "_"- and "."-prefixed
// names like any other, while go/build's matchFile rejects both outright
// before it even looks at the extension. So a glob for "*_test.go" happily
// returns _scratch_test.go and .bak_test.go — files `go test` has never
// compiled — and the caller then reports a live assertion for each. The gate
// already refused "_"- and "."-prefixed DIRECTORIES (it asks `go list`), and
// its rejection message said so in as many words, which made the identical
// file-level rule look covered when nothing enforced it.
//
// Hidden files are returned rather than dropped so resolveTestRef can say why
// a citation into one fails, instead of the misleading "no test named X".
func testFilesIn(pkgDir string) (visible, hidden []string, err error) {
	matches, err := filepath.Glob(filepath.Join(pkgDir, "*_test.go"))
	if err != nil {
		return nil, nil, err
	}
	for _, path := range matches {
		if base := filepath.Base(path); strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".") {
			hidden = append(hidden, path)
			continue
		}
		visible = append(visible, path)
	}
	return visible, hidden, nil
}

// nonPlatform is a GOOS/GOARCH value no port will ever use, so a probe context
// carrying it matches no platform-tagged file name.
const nonPlatform = "yanshi-archtest-nonplatform"

// probeContext builds a go/build context that answers name questions only: the
// file body is stubbed out via OpenFile, so MatchFile's verdict depends on the
// NAME alone and never on a //go:build line (buildConstraintOf already reads
// those, host-independently, and folding them in here would double-count).
// BuildTags and ToolTags are cleared so the host's environment cannot make a
// tagged name look untagged.
func probeContext(goos, goarch string) build.Context {
	c := build.Default
	c.GOOS, c.GOARCH = goos, goarch
	c.BuildTags, c.ToolTags = nil, nil
	c.OpenFile = func(string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("package p\n")), nil
	}
	return c
}

var (
	// neutralProbe matches nothing platform-tagged: both GOOS and GOARCH are
	// impossible values, so MatchFile returns false for exactly those names
	// whose trailing elements the toolchain recognises as a platform.
	neutralProbe = probeContext(nonPlatform, nonPlatform)
	// osProbe pins GOARCH to a real arch so goodOSArchFile's two-element clause
	// (KnownOS + KnownArch) can fire; GOOS stays impossible, so the clause
	// firing is itself the signal that the first element is a KnownOS.
	osProbe = probeContext(nonPlatform, "amd64")
)

// probeRejects reports whether a build context refuses a file purely on its
// name.
func probeRejects(ctxt build.Context, name string) bool {
	match, err := ctxt.MatchFile("archtest-probe", name)
	return err == nil && !match
}

// isPlatformToken reports whether tok is a name the toolchain treats as a
// GOOS or GOARCH in a file-name suffix.
//
// It asks (*go/build.Context).MatchFile instead of consulting a token list,
// because every list this gate has tried was the WRONG list. The first version
// hard-coded one; the second read `go tool dist list`, which enumerates
// BUILDABLE platforms, while file-name matching uses $GOROOT/src/internal/
// syslist — a deliberately larger set whose own comment reads "Do not remove
// from this list, as it is used for filename matching". Thirteen names live in
// syslist and not in the dist list (amd64p32, hurd, nacl, ppc, riscv, s390,
// sparc, sparc64, zos, …), so foo_zos_test.go resolved as an unconstrained,
// CI-executed assertion while `go list` reported it under IgnoredGoFiles. That
// is the same hole the dist-list change claimed to close, relocated.
//
// MatchFile has no such second copy to drift from: it IS the function the
// toolchain uses. syslist is internal and cannot be imported, so probing the
// exported matcher with a synthetic name is the only way to reach the real set.
func isPlatformToken(tok string) bool {
	return probeRejects(neutralProbe, "p_"+tok+"_test.go")
}

// isKnownGOOS reports whether tok is a GOOS name.
//
// The probe leans on goodOSArchFile's two-element clause: in p_<tok>_amd64_
// test.go the pair rule fires only when tok is a KnownOS, and osProbe's GOOS
// can never equal tok, so a rejection means "tok is a GOOS". When tok is not a
// GOOS the rule falls through to the trailing "amd64", which osProbe's GOARCH
// satisfies — so the file is accepted and the answer is no.
func isKnownGOOS(tok string) bool {
	return probeRejects(osProbe, "p_"+tok+"_amd64_test.go")
}

// isKnownGOARCH reports whether tok is a GOARCH name. KnownOS and KnownArch
// are disjoint in syslist, so "a platform token that is not an OS" is exactly
// the arch set.
func isKnownGOARCH(tok string) bool {
	return isPlatformToken(tok) && !isKnownGOOS(tok)
}

var (
	platformProbeOnce sync.Once
	platformProbeErr  error
)

// requirePlatformProbe fails the test unless the MatchFile probes still behave
// as isPlatformToken documents.
//
// The probes read a bool out of a function whose contract is "would this file
// be included", not "is this name platform-tagged". That inference is sound
// today and is how the toolchain has worked since Go 1.4, but it is an
// inference: if a future go/build stopped rejecting unmatched platform names
// here, every probe would answer "not a platform token", filenameConstraintOf
// would return "" for everything, and the gate would go permanently green —
// the exact failure signature of the two token lists that preceded it. A
// canary turns that silent reopening into a loud one.
func requirePlatformProbe(t *testing.T) {
	t.Helper()
	platformProbeOnce.Do(func() {
		for _, c := range []struct {
			tok      string
			os, arch bool
			why      string
		}{
			{"linux", true, false, "a GOOS that has existed for the whole life of the tool"},
			{"windows", true, false, "the GOOS this repo actually build-tags on"},
			{"amd64", false, true, "the GOARCH osProbe pins, so its own premise is checked"},
			{"zos", true, false, "syslist-only GOOS: the name the dist-list version missed"},
			{"riscv", false, true, "syslist-only GOARCH: same class, arch side"},
			{"unix", false, false, "a build tag that is NOT a file-name platform token"},
			{"helper", false, false, "an ordinary identifier must stay unconstrained"},
		} {
			if got := isKnownGOOS(c.tok); got != c.os {
				platformProbeErr = fmt.Errorf("go/build platform probe broken: isKnownGOOS(%q) = %v, want %v (%s)",
					c.tok, got, c.os, c.why)
				return
			}
			if got := isKnownGOARCH(c.tok); got != c.arch {
				platformProbeErr = fmt.Errorf("go/build platform probe broken: isKnownGOARCH(%q) = %v, want %v (%s)",
					c.tok, got, c.arch, c.why)
				return
			}
		}
	})
	if platformProbeErr != nil {
		t.Fatal(platformProbeErr)
	}
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
//
// Which names count as platforms comes from isKnownGOOS/isKnownGOARCH, i.e.
// from the toolchain's own matcher — see isPlatformToken for why every attempt
// to keep a second copy of that list has leaked.
func filenameConstraintOf(t *testing.T, name string) string {
	t.Helper()
	requirePlatformProbe(t)
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
	if n >= 2 && isKnownGOOS(parts[n-2]) && isKnownGOARCH(parts[n-1]) {
		return "file name suffix _" + parts[n-2] + "_" + parts[n-1]
	}
	if n >= 1 && isPlatformToken(parts[n-1]) {
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

	visible, hidden, err := testFilesIn(pkgDir)
	if err != nil {
		return testRefInfo{Problem: "cannot list test files in " + pkg + ": " + err.Error()}
	}
	fset := token.NewFileSet()
	for _, path := range visible {
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
	// Only now look in the files the toolchain ignores, so a name that exists
	// in both places resolves against the compiled copy and the diagnosis below
	// is reserved for citations that genuinely have nowhere else to land.
	if file := declaringFile(fset, hidden, name); file != "" {
		return testRefInfo{Problem: pkg + "::" + name + " is declared in " + file +
			", whose name starts with \"_\" or \".\" — go/build ignores such files exactly as " +
			"it ignores such directories, so `go test` has never compiled this test"}
	}
	return testRefInfo{Problem: "no test named " + name + " in package " + pkg}
}

// declaringFile returns the base name of the first file in paths that declares
// a top-level func called name, or "" when none does.
func declaringFile(fset *token.FileSet, paths []string, name string) string {
	for _, path := range paths {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			continue
		}
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == name {
				return filepath.Base(path)
			}
		}
	}
	return ""
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
			// a good many of them carry real references anyway (the live count
			// is logged by TestFeatureStatusLedgerProgress — quoting it here is
			// what made this comment wrong the first time). The documented
			// shape and the actual shape had drifted, and the gate agreed with
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
			// "unix" is a build TAG, never a file-name platform token — this
			// direction guards against a probe that reddens honest files.
			{"sock_unix_test.go", ""},
		}
		// The thirteen names that live in $GOROOT/src/internal/syslist but not
		// in `go tool dist list`. Reading the dist list — the version this
		// replaced — made every one of them resolve as an unconstrained,
		// CI-executed assertion, which is why they are pinned individually
		// rather than sampled.
		for _, tok := range []string{
			"amd64p32", "arm64be", "armbe", "hurd", "mips64p32", "mips64p32le",
			"nacl", "ppc", "riscv", "s390", "sparc", "sparc64", "zos",
		} {
			cases = append(cases, struct{ file, want string }{
				"probe_" + tok + "_test.go", "file name suffix _" + tok,
			})
		}
		for _, c := range cases {
			if got := filenameConstraintOf(t, c.file); got != c.want {
				t.Errorf("filenameConstraintOf(%q) = %q, want %q", c.file, got, c.want)
			}
		}
	})

	t.Run("toolchain-invisible test files", func(t *testing.T) {
		// filepath.Glob's "*" matches "_"- and "."-prefixed names; the
		// toolchain's does not. Before testFilesIn, a test declared only in
		// such a file resolved as a live, unconstrained assertion — the same
		// hole the gate had already closed for DIRECTORIES, and whose rejection
		// message claimed the file form was covered too.
		dir := t.TempDir()
		for _, f := range []string{"ok_test.go", "_hidden_test.go", ".dot_test.go"} {
			if err := os.WriteFile(filepath.Join(dir, f), []byte("package p\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		visible, hidden, err := testFilesIn(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(visible) != 1 || filepath.Base(visible[0]) != "ok_test.go" {
			t.Errorf("visible = %v, want only ok_test.go", visible)
		}
		var hiddenNames []string
		for _, h := range hidden {
			hiddenNames = append(hiddenNames, filepath.Base(h))
		}
		sort.Strings(hiddenNames)
		if len(hiddenNames) != 2 || hiddenNames[0] != ".dot_test.go" || hiddenNames[1] != "_hidden_test.go" {
			t.Errorf("hidden = %v, want [.dot_test.go _hidden_test.go]", hiddenNames)
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
//
// It also prints how many NON-terminal entries carry an evidence lead, which
// is the one ledger statistic prose kept getting wrong: the header of
// docs/feature-status.yaml and two comments in this package all quoted "5",
// and all three went stale the moment seven partial entries gained leads in
// one commit. Prose now points here instead of restating a number, so the
// count is derived where it is read.
func TestFeatureStatusLedgerProgress(t *testing.T) {
	entries := loadLedger(t)
	counts := make(map[string]int)
	var leads []string
	for _, e := range entries {
		counts[e.Verdict]++
		if !terminalVerdicts[e.Verdict] && !e.Evidence.Empty() {
			leads = append(leads, e.ID)
		}
	}
	done := counts["done"] + counts["removed"]
	t.Logf("S0 progress: %d/%d terminal (done=%d removed=%d) | "+
		"remaining: partial=%d missing=%d divergent=%d",
		done, len(entries), counts["done"], counts["removed"],
		counts["partial"], counts["missing"], counts["divergent"])
	sort.Strings(leads)
	t.Logf("non-terminal entries carrying an evidence lead: %d %v", len(leads), leads)
}
