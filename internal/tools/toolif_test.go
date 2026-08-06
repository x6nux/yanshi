package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/mcp"
)

// TestModelSeesOnlyResultAndTUISeesEveryField pins the field ownership rule that
// the whole ToolChunk design rests on.
//
// ToolChunk has five fields with disjoint audiences: Result goes to the model,
// Text and Status go to the TUI, Overwrite tells the TUI to replace rather than
// append, Err trips the consecutive-failure breaker. "Output goes through fixed
// ToolChunk fields" is only a real contract if the two consumers actually read
// different fields — a version of InvokableRun that also concatenated Text
// would still compile, still pass every tool's own test (SyncStream sets Text
// and Result to the SAME string, so nothing in the tree would notice), and
// would quietly feed the model every spinner frame and progress line the TUI
// was meant to own.
//
// The fixture therefore gives each field a distinct value, which no production
// tool does. That is the point: identical values are exactly what makes the
// leak invisible.
//
// It also carries the "废弃 JSON 包装" half of clause 3, which the symbol gate
// cannot: the retired JSON envelope left no named symbol behind to forbid, so
// the only thing left to assert is the behaviour that replaced it — the model
// receives the Result fields concatenated and nothing else, not a wrapper
// object carrying progress alongside the answer.
//
// ledger: M1/SPEC-TOOLIF#2 输出走固定字段 ToolChunk
//
// ledger: M1/SPEC-TOOLIF#3 废弃 JSON 包装与 ToolProgressCallback/lineProgressWriter
func TestModelSeesOnlyResultAndTUISeesEveryField(t *testing.T) {
	tl := NewGuardedTool("probe_fields", "Probe", "field ownership probe", time.Minute, nil,
		func(ctx context.Context, argsJSON string) <-chan ToolChunk {
			ch := make(chan ToolChunk, 2)
			ch <- ToolChunk{Status: "STATUS-ONLY", Text: "TEXT-ONLY"}
			ch <- ToolChunk{Text: "TEXT-TWO", Result: "RESULT-ONLY"}
			close(ch)
			return ch
		})

	var seen []ToolChunk
	ctx := WithProfile(context.Background(), defaultTestProfile())
	ctx = WithToolChunkCallback(ctx, func(_ string, c ToolChunk) { seen = append(seen, c) })

	out, err := runTool(ctx, tl, `{}`)
	if err != nil {
		t.Fatal(err)
	}

	if out != "RESULT-ONLY" {
		t.Errorf("the model got %q; it must see the Result fields and nothing else", out)
	}
	for _, leak := range []string{"TEXT-ONLY", "TEXT-TWO", "STATUS-ONLY"} {
		if strings.Contains(out, leak) {
			t.Errorf("%s reached the model: Text and Status belong to the TUI", leak)
		}
	}

	// The other half of the ownership rule: the TUI consumer must still receive
	// the fields the model does not. Without this, deleting Text and Status
	// from every chunk would satisfy the assertions above.
	var gotText, gotStatus bool
	for _, c := range seen {
		gotText = gotText || c.Text != ""
		gotStatus = gotStatus || c.Status != ""
	}
	if !gotText || !gotStatus {
		t.Errorf("the TUI callback saw text=%v status=%v across %d chunks; it must see both",
			gotText, gotStatus, len(seen))
	}
}

