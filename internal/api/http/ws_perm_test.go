package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/tools"
)

// newPermWSServer builds a WS server whose orchestrator has an fs_write tool and
// a profile that ALLOWS the tool name and grants a narrow write path that
// excludes "out.txt". Every fs_write to "out.txt" therefore returns Prompt at
// the fs dimension and must be resolved via the interactive permission
// callback. Returns the ws URL and the workdir the tool is rooted at (so the
// test can observe the file side-effect).
func newPermWSServer(t *testing.T) (url, workdir string) {
	t.Helper()
	workdir = t.TempDir()

	// Scripted model: (1) call fs_write, (2) emit a final message so ReAct ends.
	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name:      "fs_write",
			Arguments: `{"path":"out.txt","content":"hello"}`,
		}},
	})
	step2 := schema.AssistantMessage("written", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2}, nil)

	fs := tools.NewFSTools(workdir)
	orchTools := []orchestrator.BaseTool{fs.Write}
	o, err := orchestrator.New(orchestrator.Config{
		Model: mdl,
		Tools: orchTools,
		// Tool name allowed; Write allowlist grants a path that excludes the
		// test's "out.txt" -> the fs dimension returns Prompt (not HardDeny),
		// so the interactive callback is consulted. After Task 1, an empty
		// Write list would be a structural HardDeny and skip the callback.
		Profile: guard.PermissionProfile{
			Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
			FS:    guard.FSPerm{Write: []string{filepath.Join(workdir, "safe/**")}},
		},
	})
	require.NoError(t, err)

	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	url = "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/chat/ws"
	return url, workdir
}

// readFrame reads one ServerFrame with a generous deadline (the turn may block
// on a permission round-trip). Under -race the orchestrator/ADK path is
// instrumented on every memory access and runs much slower, so scale the
// deadline up to avoid spurious i/o-timeout failures unrelated to the code
// under test.
//
// The deadline only exists so a lost frame fails instead of hanging the suite
// forever — a healthy run returns in well under a second and never approaches
// it. It is deliberately longer than the server's 60s permission-callback
// timeout (ws.go): when a permission_request really does go missing, outliving
// that timeout means the client reads the resulting "done" and drainUntil
// reports `saw "done" before "permission_request"` — the actual diagnosis —
// instead of an opaque i/o timeout that says only "something was slow". 30s
// sat under the 60s and produced exactly that useless red on windows (run
// 30786620495).
func readFrame(t *testing.T, c *websocket.Conn) proto.ServerFrame {
	t.Helper()
	deadline := 120 * time.Second
	if raceDetectorEnabled {
		deadline = 180 * time.Second
	}
	require.NoError(t, c.SetReadDeadline(time.Now().Add(deadline)))
	_, data, err := c.ReadMessage()
	require.NoError(t, err)
	var f proto.ServerFrame
	require.NoError(t, json.Unmarshal(data, &f))
	return f
}

// drainUntil reads frames until one matches want (by type), returning it.
// Fails the test on read error or if "done"/"error" arrives first without match.
func drainUntil(t *testing.T, c *websocket.Conn, want string) proto.ServerFrame {
	t.Helper()
	for {
		f := readFrame(t, c)
		if f.Type == want {
			return f
		}
		if f.Type == "done" || f.Type == "error" {
			t.Fatalf("saw %q before %q", f.Type, want)
		}
	}
}

