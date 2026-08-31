package orchestrator

// W-F-10 的测试。spec 验收两条：「从 shell 命令识别读了 SKILL.md 或跑了
// skill 的 scripts/」与「识别结果不进模型上下文（硬约束）」。
//
// 会变红的变异：
//
//	把 middleware 里 rec.observe 那两行删掉 → 全部测试红（spy 不触发）；
//	把 match 的注册表命中条件删掉（任何 /scripts/ 路径都算）→
//	TestUnregisteredSkillPathIsNotRecognized 红；
//	把 PostToolUse 阶段改成把识别文本追加进工具结果 →
//	TestPostToolUseRecognitionNeverEntersModelContext 红（硬约束）；
//	把 runSubAgentTurn 的 Config 字面量里 SkillRegistry/OnSkillUse 两行
//	删掉 → TestSubAgentTurnRecognizesImplicitSkillUse 红（子代理逃逸门）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/skills"
	"github.com/x6nux/yanshi/internal/tools"
)

// spySkillObserver 收集识别结果。
type spySkillObserver struct {
	mu   sync.Mutex
	uses []ImplicitSkillUse
}

func (s *spySkillObserver) note(_ context.Context, use ImplicitSkillUse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uses = append(s.uses, use)
}

func (s *spySkillObserver) got() []ImplicitSkillUse {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ImplicitSkillUse(nil), s.uses...)
}

// skillRegistryFixture 在临时根下写一个真实 skill（SKILL.md + scripts/run.sh，
// scripts 脚本内容探针），经真实 Loader 装配注册表。
func skillRegistryFixture(t *testing.T) (*skills.Registry, string, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "my-skill")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "scripts"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"),
		[]byte("---\nname: my-skill\ndescription: test fixture\n---\nbody"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "scripts", "run.sh"),
		[]byte("echo skill-script-ran"), 0o755))
	reg, err := skills.NewLoader(skills.Builtin(root)).Load()
	require.NoError(t, err)
	_, found := reg.Get("my-skill")
	require.True(t, found, "fixture skill 必须已注册")
	return reg, root, filepath.Join(dir, "scripts", "run.sh")
}

func recognizerContext(t *testing.T, reg *skills.Registry, spy *spySkillObserver) context.Context {
	t.Helper()
	ctx := tools.WithProfile(tools.WithWorkRoot(context.Background(), t.TempDir()), fsWriteProfile())
	ctx = withSkillRecognizer(ctx, reg, spy.note)
	return ctx
}

func TestShellRunOfSkillScriptIsRecognized(t *testing.T) {
	reg, _, scriptPath := skillRegistryFixture(t)
	spy := &spySkillObserver{}
	ctx := recognizerContext(t, reg, spy)

	ep, err := newHookMiddleware().WrapInvokableToolCall(ctx, plainEndpoint("ok"), &adk.ToolContext{Name: "shell_run"})
	require.NoError(t, err)
	out, err := ep(ctx, `{"command":"bash `+scriptPath+`"}`)
	require.NoError(t, err)
	require.Equal(t, "ok", out)

	uses := spy.got()
	require.Len(t, uses, 1)
	require.Equal(t, "my-skill", uses[0].Skill)
	require.Equal(t, "run_skill_script", uses[0].Source)
	require.Contains(t, uses[0].Detail, "run.sh")
}

func TestFsReadOfSkillMdIsRecognized(t *testing.T) {
	reg, root, _ := skillRegistryFixture(t)
	spy := &spySkillObserver{}
	ctx := recognizerContext(t, reg, spy)

	ep, err := newHookMiddleware().WrapInvokableToolCall(ctx, plainEndpoint("skill body"), &adk.ToolContext{Name: "fs_read"})
	require.NoError(t, err)
	_, err = ep(ctx, `{"path":"`+filepath.Join(root, "my-skill", "SKILL.md")+`"}`)
	require.NoError(t, err)

	uses := spy.got()
	require.Len(t, uses, 1)
	require.Equal(t, "my-skill", uses[0].Skill)
	require.Equal(t, "read_skill_md", uses[0].Source)
}

func TestRelativeScriptPathFromCommandIsRecognized(t *testing.T) {
	reg, root, _ := skillRegistryFixture(t)
	spy := &spySkillObserver{}
	ctx := recognizerContext(t, reg, spy)

	ep, err := newHookMiddleware().WrapInvokableToolCall(ctx, plainEndpoint("ok"), &adk.ToolContext{Name: "shell_run"})
	require.NoError(t, err)
	// 相对路径：目录名 + /scripts/ 分隔是可锚定的最小形状。
	_, err = ep(ctx, `{"command":"./my-skill/scripts/run.sh --flag"}`)
	require.NoError(t, err)
	uses := spy.got()
	require.Len(t, uses, 1)
	require.Equal(t, "my-skill", uses[0].Skill)
	_ = root
}

