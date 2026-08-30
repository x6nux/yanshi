// internal/llm/eino/quota.go
//
// W-C-08: parse a provider's quota-window headers and slow down BEFORE the
// window is exhausted, rather than discovering the limit via a 429.
//
// HEADER SHAPE. Grounded against OpenAI Codex CLI's real backend
// (https://chatgpt.com/backend-api/codex), which reports two independent
// windows — "primary" (5h) and "secondary" (7d) — as a header triplet per
// window: `x-codex-primary-used-percent`, `x-codex-primary-window-minutes`,
// `x-codex-primary-reset-after-seconds` (and the same three with "secondary").
// Confirmed via a GitHub issue enumerating the live header set
// (NousResearch/hermes-agent#9085); this repo has no network access to the
// Codex backend itself to observe them directly. The spec's own notation
// (`x-*-primary-used-percent`) already puts the wildcard on the PROVIDER
// segment, not the window name, so the parser below matches that shape
// generically — any provider using "x-<provider>-<window>-<field>" is
// understood, not just Codex — and picks up "secondary" for free since it is
// the same convention with a different window segment.
//
// WHERE THIS PLUGS IN. Quota headers arrive on ordinary SUCCESSFUL responses
// (that is the whole point — a climbing used-percent is visible long before
// any request fails), so this cannot reuse retryafter.go's respHeaderHolder,
// which only records FAILED responses for Retry-After. Instead each adapter
// that holds a raw *http.Response — anthropic.go, responses.go, and (via
// headerCaptureTransport, for the "openai" kind whose SDK never exposes one)
// this package's own RoundTripper — calls observeQuotaHeaders unconditionally
// after every completed HTTP round trip, success or failure alike. The
// callback it feeds is bound per-call via ctx (WithQuotaObserver), the same
// context-injection pattern resilient.go's WithRetryCallback already
// establishes for this package, and AdaptiveModel is the one caller that
// installs it — see adaptive.go's withQuotaObserver — because M7 rate
// limiting is already AdaptiveModel's concern and the quota governor lives
// next to the token bucket it complements (ratelimit.go).
package eino

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// QuotaWindow is one provider-reported usage window (e.g. Codex's 5h
// "primary" or 7d "secondary" pane), as parsed from response headers.
type QuotaWindow struct {
	// UsedPercent is how much of this window's quota has been consumed, 0-100.
	UsedPercent float64
	// WindowMinutes is the window's total length. Zero means the provider did
	// not send a window-minutes header for this window (ParseQuotaWindows
	// still returns the window if used-percent and reset-after were both
	// present; WindowMinutes is informational, not needed to compute a delay).
	WindowMinutes int
	// ResetAfter is how much wall-clock time remained, AS OF WHEN THIS WAS
	// OBSERVED, before the window resets and UsedPercent goes back to 0.
	ResetAfter time.Duration
}

// quotaHeaderRe matches one field of a provider's quota-window header
// triplet: "x-<provider>-<window>-<field>". Group 1 is the window name
// (lowercased below), group 2 the field. The provider segment itself is
// deliberately unconstrained ([a-z0-9]+, not literally "codex") so this
// parses any provider using the same convention, per this file's header
// comment.
var quotaHeaderRe = regexp.MustCompile(`(?i)^x-[a-z0-9]+-([a-z0-9]+)-(used-percent|window-minutes|reset-after-seconds)$`)

// ParseQuotaWindows extracts every complete quota window found in h, keyed by
// window name ("primary", "secondary", ...) lowercased.
//
// A window is included only once BOTH used-percent and reset-after-seconds
// are present — those two are the minimum a quotaGovernor needs to compute a
// delay (see ratelimit.go). window-minutes alone, or a used-percent with no
// reset-after, describes nothing actionable and is dropped rather than
// returned half-populated, so a caller never has to re-check which fields
// made it through.
func ParseQuotaWindows(h http.Header) map[string]QuotaWindow {
	if len(h) == 0 {
		return nil
	}
	type partial struct {
		usedPercent   float64
		haveUsed      bool
		windowMinutes int
		resetAfterSec float64
		haveReset     bool
	}
	parts := map[string]*partial{}
	for k, v := range h {
		if len(v) == 0 {
			continue
		}
		m := quotaHeaderRe.FindStringSubmatch(k)
		if m == nil {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(v[0]), 64)
		if err != nil {
			continue
		}
		window := strings.ToLower(m[1])
		p := parts[window]
		if p == nil {
			p = &partial{}
			parts[window] = p
		}
		switch strings.ToLower(m[2]) {
		case "used-percent":
			p.usedPercent, p.haveUsed = f, true
		case "window-minutes":
			p.windowMinutes = int(f)
		case "reset-after-seconds":
			p.resetAfterSec, p.haveReset = f, true
		}
	}
	var out map[string]QuotaWindow
	for window, p := range parts {
		if !p.haveUsed || !p.haveReset {
			continue
		}
		if out == nil {
			out = make(map[string]QuotaWindow, len(parts))
		}
		out[window] = QuotaWindow{
			UsedPercent:   p.usedPercent,
			WindowMinutes: p.windowMinutes,
			ResetAfter:    time.Duration(p.resetAfterSec * float64(time.Second)),
		}
	}
	return out
}

// QuotaObserver receives every quota window one HTTP response reported.
// Bound per-call via WithQuotaObserver; see this file's header comment for
// why the value travels in ctx rather than a parameter.
type QuotaObserver func(windows map[string]QuotaWindow)

// quotaObserverKey is the context key WithQuotaObserver installs under.
type quotaObserverKey struct{}

// WithQuotaObserver returns ctx carrying obs. A nil obs returns ctx
// unchanged, so a caller with nothing to observe (M7 disabled — see
// AdaptiveModel.withQuotaObserver) need not special-case the call.
func WithQuotaObserver(ctx context.Context, obs QuotaObserver) context.Context {
	if obs == nil {
		return ctx
	}
	return context.WithValue(ctx, quotaObserverKey{}, obs)
}

// quotaObserverFromContext returns the observer bound to ctx, if any.
func quotaObserverFromContext(ctx context.Context) (QuotaObserver, bool) {
	obs, ok := ctx.Value(quotaObserverKey{}).(QuotaObserver)
	return obs, ok
}

// observeQuotaHeaders parses h and forwards any quota windows found to the
// observer bound to ctx.
//
// Called unconditionally — success or failure — from the one place in each
// adapter that still holds the raw *http.Response, because a provider that
// sends these headers sends them on every response, not just failed ones;
// requiring a failure first would defeat W-C-08's entire point (slowing down
// BEFORE a 429, not after one). A call with no bound observer (no
// AdaptiveModel in front — most unit tests, and any call path that predates
// M7) is a cheap no-op: the regex scan still runs but has nowhere to report
// to, matching headerCaptureTransport's "inert when nothing is installed"
// contract.
func observeQuotaHeaders(ctx context.Context, h http.Header) {
	obs, ok := quotaObserverFromContext(ctx)
	if !ok {
		return
	}
	if windows := ParseQuotaWindows(h); len(windows) > 0 {
		obs(windows)
	}
}
