package tui

import (
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
)

func TestHelp_ModesFromRegistry(t *testing.T) {
	m := newTestModel(t)
	help := m.renderHelp()
	for _, mode := range guard.Modes() {
		if !strings.Contains(help, string(mode)) {
			t.Errorf("help 应包含 mode %q (来自 guard.Modes() registry),help=%q", mode, help)
		}
	}
}

func TestHelp_CommandsFromTable(t *testing.T) {
	m := newTestModel(t)
	help := m.renderHelp()
	// commandTable.name 不带 "/",help 渲染时统一加 "/" 前缀(与 Task 4 collectActions 一致)
	for _, cmd := range commandTable {
		label := "/" + cmd.name
		if !strings.Contains(help, label) {
			t.Errorf("help 应含 command %q (来自 commandTable, 带 / 前缀)", label)
		}
	}
}

func TestHelp_ThemesFromList(t *testing.T) {
	m := newTestModel(t)
	help := m.renderHelp()
	for _, th := range themeList {
		if !strings.Contains(help, string(th.Name)) {
			t.Errorf("help 应含 theme %q (来自 themeList)", th.Name)
		}
	}
}

func TestHelp_FuzzyFilter(t *testing.T) {
	m := newTestModel(t)
	m.helpVisible = true
	m.helpQuery = "mode"
	filtered := m.rankedHelpEntries()
	// "mode" 应匹配 "/mode"(command)与 4 个 mode 名
	hasModeCmd := false
	for _, e := range filtered {
		if e.Label == "/mode" {
			hasModeCmd = true
			break
		}
	}
	if !hasModeCmd {
		t.Errorf("help filter 'mode' 应保留 /mode command,filtered=%+v", filtered)
	}
}

// TestHelp_KeybindingsStaticTable 是"防漂移哨兵":
// 新增 keybinding 时,开发者必须同步 help.keyBindings 静态表,否则此测试不会失败
// (Go 反射不出 keybinding);但至少 modes/commands/themes 三个动态段的漂移会被抓住。
// 本测试仅断言静态表里至少有这些核心键(与 commands_test.go 的 KeyMsg 分支交叉验证)。
func TestHelp_KeybindingsCoreEntries(t *testing.T) {
	m := newTestModel(t)
	help := m.renderHelp()
	core := []string{"Enter", "Ctrl+K", "Ctrl+O", "F1", "Esc"}
	for _, k := range core {
		if !strings.Contains(help, k) {
			t.Errorf("help 应含核心键 %q (防漂移哨兵)", k)
		}
	}
}

func TestHelp_PopupVisibilityAndQuery(t *testing.T) {
	m := newTestModel(t)
	if got := m.helpPopup(); got != "" {
		t.Fatalf("helpVisible=false 时 popup 应为空,得到 %q", got)
	}
	m.helpVisible = true
	m.helpQuery = "mode"
	got := m.helpPopup()
	if !strings.Contains(got, "search: mode") || !strings.Contains(got, "/mode") {
		t.Fatalf("popup 应显示 query 和匹配项,得到 %q", got)
	}
}
