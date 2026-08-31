package ctxcompact_test

// W-F-08 的总线测试。验收「三条压缩路径共用同一总线」在这层的证据是：
// 事件只从 Run / OpenNewWindow 发出（同一处代码），三条路径的差别只在
// Trigger 字段 —— 下面逐条钉住。会变红的变异：
//
//	删掉 Run 里的 emitLifecycle（pre 或 post 任一）→ 本文件对应断言全红；
//	把 OpenNewWindow 的 Trigger 改成读 runOpts → TestOpenNewWindowForcesNewWindow
//	在传入 mid_turn 时变红（调用方标签不再被覆盖）。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/ctxcompact"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// recordingSink 收集全部事件，附一个互斥锁（Run 在调用方 goroutine 上同步
// 调 sink，但测试不为这个契约上锁-free 赌博）。
type recordingSink struct {
	mu     sync.Mutex
	events []ctxcompact.LifecycleEvent
	panic_ bool
}

func (r *recordingSink) record(_ context.Context, ev ctxcompact.LifecycleEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.panic_ {
		panic("sink exploded")
	}
	r.events = append(r.events, ev)
}

func (r *recordingSink) got() []ctxcompact.LifecycleEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ctxcompact.LifecycleEvent(nil), r.events...)
}

func lifecycleFixture() []*schema.Message {
	return []*schema.Message{
		{Role: schema.User, Content: "do the task"},
		{Role: schema.Assistant, Content: strings.Repeat("thinking ", 100)},
		{Role: schema.Assistant, Content: strings.Repeat("more noise ", 100)},
		{Role: schema.User, Content: "recent"},
	}
}

func TestLifecycleEventsFireOnRun(t *testing.T) {
	sink := &recordingSink{}
	fm := einollm.NewFakeModel([]string{"the compacted summary"}, nil)
	ctx := ctxcompact.WithLifecycleSink(context.Background(), sink.record)
	res, err := ctxcompact.Run(ctx, lifecycleFixture(), ctxcompact.PlanOpts{KeepRecent: 1},
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9, Trigger: ctxcompact.TriggerPreTurn}, fm, nil)
	require.NoError(t, err)

	events := sink.got()
	require.Len(t, events, 2, "one pre and one post event on the success path")
	assert.Equal(t, ctxcompact.LifecyclePreCompact, events[0].Phase)
	assert.Equal(t, ctxcompact.TriggerPreTurn, events[0].Trigger)
	assert.Positive(t, events[0].TokensBefore)
	assert.Zero(t, events[0].TokensAfter, "pre event carries no after-count")
	assert.Equal(t, ctxcompact.LifecyclePostCompact, events[1].Phase)
	assert.Equal(t, ctxcompact.TriggerPreTurn, events[1].Trigger)
	assert.Equal(t, res.TokensBefore, events[1].TokensBefore)
	assert.Equal(t, res.TokensAfter, events[1].TokensAfter)
	assert.False(t, events[1].Overflow)
	assert.False(t, events[1].Fallback)
	assert.Empty(t, events[1].Failure)
}

func TestLifecyclePostReportsFailure(t *testing.T) {
	// EMPTY 门（摘要为空）→ Run 返回错误，post 事件带 Failure；pre 已经发出。
	sink := &recordingSink{}
	fm := einollm.NewFakeModel([]string{"   "}, nil) // blank summary → quality/empty gate
	ctx := ctxcompact.WithLifecycleSink(context.Background(), sink.record)
	_, err := ctxcompact.Run(ctx, lifecycleFixture(), ctxcompact.PlanOpts{KeepRecent: 1},
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9, Trigger: ctxcompact.TriggerMidTurn}, fm, nil)
	require.Error(t, err)

	events := sink.got()
	require.Len(t, events, 2)
	assert.Equal(t, ctxcompact.TriggerMidTurn, events[1].Trigger)
	assert.NotEmpty(t, events[1].Failure, "a failed attempt must say so on the post event")
}

func TestLifecyclePostReportsFallback(t *testing.T) {
	// W-C-04 fallback：模型调用失败不是错误，post 事件以 Fallback=true 报告。
	sink := &recordingSink{}
	fm := einollm.NewFakeModel(nil, errors.New("model down"))
	ctx := ctxcompact.WithLifecycleSink(context.Background(), sink.record)
	res, err := ctxcompact.Run(ctx, lifecycleFixture(), ctxcompact.PlanOpts{KeepRecent: 1},
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9, Trigger: ctxcompact.TriggerMidTurn}, fm, nil)
	require.NoError(t, err)
	require.True(t, res.Fallback)

	events := sink.got()
	require.Len(t, events, 2)
	assert.True(t, events[1].Fallback)
	assert.Empty(t, events[1].Failure)
}

