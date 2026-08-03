package eino

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBlockingModel_BlocksUntilReleased proves a call stays blocked until Block
// is closed, then returns Response. This is what makes a mid-turn cancel test
// deterministic: the turn cannot end on its own.
func TestBlockingModel_BlocksUntilReleased(t *testing.T) {
	m := NewBlockingModel("hi")
	done := make(chan struct{})
	go func() {
		msg, err := m.Generate(context.Background(), nil)
		assert.NoError(t, err)
		assert.Equal(t, "hi", msg.Content)
		close(done)
	}()

	<-m.Started
	// Still blocked: a short wait must not free the goroutine.
	select {
	case <-done:
		t.Fatal("Generate returned before Block was closed")
	case <-time.After(50 * time.Millisecond):
	}
	close(m.Block)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Generate did not return after Block closed")
	}
}

// TestBlockingModel_ContextCancel proves a call returns ctx.Err() when the
// context fires before Block — i.e. the model respects cancellation, which is
// what lets a WS cancel frame abort an in-flight turn.
func TestBlockingModel_ContextCancel(t *testing.T) {
	m := NewBlockingModel("hi")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := m.Generate(ctx, []*schema.Message{schema.UserMessage("x")})
		done <- err
	}()

	<-m.Started
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
		assert.True(t, errors.Is(err, context.Canceled))
	case <-time.After(time.Second):
		t.Fatal("Generate did not return after context cancel")
	}
}

// TestBlockingModel_Stream proves the Stream path works.
func TestBlockingModel_Stream(t *testing.T) {
	m := NewBlockingModel("streamed-ok")
	done := make(chan error, 1)
	go func() {
		sr, err := m.Stream(context.Background(), nil)
		if err != nil {
			done <- err
			return
		}
		defer sr.Close()
		msg, recvErr := sr.Recv()
		if recvErr != nil {
			done <- recvErr
			return
		}
		if msg.Content != "streamed-ok" {
			done <- fmt.Errorf("expected streamed-ok, got %s", msg.Content)
			return
		}
		done <- nil
	}()

	<-m.Started
	close(m.Block)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stream did not return after Block closed")
	}
}
