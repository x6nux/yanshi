package tools

import (
	"context"
	"testing"
)

func TestWithErrCounter(t *testing.T) {
	ctx := WithErrCounter(context.Background())
	c := getErrCounter(ctx)
	if c == nil {
		t.Fatal("WithErrCounter should inject a counter into context")
	}
	if c.value() != 0 {
		t.Fatalf("expected 0, got %d", c.value())
	}
	// Mutate and verify identity: the same counter comes back out.
	if c.fail(100) {
		t.Fatal("one fail must not trip a threshold of 100")
	}
	if got := getErrCounter(ctx); got != c {
		t.Fatal("getErrCounter must return the same counter instance")
	}
	if got := getErrCounter(ctx).value(); got != 1 {
		t.Fatalf("value after one fail = %d, want 1", got)
	}
	getErrCounter(ctx).reset()
	if getErrCounter(ctx).value() != 0 {
		t.Fatal("reset must zero the counter")
	}
}

func TestGetErrCounterNil(t *testing.T) {
	c := getErrCounter(context.Background())
	if c != nil {
		t.Fatal("getErrCounter without WithErrCounter should return nil")
	}
}

// TestErrCounterBreakerThreshold pins the breaker arithmetic on the atomic
// counter: the Nth fail trips, reset restarts the count.
func TestErrCounterBreakerThreshold(t *testing.T) {
	c := &errCounter{}
	for i := 1; i < 5; i++ {
		if c.fail(5) {
			t.Fatalf("fail #%d tripped the breaker early", i)
		}
	}
	if !c.fail(5) {
		t.Fatal("fail #5 must trip the breaker")
	}
	c.reset()
	if c.fail(5) {
		t.Fatal("after reset the first fail must not trip")
	}
}
