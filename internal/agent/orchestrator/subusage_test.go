package orchestrator

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// TestUsageIsReportedBeforeTheErrorCheck pins the ordering that keeps a failed
// delegation's spend on the books.
//
// The tokens were spent whether or not the turn produced a usable answer, and
// a budget that only counts successful work is a budget a failing loop can run
// past indefinitely — retry, fail, spend, repeat, with the meter reading zero.
//
// Measured W3 review round 12: moving the forwarding below the error return
// reddened nothing, so the ordering was an argued property. Checked at the
// source because reaching the error path needs an Orchestrator and a model.
func TestUsageIsReportedBeforeTheErrorCheck(t *testing.T) {
	src, err := os.ReadFile("orchestrator.go")
	if err != nil {
		t.Fatalf("read orchestrator.go: %v", err)
	}
	body := string(src)
	report := strings.Index(body, "if u := subAgentUsageForSink(subUsage); u != nil {")
	errCheck := strings.Index(body, `if errMsg != "" {
		return "", fmt.Errorf("sub-agent: %s", errMsg)`)
	if report < 0 || errCheck < 0 {
		t.Fatal("the sub-agent usage forwarding or its error check has moved; this guard needs rewriting")
	}
	if report > errCheck {
		t.Error("usage is now forwarded after the error return: a failed delegation's " +
			"tokens escape the budget entirely")
	}
}

// TestSubAgentUsageForSink pins the mapping that carries a delegated turn's
// spend into the parent's budget.
//
// A dropped field here fails silently and permanently: no error, no missing
// event, the parent simply under-counts by that much and the budget runs
// longer than the operator asked for. Measured in W3 review round 5 — zeroing
// CompletionTokens left the whole suite green.
func TestSubAgentUsageForSink(t *testing.T) {
	t.Run("nothing spent reports nothing", func(t *testing.T) {
		assert.Nil(t, subAgentUsageForSink(TurnUsage{}),
			"a zero turn must not push a no-op entry into the sink")
	})

	t.Run("every field survives", func(t *testing.T) {
		got := subAgentUsageForSink(TurnUsage{PromptTokens: 90, CompletionTokens: 10})
		require.NotNil(t, got)
		assert.Equal(t, int64(90), got.PromptTokens)
		assert.Equal(t, int64(10), got.CompletionTokens)
		assert.Equal(t, int64(100), got.TotalTokens, "the total must equal the parts, not one of them")
	})

	t.Run("either field alone still reports", func(t *testing.T) {
		promptOnly := subAgentUsageForSink(TurnUsage{PromptTokens: 5})
		require.NotNil(t, promptOnly, "a prompt-only turn still spent tokens")
		assert.Equal(t, int64(5), promptOnly.TotalTokens)

		completionOnly := subAgentUsageForSink(TurnUsage{CompletionTokens: 7})
		require.NotNil(t, completionOnly, "a completion-only turn still spent tokens")
		assert.Equal(t, int64(7), completionOnly.TotalTokens)
	})
}

// TestNewWiresCompactionIntoTheModel pins that New actually installs the
// compaction wrapper on the model the orchestrator runs with.
//
// TestWrapCompaction_WithThreshold covers the wrapper function itself, but a
// function that returns the right thing is worthless if its result is not
// stored. W4 review round 13 replaced the wrapCompaction call in New with
// cfg.Model and the whole package stayed green -- the main turn path would
// then never compact, in production, with every test still passing.
func TestNewWiresCompactionIntoTheModel(t *testing.T) {
	o, err := New(Config{
		Model:      einollm.NewFakeModel(nil, nil),
		Compaction: CompactionConfig{Threshold: 0.8, ContextWindow: 1000, KeepRecent: 4},
	})
	require.NoError(t, err)
	require.IsType(t, &einollm.CompactingModel{}, o.model,
		"the orchestrator's model must be the compacting wrapper, not the raw one")
	require.NotNil(t, o.rawModel, "runnerFor still needs the unwrapped model")
}

// TestRunnerForWiresCompactionToo guards the sub-agent half of the compaction
// wiring that TestNewWiresCompactionIntoTheModel covers for the main model.
//
// Both call sites must wrap, and they fail independently: a delegated turn
// runs its own ChatModelAgent, so an unwrapped model there means sub-agents
// never compact while the parent does. W4 review round 14 severed this one
// and the whole package stayed green.
//
// Checked at the source, not through the object: runnerFor returns an
// *adk.Runner and the wrapped model is buried inside adk's agent, with no
// accessor to assert on. Driving a real delegated turn to observe it would
// pin adk's plumbing rather than this line -- the same tradeoff
// TestUsageIsReportedBeforeTheErrorCheck documents above.
func TestRunnerForWiresCompactionToo(t *testing.T) {
	src, err := os.ReadFile("orchestrator.go")
	if err != nil {
		t.Fatalf("read orchestrator.go: %v", err)
	}
	if !strings.Contains(string(src), "Model:         wrapCompaction(chatModel, o.compaction),") {
		t.Error("runnerFor no longer wraps its model for compaction: sub-agent turns " +
			"would grow their context unbounded while the parent's is compacted")
	}
}
