package guard

import "testing"

func TestNormalizeMode(t *testing.T) {
	cases := []struct {
		in   string
		want PermissionMode
		ok   bool
	}{
		{"default", ModeDefault, true},
		{"DEFAULT", ModeDefault, true},
		{"", ModeDefault, true},
		{"allow-edits", ModeAllowEdits, true},
		{"edits", ModeAllowEdits, true},
		{"yolo", ModeYOLO, true},
		{"auto", ModeAuto, true},
		{"bogus", "", false},
	}
	for _, c := range cases {
		got, ok := NormalizeMode(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("NormalizeMode(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestCycleModeWraps(t *testing.T) {
	// default -> allow-edits -> auto -> yolo -> default
	want := []PermissionMode{ModeAllowEdits, ModeAuto, ModeYOLO, ModeDefault}
	cur := ModeDefault
	for _, w := range want {
		cur = CycleMode(cur)
		if cur != w {
			t.Fatalf("CycleMode step = %q, want %q", cur, w)
		}
	}
}

func TestCycleMode_EmptyTreatedAsDefault(t *testing.T) {
	// The zero value "" cycles to allow-edits (same as ModeDefault) so
	// Shift+Tab works before the first status reply populates permMode.
	// (ModePlan 特殊处理：返回 ModeDefault 而非循环下一位。)
	if got := CycleMode(""); got != ModeAllowEdits {
		t.Errorf("CycleMode(\"\") = %q, want %q", got, ModeAllowEdits)
	}
}

// TestCycleMode_PlanNotInCycle: CycleMode 永不返回 ModePlan；从 ModePlan
// 出发也回到 ModeDefault（让 Shift+Tab 退出只读模式，与 /plan-off 对齐）。
func TestCycleMode_PlanNotInCycle(t *testing.T) {
	// 多次循环不出现 ModePlan
	cur := ModeDefault
	for i := 0; i < 8; i++ {
		cur = CycleMode(cur)
		if cur == ModePlan {
			t.Fatalf("CycleMode stepped into ModePlan after %d iterations", i)
		}
	}
	// 从 ModePlan 出发直接回 default
	if got := CycleMode(ModePlan); got != ModeDefault {
		t.Errorf("CycleMode(ModePlan) = %q, want %q", got, ModeDefault)
	}
}

// TestModesIncludesPlan: Modes() 必须包含 ModePlan（与 CycleMode 区别）。
func TestModesIncludesPlan(t *testing.T) {
	modes := Modes()
	found := false
	for _, m := range modes {
		if m == ModePlan {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Modes() = %v, want ModePlan included", modes)
	}
}

// TestNormalizeMode_Plan: "plan"/"read-only"/"ro" 都映射到 ModePlan。
func TestNormalizeMode_Plan(t *testing.T) {
	for _, s := range []string{"plan", "PLAN", "read-only", "readonly", "ro"} {
		got, ok := NormalizeMode(s)
		if !ok || got != ModePlan {
			t.Errorf("NormalizeMode(%q) = (%q,%v), want (%q,true)", s, got, ok, ModePlan)
		}
	}
}

// TestPlanToolAllowed: 白名单核心工具放行；写工具拒绝。
func TestPlanToolAllowed(t *testing.T) {
	allowed := []string{
		"fs_read", "fs_list", "memory_search", "memory_recall",
		"task_create", "task_list", "task_read",
		"update_plan", "checklist_write", "checklist_add",
		"checklist_update", "checklist_list",
		"todo_write", "todo_add", "todo_update", "todo_list",
		"artifact_read",
	}
	for _, n := range allowed {
		if !PlanToolAllowed(n) {
			t.Errorf("PlanToolAllowed(%q) = false, want true", n)
		}
	}
	denied := []string{"fs_write", "fs_edit", "fs_mkdir", "shell_run", "task_cancel", "task_gate_run", "vcs_commit", "web_fetch"}
	for _, n := range denied {
		if PlanToolAllowed(n) {
			t.Errorf("PlanToolAllowed(%q) = true, want false", n)
		}
	}
}

// TestModeLabel_Plan: ModeLabel(ModePlan) 返回 "plan"。
func TestModeLabel_Plan(t *testing.T) {
	if got := ModeLabel(ModePlan); got != "plan" {
		t.Errorf("ModeLabel(ModePlan) = %q, want %q", got, "plan")
	}
}

func TestIsEditTool(t *testing.T) {
	edits := []string{"fs_write", "fs_edit", "fs_mkdir"}
	for _, n := range edits {
		if !IsEditTool(n) {
			t.Errorf("IsEditTool(%q) = false, want true", n)
		}
	}
	notEdits := []string{"fs_read", "shell_run", "web_fetch", "vcs_commit"}
	for _, n := range notEdits {
		if IsEditTool(n) {
			t.Errorf("IsEditTool(%q) = true, want false", n)
		}
	}
}

func TestParseRiskScore(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"3", 3, true},
		{"9", 9, true},
		{"10", 10, true},
		{"Risk: 7.", 7, true},
		{"  5  ", 5, true},
		{"0", 1, true},   // clamped up
		{"99", 10, true}, // clamped down
		{"no number here", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := ParseRiskScore(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseRiskScore(%q) = (%d,%v), want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
