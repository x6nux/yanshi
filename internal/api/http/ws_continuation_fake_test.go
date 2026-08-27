package http

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/guard"
)

// toolTestTimeout is the per-call budget for tools built in these tests. Any
// positive value works (NewGuardedTool panics on zero); it is a named constant
// so a reader does not wonder whether the number means anything.
const toolTestTimeout = 30 * time.Second

// emptyParams builds the schema for a tool that takes no arguments.
func emptyParams() *schema.ParamsOneOf {
	return schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{})
}

// judgeScriptedModel is a fake chat model that scripts the TURN responses and
// the COMPLETION-JUDGE verdicts separately.
//
// einollm.FakeModel cannot be used for the continuation tests for two reasons,
// and the second one is subtle enough to be worth stating plainly because a
// mutation probe is what surfaced it:
//
//  1. It recognises a judge probe and hardcodes `{"complete":true}`, so a turn
//     can never be judged premature and the continuation path is unreachable.
//     Driving the judge is the entire premise here.
//
//  2. It serves its script BY INDEX, so a replayed turn receives the next
//     scripted message rather than re-deciding from what it was shown. That
//     makes a replay indistinguishable from a continuation at the tool level:
//     with the continuation logic disabled, an index-served fake still does not
//     re-call the tool, so the end-to-end "no double execution" assertion
//     passes against a broken implementation. A real model decides from its
//     input, so this fake does too — see decide.
type judgeScriptedModel struct {
	mu sync.Mutex
	// toolCalls are the tool-calling responses. They are NOT served by index:
	// pendingToolCall picks whichever one names a tool that has not reported a
	// result in the input yet, so the fake's behaviour is a function of what it
	// is shown.
	toolCalls []*schema.Message
	// texts are the text responses, served in order once every scripted tool
	// has reported. Exhausted texts yield an empty assistant message, which
	// ends the ReAct loop.
	texts []*schema.Message
	// judges are the raw judge verdicts, served in order. Exhausted verdicts
	// yield complete=true so a test cannot hang in a retry loop.
	judges []string

	textIdx  int
	judgeIdx int
	// turnN counts non-judge calls; lastTurn holds the most recent non-judge
	// input. Judge probes are excluded from both because they are not attempts.
	turnN    int
	lastTurn []*schema.Message
}

// newJudgeScriptedModel builds a fake with separate turn and judge scripts.
// The turn script is split on arrival: messages carrying tool calls become the
// input-driven set, plain text becomes the ordered set. See pendingToolCall.
func newJudgeScriptedModel(turns []*schema.Message, judges []string) *judgeScriptedModel {
	m := &judgeScriptedModel{judges: judges}
	for _, msg := range turns {
		if msg != nil && len(msg.ToolCalls) > 0 {
			m.toolCalls = append(m.toolCalls, msg)
			continue
		}
		m.texts = append(m.texts, msg)
	}
	return m
}

// turnCalls returns how many non-judge model calls have been made. A turn that
// calls one tool costs two (the call, then the reply after the tool result), so
// tests assert a lower bound rather than an exact count.
func (m *judgeScriptedModel) turnCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.turnN
}

// lastTurnInput returns the messages passed to the most recent non-judge call.
func (m *judgeScriptedModel) lastTurnInput() []*schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastTurn
}

// isJudgeProbe reports whether msgs ends with the orchestrator's completion
// question.
//
// It matches the FULL opening sentence of orchestrator.judgePrompt, not the
// bare phrase "completion judge". The shorter marker (which einollm.FakeModel
// uses) collides here: orchestrator.JudgeRetryNudge phrases the retry reminder
// as "The completion judge flagged this turn…", so a continuation nudge — the
// very thing these tests exist to drive — was classified as a judge probe, and
// the continued attempt's response came out of the verdict queue. The symptom
// was a raw `{"complete":true}` rendered to the user as assistant text.
func isJudgeProbe(msgs []*schema.Message) bool {
	if len(msgs) == 0 {
		return false
	}
	last := msgs[len(msgs)-1]
	return last != nil && last.Role == schema.User &&
		strings.HasPrefix(last.Content, "You are a completion judge.")
}

// next serves the appropriate scripted response for msgs.
func (m *judgeScriptedModel) next(msgs []*schema.Message) *schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	if isJudgeProbe(msgs) {
		verdict := `{"complete":true,"reason":""}`
		if m.judgeIdx < len(m.judges) {
			verdict = m.judges[m.judgeIdx]
			m.judgeIdx++
		}
		return schema.AssistantMessage(verdict, nil)
	}
	m.turnN++
	m.lastTurn = msgs
	if call := m.pendingToolCall(msgs); call != nil {
		return call
	}
	if m.textIdx < len(m.texts) {
		msg := m.texts[m.textIdx]
		m.textIdx++
		return msg
	}
	return schema.AssistantMessage("", nil)
}

// pendingToolCall returns the scripted tool call whose tool has NOT yet
// reported a result in msgs, or nil when every scripted tool has run.
//
// This is what makes the fake input-driven rather than positional, and it is
// the whole reason the end-to-end continuation test can fail. A positional fake
// serves script[i] on the i-th call regardless of what it was shown, so a
// REPLAYED turn — which rewinds past the tool result and would make a real
// model reissue the call — instead receives the script's next (text) entry and
// quietly does not re-call. The tool-execution counter then reads 1 whether or
// not the continuation logic exists, and the test asserts nothing.
//
// Deciding from the input reproduces the real behaviour on both sides: shown
// its own tool result, the model moves on; shown a history with the result
// rewound away, it calls again. Note the script is NOT consumed here — the same
// call is reissued for as long as its result is absent, exactly as a real model
// would keep trying.
func (m *judgeScriptedModel) pendingToolCall(msgs []*schema.Message) *schema.Message {
	reported := map[string]bool{}
	for _, msg := range msgs {
		if msg != nil && msg.Role == schema.Tool && msg.ToolName != "" {
			reported[msg.ToolName] = true
		}
	}
	for _, call := range m.toolCalls {
		if call == nil || len(call.ToolCalls) == 0 {
			continue
		}
		for _, tc := range call.ToolCalls {
			if !reported[tc.Function.Name] {
				return call
			}
		}
	}
	return nil
}

// Generate implements model.BaseChatModel.
func (m *judgeScriptedModel) Generate(_ context.Context, msgs []*schema.Message,
	_ ...model.Option) (*schema.Message, error) {
	return m.next(msgs), nil
}

// Stream implements model.BaseChatModel, yielding the scripted message once.
func (m *judgeScriptedModel) Stream(_ context.Context, msgs []*schema.Message,
	_ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{m.next(msgs)}), nil
}

var _ model.BaseChatModel = (*judgeScriptedModel)(nil)
var _ = io.EOF // keep the io import meaningful if the stream form changes

// newWSServerWithTools starts a WS server whose orchestrator runs mdl with the
// given tools and a profile that ALLOWS them outright, so no permission prompt
// interferes with what these tests are measuring.
func newWSServerWithTools(t *testing.T, mdl model.BaseChatModel, ts []orchestrator.BaseTool) string {
	t.Helper()
	o, err := orchestrator.New(orchestrator.Config{
		Model:   mdl,
		Tools:   ts,
		Profile: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}},
	})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/chat/ws"
}
