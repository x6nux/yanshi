package acp

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClient_CloseClosesWriter verifies that Client.Close() closes the writer
// (stdin pipe) when it implements io.Closer. This ensures the agent subprocess
// sees EOF on stdin and can exit, preventing cmd.Wait() from deadlocking.
func TestClient_CloseClosesWriter(t *testing.T) {
	// Create a pipe pair. The writer end (w) will be passed to NewClient.
	// After Client.Close(), w should be closed so writes fail.
	r, w := io.Pipe()
	defer r.Close()

	// dummyR is a reader for the client's "in" side (unused in this test).
	dummyR, dummyW := io.Pipe()
	defer dummyR.Close()
	defer dummyW.Close()

	c := NewClient(dummyR, w)

	// Close the client — should close w (the pipe writer).
	c.Close()

	// Verify: a write to w should now fail (pipe closed).
	done := make(chan error, 1)
	go func() {
		_, err := w.Write([]byte("test"))
		done <- err
	}()
	select {
	case err := <-done:
		assert.Error(t, err, "write after Close should fail (pipe closed)")
	case <-time.After(time.Second):
		t.Fatal("write after Close did not return within 1s — writer was not closed")
	}
}

// TestClient_CloseClosesWriterWithFakeAgent verifies end-to-end that after
// Client.Close(), the FakeAgent's ReadLoop exits (proving the stdin pipe was
// closed and the agent saw EOF). This mirrors the real subprocess scenario
// where the agent blocks on stdin read until EOF.
func TestClient_CloseClosesWriterWithFakeAgent(t *testing.T) {
	fa, clientIn, clientOut := NewFakeAgent()
	defer fa.Close()

	c := NewClient(clientIn, clientOut)
	ctx := context.Background()

	// Initialize to start the ReadLoop.
	_, err := c.Initialize(ctx, ClientCapabilities{})
	require.NoError(t, err)

	// Close the client — should close the writer (clientOut pipe writer),
	// which causes the FakeAgent's reader to see EOF and exit its ReadLoop.
	c.Close()

	// Wait for the FakeAgent's ReadLoop to exit (it closes fa.done on exit).
	select {
	case <-fa.done:
		// FakeAgent ReadLoop exited — good, stdin was closed.
	case <-time.After(2 * time.Second):
		t.Fatal("FakeAgent ReadLoop did not exit after Client.Close — stdin pipe not closed")
	}
}
