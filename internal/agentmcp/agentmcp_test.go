package agentmcp

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/api/v1"
	"github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/mcp"
)

// startOverPipes 把 Server 与本仓自己的 mcp.StdioClient 用内存管道接起来：
// 客户端半是真实的（协议、帧、协商），只把传输换成内存 —— 这是「别的
// agent 能把 yanshi 当子 agent 调」这条验收的最近似替身。
func startOverPipes(t *testing.T, svc *v1.Service) *mcp.StdioClient {
	t.Helper()
	inR, inW := io.Pipe() // client → server
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- New(svc).Serve(context.Background(), inR, outW)
	}()
	t.Cleanup(func() {
		_ = inW.Close()  // server EOF → Serve 返回
		_ = outR.Close() // client 侧 reader
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Serve did not return after stdin EOF")
		}
	})
	return mcp.NewStdioClient(outR, inW)
}

// 全链路：用 yanshi 自己的 MCP 客户端连上 yanshi 的 MCP server，走
// initialize → tools/list → agent_prompt（新会话）→ agent_prompt（续接）。
//
// 这条同时核收两件事：「暴露可续接会话的工具」（同一个 session_id 两次
// agent_prompt，第二次走 Resume 挂回同一线程，turn id 递增）与「别的
// agent 能把 yanshi 当子 agent 调」（客户端半是生产代码，不是测试桩）。
//
// 断言说明：DefaultModel-only 的 v1 路径按契约只发一个 stub chunk
//（"(no real model configured)"），所以这里断言的是会话/turn 形状与
// 非空文本 —— 对话记忆语义归 v1/编排器自己的测试管。
//
// 变异：把 callPrompt 的 Resume 分支删掉、session_id 非空时也 Start 新线程
// → 续接用例变红（同一个 id 不可能有两个线程，turn 计数不递增且
// session_id 变化）；把 tools/list 的 agent_prompt 描述符删掉 →
// ListTools 断言变红。
func TestAgentMCPServerViaOwnClient(t *testing.T) {
	svc, err := v1.NewService(v1.Config{DefaultModel: eino.NewFakeModel([]string{"ok"}, nil)})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	cli := startOverPipes(t, svc)
	if err := cli.Initialize(context.Background(), "/"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	tools, err := cli.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := map[string]bool{}
	for _, td := range tools {
		names[td.ToolName] = true
	}
	if !names["agent_prompt"] || !names["agent_interrupt"] {
		t.Fatalf("advertised tools = %v; want agent_prompt and agent_interrupt", names)
	}

	type promptResult struct {
		SessionID string `json:"session_id"`
		TurnID    string `json:"turn_id"`
		Status    string `json:"status"`
		Text      string `json:"text"`
	}
	callPrompt := func(t *testing.T, args string) promptResult {
		t.Helper()
		raw, err := cli.CallTool(context.Background(), "agent_prompt", json.RawMessage(args))
		if err != nil {
			t.Fatalf("agent_prompt(%s): %v", args, err)
		}
		var res promptResult
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatalf("decode result %s: %v", raw, err)
		}
		return res
	}

	first := callPrompt(t, `{"prompt":"hello"}`)
	if first.SessionID == "" || first.TurnID == "" || first.Text == "" || first.Status != "completed" {
		t.Fatalf("first prompt result = %+v", first)
	}
	second := callPrompt(t, `{"prompt":"and then?","session_id":"`+first.SessionID+`"}`)
	if second.SessionID != first.SessionID {
		t.Fatalf("continuation landed on session %q, want the resumed %q", second.SessionID, first.SessionID)
	}
	if second.TurnID == first.TurnID {
		t.Fatalf("continuation reused turn id %q; a second turn must have run", second.TurnID)
	}
}

// 未知 session：Resume 失败必须如实报错（一个静默的新会话会让调用方把
// 对话发进没有历史的线程还以为在续接）。
//
// 变异：把 Resume 失败分支改成「当新会话继续」→ 本测试变红。
func TestAgentMCPUnknownSessionIsAnError(t *testing.T) {
	svc, err := v1.NewService(v1.Config{DefaultModel: eino.NewFakeModel([]string{"ok"}, nil)})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	cli := startOverPipes(t, svc)
	if err := cli.Initialize(context.Background(), "/"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := cli.CallTool(context.Background(), "agent_prompt",
		json.RawMessage(`{"prompt":"hi","session_id":"memory-404"}`)); err == nil {
		t.Fatal("agent_prompt on an unknown session succeeded; want an error naming the session")
	}
}

// agent_interrupt 幂等：无活动 turn 的会话返回 ok，不报错。
func TestAgentMCPInterruptIsIdempotent(t *testing.T) {
	svc, err := v1.NewService(v1.Config{DefaultModel: eino.NewFakeModel([]string{"ok"}, nil)})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	cli := startOverPipes(t, svc)
	if err := cli.Initialize(context.Background(), "/"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	raw, err := cli.CallTool(context.Background(), "agent_prompt", json.RawMessage(`{"prompt":"hello"}`))
	if err != nil {
		t.Fatalf("agent_prompt: %v", err)
	}
	var started struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(raw, &started)
	res, err := cli.CallTool(context.Background(), "agent_interrupt",
		json.RawMessage(`{"session_id":"`+started.SessionID+`"}`))
	if err != nil {
		t.Fatalf("agent_interrupt: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("empty interrupt result")
	}
}

// server 半的协议版本协商：请求 2024-11-05 → 原样回显（这就是「在两版
// 协议间切换」的 server 侧机制）；请求未知版本 → 回答最新版，让 client
// 自己决定去留（client 会对不支持的应答硬报错，见 mcp 包的协商测试）。
//
// 变异：把 initialize 改成恒回 protocolVersion 常量 → 两个用例都变红。
func TestAgentMCPServerNegotiatesProtocolVersion(t *testing.T) {
	svc, err := v1.NewService(v1.Config{DefaultModel: eino.NewFakeModel([]string{"ok"}, nil)})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s := New(svc)
	initWith := func(version string) string {
		params, _ := json.Marshal(map[string]any{"protocolVersion": version})
		result, rpcErr := s.initialize(params)
		if rpcErr != nil {
			t.Fatalf("initialize(%s): %v", version, rpcErr)
		}
		m, _ := result.(map[string]any)
		v, _ := m["protocolVersion"].(string)
		return v
	}
	if got := initWith("2024-11-05"); got != "2024-11-05" {
		t.Fatalf("server answered %q for a 2024-11-05 request; want the echo", got)
	}
	if got := initWith("1999-01-01"); got != "2025-06-18" {
		t.Fatalf("server answered %q for an unknown request; want its newest 2025-06-18", got)
	}
}
