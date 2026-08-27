package http

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/skills"
)

func TestChat_SSE(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"hello world"}, nil)})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.Chat(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/chat", strings.NewReader(`{"message":"hi"}`))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "hello world")
}

// TestChat_SSE_MultiLine verifies that multi-line content emitted through the
// SSE handler can be reconstructed by the client by concatenating data: payloads.
// Each line of content must be its own data: line per the SSE spec.
func TestChat_SSE_MultiLine(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"line1\nline2\nline3"}, nil)})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.Chat(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/chat", strings.NewReader(`{"message":"hi"}`))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// Parse structured SSE data payloads and extract agent_chunk text. Each
	// frame is one JSON "data:" line whose text may itself contain newlines.
	var texts []string
	for _, line := range strings.Split(bodyStr, "\n") {
		const pfx = "data: "
		if d, ok := strings.CutPrefix(line, pfx); ok {
			var f struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal([]byte(d), &f) == nil && f.Type == "agent_chunk" {
				texts = append(texts, f.Text)
			}
		}
	}
	assert.Equal(t, []string{"line1\nline2\nline3"}, texts)
}

// TestChat_SSE_ErrorEvent verifies that when the model returns an error, the
// SSE response includes an "event: error" line before the data lines, so the
// CLI can route errors to the error branch instead of stdout.
func TestChat_SSE_ErrorEvent(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel(nil, errors.New("model exploded"))})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.Chat(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/chat", strings.NewReader(`{"message":"hi"}`))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// The SSE body must contain "event: error" so the CLI's case "error" branch fires.
	assert.Contains(t, bodyStr, "event: error\n")

	// The error message should appear in a data: line (the orchestrator
	// wraps it, so check for the core message fragment).
	assert.Contains(t, bodyStr, "model exploded")
	assert.True(t, strings.Contains(bodyStr, "data: "), "body should contain data: lines")
}

// writeSkillFile writes a SKILL.md with frontmatter under <root>/<name>/ and
// returns the registry built from that root.
func writeSkillFile(t *testing.T, root, name, content string) *skills.Registry {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644))
	reg, err := skills.NewLoader(skills.Builtin(root)).Load()
	require.NoError(t, err)
	return reg
}

// TestChat_SkillPrefix_Unknown verifies that "/skill <unknown>" produces an SSE
// error event that lists the available skills, so the CLI routes it to its
// error branch instead of silently running the model.
func TestChat_SkillPrefix_Unknown(t *testing.T) {
	reg := writeSkillFile(t, t.TempDir(), "hi",
		"---\nname: hi\ndescription: greeting skill\n---\n# Hi\nSay hi.")

	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"should-not-run"}, nil)})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.Chat(o, nil, reg)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/chat", strings.NewReader(`{"message":"/skill nope do thing"}`))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	assert.Contains(t, bodyStr, "event: error\n")
	assert.Contains(t, bodyStr, "unknown skill")
	assert.Contains(t, bodyStr, "nope")
	// The available skill "hi" must be listed so the user knows what they can use.
	assert.Contains(t, bodyStr, "hi")
	// The model must NOT have run for an unknown skill.
	assert.NotContains(t, bodyStr, "should-not-run")
}

// TestChat_SkillPrefix_Known verifies that "/skill <name> <task>" loads the
// skill body and injects it into the query. It proves injection by using an
// Echo FakeModel that returns the last input message (the user query) as its
// response, so the injected skill body and task must appear in the SSE body.
func TestChat_SkillPrefix_Known(t *testing.T) {
	reg := writeSkillFile(t, t.TempDir(), "hi",
		"---\nname: hi\ndescription: greeting skill\n---\n# Hi\nSay hi.")

	fm := einollm.NewFakeModel(nil, nil)
	fm.Echo = true
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.Chat(o, nil, reg)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/chat", strings.NewReader(`{"message":"/skill hi build a greeter"}`))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// The skill body must have been injected into the query the model saw.
	assert.Contains(t, bodyStr, "Say hi.")
	// The trailing task must have been appended.
	assert.Contains(t, bodyStr, "Task: build a greeter")
}

