package tools

import (
	"context"
	"testing"
)

func TestWithErrCounter(t *testing.T) {
	ctx := WithErrCounter(context.Background())
	c := getErrCounter(ctx)
	if c == nil {
		t.Fatal("WithErrCounter should inject a *int into context")
	}
	if *c != 0 {
		t.Fatalf("expected 0, got %d", *c)
	}
	// Mutate and verify pointer identity.
	*c = 42
	if *getErrCounter(ctx) != 42 {
		t.Fatal("getErrCounter should return the same pointer")
	}
}

func TestGetErrCounterNil(t *testing.T) {
	c := getErrCounter(context.Background())
	if c != nil {
		t.Fatal("getErrCounter without WithErrCounter should return nil")
	}
}
