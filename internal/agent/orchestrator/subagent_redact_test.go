// internal/agent/orchestrator/subagent_redact_test.go
//
// W-A-02 fix round 2: managed sub-agents (agent_spawn/agent_resume, both in
// DefaultOrchestratorProfile's allow list) ran with no redactor at all. The
// registry Manager is built with RootContext: context.Background() -- a bare
// context -- so a managed turn's ctx carries nothing from the main turn
// except what managedTurnRunner.Run re-binds explicitly, and Redactor was
// missing from that list. Every shell_run/fs_read result a managed sub-agent
// produced reached its own model call unredacted.
//
// Two call graphs needed the fix, and this file covers each independently:
//   - managedTurnRunner.Run's own binding, exercised via TestManagedTurnRunner
//     by calling Run directly with a bare ctx -- the exact shape the real
//     registry.Manager hands it (see manager.go: parentCtx := m.rootCtx for a
//     first-level spawn).
//   - runSubAgentTurn's inline New(Config{...}) fallback, exercised via
//     TestRunSubAgentTurnInlineFallbackBindsRedactor by calling
//     o.runSubAgentTurn directly with a ctx that has neither WithManager nor
//     WithRedactor bound -- the shape agent_batch's row runner and the legacy
//     SubAgentRunner hand it when reached from an already-detached context.
//     This test is deliberately independent of managedTurnRunner.Run so it
//     cannot pass on the strength of the OTHER fix alone.
package orchestrator

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/registry"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/secrets"
	"github.com/x6nux/yanshi/internal/tools"
)

// newRedactorProbeTool builds a tool that records whether a *secrets.Redactor
// was resolvable from its execution context, into the two pointers supplied
// by the caller. Using a captured variable rather than the tool's return text
// keeps the assertion independent of what a FakeModel does with that text --
// FakeModelWithMessages plays back canned messages regardless of tool output.
func newRedactorProbeTool(sawIt *bool, got **secrets.Redactor) *tools.GuardedTool {
	return tools.NewGuardedTool("probe_redactor", "Probe", "records redactor presence", 5*time.Second, nil,
		func(ctx context.Context, _ string) <-chan tools.ToolChunk {
			ch := make(chan tools.ToolChunk, 1)
			if r, ok := tools.RedactorFromContext(ctx); ok {
				*sawIt = true
				*got = r
			}
			ch <- tools.ToolChunk{Result: "checked"}
			close(ch)
			return ch
		})
}

// probeToolCallThenAnswer returns the two-message FakeModel script every test
// in this file drives: one turn that calls probe_redactor, one that answers.
func probeToolCallThenAnswer() []*schema.Message {
	call := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name: "probe_redactor", Arguments: `{}`,
		}},
	})
	answer := schema.AssistantMessage("done", nil)
	return []*schema.Message{call, answer}
}

// TestManagedTurnRunnerBindsRedactor drives the real managed sub-agent entry
// point (managedTurnRunner.Run, the concrete registry.Runner agent_spawn and
// agent_resume ultimately invoke) with a bare context -- exactly what the
// real registry.Manager hands a first-level spawn's Run call, per
// manager.go's `parentCtx := m.rootCtx`. Before the fix this context reached
// the tool call with no redactor bound at any point in the chain.
func TestManagedTurnRunnerBindsRedactor(t *testing.T) {
	var sawIt bool
	var got *secrets.Redactor
	probe := newRedactorProbeTool(&sawIt, &got)

	red := secrets.NewRedactor()
	red.Register("managed-sub-agent-secret-XYZ")

	model := einollm.NewFakeModelWithMessages(probeToolCallThenAnswer(), nil)
	profile := guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"probe_redactor"}}}
	orch, err := New(Config{
		Model:    model,
		Tools:    []BaseTool{probe},
		Profile:  profile,
		Redactor: red,
	})
	require.NoError(t, err)

	mgr := registry.NewManager(registry.NewManagerOpts{RootContext: context.Background(), Path: t.TempDir()})
	t.Cleanup(mgr.Close)

	runner := &managedTurnRunner{
		o:       orch,
		mgr:     mgr,
		profile: orch.ProfileForTest(),
		allowed: []string{"probe_redactor"},
	}

	// context.Background(), not a context descending from any turn: this is
	// the bare shape managedTurnRunner.Run actually receives in production.
	out, err := runner.Run(context.Background(), "sub-1", "run the probe")
	require.NoError(t, err)
	assert.Equal(t, "done", out)

	require.True(t, sawIt, "managedTurnRunner.Run must bind the redactor onto the "+
		"sub-agent's execution context; a tool it ran could not resolve one via "+
		"tools.RedactorFromContext")
	assert.Same(t, red, got, "the sub-agent must see the orchestrator's own redactor, not a copy or a different instance")
}

