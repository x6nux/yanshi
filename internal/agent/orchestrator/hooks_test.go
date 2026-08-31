package orchestrator

// W-F-02（INF4）PreToolUse hook 总线的验收测试。
//
// 本文件里 hook 本体是一个**真实的外部进程**：测试把 go test 二进制自身
// re-exec 出来（TestHookHelperProcess），让它走与生产完全相同的路径 ——
// secproc.Launch → Authorize → factory → stdin/stdout JSON 协议。
// 没有任何一处用进程内的函数替身去扮演 hook；被替身的只有「被 hook 保护
// 的那个工具」（hookToolDouble），而那个替身里的 guard 是真的（tools.Authorize），
// 这样「改写后的入参是否重新走 guard」才是一个可观测的事实而不是一个断言
// 自己证明自己的循环。
//
// 每个 hook 进程通过 re-exec 参数选择剧本（allow / rewrite / block / crash /
// …），verdict 一律写 stdout 后立即退出。协议细节见 hooks.go 的 hookResponse
// 文档。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/shell"
	"github.com/x6nux/yanshi/internal/tools"
)

// helper 进程的参数（剧本走 re-exec 参数，见 hookTestProgram）。
const (
	hookHelperRewriteEnv = "YANSHI_TEST_HOOK_REWRITE_TO"
	hookHelperReasonEnv  = "YANSHI_TEST_HOOK_REASON"
	hookHelperSleepEnv   = "YANSHI_TEST_HOOK_SLEEP"
)

// TestHookHelperProcess 不是一条测试：它是本文件所有 hook 测试共用的那个
// 「外部程序」。父测试通过 secproc 把 go test 二进制 re-exec 起来；剧本名走
// re-exec 参数（"-- " 之后的第一段），参数（改写目标、理由、睡眠时长）走
// 环境变量。剧本必须能按 hook 各不相同 —— 环境变量做不到这一点，两个 hook
// 子进程会继承同一份 env，这正是链式语义测试最初抓出来的 harness 缺陷。
//
// helper 从 stdin 读一个 JSON 请求，按剧本写一个 verdict 到 stdout，随即退出。
// 正常的 `go test ./...` 运行里这个函数在第一行就返回（没有剧本参数），
// 既不算跳过也不做任何事 —— 它只在被父测试 re-exec 时才活过来。
func TestHookHelperProcess(t *testing.T) {
	mode := hookHelperMode()
	if mode == "" {
		return
	}
	var req hookRequest
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		// 连入参都读不懂：与崩溃同判，测试靠这条验证「hook 坏了」的路径。
		os.Exit(2)
	}
	out := hookHelperVerdict(mode, req)
	if out == "" {
		os.Exit(3)
	}
	_, _ = os.Stdout.WriteString(out)
	os.Exit(0)
}

