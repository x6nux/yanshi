package tools

import (
	"context"
	"testing"

	"github.com/x6nux/yanshi/internal/proto"
)

func TestWithSubAgentRunner(t *testing.T) {
	runner := SubAgentRunner(func(ctx context.Context, prompt string, allowed []string, instr string) (string, error) {
		return "ok", nil
	})
	ctx := WithSubAgentRunner(context.Background(), runner)
	if SubAgentRunnerFromContext(ctx) == nil {
		t.Fatal("SubAgentRunnerFromContext should return non-nil")
	}
	// Nil runner should return ctx unchanged.
	base := context.Background()
	ctx2 := WithSubAgentRunner(base, nil)
	if ctx2 != base {
		t.Fatal("WithSubAgentRunner(nil) should return ctx unchanged")
	}
	// Unbound context.
	if SubAgentRunnerFromContext(base) != nil {
		t.Fatal("SubAgentRunnerFromContext on bare ctx should return nil")
	}
}

func TestWithSubAgentDepth(t *testing.T) {
	ctx := WithSubAgentDepth(context.Background(), 2)
	d := SubAgentDepth(ctx)
	if d != 2 {
		t.Fatalf("SubAgentDepth = %d, want 2", d)
	}
	// Default depth is 0.
	if SubAgentDepth(context.Background()) != 0 {
		t.Fatal("SubAgentDepth on bare ctx should return 0")
	}
}

func TestLeafSubAgentTools(t *testing.T) {
	ctx := WithLeafSubAgentTools(context.Background())
	if !LeafSubAgentTools(ctx) {
		t.Fatal("LeafSubAgentTools should be true after WithLeafSubAgentTools")
	}
	if LeafSubAgentTools(context.Background()) {
		t.Fatal("LeafSubAgentTools on bare ctx should be false")
	}
}

func TestWithSubAgentEmitContext(t *testing.T) {
	var emit SubAgentEmit = func(frame proto.ServerFrame) {}
	ctx := WithSubAgentEmit(context.Background(), emit)
	emitter := SubAgentEmitFrom(ctx)
	if emitter == nil {
		t.Fatal("SubAgentEmitFrom should return non-nil")
	}
	// Verify the emit function was stored correctly.
	emitter(proto.ServerFrame{})
	// Nil emit is a no-op.
	base := context.Background()
	if WithSubAgentEmit(base, nil) != base {
		t.Fatal("WithSubAgentEmit(nil) should return ctx unchanged")
	}
	// Unbound context.
	if SubAgentEmitFrom(base) != nil {
		t.Fatal("SubAgentEmitFrom on bare ctx should return nil")
	}
}

func TestWithSubAgentProgressContext(t *testing.T) {
	var got SubAgentEvent
	cb := func(ev SubAgentEvent) { got = ev }
	ctx := WithSubAgentProgress(context.Background(), cb)
	retrieved := SubAgentProgressFromContext(ctx)
	if retrieved == nil {
		t.Fatal("SubAgentProgressFromContext should return non-nil callback")
	}
	ev := SubAgentEvent{Kind: SubAgentToolStart, ToolDisplay: "test", ToolArgs: "args", Tokens: 100}
	retrieved(ev)
	if got.ToolDisplay != "test" || got.ToolArgs != "args" || got.Tokens != 100 {
		t.Fatalf("callback didn't capture event: %+v", got)
	}
	// Nil callback is no-op.
	base := context.Background()
	if WithSubAgentProgress(base, nil) != base {
		t.Fatal("WithSubAgentProgress(nil) should return ctx unchanged")
	}
	// Unbound context.
	if SubAgentProgressFromContext(base) != nil {
		t.Fatal("SubAgentProgressFromContext on bare ctx should return nil")
	}
}

func TestWithWorkflowProgressContext(t *testing.T) {
	wp := &WorkflowProgress{SetTotal: func(int) {}, StepCB: func(string) func(SubAgentEvent) { return nil }, StepDone: func(string, error) {}}
	ctx := WithWorkflowProgress(context.Background(), wp)
	retrieved := WorkflowProgressFromContext(ctx)
	if retrieved == nil {
		t.Fatal("WorkflowProgressFromContext should return non-nil")
	}
	// Nil is no-op.
	base := context.Background()
	if WithWorkflowProgress(base, nil) != base {
		t.Fatal("WithWorkflowProgress(nil) should return ctx unchanged")
	}
	if WorkflowProgressFromContext(base) != nil {
		t.Fatal("WorkflowProgressFromContext on bare ctx should return nil")
	}
}

func TestSubAgentDepthUnbound(t *testing.T) {
	if SubAgentDepth(context.Background()) != 0 {
		t.Fatal("SubAgentDepth on bare ctx should be 0")
	}
}
