package orchestrator

// W-F-02（INF4）：生命周期 Hook 总线 —— PreToolUse。
//
// 契约（spec §1.2 INF4）：复用 loopguard 的 ADK middleware 链与 per-turn
// context 注入，挂一条 hook 分发器；hook 是操作员在 config 里配置的**外部
// 程序**，按 codex 的返回值形状（block / reason / additional_context /
// updated_input）对每次工具调用表态。
//
// ── 与 guard 的相对顺序（Ruling，与 spec 建议的机制句面不同）──
//
// spec 的承重约束写的是「guard 先判，Allow 之后才跑 PreToolUse」。本实现把
// PreToolUse 放在 ADK 工具调用包装层（WrapInvokableToolCall），而 guard 的
// 判决住在每个工具自己的执行入口里（GuardedTool.Stream / secproc.Launch 的
// 第一步 Authorize）——所以实际的顺序是 hook 先于 guard。这不是漏接：
//
//   - 那条约束保护的不变量是「**没有 guard 对最终入参的 Allow，就没有执行**；
//     hook 只能追加拒绝，不能把拒绝翻成允许」。本实现满足它，而且满足得比
//     字面顺序更硬：hook 的输出里**根本不存在 allow 这个判决**。hook 能做的
//     只有三件事——拦截（追加拒绝）、改写入参、附加上下文；前两件都在 guard
//     之前发生，而 guard 永远对**最后到达工具的那个入参**做完整判决。spec
//     想防的攻击（「guard 批准 ls，hook 改写成 rm -rf /」）在这条管线里不是
//     「被挡住」，而是「不可表达」：不存在一份先算好、后被 hook 绕过的判决。
//   - 字面顺序需要 middleware 自己先跑一次 Authorize。那是一次裸 Action 的
//     判决：交互模式下会为同一次调用多弹一个窗（W-B-02 刚为 shell_run 消灭
//     过的形状），strict 模式会把每次调用变成两次确认。安全上它什么也不多给：
//     裸 Action 只能看 tools 维度的 glob，真正的判决在工具入口那次。
//
// 后果的完整记录见 docs/adr/0027（hook 总线与 guard 的顺序）。
//
// ── fail-open / fail-closed（Ruling）──
//
// hook 超时、崩溃、退出非零、输出读不懂：一律 **fail-closed** —— 该次工具
// 调用不执行，拒绝原因作为工具结果回喂模型，turn 继续不中断（这是验收 5 的
// 「不中断」与本裁决的组合：不中断的是 turn，不是违规调用）。理由：
//
//   - 入参是模型写的，模型有能力让一个脆弱的 hook 崩溃（超长入参、特定形状）。
//     fail-open 时「hook 拦 X」退化为「X 允许」，而且静默 —— 这是本仓反复出现
//     的「失败伪装成成功」。fail-closed 时同一个攻击得到的是一串显式拒绝。
//   - guard 的 profile 地板不依赖 hook 存在，所以 fail-closed 不会把结构性地板
//     变成 hook 的俘虏；它多保住的只是 hook 自己那一层附加策略。
//
// 代价（明说）：一个坏掉的 hook 会拒绝所有工具调用，agent 退化成「只能说话」。
// 这是故意的——它在第一次工具调用就可见、可诊断（拒绝文本里带 hook 名与失败
// 原因），修不好就删配置；对比静默旁路，这个代价便宜且诚实。所以 hook 协议
// 要求**每个** hook 都输出 verdict JSON，包括只观察不拦截的 hook（输出
// {"block":false} 即可）——空 stdout 不是「弃权」，是「读不懂」，按失败拒绝。
//
// ── hook 协议 ──
//
// stdin：一个 JSON 值（hookRequest）。**读一个值即答，不要等 EOF** —— 生产
// factory 的 Stdin.Close 是整条控制台的 teardown 而非半关，等 EOF 的 hook 会
// 挂到超时被杀。
// stdout：一个 JSON 值（hookResponse），随后退出 0。非零退出、无输出、非法
// JSON、超限输出，都按 hook 失败处理（fail-closed）。stderr 仅作诊断尾随，
// 不参与判决。
// hook 的输出是**不可信外部输入**：只有结构化解析过的字段被消费，文本字段
// 截断后以「hook 名」标注进入模型可见的结果——它是数据，不是指令，与 MCP
// 反向请求同一待遇。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"

	"github.com/x6nux/yanshi/internal/secproc"
	"github.com/x6nux/yanshi/internal/tools"
)