// TestRunSubAgentTurnInlineFallbackBindsRedactor isolates the inline
// New(Config{...}) fallback in runSubAgentTurn (the branch taken whenever no
// Manager/factory is bound on ctx -- agent_batch's per-row runner and the
// legacy SubAgentRunner both reach it this way). ctx here carries neither
// WithManager nor WithRedactor, and managedTurnRunner.Run is never called, so
// this test cannot pass on the strength of that other fix -- only the
// Redactor field on the inline Config literal can make it pass.
func TestRunSubAgentTurnInlineFallbackBindsRedactor(t *testing.T) {
	var sawIt bool
	var got *secrets.Redactor
	probe := newRedactorProbeTool(&sawIt, &got)

	red := secrets.NewRedactor()
	red.Register("inline-fallback-secret-XYZ")

	model := einollm.NewFakeModelWithMessages(probeToolCallThenAnswer(), nil)
	profile := guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"probe_redactor"}}}
	orch, err := New(Config{
		Model:    model,
		Tools:    []BaseTool{probe},
		Profile:  profile,
		Redactor: red,
	})
	require.NoError(t, err)

	// A bare ctx with neither WithManager nor WithRedactor bound -- the shape
	// this path actually receives from batch.rowRunner.Run and from the
	// legacy tools.SubAgentRunner closure when invoked off a detached ctx.
	out, err := orch.runSubAgentTurn(context.Background(), "run the probe", []string{"probe_redactor"}, "", 0)
	require.NoError(t, err)
	assert.Equal(t, "done", out)

	require.True(t, sawIt, "runSubAgentTurn's inline New(Config{...}) fallback must set "+
		"Redactor: o.redactor, or the nested sub-Orchestrator it builds runs with none")
	assert.Same(t, red, got, "the sub-agent must see the orchestrator's own redactor, not a copy or a different instance")
}

// TestManagedTurnRunnerRunSourcePinsTheRedactorBind is a source-pin
// regression detector for managedTurnRunner.Run's own WithRedactor call.
//
// It exists because TestManagedTurnRunnerBindsRedactor, despite driving the
// real production Run() method, was empirically found NOT to fail when that
// binding alone is removed: Run()'s ctx never carries tools.WithManager, so
// r.o.runSubAgentTurn always takes the inline New(Config{...}) fallback
// underneath it, and that fallback independently re-establishes the redactor
// from the SAME r.o.redactor field. The behavioral test can therefore only
// catch both fixes missing at once, not either one alone. This test closes
// that gap for Run()'s own binding by pinning its source, mirroring the
// established technique in subusage_test.go.
func TestManagedTurnRunnerRunSourcePinsTheRedactorBind(t *testing.T) {
	src, err := os.ReadFile("orchestrator.go")
	if err != nil {
		t.Fatalf("read orchestrator.go: %v", err)
	}
	body := string(src)

	runStart := strings.Index(body, "func (r *managedTurnRunner) Run(ctx context.Context, agentID, assignment string) (string, error) {")
	if runStart < 0 {
		t.Fatal("managedTurnRunner.Run has moved or been renamed; this guard needs rewriting")
	}
	runEnd := strings.Index(body[runStart:], "\nfunc ")
	if runEnd < 0 {
		runEnd = len(body) - runStart
	}
	runBody := body[runStart : runStart+runEnd]

	const bindLine = "ctx = tools.WithRedactor(ctx, r.o.redactor)"
	if !strings.Contains(runBody, bindLine) {
		t.Error("managedTurnRunner.Run no longer binds the redactor onto ctx via " +
			bindLine + " -- without it, a managed sub-agent turn only gets a redactor " +
			"by accident, via runSubAgentTurn's inline fallback branch, which is not " +
			"reachable when a Manager/factory IS bound on ctx (the actual production " +
			"shape for agent_spawn/agent_resume)")
	}
}
