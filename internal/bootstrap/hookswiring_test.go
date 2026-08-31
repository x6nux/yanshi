package bootstrap

// RF-17：hooks 配置块从 YAML 到编排器的 Go 层字段对账。
//
// YAML 键名那一半由 internal/config 的 TestLoad_HooksFromYAML 钉住（typo 会
// 被零值吞掉，逐字段断言防它）；本文件钉的是**另一半**——config struct 到
// orchestrator.Config 的映射在 Go 层逐字段拷贝，typo 同样编译通过、静默传零
// 值，与 YAML 层是同一种「无声消失」，只是搬了一个 hop。三个测试分别钉：
// 映射函数本身（含 Args 切片的独立拷贝）、空块到零值、以及 Build 之后真实
// 装配出的 App 真的拿到了这份配置。

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/config"
)

// TestToOrchestratorHooksFieldMapping 逐字段对账。每个值两两不同，任何两个
// 字段的 copy-paste 互换都过不了；两个 entry 挂不同的 program，漏掉一层循环
// 之类的写法会被条数与顺序抓出来。
func TestToOrchestratorHooksFieldMapping(t *testing.T) {
	in := config.HooksConfig{PreToolUse: []config.HookConfig{
		{
			Program: "/opt/hooks/check-policy",
			Args:    []string{"--strict", "--project", "alpha"},
			Timeout: 1500 * time.Millisecond,
		},
		{
			Program: "/opt/hooks/audit",
			Args:    []string{"--json"},
			Timeout: 2 * time.Second,
		},
	}}

	got := toOrchestratorHooks(in)
	require.Len(t, got.PreToolUse, 2)
	require.Equal(t, "/opt/hooks/check-policy", got.PreToolUse[0].Program)
	require.Equal(t, []string{"--strict", "--project", "alpha"}, got.PreToolUse[0].Args)
	require.Equal(t, 1500*time.Millisecond, got.PreToolUse[0].Timeout)
	require.Equal(t, "/opt/hooks/audit", got.PreToolUse[1].Program)
	require.Equal(t, []string{"--json"}, got.PreToolUse[1].Args)
	require.Equal(t, 2*time.Second, got.PreToolUse[1].Timeout)

	// Args 必须是独立拷贝：映射之后改输入不得穿透到输出（否则共享底层数组
	// 的两份配置会在运行期互相污染）。
	in.PreToolUse[0].Args[0] = "MUTATED"
	assert.Equal(t, "--strict", got.PreToolUse[0].Args[0],
		"映射必须拷贝 Args 切片，不能共享底层数组")
}

// TestToOrchestratorHooksEmptyIsZero 钉住空块的形状：没有 hook 时映射产物是
// 零值（不是带 nil entry 的非零结构），orchestrator 侧「零值不绑总线」的语义
// 因此保持单一生边缘。
func TestToOrchestratorHooksEmptyIsZero(t *testing.T) {
	assert.Equal(t, orchestrator.HooksConfig{}, toOrchestratorHooks(config.HooksConfig{}))
}

// TestBuild_WiresHooksBlockIntoOrchestrator 是接线半边：config.yaml 的 hooks
// 块经 Build 真的落进装配出的编排器。只测映射函数钉不住这一段——把
// orchestrator.Config 字面量里的 Hooks 行删掉，映射测试照旧全绿，只有这条
// 对着真 App 的断言红。
func TestBuild_WiresHooksBlockIntoOrchestrator(t *testing.T) {
	extra := "\nhooks:\n" +
		"  pre_tool_use:\n" +
		"    - program: /opt/hooks/check-policy\n" +
		"      args: [\"--strict\"]\n" +
		"      timeout: 1500ms\n"

	app, cleanup := buildFakeApp(t, extra)
	defer cleanup()

	got := app.Orch.HooksForTest()
	require.Len(t, got.PreToolUse, 1,
		"hooks 块必须真的到达装配出的编排器，而不是停在映射函数里")
	require.Equal(t, "/opt/hooks/check-policy", got.PreToolUse[0].Program)
	require.Equal(t, []string{"--strict"}, got.PreToolUse[0].Args)
	require.Equal(t, 1500*time.Millisecond, got.PreToolUse[0].Timeout)
}

// TestBuild_WithoutHooksBlockBindsNothing 是零值兼容的那一半：没有 hooks 块
// 的安装，编排器拿到的必须是零值——不发任何 hook 进程，行为与总线存在前
// 逐字节一致。
func TestBuild_WithoutHooksBlockBindsNothing(t *testing.T) {
	app, cleanup := buildFakeApp(t, "")
	defer cleanup()

	assert.Equal(t, orchestrator.HooksConfig{}, app.Orch.HooksForTest())
}
