// Package tools holds yanshi's Eino tool implementations, guard-wrapped.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/guard"
	otelobs "github.com/x6nux/yanshi/internal/observe/otel"
)

type profileKey struct{}

// WithProfile binds a PermissionProfile to ctx (the acting agent's profile).
func WithProfile(ctx context.Context, p guard.PermissionProfile) context.Context {
	return context.WithValue(ctx, profileKey{}, p)
}

// ProfileFromContext returns the bound profile, if any.
func ProfileFromContext(ctx context.Context) (guard.PermissionProfile, bool) {
	p, ok := ctx.Value(profileKey{}).(guard.PermissionProfile)
	return p, ok
}

// DenyErr is returned when the guard rejects a tool call.
type DenyErr struct{ Reason string }

// Error returns a human-readable denial message.
func (e *DenyErr) Error() string { return "tools: denied: " + e.Reason }

// IsDenyErr reports whether err is a guard denial.
func IsDenyErr(err error) bool {
	var d *DenyErr
	return errors.As(err, &d)
}

// ToolChunk 是工具通过 Stream channel 推送的固定结构。所有工具一律产出此结构；
// TUI 与编排层各取固定字段，零 per-tool 特判。channel close = 工具结束。
//
// 字段单一归属（No Field Sharing）：每个字段只有一个消费者，绝不共享。
//   - Text      → TUI 下方正文区（按 Overwrite 追加/覆盖）
//   - Status    → TUI 右侧状态区（恒覆盖；直接型=简短指示 / subagent=统计摘要）
//   - Result    → 模型（拼接成模型结果）
//   - Overwrite → TUI 渲染模式标记（追加/覆盖），仅作用于 Text
//   - Err       → 内部标记（errcnt 熔断 + TUI 标红）；可读错误文本由工具经 Text+Result 推送
type ToolChunk struct {
	Text      string
	Status    string
	Result    string
	Overwrite bool
	Err       error
}

// StreamFunc 是工具的执行体：返回一个 channel，工具在 goroutine 里推 ToolChunk，
// 结束时 close。模型路径（InvokableRun）和 TUI 路径都消费同一个 channel。
type StreamFunc func(ctx context.Context, argsJSON string) <-chan ToolChunk

// Tool 是所有 yanshi 工具的统一契约。所有工具一律实现，无长短任务分叉。
type Tool interface {
	tool.InvokableTool                                            // Info + InvokableRun（兼容 Eino/ADK）
	DisplayName() string                                          // ① TUI 块标题
	DefaultTimeout() time.Duration                                // ③ 默认超时
	Stream(ctx context.Context, argsJSON string) <-chan ToolChunk // ②④ 唯一执行入口
}

// SyncStream 把一个同步函数 fn 包成 StreamFunc：在 goroutine 里跑 fn，把返回值同时
// 推进 Text（给 TUI）和 Result（给模型）——内容相同，字段分开（字段单一归属）。
// 错误推进 Text+Result（前缀 "✗ "）并置 Err。channel 在推完一片后 close。
//
// 这不是第二种工具构造——所有工具仍是 StreamFunc；SyncStream 只是消除同步型工具
// 手写 goroutine/channel 样板的 helper。需要流式（shell_run）或活动面板（subagent）
// 的工具直接手写 StreamFunc，不用 SyncStream。
func SyncStream(fn func(ctx context.Context, argsJSON string) (string, error)) StreamFunc {
	return func(ctx context.Context, argsJSON string) <-chan ToolChunk {
		ch := make(chan ToolChunk, 1)
		go func() {
			defer close(ch)
			out, err := fn(ctx, argsJSON)
			if err != nil {
				pushErrChunk(ch, err)
				return
			}
			ch <- ToolChunk{Text: out, Result: out}
		}()
		return ch
	}
}

// pushErrChunk 推一个错误 chunk：错误文本（前缀 "✗ " + friendlyErr）同时进 Text 和
// Result，Err 置位。供 SyncStream 与手写 StreamFunc 的工具（shell_run/subagent）复用。
func pushErrChunk(ch chan<- ToolChunk, err error) {
	msg := "✗ " + friendlyErr(err)
	ch <- ToolChunk{Text: msg, Result: msg, Err: err}
}

