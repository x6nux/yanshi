package tools

import (
	"context"

	"github.com/x6nux/yanshi/internal/secrets"
)

// redactorKey 是 Redactor 在 context 中的键。未导出，因此只能经 WithRedactor
// 写入、经 RedactorFromContext 读出。
type redactorKey struct{}

// WithRedactor 把进程级 Redactor 绑进 ctx，供 GuardedTool.InvokableRun 在把
// 工具结果交给模型之前收口。
//
// 为什么收口在这里而不在每个工具里：ToolChunk 的字段单一归属（见 guard.go 的
// ToolChunk doc 注释）意味着只有 Result 会拼进模型结果，而 InvokableRun 是
// Result 的唯一汇合点。在工具里各改一次既是重复逻辑，也保证不了下一个新工具
// 记得改 —— 而「写了但零读者」这个形状在本仓已经复发过九次。
//
// r 为 nil 时返回原 ctx：未绑定等于不脱敏，行为与引入前逐字节一致。
func WithRedactor(ctx context.Context, r *secrets.Redactor) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, redactorKey{}, r)
}

// RedactorFromContext 读出 WithRedactor 绑定的 Redactor。
//
// 双返回值形态是刻意的：本注入器带 nil 门禁，所以消费方必须处理 ok=false，
// 不能假设一定有值（CLAUDE.md 记的「nil 就不注入」那个坑）。
func RedactorFromContext(ctx context.Context) (*secrets.Redactor, bool) {
	r, ok := ctx.Value(redactorKey{}).(*secrets.Redactor)
	return r, ok && r != nil
}

// redactForModel 用 ctx 里的 Redactor 处理即将交给模型的字符串。
// 未绑定时原样返回。
func redactForModel(ctx context.Context, s string) string {
	if r, ok := RedactorFromContext(ctx); ok {
		return r.Redact(s)
	}
	return s
}
