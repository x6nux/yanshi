package http

// W-F-08 在传输层的绑定测试。外部 hook 进程与 fail-open 语义在 orchestrator
// 的 hooks_lifecycle_test.go 里用真实子进程钉过；这里钉的是 WS 侧两条路径
// 独有的两件事：
//
//  1. Server 构造时从 Config.Hooks 建出 sink，maybeAutoCompact / compactNow
//     把它绑进交给 ctxcompact 的 ctx（pre-turn 与 manual 两条路径各自到达）；
//  2. trigger 字段如实命名路径（pre_turn / manual），hook 据此可分。
//
// 会变红的变异：删掉 MaybeCompactWithOptions 调用点的 WithLifecycleSink 包装
// → pre_turn 断言红；把 compactNow 的 trigger 改成 pre_turn → manual 断言红。

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/ctxcompact"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// recordingCompactionSink 是 Go 侧的探针 sink。外部程序路径已在 orchestrator
// 测试里验证；这里关注的是传输层把 sink 绑没绑、绑的是哪个 trigger。
type recordingCompactionSink struct {
	events []ctxcompact.LifecycleEvent
}

func (r *recordingCompactionSink) note(_ context.Context, ev ctxcompact.LifecycleEvent) {
	r.events = append(r.events, ev)
}

func TestPreTurnCompactionFiresLifecycleSink(t *testing.T) {
	sink := &recordingCompactionSink{}
	fm := einollm.NewFakeModel([]string{"SUMMARY"}, nil)
	srv := &Server{
		compaction:      CompactionConfig{Model: "fm", Threshold: 0.05, ContextWindow: 4000, KeepRecent: 1},
		compactionHooks: sink.note,
	}
	cs := &connSession{perm: &permModeState{}}
	cs.history = evictableHistory(8)

	wc, client, cleanup := newWSPair(t)
	defer cleanup()
	_ = client
	maybeAutoCompact(context.Background(), srv,
		map[string]model.BaseChatModel{"fm": fm}, wc, cs)

	require.NotEmpty(t, cs.history)
	require.Len(t, sink.events, 2, "pre + post 各一条")
	assert.Equal(t, ctxcompact.LifecyclePreCompact, sink.events[0].Phase)
	assert.Equal(t, ctxcompact.TriggerPreTurn, sink.events[0].Trigger)
	assert.Equal(t, ctxcompact.LifecyclePostCompact, sink.events[1].Phase)
	assert.Equal(t, ctxcompact.TriggerPreTurn, sink.events[1].Trigger)
	assert.Positive(t, sink.events[1].TokensAfter)
}

func TestManualCompactFiresLifecycleSinkWithManualTrigger(t *testing.T) {
	sink := &recordingCompactionSink{}
	fm := einollm.NewFakeModel([]string{"SUMMARY"}, nil)
	srv := &Server{
		compaction:      CompactionConfig{Model: "fm", ContextWindow: 4000, KeepRecent: 1},
		compactionHooks: sink.note,
	}
	cs := &connSession{perm: &permModeState{}}
	cs.history = evictableHistory(8)

	wc, client, cleanup := newWSPair(t)
	defer cleanup()
	_ = client
	compactNow(context.Background(), srv,
		map[string]model.BaseChatModel{"fm": fm}, wc, cs)

	require.Len(t, sink.events, 2)
	for _, ev := range sink.events {
		assert.Equal(t, ctxcompact.TriggerManual, ev.Trigger,
			"手动 /compact 不得伪装成 pre_turn —— trigger 是 hook 区分路径的唯一凭据")
	}
}

func TestServerBuildsSinkFromConfigHooks(t *testing.T) {
	// 无压缩 hook 的 Config 建出 nil sink（直通）；有则建出非 nil。
	assert.Nil(t, New(Config{}).compactionHooks)
	srv := New(Config{Hooks: orchestrator.HooksConfig{
		PreCompact: []orchestrator.HookConfig{{Program: "yanshi-echo"}},
	}})
	assert.NotNil(t, srv.compactionHooks)
}
