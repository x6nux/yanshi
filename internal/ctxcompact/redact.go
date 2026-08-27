// internal/ctxcompact/redact.go
package ctxcompact

import "github.com/cloudwego/eino/schema"

// Redactor is the narrow contract compaction needs in order to strip
// credentials out of the text it ships to the summary model:
// substring replacement over an already-registered secret registry.
// *secrets.Redactor satisfies it (TestRedactorContractMatchesSecretsPackage is
// the compile-time proof), and defining the interface HERE rather than naming
// secrets.Redactor throughout follows the same "accept interfaces" rule
// ModelSummarizer already follows in this package — ctxcompact is service
// layer, so its dependency arrow must not widen for a one-method need.
//
// Implementations must be safe for concurrent use: compaction can run on a
// pre-turn goroutine and inside a mid-turn ReAct iteration at the same time.
type Redactor interface {
	// Redact returns s with every registered secret substring replaced.
	Redact(s string) string
}

// redactForSummary returns a copy of msgs with every text-bearing field passed
// through r. It is THE C11 boundary, and the boundary is deliberately narrow:
//
// WHAT IT PROTECTS. A shell tool that prints an API key puts that key into a
// tool_result. Without this pass the key is handed to the summary model, gets
// folded into the summary text, and the summary is a PINNED message from then
// on — so the key is re-sent to the provider on every subsequent turn, forever,
// long after the tool_result it came from was compacted away. Redacting at the
// output boundaries (WS/SSE/logs/SQLite) does not help: this path never crosses
// one of those, it goes straight back out as model input.
//
// WHAT IT MUST NOT TOUCH, and why the copy is the whole point. Only the slice
// fed to the summarizer is redacted. The caller's msgs — the messages Plan
// PINNED and Assemble re-emits verbatim — are left alone, because those are
// still live conversation the model is working from: replacing a token inside a
// pinned tool_result with "[REDACTED]" would silently rewrite history the model
// is mid-task on, and the pinned copies never reach the summarizer anyway. The
// leak this closes is "the secret becomes PERMANENT by being summarized", not
// "the secret exists in the transcript at all"; that second problem is the
// output boundaries' job and is already solved there.
//
// TOOL-CALL IDS ARE EXEMPT ON PURPOSE. ToolCall.ID and ToolCallID are the keys
// splitIsSafe and EnforceToolCallPairs pair on. A secret short enough to appear
// as a substring of a generated call id would, if redacted, rename one half of
// a pair and not the other — takeChunk would then sever it and the provider
// answers 400. Ids are provider-generated opaque handles, not credentials, so
// the trade is one-sided.
//
// Messages whose text is unchanged keep their original pointer (no allocation,
// no identity churn), so a nil redactor or an empty registry is exactly the
// old behaviour. nil entries are preserved positionally — Run's caller indexes
// by position and RunSummary tolerates a nil.
func redactForSummary(r Redactor, msgs []*schema.Message) []*schema.Message {
	if r == nil || len(msgs) == 0 {
		return msgs
	}
	out := make([]*schema.Message, len(msgs))
	changed := false
	for i, m := range msgs {
		red, dirty := redactMessage(r, m)
		out[i] = red
		changed = changed || dirty
	}
	if !changed {
		return msgs
	}
	return out
}

// redactMessage returns m with its text fields redacted, plus whether anything
// actually changed. When nothing changed the ORIGINAL pointer comes back, so
// the common case (no secrets registered, or none present in this message)
// allocates nothing.
func redactMessage(r Redactor, m *schema.Message) (*schema.Message, bool) {
	if m == nil {
		return nil, false
	}
	content := r.Redact(m.Content)
	reasoning := r.Redact(m.ReasoningContent)
	name := r.Redact(m.Name)
	toolName := r.Redact(m.ToolName)
	calls, callsDirty := redactToolCalls(r, m.ToolCalls)
	multi, multiDirty := redactChatParts(r, m.MultiContent)
	input, inputDirty := redactInputParts(r, m.UserInputMultiContent)

	if content == m.Content && reasoning == m.ReasoningContent && name == m.Name &&
		toolName == m.ToolName && !callsDirty && !multiDirty && !inputDirty {
		return m, false
	}
	// Shallow-copy the struct so every field this function does not know about
	// (ResponseMeta, Extra, AssistantGenMultiContent, ToolCallID, Role) carries
	// over untouched, then overwrite only the redacted ones. Copying field by
	// field would silently drop whatever eino adds to schema.Message next.
	cp := *m
	cp.Content = content
	cp.ReasoningContent = reasoning
	cp.Name = name
	cp.ToolName = toolName
	if callsDirty {
		cp.ToolCalls = calls
	}
	if multiDirty {
		cp.MultiContent = multi
	}
	if inputDirty {
		cp.UserInputMultiContent = input
	}
	return &cp, true
}

// redactToolCalls redacts the function name and the JSON argument blob of every
// tool call, leaving ID (and Index/Type/Extra) alone — see redactForSummary for
// why ids are exempt. Arguments are where a credential most often rides along:
// `shell_run {"command":"curl -H 'Authorization: Bearer sk-…'"}`.
func redactToolCalls(r Redactor, in []schema.ToolCall) ([]schema.ToolCall, bool) {
	if len(in) == 0 {
		return in, false
	}
	out := make([]schema.ToolCall, len(in))
	copy(out, in)
	dirty := false
	for i := range out {
		name := r.Redact(out[i].Function.Name)
		args := r.Redact(out[i].Function.Arguments)
		if name != out[i].Function.Name || args != out[i].Function.Arguments {
			out[i].Function.Name = name
			out[i].Function.Arguments = args
			dirty = true
		}
	}
	if !dirty {
		return in, false
	}
	return out, true
}

// redactChatParts redacts the Text of every part in a (deprecated but still
// populated) MultiContent slice. Non-text parts carry URLs and blobs rather
// than transcript text and are copied through unchanged.
func redactChatParts(r Redactor, in []schema.ChatMessagePart) ([]schema.ChatMessagePart, bool) {
	if len(in) == 0 {
		return in, false
	}
	out := make([]schema.ChatMessagePart, len(in))
	copy(out, in)
	dirty := false
	for i := range out {
		if text := r.Redact(out[i].Text); text != out[i].Text {
			out[i].Text = text
			dirty = true
		}
	}
	if !dirty {
		return in, false
	}
	return out, true
}

// redactInputParts is redactChatParts for the current multimodal user-input
// shape. Both exist because eino carries the deprecated and the current slice
// side by side and either may be the populated one.
func redactInputParts(r Redactor, in []schema.MessageInputPart) ([]schema.MessageInputPart, bool) {
	if len(in) == 0 {
		return in, false
	}
	out := make([]schema.MessageInputPart, len(in))
	copy(out, in)
	dirty := false
	for i := range out {
		if text := r.Redact(out[i].Text); text != out[i].Text {
			out[i].Text = text
			dirty = true
		}
	}
	if !dirty {
		return in, false
	}
	return out, true
}

// redactSummary passes the model's own output back through the registry before
// it becomes a pinned message.
//
// It is defence in depth, not the primary control: the summarizer only ever saw
// redactForSummary's output, so a registered secret should not be reconstructible
// from it. "Should not" is doing work in that sentence — the model also sees the
// pinned tail on the mid-turn path, and a summary is the one artefact in this
// package with unbounded lifetime, so the cheap second pass is worth its cost.
func redactSummary(r Redactor, summary string) string {
	if r == nil {
		return summary
	}
	return r.Redact(summary)
}
