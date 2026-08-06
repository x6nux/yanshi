package orchestrator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/lsp"
	"github.com/x6nux/yanshi/internal/tools"
)

// scriptedLSP is an LSPManager whose Diagnostics call is fully controlled by the
// test: it returns a fixed diagnostic and can be made to block for a set
// duration. It records the paths it was asked about so a test can prove the
// edit tool actually reached it.
type scriptedLSP struct {
	diag   lsp.Diagnostic
	block  time.Duration
	calls  atomic.Int32
	opened []string
}

func (s *scriptedLSP) Enabled() bool { return true }

func (s *scriptedLSP) DidChange(path, content string) { s.opened = append(s.opened, path) }

func (s *scriptedLSP) Diagnostics(path string, timeout time.Duration) []lsp.Diagnostic {
	s.calls.Add(1)
	if s.block > 0 {
		// Honour the caller's deadline the way a real server-backed manager
		// does: block, but no longer than we were given. A stub that ignored
		// timeout entirely would test the stub, not the caller's budget.
		if timeout <= 0 || timeout > s.block {
			timeout = s.block
		}
		time.Sleep(timeout)
		return nil
	}
	return []lsp.Diagnostic{s.diag}
}

func (s *scriptedLSP) OpenDocuments() []string { return s.opened }

// lspTurnFixture builds a workdir, a two-step model (edit then answer) and a
// profile that permits the edit, shared by the two tests below.
func lspTurnFixture(t *testing.T) (string, *einollm.FakeModel, *tools.FSTools, guard.PermissionProfile) {
	t.Helper()
	workdir := t.TempDir()
	target := filepath.Join(workdir, "main.go")
	require.NoError(t, os.WriteFile(target, []byte("package main\n\nfunc main() {}\n"), 0o644))

	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name:      "fs_edit",
			Arguments: `{"path":"main.go","old_string":"func main() {}","new_string":"func main() { undefinedCall() }"}`,
		}},
	})
	step2 := schema.AssistantMessage("done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2}, nil)
	mdl.RecordMessages = true

	profile := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS: guard.FSPerm{
			Read:  []string{workdir + "/**"},
			Write: []string{workdir + "/**"},
		},
	}
	return workdir, mdl, tools.NewFSTools(workdir), profile
}

// TestE2E_LSPDiagnosticsReachTheModel drives a whole turn and reads what the
// model was actually handed.
//
// fs_patch_test.go already asserts that the edit tool's RETURN VALUE carries the
// diagnostic text. That is one hop short of the claim: between the tool
// returning and the model reading there is the ADK tool-result envelope, the
// ReAct loop's history assembly, and — on a long turn — compaction. Every one of
// those could drop the field with no test going red, and the acceptance clause
// is about what the model receives.
//
// FakeModel.RecordMessages keeps only the LATEST call's input, which is exactly
// what is wanted here: the second call is the one whose history contains the
// tool result from the first.
//
// ledger: B2/LSP1#1 编辑后模型收到诊断
func TestE2E_LSPDiagnosticsReachTheModel(t *testing.T) {
	workdir, mdl, fs, profile := lspTurnFixture(t)
	_ = workdir

	const marker = "UNDEFINED_SYMBOL_MARKER"
	mgr := &scriptedLSP{diag: lsp.Diagnostic{
		Line: 3, Severity: 1, Message: marker, Source: "scripted",
	}}

	o, err := New(Config{Model: mdl, Tools: []BaseTool{fs.Edit}, Profile: profile, LSP: mgr})
	require.NoError(t, err)

	out, err := o.Query(context.Background(), "fix main.go")
	require.NoError(t, err)
	assert.Equal(t, "done", out)

	require.NotZero(t, mgr.calls.Load(), "the edit tool never consulted the LSP manager")

	var toolResults []string
	for _, m := range mdl.ReceivedMessages {
		if m.Role == schema.Tool {
			toolResults = append(toolResults, m.Content)
		}
	}
	require.NotEmpty(t, toolResults, "the model's second call carried no tool result at all")

	joined := strings.Join(toolResults, "\n")
	assert.Contains(t, joined, marker,
		"the diagnostic never reached the model: the edit tool produced it, but it was "+
			"dropped somewhere between the tool result and the model's input")
}

