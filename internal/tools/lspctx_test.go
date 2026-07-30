package tools

import (
	"context"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/lsp"
)

func TestLSPContext_BindRetrieve(t *testing.T) {
	ctx := context.Background()
	if _, ok := LSPFromContext(ctx); ok {
		t.Fatal("未绑定时应 ok=false")
	}
	var mgr LSPManager = lsp.New(lsp.Config{}) // disabled manager,满足接口
	ctx = WithLSP(ctx, mgr)
	got, ok := LSPFromContext(ctx)
	if !ok || got != mgr {
		t.Fatalf("绑后应取回同一 Manager,ok=%v", ok)
	}
}

// fakeLSPManager 是测试用 stub,验证 diagFor 读 context + 调 Manager 的行为,
// 不依赖真 server(评审 #10 的 fake-first)。
type fakeLSPManager struct {
	changedPath string
	changedText string
	diags       []lsp.Diagnostic
}

func (f *fakeLSPManager) Enabled() bool { return true }
func (f *fakeLSPManager) DidChange(path, content string) {
	f.changedPath = path
	f.changedText = content
}
func (f *fakeLSPManager) Diagnostics(path string, timeout time.Duration) []lsp.Diagnostic {
	return f.diags
}

// TestDiagFor_NoManagerEmpty 验证无 Manager 时返回空串(不追加,不污染)。
func TestDiagFor_NoManagerEmpty(t *testing.T) {
	if got := diagFor(context.Background(), "/x/y.go", "pkg"); got != "" {
		t.Fatalf("无 Manager 应返回空串,得到 %q", got)
	}
}

// TestDiagFor_DisabledManagerEmpty 验证 disabled Manager 也返回空串。
func TestDiagFor_DisabledManagerEmpty(t *testing.T) {
	ctx := WithLSP(context.Background(), lsp.New(lsp.Config{})) // disabled
	if got := diagFor(ctx, "/x/y.go", "pkg"); got != "" {
		t.Fatalf("disabled Manager 应返回空串,得到 %q", got)
	}
}

// TestDiagFor_FormatsAndDidChanges 验证 diagFor 先 DidChange 再取诊断并渲染。
func TestDiagFor_FormatsAndDidChanges(t *testing.T) {
	fm := &fakeLSPManager{diags: []lsp.Diagnostic{
		{Line: 1, Column: 1, Severity: lsp.SeverityError, Message: "boom", Source: "gopls"},
	}}
	ctx := WithLSP(context.Background(), fm)
	got := diagFor(ctx, "/x/y.go", "package main")
	if fm.changedPath != "/x/y.go" || fm.changedText != "package main" {
		t.Errorf("diagFor 应调 DidChange(path,content),得到 path=%q text=%q", fm.changedPath, fm.changedText)
	}
	if !containsStr(got, "boom") || !containsStr(got, "y.go") {
		t.Errorf("diagFor 应含诊断渲染,得到 %q", got)
	}
}

func containsStr(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
