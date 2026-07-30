# V12: 无头 `autocode exec` 子命令 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 新增无头 `autocode exec` 子命令：用 `-p` flag 或 stdin 给一个 prompt，跑一个 turn，把模型输出按 `text` 或 `jsonl` 渲染到 stdout，进度/错误到 stderr，返回稳定退出码（0 成功 / 1 运行错误 / 2 用法错误 / 124 超时 / 130 取消），支持 SIGINT 取消、`--timeout`、`--resume <id>` 恢复历史 session，退出时打印 session-id 供下次 resume，无 API key 时可用 `--fake-model`。

**Architecture:** 以 `internal/cli/exec.go` 为核心驱动器，把现有 `runHeadless`（`internal/cli/cli.go`，仅作集成测试接缝）的 resolve→send→drain 模式推广为正式的、可测的 `Exec`：`Exec` 复用 `NewSession`/`Resolve` 解析后端，再委托给可注入 fake backend 的 `execWithBackend` 核心函数；后者先（可选）发 `restore_session` 帧、再发 user_message，按 `ExecOutput`（text|jsonl）渲染每一个 `StreamEvent`。退出码、flag 解析、stdin 读取、ctx+timeout 组合、session-id 打印放在 `cmd/autocode/main.go`（package main，`os.Exit` 的唯一合法位置），拆出 `parseExecArgs` 与 `mapExecError` 两个纯函数便于单测。为让 exec 能打印**新** session 的 id 供下次 resume，需要一处服务端微改：WS 的 `statusFrame` 填充已有的 `SessionID` 字段（`ServerFrame.SessionID`、`StreamEvent.SessionID`、`toStreamEvent` 映射均已存在并服务于 `session_restored`，唯独 turn 后的 `statusFrame` 没赋值）。

**Tech Stack:** Go stdlib（`flag`/`context`/`os`/`os/signal`/`errors`/`io`/`encoding/json`/`syscall`/`time`/`fmt`）；现有 `internal/cli`（`Options`/`Session`/`NewSession`/`ChatBackend`/`StreamEvent`）、`internal/proto`（`NewRestoreSession`/`NewUserMessage`）、`internal/api/http`（`ws.go` 的 `statusFrame`）。

**不变性（回归约束）：** 既有的 WS/SSE/TUI 行为字节不变——唯一的服务端改动是在 `statusFrame` 给一个**已存在**的 `omitempty` 字段赋值（store 为 nil 时仍是 ""，序列化时仍被 omit）。

---

## File Structure

- **Modify** `internal/api/http/ws.go` — `statusFrame`（约 180-190 行）加一行 `st.SessionID = cs.sessionID`，让 turn 后的 status 帧带上 server 端 session-id（exec 打印它供 resume 的唯一来源）。
- **Create** `internal/api/http/ws_sessionid_test.go` — http 包级断言：带 store 的 WS server 跑一个 turn 后，status 帧的 `SessionID != ""`。
- **Create** `internal/cli/exec.go` — `ExecOutputFormat`（`text`/`jsonl` 常量）、`ExecOptions`、`ExecResult`、`Exec`（解析 session 的薄封装）、`execWithBackend`（可注入 backend 的可测核心）、`renderExecEvent`/`writeJSONL`/`execEventText` 辅助。
- **Create** `internal/cli/exec_test.go` — 用 fake `ChatBackend` 单测 text/jsonl 渲染、resume 帧序、server-error 与 ctx-cancel 的错误传播，外加一个走真实 in-process backend 的集成测试。
- **Modify** `cmd/autocode/main.go` — 退出码常量、`mapExecError`、`parseExecArgs`/`execConfig`、`exec(args)`，在 `switch os.Args[1]` 加 `case "exec"`，更新 usage 横幅。
- **Create** `cmd/autocode/exec_test.go` — `mapExecError` 与 `parseExecArgs` 的单测（纯函数，不触发 `os.Exit`）。

---

## Task 1: 服务端 —— WS status 帧携带 session-id（resume 的前提）

**Files:**
- Modify: `internal/api/http/ws.go:177-190`（`statusFrame`）
- Create: `internal/api/http/ws_sessionid_test.go`

> exec 需要“打印新 session 的 id 供下次 resume”。session-id 是 server 端 lazy 创建的（`connSession.ensureSession`，ws.go:214，在 `user_message` 分支 ws.go:625 被调用），目前**只**在 `session_restored` 帧里回传给客户端。turn 后必然会发一个 `status` 帧（ws.go:924），所以最小、向后兼容的做法是让 `statusFrame` 给已存在的 `ServerFrame.SessionID` 字段赋值——`StreamEvent.SessionID` 与 `toStreamEvent` 映射早已就位（服务于 `session_restored`），客户端零改动即可读到。

- [ ] **Step 1: 写失败测试**