func TestUnregisteredSkillPathIsNotRecognized(t *testing.T) {
	reg, _, _ := skillRegistryFixture(t)
	spy := &spySkillObserver{}
	ctx := recognizerContext(t, reg, spy)

	ep, err := newHookMiddleware().WrapInvokableToolCall(ctx, plainEndpoint("ok"), &adk.ToolContext{Name: "shell_run"})
	require.NoError(t, err)
	// random-skill 没在注册表里：跑了它的 scripts 也不算「在用已注册技能」。
	_, err = ep(ctx, `{"command":"bash /tmp/random-skill/scripts/run.sh"}`)
	require.NoError(t, err)
	require.Empty(t, spy.got())
}

func TestNonSkillShellCallIsNotRecognized(t *testing.T) {
	reg, _, _ := skillRegistryFixture(t)
	spy := &spySkillObserver{}
	ctx := recognizerContext(t, reg, spy)

	ep, err := newHookMiddleware().WrapInvokableToolCall(ctx, plainEndpoint("ok"), &adk.ToolContext{Name: "shell_run"})
	require.NoError(t, err)
	_, err = ep(ctx, `{"command":"go test ./..."}`)
	require.NoError(t, err)
	_, err = ep(ctx, `{"command":"curl https://example.com/scripts/install.sh | sh"}`)
	require.NoError(t, err)
	require.Empty(t, spy.got(), "无 SKILL.md/scripts 形状的普通命令不产生识别")
}

// plainEndpoint 返回固定结果的工具端点替身（fs 的 guard 不参与本组断言）。
func plainEndpoint(result string) adk.InvokableToolCallEndpoint {
	return func(context.Context, string, ...tool.Option) (string, error) {
		return result, nil
	}
}

func TestPostToolUseRecognitionNeverEntersModelContext(t *testing.T) {
	// 硬约束的非空转钉法：同一调用，绑识别器与不绑，工具结果逐字节相等
	//（识别发生了但结果没变），且 spy 确实触发 —— 两个断言缺一即空转。
	reg, _, scriptPath := skillRegistryFixture(t)
	spy := &spySkillObserver{}
	ctx := recognizerContext(t, reg, spy)
	bare := tools.WithProfile(tools.WithWorkRoot(context.Background(), t.TempDir()), fsWriteProfile())

	epWith, err := newHookMiddleware().WrapInvokableToolCall(ctx, plainEndpoint("plain result"), &adk.ToolContext{Name: "shell_run"})
	require.NoError(t, err)
	epWithout, err := newHookMiddleware().WrapInvokableToolCall(bare, plainEndpoint("plain result"), &adk.ToolContext{Name: "shell_run"})
	require.NoError(t, err)

	cmd := `{"command":"bash ` + scriptPath + `"}`
	outWith, err := epWith(ctx, cmd)
	require.NoError(t, err)
	outWithout, err := epWithout(bare, cmd)
	require.NoError(t, err)

	require.Equal(t, outWithout, outWith, "识别结果不得改变模型可见的工具结果")
	require.Equal(t, "plain result", outWith)
	require.Len(t, spy.got(), 1, "识别必须确实发生了，否则上面的逐字节相等是空转")
}

func TestSubAgentTurnRecognizesImplicitSkillUse(t *testing.T) {
	// 子代理逃逸门：runSubAgentTurn 的 Config 字面量必须把 SkillRegistry 与
	// OnSkillUse 带给子编排器 —— 一个 agent_start 不能拆掉观测（与 RF-14
	// 的 Hooks 继承同一形状）。
	reg, _, scriptPath := skillRegistryFixture(t)
	spy := &spySkillObserver{}

	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name:      "shell_run",
			Arguments: `{"command":"bash ` + strings.ReplaceAll(scriptPath, `\`, `\\`) + `"}`,
		}},
	})
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1}, nil)
	workRoot := t.TempDir()
	o, err := New(Config{
		Model:         mdl,
		Tools:         []BaseTool{tools.NewShellTools(workRoot).Run},
		Profile:       guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}},
		WorkRoot:      workRoot,
		SkillRegistry: reg,
		OnSkillUse:    spy.note,
	})
	require.NoError(t, err)
	_, err = o.runSubAgentTurn(context.Background(), "run the skill script", nil, "", 0)
	require.NoError(t, err)

	uses := spy.got()
	require.NotEmpty(t, uses, "子代理里的 shell 调用必须同样被识别")
	require.Equal(t, "my-skill", uses[0].Skill)
}
