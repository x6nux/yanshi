package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
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

// OpenDocuments reports every path the stub was seeded with, so the count
// under test comes from a real file list rather than from a nil stub -- the
// shape that kept open_diagnostics_count at 0 in production.
func (s stubLSPManager) OpenDocuments() []string {
	out := make([]string, 0, len(s.byPath))
	for p := range s.byPath {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
func (s stubLSPManager) DidChange(path, content string) {}

// toolchainProbeReply answers a toolchain probe the way the real binary does —
// INCLUDING refusing the argv the real binary refuses.
//
// The fake this replaces keyed on spec.Program alone and handed back a version
// string no matter what arguments it was given, which is exactly why `go
// --version` (a parse error the real Go tool rejects with exit 2) survived a
// green test suite for three separate rounds of "the toolchain probes are
// empty" bug fixing. A fake that cannot reproduce the failure cannot protect
// the call site; this one refuses anything not in toolchainProbeArgv.
func toolchainProbeReply(spec secproc.SecureProcessSpec, version string) cannedResult {
	want, known := toolchainProbeArgv[spec.Program]
	if !known || !slices.Equal(spec.Args, want) {
		return cannedResult{
			ExitCode: 2,
			Stderr:   fmt.Sprintf("flag provided but not defined: %s", strings.Join(spec.Args, " ")),
		}
	}
	return cannedResult{Stdout: version + "\n"}
}

// TestDiagnosticsAggregatesIndependentProbes drives one `diagnostics` call and
// requires all three independent probes — git, toolchain, LSP — to come back
// populated in the same result. Aggregation is the tool's entire reason to
// exist: three separate tool calls would cost three model round trips.
//
// ⚠️ The LSP row here is reachable only because this test injects
// diagTestProbe, and that probe does not exist in production. The single
// production construction site passes nil, which leaves diagFileListerOverride
// at defaultFileLister — whose recentFiles returns nil unconditionally — so a
// shipped binary iterates over zero files and reports
// open_diagnostics_count: 0 no matter how many diagnostics the LSP manager
// holds. Swapping the constructor below for NewDiagnosticsTool(nil) and
// changing nothing else yields available=true, open_diagnostics_count=0.
//
// So this test pins the aggregation SHAPE, not the claim that production
// aggregates all five dimensions; B3/DT5#1 stays open until defaultFileLister
// is a real implementation (or the LSP row leaves the promise).
//
// ledger: B3/DT5#1 一次调用聚合
func TestDiagnosticsAggregatesIndependentProbes(t *testing.T) {
	src := stubLSPManager{enabled: true, byPath: map[string][]lsp.Diagnostic{"a.go": {{Severity: lsp.SeverityError, Message: "bad"}}}}
	diagLSPSourceOverride = src
	t.Cleanup(func() { diagLSPSourceOverride = nil })

	factory := newScriptedFactory(t, func(spec secproc.SecureProcessSpec) cannedResult {
		if spec.Program == "go" {
			return toolchainProbeReply(spec, "go version go1.26.4 linux/amd64")
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

// TestDiagnosticsLSPUnavailableIsLocalDegradation pins the LSP half of the
// per-probe isolation rule: a disabled LSP manager reports itself unavailable
// in its own row instead of failing the call.
//
// ledger: B3/DT5#2 各子项可独立失败不拖垮
//
// ledger: B2/LSP1#2 server 缺失安全降级
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

// TestDiagnosticsGitFailureDoesNotHideOthers is the git half of the same rule,
// and the harsher one: git exits 128 ("not a repo") and the toolchain row must
// still arrive intact. A probe that aborted the aggregate would turn one
// missing subsystem into a blind spot across all of them.
//
// ledger: B3/DT5#2 各子项可独立失败不拖垮
func TestDiagnosticsGitFailureDoesNotHideOthers(t *testing.T) {
	diagLSPSourceOverride = stubLSPManager{enabled: false}
	t.Cleanup(func() { diagLSPSourceOverride = nil })
	factory := newScriptedFactory(t, func(spec secproc.SecureProcessSpec) cannedResult {
		if spec.Program == "git" {
			return cannedResult{ExitCode: 128, Stderr: "not a repo"}
		}
		return toolchainProbeReply(spec, "go version go1.26.4")
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

// TestDiagnosticsProbesEachToolchainWithItsOwnVersionArgv pins the argv table
// against the only thing that can validate it: the real binaries.
//
// The three probes are answered by a fake here, so nothing in the unit suite
// knows that `go --version` is a parse error while `cargo --version` is
// correct. This test asserts the exact argv sent to each program, so changing
// the table (or "simplifying" it back to a shared --version) fails loudly
// instead of quietly emptying a row of the diagnostics result — which is the
// failure mode that has now recurred three times.
//
// It carries the accuracy clause rather than a "version string looks right"
// assertion because the observed defect was never a wrong string — it was an
// EMPTY row: wrong argv → probe exits non-zero → runToolchainProbes drops it →
// the operator reads "no Go toolchain" from a machine that has one. The final
// loop therefore also requires all three rows to be populated.
//
// ledger: B3/DT5#3 toolchain 版本准确
func TestDiagnosticsProbesEachToolchainWithItsOwnVersionArgv(t *testing.T) {
	diagLSPSourceOverride = stubLSPManager{enabled: false}
	t.Cleanup(func() { diagLSPSourceOverride = nil })

	var mu sync.Mutex
	seen := map[string][]string{}
	factory := newScriptedFactory(t, func(spec secproc.SecureProcessSpec) cannedResult {
		if spec.Program == "git" {
			return cannedResult{}
		}
		mu.Lock()
		seen[spec.Program] = append([]string(nil), spec.Args...)
		mu.Unlock()
		return toolchainProbeReply(spec, spec.Program+" 1.0")
	})
	ctx := WithWorkRoot(secureTestContext(t, factory), t.TempDir())
	out, err := runTool(ctx, NewDiagnosticsTool(diagTestProbe{}), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	for program, want := range map[string][]string{
		"go":    {"version"},
		"cargo": {"--version"},
		"node":  {"--version"},
	} {
		mu.Lock()
		got, probed := seen[program]
		mu.Unlock()
		if !probed {
			t.Fatalf("%s was never probed", program)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s probed with %q, want %q (%[1]s rejects the other spelling, "+
				"and runToolchainProbes drops any probe that exits non-zero)", program, got, want)
		}
	}
	// Every row must be populated: a probe that exits non-zero is dropped, so
	// an empty row is exactly how a wrong argv presents to the operator.
	for _, want := range []string{`"go":"go 1.0"`, `"cargo":"cargo 1.0"`, `"node":"node 1.0"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("diagnostics is missing %s: %s", want, out)
		}
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
//
// It asserts the probe against the sandbox's OWN Report() rather than against
// a hardcoded Phase 0 triple. The hardcoded form worked only while every
// platform was a no-op adapter: the moment darwin grew a real Seatbelt
// backend, this test failed on macOS with {os-isolated, true} — a correct
// posture reported correctly. Pinning the literal would have meant either a
// per-GOOS expectation table (which goes stale as each platform lands) or
// deleting the test. Comparing against Report() keeps the actual claim intact:
// the probe must relay what is bound in ctx, whatever that is. The separate
// assertion that it is not the unknown triple is what stops the comparison
// from passing vacuously if sandboxProbe regressed to ignoring the binding —
// TestDiagnosticsSandboxUnknownWithoutBinding owns the other direction.
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
	rep := sb.Report()
	want := sandboxDiag{
		Requested: rep.Requested.String(),
		Effective: string(rep.Effective),
		Enforced:  rep.Enforced,
	}
	if got != want {
		t.Fatalf("sandbox diagnostic = %+v, want %+v (the posture actually bound in ctx)", got, want)
	}
	if got.Requested == "unknown" || got.Effective == "unknown" {
		t.Fatalf("the probe reported the unknown triple for a sandbox that IS bound: %+v", got)
	}
	if got.Requested != "workspace-write" {
		t.Fatalf("the probe lost the requested tier: %+v", got)
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
