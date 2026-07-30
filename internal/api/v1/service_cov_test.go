package v1

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/store"
)

// TestCov_Start_StoreCreateError covers the CreateSession error branch at
// service.go:111-113 by closing the store's DB before calling Start.
func TestCov_Start_StoreCreateError(t *testing.T) {
	db, err := store.Open(":memory:")
	require.NoError(t, err)
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := NewService(Config{DefaultModel: model, Store: db})
	require.NoError(t, err)

	// Close the DB so CreateSession fails on the next call.
	require.NoError(t, db.Close())

	_, err = svc.Start(context.Background(), ThreadStartParams{Title: "t"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "create thread")
}

// TestCov_Resume_StoreGetError covers the GetSession error branch at
// service.go:147-148 by closing the store's DB before calling Resume.
func TestCov_Resume_StoreGetError(t *testing.T) {
	db, err := store.Open(":memory:")
	require.NoError(t, err)
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := NewService(Config{DefaultModel: model, Store: db})
	require.NoError(t, err)

	require.NoError(t, db.Close())

	_, err = svc.Resume(context.Background(), ThreadResumeParams{ThreadID: "no-such"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "read thread")
}

// TestCov_StartTurn_ContextCanceled covers the sendItem-false branch at
// service.go:234 by passing an already-cancelled context so the first
// turn.started emit fails before any orchestrator work starts.
func TestCov_StartTurn_ContextCanceled(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := NewService(Config{DefaultModel: model})
	require.NoError(t, err)

	thread, err := svc.Start(context.Background(), ThreadStartParams{Title: "t"})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before StartTurn so the child ctx is already done

	_, _, err = svc.StartTurn(ctx, TurnStartParams{
		ThreadID: thread.ID,
		Input:    "hello",
	})
	// Returns context.Canceled, not a typed error.
	assert.ErrorIs(t, err, context.Canceled)
}
