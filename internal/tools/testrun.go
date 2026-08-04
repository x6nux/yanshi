package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
)

type runTestsArgs struct {
	Framework string   `json:"framework,omitempty"`
	Packages  []string `json:"packages,omitempty"`
	Filter    string   `json:"filter,omitempty"`
	TimeoutS  int      `json:"timeout_s,omitempty"`
}

type testFailure struct {
	Package string `json:"package,omitempty"`
	Test    string `json:"test,omitempty"`
	Message string `json:"message,omitempty"`
}

type testResult struct {
	Framework   string        `json:"framework"`
	Status      string        `json:"status"`
	Passed      int           `json:"passed"`
	Failed      int           `json:"failed"`
	Skipped     int           `json:"skipped"`
	DurationMS  int64         `json:"duration_ms"`
	Failures    []testFailure `json:"failures,omitempty"`
	Summary     string        `json:"summary"`
	ArtifactRef string        `json:"artifact_ref,omitempty"`
	Degraded    bool          `json:"degraded,omitempty"`
}

// NewTestRunTool creates a GuardedTool that runs Go/cargo/npm tests and returns
// structured results.
func NewTestRunTool() *GuardedTool {
	return NewGuardedTool("run_tests", "Run tests", "Run Go/cargo/npm tests and return structured results.", 10*time.Minute,
		params(map[string]*schema.ParameterInfo{
			"framework": {Type: schema.String, Enum: []string{"auto", "go", "cargo", "npm"}},
			"packages":  {Type: schema.Array, ElemInfo: &schema.ParameterInfo{Type: schema.String}},
			"filter":    {Type: schema.String},
			"timeout_s": {Type: schema.Integer},
		}), SyncStream(runTests))
}

func runTests(ctx context.Context, argsJSON string) (string, error) {
	var args runTestsArgs
	if err := ParseArgs(argsJSON, &args); err != nil {
		return errorResult(err.Error()), nil
	}
	root := WorkRootFromContext(ctx)
	framework := args.Framework
	if framework == "" || framework == "auto" {
		framework = detectRunner(detectMarkerFiles(root))
		if framework == "" {
			return errorResult("no go.mod / Cargo.toml / package.json in work root"), nil
		}
	}
	// Default to 10 minutes when the caller omits TimeoutS. The previous
	// clampInt(args.TimeoutS, 1, 1800) clamped 0 → 1, making the default a
	// 1-second timeout — fine for the scripted factory on a fast host but a
	// guaranteed "context deadline exceeded" under -race (helper subprocess
	// takes >1s to initialize the race detector) and on slow CI runners.
	timeout := 10 * time.Minute
	if args.TimeoutS > 0 {
		timeout = time.Duration(clampInt(args.TimeoutS, 1, 1800)) * time.Second
	}
	spec := testSpec(framework, args, root)
	start := time.Now()
	res, err := secureCommandRunner(ctx, spec, timeout)
	duration := time.Since(start).Milliseconds()
	if err != nil {
		return errorResult("run_tests: " + err.Error()), nil
	}
	parsed := reconcileExitCode(parseTestResult(framework, res), res)
	parsed.DurationMS = duration
	raw := res.Stdout + res.Stderr
	if len(raw) > SpillThreshold {
		art := writeArtifactOrSpill(ctx, "run-tests", "test-output", raw)
		parsed.Summary = truncateSummary(parsed.Summary, 4096)
		parsed.ArtifactRef = art.ArtifactRef
		parsed.Degraded = art.Degraded
	}
	return toJSON(parsed), nil
}

func testSpec(framework string, args runTestsArgs, root string) secproc.SecureProcessSpec {
	base := secproc.SecureProcessSpec{Tool: "run_tests", Dir: root, UseSandboxTier: sandbox.WorkspaceWrite}
	switch framework {
	case "go":
		argv := []string{"test", "-json"}
		if len(args.Packages) > 0 {
			argv = append(argv, args.Packages...)
		} else {
			argv = append(argv, "./...")
		}
		if args.Filter != "" {
			argv = append(argv, "-run", args.Filter)
		}
		base.Program, base.Args = "go", argv
	case "cargo":
		argv := []string{"test"}
		if args.Filter != "" {
			argv = append(argv, args.Filter)
		}
		base.Program, base.Args = "cargo", argv
	case "npm":
		argv := []string{"test", "--", "--json"}
		if args.Filter != "" {
			argv = append(argv, "--testNamePattern", args.Filter)
		}
		base.Program, base.Args = "npm", argv
	}
	return base
}

func detectMarkerFiles(root string) map[string]bool {
	markers := []string{"go.mod", "Cargo.toml", "package.json"}
	out := map[string]bool{}
	for _, name := range markers {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			out[name] = true
		}
	}
	return out
}

func detectRunner(files map[string]bool) string {
	if files["go.mod"] {
		return "go"
	}
	if files["Cargo.toml"] {
		return "cargo"
	}
	if files["package.json"] {
		return "npm"
	}
	return ""
}

