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
func runHeadlessCommand(args []string, command string, stdin io.Reader) int {
	cfg, err := parseHeadlessArgs(args, command)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yanshi %s: %v\n", command, err)
		return exitUsage
	}
	inputs := []cli.HeadlessInput(nil)
	if cfg.File != "" {
		data, rerr := os.ReadFile(cfg.File)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "yanshi %s: read file: %v\n", command, rerr)
			return exitUsage
		}
		inputs = []cli.HeadlessInput{{Prompt: strings.TrimSpace(string(data))}}
	} else if cfg.Prompt != "" {
		inputs = []cli.HeadlessInput{{Prompt: strings.TrimSpace(cfg.Prompt)}}
	} else {
		mode := cli.HeadlessInputMode(cfg.Input)
		inputs, err = cli.ReadHeadlessInputs(stdin, mode)
		if err != nil {
			fmt.Fprintf(os.Stderr, "yanshi %s: %v\n", command, err)
			return exitUsage
		}
	}
	if cfg.Resume != "" {
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
