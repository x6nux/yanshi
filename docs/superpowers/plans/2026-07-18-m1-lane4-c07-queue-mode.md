# C07: 消息排队模式（queue / batch / single）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 `/queue-mode` 可循环切换三种排队策略，并接通 turn 中消息提交路径：`queue`（FIFO 逐条排队执行）、`batch`（合并为一个 turn）、`single`（新消息取消在跑的 turn）；队列状态在 footer 始终可见；turn 结束（含断线产生的合成 done）时按策略 drain，队列状态保持一致。

**Architecture:** 排队是**纯 TUI 客户端状态**——服务端只看到一条条 `user_message` turn，无需改动。已存在的骨架（`QueueMode` 枚举 + `parseQueueMode`/`String`、`model.queueMode`/`msgQueue`/`autoProcessing` 字段、`cmdQueueMode` handler、`queueEntry`/`queueStyle`）只差三件事：① 把 `/queue-mode` 注册进 `commandTable`；② 在 `submit()` 的"turn 进行中"分支用 enqueue 替换当前的**静默丢弃**；③ 在 `done` 事件上按 mode drain。新增一个 `queue.go` 承载 enqueue/drain 分派核心（`dispatchSend`/`enqueue`/`drainQueue`/`syncQueueEntry`），`model.go` 只做 submit 重构 + Update 里插一行 drain 钩子，`view.go` 加一段 footer。强调最小接通，不重写已有类型。

**关键设计决策（语义冲突，以验收为准）：** 现有 `cmdQueueMode` doc 注释（commands.go:351-356）描述的 single 语义是"auto-send each queued message one by one after turn ends"，与本次验收"single 模式新消息取消在跑的 turn"**直接冲突**。由于 `msgQueue` 从未被读写、分派逻辑从未实现，这些注释只是"设想"而非事实。本 plan 以**验收标准为权威**：`queue`=FIFO 逐 turn drain；`batch`=合并；`single`=enqueue 时取消在跑的 turn，drain 时只发最新一条（丢弃更早的）。因此 Task 1 会**修正** `cmdQueueMode` 的 doc 与显示文案以匹配验收语义（surgical 修改，非重写）。

**Tech Stack:** Go stdlib；Bubble Tea（`tea.Model`/`tea.Cmd`）；现有 `internal/cli/tui`（model.go / commands.go / view.go / entries.go）。无新依赖。

**不变性（回归约束）：** turn 空闲（`streamCh == nil`）时的提交路径字节不变——既有 `TestModel_PlainTextRoutesToUserMessage` / `TestModel_CommandRoutesSlash` 必须继续通过；斜杠命令在 turn 进行中仍为 no-op（`sendControlFrame` 会覆写 `streamCh`，C07 只排队纯文本）。

---

## File Structure

- **Create** `internal/cli/tui/queue.go` — C07 排队分派核心：`dispatchSend`（手动提交与 drain 共用的"发送一个 turn"内核）、`enqueue`（turn 中提交的分派入口）、`drainQueue`（done 时按 mode drain）、`syncQueueEntry`（同步 transcript 中的 queue 标记）。同包方法，按职责单独成文件以免 `model.go` 超过 1000 纯代码行。
- **Create** `internal/cli/tui/queue_test.go` — C07 各策略的 enqueue/drain/footer/断线测试。
- **Modify** `internal/cli/tui/commands.go` — ① `commandTable` 注册 `/queue-mode`；② 重写 `cmdQueueMode`（no-arg 循环 + 修正后的语义文案 + 复用 `queueModeHelp`）；③ 新增 `nextQueueMode`/`queueModeHelp`/`queueModeDesc`；④ `queueEntry` 改为指针接收者（支持原地更新计数）。
- **Modify** `internal/cli/tui/model.go` — 重构 `submit()`（turn 中分支走 enqueue；空闲路径抽成 `dispatchSend` 调用）；在 `Update` 的 `streamMsg` 分支插 done→drain 钩子。
- **Modify** `internal/cli/tui/view.go` — `statusHeader` 末尾加 queue 段（mode != queue 或队列非空时显示）。
- **Modify** `internal/cli/tui/model_test.go` — `recordingSession` 加 `canceled` 计数（供 single-mode cancel 断言；现有测试不读该字段，向后兼容）。

---

## Task 1: 注册 `/queue-mode` + 重写 handler（循环切换 + 修正语义）

**Files:**
- Modify: `internal/cli/tui/commands.go`（`commandTable` 约 29-41 行；`cmdQueueMode` 约 351-385 行；`queueEntry` 约 596-609 行）
- Test: `internal/cli/tui/commands_test.go`（追加）

> 现状：`cmdQueueMode` 已存在但未注册；其 doc/文案描述的 single 语义与验收冲突。本任务注册命令、把 no-arg 改为"循环到下一个 mode"、并修正 doc/文案。`queueEntry` 改指针接收者是为 Task 2 的 `syncQueueEntry` 原地更新做准备。

- [ ] **Step 1: 写失败测试**

