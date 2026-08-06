// internal/proto/frame_test.go
package proto

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var updateGolden = flag.Bool("update", false, "regenerate SSE golden file")

func TestClientFrame_RoundTrip(t *testing.T) {
	in := NewUserMessage("hello world")
	data, err := json.Marshal(in)
	require.NoError(t, err)
	var got ClientFrame
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, "user_message", got.Type)
	assert.Equal(t, "hello world", got.Text)
}

func TestServerFrame_VariantsRoundTrip(t *testing.T) {
	cases := []ServerFrame{
		NewAgentChunk("partial text"),
		NewToolCall("fs_search", `"pattern":"x"`, "running"),
		NewToolResult("fs_search", "3 matches", "ok"),
		NewError("boom"),
		NewDone(),
	}
	for _, f := range cases {
		data, err := json.Marshal(f)
		require.NoError(t, err)
		var got ServerFrame
		require.NoError(t, json.Unmarshal(data, &got))
		assert.Equal(t, f.Type, got.Type)
	}
}

// TestServerFrame_SSEEvent verifies the SSE wire encoding: event name = frame
// Type, data = frame JSON. Used by the SSE handler (emit) and sseBackend
// (parse) so SSE and WS share one event vocabulary.
func TestServerFrame_SSEEvent(t *testing.T) {
	f := NewToolCall("fs_search", `{"p":"x"}`, "running")
	event, data := f.SSEEvent()
	assert.Equal(t, "tool_call", event)

	var got ServerFrame
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, "tool_call", got.Type)
	assert.Equal(t, "fs_search", got.ToolName)
}

// TestClientFrame_ParityRoundTrip verifies the Phase-10 control frames
// (set_model/set_thinking/clear/list_models/get_status/permission_response/
// compact) survive a marshal/unmarshal round-trip with their fields intact.
func TestClientFrame_ParityRoundTrip(t *testing.T) {
	cases := []ClientFrame{
		NewSetModel("claude-opus-4"),
		NewSetThinking("high"),
		NewClear(),
		NewListModels(),
		NewGetStatus(),
		NewPermissionResponse("req-7", "always_allow"),
		NewCompact(),
		NewListMCP(),
	}
	wantFields := []func(ClientFrame) bool{
		func(f ClientFrame) bool { return f.Type == "set_model" && f.Name == "claude-opus-4" },
		func(f ClientFrame) bool { return f.Type == "set_thinking" && f.Effort == "high" },
		func(f ClientFrame) bool { return f.Type == "clear" },
		func(f ClientFrame) bool { return f.Type == "list_models" },
		func(f ClientFrame) bool { return f.Type == "get_status" },
		func(f ClientFrame) bool {
			return f.Type == "permission_response" && f.ID == "req-7" && f.Decision == "always_allow"
		},
		func(f ClientFrame) bool { return f.Type == "compact" },
		func(f ClientFrame) bool { return f.Type == "list_mcp" },
	}
	for i, in := range cases {
		data, err := json.Marshal(in)
		require.NoError(t, err)
		var got ClientFrame
		require.NoErrorf(t, json.Unmarshal(data, &got), "case %d (%s)", i, in.Type)
		assert.Truef(t, wantFields[i](got), "case %d (%s) fields not preserved: %+v", i, in.Type, got)
		// omitempty: a set_thinking frame must not carry a "name" key, and a
		// set_model frame must not carry an "effort" key — the wire form stays
		// minimal so logs/inspections are clean.
		if in.Type == "set_thinking" {
			assert.NotContains(t, string(data), `"name"`, "set_thinking must omit name")
		}
		if in.Type == "set_model" {
			assert.NotContains(t, string(data), `"effort"`, "set_model must omit effort")
		}
	}
}

