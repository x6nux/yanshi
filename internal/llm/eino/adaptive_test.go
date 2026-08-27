package eino

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/eino-contrib/jsonschema"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	obslog "github.com/x6nux/yanshi/internal/observe/log"
)

// reasoningRequiredErr is the 400 a thinking model returns when the history
// carries no reasoning_content.
var reasoningRequiredErr = errors.New(
	"error, status code: 400, message: assistant message must contain reasoning_content")

// schemaRefErr is the 400 a strict gateway returns for a generated tool schema.
var schemaRefErr = errors.New(
	"error, status code: 400, message: tools[0].function.parameters: $ref is not supported")

// systemRoleErr is the 400 a restrictive chat template returns.
var systemRoleErr = errors.New(
	"error, status code: 400, message: role 'system' is not supported by this model")

// refSchemaToolInfo builds a ToolInfo whose parameter schema uses the shapes a
// strict gateway rejects.
func refSchemaToolInfo(t *testing.T, name string) *schema.ToolInfo {
	t.Helper()
	js := &jsonschema.Schema{}
	raw := `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object",
	         "properties":{"p":{"$ref":"#/$defs/P"}},
	         "$defs":{"P":{"type":["string","null"]}}}`
	if err := json.Unmarshal([]byte(raw), js); err != nil {
		t.Fatal(err)
	}
	return &schema.ToolInfo{Name: name, ParamsOneOf: schema.NewParamsOneOfByJSONSchema(js)}
}

