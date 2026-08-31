package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// redirectProbe records the credential-shaped headers a hop received.
type redirectProbe struct {
	mu   sync.Mutex
	auth string
	sess string
}

func (p *redirectProbe) observe(r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.auth = r.Header.Get("Authorization")
	p.sess = r.Header.Get("Mcp-Session-Id")
}

func (p *redirectProbe) answer(w http.ResponseWriter, r *http.Request) {
	p.observe(r)
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	w.Header().Set("Mcp-Session-Id", "sess-1")
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	switch req.Method {
	case "initialize":
		resp["result"] = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
	case "tools/list":
		resp["result"] = map[string]any{"tools": []any{}}
	default:
		w.WriteHeader(http.StatusAccepted)
		return
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (p *redirectProbe) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { p.answer(w, r) }
}

func (p *redirectProbe) values() (auth, sess string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.auth, p.sess
}

// 同源 307：Authorization 与 Mcp-Session-Id 照常到达（重定向限制只剪跨源，
// 不剪同源 —— API 主机内部的 307 是正常拓扑）。
//
// 变异：把 checkRedirect 的跨源条件改成「一律 strip」→ 本测试变红
//（同源跳也丢了凭据）。
func TestHTTPClient_SameOriginRedirectKeepsCredentials(t *testing.T) {
	probe := &redirectProbe{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/echo", http.StatusTemporaryRedirect)
			return
		}
		probe.answer(w, r)
	}))
	defer ts.Close()

	cli := NewHTTPClient(ts.URL+"/start", "secret-token")
	if err := cli.Initialize(context.Background(), "/"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := cli.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	auth, sess := probe.values()
	if auth != "Bearer secret-token" {
		t.Fatalf("same-origin redirect lost Authorization: got %q", auth)
	}
	if sess != "sess-1" {
		t.Fatalf("same-origin redirect lost Mcp-Session-Id: got %q", sess)
	}
}

// 跨源 307（不同端口即不同 origin）：凭据不得跟过去。对照组断言凭据确实
// 在起点上被发出过，否则「B 处为空」什么都没证。
//
// 变异：删掉 checkRedirect 里的三行 Del → 本测试变红
//（Authorization 走 stdlib 的剥离侥幸为空，Mcp-Session-Id 必然泄漏 ——
// 这正是 stdlib 默认策略只护 Authorization/Cookie 留下的缝）。
func TestHTTPClient_CrossOriginRedirectStripsCredentials(t *testing.T) {
	target := &redirectProbe{}
	// 先起 B：A 的 handler 在请求时才求值 tsB.URL，但先创建能让顺序可读。
	tsB := httptest.NewServer(target.handler())
	defer tsB.Close()

	origin := &redirectProbe{}
	tsA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin.observe(r)
		http.Redirect(w, r, tsB.URL+"/echo", http.StatusTemporaryRedirect)
	}))
	defer tsA.Close()

	cli := NewHTTPClient(tsA.URL, "secret-token")
	if err := cli.Initialize(context.Background(), "/"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := cli.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	auth, _ := origin.values()
	if auth != "Bearer secret-token" {
		t.Fatalf("control: origin hop did not even see the bearer (%q) — the probe is broken", auth)
	}
	crossAuth, crossSess := target.values()
	if crossAuth != "" {
		t.Fatalf("cross-origin redirect carried Authorization: %q", crossAuth)
	}
	if crossSess != "" {
		t.Fatalf("cross-origin redirect carried Mcp-Session-Id: %q", crossSess)
	}
}

// 太多跳：超过上限的重定向链报错，而不是跟着 stdlib 的 10 跳默认值打满。
//
// 变异：删掉 checkRedirect 的 len(via) 上限分支 → 本测试变红（链会走满
// stdlib 默认 10 跳后以 stdlib 的报错文案失败，而不是 ours）。
func TestHTTPClient_RedirectChainCapped(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/hop", http.StatusTemporaryRedirect)
	}))
	defer ts.Close()

	cli := NewHTTPClient(ts.URL, "")
	_, err := cli.ListTools(context.Background())
	if err == nil {
		t.Fatal("uncapped redirect chain succeeded; want an error")
	}
	if !strings.Contains(err.Error(), "stopped after 5 redirects") {
		t.Fatalf("error %v does not name our redirect cap", err)
	}
}
