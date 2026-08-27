// internal/api/http/ws_continuation.go
//
// Premature-stop continuation for the WS turn loop (L5).
//
// The stop judge can decide a turn ended before it finished the user's
// request. The question this file answers is what to hand the model for the
// next attempt.
//
// The answer used to be "the turn's starting history plus a nudge", which is a
// REPLAY: the model re-derives the whole turn from scratch, so every
// side-effecting tool it called on the earlier attempt — shell_run, fs_write —
// is issued a second time. That is data corruption, not a UX wrinkle: one
// fs_write executing twice can append twice, and a shell_run that was not
// idempotent has no way to signal it.
//
// The answer here is the one QwenPaw's stop-hook uses: do not rewind, append.
// Its react_agent, on a blocked exit, appends the continuation message as a
// new user turn onto `self.state.context` — the live conversation, with every
// tool call and result already in it — and lets the outer loop keep going. The
// injected wording follows its RubricGateConfig prompt ("You did not call any
// tool in the last turn. If the task is truly complete, confirm it. Otherwise,
// continue working…") plus the one clause that only matters once you stop
// replaying: the model is looking at its own completed tool calls, so it must
// be told they are done rather than pending.

package http

import (
	"github.com/cloudwego/eino/schema"
)

// continuationFallbackNudge is the injected user turn when the caller supplied
// no reason. Mirrors QwenPaw's `stop_result.continuation_message or "Continue
// working on the task."` default.
const continuationFallbackNudge = "Continue and finish addressing the user's request."

// continuationPreamble is prepended to every continuation nudge. It exists only
// because the history is no longer a replay: on the old path the model saw a
// fresh turn and naturally redid the work, and that WAS the intent. Now it sees
// its own tool_call/tool_result pairs already present, and without this line the
// most likely reading of "you did not finish" is "start over" — which would
// reintroduce by prompt exactly the double execution the history change removed.
const continuationPreamble = "The tool calls and results above already happened in this same turn — " +
	"they are done, do not repeat them. "

// buildContinuationHistory returns the message slice for a continued attempt.
//
// recorded is the conversation the ADK captured for the attempt that just ran
// (orchestrator.RecordedTurnMessages): the base history, every assistant
// message, and every tool_call/tool_result pair. When it is present, the
// continuation is exactly it plus one appended user nudge — nothing is
// rewound, so no completed tool call is offered to the model as outstanding
// work.
//
// base is the turn's starting history and is used ONLY when recorded is empty.
// That case is not a silent degradation to the old replay behaviour, because
// the two conditions coincide: the recorder captures after every model call,
// so an empty capture means no model call completed, which means no tool ran,
// which means there is nothing that could be executed twice. prevAssistantText
// covers the sub-case where text was streamed but the capture is unavailable,
// so the nudge still has something to refer back to.
//
// reason is the judge's or validator's explanation; empty falls back to the
// generic nudge. The returned slice is always freshly allocated — the caller's
// cs.history must never gain the nudge, or the persistent session history
// would carry a synthetic user turn into every later turn.
func buildContinuationHistory(base, recorded []*schema.Message,
	prevAssistantText, reason string) []*schema.Message {

	nudge := reason
	if nudge == "" {
		nudge = continuationFallbackNudge
	}

	if len(recorded) > 0 {
		out := make([]*schema.Message, 0, len(recorded)+1)
		out = append(out, recorded...)
		out = append(out, schema.UserMessage(continuationPreamble+nudge))
		return out
	}

	// No capture: nothing executed, so replaying the base history cannot
	// double-run anything. The preamble is omitted here on purpose — there are
	// no tool results above for it to refer to, and a sentence pointing at
	// messages that do not exist is worse than no sentence.
	out := make([]*schema.Message, 0, len(base)+2)
	out = append(out, base...)
	if prevAssistantText != "" {
		out = append(out, schema.AssistantMessage(prevAssistantText, nil))
	}
	out = append(out, schema.UserMessage(nudge))
	return out
}

// executedToolNames returns the names of every tool whose RESULT is present in
// msgs, in first-seen order.
//
// It is the observable definition of "this already ran": a tool message exists
// only after the tool returned, so a name here is a side effect that has
// already landed. The continuation path exists to keep this set from growing a
// duplicate entry, and a test can only assert that by counting — which is why
// this is a named function and not an inline loop in the test file.
func executedToolNames(msgs []*schema.Message) []string {
	var out []string
	for _, m := range msgs {
		if m == nil || m.Role != schema.Tool || m.ToolName == "" {
			continue
		}
		out = append(out, m.ToolName)
	}
	return out
}
