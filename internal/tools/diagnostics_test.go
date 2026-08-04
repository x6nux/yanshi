package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/lsp"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
)

type stubLSPManager struct {
	enabled bool
	byPath  map[string][]lsp.Diagnostic
}

func (s stubLSPManager) Enabled() bool { return s.enabled }
func (s stubLSPManager) Diagnostics(path string, _ time.Duration) []lsp.Diagnostic {
	return s.byPath[path]
}
func (s stubLSPManager) DidChange(path, content string) {}

func TestDiagnosticsAggregatesIndependentProbes(t *testing.T) {
	src := stubLSPManager{enabled: true, byPath: map[string][]lsp.Diagnostic{"a.go": {{Severity: lsp.SeverityError, Message: "bad"}}}}
	diagLSPSourceOverride = src
	t.Cleanup(func() { diagLSPSourceOverride = nil })

	factory := newScriptedFactory(t, func(spec secproc.SecureProcessSpec) cannedResult {
		if spec.Program == "go" {
			return cannedResult{Stdout: "go version go1.26.4 linux/amd64\n"}
		}
		return cannedResult{Stdout: ""}
	})
	ctx := WithWorkRoot(secureTestContext(t, factory), t.TempDir())
	out, err := runTool(ctx, NewDiagnosticsTool(diagTestProbe{files: []string{"a.go"}}), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Git struct {
			Available bool `json:"available"`
		} `json:"git"`
		Toolchain struct {
			Go string `json:"go"`
		} `json:"toolchain"`
		LSP struct {
			Available            bool `json:"available"`
			OpenDiagnosticsCount int  `json:"open_diagnostics_count"`
		} `json:"lsp"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Git.Available || !strings.Contains(res.Toolchain.Go, "go1.26") {
		t.Fatalf("toolchain=%s", res.Toolchain.Go)
	}
	if !res.LSP.Available || res.LSP.OpenDiagnosticsCount != 1 {
		t.Fatalf("lsp=%+v", res.LSP)
	}
}

func TestDiagnosticsLSPUnavailableIsLocalDegradation(t *testing.T) {
	diagLSPSourceOverride = stubLSPManager{enabled: false}
	t.Cleanup(func() { diagLSPSourceOverride = nil })
	factory := newScriptedFactory(t, func(secproc.SecureProcessSpec) cannedResult { return cannedResult{} })
	ctx := WithWorkRoot(secureTestContext(t, factory), t.TempDir())
	out, err := runTool(ctx, NewDiagnosticsTool(diagTestProbe{files: nil}), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"lsp":{"available":false,"open_diagnostics_count":0}`) {
		t.Fatalf("out=%s", out)
	}
}

func TestDiagnosticsGitFailureDoesNotHideOthers(t *testing.T) {
	diagLSPSourceOverride = stubLSPManager{enabled: false}
	t.Cleanup(func() { diagLSPSourceOverride = nil })
	factory := newScriptedFactory(t, func(spec secproc.SecureProcessSpec) cannedResult {
		if spec.Program == "git" {
			return cannedResult{ExitCode: 128, Stderr: "not a repo"}
		}
		return cannedResult{Stdout: "go version go1.26.4\n"}
	})
	ctx := WithWorkRoot(secureTestContext(t, factory), t.TempDir())
	out, err := runTool(ctx, NewDiagnosticsTool(diagTestProbe{files: nil}), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"go":"go version go1.26.4"`) {
		t.Fatalf("out=%s", out)
	}
	if !strings.Contains(out, `"git":{"available":false`) {
		t.Fatalf("out=%s", out)
	}
}

type diagTestProbe struct{ files []string }

func (d diagTestProbe) recentFiles(ctx context.Context, root string) []string { return d.files }

// TestDiagnosticsReportsTheBoundSandboxPosture is the regression test for
// Config.Sandbox being a value nothing read.
//
// The chain orchestrator.Config.Sandbox → o.sandbox → tools.WithSandbox →
// securityctx existed and was tested end to end, but SandboxFromContext had no
// production caller: sandboxProbe returned a hardcoded
// {"requested":"unknown","effective":"unknown","enforced":false} while the
// process was holding the real posture in a context value two frames up. So
// `diagnostics` — the one tool whose entire job is reporting the posture —
// confidently reported that it did not know it.
//
// Replacing securityctx.Sandbox with a panic used to leave the whole suite
// green except its own round-trip unit test; this test is what makes that
// experiment fail loudly now.
func TestDiagnosticsReportsTheBoundSandboxPosture(t *testing.T) {
	factory := newScriptedFactory(t, func(secproc.SecureProcessSpec) cannedResult {
		return cannedResult{Stdout: ""}
	})
	sb := sandbox.New(sandbox.Config{Enabled: true, Tier: sandbox.WorkspaceWrite})
	ctx := WithSandbox(WithWorkRoot(secureTestContext(t, factory), t.TempDir()), sb)
	out, err := runTool(ctx, NewDiagnosticsTool(diagTestProbe{}), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeSandboxDiag(t, out)
	want := sandboxDiag{
		Requested: "workspace-write",
		Effective: string(sandbox.DegradedHostGuard),
		Enforced:  false, // Phase 0 has no OS backing and must say so
	}
	if got != want {
		t.Fatalf("sandbox diagnostic = %+v, want %+v (the posture actually bound in ctx)", got, want)
	}
}

// TestDiagnosticsSandboxUnknownWithoutBinding keeps "unknown" honest: it is
// the answer for a turn with no sandbox bound (SSE, unit tests, anything built
// outside the orchestrator), NOT a placeholder for "not implemented".
func TestDiagnosticsSandboxUnknownWithoutBinding(t *testing.T) {
	factory := newScriptedFactory(t, func(secproc.SecureProcessSpec) cannedResult {
		return cannedResult{Stdout: ""}
	})
	ctx := WithWorkRoot(secureTestContext(t, factory), t.TempDir())
	out, err := runTool(ctx, NewDiagnosticsTool(diagTestProbe{}), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeSandboxDiag(t, out)
	if got.Requested != "unknown" || got.Effective != "unknown" || got.Enforced {
		t.Fatalf("sandbox diagnostic = %+v, want the unknown triple", got)
	}
}

// decodeSandboxDiag pulls the "sandbox" object out of a diagnostics result.
func decodeSandboxDiag(t *testing.T, out string) sandboxDiag {
	t.Helper()
	var res struct {
		Sandbox sandboxDiag `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	return res.Sandbox
}
