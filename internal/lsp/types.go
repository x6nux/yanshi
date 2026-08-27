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

// Location is one resolved place in the workspace: a file plus a 1-based
// range. It is what textDocument/definition and textDocument/references
// return after parsing.
//
// Line/Column are 1-based, matching Diagnostic and every editor the model's
// output is read next to. LSP is 0-based on the wire; the +1 happens once, in
// locationFrom, so no consumer has to remember which convention it is holding.
// Zero means "the server gave no range" — WorkspaceSymbol permits a location
// with a uri and nothing else — and callers must treat it as unknown rather
// than as line 0.
type Location struct {
	// Path is the local filesystem path decoded from URI. Empty when the URI
	// is not a file:// one (some servers answer with untitled: or jdt:).
	Path string
	// URI is the raw location URI as the server sent it.
	URI string
	// Line is the 1-based start line, or 0 when the server sent no range.
	Line int
	// Column is the 1-based start column, or 0 when the server sent no range.
	Column int
	// EndLine is the 1-based end line, or 0 when the server sent no range.
	EndLine int
	// EndColumn is the 1-based end column, or 0 when the server sent no range.
	EndColumn int
}

// SymbolKind is the LSP SymbolKind enum. The zero value means the server
// omitted it, which is legal for WorkspaceSymbol responses.
type SymbolKind int

// LSP SymbolKind values, matching the LSP specification numbering. The whole
// enum is listed rather than the subset currently rendered: these numbers
// arrive from the server, so an unlisted one would print as a bare integer.
const (
	SymbolFile          SymbolKind = 1
	SymbolModule        SymbolKind = 2
	SymbolNamespace     SymbolKind = 3
	SymbolPackage       SymbolKind = 4
	SymbolClass         SymbolKind = 5
	SymbolMethod        SymbolKind = 6
	SymbolProperty      SymbolKind = 7
	SymbolField         SymbolKind = 8
	SymbolConstructor   SymbolKind = 9
	SymbolEnum          SymbolKind = 10
	SymbolInterface     SymbolKind = 11
	SymbolFunction      SymbolKind = 12
	SymbolVariable      SymbolKind = 13
	SymbolConstant      SymbolKind = 14
	SymbolString        SymbolKind = 15
	SymbolNumber        SymbolKind = 16
	SymbolBoolean       SymbolKind = 17
	SymbolArray         SymbolKind = 18
	SymbolObject        SymbolKind = 19
	SymbolKey           SymbolKind = 20
	SymbolNull          SymbolKind = 21
	SymbolEnumMember    SymbolKind = 22
	SymbolStruct        SymbolKind = 23
	SymbolEvent         SymbolKind = 24
	SymbolOperator      SymbolKind = 25
	SymbolTypeParameter SymbolKind = 26
)

// symbolKindNames maps the enum to the lowercase labels put in tool results.
var symbolKindNames = map[SymbolKind]string{
	SymbolFile: "file", SymbolModule: "module", SymbolNamespace: "namespace",
	SymbolPackage: "package", SymbolClass: "class", SymbolMethod: "method",
	SymbolProperty: "property", SymbolField: "field", SymbolConstructor: "constructor",
	SymbolEnum: "enum", SymbolInterface: "interface", SymbolFunction: "function",
	SymbolVariable: "variable", SymbolConstant: "constant", SymbolString: "string",
	SymbolNumber: "number", SymbolBoolean: "boolean", SymbolArray: "array",
	SymbolObject: "object", SymbolKey: "key", SymbolNull: "null",
	SymbolEnumMember: "enum-member", SymbolStruct: "struct", SymbolEvent: "event",
	SymbolOperator: "operator", SymbolTypeParameter: "type-parameter",
}

// String renders the kind as a lowercase label. Unknown or absent kinds render
// as "" so a result can omit the field instead of printing a bare number the
// model would have to look up.
func (k SymbolKind) String() string { return symbolKindNames[k] }

// SymbolInfo is one symbol from textDocument/documentSymbol or
// workspace/symbol, flattened.
//
// The two requests answer in four different shapes (DocumentSymbol tree,
// SymbolInformation list, WorkspaceSymbol list, and DocumentSymbol trees whose
// children carry no uri of their own); parseSymbols normalises all of them to
// this, so callers never branch on which one arrived.
type SymbolInfo struct {
	// Name is the symbol's own name, without any container prefix.
	Name string
	// Kind is the LSP SymbolKind, or 0 when the server omitted it.
	Kind SymbolKind
	// Container is the enclosing symbol or containerName, when the server
	// supplied one. Empty for top-level symbols.
	Container string
	// Location points at the symbol's name (selectionRange) when the server
	// distinguishes it from the full body range.
	Location Location
}

// Hover is a parsed textDocument/hover response: the rendered documentation
// plus, when the server supplied one, the range the hover applies to.
type Hover struct {
	// Contents is the hover text with every MarkedString / MarkupContent
	// shape collapsed to plain markdown. Empty when the server had nothing.
	Contents string
	// Line is the 1-based start line of the hover range, 0 when absent.
	Line int
	// Column is the 1-based start column of the hover range, 0 when absent.
	Column int
	// EndLine is the 1-based end line of the hover range, 0 when absent.
	EndLine int
	// EndColumn is the 1-based end column of the hover range, 0 when absent.
	EndColumn int
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
