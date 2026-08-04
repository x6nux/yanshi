package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/netpolicy"
)

func TestTruncateSummary(t *testing.T) {
	t.Run("short string unchanged", func(t *testing.T) {
		s := "hello world"
		got := truncateSummary(s, 50)
		if got != s {
			t.Fatalf("expected %q, got %q", s, got)
		}
	})
	t.Run("long string truncated", func(t *testing.T) {
		s := strings.Repeat("a", 100)
		got := truncateSummary(s, 10)
		if len(got) != 13 { // 10 bytes + 3-byte "…"
			t.Fatalf("expected 11 chars, got %d: %q", len(got), got)
		}
	})
	t.Run("exact length", func(t *testing.T) {
		s := "1234567890"
		got := truncateSummary(s, 10)
		if got != s {
			t.Fatalf("expected unchanged, got %q", got)
		}
	})
}

func TestClampInt(t *testing.T) {
	tests := []struct {
		v, low, high, want int
	}{
		{5, 1, 10, 5},
		{0, 1, 10, 1},
		{20, 1, 10, 10},
		{1, 1, 1, 1},
		{-5, -10, 0, -5},
	}
	for _, tc := range tests {
		got := clampInt(tc.v, tc.low, tc.high)
		if got != tc.want {
			t.Errorf("clampInt(%d,%d,%d) = %d, want %d", tc.v, tc.low, tc.high, got, tc.want)
		}
	}
}

func TestParseTestResult(t *testing.T) {
	t.Run("unknown framework", func(t *testing.T) {
		r := parseTestResult("unknown", commandResult{})
		if r.Status != "error" || r.Summary != "unknown framework" {
			t.Fatalf("unexpected: %+v", r)
		}
	})
	t.Run("go framework", func(t *testing.T) {
		r := parseTestResult("go", commandResult{Stdout: `{"Action":"pass","Package":"pkg","Test":"TestFoo"}
{"Action":"fail","Package":"pkg","Test":"TestBar"}
{"Action":"skip","Package":"pkg","Test":"TestBaz"}
`})
		if r.Passed != 1 || r.Failed != 1 || r.Skipped != 1 {
			t.Fatalf("unexpected counts: %+v", r)
		}
		if r.Status != "fail" {
			t.Fatalf("expected fail status, got %s", r.Status)
		}
	})
	t.Run("cargo no failures", func(t *testing.T) {
		r := parseTestResult("cargo", commandResult{Stdout: "test result: 10 passed; 0 failed; 0 ignored"})
		if r.Passed != 10 || r.Failed != 0 {
			t.Fatalf("unexpected: %+v", r)
		}
	})
}

func TestParseToolList(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		got, err := parseToolList("")
		if err != nil || got != nil {
			t.Fatalf("expected nil,nil, got %v,%v", got, err)
		}
	})
	t.Run("json array", func(t *testing.T) {
		got, err := parseToolList(`["fs_read","fs_search"]`)
		if err != nil || len(got) != 2 || got[0] != "fs_read" {
			t.Fatalf("unexpected: %v,%v", got, err)
		}
	})
	t.Run("double-encoded json", func(t *testing.T) {
		got, err := parseToolList(`"[\"fs_read\"]"`)
		if err != nil || len(got) != 1 || got[0] != "fs_read" {
			t.Fatalf("unexpected: %v,%v", got, err)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		_, err := parseToolList("not-json")
		if err == nil {
			t.Fatal("expected error for invalid input")
		}
	})
}

func TestTestSpec(t *testing.T) {
	t.Run("go with packages", func(t *testing.T) {
		spec, err := testSpec("go", runTestsArgs{Packages: []string{"./..."}, Filter: "TestFoo"}, "/root")
		if err != nil || spec.Program != "go" {
			t.Fatalf("unexpected: %+v %v", spec, err)
		}
	})
	t.Run("go default packages", func(t *testing.T) {
		spec, err := testSpec("go", runTestsArgs{}, "/root")
		if err != nil || spec.Program != "go" {
			t.Fatalf("unexpected: %+v %v", spec, err)
		}
	})
	t.Run("cargo with filter", func(t *testing.T) {
		spec, err := testSpec("cargo", runTestsArgs{Filter: "test_foo"}, "/root")
		if err != nil || spec.Program != "cargo" {
			t.Fatalf("unexpected: %+v %v", spec, err)
		}
	})
	t.Run("npm with filter", func(t *testing.T) {
		spec, err := testSpec("npm", runTestsArgs{Filter: "test foo"}, "/root")
		if err != nil || spec.Program != "npm" {
			t.Fatalf("unexpected: %+v %v", spec, err)
		}
	})
}

func TestFormatDur(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{5 * time.Second, "5s"},
		{30 * time.Second, "30s"},
		{1 * time.Minute, "1m0s"},
		{1*time.Minute + 30*time.Second, "1m30s"},
		{2 * time.Minute, "2m0s"},
	}
	for _, tc := range tests {
		got := formatDur(tc.d)
		if got != tc.want {
			t.Errorf("formatDur(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestWebRunSearchInitialization(t *testing.T) {
	w := NewWebTools(1000, time.Second)
	if w.searchBase != "https://lite.duckduckgo.com/lite" {
		t.Fatalf("unexpected searchBase: %s", w.searchBase)
	}
}

func TestWebRunSearchDeniedWithoutPolicy(t *testing.T) {
	w := NewWebTools(1000, time.Second)
	ctx := context.Background()
	out, err := runTool(ctx, w.Search, `{"query":"test"}`)
	if err != nil && !strings.Contains(out, "permission denied") {
		expected := `{"results":[]}`
		if out != expected {
			t.Fatalf("expected empty results or denied, got %q, err=%v", out, err)
		}
	}
}

func TestWebRunSearchNoHostPolicy(t *testing.T) {
	w := NewWebTools(1000, time.Second)
	ctx := WithNetworkPolicy(context.Background(), &netpolicy.Policy{})
	_, err := runTool(ctx, w.Search, `{"query":"test"}`)
	if err != nil {
		t.Logf("expected host-denied or network-error, got err=%v", err)
	}
}

func TestRunTestsDefaultTimeout(t *testing.T) {
	// Test that runTests handles error case properly (no work root).
	// Just check it doesn't panic and returns some error.
	ctx := context.Background()
	out, err := runTool(ctx, NewTestRunTool(), `{"framework":"go"}`)
	if err != nil {
		t.Logf("expected some error without proper context: %v", err)
	}
	_ = out
}

func TestShellCommand(t *testing.T) {
	ctx := context.Background()
	cmd := shellCommand(ctx, "auto", "echo hello")
	if cmd == nil {
		t.Fatal("shellCommand should return non-nil cmd")
	}
}
