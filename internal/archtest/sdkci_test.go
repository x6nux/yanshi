package archtest

import (
	"os"
	"path/filepath"
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
//
// ledger: H2/APIREF1#2 SDK 用法有示例
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
//
// ledger: D1/V12#4 CI 可脚本化
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

// TestSDKSnippetsAreExecutablyChecked closes the loop on the shipped examples.
//
// docs/api/sdk-python.md and examples/sdk-python/main.py both read
// `item.toolName` in the one line each used to show how to consume the item
// stream. toolName is a pydantic wire ALIAS; the attribute is tool_name, so
// both raised AttributeError on the first item. CI ran `py_compile` and
// `import yanshi_sdk` — both pass on code that raises the moment it executes,
// because neither executes it.
//
// The guard is sdk/python/tests/test_alias_attributes.py, which pulls the
// alias set out of model_fields and scans both snippets. This asserts it still
// exists and still names both files; TestSDKContractSuitesRunInCI asserts CI
// runs it. Without this half, deleting the scan or narrowing it to one file
// leaves the pytest job green and the examples unchecked.
//
// ledger: H2/APIREF1#2 SDK 用法有示例
func TestSDKSnippetsAreExecutablyChecked(t *testing.T) {
	guard, err := os.ReadFile(abs(filepath.Join("sdk", "python", "tests", "test_alias_attributes.py")))
	if err != nil {
		t.Fatalf("the snippet guard is gone: %v", err)
	}
	for _, want := range []string{
		`"docs" / "api" / "sdk-python.md"`,
		`"examples" / "sdk-python" / "main.py"`,
		"model_fields",
	} {
		if !strings.Contains(string(guard), want) {
			t.Errorf("the snippet guard no longer references %s", want)
		}
	}
	// Both snippets must exist, or the guard scans nothing and reports clean.
	for _, p := range []string{
		filepath.Join("docs", "api", "sdk-python.md"),
		filepath.Join("examples", "sdk-python", "main.py"),
	} {
		if _, err := os.Stat(abs(p)); err != nil {
			t.Errorf("%s is missing; the guard has nothing to scan: %v", p, err)
		}
	}
}

// TestAPIReferenceDocsCarryTheGeneratedBlocks asserts the reference pages
// exist on disk with generated content in them.
//
// The ledger cited cmd/api-schema's render tests for this. Those are necessary
// and not sufficient: they prove the generator PRODUCES those blocks, in
// memory, from a schema handed to them. They hold just as well if
// docs/api/resources.md was never written, or was written once and later
// emptied — nothing in Go read the files. The only thing that did was
// docs.yml's `git diff --exit-code`, which is a workflow step, not a test, and
// which docs.yml's paths filter could skip.
//
// ledger: H2/APIREF1#1 v1 API 有参考
func TestAPIReferenceDocsCarryTheGeneratedBlocks(t *testing.T) {
	want := map[string][]string{
		filepath.Join("docs", "api", "schema.md"): {
			"<!-- BEGIN GENERATED: api-schema-full -->",
			`"$defs"`,
		},
		filepath.Join("docs", "api", "resources.md"): {
			"<!-- BEGIN GENERATED: api-defs:Thread -->",
			"<!-- BEGIN GENERATED: api-defs:Item -->",
			"<!-- BEGIN GENERATED: api-defs:TurnStartParams -->",
		},
	}
	for path, markers := range want {
		data, err := os.ReadFile(abs(path))
		if err != nil {
			t.Errorf("%s is missing: %v", path, err)
			continue
		}
		for _, m := range markers {
			if !strings.Contains(string(data), m) {
				t.Errorf("%s does not contain %s — the generator can emit it, but the "+
					"committed reference page does not have it", path, m)
			}
		}
	}
}

// jobLevelContinueOnError reports whether a job body carries continue-on-error
// on the JOB, at 4-space indent.
//
// Indentation is the whole point. A plain substring search cannot tell a job
// key from a step key, and nightly's bench job has BOTH: soft at the job
// level, plus a soft "download previous baseline" step. The first version of
// the assertions below used Contains and stayed green when the job-level key
// was deleted, because the step-level one kept matching — an assertion written
// down that was not sensitive to the fact it asserted.
func jobLevelContinueOnError(body string) bool {
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, "    continue-on-error:") &&
			!strings.HasPrefix(line, "     ") {
			return true
		}
	}
	return false
}