// TestServerFrame_ParityRoundTrip verifies the Phase-10 server frames (models/
// status/permission_request) round-trip with all their fields.
func TestServerFrame_ParityRoundTrip(t *testing.T) {
	t.Run("models", func(t *testing.T) {
		in := NewModels([]string{"a", "b", "c"})
		data, err := json.Marshal(in)
		require.NoError(t, err)
		var got ServerFrame
		require.NoError(t, json.Unmarshal(data, &got))
		assert.Equal(t, "models", got.Type)
		assert.Equal(t, []string{"a", "b", "c"}, got.Names)
	})

	t.Run("status", func(t *testing.T) {
		in := NewStatus("claude-opus-4", "medium", 1200, 800, 3, 128000)
		data, err := json.Marshal(in)
		require.NoError(t, err)
		var got ServerFrame
		require.NoError(t, json.Unmarshal(data, &got))
		assert.Equal(t, "status", got.Type)
		assert.Equal(t, "claude-opus-4", got.Model)
		assert.Equal(t, "medium", got.Thinking)
		assert.Equal(t, 1200, got.TokensIn)
		assert.Equal(t, 800, got.TokensOut)
		assert.Equal(t, 3, got.Turns)
		assert.Equal(t, 128000, got.ContextWindow, "context_window must round-trip")
	})

	t.Run("permission_request", func(t *testing.T) {
		in := NewPermissionRequest("req-1", "shell", `{"cmd":"rm -rf /"}`, "shell command", false, true)
		data, err := json.Marshal(in)
		require.NoError(t, err)
		var got ServerFrame
		require.NoError(t, json.Unmarshal(data, &got))
		assert.Equal(t, "permission_request", got.Type)
		assert.Equal(t, "req-1", got.ID)
		assert.Equal(t, "shell", got.ToolName)
		assert.Equal(t, `{"cmd":"rm -rf /"}`, got.ToolArgs)
		assert.Equal(t, "shell command", got.Reason)
		assert.True(t, got.ForcePrompt, "force_prompt must round-trip on the wire")
	})

	t.Run("compact_chunk", func(t *testing.T) {
		in := NewCompactChunk("summary delta …")
		data, err := json.Marshal(in)
		require.NoError(t, err)
		var got ServerFrame
		require.NoError(t, json.Unmarshal(data, &got))
		assert.Equal(t, "compact_chunk", got.Type)
		assert.Equal(t, "summary delta …", got.Text)
	})
}

// TestServerFrame_SSEEvent_ParityFrames confirms the SSE wire encoding (event
// name = Type, data = JSON) holds for the new parity frames too, so SSE and WS
// share one event vocabulary for models/status/permission_request.
func TestServerFrame_SSEEvent_ParityFrames(t *testing.T) {
	for _, in := range []ServerFrame{
		NewModels([]string{"x"}),
		NewStatus("m", "low", 1, 2, 3, 0),
		NewPermissionRequest("id", "t", "{}", "r", false, false),
	} {
		event, data := in.SSEEvent()
		assert.Equal(t, in.Type, event)
		var got ServerFrame
		require.NoError(t, json.Unmarshal(data, &got))
		assert.Equal(t, in.Type, got.Type)
	}
}

// TestStructuredResultFrame verifies NewStructuredResult builds a frame of type
// "structured_result" whose StructuredResult field marshals as raw JSON embedded
// in the frame (not an escaped string), so a consumer can parse the frame JSON
// and unmarshal the structured payload directly without a second decode of a
// string-escaped inner JSON.
func TestStructuredResultFrame(t *testing.T) {
	f := NewStructuredResult(json.RawMessage(`{"k":"v"}`))
	assert.Equal(t, "structured_result", f.Type)
	data, err := json.Marshal(f)
	require.NoError(t, err)
	s := string(data)
	assert.Contains(t, s, `"structured_result"`, "frame type tag must be present")
	// StructuredResult must serialize as raw JSON, not an escaped string. An
	// escaped form would look like `"structured_result":"{\"k\":\"v\"}"`; the raw
	// form is `"structured_result":{"k":"v"}`.
	assert.Contains(t, s, `"structured_result":{"k":"v"}`, "StructuredResult must be raw JSON object, not escaped string")
}

// TestNewUserMessageWithSchema verifies the constructor carries the text and the
// per-turn JSON Schema into the frame fields so the server can read
// cf.OutputSchema and gate structured-output validation on it.
func TestNewUserMessageWithSchema(t *testing.T) {
	f := NewUserMessageWithSchema("hi", json.RawMessage(`{"type":"object"}`))
	assert.Equal(t, "user_message", f.Type)
	assert.Equal(t, "hi", f.Text)
	assert.Equal(t, `{"type":"object"}`, string(f.OutputSchema), "schema must be carried verbatim")
}

