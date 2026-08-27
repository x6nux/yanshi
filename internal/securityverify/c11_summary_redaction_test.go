package securityverify

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/ctxcompact"
	"github.com/x6nux/yanshi/internal/secrets"
)

// c11_summary_redaction_test.go verifies summary-input redaction by RECORDING
// WHAT THE SUMMARY MODEL ACTUALLY RECEIVED.
//
// The leak C11 closes is specific and worth restating, because it is not the
// usual one: a shell tool prints an API key, the key rides into a tool_result,
// compaction feeds that result to the summary model, and the resulting summary
// is a PINNED message. From then on the key is re-sent to the provider on every
// single turn, forever, long after the tool_result it came from was compacted
// away. Redaction at the output boundaries (WS, SSE, logs, SQLite) cannot help:
// this path never crosses one of them — it goes straight back out as model
// input.
//
// So the assertion is on the summarizer's INPUT, captured through a recording
// model. Asserting on redactForSummary's return value would test the function;
// capturing the input tests the pipeline that calls it.
//
// The second, equally important assertion is that the PINNED originals are NOT
// redacted. Those messages are live conversation the model is still working
// from, and rewriting a token inside one to "[REDACTED]" would silently corrupt
// history mid-task. The two requirements pull in opposite directions, which is
// exactly why both are checked here rather than one of them.

// recordingSummarizer captures every message slice it is asked to summarize.
type recordingSummarizer struct {
	mu   sync.Mutex
	seen [][]*schema.Message
	out  string
}