// HookConfig 描述一个操作员配置的 hook 程序（config.yaml 的
// hooks.pre_tool_use[] 的一项）。
//
// Program 按 secproc 的约定是可执行文件路径（不经 shell 解释，没有引号展开；
// 需要shell 语义的操作员可以显式配置 /bin/sh -c 的形状，那会照常过 guard 的
// shell 维度）。Timeout ≤ 0 取 defaultHookTimeout。
type HookConfig struct {
	// Program 是 hook 可执行文件的路径。空路径在 config 加载期即被拒绝；
	// 路径不存在则在该 hook 第一次被调用时以 fail-closed 拒绝该次工具调用。
	Program string
	// Args 是传给 hook 的固定参数（不经 shell 解释）。
	Args []string
	// Timeout 是单次 hook 调用的墙钟上限。≤ 0 取 defaultHookTimeout。
	Timeout time.Duration
}

// HooksConfig 是 hook 总线的配置。事件集按 INF4 的 codex 子集逐步接入，本批
// 只有 PreToolUse；后续批次（Stop / PostToolUse / 压缩生命周期）挂进同一个
// 结构，而不是各自再造一条分发链。
type HooksConfig struct {
	// PreToolUse 在每次工具调用执行前按声明顺序逐个运行。语义是流水线：
	// 后一级 hook 看到的是前一级 hook 改写后的入参；guard 对**全部 hook
	// 结束后**的最终入参做判决。
	PreToolUse []HookConfig
}

// defaultHookTimeout 是未配置 Timeout 时的单次 hook 上限。选 10s 是因为 hook
// 的正常工作量是「读一段 JSON、给一个 verdict」，秒级都嫌多；它不是 loopguard
// 的 turn 级预算，别把两者的量纲混掉。
const defaultHookTimeout = 10 * time.Second

// 不可信输出的边界。OutputLimit 约束 hook 的 verdict 大小（超出即失败，不是
// 截断后照读——截断的 JSON 是另一次解析事故）；TextLimit 约束进入模型可见
// 结果的单段文本；ContextMax 约束 additional_context 的条数；StderrLimit 是
// 诊断尾随的保留量。hook 程序是外部进程，这四条是它影响本进程内存与提示词
// 预算的全部通道。
const (
	hookOutputLimit = 1 << 20
	hookTextLimit   = 2048
	hookContextMax  = 8
	hookStderrLimit = 4096
)

// hookEventPreToolUse 是请求里的事件名。写成常量而不内联，是为了让后续批次
// 接新事件时有一个可对账的名字集合。
const hookEventPreToolUse = "pre_tool_use"

// hookRequest 是写给 hook stdin 的请求。Args 原样携带模型的入参 JSON——
// hook 判断的对象就是它，把它重新编码只会制造第二份表示。
type hookRequest struct {
	Event string          `json:"event"`
	Tool  string          `json:"tool"`
	Args  json.RawMessage `json:"args"`
}

// hookResponse 是 hook 写到 stdout 的 verdict，字段形状即 spec INF4 契约里的
// codex 子集。解析用宽松的 Unmarshal（未知字段忽略），消费只认这四个字段；
// 任何别的形状（例如某天有人给协议加 allow 布尔）在这里被静默丢弃——这是
// 故意的：判决权不经过 hook，见文件头的顺序 Ruling。
type hookResponse struct {
	Block             bool            `json:"block"`
	Reason            string          `json:"reason"`
	AdditionalContext []string        `json:"additional_context"`
	UpdatedInput      json.RawMessage `json:"updated_input"`
}

