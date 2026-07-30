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
	timeout := time.Duration(clampInt(args.TimeoutS, 1, 1800)) * time.Second
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	spec := testSpec(framework, args, root)
	start := time.Now()
	res, err := secureCommandRunner(ctx, spec, timeout)
	duration := time.Since(start).Milliseconds()
	if err != nil {
		return errorResult("run_tests: " + err.Error()), nil
	}
	parsed := parseTestResult(framework, res)
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

func parseTestResult(framework string, res commandResult) testResult {
	switch framework {
	case "go":
		return parseGoJSON(res.Stdout)
	case "cargo":
		return parseCargoOutput(res.Stdout + res.Stderr)
	case "npm":
		return parseNPMOutput(res.Stdout + res.Stderr)
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
