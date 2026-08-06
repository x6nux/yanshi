package rlm_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/rlm"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// delayedFake 在每次 Generate 调用上阻塞，直到 gate 关闭；用于证明 MaxConcurrency
// 被严格 cap。嵌入 *einollm.FakeModel 以复用 GenerateCalls/StreamCalls 字段。
type delayedFake struct {
	*einollm.FakeModel
	gate   <-chan struct{}
	active int32
	max    int32
}

func (m *delayedFake) Generate(
	ctx context.Context,
	messages []*schema.Message,
	opts ...model.Option,
) (*schema.Message, error) {
	current := atomic.AddInt32(&m.active, 1)
	for {
		old := atomic.LoadInt32(&m.max)
		if current <= old || atomic.CompareAndSwapInt32(&m.max, old, current) {
			break
		}
	}
	defer atomic.AddInt32(&m.active, -1)
	select {
	case <-m.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return m.FakeModel.Generate(ctx, messages, opts...)
}

// ledger: C1/RLM1#2 顺序对应
func TestRunUsesGenerateAndPreservesOrder(t *testing.T) {
	fake := einollm.NewFakeModel([]string{"ok-a", "ok-b", "ok-c"}, nil)
	runner := rlm.Runner{Model: fake, MaxConcurrency: 4}
	results, err := runner.Run(context.Background(), []string{"a", "b", "c"})
	require.NoError(t, err)
	require.Len(t, results, 3)
	for i, result := range results {
		assert.Equal(t, i, result.Index, "Index")
		assert.NotEmpty(t, result.Output, "Output must not be empty for index %d", i)
	}
	// FakeModel.GenerateCalls / StreamCalls 是 struct field，不是方法。
	assert.Equal(t, 3, fake.GenerateCalls, "GenerateCalls")
	assert.Equal(t, 0, fake.StreamCalls, "StreamCalls")
}

// ledger: C1/RLM1#3 cap 生效
func TestRunCapsConcurrencyAtSixteen(t *testing.T) {
	gate := make(chan struct{})
	fake := &delayedFake{
		FakeModel: einollm.NewFakeModel([]string{"ok"}, nil),
		gate:      gate,
	}
	runner := rlm.Runner{Model: fake, MaxConcurrency: 99}
	done := make(chan struct{})
	go func() {
		_, _ = runner.Run(context.Background(), makePrompts(32))
		close(done)
	}()
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&fake.active) < 16 {
		select {
		case <-deadline:
			t.Fatalf("did not reach the 16-call cap; active=%d", atomic.LoadInt32(&fake.active))
		default:
			time.Sleep(time.Millisecond)
		}
	}
	assert.LessOrEqual(t, atomic.LoadInt32(&fake.max), int32(16), "max active must be <= 16")
	close(gate)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not finish")
	}
}

func TestRunKeepsPerItemErrors(t *testing.T) {
	fake := einollm.NewFakeModel(nil, errors.New("model unavailable"))
	runner := rlm.Runner{Model: fake, MaxConcurrency: 2}
	results, err := runner.Run(context.Background(), []string{"a", "b"})
	require.NoError(t, err, "per-item errors must not surface as batch error")
	for i, result := range results {
		assert.Equal(t, i, result.Index)
		assert.Equal(t, "model unavailable", result.Error)
	}
}

func TestRunMarksPendingItemsWhenCanceled(t *testing.T) {
	gate := make(chan struct{})
	fake := &delayedFake{
		FakeModel: einollm.NewFakeModel([]string{"ok"}, nil),
		gate:      gate,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results, err := (rlm.Runner{Model: fake, MaxConcurrency: 4}).Run(ctx, []string{"a", "b"})
	require.NoError(t, err)
	for i, result := range results {
		assert.Equal(t, i, result.Index)
		assert.Equal(t, context.Canceled.Error(), result.Error)
	}
}

func TestRunRejectsNilModel(t *testing.T) {
	_, err := (rlm.Runner{}).Run(context.Background(), []string{"a"})
	require.Error(t, err)
}

func makePrompts(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("prompt-%d", i)
	}
	return out
}

var _ model.BaseChatModel = (*delayedFake)(nil)
