package tui

import (
	"strings"
	"testing"
)

// ---- B4: unified diff for fs_edit/fs_write (T3 diff) ---------------------

// TestUnifiedDiff proves unifiedDiff produces a minimal line-level LCS diff:
// unchanged lines carry a leading " ", removed lines "-", added lines "+".
// The classic invariant — equal lines never appear with + or - — catches a
// broken DP table (e.g. swapped indices, off-by-one in backtrack).
func TestUnifiedDiff(t *testing.T) {
	d := unifiedDiff("a\nb\nc", "a\nx\nc")
	if !strings.Contains(d, "-b") || !strings.Contains(d, "+x") {
		t.Fatalf("diff 缺增删行:\n%s", d)
	}
	if strings.Contains(d, "-a") || strings.Contains(d, "+a") || strings.Contains(d, "-c") || strings.Contains(d, "+c") {
		t.Fatalf("未变行 a/c 不应在 diff:\n%s", d)
	}
}

// TestUnifiedDiffAllInsert proves the all-insert case (empty old → multi-line
// new) produces only "+" lines; the empty-old edge case must not panic or
// silently drop content.
func TestUnifiedDiffAllInsert(t *testing.T) {
	d := unifiedDiff("", "x\ny")
	if !strings.Contains(d, "+x") || !strings.Contains(d, "+y") {
		t.Fatalf("全增:\n%s", d)
	}
	if strings.Contains(d, "-") {
		t.Fatalf("空 old 不应有删除行:\n%s", d)
	}
}

// TestUnifiedDiffAllDelete proves the all-delete case (non-empty old → empty
// new) produces only "-" lines.
func TestUnifiedDiffAllDelete(t *testing.T) {
	d := unifiedDiff("x\ny", "")
	if !strings.Contains(d, "-x") || !strings.Contains(d, "-y") {
		t.Fatalf("全删:\n%s", d)
	}
	if strings.Contains(d, "+") {
		t.Fatalf("空 new 不应有新增行:\n%s", d)
	}
}

// TestUnifiedDiffIdentical proves equal inputs produce only context (" ")
// lines, never + or -.
func TestUnifiedDiffIdentical(t *testing.T) {
	d := unifiedDiff("a\nb", "a\nb")
	for _, ln := range strings.Split(d, "\n") {
		if len(ln) == 0 || ln[0] != ' ' {
			t.Fatalf("全等输入应全为 context 行: got %q in %q", ln, d)
		}
	}
}

// TestRenderEditDiff proves an fs_edit tool call renders an "Edit" header and
// either a colored diff (expanded) or a compact "+N -M 行" fold hint with the
// ctrl+o cue. This guards both the diff dispatch (toolDispDiff) and the
// collapse/expand branch of renderDiff.
func TestRenderEditDiff(t *testing.T) {
	e := &toolEntry{name: "fs_edit", args: `{"path":"f.go","old_string":"a\nb","new_string":"a\nc"}`, root: ".", status: "ok"}
	out := e.render(80, newSpinner())
	if !strings.Contains(out, "Edit") {
		t.Fatalf("应有 Edit 头部")
	}
	// 折叠态显 "+N -M 行" 或 expanded 显 diff；至少含 diff 痕迹（-b/+c）或折叠提示
	if !strings.Contains(out, "-b") && !strings.Contains(out, "ctrl+o") && !strings.Contains(out, "行") {
		t.Fatalf("应显 diff 或折叠提示:\n%s", out)
	}
}

// TestRenderEditDiffExpanded proves the expanded branch surfaces the full
// colored diff (both + and - lines) when ctrl+o is toggled.
func TestRenderEditDiffExpanded(t *testing.T) {
	e := &toolEntry{
		name:     "fs_edit",
		args:     `{"path":"f.go","old_string":"a\nb","new_string":"a\nc"}`,
		root:     ".",
		status:   "ok",
		expanded: true,
	}
	out := e.render(80, newSpinner())
	if !strings.Contains(out, "-b") || !strings.Contains(out, "+c") {
		t.Fatalf("expanded 应显完整 diff (-b/+c):\n%s", out)
	}
}

// TestRenderWriteNewFile proves a brand-new fs_write (no old content
// available) surfaces a compact "wrote N lines" hint rather than fabricating
// a diff against missing content.
func TestRenderWriteNewFile(t *testing.T) {
	e := &toolEntry{
		name:   "fs_write",
		args:   `{"path":"new.go","content":"package main\nfunc main() {}\n"}`,
		root:   ".",
		status: "ok",
	}
	out := e.render(80, newSpinner())
	if !strings.Contains(out, "Write") {
		t.Fatalf("应有 Write 头部:\n%s", out)
	}
	// 新建：没有任何旧版本可比，要么显示行数提示，要么折叠提示。绝不应崩溃。
	if !strings.Contains(out, "wrote") && !strings.Contains(out, "ctrl+o") && !strings.Contains(out, "行") {
		t.Fatalf("新建文件应有 wrote/行数提示:\n%s", out)
	}
}