// TestFuzzGatesHaveNoEscapeHatch pins that the fuzz jobs fail when their
// targets are gone.
//
// Both jobs used to end their guard with `soft-pass; exit 0`: delete every
// fuzz and property target and they stayed green. The hatch was in the SHELL,
// not on a YAML key, so removing continue-on-error would not have closed it —
// and no gate in this package could see it, because they all reason about Go
// source or workflow keys.
//
// nightly's fuzz-long also carried continue-on-error with the comment "soft
// until E2 fuzz targets land". FuzzMatchGlob has landed. Nightly blocks no
// merge, so that key's only effect was turning red into silence, on a job
// whose entire value is the alert.
func TestFuzzGatesHaveNoEscapeHatch(t *testing.T) {
	cases := []struct{ workflow, job string }{
		{"ci.yml", "fuzz-seed"},
		{"nightly.yml", "fuzz-long"},
	}
	for _, tc := range cases {
		body, ok := workflowJobBody(readWorkflow(t, tc.workflow), tc.job)
		if !ok {
			t.Errorf("%s has no %s job", tc.workflow, tc.job)
			continue
		}
		if strings.Contains(body, "soft-pass") || strings.Contains(body, "exit 0") {
			t.Errorf("%s/%s still soft-passes when its targets are missing: deleting "+
				"every fuzz target would leave it green", tc.workflow, tc.job)
		}
		if jobLevelContinueOnError(body) {
			t.Errorf("%s/%s is continue-on-error; a fuzz job that cannot go red "+
				"reports nothing", tc.workflow, tc.job)
		}
	}
	// The bench trend job stays soft on purpose — ns/op on a shared runner
	// swings past any threshold from neighbour load alone. Asserting it keeps
	// the exemption from being "fixed" by someone applying the rule above
	// uniformly.
	bench, ok := workflowJobBody(readWorkflow(t, "nightly.yml"), "bench")
	if !ok {
		t.Fatal("nightly.yml has no bench job")
	}
	if !jobLevelContinueOnError(bench) {
		t.Error("nightly's bench trend job lost its JOB-level continue-on-error: it is " +
			"trend-only, and a hard gate there manufactures flakes rather than " +
			"catching regressions")
	}
}

// TestReleaseSnapshotIsVerifiedNightly pins the artifact check.
//
// `goreleaser check` in ci.yml only proves the config PARSES. The repo has no
// tags at all, so release.yml has never fired and nothing had ever verified
// that the artifacts build, that the archives carry the licence, or that the
// CGO_ENABLED=0 -tags=nokeyring binary starts.
func TestReleaseSnapshotIsVerifiedNightly(t *testing.T) {
	body, ok := workflowJobBody(readWorkflow(t, "nightly.yml"), "release-snapshot")
	if !ok {
		t.Fatal("nightly.yml has no release-snapshot job: `goreleaser check` alone " +
			"never builds an artifact")
	}
	for _, want := range []string{
		"release --snapshot",
		"grep -qx LICENSE",
		"yanshi -h",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the release-snapshot job does not %s", want)
		}
	}
	if jobLevelContinueOnError(body) {
		t.Error("release-snapshot is continue-on-error: a broken release pipeline " +
			"would report green until the day someone pushes a tag")
	}
}

