// Package orchestrator is yanshi's main coordinating agent (Eino ADK ChatModelAgent).
package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/proto"
)

// TurnUsage holds the per-turn token usage drained from an ADK event stream.
// The OpenAI acl wrapper records usage on each assistant message's
// ResponseMeta.Usage (eino-ext/libs/acl/openai chat_model.go:771,1215,1238);
// ClassifyEventsWithUsage reads it while classifying. For models that do not
// return usage (e.g. FakeModel, or providers that omit it) the values stay zero.
//
// Fields beyond PromptTokens/CompletionTokens (Task A6):
//   - CachedTokens    — prompt-cache hits (schema.TokenUsage.PromptTokenDetails.
//     CachedTokens). Carried on status frames + persisted so /cost can show
//     "X cached" and the footer can render savings from prompt caching.
//   - ReasoningTokens — reasoning-model spend (schema.TokenUsage.
//     CompletionTokensDetails.ReasoningTokens). Surfaces the cost of "thinking"
//     separately from the visible completion so users of reasoning models see
//     the real bill.
//   - TotalTokens     — the API's own sum (may differ from in+out when cached
//     and reasoning are priced differently). Used for /cost's grand total.
type TurnUsage struct {
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
	ReasoningTokens  int
	TotalTokens      int
}

// set overwrites the accumulator with u (the latest model-call usage). The API
// reports CUMULATIVE token counts per call (prompt includes the full context
// sent on that call), so the latest value is the current context size — using
// set (not a running sum) is what makes the ctx bar reflect real fill rather
// than an ever-growing total.
func (t *TurnUsage) set(u *schema.TokenUsage) {
	if u == nil {
		return
	}
	t.PromptTokens = u.PromptTokens
	t.CompletionTokens = u.CompletionTokens
	t.CachedTokens = u.PromptTokenDetails.CachedTokens
	t.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	t.TotalTokens = u.TotalTokens
}

// ClassifyEvents drains an ADK event iterator and invokes emit for each
// ServerFrame derived from the events:
//   - Assistant message with ReasoningContent -> thinking (one per reasoning delta)
//   - Assistant message with Content  -> agent_chunk
//   - Assistant message with ToolCalls -> one tool_call frame per call (status "running")
//   - Tool message (role Tool)         -> tool_result (status "ok", unless content is empty or a JSON {"error":...} -> "error")
//   - ev.Err != nil                    -> error
//
// Tool-result status is "ok" for normal content; "error" (with the extracted
// error message as Text) when content is empty or a JSON {"error":...} blob.
//
// It does not accumulate token usage; use ClassifyEventsWithUsage for that.
//
// Both streaming and non-streaming events are handled. With EnableStreaming
// true the ADK delivers each model/tool output as one event whose
// MessageOutput is a streaming variant (IsStreaming=true, MessageStream set,
// Message nil); classifyStream drains it chunk by chunk so one agent_chunk is
// emitted per delta (token-by-token streaming).
func ClassifyEvents(iter *adk.AsyncIterator[*adk.AgentEvent], emit func(proto.ServerFrame)) {
	classifyEvents(iter, nil, nil, emit)
}

// ClassifyEventsWithUsage is like ClassifyEvents and additionally captures
// token usage from each assistant message's ResponseMeta.Usage into usage. It
// is the variant the WS/SSE handlers use so /cost can report real per-turn
// tokens after the turn completes. emit receives exactly the same frames as
// ClassifyEvents would produce. Usage is OVERWRITTEN per model response (set,
// not summed): the API reports cumulative counts per call, so the latest value
// held in usage at turn end is the current context size.
//
// The optional onUsage callback (at most one — pass it as the trailing arg) is
// invoked with the latest *usage each time usage is captured from a model
// response (once per assistant message in the stream, i.e. between tool calls
// and as the model generates). This lets a caller emit a LIVE status frame
// during the turn — the WS handler uses it for real-time context-window
// updates — instead of only learning the totals at turn end. It is a variadic
// trailing parameter so existing 3-arg callers compile unchanged.
func ClassifyEventsWithUsage(iter *adk.AsyncIterator[*adk.AgentEvent], usage *TurnUsage, emit func(proto.ServerFrame), onUsage ...func(TurnUsage)) {
	var cb func(TurnUsage)
	if len(onUsage) > 0 {
		cb = onUsage[0]
	}
	classifyEvents(iter, usage, cb, emit)
}

