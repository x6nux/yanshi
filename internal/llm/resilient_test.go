package llm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClient is a configurable Client for tests.
type fakeClient struct {
	name         string
	calls        int32
	failFor      int32 // number of leading calls that return a retryable err
	nonRetryable bool
}

func (f *fakeClient) Name() string { return f.name }
func (f *fakeClient) Chat(ctx context.Context, _ []Message) (Response, error) {
	n := atomic.AddInt32(&f.calls, 1)
	if n <= f.failFor {
		if f.nonRetryable {
			return Response{}, errors.New("boom")
		}
		return Response{}, Retryable(errors.New("transient"))
	}
	return Response{Content: "ok"}, nil
}

func fastCfg() ResilientConfig {
	return ResilientConfig{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
}

func TestResilient_RetriesThenSucceeds(t *testing.T) {
	f := &fakeClient{name: "p1", failFor: 2}
	r, err := NewResilient([]Client{f}, fastCfg())
	require.NoError(t, err)

	resp, err := r.Chat(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Content)
	assert.Equal(t, int32(3), atomic.LoadInt32(&f.calls)) // 2 failures + 1 success
}

func TestResilient_NonRetryableBails(t *testing.T) {
	f := &fakeClient{name: "p1", failFor: 9, nonRetryable: true}
	r, err := NewResilient([]Client{f}, fastCfg())
	require.NoError(t, err)

	_, err = r.Chat(context.Background(), nil)
	require.Error(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&f.calls)) // no retries on non-retryable
}

func TestResilient_FailoverToNextProvider(t *testing.T) {
	bad := &fakeClient{name: "bad", failFor: 9} // always retryable-fails
	good := &fakeClient{name: "good", failFor: 0}
	r, err := NewResilient([]Client{bad, good}, fastCfg())
	require.NoError(t, err)

	resp, err := r.Chat(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Content)
	assert.Equal(t, int32(4), atomic.LoadInt32(&bad.calls)) // MaxRetries+1 attempts
	assert.Equal(t, int32(1), atomic.LoadInt32(&good.calls))
}

func TestResilient_AllFail(t *testing.T) {
	a := &fakeClient{name: "a", failFor: 9}
	b := &fakeClient{name: "b", failFor: 9}
	r, err := NewResilient([]Client{a, b}, fastCfg())
	require.NoError(t, err)

	_, err = r.Chat(context.Background(), nil)
	require.Error(t, err)
}

func TestResilient_EmptyChain(t *testing.T) {
	_, err := NewResilient(nil, fastCfg())
	require.Error(t, err)
}

func TestResilient_DefaultMaxRetries(t *testing.T) {
	// A zero-value config should default to MaxRetries=3, not 0.
	f := &fakeClient{name: "p1", failFor: 9} // always fails retryable
	r, err := NewResilient([]Client{f}, ResilientConfig{})
	require.NoError(t, err)
	assert.Equal(t, 3, r.cfg.MaxRetries)

	_, err = r.Chat(context.Background(), nil)
	require.Error(t, err)
	// MaxRetries=3 means 4 attempts (1 initial + 3 retries).
	assert.Equal(t, int32(4), atomic.LoadInt32(&f.calls))
}

func TestResilient_RespectsContextCancel(t *testing.T) {
	f := &fakeClient{name: "p1", failFor: 9}
	cfg := ResilientConfig{MaxRetries: 10, BaseDelay: 200 * time.Millisecond, MaxDelay: time.Second}
	r, err := NewResilient([]Client{f}, cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = r.Chat(ctx, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestResilient_BackoffCapsOverflow(t *testing.T) {
	cfg := ResilientConfig{MaxRetries: 3, BaseDelay: 200 * time.Millisecond, MaxDelay: 5 * time.Second}
	r, err := NewResilient([]Client{&fakeClient{name: "p1"}}, cfg)
	require.NoError(t, err)

	// Large attempt would overflow int64 ns -> negative; must be clamped to MaxDelay.
	assert.Equal(t, 5*time.Second, r.backoff(60))
	// Normal case is unchanged.
	assert.Equal(t, 200*time.Millisecond, r.backoff(1))
}

func TestResilient_PreCancelledContext(t *testing.T) {
	f := &fakeClient{name: "p1"}
	r, err := NewResilient([]Client{f}, fastCfg())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = r.Chat(ctx, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
	assert.Equal(t, int32(0), atomic.LoadInt32(&f.calls)) // guard prevented any provider call
}
