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
	diags := mgr.Diagnostics(absPath, 0) // 0 → Manager 用 cfg.Timeout
	return lsp.FormatDiags(filepath.Base(absPath), diags)
}