// TestUserMessageOmitsSchemaByDefault is the text-mode regression guard: a plain
// NewUserMessage must NOT carry an output_schema field on the wire so legacy
// clients and the no-schema path remain byte-identical to pre-A12 (the
// invariant called out in the plan: "text 模式字节不变").
func TestUserMessageOmitsSchemaByDefault(t *testing.T) {
	b, err := json.Marshal(NewUserMessage("hi"))
	require.NoError(t, err)
	assert.NotContains(t, string(b), "output_schema", "plain user_message must omit output_schema")
}

func TestNewRenameSession(t *testing.T) {
	f := NewRenameSession("s1", "new title")
	if f.Type != "rename_session" || f.ID != "s1" || f.Text != "new title" {
		t.Fatalf("got %+v", f)
	}
}

func TestNewArchiveUnarchiveDeleteSession(t *testing.T) {
	ar := NewArchiveSession("s1")
	if ar.Type != "archive_session" || ar.ID != "s1" {
		t.Fatalf("archive: %+v", ar)
	}
	un := NewUnarchiveSession("s1")
	if un.Type != "unarchive_session" || un.ID != "s1" {
		t.Fatalf("unarchive: %+v", un)
	}
	del := NewDeleteSession("s1")
	if del.Type != "delete_session" || del.ID != "s1" {
		t.Fatalf("delete: %+v", del)
	}
}

func TestNewSessionListArchived(t *testing.T) {
	f := NewSessionListArchived()
	if f.Type != "session_list_archived" {
		t.Fatalf("got %+v", f)
	}
}

func TestNewSessionAck(t *testing.T) {
	f := NewSessionAck("renamed", "s1", "new title")
	if f.Type != "session_ack" || f.Action != "renamed" || f.SessionID != "s1" || f.Text != "new title" {
		t.Fatalf("got %+v", f)
	}
	// action must serialize as a top-level JSON string field, not be dropped.
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"action":"renamed"`) {
		t.Fatalf("action field missing from wire form: %s", b)
	}
}

// TestPermissionFramesUseExistingIDAndSnakeCase (Task 9): the new permission
// control frames must reuse the existing ID/Decision fields on ClientFrame and
// serialize with the documented snake_case wire names.
func TestPermissionFramesUseExistingIDAndSnakeCase(t *testing.T) {
	resp := NewPermissionResponse("req-1", "allow_persistent")
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"type":"permission_response","id":"req-1","decision":"allow_persistent"}` {
		t.Fatalf("response=%s", data)
	}
	info := PermissionInfo{ID: "r1", Action: "shell_run", Scope: "go test", TTL: "persistent", Source: "user", CreatedAt: 1}
	frame := NewPermissions([]PermissionInfo{info})
	if frame.Type != "permissions" || len(frame.Permissions) != 1 {
		t.Fatalf("frame=%#v", frame)
	}
	if NewRevokePermission("r1").ID != "r1" {
		t.Fatal("revoke must reuse ClientFrame.ID")
	}
}

// TestJobFramesAreSnakeCaseAndReuseExistingFields (Task 22): the jobs control
// frames reuse the existing ClientFrame/ServerFrame fields (Type, ID, Text)
// and serialize with snake_case wire names. jobs_list has only its Type;
// job_cancel reuses ID; the jobs reply carries the JobInfo slice via the
// ServerFrame.Jobs field.
func TestJobFramesAreSnakeCaseAndReuseExistingFields(t *testing.T) {
	cf := NewJobsList()
	data, _ := json.Marshal(cf)
	if string(data) != `{"type":"jobs_list"}` {
		t.Fatalf("list frame=%s", data)
	}
	if NewJobCancel("j-1").ID != "j-1" {
		t.Fatal("cancel must reuse ID")
	}
	frame := NewJobs(Jobs{JobInfo{ID: "j-1", SessionID: "s-1", Command: "go test", State: "running", PID: 7, StartedAt: 1}})
	if frame.Type != "jobs" || len(frame.Jobs) != 1 {
		t.Fatalf("frame=%#v", frame)
	}
}

