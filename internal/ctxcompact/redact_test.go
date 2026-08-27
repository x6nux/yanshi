// internal/ctxcompact/redact_test.go
package ctxcompact

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/secrets"
)

// TestRedactorContractIsSatisfiedBySecretsPackage is the compile-time proof
// behind the Redactor interface's doc claim. ctxcompact deliberately declares
// its own one-method interface instead of naming *secrets.Redactor, and the
// only thing keeping that honest is an assertion that the real type still fits:
// if secrets.Redactor's Redact signature changes, this fails here rather than
// at the bootstrap wiring site.
func TestRedactorContractIsSatisfiedBySecretsPackage(t *testing.T) {
	var _ Redactor = secrets.NewRedactor()
}

// newTestRedactor returns a real *secrets.Redactor with the given secrets
// registered. It is deliberately the PRODUCTION type rather than a stub: the
// interesting behaviours here (MinSecretLength dropping short values,
// longest-first replacement) live in that type, and a stub that replaced
// substrings unconditionally would let this file assert redaction the shipped
// registry does not actually perform.
func newTestRedactor(vals ...string) *secrets.Redactor {
	r := secrets.NewRedactor()
	for _, v := range vals {
		r.Register(v)
	}
	return r
}

const redactedMarker = "[REDACTED]"

// TestRedactForSummary_CoversEveryTextBearingField walks each field that can
// carry a credential into the summary model and asserts the redacted copy no
// longer contains it.
//
// Table-driven over FIELDS rather than over inputs because the failure mode
// this guards is a field nobody thought of: the leak is not "the regex was
// wrong", it is "ToolCalls[].Function.Arguments was never passed through the
// redactor at all". Each case names one field and proves that field is reached.
func TestRedactForSummary_CoversEveryTextBearingField(t *testing.T) {
	const secret = "sk-live-abcdef123456"
	cases := []struct {
		name string
		msg  *schema.Message
		// get extracts the field under test from a redacted message.
		get func(*schema.Message) string
	}{
		{
			name: "Content",
			msg:  &schema.Message{Role: schema.Tool, ToolCallID: "c1", Content: "export TOKEN=" + secret},
			get:  func(m *schema.Message) string { return m.Content },
		},
		{
			name: "ReasoningContent",
			msg:  &schema.Message{Role: schema.Assistant, ReasoningContent: "the key is " + secret},
			get:  func(m *schema.Message) string { return m.ReasoningContent },
		},
		{
			name: "Name",
			msg:  &schema.Message{Role: schema.User, Name: secret},
			get:  func(m *schema.Message) string { return m.Name },
		},
		{
			name: "ToolName",
			msg:  &schema.Message{Role: schema.Tool, ToolCallID: "c1", ToolName: secret},
			get:  func(m *schema.Message) string { return m.ToolName },
		},
		{
			// The field a credential most often actually rides in: the model
			// echoes a curl command with an Authorization header into the
			// arguments of a shell_run call.
			name: "ToolCalls.Function.Arguments",
			msg: &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
				ID:       "c1",
				Function: schema.FunctionCall{Name: "shell_run", Arguments: `{"command":"curl -H 'Authorization: Bearer ` + secret + `'"}`},
			}}},
			get: func(m *schema.Message) string { return m.ToolCalls[0].Function.Arguments },
		},
		{
			name: "ToolCalls.Function.Name",
			msg: &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
				ID: "c1", Function: schema.FunctionCall{Name: secret},
			}}},
			get: func(m *schema.Message) string { return m.ToolCalls[0].Function.Name },
		},
		{
			name: "MultiContent.Text",
			msg: &schema.Message{Role: schema.User, MultiContent: []schema.ChatMessagePart{
				{Type: schema.ChatMessagePartTypeText, Text: "creds: " + secret},
			}},
			get: func(m *schema.Message) string { return m.MultiContent[0].Text },
		},
		{
			name: "UserInputMultiContent.Text",
			msg: &schema.Message{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
				{Type: schema.ChatMessagePartTypeText, Text: "creds: " + secret},
			}},
			get: func(m *schema.Message) string { return m.UserInputMultiContent[0].Text },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := []*schema.Message{tc.msg}
			require.Contains(t, tc.get(tc.msg), secret,
				"fixture must actually contain the secret, or this case proves nothing")

			out := redactForSummary(newTestRedactor(secret), in)
			require.Len(t, out, 1)
			got := tc.get(out[0])

			assert.NotContains(t, got, secret,
				"%s reaches the summary model unredacted: a secret in this field is folded "+
					"into the summary and then re-sent on every later turn", tc.name)
			assert.Contains(t, got, redactedMarker, "the secret should be replaced, not merely dropped")
		})
	}
}