`internal/api/http/ws_sessionid_test.go`:
```go
package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/autocode/internal/agent/orchestrator"
	einollm "github.com/x6nux/autocode/internal/llm/eino"
	"github.com/x6nux/autocode/internal/proto"
	"github.com/x6nux/autocode/internal/store"
)

// TestChatWS_StatusFrameCarriesSessionID proves the WS handler surfaces the
// server-side session id on the status frame emitted after a turn, so a headless
// client (`autocode exec`) can print it for the next --resume. Requires a
// non-nil store so ensureSession creates a real session row; with store == nil
// the id stays empty and resume is unavailable (matching today's behavior).
func TestChatWS_StatusFrameCarriesSessionID(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)

	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"hi"}, nil)})
	require.NoError(t, err)
	s := New(Config{Token: "t", Store: st})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c, _, err := websocket.DefaultDialer.DialContext(context.Background(),
		"ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws", nil)
	require.NoError(t, err)
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("hello")))

	var statusID string
	// Read frames until done. A no-tool-call fake model yields agent_chunk,
	// status, done. The status frame is what exec reads for the session id.
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, data, err := c.ReadMessage()
		require.NoError(t, err)
		var f proto.ServerFrame
		require.NoError(t, json.Unmarshal(data, &f))
		if f.Type == "status" {
			statusID = f.SessionID
		}
		if f.Type == "done" {
			break
		}
	}
	assert.NotEmpty(t, statusID, "status frame after a turn must carry the session id for --resume")
}
```

- [ ] **Step 2: 运行确认失败**

Run:
```sh
go test ./internal/api/http -run TestChatWS_StatusFrameCarriesSessionID -v
```
Expected: FAIL（`statusID` 为空，断言 `NotEmpty` 不通过）。

- [ ] **Step 3: 修改 `ws.go` 的 `statusFrame`**

把 `internal/api/http/ws.go` 的 `statusFrame`（177-190 行）整体替换为：
```go
// statusFrame snapshots the session as a status ServerFrame, including the
// compaction context-window budget so the client can render "ctx: <in>/<window>"
// and the permission mode so the footer reflects it. It also carries the
// server-side session id so a headless client (autocode exec) can print it for
// --resume: SessionID is created lazily on the first user_message (ensureSession)
// and was previously only surfaced on session_restored.
func (cs *connSession) statusFrame(s *Server) proto.ServerFrame {
	mode, auto := cs.perm.get()
	st := proto.NewStatusWithMode(cs.displayModel(), cs.thinking, cs.tokensIn, cs.tokensOut, cs.turns,
		contextWindowFor(cs.model, s.compaction), string(mode), auto)
	// CachedTokens / ReasoningTokens are populated after construction so
	// NewStatusWithMode's signature (and its many callers) stay unchanged. The
	// omitempty JSON tag drops them when zero (pre-record / non-reporting model).
	st.CachedTokens = cs.cachedTokens
	st.ReasoningTokens = cs.reasoningTokens
	// SessionID: empty when recording is disabled (store == nil) or before the
	// first user_message; omitempty drops it on the wire so legacy clients and
	// the no-store path are unchanged. Headless exec reads it to print the id.
	st.SessionID = cs.sessionID
	return st
}
```

- [ ] **Step 4: 运行确认通过**

Run:
```sh
go test ./internal/api/http -run TestChatWS_StatusFrameCarriesSessionID -v
go test ./internal/api/http -v
```
Expected: 新测试 PASS；既有 WS 测试全 PASS（`SessionID` 是 `omitempty`，store 为 nil 或测试不读该字段时序列化不变）。

- [ ] **Step 5: 提交**

```sh
git add internal/api/http/ws.go internal/api/http/ws_sessionid_test.go
git commit -m "feat(http): surface session id on WS status frame (V12 exec)"
```

---

## Task 2: 客户端 —— `internal/cli/exec.go` 驱动器

**Files:**
- Create: `internal/cli/exec.go`
- Create: `internal/cli/exec_test.go`

> 推广 `runHeadless`（cli.go:20，仅测试用）为正式驱动器。沿用其与 `runHeadlessWithFrames`（cli.go:64）相同的“解析 vs. 可注入核心”切分：`Exec` 解析 session、`execWithBackend` 接受一个 `ChatBackend` 并可被 fake 注入（fake 优先，遵循 CLAUDE.md）。渲染按 `ExecOutput` 分流：text 把 agent_chunk 写 stdout、tool/error 写 stderr 单行（沿用 chatLegacy 在 main.go:291 的字形），jsonl 把**每个** `StreamEvent` `json.Marshal` 成一行写 stdout。ctx 取消/超时由底层 `wsBackend.Send` 已有的 innerCtx 机制落到 event 边界，`execWithBackend` 在 drain 结束后返回 `ctx.Err()`（取消/超时优先于 server error 帧）。

- [ ] **Step 1: 写失败测试**

