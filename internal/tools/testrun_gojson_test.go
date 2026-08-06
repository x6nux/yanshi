package tools

import "testing"

// TestParseGoJSONCountsTestsNotPackages pins the distinction `go test -json`
// draws and the parser did not.
//
// Every package emits its own pass/fail event with an EMPTY Test field, in
// addition to one event per test function. Counting both inflates `passed` by
// exactly the number of packages -- so `go test ./...` over this repo reported
// dozens of tests that do not exist, and the count drifted further from the
// truth the more packages were run. A package-level "fail" also entered the
// failure roster as {Package: "x", Test: ""}, which reads as a nameless failing
// test.
//
// The package-level event is not noise, though: a package that fails to
// COMPILE emits a package fail and no test events at all. Dropping it from the
// counts must not drop it from the failure list, or a build break comes back
// as a clean run with zero tests.
func TestParseGoJSONCountsTestsNotPackages(t *testing.T) {
	t.Run("package events do not inflate the counts", func(t *testing.T) {
		in := `{"Action":"pass","Package":"p1","Test":"TestA"}
{"Action":"pass","Package":"p1","Test":"TestB"}
{"Action":"pass","Package":"p1"}
{"Action":"pass","Package":"p2","Test":"TestC"}
{"Action":"pass","Package":"p2"}
`
		got := parseGoJSON(in)
		if got.Passed != 3 {
			t.Fatalf("passed = %d, want 3 (two package events must not count)", got.Passed)
		}
		if got.Status != "pass" {
			t.Fatalf("status = %q", got.Status)
		}
	})

	t.Run("a failing test is counted and named", func(t *testing.T) {
		in := `{"Action":"pass","Package":"p1","Test":"TestA"}
{"Action":"fail","Package":"p1","Test":"TestB"}
{"Action":"fail","Package":"p1"}
`
		got := parseGoJSON(in)
		if got.Failed != 1 {
			t.Fatalf("failed = %d, want 1: the package event is the same failure reported twice", got.Failed)
		}
		if len(got.Failures) != 1 || got.Failures[0].Test != "TestB" {
			t.Fatalf("failure roster = %+v", got.Failures)
		}
		if got.Status != "fail" {
			t.Fatalf("status = %q", got.Status)
		}
	})

	t.Run("a package that fails to compile is still reported", func(t *testing.T) {
		// No test events at all: this is what a build break looks like.
		in := `{"Action":"output","Package":"p1","Output":"syntax error"}
{"Action":"fail","Package":"p1"}
`
		got := parseGoJSON(in)
		if got.Status != "fail" {
			t.Fatalf("a package-level failure with no tests must not read as a pass: %+v", got)
		}
		if len(got.Failures) != 1 || got.Failures[0].Package != "p1" {
			t.Fatalf("the failing package must appear in the roster: %+v", got.Failures)
		}
	})

	t.Run("skips are counted per test", func(t *testing.T) {
		in := `{"Action":"skip","Package":"p1","Test":"TestA"}
{"Action":"skip","Package":"p1"}
`
		got := parseGoJSON(in)
		if got.Skipped != 1 {
			t.Fatalf("skipped = %d, want 1", got.Skipped)
		}
	})
}