// classifyEvents is the shared core of ClassifyEvents and ClassifyEventsWithUsage.
// usage may be nil, in which case no usage is captured. onUsage, when non-nil,
// is fired with the latest usage after each model response so callers can render
// real-time token usage mid-turn.
func classifyEvents(iter *adk.AsyncIterator[*adk.AgentEvent], usage *TurnUsage, onUsage func(TurnUsage), emit func(proto.ServerFrame)) {
	for {
		ev, ok := iter.Next()
		if !ok {
			return
		}
		if ev.Err != nil {
			if errors.Is(ev.Err, adk.ErrExceedMaxIterations) {
				emit(proto.NewError("The agent reached its maximum step limit. Try simplifying your request or breaking it into smaller steps."))
			} else {
				emit(proto.NewError(ev.Err.Error()))
			}
			return
		}
		if ev.Output == nil || ev.Output.MessageOutput == nil {
			continue
		}
		mv := ev.Output.MessageOutput
		// Streaming variant: the message hasn't materialized — content lives on
		// MessageStream. Drain it chunk by chunk (one frame per delta).
		if mv.IsStreaming && mv.MessageStream != nil {
			classifyStream(mv, usage, onUsage, emit)
			continue
		}
		msg := mv.Message
		if msg == nil {
			continue
		}
		classifyMessage(msg, usage, onUsage, emit)
	}
}

// notifyUsage fires onUsage with the current accumulator when both usage
// tracking and a callback are configured.
func notifyUsage(usage *TurnUsage, onUsage func(TurnUsage)) {
	if usage != nil && onUsage != nil {
		onUsage(*usage)
	}
} // classifyMessage emits frames for a fully-materialized (non-streaming) message.
func classifyMessage(msg *schema.Message, usage *TurnUsage, onUsage func(TurnUsage), emit func(proto.ServerFrame)) {
	switch msg.Role {
	case schema.Assistant:
		// Surface token usage when the model provided it (real OpenAI models
		// do; FakeModel does not, so usage simply stays at its prior value).
		if usage != nil && msg.ResponseMeta != nil {
			usage.set(msg.ResponseMeta.Usage)
			notifyUsage(usage, onUsage)
		}
		emitAssistant(msg, emit)
	case schema.Tool:
		emitToolResult(msg, emit)
	}
}

// classifyStream drains a streaming MessageOutput, emitting one frame per
// chunk. mv.Role identifies the stream's origin (Assistant for model output,
// Tool for a streaming tool result) since per-chunk message roles may be unset
// on deltas.
//
// Token usage: within ONE stream the usage on each chunk is CUMULATIVE — many
// gateways (and the openai acl wrapper) carry the FULL running totals on every
// chunk, not a per-chunk delta. Summing per chunk would therefore multiply the
// count by the number of chunks. Instead the LAST non-nil usage seen in the
// stream is captured and applied ONCE at stream end via set (overwrite), not
// summed. Across separate model calls within a turn the same applies — each
// call's latest value overwrites, because that value is the current context
// size (the API reports cumulative counts). onUsage fires once at stream end
// with the overwritten accumulator.
//
// Tool-call argument DELTAS are accumulated per call (keyed by the ToolCall
// Index — the streaming merge key) and emitted as exactly ONE tool_call frame
// per call, rather than one frame per delta fragment (which produced a cascade
// of partial blocks like `({) ⡿ (") ⡿ (path) ⡿ …` in the TUI). Completion is
// detected by the accumulated arguments parsing as valid JSON (the closing
// brace arrived); calls whose args never reach valid JSON (no-arg tools, a
// single complete blob, or degenerate input) are flushed when a subsequent
// delta carries no tool calls (the stream moved past tool-call emission) or at
// stream end. See toolCallAccumulator for details.
func classifyStream(mv *adk.MessageVariant, usage *TurnUsage, onUsage func(TurnUsage), emit func(proto.ServerFrame)) {
	defer mv.MessageStream.Close()
	acc := newToolCallAccumulator()
	// lastUsage is the most recent non-nil usage seen in this stream. It is
	// applied once at stream end (deferred) rather than summed per chunk — see
	// the function comment. Each chunk carries a fresh *TokenUsage, so holding
	// the pointer and reading it once at the end yields the stream's final
	// cumulative total.
	var lastUsage *schema.TokenUsage
	defer func() {
		if usage != nil && lastUsage != nil {
			usage.set(lastUsage)
			notifyUsage(usage, onUsage)
		}
	}()
	for {
		msg, err := mv.MessageStream.Recv()
		if errors.Is(err, io.EOF) {
			acc.flush(emit)
			return
		}
		if err != nil {
			acc.flush(emit)
			emit(proto.NewError(err.Error()))
			return
		}
		if msg == nil {
			continue
		}
		if msg.ResponseMeta != nil && msg.ResponseMeta.Usage != nil {
			lastUsage = msg.ResponseMeta.Usage
		}
		switch mv.Role {
		case schema.Tool:
			emitToolResult(msg, emit)
		default:
			// Assistant delta: emit thinking + content immediately (one frame per
			// chunk), but accumulate tool-call argument deltas and emit ONE
			// tool_call frame per call once its args are complete.
			emitAssistantContent(msg, emit)
			if len(msg.ToolCalls) > 0 {
				acc.update(msg.ToolCalls)
				acc.emitReady(emit)
			} else {
				// No tool calls on this delta → tool-call emission for this
				// assistant turn is done. Flush any still-pending calls whose
				// args never formed valid JSON so they appear now, not at EOF.
				acc.flush(emit)
			}
		}
	}
}

