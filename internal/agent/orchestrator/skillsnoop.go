package orchestrator

// W-F-10（INF4 / B54）：隐式 skill 调用识别 —— PostToolUse 阶段的内建观察者。
//
// 模型从不「调用」skill 工具；它读一份 SKILL.md、跑一个 skill 的 scripts/
// 下的脚本，skill 就被用了。操作员想知道「我写的技能哪些真的在被用」，只能
// 靠对工具调用的事后识别 —— 这就是本观察者。它挂在 hook 总线的 PostToolUse
// 阶段（工具执行**之后**，PreToolUse 改写后的最终入参才是真实执行的入参），
// 与 F3 的外部 hook 程序共用同一条 middleware、同一个注入纪律（per-turn
// ctx，middleware 零状态）。
//
// ── 硬约束：识别结果不进模型上下文 ──
//
// 观察者的输出只有一个去处（observer 回调，生产默认是结构化日志），在
// 中间件里被**结构性**丢弃 —— 它拿不到工具结果的写权限。为什么必须是结构
// 而不是纪律：把「你刚才用了 my-skill」回喂模型，模型下一次会**显式地**
// 读 SKILL.md 来讨好这条反馈（或者反过来绕开它），识别从此只看见自己的
// 回声 —— 自指循环。TestPostToolUseRecognitionNeverEntersModelContext 用
// 「结果逐字节相等 + 观察者确实被触发」两个断言把这个约束钉成非空转。
//
// ── 识别判据：注册表优先，不做无锚点的路径猜测 ──
//
// 候选只认两类形状（spec 点名的两类）：路径含 SKILL.md（skill 名取其所在
// 目录名），或含 /scripts/ 分隔（skill 名取 scripts 前一段）。两类都必须
// 在 skills 注册表里命中（名字或目录基名对上）才算数 —— 一个随机 URL 里的
// /scripts/ 或模型手写的一份未注册 SKILL.md 不是「在用已注册的技能」。
// 已知的识别盲区（刻意接受，注释在 observe 上）：命令里的相对路径若绕过
// skill 目录名（cd 后的裸 scripts/foo），注册表无从锚定，不识别。

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/x6nux/yanshi/internal/skills"
)

// ImplicitSkillUse 是一次被识别的隐式 skill 使用。
type ImplicitSkillUse struct {
	// Skill 是注册表里的 skill 名。
	Skill string
	// Source 是识别形状："read_skill_md"（读了 SKILL.md）或
	// "run_skill_script"（跑了 skill 的 scripts/ 下的东西）。
	Source string
	// Detail 是触发识别的路径或命令摘录（截断到 200 字节）。
	Detail string
}

// SkillUseObserver 消费一次识别结果。它**影响不了**任何工具调用 —— 不是
// 姿态而是结构：observer 在中间件的 PostToolUse 阶段被调用，返回值被丢弃，
// 工具结果已经定形。
type SkillUseObserver func(ctx context.Context, use ImplicitSkillUse)

// defaultSkillUseLogger 是生产默认的 observer：结构化日志，经 /logs 可见。
// 选日志而不是 work event / TUI 帧，是因为识别是统计信号不是需要操作员
// 即时反应的事件；日志聚合才是它的消费形态。
func defaultSkillUseLogger(ctx context.Context, use ImplicitSkillUse) {
	slog.InfoContext(ctx, "implicit skill use detected",
		"skill", use.Skill, "source", use.Source, "detail", use.Detail)
}

// skillRecognizer 携带一次 turn 的识别配置，经 ctx 绑定（与 hook 总线同一
// 纪律：middleware 实例共享，一切状态走 ctx）。
type skillRecognizer struct {
	registry *skills.Registry
	notify   SkillUseObserver
}

// skillRecognizerKey 是识别器的 per-turn context key。
type skillRecognizerKey struct{}

// withSkillRecognizer 把识别器绑进 turn ctx。registry 为 nil 时原样返回
// —— 未装配 skills 的部署没有可锚定的注册表，也没有识别可言。
func withSkillRecognizer(ctx context.Context, registry *skills.Registry, notify SkillUseObserver) context.Context {
	if registry == nil {
		return ctx
	}
	return context.WithValue(ctx, skillRecognizerKey{}, &skillRecognizer{
		registry: registry,
		notify:   notify,
	})
}

// skillRecognizerFromContext 读回识别器；未绑定时 ok=false。
func skillRecognizerFromContext(ctx context.Context) (*skillRecognizer, bool) {
	rec, ok := ctx.Value(skillRecognizerKey{}).(*skillRecognizer)
	return rec, ok && rec != nil
}

// observe 是中间件 PostToolUse 阶段的入口。它只读入参、只调 observer，
// 返回值只有一个 bool 供测试断言「确实触发了识别」—— 中间件不消费它。
func (r *skillRecognizer) observe(toolName, argsJSON string) bool {
	path := candidatePathFor(toolName, argsJSON)
	if path == "" {
		return false
	}
	use, ok := r.match(path)
	if !ok {
		return false
	}
	if r.notify != nil {
		r.notify(context.Background(), use)
	}
	return true
}

// match 把一个候选路径对到注册表里的 skill。两条判据见文件头。
func (r *skillRecognizer) match(path string) (ImplicitSkillUse, bool) {
	norm := filepath.ToSlash(path)
	var (
		source   string
		skillDir string
	)
	switch {
	case strings.Contains(norm, "SKILL.md"):
		source = "read_skill_md"
		skillDir = filepath.Dir(filepath.Clean(norm))
	case strings.Contains(norm, "/scripts/"):
		source = "run_skill_script"
		skillDir = filepath.Clean(norm[:strings.Index(norm, "/scripts/")])
	default:
		return ImplicitSkillUse{}, false
	}
	name := filepath.Base(skillDir)
	for _, sk := range r.registry.List() {
		if sk.Name == name || filepath.Base(filepath.Clean(sk.Dir)) == name {
			return ImplicitSkillUse{Skill: sk.Name, Source: source, Detail: clipText(norm, 200)}, true
		}
	}
	return ImplicitSkillUse{}, false
}

// candidatePathFor 从一次工具调用里取出候选路径。只认三类载体：
// fs_read 的 path 字段；shell_run / shell_start / task_shell_start 的
// command 字段按空白与引号切分后的每个词。切分是词法近似不是 shell 解析
// —— 它可能把一个词切错，代价是漏识别（fail-direction 文档化在文件头），
// 不会把没发生的调用识别成发生了（候选还要过注册表）。
func candidatePathFor(toolName, argsJSON string) string {
	var args struct {
		Path    string `json:"path"`
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ""
	}
	if args.Path != "" {
		return args.Path
	}
	if args.Command == "" {
		return ""
	}
	switch toolName {
	case "shell_run", "shell_start", "task_shell_start":
		for _, field := range strings.FieldsFunc(args.Command, func(r rune) bool {
			return strings.ContainsRune(" \t\n;'\"|&()", r)
		}) {
			if strings.Contains(field, "SKILL.md") || strings.Contains(field, "/scripts/") {
				return field
			}
		}
	}
	return ""
}
