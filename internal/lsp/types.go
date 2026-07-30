// Package lsp 提供轻量的语言服务器协议客户端:按工作区语言 spawn 对应 server
// (MVP: gopls),在 agent 编辑文件后把诊断回喂模型。无可用 server 时 Manager
// 退化为 no-op,不阻塞启动(沿用 VCS 软降级模式)。
package lsp

import (
	"strconv"
	"strings"
)

// Severity 是 LSP DiagnosticSeverity 的子集(只取我们消费的几档)。
type Severity int

// LSP diagnostic severity levels, matching the LSP specification values.
const (
	SeverityError   Severity = 1
	SeverityWarning Severity = 2
	SeverityInfo    Severity = 3
	SeverityHint    Severity = 4
)

// Diagnostic 是一条回喂给模型的诊断。Line/Column 是 1-based(LSP 是 0-based,
// 在解析处 +1 转换),便于模型/人直接定位。
type Diagnostic struct {
	Line     int
	Column   int
	Severity Severity
	Message  string
	Source   string // 诊断来源,通常是 server 名(gopls)
}

// sevLabel 把 Severity 映射成短标签放进结果文本。
func sevLabel(s Severity) string {
	switch s {
	case SeverityWarning:
		return "WARN"
	case SeverityInfo:
		return "INFO"
	case SeverityHint:
		return "HINT"
	default:
		return "ERR"
	}
}

// FormatDiags 把 file 的诊断渲染成追加到工具结果的文本。空诊断返回空串
// (调用方据此决定是否写进 JSON 的 "diagnostics" 字段),避免给模型灌无信息内容。
func FormatDiags(file string, diags []Diagnostic) string {
	if len(diags) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nDiagnostics for " + file + ":")
	for _, d := range diags {
		b.WriteString("\n  [" + sevLabel(d.Severity) + "] L" +
			strconv.Itoa(d.Line) + ":" + strconv.Itoa(d.Column))
		if d.Source != "" {
			b.WriteString(" (" + d.Source + ")")
		}
		b.WriteString(" " + d.Message)
	}
	return b.String()
}
