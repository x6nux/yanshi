package tools

import (
	"encoding/json"
	"testing"

	"github.com/x6nux/yanshi/internal/lsp"
	"github.com/x6nux/yanshi/internal/secproc"
)

// TestDiagnosticsLSPRowIsLiveInTheProductionConstruction closes the hard spot
// the acceptance breakdown recorded against DT5's first clause.
//
// The finding was that the LSP row is dead in production: defaultFileLister
// returns nil, bootstrap passes nil, so open_diagnostics_count would always be
// 0 and the "aggregates in one call" promise covered 4 of 5 dimensions. The
// tests got 1 only because they injected diagTestProbe, which production does
// not have.
//
// That reading of the seam is wrong, and this test is what says so: the lister
// is an OVERRIDE consulted after the real source. Production reads
// lsp.Manager.OpenDocuments, which fs_patch and lspctx populate through
// DidChange → rememberOpen on every edit the agent makes. So the row is live
// exactly when the agent has edited something — which is the only time it has
// anything to report.
//
// Constructed with NewDiagnosticsTool(nil), the production shape, so the
// injected probe cannot be what makes it pass.
//
// ledger: B3/DT5#1 一次调用聚合
func TestDiagnosticsLSPRowIsLiveInTheProductionConstruction(t *testing.T) {
	src := stubLSPManager{
		enabled: true,
		byPath:  map[string][]lsp.Diagnostic{"edited.go": {{Severity: lsp.SeverityError, Message: "bad"}}},
	}
	diagLSPSourceOverride = src
	t.Cleanup(func() { diagLSPSourceOverride = nil })
	// Reset the package-global the constructor writes, or a later test in this
	// package inherits whatever the last one injected.
	t.Cleanup(func() { diagFileListerOverride = defaultFileLister{} })

	factory := newScriptedFactory(t, func(spec secproc.SecureProcessSpec) cannedResult {
		if spec.Program == "go" {
			return toolchainProbeReply(spec, "go version go1.26.4 linux/amd64")
		}
		return cannedResult{Stdout: ""}
	})
	ctx := WithWorkRoot(secureTestContext(t, factory), t.TempDir())

	out, err := runTool(ctx, NewDiagnosticsTool(nil), `{}`)
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
	if !res.LSP.Available {
		t.Fatal("the LSP row reports unavailable under the production construction")
	}
	if res.LSP.OpenDiagnosticsCount == 0 {
		t.Error("open_diagnostics_count is 0 with NewDiagnosticsTool(nil) even though " +
			"the source has an edited file with a diagnostic: the LSP dimension really " +
			"is dead in production and the aggregate covers 4 of 5 dimensions")
	}
	// The other dimensions must still be in the same response — "aggregates in
	// one call" is about all of them arriving together.
	if !res.Git.Available || res.Toolchain.Go == "" {
		t.Errorf("the aggregate lost a dimension: git=%v toolchain=%q",
			res.Git.Available, res.Toolchain.Go)
	}
}

// TestDiagnosticsToolchainProbeFailureLeavesTheOthers is the reverse case the
// breakdown asked for: the existing tests kill git and kill LSP, but never a
// toolchain probe.
//
// ledger: B3/DT5#2 各子项可独立失败不拖垮
func TestDiagnosticsToolchainProbeFailureLeavesTheOthers(t *testing.T) {
	src := stubLSPManager{enabled: true,
		byPath: map[string][]lsp.Diagnostic{"a.go": {{Severity: lsp.SeverityError, Message: "bad"}}}}
	diagLSPSourceOverride = src
	t.Cleanup(func() { diagLSPSourceOverride = nil })
	t.Cleanup(func() { diagFileListerOverride = defaultFileLister{} })

	factory := newScriptedFactory(t, func(spec secproc.SecureProcessSpec) cannedResult {
		switch spec.Program {
		case "go":
			return toolchainProbeReply(spec, "go version go1.26.4 linux/amd64")
		case "node":
			return toolchainProbeReply(spec, "v22.0.0")
		case "cargo":
			// The one probe that dies. Not a non-zero exit — a launch failure,
			// which is the path no existing case covered.
			return cannedResult{ExitCode: 127, Stderr: "cargo: not found"}
		}
		return cannedResult{Stdout: ""}
	})
	ctx := WithWorkRoot(secureTestContext(t, factory), t.TempDir())

	out, err := runTool(ctx, NewDiagnosticsTool(nil), `{}`)
	if err != nil {
		t.Fatalf("a failing toolchain probe failed the whole call: %v", err)
	}
	var res struct {
		Toolchain map[string]string `json:"toolchain"`
		LSP       struct {
			Available bool `json:"available"`
		} `json:"lsp"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.Toolchain["go"] == "" || res.Toolchain["node"] == "" {
		t.Errorf("a dead cargo probe took the surviving toolchains with it: %+v", res.Toolchain)
	}
	if !res.LSP.Available {
		t.Error("a dead cargo probe took the LSP row with it")
	}
}
