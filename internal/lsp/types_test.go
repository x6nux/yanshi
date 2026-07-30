package lsp

import (
	"strings"
	"testing"
)

func TestFormatDiags_Empty(t *testing.T) {
	if got := FormatDiags("foo.go", nil); got != "" {
		t.Fatalf("空诊断应返回空串,得到 %q", got)
	}
}

func TestFormatDiags_RendersSeverityAndLocation(t *testing.T) {
	diags := []Diagnostic{
		{Line: 11, Column: 4, Severity: SeverityError, Message: "undefined: Foo", Source: "gopls"},
		{Line: 21, Column: 2, Severity: SeverityWarning, Message: "unused var x", Source: "gopls"},
	}
	got := FormatDiags("main.go", diags)
	for _, want := range []string{"main.go", "ERR", "L11:4", "undefined: Foo", "WARN", "L21:2", "unused var x"} {
		if !strings.Contains(got, want) {
			t.Errorf("FormatDiags 缺少 %q;完整输出:\n%s", want, got)
		}
	}
}