// toolSchemaOf extracts the generic JSON Schema of the first tool in a
// recorded option set.
func toolSchemaOf(t *testing.T, opts *model.Options) map[string]any {
	t.Helper()
	if opts == nil || len(opts.Tools) == 0 {
		t.Fatal("no tools were passed to the model")
	}
	js, err := opts.Tools[0].ToJSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(js)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestAdaptiveLearnsReasoningContentQuirk is the M5 end-to-end assertion: the
// first request fails, the repaired retry succeeds, the quirk is recorded, and
// the NEXT call is repaired up front — one failed request per model per
// process, not one per turn.
func TestAdaptiveLearnsReasoningContentQuirk(t *testing.T) {
	inner := &stagedModel{errs: []error{reasoningRequiredErr}}
	quirks := NewQuirkStore()
	a := NewAdaptiveModel(inner, AdaptiveConfig{ModelID: "deepseek-r1", Quirks: quirks})

	msgs := []*schema.Message{
		schema.UserMessage("hi"),
		schema.AssistantMessage("hello", nil),
		schema.UserMessage("again"),
	}
	if _, err := a.Generate(context.Background(), msgs); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if inner.callCount() != 2 {
		t.Fatalf("inner called %d times, want 2", inner.callCount())
	}
	if inner.call(0)[1].ReasoningContent != "" {
		t.Error("the FIRST attempt was already repaired; the quirk cannot have been learned from it")
	}
	if got := inner.call(1)[1].ReasoningContent; got != reasoningPlaceholder {
		t.Errorf("the retry's assistant message reasoning = %q, want the placeholder", got)
	}
	if !quirks.Has("deepseek-r1", QuirkNeedsReasoningContent) {
		t.Fatal("the quirk was not learned after a successful recovery")
	}
	// The caller's history must be untouched — it is the ADK's live slice.
	if msgs[1].ReasoningContent != "" {
		t.Error("the caller's history was mutated in place")
	}

	// Second call: repaired up front, no failure at all.
	inner2 := &stagedModel{}
	b := NewAdaptiveModel(inner2, AdaptiveConfig{ModelID: "deepseek-r1", Quirks: quirks})
	if _, err := b.Generate(context.Background(), msgs); err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	if inner2.callCount() != 1 {
		t.Errorf("inner called %d times on a learned model, want 1", inner2.callCount())
	}
	if inner2.call(0)[1].ReasoningContent != reasoningPlaceholder {
		t.Error("the learned quirk was not applied up front")
	}
}

// TestAdaptiveDoesNotLearnWhenTheRetryAlsoFails pins the evidence gate: a quirk
// is a claim about the model, and one ambiguous 400 must not permanently alter
// every future request.
func TestAdaptiveDoesNotLearnWhenTheRetryAlsoFails(t *testing.T) {
	inner := &stagedModel{errs: []error{reasoningRequiredErr, errors.New("status code: 500, boom")}}
	quirks := NewQuirkStore()
	a := NewAdaptiveModel(inner, AdaptiveConfig{ModelID: "m", Quirks: quirks})

	msgs := []*schema.Message{schema.UserMessage("hi"), schema.AssistantMessage("hello", nil)}
	if _, err := a.Generate(context.Background(), msgs); err == nil {
		t.Fatal("Generate succeeded; want the second failure")
	}
	if quirks.Has("m", QuirkNeedsReasoningContent) {
		t.Error("the quirk was learned from a failed recovery")
	}
	if inner.callCount() != 2 {
		t.Errorf("inner called %d times, want exactly 2", inner.callCount())
	}
}

// TestAdaptiveDoesNotRetryWhenTheRepairChangesNothing pins that a repair with
// nothing to do does not trigger a resend: an identical second request only
// costs quota.
func TestAdaptiveDoesNotRetryWhenTheRepairChangesNothing(t *testing.T) {
	inner := &stagedModel{errs: []error{reasoningRequiredErr}}
	quirks := NewQuirkStore()
	a := NewAdaptiveModel(inner, AdaptiveConfig{ModelID: "m", Quirks: quirks})

	// Every assistant message already carries reasoning_content, so the repair
	// is a no-op.
	msgs := []*schema.Message{
		schema.UserMessage("hi"),
		{Role: schema.Assistant, Content: "hello", ReasoningContent: "thought"},
	}
	if _, err := a.Generate(context.Background(), msgs); err == nil {
		t.Fatal("Generate succeeded; want the provider error")
	}
	if inner.callCount() != 1 {
		t.Errorf("inner called %d times, want 1 — an unchanged retry is a wasted charge", inner.callCount())
	}
	if quirks.Has("m", QuirkNeedsReasoningContent) {
		t.Error("a quirk was learned without a repair having been applied")
	}
}

// TestAdaptiveLearnsSchemaQuirkAndSanitizes wires M5's detection to M6's
// rewrite: a schema rejection makes the retry carry sanitized tools.
func TestAdaptiveLearnsSchemaQuirkAndSanitizes(t *testing.T) {
	inner := &stagedModel{errs: []error{schemaRefErr}}
	quirks := NewQuirkStore()
	a := NewAdaptiveModel(inner, AdaptiveConfig{ModelID: "glm", Quirks: quirks})

	tools := []*schema.ToolInfo{refSchemaToolInfo(t, "fs_read")}
	if _, err := a.Generate(context.Background(),
		[]*schema.Message{schema.UserMessage("hi")}, model.WithTools(tools)); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if inner.callCount() != 2 {
		t.Fatalf("inner called %d times, want 2", inner.callCount())
	}
	first := toolSchemaOf(t, inner.seenOpts[0])
	if _, has := first["$defs"]; !has {
		t.Error("the FIRST attempt was already sanitized; nothing could have been learned from it")
	}
	second := toolSchemaOf(t, inner.seenOpts[1])
	if _, has := second["$defs"]; has {
		t.Error("the retry still carried $defs")
	}
	props, _ := second["properties"].(map[string]any)
	p, _ := props["p"].(map[string]any)
	if p["type"] != "string" {
		t.Errorf("p.type = %v, want string (ref inlined and null flattened)", p["type"])
	}
	if !quirks.Has("glm", QuirkRejectsToolSchemaRefs) {
		t.Error("the schema quirk was not learned")
	}
}

// TestAdaptiveSanitizeAlwaysAndNever pins the two non-default policies: always
// pays no failed request to learn, never refuses to sanitize even after a
// rejection.
func TestAdaptiveSanitizeAlwaysAndNever(t *testing.T) {
	t.Run("always sanitizes the first request", func(t *testing.T) {
		inner := &stagedModel{}
		a := NewAdaptiveModel(inner, AdaptiveConfig{ModelID: "m", Sanitize: SanitizeAlways})
		tools := []*schema.ToolInfo{refSchemaToolInfo(t, "fs_read")}
		if _, err := a.Generate(context.Background(),
			[]*schema.Message{schema.UserMessage("hi")}, model.WithTools(tools)); err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if _, has := toolSchemaOf(t, inner.seenOpts[0])["$defs"]; has {
			t.Error("SanitizeAlways did not sanitize the first request")
		}
	})

	t.Run("never leaves the schema alone even after a rejection", func(t *testing.T) {
		inner := &stagedModel{errs: []error{schemaRefErr}}
		quirks := NewQuirkStore()
		a := NewAdaptiveModel(inner, AdaptiveConfig{
			ModelID: "m", Quirks: quirks, Sanitize: SanitizeNever,
		})
		tools := []*schema.ToolInfo{refSchemaToolInfo(t, "fs_read")}
		if _, err := a.Generate(context.Background(),
			[]*schema.Message{schema.UserMessage("hi")}, model.WithTools(tools)); err == nil {
			t.Fatal("Generate succeeded; SanitizeNever must let the rejection stand")
		}
		if inner.callCount() != 1 {
			t.Errorf("inner called %d times, want 1", inner.callCount())
		}
	})
}

// TestNormalizeSanitizeMode pins that an unrecognised operator value falls back
// rather than erroring: a typo in a portability aid must not stop a yanshi that
// would otherwise run.
func TestNormalizeSanitizeMode(t *testing.T) {
	cases := map[string]SanitizeMode{
		"":         SanitizeAuto,
		"auto":     SanitizeAuto,
		"always":   SanitizeAlways,
		"never":    SanitizeNever,
		"ALWAYS":   SanitizeAuto, // case-sensitive by design; unknown falls back
		"nonsense": SanitizeAuto,
	}
	for in, want := range cases {
		if got := NormalizeSanitizeMode(in); got != want {
			t.Errorf("NormalizeSanitizeMode(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAdaptiveLearnsSystemRoleQuirk covers the third quirk end to end.
func TestAdaptiveLearnsSystemRoleQuirk(t *testing.T) {
	inner := &stagedModel{errs: []error{systemRoleErr}}
	quirks := NewQuirkStore()
	a := NewAdaptiveModel(inner, AdaptiveConfig{ModelID: "local", Quirks: quirks})

	msgs := []*schema.Message{schema.SystemMessage("rules"), schema.UserMessage("hi")}
	if _, err := a.Generate(context.Background(), msgs); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	retry := inner.call(1)
	for _, m := range retry {
		if m.Role == schema.System {
			t.Fatal("the retry still carried a system message")
		}
	}
	if retry[0].Role != schema.User || retry[0].Content != "rules" {
		t.Errorf("retry[0] = %+v, want the system text as a leading user message", retry[0])
	}
	if !quirks.Has("local", QuirkRejectsSystemRole) {
		t.Error("the system-role quirk was not learned")
	}
}

// TestAdaptiveSharedRetryBudget pins that the two recovery mechanisms share ONE
// attempt: two independent budgets would multiply, and both features exist so
// that a failing turn does not become a metered loop.
func TestAdaptiveSharedRetryBudget(t *testing.T) {
	inner := &stagedModel{errs: []error{overflowErr, reasoningRequiredErr, nil}}
	summarizer := NewFakeModel([]string{"summary"}, nil)
	summarizer.Repeat = true
	quirks := NewQuirkStore()
	a := NewAdaptiveModel(inner, AdaptiveConfig{
		ModelID:  "m",
		Quirks:   quirks,
		Overflow: OverflowRecoveryConfig{ContextWindow: 4096, Summarizer: summarizer},
	})
	_, err := a.Generate(context.Background(), longHistory(12))
	if err == nil {
		t.Fatal("Generate succeeded; the second attempt's error must surface")
	}
	if inner.callCount() != 2 {
		t.Errorf("inner called %d times, want exactly 2 across both mechanisms", inner.callCount())
	}
	if quirks.Has("m", QuirkNeedsReasoningContent) {
		t.Error("a quirk was learned from an attempt that never succeeded")
	}
}

// TestAdaptiveNoQuirkStoreDisablesLearning pins that M5 is fully optional.
func TestAdaptiveNoQuirkStoreDisablesLearning(t *testing.T) {
	inner := &stagedModel{errs: []error{reasoningRequiredErr}}
	a := NewAdaptiveModel(inner, AdaptiveConfig{ModelID: "m"})
	if _, err := a.Generate(context.Background(),
		[]*schema.Message{schema.UserMessage("hi"), schema.AssistantMessage("x", nil)}); err == nil {
		t.Fatal("Generate succeeded; with no quirk store there is no repair")
	}
	if inner.callCount() != 1 {
		t.Errorf("inner called %d times, want 1", inner.callCount())
	}
}

// TestAdaptivePassesThroughOnSuccess pins that the zero-config wrapper is
// transparent — which is what makes it safe to install unconditionally.
func TestAdaptivePassesThroughOnSuccess(t *testing.T) {
	want := schema.AssistantMessage("the answer", nil)
	inner := &stagedModel{reply: want}
	a := NewAdaptiveModel(inner, AdaptiveConfig{})
	got, err := a.Generate(context.Background(), []*schema.Message{schema.UserMessage("q")})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got != want {
		t.Errorf("Generate returned %v, want the inner model's message", got)
	}
	sr, err := a.Stream(context.Background(), []*schema.Message{schema.UserMessage("q")})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	msgs := drain(t, sr)
	if len(msgs) != 1 || msgs[0] != want {
		t.Errorf("Stream delivered %v, want the inner model's message", msgs)
	}
}

// TestNewAdaptiveModelNilInner pins that a missing inner model is not silently
// papered over into a wrapper that answers nothing.
func TestNewAdaptiveModelNilInner(t *testing.T) {
	if a := NewAdaptiveModel(nil, AdaptiveConfig{}); a != nil {
		t.Error("NewAdaptiveModel(nil) returned a wrapper")
	}
}

// TestAdaptiveRateLimitRunsBeforeTheRequest pins M7's placement: the throttle
// must gate the call, and a cancelled context must not send anything.
func TestAdaptiveRateLimitRunsBeforeTheRequest(t *testing.T) {
	inner := &stagedModel{}
	limiter := NewRateLimiter(RateLimitConfig{QPM: 1, Burst: 1}, nil)
	a := NewAdaptiveModel(inner, AdaptiveConfig{ModelID: "m", Limiter: limiter})

	if _, err := a.Generate(context.Background(), []*schema.Message{schema.UserMessage("q")}); err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Generate(ctx, []*schema.Message{schema.UserMessage("q")}); err == nil {
		t.Fatal("Generate succeeded on a cancelled, throttled call")
	}
	if inner.callCount() != 1 {
		t.Errorf("inner called %d times, want 1 — the throttled call must not reach the provider",
			inner.callCount())
	}
}

// TestAdaptiveRecordsUsage is the M10 assertion for the non-streaming path.
func TestAdaptiveRecordsUsage(t *testing.T) {
	reply := schema.AssistantMessage("hi", nil)
	reply.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{
		PromptTokens:     100,
		CompletionTokens: 20,
		TotalTokens:      120,
	}}
	reply.ResponseMeta.Usage.PromptTokenDetails.CachedTokens = 40
	reply.ResponseMeta.Usage.CompletionTokensDetails.ReasoningTokens = 5

	inner := &stagedModel{reply: reply}
	sink := &MemoryUsageSink{}
	a := NewAdaptiveModel(inner, AdaptiveConfig{ModelID: "m", Provider: "openai", UsageSink: sink})

	ctx := obslog.WithIDs(context.Background(), obslog.IDs{SessionID: "sess-1"})
	if _, err := a.Generate(ctx, []*schema.Message{schema.UserMessage("q")}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	recs := sink.Records()
	if len(recs) != 1 {
		t.Fatalf("got %d usage records, want 1", len(recs))
	}
	r := recs[0]
	if r.Model != "m" || r.Provider != "openai" || r.SessionID != "sess-1" {
		t.Errorf("attribution wrong: %+v", r)
	}
	if r.PromptTokens != 100 || r.CompletionTokens != 20 || r.CachedTokens != 40 || r.ReasoningTokens != 5 {
		t.Errorf("token counts wrong: %+v", r)
	}
	if !r.CacheHit {
		t.Error("CacheHit = false despite 40 cached tokens")
	}
	if r.TS.IsZero() {
		t.Error("TS was not filled in")
	}
}

// TestAdaptiveRecordsStreamUsageOnce pins that a stream writes ONE row from the
// last delta that carried usage — summing would multiply one call's tokens by
// the number of chunks that mentioned them.
func TestAdaptiveRecordsStreamUsageOnce(t *testing.T) {
	mk := func(text string, prompt, completion int) *schema.Message {
		m := schema.AssistantMessage(text, nil)
		if prompt > 0 || completion > 0 {
			m.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{
				PromptTokens: prompt, CompletionTokens: completion,
			}}
		}
		return m
	}
	chunks := []*schema.Message{mk("a", 0, 0), mk("b", 100, 5), mk("c", 100, 20)}
	inner := &chunkModel{chunks: chunks}
	sink := &MemoryUsageSink{}
	a := NewAdaptiveModel(inner, AdaptiveConfig{ModelID: "m", UsageSink: sink})

	sr, err := a.Stream(context.Background(), []*schema.Message{schema.UserMessage("q")})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := drain(t, sr)
	if len(got) != len(chunks) {
		t.Fatalf("stream delivered %d messages, want %d", len(got), len(chunks))
	}
	recs := sink.Records()
	if len(recs) != 1 {
		t.Fatalf("got %d usage records for one stream, want 1", len(recs))
	}
	if recs[0].CompletionTokens != 20 {
		t.Errorf("CompletionTokens = %d, want the LAST reported value 20",
			recs[0].CompletionTokens)
	}
}

// chunkModel streams a fixed sequence of messages.
type chunkModel struct{ chunks []*schema.Message }

func (m *chunkModel) Generate(context.Context, []*schema.Message, ...model.Option) (
	*schema.Message, error) {
	return m.chunks[len(m.chunks)-1], nil
}

func (m *chunkModel) Stream(context.Context, []*schema.Message, ...model.Option) (
	*schema.StreamReader[*schema.Message], error) {
	sr, sw := schema.Pipe[*schema.Message](1)
	go func() {
		defer sw.Close()
		for _, c := range m.chunks {
			if sw.Send(c, nil) {
				return
			}
		}
	}()
	return sr, nil
}

// TestUsageFromMessage pins the projection, including the messages that carry
// no usage (the normal case for a streaming delta).
func TestUsageFromMessage(t *testing.T) {
	if _, ok := usageFromMessage(nil); ok {
		t.Error("nil message reported usage")
	}
	if _, ok := usageFromMessage(schema.AssistantMessage("x", nil)); ok {
		t.Error("a message with no ResponseMeta reported usage")
	}
	m := schema.AssistantMessage("x", nil)
	m.ResponseMeta = &schema.ResponseMeta{}
	if _, ok := usageFromMessage(m); ok {
		t.Error("a ResponseMeta with no Usage reported usage")
	}
	m.ResponseMeta.Usage = &schema.TokenUsage{PromptTokens: 7, CompletionTokens: 3}
	rec, ok := usageFromMessage(m)
	if !ok {
		t.Fatal("usage was not projected")
	}
	if rec.PromptTokens != 7 || rec.CompletionTokens != 3 {
		t.Errorf("rec = %+v", rec)
	}
	if rec.CacheHit {
		t.Error("CacheHit = true with no cached tokens")
	}
}

// TestUsageSessionComesFromTheCorrelationIDs pins that attribution reads the
// id the transports ALREADY bind, rather than a second key nothing would set.
// The negative half is equally load-bearing: a context with no correlation ids
// yields an unattributed row, not a panic and not a wrong session.
func TestUsageSessionComesFromTheCorrelationIDs(t *testing.T) {
	base := context.Background()
	if got := usageSessionFrom(base); got != "" {
		t.Errorf("usageSessionFrom on a bare context = %q, want empty", got)
	}
	ctx := obslog.WithIDs(base, obslog.IDs{TraceID: "t", SessionID: "s1", TurnID: "u"})
	if got := usageSessionFrom(ctx); got != "s1" {
		t.Errorf("usageSessionFrom = %q, want s1", got)
	}
	// WithoutIDs suppresses correlation, so the row is written unattributed.
	suppressed := obslog.WithIDs(obslog.WithoutIDs(base), obslog.IDs{SessionID: "s1"})
	if got := usageSessionFrom(suppressed); got != "" {
		t.Errorf("usageSessionFrom under WithoutIDs = %q, want empty", got)
	}
}

// TestRecordUsageNilSinkIsSafe pins that M10 is fully optional.
func TestRecordUsageNilSinkIsSafe(t *testing.T) {
	recordUsage(context.Background(), nil, UsageRecord{Model: "m"})
}

// failingSink returns an error from every write, to prove accounting never
// fails a turn.
type failingSink struct{ calls int }

func (f *failingSink) RecordUsage(context.Context, UsageRecord) error {
	f.calls++
	return errors.New("disk full")
}

// TestUsageSinkErrorDoesNotFailTheTurn pins that a full disk cannot
// retroactively fail a call that already produced an answer.
func TestUsageSinkErrorDoesNotFailTheTurn(t *testing.T) {
	reply := schema.AssistantMessage("hi", nil)
	reply.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 1}}
	sink := &failingSink{}
	a := NewAdaptiveModel(&stagedModel{reply: reply}, AdaptiveConfig{ModelID: "m", UsageSink: sink})
	if _, err := a.Generate(context.Background(), []*schema.Message{schema.UserMessage("q")}); err != nil {
		t.Fatalf("Generate failed because the usage sink did: %v", err)
	}
	if sink.calls != 1 {
		t.Errorf("sink called %d times, want 1", sink.calls)
	}
}
