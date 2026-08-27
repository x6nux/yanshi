package eino

import (
	"context"
	"errors"
	"time"

	"github.com/cloudwego/eino/schema"
)

// ErrStreamIdle is returned by watchdogReader.Recv when the upstream provider
// has not delivered a content-bearing chunk within the configured budget.
//
// It is declared as a *RetryableModelError (not a plain sentinel) so that
// IsRetryableModelErr(ErrStreamIdle) is true by construction: a stalled
// gateway is a network-level condition, not a model or client failure, and a
// non-retryable verdict here would burn the whole failover chain on one dead
// stream.
//
// Its message deliberately does NOT contain "timeout", "eof", or any other
// word from errclass.go's keyword marker tables. isRetryableStreamErr checks
// errors.Is(err, ErrStreamIdle) by identity, ahead of ClassifyError, precisely
// so this does not depend on incidental keyword overlap — a future edit to
// those tables (e.g. narrowing "timeout" because it over-matches provider
// text) must not be able to silently stop this from retrying.
var ErrStreamIdle error = &RetryableModelError{Err: errors.New("eino: stream idle, no content received")}

// watchdogReader wraps a *schema.StreamReader[*schema.Message] with a first-
// chunk and a steady-state idle budget.
//
// Why it exists: consumeStream's loop only checks ctx.Err() and then blocks
// on sr.Recv() — Recv itself carries no timeout. A gateway that accepts the
// connection and then sends nothing hangs the stream forever. loopguard's
// DeadlineGate cannot catch this: it checks at ReAct iteration boundaries,
// and a stream stuck inside Recv never reaches the next one. An unattended
// goal loop then loses its whole budget to one dead stream.
//
// Two budgets, not one, because they measure different things. first bounds
// "did the upstream ever start" (safe to set tight). idle bounds "is the
// upstream still going" (must be set loose — the gap between chunks in a long
// reasoning turn is legitimately large).
//
// Load-bearing: empty control chunks do not renew the deadline. A chunk
// carrying only Role, or an otherwise all-empty delta, must not reset the
// timer — otherwise a gateway that emits heartbeats and nothing else would
// hang forever, which is exactly the shape this defence exists to catch. The
// judgement of "does this chunk carry content" is isBlank (resilient.go),
// the SAME function consumeStream uses to drop no-op deltas — not a second,
// possibly-divergent copy of that logic.
type watchdogReader struct {
	sr     *schema.StreamReader[*schema.Message]
	first  time.Duration
	idle   time.Duration
	cancel context.CancelFunc // aborts the provider call that owns sr; see Recv
	begun  bool               // has a content-bearing chunk arrived yet?

	// deadline is the absolute time by which the next content-bearing chunk
	// must arrive. It is deliberately NOT re-armed on every Recv call — only
	// on construction of the current budget period and on a genuine renewal
	// (a non-blank chunk). A control-only chunk arriving mid-period must let
	// the countdown keep running across it; re-arming per call would let an
	// unbroken stream of heartbeats (each one comfortably inside a single
	// call's full budget) survive forever, which is precisely the failure
	// this field exists to prevent. Zero value means "not yet armed for the
	// current budget()".
	deadline time.Time
}

// newWatchdogReader wraps sr. first and idle are independently optional: a
// zero value disables that budget. When both are zero, Recv behaves exactly
// like calling sr.Recv directly — same call, no goroutine, no timer — so
// leaving both unset reproduces pre-W-A-06 behaviour byte-for-byte.
//
// cancel aborts the provider call that produced sr (the per-attempt context
// passed to that provider's Stream method) — see Recv's doc comment for why
// this, not sr.Close(), is what actually releases the goroutine on timeout.
func newWatchdogReader(sr *schema.StreamReader[*schema.Message], first, idle time.Duration, cancel context.CancelFunc) *watchdogReader {
	return &watchdogReader{sr: sr, first: first, idle: idle, cancel: cancel}
}

// budget returns the timeout that applies to the NEXT Recv: first before any
// content-bearing chunk has arrived, idle afterward. Zero means unbounded.
func (w *watchdogReader) budget() time.Duration {
	if w.begun {
		return w.idle
	}
	return w.first
}

// Recv reads the next chunk, returning ErrStreamIdle if the applicable budget
// elapses first.
//
// The deadline for the CURRENT budget period is armed once (lazily, on the
// first Recv of that period) and only pushed forward again by a genuine
// renewal (see note); each individual call waits only the time REMAINING
// until that deadline, not a fresh full budget. Racing a full timer per call
// instead would let a steady drip of control-only chunks — each comfortably
// inside one call's budget — survive indefinitely, exactly the heartbeat-only
// gateway this defence exists to catch.
//
// sr.Recv is a blocking call with no context form, so a non-zero budget races
// it against a timer in a goroutine.
//
// On timeout, w.cancel is called before returning: it is NOT sr.Close(), and
// that distinction is load-bearing. sr wraps an eino schema.stream, whose
// recv() blocks purely on <-items; StreamReader.Close() (receiver-side)
// closes an unrelated "closed" channel that recv() never selects on, so it
// does not unblock a Recv already in flight — only the WRITER closing items
// (via StreamWriter.Close/Send) can do that. w.cancel is the per-attempt
// context.CancelFunc threaded down into the provider's Stream(ctx, ...) call
// that produced sr; cancelling it aborts that provider's in-flight HTTP
// request, which makes the provider's own read goroutine observe an error
// and run its (always-present, in every provider this ships with) deferred
// StreamWriter.Close/Send — that is what unblocks the goroutine leaked
// below, not this function returning. The leaked goroutine's own exit is
// therefore bounded by the provider's request-abort latency, not by this
// call, which is why Recv does not wait for it: the channel is buffered so
// the eventual send never blocks even if nothing is left to receive it.
func (w *watchdogReader) Recv() (*schema.Message, error) {
	d := w.budget()
	if d <= 0 {
		msg, err := w.sr.Recv()
		w.note(msg, err)
		return msg, err
	}
	if w.deadline.IsZero() {
		w.deadline = time.Now().Add(d)
	}
	remaining := time.Until(w.deadline)
	if remaining <= 0 {
		w.cancel()
		return nil, ErrStreamIdle
	}

	type result struct {
		msg *schema.Message
		err error
	}
	ch := make(chan result, 1)
	go func() {
		msg, err := w.sr.Recv()
		ch <- result{msg, err}
	}()

	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case r := <-ch:
		w.note(r.msg, r.err)
		return r.msg, r.err
	case <-timer.C:
		w.cancel()
		return nil, ErrStreamIdle
	}
}

// note updates the watchdog's state after a successful sr.Recv. A control-
// only chunk (isBlank) does NOT renew the deadline — it is dropped from
// consideration entirely, leaving both w.begun and w.deadline exactly as they
// were. A genuine content-bearing chunk both flips w.begun (so budget()
// switches from first to idle) and re-arms the deadline for the budget that
// now applies; when that budget is 0 (disabled), the deadline is cleared so
// the NEXT Recv takes the d<=0 passthrough branch instead of reusing a stale
// timestamp.
//
// An error (including io.EOF) leaves the deadline untouched: the caller is
// about to stop calling Recv on this stream either way, and EOF is not an
// idle failure.
func (w *watchdogReader) note(msg *schema.Message, err error) {
	if err != nil || isBlank(msg) {
		return
	}
	w.begun = true
	if d := w.budget(); d > 0 {
		w.deadline = time.Now().Add(d)
	} else {
		w.deadline = time.Time{}
	}
}
