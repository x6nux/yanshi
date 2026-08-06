package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/secproc"
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
	// The lite endpoint became an anti-bot page returning no results; the
	// html endpoint is what serves results to a POSTed form (W6/T11).
	if w.searchBase != "https://html.duckduckgo.com/html/" {
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

// TestWebRunSearchNoHostPolicy pins that an EMPTY netpolicy.Policy denies the
// search host instead of falling through to the network.
//
// The empty Policy is the fail-closed case that matters: Default is "" and
// netpolicy.Policy.CheckHost treats every value other than an exact "allow" as
// deny, so web_search must refuse BEFORE building a request. The previous
// version of this test logged whatever came back and asserted nothing, so it
// passed identically whether the policy was consulted, ignored, or absent —
// including the one outcome it exists to forbid, a real outbound request.
func TestWebRunSearchNoHostPolicy(t *testing.T) {
	w := NewWebTools(1000, time.Second)
	// The profile must ALLOW the tool, or the guard denies first and the
	// assertion below would hold no matter what the network policy said.
	ctx := WithNetworkPolicy(WithProfile(context.Background(), allowAllProfile()), &netpolicy.Policy{})
	out, err := runTool(ctx, w.Search, `{"query":"test"}`)
	// GuardedTool converts a DenyErr into a result STRING with a nil error, so
	// that the model can read the refusal instead of the turn aborting — the
	// denial is observable on out, not on err.
	if err != nil {
		t.Fatalf("a denial must be reported as a result, not a Go error: %v", err)
	}
	// The reason pins WHICH layer refused: netpolicy's default-deny, not the
	// guard (the profile above allows the tool and the host). Without it, a
	// regression that dropped the policy check would still look denied.
	if want := "permission denied: host denied by default"; !strings.Contains(out, want) {
		t.Fatalf("empty policy must refuse via netpolicy default-deny (%q), got %q", want, out)
	}
	if strings.Contains(out, `"results"`) {
		t.Fatalf("a denied search must not return a result set, got %q", out)
	}
}

// TestRunTestsDefaultTimeout drives the timeout run_tests hands to the process
// runner, which is the only thing its name ever promised.
//
// It used to invoke the tool with no work root, log the error and drop the
// output on the floor (`_ = out`) — the t.Logf-swallows-the-error shape the
// review checklist calls out by name. Nothing about a timeout was observable,
// so the 1-second-default regression that testrun.go's own comment records
// (clampInt(0, 1, 1800) collapsing the default to one second) would have left
// it green.
//
// Stubbing secureCommandRunner is what makes the timeout observable: it is the
// single seam the value travels through, and no subprocess is started.
func TestRunTestsDefaultTimeout(t *testing.T) {
	var got []time.Duration
	orig := secureCommandRunner
	secureCommandRunner = func(_ context.Context, _ secproc.SecureProcessSpec, d time.Duration) (commandResult, error) {
		got = append(got, d)
		return commandResult{Stdout: `{"Action":"pass","Package":"p","Test":"T"}` + "\n"}, nil
	}
	t.Cleanup(func() { secureCommandRunner = orig })

	for _, tc := range []struct {
		name string
		args string
		want time.Duration
	}{
		{"omitted defaults to ten minutes", `{"framework":"go"}`, 10 * time.Minute},
		{"zero is not clamped to one second", `{"framework":"go","timeout_s":0}`, 10 * time.Minute},
		{"explicit value wins", `{"framework":"go","timeout_s":30}`, 30 * time.Second},
		{"above the ceiling clamps to 1800s", `{"framework":"go","timeout_s":9999}`, 1800 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got = nil
			ctx := WithProfile(context.Background(), allowAllProfile())
			if _, err := runTool(ctx, NewTestRunTool(), tc.args); err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("expected exactly one runner invocation, got %d", len(got))
			}
			if got[0] != tc.want {
				t.Fatalf("run_tests handed the runner %v, want %v", got[0], tc.want)
			}
		})
	}

	// The tool's declared DefaultTimeout is what the orchestrator budgets with,
	// and it must not drift away from the value above.
	if d := NewTestRunTool().DefaultTimeout(); d != 10*time.Minute {
		t.Fatalf("run_tests DefaultTimeout = %v, want 10m", d)
	}
}

func TestShellCommand(t *testing.T) {
	ctx := context.Background()
	cmd := shellCommand(ctx, "auto", "echo hello")
	if cmd == nil {
		t.Fatal("shellCommand should return non-nil cmd")
	}
}
