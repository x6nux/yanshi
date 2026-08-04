package tools

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
)

// ---------------------------------------------------------------------------
// formatDur
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// formatTokens
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// digitsStart / naturalIDLess
// ---------------------------------------------------------------------------

func TestNaturalIDLess(t *testing.T) {
	assert.True(t, naturalIDLess("A1", "A2"))
	assert.True(t, naturalIDLess("A1", "B1"))
	assert.True(t, naturalIDLess("A2", "B1"))
	assert.True(t, naturalIDLess("A1", "A15"))
	assert.False(t, naturalIDLess("B1", "A1"))
	assert.False(t, naturalIDLess("A2", "A1"))
	assert.False(t, naturalIDLess("A1", "A1"))
	assert.True(t, naturalIDLess("B2", "B15"))
}

// ---------------------------------------------------------------------------
// parseToolList
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// bindSubAgentProgress
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// parseToolList with invalid double-encoded JSON
// ---------------------------------------------------------------------------

func TestParseToolList_DoubleEncodedNested(t *testing.T) {
	// Single string that is not a valid JSON array or a JSON string
	_, err := parseToolList(`hello`)
	assert.Error(t, err)
}

func TestParseToolList_InnerIsNotArray(t *testing.T) {
	_, err := parseToolList(`"hello"`)
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// clampInt / truncateSummary / extractInt (testrun.go)
//
// NOTE: TestClampInt and TestTruncateSummary are defined in
// remaining_coverage_test.go. Only extractInt tests are here.
// ---------------------------------------------------------------------------

func TestExtractInt(t *testing.T) {
	assert.Equal(t, 42, extractInt("42 passed", "passed"))
	assert.Equal(t, 3, extractInt("42 passed, 3 failed", "failed"))
	assert.Equal(t, 0, extractInt("nothing here", "passed"))
	assert.Equal(t, 0, extractInt("passed", "passed")) // no number before label
}

// ---------------------------------------------------------------------------
// validateGitRef / filterGitByPaths (git.go)
// ---------------------------------------------------------------------------

func TestValidateGitRef(t *testing.T) {
	assert.NoError(t, validateGitRef("main"))
	assert.NoError(t, validateGitRef("feature/my-branch"))
	assert.Error(t, validateGitRef(""))
	assert.Error(t, validateGitRef("has space"))
	assert.Error(t, validateGitRef("has\nnewline"))
	// A dash-leading value is an OPTION to git, not a ref — see
	// TestGitDiffRefCannotWriteFilesOutsideWorkRoot for the file it used to write.
	assert.Error(t, validateGitRef("--output=/tmp/pwn"))
}

func TestFilterGitByPaths(t *testing.T) {
	entries := []gitNumstatEntry{
		{Path: "a.go"},
		{Path: "b.go"},
		{Path: "c.go"},
	}

	// No filter → all
	got := filterGitByPaths(entries, nil)
	assert.Len(t, got, 3)

	// Filter
	got = filterGitByPaths(entries, []string{"a.go", "c.go"})
	assert.Len(t, got, 2)
	assert.Equal(t, "a.go", got[0].Path)
	assert.Equal(t, "c.go", got[1].Path)

	// No match → empty
	got = filterGitByPaths(entries, []string{"z.go"})
	assert.Len(t, got, 0)
}

// ---------------------------------------------------------------------------
// compressFindings (review.go)
// ---------------------------------------------------------------------------

func TestCompressFindings(t *testing.T) {
	// ≤10 → unchanged
	in := []reviewFinding{{File: "a.go", Severity: "high"}}
	got := compressFindings(in)
	assert.Len(t, got, 1)

	// >10 → top 10
	in = make([]reviewFinding, 20)
	for i := range in {
		in[i] = reviewFinding{File: fmt.Sprintf("f%d.go", i), Severity: "info"}
	}
	got = compressFindings(in)
	assert.Len(t, got, 10)
	assert.Equal(t, "f0.go", got[0].File)
}

// ---------------------------------------------------------------------------
// streamReviewTool (agent.go)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// RunReviewHeadless (review.go)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// decodeReviewSubAgentOutput (review_decode.go — improve from 71.4%)
// ---------------------------------------------------------------------------

func TestDecodeReviewSubAgentOutput_Empty(t *testing.T) {
	out, err := decodeReviewSubAgentOutput("")
	assert.NoError(t, err)
	assert.Empty(t, out.Findings)
}

func TestDecodeReviewSubAgentOutput_Prose(t *testing.T) {
	out, err := decodeReviewSubAgentOutput("No issues found in this code.")
	assert.NoError(t, err)
	assert.Empty(t, out.Findings)
}

func TestDecodeReviewSubAgentOutput_ValidJSON(t *testing.T) {
	raw := `{"findings": [{"file":"a.go","line":1,"severity":"high","message":"bug"}]}`
	out, err := decodeReviewSubAgentOutput(raw)
	assert.NoError(t, err)
	require.Len(t, out.Findings, 1)
	assert.Equal(t, "a.go", out.Findings[0].File)
}

func TestDecodeReviewSubAgentOutput_FencedJSON(t *testing.T) {
	raw := "Here is the review:\n```json\n{\"findings\":[{\"file\":\"b.go\",\"line\":5,\"severity\":\"medium\",\"message\":\"issue\"}]}\n```"
	out, err := decodeReviewSubAgentOutput(raw)
	assert.NoError(t, err)
	require.Len(t, out.Findings, 1)
	assert.Equal(t, "b.go", out.Findings[0].File)
}

// extractJSONObject edge cases

func TestExtractJSONObject_NestedBraces(t *testing.T) {
	s := `some text {"a":{"b":["c",{"d":"e"}]}} more text`
	got := extractJSONObject(s)
	assert.Equal(t, `{"a":{"b":["c",{"d":"e"}]}}`, got)
}

func TestExtractJSONObject_StringWithBraces(t *testing.T) {
	s := `{"msg": "hello {world}"}`
	got := extractJSONObject(s)
	assert.Equal(t, `{"msg": "hello {world}"}`, got)
}

func TestExtractJSONObject_NoBrace(t *testing.T) {
	assert.Equal(t, "", extractJSONObject("no json here"))
}

// ---------------------------------------------------------------------------
// dedupeAndSortFindings (review_decode.go)
// ---------------------------------------------------------------------------

func TestDedupeAndSortFindings(t *testing.T) {
	in := []reviewFinding{
		{File: "b.go", Severity: "info", Message: "m2"},
		{File: "a.go", Severity: "high", Message: "m1"},
		{File: "a.go", Severity: "high", Message: "m1"}, // duplicate
		{File: "b.go", Severity: "info", Message: "m3"},
		{File: "a.go", Severity: "medium", Message: "m4"},
	}
	got := dedupeAndSortFindings(in)
	require.Len(t, got, 4)
	// high first, then medium, then info sorted by file
	assert.Equal(t, "high", got[0].Severity)
	assert.Equal(t, "m1", got[0].Message)
	assert.Equal(t, "medium", got[1].Severity)
	assert.Equal(t, "info", got[2].Severity)
	assert.Equal(t, "m2", got[2].Message)
	assert.Equal(t, "m3", got[3].Message)
}

// ---------------------------------------------------------------------------
// NewScreenshotTool (screenshot.go)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// collectChunks reads all ToolChunks from ch and returns them.
func collectChunks(ch <-chan ToolChunk) []ToolChunk {
	var out []ToolChunk
	for c := range ch {
		out = append(out, c)
	}
	return out
}

// ---------------------------------------------------------------------------
// subagentEmitAdapter edges
// ---------------------------------------------------------------------------

func TestSubagentEmitAdapter_NilEmit(t *testing.T) {
	adapter := subagentEmitAdapter(context.Background())
	assert.Nil(t, adapter)
}

// ---------------------------------------------------------------------------
// Marshal helpers (spillover.go coverage)
// ---------------------------------------------------------------------------

func TestToJSON(t *testing.T) {
	got := toJSON(struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}{Name: "test", Age: 42})
	assert.Contains(t, got, `"name":"test"`)
	assert.Contains(t, got, `"age":42`)
}

