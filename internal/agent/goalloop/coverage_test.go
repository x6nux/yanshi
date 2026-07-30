package goalloop

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// ---------------------------------------------------------------------------
// implementer.go: firstNonEmpty with all empty
// ---------------------------------------------------------------------------

func TestFirstNonEmpty_AllEmpty(t *testing.T) {
	t.Parallel()
	got := firstNonEmpty("", "", "")
	assert.Equal(t, "", got, "all empty should return empty string")
}

// ---------------------------------------------------------------------------
// trigger.go: WithTierer(nil), OSConfirmer scan error, DecideTier nil tierer
// ---------------------------------------------------------------------------

func TestWithTierer_NilRestoresDefault(t *testing.T) {
	t.Parallel()
	tr := NewTrigger(RuleClassifier{}, AutoConfirmer{Result: true})
	tr.WithTierer(nil) // should set RuleTierer
	enter, tier, reason := tr.DecideTier(context.Background(), "/dev implement X")
	assert.True(t, enter)
	assert.Equal(t, TierStandard, tier)
	assert.Equal(t, "explicit", reason)
}

func TestDecideTier_NilTiererField(t *testing.T) {
	t.Parallel()
	// Trigger created without WithTierer, then set tier to nil explicitly
	tr := &Trigger{
		clf:  RuleClassifier{},
		conf: AutoConfirmer{Result: false},
		tier: nil, // nil tierer — should not panic
	}
	// DecideTier should handle nil tierer gracefully.
	enter, tier, reason := tr.DecideTier(context.Background(), "/dev implement X")
	assert.True(t, enter)
	assert.Equal(t, TierStandard, tier)
	assert.Equal(t, "explicit", reason)
}

// errorReader returns an error on every read.
type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errors.New("read error")
}

func TestOSConfirmer_ScanError(t *testing.T) {
	t.Parallel()
	c := OSConfirmer{in: errorReader{}}
	_, err := c.Confirm(context.Background(), "test?")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read error")
}

func TestOSConfirmer_ScanEOF(t *testing.T) {
	t.Parallel()
	c := OSConfirmer{in: strings.NewReader("")}
	ok, err := c.Confirm(context.Background(), "test?")
	require.NoError(t, err)
	assert.False(t, ok)
}

// ---------------------------------------------------------------------------
// tier.go: tierFlag negative tier
// ---------------------------------------------------------------------------

func TestTierFlag_Negative(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "t1", tierFlag(Tier(-1)))
}

// ---------------------------------------------------------------------------
// implementer.go: WorktreeHelper error paths
// ---------------------------------------------------------------------------

func TestWorktreeHelper_AddInNonRepoDir(t *testing.T) {
	t.Parallel()
	h := &WorktreeHelper{RepoDir: t.TempDir()} // not a git repo
	_, err := h.Add("test")
	require.Error(t, err)
}

func TestWorktreeHelper_ListInNonRepoDir(t *testing.T) {
	t.Parallel()
	h := &WorktreeHelper{RepoDir: t.TempDir()}
	_, err := h.List()
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// loop.go: Loop Run over budget detection
// ---------------------------------------------------------------------------

func TestLoop_OverBudget(t *testing.T) {
	t.Parallel()
	planner := FakePlanner{
		Tests: []AcceptanceTest{{Name: "t", Command: "true"}},
		Steps: []string{"s1"},
	}
	impl := &FakeImplementer{Result: "done", FailFirst: 0}
	eval := &CounterEvaluator{passAt: 1}
	sink := &UsageSink{}
	sink.Add(Usage{TotalTokens: 999_999_999}) // pre-load sink to exceed budget
	loop := New(Config{
		Planner:     planner,
		Implementer: impl,
		Evaluators:  []Evaluator{eval},
		Judge:       AggregateJudge{},
		Budget:      Budget{MaxTokens: 1000, MaxIterations: 10},
		Sink:        sink,
	})
	decision, err := loop.Run(context.Background(), Goal{Text: "test"}, nil)
	require.NoError(t, err)
	assert.False(t, decision.Complete)
	assert.Equal(t, StopReasonTokenBudget, decision.StopReason)
}

// ---------------------------------------------------------------------------
// evaluators.go: QualityEvaluator with model responses
// ---------------------------------------------------------------------------

func TestQualityEvaluator_EvaluateWithModel(t *testing.T) {
	t.Parallel()
	// Fake model that returns a passing verdict.
	m := einollm.NewFakeModel([]string{
		`{"pass":true,"gaps":[],"reason":"looks good"}`,
	}, nil)
	sink := &UsageSink{}
	ev := QualityEvaluator{Model: m, Sink: sink}
	v, err := ev.Evaluate(context.Background(), Goal{}, Plan{}, "")
	require.NoError(t, err)
	assert.True(t, v.Pass)
	assert.Equal(t, "looks good", v.Evidence)
	assert.Equal(t, "quality", v.Evaluator)
}

func TestQualityEvaluator_ModelError(t *testing.T) {
	t.Parallel()
	m := einollm.NewFakeModel(nil, errors.New("model boom"))
	ev := QualityEvaluator{Model: m}
	_, err := ev.Evaluate(context.Background(), Goal{}, Plan{}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "quality evaluator generate")
}

// ---------------------------------------------------------------------------
// loop.go: Run context cancellation
// ---------------------------------------------------------------------------

func TestLoop_ContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	loop := New(Config{
		Planner:     FakePlanner{Tests: []AcceptanceTest{{Name: "t", Command: "true"}}, Steps: []string{"s1"}},
		Implementer: &FakeImplementer{Result: "done"},
		Evaluators:  []Evaluator{&CounterEvaluator{passAt: 1}},
		Judge:       AggregateJudge{},
		Budget:      Budget{MaxIterations: 10},
	})
	_, err := loop.Run(ctx, Goal{Text: "test"}, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// ---------------------------------------------------------------------------
// trigger.go: Decide classifier error path
// ---------------------------------------------------------------------------

// errClassifier always returns an error.
type errClassifier struct{}

func (errClassifier) Classify(_ context.Context, _ string) (Intent, error) {
	return IntentOther, errors.New("classifier error")
}

func TestDecide_ClassifierError(t *testing.T) {
	t.Parallel()
	tr := NewTrigger(errClassifier{}, AutoConfirmer{Result: true})
	enter, reason := tr.Decide(context.Background(), "implement X")
	assert.False(t, enter)
	assert.Equal(t, "not a goal", reason)
}

// ---------------------------------------------------------------------------
// implementer.go: Add with bad temp dir path
// ---------------------------------------------------------------------------

func TestWorktreeHelper_AddTempDirFail(t *testing.T) {
	t.Parallel()
	// WorktreeHelper.Add creates a temp dir via os.MkdirTemp.
	// Use a path that cannot be removed to cause an error.
	h := &WorktreeHelper{RepoDir: "nonexistent"}
	_, err := h.Add("test")
	require.Error(t, err)
}
