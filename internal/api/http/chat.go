package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/ctxcompact"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	obslog "github.com/x6nux/yanshi/internal/observe/log"
	otelobs "github.com/x6nux/yanshi/internal/observe/otel"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/secrets"
	"github.com/x6nux/yanshi/internal/skills"
	"github.com/x6nux/yanshi/internal/tools"
	"github.com/x6nux/yanshi/internal/vcs"
)

// Chat registers POST /api/v1/chat — the SSE transport. See handleSSEInternal
// for the lifecycle; Chat only registers the route and forwards parameters
// (CB5: the extracted core is callable directly from tests without
// re-registering the mux).
func (s *Server) Chat(o *orchestrator.Orchestrator, models map[string]model.BaseChatModel, reg *skills.Registry) {
	s.HandleFunc("POST /api/v1/chat", func(w http.ResponseWriter, r *http.Request) {
		s.handleSSEInternal(w, r, o, models, reg)
	})
}

// handleSSEInternal owns one SSE request lifecycle. It is a production seam
// (also invoked directly by deterministic tests), not a test-only helper.
//
// B2-RB1: a valid SSE request is one logical turn. SSE keeps history client-
// side, so session_id is intentionally empty; the message count is still sealed
// for audit symmetry with WS. The post-turn seam is registered via defer
// immediately after the pre-turn seam so every later runner / compaction /
// schema exit, including panic, executes it (必修项 F).
func (s *Server) handleSSEInternal(w http.ResponseWriter, r *http.Request,
	o *orchestrator.Orchestrator, models map[string]model.BaseChatModel,
	reg *skills.Registry) {
	var req struct {
		Message  string           `json:"message"`
		Messages []schema.Message `json:"messages"`
		Model    string           `json:"model,omitempty"`
		Thinking string           `json:"thinking,omitempty"`
		// OutputSchema carries an optional JSON Schema for this turn (A12-core).
		// When non-empty the handler validates the model's final assistant text
		// against it and retries with a reminder on failure (up to
		// maxSchemaRetries); on success it emits a structured_result event
		// before status/done. Empty/absent ⇒ text mode, byte-identical to
		// pre-A12 (the entire schema retry loop is skipped).
		OutputSchema json.RawMessage `json:"output_schema,omitempty"`
		ThreadID     string          `json:"thread_id,omitempty"`
		TurnID       string          `json:"turn_id,omitempty"`
		// Images carries image attachments for this turn, matching the WS
		// ClientFrame.Images wire form. SSE keeps its own request struct (the
		// shared frame vocabulary is ServerFrame only), so this field has to be
		// declared here explicitly: json.Decode silently ignores unknown keys,
		// which is exactly how attachments POSTed to SSE used to disappear
		// without even a parse error.
		Images []proto.ImageAttach `json:"images,omitempty"`
	}
	if err := json.NewDecoder(limitBody(w, r)).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	history := req.Messages
	if len(history) == 0 && req.Message != "" {
		history = []schema.Message{{Role: schema.User, Content: req.Message}}
	}
	if len(history) == 0 {
		writeSSEError(w, "empty request", s.redactor)
		return
	}

	// Apply /skill prefix to the last user turn (the new message).
	if last := &history[len(history)-1]; last.Role == schema.User {
		q, errMsg := resolveQuery(reg, last.Content)
		if errMsg != "" {
			writeSSEError(w, errMsg, s.redactor)
			return
		}
		last.Content = q
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)

	// Correlation IDs + the turn span. SSE bound neither: its log lines during
	// a turn carried no ids, and otelobs.StartTurn's only production caller was
	// the synchronous Orchestrator.Query, which this path does not use -- so a
	// real request produced no span at all. This is the SSE half of the drain
	// boundary the orchestrator's Query doc comment refers to; the span has to
	// wrap the whole retry loop below, because a schema retry is one turn from
	// the client's point of view, not several.
	//
	// thread_id / turn_id come off the wire. They were declared on the request
	// struct and read nowhere in the file, so a client that sent them got a 200
	// and silently no correlation. SSE is stateless -- the server has no
	// conversation identity of its own -- which makes the client's id the only
	// thing that can link two requests of one conversation. An absent turn_id
	// still gets a minted one so a single request's own lines stay joined.
	// Sanitized, not trusted: these are the only two identifiers in the system
	// that a client chooses, and they land in every log line and span attribute
	// of the request. See obslog.SanitizeID for the three ways a raw one bites.
	turnID := obslog.SanitizeID(req.TurnID)
	if turnID == "" {
		turnID = obslog.NewTurnID()
	}
	reqCtx := obslog.WithIDs(r.Context(), obslog.IDs{
		TraceID:   obslog.NewTraceID(),
		SessionID: obslog.SanitizeID(req.ThreadID),
		TurnID:    turnID,
	})
	var turnErr error
	reqCtx, endTurn := otelobs.StartTurn(reqCtx, einollm.ResolveModelName(models, req.Model))
	defer func() { endTurn(turnErr) }()

	msgs := make([]*schema.Message, len(history))
	for i := range history {
		msgs[i] = &history[i]
	}

	// B2-RB1: pre-turn seam + post-turn seam defer. defer params evaluate at
	// registration, so post-turn uses the same pre-runner message count; every
	// later error / cancel / panic / early return path fires it.
	s.sealTurnBoundary("", 1, len(msgs),
		string(vcs.SeamPreTurn), "sse:pre-turn:1")
	defer s.sealTurnBoundary("", 1, len(msgs),
		string(vcs.SeamPostTurn), "sse:post-turn:1")

	// Auto context-compaction (Task 35b): if the received history exceeds
	// the threshold, summarize the older turns on a remote model, STREAMING
	// each summary delta as a compact_chunk SSE event, then emit
	// history_replaced (the compacted slice, so sseBackend adopts it before
	// its next request) and status{compacted} before the turn frames.
	// Disabled (threshold <= 0) or under-threshold histories are a no-op.
	// SSE holds history client-side, so publishing the compacted slice is
	// what keeps the next POST consistent with the server's view.
	kr := keepRecentOrDefault(s.compaction.KeepRecent)
	cw := contextWindowFor(req.Model, s.compaction)
	sumModel := compactionModel(s.compaction, models, req.Model)
	var newMsgs []*schema.Message
	var tb, ta int
	compacted := false
	if sumModel != nil {
		newMsgs, tb, ta, compacted = ctxcompact.MaybeCompact(r.Context(), msgs,
			s.compaction.Threshold, cw, kr, sumModel,
			func(chunk string) { writeSSEFrame(w, fl, proto.NewCompactChunk(chunk), s.redactor) })
	}
	if compacted {
		msgs = newMsgs
		compactedHistory := make([]schema.Message, len(msgs))
		for i, m := range msgs {
			compactedHistory[i] = *m
		}
		writeSSEFrame(w, fl, proto.NewHistoryReplaced(compactedHistory), s.redactor)
		st := proto.NewStatus(req.Model, req.Thinking, 0, 0, 0, contextWindowFor(req.Model, s.compaction))
		st.Compacted, st.TokensBefore, st.TokensAfter = true, tb, ta
		writeSSEFrame(w, fl, st, s.redactor)
	}

	// Per-request model + thinking. An unknown/empty model name falls back
	// to the orchestrator default (models[name] is nil for a nil map or an
	// absent name); an unrecognized thinking effort is a no-op downstream.
	// turnModel is the registry NAME this turn runs on: the requested model, else
	// the first sorted registry name (einollm.ResolveModelName — the same
	// fallback the WS session default and the /api/v1 turn path use). It feeds
	// both TurnOpts.ModelID (which decides native image parts vs. stored
	// placeholders) and the billing ledger below, so the model we describe to
	// the orchestrator is the model we charge.
	turnModel := einollm.ResolveModelName(models, req.Model)
	opts := orchestrator.TurnOpts{
		// No ThreadID: SSE is stateless. The client holds the history and
		// replays it every request, so the server has no conversation identity
		// to correlate against — minting one here would produce a fresh id per
		// request, which is worse than none because it looks like a thread.
		// ensureTurnIDs still mints a TurnID, so a single turn's logs stay
		// correlated with each other.
		ThinkingEffort: req.Thinking,
		OutputSchema:   req.OutputSchema,
		ModelID:        turnModel,
		Images:         req.Images,
	}
	if req.Model != "" && models[req.Model] != nil {
		opts.Model = models[req.Model]
	}

	// C4 COST1 SSE billing. SSE is stateless across POSTs, so the ledger is
	// per-request (not per-session like WS). We charge the SAME resolved name
	// the turn declares (turnModel), so an unset model still bills the
	// orchestrator's default pick — matching the WS path's
	// resetBilling-on-default behavior.
	billingModel := turnModel
	var sseLedger einollm.Ledger
	sseCostUSD := 0.0
	_, sseCostKnown := s.priceTab[billingModel]
	sseHasBilled := false
	sseOnUsage := func(u orchestrator.TurnUsage) {
		priced := usageForPricing(u)
		if priced.Prompt <= 0 && priced.Cached <= 0 && priced.Completion <= 0 {
			return
		}
		sseLedger.Add(priced)
		cost, known := einollm.CostOK(s.priceTab, billingModel, priced)
		if !sseHasBilled {
			sseCostKnown = known
			sseHasBilled = true
		} else {
			sseCostKnown = sseCostKnown && known
		}
		if known {
			sseCostUSD += cost
		}
	}

	// A12-core: per-turn structured output. When the POST body declares an
	// output_schema the loop below validates the model's final assistantText
	// against it and retries with a reminder on failure, mirroring the WS
	// handler's schema path. hasSchema=false keeps the text path
	// byte-identical to pre-A12 (retryCap=0 ⇒ single attempt, original flow).
	hasSchema := len(req.OutputSchema) > 0
	retryCap := 0
	if hasSchema {
		retryCap = maxSchemaRetries
	}
	var usage orchestrator.TurnUsage
	var structuredResult json.RawMessage
	var prevAssistantText string
	var lastVErr error
	for attempt := 0; attempt <= retryCap; attempt++ {
		// Build the history for this attempt. Attempt 0 uses msgs unchanged.
		// attempt > 0 extends a COPY of msgs with the previous attempt's
		// assistant output plus a user reminder carrying the validation
		// error — without this extension every retry would re-send the SAME
		// history, the model would reproduce the same invalid output, and
		// the retry would be useless. The copy is mandatory: appending to
		// msgs directly would alias its backing array and leak the reminder
		// into subsequent attempts' baseline.
		runMsgs := msgs
		if attempt > 0 {
			extra := make([]*schema.Message, 0, 2)
			if prevAssistantText != "" {
				extra = append(extra, schema.AssistantMessage(prevAssistantText, nil))
			}
			extra = append(extra, schema.UserMessage(schemaRetryReminder(prevAssistantText, lastVErr)))
			runMsgs = make([]*schema.Message, 0, len(msgs)+len(extra))
			runMsgs = append(runMsgs, msgs...)
			runMsgs = append(runMsgs, extra...)
		}

		// Reset per-attempt state so earlier attempts' partial output is
		// discarded — the FINAL attempt's usage is what sseStatus reports.
		usage = orchestrator.TurnUsage{}
		var assistantText string
		// SSE is stateless and unidirectional, so it does NOT install a
		// permission callback: tool calls denied by the static profile are
		// denied (no interactive prompt). Interactive permissions are
		// WS-only (see ws.go). The orchestrator injects the static profile
		// here. tc is recreated per attempt so the err-counter is fresh.
		tc := tools.WithErrCounter(reqCtx)

		lifecycleRelay := newSSELifecycleRelay()
		tc = tools.WithSubAgentEmit(tc, lifecycleRelay.Emit)

		iter := o.EventsWithHistoryOpts(tc, runMsgs, opts)
		var hadError bool
		mainFrames := make(chan proto.ServerFrame, 64)
		classDone := make(chan struct{})

		go func() {
			defer close(classDone)
			orchestrator.ClassifyEventsWithUsage(iter, &usage, func(f proto.ServerFrame) {
				select {
				case mainFrames <- f:
				case <-r.Context().Done():
				}
			}, sseOnUsage)
		}()

	mergeLoop:
		for {
			select {
			case f, ok := <-mainFrames:
				if !ok {
					break mergeLoop
				}
				if f.Type == "error" {
					hadError = true
				}
				if f.Type == "agent_chunk" {
					assistantText += f.Text
				}
				writeSSEFrame(w, fl, f, s.redactor)
			case f := <-lifecycleRelay.terminal:
				writeSSEFrame(w, fl, f, s.redactor)
			case f := <-lifecycleRelay.progress:
				writeSSEFrame(w, fl, f, s.redactor)
			case <-classDone:
				break mergeLoop
			case <-r.Context().Done():
				break mergeLoop
			}
		}
		// Drain lifecycle frames (e.g. subagent events) still buffered in the
		// relay: the merge select above can race past progress events when
		// classDone fires, so flush them before the status/done terminator.
		drainLifecycleFrames(w, fl, lifecycleRelay, s.redactor)
		// Hard failures break regardless of mode: a model error or a user
		// cancel must not trigger a schema retry. The error frame has
		// already been emitted above; the post-loop path still emits status
		// + done.
		if hadError || r.Context().Err() != nil {
			// Attribute the failure to the turn span. Without this every SSE
			// turn ends with a nil error and reports success, so a model
			// failure or a client disconnect is indistinguishable from a
			// clean run in any tracing backend -- the one question a turn
			// span exists to answer.
			if hadError {
				turnErr = errors.New("turn emitted an error frame")
			} else {
				turnErr = r.Context().Err()
			}
			break
		}
		if !hasSchema {
			break // text mode: single attempt, original behavior
		}
		// Schema path: validate the final assistant text. Success sets
		// structuredResult and breaks; failure either retries with a
		// reminder (carrying the validation error) or, at the cap, emits an
		// error frame and breaks.
		validated, verr := ValidateStructuredOutput(assistantText, req.OutputSchema)
		if verr == nil {
			structuredResult = validated
			break
		}
		lastVErr = verr
		if attempt == retryCap {
			turnErr = fmt.Errorf("output did not match the required schema after %d attempt(s): %w",
				attempt+1, verr)
			writeSSEFrame(w, fl, proto.NewError(turnErr.Error()), s.redactor)
			break
		}
		prevAssistantText = assistantText
	}

	// A12-core: emit the validated structured result before status/done so
	// a schema-constrained consumer (exec --output-schema, API client, later
	// the TUI) can take the parsed JSON without re-parsing the stream.
	// Skipped on text-mode turns (structuredResult stays nil).
	if structuredResult != nil {
		writeSSEFrame(w, fl, proto.NewStructuredResult(structuredResult), s.redactor)
	}
	// Emit a status frame with the selection + usage before terminating so
	// the client can update its model indicator and /cost from either
	// transport. turns is always 1 for a stateless SSE request. Cached /
	// Reasoning (Task A6) are populated post-construction so NewStatus's
	// signature stays unchanged; the SSE and WS paths emit the same fields.
	sseStatus := proto.NewStatus(req.Model, req.Thinking, usage.PromptTokens, usage.CompletionTokens, 1, contextWindowFor(req.Model, s.compaction))
	sseStatus.CachedTokens = usage.CachedTokens
	sseStatus.ReasoningTokens = usage.ReasoningTokens
	// C4 COST1: surface the per-request cost. /cost on SSE reflects ONE POST
	// (the stateless model); the WS path reflects the cumulative session.
	// Gated by observe.cost_in_status, same as the WS statusFrame — a flag
	// honoured on one transport only is worse than one honoured on neither.
	if s.featuresReg.EnabledOrDefault("observe.cost_in_status") {
		sseStatus.CostUSD = sseCostUSD
		sseStatus.CostKnown = sseCostKnown
	}
	// MEM1: surface memory path on SSE status too, for remote clients.
	sseStatus.MemoryPath = s.memoryPath
	// C4: surface log path on SSE status too (parity with WS).
	sseStatus.LogPath = s.logPath
	writeSSEFrame(w, fl, sseStatus, s.redactor)
	writeSSEFrame(w, fl, proto.NewDone(), s.redactor)
}