// hookBusKey 是 hook 总线的 per-turn context key。
type hookBusKey struct{}

// withTurnHooks 把本 turn 的 hook 配置绑进 ctx。没有配置任何 hook 时原样返回
// ctx——中间件查不到总线就直接放行，行为与引入前逐字节一致。
//
// 故意不导出：WithLoopGuard 导出是因为 goalloop 这类非 turn 入口需要它；hook
// 总线的每个生产入口都经过 withTurnContext（它同时是 GOV6 的调用点），子代理
// 走自己 Orchestrator 的 withTurnContext，天然带各自的总线。多一个导出形式
// 只多一条「绑了却没人消费」的路。
func withTurnHooks(ctx context.Context, cfg HooksConfig) context.Context {
	if len(cfg.PreToolUse) == 0 {
		return ctx
	}
	return context.WithValue(ctx, hookBusKey{}, cfg)
}

// turnHooksFromContext 读回 withTurnHooks 绑定的配置。未绑定时 ok=false。
func turnHooksFromContext(ctx context.Context) (HooksConfig, bool) {
	cfg, ok := ctx.Value(hookBusKey{}).(HooksConfig)
	return cfg, ok && len(cfg.PreToolUse) > 0
}

// hookMiddleware 是 hook 总线的 ADK 半边。
//
// 与 loopGuardMiddleware 同一条纪律：实例是共享的（runnerFor 会 memoise
// runner，一个中间件实例服务同一 model 的所有会话），所以它自己一个字节都
// 不存 —— 一切都从 turn 的 ctx 里找，找不到就原样放行。
type hookMiddleware struct {
	*adk.BaseChatModelAgentMiddleware
}

// newHookMiddleware 构建可装进 adk.ChatModelAgentConfig.Handlers 的 hook
// 中间件。
func newHookMiddleware() *hookMiddleware {
	return &hookMiddleware{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{}}
}

// WrapInvokableToolCall 在每次工具调用前跑 PreToolUse hook 链。
//
// 工厂与 workRoot 在**包装时**从 turn ctx 取（此刻 ctx 保证带着
// bindExecutionContext 绑的值），callCtx 只用于取消与超时 —— 与预算中间件
// 同一个姿势。拒绝与预算拒绝同约定：作为工具**结果**返回、Go error 为 nil，
// 因为工具节点里的 Go error 是 NodeRunError，会拆掉整个 turn（验收 5 的
// 「不中断 turn」在结构上就落在这里）。
func (m *hookMiddleware) WrapInvokableToolCall(
	ctx context.Context,
	endpoint adk.InvokableToolCallEndpoint,
	tCtx *adk.ToolContext,
) (adk.InvokableToolCallEndpoint, error) {
	cfg, ok := turnHooksFromContext(ctx)
	if !ok || tCtx == nil || tCtx.Name == "" {
		return endpoint, nil
	}
	factory, _ := secproc.FromContext(ctx)
	workRoot := tools.WorkRootFromContext(ctx)
	name := tCtx.Name
	return func(callCtx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
		final, extra, refusal := runPreToolUseHooks(callCtx, cfg, factory, workRoot, name, argsJSON)
		if refusal != "" {
			return refusal, nil
		}
		out, err := endpoint(callCtx, final, opts...)
		if err != nil {
			return out, err
		}
		if len(extra) > 0 {
			out = strings.TrimRight(out, "\n") + "\n\n" + strings.Join(extra, "\n")
		}
		return out, nil
	}, nil
}

