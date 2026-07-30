package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestParseHeadlessArgs verifies the shared parser accepts the full flag set
// for both `exec` and `chat`. Only the default input mode differs between the
// two commands (text for exec, lines for chat) so legacy chat scripts keep
// their one-prompt-per-line behavior without specifying --input.
func TestParseHeadlessArgs(t *testing.T) {
	cfg, err := parseHeadlessArgs([]string{"--input", "jsonl", "--output", "jsonl", "--resume", "s1", "--timeout", "2s"}, "exec")
	if err != nil {
		t.Fatalf("parseHeadlessArgs: %v", err)
	}
	if cfg.Input != "jsonl" || cfg.Output != "jsonl" || cfg.Resume != "s1" || cfg.Timeout != 2*time.Second {
		t.Fatalf("cfg = %#v", cfg)
	}
}

// TestParseHeadlessArgsChatDefaultsToLines proves `chat --no-tui` preserves the
// legacy line-REPL feel: a bare `chat` (no --input) parses with Input="lines".
func TestParseHeadlessArgsChatDefaultsToLines(t *testing.T) {
	cfg, err := parseHeadlessArgs(nil, "chat")
	if err != nil {
		t.Fatalf("parseHeadlessArgs chat: %v", err)
	}
	if cfg.Input != "lines" {
		t.Fatalf("chat default input = %q, want lines", cfg.Input)
	}
}

// TestParseHeadlessArgsRejectsPromptWithJSONL proves --prompt only works with
// --input text. Mixing --prompt with lines/jsonl would be ambiguous (which
// line gets the prompt?) so it is a usage error.
func TestParseHeadlessArgsRejectsPromptWithJSONL(t *testing.T) {
	_, err := parseHeadlessArgs([]string{"-p", "hello", "--input", "jsonl"}, "exec")
	if err == nil {
		t.Fatal("prompt + jsonl should fail")
	}
}

// TestParseHeadlessArgsRejectsFileWithPrompt proves --file and -p are mutually
// exclusive. They are two ways to supply the prompt inline; combining them is
// always a mistake.
func TestParseHeadlessArgsRejectsFileWithPrompt(t *testing.T) {
	_, err := parseHeadlessArgs([]string{"-p", "hello", "--file", "a.txt"}, "exec")
	if err == nil {
		t.Fatal("--file and -p should be mutually exclusive")
	}
}

// TestParseHeadlessArgsRejectsUnknownPositional proves stray positional args
// are usage errors (the headless commands take no positional args — only flags).
func TestParseHeadlessArgsRejectsUnknownPositional(t *testing.T) {
	_, err := parseHeadlessArgs([]string{"bogus"}, "exec")
	if err == nil {
		t.Fatal("positional arg should fail")
	}
}

// TestHeadlessExitCode proves the stable exit code contract: nil -> 0, generic
// error -> 1, DeadlineExceeded -> 124 (coreutils timeout), Canceled -> 130
// (128+SIGINT). The mapping is the headless runner's contract with scripts.
func TestHeadlessExitCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"ok", nil, exitOK},
		{"runtime", errors.New("boom"), exitErr},
		{"timeout", context.DeadlineExceeded, exitTimeout},
		{"cancel", context.Canceled, exitCancel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapExecError(tc.err); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
}
