package goalloop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// ---------------------------------------------------------------------------
// TestEvaluator
// ---------------------------------------------------------------------------

func TestTestEvaluator_AllPass(t *testing.T) {
	t.Parallel()
	ev := TestEvaluator{}
	plan := Plan{
		Tests: []AcceptanceTest{
			{Name: "go-version", Command: "echo go-version"},
		},
	}
	v, err := ev.Evaluate(context.Background(), Goal{}, plan, "")
	require.NoError(t, err)
	assert.True(t, v.Pass)
	assert.Equal(t, "test", v.Evaluator)
	assert.Empty(t, v.Gaps)
	assert.Contains(t, v.Evidence, "go-version")
}

func TestTestEvaluator_OneFails(t *testing.T) {
	t.Parallel()
	ev := TestEvaluator{}
	plan := Plan{
		Tests: []AcceptanceTest{
			{Name: "good", Command: "echo good"},
			{Name: "bad", Command: "go vet ./nonexistent"},
		},
	}
	v, err := ev.Evaluate(context.Background(), Goal{}, plan, "")
	require.NoError(t, err)
	assert.False(t, v.Pass)
	assert.Len(t, v.Gaps, 1)
	assert.Equal(t, "bad", v.Gaps[0])
}

func TestTestEvaluator_NoTests(t *testing.T) {
	t.Parallel()
	ev := TestEvaluator{}
	v, err := ev.Evaluate(context.Background(), Goal{}, Plan{}, "")
	require.NoError(t, err)
	assert.False(t, v.Pass)
	assert.Equal(t, "test", v.Evaluator)
	assert.Contains(t, v.Gaps[0], "no acceptance tests configured")
}

// TestTestEvaluator_GuardDeniesDangerousCommand verifies that the TestEvaluator
// with the default profile marks a dangerous command as failed without
// executing it, while allowing safe commands like "go test ./...".
func TestTestEvaluator_GuardDeniesDangerousCommand(t *testing.T) {
	t.Parallel()

	ev := TestEvaluator{}

	// Create a sentinel file; the dangerous command would delete it if
	// executed. We verify the file still exists after Evaluate to prove
	// the command was NOT run.
	sentinelDir := t.TempDir()
	sentinel := filepath.Join(sentinelDir, "sentinel")
	require.NoError(t, os.WriteFile(sentinel, []byte("alive"), 0o644))

	// Use an isolated workdir so "go test ./..." doesn't recursively
	// re-run this test suite (which would hang). An empty temp dir has
	// no Go files, so "go test ./..." exits quickly with an error —
	// the test only asserts the dangerous command was denied, not that
	// the safe one passed.
	workdir := t.TempDir()

	plan := Plan{
		Tests: []AcceptanceTest{
			{Name: "dangerous", Command: "rm -rf " + sentinelDir},
			{Name: "safe", Command: "go test ./..."},
		},
	}

	v, err := ev.Evaluate(context.Background(), Goal{}, plan, workdir)
	require.NoError(t, err)

	// The dangerous test must be marked as failed.
	assert.False(t, v.Pass)
	assert.Contains(t, v.Gaps, "dangerous")

	// The evidence must mention the guard denial.
	assert.Contains(t, v.Evidence, "not allowed by guard")

	// The sentinel file must still exist (command was NOT executed).
	data, err := os.ReadFile(sentinel)
	require.NoError(t, err, "sentinel file should still exist — rm command must not have run")
	assert.Equal(t, "alive", string(data))
}

// TestTestEvaluator_WithProfile verifies that a custom profile is respected.
func TestTestEvaluator_WithProfile(t *testing.T) {
	t.Parallel()

	// Custom profile that denies all shell commands.
	ev := TestEvaluator{}
	ev.WithProfile(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		Shell: guard.ShellPerm{Policy: "deny"},
	})

	plan := Plan{
		Tests: []AcceptanceTest{
			{Name: "blocked", Command: "echo hello"},
		},
	}

	v, err := ev.Evaluate(context.Background(), Goal{}, plan, "")
	require.NoError(t, err)
	assert.False(t, v.Pass)
	assert.Contains(t, v.Gaps, "blocked")
	assert.Contains(t, v.Evidence, "not allowed by guard")
}

// ---------------------------------------------------------------------------
// FakeEvaluator
// ---------------------------------------------------------------------------

