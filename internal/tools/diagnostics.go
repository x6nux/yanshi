package tools

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/x6nux/yanshi/internal/lsp"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
)

type lspSource interface {
	Enabled() bool
	Diagnostics(path string, timeout time.Duration) []lsp.Diagnostic
	// OpenDocuments returns the files this session has notified the language
	// server about, most recent first. Before it existed the file list came
	// from a stub that always returned nil, so open_diagnostics_count was
	// permanently 0 -- a field reporting "no problems" while never having
	// looked.
	OpenDocuments() []string
}

type diagFileLister interface {
	recentFiles(ctx context.Context, root string) []string
}

type defaultFileLister struct{}

func (defaultFileLister) recentFiles(_ context.Context, _ string) []string { return nil }

var diagFileListerOverride diagFileLister = defaultFileLister{}

// diagLSPSourceOverride is a test seam for injecting stub LSP sources.
// Production leaves it nil (reads LSPFromContext); tests set it and defer cleanup.
var diagLSPSourceOverride lspSource

// NewDiagnosticsTool creates a GuardedTool that aggregates workspace, git,
// sandbox, toolchain, and LSP diagnostics.
func NewDiagnosticsTool(probe diagFileLister) *GuardedTool {
	if probe != nil {
		diagFileListerOverride = probe
	}
	return NewGuardedTool("diagnostics", "Diagnostics",
		"Aggregate workspace, git, sandbox, toolchain, and LSP diagnostics.",
		15*time.Second, nil, SyncStream(runDiagnostics))
}

func runDiagnostics(ctx context.Context, argsJSON string) (string, error) {
	root := WorkRootFromContext(ctx)

	// The probes are independent and three of them spawn subprocesses (git,
	// and one per toolchain) or make LSP round trips, so running them in
	// sequence made the tool's latency the SUM of five waits when it is
	// bounded below by the slowest. Nothing here shares mutable state; each
	// goroutine writes one field.
	var (
		wg        sync.WaitGroup
		gitD      probeDiag
		sandboxD  sandboxDiag
		toolchain toolchainDiag
		lspD      lspDiag
	)
	run := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	run(func() { gitD = runGitProbe(ctx, root) })
	run(func() { sandboxD = sandboxProbe(ctx) })
	run(func() { toolchain = runToolchainProbes(ctx) })
	run(func() { lspD = runLSPProbe(ctx, root) })
	wg.Wait()

	return toJSON(struct {
		Workspace map[string]any `json:"workspace"`
		Git       probeDiag      `json:"git"`
		Sandbox   sandboxDiag    `json:"sandbox"`
		Toolchain toolchainDiag  `json:"toolchain"`
		LSP       lspDiag        `json:"lsp"`
	}{
		Workspace: workspaceSummary(root),
		Git:       gitD,
		Sandbox:   sandboxD,
		Toolchain: toolchain,
		LSP:       lspD,
	}), nil
}

type probeDiag struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

type sandboxDiag struct {
	Requested string `json:"requested"`
	Effective string `json:"effective"`
	Enforced  bool   `json:"enforced"`
}

type toolchainDiag struct {
	Go    string `json:"go,omitempty"`
	Cargo string `json:"cargo,omitempty"`
	Node  string `json:"node,omitempty"`
}

type lspDiag struct {
	Available            bool `json:"available"`
	OpenDiagnosticsCount int  `json:"open_diagnostics_count"`
}

func runGitProbe(ctx context.Context, root string) probeDiag {
	spec := secproc.SecureProcessSpec{Tool: "diagnostics", Program: "git", Dir: root,
		Args: []string{"rev-parse", "--show-toplevel"}, UseSandboxTier: sandbox.ReadOnly}
	res, err := secureCommandRunner(ctx, spec, 5*time.Second)
	if err != nil || res.ExitCode != 0 {
		return probeDiag{Reason: "git unavailable"}
	}
	return probeDiag{Available: true}
}

// sandboxProbe reports the security posture the CURRENT turn actually runs
// under, read from the sandbox the orchestrator binds into the tool context.
//
// This is the sole production consumer of tools.WithSandbox. Until it existed,
// the whole chain (orchestrator.Config.Sandbox → o.sandbox → tools.WithSandbox
// → securityctx) bound a value nothing ever read, while this function returned
// a hardcoded "unknown" — so `diagnostics` claimed ignorance about a posture
// the process was holding in a context value two frames up the stack.
//
// "unknown" survives only as the honest answer for the case it describes: no
// sandbox bound in ctx (SSE requests, unit tests, any turn built outside the
// orchestrator). A bound sandbox always yields its real CapabilityReport.
func sandboxProbe(ctx context.Context) sandboxDiag {
	sb, ok := SandboxFromContext(ctx)
	if !ok {
		return sandboxDiag{Requested: "unknown", Effective: "unknown", Enforced: false}
	}
	rep := sb.Report()
	return sandboxDiag{
		Requested: rep.Requested.String(),
		Effective: string(rep.Effective),
		Enforced:  rep.Enforced,
	}
}

// toolchainProbeArgv is the argv each toolchain answers its version on.
//
// It is a table rather than a shared []string{"--version"} because the three
// toolchains genuinely disagree: the Go tool spells it as a SUB-COMMAND, and
// `go --version` is not an alias but a parse failure —
//
//	$ go --version
//	flag provided but not defined: -version   (exit 2)
//
// runToolchainProbes skips any probe that exits non-zero, so sending the GNU
// flag to `go` made the Go row silently disappear from every diagnostics
// result — including, most uselessly, every result produced inside a Go
// repository. Adding a toolchain here means looking up ITS spelling, not
// copying a neighbour's.
var toolchainProbeArgv = map[string][]string{
	"go":    {"version"},
	"cargo": {"--version"},
	"node":  {"--version"},
}

func runToolchainProbes(ctx context.Context) toolchainDiag {
	out := toolchainDiag{}
	for _, entry := range []struct{ name, program string }{
		{"Go", "go"}, {"Cargo", "cargo"}, {"Node", "node"},
	} {
		spec := secproc.SecureProcessSpec{Tool: "diagnostics", Program: entry.program,
			Args: toolchainProbeArgv[entry.program], UseSandboxTier: sandbox.ReadOnly}
		res, err := secureCommandRunner(ctx, spec, 3*time.Second)
		if err != nil || res.ExitCode != 0 {
			continue
		}
		line := strings.TrimSpace(res.Stdout)
		switch entry.name {
		case "Go":
			out.Go = line
		case "Cargo":
			out.Cargo = line
		case "Node":
			out.Node = line
		}
	}
	return out
}

func runLSPProbe(ctx context.Context, root string) lspDiag {
	var source lspSource
	if mgr, ok := LSPFromContext(ctx); ok && mgr != nil {
		source = mgr
	}
	if source == nil {
		source = diagLSPSourceOverride
	}
	if source == nil || !source.Enabled() {
		return lspDiag{}
	}
	files := source.OpenDocuments()
	// The lister seam stays as an override for tests that want to drive a
	// specific file set without standing up a manager; production leaves it
	// returning nil and falls through to the server's own view.
	if extra := diagFileListerOverride.recentFiles(ctx, root); len(extra) > 0 {
		files = extra
	}
	count := 0
	for _, path := range files {
		count += len(source.Diagnostics(path, 2*time.Second))
	}
	return lspDiag{Available: true, OpenDiagnosticsCount: count}
}

func workspaceSummary(root string) map[string]any {
	return map[string]any{"root": root}
}
