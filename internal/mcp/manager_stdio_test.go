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

// knobEnv reads an integer knob from the environment; 0 when absent or
// malformed (knobs are opt-in test choreography, never defaults).
func knobEnv(key string) int {
	if d := os.Getenv(key); d != "" {
		if n, err := strconv.Atoi(d); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

func serveMCPHelper(r io.Reader, w io.Writer) {
	buf := bufio.NewReader(r)
	changed := false
	servedList := false
	resourcesGrew := false
	subscribed := false
	// 两个确定性旋钮（环境变量下发）：
	//   NOTIFY_BEFORE=N —— 处理第 N 个请求前，先同步推一条 list_changed。
	//     主循环内写，与响应无并发写。让「通知先于 ready」成为必然而非竞速。
	//   ANSWER_LIMIT=N —— 答完第 N 个请求后永久 park（不再读不答）。之后的
	//     client 写会永远阻塞 —— 用来让「install 路径上的刷新」可观测：
	//     内联跑就堵死调用方，spawn 出去则调用方照常返回。进程由 Manager
	//     Close→Kill 回收。
	notifyBefore := knobEnv("YANSHI_TEST_MCP_NOTIFY_BEFORE")
	answerLimit := knobEnv("YANSHI_TEST_MCP_ANSWER_LIMIT")
	count := 0
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
		count++
		if notifyBefore > 0 && count == notifyBefore {
			_ = WriteLineMessage(w, map[string]any{
				"jsonrpc": "2.0",
				"method":  "notifications/tools/list_changed",
				"params":  map[string]any{"tools": []any{map[string]any{"name": "pwned"}}},
			})
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
		case "resources/list":
			// 两页目录：首页 [res_a] + nextCursor "p2"，第二页 [res_b]
			//（更新后 [res_b, res_c]）。cursor 语义由此被端到端钉住：只在
			// 请求带 cursor=p2 时才给第二页。
			cursor := ""
			if p, ok := msg["params"].(map[string]any); ok {
				cursor, _ = p["cursor"].(string)
			}
			if cursor != "p2" {
				page := []map[string]any{{"uri": "file:///res_a", "name": "res_a"}}
				// 首页恒带 nextCursor：目录翻页结构不随更新变化，增长只发生
				// 在第二页（否则 walk 会在首页提前终止，测不到第二页刷新）。
				resp["result"] = map[string]any{"resources": page, "nextCursor": "p2"}
			} else {
				page := []map[string]any{{"uri": "file:///res_b", "name": "res_b"}}
				if resourcesGrew {
					page = append(page, map[string]any{"uri": "file:///res_c", "name": "res_c"})
				}
				resp["result"] = map[string]any{"resources": page}
			}
		case "resources/subscribe":
			uri := ""
			if p, ok := msg["params"].(map[string]any); ok {
				uri, _ = p["uri"].(string)
			}
			if uri == "file:///res_a" {
				subscribed = true
				resp["result"] = map[string]any{}
				// 因果链：只有真的收到了订阅请求，server 才会（按订阅语义）
				// 推送变更通知并切换目录。Manager 不自动订阅 → 这里永远不跑
				// → resourceMap 停在两页旧目录 → 测试按 deadline 变红。
				go func() {
					time.Sleep(150 * time.Millisecond)
					resourcesGrew = true
					// 不可信 uri 载荷：自称变更的是 file:///attacker。守约的
					// Manager 只把它当「该重列目录」的信号，绝不按这个 uri
					// 去 ReadResource，也绝不把它当资源收进 resourceMap。
					_ = WriteLineMessage(w, map[string]any{
						"jsonrpc": "2.0",
						"method":  "notifications/resources/updated",
						"params":  map[string]any{"uri": "file:///attacker"},
					})
				}()
			} else {
				resp["error"] = map[string]any{"code": -32602, "message": "not subscribed"}
			}
		case "resources/read":
			// Manager 永不该走到这里：updated 刷新一律整目录重列，不按通知
			// 里的 uri 取读。真走到 = 消费端把不可信 uri 当成了取数地址。
			resp["error"] = map[string]any{"code": -32601, "message": "read must not happen"}
		default:
			resp["error"] = map[string]any{"code": -32601, "message": "method not found"}
		}
		_ = subscribed // 订阅状态只经因果链表达（推送与目录翻转），无需上报
		if err := WriteLineMessage(w, resp); err != nil {
			return
		}
		if answerLimit > 0 && count >= answerLimit {
			// park：此后不再读不答。必须留一个长 timer 作 keepalive —— 裸
			// select{} 会在最后一个 timer goroutine 退出后触发 runtime 的
			// 死锁检测器（「all goroutines are asleep」）把进程整个杀掉，
			// client 侧随后的写拿到 broken pipe、阻塞测试假绿（RF-9 修复
			// 时实测）。进程由 Manager Close→Kill 回收，或 10 分钟后 go
			// test 的超时兜底。
			go func() { time.Sleep(10 * time.Minute) }()
			select {}
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
			delay := knobEnv("YANSHI_TEST_MCP_PUSH_DELAY_MS")
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

// W-F-04 全链路：自动订阅 → server 推 resources/updated → Manager 整目录
// 重列 → resourceMap 反映最新目录（/mcp 状态面因此不再是启动快照）。
//
// 断言四个方向：
//   - 收敛后 res_c 在 resourceMap 里 —— 订阅因果链完整（helper 只在收到
//     subscribe 后才推通知并翻页）；分页 walk 也被钉住（res_b 只在第二页，
//     只有带 cursor=p2 的请求才拿得到）；
//   - resMap 恰好是 a/b/c —— 旧目录被替换，不累积；
//   - attacker（通知载荷自称变更的 uri）永远不是资源 —— 不可信 uri 只当
//     信号（与 listChanged 的标注同一约定，这是 resources 半的机器判据）；
//   - helper 对 resources/read 一律回错 —— Manager 真按载荷 uri 取读的话
//     不会有任何资源变化能掩盖它，且 walk 路径根本不产生 read。
//
// 变异：删掉 startOne 资源块里的 SubscribeResource 循环 → helper 永不推送，
// res_c 永不出现，变红；删掉 handleServerPush 的 resources/updated 分支 →
// 通知被丢弃，同样红；把 refreshResources 改成 ReadResource(载荷 uri) →
// helper 回错 + res_c 不出现，红。
func TestManagerStdioResourceUpdatesRefreshCatalog(t *testing.T) {
	m := NewManager(map[string]*ServerConfig{
		"rsrv": {
			Name: "rsrv", Enabled: true, Transport: TransportStdio,
			Command: os.Args[0],
			Args:    []string{"-test.run=^TestMCPHelperServerProcess$", "--"},
			Env:     map[string]string{"YANSHI_TEST_MCP_SERVER": "1"},
		},
	})
	if st := m.StartAll(context.Background()); len(st) != 1 || st[0].Status != StatusReady {
		t.Fatalf("status=%+v", st)
	}
	// 启动快照只有两页旧目录（a、b）。
	m.mu.Lock()
	startResources := m.resourceMap["rsrv"]
	m.mu.Unlock()
	if len(startResources) != 2 {
		t.Fatalf("startup resource snapshot = %+v; want the two-page catalog [a b]", startResources)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		m.mu.Lock()
		resources := m.resourceMap["rsrv"]
		m.mu.Unlock()
		uris := map[string]bool{}
		for _, rd := range resources {
			uris[rd.URI] = true
		}
		if uris["file:///res_c"] {
			if len(resources) != 3 {
				t.Fatalf("resources after update = %+v; the refreshed catalog must replace, not accumulate", resources)
			}
			for _, rd := range resources {
				if rd.URI == "file:///attacker" {
					t.Fatal("attacker URI from the notification payload became a resource; the payload is a signal, not a catalog")
				}
			}
			return
		}
		if uris["file:///attacker"] {
			t.Fatal("attacker URI from the notification payload became a resource; the payload is a signal, not a catalog")
		}
		if time.Now().After(deadline) {
			t.Fatalf("resource catalog never refreshed after subscribe+update; resources=%+v", resources)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// RF-9 守护：installReadyClient 的 drain spawn 点 —— pending 的 list_changed
// 在 client 安装点放行时，刷新必须离开 install 调用方的 goroutine。
//
// 剧本（确定性，无竞速）：helper 在处理第 3 个请求（resources/list 首页）
// 之前同步推 list_changed —— 此刻 startOne 还堵在等该页响应，ready 必然为
// false，pending 必然置位；答完第 5 个请求（resources/subscribe）后 park，
// 不再读不答。于是 install 点的 drain 必然为 true，而那次刷新的 tools/list
// 请求写进一个没人读的管道。
//
// 断言：StartAll 必须返回。干净实现下 drain 是 goroutine，install 立即返回；
// 变异（drain 去掉 go）下刷新内联跑在 StartAll 的 goroutine 上，写永久阻塞
// —— StartAll 永不返回，按 8s 上限红。park 后写阻塞无超时（io.Pipe 同步写
// 不走 doRequest 的 deadline），红是确定的不是概率的。
//
// 变异：把 installReadyClient 里 go m.refreshTools(name) 的 go 去掉 →
// 本测试 8s 红（「StartAll did not return ...」）。
func TestManagerStdioListChangedDrainDoesNotBlockInstall(t *testing.T) {
	m := NewManager(map[string]*ServerConfig{
		"drain": {
			Name: "drain", Enabled: true, Transport: TransportStdio,
			Command: os.Args[0],
			Args:    []string{"-test.run=^TestMCPHelperServerProcess$", "--"},
			Env: map[string]string{
				"YANSHI_TEST_MCP_SERVER":        "1",
				"YANSHI_TEST_MCP_NOTIFY_BEFORE": "3",
				"YANSHI_TEST_MCP_ANSWER_LIMIT":  "5",
			},
		},
	})
	t.Cleanup(func() { m.Shutdown() }) // 关 pipe：解锁 park 服务器堵住的写，goroutine 得以退出

	done := make(chan struct{})
	go func() {
		m.StartAll(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("StartAll did not return; a pending list_changed drained at install must spawn its refresh, not run it on the install path")
	}
	m.mu.Lock()
	st := m.status["drain"]
	m.mu.Unlock()
	if st != StatusReady {
		t.Fatalf("drain server status = %v; the choreographed handshake must complete", st)
	}
}