`internal/cli/exec_test.go`:
```go
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/autocode/internal/proto"
)

// fakeExecBackend is a deterministic ChatBackend for driver unit tests. Send
// streams a scripted turn (agent_chunk + tool_call + tool_result + status +
// done); SendFrame("restore_session") replies with session_restored. It records
// the Type (and payload) of every call so tests can assert ordering.
type fakeExecBackend struct {
	mode     string
	sendText string
	statusID string // SessionID attached to the status event on Send

	mu     sync.Mutex
	frames []string // recorded "<type>:<payload>" for each Send/SendFrame
}

func (f *fakeExecBackend) Send(_ context.Context, text string) (<-chan StreamEvent, error) {
	f.mu.Lock()
	f.frames = append(f.frames, "user_message:"+text)
	f.mu.Unlock()
	ch := make(chan StreamEvent, 8)
	ch <- StreamEvent{Kind: "agent_chunk", Text: f.sendText}
	ch <- StreamEvent{Kind: "tool_call", ToolName: "fs_read", ToolArgs: "{}"}
	ch <- StreamEvent{Kind: "tool_result", ToolName: "fs_read", ToolStatus: "ok"}
	ch <- StreamEvent{Kind: "status", SessionID: f.statusID}
	ch <- StreamEvent{Kind: "done"}
	close(ch)
	return ch, nil
}

func (f *fakeExecBackend) SendFrame(_ context.Context, fr proto.ClientFrame) (<-chan StreamEvent, error) {
	f.mu.Lock()
	f.frames = append(f.frames, fr.Type+":"+fr.ID)
	f.mu.Unlock()
	ch := make(chan StreamEvent, 4)
	if fr.Type == "restore_session" {
		ch <- StreamEvent{Kind: "session_restored", SessionID: fr.ID}
	}
	close(ch)
	return ch, nil
}

func (f *fakeExecBackend) Cancel() error      { return nil }
func (f *fakeExecBackend) Close() error       { return nil }
func (f *fakeExecBackend) Mode() string       { return f.mode }
func (f *fakeExecBackend) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]string, len(f.frames))
	copy(cp, f.frames)
	return cp
}

// TestExec_TextMode_RendersAgentChunkToStdout proves text mode routes the model
// stream to stdout and tool activity to stderr (stdout stays parseable).
func TestExec_TextMode_RendersAgentChunkToStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	b := &fakeExecBackend{mode: "ws", sendText: "hello world", statusID: "sess-1"}
	res, err := execWithBackend(context.Background(), b, ExecOptions{
		Prompt: "hi", Output: ExecOutputText, Stdout: &stdout, Stderr: &stderr,
	})
	require.NoError(t, err)
	assert.Equal(t, "sess-1", res.SessionID, "status event's SessionID surfaces in the result")
	assert.Contains(t, stdout.String(), "hello world", "agent_chunk text -> stdout")
	assert.NotContains(t, stdout.String(), "fs_read", "tool activity must NOT pollute stdout")
	assert.Contains(t, stderr.String(), "fs_read", "tool_call one-liner -> stderr")
	assert.Contains(t, stderr.String(), "[ok]", "tool_result status -> stderr")
}

// TestExec_JSONLMode_OneJSONLinePerEvent proves jsonl mode emits one StreamEvent
// JSON object per line on stdout (every event, including status/done), so the
// stream is machine-parseable.
func TestExec_JSONLMode_OneJSONLinePerEvent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	b := &fakeExecBackend{mode: "ws", sendText: "chunk", statusID: "sess-2"}
	_, err := execWithBackend(context.Background(), b, ExecOptions{
		Prompt: "hi", Output: ExecOutputJSONL, Stdout: &stdout, Stderr: &stderr,
	})
	require.NoError(t, err)
	assert.Empty(t, stderr.String(), "jsonl mode routes all events to stdout")

	out := strings.TrimRight(stdout.String(), "\n")
	require.NotEmpty(t, out, "jsonl mode must emit at least one event")
	lines := strings.Split(out, "\n")
	kinds := make([]string, 0, len(lines))
	for i, ln := range lines {
		var ev StreamEvent
		require.NoErrorf(t, json.Unmarshal([]byte(ln), &ev), "line %d not a StreamEvent JSON: %q", i, ln)
		kinds = append(kinds, ev.Kind)
	}
	assert.Contains(t, kinds, "agent_chunk")
	assert.Contains(t, kinds, "status")
	assert.Contains(t, kinds, "done")
}

// TestExec_ResumeSendsRestoreBeforeUserMessage proves --resume sends
// restore_session FIRST and the user turn SECOND.
func TestExec_ResumeSendsRestoreBeforeUserMessage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	b := &fakeExecBackend{mode: "ws", sendText: "ok", statusID: "sess-r"}
	res, err := execWithBackend(context.Background(), b, ExecOptions{
		Prompt: "more", Resume: "sess-r", Output: ExecOutputText, Stdout: &stdout, Stderr: &stderr,
	})
	require.NoError(t, err)
	assert.Equal(t, "sess-r", res.SessionID)
	rec := b.recorded()
	require.GreaterOrEqual(t, len(rec), 2, "resume must send restore + user_message")
	assert.Equal(t, "restore_session:sess-r", rec[0], "restore must precede the user turn")
	assert.Equal(t, "user_message:more", rec[1])
}

// TestExec_ServerErrorFrameReturnsError proves a server error frame is surfaced
// as a non-nil error (main maps it to exit 1) and rendered to stderr.
func TestExec_ServerErrorFrameReturnsError(t *testing.T) {
	b := &errExecBackend{fakeExecBackend: fakeExecBackend{mode: "ws"}}
	var stdout, stderr bytes.Buffer
	_, err := execWithBackend(context.Background(), b, ExecOptions{
		Prompt: "hi", Output: ExecOutputText, Stdout: &stdout, Stderr: &stderr,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
	assert.Contains(t, stderr.String(), "error: boom")
}

// errExecBackend is a fakeExecBackend whose Send emits a single error frame.
type errExecBackend struct{ fakeExecBackend }

func (e *errExecBackend) Send(_ context.Context, _ string) (<-chan StreamEvent, error) {
	e.mu.Lock()
	e.frames = append(e.frames, "user_message:err")
	e.mu.Unlock()
	ch := make(chan StreamEvent, 1)
	ch <- StreamEvent{Kind: "error", Text: "boom"}
	close(ch)
	return ch, nil
}

// TestExec_CancelledContextReturnsCanceled proves a cancelled ctx makes the
// driver return context.Canceled (cancel/timeout wins over a server error).
// blockExecBackend blocks its Send channel until ctx.Done, mirroring wsBackend's
// ctx wiring (which closes the channel on cancel).
type blockExecBackend struct{ fakeExecBackend }

func (b *blockExecBackend) Send(ctx context.Context, text string) (<-chan StreamEvent, error) {
	b.mu.Lock()
	b.frames = append(b.frames, "user_message:"+text)
	b.mu.Unlock()
	ch := make(chan StreamEvent)
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}

func TestExec_CancelledContextReturnsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel: the drain sees an empty, closed channel
	b := &blockExecBackend{fakeExecBackend: fakeExecBackend{mode: "ws"}}
	var stdout, stderr bytes.Buffer
	_, err := execWithBackend(ctx, b, ExecOptions{
		Prompt: "hi", Output: ExecOutputText, Stdout: &stdout, Stderr: &stderr,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestExec_InProcessFakeModelTurn is the composition proof: the real Exec path
// (bootstrap.Build via Resolve, in-process store, bootstrap fake model) runs one
// turn and surfaces a non-empty session id. Depends on Task 1's statusFrame change.
func TestExec_InProcessFakeModelTurn(t *testing.T) {
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	res, err := Exec(context.Background(), ExecOptions{
		Options: Options{Root: root, ConfigPath: writeTestConfig(t, root), FakeModel: true},
		Prompt:  "hello",
		Output:  ExecOutputText,
		Stdout:  &stdout,
		Stderr:  &stderr,
	})
	require.NoError(t, err)
	// The bootstrap fake model (bootstrap.go:114) emits this exact text + task_end.
	assert.Contains(t, stdout.String(), "(no real model configured)")
	assert.NotEmpty(t, res.SessionID, "store-backed in-process backend must surface a session id")
}
```