追加到 `internal/cli/tui/commands_test.go` 末尾：
```go
// ---- C07: /queue-mode registration + cycle ----

// TestModel_QueueMode_Registered verifies /queue-mode is in the command table
// (lookupCommand finds it) and /help lists it — the C07 registration gap.
func TestModel_QueueMode_Registered(t *testing.T) {
	_, ok := lookupCommand("queue-mode")
	require.True(t, ok, "/queue-mode must be registered in commandTable")
	assert.Contains(t, helpEntry{}.render(80, spinner.Model{}), "queue-mode")
}

// TestModel_QueueMode_Cycle verifies /queue-mode with no arg cycles through the
// three modes (queue → single → batch → queue) and renders the mode list.
func TestModel_QueueMode_Cycle(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	require.Equal(t, QueueModeQueue, m.queueMode)

	for _, want := range []QueueMode{QueueModeSingle, QueueModeBatch, QueueModeQueue} {
		mm, _ := m.runCommand("/queue-mode")
		m = mm.(model)
		assert.Equal(t, want, m.queueMode)
	}
	assert.Empty(t, rec.frames, "/queue-mode is local-only (no control frame)")

	// The last cycle rendered the active mode list.
	last := m.entries[len(m.entries)-1]
	ee, ok := last.(errorEntry)
	require.True(t, ok, "cycle renders an errorEntry list")
	assert.Contains(t, ee.text, "queue mode: queue")
}

// TestModel_QueueMode_ExplicitSet verifies /queue-mode <name> sets the mode and
// rejects unknown names without changing state.
func TestModel_QueueMode_ExplicitSet(t *testing.T) {
	for _, tc := range []struct {
		arg  string
		want QueueMode
	}{
		{"queue", QueueModeQueue},
		{"single", QueueModeSingle},
		{"batch", QueueModeBatch},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			m := newModel(&recordingSession{}, "/proj")
			mm, _ := m.runCommand("/queue-mode " + tc.arg)
			m = mm.(model)
			assert.Equal(t, tc.want, m.queueMode)
		})
	}

	// Unknown arg → local error, mode unchanged.
	m := newModel(&recordingSession{}, "/proj")
	m.queueMode = QueueModeBatch
	mm, _ := m.runCommand("/queue-mode nope")
	m = mm.(model)
	assert.Equal(t, QueueModeBatch, m.queueMode, "unknown arg must not change the mode")
}
```

- [ ] **Step 2: 运行测试确认失败**

Run:
```sh
go test ./internal/cli/tui -run "TestModel_QueueMode_Registered|TestModel_QueueMode_Cycle|TestModel_QueueMode_ExplicitSet" -v
```
Expected: `TestModel_QueueMode_Registered` FAIL（`lookupCommand("queue-mode")` 返回 false）；`TestModel_QueueMode_Cycle` FAIL（no-arg 当前只显示不循环，`queueMode` 仍是 `QueueModeQueue`）。

- [ ] **Step 3: 修改 `commands.go`**

(a) 在 `commandTable`（约 29-41 行）的 `{name: "mode", ...}` 一行**之后**插入：
```go
		{name: "queue-mode", help: "set/cycle queue mode (queue|single|batch)", run: cmdQueueMode},
```

(b) 用下面版本**整体替换**现有 `cmdQueueMode`（约 351-385 行）及其上方 doc 注释：
```go
// cmdQueueMode: set or cycle the message queue mode. Forms:
//
//	/queue-mode              cycle to the next mode (queue → single → batch → queue)
//	/queue-mode queue        queue follow-ups FIFO; drain one per turn in order
//	/queue-mode single       a follow-up CANCELS the running turn and supersedes the queue
//	/queue-mode batch        merge all queued follow-ups into one turn after the current ends
//
// The mode governs what submit() does when a message arrives mid-turn (see
// enqueue) and how the queue unwinds when the turn ends (see drainQueue, hooked
// in Update's streamMsg case). With no arg the command CYCLES (acceptance:
// "可循环切换") so the user can flip modes without typing; every invocation
// renders the mode list so the change is visible. An unknown arg renders a local
// error and sends nothing.
func cmdQueueMode(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) > 0 {
		qm, ok := parseQueueMode(args[0])
		if !ok {
			m.entries = append(m.entries, errorEntry{
				text: "unknown queue mode: " + args[0] + " (queue|single|batch)",
			})
			m.refresh()
			m.viewport.GotoBottom()
			return m, nil
		}
		m.queueMode = qm
	} else {
		// No-arg form: cycle queue → single → batch → queue.
		m.queueMode = nextQueueMode(m.queueMode)
	}
	m.entries = append(m.entries, errorEntry{text: queueModeHelp(m.queueMode)})
	m.refresh()
	m.viewport.GotoBottom()
	return m, nil
}

// nextQueueMode returns the mode after qm in the cycle queue → single → batch →
// queue. Used by the no-arg /queue-mode form.
func nextQueueMode(qm QueueMode) QueueMode {
	switch qm {
	case QueueModeQueue:
		return QueueModeSingle
	case QueueModeSingle:
		return QueueModeBatch
	default:
		return QueueModeQueue
	}
}

// queueModeHelp renders the three-line mode list with the active one marked, so
// every /queue-mode invocation (cycle or explicit) shows what changed. The
// descriptions match the dispatch behavior in enqueue / drainQueue.
func queueModeHelp(active QueueMode) string {
	modes := []QueueMode{QueueModeQueue, QueueModeSingle, QueueModeBatch}
	rows := make([]string, 0, len(modes))
	for _, qm := range modes {
		marker := "  "
		if qm == active {
			marker = "▶ "
		}
		rows = append(rows, "  "+marker+qm.String()+" — "+queueModeDesc(qm))
	}
	return "queue mode: " + active.String() + "\n" + strings.Join(rows, "\n")
}

// queueModeDesc is the one-line, user-facing description of a mode's effect,
// matching the dispatch in enqueue (mid-turn) and drainQueue (at turn end).
func queueModeDesc(qm QueueMode) string {
	switch qm {
	case QueueModeSingle:
		return "a new message cancels the running turn"
	case QueueModeBatch:
		return "merge all queued messages into one turn"
	default:
		return "queue follow-ups; run one per turn in order"
	}
}
```