// TestRedactForSummary_DoesNotMutateTheCaller is the C11 boundary, asserted
// from the side that matters most.
//
// The pinned messages are live conversation the model is still working from.
// Rewriting a token inside one to "[REDACTED]" would silently alter history
// mid-task. The redaction must therefore be visible ONLY in the returned slice.
// Both halves are checked: the caller's messages are untouched, AND the copy
// really was redacted — without the second half a redactForSummary that did
// nothing at all would pass.
func TestRedactForSummary_DoesNotMutateTheCaller(t *testing.T) {
	const secret = "ghp_secretsecret1234"
	original := &schema.Message{
		Role:    schema.Tool,
		Content: "token=" + secret,
		ToolCalls: []schema.ToolCall{{
			ID: "c1", Function: schema.FunctionCall{Name: "fs_read", Arguments: `{"k":"` + secret + `"}`},
		}},
	}
	in := []*schema.Message{original}

	out := redactForSummary(newTestRedactor(secret), in)

	assert.Equal(t, "token="+secret, original.Content,
		"the caller's message was mutated: pinned history must survive compaction verbatim")
	assert.Contains(t, original.ToolCalls[0].Function.Arguments, secret,
		"the caller's tool call arguments were mutated in place")
	assert.NotSame(t, in[0], out[0], "a redacted message must be a copy, not the original pointer")
	assert.NotContains(t, out[0].Content, secret, "the copy handed to the summarizer was not redacted")
}

// TestRedactForSummary_PreservesToolCallIdentity pins the documented exemption.
//
// Ids are what splitIsSafe and EnforceToolCallPairs pair on. Redacting one half
// of a pair and not the other severs it, and a severed pair is a provider 400 —
// so a redactor registered with a value that happens to be a substring of an id
// must leave ids alone. The fixture registers the id itself, which is the
// strongest form of the case.
func TestRedactForSummary_PreservesToolCallIdentity(t *testing.T) {
	const id = "call_abc123xyz"
	in := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			ID: id, Function: schema.FunctionCall{Name: "fs_read", Arguments: "{}"},
		}}},
		{Role: schema.Tool, ToolCallID: id, Content: "file contents"},
	}

	out := redactForSummary(newTestRedactor(id), in)

	assert.Equal(t, id, out[0].ToolCalls[0].ID, "tool_call id must survive redaction")
	assert.Equal(t, id, out[1].ToolCallID, "tool_result id must survive redaction")
	// splitIsSafe reports whether a cut here is SAFE, so the pair being intact
	// means it must answer false: the call is on the left and its result on the
	// right, which is exactly the cut takeChunk has to refuse. If redaction had
	// renamed either half, the two ids would no longer match, splitIsSafe would
	// see no pair to protect, and takeChunk would happily sever them — which is
	// the provider 400 this exemption exists to avoid.
	assert.False(t, splitIsSafe(out, 1),
		"redaction broke the id linkage: the pair is no longer recognised as one, "+
			"so takeChunk may sever it and the provider answers 400")
}

// TestRedactForSummary_EdgeCases covers the shapes that must not panic or
// allocate needlessly.
func TestRedactForSummary_EdgeCases(t *testing.T) {
	msgs := []*schema.Message{{Role: schema.User, Content: "nothing secret here"}}

	t.Run("nil redactor returns the input slice untouched", func(t *testing.T) {
		out := redactForSummary(nil, msgs)
		assert.Equal(t, msgs, out)
	})

	t.Run("no match keeps the original pointers", func(t *testing.T) {
		// Identity, not just equality: an allocation per message on every
		// compaction for a registry that matched nothing would be pure waste,
		// and the identity check is the only way to observe it.
		out := redactForSummary(newTestRedactor("unrelated-secret-value"), msgs)
		require.Len(t, out, 1)
		assert.Same(t, msgs[0], out[0])
	})

	t.Run("nil messages are preserved positionally", func(t *testing.T) {
		withNil := []*schema.Message{nil, {Role: schema.User, Content: "x"}, nil}
		out := redactForSummary(newTestRedactor("secret-value-here"), withNil)
		require.Len(t, out, 3, "positions must be stable: callers index by position")
		assert.Nil(t, out[0])
		assert.Nil(t, out[2])
	})

	t.Run("empty history", func(t *testing.T) {
		assert.Empty(t, redactForSummary(newTestRedactor("secret-value-here"), nil))
	})
}