- [ ] **Step 2: 运行确认失败**

Run:
```sh
go test ./internal/cli -run "TestExec_" -v
```
Expected: 编译失败（`ExecOptions`/`ExecOutputText`/`execWithBackend`/`Exec` 未定义）。

- [ ] **Step 3: 实现 `exec.go`**

`internal/cli/exec.go`:
```go
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/x6nux/autocode/internal/proto"
)

// ExecOutputFormat selects how Exec renders the event stream.
type ExecOutputFormat string

const (
	// ExecOutputText renders agent_chunk text to stdout and tool/error activity
	// to stderr one-liners — the default, for humans.
	ExecOutputText ExecOutputFormat = "text"
	// ExecOutputJSONL emits every observed StreamEvent as one JSON object per
	// line on stdout — for programmatic consumers (pipes, tests). Every event,
	// including status/done/error, is one line.
	ExecOutputJSONL ExecOutputFormat = "jsonl"
)

// ExecOptions configures the headless Exec driver. Prompt is the user turn
// (required; cmd/autocode reads stdin when the -p flag is empty). Output selects
// the render format. Resume, when non-empty, restores a prior session before the
// turn (WebSocket transport only — SSE is stateless). Stdout/Stderr default to
// io.Discard so the driver is safe without a terminal; cmd/autocode passes
// os.Stdout/os.Stderr.
type ExecOptions struct {
	Options          // session resolution (Root, ConfigPath, FakeModel, Server, InProcess)
	Prompt  string // the user turn text (required)
	Output  ExecOutputFormat
	Resume  string // optional session id to restore before the turn ("" = fresh)
	Stdout  io.Writer
	Stderr  io.Writer
}

// ExecResult carries the session id the server assigned (new turn) or resumed,
// so cmd/autocode can print it for the next --resume.
type ExecResult struct {
	SessionID string
}

// Exec resolves a backend and runs one headless turn. It is the thin resolver
// wrapper around execWithBackend (the testable core) — split the same way
// runHeadless wraps runHeadlessWithFrames so the turn-driving logic can be
// exercised against a fake ChatBackend without bootstrapping.
func Exec(ctx context.Context, opts ExecOptions) (ExecResult, error) {
	sess := NewSession(opts.Options)
	if err := sess.Resolve(ctx); err != nil {
		return ExecResult{}, err
	}
	defer sess.Close()
	return execWithBackend(ctx, sess.Backend(), opts)
}

// execWithBackend is the testable core of the headless driver: it drives an
// already-resolved backend through an optional restore_session and exactly one
// user turn, rendering events to opts.Stdout/Stderr per opts.Output.
//
// ctx drives cancellation (SIGINT) and timeout: wsBackend.Send wires ctx to its
// in-flight turn, so cancelling aborts at the next event boundary and the event
// channel closes. execWithBackend returns ctx.Err() (context.Canceled or
// context.DeadlineExceeded) when ctx is done — taking precedence over a server
// error frame — so cmd/autocode can map those to exit codes 130/124. A server
// error frame is returned as a plain error (exit 1).
func execWithBackend(ctx context.Context, b ChatBackend, opts ExecOptions) (ExecResult, error) {
	stdout, stderr := opts.Stdout, opts.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	out := opts.Output
	if out == "" {
		out = ExecOutputText
	}
	result := ExecResult{}

	// Optional resume: send restore_session before the turn and drain its reply.
	// Resume needs the WebSocket transport (SSE is stateless): SendFrame returns
	// ErrSSEControlUnsupported there, which we wrap so main reports a clear error.
	if opts.Resume != "" {
		rch, err := b.SendFrame(ctx, proto.NewRestoreSession(opts.Resume))
		if err != nil {
			return result, fmt.Errorf("resume %q: %w", opts.Resume, err)
		}
		for ev := range rch {
			// In JSONL the restore reply is a real event the consumer wants; in
			// text mode it is internal plumbing and stays silent.
			if out == ExecOutputJSONL {
				writeJSONL(stdout, stderr, ev)
			}
			if ev.Kind == "session_restored" {
				result.SessionID = ev.SessionID
			}
			if ev.Kind == "error" {
				return result, fmt.Errorf("resume %q: %s", opts.Resume, execEventText(ev))
			}
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
	}

	// Run the turn.
	ch, err := b.Send(ctx, opts.Prompt)
	if err != nil {
		return result, fmt.Errorf("send: %w", err)
	}
	var serverErr string
	for ev := range ch {
		// The turn's status frame carries the (possibly new) session id.
		if ev.Kind == "status" && ev.SessionID != "" {
			result.SessionID = ev.SessionID
		}
		renderExecEvent(stdout, stderr, out, ev)
		if ev.Kind == "error" {
			serverErr = execEventText(ev)
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err // cancel/timeout wins over a server error frame
	}
	if serverErr != "" {
		return result, fmt.Errorf("server error: %s", serverErr)
	}
	return result, nil
}

// renderExecEvent renders one turn event per the output format.
func renderExecEvent(stdout, stderr io.Writer, out ExecOutputFormat, ev StreamEvent) {
	if out == ExecOutputJSONL {
		writeJSONL(stdout, stderr, ev)
		return
	}
	// text: agent_chunk -> stdout; tool/error activity -> stderr one-liners,
	// matching the chatLegacy render style (main.go). status/done/thinking/retry
	// are silent (metadata, not turn output).
	switch ev.Kind {
	case "agent_chunk":
		fmt.Fprint(stdout, ev.Text)
	case "tool_call":
		fmt.Fprintf(stderr, "🔧 %s %s …\n", ev.ToolName, ev.ToolArgs)
	case "tool_result":
		fmt.Fprintf(stderr, "↳ %s [%s]\n", ev.ToolName, ev.ToolStatus)
	case "error":
		fmt.Fprintf(stderr, "error: %s\n", execEventText(ev))
	}
}

// writeJSONL emits one StreamEvent as a JSON object line on stdout. A marshal
// failure (unexpected; StreamEvent is plain data) is reported on stderr instead.
func writeJSONL(stdout, stderr io.Writer, ev StreamEvent) {
	line, err := json.Marshal(ev)
	if err != nil {
		fmt.Fprintf(stderr, "exec: marshal event: %v\n", err)
		return
	}
	fmt.Fprintln(stdout, string(line))
}

// execEventText returns the human-readable message for an error event: ev.Err
// when the transport set it (e.g. a dropped connection), else ev.Text (the
// server's error frame text).
func execEventText(ev StreamEvent) string {
	if ev.Err != nil {
		return ev.Err.Error()
	}
	return ev.Text
}
```

