package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/x6nux/yanshi/internal/agentmcp"
	"github.com/x6nux/yanshi/internal/bootstrap"
)

// mcpServer implements `yanshi mcp`: expose yanshi's own orchestrator as a
// general-purpose MCP server on stdio (W-F-06), so an external agent adds
// yanshi as a tool server and drives it as a sub-agent. It is the spawnable
// counterpart of `vcs-mcp` (which serves only the 5 VCS tools): here the
// exposed tools are session-ful orchestrator turns — agent_prompt continues
// a conversation across calls via the returned session_id.
//
// stdout carries nothing but protocol frames — the same contract `acp` and
// `vcs-mcp` established, for the same reason: the host parses stdout line by
// line and one stray log line desynchronises it. Diagnostics go to stderr.
func mcpServer(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "config.yaml", "path to configuration file")
	fakeModel := fs.Bool("fake-model", false, "use a deterministic fake model (no API keys needed)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "yanshi mcp: unexpected positional argument")
		return exitUsage
	}
	return runMCPServer(context.Background(), *configPath, *fakeModel, os.Stdin, os.Stdout, os.Stderr)
}

// runMCPServer is the testable core of the mcp subcommand: it builds the app,
// serves MCP on r/w until ctx is cancelled or stdin hits EOF, and returns the
// exit code.
func runMCPServer(ctx context.Context, configPath string, fakeModel bool, r io.Reader, w io.Writer, stderr io.Writer) int {
	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: configPath, FakeModel: fakeModel})
	if err != nil {
		fmt.Fprintf(stderr, "yanshi mcp: %v\n", err)
		return exitErr
	}
	defer app.Shutdown(context.Background())

	serveCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := agentmcp.New(app.AgentAPI)
	if err := srv.Serve(serveCtx, r, w); err != nil {
		fmt.Fprintf(stderr, "yanshi mcp: %v\n", err)
		return exitErr
	}
	return exitOK
}
