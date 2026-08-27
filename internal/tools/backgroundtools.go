// Model-facing tools for the background runs T3 creates.
//
// A handle nothing can query is a token the model is told to remember and can
// never use. These three close that: list what is running, read one run's
// result, stop one. They are the query half of background.go — see that file
// for the lifecycle and for why the completion notice is not a tool result.
package tools

import (
	"context"
	"errors"
	"time"

	"github.com/cloudwego/eino/schema"
)

// backgroundToolTimeout bounds the three query tools. They only read a mutex-
// guarded map, so anything above a second is already generous; the value is
// non-zero because NewGuardedTool rejects 0 (an already-expired context).
const backgroundToolTimeout = 10 * time.Second

// BackgroundTools exposes the query surface over BackgroundManager:
// background_list / background_result / background_cancel.
type BackgroundTools struct {
	List   *GuardedTool
	Result *GuardedTool
	Cancel *GuardedTool
}

// NewBackgroundTools builds the three background query tools. They read the
// manager from context (WithBackgroundManager) rather than closing over one,
// so a sub-agent with its own manager is not served the parent's runs.
func NewBackgroundTools() *BackgroundTools {
	bt := &BackgroundTools{}
	bt.List = NewGuardedTool(
		"background_list", "Background",
		"List tool calls that were moved to the background, with their state.",
		backgroundToolTimeout, nil, SyncStream(runBackgroundList),
	)
	bt.Result = NewGuardedTool(
		"background_result", "Background",
		"Read one background run by id, including its output once it has finished.",
		backgroundToolTimeout,
		params(map[string]*schema.ParameterInfo{
			"id": {Type: schema.String, Desc: "background run id (e.g. bg-1)", Required: true},
		}),
		SyncStream(runBackgroundResult),
	)
	bt.Cancel = NewGuardedTool(
		"background_cancel", "Background",
		"Cancel a still-running background tool call by id.",
		backgroundToolTimeout,
		params(map[string]*schema.ParameterInfo{
			"id": {Type: schema.String, Desc: "background run id (e.g. bg-1)", Required: true},
		}),
		SyncStream(runBackgroundCancel),
	)
	return bt
}

// Tools returns the three tools as a slice, for registration.
func (b *BackgroundTools) Tools() []*GuardedTool {
	return []*GuardedTool{b.List, b.Result, b.Cancel}
}

// errNoBackgroundManager is returned to the MODEL (as a result, not a Go
// error) when no manager is bound. That happens in a sub-agent or a headless
// invocation, where nothing can have been backgrounded in the first place, so
// the honest answer is "this scope has none" rather than a failure.
var errNoBackgroundManager = errors.New("no background runs in this scope")

// backgroundIDArgs is the shared argument shape of result and cancel.
type backgroundIDArgs struct {
	ID string `json:"id"`
}

// runBackgroundList reports every background run, newest first.
func runBackgroundList(ctx context.Context, _ string) (string, error) {
	mgr, ok := BackgroundManagerFromContext(ctx)
	if !ok {
		return errorResult(errNoBackgroundManager.Error()), nil
	}
	runs := mgr.List()
	if len(runs) == 0 {
		return "No tool calls have been moved to the background.", nil
	}
	return toJSON(struct {
		Runs []BackgroundRun `json:"runs"`
	}{Runs: runs}), nil
}

// runBackgroundResult reads one run.
//
// A run that is still going returns its state and how long it has been
// running, NOT a block until it finishes. Blocking here would spend the very
// turn the offload was meant to free, and the completion notice already
// reaches the model without being asked for.
func runBackgroundResult(ctx context.Context, argsJSON string) (string, error) {
	mgr, ok := BackgroundManagerFromContext(ctx)
	if !ok {
		return errorResult(errNoBackgroundManager.Error()), nil
	}
	var args backgroundIDArgs
	if err := ParseArgs(argsJSON, &args); err != nil {
		return "", err
	}
	run, found := mgr.Get(args.ID)
	if !found {
		return errorResult("no background run with id " + args.ID), nil
	}
	return toJSON(struct {
		Run     BackgroundRun `json:"run"`
		Elapsed string        `json:"elapsed"`
	}{Run: run, Elapsed: backgroundElapsed(run).String()}), nil
}

// backgroundElapsed is how long the run has been going, or how long it took.
func backgroundElapsed(run BackgroundRun) time.Duration {
	if run.EndedAt.IsZero() {
		return time.Since(run.StartedAt).Truncate(time.Second)
	}
	return run.EndedAt.Sub(run.StartedAt).Truncate(time.Second)
}

// runBackgroundCancel stops a running background call.
//
// The result says "cancellation requested", not "cancelled": the state only
// flips when the run's goroutine actually unwinds. See
// BackgroundManager.Cancel for why the two are not collapsed.
func runBackgroundCancel(ctx context.Context, argsJSON string) (string, error) {
	mgr, ok := BackgroundManagerFromContext(ctx)
	if !ok {
		return errorResult(errNoBackgroundManager.Error()), nil
	}
	var args backgroundIDArgs
	if err := ParseArgs(argsJSON, &args); err != nil {
		return "", err
	}
	if !mgr.Cancel(args.ID) {
		return errorResult("background run " + args.ID + " is unknown or already finished"), nil
	}
	return "Cancellation requested for background run " + args.ID +
		"; it stops once the running command unwinds.", nil
}