// writeSSEFrame emits one ServerFrame as a structured SSE event:
//
//	event: <f.Type>
//	data: <ServerFrame JSON>
//
// r is the process-wide secrets redactor; pass nil to disable redaction (only
// acceptable in tests that don't register secrets). RedactJSON covers both raw
// and JSON-escaped spellings of every registered secret, so all current and
// future ServerFrame fields (Text, ToolArgs, StructuredResult, Messages,
// Sessions, etc.) inherit coverage without naming fields here.
func writeSSEFrame(w http.ResponseWriter, fl http.Flusher, f proto.ServerFrame, r *secrets.Redactor) {
	event, data := f.SSEEvent()
	if r != nil {
		data = r.RedactJSON(data)
	}
	_, _ = w.Write([]byte("event: " + event + "\ndata: "))
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n\n"))
	if fl != nil {
		fl.Flush()
	}
}

// writeSSEError emits an error frame followed by a done frame (and flushes)
// before the stream is established (bad skill, empty request) so clients route
// it to the error branch like an orchestrator error. The trailing done matches
// the WS handler's error-then-done shape so both transports terminate the same
// way on a pre-stream error. r is the same process-wide redactor writeSSEFrame
// uses; nil disables redaction (tests only).
func writeSSEError(w http.ResponseWriter, msg string, r *secrets.Redactor) {
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	writeSSEFrame(w, fl, proto.NewError(msg), r)
	writeSSEFrame(w, fl, proto.NewDone(), r)
}