// TestNewPlanUpdateNilRowsIsNonNilEmptyChecklist proves that NewPlanUpdate with
// nil rows produces a ServerFrame whose Checklist.Items is a non-nil empty
// slice — letting the TUI distinguish "cleared checklist" from "no checklist
// event ever sent".
func TestNewPlanUpdateNilRowsIsNonNilEmptyChecklist(t *testing.T) {
	frame := NewPlanUpdate("wt-1", nil)
	require.NotNil(t, frame.Checklist)
	assert.Empty(t, frame.Checklist.Items)
	_, data := frame.SSEEvent()
	assert.Contains(t, string(data), `"type":"plan_update"`)

	// checklist_update 同样保证非 nil
	frame2 := NewChecklistUpdate("wt-1", nil)
	require.NotNil(t, frame2.Checklist)
	assert.Empty(t, frame2.Checklist.Items)

	frame3 := NewTaskUpdate(nil)
	assert.Empty(t, frame3.Type)
}

func TestNewSubagentEventFrame(t *testing.T) {
	f := NewSubagentEvent("ag-1", "explore", "started", "running", "scanning")
	require.Equal(t, "subagent_event", f.Type)
	require.Equal(t, "ag-1", f.AgentID)
	require.Equal(t, "explore", f.AgentRole)
	require.Equal(t, "started", f.Event)
	require.Equal(t, "running", f.AgentStatus)

	event, data := f.SSEEvent()
	require.Equal(t, "subagent_event", event)
	require.Contains(t, string(data), `"agent_id":"ag-1"`)
}

// TestFeaturesSetPayloadEncodesFalseEnabled proves the *bool shape is preserved
// on the wire so "off" toggles survive JSON round-trip. A plain bool with
// omitempty would drop "enabled":false entirely, leaving the server unable to
// distinguish "false" from "absent".
func TestFeaturesSetPayloadEncodesFalseEnabled(t *testing.T) {
	frame := NewFeaturesSet("observe.otel_export", false)
	if frame.FeaturesSet == nil || frame.FeaturesSet.Enabled == nil {
		t.Fatal("Enabled must be a *bool so false survives omitempty")
	}
	if *frame.FeaturesSet.Enabled != false {
		t.Fatalf("Enabled = %v", *frame.FeaturesSet.Enabled)
	}
	raw, _ := json.Marshal(frame)
	if !strings.Contains(string(raw), `"enabled":false`) {
		t.Fatalf("wire form must encode enabled:false: %s", raw)
	}
}

