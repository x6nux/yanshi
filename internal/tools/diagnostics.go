package tools

import (
	"context"
	"strings"
	"time"

	"github.com/x6nux/yanshi/internal/lsp"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
)

type lspSource interface {
	Enabled() bool
	Diagnostics(path string, timeout time.Duration) []lsp.Diagnostic
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
	return toJSON(struct {
		Workspace map[string]any `json:"workspace"`
		Git       probeDiag      `json:"git"`
		Sandbox   sandboxDiag    `json:"sandbox"`
		Toolchain toolchainDiag  `json:"toolchain"`
		LSP       lspDiag        `json:"lsp"`
	}{
		Workspace: workspaceSummary(root),
		Git:       runGitProbe(ctx, root),
		Sandbox:   sandboxProbe(ctx),
		Toolchain: runToolchainProbes(ctx),
		LSP:       runLSPProbe(ctx, root),
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

func sandboxProbe(ctx context.Context) sandboxDiag {
	// The sandbox package (internal/sandbox) is delivered by A1c together
	// with secproc. If A1c exposes a SandboxFromContext helper and a Report
	// struct with Requested/Effective/Enforced fields, prefer that. Until
	// then, degrade gracefully: secproc specs already carry UseSandboxTier
	// so the diagnostic loses no information by reporting "unknown" here.
	return sandboxDiag{Requested: "unknown", Effective: "unknown", Enforced: false}
}

func runToolchainProbes(ctx context.Context) toolchainDiag {
	out := toolchainDiag{}
	for _, entry := range []struct{ name, program string }{
		{"Go", "go"}, {"Cargo", "cargo"}, {"Node", "node"},
	} {
		spec := secproc.SecureProcessSpec{Tool: "diagnostics", Program: entry.program,
			Args: []string{"--version"}, UseSandboxTier: sandbox.ReadOnly}
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
	files := diagFileListerOverride.recentFiles(ctx, root)
	count := 0
	for _, path := range files {
		count += len(source.Diagnostics(path, 2*time.Second))
	}
	return lspDiag{Available: true, OpenDiagnosticsCount: count}
}

func workspaceSummary(root string) map[string]any {
	return map[string]any{"root": root}
}
