package orchestrator

// W-F-08 的发射器测试。总线本身（事件形状、三条路径共用）在 ctxcompact 测；
// 这里钉的是装配层的三个事实：
//
//  1. launcher 真的把事件发给了真的外部程序（不是 Go 回调自嗨）；
//  2. hook 失败 fail-open（崩溃的 hook 不影响同段后续 hook，更不破坏压缩）；
//  3. withTurnContext 把 sink 绑进 turn ctx —— mid-turn 路径
//     （CompactingModel 从 turn ctx 读总线）端到端能打到外部程序。
//
// 子代理的继承不需要新测试机位：runSubAgentTurn 的 Config 字面量拷贝
// Hooks（RF-14，TestSubAgentTurnHooksReachSubAgentTools 钉住），而压缩
// sink 由 New 从同一个 cfg.Hooks 派生 —— 子编排器拿到同一个发射器，它的
// withTurnContext 同样绑定。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/ctxcompact"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/tools"
)

// lifecycleTestEnv 把探针日志路径放进 env（helper 进程经清洗后的继承 env
// 拿到它 —— YANSHI_TEST_* 不在凭据名单上）。
func lifecycleTestEnv(t *testing.T) string {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	t.Setenv("YANSHI_TEST_LIFECYCLE_LOG", logPath)
	return logPath
}

func readLifecycleEvents(t *testing.T, logPath string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(logPath)
	require.NoError(t, err, "hook 探针必须已经落盘事件")
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m))
		out = append(out, m)
	}
	return out
}

func TestCompactionLifecycleHookReceivesEvent(t *testing.T) {
	logPath := lifecycleTestEnv(t)
	sink := NewCompactionHookSink(HooksConfig{
		PreCompact:  []HookConfig{hookTestProgram(t, "record_lifecycle")},
		PostCompact: []HookConfig{hookTestProgram(t, "record_lifecycle")},
	})
	require.NotNil(t, sink)

	ctx := tools.WithWorkRoot(t.Context(), t.TempDir())
	sink(ctx, ctxcompact.LifecycleEvent{
		Phase: ctxcompact.LifecyclePreCompact, Trigger: ctxcompact.TriggerMidTurn, TokensBefore: 4321,
	})
	sink(ctx, ctxcompact.LifecycleEvent{
		Phase: ctxcompact.LifecyclePostCompact, Trigger: ctxcompact.TriggerManual,
		TokensBefore: 4321, TokensAfter: 777,
	})

	events := readLifecycleEvents(t, logPath)
	require.Len(t, events, 2, "pre 与 post 各打到登记在对应段的 hook")
	field := func(m map[string]any, key string) string {
		got, _ := m[key].(string)
		return got
	}
	require.Equal(t, "pre_compact", field(events[0], "event"))
	require.Equal(t, "mid_turn", field(events[0], "trigger"))
	require.Equal(t, "post_compact", field(events[1], "event"))
	require.Equal(t, "manual", field(events[1], "trigger"))
	require.Equal(t, float64(4321), events[0]["tokens_before"])
	require.Equal(t, float64(777), events[1]["tokens_after"])
}

func TestCompactionHookFailureIsFailOpen(t *testing.T) {
	// 崩溃的 hook 在前，记录的 hook 在后：sink 返回后第二个 hook 仍然跑过
	// —— 段内一个失败不吞掉后续，也不向上传播（Ruling：压缩对 hook 失败
	// fail-open）。会变红的变异：把 runLifecycleHook 的错误改成向上 return
	//（fail-closed 形状）→ 本测试因第二个 hook 的事件缺席而红。
	logPath := lifecycleTestEnv(t)
	sink := NewCompactionHookSink(HooksConfig{
		PreCompact: []HookConfig{
			hookTestProgram(t, "crash"),
			hookTestProgram(t, "record_lifecycle"),
		},
	})
	require.NotNil(t, sink)

	require.NotPanics(t, func() {
		sink(tools.WithWorkRoot(t.Context(), t.TempDir()), ctxcompact.LifecycleEvent{
			Phase: ctxcompact.LifecyclePreCompact, Trigger: ctxcompact.TriggerPreTurn, TokensBefore: 10,
		})
	})
	events := readLifecycleEvents(t, logPath)
	require.Len(t, events, 1, "崩溃的 hook 之后的 hook 必须仍然运行")
}

func TestNewCompactionHookSinkNilWhenUnconfigured(t *testing.T) {
	require.Nil(t, NewCompactionHookSink(HooksConfig{}))
	// 只配置 PreToolUse 不产生压缩 sink —— 未配置压缩 hook 的部署在传输层
	// 与 turn 层都保持「没有总线」的直通行为。
	require.Nil(t, NewCompactionHookSink(HooksConfig{
		PreToolUse: []HookConfig{hookTestProgram(t, "allow")},
	}))
}

func TestMidTurnCompactionPathFiresLifecycleHook(t *testing.T) {
	// 端到端：真实装配的 orchestrator → withTurnContext 绑总线 → turn ctx
	// → CompactingModel 触发压缩 → ctxcompact 发事件 → 真实外部 hook 进程
	// 落盘。mid-turn 是三条压缩路径里唯一从 turn ctx 读总线的；pre-turn 与
	// 手动 /compact 的传输层绑定在 api/http 的 compaction_lifecycle_test 里
	// 各有探针 sink 测试；config 到装配的两段（压缩段映射的方向、到
	// apihttp 配置的传递）由 bootstrap 的 hookswiring 压缩段测试钉住。
	logPath := lifecycleTestEnv(t)
	workRoot := t.TempDir()

	fm := einollm.NewFakeModel([]string{"turn answer"}, nil)
	o, err := New(Config{
		Model:    fm,
		Profile:  fsWriteProfile(),
		WorkRoot: workRoot,
		Hooks: HooksConfig{
			PreCompact: []HookConfig{hookTestProgram(t, "record_lifecycle")},
		},
	})
	require.NoError(t, err)

	turnCtx := o.WithTurnContextForTest(t.Context(), TurnOpts{})

	inner := einollm.NewFakeModel([]string{"inner answer"}, nil)
	cm := &einollm.CompactingModel{Inner: inner, Threshold: 0.01, ContextWindow: 100000, KeepRecent: 2}
	big := []*schema.Message{
		{Role: schema.User, Content: "task"},
		{Role: schema.Assistant, Content: strings.Repeat("noise ", 5000)},
		{Role: schema.Assistant, Content: strings.Repeat("more ", 5000)},
		{Role: schema.User, Content: "recent"},
	}
	_, gerr := cm.Generate(turnCtx, big)
	require.NoError(t, gerr)

	events := readLifecycleEvents(t, logPath)
	require.NotEmpty(t, events, "mid-turn 压缩必须打到 hook")
	first := events[0]
	gotEvent, _ := first["event"].(string)
	gotTrigger, _ := first["trigger"].(string)
	require.Equal(t, "pre_compact", gotEvent)
	require.Equal(t, "mid_turn", gotTrigger, "mid-turn 路径必须以自己的名字出现在事件上")
}
