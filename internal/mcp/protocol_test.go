package mcp

import (
	"context"
	"strings"
	"testing"
)

// 协商矩阵：server 带回的三版都采纳；空应答与未知版本都是硬错误。
// 变异：把 negotiateProtocolVersion 里 !supportedProtocolVersions[v] 的分支
// 改成「返回 preferred 降级继续」→ 本测试的三个 reject 用例全部变红
// （静默降级正是这条要拦的行为）。
func TestNegotiateProtocolVersionMatrix(t *testing.T) {
	for _, v := range []string{"2025-06-18", "2025-03-26", "2024-11-05"} {
		got, err := negotiateProtocolVersion(v)
		if err != nil || got != v {
			t.Fatalf("negotiate(%q) = %q, %v; want %q, nil", v, got, err, v)
		}
	}
	for _, v := range []string{"", "   ", "1999-01-01", "2024-10-07"} {
		got, err := negotiateProtocolVersion(v)
		if err == nil {
			t.Fatalf("negotiate(%q) = %q, nil; want an error", v, got)
		}
		if got != "" {
			t.Fatalf("negotiate(%q) returned version %q alongside an error", v, got)
		}
	}
}

// drainInitialized 是 handshake 的容错变体：消费 initialize 请求并应答，
// 然后排空到 initialized 通知为止，但 Initialize 的错误由调用方（测试
// goroutine）自己拿到 —— 握手被预期失败时 handshake 的 t.Fatalf 用不了。
// 必须先起这个 goroutine 再调 Initialize：io.Pipe 是同步的，没人读时
// client 的第一个写会永远阻塞。
func drainInitialized(srv *bidirServer) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.handleOne()
		for {
			srv.mu.Lock()
			msg, err := ReadMessage(srv.buf)
			srv.mu.Unlock()
			if err != nil {
				return
			}
			if m, _ := msg["method"].(string); m == "notifications/initialized" {
				return
			}
		}
	}()
	return done
}

// stdio 半：server 应答 2024-11-05 时 Initialize 成功，且会话落在这版上；
// 应答未知版本时 Initialize 原地报错（不静默降级到我们请求的那版）。
//
// 变异：删掉 Initialize 里的 negotiateProtocolVersion 调用（连校验一起）→
// 两个用例都变红：不支持的版本不再报错，c.protocol 也不再记录应答值。
func TestStdioClient_NegotiatesProtocolVersion(t *testing.T) {
	t.Run("server offers 2024-11-05", func(t *testing.T) {
		srv, cli := newBidirServer(t)
		srv.protoVersion = "2024-11-05"
		srv.handshake(cli)
		if cli.protocol != "2024-11-05" {
			t.Fatalf("session protocol = %q, want the server's 2024-11-05", cli.protocol)
		}
	})
	t.Run("server offers an unknown version", func(t *testing.T) {
		srv, cli := newBidirServer(t)
		srv.protoVersion = "1999-01-01"
		done := drainInitialized(srv)
		err := cli.Initialize(context.Background(), "/")
		if err == nil {
			t.Fatal("Initialize succeeded against an unsupported protocol version; want a hard error")
		}
		if !strings.Contains(err.Error(), "1999-01-01") {
			t.Fatalf("error %v does not name the offending version", err)
		}
		// 不等 done：失败路径上 client 不会发 initialized，drainInitialized
		// 会一直阻塞在读上（与套件里其他测试留下的 readLoop 泄漏同形）。
		_ = done
	})
}

// HTTP 半：协商出的版本作为 MCP-Protocol-Version 头出现在后续请求上
// （2025-03-26 起的契约），initialize 本身不带（那时还没协商出来）。
//
// 变异：删掉 post 里 version != "" 的 SetHeader 两行 → 本测试变红。
func TestHTTPClient_SendsNegotiatedProtocolVersionHeader(t *testing.T) {
	ts, fake := NewFakeHTTPServer(nil)
	defer ts.Close()
	fake.ProtocolVersion = "2025-03-26"
	cli := NewHTTPClient(ts.URL, "")
	if err := cli.Initialize(context.Background(), "/"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := cli.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if got := fake.InitProtocolHeader; got != "" {
		t.Fatalf("initialize request already carried %q; the header belongs to post-handshake requests only", got)
	}
	if got := fake.LastProtocolHeader; got != "2025-03-26" {
		t.Fatalf("MCP-Protocol-Version after handshake = %q, want the negotiated 2025-03-26", got)
	}
}

// HTTP 半（失败方向）：server 应答未知版本 → Initialize 报错，且报错里
// 点名版本。
func TestHTTPClient_RejectsUnknownProtocolVersion(t *testing.T) {
	ts, fake := NewFakeHTTPServer(nil)
	defer ts.Close()
	fake.ProtocolVersion = "2024-10-07"
	cli := NewHTTPClient(ts.URL, "")
	err := cli.Initialize(context.Background(), "/")
	if err == nil {
		t.Fatal("Initialize succeeded against an unsupported protocol version; want a hard error")
	}
	if !strings.Contains(err.Error(), "2024-10-07") {
		t.Fatalf("error %v does not name the offending version", err)
	}
}
