package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file is the REAL-BINARY half of the T2 verification.
//
// Every other ast_search test replaces secureCommandRunner with a fake that
// returns canned JSON. Those pin the parsing, the truncation and the jail, and
// they run everywhere -- but between them and a working tool sit three things
// no fixture can check: that the argv this code builds is one the real CLI
// accepts, that `--json=compact` still emits the field names astGrepMatch
// declares, and that the 0-based-to-1-based line conversion lands on the line
// a human reading the file would name.
//
// A schema change in ast-grep would leave every fixture-driven test green
// while the tool silently returned zero matches for every query -- the worst
// possible failure for a search tool, because an empty result reads as "there
// are none" rather than "this is broken".
//
// Skips when no ast-grep is on PATH, which is the state of a plain CI runner.

// requireAstGrep skips unless a real ast-grep CLI is installed, and returns
// nothing: the tool finds the binary itself through astLookPath, which this
// file deliberately leaves at the real exec.LookPath.
func requireAstGrep(t *testing.T) {
	t.Helper()
	for _, name := range astGrepBinaries {
		if _, err := exec.LookPath(name); err == nil {
			return
		}
	}
	t.Skip("ast-grep not on PATH; install it (https://ast-grep.github.io) to run the real-binary structural search checks")
}

// astRealFixture writes a Go file whose swallowed-error branches are at known
// line numbers, and returns the work root.
//
// Line numbers are the assertion, so the layout is fixed and commented. The
// file is deliberately NOT a valid part of any module: ast-grep parses with
// tree-sitter and never type-checks, and depending on a buildable module would
// couple this test to the Go toolchain for no gain.
func astRealFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	body := strings.Join([]string{
		"package fixture",                 // 1
		"",                                // 2
		"import \"os\"",                   // 3
		"",                                // 4
		"func swallow() {",                // 5
		"\tf, err := os.Open(\"/tmp/a\")", // 6
		"\tif err != nil {",               // 7  <- swallowed, empty body
		"\t}",                             // 8
		"\t_ = f",                         // 9
		"}",                               // 10
		"",                                // 11
		"func handle() error {",           // 12
		"\terr := os.Remove(\"/tmp/b\")",  // 13
		"\tif err != nil {",               // 14 <- NOT swallowed, returns
		"\t\treturn err",                  // 15
		"\t}",                             // 16
		"\treturn nil",                    // 17
		"}",                               // 18
		"",                                // 19
	}, "\n")
	if err := os.WriteFile(filepath.Join(root, "fixture.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// astRealCtx is astCtx plus the secure-process factory bootstrap really
// installs.
//
// Binding it is not test scaffolding, it is part of what this file verifies.
// ast_search launches through secproc, which is fail-closed: with no Factory in
// context every invocation returns "no Factory in context" instead of running
// the CLI. The fixture-driven tests never noticed because they replace the
// runner wholesale, so the one context value the real path cannot work without
// was, until this file, never bound in any ast_search test.
func astRealCtx(root string) context.Context {
	return WithSecureProcessFactory(astCtx(root), prodFactory())
}

// TestAstSearchReal_SwallowedErrorQuery runs the exact query the capability was
// justified by -- "find every branch that swallows an error", which a regexp
// cannot express -- through the real CLI, and checks that it finds the one
// swallowing branch and NOT the one that returns the error.
//
// The negative half is what makes this a structural check rather than a text
// one: both branches contain the characters `if err != nil {`, so any
// regexp-shaped implementation would report two matches.
func TestAstSearchReal_SwallowedErrorQuery(t *testing.T) {
	requireAstGrep(t)
	root := astRealFixture(t)
	fs := NewFSTools(root)
	tool := fs.NewAstSearchTool()

	out, err := runTool(astRealCtx(root), tool, `{"pattern":"if err != nil { }","language":"go"}`)
	if err != nil {
		t.Fatalf("ast_search returned a Go error, which aborts the turn: %v", err)
	}
	var res astSearchResult
	if uerr := json.Unmarshal([]byte(out), &res); uerr != nil {
		t.Fatalf("result is not the documented JSON shape (%v): %s", uerr, out)
	}
	if len(res.Matches) != 1 {
		t.Fatalf("real ast-grep reported %d matches, want exactly 1 (the empty-body branch "+
			"on line 7, not the one that returns err on line 14): %s", len(res.Matches), out)
	}
	m := res.Matches[0]
	if m.Line != 7 {
		t.Errorf("match reported at line %d, want 7; ast-grep emits 0-based lines and the "+
			"tool must add one, so an off-by-one here is a real user-visible bug", m.Line)
	}
	if !strings.HasSuffix(m.Path, "fixture.go") {
		t.Errorf("match path = %q, want the fixture file", m.Path)
	}
	if !strings.Contains(m.Snippet, "err != nil") {
		t.Errorf("snippet does not contain the matched source: %q", m.Snippet)
	}
	if res.Total != 1 {
		t.Errorf("Total = %d, want 1", res.Total)
	}
}

// TestAstSearchReal_MetavariablePatternMatchesEveryBranch drives a pattern
// with a `$$$` sequence capture, which is the form the tool's own description
// teaches the model to use. It must match BOTH branches of the fixture.
//
// Running both this and the empty-body query against the same file is what
// proves the CLI is really parsing structurally: the two patterns differ only
// in whether the body is captured, and they must return different counts.
func TestAstSearchReal_MetavariablePatternMatchesEveryBranch(t *testing.T) {
	requireAstGrep(t)
	root := astRealFixture(t)
	fs := NewFSTools(root)
	tool := fs.NewAstSearchTool()

	out, err := runTool(astRealCtx(root), tool, `{"pattern":"if err != nil { $$$BODY }","language":"go"}`)
	if err != nil {
		t.Fatalf("ast_search returned a Go error: %v", err)
	}
	var res astSearchResult
	if uerr := json.Unmarshal([]byte(out), &res); uerr != nil {
		t.Fatalf("result is not JSON (%v): %s", uerr, out)
	}
	if res.Total != 2 {
		t.Fatalf("a $$$-body pattern matched %d branches, want 2 (both the swallowing and "+
			"the returning one): %s", res.Total, out)
	}
	lines := map[int]bool{}
	for _, m := range res.Matches {
		lines[m.Line] = true
	}
	for _, want := range []int{7, 14} {
		if !lines[want] {
			t.Errorf("no match on line %d; got lines %v", want, lines)
		}
	}
}

// TestAstSearchReal_MaxMatchesTruncatesRealOutput checks the cap against real
// CLI output rather than a fixture, because the truncation reads Total off the
// parsed payload and a schema change would make Total wrong in a way the
// fixture (which the test itself authored) cannot reveal.
func TestAstSearchReal_MaxMatchesTruncatesRealOutput(t *testing.T) {
	requireAstGrep(t)
	root := astRealFixture(t)
	fs := NewFSTools(root)
	tool := fs.NewAstSearchTool()

	out, err := runTool(astRealCtx(root), tool,
		`{"pattern":"if err != nil { $$$BODY }","language":"go","max_matches":1}`)
	if err != nil {
		t.Fatalf("ast_search returned a Go error: %v", err)
	}
	var res astSearchResult
	if uerr := json.Unmarshal([]byte(out), &res); uerr != nil {
		t.Fatalf("result is not JSON (%v): %s", uerr, out)
	}
	if len(res.Matches) != 1 || !res.Truncated {
		t.Errorf("max_matches=1 gave %d matches truncated=%v, want 1 and true: %s",
			len(res.Matches), res.Truncated, out)
	}
	if res.Total != 2 {
		t.Errorf("Total = %d, want the untruncated count 2; a truncated result that "+
			"under-reports the total tells the model it saw everything", res.Total)
	}
}

// TestAstSearchReal_BadPatternIsAResultNotAnError feeds the real CLI a pattern
// it cannot parse. The tool must surface the failure as a result the model can
// correct, not as a Go error that ends the turn.
//
// This is only checkable against the real binary: whether a given string is a
// parse error is ast-grep's judgement, and a fixture-driven test asserts the
// handling of an exit code the test itself chose.
func TestAstSearchReal_BadPatternIsAResultNotAnError(t *testing.T) {
	requireAstGrep(t)
	root := astRealFixture(t)
	fs := NewFSTools(root)
	tool := fs.NewAstSearchTool()

	// An unknown language is rejected by the CLI itself, unambiguously and
	// across versions -- unlike a malformed pattern, which newer ast-grep
	// versions accept as a literal search.
	out, err := runTool(astRealCtx(root), tool,
		`{"pattern":"if err != nil { $$$ }","language":"nosuchlanguage"}`)
	if err != nil {
		t.Fatalf("a CLI rejection became a Go error, which aborts the turn: %v", err)
	}
	if !strings.Contains(out, "ast_search") {
		t.Errorf("failure result does not name the tool, so the model cannot tell what "+
			"failed: %s", out)
	}
}

// TestAstSearchReal_NoMatchesIsAnEmptyResult pins the difference between "the
// query found nothing" and "the query broke". Both were an empty match list
// before the Total field existed, and against the real CLI this is the case
// most likely to regress silently.
func TestAstSearchReal_NoMatchesIsAnEmptyResult(t *testing.T) {
	requireAstGrep(t)
	root := astRealFixture(t)
	fs := NewFSTools(root)
	tool := fs.NewAstSearchTool()

	out, err := runTool(astRealCtx(root), tool,
		`{"pattern":"func nosuchfunction() { $$$ }","language":"go"}`)
	if err != nil {
		t.Fatalf("ast_search returned a Go error: %v", err)
	}
	var res astSearchResult
	if uerr := json.Unmarshal([]byte(out), &res); uerr != nil {
		t.Fatalf("a zero-match run did not return the documented JSON shape (%v): %s", uerr, out)
	}
	if len(res.Matches) != 0 || res.Total != 0 {
		t.Errorf("expected an empty result, got %d matches / total %d: %s",
			len(res.Matches), res.Total, out)
	}
	if res.Truncated {
		t.Error("an empty result must not be marked truncated")
	}
}

// TestAstSearchReal_MissingBinaryUsesTheRealPathLookup verifies the
// not-installed path against the REAL process PATH rather than the astLookPath
// stub every other test uses.
//
// The stub asserts what the code does when the resolver says "absent"; this
// asserts that the resolver says "absent" on a machine where the binary really
// is. Those differ if the probe ever consults something other than PATH (a
// cached path, a config key, a hardcoded /usr/local/bin), in which case a
// machine without ast-grep would get a subprocess spawn failure -- an opaque
// exec error -- instead of the guidance message that names the fallback.
//
// t.Setenv makes the test non-parallel, which is why the PATH is restored by
// the framework rather than by hand.
func TestAstSearchReal_MissingBinaryUsesTheRealPathLookup(t *testing.T) {
	// An empty PATH is the one value guaranteed to resolve nothing on every
	// platform; pointing it at an empty temp dir would still let a Windows
	// runner find a binary through PATHEXT in the current directory.
	t.Setenv("PATH", "")
	// exec.LookPath caches nothing, but Go does cache a failed lookup's error
	// inside exec.Cmd, not in LookPath, so a fresh probe here is honest.
	if _, ok := astGrepBinary(); ok {
		t.Skip("a binary is still resolvable with an empty PATH (absolute-path install); " +
			"the not-installed path cannot be exercised on this machine")
	}

	root := astRealFixture(t)
	fs := NewFSTools(root)
	tool := fs.NewAstSearchTool()

	out, err := runTool(astRealCtx(root), tool, `{"pattern":"if err != nil { }","language":"go"}`)
	if err != nil {
		t.Fatalf("a missing binary became a Go error, which aborts the turn: %v", err)
	}
	// The model must be told the fallback and the fix, in words it can act on.
	for _, want := range []string{"ast-grep is not installed", "fs_search"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing-binary result does not mention %q, so the model cannot "+
				"recover: %s", want, out)
		}
	}
	if strings.Contains(out, "exec:") || strings.Contains(out, "no such file") {
		t.Errorf("missing-binary result leaked a raw exec error instead of the guidance "+
			"message: %s", out)
	}
}