func TestOpenNewWindowEmitsNewWindowTrigger(t *testing.T) {
	sink := &recordingSink{}
	ctx := ctxcompact.WithLifecycleSink(context.Background(), sink.record)
	res := ctxcompact.OpenNewWindow(ctx, lifecycleFixture(), ctxcompact.PlanOpts{KeepRecent: 1},
		ctxcompact.RunOpts{ModelWindow: 10000})
	require.NotNil(t, res)

	events := sink.got()
	require.Len(t, events, 2)
	for _, ev := range events {
		assert.Equal(t, ctxcompact.TriggerNewWindow, ev.Trigger)
	}
	assert.Equal(t, ctxcompact.LifecyclePreCompact, events[0].Phase)
	assert.Equal(t, ctxcompact.LifecyclePostCompact, events[1].Phase)
	assert.Equal(t, res.TokensAfter, events[1].TokensAfter)
}

func TestOpenNewWindowForcesNewWindow(t *testing.T) {
	// 调用方即使把 mid_turn 填进 RunOpts 也不能给 new-window 事件贴错标签。
	sink := &recordingSink{}
	ctx := ctxcompact.WithLifecycleSink(context.Background(), sink.record)
	ctxcompact.OpenNewWindow(ctx, lifecycleFixture(), ctxcompact.PlanOpts{KeepRecent: 1},
		ctxcompact.RunOpts{ModelWindow: 10000, Trigger: ctxcompact.TriggerMidTurn})
	for _, ev := range sink.got() {
		assert.Equal(t, ctxcompact.TriggerNewWindow, ev.Trigger)
	}
}

func TestMaybeCompactWithOptionsCarriesTrigger(t *testing.T) {
	// 传输层入口（WS/SSE 用的就是这个）把 Options.Trigger 传到事件上；
	// under-threshold 时 Run 不被调到，也就一个事件都没有 —— 「pre 意味着
	// 真的在尝试压缩」的契约顺带钉住。
	sink := &recordingSink{}
	ctx := ctxcompact.WithLifecycleSink(context.Background(), sink.record)
	msgs := lifecycleFixture()

	_, _, _, did := ctxcompact.MaybeCompactWithOptions(ctx, msgs, 0.8, 100000, 1,
		einollm.NewFakeModel([]string{"SUM"}, nil), nil,
		ctxcompact.Options{Trigger: ctxcompact.TriggerPreTurn})
	assert.False(t, did)
	assert.Empty(t, sink.got(), "under-threshold: no attempt, no events")

	_, _, _, did = ctxcompact.MaybeCompactWithOptions(ctx, msgs, 0.0001, 100000, 1,
		einollm.NewFakeModel([]string{"SUM"}, nil), nil,
		ctxcompact.Options{Trigger: ctxcompact.TriggerPreTurn})
	assert.True(t, did)
	events := sink.got()
	require.NotEmpty(t, events)
	for _, ev := range events {
		assert.Equal(t, ctxcompact.TriggerPreTurn, ev.Trigger)
	}
}

func TestSinkPanicDoesNotBreakCompaction(t *testing.T) {
	// Ruling 的机器半边：sink 崩溃 fail-open，压缩照常出结果。
	sink := &recordingSink{panic_: true}
	fm := einollm.NewFakeModel([]string{"the compacted summary"}, nil)
	ctx := ctxcompact.WithLifecycleSink(context.Background(), sink.record)
	res, err := ctxcompact.Run(ctx, lifecycleFixture(), ctxcompact.PlanOpts{KeepRecent: 1},
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9}, fm, nil)
	require.NoError(t, err)
	assert.Less(t, res.TokensAfter, res.TokensBefore)
}

func TestNilSinkIsPassthrough(t *testing.T) {
	// WithLifecycleSink(nil) 原样返回 ctx —— 未接线的调用方零行为差异。
	ctx := ctxcompact.WithLifecycleSink(context.Background(), nil)
	fm := einollm.NewFakeModel([]string{"the compacted summary"}, nil)
	res, err := ctxcompact.Run(ctx, lifecycleFixture(), ctxcompact.PlanOpts{KeepRecent: 1},
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9}, fm, nil)
	require.NoError(t, err)
	assert.Less(t, res.TokensAfter, res.TokensBefore)
}