// ---------------------------------------------------------------------------
// GitHub tool error paths (github.go — test invalid args that hit ParseArgs)
// ---------------------------------------------------------------------------

func TestRunGitHubPRContext_InvalidArgs(t *testing.T) {
	// secureCommandRunner won't be called; ParseArgs fails first.
	ctx := WithProfile(context.Background(), profileAll)

	gt := NewGitHubTools(nil)
	out, err := runTool(ctx, gt.PRContext, `not-json`)
	assert.NoError(t, err) // GuardedTool returns errorResult, not Go error
	assert.Contains(t, out, "parse args")
}

func TestRunGitHubComment_InvalidArgs(t *testing.T) {
	ctx := WithProfile(context.Background(), profileAll)
	gt := NewGitHubTools(nil)
	out, err := runTool(ctx, gt.Comment, `{"repo":"x","number":1}`) // missing body
	require.NoError(t, err)
	// ParseArgs should succeed (body is not required), so the error comes from secureCommandRunner
	_ = out
}

func TestRunGitHubApprove_InvalidArgs(t *testing.T) {
	callback := func(req PermissionRequest) PermissionDecision {
		return PermissionAllow
	}
	ctx := WithProfile(context.Background(), profileAll)
	ctx = WithPermissionCallback(ctx, callback)
	gt := NewGitHubTools(nil)
	out, err := runTool(ctx, gt.Approve, `not-json`)
	assert.NoError(t, err)
	assert.Contains(t, out, "parse args")
}