// runPreToolUseHooks 按声明顺序逐个跑 hook，返回三元组：
//
//	finalArgs —— 全部 hook 结束后的入参（流水线语义：后一级看到前一级改写后
//	            的入参）。有拒绝时返回原始入参，调用方反正不会用它执行。
//	contextLines —— 各 hook 的 additional_context，已带 hook 名标注与截断。
//	refusal     —— 非空表示本次调用必须被拒绝（hook 拦截或 hook 失败），
//	               文本即回喂模型的工具结果。
func runPreToolUseHooks(
	ctx context.Context,
	cfg HooksConfig,
	factory secproc.Factory,
	workRoot string,
	toolName string,
	argsJSON string,
) (finalArgs string, contextLines []string, refusal string) {
	if strings.TrimSpace(argsJSON) == "" {
		// 无参调用规范化成空对象：hook 看到的入参永远是合法 JSON，
		// 「没有参数」和「参数是 null」是两个协议里都不存在的形状。
		argsJSON = "{}"
	}
	finalArgs = argsJSON
	for _, h := range cfg.PreToolUse {
		res, err := runOneHook(ctx, h, factory, workRoot, toolName, finalArgs)
		if err != nil {
			return argsJSON, nil, fmt.Sprintf("✗ pre_tool_use hook %s failed: %s",
				filepath.Base(h.Program), clipText(err.Error(), hookTextLimit))
		}
		if res.Block {
			reason := strings.TrimSpace(res.Reason)
			if reason == "" {
				reason = "no reason given"
			}
			return argsJSON, nil, fmt.Sprintf("✗ blocked by pre_tool_use hook %s: %s",
				filepath.Base(h.Program), clipText(reason, hookTextLimit))
		}
		if v := bytes.TrimSpace(res.UpdatedInput); len(v) > 0 && !bytes.Equal(v, []byte("null")) {
			finalArgs = string(v)
		}
		for _, line := range res.AdditionalContext {
			if len(contextLines) >= hookContextMax {
				break
			}
			line = strings.TrimSpace(clipText(line, hookTextLimit))
			if line != "" {
				contextLines = append(contextLines, "[hook "+filepath.Base(h.Program)+"] "+line)
			}
		}
	}
	return finalArgs, contextLines, ""
}

