package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/store"
)

// headlessConfig captures the flags shared by `exec` and `chat --no-tui`. The
// two commands differ only in the default input mode (`text` for exec, `lines`
// for chat) so existing chat scripts that pipe one prompt per line keep working
// without specifying --input.
type headlessConfig struct {
	ConfigPath string
	Prompt     string
	Input      string
	Output     string
	File       string // read input from file instead of stdin
	Timeout    time.Duration
	Resume     string
	FakeModel  bool
	Server     string
	InProcess  bool
}

// parseHeadlessArgs parses the shared headless flag set for `exec` and `chat`.
// command is "exec" or "chat" and only affects the default input mode. The flag
// set uses ContinueOnError so callers can render their own usage line; unknown
// flags, invalid --input/--output values, and conflicting flag combos (e.g.
// --prompt with --input lines) return an error that the caller maps to exit 2.
//
// Mutual-exclusion rules (kept here rather than scattered across the runner):
//   - --prompt is only valid with --input text (otherwise stdin must supply the
//     batch of prompts).
//   - --file and --prompt are mutually exclusive (--file reads the prompt from
//     a file, --prompt carries it inline).
func parseHeadlessArgs(args []string, command string) (headlessConfig, error) {
	cfg := headlessConfig{Input: "text", Output: "text"}
	if command == "chat" {
		cfg.Input = "lines"
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.StringVar(&cfg.ConfigPath, "config", "config.yaml", "path to configuration file")
	fs.StringVar(&cfg.Prompt, "p", "", "prompt text; with input=text only")
	fs.StringVar(&cfg.Prompt, "prompt", "", "alias for -p")
	fs.StringVar(&cfg.Input, "input", cfg.Input, "input mode: text | lines | jsonl")
	fs.StringVar(&cfg.Output, "output", "text", "output format: text | jsonl")
	fs.StringVar(&cfg.File, "file", "", "read input from FILE instead of stdin")
	fs.DurationVar(&cfg.Timeout, "timeout", 0, "abort after this duration (0 = no limit)")
	fs.StringVar(&cfg.Resume, "resume", "", "restore session id before the first turn")
	fs.BoolVar(&cfg.FakeModel, "fake-model", false, "use deterministic fake model")
	fs.StringVar(&cfg.Server, "server", "", "force connect to this server URL")
	fs.BoolVar(&cfg.InProcess, "inprocess", false, "force in-process backend")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() != 0 {
		return cfg, fmt.Errorf("unexpected positional argument %q", fs.Arg(0))
	}
	if cfg.Output != "text" && cfg.Output != "jsonl" {
		return cfg, fmt.Errorf("invalid --output %q (want text or jsonl)", cfg.Output)
	}
	if cfg.Input != "text" && cfg.Input != "lines" && cfg.Input != "jsonl" {
		return cfg, fmt.Errorf("invalid --input %q (want text, lines, or jsonl)", cfg.Input)
	}
	if cfg.Prompt != "" && cfg.Input != "text" {
		return cfg, fmt.Errorf("-p/--prompt requires --input text")
	}
	if cfg.File != "" && cfg.Prompt != "" {
		return cfg, fmt.Errorf("--file and -p/--prompt are mutually exclusive")
	}
	return cfg, nil
}

// runHeadlessCommand is the shared entry point for `exec` and `chat --no-tui`.
// It parses flags, reads prompts (from --file, --prompt, or stdin by mode),
// composes a context with SIGINT/SIGTERM + optional timeout, runs the headless
// turn loop, prints the resolved session id to stderr, and returns the stable
// exit code (0/1/2/124/130). command is "exec" or "chat" for error messages.
// stdin is the reader for the stdin-by-mode input path (os.Stdin in production;
// tests inject a bytes.Buffer/strings.Reader so the stdin branches are
// exercisable without replacing the process's real stdin).
// headlessInputs resolves the prompts for one headless run from --file,
// --prompt, or stdin.
//
// All three go through cli.ReadHeadlessInputs. The --file branch used to read
// the whole file and hand it back as ONE prompt without ever calling it, so
// `--input jsonl --file <3-line file>` ran a single turn whose prompt was
// three lines of raw JSON: the modes the flag advertises (text/lines/jsonl,
// 1MiB line cap, per-line error reporting for jsonl) applied to stdin only.
// A file is just another reader; there was never a reason for it to have its
// own parser.
//
// --prompt stays a single text prompt regardless of --input, and
// parseHeadlessArgs already refuses --prompt with a non-text mode, so the two
// cannot disagree.
func headlessInputs(cfg headlessConfig, stdin io.Reader) ([]cli.HeadlessInput, error) {
	if cfg.File != "" {
		f, err := os.Open(cfg.File)
		if err != nil {
			return nil, fmt.Errorf("read file: %w", err)
		}
		defer f.Close()
		return cli.ReadHeadlessInputs(f, cli.HeadlessInputMode(cfg.Input))
	}
	if cfg.Prompt != "" {
		return []cli.HeadlessInput{{Prompt: strings.TrimSpace(cfg.Prompt)}}, nil
	}
	return cli.ReadHeadlessInputs(stdin, cli.HeadlessInputMode(cfg.Input))
}

// drainQueue returns the messages queued for sessionID as headless prompts,
// marking them consumed (W-D-08).
//
// EVERY FAILURE IS SILENT AND YIELDS NOTHING. The queue is an addition to the
// run, not a precondition for it: a missing config, an unreadable database or a
// session with no queue must not stop a prompt the user typed on this command
// line from being answered. That is the same soft-degrade rule bootstrap.Build
// applies to VCS and plugin discovery.
//
// It opens its own handle rather than reaching into the backend, because the
// backend may be a different process — which is exactly the case `yanshi
// enqueue` exists for. store.Open keeps SelfHeal off, as every incidental
// reader must.
//
// DELIVERY IS AT-MOST-ONCE: the rows are marked consumed here, before the turns
// run, so a crashed run loses them. See store.ConsumeQueuedMessages for why
// that direction was chosen over redelivering user input.
//
// THE HEADLESS RESUME IS THE WHOLE DELIVERY SURFACE, by design and not by
// omission. W-D-08's acceptance lands in internal/store plus this command; the
// WebSocket session-resume path was never part of it. Draining there would be a
// NEW capability rather than a gap being left open, and a bigger one than it
// sounds: a headless run already has a list of prompts to execute, so the queue
// is a slice concatenation, while a reconnecting TUI is idle — the server would
// have to start a turn nobody asked for, decide what to do when the user is
// mid-type, and answer for a queue drained by a reconnect that then dropped. An
// earlier version of this note called it "the same one line"; that was a cost
// estimate nobody had checked.
func drainQueue(configPath, sessionID string) []cli.HeadlessInput {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil
	}
	st, err := store.Open(cfg.Storage.SQLitePath)
	if err != nil {
		return nil
	}
	defer st.Close()
	msgs, err := st.ConsumeQueuedMessages(sessionID)
	if err != nil || len(msgs) == 0 {
		return nil
	}
	out := make([]cli.HeadlessInput, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, cli.HeadlessInput{Prompt: m})
	}
	fmt.Fprintf(os.Stderr, "delivering %d queued message(s) to %s\n", len(msgs), sessionID)
	return out
}