// TestChat_SkillPrefix_NilRegistry verifies that "/skill" against a server
// with no registry (the legacy/nil path used by non-skill tests) produces an
// SSE error rather than panicking.
func TestChat_SkillPrefix_NilRegistry(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"should-not-run"}, nil)})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.Chat(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/chat", strings.NewReader(`{"message":"/skill hi anything"}`))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	assert.Contains(t, bodyStr, "event: error\n")
	assert.Contains(t, bodyStr, "no skill registry available")
	assert.NotContains(t, bodyStr, "should-not-run")
}

// TestChat_SSE_MultiTurnHistory proves the SSE handler honors a client-supplied
// messages[] history: an Echo model returns the concatenation of every input
// message, so the second turn's text must surface in the response (multi-turn).
func TestChat_SSE_MultiTurnHistory(t *testing.T) {
	fm := einollm.NewFakeModel(nil, nil)
	fm.Echo = true // echoes last input -> proves history was passed
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.Chat(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	body := `{"messages":[{"role":"user","content":"remember ZETA"},{"role":"assistant","content":"ok"},{"role":"user","content":"echo"}]}`
	resp, err := ts.Client().Post(ts.URL+"/api/v1/chat", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	assert.Contains(t, string(b), "event: agent_chunk")
	assert.Contains(t, string(b), "event: done")
	assert.Contains(t, string(b), "remember ZETA", "second turn must see first turn (multi-turn)")
}

// TestChat_SSE_AutoCompaction proves the SSE auto-compaction path (Task 35b):
// when the POSTed history exceeds the threshold, the handler streams
// compact_chunk events, then a history_replaced event (carrying the compacted
// slice) and a status{compacted} event, all before the turn's agent_chunk.
func TestChat_SSE_AutoCompaction(t *testing.T) {
	// First scripted response is the summary; second is the turn reply.
	fm := einollm.NewFakeModel([]string{"SSE-SUMMARY", "turn-reply"}, nil)
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)
	// ContextWindow must clear the summary INSTRUCTION's own cost, which the
	// C4 structured prompt made non-trivial: even the terse form is ~130
	// tokens, so the historical 100-token fixture left a negative chunk budget
	// and RunSummary refused with ErrNoWindowRoom — a correct refusal that
	// reported did=false and made this test read as "compaction is broken".
	// 2000 is the smallest round window where instruction + a real history
	// still fits the single cache-aligned call.
	s := New(Config{Token: "t", Compaction: CompactionConfig{Threshold: 0.5, ContextWindow: 2000, KeepRecent: 1}})
	// Register the fake model so compactionModel can pick it (the new
	// MaybeCompact signature takes a model directly, not the orchestrator).
	models := map[string]model.BaseChatModel{"fm": fm}
	s.Chat(o, models, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// Long assistant content is what compaction actually shrinks under the new
	// Plan: user messages are pinned verbatim (intent is never lost), so only
	// the assistant turns are folded into the summary. A long assistant turn
	// is larger than the small scripted summary, so TokensAfter < TokensBefore
	// and MaybeCompact returns did=true. It is sized against the 2000-token
	// window above: the history has to clear Threshold*window = 1000 tokens.
	long := strings.Repeat("b", 2200)
	body := `{"messages":[` +
		`{"role":"user","content":"task"},` +
		`{"role":"assistant","content":"` + long + `"},` +
		`{"role":"assistant","content":"` + long + `"},` +
		`{"role":"user","content":"final-turn"}]}`
	resp, err := ts.Client().Post(ts.URL+"/api/v1/chat", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	bodyStr := string(b)

	assert.Contains(t, bodyStr, "event: compact_chunk", "compaction streams compact_chunk")
	assert.Contains(t, bodyStr, "SSE-SUMMARY", "summary delta streamed")
	assert.Contains(t, bodyStr, "event: history_replaced", "history_replaced emitted for sseBackend")
	assert.Contains(t, bodyStr, `"compacted":true`, "status carries compacted=true")
	assert.Contains(t, bodyStr, "event: agent_chunk", "the turn still runs after compaction")
	assert.Contains(t, bodyStr, "turn-reply")

	// Ordering: compact_chunk before history_replaced before agent_chunk.
	require.Less(t, strings.Index(bodyStr, "event: compact_chunk"),
		strings.Index(bodyStr, "event: history_replaced"),
		"compact_chunk must stream before history_replaced")
	require.Less(t, strings.Index(bodyStr, "event: history_replaced"),
		strings.Index(bodyStr, "event: agent_chunk"),
		"history_replaced must precede the turn frames")
}

// TestChat_SSE_NoCompactionWhenDisabled proves that with compaction disabled
// (threshold 0), an over-long history passes through untouched: no
// compact_chunk / history_replaced events, just the normal turn stream.
func TestChat_SSE_NoCompactionWhenDisabled(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"reply"}, nil)
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)
	s := New(Config{Token: "t"}) // zero Compaction -> disabled
	s.Chat(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	long := strings.Repeat("c", 100)
	body := `{"messages":[` +
		`{"role":"user","content":"` + long + `"},` +
		`{"role":"assistant","content":"a1"},` +
		`{"role":"user","content":"` + long + `"},` +
		`{"role":"assistant","content":"a2"},` +
		`{"role":"user","content":"turn"}]}`
	resp, err := ts.Client().Post(ts.URL+"/api/v1/chat", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	bodyStr := string(b)

	assert.NotContains(t, bodyStr, "compact_chunk", "disabled -> no compaction")
	assert.NotContains(t, bodyStr, "history_replaced", "disabled -> no history_replaced")
	assert.Contains(t, bodyStr, "event: agent_chunk")
}

// --- A12-core: SSE per-turn structured output (schema validation + retry) ---
//
// The SSE handler mirrors the WS schema-validation path (see
// TestWSStructuredOutput*) but on the stateless SSE transport: when the POST
// body carries an "output_schema" the handler validates the model's final
// assistant text against it, retries with a reminder on failure (up to
// maxSchemaRetries), and emits a structured_result event before done. No
// output_schema ⇒ text mode, byte-identical to pre-A12 (regression guard).

// parseSSEEvents parses the SSE wire format emitted by writeSSEFrame into a
// sequence of (event, data) pairs. Used by the structured-output tests below
// to assert event ordering (structured_result MUST come before done) without
// scraping raw substrings. Each frame in the body is:
//
//	event: <name>\n
//	data: <json>\n
//	\n
func parseSSEEvents(body string) []struct {
	Event string
	Data  string
} {
	var out []struct {
		Event string
		Data  string
	}
	var cur struct {
		Event string
		Data  string
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			if cur.Event != "" || cur.Data != "" {
				out = append(out, cur)
			}
			cur.Event, cur.Data = "", ""
			continue
		}
		if e, ok := strings.CutPrefix(line, "event: "); ok {
			cur.Event = e
		} else if d, ok := strings.CutPrefix(line, "data: "); ok {
			cur.Data = d
		}
	}
	if cur.Event != "" || cur.Data != "" {
		out = append(out, cur)
	}
	return out
}

// TestChatStructuredOutputSuccess proves the SSE happy path: when the POST body
// declares an output_schema and the model's single attempt satisfies it, the
// handler emits a structured_result event carrying the validated JSON BEFORE
// done. No retry happens (the model is streamed exactly once).
func TestChatStructuredOutputSuccess(t *testing.T) {
	fm := einollm.NewFakeModel([]string{`{"name":"Ada","age":36}`}, nil)
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)

	s := New(Config{Token: "t"})
	s.Chat(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	body := `{"message":"list users","output_schema":` + string(personSchema) + `}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/chat", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	events := parseSSEEvents(string(b))
	var sawStructured bool
	var structuredJSON string
	var structuredIndex, doneIndex int = -1, -1
	for i, ev := range events {
		switch ev.Event {
		case "structured_result":
			sawStructured = true
			structuredIndex = i
			var f struct {
				StructuredResult json.RawMessage `json:"structured_result"`
			}
			require.NoError(t, json.Unmarshal([]byte(ev.Data), &f))
			structuredJSON = string(f.StructuredResult)
		case "done":
			doneIndex = i
		}
	}
	assert.True(t, sawStructured,
		"schema-constrained SSE turn must emit a structured_result event")
	assert.JSONEq(t, `{"name":"Ada","age":36}`, structuredJSON,
		"structured_result must carry the validated JSON")
	require.GreaterOrEqual(t, doneIndex, 0, "body must terminate with done")
	assert.Greater(t, doneIndex, structuredIndex,
		"structured_result MUST come before done")
	assert.Equal(t, 1, fm.StreamCalls,
		"valid first attempt must not trigger a retry")
}

// TestChatStructuredOutputRetryCapError proves the cap-exhaustion path on SSE:
// when the model NEVER emits schema-valid JSON, the handler exhausts
// maxSchemaRetries and emits an error event mentioning the schema; no
// structured_result is emitted. The stream still ends with done.
func TestChatStructuredOutputRetryCapError(t *testing.T) {
	// 4 invalid responses: maxSchemaRetries=3 → attempts 0,1,2,3.
	fm := einollm.NewFakeModel([]string{"bad1", "bad2", "bad3", "bad4"}, nil)
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)

	s := New(Config{Token: "t"})
	s.Chat(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	body := `{"message":"dump","output_schema":` + string(personSchema) + `}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/chat", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	events := parseSSEEvents(string(b))
	var sawStructured bool
	var lastErr string
	var sawDone bool
	for _, ev := range events {
		switch ev.Event {
		case "structured_result":
			sawStructured = true
		case "error":
			var f struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal([]byte(ev.Data), &f)
			lastErr = f.Text
		case "done":
			sawDone = true
		}
	}
	assert.NotEmpty(t, lastErr,
		"cap exhaustion must emit an error frame")
	assert.Contains(t, lastErr, "schema",
		"the cap-exhaustion error must mention the schema")
	assert.False(t, sawStructured,
		"no structured_result when validation never succeeded")
	assert.True(t, sawDone, "stream must still terminate with done")
	assert.Equal(t, 4, fm.StreamCalls,
		"must run exactly maxSchemaRetries+1 attempts before giving up")
}

// TestChatNoSchemaIsTextMode is the regression guard for SSE: a POST without
// output_schema must NEVER emit a structured_result event, and the turn must
// complete with a single model call. This proves the hasSchema gate keeps the
// text path byte-identical to pre-A12 behavior (the entire schema retry loop
// is skipped when len(req.OutputSchema) == 0).
func TestChatNoSchemaIsTextMode(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"hello"}, nil)
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)

	s := New(Config{Token: "t"})
	s.Chat(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// Plain text-mode POST: no output_schema key at all.
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/chat", strings.NewReader(`{"message":"hi"}`))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	events := parseSSEEvents(string(b))
	var sawStructured, sawDone bool
	var assistantText strings.Builder
	for _, ev := range events {
		switch ev.Event {
		case "agent_chunk":
			var f struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal([]byte(ev.Data), &f)
			assistantText.WriteString(f.Text)
		case "structured_result":
			sawStructured = true
		case "done":
			sawDone = true
		}
	}
	assert.False(t, sawStructured,
		"text-mode SSE turn must NOT emit structured_result (hasSchema gate)")
	assert.Equal(t, "hello", assistantText.String(),
		"text-mode turn must still stream the model's reply")
	assert.True(t, sawDone, "stream must terminate with done")
	assert.Equal(t, 1, fm.StreamCalls,
		"text-mode turn must run exactly one model call (no retry loop)")
}

// TestChat_OutputSchemaReachesModel proves the SSE chat handler fills
// TurnOpts.OutputSchema from the POST body's output_schema so the schema
// reaches the model end-to-end (transport -> orchestrator -> adapter). The
// model returns schema-valid JSON so A12-core's validation passes on the first
// attempt; the assertion is about option forwarding (RecordOpts +
// ReceivedOutputSchema), not retry behavior.
func TestChat_OutputSchemaReachesModel(t *testing.T) {
	fm := einollm.NewFakeModel([]string{`{"name":"Ada","age":36}`}, nil)
	fm.RecordOpts = true
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)

	s := New(Config{Token: "t"})
	s.Chat(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	body := `{"message":"list users","output_schema":` + string(personSchema) + `}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/chat", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	assert.JSONEq(t, string(personSchema), string(fm.ReceivedOutputSchema),
		"req.OutputSchema must reach the model via TurnOpts.OutputSchema -> option -> adapter")
}
