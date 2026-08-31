package mcp

import (
	"bufio"
	"context"
	"io"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestMCPHelperServerProcess 是 Manager stdio 集成测试的 re-exec helper：
// 在子进程里扮演一个会说 newline-delimited JSON-RPC 的 MCP server。
// 仅当 YANSHI_TEST_MCP_SERVER 被设置时才真正运行（该标记经
// ServerConfig.Env → stdioServerEnv 传给它）；作为普通测试跑时直接跳过。
//
// 剧本（为 RF-2 的时序而设计）：
//  1. 应答 initialize；
//  2. 应答第一次 tools/list，目录为 [echo]；
//  3. 随后立即在线上推 notifications/tools/list_changed —— params 里带着
//     攻击者形状的自称目录（"pwned"），用于钉「params 只是信号，载荷里的
//     目录内容绝不进 toolMap」这条标注约定；
//  4. 之后的 tools/list 返回 [echo, extra]。
//
// 于是客户端必然先读到旧目录的响应、再读到通知 —— 正是 pending/ready
// 机制要正确处理的乱序窗口。
func TestMCPHelperServerProcess(t *testing.T) {
	if os.Getenv("YANSHI_TEST_MCP_SERVER") == "" {
		t.Skip("re-exec helper for TestManagerStdioListChangedRefreshesToolTable; not a test")
	}
	serveMCPHelper(os.Stdin, os.Stdout)
}

func serveMCPHelper(r io.Reader, w io.Writer) {
	buf := bufio.NewReader(r)
	changed := false
	servedList := false
	for {
		msg, err := ReadMessage(buf)
		if err != nil {
			return
		}
		id := msg["id"]
		method, _ := msg["method"].(string)
		if id == nil {
			continue // notifications/initialized etc.
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": id}
		tools := []map[string]any{{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}
		if changed {
			tools = append(tools, map[string]any{"name": "extra", "inputSchema": map[string]any{"type": "object"}})
		}
		switch method {
		case "initialize":
			resp["result"] = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
		case "tools/list":
			resp["result"] = map[string]any{"tools": tools}
		case "ping":
			resp["result"] = map[string]any{}
		default:
			resp["error"] = map[string]any{"code": -32601, "message": "method not found"}
		}
		if err := WriteLineMessage(w, resp); err != nil {
			return
		}
		if method == "tools/list" && !servedList {
			servedList = true
			changed = true
			// 推送延迟由环境变量给出：0（默认）= 紧跟首轮 tools/list 响应
			// 之后 —— 通知几乎必然先于 Manager 的 ready 置位到达，覆盖
			// pending 缓冲路径；>0 = 从旁路 goroutine 等待若干毫秒再推 ——
			// Manager 早已 ready，覆盖直接 spawn goroutine 的路径。延迟必须
			// 不占住 serve 主循环：io.Pipe 是同步的，主循环停读期间 client
			// 的下一个请求会阻塞，startOne 就被拖到通知之后，spawn 路径
			// 永远等不到。窗口内 client 对本连接无其他写，旁路写不撕帧。
			delay := 0
			if d := os.Getenv("YANSHI_TEST_MCP_PUSH_DELAY_MS"); d != "" {
				if n, err := strconv.Atoi(d); err == nil && n > 0 {
					delay = n
				}
			}
			go func() {
				if delay > 0 {
					time.Sleep(time.Duration(delay) * time.Millisecond)
				}
				// 不可信载荷：自称的目录内容。守约的客户端只拿它当「该刷新了」
				// 的信号，绝不把 pwned 注册进工具表。
				_ = WriteLineMessage(w, map[string]any{
					"jsonrpc": "2.0",
					"method":  "notifications/tools/list_changed",
					"params":  map[string]any{"tools": []any{map[string]any{"name": "pwned"}}},
				})
			}()
		}
	}
}

// Manager 全链路（F1 移交 RF-2 的核收验收）：stdio server 推
// list_changed → readLoop 分发到 Manager 注册的 handler → 工具表刷新。
// 两种到达时序各跑一遍：
//   - 立即推送（pending 缓冲路径：通知先于首轮 merge 完成）；
//   - 延迟推送（spawn 路径：通知到达时 Manager 早已 ready）。
//
// 断言三个方向：
//   - 收敛后 extra 在表里 —— 通知被消费、目录被重新拉取；
//   - pwned 永远不在表里 —— 不可信 params 只是信号（ServerHandler 标注
//     约定的机器判据，AC 由本测试兑现）；
//   - 若 handler 内联跑刷新（refreshTools 不 spawn goroutine），readLoop
//     会自己等自己的 tools/list 响应，30s 客户端超时内不可能收敛，延迟
//     用例按 deadline 变红。
//
// 变异：把 handleServerPush 里的 go m.refreshTools(...) 改成内联调用 →
// 延迟用例超时变红；删掉 startOne 的 SetHandler 绑定 → 两个用例的 extra
// 都永不出现，变红。
func TestManagerStdioListChangedRefreshesToolTable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pushInMs string
	}{
		{"push before ready (pending path)", ""},
		{"push after ready (spawn path)", "300"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := "helper"
			if tc.pushInMs != "" {
				srv = "helperlate"
			}
			m := NewManager(map[string]*ServerConfig{
				srv: {
					Name: srv, Enabled: true, Transport: TransportStdio,
					Command: os.Args[0],
					Args:    []string{"-test.run=^TestMCPHelperServerProcess$", "--"},
					Env: map[string]string{
						"YANSHI_TEST_MCP_SERVER":        "1",
						"YANSHI_TEST_MCP_PUSH_DELAY_MS": tc.pushInMs,
					},
				},
			})
			st := m.StartAll(context.Background())
			if len(st) != 1 || st[0].Status != StatusReady {
				t.Fatalf("status=%+v", st)
			}
			// 不在此处断言「初始只有 echo」：刷新 goroutine 与 StartAll 返回
			// 是并发的（-race 下刷新常先收敛），wire 上「响应先于通知」的
			// 时序由 helper 的剧本钉住，Manager 侧只需对收敛终态负责。

			deadline := time.Now().Add(10 * time.Second)
			for {
				tools, err := m.ListAllTools(context.Background())
				if err != nil {
					t.Fatalf("ListAllTools: %v", err)
				}
				names := map[string]bool{}
				for _, td := range tools {
					names[td.Qualified] = true
				}
				if names["mcp_"+srv+"_extra"] {
					if names["mcp_"+srv+"_pwned"] {
						t.Fatal("attacker-named tool from the notification payload registered; params must be treated as an untrusted signal, not a catalog")
					}
					if len(tools) != 2 {
						t.Fatalf("tools after refresh = %+v; stale entries must be replaced, not accumulated", tools)
					}
					return
				}
				if names["mcp_"+srv+"_pwned"] {
					t.Fatal("attacker-named tool from the notification payload registered; params must be treated as an untrusted signal, not a catalog")
				}
				if time.Now().After(deadline) {
					t.Fatalf("tool table never refreshed after list_changed; tools=%+v", tools)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

// list_changed 之后，reload（Disable→Enable）拉到的必须是刷新后的目录：
// refreshTools 先失效旧条目再写入新目录（否则配置 A→B→A 往返会让改动前
// 的陈旧条目复活）。
//
// 变异：删掉 refreshTools 里的 catalogInvalidateServer → 本测试在缓存命中
// 下拿到改动前的 [echo]，extra 消失，变红。
func TestManagerStdioListChangedSurvivesReload(t *testing.T) {
	m := NewManager(map[string]*ServerConfig{
		"helper2": {
			Name: "helper2", Enabled: true, Transport: TransportStdio,
			Command: os.Args[0],
			Args:    []string{"-test.run=^TestMCPHelperServerProcess$", "--"},
			Env:     map[string]string{"YANSHI_TEST_MCP_SERVER": "1"},
		},
	})
	m.StartAll(context.Background())
	deadline := time.Now().Add(10 * time.Second)
	for {
		tools, _ := m.ListAllTools(context.Background())
		if len(tools) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tools never refreshed: %+v", tools)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := m.Disable(context.Background(), "helper2"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := m.Enable(context.Background(), "helper2"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	tools, _ := m.ListAllTools(context.Background())
	if len(tools) != 2 {
		t.Fatalf("tools after reload = %+v; the post-change catalog must be served, not a resurrected stale entry", tools)
	}
}
