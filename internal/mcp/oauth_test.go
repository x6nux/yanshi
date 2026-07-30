package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientCredentialsSource_CachesAndInvalidates(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.Form.Get("grant_type") != "client_credentials" {
			t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
		}
		if r.Form.Get("client_id") != "client" || r.Form.Get("client_secret") != "secret" {
			t.Errorf("credentials missing: %v", r.Form)
		}
		n := calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "token-" + string(rune('0'+n)),
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}))
	defer ts.Close()

	src := NewClientCredentialsSource(ts.URL, "client", "secret", []string{"tools"}, ts.Client())
	first, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token first: %v", err)
	}
	second, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token cached: %v", err)
	}
	if first != second || calls.Load() != 1 {
		t.Fatalf("cache miss: first=%q second=%q calls=%d", first, second, calls.Load())
	}
	src.Invalidate()
	third, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token after invalidate: %v", err)
	}
	if third == first || calls.Load() != 2 {
		t.Fatalf("invalidate did not refetch: first=%q third=%q calls=%d", first, third, calls.Load())
	}
}

func TestClientCredentialsSource_RefreshesBeforeExpiry(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "short",
			"token_type":   "Bearer",
			"expires_in":   1,
		})
	}))
	defer ts.Close()

	src := NewClientCredentialsSource(ts.URL, "c", "s", nil, ts.Client())
	src.now = func() time.Time { return time.Unix(100, 0) }
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 30 秒 safety skew 大于 1 秒寿命，因此第二次必须立即刷新。
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("short token should refresh, calls=%d", calls.Load())
	}
}