// reconcileExitCode makes the runner's exit status authoritative over the
// output parsers.
//
// Every parser derives status from the per-test events it can see, so a run
// that never got as far as running a test parses as "0 passed, 0 failed" and
// each parser then declares status "pass". That is how run_tests reported
// {"status":"pass","passed":0,"failed":0} for `go test` failing with "module
// cache not found": a green light for a command that never executed. The exit
// code is the one signal that is present in every one of those cases.
//
// Non-zero exit with parsed failures stays "fail" (the parser already knows
// what broke). Non-zero exit with no parsed failures becomes "error" and
// carries the tail of the runner's own output, because that is the only place
// the reason exists — including when a parser already said "error" with a
// summary describing its own confusion ("npm: no JSON output") rather than the
// runner's complaint ("npm ERR! missing script: test").
func reconcileExitCode(result testResult, res commandResult) testResult {
	if res.ExitCode == 0 {
		return result
	}
	if result.Failed > 0 {
		if result.Status == "pass" {
			result.Status = "fail"
		}
		return result
	}
	result.Status = "error"
	result.Summary = fmt.Sprintf("%s exited %d without reporting any test result: %s",
		result.Framework, res.ExitCode, commandFailureTail(res))
	return result
}

// parseTestResult routes a finished run to its framework parser. EVERY parser
// gets res.Stdout ALONE — never Stdout+Stderr — because all three runners emit
// their machine-readable result on stdout and their chatter on stderr, and
// splicing the two makes the parser report things that did not happen:
//
//   - npm: the reporter's JSON document is the whole of stdout, so appending
//     stderr puts trailing text after the top-level value and json.Unmarshal
//     fails. A passing suite plus one "npm WARN config …" line came back as
//     status:"error", summary:"npm: invalid character 'n' after top-level
//     value" — a green run reported as a broken runner.
//   - cargo: the failure roster is found by searching for "failures:\n". Any
//     stderr line ending in "failures:" (a rustc note, a build warning
//     preamble) makes the following stderr lines get harvested as test names,
//     fabricating failures that no test binary ever produced.
//   - go: already stdout-only; `go test -json` puts even build errors on
//     stdout as build-output events (verified on Go 1.26: stderr is empty for
//     a package that fails to compile).
//
// Nothing is lost by dropping stderr here: a runner that dies without emitting
// a parseable result exits non-zero, and reconcileExitCode then reports
// commandFailureTail(res), which reads stderr precisely for that case.
func parseTestResult(framework string, res commandResult) testResult {
	switch framework {
	case "go":
		return parseGoJSON(res.Stdout)
	case "cargo":
		return parseCargoOutput(res.Stdout)
	case "npm":
		return parseNPMOutput(res.Stdout)
	}
	return testResult{Framework: framework, Status: "error", Summary: "unknown framework"}
}

func parseGoJSON(stdout string) testResult {
	result := testResult{Framework: "go"}
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var ev struct {
			Action, Package, Test string
		}
		if json.Unmarshal(scanner.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Action {
		case "pass":
			result.Passed++
		case "fail":
			result.Failed++
			result.Failures = append(result.Failures, testFailure{Package: ev.Package, Test: ev.Test})
		case "skip":
			result.Skipped++
		}
	}
	result.Status = "pass"
	if result.Failed > 0 {
		result.Status = "fail"
	}
	result.Summary = fmt.Sprintf("go: %d passed, %d failed, %d skipped", result.Passed, result.Failed, result.Skipped)
	return result
}

func parseCargoOutput(stdout string) testResult {
	result := testResult{Framework: "cargo"}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "test result:") {
			result.Passed += extractInt(line, "passed")
			result.Failed += extractInt(line, "failed")
			result.Skipped += extractInt(line, "ignored")
		}
	}
	if idx := strings.Index(stdout, "failures:\n"); idx >= 0 {
		for _, line := range strings.Split(stdout[idx+len("failures:\n"):], "\n") {
			name := strings.TrimSpace(line)
			if name == "" || strings.HasPrefix(name, "----") {
				continue
			}
			if strings.Contains(name, "stdout") || strings.Contains(name, "running") {
				break
			}
			result.Failures = append(result.Failures, testFailure{Test: name})
		}
	}
	result.Status = "pass"
	if result.Failed > 0 {
		result.Status = "fail"
	}
	result.Summary = fmt.Sprintf("cargo: %d passed, %d failed", result.Passed, result.Failed)
	return result
}

func parseNPMOutput(stdout string) testResult {
	result := testResult{Framework: "npm"}
	start := strings.Index(stdout, "{")
	if start < 0 {
		result.Status = "error"
		result.Summary = "npm: no JSON output"
		return result
	}
	var payload struct {
		Stats struct {
			Passes, Failures, Pending int
		}
		Failures []struct{ Name string }
	}
	if err := json.Unmarshal([]byte(stdout[start:]), &payload); err != nil {
		result.Status = "error"
		result.Summary = "npm: " + err.Error()
		return result
	}
	result.Passed, result.Failed, result.Skipped = payload.Stats.Passes, payload.Stats.Failures, payload.Stats.Pending
	for _, f := range payload.Failures {
		result.Failures = append(result.Failures, testFailure{Test: f.Name})
	}
	result.Status = "pass"
	if result.Failed > 0 {
		result.Status = "fail"
	}
	result.Summary = fmt.Sprintf("npm: %d passed, %d failed, %d pending", result.Passed, result.Failed, result.Skipped)
	return result
}

func extractInt(line, label string) int {
	idx := strings.Index(line, label)
	if idx < 0 {
		return 0
	}
	prefix := line[:idx]
	fields := strings.Fields(strings.TrimSpace(prefix))
	if len(fields) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(fields[len(fields)-1])
	return n
}

func clampInt(v, low, high int) int {
	if v < low {
		return low
	}
	if v > high {
		return high
	}
	return v
}

func truncateSummary(s string, max int) string {
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