(c) 把 `queueEntry` 的 render 接收者由值改为指针（约 599-609 行）。`type queueEntry struct { count int }` 保留不变；render 改为：
```go
// queueEntry renders a queued-message marker in the transcript. It is a pointer
// (*queueEntry) so syncQueueEntry can update its count in place as messages are
// buffered, and remove it once the queue drains. The always-visible live count
// also lives in the footer (statusHeader); this marker gives per-enqueue
// in-transcript feedback.
type queueEntry struct{ count int }

func (e *queueEntry) render(_ int, _ spinner.Model) string {
	label := "message"
	if e.count > 1 {
		label = "messages"
	}
	return "  " + queueStyle.Render(fmt.Sprintf("📨 Queued (%d %s in queue)", e.count, label)) + "\n"
}
```

- [ ] **Step 4: 运行测试确认通过**

Run:
```sh
go test ./internal/cli/tui -run "TestModel_QueueMode_Registered|TestModel_QueueMode_Cycle|TestModel_QueueMode_ExplicitSet" -v
```
Expected: 三个新测试 PASS。

- [ ] **Step 5: 提交**

```sh
git add internal/cli/tui/commands.go internal/cli/tui/commands_test.go
git commit -m "feat(tui): register /queue-mode with cycle + corrected semantics (C07)"
```

---

## Task 2: enqueue 路径 —— turn 中提交按 mode 入队（替掉静默丢弃）

**Files:**
- Create: `internal/cli/tui/queue.go`
- Modify: `internal/cli/tui/model.go`（`submit()` 约 642-672 行）
- Modify: `internal/cli/tui/model_test.go`（`recordingSession` 约 67-82 行）
- Test: `internal/cli/tui/queue_test.go`（新建）

> 现状：`submit()`（model.go:644）在 `m.streamCh != nil` 时 `return m, nil`——turn 中的消息被**静默丢弃**。本任务把该分支改为按 `queueMode` 入队；并抽出 `dispatchSend` 供手动提交与 Task 3 的 drain 共用（DRY）。`recordingSession` 加 `canceled` 计数以断言 single 的 cancel。

- [ ] **Step 1: 给 `recordingSession` 加 cancel 计数**

修改 `internal/cli/tui/model_test.go` 的 `recordingSession`（约 67-82 行）：在结构体加 `canceled int` 字段，并把 `CancelCurrent` 改为自增。改后应为：
```go
// recordingSession records every ClientFrame passed to SendFrame and every
// prompt passed to Send so command tests can assert on routing ("/clear" sent a
// clear frame, not a user_message). SendFrame returns nil (no synthetic reply —
// tests drive applyEvent of replies directly). canceled counts CancelCurrent
// calls so the C07 single-mode interrupt can be asserted.
type recordingSession struct {
	frames   []proto.ClientFrame
	sentText []string
	canceled int
}

func (r *recordingSession) Send(text string) <-chan cli.StreamEvent {
	r.sentText = append(r.sentText, text)
	return nil
}
func (r *recordingSession) SendFrame(f proto.ClientFrame) <-chan cli.StreamEvent {
	r.frames = append(r.frames, f)
	return nil
}
func (r *recordingSession) CancelCurrent() error { r.canceled++; return nil }
func (r *recordingSession) Mode() string         { return "ws" }
func (r *recordingSession) Root() string         { return "/proj" }
```

- [ ] **Step 2: 写失败测试**

