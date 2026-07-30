package orchestrator

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// TestParseJudgeVerdict covers the JSON extraction: plain verdicts, the two
// fallback shapes (markdown-fenced, embedded in prose), and the failure cases
// (empty / non-JSON). The false-with-reason case is the load-bearing one — it
// is what makes a retry carry a useful "what remains" nudge.
func TestParseJudgeVerdict(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		complete bool
		reason   string
		ok       bool
	}{
		{"plain true", `{"complete": true, "reason": ""}`, true, "", true},
		{"plain false with reason", `{"complete": false, "reason": "still need to run tests"}`, false, "still need to run tests", true},
		{"markdown fenced", "```json\n{\"complete\": true}\n```", true, "", true},
		{"json embedded in prose", `Sure! {"complete": true} done.`, true, "", true},
		{"empty", "", false, "", false},
		{"non-json text", "the task is complete", false, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, err := parseJudgeVerdict(c.content)
			if !c.ok {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, c.complete, v.Complete)
			assert.Equal(t, c.reason, v.Reason)
		})
	}
}

// TestJudgeCompletion_NoRecorder: a ctx with no recorder bound must fall back
// to complete=true so a caller that forgot to bind one never gets wedged.
func TestJudgeCompletion_NoRecorder(t *testing.T) {
	o, err := New(Config{Model: einollm.NewFakeModel(nil, nil)})
	require.NoError(t, err)
	complete, reason, _ := o.JudgeCompletion(context.Background())
	assert.True(t, complete, "no recorder bound → accept as complete")
	assert.Empty(t, reason)
}

// TestJudgeCompletion_EmptyRecorder: a bound recorder with no captured messages
// (e.g. the model never ran) must also fall back to complete=true.
func TestJudgeCompletion_EmptyRecorder(t *testing.T) {
	o, err := New(Config{Model: einollm.NewFakeModel(nil, nil)})
	require.NoError(t, err)
	ctx := WithNewTurnRecorder(context.Background())
	complete, reason, _ := o.JudgeCompletion(ctx)
	assert.True(t, complete, "empty recorder → accept as complete")
	assert.Empty(t, reason)
}

// TestJudgeCompletion_FakeJudgeSaysComplete: with a real captured
// conversation, JudgeCompletion replays it + the judge question to the model.
// The fake's isJudgeProbe short-circuit answers with {"complete":true} WITHOUT
// consuming a scripted response, so this also proves the judge path is taken.
func TestJudgeCompletion_FakeJudgeSaysComplete(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"the answer"}, nil)
	o, err := New(Config{Model: fm})
	require.NoError(t, err)

	rec := &turnRecorder{}
	rec.store([]*schema.Message{
		schema.UserMessage("hi"),
		schema.AssistantMessage("hello there", nil),
	})
	ctx := WithTurnRecorder(context.Background(), rec)

	complete, reason, _ := o.JudgeCompletion(ctx)
	assert.True(t, complete)
	assert.Empty(t, reason)
}

// TestJudgeRetryNudge: an empty/whitespace reason falls back to the generic
// nudge; a real reason is surfaced verbatim and attributed to the judge.
func TestJudgeRetryNudge(t *testing.T) {
	assert.Contains(t, JudgeRetryNudge(""), "Continue and finish",
		"empty reason → generic nudge")
	assert.Contains(t, JudgeRetryNudge("   "), "Continue and finish",
		"whitespace-only reason → generic nudge")
	nudge := JudgeRetryNudge("run the test suite")
	assert.Contains(t, nudge, "run the test suite", "reason must appear in nudge")
	assert.Contains(t, nudge, "completion judge", "nudge must attribute the judge")
}
