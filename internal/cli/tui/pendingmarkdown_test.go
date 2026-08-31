package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/cli"
)

// TestRefreshPendingMarkdown_FirstChunkRendersImmediately proves the first
// agent_chunk of a turn (pendingRenderedAt.IsZero(), set by flushAssistant's
// reset) is never held back by pendingMarkdownThrottle — the acceptance
// criterion ("renders progressively during streaming rather than waiting for
// completion") would be violated if even the FIRST pass had to wait.
func TestRefreshPendingMarkdown_FirstChunkRendersImmediately(t *testing.T) {
	m := newModel(nil, "/proj")
	m.width = 80
	m.pending = "# heading"

	got := m.refreshPendingMarkdown()

	want := renderMarkdown(80, "# heading")
	if got.pendingRendered != want {
		t.Fatalf("pendingRendered = %q, want %q (renderMarkdown output)", got.pendingRendered, want)
	}
	if got.pendingRenderedText != "# heading" {
		t.Fatalf("pendingRenderedText = %q, want %q", got.pendingRenderedText, "# heading")
	}
	if got.pendingRenderedWidth != 80 {
		t.Fatalf("pendingRenderedWidth = %d, want 80", got.pendingRenderedWidth)
	}
	if got.pendingRenderedAt.IsZero() {
		t.Fatal("pendingRenderedAt still zero after the first pass")
	}
}

// TestRefreshPendingMarkdown_NoOpWhenUnchanged proves a call with the same
// (pending, width) as the last pass does not recompute.
//
// RE-27-era note (RE-31): this comment used to credit the fast path to
// "renderBody's ~24 FPS activityTick redraws". That caller does not exist —
// refreshPendingMarkdown has exactly one production call site, applyEvent's
// agent_chunk branch (model.go); the activity tick redraws go through
// renderPendingBody, a pure cache read that never calls this at all. What the
// fast path actually buys is on the agent_chunk path itself: without it, a
// chunk that leaves m.pending unchanged (an empty or duplicate delta) still
// resets pendingRenderedAt and so pushes the NEXT real refresh a further
// throttle window into the future.
//
// The backdate below is what makes this test about the fast path rather than
// about the throttle. Two back-to-back calls land inside the same 200ms
// window, so the throttle gate alone would return early and the assertion
// would pass with the identity check deleted — measured: deleting it left
// this test green.
func TestRefreshPendingMarkdown_NoOpWhenUnchanged(t *testing.T) {
	m := newModel(nil, "/proj")
	m.width = 80
	m.pending = "hello"
	m = m.refreshPendingMarkdown()

	m.pendingRenderedAt = m.pendingRenderedAt.Add(-2 * pendingMarkdownThrottle)
	backdated := m.pendingRenderedAt

	m = m.refreshPendingMarkdown()
	if !m.pendingRenderedAt.Equal(backdated) {
		t.Fatalf("pendingRenderedAt changed on a no-op call with the throttle already expired: %v -> %v", backdated, m.pendingRenderedAt)
	}
}

// TestRefreshPendingMarkdown_ThrottledWithinWindow proves new text arriving
// less than pendingMarkdownThrottle after the last pass does NOT trigger a
// second glamour pass — this is what bounds W-E-07's CPU cost under
// high-frequency agent_chunk deltas (the whole reason the streaming path was
// plain-text-only before this feature; see pendingStyle's doc comment).
func TestRefreshPendingMarkdown_ThrottledWithinWindow(t *testing.T) {
	m := newModel(nil, "/proj")
	m.width = 80
	m.pending = "hello"
	m = m.refreshPendingMarkdown()
	staleText := m.pendingRenderedText
	staleAt := m.pendingRenderedAt

	m.pending += " world" // a new chunk arrives well within the throttle window
	m = m.refreshPendingMarkdown()

	if m.pendingRenderedText != staleText {
		t.Fatalf("pendingRenderedText advanced despite the throttle window: got %q, want stale %q", m.pendingRenderedText, staleText)
	}
	if !m.pendingRenderedAt.Equal(staleAt) {
		t.Fatalf("pendingRenderedAt advanced despite the throttle window: %v -> %v", staleAt, m.pendingRenderedAt)
	}
}