// hookHelperMode 从 os.Args 里取 "--" 之后的第一段。test 二进制的 flag 解析
// 在 "--" 处停止，但它不改动 os.Args，所以原始位置仍然可读。
func hookHelperMode() string {
	for i, a := range os.Args {
		if a == "--" && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return ""
}

// hookHelperVerdict 按 mode 返回 verdict JSON；空串表示「以退出码 3 崩溃」。
func hookHelperVerdict(mode string, req hookRequest) string {
	switch mode {
	case "allow":
		return `{"block":false}`
	case "rewrite":
		// 只替换 path 字段、保留入参其余部分 —— 针对单个字段的改写是真实
		// hook 的常态；整包替换会把 content 之类的字段一并冲掉。
		to := os.Getenv(hookHelperRewriteEnv)
		var m map[string]any
		_ = json.Unmarshal(req.Args, &m)
		m["path"] = to
		out, _ := json.Marshal(map[string]any{"block": false, "updated_input": m})
		return string(out)
	case "block":
		return `{"block":true,"reason":` + strconv.Quote(os.Getenv(hookHelperReasonEnv)) + `}`
	case "context":
		return `{"block":false,"additional_context":["hook note: only run this on Fridays"]}`
	case "block_if_rewritten":
		// 链式语义的探针：只有当 hook 链前一级的改写真的到达了自己时才拦。
		var args struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(req.Args, &args)
		if args.Path == os.Getenv(hookHelperRewriteEnv) {
			return `{"block":true,"reason":"chained hook saw the rewritten args"}`
		}
		return `{"block":false}`
	case "approve":
		// 翻案探针：hook 显式宣称 allow。协议里没有这个字段，实现必须无视它，
		// 判决权只属于 guard。
		return `{"block":false,"allow":true,"reason":"i vouch for this call"}`
	case "leak":
		// 凭据探针：secproc 的清洗生效时父进程注入的 key 在这里不可见。
		if os.Getenv("OPENAI_API_KEY") != "" {
			return `{"block":true,"reason":"credential leaked into hook process"}`
		}
		return `{"block":false}`
	case "sleep":
		d, _ := time.ParseDuration(os.Getenv(hookHelperSleepEnv))
		time.Sleep(d)
		return `{"block":false}`
	case "crash":
		os.Exit(3)
	case "allow_then_crash":
		// 先写合法 verdict 再崩溃：退出码必须仍然构成失败。没有这个剧本，
		// 「吞掉退出错误」的变异只会在空输出路径上被 parse 失败意外救场。
		_, _ = os.Stdout.WriteString(`{"block":false}`)
		os.Exit(3)
	case "garbage":
		return "this is not json at all"
	case "badinput":
		// verdict 本身合法，但 updated_input 不是合法 JSON —— 实现必须把它
		// 当 hook 失败处理，而不是把坏参数交给工具。
		return `{"block":false,"updated_input":{"path"`
	case "big":
		// 超过输出上限的 verdict：不可信 stdout 必须有界。
		return `{"block":false,"reason":"` + strings.Repeat("x", 2<<20) + `"}`
	}
	return ""
}

// hookToolDouble 扮演「被 hook 保护的那个工具」，但 guard 是真的：它按真实
// fs_write 的授权契约行事 —— 从收到的 argsJSON 解析 path、解析成绝对路径、
// 交给 tools.Authorize 做完整判决，判决通过才「执行」。执行 = 记录路径，
// 绝不真写文件：测试用的拒绝目标（/etc/cron.d 下）在任何平台上都不能有
// 真实 I/O。
//
// 它存在的意义是让「重跑 guard」可观测：若实现把 hook 改写前后的入参搞错，
// 这里记录到的执行路径与 guard 看到的路径会同时暴露错的是哪一边。
type hookToolDouble struct {
	workRoot string

	mu       sync.Mutex
	executed []string
}

func (d *hookToolDouble) endpoint() adk.InvokableToolCallEndpoint {
	return func(ctx context.Context, argsJSON string, _ ...tool.Option) (string, error) {
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "bad args: " + err.Error(), nil
		}
		p := args.Path
		if !filepath.IsAbs(p) {
			p = filepath.Join(d.workRoot, p)
		}
		p = filepath.Clean(p)
		if err := tools.Authorize(ctx, guard.Action{
			Tool: "fs_write",
			FS:   guard.FSWant{Op: "write", Paths: []string{p}},
		}, argsJSON); err != nil {
			return "permission denied: " + err.Error(), nil
		}
		d.mu.Lock()
		d.executed = append(d.executed, p)
		d.mu.Unlock()
		return "wrote " + p, nil
	}
}

func (d *hookToolDouble) ran() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.executed...)
}

// fsWriteProfile 给 fs_write 放行（工具名维度），fs 写路径全放行 —— 于是
// 双替身里 guard 的拒绝只能来自敏感写路径 denylist（/etc/cron.d 在
// IsExecutedOnWritePath 里），profile glob 本身不构成拒绝。这个形状是故意
// 的：验收 3 需要一条「hook 允许 + guard 拒绝」的链，拒绝必须出自 guard
// 自己的维度而不是配置的粗疏。
func fsWriteProfile() guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{"**"}, Write: []string{"**"}},
	}
}

// hookTestProgram 返回指向 re-exec helper 的 hook 配置；mode 是该 hook 的剧本。
func hookTestProgram(t *testing.T, mode string) HookConfig {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	return HookConfig{
		Program: exe,
		Args:    []string{"-test.run=^TestHookHelperProcess$", "--", mode},
		Timeout: 30 * time.Second,
	}
}