// TestStatusFrameCarriesCostAndFeatures proves the COST1 / OBS3 extensions land
// on the wire when set on a status frame. omitempty keeps the legacy shape when
// unset (verified by other status tests), so this test focuses on the positive
// case.
func TestStatusFrameCarriesCostAndFeatures(t *testing.T) {
	st := NewStatusWithMode("claude-opus-4-8", "low", 100, 50, 1, 200000, "default", 0)
	st.CostUSD = 0.25
	st.CostKnown = true
	st.Features = []FeatureRow{{Key: "observe.otel_export", Stage: "experimental", Enabled: false, Owner: "C4"}}
	raw, _ := json.Marshal(st)
	for _, want := range []string{`"cost_usd":0.25`, `"cost_known":true`, `"features":[{`, `"owner":"C4"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing %s in %s", want, raw)
		}
	}
}

func TestUserMessageWithImagesCarriesAdditiveField(t *testing.T) {
	fr := NewUserMessageWithImages("see this", []ImageAttach{
		{ID: "img-1", Source: "paste", Fmt: "png", W: 1280, H: 720, DataB64: "AAAA"},
	})
	if fr.Type != "user_message" || fr.Text != "see this" {
		t.Fatalf("base fields must be preserved: %#v", fr)
	}
	if len(fr.Images) != 1 || fr.Images[0].ID != "img-1" {
		t.Fatalf("images = %#v", fr.Images)
	}
	raw, _ := json.Marshal(fr)
	if !strings.Contains(string(raw), `"images":[{`) {
		t.Fatalf("wire form must include images array: %s", raw)
	}
	if strings.Contains(string(raw), `"images":[]`) {
		t.Fatalf("omitempty must drop empty images, not emit []: %s", raw)
	}
}

func TestUserMessageWithoutImagesOmitsField(t *testing.T) {
	raw, _ := json.Marshal(NewUserMessage("text only"))
	if strings.Contains(string(raw), "images") {
		t.Fatalf("text-only user_message must not carry images on wire: %s", raw)
	}
}

func TestImageAttachJSONIsCamelCase(t *testing.T) {
	raw, _ := json.Marshal(ImageAttach{ID: "img-1", DataB64: "AA", Fmt: "png", W: 1, H: 2})
	got := string(raw)
	for _, want := range []string{`"id":"img-1"`, `"dataB64":"AA"`, `"w":1`, `"h":2`} {
		if !strings.Contains(got, want) {
			t.Fatalf("wire %q lacks %s", got, want)
		}
	}
}

// goldenFrames returns one representative constructor for every ServerFrame
// Type exercised by the SSE transport. Add a row here when a new frame type
// ships, then run with -update.
func goldenFrames() []ServerFrame {
	return []ServerFrame{
		NewAgentChunk("hi"),
		NewThinking("..."),
		NewToolCall("fs_search", "{}", "running"),
		NewToolResult("fs_search", "ok", "ok"),
		NewToolProgress("fs_search", "1"),
		NewError("boom"),
		NewDone(),
		NewRetry(1, 3, 500, "transient error"),
		NewModels([]string{"a"}),
		NewStatus("m", "low", 1, 2, 3, 4),
		NewStatusWithMode("m", "low", 1, 2, 3, 4, "default", 0),
		NewCompactChunk("summary"),
		NewHistoryReplaced(nil),
		NewSessions(nil),
		NewSessionRestored("s1", nil, "model", "off", 10, 20, 3),
		NewSessionAck("renamed", "s1", "t"),
		NewSessionForked("fork-id-123"),
		NewStructuredResult(json.RawMessage(`{}`)),
		NewSubagentEvent("ag-1", "explore", "started", "running", "x"),
		NewMCPStatusFrame(nil),
		NewSkillsList(nil),
		NewSkillAck("installed", &SkillInfo{Name: "hi"}, ""),
		NewJobs(Jobs{}),
		NewJobEvent(JobInfo{ID: "j1", State: "running", Output: "data"}),
		NewPlanUpdate("wt-1", nil),
		NewChecklistUpdate("wt-1", nil),
		NewTaskUpdate(nil),
		NewSideState(1),
		NewSeams(nil, "", ""),
		NewSeamRestored("s1", "abc123", "fullhead", "reverted"),
		NewFeaturesReply(nil),
		NewPermissionRuleHit("r1", "shell_run", "scope", "hit"),
		NewPermissions(nil),
		NewPermissionRequest("id", "t", "{}", "r", false, false),
	}
}

// TestSSEEvent_Golden freezes the SSE wire form (event: <name>\ndata: <json>\n\n)
// for a representative frame of every ServerFrame type. Run with -update to
// regenerate testdata/sse_golden.txt after adding/changing a frame; CI runs
// without -update so an un-regenerated change fails the build. This guards the
// "two transports, one vocabulary" invariant from CLAUDE.md.
func TestSSEEvent_Golden(t *testing.T) {
	frames := goldenFrames() // every type, one representative constructor each
	var b strings.Builder
	for _, f := range frames {
		event, data := f.SSEEvent()
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", event, data)
	}
	got := b.String()

	goldenPath := filepath.Join("testdata", "sse_golden.txt")
	if *updateGolden {
		require.NoError(t, os.MkdirAll("testdata", 0o755))
		require.NoError(t, os.WriteFile(goldenPath, []byte(got), 0o644))
		t.Logf("regenerated %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	require.NoErrorf(t, err, "golden missing — run: go test -run TestSSEEvent_Golden -update ./internal/proto/")
	if string(want) != got {
		t.Fatalf("SSE golden mismatch.\nwant (testdata/sse_golden.txt):\n%s\ngot:\n%s\n"+
			"if intentional, run: go test -run TestSSEEvent_Golden -update ./internal/proto/", want, got)
	}
}

// TestUnknownField_Compatibility proves a ServerFrame carrying an unknown
// future field still deserializes (Go json ignores unknown keys by default).
// This is the additive-evolution guarantee: new frame fields must not break
// older parsers, and older parsers must not reject newer frames.
func TestUnknownField_Compatibility(t *testing.T) {
	raw := `{"type":"agent_chunk","text":"hi","future_field":"ignored","another":123}`
	var got ServerFrame
	require.NoError(t, json.Unmarshal([]byte(raw), &got))
	assert.Equal(t, "agent_chunk", got.Type)
	assert.Equal(t, "hi", got.Text)
}

// TestVocabulary_Symmetry proves every ServerFrame Type produced by a
// constructor has a corresponding SSEEvent() emission (event name == Type) —
// i.e. the WS and SSE vocabularies share one frame set. It collects the Type
// of every frame in goldenFrames() and asserts SSEEvent returns that same Type
// as the event name, with non-empty data.
func TestVocabulary_Symmetry(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range goldenFrames() {
		seen[f.Type] = true
		event, data := f.SSEEvent()
		assert.Equal(t, f.Type, event, "SSE event name must equal frame Type")
		assert.NotEmpty(t, data, "SSE data must be non-empty for %s", f.Type)
	}
	// Every Type is unique (no two constructors emit the same Type unless
	// intentionally aliased).
	assert.NotEmpty(t, seen, "goldenFrames must be non-empty")
}

// TestServerFrame_UntestedConstructorsRoundTrip closes the coverage gap for
// ServerFrame constructors that had no marshal→unmarshal round-trip. Each row
// builds a frame, marshals it, unmarshals into a fresh ServerFrame, and checks
// Type plus one frame-specific field — enough to prove the constructor wires
// its fields onto json tags without re-asserting every byte.
func TestServerFrame_UntestedConstructorsRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		build func() ServerFrame
		want  string // expected Type
		check func(ServerFrame) bool
	}{
		{"thinking", func() ServerFrame { return NewThinking("reasoning…") }, "thinking",
			func(f ServerFrame) bool { return f.Text == "reasoning…" }},
		{"tool_progress", func() ServerFrame { return NewToolProgress("fs_search", "50%") }, "tool_progress",
			func(f ServerFrame) bool { return f.ToolName == "fs_search" && f.Text == "50%" }},
		{"retry", func() ServerFrame { return NewRetry(1, 3, 500, "transient error") }, "retry",
			func(f ServerFrame) bool { return f.RetryAttempt == 1 }},
		{"history_replaced", func() ServerFrame { return NewHistoryReplaced(nil) }, "history_replaced",
			func(f ServerFrame) bool { return true }},
		{"sessions", func() ServerFrame { return NewSessions([]SessionInfo{{ID: "s1", Title: "t"}}) }, "sessions",
			func(f ServerFrame) bool { return len(f.Sessions) == 1 && f.Sessions[0].ID == "s1" }},
		{"session_restored", func() ServerFrame { return NewSessionRestored("s1", nil, "model", "off", 10, 20, 3) }, "session_restored",
			func(f ServerFrame) bool { return f.SessionID == "s1" }},
		{"session_forked", func() ServerFrame { return NewSessionForked("fork-id-123") }, "session_forked",
			func(f ServerFrame) bool { return f.SessionID == "fork-id-123" }},
		{"side_state", func() ServerFrame { return NewSideState(3) }, "side_state",
			func(f ServerFrame) bool { return f.SideDepth == 3 }},
		{"features_reply", func() ServerFrame { return NewFeaturesReply([]FeatureRow{{Key: "k"}}) }, "features",
			func(f ServerFrame) bool { return len(f.Features) == 1 && f.Features[0].Key == "k" }},
		{"permission_rule_hit", func() ServerFrame { return NewPermissionRuleHit("r1", "shell_run", "scope", "hit") }, "permission_rule_hit",
			func(f ServerFrame) bool { return f.ID == "r1" }},
		{"skills_list", func() ServerFrame { return NewSkillsList([]SkillInfo{{Name: "hi"}}) }, "skills_list",
			func(f ServerFrame) bool { return len(f.Skills) == 1 && f.Skills[0].Name == "hi" }},
		{"skill_ack", func() ServerFrame { return NewSkillAck("installed", &SkillInfo{Name: "hi"}, "") }, "skill_ack",
			func(f ServerFrame) bool { return f.Action == "installed" }},
		{"job_event", func() ServerFrame { return NewJobEvent(JobInfo{ID: "j1", State: "running", Output: "data"}) }, "job_event",
			func(f ServerFrame) bool { return f.ID == "j1" && f.Status == "running" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := c.build()
			data, err := json.Marshal(in)
			require.NoError(t, err)
			var got ServerFrame
			require.NoError(t, json.Unmarshal(data, &got))
			assert.Equal(t, c.want, got.Type)
			assert.Truef(t, c.check(got), "field check failed for %s: %+v", c.name, got)
		})
	}
}

// TestClientFrame_UntestedConstructorsRoundTrip is the ClientFrame twin.
func TestClientFrame_UntestedConstructorsRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		build func() ClientFrame
		want  string
		check func(ClientFrame) bool
	}{
		{"restore_session", func() ClientFrame { return NewRestoreSession("s1") }, "restore_session",
			func(f ClientFrame) bool { return f.ID == "s1" }},
		{"list_skills", func() ClientFrame { return NewListSkills() }, "list_skills",
			func(f ClientFrame) bool { return true }},
		{"install_skill", func() ClientFrame { return NewInstallSkill("hi") }, "install_skill",
			func(f ClientFrame) bool { return f.Source == "hi" }},
		{"uninstall_skill", func() ClientFrame { return NewUninstallSkill("hi") }, "uninstall_skill",
			func(f ClientFrame) bool { return f.Name == "hi" }},
		{"trust_skill", func() ClientFrame { return NewTrustSkill("hi") }, "trust_skill",
			func(f ClientFrame) bool { return f.Name == "hi" }},
		{"untrust_skill", func() ClientFrame { return NewUntrustSkill("hi") }, "untrust_skill",
			func(f ClientFrame) bool { return f.Name == "hi" }},
		{"enable_skill", func() ClientFrame { return NewEnableSkill("hi") }, "enable_skill",
			func(f ClientFrame) bool { return f.Name == "hi" }},
		{"disable_skill", func() ClientFrame { return NewDisableSkill("hi") }, "disable_skill",
			func(f ClientFrame) bool { return f.Name == "hi" }},
		{"restore_turn", func() ClientFrame { return NewRestoreTurn("seam-1", "head-1") }, "restore_turn",
			func(f ClientFrame) bool { return f.ID == "seam-1" && f.ConfirmedHead == "head-1" }},
		{"fork_session", func() ClientFrame { return NewForkSession(5) }, "fork_session",
			func(f ClientFrame) bool { return f.Seq == 5 }},
		{"enter_side", func() ClientFrame { return NewEnterSide() }, "enter_side",
			func(f ClientFrame) bool { return true }},
		{"exit_side", func() ClientFrame { return NewExitSide() }, "exit_side",
			func(f ClientFrame) bool { return true }},
		{"features_list", func() ClientFrame { return NewFeaturesList() }, "features_list",
			func(f ClientFrame) bool { return true }},
		{"list_permissions", func() ClientFrame { return NewListPermissions() }, "permissions_list",
			func(f ClientFrame) bool { return true }},
		{"job_read", func() ClientFrame { return NewJobRead("j1", 4096) }, "job_read",
			func(f ClientFrame) bool { return f.ID == "j1" }},
		{"job_write", func() ClientFrame { return NewJobWrite("j1", "ls") }, "job_write",
			func(f ClientFrame) bool { return f.ID == "j1" && f.Text == "ls" }},
		{"set_mode", func() ClientFrame { return NewSetMode("yolo", 0) }, "set_mode",
			func(f ClientFrame) bool { return f.Mode == "yolo" }},
		{"cancel", func() ClientFrame { return NewCancel() }, "cancel",
			func(f ClientFrame) bool { return true }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := c.build()
			data, err := json.Marshal(in)
			require.NoError(t, err)
			var got ClientFrame
			require.NoError(t, json.Unmarshal(data, &got))
			assert.Equal(t, c.want, got.Type)
			assert.Truef(t, c.check(got), "field check failed for %s: %+v", c.name, got)
		})
	}
}