// queuedFirst puts a session's queued messages ahead of whatever this
// invocation was given (W-D-08).
//
// FIRST, IN ENQUEUE ORDER. They were said earlier, and a queue that delivered
// out of order would make "queue this, then ask about it" impossible to
// express. The `-h` text and docs/user-guide/entrypoints.md both promise the
// order, so it is a contract rather than an implementation detail.
//
// A separate function purely so that promise has somewhere to be asserted:
// inlined, the concatenation had no observation point, and reversing it left
// the whole suite green — TestDrainQueue_ConsumesOnResume checks the order
// drainQueue itself returns, and TestRunHeadless_ResumeDrainsTheQueue only
// checks that the queue was emptied. TestQueuedFirst_QueueLeadsTypedInput is
// what goes red now.
func queuedFirst(configPath, sessionID string, inputs []cli.HeadlessInput) []cli.HeadlessInput {
	return append(drainQueue(configPath, sessionID), inputs...)
}

func runHeadlessCommand(args []string, command string, stdin io.Reader) int {
	cfg, err := parseHeadlessArgs(args, command)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yanshi %s: %v\n", command, err)
		return exitUsage
	}
	inputs, err := headlessInputs(cfg, stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yanshi %s: %v\n", command, err)
		return exitUsage
	}
	if cfg.Resume != "" {
		inputs = queuedFirst(cfg.ConfigPath, cfg.Resume, inputs)
		inputs[0].Resume = cfg.Resume
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}
	result, err := cli.RunHeadless(ctx, cli.Options{
		ConfigPath: cfg.ConfigPath,
		FakeModel:  cfg.FakeModel,
		Server:     cfg.Server,
		InProcess:  cfg.InProcess,
	}, cli.HeadlessRunOptions{
		Inputs: inputs,
		Output: cli.ExecOutputFormat(cfg.Output),
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	if result.SessionID != "" {
		fmt.Fprintf(os.Stderr, "session: %s\n", result.SessionID)
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		fmt.Fprintf(os.Stderr, "yanshi %s: %v\n", command, err)
	}
	return mapExecError(err)
}
