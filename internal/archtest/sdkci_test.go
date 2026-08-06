package archtest

import (
	"strings"
	"testing"
)

// TestSDKContractSuitesRunInCI pins that both client test suites are actually
// executed by a workflow.
//
// Before the sdk-contract job, NO workflow ran either one. The only mention of
// the SDKs anywhere under .github/ was docs.yml doing `tsc --noEmit` and
// `python -c "import yanshi_sdk"` — a typecheck and an import smoke. `vitest`
// and `pytest` had zero hits across every workflow file, so 60-odd contract
// assertions ran on developer machines only. Nothing was red; the axis simply
// was not covered.
func TestSDKContractSuitesRunInCI(t *testing.T) {
	ci := readWorkflow(t, "ci.yml")
	body, ok := workflowJobBody(ci, "sdk-contract")
	if !ok {
		t.Fatal("ci.yml has no sdk-contract job: the SDK client suites are not run by CI")
	}
	for _, want := range []string{"vitest run", "pytest sdk/python"} {
		if !strings.Contains(body, want) {
			t.Errorf("the sdk-contract job does not run %q", want)
		}
	}
	// The Python extra is what supplies jsonschema and pytest-asyncio. Without
	// it the suite errors on import, which reads as a broken job rather than
	// as a missing dependency.
	if !strings.Contains(body, "sdk/python[test]") {
		t.Error("the sdk-contract job installs sdk/python without the [test] extra; " +
			"pytest will fail on `import jsonschema` before running an assertion")
	}
	if strings.Contains(body, "continue-on-error") {
		t.Error("the sdk-contract job is continue-on-error: it reports green whatever " +
			"the contract suites say, which is the state it was created to leave")
	}
}

// TestSDKContractJobIsNotPathsFiltered keeps the job on every PR.
//
// docs.yml is paths-filtered, so putting the SDK suites there would skip them
// for an sdk-only change — the gate filtered out of its own subject. ci.yml
// has no paths filter; this asserts nobody adds one.
func TestSDKContractJobIsNotPathsFiltered(t *testing.T) {
	ci := readWorkflow(t, "ci.yml")
	head, _, _ := strings.Cut(ci, "\njobs:")
	if strings.Contains(head, "paths:") {
		t.Error("ci.yml gained a paths filter; sdk-contract (and every other hard gate " +
			"in this file) would stop running for changes outside the list")
	}
}

// TestDocsWorkflowFiltersCoverWhatItReads pins docs.yml's paths against the
// directories its own steps read.
//
// The filter listed docs/, examples/, the two generators, internal/config and
// internal/api/v1 — but the workflow also typechecks sdk/ts, pip-installs
// sdk/python, and runs the binary built from cmd/yanshi. An sdk-only or
// headless-only change therefore skipped the steps that exercise it. That is
// worse than an absent gate: the checks exist, look green in the branch
// protection list, and never ran.
func TestDocsWorkflowFiltersCoverWhatItReads(t *testing.T) {
	docs := readWorkflow(t, "docs.yml")
	head, _, found := strings.Cut(docs, "\njobs:")
	if !found {
		t.Fatal("docs.yml has no jobs: block")
	}
	// Both triggers (push and pull_request) carry their own list; a path added
	// to one only is the same hole on the other trigger.
	for _, want := range []string{"'sdk/**'", "'cmd/yanshi/**'"} {
		if strings.Count(head, want) != 2 {
			t.Errorf("docs.yml's paths filters mention %s %d time(s), want 2 "+
				"(once under push, once under pull_request) — this workflow reads "+
				"that directory", want, strings.Count(head, want))
		}
	}
}

// TestHeadlessSmokeAssertsTurnCount pins the assertion that guards --file.
//
// The smoke step ran `--input jsonl --file <3-line file>` with output
// redirected to /dev/null and no assertion at all, while the --file branch
// made the whole file ONE prompt regardless of --input. The single thing
// exercising that path could not observe that it ran 1 turn instead of 3.
//
// Counting "done" records specifically matters: one turn already emits three
// output lines, so a laxer `grep -c '"type"'` passes on the very bug this
// guards.
func TestHeadlessSmokeAssertsTurnCount(t *testing.T) {
	docs := readWorkflow(t, "docs.yml")
	body, ok := workflowJobBody(docs, "docs-gate")
	if !ok {
		t.Fatal("docs.yml has no docs-gate job")
	}
	smoke, _, _ := strings.Cut(body, "TS typecheck")
	if !strings.Contains(smoke, "--file examples/headless-batch/sample.jsonl") {
		t.Fatal("the headless smoke no longer runs the --file batch")
	}
	if strings.Contains(smoke, "sample.jsonl >/dev/null") {
		t.Error("the --file smoke discards its output again; it cannot observe " +
			"whether --input was honoured")
	}
	if !strings.Contains(smoke, `grep -c '"type":"done"'`) {
		t.Error("the --file smoke does not count completed turns; without that it " +
			"passes when the whole file collapses into a single turn")
	}
}