// friendlyErr 把权限拒绝（DenyErr）渲染为 "permission denied: <reason>"，其他错误用
// .Error()。让工具内部 Authorize 拒绝（fs/shell/net 维度的 DenyErr）与 Stream 入口的
// 权限拒绝都统一带 "permission denied" 前缀，模型/测试可据此识别权限拒绝。
func friendlyErr(err error) string {
	if IsDenyErr(err) {
		return "permission denied: " + denyReason(err)
	}
	return err.Error()
}

// ToolChunkCallback tees a tool's Stream chunks to the transport layer so the
// TUI can render them live. The WS handler binds it (emitting tool_chunk frames
// to the TUI) via WithToolChunkCallback. When bound, GuardedTool.Stream forwards
// each chunk to the callback (TUI) IN ADDITION to the returned channel (which
// InvokableRun drains to collect Result for the model). This resolves the
// architectural tension between ADK's InvokableRun (returns string) and the TUI
// needing the live Stream: the model path still goes through InvokableRun, while
// the TUI path rides the callback — no ADK tool-call loop rewrite required.
type ToolChunkCallback func(displayName string, c ToolChunk)

type toolChunkCallbackKey struct{}

// WithToolChunkCallback binds cb into ctx. A nil cb leaves ctx unchanged (Stream
// does not tee; only the channel consumer — typically InvokableRun — sees chunks).
func WithToolChunkCallback(ctx context.Context, cb ToolChunkCallback) context.Context {
	if cb == nil {
		return ctx
	}
	return context.WithValue(ctx, toolChunkCallbackKey{}, cb)
}

// ToolChunkCallbackFromContext returns the bound callback, or nil.
func ToolChunkCallbackFromContext(ctx context.Context) ToolChunkCallback {
	cb, _ := ctx.Value(toolChunkCallbackKey{}).(ToolChunkCallback)
	return cb
}

// errorResult 返回错误文本（前缀 "✗ "），供同步型工具把操作错误作为"结果"回喂模型
// （而非 Go error）——让模型改路径重试，而不是中断整个 turn。与 SyncStream 配合：
// 工具写 `return errorResult("..."), nil`。真·系统错误仍 `return "", err`（SyncStream
// 会推 Err chunk 触发 errcnt 熔断）。
func errorResult(msg string) string {
	return "✗ " + msg
}

// GuardedTool is a tool.InvokableTool whose run is gated by the acting agent's
// PermissionProfile (bound via WithProfile). If no profile is bound, the run is
// denied (fail-closed). When a permission callback is bound (WS interactive
// mode), a denial is escalated to the user via Authorize before failing closed.
type GuardedTool struct {
	name             string
	display          string
	desc             string
	timeout          time.Duration
	params           *schema.ParamsOneOf
	stream           StreamFunc
	approvalRequired bool
}

// NoTimeout marks a tool whose execution is bounded by the turn context
// alone. Stream branches on it explicitly and uses context.WithCancel instead
// of WithTimeout.
//
// The explicit branch is not optional and swapping the constant for another
// value does not replace it: context.WithTimeout treats ANY non-positive
// duration as already expired, so a negative sentinel handed to WithTimeout
// fails exactly as instantly as 0 did. The value is a marker; the branch is
// the mechanism.
//
// "Bounded by the turn" is a real bound, not a euphemism for unbounded -- the
// turn context is cancelled on user interrupt, disconnect, and turn end. Only
// agent_wait uses this: Manager.Wait already blocks until the subagent reaches
// a terminal state, and wrapping a second deadline around that would cut off
// the very wait the caller asked for.
const NoTimeout = time.Duration(-1)

// NewGuardedTool builds a GuardedTool.
//
// A timeout of 0 panics. context.WithTimeout(ctx, 0) yields an
// already-expired context, so such a tool fails on its first line every time
// -- and because GuardedTool returns that failure as a tool RESULT rather
// than an error, the ReAct loop feeds "context deadline exceeded" back to the
// model, which retries and fails again. Nothing crashes and nothing logs; the
// turn just burns tokens. Eight agent_* tools shipped in that state.
//
// 0 is the natural way to write "no timeout set", which is exactly why it
// cannot be allowed to mean "expire immediately". Callers that want no
// deadline must say NoTimeout.
func NewGuardedTool(name, display, desc string, timeout time.Duration,
	params *schema.ParamsOneOf, stream StreamFunc) *GuardedTool {
	mustHaveTimeout(name, timeout)
	mustHaveDisplayName(name, display)
	return &GuardedTool{name: name, display: display, desc: desc, timeout: timeout, params: params, stream: stream}
}

