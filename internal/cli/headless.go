package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// HeadlessRunOptions drives one or more prompts through one resolved backend.
type HeadlessRunOptions struct {
	Inputs []HeadlessInput
	Output ExecOutputFormat
	Stdout io.Writer
	Stderr io.Writer
	Resume string
}

// HeadlessResult reports the last known server session id and completed prompts.
type HeadlessResult struct {
	SessionID string
	Completed int
}

// RunHeadless resolves one backend and keeps it alive for all input records.
// Resume from the CLI flag is applied before the first record; a JSONL record's
// resume is used only when the flag is empty and the record is first.
func RunHeadless(ctx context.Context, opts Options, run HeadlessRunOptions) (HeadlessResult, error) {
	if len(run.Inputs) == 0 {
		return HeadlessResult{}, fmt.Errorf("headless: no input")
	}
	sess := NewSession(opts)
	if err := sess.Resolve(ctx); err != nil {
		return HeadlessResult{}, err
	}
	defer sess.Close()
	return runHeadlessWithBackend(ctx, sess.Backend(), run)
}

// runHeadlessWithBackend is the testable core: it drives an already-resolved
// backend through an optional one-shot restore (CLI flag or first-record resume)
// and then one turn per input. The same backend is reused for every record so
// server-side session state (history, usage, model choice) carries across
// prompts. Subsequent records never re-resume: a stale first-record resume must
// not overwrite an active session mid-stream.
func runHeadlessWithBackend(ctx context.Context, b ChatBackend, opts HeadlessRunOptions) (HeadlessResult, error) {
	if b == nil {
		return HeadlessResult{}, fmt.Errorf("headless: backend is nil")
	}
	stdout, stderr := opts.Stdout, opts.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	output := opts.Output
	if output == "" {
		output = ExecOutputText
	}
	result := HeadlessResult{}
	resume := opts.Resume
	for i, input := range opts.Inputs {
		if i == 0 && resume == "" {
			resume = input.Resume
		}
		one := ExecOptions{
			Prompt: input.Prompt,
			Output: output,
			Resume: resume,
			Stdout: stdout,
			Stderr: stderr,
		}
		oneResult, err := execWithBackend(ctx, b, one)
		if err != nil {
			return result, err
		}
		if oneResult.SessionID != "" {
			result.SessionID = oneResult.SessionID
		}
		result.Completed++
		resume = ""
	}
	return result, nil
}

// HeadlessOutputEvent is the stable JSONL projection of a StreamEvent. Field
// names are deliberately camelCase/short (not the Go field names) so external
// consumers can depend on them without breakage when internal fields are
// refactored. Error is a string rather than an error interface, so transport
// errors and server error frames share one machine-readable representation.
type HeadlessOutputEvent struct {
	Type             string          `json:"type"`
	Text             string          `json:"text,omitempty"`
	ToolName         string          `json:"toolName,omitempty"`
	ToolArgs         string          `json:"toolArgs,omitempty"`
	Status           string          `json:"status,omitempty"`
	ID               string          `json:"id,omitempty"`
	Model            string          `json:"model,omitempty"`
	Thinking         string          `json:"thinking,omitempty"`
	SessionID        string          `json:"sessionId,omitempty"`
	TokensIn         int             `json:"tokensIn,omitempty"`
	TokensOut        int             `json:"tokensOut,omitempty"`
	Turns            int             `json:"turns,omitempty"`
	Items            []string        `json:"items,omitempty"`
	StructuredResult json.RawMessage `json:"structuredResult,omitempty"`
	Error            string          `json:"error,omitempty"`
}

// projectHeadlessEvent maps a StreamEvent into its stable wire shape. Centralizing
// the projection here keeps exec.go's JSONL branch and the V14 item mapping in
// sync — a new StreamEvent field is added in exactly one place.
func projectHeadlessEvent(ev StreamEvent) HeadlessOutputEvent {
	out := HeadlessOutputEvent{
		Type: ev.Kind, Text: ev.Text, ToolName: ev.ToolName, ToolArgs: ev.ToolArgs,
		Status: ev.ToolStatus, ID: ev.ID, Model: ev.Model, Thinking: ev.Thinking,
		SessionID: ev.SessionID, TokensIn: ev.TokensIn, TokensOut: ev.TokensOut,
		Turns: ev.Turns, Items: ev.Items, StructuredResult: ev.StructuredResult,
	}
	if ev.Err != nil {
		out.Error = ev.Err.Error()
	}
	return out
}

// renderHeadlessEvent renders one event for the headless runner. JSONL mode
// emits one HeadlessOutputEvent per line on stdout; text mode delegates to
// renderExecEvent so the headless runner and the single-turn exec path share
// their human-readable format.
func renderHeadlessEvent(stdout, stderr io.Writer, output ExecOutputFormat, ev StreamEvent) {
	if output == ExecOutputJSONL {
		line, err := json.Marshal(projectHeadlessEvent(ev))
		if err != nil {
			fmt.Fprintf(stderr, "exec: marshal event: %v\n", err)
			return
		}
		fmt.Fprintln(stdout, string(line))
		return
	}
	renderExecEvent(stdout, stderr, ExecOutputText, ev)
}