func TestFakeEvaluator(t *testing.T) {
	t.Parallel()
	want := EvalVerdict{Evaluator: "intent", Pass: true, Evidence: "looks good"}
	ev := FakeEvaluator{Verdict: want}
	got, err := ev.Evaluate(context.Background(), Goal{}, Plan{}, "")
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestFakeEvaluator_Error(t *testing.T) {
	t.Parallel()
	ev := FakeEvaluator{Err: errors.New("boom")}
	_, err := ev.Evaluate(context.Background(), Goal{}, Plan{}, "")
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// IntentEvaluator (parsing only — uses FakeModel)
// ---------------------------------------------------------------------------

func TestIntentEvaluator_ParsePass(t *testing.T) {
	t.Parallel()
	canned := schema.AssistantMessage(
		`{"pass":true,"gaps":[],"reason":"implementation matches the goal"}`,
		nil,
	)
	m := einollm.NewFakeModelWithMessages([]*schema.Message{canned}, nil)
	ev := IntentEvaluator{Model: m}
	v, err := ev.Evaluate(context.Background(), Goal{Text: "implement foo"}, Plan{}, "/tmp")
	require.NoError(t, err)
	assert.True(t, v.Pass)
	assert.Equal(t, "intent", v.Evaluator)
	assert.Empty(t, v.Gaps)
	assert.Contains(t, v.Evidence, "matches")
}

func TestIntentEvaluator_ParseFail(t *testing.T) {
	t.Parallel()
	canned := schema.AssistantMessage(
		`{"pass":false,"gaps":["missing error handling","no tests"],"reason":"incomplete"}`,
		nil,
	)
	m := einollm.NewFakeModelWithMessages([]*schema.Message{canned}, nil)
	ev := IntentEvaluator{Model: m}
	v, err := ev.Evaluate(context.Background(), Goal{Text: "implement foo"}, Plan{}, "/tmp")
	require.NoError(t, err)
	assert.False(t, v.Pass)
	assert.Len(t, v.Gaps, 2)
	assert.Contains(t, v.Evidence, "incomplete")
}

func TestIntentEvaluator_GenerateError(t *testing.T) {
	t.Parallel()
	m := einollm.NewFakeModel(nil, errors.New("model down"))
	ev := IntentEvaluator{Model: m}
	_, err := ev.Evaluate(context.Background(), Goal{Text: "x"}, Plan{}, "/tmp")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "model down")
}

func TestIntentEvaluator_BadJSON(t *testing.T) {
	t.Parallel()
	canned := schema.AssistantMessage("not json", nil)
	m := einollm.NewFakeModelWithMessages([]*schema.Message{canned}, nil)
	ev := IntentEvaluator{Model: m}
	_, err := ev.Evaluate(context.Background(), Goal{Text: "x"}, Plan{}, "/tmp")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
}

// ---------------------------------------------------------------------------
// QualityEvaluator (parsing only — uses FakeModel)
// ---------------------------------------------------------------------------

func TestQualityEvaluator_ParsePass(t *testing.T) {
	t.Parallel()
	canned := schema.AssistantMessage(
		`{"pass":true,"gaps":[],"reason":"code quality is acceptable"}`,
		nil,
	)
	m := einollm.NewFakeModelWithMessages([]*schema.Message{canned}, nil)
	ev := QualityEvaluator{Model: m}
	v, err := ev.Evaluate(context.Background(), Goal{Text: "implement foo"}, Plan{}, "/tmp")
	require.NoError(t, err)
	assert.True(t, v.Pass)
	assert.Equal(t, "quality", v.Evaluator)
}

func TestQualityEvaluator_ParseFail(t *testing.T) {
	t.Parallel()
	canned := schema.AssistantMessage(
		`{"pass":false,"gaps":["poor naming"],"reason":"needs cleanup"}`,
		nil,
	)
	m := einollm.NewFakeModelWithMessages([]*schema.Message{canned}, nil)
	ev := QualityEvaluator{Model: m}
	v, err := ev.Evaluate(context.Background(), Goal{Text: "implement foo"}, Plan{}, "/tmp")
	require.NoError(t, err)
	assert.False(t, v.Pass)
	assert.Len(t, v.Gaps, 1)
	assert.Equal(t, "poor naming", v.Gaps[0])
}

// ---------------------------------------------------------------------------
// AggregateJudge
// ---------------------------------------------------------------------------

func TestAggregateJudge_AllPass(t *testing.T) {
	t.Parallel()
	j := AggregateJudge{}
	verdicts := []EvalVerdict{
		{Evaluator: "test", Pass: true},
		{Evaluator: "intent", Pass: true},
		{Evaluator: "quality", Pass: true},
	}
	d, err := j.Judge(context.Background(), verdicts)
	require.NoError(t, err)
	assert.True(t, d.Complete)
	assert.Empty(t, d.Gaps)
	assert.Contains(t, d.Summary, "3")
	assert.Contains(t, d.Summary, "passed")
}

func TestAggregateJudge_OneFails(t *testing.T) {
	t.Parallel()
	j := AggregateJudge{}
	verdicts := []EvalVerdict{
		{Evaluator: "test", Pass: true},
		{Evaluator: "intent", Pass: false, Gaps: []string{"missing error handling"}},
		{Evaluator: "quality", Pass: true},
	}
	d, err := j.Judge(context.Background(), verdicts)
	require.NoError(t, err)
	assert.False(t, d.Complete)
	assert.Len(t, d.Gaps, 1)
	assert.Equal(t, "missing error handling", d.Gaps[0])
	assert.Contains(t, d.Summary, "2/3")
}

func TestAggregateJudge_MultipleFail(t *testing.T) {
	t.Parallel()
	j := AggregateJudge{}
	verdicts := []EvalVerdict{
		{Evaluator: "test", Pass: false, Gaps: []string{"test1", "test2"}},
		{Evaluator: "intent", Pass: false, Gaps: []string{"intent1"}},
	}
	d, err := j.Judge(context.Background(), verdicts)
	require.NoError(t, err)
	assert.False(t, d.Complete)
	assert.Len(t, d.Gaps, 3)
}

func TestAggregateJudge_Empty(t *testing.T) {
	t.Parallel()
	j := AggregateJudge{}
	d, err := j.Judge(context.Background(), nil)
	require.NoError(t, err)
	assert.False(t, d.Complete, "zero evaluators should not be complete")
	assert.Empty(t, d.Gaps)
	assert.Contains(t, d.Summary, "no evaluators")
}

func TestAggregateJudge_NoEvaluators(t *testing.T) {
	t.Parallel()
	j := AggregateJudge{}
	d, err := j.Judge(context.Background(), []EvalVerdict{})
	require.NoError(t, err)
	assert.False(t, d.Complete, "zero evaluators should not declare complete")
	assert.Contains(t, d.Summary, "no evaluators configured")
}

// ---------------------------------------------------------------------------
// Intent / Quality evaluator usage accounting (G02)
// ---------------------------------------------------------------------------

// verdictMsg builds an assistant message carrying a parseable verdict JSON and
// the given token usage, for evaluator usage-accounting tests.
func verdictMsg(usage *schema.TokenUsage) *schema.Message {
	msg := schema.AssistantMessage(`{"pass":true,"gaps":[],"reason":"ok"}`, nil)
	msg.ResponseMeta = &schema.ResponseMeta{Usage: usage}
	return msg
}

func TestIntentEvaluator_RecordsUsage(t *testing.T) {
	t.Parallel()
	m := einollm.NewFakeModelWithMessages([]*schema.Message{
		verdictMsg(&schema.TokenUsage{PromptTokens: 50, CompletionTokens: 5, TotalTokens: 55}),
	}, nil)
	sink := &UsageSink{}
	ev := IntentEvaluator{Model: m, Sink: sink}
	_, err := ev.Evaluate(context.Background(), Goal{Text: "g"}, Plan{Steps: []string{"s"}}, "")
	require.NoError(t, err)
	assert.Equal(t, 55, sink.Snapshot().TotalTokens, "intent evaluator must count its Generate call")
}

func TestQualityEvaluator_RecordsUsage(t *testing.T) {
	t.Parallel()
	m := einollm.NewFakeModelWithMessages([]*schema.Message{
		verdictMsg(&schema.TokenUsage{PromptTokens: 60, CompletionTokens: 6, TotalTokens: 66}),
	}, nil)
	sink := &UsageSink{}
	ev := QualityEvaluator{Model: m, Sink: sink}
	_, err := ev.Evaluate(context.Background(), Goal{Text: "g"}, Plan{Steps: []string{"s"}}, "")
	require.NoError(t, err)
	assert.Equal(t, 66, sink.Snapshot().TotalTokens, "quality evaluator must count its Generate call")
}

func TestEvaluators_SharedSinkSumsAcrossBoth(t *testing.T) {
	t.Parallel()
	// Two evaluators sharing ONE sink must not double-count: each contributes
	// its own single Generate call, summed once.
	shared := &UsageSink{}
	intentM := einollm.NewFakeModelWithMessages([]*schema.Message{
		verdictMsg(&schema.TokenUsage{TotalTokens: 10}),
	}, nil)
	qualityM := einollm.NewFakeModelWithMessages([]*schema.Message{
		verdictMsg(&schema.TokenUsage{TotalTokens: 20}),
	}, nil)
	ie := IntentEvaluator{Model: intentM, Sink: shared}
	qe := QualityEvaluator{Model: qualityM, Sink: shared}
	_, _ = ie.Evaluate(context.Background(), Goal{Text: "g"}, Plan{Steps: []string{"s"}}, "")
	_, _ = qe.Evaluate(context.Background(), Goal{Text: "g"}, Plan{Steps: []string{"s"}}, "")
	assert.Equal(t, 30, shared.Snapshot().TotalTokens, "shared sink sums both evaluators exactly once")
}