// TestE2E_LSPTimeoutDoesNotBlockTheTurn pins the budget.
//
// The timeout parameter existed and was passed through, but nothing ever made a
// source slow, so a change that dropped the deadline — or waited on the wrong
// channel — would not have failed anything. Asserting the parameter equals
// 2*time.Second would have been the cheap version of this test and would pass on
// code that never applies it.
//
// The stub blocks for a minute unless the caller's own timeout cuts it short, so
// the wall clock here IS the assertion: if the edit path did not bound the call,
// this test takes a minute instead of seconds.
//
// ledger: B2/LSP1#3 超时不阻塞 turn
func TestE2E_LSPTimeoutDoesNotBlockTheTurn(t *testing.T) {
	workdir, mdl, fs, profile := lspTurnFixture(t)

	mgr := &scriptedLSP{block: time.Minute}

	o, err := New(Config{Model: mdl, Tools: []BaseTool{fs.Edit}, Profile: profile, LSP: mgr})
	require.NoError(t, err)

	start := time.Now()
	out, err := o.Query(context.Background(), "fix main.go")
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Equal(t, "done", out, "the turn must complete normally despite the stalled LSP")
	require.NotZero(t, mgr.calls.Load(), "the edit tool never consulted the LSP manager")

	// Generous relative to the edit path's own budget, tight relative to the
	// stub's minute: this fails on "no bound", not on a slow machine.
	assert.Less(t, elapsed, 30*time.Second,
		"a stalled LSP server held the turn for %v; the diagnostics call must be bounded", elapsed)

	// The other half of "does not block": the edit itself still happened. A
	// turn that bounded the call by abandoning the write would also be fast.
	got, err := os.ReadFile(filepath.Join(workdir, "main.go"))
	require.NoError(t, err)
	assert.Contains(t, string(got), "undefinedCall()",
		"the edit was lost while the LSP call timed out")
}

// TestE2E_NoLSPStillCompletesTheEdit is the degradation half.
//
// TestDiagnosticsLSPUnavailableIsLocalDegradation already asserts the
// diagnostics TOOL reports available:false when no manager is bound. It says
// nothing about the edit tools, which call the same context lookup on a path
// where the user asked for a file change, not for diagnostics. "Safe
// degradation" for them means the edit still lands and the model still gets a
// normal success — not an error mentioning a subsystem the user never enabled.
//
// The same fixture as the two tests above, with the LSP field left nil.
//
// ledger: B2/LSP1#2 server 缺失安全降级
func TestE2E_NoLSPStillCompletesTheEdit(t *testing.T) {
	workdir, mdl, fs, profile := lspTurnFixture(t)

	o, err := New(Config{Model: mdl, Tools: []BaseTool{fs.Edit}, Profile: profile}) // no LSP
	require.NoError(t, err)

	out, err := o.Query(context.Background(), "fix main.go")
	require.NoError(t, err)
	assert.Equal(t, "done", out)

	got, err := os.ReadFile(filepath.Join(workdir, "main.go"))
	require.NoError(t, err)
	assert.Contains(t, string(got), "undefinedCall()", "the edit did not land without an LSP manager")

	// The result must be a normal success envelope: the edit's own fields, no
	// error, and no diagnostics section invented for a subsystem that is not
	// running. Parsing rather than substring-matching because the workdir is a
	// t.TempDir whose name contains the test's own name — an earlier version
	// searched for "lsp" in the raw text and matched the path.
	for _, m := range mdl.ReceivedMessages {
		if m.Role != schema.Tool {
			continue
		}
		var env struct {
			Edited       string `json:"edited"`
			Replacements int    `json:"replacements"`
			Diagnostics  string `json:"diagnostics"`
			Error        string `json:"error"`
		}
		require.NoError(t, json.Unmarshal([]byte(m.Content), &env),
			"tool result is not the usual JSON envelope: %q", m.Content)
		assert.Empty(t, env.Error, "the edit reported an error with no LSP manager bound")
		assert.Equal(t, 1, env.Replacements, "the edit did not report its own work")
		assert.Empty(t, env.Diagnostics,
			"a diagnostics section appeared with no LSP manager: %q", env.Diagnostics)
	}
}