新建 `internal/cli/tui/queue_test.go`：
```go
package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// findQueueEntry returns the most recent *queueEntry in the transcript, or nil.
func findQueueEntry(t *testing.T, m model) *queueEntry {
	t.Helper()
	for i := len(m.entries) - 1; i >= 0; i-- {
		if qe, ok := m.entries[i].(*queueEntry); ok {
			return qe
		}
	}
	return nil
}

// ---- C07 Task 2: enqueue ----

// TestModel_QueueEnqueue_QueueBuffers verifies that in queue mode a message
// submitted mid-turn is buffered (not sent), the input clears, the running turn
// is NOT canceled, and a transcript marker reflects the count.
func TestModel_QueueEnqueue_QueueBuffers(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.streamCh = make(chan cli.StreamEvent) // simulate an in-flight turn
	m.queueMode = QueueModeQueue
	m.input.SetValue("follow-up")

	mm, _ := m.submit()
	m = mm.(model)

	assert.Empty(t, rec.sentText, "queue mode must not send mid-turn")
	assert.Equal(t, []string{"follow-up"}, m.msgQueue)
	assert.Equal(t, 0, rec.canceled, "queue mode must not cancel the running turn")
	assert.Equal(t, "", m.input.Value(), "input must clear after enqueuing")
	qe := findQueueEntry(t, m)
	require.NotNil(t, qe, "a queue marker must be appended")
	assert.Equal(t, 1, qe.count)
}

// TestModel_QueueEnqueue_BatchBuffers is the same guard for batch mode (buffer,
// no cancel); the batch/queue difference is only at drain time.
func TestModel_QueueEnqueue_BatchBuffers(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.streamCh = make(chan cli.StreamEvent)
	m.queueMode = QueueModeBatch

	m.input.SetValue("a")
	mm, _ := m.submit()
	m = mm.(model)
	m.input.SetValue("b")
	mm, _ = m.submit()
	m = mm.(model)

	assert.Equal(t, []string{"a", "b"}, m.msgQueue)
	assert.Equal(t, 0, rec.canceled)
	qe := findQueueEntry(t, m)
	require.NotNil(t, qe)
	assert.Equal(t, 2, qe.count, "marker tracks the growing queue")
}

// TestModel_QueueEnqueue_SingleCancels verifies single mode cancels the running
// turn at enqueue time (interrupt semantics) while still buffering the message.
func TestModel_QueueEnqueue_SingleCancels(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.streamCh = make(chan cli.StreamEvent)
	m.queueMode = QueueModeSingle
	m.input.SetValue("interrupt")

	mm, _ := m.submit()
	m = mm.(model)

	assert.Equal(t, []string{"interrupt"}, m.msgQueue, "single still buffers (drain sends latest)")
	assert.Equal(t, 1, rec.canceled, "single must cancel the running turn")
	assert.True(t, m.canceling, "canceling flag dedupes a second cancel")
}

// TestModel_QueueEnqueue_SingleCancelIdempotent verifies a second mid-turn
// submit in single mode does not cancel twice (the first cancel is still in
// flight). Both messages buffer; drain (Task 3) keeps only the latest.
func TestModel_QueueEnqueue_SingleCancelIdempotent(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.streamCh = make(chan cli.StreamEvent)
	m.queueMode = QueueModeSingle

	m.input.SetValue("a")
	mm, _ := m.submit()
	m = mm.(model)
	m.input.SetValue("b")
	mm, _ = m.submit()
	m = mm.(model)

	assert.Equal(t, []string{"a", "b"}, m.msgQueue)
	assert.Equal(t, 1, rec.canceled, "cancel must not be re-issued while already canceling")
}

// TestModel_QueueEnqueue_SlashCommandDropped verifies a slash command submitted
// mid-turn is still a no-op: control frames would clobber the in-flight
// streamCh via sendControlFrame, so C07 queues only plain messages.
func TestModel_QueueEnqueue_SlashCommandDropped(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.streamCh = make(chan cli.StreamEvent)
	m.queueMode = QueueModeQueue
	m.input.SetValue("/model foo")

	mm, _ := m.submit()
	m = mm.(model)

	assert.Empty(t, rec.frames, "slash commands mid-turn must not send a control frame")
	assert.Empty(t, m.msgQueue, "slash commands must not enqueue")
}

// TestModel_QueueSubmit_IdlePathUntouched verifies the idle submit path
// (streamCh == nil) is byte-identical to pre-C07: plain text goes through Send
// as a user_message. Regression guard for the submit() restructure.
func TestModel_QueueSubmit_IdlePathUntouched(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.input.SetValue("hello there")

	mm, _ := m.submit()
	m = mm.(model)

	require.Equal(t, []string{"hello there"}, rec.sentText)
	assert.Empty(t, m.msgQueue, "idle submit never queues")
}
```

- [ ] **Step 3: 运行测试确认失败**

Run:
```sh
go test ./internal/cli/tui -run "TestModel_QueueEnqueue|TestModel_QueueSubmit_IdlePathUntouched" -v
```
Expected: FAIL（`submit()` 仍在 turn 中 return nil 丢弃，`msgQueue` 为空；`enqueue`/`dispatchSend` 未定义）。

- [ ] **Step 4: 新建 `queue.go`**

