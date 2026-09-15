package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/guard"
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
//
// ledger: D1/V12#2 退出码稳定
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

// TestParseHeadlessArgsAcceptsEveryGuardMode pins -mode against guard's own
// catalogue rather than a literal list copied here: a mode added to guard but
// not accepted by the CLI would be reachable in the TUI and unreachable
// headlessly, which is the gap -mode exists to close.
func TestParseHeadlessArgsAcceptsEveryGuardMode(t *testing.T) {
	for _, m := range guard.Modes() {
		cfg, err := parseHeadlessArgs([]string{"-mode", string(m)}, "exec")
		if err != nil {
			t.Errorf("-mode %s: %v", m, err)
			continue
		}
		if cfg.Mode != string(m) {
			t.Errorf("-mode %s parsed as %q", m, cfg.Mode)
		}
	}
}

// TestParseHeadlessArgsRejectsUnknownMode proves a typo is a usage error instead
// of a silent no-op: an unattended run that quietly ignored -mode yoloo would
// look exactly like one that had set it.
func TestParseHeadlessArgsRejectsUnknownMode(t *testing.T) {
	if _, err := parseHeadlessArgs([]string{"-mode", "yoloo"}, "exec"); err == nil {
		t.Fatal("-mode yoloo should be a usage error")
	}
}

// TestParseHeadlessArgsApprovalPolicies pins the -approve vocabulary and the
// default. The default matters more than the values: a run that was not told to
// approve anything must deny, and a typo must be a usage error rather than a
// silent fallback to one of the two useful answers.
func TestParseHeadlessArgsApprovalPolicies(t *testing.T) {
	cfg, err := parseHeadlessArgs(nil, "exec")
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if cfg.Approve != "never" {
		t.Fatalf("default -approve = %q, want never", cfg.Approve)
	}
	for _, policy := range []string{"never", "required", "all"} {
		cfg, err := parseHeadlessArgs([]string{"-approve", policy}, "exec")
		if err != nil {
			t.Errorf("-approve %s: %v", policy, err)
			continue
		}
		if cfg.Approve != policy {
			t.Errorf("-approve %s parsed as %q", policy, cfg.Approve)
		}
	}
	if _, err := parseHeadlessArgs([]string{"-approve", "always"}, "exec"); err == nil {
		t.Fatal("-approve always should be a usage error")
	}
}
