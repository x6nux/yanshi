package eino

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestParseQuotaWindowsRealCodexHeaders pins the parser against the actual
// header names OpenAI Codex CLI's backend sends (grounded via
// NousResearch/hermes-agent#9085, since this repo has no network access to
// observe them directly — see quota.go's header comment). If these literal
// names ever drift, this is the test that must be updated, not the parser's
// pattern.
func TestParseQuotaWindowsRealCodexHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "42.5")
	h.Set("x-codex-primary-window-minutes", "300")
	h.Set("x-codex-primary-reset-after-seconds", "1800")
	h.Set("x-codex-secondary-used-percent", "10")
	h.Set("x-codex-secondary-window-minutes", "10080")
	h.Set("x-codex-secondary-reset-after-seconds", "604800")

	got := ParseQuotaWindows(h)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 windows (primary, secondary): %+v", len(got), got)
	}
	primary, ok := got["primary"]
	if !ok {
		t.Fatal("missing primary window")
	}
	if primary.UsedPercent != 42.5 || primary.WindowMinutes != 300 || primary.ResetAfter != 1800*time.Second {
		t.Errorf("primary = %+v, want {42.5 300 30m0s}", primary)
	}
	secondary, ok := got["secondary"]
	if !ok {
		t.Fatal("missing secondary window")
	}
	if secondary.UsedPercent != 10 || secondary.WindowMinutes != 10080 || secondary.ResetAfter != 604800*time.Second {
		t.Errorf("secondary = %+v, want {10 10080 168h0m0s}", secondary)
	}
}

// TestParseQuotaWindowsGenericProviderWildcard proves the provider segment is
// truly wildcarded (matching the spec's own "x-*-primary-used-percent"
// notation) and not secretly hardcoded to "codex" — a differently-named
// provider using the same "x-<provider>-<window>-<field>" convention must
// still parse.
func TestParseQuotaWindowsGenericProviderWildcard(t *testing.T) {
	h := http.Header{}
	h.Set("X-Acme-Primary-Used-Percent", "99")
	h.Set("X-Acme-Primary-Reset-After-Seconds", "5")

	got := ParseQuotaWindows(h)
	w, ok := got["primary"]
	if !ok {
		t.Fatalf("windows = %+v, want a primary window from a non-codex provider prefix", got)
	}
	if w.UsedPercent != 99 || w.ResetAfter != 5*time.Second {
		t.Errorf("window = %+v, want {99 0 5s}", w)
	}
}

// TestParseQuotaWindowsRequiresUsedAndReset pins that a window missing either
// used-percent or reset-after-seconds is dropped rather than returned
// half-populated (window-minutes alone, or used-percent with no reset-after,
// describes nothing a quotaGovernor can act on).
func TestParseQuotaWindowsRequiresUsedAndReset(t *testing.T) {
	cases := []struct {
		name string
		h    http.Header
	}{
		{"window-minutes only", http.Header{"X-Codex-Primary-Window-Minutes": {"300"}}},
		{"used-percent without reset-after", http.Header{"X-Codex-Primary-Used-Percent": {"50"}}},
		{"reset-after without used-percent", http.Header{"X-Codex-Primary-Reset-After-Seconds": {"5"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseQuotaWindows(tc.h)
			if len(got) != 0 {
				t.Errorf("windows = %+v, want none", got)
			}
		})
	}
}

// TestParseQuotaWindowsIgnoresUnrelatedHeaders proves the regex does not
// over-match ordinary headers that merely start with "x-" or contain dashes.
func TestParseQuotaWindowsIgnoresUnrelatedHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-Request-Id", "abc-123")
	h.Set("Content-Type", "application/json")
	h.Set("Retry-After", "5")
	if got := ParseQuotaWindows(h); len(got) != 0 {
		t.Errorf("windows = %+v, want none from unrelated headers", got)
	}
}

// TestParseQuotaWindowsEmptyOrUnparsableValue pins that a non-numeric value
// is dropped rather than panicking or zero-filling.
func TestParseQuotaWindowsEmptyOrUnparsableValue(t *testing.T) {
	h := http.Header{}
	h.Set("X-Codex-Primary-Used-Percent", "not-a-number")
	h.Set("X-Codex-Primary-Reset-After-Seconds", "5")
	if got := ParseQuotaWindows(h); len(got) != 0 {
		t.Errorf("windows = %+v, want none: used-percent failed to parse", got)
	}
}

// TestParseQuotaWindowsNilAndEmpty pins the zero-header cases return nil, not
// a non-nil empty map — matching len(h)==0's early return.
func TestParseQuotaWindowsNilAndEmpty(t *testing.T) {
	if got := ParseQuotaWindows(nil); got != nil {
		t.Errorf("nil header = %+v, want nil", got)
	}
	if got := ParseQuotaWindows(http.Header{}); got != nil {
		t.Errorf("empty header = %+v, want nil", got)
	}
}

// TestWithQuotaObserverNilIsNoop pins that binding a nil observer leaves ctx
// unchanged, so observeQuotaHeaders's ok==false path (and every call site
// that does WithQuotaObserver(ctx, nil) when Limiter is nil) needs no special
// case.
func TestWithQuotaObserverNilIsNoop(t *testing.T) {
	ctx := context.Background()
	got := WithQuotaObserver(ctx, nil)
	if _, ok := quotaObserverFromContext(got); ok {
		t.Error("a nil observer was still found bound to the context")
	}
}

// TestObserveQuotaHeaders pins the full round trip: bind an observer, feed a
// header set through observeQuotaHeaders, see it delivered — and pins that no
// bound observer, or a header set with nothing to report, never invokes it.
func TestObserveQuotaHeaders(t *testing.T) {
	t.Run("no bound observer is a silent no-op", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Codex-Primary-Used-Percent", "50")
		h.Set("X-Codex-Primary-Reset-After-Seconds", "5")
		observeQuotaHeaders(context.Background(), h) // must not panic
	})

	t.Run("bound observer receives parsed windows", func(t *testing.T) {
		var got map[string]QuotaWindow
		ctx := WithQuotaObserver(context.Background(), func(windows map[string]QuotaWindow) {
			got = windows
		})
		h := http.Header{}
		h.Set("X-Codex-Primary-Used-Percent", "50")
		h.Set("X-Codex-Primary-Reset-After-Seconds", "5")
		observeQuotaHeaders(ctx, h)
		if got == nil {
			t.Fatal("observer was never called")
		}
		if got["primary"].UsedPercent != 50 {
			t.Errorf("delivered windows = %+v", got)
		}
	})

	t.Run("bound observer is not called when nothing parses", func(t *testing.T) {
		called := false
		ctx := WithQuotaObserver(context.Background(), func(map[string]QuotaWindow) { called = true })
		observeQuotaHeaders(ctx, http.Header{"Content-Type": {"application/json"}})
		if called {
			t.Error("observer was called with no quota headers present")
		}
	})
}