新建 `internal/cli/tui/queue.go`：
```go
package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// C07 — message queue dispatch. This file holds the enqueue/drain core that
// turns the previously-stubbed model.msgQueue + model.queueMode into working
// behavior. The queue is PURE CLIENT STATE: the server only sees a sequence of
// user_message turns, so all three strategies are implemented in the TUI.
//
// submit() routes a mid-turn submission to enqueue (see model.go); Update's
// streamMsg case calls drainQueue on a "done" event. dispatchSend is the shared
// "start one user turn" kernel reused by the manual idle path and the drain.

// dispatchSend is the shared "send text as a new user turn" core used by both
// the manual submit path and the queue drain. It appends a userEntry, resets
// pending assistant text, starts the activity status line, opens the stream,
// and arms waitForEvent + the glyph animation tick. Factored out so manual and
// drained turns share identical plumbing (DRY).
func (m model) dispatchSend(text string, pasted bool) (model, tea.Cmd) {
	m.entries = append(m.entries, &userEntry{text: text, pasteID: pasteID(text), pasted: pasted})
	m.pending = ""
	m.startTurn()
	m.streamCh = m.sess.Send(text)
	m.reflow()
	m.viewport.GotoBottom()
	return m, tea.Batch(m.waitForEvent(), activityTick())
}

// enqueue stashes text in the message queue while a turn is in flight and applies
// the active queue mode's mid-turn behavior. The queue drains when the turn ends
// (see drainQueue, hooked in Update's streamMsg case on a "done" event).
//
//   - queue / batch: buffer the message; the user's follow-up runs after the
//     current turn ends (queue: one per turn in order; batch: all merged).
//   - single: buffer the message AND cancel the running turn so the follow-up is
//     handled immediately (interrupt semantics). The cancel-induced done drains
//     the queue, which for single sends only the LATEST buffered message.
//
// The input box is cleared (the user's keystroke was consumed) and the transcript
// queue marker is synced so the buffer depth is visible in-transcript too.
func (m model) enqueue(text string) (tea.Model, tea.Cmd) {
	m.msgQueue = append(m.msgQueue, text)
	m.input.Reset()
	m.paletteItems = nil
	m.growInput()
	m.syncQueueEntry()
	if m.queueMode == QueueModeSingle && !m.canceling {
		_ = m.sess.CancelCurrent()
		m.canceling = true
	}
	m.reflow()
	m.viewport.GotoBottom()
	return m, nil
}

// drainQueue sends queued messages when a turn ends, per the active queue mode:
//
//   - queue: send the HEAD only (FIFO). Each sent message starts its own turn,
//     whose done drains the next — so the queue unwinds one turn at a time.
//   - batch: join ALL queued messages with a blank line and send as ONE turn.
//   - single: send only the LAST queued message (earlier ones are discarded);
//     this is the message that interrupted the turn at enqueue time.
//
// Returns a nil Cmd when the queue is empty (nothing to drain). On a real drain
// the returned Cmd (from dispatchSend) arms waitForEvent + activityTick for the
// next turn; Update uses the non-nil Cmd to skip its own waitForEvent arming so
// the stream is not read twice.
func (m model) drainQueue() (model, tea.Cmd) {
	if len(m.msgQueue) == 0 {
		return m, nil
	}
	var text string
	switch m.queueMode {
	case QueueModeBatch:
		text = strings.Join(m.msgQueue, "\n\n")
		m.msgQueue = nil
	case QueueModeSingle:
		text = m.msgQueue[len(m.msgQueue)-1]
		m.msgQueue = nil
	default: // QueueModeQueue
		text = m.msgQueue[0]
		m.msgQueue = m.msgQueue[1:]
	}
	m.syncQueueEntry()
	return m.dispatchSend(text, false)
}

// syncQueueEntry keeps the transcript's queue marker in sync with m.msgQueue:
// update the most recent queueEntry's count when messages are buffered, create
// one if none exists, and remove it once the queue has drained. The marker may
// sit under turn output that arrived after the user enqueued, so the search
// starts from the end of the transcript. The always-visible count also lives in
// the footer (statusHeader); this marker gives per-enqueue in-transcript feedback.
func (m *model) syncQueueEntry() {
	for i := len(m.entries) - 1; i >= 0; i-- {
		if qe, ok := m.entries[i].(*queueEntry); ok {
			if len(m.msgQueue) == 0 {
				m.entries = append(m.entries[:i], m.entries[i+1:]...)
			} else {
				qe.count = len(m.msgQueue)
			}
			return
		}
	}
	if len(m.msgQueue) > 0 {
		m.entries = append(m.entries, &queueEntry{count: len(m.msgQueue)})
	}
}
```

- [ ] **Step 5: 重构 `model.go` 的 `submit()`**

用下面版本**整体替换** `submit()`（约 642-672 行）：
```go
// submit handles an Enter-keyed input. An empty input is a no-op. A turn in
// flight routes to enqueue (C07): plain messages buffer per queueMode (single
// also cancels the running turn), and slash commands stay no-ops because control
// frames would clobber the in-flight streamCh via sendControlFrame. When idle,
// slash commands run as control frames and plain text starts a user turn via the
// shared dispatchSend kernel.
func (m model) submit() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return m, nil
	}
	if m.streamCh != nil {
		if strings.HasPrefix(text, "/") {
			return m, nil
		}
		return m.enqueue(text)
	}
	m.input.Reset()
	m.paletteItems = nil
	m.growInput()
	if strings.HasPrefix(text, "/") {
		return m.runCommand(text)
	}
	pasted := m.inputPasted
	m.inputPasted = false
	mm, cmd := m.dispatchSend(text, pasted)
	return mm, cmd
}
```

- [ ] **Step 6: 运行测试确认通过**

Run:
```sh
go test ./internal/cli/tui -run "TestModel_QueueEnqueue|TestModel_QueueSubmit_IdlePathUntouched" -v
go test ./internal/cli/tui -run "TestModel_CommandRoutesSlash|TestModel_PlainTextRoutesToUserMessage|TestModel_PermissionPrompt" -v
```
Expected: 新测试 PASS；既有 routing/permission 回归测试 PASS（空闲路径字节不变）。

- [ ] **Step 7: 提交**

```sh
git add internal/cli/tui/queue.go internal/cli/tui/queue_test.go internal/cli/tui/model.go internal/cli/tui/model_test.go
git commit -m "feat(tui): enqueue mid-turn messages per queue mode (C07)"
```

---

## Task 3: done 时 drain —— 接通三种策略的出队

**Files:**
- Modify: `internal/cli/tui/model.go`（`Update` 的 `streamMsg` 分支，约 411-420 行）
- Test: `internal/cli/tui/queue_test.go`（追加）

> `drainQueue` 已在 Task 2 的 `queue.go` 实现。本任务只需在 `Update` 处理 `done` 事件后调用它，并避免与默认的 `waitForEvent` 装载重复读流。

- [ ] **Step 1: 写失败测试**

