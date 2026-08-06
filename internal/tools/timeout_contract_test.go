package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/guard"
)

// TestZeroTimeoutPanicsAtConstruction pins the fix for a defect whose whole
// danger was that it did not look like one.
//
// context.WithTimeout(ctx, 0) produces an ALREADY-EXPIRED context. A tool
// registered with timeout 0 therefore fails on its first line, every time --
// and GuardedTool hands that failure back as a tool RESULT, not an error, so
// the ReAct loop treats "context deadline exceeded" as something the model
// should react to. The model retries, fails again, and burns tokens in a loop
// that never raises anything. Nothing crashes, nothing logs, no test fails.
//
// 0 reads as "unset" to anyone writing a registration, so the mismatch is
// between two reasonable readings, not carelessness. Refusing it at
// construction is the only place where the mistake is still cheap.
func TestZeroTimeoutPanicsAtConstruction(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewGuardedTool with timeout 0 must panic")
		}
		if !strings.Contains(strings.ToLower(err2str(r)), "timeout") {
			t.Fatalf("panic message should name the problem, got %v", r)
		}
	}()
	NewGuardedTool("x", "X", "d", 0, nil, func(context.Context, string) <-chan ToolChunk { return nil })
}

func err2str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if e, ok := v.(error); ok {
		return e.Error()
	}
	return ""
}

// TestNoTimeoutRunsButStillObeysTheTurnContext pins both halves of the
// unbounded case. NoTimeout must not be implemented by swapping one
// immediately-expiring duration for another -- a negative duration expires
// just as instantly as zero under WithTimeout -- and it must not mean "runs
// forever", because the turn context is a real and honest upper bound.
func TestNoTimeoutRunsButStillObeysTheTurnContext(t *testing.T) {
	started := make(chan struct{})
	tool := NewGuardedTool("x", "X", "d", NoTimeout,
		nil,
		func(ctx context.Context, _ string) <-chan ToolChunk {
			ch := make(chan ToolChunk, 2)
			go func() {
				defer close(ch)
				close(started)
				<-ctx.Done() // blocks until the TURN is cancelled, not a timeout
				ch <- ToolChunk{Err: ctx.Err()}
			}()
			return ch
		})

	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"x"}},
	})
	ctx, cancel := context.WithCancel(ctx)

	out := tool.Stream(ctx, "{}")
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("a NoTimeout tool never started: it was cancelled before running")
	}

	// It is still running here, which is the first half: no deadline fired.
	cancel()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-out:
			if !ok {
				return // channel closed after cancel: the turn ctx propagated
			}
		case <-deadline:
			t.Fatal("cancelling the turn did not stop a NoTimeout tool")
		}
	}
}

// TestNoTimeoutIsNotZero guards the sentinel itself. If NoTimeout were ever
// defined as 0 the construction panic above would fire on every unbounded
// tool, and if it were defined as some large positive duration the contract
// would silently become "very long" instead of "turn-bounded".
func TestNoTimeoutIsNotZero(t *testing.T) {
	if NoTimeout == 0 {
		t.Fatal("NoTimeout must not be 0: that is the value the constructor rejects")
	}
	if NoTimeout > 0 {
		t.Fatalf("NoTimeout must be a sentinel, not a long duration, got %v", NoTimeout)
	}
}
