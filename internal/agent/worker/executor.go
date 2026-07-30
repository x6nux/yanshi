// Package worker implements the remote agent worker loop: it connects to
// the yanshi Task API over HTTP, listens for task_available signals via
// SSE, claims tasks, executes them via a pluggable Executor, and reports
// results back to the server.
package worker

import (
	"context"

	"github.com/x6nux/yanshi/internal/store"
)

// Executor executes a single task and returns its result.
// On error, the worker reports status "failed" to the Task API.
type Executor interface {
	Execute(ctx context.Context, t store.Task) (result string, err error)
}

// EchoExecutor returns the task's Input unchanged. It is the demo executor
// for M5; M6 will swap in a real agent-backed executor.
type EchoExecutor struct{}

// Execute returns the task's Input verbatim.
func (EchoExecutor) Execute(_ context.Context, t store.Task) (string, error) {
	return t.Input, nil
}