// mustHaveTimeout rejects the one duration that silently disables a tool.
// Shared by both constructors so an approval-guarded tool cannot slip through
// the door the ordinary one closed.
func mustHaveTimeout(name string, timeout time.Duration) {
	if timeout == 0 {
		panic("tools: " + name + ": timeout 0 means an already-expired context, " +
			"which disables the tool silently; pass a duration or tools.NoTimeout")
	}
}

// mustHaveDisplayName rejects an empty TUI block title at construction, for the
// same reason mustHaveTimeout rejects a zero duration: the failure is silent
// and downstream. DisplayName is what labels the tool's block in the TUI and
// what ToolChunkCallback carries as its first argument, so an empty one gives
// the operator an unlabelled block and gives every consumer keyed by display
// name a shared "" bucket. Nothing errors; the output is just anonymous.
//
// Checking here rather than counting non-empty names over the registry makes it
// unrepresentable instead of merely absent — a tool constructed anywhere,
// including in a test or a plugin, cannot reach the registry without one.
func mustHaveDisplayName(name, display string) {
	if display == "" {
		panic("tools: " + name + ": empty DisplayName leaves the TUI block unlabelled " +
			"and collapses every display-keyed consumer into one bucket")
	}
}

// NewApprovalGuardedTool builds a GuardedTool that ALWAYS requires explicit
// per-call user approval, regardless of the PermissionProfile or permission
// mode. Used for mutation tools that must not be auto-approved (e.g. GitHub
// comment/approve/merge). The Stream method calls AuthorizeApprovalRequired
// instead of Authorize, so YOLO/auto modes and the approval.Manager allowlist
// cannot bypass the prompt.
func NewApprovalGuardedTool(name, display, desc string, timeout time.Duration,
	params *schema.ParamsOneOf, stream StreamFunc) *GuardedTool {
	mustHaveTimeout(name, timeout)
	mustHaveDisplayName(name, display)
	return &GuardedTool{name: name, display: display, desc: desc, timeout: timeout, params: params, stream: stream, approvalRequired: true}
}

// ApprovalRequired reports whether this tool goes through
// AuthorizeApprovalRequired (mandatory per-call user approval) instead of the
// ordinary profile-based Authorize.
//
// It exists because NewApprovalGuardedTool returns the very same *GuardedTool
// type as NewGuardedTool — the only difference is an unexported field — so a
// type assertion cannot tell the two apart. Several tools advertise "Approval
// required" in their model-facing description; without this accessor that
// promise is untestable and can silently drift away from the constructor
// actually used (which is exactly how automation_*/agent_batch ended up lying
// to users). Callers (tests, docs generators, TUI) use it to assert or render
// the real gating.
func (g *GuardedTool) ApprovalRequired() bool { return g.approvalRequired }

// DisplayName returns the TUI-facing tool title.
func (g *GuardedTool) DisplayName() string { return g.display }

// DefaultTimeout returns the tool's default execution timeout.
func (g *GuardedTool) DefaultTimeout() time.Duration { return g.timeout }

// Info returns the tool's schema.ToolInfo.
func (g *GuardedTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: g.name, Desc: g.desc, ParamsOneOf: g.params}, nil
}