// TestRefreshPendingMarkdown_RefreshesAfterThrottleElapses proves the pass
// DOES catch up once pendingMarkdownThrottle has elapsed — the other half of
// the throttle test above; without this half a regression that made the
// throttle permanent (never re-rendering) would read as "correctly
// throttled" by the test above alone.
func TestRefreshPendingMarkdown_RefreshesAfterThrottleElapses(t *testing.T) {
	m := newModel(nil, "/proj")
	m.width = 80
	m.pending = "hello"
	m = m.refreshPendingMarkdown()
	m.pendingRenderedAt = time.Now().Add(-pendingMarkdownThrottle - time.Millisecond)

	m.pending += " world"
	m = m.refreshPendingMarkdown()

	if m.pendingRenderedText != "hello world" {
		t.Fatalf("pendingRenderedText = %q, want %q — throttle window elapsed, pass should have refreshed", m.pendingRenderedText, "hello world")
	}
}

// TestRenderPendingBody_BeforeFirstPass_PlainWholeBuffer proves that before
// any progressive pass has run (pendingRenderedText == ""), the streaming
// block shows the whole buffer plain — byte-identical to pre-W-E-07 behavior
// — never raw, unclosed markdown syntax dressed up as something it isn't.
func TestRenderPendingBody_BeforeFirstPass_PlainWholeBuffer(t *testing.T) {
	m := newModel(nil, "/proj")
	m.width = 80
	m.pending = "**bold**"

	got := m.renderPendingBody()
	want := pendingStyle.Render("**bold**")
	if got != want {
		t.Fatalf("renderPendingBody() = %q, want plain %q", got, want)
	}
}

// TestRenderPendingBody_ExactMatchReturnsRenderedUnwrapped proves that once
// pendingRenderedText == m.pending (no new chunk since the last pass), the
// cached render is returned as-is — not re-wrapped in pendingStyle, which
// would double-style (or strip) the glamour output.
func TestRenderPendingBody_ExactMatchReturnsRenderedUnwrapped(t *testing.T) {
	m := newModel(nil, "/proj")
	m.width = 80
	m.pending = "hello"
	m.pendingRenderedText = "hello"
	m.pendingRenderedWidth = 80
	m.pendingRendered = "RENDERED_SENTINEL"

	got := m.renderPendingBody()
	if got != "RENDERED_SENTINEL" {
		t.Fatalf("renderPendingBody() = %q, want the cached render returned unmodified", got)
	}
}

// TestRenderPendingBody_TailAppendedPlain proves the delta since the last
// pass (the part refreshPendingMarkdown hasn't seen yet) is appended plain,
// not silently dropped — a viewer must still see new tokens arriving between
// throttled passes, just unformatted until the next pass folds them in.
func TestRenderPendingBody_TailAppendedPlain(t *testing.T) {
	m := newModel(nil, "/proj")
	m.width = 80
	m.pending = "hello world"
	m.pendingRenderedText = "hello"
	m.pendingRenderedWidth = 80
	m.pendingRendered = "RENDERED[hello]"

	got := m.renderPendingBody()
	want := "RENDERED[hello]" + pendingStyle.Render(" world")
	if got != want {
		t.Fatalf("renderPendingBody() = %q, want %q", got, want)
	}
}

// TestRenderPendingBody_WidthMismatchFallsBackToPlain proves a stale render
// from a since-resized terminal is never reused at the new width (glamour
// word-wraps to a fixed column count baked into the render).
func TestRenderPendingBody_WidthMismatchFallsBackToPlain(t *testing.T) {
	m := newModel(nil, "/proj")
	m.width = 80
	m.pending = "hello world"
	m.pendingRenderedText = "hello"
	m.pendingRenderedWidth = 40 // stale: rendered before a resize to 80
	m.pendingRendered = "RENDERED[hello]"

	got := m.renderPendingBody()
	want := pendingStyle.Render("hello world")
	if got != want {
		t.Fatalf("renderPendingBody() = %q, want plain whole-buffer %q (width changed since the cached pass)", got, want)
	}
}