// runOneHook 起一次 hook 进程并与它做一次协议往返。
//
// 发射走 secproc.Launch（INF4 承重约束 3：hook 是不受信程序）。spec.Tool 填的
// 是**被 hook 的那个工具的名字**：secproc.Launch 的第一步 Authorize 用裸
// Action 判决（无 Shell 无 FS，实际只有 tools 维度的 glob 与 plan/role/toolreg
// 检查），于是 hook 子进程的授权与「这次工具调用本身是否被 profile 允许」是
// 同一个问题 —— hook 不会引入第二个需要配置的名字（新名字过不了 toolreg 的
// 未注册检查，注册进 agentTools 又会让模型可以直接调用它，两个方向都不对）。
// 真正的判决在工具入口那次，hook 子进程的这一次只是同一把尺子的量法。
// AllowEnv 留空：hook 默认拿不到任何凭据（验收 4 的行为测试钉住它）。
func runOneHook(
	ctx context.Context,
	h HookConfig,
	factory secproc.Factory,
	workRoot string,
	toolName string,
	argsJSON string,
) (*hookResponse, error) {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = defaultHookTimeout
	}
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := json.Marshal(hookRequest{
		Event: hookEventPreToolUse,
		Tool:  toolName,
		Args:  json.RawMessage(argsJSON),
	})
	if err != nil {
		return nil, fmt.Errorf("hook request encode: %w", err)
	}
	started, err := secproc.Launch(hctx, secproc.SecureProcessSpec{
		Tool:    toolName,
		Program: h.Program,
		Args:    h.Args,
		Dir:     workRoot,
		Workdir: workRoot,
		// 弹窗里给操作员看的是 hook 的请求原文，而不是一个空壳参数——
		// hook 拿到了什么，审批界面就该显示什么。
		ArgsJSON: string(req),
	})
	if err != nil {
		return nil, err
	}
	if started.Stdin == nil || started.Stdout == nil {
		_, _ = io.Copy(io.Discard, started.Stdout)
		reapInBackground(started)
		return nil, fmt.Errorf("factory returned a process without stdin/stdout (fail-closed)")
	}

	// 写请求。故意不 Close stdin：生产 factory 的 Stdin.Close 会拆掉整条
	// 控制台（见 internal/shell/factory.go 的注释），协议因此定为「读一个
	// JSON 值即答」。写阻塞时不用管 —— hctx 到期会杀掉整棵进程树，管道
	// 断开自然解除阻塞。
	if _, err := started.Stdin.Write(req); err != nil {
		reapInBackground(started)
		return nil, fmt.Errorf("hook stdin: %w", err)
	}

	// stdout 有界读取；stderr 进有界尾随（一直消费到 EOF，chatty 的 hook
	// 撑不爆 64K 的管道缓冲，也就死不了锁）。
	tail := newBoundedTail(hookStderrLimit)
	if started.Stderr != nil {
		go func() { _, _ = io.Copy(tail, started.Stderr) }()
	}
	type readResult struct {
		out []byte
		err error
	}
	readCh := make(chan readResult, 1)
	go func() {
		b, rerr := io.ReadAll(io.LimitReader(started.Stdout, hookOutputLimit+1))
		readCh <- readResult{out: b, err: rerr}
	}()

	var out []byte
	select {
	case r := <-readCh:
		out = r.out
		if r.err != nil {
			reapInBackground(started)
			return nil, fmt.Errorf("read hook stdout: %w", r.err)
		}
	case <-hctx.Done():
		reapInBackground(started)
		return nil, fmt.Errorf("timed out after %s", timeout)
	}
	if len(out) > hookOutputLimit {
		reapInBackground(started)
		return nil, fmt.Errorf("hook verdict exceeds %d bytes", hookOutputLimit)
	}

	// 排到 EOF 之后才收割（secproc 的管线顺序契约）；hook 崩溃在这里显形。
	waitCh := make(chan error, 1)
	go func() { waitCh <- started.Wait() }()
	select {
	case waitErr := <-waitCh:
		if waitErr != nil {
			return nil, fmt.Errorf("hook exited: %v%s", waitErr, tail.suffix())
		}
	case <-hctx.Done():
		reapInBackground(started)
		return nil, fmt.Errorf("timed out after %s (while reaping)", timeout)
	}

	var resp hookResponse
	if err := json.Unmarshal(bytes.TrimSpace(out), &resp); err != nil {
		return nil, fmt.Errorf("hook verdict is not valid JSON%s", tail.suffix())
	}
	if v := bytes.TrimSpace(resp.UpdatedInput); len(v) > 0 && !json.Valid(v) {
		return nil, fmt.Errorf("hook updated_input is not valid JSON")
	}
	return &resp, nil
}

// reapInBackground 在提前放弃的路径上把收割放到后台：进程已被 hctx 杀掉，
// Wait 只是收尸，不能因为没人在等就留一个僵尸。
func reapInBackground(p *secproc.StartedProcess) {
	if p == nil || p.Wait == nil {
		return
	}
	go func() { _ = p.Wait() }()
}

// boundedTail 是 io.Writer 形状的有界尾随缓冲：一直消费到 EOF（这是它比
// io.ReadAll(io.LimitReader(...)) 对的地方 —— 后者停止读取后，写爆管道的
// 子进程会卡死直到超时），内存上限由 limit 保证，保留的是**最后** limit
// 字节（崩溃现场在尾部，不在头部）。
type boundedTail struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func newBoundedTail(limit int) *boundedTail { return &boundedTail{limit: limit} }

func (t *boundedTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
	return len(p), nil
}

// suffix 把尾随渲染成错误信息的一个从句；空尾随返回空串。
func (t *boundedTail) suffix() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := strings.TrimSpace(string(t.buf))
	if s == "" {
		return ""
	}
	return "; stderr: " + clipText(s, hookTextLimit)
}

// clipText 把不可信文本截到 limit 字节并修复切断的 UTF-8 序列。hook 的
// reason / additional_context / stderr 在进入模型可见结果之前都要过这里 ——
// 一条 2MB 的「理由」就是对提示词预算的拒绝服务。
func clipText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return strings.ToValidUTF8(s[:limit], "") + "…(truncated)"
}
