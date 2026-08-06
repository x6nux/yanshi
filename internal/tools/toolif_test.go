package tools

import (
	"context"
	"strings"
	"testing"
	"time"
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
// ledger: M1/SPEC-TOOLIF#2 输出走固定字段 ToolChunk
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