// TestSubAgentProgressArrivesAsStatusNotResult drives the real agent_start.
//
// agent_start, workflow_start and analysis run for minutes and emit progress
// while they run. That progress is a TUI concern — "3 tools 1.2k tokens 45s"
// is meaningless to a model and, repeated once per iteration, is pure token
// cost. bindSubAgentProgress in agent.go is what routes it: SubAgentToolStart
// becomes a Text activity line plus a recomputed Status, SubAgentTokens
// becomes a Status alone.
//
// The runner here plays the part the orchestrator plays in production — it is
// the orchestrator that calls SubAgentProgressFromContext and emits the events
// (orchestrator.go) — so the code under test is agent.go's real callback, not
// a hand-built chunk sequence. An earlier version of this test pushed the
// chunks itself and proved only that GuardedTool forwards fields, which is the
// clause above, not this one.
//
// ledger: M1/SPEC-TOOLIF#4 subagent 进度喂 Status+Text
func TestSubAgentProgressArrivesAsStatusNotResult(t *testing.T) {
	at, ctx := newAgentTools(t)

	runner := func(ic context.Context, prompt string, allowed []string, instr string) (string, error) {
		progress := SubAgentProgressFromContext(ic)
		if progress == nil {
			t.Error("agent_start did not bind a progress callback into the sub-agent context")
			return "final answer", nil
		}
		progress(SubAgentEvent{Kind: SubAgentToolStart, ToolDisplay: "Read", ToolArgs: "x.go"})
		progress(SubAgentEvent{Kind: SubAgentTokens, Tokens: 400})
		return "final answer", nil
	}
	ctx = WithSubAgentRunner(ctx, SubAgentRunner(runner))

	var texts, statuses []string
	ctx = WithToolChunkCallback(ctx, func(_ string, c ToolChunk) {
		if c.Text != "" {
			texts = append(texts, c.Text)
		}
		if c.Status != "" {
			statuses = append(statuses, c.Status)
		}
	})

	out, err := runTool(ctx, at.StartAgent, `{"prompt":"do something"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "final answer" {
		t.Errorf("the model got %q; progress frames must not be part of its answer", out)
	}

	// Two events, and the tool-start one pushes both a Text line and a Status,
	// so: 2 statuses, 1 text.
	if len(statuses) != 2 {
		t.Errorf("the TUI got %d status frames, want 2 (one per event): %q", len(statuses), statuses)
	}
	if len(texts) != 1 {
		t.Errorf("the TUI got %d activity lines, want 1 (only tool starts produce one): %q",
			len(texts), texts)
	}
	if len(texts) == 1 && !strings.Contains(texts[0], "Read") {
		t.Errorf("activity line %q does not name the tool the sub-agent called", texts[0])
	}
	// The recomputed stats have to actually move; a Status that never changes
	// would satisfy a pure count.
	if len(statuses) == 2 && statuses[0] == statuses[1] {
		t.Errorf("both status frames read %q: the token event did not update the summary",
			statuses[0])
	}
}

// TestEmptyDisplayNamePanicsAtConstruction is the DisplayName twin of
// TestZeroTimeoutPanicsAtConstruction.
//
// Counting non-empty display names over the registry would report today's
// state; panicking at construction makes the state unreachable. Both
// constructors are checked because NewApprovalGuardedTool once shipped without
// the timeout check the ordinary one already had.
//
// ledger: M1/SPEC-TOOLIF#1 所有工具统一到 Tool 接口（DisplayName+DefaultTimeout+Stream）
func TestEmptyDisplayNamePanicsAtConstruction(t *testing.T) {
	stream := func(ctx context.Context, argsJSON string) <-chan ToolChunk {
		ch := make(chan ToolChunk)
		close(ch)
		return ch
	}
	for _, tc := range []struct {
		name string
		ctor func()
	}{
		{"NewGuardedTool", func() { NewGuardedTool("x", "", "d", time.Minute, nil, stream) }},
		{"NewApprovalGuardedTool", func() { NewApprovalGuardedTool("x", "", "d", time.Minute, nil, stream) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("an empty DisplayName was accepted: the tool's TUI block " +
						"ships unlabelled and no test in the tree would notice")
				}
			}()
			tc.ctor()
		})
	}
}

// TestMCPToolCallIsGatedEndToEnd wires the guard's MCP dimension to a real
// manager-constructed tool.
//
// The guard's mcp dimension has its own unit tests, and the manager has its
// own. Between them sits NewMCPTools, which is what decides that a registered
// MCP tool is named "mcp_<server>_<tool>" — the very prefix checkMCPTools keys
// on. A change to either naming convention alone would leave both sets of unit
// tests green while every MCP call silently skipped the dimension: checkMCPTools
// returns Allow for anything not prefixed "mcp_", so a rename does not deny, it
// EXEMPTS.
//
// The empty-allowlist case is the fail-closed one, and the reason has to be
// legible: "no tools permitted" and "no MCP tools permitted" send the operator
// to different config sections.
//
// ledger: A3/V16#3 启动超时/重连/权限检查有测试
func TestMCPToolCallIsGatedEndToEnd(t *testing.T) {
	srv, _ := mcp.NewFakeHTTPServer([]mcp.ToolDescriptor{{ToolName: "search"}})
	defer srv.Close()

	mgr := mcp.NewManager(map[string]*mcp.ServerConfig{
		"srv": {Enabled: true, Transport: mcp.TransportHTTP, URL: srv.URL},
	})
	ctx := context.Background()
	for _, st := range mgr.StartAll(ctx) {
		if st.Error != "" {
			t.Fatalf("fake server failed to start: %s", st.Error)
		}
	}
	defer mgr.Shutdown()

	mcpTools := NewMCPTools(mgr)
	if len(mcpTools) != 1 {
		t.Fatalf("got %d MCP tools, want 1", len(mcpTools))
	}
	tl := mcpTools[0]
	info, err := tl.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(info.Name, "mcp_") {
		t.Fatalf("registered name %q lacks the mcp_ prefix checkMCPTools keys on: "+
			"the dimension would return Allow for every call", info.Name)
	}

	// Tools.Allow is wide open on purpose: this must be the MCP dimension
	// denying, not the tools one. checkMCPTools runs FIRST for exactly this
	// reason — a broad Tools.Allow must not silently authorise a newly
	// configured MCP server.
	denied := WithMCP(WithProfile(ctx, guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		Net:   guard.NetPerm{Allow: true},
	}), mgr)
	out, err := tl.InvokableRun(denied, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "permission denied") {
		t.Errorf("an empty MCP allowlist did not deny the call: %s", out)
	}
	if !strings.Contains(strings.ToLower(out), "mcp") {
		t.Errorf("the denial does not say it was the MCP dimension: %s\n"+
			"  the operator cannot tell which config section to edit", out)
	}

	// Positive control: with the tool allowed, the guard lets it through. The
	// call itself may still fail against the fake server — what must change is
	// that it is no longer a permission denial.
	allowed := WithMCP(WithProfile(ctx, guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		MCP:   guard.ToolsPerm{Allow: []string{"mcp_*"}},
		Net:   guard.NetPerm{Allow: true},
	}), mgr)
	aout, err := tl.InvokableRun(allowed, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(aout, "permission denied") {
		t.Errorf("an explicit allowlist entry still got denied: %s", aout)
	}
}