追加到 `internal/cli/tui/queue_test.go`：
```go
// ---- C07 Task 3: drain ----

// TestModel_QueueDrain_QueueSendsHeadOnly verifies queue mode drains the FIFO
// head only; the tail stays queued for the next turn's done.
func TestModel_QueueDrain_QueueSendsHeadOnly(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.msgQueue = []string{"first", "second"}
	m.queueMode = QueueModeQueue

	mm, _ := m.drainQueue()
	m = mm.(model)

	require.Equal(t, []string{"first"}, rec.sentText)
	assert.Equal(t, []string{"second"}, m.msgQueue, "tail remains for the next drain")
}

// TestModel_QueueDrain_BatchMergesAll verifies batch mode joins every queued
// message (blank-line separated) into one turn.
func TestModel_QueueDrain_BatchMergesAll(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.msgQueue = []string{"a", "b", "c"}
	m.queueMode = QueueModeBatch

	mm, _ := m.drainQueue()
	m = mm.(model)

	require.Equal(t, []string{"a\n\nb\n\nc"}, rec.sentText)
	assert.Empty(t, m.msgQueue)
}

// TestModel_QueueDrain_SingleSendsLatest verifies single mode sends only the
// most recent queued message (interrupt semantics: earlier ones are discarded).
func TestModel_QueueDrain_SingleSendsLatest(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.msgQueue = []string{"first", "second"}
	m.queueMode = QueueModeSingle

	mm, _ := m.drainQueue()
	m = mm.(model)

	require.Equal(t, []string{"second"}, rec.sentText)
	assert.Empty(t, m.msgQueue)
}

// TestModel_QueueDrain_EmptyNoOp verifies a drain on an empty queue is a no-op
// (no send, nil Cmd).
func TestModel_QueueDrain_EmptyNoOp(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")

	mm, cmd := m.drainQueue()
	m = mm.(model)

	assert.Empty(t, rec.sentText)
	assert.Nil(t, cmd, "empty drain returns a nil Cmd (no turn started)")
}

// TestModel_QueueDrain_OnDoneViaUpdate verifies the drain is hooked into the
// done event through Update: a turn ending with a queued batch automatically
// sends the merged message, and the default waitForEvent arming is skipped.
func TestModel_QueueDrain_OnDoneViaUpdate(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.streamCh = make(chan cli.StreamEvent) // in-flight; cleared by done
	m.msgQueue = []string{"x", "y"}
	m.queueMode = QueueModeBatch

	mm, _ := m.Update(streamMsg{ev: cli.StreamEvent{Kind: "done"}})
	m = mm.(model)

	require.Equal(t, []string{"x\n\ny"}, rec.sentText, "done must trigger the drain")
	assert.Empty(t, m.msgQueue)
}

// TestModel_QueueDrain_NoQueueDoesNotDrain verifies a normal done with an empty
// queue does not send anything (regression guard on the hook's guard clause).
func TestModel_QueueDrain_NoQueueDoesNotDrain(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.streamCh = make(chan cli.StreamEvent)

	mm, _ := m.Update(streamMsg{ev: cli.StreamEvent{Kind: "done"}})
	m = mm.(model)

	assert.Empty(t, rec.sentText, "empty queue + done must not send")
}
```

> 注：`streamMsg` 与 `cli.StreamEvent` 在 `model_test.go` 同包内已可用（见既有用法）。`queue_test.go` 需补 `import "github.com/x6nux/autocode/internal/cli"`——见 Step 3 的完整 import 块。

- [ ] **Step 2: 运行测试确认失败**

`drainQueue` 直接调用类测试（`TestModel_QueueDrain_QueueSendsHeadOnly` 等）在 Task 2 已随 `queue.go` PASS。`TestModel_QueueDrain_OnDoneViaUpdate` / `TestModel_QueueDrain_NoQueueDoesNotDrain` 会 FAIL——当前 `Update` 的 `streamMsg` 分支没有 done→drain 钩子。

Run:
```sh
go test ./internal/cli/tui -run "TestModel_QueueDrain_OnDoneViaUpdate|TestModel_QueueDrain_NoQueueDoesNotDrain" -v
```
Expected: 上述两个 FAIL（`rec.sentText` 为空）。

- [ ] **Step 3: 修改 `model.go` 的 `Update` streamMsg 分支**

用下面版本**整体替换** `streamMsg` case（约 411-420 行）。补 `cli` import（`model.go` 已 import `internal/cli`，无需新增）：
```go
	case streamMsg:
		m = m.applyEvent(msg.ev)
		var cmds []tea.Cmd
		// C07: when a turn ends with queued messages waiting, drain per
		// queueMode. drainQueue's Cmd (from dispatchSend) arms waitForEvent +
		// activityTick for the next turn, so on a real drain we skip the
		// default waitForEvent arming below to avoid reading the stream twice.
		drained := false
		if msg.ev.Kind == "done" && len(m.msgQueue) > 0 {
			dm, dcmd := m.drainQueue()
			m = dm
			if dcmd != nil {
				cmds = append(cmds, dcmd)
				drained = true
			}
		}
		if !drained && m.streamCh != nil {
			cmds = append(cmds, m.waitForEvent())
		}
		if m.anyToolRunning() {
			cmds = append(cmds, m.spinner.Tick)
		}
		return m, tea.Batch(cmds...)
```

`queue_test.go` 顶部 import 块应为（Step 1 追加后需要 `cli`）：
```go
import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/autocode/internal/cli"
)
```

- [ ] **Step 4: 运行测试确认通过**

Run:
```sh
go test ./internal/cli/tui -run "TestModel_QueueDrain" -v
go test ./internal/cli/tui -v
```
Expected: 所有 `TestModel_QueueDrain_*` PASS；既有 TUI 测试全 PASS（回归）。

- [ ] **Step 5: 提交**

```sh
git add internal/cli/tui/model.go internal/cli/tui/queue_test.go
git commit -m "feat(tui): drain queued messages on turn-done per queue mode (C07)"
```

---

## Task 4: footer 队列状态段（始终可见）

**Files:**
- Modify: `internal/cli/tui/view.go`（`statusHeader` 约 478-537 行）
- Test: `internal/cli/tui/queue_test.go`（追加）

> 验收"队列状态始终可见"：在持久 footer 加一段 `queue:<mode>`（+ `·N` 计数）。默认空闲态（queue 模式 + 空队列）隐藏以免噪声；只要 mode 非 queue 或队列非空就显示，让用户随时知道当前策略与积压。