- [ ] **Step 4: 运行确认通过**

Run:
```sh
go test ./internal/cli -run "TestExec_" -v
go test ./internal/cli -v
```
Expected: 新测试全 PASS；既有 cli 测试全 PASS（`runHeadless` 与 `runHeadlessWithFrames` 未改动）。

- [ ] **Step 5: 提交**

```sh
git add internal/cli/exec.go internal/cli/exec_test.go
git commit -m "feat(cli): add headless Exec driver (V12)"
```

---

## Task 3: `cmd/autocode` —— `exec` 子命令接线 + 退出码 + usage

**Files:**
- Modify: `cmd/autocode/main.go`（switch 约 66-85、usage 约 35-54、新增 `exec` 一节）
- Create: `cmd/autocode/exec_test.go`

> main.go 是 `os.Exit` 的唯一合法位置。把 flag 解析与错误→退出码映射拆成纯函数 `parseExecArgs`/`mapExecError`（可单测，不触发 `os.Exit`）；`exec(args)` 组合 ctx（SIGINT + 可选 timeout）、读 stdin、调 `cli.Exec`、按退出码退出、打印 session-id。timeout ctx 套在 signal ctx 内层，使最内层 `ctx.Err()` 能区分 `DeadlineExceeded`（超时→124）与 `Canceled`（中断→130）。

- [ ] **Step 1: 写失败测试**

`cmd/autocode/exec_test.go`:
```go
package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMapExecError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil success", nil, exitOK},
		{"deadline -> timeout", context.DeadlineExceeded, exitTimeout},
		{"canceled -> cancel", context.Canceled, exitCancel},
		{"plain error -> err", errors.New("boom"), exitErr},
		{"wrapped deadline -> timeout", fmt.Errorf("ctx: %w", context.DeadlineExceeded), exitTimeout},
		{"wrapped canceled -> cancel", fmt.Errorf("ctx: %w", context.Canceled), exitCancel},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, mapExecError(tc.err))
		})
	}
}

func TestParseExecArgs(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := parseExecArgs(nil)
		require.NoError(t, err)
		assert.Equal(t, "text", cfg.output)
		assert.Equal(t, "config.yaml", cfg.configPath)
		assert.Equal(t, 0*time.Second, cfg.timeout)
		assert.False(t, cfg.fakeModel)
	})

	t.Run("p and prompt alias", func(t *testing.T) {
		cfg, err := parseExecArgs([]string{"-p", "hi"})
		require.NoError(t, err)
		assert.Equal(t, "hi", cfg.prompt)

		cfg, err = parseExecArgs([]string{"--prompt", "hello"})
		require.NoError(t, err)
		assert.Equal(t, "hello", cfg.prompt)
	})

	t.Run("output and timeout and resume", func(t *testing.T) {
		cfg, err := parseExecArgs([]string{"--output", "jsonl", "--timeout", "30s", "--resume", "sess-1", "--fake-model", "--inprocess"})
		require.NoError(t, err)
		assert.Equal(t, "jsonl", cfg.output)
		assert.Equal(t, 30*time.Second, cfg.timeout)
		assert.Equal(t, "sess-1", cfg.resume)
		assert.True(t, cfg.fakeModel)
		assert.True(t, cfg.inProcess)
	})

	t.Run("invalid output rejected", func(t *testing.T) {
		_, err := parseExecArgs([]string{"--output", "csv"})
		require.Error(t, err)
	})

	t.Run("unknown flag rejected", func(t *testing.T) {
		_, err := parseExecArgs([]string{"--nope"})
		require.Error(t, err)
	})
}
```

