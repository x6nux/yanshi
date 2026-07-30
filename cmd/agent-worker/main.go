// Package main implements the agent-worker binary: a standalone remote
// worker that connects to an yanshi server's Task API, listens for
// task_available signals via SSE, claims tasks, executes them, and reports
// results back. The executor is pluggable; for M5 the EchoExecutor is used
// (returns the task input unchanged). M6 will swap in a real agent executor.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/x6nux/yanshi/internal/agent/worker"
)

func main() {
	os.Exit(runWorker(os.Args[1:], os.Stderr))
}

// runWorker is the testable core of the agent-worker binary: it parses flags,
// validates the required -server/-token/-name triple, builds the capability
// list, and runs the worker until ctx is cancelled or the connect fails. It
// returns the exit code (0 ok / 1 runtime / 2 usage) instead of calling
// os.Exit so the validation, caps parsing, and run-error mapping are
// unit-testable.
func runWorker(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent-worker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "", "yanshi server base URL (required)")
	token := fs.String("token", "", "bearer token for authentication (required)")
	name := fs.String("name", "", "worker name (required)")
	capsStr := fs.String("caps", "", "comma-separated capability tags (optional)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *server == "" || *token == "" || *name == "" {
		fmt.Fprintln(stderr, "agent-worker: -server, -token, and -name are required")
		fs.Usage()
		return 2
	}

	caps := parseCaps(*capsStr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(stderr, "agent-worker %q connecting to %s\n", *name, *server)
	if err := runAgent(ctx, *server, *token, *name, caps); err != nil && ctx.Err() == nil {
		fmt.Fprintf(stderr, "agent-worker: %v\n", err)
		return 1
	}
	return 0
}

// runAgent builds the client+worker and runs the SSE claim/execute loop until
// ctx is cancelled. It is a package-level var so tests can inject a fast
// stand-in — the real worker.Run blocks on the SSE reconnect loop (bounded
// exponential backoff) until ctx is cancelled, so it is not unit-testable; its
// end-to-end behaviour is covered by internal/agent/worker's tests.
var runAgent = func(ctx context.Context, server, token, name string, caps []string) error {
	client := worker.NewClient(server, token)
	w := worker.NewWorker(client, name, caps, &worker.EchoExecutor{})
	return w.Run(ctx)
}

// parseCaps splits a comma-separated capability string into trimmed, non-empty
// tags. Extracted from runWorker so the parsing (including the empty-input and
// blank-element cases) is unit-testable.
func parseCaps(capsStr string) []string {
	var caps []string
	if capsStr != "" {
		for _, c := range strings.Split(capsStr, ",") {
			if c = strings.TrimSpace(c); c != "" {
				caps = append(caps, c)
			}
		}
	}
	return caps
}