- [ ] **Step 1: 写失败测试**

追加到 `internal/cli/tui/queue_test.go`：
```go
// ---- C07 Task 4: footer queue segment ----

// TestModel_QueueFooter_SegmentVisibility verifies the footer queue segment
// appears when the mode is non-default OR the queue is non-empty, and is hidden
// in the default idle state (queue mode, empty).
func TestModel_QueueFooter_SegmentVisibility(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	// Default: queue mode, empty → hidden.
	assert.NotContains(t, m.statusHeader(), "queue:", "default idle state hides the segment")

	// Non-default mode, empty → shown so the user sees their active mode.
	m.queueMode = QueueModeSingle
	assert.Contains(t, m.statusHeader(), "queue:single")

	// Default mode, non-empty queue → shown with count.
	m.queueMode = QueueModeQueue
	m.msgQueue = []string{"a", "b"}
	hdr := m.statusHeader()
	assert.Contains(t, hdr, "queue:queue")
	assert.Contains(t, hdr, "·2")
}

// TestModel_QueueFooter_CountUpdates verifies the footer count tracks the queue
// as it grows and shrinks.
func TestModel_QueueFooter_CountUpdates(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.queueMode = QueueModeQueue
	m.msgQueue = []string{"a"}
	assert.Contains(t, m.statusHeader(), "·1")
	m.msgQueue = append(m.msgQueue, "b", "c")
	assert.Contains(t, m.statusHeader(), "·3")
	m.msgQueue = nil
	assert.NotContains(t, m.statusHeader(), "queue:", "drained queue hides the segment")
}
```

- [ ] **Step 2: 运行测试确认失败**

Run:
```sh
go test ./internal/cli/tui -run "TestModel_QueueFooter" -v
```
Expected: FAIL（`statusHeader` 当前不含 `queue:` 段）。

- [ ] **Step 3: 修改 `view.go` 的 `statusHeader`**

在 `statusHeader`（约 478-537 行）的 `if m.toolsRun > 0 { ... }` 块**之后**、`return strings.Join(parts, sep)` **之前**插入：
```go
	// Queue-mode segment (C07): shown whenever the mode is non-default or the
	// queue holds messages, so the user always knows the active strategy and any
	// backlog. Hidden in the default idle state (queue mode, empty) to avoid
	// footer noise. Reuses queueStyle (amber) to match the transcript marker.
	if len(m.msgQueue) > 0 || m.queueMode != QueueModeQueue {
		seg := "queue:" + m.queueMode.String()
		if n := len(m.msgQueue); n > 0 {
			seg += fmt.Sprintf("·%d", n)
		}
		parts = append(parts, queueStyle.Render(seg))
	}
```

- [ ] **Step 4: 运行测试确认通过**

Run:
```sh
go test ./internal/cli/tui -run "TestModel_QueueFooter" -v
go test ./internal/cli/tui -run "TestModel_StatusUpdatesHeader" -v
```
Expected: 新测试 PASS；既有 footer/header 测试 PASS（默认态不显示，回归）。

- [ ] **Step 5: 提交**

```sh
git add internal/cli/tui/view.go internal/cli/tui/queue_test.go
git commit -m "feat(tui): always-visible queue-mode footer segment (C07)"
```

---

## Task 5: 断线/恢复一致性 + 全量回归

**Files:**
- Test: `internal/cli/tui/queue_test.go`（追加）
- 无产品代码改动；跑全量测试 + vet + 构建。

> 排队是 TUI 进程内内存状态。`waitForEvent`（model.go:679-685）在流 channel 关闭时返回**合成 done**——这正是断线时 WS read loop 关闭 `cur` channel 的效果。因此断线→合成 done→`Update` 的 done→drain 钩子自然触发，队列在**同一 TUI 进程**内保持一致（多窗口自愈复用客户端进程）。本任务用合成 done 覆盖该路径，并跑全量门禁。

**假设（v1 边界）：** 队列不跨**完整进程重启**持久化（不写入 SQLite/session 状态）——这超出"改动应较小"的范围。若未来需要跨重启恢复，应把 `msgQueue` + `queueMode` 纳入 session 持久化（依赖 V14 公共 Agent API）。

- [ ] **Step 1: 写断线 drain 测试**

追加到 `internal/cli/tui/queue_test.go`：
```go
// ---- C07 Task 5: disconnect/recovery consistency ----

// TestModel_QueueSurvives_DisconnectDone verifies the queue drains when a turn
// ends because the stream closed (a disconnect makes waitForEvent return a
// synthetic "done"). The queue is in-memory model state, so it is consistent
// across reconnect (same TUI process) without explicit persistence.
func TestModel_QueueSurvives_DisconnectDone(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.streamCh = make(chan cli.StreamEvent) // in-flight; a closed channel yields a synthetic done
	m.msgQueue = []string{"queued during disconnect"}
	m.queueMode = QueueModeQueue

	mm, _ := m.Update(streamMsg{ev: cli.StreamEvent{Kind: "done"}})
	m = mm.(model)

	require.Equal(t, []string{"queued during disconnect"}, rec.sentText,
		"queue must drain on the disconnect-induced done")
	assert.Empty(t, m.msgQueue)
}

// TestModel_QueueSurvives_FifoChainAcrossDisconnects verifies that in queue mode
// repeated done events (e.g. a flaky connection producing several synthetic
// dones) unwind the FIFO queue one message per turn, never dropping or reordering.
func TestModel_QueueSurvives_FifoChainAcrossDisconnects(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.streamCh = make(chan cli.StreamEvent)
	m.msgQueue = []string{"one", "two", "three"}
	m.queueMode = QueueModeQueue

	for range []int{0, 1, 2} {
		m.streamCh = make(chan cli.StreamEvent) // each drained turn is "in flight" then done
		mm, _ := m.Update(streamMsg{ev: cli.StreamEvent{Kind: "done"}})
		m = mm.(model)
	}

	assert.Equal(t, []string{"one", "two", "three"}, rec.sentText, "FIFO order preserved across the chain")
	assert.Empty(t, m.msgQueue)
}
```