- [ ] **Step 2: 运行确认失败**

Run:
```sh
go test ./cmd/autocode -run "TestMapExecError|TestParseExecArgs" -v
```
Expected: 编译失败（`exitOK`/`exitTimeout`/`exitCancel`/`exitErr` 常量、`mapExecError`、`parseExecArgs`、`execConfig` 未定义）。

- [ ] **Step 3a: 在 main.go 加退出码常量与 `mapExecError`**

在 `cmd/autocode/main.go` import 块加 `"errors"` 与 `"time"`（两者当前都未导入——`errors` 用于 `mapExecError`/`exec`，`time` 用于 `execConfig.timeout` 的类型 `time.Duration` 与 `fs.DurationVar`）。import 块（约 10-33 行）按字母序插入后应为：
```go
import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/x6nux/autocode/internal/agent/goalloop"
	"github.com/x6nux/autocode/internal/bootstrap"
	"github.com/x6nux/autocode/internal/cli"
	"github.com/x6nux/autocode/internal/cli/tui"
	"github.com/x6nux/autocode/internal/store"
	"github.com/x6nux/autocode/internal/vcs"
	vcsmcp "github.com/x6nux/autocode/internal/vcs/mcp"
)
```

然后在文件末尾（`expandHome` 之后）新增退出码常量与 `mapExecError`：

```go
// ---------------------------------------------------------------------------
// exec subcommand (V12): headless single-turn runner
// ---------------------------------------------------------------------------

// Exit codes for the exec subcommand. They are stable so scripts can branch on
// them. 0 success, 1 runtime error, 2 usage error, 124 timeout (the conventional
// coreutils timeout code), 130 cancelled (128 + SIGINT(2)).
const (
	exitOK      = 0
	exitErr     = 1
	exitUsage   = 2
	exitTimeout = 124
	exitCancel  = 130
)

// mapExecError maps a cli.Exec error to a stable exit code. nil -> 0; ctx
// DeadlineExceeded -> 124 (timeout); ctx Canceled -> 130 (interrupt); anything
// else -> 1 (runtime error). Split out so it is unit-testable.
func mapExecError(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, context.DeadlineExceeded):
		return exitTimeout
	case errors.Is(err, context.Canceled):
		return exitCancel
	default:
		return exitErr
	}
}
```

- [ ] **Step 3b: 在 main.go 加 `parseExecArgs`/`execConfig` 与 `exec`**

紧接上方新增：
```go
// execConfig holds the parsed exec flags.
type execConfig struct {
	configPath string
	prompt     string
	output     string
	timeout    time.Duration
	resume     string
	fakeModel  bool
	server     string
	inProcess  bool
}

// parseExecArgs parses the exec subcommand flags. Split from exec so flag
// handling is unit-testable without os.Exit. -p and --prompt are aliases (both
// write to cfg.prompt); invalid --output or an unknown flag returns an error.
func parseExecArgs(args []string) (execConfig, error) {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	cfg := execConfig{}
	fs.StringVar(&cfg.configPath, "config", "config.yaml", "path to configuration file")
	fs.StringVar(&cfg.prompt, "p", "", "prompt text (reads stdin when empty)")
	fs.StringVar(&cfg.prompt, "prompt", "", "alias for -p")
	fs.StringVar(&cfg.output, "output", "text", "output format: text | jsonl")
	fs.DurationVar(&cfg.timeout, "timeout", 0, "abort after this duration (0 = no limit)")
	fs.StringVar(&cfg.resume, "resume", "", "restore session id before the turn")
	fs.BoolVar(&cfg.fakeModel, "fake-model", false, "use a deterministic fake model (no API keys needed)")
	fs.StringVar(&cfg.server, "server", "", "force connect to this server URL")
	fs.BoolVar(&cfg.inProcess, "inprocess", false, "force in-process backend")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	switch cfg.output {
	case "text", "jsonl":
	default:
		return cfg, fmt.Errorf("invalid --output %q (want text or jsonl)", cfg.output)
	}
	return cfg, nil
}

// exec is the headless entry: one turn, structured output, stable exit codes,
// resume, and timeout/cancel. It supersedes the chatLegacy line REPL (main.go),
// which is single-turn SSE only — no JSONL, no exit codes, no timeout/resume.
func exec(args []string) {
	cfg, err := parseExecArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "autocode exec: %v\n", err)
		os.Exit(exitUsage)
	}

	// Prompt: -p/--prompt flag, else read all of stdin.
	prompt := cfg.prompt
	if prompt == "" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "autocode exec: read stdin: %v\n", err)
			os.Exit(exitErr)
		}
		prompt = strings.TrimSpace(string(b))
	}
	if prompt == "" {
		fmt.Fprintln(os.Stderr, "autocode exec: prompt is required (use -p or pipe stdin)")
		os.Exit(exitUsage)
	}

	// ctx: SIGINT/SIGTERM cancellation composed with an optional timeout. The
	// innermost ctx is what cli.Exec honors, so ctx.Err() distinguishes
	// DeadlineExceeded (timeout) from Canceled (interrupt).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.timeout > 0 {
		ctx, cancel := context.WithTimeout(ctx, cfg.timeout)
		defer cancel()
	}

	result, err := cli.Exec(ctx, cli.ExecOptions{
		Options: cli.Options{
			ConfigPath: cfg.configPath, FakeModel: cfg.fakeModel,
			Server: cfg.server, InProcess: cfg.inProcess,
		},
		Prompt: prompt,
		Output: cli.ExecOutputFormat(cfg.output),
		Resume: cfg.resume,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})

	// Print the surfaced session id for the next --resume (stderr so stdout stays
	// parseable, even in JSONL mode).
	if err == nil && result.SessionID != "" {
		fmt.Fprintf(os.Stderr, "session: %s\n", result.SessionID)
	}
	// Cancel/timeout already carry their meaning via the exit code; surface the
	// message for genuine runtime errors only.
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		fmt.Fprintf(os.Stderr, "autocode exec: %v\n", err)
	}
	os.Exit(mapExecError(err))
}
```

