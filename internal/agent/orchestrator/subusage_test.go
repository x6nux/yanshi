package orchestrator

import (
	"os"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
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
// The match deliberately stops before the window argument: Task 2 made it a
// parameter and Task 3 will start passing the turn's resolved value, so
// pinning the whole line would make this guard fail on the very change it
// should survive.
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
	if !strings.Contains(string(src), "Model:         wrapCompaction(chatModel, o.compaction, ") {
		t.Error("runnerFor no longer wraps its model for compaction: sub-agent turns " +
			"would grow their context unbounded while the parent's is compacted")
	}
}

// TestCompactionGatesUseTheResolvedWindow pins that all three gates are
// computed from the window the turn's provider actually has.
//
// bootstrap used to pre-multiply CooldownTokens against the global fallback
// window, which left the orchestrator with no way to learn the real one. A
// provider with a 128K window then got a threshold gate sized for 256K --
// 1.9x its actual capacity, so the gate never fires at all. That is not a
// rounding problem: compaction simply stops existing for that provider.
func TestCompactionGatesUseTheResolvedWindow(t *testing.T) {
	cc := CompactionConfig{
		Threshold:         0.8,
		ContextWindow:     256000, // global fallback
		KeepRecent:        4,
		CooldownFraction:  0.1,
		HardForceFraction: 0.9,
	}

	wrapped := wrapCompaction(einollm.NewFakeModel(nil, nil), cc, 128000, 0)
	cm, ok := wrapped.(*einollm.CompactingModel)
	require.True(t, ok, "compaction must be enabled")

	require.Equal(t, 128000, cm.ContextWindow,
		"the gates must size against the provider's window, not the global fallback")
	require.Equal(t, 12800, cm.CooldownTokens,
		"cooldown is a fraction of the resolved window, resolved here rather than pre-multiplied in bootstrap")
}

// TestRunnerForSizesGatesToTheTurnsModel pins that a turn's own model decides
// the window its compaction gates are sized against.
//
// windowFor is the whole point of Task 3: without it every provider shares the
// global fallback, and the one with the smallest real window gets a threshold
// it can never reach. The lookup is keyed by TurnOpts.ModelID, so an unknown
// or empty model must return 0 and let wrapCompaction fall back rather than
// silently sizing every gate to zero.
func TestRunnerForSizesGatesToTheTurnsModel(t *testing.T) {
	cc := CompactionConfig{
		Threshold:       0.8,
		ContextWindow:   256000,
		ProviderWindows: map[string]int{"small": 128000},
	}

	assert.Equal(t, 128000, cc.windowFor("small"), "a known model uses its own window")
	assert.Equal(t, 0, cc.windowFor("unknown"), "an unknown model defers to the fallback")
	assert.Equal(t, 0, cc.windowFor(""), "an unset ModelID defers to the fallback")

	// And the fallback really is the global one, not zero.
	wrapped := wrapCompaction(einollm.NewFakeModel(nil, nil), cc, cc.windowFor("unknown"), cc.thresholdFor("unknown"))
	cm, ok := wrapped.(*einollm.CompactingModel)
	require.True(t, ok)
	require.Equal(t, 256000, cm.ContextWindow,
		"an unknown model must fall back to the configured window, not to a zero that disables every gate")
}

// TestCompactionConfig_thresholdFor mirrors TestRunnerForSizesGatesToTheTurnsModel
// for the W-C-01 (INF2) threshold sibling of windowFor: a known model uses its
// own resolved threshold, and an unknown or empty model id defers to the
// caller (0 tells wrapCompaction to keep the global cc.Threshold).
func TestCompactionConfig_thresholdFor(t *testing.T) {
	cc := CompactionConfig{
		Threshold:          0.8,
		ProviderThresholds: map[string]float64{"small": 0.55},
	}

	assert.Equal(t, 0.55, cc.thresholdFor("small"), "a known model uses its own resolved threshold")
	assert.Equal(t, float64(0), cc.thresholdFor("unknown"), "an unknown model defers to the global threshold")
	assert.Equal(t, float64(0), cc.thresholdFor(""), "an unset ModelID defers to the global threshold")

	// A nil map must not panic (the zero-value CompactionConfig, e.g. compaction
	// disabled entirely, has no ProviderThresholds at all).
	var zero CompactionConfig
	assert.Equal(t, float64(0), zero.thresholdFor("small"))
}

