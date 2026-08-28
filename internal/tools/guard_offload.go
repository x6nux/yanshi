// The offload half of GuardedTool.Stream (T3).
//
// Kept out of guard.go because that file is the tool contract itself and the
// offload path is one narrow behaviour on top of it; see background.go for the
// design and for why the completion notice is not a tool result.
package tools

import (
	"context"
	"strings"
	"time"
)

// streamWithOffload is the alternative body of GuardedTool.Stream for a call
// that MAY be moved to the background. It returns (channel, true) when it took
// the call, and (nil, false) when the ordinary foreground path must run.
//
// Three conditions must all hold, and each rules out a real hazard:
//
//   - a BackgroundManager is bound. Without one there is nothing to own the
//     run after the turn, so offloading would leak a subprocess nobody can
//     cancel or wait for.
//   - the tool is in BackgroundableTools. See that function for why the set is
//     hand-picked.
//   - the tool has a finite foreground timeout. NoTimeout means "bounded by the
//     turn", which is agent_wait's contract; there is no deadline at which to
//     offload, and detaching it from the turn would remove the only bound it
//     has.
func (g *GuardedTool) streamWithOffload(ctx context.Context, argsJSON string, end func(error)) (<-chan ToolChunk, bool) {
	mgr, ok := BackgroundManagerFromContext(ctx)
	if !ok || !IsBackgroundable(g.name) || g.timeout == NoTimeout {
		return nil, false
	}
	// Idempotency (QwenPaw: "do not re-run the same tool"). A model that
	// re-issues the call while the previous one is still going gets told the
	// handle it already has. Without this the refusal to re-run is only a
	// sentence in a prompt, which is not a mechanism.
	if run, running := mgr.Active(g.name, argsJSON); running {
		end(nil)
		ch := make(chan ToolChunk, 1)
		msg := AlreadyRunningNotice(run)
		ch <- ToolChunk{Text: msg, Result: msg, Status: "already in background"}
		close(ch)
		return ch, true
	}

	// context.WithoutCancel is the whole mechanism. The run has to outlive the
	// turn, and every context a tool is handed descends from the turn context,
	// which the transport cancels the moment the turn returns. Deriving the
	// run's context from the turn's would make the offload a no-op with extra
	// steps: the model would be told "still running" about a process killed
	// microseconds later.
	//
	// Values are preserved by WithoutCancel, so the work root, the profile and
	// the VCS scope all still resolve inside the tool after the turn is gone.
	// Cancellation is replaced, not removed: BackgroundHardLimit bounds the
	// whole call from its start, and BackgroundManager.Cancel/Close bound it
	// from the outside.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), BackgroundHardLimit)
	out := g.stream(runCtx, argsJSON)
	wrapped := make(chan ToolChunk, 16)
	go g.pumpOffloadable(ctx, runCtx, offloadPump{
		mgr: mgr, cancel: cancel, args: argsJSON,
		out: out, wrapped: wrapped,
		tuiCB: ToolChunkCallbackFromContext(ctx), end: end,
	})
	return wrapped, true
}

// offloadPump bundles what pumpOffloadable needs. A struct rather than nine
// parameters, and unexported because nothing outside this file constructs one.
type offloadPump struct {
	mgr     *BackgroundManager
	cancel  context.CancelFunc
	args    string
	out     <-chan ToolChunk
	wrapped chan ToolChunk
	tuiCB   ToolChunkCallback
	end     func(error)
}

// pumpOffloadable drains the tool's channel in two phases.
//
// FOREGROUND, until g.timeout: every chunk goes to the caller's channel and to
// the TUI, exactly as the ordinary path does. The turn is blocked on it.
//
// BACKGROUND, after the deadline: the caller's channel is closed (so the model
// gets its answer and the ReAct loop moves on) and the same goroutine keeps
// draining into the handle. Nothing restarts and nothing is re-authorized —
// this is the SAME channel from the SAME already-authorized call, which is the
// reason the run is not handed to internal/task instead.
//
// turnCtx cancellation during the FOREGROUND phase cancels the run: a user who
// pressed Ctrl-C wants the command stopped, not silently promoted to a
// background job they never asked for. Once offloaded, turnCtx is deliberately
// no longer consulted.
func (g *GuardedTool) pumpOffloadable(turnCtx, runCtx context.Context, p offloadPump) {
	timer := time.NewTimer(g.timeout)
	defer timer.Stop()

	var result strings.Builder
	var toolErr error
	var handle *BackgroundHandle
	closed := false

	// collect applies one chunk to the accumulating result. Shared by both
	// phases so a chunk that arrives one microsecond after the deadline is
	// recorded identically to one that arrived just before it.
	collect := func(c ToolChunk) {
		if c.Result != "" {
			result.WriteString(c.Result)
		}
		if c.Err != nil {
			toolErr = c.Err
		}
	}

foreground:
	for {
		select {
		case c, open := <-p.out:
			if !open {
				break foreground
			}
			collect(c)
			if p.tuiCB != nil {
				p.tuiCB(g.name, c)
			}
			p.wrapped <- c
		case <-timer.C:
			handle = p.mgr.Adopt(g.name, p.args, p.cancel)
			if handle == nil {
				// The manager is closed: the process is shutting down and
				// nobody would wait for this run. Fall back to the ordinary
				// deadline behaviour rather than detaching an orphan.
				p.cancel()
				continue
			}
			notice := OffloadNotice(handle.ID(), g.name, g.timeout)
			chunk := ToolChunk{Text: notice, Result: notice, Status: "backgrounded"}
			if p.tuiCB != nil {
				p.tuiCB(g.name, chunk)
			}
			p.wrapped <- chunk
			close(p.wrapped)
			closed = true
			break foreground
		case <-turnCtx.Done():
			p.cancel()
			// Keep draining below so the tool's goroutine is not left blocked
			// on an unread send; the loop exits as soon as it closes.
			break foreground
		}
	}

	if handle == nil {
		for c := range p.out {
			collect(c)
			if p.tuiCB != nil {
				p.tuiCB(g.name, c)
			}
			if !closed {
				p.wrapped <- c
			}
		}
		if !closed {
			close(p.wrapped)
		}
		p.cancel()
		p.end(toolErr)
		return
	}

	// Background phase. The TUI callback is deliberately dropped: it enqueues
	// onto a per-turn frame channel whose reader stops at turn end, so calling
	// it here would pin a dead turn's buffer for the life of the run.
	for c := range p.out {
		collect(c)
	}
	p.cancel()
	// The same spillover cap the foreground path applies in InvokableRun. A
	// background run is exactly the shape that produces a megabyte of test
	// output, and reinjecting it verbatim would spend the window the offload
	// just saved. runCtx (not turnCtx) because it still carries the work root
	// and is not cancelled.
	//
	// W-A-02 fix round 1: the redaction must run here too, and BEFORE
	// spillIfTooLong for the same reason InvokableRun orders them — spilling
	// an unredacted result writes the secret to disk where artifact_read
	// fetches it straight back. This path does not go through InvokableRun:
	// a backgrounded run's completion notice is injected directly into
	// state.Messages by hygiene.go, so InvokableRun's redaction never sees
	// it. runCtx carries the redactor because context.WithoutCancel (used to
	// build it, above) preserves Values from the turn ctx that
	// bindExecutionContext bound WithRedactor onto — the same mechanism the
	// comment above already relies on for the work root and profile.
	handle.Finish(spillIfTooLong(runCtx, g.name, g.redactResult(runCtx, result.String())), toolErr)
	p.end(toolErr)
}