// TestRunSummary_RedactsBeforeReachingTheModel is the end-to-end C11
// assertion, and the one that would have caught the original defect: it
// inspects what the summarizer was ACTUALLY handed.
//
// Asserting on RunSummary's return value alone would not do it — a summarizer
// that never echoes its input produces a clean summary whether or not the input
// leaked. The recorded call inputs are the evidence.
func TestRunSummary_RedactsBeforeReachingTheModel(t *testing.T) {
	const secret = "sk-proj-DEADBEEF1234"
	msgs := []*schema.Message{
		{Role: schema.User, Content: "print the env"},
		{Role: schema.Tool, ToolCallID: "c1", Content: "OPENAI_API_KEY=" + secret},
	}
	rs := &recordingSummarizer{Return: "The session read the environment and found an API key."}

	_, err := RunSummary(context.Background(), msgs, RunOpts{
		ModelWindow: 10000, ChunkThreshold: 0.9, Redactor: newTestRedactor(secret),
	}, rs, nil)
	require.NoError(t, err)

	calls := append(rs.GenerateCalls, rs.StreamCalls...)
	require.NotEmpty(t, calls, "the summarizer must have been called for this to prove anything")
	for i, call := range calls {
		for j, m := range call {
			require.NotNil(t, m)
			assert.NotContains(t, m.Content, secret,
				"call[%d] msg[%d] carried the secret to the summary model", i, j)
		}
	}
}

// TestRunSummary_RedactsTheSummaryItself covers the defence-in-depth second
// pass: a model that echoes a secret back (because it saw it in a pinned tail,
// or reconstructed it) must not have that echo become a PINNED summary message,
// which is the one artefact in this package with unbounded lifetime.
func TestRunSummary_RedactsTheSummaryItself(t *testing.T) {
	const secret = "sk-echo-9876543210"
	rs := &recordingSummarizer{Return: "The key was " + secret + " and the build then succeeded."}

	summary, err := RunSummary(context.Background(),
		[]*schema.Message{{Role: schema.User, Content: "do it"}},
		RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9, Redactor: newTestRedactor(secret)}, rs, nil)
	require.NoError(t, err)

	assert.NotContains(t, summary, secret,
		"a secret echoed by the summarizer becomes a pinned message re-sent every turn")
	assert.Contains(t, summary, redactedMarker)
}

// TestRun_RedactionDoesNotReachPinnedMessages is the integration statement of
// the boundary: after a full Run, the PINNED messages in the output still carry
// their original text, while the summary was produced from redacted input.
//
// This is the property a future refactor is most likely to break — moving the
// redaction from RunSummary up into Run's message collection would redact the
// slice Assemble re-emits, and every assertion in this file except this one
// would still pass.
func TestRun_RedactionDoesNotReachPinnedMessages(t *testing.T) {
	const secret = "sk-pinned-112233445566"
	// The user message is pinned by policy 2 (user intent) and carries the
	// secret; the long assistant messages are what actually gets summarized.
	msgs := []*schema.Message{
		{Role: schema.User, Content: "deploy with key " + secret},
		{Role: schema.Assistant, Content: strings.Repeat("build output ", 100)},
		{Role: schema.Assistant, Content: strings.Repeat("more output ", 100)},
		{Role: schema.Assistant, Content: strings.Repeat("still going ", 100)},
		{Role: schema.User, Content: "status?"},
		{Role: schema.Assistant, Content: "done"},
	}
	rs := &recordingSummarizer{
		Return: "The assistant produced build output across three turns and reported completion.",
	}

	res, err := Run(context.Background(), msgs, PlanOpts{KeepRecent: 1},
		RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9, Redactor: newTestRedactor(secret)}, rs, nil)
	require.NoError(t, err)

	var sawPinnedSecret bool
	for _, m := range res.Messages {
		if strings.Contains(m.Content, secret) {
			sawPinnedSecret = true
		}
	}
	assert.True(t, sawPinnedSecret,
		"the pinned user message lost its original text: compaction must not rewrite live history")
}