// TestChatWS_InteractivePermission_Allow runs the tool: the client receives a
// permission_request, replies allow, and the fs_write side-effect appears.
func TestChatWS_InteractivePermission_Allow(t *testing.T) {
	url, workdir := newPermWSServer(t)
	c := dial(t, url)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("write the file")))

	// Drain straight to permission_request — do NOT drain tool_call first.
	// The two frames are written by DIFFERENT goroutines (tool_call by the
	// main loop draining the ADK event iterator, permission_request by the ADK
	// worker inside the tool's permission callback — see the wsConn doc in
	// ws_handlers.go), so their order is a race, not a sequence. drainUntil
	// silently discards non-matching frames, so whenever permission_request
	// won that race the tool_call drain ATE it: the next drain then waited out
	// the server's 60s permission timeout and saw "done". That is the whole
	// story behind two CI reds — an i/o timeout on windows (run 30786620495)
	// and "saw done before permission_request" on ubuntu (run 30788165405).
	req := drainUntil(t, c, "permission_request")
	assert.Equal(t, "fs_write", req.ToolName)
	assert.Contains(t, req.ToolArgs, "out.txt")
	assert.NotEmpty(t, req.ID, "permission_request must carry an id")
	assert.NotEmpty(t, req.Reason, "reason must explain the static denial")

	// Approve.
	require.NoError(t, c.WriteJSON(proto.NewPermissionResponse(req.ID, "allow")))

	// The tool must run and a tool_result must arrive, then the turn ends.
	var sawResult bool
	for {
		f := readFrame(t, c)
		if f.Type == "tool_result" {
			sawResult = true
			assert.Equal(t, "fs_write", f.ToolName)
		}
		if f.Type == "done" {
			break
		}
	}
	assert.True(t, sawResult, "fs_write must produce a tool_result after allow")

	// The real side-effect: the file exists in the workdir.
	assert.FileExists(t, filepath.Join(workdir, "out.txt"),
		"allow must let fs_write run and create the file")
}

// TestChatWS_InteractivePermission_Deny skips the tool: the client denies, no
// file is written, and the turn still completes (done).
func TestChatWS_InteractivePermission_Deny(t *testing.T) {
	url, workdir := newPermWSServer(t)
	c := dial(t, url)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("write the file")))

	// No tool_call drain first — it races with permission_request and would
	// swallow it. See the comment in TestChatWS_InteractivePermission_Allow.
	req := drainUntil(t, c, "permission_request")

	// Deny.
	require.NoError(t, c.WriteJSON(proto.NewPermissionResponse(req.ID, "deny")))

	// Turn must still terminate (done) even though the tool was denied.
	for {
		f := readFrame(t, c)
		if f.Type == "done" {
			break
		}
	}

	// The file must NOT exist — the deny prevented the write.
	_, err := os.ReadFile(filepath.Join(workdir, "out.txt"))
	assert.Error(t, err, "deny must prevent the file from being written")
}

// TestChatWS_InteractivePermission_AlwaysAllow_NoReprompt proves a second
// identical fs_write in the same connection is approved by the session
// allowlist without prompting the user again.
func TestChatWS_InteractivePermission_AlwaysAllow_NoReprompt(t *testing.T) {
	workdir := t.TempDir()

	// Model: call fs_write twice, then a final message each round needs its own
	// "finish" — so script: call, finish, call, finish.
	call := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name:      "fs_write",
			Arguments: `{"path":"a.txt","content":"x"}`,
		}},
	})
	finish := schema.AssistantMessage("ok", nil)
	call2 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c2", Type: "function", Function: schema.FunctionCall{
			Name:      "fs_write",
			Arguments: `{"path":"a.txt","content":"x"}`,
		}},
	})
	mdl := einollm.NewFakeModelWithMessages(
		[]*schema.Message{call, finish, call2, finish}, nil)

	fs := tools.NewFSTools(workdir)
	o, err := orchestrator.New(orchestrator.Config{
		Model: mdl,
		Tools: []orchestrator.BaseTool{fs.Write},
		Profile: guard.PermissionProfile{
			Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
			// Write allowlist excludes "a.txt" so fs_write returns Prompt (the
			// interactive path). An empty list would HardDeny after Task 1.
			FS: guard.FSPerm{Write: []string{filepath.Join(workdir, "safe/**")}},
		},
	})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()

	// Turn 1: prompt -> always_allow.
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("go")))
	// No tool_call drain first — it races with permission_request and would
	// swallow it. See the comment in TestChatWS_InteractivePermission_Allow.
	req := drainUntil(t, c, "permission_request")
	require.NoError(t, c.WriteJSON(proto.NewPermissionResponse(req.ID, "always_allow")))
	for {
		f := readFrame(t, c)
		if f.Type == "done" {
			break
		}
	}

	// Turn 2: identical action -> allowlist hit, NO permission_request expected.
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("again")))
	var sawPrompt bool
	for {
		f := readFrame(t, c)
		if f.Type == "permission_request" {
			sawPrompt = true
		}
		if f.Type == "done" {
			break
		}
	}
	assert.False(t, sawPrompt, "always_allow must suppress re-prompting the identical action")
}

