package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/x6nux/yanshi/internal/proto"
)

// ExecOutputFormat selects how Exec renders the event stream.
type ExecOutputFormat string

const (
	// ExecOutputText renders agent_chunk text to stdout and tool/error activity
	// to stderr one-liners — the default, for humans.
	ExecOutputText ExecOutputFormat = "text"
	// ExecOutputJSONL emits every observed StreamEvent as one JSON object per
	// line on stdout — for programmatic consumers (pipes, tests). Every event,
	// including status/done/error, is one line.
	ExecOutputJSONL ExecOutputFormat = "jsonl"
)

// ExecOptions configures the headless Exec driver. Prompt is the user turn
// (required; cmd/yanshi reads stdin when the -p flag is empty). Output selects
// the render format. Resume, when non-empty, restores a prior session before the
// turn (WebSocket transport only — SSE is stateless). Stdout/Stderr default to
// io.Discard so the driver is safe without a terminal; cmd/yanshi passes
// os.Stdout/os.Stderr.
type ExecOptions struct {
	Options        // session resolution (Root, ConfigPath, FakeModel, Server, InProcess)
	Prompt  string // the user turn text (required)
	Output  ExecOutputFormat
	Resume  string // optional session id to restore before the turn ("" = fresh)
	Stdout  io.Writer
	Stderr  io.Writer
}

// ExecResult carries the session id the server assigned (new turn) or resumed,
// so cmd/yanshi can print it for the next --resume.
type ExecResult struct {
	SessionID string
}

// Exec resolves a backend and runs one headless turn. It is the thin resolver
// wrapper around execWithBackend (the testable core) — split the same way
// runHeadless wraps runHeadlessWithFrames so the turn-driving logic can be
// exercised against a fake ChatBackend without bootstrapping.
func Exec(ctx context.Context, opts ExecOptions) (ExecResult, error) {
	sess := NewSession(opts.Options)
	if err := sess.Resolve(ctx); err != nil {
		return ExecResult{}, err
	}
	defer sess.Close()
	return execWithBackend(ctx, sess.Backend(), opts)
}

// execWithBackend is the testable core of the headless driver: it drives an
// already-resolved backend through an optional restore_session and exactly one
// user turn, rendering events to opts.Stdout/Stderr per opts.Output.
//
// ctx drives cancellation (SIGINT) and timeout: wsBackend.Send wires ctx to its
// in-flight turn, so cancelling aborts at the next event boundary and the event
// channel closes. execWithBackend returns ctx.Err() (context.Canceled or
// context.DeadlineExceeded) when ctx is done — taking precedence over a server
// error frame — so cmd/yanshi can map those to exit codes 130/124. A server
// error frame is returned as a plain error (exit 1).
func execWithBackend(ctx context.Context, b ChatBackend, opts ExecOptions) (ExecResult, error) {
	stdout, stderr := opts.Stdout, opts.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	out := opts.Output
	if out == "" {
		out = ExecOutputText
	}
	result := ExecResult{}

	// Optional resume: send restore_session before the turn and drain its reply.
	// Resume needs the WebSocket transport (SSE is stateless): SendFrame returns
	// ErrSSEControlUnsupported there, which we wrap so main reports a clear error.
	if opts.Resume != "" {
		rch, err := b.SendFrame(ctx, proto.NewRestoreSession(opts.Resume))
		if err != nil {
			return result, fmt.Errorf("resume %q: %w", opts.Resume, err)
		}
		for ev := range rch {
			// In JSONL the restore reply is a real event the consumer wants; in
			// text mode it is internal plumbing and stays silent.
			if out == ExecOutputJSONL {
				renderHeadlessEvent(stdout, stderr, ExecOutputJSONL, ev)
			}
			if ev.Kind == "session_restored" {
				result.SessionID = ev.SessionID
			}
			if ev.Kind == "error" {
				return result, fmt.Errorf("resume %q: %s", opts.Resume, execEventText(ev))
			}
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
	}

	// Run the turn.
	ch, err := b.Send(ctx, opts.Prompt)
	if err != nil {
		return result, fmt.Errorf("send: %w", err)
	}
	var serverErr string
	for ev := range ch {
		// The turn's status frame carries the (possibly new) session id.
		if ev.Kind == "status" && ev.SessionID != "" {
			result.SessionID = ev.SessionID
		}
		renderExecEvent(stdout, stderr, out, ev)
		if ev.Kind == "error" {
			serverErr = execEventText(ev)
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err // cancel/timeout wins over a server error frame
	}
	if serverErr != "" {
		return result, fmt.Errorf("server error: %s", serverErr)
	}
	return result, nil
}

// renderExecEvent renders one turn event per the output format. JSONL delegates
// to renderHeadlessEvent so exec and the multi-prompt headless runner share one
// stable wire projection (HeadlessOutputEvent). Text mode writes agent_chunk
// text to stdout and tool/error one-liners to stderr; the one-liners avoid
// non-ASCII emoji so CI in a non-UTF-8 locale can diff them byte-for-byte.
func renderExecEvent(stdout, stderr io.Writer, out ExecOutputFormat, ev StreamEvent) {
	if out == ExecOutputJSONL {
		renderHeadlessEvent(stdout, stderr, ExecOutputJSONL, ev)
		return
	}
	// text: agent_chunk -> stdout; tool/error activity -> stderr one-liners,
	// matching the chatLegacy render style (main.go). status/done/thinking/retry
	// are silent (metadata, not turn output).
	switch ev.Kind {
	case "agent_chunk":
		fmt.Fprint(stdout, ev.Text)
	case "tool_call":
		fmt.Fprintf(stderr, "tool %s %s ...\n", ev.ToolName, ev.ToolArgs)
	case "tool_result":
		fmt.Fprintf(stderr, "result %s [%s]\n", ev.ToolName, ev.ToolStatus)
	case "error":
		fmt.Fprintf(stderr, "error: %s\n", execEventText(ev))
	}
}

// execEventText returns the human-readable message for an error event: ev.Err
// when the transport set it (e.g. a dropped connection), else ev.Text (the
// server's error frame text).
func execEventText(ev StreamEvent) string {
	if ev.Err != nil {
		return ev.Err.Error()
	}
	return ev.Text
}
