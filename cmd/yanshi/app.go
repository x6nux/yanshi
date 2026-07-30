package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/x6nux/yanshi/internal/appserver"
	"github.com/x6nux/yanshi/internal/bootstrap"
)

// runApp is the testable core of `yanshi app`: it parses flags, builds the
// bootstrap App (which exposes the shared *v1.Service via app.AgentAPI),
// serves JSON-RPC on r/w, and returns the exit code. r/w are parameters so
// tests can drive stdio via bytes.Buffer/strings.Reader without spawning a
// subprocess. Diagnostics always go to stderr — stdout is reserved for the
// JSON-RPC wire so external parsers never see log noise interleaved with
// responses.
func runApp(args []string, in io.Reader, out io.Writer) int {
	fs := flag.NewFlagSet("app", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "config.yaml", "path to configuration file")
	fakeModel := fs.Bool("fake-model", false, "use deterministic fake model (no API keys needed)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "yanshi app: unexpected positional argument")
		return exitUsage
	}

	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: *configPath, FakeModel: *fakeModel})
	if err != nil {
		fmt.Fprintf(os.Stderr, "yanshi app: %v\n", err)
		return exitErr
	}
	defer app.Shutdown(context.Background())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := appserver.NewMemoryConfig()
	srv := appserver.New(app.AgentAPI, cfg)
	if err := srv.Serve(ctx, in, out); err != nil {
		fmt.Fprintf(os.Stderr, "yanshi app: %v\n", err)
		return exitErr
	}
	return exitOK
}