- [ ] **Step 3c: 在 main.go 的 switch 里分发 `exec`，并更新 usage**

(a) 在 `switch os.Args[1]`（约 66-85 行）的 `case "goal":` 之前（或紧邻 `case "chat":` 之后）加一行：
```go
	case "exec":
		exec(os.Args[2:])
```

(b) 把 `usage` 字符串（约 35-54 行）在 `autocode goal ...` 那一行之后加一行 usage，并在 `Subcommands:` 段加一条。修改后 usage 的相关片段应为：
```go
var usage = `autocode ` + Version + ` — the CLI for the autocode agent server.

Usage:
  autocode                                self-contained TUI (discovers or embeds the backend)
  autocode chat    [--no-tui] [-server URL] [-inprocess] [-fake-model] [-config FILE] [-token TOKEN]
  autocode exec    [-p "prompt" | stdin] [-output text|jsonl] [-timeout 1m] [-resume ID] [-fake-model] [-inprocess] [-server URL] [-config FILE]
  autocode serve   [-config config.yaml] [-fake-model] [-addr ADDR]
  autocode goal    [-config config.yaml] [-fake-model] [-workdir DIR] [-agent claudecode] [-max-iters 5] [-goal "text"] [-tier auto|t0..t4]
  autocode vcs-mcp (env-driven; spawned by the ACP adapter — ACODE_DB_PATH/ACODE_REPO_ID/ACODE_WT_ID/ACODE_AGENT/ACODE_WORKTREE_DIR)

Subcommands:
  (none)   Launch the self-contained TUI. Discovers a running backend for the
           current project via a lockfile, or embeds one in-process. WebSocket is
           the primary transport (multi-turn, tool-aware); SSE is the fallback.
  chat     Same TUI as the bare invocation. --no-tui drops to the legacy
           line-based REPL (SSE, single-turn). -server/-inprocess force a backend.
  exec     Headless single-turn runner. Reads a prompt from -p (or stdin), runs one
           turn, prints model output to stdout (text) or one JSON object per event
           (jsonl), progress/errors to stderr. Stable exit codes: 0 ok, 1 runtime
           error, 2 usage, 124 timeout, 130 cancelled. --resume continues a prior
           session; the session id is printed on exit. --fake-model needs no API key.
  serve    Start the HTTP server as a shared daemon (SIGINT/SIGTERM to stop).
           Other autocode invocations in the same project discover it.
  goal     Run the self-driven goal loop (plan-implement-evaluate-judge).
  vcs-mcp  Run the autoVCS MCP server on stdio (spawned by the ACP adapter).
`
```

- [ ] **Step 4: 运行确认通过**

Run:
```sh
go test ./cmd/autocode -run "TestMapExecError|TestParseExecArgs" -v
go vet ./cmd/autocode
```
Expected: 新测试 PASS；vet 无输出。

- [ ] **Step 5: 提交**

```sh
git add cmd/autocode/main.go cmd/autocode/exec_test.go
git commit -m "feat(cli): wire headless exec subcommand + exit codes (V12)"
```

---

## Task 4: 全量回归 + 构建冒烟

**Files:**
- 无新增；运行全量测试、vet、构建、冒烟。

- [ ] **Step 1: 全量测试**

Run:
```sh
go test ./...
```
Expected: 全 PASS（允许 CLAUDE.md 记载的预期 `t.Skip`：`e2e_real` 门禁、部分 eino/bootstrap 测试在 openai provider 不可用时 skip）。

- [ ] **Step 2: vet**

Run:
```sh
go vet ./...
```
Expected: 无输出。

- [ ] **Step 3: 构建**

Run:
```sh
go build -o autocode ./cmd/autocode
```
Expected: 成功。

- [ ] **Step 4: text 模式冒烟（fake-model，无需 key）**

Run:
```sh
echo "what is 2+2" | ./autocode exec --fake-model -inprocess
echo "exit=$?"
```
Expected: stdout 含 `(no real model configured)`（bootstrap fake model 的固定输出）；stderr 含 `session: <id>`；`exit=0`。

- [ ] **Step 5: jsonl 模式 + 超时冒烟**

Run:
```sh
./autocode exec --fake-model -inprocess --output jsonl -p "hi" | head -n 1
./autocode exec --fake-model -inprocess --timeout 1ms -p "hi"; echo "exit=$?"
```
Expected: 第一行 stdout 是合法 JSON（含 `"Kind":"agent_chunk"` 或同类）。第二条因 1ms 超时几乎必然命中 `exit=124`（若该 turn 在 1ms 内完成则 `exit=0`，重跑即可观察到 124）。

