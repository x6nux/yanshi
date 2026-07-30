package http

import (
	"bufio"
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

func TestChatSSE_ForwardsTypedSubagentEventWithSingleWriter(t *testing.T) {
	emitCall := schema.AssistantMessage("", []schema.ToolCall{{
		ID: "emit-1", Type: "function",
		Function: schema.FunctionCall{Name: "emit_subagent_test", Arguments: `{}`},
	}})
	done := schema.AssistantMessage("top done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{emitCall, done}, nil)

	emitTool := newSubagentEmitTestTool(t)
	o, err := orchestrator.New(orchestrator.Config{
		Model: mdl, Tools: []orchestrator.BaseTool{emitTool},
		Profile: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}},
	})
	require.NoError(t, err)

	s := New(Config{Token: "t"})
	s.Chat(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	body := bytes.NewBufferString(`{"messages":[{"role":"user","content":"emit"}]}`)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/chat", body)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	scanner := bufio.NewScanner(resp.Body)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	require.NoError(t, scanner.Err())
	joined := strings.Join(lines, "\n")
	require.Contains(t, joined, "event: subagent_event")
	require.Contains(t, joined, `"agent_id":"ag-test"`)
}
