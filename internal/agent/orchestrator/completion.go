package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// messageRecorder is an adk.ChatModelAgentMiddleware that captures the ADK
// state's message slice after every model invocation. The FINAL capture (after
// the last model call of the turn) is the complete ReAct conversation for the
// turn — the user query, every assistant message, every tool_call/tool_result
// pair, and the assistant's final reply. That slice is exactly what the model
// saw on its last call, so feeding it back to the SAME model with only a judge
// question appended hits the provider's KV cache for the whole prefix.
//
// Why record instead of re-reading the event stream: the WS turn loop only
// accumulates the assistant's TEXT (agent_chunk frames). tool_call and
// tool_result frames are forwarded to the client but never reconstructed into
// schema.Message history at the transport layer. The ADK state, by contrast,
// holds the authoritative message slice, so capturing it here is the only way
// to hand JudgeCompletion the full turn conversation.
//
// This replaces the old task_end completionHandler. The completion signal no
// longer comes from the model calling a no-op tool — instead every turn stops
// naturally and JudgeCompletion arbitrates whether the stop was premature.
//
// Embedding *adk.BaseChatModelAgentMiddleware gives default no-op
// implementations for the other six ChatModelAgentMiddleware methods.
type messageRecorder struct {
	*adk.BaseChatModelAgentMiddleware
}

// newMessageRecorder builds a messageRecorder ready for installation on an
// adk.ChatModelAgentConfig.Handlers slice.
func newMessageRecorder() *messageRecorder {
	r := &messageRecorder{}
	r.BaseChatModelAgentMiddleware = &adk.BaseChatModelAgentMiddleware{}
	return r
}

// recorderKey is the context key for the per-turn capture slot. An unexported
// struct type prevents collisions with other context values.
type recorderKey struct{}

// turnRecorder is the mutable capture slot bound into a turn's context. The
// recorder middleware writes the latest state.Messages here; JudgeCompletion
// reads it after the turn. Mutex-guarded for robustness even though a single
// turn's model calls are driven sequentially by the ADK.
type turnRecorder struct {
	mu       sync.Mutex
	messages []*schema.Message
}

func (r *turnRecorder) store(msgs []*schema.Message) {
	cp := make([]*schema.Message, len(msgs))
	copy(cp, msgs)
	r.mu.Lock()
	r.messages = cp
	r.mu.Unlock()
}

func (r *turnRecorder) load() []*schema.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.messages
}

// WithTurnRecorder binds a turnRecorder into ctx. The recorder middleware
// writes to it during the turn, and Orchestrator.JudgeCompletion reads it
// after. Returns ctx unchanged when rec is nil (nil makes the binding a
// no-op).
//
// This is the single binding implementation; WithNewTurnRecorder delegates
// here. Production callers use WithNewTurnRecorder (ws.go's turn loop);
// tests use this form when they need to inject a specific recorder.
func WithTurnRecorder(ctx context.Context, rec *turnRecorder) context.Context {
	if rec == nil {
		return ctx
	}
	return context.WithValue(ctx, recorderKey{}, rec)
}

// WithNewTurnRecorder creates a fresh per-turn recorder, binds it into ctx,
// and returns the new ctx. This is the convenience entry point for callers
// (ws.go's turn loop) that just need a recorder bound — the recorder type
// itself stays unexported. The recorder middleware populates it during the
// turn; JudgeCompletion(ctx) reads it after.
func WithNewTurnRecorder(ctx context.Context) context.Context {
	return WithTurnRecorder(ctx, &turnRecorder{})
}

// AfterModelRewriteState captures the ADK state's messages into the per-turn
// recorder bound in ctx. Called after every model invocation within the turn,
// so by turn end the recorder holds the final, complete message slice — which
// is what JudgeCompletion needs to judge completion against the full
// conversation.
func (r *messageRecorder) AfterModelRewriteState(
	ctx context.Context,
	state *adk.ChatModelAgentState,
	_ *adk.ModelContext,
) (context.Context, *adk.ChatModelAgentState, error) {
	if rec, ok := ctx.Value(recorderKey{}).(*turnRecorder); ok && rec != nil && state != nil {
		rec.store(state.Messages)
	}
	return ctx, state, nil
}