// TestPermissionRequestFrameCarriesForcePrompt pins the wire half of the
// force-prompt contract. resolvePermissionMode already refuses to auto-resolve
// req.ForcePrompt / req.Force, but that refusal only survives if the flag is
// forwarded: the TUI runs its own auto-approve pass on every mode switch and
// can honour only what it can see. When ServerFrame had no ForcePrompt field
// the prompt appeared, the user switched to YOLO, and the client answered
// "allow" for them.
//
// task_cancel is on tools.forcePromptTools, so Authorize takes the force-prompt
// branch before consulting the profile — a wildcard profile still prompts.
func TestPermissionRequestFrameCarriesForcePrompt(t *testing.T) {
	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name: "task_cancel", Arguments: `{"id":"t1"}`,
		}},
	})
	step2 := schema.AssistantMessage("done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2}, nil)

	cancelTool := tools.NewGuardedTool("task_cancel", "Cancel", "cancel a task", time.Second, nil,
		tools.SyncStream(func(context.Context, string) (string, error) { return "cancelled", nil }))
	o, err := orchestrator.New(orchestrator.Config{
		Model: mdl,
		Tools: []orchestrator.BaseTool{cancelTool},
		// Wildcard profile: nothing here can explain the prompt except the
		// force-prompt list itself.
		Profile: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}},
	})
	require.NoError(t, err)

	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()

	// YOLO is the mode that used to swallow this prompt client-side; the
	// server must still emit it, flagged.
	require.NoError(t, c.WriteJSON(proto.ClientFrame{Type: "set_mode", Mode: string(guard.ModeYOLO)}))
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("cancel the task")))

	f := drainUntil(t, c, "permission_request")
	assert.Equal(t, "task_cancel", f.ToolName)
	assert.True(t, f.ForcePrompt,
		"permission_request must carry force_prompt=true, else the client auto-approves it")

	// And it must be on the JSON, not just the Go struct: the TUI decodes the
	// wire bytes, so an unexported/omitted field would be invisible to it.
	raw, err := json.Marshal(f)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"force_prompt":true`)

	require.NoError(t, c.WriteJSON(proto.NewPermissionResponse(f.ID, "deny")))
}

// TestForcePromptFlagCoversBothServerFlags pins the predicate that feeds the
// wire field. Both server-side flags mean "explicit decision only", and both
// have a matching refusal to auto-resolve (req.ForcePrompt in
// resolvePermissionMode, req.Force in resolvePermissionRequest). The wire flag
// must therefore be true for BOTH — the revert_turn (Force) half has no
// separate wire field of its own, so dropping it here would silently restore
// the client-side auto-approve for destructive actions.
//
// The second assertion in each row is the anti-drift one: it re-derives the
// expectation from the refusal path instead of restating the boolean.
func TestForcePromptFlagCoversBothServerFlags(t *testing.T) {
	cases := []struct {
		name string
		req  tools.PermissionRequest
		want bool
	}{
		{"force_prompt_tool", tools.PermissionRequest{Tool: "task_cancel", ForcePrompt: true}, true},
		{"require_approval_destructive", tools.PermissionRequest{Tool: "revert_turn", Force: true}, true},
		{"both", tools.PermissionRequest{Tool: "task_cancel", ForcePrompt: true, Force: true}, true},
		{"ordinary", tools.PermissionRequest{Tool: "fs_write"}, false},
		// approval_required rides its own wire field; the client ORs them.
		{"approval_required_only", tools.PermissionRequest{Tool: "github_comment", ApprovalRequired: true}, false},
	}
	cs := &connSession{perm: &permModeState{mode: guard.ModeYOLO}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, forcePromptFlag(tc.req))
			if tc.want {
				_, resolved := resolvePermissionRequest(context.Background(), cs, nil, tc.req)
				assert.False(t, resolved,
					"flagged on the wire but the server auto-resolved it — the two halves have drifted")
			}
		})
	}
}
