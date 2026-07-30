package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestParseCaps covers empty input, single, multiple, and blank-element cases.
func TestParseCaps(t *testing.T) {
	assert.Nil(t, parseCaps(""))
	assert.Equal(t, []string{"go"}, parseCaps("go"))
	assert.Equal(t, []string{"go", "py"}, parseCaps("go,py"))
	// Whitespace is trimmed and blanks are dropped.
	assert.Equal(t, []string{"go", "py"}, parseCaps(" go , , py , "))
}

// TestRunWorkerMissingFlags proves a missing required flag returns usage (2).
func TestRunWorkerMissingFlags(t *testing.T) {
	cases := [][]string{
		nil,
		{"-server", "http://x"},               // missing token + name
		{"-token", "t", "-name", "n"},         // missing server
		{"-server", "http://x", "-name", "n"}, // missing token
	}
	for i, args := range cases {
		var stderr bytes.Buffer
		code := runWorker(args, &stderr)
		assert.Equal(t, 2, code, "case %d args=%v stderr=%s", i, args, stderr.String())
	}
}

// TestRunWorkerBadFlag proves a flag parse error returns 2.
func TestRunWorkerBadFlag(t *testing.T) {
	var stderr bytes.Buffer
	code := runWorker([]string{"-nope"}, &stderr)
	assert.Equal(t, 2, code)
}

// withRunAgent swaps the runAgent seam for fn and restores it on cleanup.
func withRunAgent(t *testing.T, fn func(ctx context.Context, server, token, name string, caps []string) error) {
	t.Helper()
	prev := runAgent
	runAgent = fn
	t.Cleanup(func() { runAgent = prev })
}

// TestRunWorkerRunError proves a runAgent error (with a live ctx) maps to exit 1.
func TestRunWorkerRunError(t *testing.T) {
	withRunAgent(t, func(ctx context.Context, server, token, name string, caps []string) error {
		return errors.New("sse down")
	})
	var stderr bytes.Buffer
	code := runWorker([]string{"-server", "http://x", "-token", "t", "-name", "n", "-caps", "a,b"}, &stderr)
	assert.Equal(t, 1, code, stderr.String())
	assert.Contains(t, stderr.String(), "sse down")
}

// TestRunWorkerRunOK proves a nil runAgent result maps to exit 0 and the caps
// are parsed and forwarded.
func TestRunWorkerRunOK(t *testing.T) {
	var gotCaps []string
	withRunAgent(t, func(ctx context.Context, server, token, name string, caps []string) error {
		gotCaps = caps
		assert.Equal(t, "http://x", server)
		assert.Equal(t, "n", name)
		return nil
	})
	var stderr bytes.Buffer
	code := runWorker([]string{"-server", "http://x", "-token", "t", "-name", "n", "-caps", "a, b"}, &stderr)
	assert.Equal(t, 0, code)
	assert.Equal(t, []string{"a", "b"}, gotCaps)
}

// TestRunAgentCancelledCtx covers the real runAgent closure (NewClient +
// NewWorker + w.Run): a pre-cancelled context makes worker.Run return promptly
// via connectEvents' ctx.Err() check, so the closure runs without blocking on
// the SSE reconnect loop.
func TestRunAgentCancelledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Call the real (un-injected) runAgent closure directly.
	err := runAgent(ctx, "http://127.0.0.1:1", "t", "n", []string{"go"})
	assert.Error(t, err) // context.Canceled propagated from connectEvents
}