// judgePrompt is appended to the recorded turn conversation as the final user
// message when judging completion. It asks for a fixed JSON verdict so the
// orchestrator can parse it mechanically. The wording is deliberately
// permissive about conversational replies: a "hi" / "thanks" / yes-no answer
// with no tool use IS a complete turn, and the judge must say so rather than
// flag it unfinished — flagging such replies is exactly the failure that, under
// the old mandatory-task_end regime, made the model invent work (read memory,
// run tests) to justify ending a conversational turn.
const judgePrompt = "You are a completion judge. Decide whether the assistant fully addressed the user's latest request in the conversation above. Reply with ONLY this JSON and nothing else: {\"complete\": true|false, \"reason\": \"...\"}.\n- complete=true when the request is done. This INCLUDES brief or conversational replies (greetings, acknowledgements, yes/no answers, clarifications) that need no further action — a plain text answer is a complete response.\n- complete=false only when the request is genuinely unfinished, with a short reason describing what still remains.\nDo not demand tool use, tests, or extra work beyond what the user actually asked."

// judgeVerdict is the fixed JSON shape JudgeCompletion asks the model to return.
type judgeVerdict struct {
	Complete bool   `json:"complete"`
	Reason   string `json:"reason"`
}

// JudgeCompletion asks the main model to judge whether the turn just run on ctx
// is complete. It uses the message slice captured by the recorder middleware —
// the exact conversation the model saw on its last call — plus the judge
// question appended as the final user message, so the provider's KV cache
// covers the whole prefix (only the judge question is new tokens).
//
// Failure mode is deliberately permissive: any generate error, missing
// recorder, empty capture, or JSON parse failure returns complete=true.
// JudgeCompletion exists to retry genuinely-incomplete turns; it must never
// wedge the user when the judge itself misbehaves (transient API error, a
// non-JSON reply from a fake or odd model). Erring toward "complete" lets the
// turn end normally; the worst case is a retryable turn isn't retried — far
// better than an infinite retry loop.
func (o *Orchestrator) JudgeCompletion(ctx context.Context) (complete bool, reason string, usage TurnUsage) {
	rec, ok := ctx.Value(recorderKey{}).(*turnRecorder)
	if !ok || rec == nil {
		return true, "", usage
	}
	msgs := rec.load()
	if len(msgs) == 0 {
		return true, "", usage
	}
	judgeMessages := make([]*schema.Message, 0, len(msgs)+1)
	judgeMessages = append(judgeMessages, msgs...)
	judgeMessages = append(judgeMessages, schema.UserMessage(judgePrompt))
	resp, gerr := o.model.Generate(ctx, judgeMessages)
	if gerr != nil || resp == nil {
		return true, "", usage
	}
	// Capture the judge call's token usage so the transport can account for
	// the cost of judging — the verdict is real extra completion spend on top
	// of the turn, and (for cached providers) the replayed prefix hits cache.
	// set is a no-op when ResponseMeta/Usage is nil (FakeModel's judge probe,
	// or providers that omit usage on non-streaming Generate).
	if resp.ResponseMeta != nil {
		usage.set(resp.ResponseMeta.Usage)
	}
	v, perr := parseJudgeVerdict(resp.Content)
	if perr != nil {
		return true, "", usage
	}
	return v.Complete, v.Reason, usage
}

// parseJudgeVerdict extracts the {complete, reason} JSON from the model's reply.
// Models occasionally wrap JSON in prose or markdown fences, so the parser
// first tries the whole content, then falls back to the outermost {...} span.
func parseJudgeVerdict(content string) (judgeVerdict, error) {
	content = strings.TrimSpace(content)
	var v judgeVerdict
	if err := json.Unmarshal([]byte(content), &v); err == nil {
		return v, nil
	}
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start >= 0 && end > start {
		var v2 judgeVerdict
		if err := json.Unmarshal([]byte(content[start:end+1]), &v2); err == nil {
			return v2, nil
		}
	}
	return judgeVerdict{}, fmt.Errorf("judge: no JSON verdict in reply")
}

// JudgeRetryNudge turns the stop judge's "what remains" reason into the user
// message a transport injects on a judge-driven retry. An empty reason falls
// back to a generic nudge so the model still gets a signal to continue.
func JudgeRetryNudge(reason string) string {
	r := strings.TrimSpace(reason)
	if r == "" {
		return "Continue and finish addressing the user's request."
	}
	return "The completion judge flagged this turn as possibly unfinished: " + r + ". Address it and finish your reply."
}