// toolCallAccumulator collects streaming tool-call argument deltas per call
// (keyed by ToolCall.Index, the streaming merge key the openai acl sets on
// every fragment) and emits exactly ONE tool_call frame per call once its
// arguments are complete. This collapses the per-delta cascade of partial
// tool_call frames that streaming otherwise produces.
//
// Completion (checked after every update via emitReady):
//   - PRIMARY: the accumulated arguments parse as valid JSON — the closing
//     brace of the arguments object has arrived. The openai streaming format
//     always sends arguments as a JSON object fragmented across deltas, so this
//     is reliable and never fires mid-fragment (an unterminated string or
//     trailing comma is not valid JSON).
//   - FALLBACK (flush): a delta carries no tool calls (the stream moved on to
//     content/reasoning) OR the stream ends. This catches calls whose args
//     never reach valid JSON — no-arg tools whose Arguments stayed "", a single
//     complete blob, or malformed input — so they are not lost.
//
// "emitted" tracks which indices have already produced a frame so subsequent
// fragments for the same call do not re-emit. Parallel/interleaved tool calls
// are handled correctly: each index accumulates independently, and the
// no-tool-calls fallback only fires once ALL interleaved calls have stopped
// (arg fragments always carry their ToolCall entry, so an interleaving delta
// never triggers the flush).
type toolCallAccumulator struct {
	calls   map[int]*pendingToolCall
	emitted map[int]bool
}

// pendingToolCall is the per-index accumulator: the call's name (set by the
// first fragment that carries it) and the concatenation of every argument
// fragment in arrival order.
type pendingToolCall struct {
	id, name, args string
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{
		calls:   map[int]*pendingToolCall{},
		emitted: map[int]bool{},
	}
}

