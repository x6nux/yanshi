package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/cli"
)

// TestThinkingParity_LiveDeltaAccumulates 验证连续 reasoning delta 合并到同一个
// live thinkingEntry(不会每 delta 开新 block)。
func TestThinkingParity_LiveDeltaAccumulates(t *testing.T) {
	m := newTestModel(t)
	m = m.applyEvent(cli.StreamEvent{Kind: "thinking", Text: "Hello "})
	m = m.applyEvent(cli.StreamEvent{Kind: "thinking", Text: "World"})
	te := m.lastLiveThinking()
	if te == nil {
		t.Fatal("应存在 live thinkingEntry")
	}
	if te.text != "Hello World" {
		t.Errorf("连续 delta 应合并文本,得到 %q", te.text)
	}
	if !te.live {
		t.Errorf("应仍处于 live 状态")
	}
	// Count only thinkingEntry instances — newModel also adds a startupBanner
	// entry, so the raw entries slice length is not a reliable signal.
	thinkingCount := 0
	for _, e := range m.entries {
		if _, ok := e.(*thinkingEntry); ok {
			thinkingCount++
		}
	}
	if thinkingCount != 1 {
		t.Errorf("应只有 1 个 thinkingEntry(合并),得到 %d", thinkingCount)
	}
}

// TestThinkingParity_NonThinkingFinalizesLive 验证首个非 thinking 帧把 live block
// finalize(startedAt/endedAt 均非零,live=false)。
//
// 这是「分离」在 TUI 侧的形态:思考被 detach 成 assistantEntry.thought,与正文
// 各占一个字段,而不是拼进同一段文本。线路侧的对应证据是
// internal/agent/orchestrator::TestClassifyEvents_ReasoningOnlyEmitsOnlyThinking。
//
// ledger: C2/UX8#2 正文与思考分离
func TestThinkingParity_NonThinkingFinalizesLive(t *testing.T) {
	m := newTestModel(t)
	m = m.applyEvent(cli.StreamEvent{Kind: "thinking", Text: "reasoning"})
	m = m.applyEvent(cli.StreamEvent{Kind: "agent_chunk", Text: "answer"})
	m.flushAssistant()
	// flushAssistant 把 finalized thinkingEntry detach 到 assistantEntry.thought。
	last := m.entries[len(m.entries)-1]
	ae, ok := last.(assistantEntry)
	if !ok {
		t.Fatalf("最后一个 entry 应是 assistantEntry,得到 %T", last)
	}
	if ae.thought == nil {
		t.Fatal("assistantEntry.thought 应非 nil(被 detach 注入)")
	}
	if ae.thought.live {
		t.Errorf("thought 应已 finalize(live=false)")
	}
	if ae.thought.startedAt.IsZero() || ae.thought.endedAt.IsZero() {
		t.Errorf("thought 的 startedAt/endedAt 均应非零,得到 started=%v ended=%v",
			ae.thought.startedAt, ae.thought.endedAt)
	}
}

// TestThinkingParity_InterleavedPhasesMerge 是关键 bug⑧ 回归:多个 finalized
// thinking phase(thinking → content → thinking → content)应折叠为 ONE
// "Thought for Xs" header,而不是每个 phase 独占一行。
func TestThinkingParity_InterleavedPhasesMerge(t *testing.T) {
	m := newTestModel(t)
	// 两轮 reasoning/content 都属于同一个尚未 flush 的 assistant answer。
	m = m.applyEvent(cli.StreamEvent{Kind: "thinking", Text: "phase1 "})
	m = m.applyEvent(cli.StreamEvent{Kind: "agent_chunk", Text: "answer1"})
	m = m.applyEvent(cli.StreamEvent{Kind: "thinking", Text: "phase2"})
	m = m.applyEvent(cli.StreamEvent{Kind: "agent_chunk", Text: "answer2"})
	m.flushAssistant()

	standaloneThoughts := 0
	thoughtsAttached := 0
	var merged *thinkingEntry
	for _, e := range m.entries {
		if te, ok := e.(*thinkingEntry); ok && !te.live {
			standaloneThoughts++
		}
		if ae, ok := e.(assistantEntry); ok && ae.thought != nil {
			thoughtsAttached++
			merged = ae.thought
		}
	}
	if standaloneThoughts != 0 {
		t.Errorf("不应残留 standalone finalized thinkingEntry,得到 %d", standaloneThoughts)
	}
	if thoughtsAttached != 1 {
		t.Fatalf("两轮 phase 应合并成一个 attached thought,得到 %d", thoughtsAttached)
	}
	if !strings.Contains(merged.text, "phase1") || !strings.Contains(merged.text, "phase2") {
		t.Errorf("合并 thought 应含两个 phase,得到 %q", merged.text)
	}
}

