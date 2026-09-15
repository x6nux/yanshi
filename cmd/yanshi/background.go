package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/x6nux/yanshi/internal/cli"
)

// stripBackgroundFlag removes -b/--background from args wherever it appears.
//
// A scan rather than a flag definition, for the same reason chat does it for
// --no-tui: `yanshi serve -b -addr X` and `yanshi -b serve -addr X` both have
// to mean the same thing, and the flag sets on the way in are two different
// ones — serve's does not know -b, and the top-level one does not know -addr.
// Returns the remaining args unchanged when the flag is absent.
func stripBackgroundFlag(args []string) ([]string, bool) {
	out := make([]string, 0, len(args))
	found := false
	for _, a := range args {
		if a == "-b" || a == "--background" {
			found = true
			continue
		}
		out = append(out, a)
	}
	return out, found
}

// runBackground implements `yanshi -b` / `yanshi serve -b`: start the backend
// as a daemon detached from this terminal, wait until it is READY, print where
// it is, and return.
//
// It exists because the alternative an operator had was `yanshi serve &` inside
// a shell — which is not the same thing in three ways that matter: the child
// keeps the terminal's session (so a closing terminal or a SIGHUP to the
// process group takes the backend down), its output goes to wherever the shell
// pointed stdout rather than to a log the next command can find, and the shell
// returns before the daemon is actually listening, so the next command in the
// script races the bootstrap.
func runBackground(args []string, stdout, stderr io.Writer) int {
	// Accept the flag both ways round: `yanshi -b` and `yanshi serve -b` are
	// the same request, and dispatch strips -b before this function sees the
	// args, so the serve subcommand word can still be present.
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	fs := flag.NewFlagSet("background", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "config.yaml", "path to configuration file")
	fakeModel := fs.Bool("fake-model", false, "use a deterministic fake model (no API keys needed)")
	addr := fs.String("addr", "", "override the config's HTTP listen address")
	asJSON := fs.Bool("json", false, "print one JSON object instead of a human line")
	wait := fs.Duration("wait", cli.DefaultBackgroundWait, "how long to wait for the daemon to become ready")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "yanshi -b: unexpected positional argument %q\n", fs.Arg(0))
		return exitUsage
	}

	var extra []string
	if *fakeModel {
		extra = append(extra, "-fake-model")
	}
	if *addr != "" {
		extra = append(extra, "-addr", *addr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res, err := cli.StartBackground(ctx, cli.BackgroundOptions{
		ConfigPath: *configPath,
		ExtraArgs:  extra,
		Wait:       *wait,
	})
	if err != nil {
		fmt.Fprintf(stderr, "yanshi -b: %v\n", err)
		return exitErr
	}
	cli.RenderBackgroundResult(stdout, res, *asJSON)
	return exitOK
}

// unused keeps time imported for the flag duration above when the file is
// compiled without the flag (the DurationVar reference is enough, but gofmt
// ordering tools read better with the explicit use).
var _ = time.Second