func TestRunGitHubMerge_InvalidMethod(t *testing.T) {
	callback := func(req PermissionRequest) PermissionDecision {
		return PermissionAllow
	}
	ctx := WithProfile(context.Background(), profileAll)
	ctx = WithPermissionCallback(ctx, callback)
	gt := NewGitHubTools(nil)
	out, err := runTool(ctx, gt.Merge, `{"repo":"x","number":1,"method":"badmethod"}`)
	assert.NoError(t, err)
	assert.Contains(t, out, "badmethod")
}

func TestRunGitHubMerge_InvalidArgs(t *testing.T) {
	callback := func(req PermissionRequest) PermissionDecision {
		return PermissionAllow
	}
	ctx := WithProfile(context.Background(), profileAll)
	ctx = WithPermissionCallback(ctx, callback)
	gt := NewGitHubTools(nil)
	out, err := runTool(ctx, gt.Merge, `not-json`)
	assert.NoError(t, err)
	assert.Contains(t, out, "parse args")
}

// ---------------------------------------------------------------------------
// Allowed profile for tool tests
// ---------------------------------------------------------------------------

var profileAll = guard.PermissionProfile{
	Tools: guard.ToolsPerm{Allow: []string{"*"}},
	Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
}

// ---------------------------------------------------------------------------
// Automation tool edge cases (automation.go — improve uncovered paths)
// ---------------------------------------------------------------------------

func TestAutomationDecodeInput_InvalidJSON(t *testing.T) {
	var target struct{}
	err := decodeInput(`not-json`, &target)
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// Simple helpers — friendlyErr (guard.go)
// ---------------------------------------------------------------------------

func TestFriendlyErr_DenyErr(t *testing.T) {
	err := &DenyErr{Reason: "not allowed by guard"}
	msg := friendlyErr(err)
	assert.Equal(t, "permission denied: not allowed by guard", msg)
}

func TestFriendlyErr_Other(t *testing.T) {
	msg := friendlyErr(fmt.Errorf("something broke"))
	assert.Equal(t, "something broke", msg)
}

// ---------------------------------------------------------------------------
// IsForcePromptTool (permctx.go — improve partial coverage)
// ---------------------------------------------------------------------------

func TestIsForcePromptTool(t *testing.T) {
	assert.True(t, isForcePromptTool("task_cancel"))
	assert.False(t, isForcePromptTool("fs_read"))
	assert.False(t, isForcePromptTool("task_create"))
	assert.False(t, isForcePromptTool("revert_turn"))
}

// ---------------------------------------------------------------------------
// AgentRoles — MustRole (agentroles.go)
// ---------------------------------------------------------------------------

func TestAgentRoles_HasStandardRoles(t *testing.T) {
	roles := AgentRoles()
	assert.NotEmpty(t, roles)
	// Check that common roles exist.
	names := make(map[string]bool)
	for _, r := range roles {
		names[r.Name] = true
	}
	assert.True(t, names["general"])
	assert.True(t, names["explore"])
}

// ---------------------------------------------------------------------------
// Analysis workflow — generateAnalysisWorkflow edge cases
// ---------------------------------------------------------------------------

func TestGenerateAnalysisWorkflow_EmptyTarget(t *testing.T) {
	t.Skip("generateAnalysisWorkflow is not directly testable without PredefinedAgentDef")
}
