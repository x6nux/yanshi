package orchestrator

import (
	"context"

	"github.com/cloudwego/eino/schema"
)

// RecordedTurnMessages returns the conversation the messageRecorder captured
// for the turn running on ctx: the user query, every assistant message, and
// every tool_call/tool_result pair the ReAct loop actually produced. It is the
// same capture JudgeCompletion judges against, exposed so a transport can
// CONTINUE a prematurely stopped turn instead of re-running it.
//
// Why this exists at all: a transport that decides a turn stopped early has
// only two ways to give the model another push. It can re-send the turn's
// starting history — which is a full replay, so every side-effecting tool the
// model already called (shell_run, fs_write) runs a second time — or it can
// append to what already happened. Only the second is safe, and only this
// capture holds "what already happened": the ADK state is the sole place the
// tool_call/tool_result pairs exist as messages, because the transports see
// them as frames and never reconstruct them into history.
//
// The leading system message is stripped. The captured slice is a MODEL INPUT
// (genModelInput prepended the agent instruction before the first call), while
// the value returned here is fed back in as an agent input, where the
// instruction is prepended again. Leaving it in would put two system messages
// in front of the next attempt.
//
// Returns nil when no recorder is bound or nothing was captured (a turn that
// failed before its first model call). Callers must treat nil as "no
// continuation available" and fall back rather than continuing from nothing.
// The result is a fresh slice, so appending to it cannot reach the recorder's
// own storage.
func RecordedTurnMessages(ctx context.Context) []*schema.Message {
	rec, ok := ctx.Value(recorderKey{}).(*turnRecorder)
	if !ok || rec == nil {
		return nil
	}
	msgs := rec.load()
	out := make([]*schema.Message, 0, len(msgs))
	for _, m := range msgs {
		if m == nil {
			continue
		}
		if len(out) == 0 && m.Role == schema.System {
			continue
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