// TestChangelogGenerationKeepsHistory pins the two release.yml defects that
// had never fired, because this repository has no tags and release.yml has
// never run.
//
//  1. CHANGELOG.md was generated with --latest, which emits only the newest
//     tag's section, while --output OVERWRITES. The first release would have
//     looked fine — no previous tag means the range is all of history — and the
//     SECOND would have replaced the file with just v2's section, dropping v1.
//     RELEASE_NOTES.md keeps --latest on purpose: those notes describe exactly
//     one release.
//
//  2. The write-back ran BEFORE goreleaser and pushed to main. A tag build
//     checks out a detached HEAD, so the push is a non-fast-forward the moment
//     main has moved on — the step fails and the entire Release never goes out.
func TestChangelogGenerationKeepsHistory(t *testing.T) {
	body, ok := workflowJobBody(readWorkflow(t, "release.yml"), "release")
	if !ok {
		t.Fatal("release.yml has no release job")
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "CHANGELOG.md") && strings.Contains(line, "git-cliff") {
			if strings.Contains(line, "--latest") {
				t.Errorf("CHANGELOG.md is generated with --latest and --output overwrites: "+
					"the second release drops every earlier version from the file.\n  %s",
					strings.TrimSpace(line))
			}
		}
		if strings.Contains(line, "RELEASE_NOTES.md") && strings.Contains(line, "git-cliff") {
			if !strings.Contains(line, "--latest") {
				t.Errorf("RELEASE_NOTES.md lost --latest; the notes for one release would "+
					"become the whole history.\n  %s", strings.TrimSpace(line))
			}
		}
	}

	// Ordering: the changelog write-back must come after goreleaser, or a
	// failed push takes the release with it.
	gorel := strings.Index(body, "goreleaser-action")
	changelog := strings.Index(body, "update CHANGELOG.md")
	if gorel < 0 || changelog < 0 {
		t.Fatalf("release.yml lost a step (goreleaser=%d changelog=%d)", gorel, changelog)
	}
	if changelog < gorel {
		t.Error("the changelog write-back runs before goreleaser: a non-fast-forward " +
			"push on a detached HEAD would block artifact publication")
	}
}

// withoutComments drops whole-line comments so an assertion cannot match the
// prose that explains the state it guards against.
//
// This is the third time in this repo that a gate read its own explanation as
// configuration: W7's bench hard-gate check found "continue-on-error" in a
// comment above the job, and the first version of
// TestCliffParsersCoverTheDocumentedPrefixes flagged `"^feat!"` because the
// comment right below the fixed rule quotes the broken one.
func withoutComments(src, marker string) string {
	var out []string
	for line := range strings.SplitSeq(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), marker) {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// TestCliffParsersCoverTheDocumentedPrefixes reconciles cliff.toml with
// docs/commit-convention.md.
//
// The doc has always been the contract; the config is the side that drifted,
// twice, both found in W10. The breaking-change rule was "^feat!", so
// `fix(api)!: drop ThreadSnapshot.Items` — a removal from the wire contract —
// shipped as an ordinary bug fix, while the doc already said any type plus "!"
// is breaking. And `style` had no rule at all, so `style: gofmt the whole
// tree` landed in a section git-cliff derived on its own, outside the ordering
// every other group declares.
//
// Only the parser SIDE is checked. Asserting the rendered group names would
// mean re-deriving git-cliff's template behaviour in Go, and the probe that
// found the style defect showed how easily an assumption about that goes
// wrong: the plan predicted the commit would vanish silently; it did not.
func TestCliffParsersCoverTheDocumentedPrefixes(t *testing.T) {
	cliff, err := os.ReadFile(abs("cliff.toml"))
	if err != nil {
		t.Fatalf("read cliff.toml: %v", err)
	}
	src := withoutComments(string(cliff), "#")

	// Every type the convention doc lists must have a parser.
	for _, prefix := range []string{
		"feat", "fix", "perf", "refactor", "docs", "test", "chore", "ci", "build",
		"style", "revert",
	} {
		if !strings.Contains(src, "^"+prefix) {
			t.Errorf("cliff.toml has no parser for %q; an unmatched type lands in a "+
				"section git-cliff names itself, outside this file's ordering", prefix)
		}
	}

	// The breaking rule must not be tied to one type.
	if strings.Contains(src, `"^feat!"`) {
		t.Error(`cliff.toml's breaking rule is "^feat!": a breaking fix, refactor or ` +
			`perf commit is reported as an ordinary change of its own type`)
	}
	if !strings.Contains(src, `!:`) {
		t.Error("cliff.toml has no rule matching the Conventional Commits `!` marker " +
			"on an arbitrary type")
	}

	// And the doc must not go back to naming only two types.
	doc, err := os.ReadFile(abs(filepath.Join("docs", "commit-convention.md")))
	if err != nil {
		t.Fatalf("read commit-convention.md: %v", err)
	}
	if strings.Contains(string(doc), "| `feat!` / `fix!` |") {
		t.Error("commit-convention.md lists only feat!/fix! as breaking; the rule " +
			"applies to any type")
	}
}
