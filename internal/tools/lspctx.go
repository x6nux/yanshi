package tools

import (
	"context"
	"path/filepath"
	"time"

	"github.com/x6nux/yanshi/internal/lsp"
)

// LSPManager 是 edit 工具消费的诊断回喂契约(接口而非具体类型,便于测试注入
// fake,且让 orchestrator 经本接口引用 LSP 而不直接 import lsp)。*lsp.Manager
// 满足该接口。
type LSPManager interface {
	Enabled() bool
	DidChange(path, content string)
	Diagnostics(path string, timeout time.Duration) []lsp.Diagnostic
	// OpenDocuments lists the files this session has notified the server
	// about, most recent first. diagnostics uses it to decide what to ask
	// about; without it open_diagnostics_count could only ever be 0.
	OpenDocuments() []string
}

type lspKey struct{}

// WithLSP 把一个 LSPManager 绑进 ctx(镜像 WithVCS)。编排器每 turn 注入;
// edit 工具据此在写盘后查诊断。nil mgr 等价于不绑定(disabled Manager 也是
// no-op,二者都让工具无副作用)。
func WithLSP(ctx context.Context, mgr LSPManager) context.Context {
	if mgr == nil {
		return ctx
	}
	return context.WithValue(ctx, lspKey{}, mgr)
}

// LSPFromContext 取回绑定的 Manager;未绑定时 ok=false。
func LSPFromContext(ctx context.Context) (LSPManager, bool) {
	m, ok := ctx.Value(lspKey{}).(LSPManager)
	return m, ok
}

// diagFor 在一次文件写入后,从 ctx 取 LSP Manager 查诊断,返回写进工具结果
// JSON "diagnostics" 字段的文本(无 Manager / disabled / 无诊断 → 空串)。
//
// 它从 context 取 Manager(不是包级全局变量——评审 #10:旧版 diagFor 是全局
// var,并发 turn 会互相覆盖;context 注入天然每 turn 独立,无 race)。被
// fs.go(runWrite/runEdit)与 fs_patch.go(runPatch)复用,签名一致。
func diagFor(ctx context.Context, absPath, content string) string {
	mgr, ok := LSPFromContext(ctx)
	if !ok || !mgr.Enabled() {
		return ""
	}
	mgr.DidChange(absPath, content)
	diags := diagnosticsWithin(mgr, absPath, LSPDiagBudget)
	return lsp.FormatDiags(filepath.Base(absPath), diags)
}

// LSPDiagBudget is the wall-clock ceiling on post-edit diagnostics for one
// tool call. Shared by the single-file edit path (diagFor) and the multi-file
// patch path, which divides it across the files it touches.
const LSPDiagBudget = 2 * time.Second

// lspDiagGrace is how much longer diagnosticsWithin waits than the budget it
// passed down. Small enough that a stalled manager is still cut off promptly,
// large enough to cover the scheduling delay between an implementation
// returning and its value reaching the channel.
const lspDiagGrace = 50 * time.Millisecond

// diagnosticsWithin calls mgr.Diagnostics on its own goroutine and abandons the
// result after budget.
//
// Passing the budget as the timeout argument — which is all both call sites used
// to do, and diagFor did not even do that, passing 0 to mean "use the manager's
// own default" — delegates the guarantee to the implementation. LSPManager is an
// INTERFACE: the orchestrator takes whatever is wired into Config.LSP, and
// Diagnostics is a synchronous call with no context parameter, so an
// implementation that ignores its timeout argument holds the turn for as long as
// it likes. "A timeout does not block the turn" then depends on a promise the
// type system does not carry. Enforcing it here makes it the caller's property.
//
// The channel is buffered so an implementation that returns late still finishes
// and exits rather than blocking forever on a send nobody will receive. One that
// never returns leaks its goroutine — the deliberate trade: a leaked goroutine
// is bounded harm, a held turn is a hung session.
func diagnosticsWithin(mgr LSPManager, path string, budget time.Duration) []lsp.Diagnostic {
	if budget <= 0 {
		return nil
	}
	done := make(chan []lsp.Diagnostic, 1)
	go func() { done <- mgr.Diagnostics(path, budget) }()
	// The timer gets a grace margin over the budget handed to the
	// implementation, so a manager that honours its argument wins the select
	// and its result is used. With both set to exactly budget, an
	// implementation returning right on its own deadline makes the select a
	// coin flip and throws away diagnostics it had already produced. This
	// caller-side bound is a backstop for an implementation that ignores the
	// argument, not the primary clock.
	timer := time.NewTimer(budget + lspDiagGrace)
	defer timer.Stop()
	select {
	case d := <-done:
		return d
	case <-timer.C:
		return nil
	}
}