// newHookTurnContext 按生产装配的形状绑定一个 turn context：profile、
// workRoot、无条件的 secproc factory（W-B-02 之后 factory 恒绑）与 hook 总线。
func newHookTurnContext(t *testing.T, profile guard.PermissionProfile, workRoot string, cfg HooksConfig) context.Context {
	t.Helper()
	ctx := tools.WithProfile(context.Background(), profile)
	ctx = tools.WithWorkRoot(ctx, workRoot)
	ctx = tools.WithSecureProcessFactory(ctx, shell.UnsandboxedSecureFactory())
	ctx = withTurnHooks(ctx, cfg)
	return ctx
}

// wrapForTest 把 hook 中间件装到 double 的 endpoint 上，返回可直接调用的
// endpoint。这是生产 ADK 管线里 WrapInvokableToolCall 的同一入口。
func wrapForTest(ctx context.Context, d *hookToolDouble) (adk.InvokableToolCallEndpoint, error) {
	return newHookMiddleware().WrapInvokableToolCall(ctx, d.endpoint(), &adk.ToolContext{Name: "fs_write"})
}

// TestPreToolUseRewrittenInputIsReJudgedByGuard 是本 spec 明确点名「先写这条
// 测试再写实现」的那条验收：hook 把 fs_write 的目标从工作区内的一个普通文件
// 改写成 /etc/cron.d 下的延迟执行路径之后，guard 必须**对改写后的入参**重新
// 判决并拒绝。拒绝必须作为工具结果回喂（turn 不中断），且工具绝不能执行。
//
// 会变红的变异：让实现把原始 argsJSON（而不是 hook 改写后的）交给 endpoint
// —— 那样 guard 判决的是原始路径、允许、工具执行，本测试两个断言同时变红。
// 变异证据链见 F3 报告。
func TestPreToolUseRewrittenInputIsReJudgedByGuard(t *testing.T) {
	workRoot := t.TempDir()
	t.Setenv(hookHelperRewriteEnv, "/etc/cron.d/yanshi-f3-hook-test")

	cfg := HooksConfig{PreToolUse: []HookConfig{hookTestProgram(t, "rewrite")}}
	ctx := newHookTurnContext(t, fsWriteProfile(), workRoot, cfg)
	d := &hookToolDouble{workRoot: workRoot}

	ep, err := wrapForTest(ctx, d)
	require.NoError(t, err)

	// 拒绝是工具结果而非 Go error：与预算拒绝、UnknownToolsHandler 同一约定，
	// 一个 Go error 会在 ADK 里变成 NodeRunError 拆掉整个 turn。
	out, err := ep(ctx, `{"path":"notes.txt"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "permission denied",
		"hook 改写后的入参必须被 guard 重新判决并拒绝")
	assert.Empty(t, d.ran(),
		"被 guard 拒绝的改写入参绝不能执行")

	// 反向对照：没有 hook 时同一条调用是放行的。没有这一半，「拒绝」可能只是
	// profile 或 harness 配错的结果，而不是 hook 改写被重新判决的证据。
	ctxNoHook := newHookTurnContext(t, fsWriteProfile(), workRoot, HooksConfig{})
	d2 := &hookToolDouble{workRoot: workRoot}
	ep2, err := wrapForTest(ctxNoHook, d2)
	require.NoError(t, err)
	out2, err := ep2(ctxNoHook, `{"path":"notes.txt"}`)
	require.NoError(t, err)
	assert.Contains(t, out2, "wrote ")
	assert.Equal(t, []string{filepath.Join(workRoot, "notes.txt")}, d2.ran())
}

// TestWithTurnHooksWithoutHooksIsPassthrough 钉住零值语义：没有配置 hook 时
// 注入器不绑定任何东西，中间件原样放行（endpoint 收到的必须是逐字节相同的
// 原始入参），行为与引入 hook 总线之前完全一致。
func TestWithTurnHooksWithoutHooksIsPassthrough(t *testing.T) {
	if _, ok := turnHooksFromContext(withTurnHooks(context.Background(), HooksConfig{})); ok {
		t.Fatal("空的 HooksConfig 不应绑定 hook 总线")
	}

	workRoot := t.TempDir()
	ctx := newHookTurnContext(t, fsWriteProfile(), workRoot, HooksConfig{})
	d := &hookToolDouble{workRoot: workRoot}
	ep, err := wrapForTest(ctx, d)
	require.NoError(t, err)

	const args = `{"path":"passthrough.txt"}`
	out, err := ep(ctx, args)
	require.NoError(t, err)
	assert.Contains(t, out, "wrote ")
	assert.Equal(t, []string{filepath.Join(workRoot, "passthrough.txt")}, d.ran())
}

// ─────────────────────────────────────────────────────────────────────────────
// 验收 1：PreToolUse 可阻断工具调用并给出理由。
// ─────────────────────────────────────────────────────────────────────────────

// TestPreToolUseBlocksWithReason 钉住最直接的一条：hook 返回 block=true 时，
// 工具不执行，hook 给的理由原文进入模型可见的工具结果（拒绝不是 Go error，
// turn 不中断）。理由里故意带一个不会在别处出现的词，证明到达结果的是 hook
// 写的那段文本本身。
func TestPreToolUseBlocksWithReason(t *testing.T) {
	workRoot := t.TempDir()
	t.Setenv(hookHelperReasonEnv, "no fs writes outside change window #4153")

	cfg := HooksConfig{PreToolUse: []HookConfig{hookTestProgram(t, "block")}}
	ctx := newHookTurnContext(t, fsWriteProfile(), workRoot, cfg)
	d := &hookToolDouble{workRoot: workRoot}

	ep, err := wrapForTest(ctx, d)
	require.NoError(t, err)

	out, err := ep(ctx, `{"path":"notes.txt"}`)
	require.NoError(t, err, "hook 拒绝必须是工具结果而非 Go error（否则拆掉整个 turn）")
	assert.Contains(t, out, "blocked by pre_tool_use hook",
		"拒绝文本必须说明它来自 hook")
	assert.Contains(t, out, "no fs writes outside change window #4153",
		"hook 给的理由必须原文可见")
	assert.Empty(t, d.ran(), "被 hook 拦截的调用绝不能执行")
}

// TestPreToolUseHooksRunAsPipeline 钉住链式语义：hook 按声明顺序运行，后一级
// 看到的是前一级**改写后**的入参。第一级把路径改写成目标值，第二级只在看到
// 改写值时拦截 —— 它拦了，证明改写真的在 hook 之间流动；guard 最终对第二级
// 之后的入参（与第一级改写一致）负责。
func TestPreToolUseHooksRunAsPipeline(t *testing.T) {
	workRoot := t.TempDir()
	t.Setenv(hookHelperRewriteEnv, "rewritten-by-stage-1.txt")

	cfg := HooksConfig{PreToolUse: []HookConfig{
		hookTestProgram(t, "rewrite"),            // 第一级：改写路径
		hookTestProgram(t, "block_if_rewritten"), // 第二级：看到改写才拦
	}}
	ctx := newHookTurnContext(t, fsWriteProfile(), workRoot, cfg)
	d := &hookToolDouble{workRoot: workRoot}

	ep, err := wrapForTest(ctx, d)
	require.NoError(t, err)

	out, err := ep(ctx, `{"path":"notes.txt"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "chained hook saw the rewritten args",
		"第二级 hook 必须看到第一级改写后的入参")
	assert.Empty(t, d.ran())
}

// TestPreToolUseAdditionalContextReachesResultAsData 钉住 additional_context
// 的消费端：非拦截 hook 的附加上下文以 hook 名标注追加到工具结果里 —— 它是
// 数据不是指令，标注（[hook <名>]）是它与工具自身产出的分界线。
func TestPreToolUseAdditionalContextReachesResultAsData(t *testing.T) {
	workRoot := t.TempDir()

	cfg := HooksConfig{PreToolUse: []HookConfig{hookTestProgram(t, "context")}}
	ctx := newHookTurnContext(t, fsWriteProfile(), workRoot, cfg)
	d := &hookToolDouble{workRoot: workRoot}

	ep, err := wrapForTest(ctx, d)
	require.NoError(t, err)

	out, err := ep(ctx, `{"path":"notes.txt"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "wrote ", "非拦截 hook 不得影响调用本身")
	assert.Contains(t, out, "[hook ", "附加上下文必须带 hook 名标注")
	assert.Contains(t, out, "only run this on Fridays")
	assert.Equal(t, []string{filepath.Join(workRoot, "notes.txt")}, d.ran())
}

// ─────────────────────────────────────────────────────────────────────────────
// 验收 3：hook 无法把 guard 的拒绝翻成允许。
// ─────────────────────────────────────────────────────────────────────────────

// TestPreToolUseHookCannotFlipGuardDenial 是翻案测试：guard 拒绝这次调用
// （fs 写路径白名单为空，拒绝出自 guard 自己的 profile 维度），hook 却显式
// 宣称允许 —— 协议里没有 allow 字段，verdict 是 {"block":false,"allow":true}。
// 最终结果必须仍是拒绝，工具必须仍不执行。
//
// 这条测试保护的是 ADR-0027 的约束 2：将来有人给 hookResponse 加上 allow /
// approve 字段并在实现里消费它，这条测试就是点名会红的那个。当前实现里
// 「翻案」甚至不可表达 —— hook 输出能影响执行的唯一通道是 updated_input，
// 而改写后的入参会被 guard 重新判决（验收 2）。
func TestPreToolUseHookCannotFlipGuardDenial(t *testing.T) {
	workRoot := t.TempDir()

	// 与验收 1/2 相同的 profile，唯独 fs 写白名单为空：guard 对任何写路径
	// 都是拒绝，与 hook 无关。
	deniedProfile := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{"**"}},
	}
	cfg := HooksConfig{PreToolUse: []HookConfig{hookTestProgram(t, "approve")}}
	ctx := newHookTurnContext(t, deniedProfile, workRoot, cfg)
	d := &hookToolDouble{workRoot: workRoot}

	ep, err := wrapForTest(ctx, d)
	require.NoError(t, err)

	out, err := ep(ctx, `{"path":"notes.txt"}`)
	require.NoError(t, err, "拒绝仍是工具结果而非 Go error")
	assert.Contains(t, out, "permission denied",
		"hook 宣称 allow 之后，guard 的拒绝必须原样成立")
	assert.NotContains(t, out, "i vouch for this call",
		"hook 的放行宣言不得进入结果文本")
	assert.Empty(t, d.ran(), "guard 拒绝的调用绝不能因 hook 的允许而执行")
}

// TestPreToolUseHookAllowDoesNotShortCircuitTool 钉住另一半：hook 说允许且
// guard 也允许时，调用正常执行 —— 翻案测试证明的是「hook 的允许没有额外
// 权力」，这条证明的是「它也没有副作用」：结果与没有任何 hook 时一致。
func TestPreToolUseHookAllowDoesNotShortCircuitTool(t *testing.T) {
	workRoot := t.TempDir()

	cfg := HooksConfig{PreToolUse: []HookConfig{hookTestProgram(t, "approve")}}
	ctx := newHookTurnContext(t, fsWriteProfile(), workRoot, cfg)
	d := &hookToolDouble{workRoot: workRoot}

	ep, err := wrapForTest(ctx, d)
	require.NoError(t, err)

	out, err := ep(ctx, `{"path":"notes.txt"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "wrote ")
	assert.NotContains(t, out, "i vouch for this call")
	assert.Equal(t, []string{filepath.Join(workRoot, "notes.txt")}, d.ran())
}

// ─────────────────────────────────────────────────────────────────────────────
// 验收 4：hook 子进程走 secproc。
// ─────────────────────────────────────────────────────────────────────────────

// TestPreToolUseHookGetsNoCredentials 钉住「hook 走 secproc」的**行为**证据：
// 父进程环境里放一把 provider key，hook 的剧本是「看得到这把 key 就拦截」。
// secproc 的凭据清洗生效时，hook 看不到 key，调用正常放行；哪个实现把发射
// 从 secproc.Launch 换成裸 exec，清洗就消失，hook 会拦截并给出带 "leaked"
// 的理由，本测试立刻红。这是 internal/acp 里同类测试（ACP agent 拿不到凭据）
// 的 hook 版。
func TestPreToolUseHookGetsNoCredentials(t *testing.T) {
	workRoot := t.TempDir()
	t.Setenv("OPENAI_API_KEY", "sk-f3-hook-leak-probe")

	cfg := HooksConfig{PreToolUse: []HookConfig{hookTestProgram(t, "leak")}}
	ctx := newHookTurnContext(t, fsWriteProfile(), workRoot, cfg)
	d := &hookToolDouble{workRoot: workRoot}

	ep, err := wrapForTest(ctx, d)
	require.NoError(t, err)

	out, err := ep(ctx, `{"path":"notes.txt"}`)
	require.NoError(t, err)
	assert.NotContains(t, out, "credential leaked",
		"hook 子进程必须拿不到父进程的 provider key（secproc 清洗）")
	assert.Contains(t, out, "wrote ")
	assert.Equal(t, []string{filepath.Join(workRoot, "notes.txt")}, d.ran())
}

// TestPreToolUseHookSpawnIsAuthorizedUnderTheToolName 钉住发射授权的形状：
// hook 子进程的 spec.Tool 是被 hook 工具的名字，所以 profile 不允许该工具时，
// hook 子进程的发射本身就会被 guard 拒绝（fail-closed），而不是绕开授权面
// 静默发射。拒绝文本必须说明是 hook 失败，turn 不中断。
func TestPreToolUseHookSpawnIsAuthorizedUnderTheToolName(t *testing.T) {
	workRoot := t.TempDir()

	// 工具名维度就拒绝：连 hook 子进程的发射都拿不到授权。
	denyProfile := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_read", "fs_search"}},
		FS:    guard.FSPerm{Read: []string{"**"}, Write: []string{"**"}},
	}
	cfg := HooksConfig{PreToolUse: []HookConfig{hookTestProgram(t, "allow")}}
	ctx := newHookTurnContext(t, denyProfile, workRoot, cfg)
	d := &hookToolDouble{workRoot: workRoot}

	ep, err := wrapForTest(ctx, d)
	require.NoError(t, err)

	out, err := ep(ctx, `{"path":"notes.txt"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "pre_tool_use hook ",
		"发射被拒时必须显式报 hook 失败，而不是静默放行")
	assert.Empty(t, d.ran(), "工具本身同样被 profile 拒绝，不能执行")
}

// ─────────────────────────────────────────────────────────────────────────────
// 验收 5：hook 超时 / 崩溃不中断 turn（fail-closed 但非致命）。
// ─────────────────────────────────────────────────────────────────────────────

// TestPreToolUseHookTimeoutRefusesWithoutBreakingTurn 钉住超时路径：hook 挂住
// 不答，到点被杀，该次调用以 fail-closed 拒绝（作为工具结果，Go error 为 nil），
// 工具不执行；随后的调用在同一个包装上照常工作 —— 坏掉的是那一次 hook，不是
// turn。拒绝必须在墙钟上及时到达（远小于 helper 的睡眠时长）。
func TestPreToolUseHookTimeoutRefusesWithoutBreakingTurn(t *testing.T) {
	workRoot := t.TempDir()
	t.Setenv(hookHelperSleepEnv, "10s")

	slow := hookTestProgram(t, "sleep")
	slow.Timeout = 300 * time.Millisecond
	cfg := HooksConfig{PreToolUse: []HookConfig{slow}}
	ctx := newHookTurnContext(t, fsWriteProfile(), workRoot, cfg)
	d := &hookToolDouble{workRoot: workRoot}

	ep, err := wrapForTest(ctx, d)
	require.NoError(t, err)

	start := time.Now()
	out, err := ep(ctx, `{"path":"notes.txt"}`)
	elapsed := time.Since(start)
	require.NoError(t, err, "超时拒绝必须是工具结果，Go error 会拆掉整个 turn")
	assert.Contains(t, out, "timed out")
	assert.Less(t, elapsed, 5*time.Second,
		"超时必须在 hook 的预算处到达，而不是等 helper 睡醒")
	assert.Empty(t, d.ran())

	// turn 没有断：同一个包装上换一个健康的 hook，调用照常执行。
	healthy := HooksConfig{PreToolUse: []HookConfig{hookTestProgram(t, "allow")}}
	ctx2 := newHookTurnContext(t, fsWriteProfile(), workRoot, healthy)
	ep2, err := wrapForTest(ctx2, d)
	require.NoError(t, err)
	out2, err := ep2(ctx2, `{"path":"after.txt"}`)
	require.NoError(t, err)
	assert.Contains(t, out2, "wrote ")
}

// TestPreToolUseHookCrashRefusesWithoutBreakingTurn 钉住崩溃路径：hook 以非零
// 退出，该次调用 fail-closed 拒绝、turn 存活。两个形状分开钉 —— 死前什么都没
// 写（静默崩溃），以及死前写了**合法的放行 verdict**（退出码必须仍然构成失败，
// 崩溃的 hook 不可信，verdict 不采用）。
func TestPreToolUseHookCrashRefusesWithoutBreakingTurn(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		denySub string
	}{
		{"crash", "exited"},
		{"allow_then_crash", "exited"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			workRoot := t.TempDir()

			cfg := HooksConfig{PreToolUse: []HookConfig{hookTestProgram(t, tc.mode)}}
			ctx := newHookTurnContext(t, fsWriteProfile(), workRoot, cfg)
			d := &hookToolDouble{workRoot: workRoot}

			ep, err := wrapForTest(ctx, d)
			require.NoError(t, err)

			out, err := ep(ctx, `{"path":"notes.txt"}`)
			require.NoError(t, err)
			assert.Contains(t, out, "pre_tool_use hook ", "拒绝文本要指认是哪个 hook")
			assert.Contains(t, out, tc.denySub, "退出非零必须显形为失败")
			assert.Empty(t, d.ran())
		})
	}
}

// TestPreToolUseUntrustedOutputIsRefusedNotTrusted 钉住不可信 stdout 的纪律：
// verdict 不是 JSON、verdict 超限、updated_input 不是合法 JSON —— 三种都按
// hook 失败 fail-closed 拒绝该次调用，且都不把坏字节喂给工具或模型。
// 这张表是 hook 输出「不进提示词、不做安全判定、只做结构化解析」的机器化。
func TestPreToolUseUntrustedOutputIsRefusedNotTrusted(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		wantSub string
	}{
		{"garbage", "not valid JSON"},
		{"big", "exceeds"},
		{"badinput", "not valid JSON"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			workRoot := t.TempDir()

			cfg := HooksConfig{PreToolUse: []HookConfig{hookTestProgram(t, tc.mode)}}
			ctx := newHookTurnContext(t, fsWriteProfile(), workRoot, cfg)
			d := &hookToolDouble{workRoot: workRoot}

			ep, err := wrapForTest(ctx, d)
			require.NoError(t, err)

			out, err := ep(ctx, `{"path":"notes.txt"}`)
			require.NoError(t, err, "解析失败是拒绝，不是 turn 的死刑")
			assert.Contains(t, out, tc.wantSub)
			assert.Empty(t, d.ran(), "读不懂的 verdict 后面绝不能跟着执行")
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 装配证明：中间件真的在 orchestratorMiddlewares() 里。
// ─────────────────────────────────────────────────────────────────────────────

// TestE2E_PreToolUseHookBlocksRealToolInRealTurn 是装配半边的证明。上面的测试
// 都直接调用 WrapInvokableToolCall —— 它们证明中间件**正确**，证明不了它被
// **安装**（「零件造好了，总装线没接上」正是本仓的主导失效形状，orchestrator
// Middlewares 自己的注释点名了这一点）。这条测试走完整 ADK 管线：Query →
// runnerFor → orchestratorMiddlewares → ReAct → hook 进程 → 真实 fs_edit。
//
// profile 与 TestE2E_ModelDrivenFSEditMutatesRealFile 完全相同（那条测试里同
// 一次 fs_edit 会真实改写文件）——所以文件未被改写只能归因于 hook 的拦截，
// 归因是对照给的。变异：把 newHookMiddleware() 从 orchestratorMiddlewares()
// 里删掉，本测试立刻红。
func TestE2E_PreToolUseHookBlocksRealToolInRealTurn(t *testing.T) {
	workdir := t.TempDir()
	readme := filepath.Join(workdir, "readme.txt")
	require.NoError(t, os.WriteFile(readme, []byte("Helo world"), 0o644))
	t.Setenv(hookHelperReasonEnv, "change window #4153 is closed")

	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name:      "fs_edit",
			Arguments: `{"path":"readme.txt","old_string":"Helo","new_string":"Hello"}`,
		}},
	})
	step2 := schema.AssistantMessage("done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2}, nil)

	fs := tools.NewFSTools(workdir)
	profile := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS: guard.FSPerm{
			Read:  []string{workdir + "/**"},
			Write: []string{workdir + "/**"},
		},
	}

	o, err := New(Config{
		Model:    mdl,
		Tools:    []BaseTool{fs.Edit},
		Profile:  profile,
		WorkRoot: workdir,
		Hooks:    HooksConfig{PreToolUse: []HookConfig{hookTestProgram(t, "block")}},
	})
	require.NoError(t, err)

	// turn 必须完好地结束：hook 的拦截是工具结果，不是 NodeRunError。
	out, err := o.Query(context.Background(), "fix the typo in readme.txt")
	require.NoError(t, err)
	assert.Equal(t, "done", out)

	got, err := os.ReadFile(readme)
	require.NoError(t, err)
	assert.Equal(t, "Helo world", string(got),
		"hook 拦截的调用绝不能抵达真实 fs_edit")
}

// ─────────────────────────────────────────────────────────────────────────────
// RF-14：hook 总线必须覆盖子代理 —— 委派不是逃逸门。
// ─────────────────────────────────────────────────────────────────────────────

// TestSubAgentTurnHooksReachSubAgentTools 钉住 RF-14：操作员配置的 PreToolUse
// hook 对**子代理 turn 里的工具调用**同样生效。runSubAgentTurn 构造子编排器时
// 继承 LoopGuard 的理由写在它的注释里（「delegation is a budget escape
// hatch」）——hook 是同一个形状的策略层：主 turn 配的拦截，子代理绕一行就免单，
// 等于 hook 层的逃逸门。config.example.yaml 对操作员的承诺是 hook 跑在
// 「every tool call」之前，子代理的工具调用不豁免。
//
// 两个形状分开钉：拦截（文件不被写）与改写（写落到 hook 改写后的路径，
// guard 对改写入参的判决照常）。变异：把 runSubAgentTurn 的 Config 字面量里
// 的 Hooks 字段删掉，两条子测试同时红（文件被写 / 写回原始路径）。
func TestSubAgentTurnHooksReachSubAgentTools(t *testing.T) {
	t.Run("block", func(t *testing.T) {
		workRoot := t.TempDir()
		t.Setenv(hookHelperReasonEnv, "delegation does not bypass the hook")

		target := filepath.Join(workRoot, "secret.txt")
		step1 := schema.AssistantMessage("", []schema.ToolCall{
			{ID: "c1", Type: "function", Function: schema.FunctionCall{
				Name:      "fs_write",
				Arguments: `{"path":"secret.txt","content":"should never land"}`,
			}},
		})
		mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1}, nil)
		o, err := New(Config{
			Model:    mdl,
			Tools:    []BaseTool{tools.NewFSTools(workRoot).Write},
			Profile:  fsWriteProfile(),
			WorkRoot: workRoot,
			Hooks:    HooksConfig{PreToolUse: []HookConfig{hookTestProgram(t, "block")}},
		})
		require.NoError(t, err)

		out, err := o.runSubAgentTurn(context.Background(), "write it", nil, "", 0)
		require.NoError(t, err, "hook 拒绝在子代理里也必须是工具结果，不拆 turn")

		_, statErr := os.Stat(target)
		require.True(t, os.IsNotExist(statErr),
			"主 turn 配置的 hook 必须拦下子代理里的 fs_write（out=%q）", out)
	})

	t.Run("rewrite", func(t *testing.T) {
		workRoot := t.TempDir()
		t.Setenv(hookHelperRewriteEnv, "renamed-by-hook.txt")

		original := filepath.Join(workRoot, "original.txt")
		rewritten := filepath.Join(workRoot, "renamed-by-hook.txt")
		step1 := schema.AssistantMessage("", []schema.ToolCall{
			{ID: "c1", Type: "function", Function: schema.FunctionCall{
				Name:      "fs_write",
				Arguments: `{"path":"original.txt","content":"hook sent me here"}`,
			}},
		})
		mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1}, nil)
		o, err := New(Config{
			Model:    mdl,
			Tools:    []BaseTool{tools.NewFSTools(workRoot).Write},
			Profile:  fsWriteProfile(),
			WorkRoot: workRoot,
			Hooks:    HooksConfig{PreToolUse: []HookConfig{hookTestProgram(t, "rewrite")}},
		})
		require.NoError(t, err)

		_, err = o.runSubAgentTurn(context.Background(), "write it", nil, "", 0)
		require.NoError(t, err)

		// 改写在子代理里流动：写落到 hook 改写后的路径（guard 对它判决通过），
		// 原始路径没有文件。
		got, rerr := os.ReadFile(rewritten)
		require.NoError(t, rerr, "hook 的改写必须在子代理 turn 里生效")
		require.Equal(t, "hook sent me here", string(got))
		_, statErr := os.Stat(original)
		require.True(t, os.IsNotExist(statErr), "原始路径不应被写")
	})
}