// TestRenderPendingBody_NonPrefixCacheFallsBackSafely is the direct guard for
// W-E-07's "no corrupted/residual state" acceptance criterion. It fabricates
// exactly the invariant violation flushAssistant's reset exists to prevent
// (see events.go): pendingRenderedText NOT a prefix of the current m.pending
// — the shape an interrupted turn followed by a shorter new turn would
// produce if that reset were ever dropped. A bare `m.pending[len(prefix):]`
// slice would panic here (prefix is longer than pending); renderPendingBody
// must instead fall back to the plain whole buffer with no panic.
func TestRenderPendingBody_NonPrefixCacheFallsBackSafely(t *testing.T) {
	m := newModel(nil, "/proj")
	m.width = 80
	m.pending = "hi" // much shorter than the stale cached text below
	m.pendingRenderedText = "partial answer with `code` from a prior, interrupted turn"
	m.pendingRenderedWidth = 80
	m.pendingRendered = "STALE_RENDERED_FROM_PRIOR_TURN"

	got := m.renderPendingBody() // must not panic
	want := pendingStyle.Render("hi")
	if got != want {
		t.Fatalf("renderPendingBody() = %q, want plain fallback %q", got, want)
	}
	if strings.Contains(got, "STALE_RENDERED_FROM_PRIOR_TURN") {
		t.Fatal("renderPendingBody() leaked the stale cached render from a mismatched prefix")
	}
}

// TestFlushAssistant_ResetsPendingMarkdownCache proves flushAssistant clears
// all four pending-render fields, not just m.pending — the mechanism that
// makes TestRenderPendingBody_NonPrefixCacheFallsBackSafely's scenario
// unreachable in production: an interrupted turn (any of flushAssistant's
// ~20 call sites in model.go, including cancel/error paths) must leave a
// clean slate for the next turn's first chunk, not a stale cache pointing at
// content that no longer exists.
func TestFlushAssistant_ResetsPendingMarkdownCache(t *testing.T) {
	m := newModel(nil, "/proj")
	m.width = 80
	m = m.applyEvent(cli.StreamEvent{Kind: "agent_chunk", Text: "partial answer with `code`"})
	if m.pendingRenderedText == "" {
		t.Fatal("setup: expected the first chunk to have run a progressive pass already")
	}

	m.flushAssistant()

	if m.pending != "" {
		t.Fatalf("m.pending = %q after flush, want empty", m.pending)
	}
	if m.pendingRendered != "" {
		t.Fatalf("m.pendingRendered = %q after flush, want empty", m.pendingRendered)
	}
	if m.pendingRenderedText != "" {
		t.Fatalf("m.pendingRenderedText = %q after flush, want empty", m.pendingRenderedText)
	}
	if m.pendingRenderedWidth != 0 {
		t.Fatalf("m.pendingRenderedWidth = %d after flush, want 0", m.pendingRenderedWidth)
	}
	if !m.pendingRenderedAt.IsZero() {
		t.Fatalf("m.pendingRenderedAt = %v after flush, want zero", m.pendingRenderedAt)
	}
}

// TestApplyEvent_AgentChunkDrivesProgressiveMarkdown is an end-to-end proof
// (through the real applyEvent dispatch, not refreshPendingMarkdown called
// directly) that a streamed agent_chunk event actually reaches the
// progressive-markdown path — guarding against the wiring itself silently
// coming unplugged (the shape RE-12/RE-B already found twice elsewhere in
// this package for other capability-gated fields).
func TestApplyEvent_AgentChunkDrivesProgressiveMarkdown(t *testing.T) {
	m := newModel(nil, "/proj")
	m.width = 80

	m = m.applyEvent(cli.StreamEvent{Kind: "agent_chunk", Text: "**bold**"})

	if m.pendingRenderedText != "**bold**" {
		t.Fatalf("pendingRenderedText = %q after one agent_chunk event, want %q — refreshPendingMarkdown not wired into applyEvent", m.pendingRenderedText, "**bold**")
	}
}
