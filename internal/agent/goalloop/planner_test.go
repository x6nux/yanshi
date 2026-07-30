package goalloop

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

func TestFakePlanner_Scripted(t *testing.T) {
	tests := []AcceptanceTest{
		{Name: "unit", Command: "go test ./..."},
	}
	steps := []string{"write code", "run tests"}

	p := FakePlanner{Tests: tests, Steps: steps}

	plan, err := p.Plan(context.Background(), Goal{Text: "implement foo"})
	require.NoError(t, err)
	assert.Equal(t, "implement foo", plan.Goal)
	assert.Len(t, plan.Tests, 1)
	assert.Equal(t, "unit", plan.Tests[0].Name)
	assert.Equal(t, "go test ./...", plan.Tests[0].Command)
	assert.Equal(t, steps, plan.Steps)
}

func TestFakePlanner_Error(t *testing.T) {
	p := FakePlanner{Err: errors.New("nope")}
	_, err := p.Plan(context.Background(), Goal{Text: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nope")
}

func TestLLMPlanner_ParseJSON(t *testing.T) {
	canned := schema.AssistantMessage(
		`{"goal":"x","tests":[{"name":"t","command":"go test ./..."}],"steps":["s1"]}`,
		nil,
	)
	m := einollm.NewFakeModelWithMessages([]*schema.Message{canned}, nil)

	pl := LLMPlanner{Model: m}
	plan, err := pl.Plan(context.Background(), Goal{Text: "implement foo"})
	require.NoError(t, err)
	assert.Equal(t, "x", plan.Goal)
	assert.Len(t, plan.Tests, 1)
	assert.Equal(t, "t", plan.Tests[0].Name)
	assert.Equal(t, "go test ./...", plan.Tests[0].Command)
	assert.Equal(t, []string{"s1"}, plan.Steps)
}

func TestLLMPlanner_CodeFence(t *testing.T) {
	canned := schema.AssistantMessage(
		"```json\n"+`{"goal":"y","tests":[],"steps":["a","b"]}`+"\n```",
		nil,
	)
	m := einollm.NewFakeModelWithMessages([]*schema.Message{canned}, nil)

	pl := LLMPlanner{Model: m}
	plan, err := pl.Plan(context.Background(), Goal{Text: "do thing"})
	require.NoError(t, err)
	assert.Equal(t, "y", plan.Goal)
	assert.Empty(t, plan.Tests)
	assert.Equal(t, []string{"a", "b"}, plan.Steps)
}

func TestLLMPlanner_GenerateError(t *testing.T) {
	m := einollm.NewFakeModel(nil, errors.New("model down"))
	pl := LLMPlanner{Model: m}
	_, err := pl.Plan(context.Background(), Goal{Text: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "model down")
}

func TestLLMPlanner_BadJSON(t *testing.T) {
	canned := schema.AssistantMessage("not json at all", nil)
	m := einollm.NewFakeModelWithMessages([]*schema.Message{canned}, nil)

	pl := LLMPlanner{Model: m}
	_, err := pl.Plan(context.Background(), Goal{Text: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
}

func TestLLMPlanner_RejectsEmptyPlan(t *testing.T) {
	canned := schema.AssistantMessage(`{"goal":"x"}`, nil)
	m := einollm.NewFakeModelWithMessages([]*schema.Message{canned}, nil)

	pl := LLMPlanner{Model: m}
	_, err := pl.Plan(context.Background(), Goal{Text: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no steps and no tests")
}

func TestLLMPlanner_RecordsUsage(t *testing.T) {
	t.Parallel()
	// A canned plan reply that ALSO carries token usage on ResponseMeta.
	canned := schema.AssistantMessage(
		`{"goal":"x","tests":[{"name":"t","command":"go test ./..."}],"steps":["s1"]}`,
		nil,
	)
	canned.ResponseMeta = &schema.ResponseMeta{
		Usage: &schema.TokenUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
	}
	m := einollm.NewFakeModelWithMessages([]*schema.Message{canned}, nil)

	sink := &UsageSink{}
	pl := LLMPlanner{Model: m, Sink: sink}
	_, err := pl.Plan(context.Background(), Goal{Text: "implement foo"})
	require.NoError(t, err)

	got := sink.Snapshot()
	assert.Equal(t, 120, got.TotalTokens, "planner must add its Generate usage to the sink")
	assert.Equal(t, 100, got.PromptTokens)
}

func TestLLMPlanner_NilSinkNoPanic(t *testing.T) {
	t.Parallel()
	canned := schema.AssistantMessage(
		`{"goal":"x","tests":[],"steps":["s1"]}`, nil,
	)
	m := einollm.NewFakeModelWithMessages([]*schema.Message{canned}, nil)
	pl := LLMPlanner{Model: m} // Sink nil
	_, err := pl.Plan(context.Background(), Goal{Text: "x"})
	require.NoError(t, err, "nil Sink must be tolerated")
}