// sortedIndices returns the pending call indices in ascending order so
// tool_call frames are emitted deterministically. Go map iteration is
// randomized, so without sorting, parallel/interleaved calls would appear in a
// nondeterministic order — which is both flaky for tests and jarring in the TUI
// (calls shuffling between renders). Index is the streaming merge key, so
// ascending order matches the order the model emits the calls.
func (a *toolCallAccumulator) sortedIndices() []int {
	idxs := make([]int, 0, len(a.calls))
	for idx := range a.calls {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	return idxs
}

// update merges a delta's tool-call fragments into the accumulator. The Index
// field (the streaming merge key) identifies the call; a fragment without Index
// is keyed by its position (treated as a self-contained call). The name is
// taken from the first fragment carrying it; argument fragments are appended.
func (a *toolCallAccumulator) update(tcs []schema.ToolCall) {
	for i := range tcs {
		tc := &tcs[i]
		idx := i
		if tc.Index != nil {
			idx = *tc.Index
		}
		p, ok := a.calls[idx]
		if !ok {
			p = &pendingToolCall{}
			a.calls[idx] = p
		}
		if tc.ID != "" {
			p.id = tc.ID
		}
		if tc.Function.Name != "" {
			p.name = tc.Function.Name
		}
		if tc.Function.Arguments != "" {
			p.args += tc.Function.Arguments
		}
	}
}

// emitReady emits one tool_call frame (status "running") for each pending call
// whose accumulated arguments now parse as valid JSON, marking it emitted so
// later fragments for the same call do not re-emit. The emitted frame carries
// the COMPLETE accumulated arguments so the TUI can render the call's summary
// (e.g. the fs_read path).
func (a *toolCallAccumulator) emitReady(emit func(proto.ServerFrame)) {
	for _, idx := range a.sortedIndices() {
		p := a.calls[idx]
		if a.emitted[idx] {
			continue
		}
		if p.args != "" && json.Valid([]byte(p.args)) {
			emit(proto.NewToolCall(p.name, p.args, "running"))
			a.emitted[idx] = true
		}
	}
}

// flush emits every still-pending call (used when the stream moves past
// tool-call emission or ends), so a call whose args never reached valid JSON
// is not lost. Each is marked emitted to guard against a second flush.
func (a *toolCallAccumulator) flush(emit func(proto.ServerFrame)) {
	for _, idx := range a.sortedIndices() {
		p := a.calls[idx]
		if a.emitted[idx] {
			continue
		}
		emit(proto.NewToolCall(p.name, p.args, "running"))
		a.emitted[idx] = true
	}
}

// emitAssistantContent emits the content portion of an assistant message (or
// streaming delta): one thinking frame per non-empty ReasoningContent delta, and
// one agent_chunk per non-empty Content. Tool calls are deliberately NOT
// handled here — the non-streaming path emits them directly via emitAssistant,
// while the streaming path accumulates argument deltas (toolCallAccumulator) so
// only one tool_call frame is emitted per call. Reasoning is emitted before
// content so the TUI's thinking block renders above the answer (think → answer).
func emitAssistantContent(msg *schema.Message, emit func(proto.ServerFrame)) {
	if msg.ReasoningContent != "" {
		emit(proto.NewThinking(msg.ReasoningContent))
	}
	if msg.Content != "" {
		emit(proto.NewAgentChunk(msg.Content))
	}
}

// emitAssistant emits frames for a fully-materialized assistant message:
//   - one thinking frame per non-empty ReasoningContent delta (reasoning models)
//   - one agent_chunk per non-empty Content
//   - one tool_call frame per ToolCall (each carries its complete Arguments)
//
// Used only by the non-streaming path (classifyMessage). The streaming path
// (classifyStream) emits content via emitAssistantContent and accumulates tool
// calls so partial argument deltas collapse to one frame per call.
func emitAssistant(msg *schema.Message, emit func(proto.ServerFrame)) {
	emitAssistantContent(msg, emit)
	for _, tc := range msg.ToolCalls {
		args := ""
		if tc.Function.Arguments != "" {
			args = tc.Function.Arguments
		}
		emit(proto.NewToolCall(tc.Function.Name, args, "running"))
	}
}

// emitToolResult emits a tool_result frame. Status is "error" when the content
// is empty OR is a JSON object carrying a non-empty "error" field (the form
// GuardedTool produces when a tool fails, so the model can retry); in that case
// Text is the extracted error message (human-readable, not the raw JSON).
// Otherwise status is "ok" with the full content.
func emitToolResult(msg *schema.Message, emit func(proto.ServerFrame)) {
	text := msg.Content
	status := "ok"
	if msg.Content == "" {
		status = "error"
	} else if em := extractErrorResult(msg.Content); em != "" {
		status = "error"
		text = em
	}
	emit(proto.NewToolResult(msg.ToolName, text, status))
}

// extractErrorResult returns the "error" field's value when content is a JSON
// object with a non-empty string "error" field (the shape GuardedTool returns
// for tool/permission failures); otherwise "". Lenient: invalid JSON, non-error
// objects, and empty/missing "error" all return "".
func extractErrorResult(content string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(content), &m); err != nil {
		return ""
	}
	if e, ok := m["error"]; ok {
		if s, ok := e.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// finalOutputAccumulator tracks the user-facing final answer across a turn's
// events: the last non-empty assistant Content.
//
// The model now ends a turn by stopping naturally — JudgeCompletion arbitrates
// whether the stop was premature — so the final assistant message IS the
// answer. There is no task_end tool result to skip over (the old ReturnDirectly
// shape ended state.Messages on Role=Tool, which is why this accumulator used
// to special-case task_end). observe scans every assistant message and keeps
// the last non-empty Content.
//
// EMPTY-ANSWER handling: if the model never produced Content, finalize returns
// the "no assistant message produced" error — the turn produced nothing
// user-visible.
type finalOutputAccumulator struct {
	lastContent string
}

// observe processes one materialized message from the event stream. It ignores
// nil and non-assistant messages (tool results, user echo) and, for assistant
// messages, records the last non-empty Content.
func (a *finalOutputAccumulator) observe(msg *schema.Message) {
	if msg == nil || msg.Role != schema.Assistant {
		return
	}
	if msg.Content != "" {
		a.lastContent = msg.Content
	}
}

// finalize returns the accumulated final answer, or the "no assistant message
// produced" error when the model never produced Content.
func (a *finalOutputAccumulator) finalize() (string, error) {
	if a.lastContent == "" {
		return "", fmt.Errorf("orchestrator: no assistant message produced")
	}
	return a.lastContent, nil
}