- [ ] **Step 6: resume 往返冒烟（fake-model + 磁盘 store）**

Run:
```sh
DB=$(mktemp -d)
printf 'storage:\n  sqlite_path: "%s/db"\n' "$DB" > "$DB/config.yaml"
ID=$(./autocode exec --fake-model -inprocess -config "$DB/config.yaml" -p "remember ACME" 2>&1 >/dev/null | sed -n 's/^session: //p')
echo "first id=$ID"
./autocode exec --fake-model -inprocess -config "$DB/config.yaml" --resume "$ID" -p "what did i say" 2>&1 >/dev/null | head
echo "exit=$?"
```
Expected: 第一次打印一个非空 `session: <id>`；第二次以 `--resume <id>` 跑通（`exit=0`，不再报 "session not found"）。

- [ ] **Step 7: usage 冒烟**

Run:
```sh
./autocode -h | grep -i exec
./autocode exec --output csv -p x 2>&1; echo "exit=$?"
```
Expected: `-h` 输出含 `exec` 行；`--output csv` 因非法取值 `exit=2`。

- [ ] **Step 8: 提交（若前序步骤有未提交的小修）**

```sh
git add -A
git commit -m "test: V12 exec regression green" || echo "nothing to commit"
```

---

## Self-Review（写完后自查结果）

1. **Spec 覆盖**：验收逐条对照——「`-p "..."` 跑一个 turn，text→stdout、进度/错误→stderr」（Task 2 `renderExecEvent` + Task 3 `exec`）✅；「`--output jsonl` 每个 StreamEvent 一行 JSON」（Task 2 `writeJSONL`）✅；「稳定退出码 0/1/2/124/130」（Task 3 常量 + `mapExecError`）✅；「--timeout 到期取消并退 124」（Task 3 ctx 套层 + `mapExecError` DeadlineExceeded；Task 2 返回 `ctx.Err()`）✅；「SIGINT 取消退 130」（Task 3 `signal.NotifyContext` + `mapExecError` Canceled）✅；「`--resume <id>` 在首条 prompt 前发 restore_session」（Task 2 `execWithBackend` resume 段，测试 `TestExec_ResumeSendsRestoreBeforeUserMessage` 断言帧序）✅；「退出时打印 session-id」（Task 1 让 status 帧带 id + Task 3 `exec` 末尾打印）✅；「无 API key 用 --fake-model」（Task 3 flag + Task 2 `TestExec_InProcessFakeModelTurn`）✅；「stdin 输入」（Task 3 `exec` 读 stdin）✅。覆盖完整。
2. **Placeholder 扫描**：无 TODO/TBD/"类似上文"；每个代码步骤均给出完整可编译代码；冒烟命令均为可直接执行的具体命令。
3. **类型一致性**：`ExecOptions`/`ExecResult`/`ExecOutputFormat`/`ExecOutputText`/`ExecOutputJSONL`/`Exec`/`execWithBackend`/`renderExecEvent`/`writeJSONL`/`execEventText` 在 Task 2 定义、Task 3 消费，命名一致 ✅。`exitOK`/`exitErr`/`exitUsage`/`exitTimeout`/`exitCancel`/`mapExecError`/`parseExecArgs`/`execConfig` 在 Task 3 定义与测试一致 ✅。`StreamEvent.SessionID`（backend.go:64）、`ServerFrame.SessionID`（frame.go:173）、`toStreamEvent` 的 `SessionID: f.SessionID`（wsbackend.go:287）均为既有字段，Task 1 仅在 `statusFrame` 赋值，沿用既有链路 ✅。`StreamEvent.Kind`（非 `Type`）在测试与实现中一致 ✅（cli.go:82 用 `ev.Kind` 印证）。
4. **已知限制（非 placeholder，是 v1 边界）**：
   - **resume 仅 WS**：SSE 无状态、不支持 control frame（`ErrSSEControlUnsupported`），`--resume` 走 SSE 会以 exit 1 报错。in-process 与发现到的后端几乎总是 WS（主传输），故实际可用。
   - **store 为 nil 时无 session-id**：`statusFrame` 的 `SessionID` 为 ""，exec 不打印；`--resume` 在此情形服务端回 "session recording is disabled"。正常运行（config 有 `storage.sqlite_path`）store 非 nil，不受影响。
   - **JSONL 的 `Err` 字段**：`json.Marshal(StreamEvent)` 对非 nil 的 `Err`（error 接口）会输出 `{}`（动态具体类型无导出字段）。错误语义仍由 `Kind=="error"` 与退出码承载；若后续需要可在 StreamEvent 上加自定义 `MarshalJSON`，v1 按规格「StreamEvent 的 json.Marshal」直出。
   - **JSONL 的 session-id**：session-id 走 stderr（`session: <id>`），不混入 stdout 的 JSONL 流，保证 stdout 可逐行 `json.Unmarshal`。

## 执行交接

Plan complete and saved to `docs/superpowers/plans/2026-07-18-m1-lane1b-v12-headless-exec.md`。两种执行方式：

1. **Subagent-Driven（推荐）** — 每个任务派一个新 subagent，任务间 review。建议按 Task 1 → Task 2 → Task 3 → Task 4 顺序（Task 2 的集成测试依赖 Task 1）。
2. **Inline Execution** — 本会话内按 executing-plans 批次执行 + checkpoint。
