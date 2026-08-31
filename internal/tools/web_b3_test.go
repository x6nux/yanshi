package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewWebToolsPreservesDefaultsWhenTimeoutAdded(t *testing.T) {
	w := NewWebTools(0, 0)
	if w.maxBytes != 1<<20 {
		t.Fatalf("maxBytes=%d", w.maxBytes)
	}
	w2 := NewWebTools(4096, 0)
	if w2.maxBytes != 4096 {
		t.Fatalf("maxBytes=%d", w2.maxBytes)
	}
	w3 := NewWebTools(0, 30*time.Second)
	if w3.maxBytes != 1<<20 {
		t.Fatalf("maxBytes=%d", w3.maxBytes)
	}
	if w3.timeout != 30*time.Second {
		t.Fatalf("timeout=%v", w3.timeout)
	}
	if w.Search == nil {
		t.Fatalf("Search tool not built")
	}
}

func TestWebFetchMarksOversizeBodyDegraded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 200)))
	}))
	defer srv.Close()
	ctx := WithProfile(context.Background(), profileWithWebTool())
	ctx = WithNetworkPolicy(ctx, allowAllPolicy())
	w := NewWebTools(100, 5*time.Second)
	out, err := w.Fetch.InvokableRun(ctx, `{"url":"`+srv.URL+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "body truncated") {
		t.Fatalf("out=%s", out)
	}
}

// TestWebSearchReturnsEmptyOnUnreachable pins the degradation contract, as
// tightened by W-F-27: an unreachable backend still degrades instead of
// aborting the turn, but the degradation is VISIBLE — status backend_error
// plus a note — rather than an empty result set the model would read as
// "nothing on the web matches".
//
// ledger: B3/T11#4 后端不可用降级
func TestWebSearchReturnsEmptyOnUnreachable(t *testing.T) {
	ctx := WithProfile(context.Background(), profileWithWebTool())
	ctx = WithNetworkPolicy(ctx, allowAllPolicy())
	w := NewWebTools(1<<20, 5*time.Second)
	setSearchEndpoint(w, "http://localhost:0/invalid")
	out, err := w.Search.InvokableRun(ctx, `{"query":"test","max_results":5}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"status":"backend_error"`) {
		t.Fatalf("unreachable backend must degrade VISIBLY, got %s", out)
	}
}

func TestWebSearchRejectsDisallowedHost(t *testing.T) {
	ctx := WithProfile(context.Background(), profileWithWebTool())
	ctx = WithNetworkPolicy(ctx, denyAllPolicy())
	w := NewWebTools(1<<20, 5*time.Second)
	out, err := w.Search.InvokableRun(ctx, `{"query":"test","max_results":5}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "permission denied") {
		t.Fatalf("out=%s", out)
	}
}