// Stream 是工具的唯一执行入口：权限检查 → ctx 超时 → 调 streamFunc。TUI 路径与模型
// 路径（InvokableRun）都过这里，因此权限只检查一次。streamFunc 必须响应 ctx.Done
// （超时/取消）及时收尾并 close channel——Stream 的 wrapped channel 在 out 关闭后
// cancel ctx 兜底，防止 streamFunc 泄漏 goroutine/子进程。
//
// 权限拒绝时直接返回一个单 chunk 的错误流（文本 "✗ permission denied: ..."），
// 让 InvokableRun 把它当结果回喂模型，模型可改路径重试——与旧的 fail-closed→errorResult
// 语义一致，只是载体从 Go error 改为 ToolChunk。
func (g *GuardedTool) Stream(ctx context.Context, argsJSON string) <-chan ToolChunk {
	ctx, end := otelobs.StartTool(ctx, g.name)
	var authErr error
	if g.approvalRequired {
		authErr = AuthorizeApprovalRequired(ctx, guard.Action{Tool: g.name}, argsJSON)
	} else {
		authErr = Authorize(ctx, guard.Action{Tool: g.name}, argsJSON)
	}
	if authErr != nil {
		end(authErr)
		ch := make(chan ToolChunk, 1)
		pushErrChunk(ch, authErr) // Authorize 的 DenyErr；friendlyErr 加 "permission denied:" 前缀
		close(ch)
		return ch
	}
	// T3: a long-running, backgroundable call takes a different pump — one
	// that hands the model a handle at the deadline instead of a bare
	// "context deadline exceeded" with the work discarded. Everything above
	// (authorization, the OTel span) has already happened, so the offload path
	// cannot widen anything; it only changes what happens to a call that has
	// ALREADY been authorized once its foreground budget runs out. See
	// guard_offload.go.
	if ch, taken := g.streamWithOffload(ctx, argsJSON, end); taken {
		return ch
	}
	// NoTimeout must take a different call, not a different number: WithTimeout
	// treats every non-positive duration as already expired.
	var runCtx context.Context
	var cancel context.CancelFunc
	if g.timeout == NoTimeout {
		runCtx, cancel = context.WithCancel(ctx)
	} else {
		runCtx, cancel = context.WithTimeout(ctx, g.timeout)
	}
	out := g.stream(runCtx, argsJSON)
	wrapped := make(chan ToolChunk, 16)
	tuiCB := ToolChunkCallbackFromContext(ctx)
	go func() {
		defer cancel()
		defer close(wrapped)
		var toolErr error
		for c := range out {
			if c.Err != nil {
				toolErr = c.Err
			}
			if tuiCB != nil {
				tuiCB(g.name, c) // tee to TUI (matched by internal name, same as tool_call frame)
			}
			wrapped <- c // to channel consumer (InvokableRun collects Result)
		}
		end(toolErr)
	}()
	return wrapped
}

// InvokableRun 是 Eino/ADK 的模型入口：驱动 Stream，只收集 Result 字段拼成模型结果。
// Text/Status 均不计入（字段单一归属）。遇 Err 触发 errcnt 连续失败熔断（连续 5 次
// 中断 turn）；错误文本已由工具经 Result 推送，故这里不再单独 errorResult。
// spillIfTooLong 对最终拼装结果收口。
//
// 错误处理语义保留：权限/操作错误作为工具的 *结果内容*（非 Go error）回喂模型，让
// 模型改路径重试（capped by MaxIterations）；返回 Go error 会中断整个 turn。连续失败
// 熔断仍由 errcnt 触发。
//
// W-A-02：返回前一律过 redactForModel。这是工具输出通往 provider 的唯一出口，
// 在这里收口而不是在每个工具里，是因为 Result 只在这里汇合 —— 见 redactctx.go。
// 两个返回点都要过：错误分支的 result 同样会被回喂给模型。
func (g *GuardedTool) InvokableRun(ctx context.Context, argsJSON string, _ ...tool.Option) (string, error) {
	ch := g.Stream(ctx, argsJSON)
	var result strings.Builder
	var runErr error
	for c := range ch {
		if c.Result != "" {
			result.WriteString(c.Result)
		}
		if c.Err != nil {
			runErr = c.Err
		}
	}
	if runErr != nil {
		// Permission denials (DenyErr) surface as a result for the model to retry,
		// but do NOT count toward the consecutive-error circuit breaker — a user
		// may legitimately deny several calls in a row. Only operational errors
		// trip the breaker. (Preserves the pre-rewrite DenyErr carve-out.)
		var d *DenyErr
		if !errors.As(runErr, &d) {
			if c := getErrCounter(ctx); c != nil {
				*c++
				if *c >= 5 {
					return "", fmt.Errorf("tool %q failed %d consecutive times; aborting turn", g.name, *c)
				}
			}
		}
		return redactForModel(ctx, result.String()), nil
	}
	if c := getErrCounter(ctx); c != nil {
		*c = 0
	}
	return spillIfTooLong(ctx, g.name, redactForModel(ctx, result.String())), nil
}

// denyReason extracts the human-readable reason from a DenyErr; other errors
// fall back to their full message.
func denyReason(err error) string {
	var d *DenyErr
	if errors.As(err, &d) {
		return d.Reason
	}
	return err.Error()
}

var (
	_ tool.InvokableTool = (*GuardedTool)(nil)
	_ Tool               = (*GuardedTool)(nil)
)

// ParseArgs unmarshals argsJSON into out.
func ParseArgs(argsJSON string, out any) error {
	if err := json.Unmarshal([]byte(argsJSON), out); err != nil {
		return fmt.Errorf("tools: parse args: %w", err)
	}
	return nil
}
