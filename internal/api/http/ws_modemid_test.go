package http

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/tools"
)

// TestChatWS_ModeSwitchMidTurn_SubsequentCallUsesNewMode is the regression test
// for "切换之后就直接以新权限来判断" inside a running turn (notably a sub-agent):
//
// A sub-agent does two fs_write calls that the static profile denies. The FIRST
// prompts (mode=default). While it is pending, the client switches to yolo and
// answers the first prompt. The SECOND write must then be auto-approved by the
// new mode — it must NOT prompt again.
//
// BUG: set_mode was forwarded to the frames channel (drained only by the main
// loop), which is blocked for the whole turn, so the session mode stayed
// "default" and the second write re-prompted. FIX: the reader goroutine applies
// set_mode to a live, mutex-guarded mode state immediately, so mid-turn
// callbacks see it.
func TestChatWS_ModeSwitchMidTurn_SubsequentCallUsesNewMode(t *testing.T) {
	workdir := t.TempDir()

	// Shared model consumed across top-level + sub-agent:
	//   1) top: agent_start delegating two writes
	//   2) sub: fs_write a.txt  (static-denied -> prompt r1)
	//   3) sub: fs_write b.txt  (after mode->yolo: must auto-approve)
	//   4) sub: finish
	//   5) top: finish
	agentStart := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "a1", Type: "function", Function: schema.FunctionCall{
			Name:      "agent_start",
			Arguments: `{"prompt":"write a.txt and b.txt","tools":"[\"fs_write\"]"}`,
		}},
	})
	writeA := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name: "fs_write", Arguments: `{"path":"a.txt","content":"x"}`,
		}},
	})
	writeB := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c2", Type: "function", Function: schema.FunctionCall{
			Name: "fs_write", Arguments: `{"path":"b.txt","content":"y"}`,
		}},
	})
	finish := schema.AssistantMessage("done", nil)
	mdl := einollm.NewFakeModelWithMessages(
		[]*schema.Message{agentStart, writeA, writeB, finish, finish}, nil)

	fs := tools.NewFSTools(workdir)
	agentTools := tools.NewAgentTools(mdl)
	o, err := orchestrator.New(orchestrator.Config{
		Model: mdl,
		Tools: []orchestrator.BaseTool{agentTools.StartAgent, fs.Write},
		Profile: guard.PermissionProfile{
			Tools: guard.ToolsPerm{Allow: []string{"agent_start", "fs_*"}},
			// Write allowlist excludes the test paths so fs_write returns
			// Prompt (interactive path). After Task 1, an empty Write list
			// would HardDeny and skip the callback entirely.
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

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("delegate two writes")))

	// First write prompts (mode=default).
	req := drainUntil(t, c, "permission_request")
	assert.Equal(t, "fs_write", req.ToolName)
	firstID := req.ID

	// Switch to yolo MID-TURN, then answer the pending prompt.
	require.NoError(t, c.WriteJSON(proto.NewSetMode("yolo")))
	require.NoError(t, c.WriteJSON(proto.NewPermissionResponse(firstID, "allow")))

	// Drain the rest of the turn. Count any further permission_request frames.
	// If a second prompt appears (the bug), answer it so the turn can finish and
	// the assertions below report the failure cleanly instead of hanging.
	var prompts int
	for {
		f := readFrame(t, c)
		if f.Type == "permission_request" {
			prompts++
			// Unblock the tool so the turn can terminate.
			_ = c.WriteJSON(proto.NewPermissionResponse(f.ID, "deny"))
		}
		if f.Type == "done" || f.Type == "error" {
			break
		}
	}
	assert.Equal(t, 0, prompts,
		"after switching to yolo mid-turn, the sub-agent's next write must NOT prompt")
	assert.FileExists(t, filepath.Join(workdir, "a.txt"), "first write (allowed) must land")
	assert.FileExists(t, filepath.Join(workdir, "b.txt"),
		"second write must run under the new yolo mode")
}