> 第二个测试说明：queue 模式下 drainQueue 只发 head 并启动一个新 turn；循环里在每轮前重置 `streamCh` 模拟"drain 起的新 turn 也走完其 done"，从而把 3 条消息按 FIFO 各发一次。

- [ ] **Step 2: 运行确认通过**

Run:
```sh
go test ./internal/cli/tui -run "TestModel_QueueSurvives" -v
```
Expected: PASS（done→drain 钩子在 Task 3 已接通；本测试覆盖合成 done 与 FIFO 链）。

- [ ] **Step 3: 全量 TUI 测试**

Run:
```sh
go test ./internal/cli/tui -v
```
Expected: 全 PASS。

- [ ] **Step 4: 全量测试 + vet**

Run:
```sh
go test ./...
go vet ./...
```
Expected: 全 PASS（允许 CLAUDE.md 记载的预期 `t.Skip`：`e2e_real` 门禁、部分 eino/bootstrap 测试在 openai provider 不可用时 skip）；vet 无输出。

- [ ] **Step 5: 构建 + 冒烟**

Run:
```sh
go build -o autocode ./cmd/autocode
timeout 5 ./autocode --fake-model -inprocess -h
```
Expected: 构建成功；`-h` 打印用法并退出 0（确认改动没破坏启动）。

- [ ] **Step 6: 提交（若前序步骤有未提交的小修）**

```sh
git add -A
git commit -m "test(tui): C07 disconnect/recovery + regression green" || echo "nothing to commit"
```

---

## Self-Review（写完后自查结果）

1. **Spec 覆盖**（对照验收）：
   - 「`/queue-mode` 可循环切换 queue/batch/single」→ Task 1 注册 + no-arg `nextQueueMode` 循环 + 显式设值 ✅
   - 「queue 模式排队顺序执行」→ `drainQueue` FIFO 发 head（Task 3）+ enqueue 缓冲（Task 2）✅
   - 「batch 模式合并多条消息为一个 turn」→ `drainQueue` `strings.Join(msgQueue, "\n\n")`（Task 3）✅
   - 「single 模式新消息取消在跑的 turn」→ `enqueue` 在 `QueueModeSingle` 时 `CancelCurrent()`（Task 2）；drain 只发最新（Task 3）✅
   - 「断线/恢复后队列状态一致」→ Task 5 合成 done 触发 drain（队列内存态，同进程一致）+ 假设说明 ✅
   - 「三种策略各有测试」→ `TestModel_QueueEnqueue_*`（Task 2）+ `TestModel_QueueDrain_QueueSendsHeadOnly`/`BatchMergesAll`/`SingleSendsLatest`（Task 3）+ `TestModel_QueueMode_*`（Task 1）✅
   - 「队列状态始终可见」→ footer `queue:<mode>·N` 段（Task 4）✅
   - 覆盖完整。

2. **Placeholder 扫描**：无 TODO/TBD；所有步骤含完整可编译代码与确切命令。Task 3 Step 1 的 `queue_test.go` import 在 Step 3 显式给出（补 `cli`）。无"类似 Task N"省略。

3. **类型一致性**：`QueueMode`/`QueueModeQueue|Single|Batch`/`parseQueueMode`/`String()`（既有，model.go:177-207）；`nextQueueMode`/`queueModeHelp`/`queueModeDesc`（Task 1 新增，commands.go）；`dispatchSend`/`enqueue`/`drainQueue`/`syncQueueEntry`（Task 2 新增，queue.go）；`*queueEntry`/`queueEntry.count`（Task 1 改指针，Task 2 用）；`recordingSession.canceled`（Task 1/2 加）。命名跨任务一致 ✅。`submit()` 空闲路径经 `dispatchSend` 与 drain 共用同一内核 ✅。

4. **回归约束**：空闲提交字节不变（`TestModel_QueueSubmit_IdlePathUntouched` + 既有 routing 测试）；斜杠命令 turn 中仍 no-op（`TestModel_QueueEnqueue_SlashCommandDropped`）；`done` 空队列不发（`TestModel_QueueDrain_NoQueueDoesNotDrain`）；footer 默认态不显示（`TestModel_QueueFooter_SegmentVisibility`）✅。

5. **已知限制（非 placeholder）**：队列不跨完整进程重启持久化（v1 内存态，Task 5 假设说明）；`model.autoProcessing` 字段为既有未用代码，本 plan 不触碰（最小改动）；single 模式 cancel 依赖后端把取消ACK 为 done（WS/SSE 均如此，已由既有 `CancelCurrent` 路径保证）。

## 执行交接

Plan complete and saved to `docs/superpowers/plans/2026-07-18-m1-lane4-c07-queue-mode.md`. 两种执行方式：

1. **Subagent-Driven（推荐）** — 每个任务派一个新 subagent，任务间 review。
2. **Inline Execution** — 本会话内按 executing-plans 批次执行 + checkpoint。
