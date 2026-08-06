package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/registry"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// parentHintMarker is the literal the parent-facing hint block is introduced
// with. Tests assert on it verbatim so a silent rename of the marker (which the
// parent agent's prompt contract depends on) shows up as a red test.
const parentHintMarker = "[parent working-set hint — EVIDENCE]"

// fiveSectionResult is a sub-agent result honouring the five-section output
// contract, with a non-empty EVIDENCE block.
const fiveSectionResult = `SUMMARY:
looked at the loader
CHANGES:
none
EVIDENCE:
internal/config/config.go:275
internal/config/config.go:551
RISKS:
none
BLOCKERS:
none`

// drainResult consumes a tool chunk stream and returns the concatenation of
// the terminal Result chunks (progress chunks only carry Text/Status).
func drainResult(ch <-chan ToolChunk) string {
	var sb strings.Builder
	for c := range ch {
		if c.Result != "" {
			sb.WriteString(c.Result)
		}
	}
	return sb.String()
}

// hintTools builds an AgentTools whose sub-agent runner returns the given
// canned result, so the test observes exactly what the parent-facing path does
// to that result and nothing else.
func hintTools(t *testing.T, canned string) (*AgentTools, context.Context) {
	t.Helper()
	at := NewAgentTools(einollm.NewFakeModel([]string{"unused"}, nil))
	runner := SubAgentRunner(func(context.Context, string, []string, string) (string, error) {
		return canned, nil
	})
	return at, WithSubAgentRunner(context.Background(), runner)
}

// TestAgentStart_AppendsParentWorkingSetHint pins the last acceptance bullet of
// ledger entry B1/M04b ("the parent can consume EVIDENCE"): agent_start is a
// terminal path — its result goes straight back to the parent agent — so the
// EVIDENCE section must be re-surfaced as an explicit parent-facing hint rather
// than staying buried in an opaque blob.
//
// ledger: B1/M04b#4 父可消费 EVIDENCE
func TestAgentStart_AppendsParentWorkingSetHint(t *testing.T) {
	at, ctx := hintTools(t, fiveSectionResult)
	got := drainResult(at.streamStartAgent(ctx, `{"prompt":"inspect the config loader"}`))

	require.Contains(t, got, parentHintMarker, "agent_start must surface EVIDENCE to the parent")
	require.Contains(t, got, "internal/config/config.go:275", "the hint must carry the evidence paths")
	require.True(t, strings.HasPrefix(got, fiveSectionResult),
		"the original result must be preserved verbatim ahead of the hint")
}

// TestAnalysis_AppendsParentWorkingSetHint covers the second terminal path:
// analysis in "agent" mode also hands its result straight to the parent.
//
// ledger: B1/M04b#4 父可消费 EVIDENCE
func TestAnalysis_AppendsParentWorkingSetHint(t *testing.T) {
	at, ctx := hintTools(t, fiveSectionResult)
	got := drainResult(at.streamAnalysis(ctx, `{"target":"internal/config","mode":"agent"}`))

	require.Contains(t, got, parentHintMarker, "analysis must surface EVIDENCE to the parent")
	require.Contains(t, got, "internal/config/config.go:551", "the hint must carry the evidence paths")
}

// TestAgentStart_NoEvidenceIsPassedThroughVerbatim guards the other half of the
// contract: a sub-agent that does not follow the five-section format (no
// EVIDENCE block) must have its result forwarded byte-for-byte. Appending an
// empty hint header would corrupt free-form tool output.
//
// ledger: B1/M04b#4 父可消费 EVIDENCE
func TestAgentStart_NoEvidenceIsPassedThroughVerbatim(t *testing.T) {
	const freeform = "just a plain answer\nwith two lines and no sections at all"
	at, ctx := hintTools(t, freeform)
	got := drainResult(at.streamStartAgent(ctx, `{"prompt":"answer briefly"}`))

	require.Equal(t, freeform, got, "a result without EVIDENCE must pass through unchanged")
}

// TestRegistryAndToolsAgreeOnMaxDepth reconciles the two depth constants.
//
// registry cannot import tools — tools imports registry, so the edge would be
// a cycle — which is why registry.MaxDepth is a second constant rather than a
// reference. That leaves nothing structural to keep them equal: the value was
// an inline `depth > 3` with the name in a comment, and raising
// MaxSubAgentDepth would have left registry stopping at the old number with
// every test still green. This package can see both, so the reconciliation
// lives here.
//
// The comparison is on the value AND on the boundary: registry rejects at
// `depth >= MaxDepth` and orchestrator.runSubAgentTurn at
// `depth >= tools.MaxSubAgentDepth`, so equal constants now mean equal
// deepest legal chains. They did not before — registry used `>`, admitting one
// level more than the orchestrator did on the same nominal limit.
//
// ledger: F2/LEAK2#4 与深度上限交互文档化
func TestRegistryAndToolsAgreeOnMaxDepth(t *testing.T) {
	require.Equal(t, MaxSubAgentDepth, registry.MaxDepth,
		"the two nesting limits disagree: a sub-agent chain would be cut at one depth by "+
			"the registry and at another by the orchestrator, and which one wins depends on "+
			"whether a Manager is bound to the turn")
	require.Positive(t, registry.MaxDepth, "a limit of zero forbids every sub-agent")
}