// TestThinkingParity_CtrlOExpandsCollapsed 验证 ctrl+O 翻转最近可折叠 block 的
// expanded 标志(standalone finalized thinking 或 attached thought)。
func TestThinkingParity_CtrlOExpandsCollapsed(t *testing.T) {
	m := newTestModel(t)
	m = m.applyEvent(cli.StreamEvent{Kind: "thinking", Text: "reasoning"})
	// 手动 finalize(不引入 agent_chunk 以保持独立 thinkingEntry)
	if te := m.lastLiveThinking(); te != nil {
		te.endedAt = time.Now()
		te.live = false
	}
	before := m.entries[len(m.entries)-1].(*thinkingEntry).expanded
	if m.toggleExpand() {
		// 翻转成功
		after := m.entries[len(m.entries)-1].(*thinkingEntry).expanded
		if before == after {
			t.Errorf("ctrl+O 应翻转 expanded:%v → %v", before, after)
		}
	} else {
		t.Errorf("toggleExpand 应返回 true(存在可折叠 block)")
	}
}

// TestThinkingParity_LiveBlockNotExpandable 验证 live thinkingEntry 不受 ctrl+O
// 影响(应跳过 live block,找更早的可折叠项或返回 false)。
//
// 它是折叠语义的负向边界:折叠只对「已结束的思考」成立,对正在流的块折叠会让
// 用户看不到还在增长的内容。
//
// ledger: C2/UX8#4 可折叠
func TestThinkingParity_LiveBlockNotExpandable(t *testing.T) {
	m := newTestModel(t)
	m = m.applyEvent(cli.StreamEvent{Kind: "thinking", Text: "ongoing"})
	if m.toggleExpand() {
		t.Errorf("只有 live thinkingEntry 时不应翻转(live 不可折叠)")
	}
}

// TestThinkingParity_NoThinkingNoEntry 锁定"非思考模型无影响":
// 只 apply agent_chunk(无 thinking frame),再调用现有 flushAssistant 把 pending
// 正文落成 entry;不得创建 thinkingEntry,assistantEntry.thought 必须为 nil。
//
// ledger: C2/UX8#3 非思考模型无影响
func TestThinkingParity_NoThinkingNoEntry(t *testing.T) {
	m := newTestModel(t)
	m = m.applyEvent(cli.StreamEvent{Kind: "agent_chunk", Text: "plain answer"})
	m.flushAssistant()
	seenAssistant := false
	for _, e := range m.entries {
		if _, ok := e.(*thinkingEntry); ok {
			t.Fatalf("无 thinking frame 时不应创建 *thinkingEntry")
		}
		if ae, ok := e.(assistantEntry); ok {
			seenAssistant = true
			if ae.thought != nil {
				t.Fatalf("无 thinking frame 时 assistantEntry.thought 应为 nil,得到 %+v", ae.thought)
			}
		}
	}
	if !seenAssistant {
		t.Fatal("flushAssistant 后应存在 assistantEntry")
	}
}