// TestCompactionThresholdGatesUseTheResolvedThreshold pins the mid-turn half
// of W-C-01 (INF2) acceptance clause 4: a per-model auto-compact threshold —
// sourced from config override or the embedded catalog by bootstrap, reduced
// here to a plain map — must reach the CompactingModel that actually gates
// compaction, distinctly from the global fallback Threshold.
//
// Mirrors TestCompactionGatesUseTheResolvedWindow's shape: without this, a
// provider with a catalog-sourced 0.6 threshold would silently share the
// operator's global 0.8, exactly the "wrong number, still compiles" failure
// class the window test above already guards for ContextWindow.
func TestCompactionThresholdGatesUseTheResolvedThreshold(t *testing.T) {
	cc := CompactionConfig{
		Threshold:          0.8, // global fallback
		ContextWindow:      256000,
		KeepRecent:         4,
		ProviderThresholds: map[string]float64{"small": 0.6},
	}

	wrapped := wrapCompaction(einollm.NewFakeModel(nil, nil), cc, 128000, cc.thresholdFor("small"))
	cm, ok := wrapped.(*einollm.CompactingModel)
	require.True(t, ok, "compaction must be enabled")

	require.Equal(t, 0.6, cm.Threshold,
		"the gate must use the model's resolved threshold, not the global fallback")
	require.NotEqual(t, cc.Threshold, cm.Threshold,
		"this test is only meaningful if the resolved value actually differs from the global one")
}

// TestWrapCompaction_ZeroResolvedThresholdKeepsTheGlobalOne pins the other
// half of the same ladder: a model absent from ProviderThresholds (0 from
// thresholdFor) must NOT disable or zero out compaction — it must fall back
// to cc.Threshold exactly like an absent window falls back to
// cc.ContextWindow.
func TestWrapCompaction_ZeroResolvedThresholdKeepsTheGlobalOne(t *testing.T) {
	cc := CompactionConfig{Threshold: 0.8, ContextWindow: 128000, KeepRecent: 4}

	wrapped := wrapCompaction(einollm.NewFakeModel(nil, nil), cc, 0, cc.thresholdFor("unknown-model"))
	cm, ok := wrapped.(*einollm.CompactingModel)
	require.True(t, ok)
	assert.Equal(t, 0.8, cm.Threshold, "an unresolved per-model threshold must fall back to the global one")
}

// TestWrapCompaction_GlobalThresholdZeroStaysOffEvenWithACatalogHit pins the
// design decision documented on wrapCompaction: the enable/disable switch is
// keyed SOLELY on the global cc.Threshold. A catalog-resolved per-model
// threshold only resizes an ALREADY-enabled gate — it must never be able to
// silently re-enable compaction an operator (or a test constructing
// CompactionConfig directly, bypassing config.Load's applyDefaults) turned
// off with Threshold: 0.
func TestWrapCompaction_GlobalThresholdZeroStaysOffEvenWithACatalogHit(t *testing.T) {
	cc := CompactionConfig{Threshold: 0, ProviderThresholds: map[string]float64{"small": 0.6}}

	fm := einollm.NewFakeModel(nil, nil)
	wrapped := wrapCompaction(fm, cc, 128000, cc.thresholdFor("small"))
	assert.Same(t, model.BaseChatModel(fm), wrapped,
		"Threshold<=0 must return the model unwrapped regardless of a per-model catalog hit")
	_, stillWrapped := wrapped.(*einollm.CompactingModel)
	assert.False(t, stillWrapped, "a catalog hit must not silently re-enable compaction the operator turned off")
}

// TestRunnerForPassesTheModelIDThrough guards the wiring that
// TestRunnerForSizesGatesToTheTurnsModel does not reach.
//
// That test exercises windowFor directly, so it stays green even if runnerFor
// stops calling it -- measured while writing this: replacing the argument with
// a literal 0 reddened nothing. windowFor returning the right number is
// useless if the turn's model never reaches it, which is the same
// unit-versus-wiring split rounds 11 and 13 of this package's review found.
//
// Source-level for the reason given on TestRunnerForWiresCompactionToo: the
// wrapped model is buried inside adk's agent with no accessor.
func TestRunnerForPassesTheModelIDThrough(t *testing.T) {
	src, err := os.ReadFile("orchestrator.go")
	if err != nil {
		t.Fatalf("read orchestrator.go: %v", err)
	}
	body := string(src)
	if !strings.Contains(body, "o.compaction.windowFor(modelID)") {
		t.Error("runnerFor no longer sizes compaction from the turn's model: " +
			"every provider would share the global fallback window again")
	}
	if !strings.Contains(body, "o.runnerFor(selectedModel, opts.PlanMode, opts.ModelID)") {
		t.Error("the turn no longer hands its ModelID to runnerFor, so windowFor " +
			"always sees an empty string and always returns the fallback")
	}
}