// Stream satisfies model.BaseChatModel. Compaction calls Generate, so this
// exists only to satisfy the interface and returns a single-element stream.
func (r *recordingSummarizer) Stream(ctx context.Context, msgs []*schema.Message,
	opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := r.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

func (r *recordingSummarizer) Generate(_ context.Context, msgs []*schema.Message,
	_ ...model.Option) (*schema.Message, error) {
	r.mu.Lock()
	cp := make([]*schema.Message, len(msgs))
	copy(cp, msgs)
	r.seen = append(r.seen, cp)
	r.mu.Unlock()
	out := r.out
	if out == "" {
		out = "Summary: the agent configured a deployment and ran the test suite. " +
			"Outstanding work: verify the staging rollout."
	}
	return schema.AssistantMessage(out, nil), nil
}

// everythingSent flattens every message the summarizer ever saw into one string.
func (r *recordingSummarizer) everythingSent() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, batch := range r.seen {
		for _, m := range batch {
			if m == nil {
				continue
			}
			b.WriteString(m.Content)
			b.WriteString("\n")
			b.WriteString(m.ReasoningContent)
			b.WriteString("\n")
			for _, tc := range m.ToolCalls {
				b.WriteString(tc.Function.Name)
				b.WriteString(" ")
				b.WriteString(tc.Function.Arguments)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// leakyHistory builds a conversation long enough to force compaction, with a
// live credential inside a tool_result and inside a tool_call's arguments —
// the two places a secret actually shows up.
func leakyHistory(secret string) []*schema.Message {
	var msgs []*schema.Message
	filler := strings.Repeat("the deployment pipeline ran and produced ordinary log output. ", 60)
	for i := 0; i < 24; i++ {
		msgs = append(msgs, schema.UserMessage("step: "+filler))
		msgs = append(msgs, schema.AssistantMessage("acknowledged: "+filler, nil))
	}
	// The tool call whose ARGUMENTS carry the credential.
	msgs = append(msgs, schema.AssistantMessage("", []schema.ToolCall{{
		ID: "call-1", Type: "function", Function: schema.FunctionCall{
			Name:      "shell_run",
			Arguments: `{"command":"curl -H 'Authorization: Bearer ` + secret + `' https://api.example.com"}`,
		},
	}}))
	// The tool RESULT that printed it back.
	msgs = append(msgs, &schema.Message{
		Role: schema.Tool, ToolCallID: "call-1", ToolName: "shell_run",
		Content: "HTTP 200\nrequest used token " + secret + "\n" + filler,
	})
	for i := 0; i < 6; i++ {
		msgs = append(msgs, schema.UserMessage("continue: "+filler))
		msgs = append(msgs, schema.AssistantMessage("continuing: "+filler, nil))
	}
	return msgs
}

// TestC11_SecretNeverReachesTheSummaryModel is the primary measurement.
func TestC11_SecretNeverReachesTheSummaryModel(t *testing.T) {
	const secret = "sk-live-C11CANARY-0123456789abcdefghij"
	red := secrets.NewRedactor()
	red.Register(secret)

	rec := &recordingSummarizer{}
	msgs := leakyHistory(secret)

	out, before, after, ok := ctxcompact.ForceCompactWithOptions(
		context.Background(), msgs, 4000, 4, rec, nil,
		ctxcompact.Options{Redactor: red},
	)
	if !ok {
		t.Fatalf("compaction did not run (tokens %d -> %d); nothing was measured", before, after)
	}
	if len(rec.seen) == 0 {
		t.Fatal("the summarizer was never called; nothing was measured")
	}
	t.Logf("summarizer invoked %d time(s); history %d -> %d messages, tokens %d -> %d",
		len(rec.seen), len(msgs), len(out), before, after)

	sent := rec.everythingSent()
	if strings.Contains(sent, secret) {
		for _, line := range strings.Split(sent, "\n") {
			if strings.Contains(line, secret) {
				t.Errorf("LEAK to the summary model: %s", line)
			}
		}
		t.FailNow()
	}
	if !strings.Contains(sent, "[REDACTED]") {
		t.Error("no redaction marker in the summarizer's input — was the redactor consulted at all?")
	}
	t.Logf("summarizer input carried no secret (%d bytes inspected)", len(sent))
}

// TestC11_PinnedOriginalsAreNotRewritten is the other half, and the one a
// naive "redact everything" implementation would fail. The pinned messages are
// live conversation: the model is still working from them, and quietly
// replacing a token inside one with "[REDACTED]" corrupts the task in progress.
func TestC11_PinnedOriginalsAreNotRewritten(t *testing.T) {
	const secret = "sk-live-C11CANARY-0123456789abcdefghij"
	red := secrets.NewRedactor()
	red.Register(secret)

	rec := &recordingSummarizer{}
	msgs := leakyHistory(secret)

	// Capture the tail's identity BEFORE compaction so the comparison is
	// against what the caller actually held, not a re-derivation.
	before := make([]string, len(msgs))
	for i, m := range msgs {
		if m != nil {
			before[i] = m.Content
		}
	}

	if _, _, _, ok := ctxcompact.ForceCompactWithOptions(
		context.Background(), msgs, 4000, 4, rec, nil,
		ctxcompact.Options{Redactor: red},
	); !ok {
		t.Fatal("compaction did not run; nothing was measured")
	}

	var mutated int
	for i, m := range msgs {
		if m != nil && m.Content != before[i] {
			mutated++
			t.Errorf("caller's message %d was rewritten in place:\n  was: %.80s\n  now: %.80s",
				i, before[i], m.Content)
		}
	}
	if mutated == 0 {
		t.Logf("all %d caller messages left byte-identical; redaction operated on a copy", len(msgs))
	}
}

// TestC11_SummaryOutputIsAlsoRedacted covers the second pass. It is defence in
// depth rather than the primary control — the summarizer only ever saw redacted
// input — but a summary is the one artefact in this pipeline with unbounded
// lifetime, so a model that echoes something secret-shaped must not have it
// pinned forever.
func TestC11_SummaryOutputIsAlsoRedacted(t *testing.T) {
	const secret = "sk-live-C11CANARY-0123456789abcdefghij"
	red := secrets.NewRedactor()
	red.Register(secret)

	// A summarizer that emits the secret regardless of its input, standing in
	// for a model that reconstructed or hallucinated it.
	//
	// The body is a realistic multi-section summary rather than one line
	// because the C10 quality gate rejects a summary too short to be one, and
	// a rejected summary is discarded — which would make this test pass for
	// the wrong reason (nothing was pinned, so nothing leaked).
	rec := &recordingSummarizer{out: "## What happened\n" +
		"The agent configured the deployment pipeline, authenticated against the staging API " +
		"using token " + secret + ", and ran the full test suite against the staging environment.\n\n" +
		"## Decisions\n" +
		"Chose the blue-green rollout over rolling restarts because the health check needs a warm cache.\n\n" +
		"## Files touched\n" +
		"deploy/pipeline.yaml, deploy/staging.env, internal/api/health.go\n\n" +
		"## Outstanding work\n" +
		"Verify the staging rollout completes, then promote to production. The credential " + secret +
		" is still configured in the pipeline and must be rotated before promotion.\n"}
	msgs := leakyHistory(secret)

	out, _, _, ok := ctxcompact.ForceCompactWithOptions(
		context.Background(), msgs, 4000, 4, rec, nil,
		ctxcompact.Options{Redactor: red},
	)
	if !ok {
		t.Fatal("compaction did not run; nothing was measured")
	}
	for i, m := range out {
		if m == nil {
			continue
		}
		if strings.Contains(m.Content, secret) {
			t.Fatalf("the pinned summary (message %d) carries the secret and will be "+
				"re-sent to the provider every turn from now on:\n%s", i, m.Content)
		}
	}
	t.Log("a summary that echoed the secret was redacted before being pinned")
}

// TestC11_NilRedactorIsExactlyTheOldBehaviour pins the degradation path. An
// embedding with no secrets backend passes a nil redactor, and compaction must
// still work rather than fail closed — this is a confidentiality control, not
// an availability one, and refusing to compact would wedge the turn.
func TestC11_NilRedactorIsExactlyTheOldBehaviour(t *testing.T) {
	rec := &recordingSummarizer{}
	msgs := leakyHistory("sk-not-registered-anywhere")
	out, _, _, ok := ctxcompact.ForceCompactWithOptions(
		context.Background(), msgs, 4000, 4, rec, nil, ctxcompact.Options{},
	)
	if !ok {
		t.Fatal("compaction with no redactor must still run")
	}
	if len(out) == 0 {
		t.Fatal("compaction produced no messages")
	}
	t.Logf("no-redactor compaction succeeded: %d -> %d messages", len(msgs), len(out))
}
