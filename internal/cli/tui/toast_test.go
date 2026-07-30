package tui

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

func TestToast_PushAndQueue(t *testing.T) {
	ts := &toastQueue{}
	ts.push(toast{Level: "info", Text: "saved"})
	if len(ts.items) != 1 {
		t.Fatalf("push 后应有 1 条,得到 %d", len(ts.items))
	}
	for i := 0; i < 10; i++ {
		ts.push(toast{Level: "info", Text: "t"})
	}
	if len(ts.items) > 5 {
		t.Errorf("堆叠应限 5 条(FIFO 丢旧),得到 %d", len(ts.items))
	}
}

func TestToast_PruneExpired(t *testing.T) {
	ts := &toastQueue{}
	ts.push(toast{Level: "info", Text: "old", ExpiresAt: time.Now().Add(-time.Second)})
	ts.push(toast{Level: "info", Text: "new", ExpiresAt: time.Now().Add(time.Second)})
	before := len(ts.items)
	ts.prune(time.Now())
	if len(ts.items) != 1 || ts.items[0].Text != "new" {
		t.Errorf("prune 应移除过期,保留 new;before=%d after=%d items=%v", before, len(ts.items), ts.items)
	}
}

// TestToast_PruneReflow 验证 prune 后 blockHeight 重新计算(review 必修)。
func TestToast_PruneReflow(t *testing.T) {
	ts := &toastQueue{}
	for i := 0; i < 3; i++ {
		ts.push(toast{Level: "info", Text: "t", ExpiresAt: time.Now().Add(-time.Second)})
	}
	hBefore := ts.blockHeight()
	ts.prune(time.Now())
	hAfter := ts.blockHeight()
	if hBefore == 0 {
		t.Fatalf("prune 前 blockHeight 应非 0")
	}
	if hAfter != 0 {
		t.Errorf("prune 后所有项过期,blockHeight 应归 0,得到 %d", hAfter)
	}
}

func TestToast_ErrorNeverExpires(t *testing.T) {
	ts := &toastQueue{}
	ts.push(toast{Level: "error", Text: "fail"}) // 不设 ExpiresAt
	ts.prune(time.Now().Add(time.Hour))
	if len(ts.items) != 1 {
		t.Errorf("error toast 不应过期,得到 %d", len(ts.items))
	}
}

// TestToast_TruncateWidthSafe 验证 width<6 不 panic(review 必修)。
func TestToast_TruncateWidthSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("truncate width<6 panic: %v", r)
		}
	}()
	long := strings.Repeat("x", 100)
	for w := 0; w < 10; w++ {
		_ = truncateToast(long, w)
	}
}

func TestToast_TruncateAddsEllipsis(t *testing.T) {
	got := truncateToast("hello world", 8)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("width=8 应加省略号,得到 %q", got)
	}
	if lipgloss.Width(got) > 8 {
		t.Errorf("结果 display width 不应超过 8,得到 %q(width=%d)", got, lipgloss.Width(got))
	}
}

func TestToast_TruncateUnicodeWidthSafe(t *testing.T) {
	got := truncateToast("你好，世界 hello", 8)
	if !utf8.ValidString(got) {
		t.Fatalf("不得从 UTF-8 rune 中间切断,得到 %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("Unicode 长文本应加省略号,得到 %q", got)
	}
	if lipgloss.Width(got) > 8 {
		t.Errorf("Unicode 结果 display width 不应超过 8,得到 %q(width=%d)", got, lipgloss.Width(got))
	}
}

// TestToast_PushStartsOnlyOneTickChain 验证多个 toast 只启动一条 tick 链:
// 第一次 push 返回 tea.Cmd 并置 active;第二次 push 返回 nil(复用已有 tick)。
func TestToast_PushStartsOnlyOneTickChain(t *testing.T) {
	m := newTestModel(t)
	first := m.pushToast("info", "one")
	if first == nil || !m.toastTickActive {
		t.Fatalf("第一条 toast 应启动 tick 并置 active")
	}
	second := m.pushToast("warn", "two")
	if second != nil {
		t.Errorf("tick 已 active 时第二条 toast 不应再启动 tick")
	}
	if len(m.toasts.items) != 2 {
		t.Errorf("两条 toast 均应在队列中,得到 %d", len(m.toasts.items))
	}
}

// TestToast_TickPrunesExpiredAndStops 验证 toastTickMsg 真的驱动自动过期:
// 过期后队列空、active=false、Update 不再 re-arm Cmd。
func TestToast_TickPrunesExpiredAndStops(t *testing.T) {
	m := newTestModel(t)
	_ = m.pushToast("info", "expired")
	m.toasts.items[0].ExpiresAt = time.Now().Add(-time.Second)
	updated, cmd := m.Update(toastTickMsg{})
	got := updated.(model)
	if len(got.toasts.items) != 0 {
		t.Errorf("过期 toast 应在 tick 后自动消失,得到 %d", len(got.toasts.items))
	}
	if got.toastTickActive {
		t.Errorf("队列空后 toastTickActive 应为 false")
	}
	if cmd != nil {
		t.Errorf("队列空后不应 re-arm tick")
	}
}
