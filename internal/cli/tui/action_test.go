package tui

import (
	"testing"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/guard"
)

func TestCollectActions_CommandsSource(t *testing.T) {
	m := newTestModel(t)
	items := m.collectActions()
	// commandTable 至少有这些:/help /model /think /mode /clear /cost /stats /sessions /theme
	hasHelp := false
	for _, it := range items {
		if it.Label == "/help" && it.Source == "command" {
			hasHelp = true
			break
		}
	}
	if !hasHelp {
		t.Errorf("collectActions 应包含 /help(command 源),items=%+v", items)
	}
}

func TestCollectActions_ModesFromRegistry(t *testing.T) {
	m := newTestModel(t)
	items := m.collectActions()
	// guard.Modes() 返回 [ModeDefault, ModeAllowEdits, ModeAuto, ModeYOLO]
	expected := guard.Modes()
	for _, exp := range expected {
		found := false
		for _, it := range items {
			if it.Source == "mode" && it.Label == string(exp) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("mode 源应含 %q (guard.Modes registry),items=%+v", exp, items)
		}
	}
}

func TestCollectActions_ThemesFromRegistry(t *testing.T) {
	m := newTestModel(t)
	items := m.collectActions()
	// themeList = [themeDefault, themeHighContrast, themeMuted]
	if len(themeList) == 0 {
		t.Fatal("themeList 不能为空(预置)")
	}
	for _, exp := range themeList {
		found := false
		for _, it := range items {
			if it.Source == "theme" && it.Label == string(exp.Name) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("theme 源应含 %q (themeList registry)", exp.Name)
		}
	}
}

// TestAction_FuzzyRank.
//
// ledger: C2/UX1#2 fuzzy 过滤
func TestAction_FuzzyRank(t *testing.T) {
	m := newTestModel(t)
	m.action = &actionState{query: "mod"}
	ranked := m.rankedActions()
	// /model 应比 /clear 排前(都含字母但 /model 的"mod"是连续前缀)
	if len(ranked) < 2 {
		t.Fatalf("ranked 应有多个项")
	}
	// 找出 /model 的位置
	modelIdx := -1
	for i, it := range ranked {
		if it.Label == "/model" {
			modelIdx = i
			break
		}
	}
	if modelIdx == -1 {
		t.Fatalf("/model 应在 ranked 里")
	}
	if modelIdx > 2 {
		t.Errorf("/model (fuzzy 'mod' 前缀匹配) 应排前三,实际位置 %d", modelIdx)
	}
}

func TestAction_EmptyQueryShowsAll(t *testing.T) {
	m := newTestModel(t)
	m.action = &actionState{query: ""}
	ranked := m.rankedActions()
	commands := m.collectActions()
	if len(ranked) != len(commands) {
		t.Errorf("空 query 应显示全部,得到 %d / %d", len(ranked), len(commands))
	}
}

// TestAction_ModelsReplyPopulatesCacheAndRefreshesPopup 验证真实异步模型流程:
// 1. Ctrl+K/openActionPopup 设 actionLoadingModels=true;
// 2. popup 先显示 command/mode/theme + 旧 model cache;
// 3. 后端真实回复 Kind="models", Items=[]string{"gpt-4o", "claude-3"};
// 4. applyEvent case "models" 复制 m.models、清 pending、active popup 原地重排。
// 不设置本地完成信号:绝不在 reply 到达前抢跑 loaded。
func TestAction_ModelsReplyPopulatesCacheAndRefreshesPopup(t *testing.T) {
	m := newTestModel(t)
	m.openActionPopup()
	if !m.actionLoadingModels {
		t.Errorf("openActionPopup 后应 actionLoadingModels=true")
	}
	before := len(m.action.items)
	entriesBefore := len(m.entries)
	m = m.applyEvent(cli.StreamEvent{
		Kind:  "models",
		Items: []string{"gpt-4o", "claude-3"},
	})
	if m.actionLoadingModels {
		t.Errorf("真实 models reply 到达后应清 actionLoadingModels")
	}
	if len(m.models) != 2 || m.models[0] != "gpt-4o" || m.models[1] != "claude-3" {
		t.Fatalf("case models 应复制 ev.Items 到 m.models,得到 %v", m.models)
	}
	if m.action == nil || len(m.action.items) <= before {
		t.Fatalf("active popup 应在 reply 后原地加入 model items,before=%d after=%d", before, len(m.action.items))
	}
	if m.action.cursor != 0 {
		t.Errorf("model reply 重排后 cursor 应重置为 0,得到 %d", m.action.cursor)
	}
	if len(m.entries) != entriesBefore {
		t.Errorf("Ctrl+K 后台 refresh 不应追加 modelsEntry,before=%d after=%d", entriesBefore, len(m.entries))
	}
	hasModel := false
	for _, it := range m.action.items {
		if it.Source == "model" && it.Label == "claude-3" {
			hasModel = true
			break
		}
	}
	if !hasModel {
		t.Errorf("model 源应含 claude-3,items=%+v", m.action.items)
	}
}

// TestAction_OpenUsesCacheButStillRefreshes.
//
// ledger: C2/UX1#1 Ctrl+K 打开全局面板
func TestAction_OpenUsesCacheButStillRefreshes(t *testing.T) {
	m := newTestModel(t)
	m.models = []string{"cached-model"}
	m.openActionPopup()
	if !m.actionLoadingModels {
		t.Fatal("cache 非空时每次打开仍应标记 refresh pending")
	}
	foundCached := false
	for _, it := range m.action.items {
		if it.Source == "model" && it.Label == "cached-model" {
			foundCached = true
		}
	}
	if !foundCached {
		t.Fatalf("等待 refresh 时应先展示 cache,items=%+v", m.action.items)
	}
	m.closeActionPopup()
	if !m.actionLoadingModels {
		t.Fatal("reply 到达前关闭 popup 不得清 pending")
	}
	m = m.applyEvent(cli.StreamEvent{Kind: "models", Items: []string{"fresh-model"}})
	if m.actionLoadingModels {
		t.Fatal("popup 已关闭时真实 reply 仍应清 pending")
	}
	if len(m.models) != 1 || m.models[0] != "fresh-model" {
		t.Fatalf("popup 关闭不应妨碍 cache 更新,得到 %v", m.models)
	}
}

func TestAction_PopupVisibleWithZeroItems(t *testing.T) {
	m := newTestModel(t)
	m.action = &actionState{visible: true, items: nil}
	m.actionLoadingModels = true
	if got := m.actionPopup(); got == "" {
		t.Fatal("active popup 即使零候选也应渲染 loading/no-match 状态")
	}
	m.closeActionPopup()
	if got := m.actionPopup(); got != "" {
		t.Fatalf("关闭后 actionPopup 应为空,得到 %q", got)
	}
}

// TestAction_OpenDoesNotPretendModelsLoaded 锁定竞态修复:reply 未到前
// actionLoadingModels 必须保持 true,且 popup 只能消费已有 cache。
func TestAction_OpenDoesNotPretendModelsLoaded(t *testing.T) {
	m := newTestModel(t)
	m.models = nil
	m.openActionPopup()
	if !m.actionLoadingModels {
		t.Fatal("reply 未到前不得标 loaded")
	}
	for _, it := range m.action.items {
		if it.Source == "model" {
			t.Fatalf("cache 为空且 reply 未到时不应出现 model item:%+v", it)
		}
	}
}
