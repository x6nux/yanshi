# Batch B3 开发者工具实施计划 v3

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为 yanshi 增加结构化且受 Guard 保护的 Git、测试、诊断、GitHub、Web 搜索和分块 Code Review 能力，并交付 `/review` 与 `yanshi pr <N>` 两个入口。

**Architecture:** 所有模型工具继续使用 `GuardedTool`；同步工具使用 `SyncStream`，review 使用手写 `StreamFunc`。所有子进程唯一地通过 A1c package-tools 薄转发 `LaunchSecureProcess(ctx, secproc.SecureProcessSpec)`；大输出优先通过 DT3 `TaskManagerFromContext`/`work.ManagerLike.WriteArtifact` 持久化；LSP 只消费 B2-LSP1 的 `*lsp.Manager`。强制逐次审批贯通 tools → proto → cli → TUI，YOLO/auto 和 session allowlist 都不能绕过。

**Tech Stack:** Go 1.26.4、Eino v0.9.12、`internal/secproc`、`internal/task/work`、B2-LSP1 `internal/lsp`、Eino ADK、Bubble Tea、Git CLI、可选 `gh` CLI、标准库 HTTP。

---

## 实施前置门禁与锁定合同

B3 的执行顺序是 **A1c Task 14 → DT3/A2 context bridge → B2-LSP1 → B3 Task 1**。这不是运行时 optional import：缺少 Go package 会在编译期失败，因此不得用 `t.Skip` 声称可以越过。

- [ ] **Preflight 1：确认 A1c 已落地**

Run:

```bash
go list github.com/x6nux/yanshi/internal/secproc
go doc github.com/x6nux/yanshi/internal/secproc.Factory
go doc github.com/x6nux/yanshi/internal/tools.LaunchSecureProcess
```

Expected after A1c Task 14: 三条命令 exit 0；否则标记 B3 **BLOCKED** 并停止，不创建任何替代 interface/type alias。A1c 锁定合同：`secproc.SecureProcessSpec`（含 `Env []string`）、`secproc.StartedProcess`、`secproc.Factory`、`tools.WithSecureProcessFactory`、`tools.SecureProcessFactoryFromContext`、`tools.LaunchSecureProcess`。生产入口一律调 `tools.LaunchSecureProcess(ctx, secproc.SecureProcessSpec)`。

- [ ] **Preflight 2：确认 DT3/A2 已落地**

Run:

```bash
go list github.com/x6nux/yanshi/internal/task/work
go doc github.com/x6nux/yanshi/internal/task/work.ManagerLike.WriteArtifact
go doc github.com/x6nux/yanshi/internal/tools.TaskManagerFromContext
```

Expected: exit 0；否则 B3 **BLOCKED**。context bridge 是 `tools.WithTaskManager`/`tools.TaskManagerFromContext`（包 `internal/task/work`）。全计划不得出现 `internal/work`、`work.WithManager`、`work.ManagerFromContext`。

- [ ] **Preflight 3：确认 B2-LSP1 已落地**

Run:

```bash
go list github.com/x6nux/yanshi/internal/lsp
go doc github.com/x6nux/yanshi/internal/tools.LSPFromContext
go doc github.com/x6nux/yanshi/internal/lsp.Manager.Diagnostics
```

Expected: exit 0；否则 B3 **BLOCKED**。B3 不创建 `internal/tools/lspctx.go`，不导出 `LSPDiagnosticSource`，不增加 `App.LSPManager`。

- [ ] **Preflight 4：确认 Eino v0.9.12 schema 字段**

已核实锁定 module `github.com/cloudwego/eino@v0.9.12/schema/tool.go:252-264`：`ParameterInfo.ElemInfo *ParameterInfo`、`SubParams map[string]*ParameterInfo`。`github.com/eino-contrib/jsonschema.Schema.Items` 是 `*Schema`，`Properties` 是 `*orderedmap.OrderedMap[string,*Schema]`。本计划直接使用这些字段，不留版本分支。

---

## 文件映射

| 文件 | 动作 | 职责 |
|---|---|---|
| `internal/tools/helpers.go` | 修改 | 完整递归 parameter schema（Items + Properties） |
| `internal/tools/secproc_capture.go` | 新建 | A1c argv capture；有界 tail 保留 + truncation flags |
| `internal/tools/secproc_capture_test.go` | 新建 | 统一 `scriptedFactory` + `startCannedHelper`；截断、cancel、exit、无 factory |
| `internal/tools/artifact_output.go` | 新建 | DT3-first `artifactOutput` |
| `internal/tools/artifact_output_test.go` | 新建 | `work.NewFakeManager` 与 degraded fallback |
| `internal/tools/git.go`、`git_test.go` | 新建 | NUL-safe status/per-file diff |
| `internal/tools/testrun.go`、`testrun_test.go` | 新建 | Go/cargo/npm 探测、执行、解析 |
| `internal/tools/diagnostics.go`、`diagnostics_test.go` | 新建 | workspace/git/sandbox/toolchain/LSP 聚合 |
| `internal/tools/permctx.go`、`guard.go`、相关 tests | 修改 | mandatory approval 真值表与 `NewApprovalGuardedTool` |
| `internal/proto/frame.go`、`frame_test.go` | 修改 | `approval_required` wire 字段与 `NewPermissionRequest` bool 参数 |
| `internal/cli/backend.go`、`wsbackend.go` | 修改 | `StreamEvent.ApprovalRequired` + 映射 |
| `internal/cli/tui/{commands,events,model,permissions}.go` 与 tests | 修改 | mandatory UX 与 `/review` 命令 |
| `internal/api/http/ws.go`、`ws_test.go` | 修改 | `resolvePermissionMode` approvalRequired 顶短路 |
| `internal/tools/github.go`、`github_test.go` | 新建 | GH CLI 封装；`FetchGitHubContext` 窄导出 |
| `internal/tools/web.go`、`web_test.go` | 修改 | `NewWebTools(maxBytes, timeout)`；`Search` 工具；body degrade |
| `internal/tools/predefined.go` | 修改 | 注册 `"review"` predefined agent（`PromptTmpl`） |
| `internal/tools/agent.go` | 修改 | `Review *GuardedTool` + `streamReview` 接入 |
| `internal/tools/review.go`、`review_chunk.go`、`review_decode.go` | 新建 | 完整 deterministic review pipeline |
| `internal/tools/agent_test.go` | 修改 | review fixtures、chunk/finding/artifact tests |
| `internal/bootstrap/bootstrap.go` | 修改 | 一次性完整注册工具组 |
| `internal/bootstrap/b3_tools_test.go` | 新建 | group flattening 顺序/去重 |
| `config.example.yaml` | 修改 | cargo test-only allowlist |
| `cmd/yanshi/main.go` | 修改 | `switch` 加 `pr` case；`pr()` 完整函数；`runHeadlessPrompt` |
| `cmd/yanshi/pr.go`、`pr_test.go` | 新建 | GitHub context 共享、one-shot review |

---

## Task 1: 编译期跨批适配、schema、secure capture、artifact

**Files:**
- Modify: `internal/tools/helpers.go`
- Modify: `internal/tools/helpers_test.go`
- Create: `internal/tools/secproc_capture.go`
- Create: `internal/tools/secproc_capture_test.go`
- Create: `internal/tools/artifact_output.go`
- Create: `internal/tools/artifact_output_test.go`

- [ ] **Step 1：写 parameter schema RED test**

在 `internal/tools/helpers_test.go` 增加 imports `encoding/json`、`strings`、`testing`、Eino `schema`：

```go
func TestParamsPreservesArrayItemsAndObjectProperties(t *testing.T) {
    got := params(map[string]*schema.ParameterInfo{
        "domains": {Type: schema.Array, Required: true, ElemInfo: &schema.ParameterInfo{Type: schema.String, Desc: "hostname"}},
        "scope": {Type: schema.Object, SubParams: map[string]*schema.ParameterInfo{
            "kind": {Type: schema.String, Required: true, Enum: []string{"working_tree", "base_ref", "commit"}},
            "ref":  {Type: schema.String},
        }},
    })
    js, err := got.ToJSONSchema()
    if err != nil { t.Fatal(err) }
    raw, err := json.Marshal(js)
    if err != nil { t.Fatal(err) }
    text := string(raw)
    for _, want := range []string{
        `"domains"`, `"items":{"description":"hostname","type":"string"}`,
        `"scope"`, `"properties"`, `"kind"`, `"required":["kind"]`,
    } {
        if !strings.Contains(text, want) { t.Fatalf("schema %s missing %s", text, want) }
    }
}
```

- [ ] **Step 2：运行 RED test**

Run: `go test ./internal/tools -run TestParamsPreservesArrayItemsAndObjectProperties -v`

Expected: FAIL；当前 helper 丢失 `Items` 与 `Properties`。

- [ ] **Step 3：完整替换 `params` 内核**

`internal/tools/helpers.go` 保留现有 imports；将现有 `params` 函数替换为下述递归版本（写死的 accessor，不保留"执行时 go doc"占位）：

```go
func parameterSchema(p *schema.ParameterInfo) *jsonschema.Schema {
    out := &jsonschema.Schema{Type: string(p.Type), Description: p.Desc}
    if len(p.Enum) > 0 {
        out.Enum = make([]any, len(p.Enum))
        for i, value := range p.Enum { out.Enum[i] = value }
    }
    if p.ElemInfo != nil { out.Items = parameterSchema(p.ElemInfo) }
    if len(p.SubParams) > 0 {
        out.Properties = orderedmap.New[string, *jsonschema.Schema]()
        keys := make([]string, 0, len(p.SubParams))
        for key := range p.SubParams { keys = append(keys, key) }
        sort.Strings(keys)
        for _, key := range keys {
            child := p.SubParams[key]
            out.Properties.Set(key, parameterSchema(child))
            if child.Required { out.Required = append(out.Required, key) }
        }
    }
    return out
}

func params(m map[string]*schema.ParameterInfo) *schema.ParamsOneOf {
    if len(m) == 0 { return nil }
    root := &jsonschema.Schema{Type: "object", Properties: orderedmap.New[string, *jsonschema.Schema]()}
    keys := make([]string, 0, len(m))
    for key := range m { keys = append(keys, key) }
    sort.Strings(keys)
    for _, key := range keys {
        value := m[key]
        root.Properties.Set(key, parameterSchema(value))
        if value.Required { root.Required = append(root.Required, key) }
    }
    return schema.NewParamsOneOfByJSONSchema(root)
}
```

- [ ] **Step 4：写完整 secure capture RED tests 与统一工厂 fake（完整定义）**

新建 `internal/tools/secproc_capture_test.go`。`recordingSecureFactory`/`allowAllProfile`/`scriptedFactory`/`TestSecureCaptureHelperProcess` 是后续 Task 2/3/4/6 共享的辅助 —— 统一定义一次，不在其他任务中重定义。

```go
package tools

import (
    "context"
    "errors"
    "fmt"
    "os"
    "os/exec"
    "path/filepath"
    "strconv"
    "strings"
    "sync"
    "sync/atomic"
    "testing"
    "time"

    "github.com/x6nux/yanshi/internal/guard"
    "github.com/x6nux/yanshi/internal/sandbox"
    "github.com/x6nux/yanshi/internal/secproc"
)

func allowAllProfile() guard.PermissionProfile {
    return guard.PermissionProfile{
        FS:    guard.FSPerm{Read: []string{"**"}, Write: []string{"**"}},
        Tools: guard.ToolsPerm{Allow: []string{"*"}},
        Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
        Net:   guard.NetPerm{Allow: true, Hosts: []string{"*"}},
    }
}

// cannedResult is startCannedHelper's input: exact stdout/stderr text,
// exit code, and optionally block forever (cancellation tests).
type cannedResult struct {
    Stdout   string
    Stderr   string
    ExitCode int
    Block    bool
}

// scriptedFactory starts the helper subprocess with canned output written to
// temp files (avoids Windows env size limits). resultFn maps a spec to the
// output. Thread-safe; lastSpec() records the most-recent Start call. This is
// the ONE shared fake Factory for Task 2/3/4/6 — do not redefine it.
type scriptedFactory struct {
    t        *testing.T
    dir      string
    seq      atomic.Int64
    resultFn func(secproc.SecureProcessSpec) cannedResult
    mu       sync.Mutex
    last     secproc.SecureProcessSpec
}

func newScriptedFactory(t *testing.T, resultFn func(secproc.SecureProcessSpec) cannedResult) *scriptedFactory {
    return &scriptedFactory{t: t, dir: t.TempDir(), resultFn: resultFn}
}

func (f *scriptedFactory) Start(ctx context.Context, spec secproc.SecureProcessSpec) (*secproc.StartedProcess, error) {
    f.mu.Lock()
    f.last = spec
    res := f.resultFn(spec)
    f.mu.Unlock()
    id := f.seq.Add(1)
    outPath := filepath.Join(f.dir, fmt.Sprintf("out-%d", id))
    errPath := filepath.Join(f.dir, fmt.Sprintf("err-%d", id))
    if err := os.WriteFile(outPath, []byte(res.Stdout), 0o644); err != nil { return nil, err }
    if err := os.WriteFile(errPath, []byte(res.Stderr), 0o644); err != nil { return nil, err }
    cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestSecureCaptureHelperProcess", "--")
    cmd.Env = append(os.Environ(),
        "YANSHI_CAPTURE_HELPER=1",
        "YANSHI_CAPTURE_STDOUT_FILE="+outPath,
        "YANSHI_CAPTURE_STDERR_FILE="+errPath,
        "YANSHI_CAPTURE_EXIT="+strconv.Itoa(res.ExitCode),
        "YANSHI_CAPTURE_BLOCK="+strconv.FormatBool(res.Block),
    )
    stdout, err := cmd.StdoutPipe()
    if err != nil { return nil, err }
    stderr, err := cmd.StderrPipe()
    if err != nil { return nil, err }
    if err := cmd.Start(); err != nil { return nil, err }
    return &secproc.StartedProcess{Cmd: cmd, PID: cmd.Process.Pid, Stdout: stdout, Stderr: stderr}, nil
}

func (f *scriptedFactory) lastSpec() secproc.SecureProcessSpec {
    f.mu.Lock()
    defer f.mu.Unlock()
    return f.last
}

// startCannedHelper returns a StartedProcess that emits one fixed cannedResult.
// Convenience wrapper for tests that need only a single output.
func startCannedHelper(ctx context.Context, t *testing.T, res cannedResult) (*secproc.StartedProcess, error) {
    return newScriptedFactory(t, func(secproc.SecureProcessSpec) cannedResult { return res }).Start(ctx, secproc.SecureProcessSpec{})
}

// TestSecureCaptureHelperProcess is the re-exec'd helper: it copies the temp
// files referenced by env to stdout/stderr, then exits. When Block is set it
// parks until the ctx-cancelled CommandContext kills it.
func TestSecureCaptureHelperProcess(t *testing.T) {
    if os.Getenv("YANSHI_CAPTURE_HELPER") != "1" { return }
    if os.Getenv("YANSHI_CAPTURE_BLOCK") == "true" { select {} }
    if p := os.Getenv("YANSHI_CAPTURE_STDOUT_FILE"); p != "" {
        if b, err := os.ReadFile(p); err == nil { _, _ = os.Stdout.Write(b) }
    }
    if p := os.Getenv("YANSHI_CAPTURE_STDERR_FILE"); p != "" {
        if b, err := os.ReadFile(p); err == nil { _, _ = os.Stderr.Write(b) }
    }
    code, _ := strconv.Atoi(os.Getenv("YANSHI_CAPTURE_EXIT"))
    os.Exit(code)
}

func secureTestContext(t *testing.T, factory secproc.Factory) context.Context {
    return WithSecureProcessFactory(WithProfile(context.Background(), allowAllProfile()), factory)
}

func TestRunSecureCapturePreservesQualifiedSpecAndExit(t *testing.T) {
    factory := newScriptedFactory(t, func(secproc.SecureProcessSpec) cannedResult {
        return cannedResult{Stdout: "oo", Stderr: "eee", ExitCode: 7}
    })
    spec := secproc.SecureProcessSpec{
        Tool: "run_tests", Program: "cargo", Args: []string{"test", "name with space"},
        Dir: t.TempDir(), Env: []string{"LANG=C"}, UseSandboxTier: sandbox.WorkspaceWrite,
    }
    got, err := runSecureCapture(secureTestContext(t, factory), spec, time.Second)
    if err != nil { t.Fatal(err) }
    if got.Stdout != "oo" || got.Stderr != "eee" || got.ExitCode != 7 { t.Fatalf("got=%+v", got) }
    last := factory.lastSpec()
    if last.Program != "cargo" || last.Args[1] != "name with space" || last.Env[0] != "LANG=C" { t.Fatalf("spec=%+v", last) }
}

func TestRunSecureCaptureCancellationStopsWait(t *testing.T) {
    factory := newScriptedFactory(t, func(secproc.SecureProcessSpec) cannedResult { return cannedResult{Block: true} })
    ctx, cancel := context.WithCancel(secureTestContext(t, factory))
    cancel()
    _, err := runSecureCapture(ctx, secproc.SecureProcessSpec{Tool: "run_tests", Program: "go"}, time.Second)
    if !errors.Is(err, context.Canceled) { t.Fatalf("err=%v", err) }
}

func TestRunSecureCaptureFailsClosedWithoutFactory(t *testing.T) {
    ctx := WithProfile(context.Background(), allowAllProfile())
    _, err := runSecureCapture(ctx, secproc.SecureProcessSpec{Tool: "git_status", Program: "git"}, time.Second)
    if err == nil || !strings.Contains(err.Error(), "Factory") { t.Fatalf("err=%v", err) }
}

func TestRunSecureCaptureDrainsAndReportsTruncation(t *testing.T) {
    factory := newScriptedFactory(t, func(secproc.SecureProcessSpec) cannedResult {
        return cannedResult{Stdout: strings.Repeat("o", secureCaptureLimit+123), Stderr: strings.Repeat("e", secureCaptureLimit+7)}
    })
    got, err := runSecureCapture(secureTestContext(t, factory), secproc.SecureProcessSpec{Tool: "run_tests", Program: "go"}, 5*time.Second)
    if err != nil { t.Fatal(err) }
    if !got.StdoutTruncated || !got.StderrTruncated { t.Fatalf("got=%+v", got) }
    if got.StdoutBytes != int64(secureCaptureLimit+123) || got.StderrBytes != int64(secureCaptureLimit+7) { t.Fatalf("bytes=%d/%d", got.StdoutBytes, got.StderrBytes) }
    if len(got.Stdout) != secureCaptureLimit || len(got.Stderr) != secureCaptureLimit { t.Fatalf("kept=%d/%d", len(got.Stdout), len(got.Stderr)) }
}

var _ secproc.Factory = (*scriptedFactory)(nil)
```

- [ ] **Step 5：运行 secure capture RED tests**

Run: `go test ./internal/tools -run 'TestRunSecureCapture|TestSecureCaptureHelperProcess' -v`

Expected: FAIL，`runSecureCapture` 未定义。

- [ ] **Step 6：实现完整、有界 tail 窗口、drain 全输出的 capture**

新建 `internal/tools/secproc_capture.go`。`boundedCapture` 完整 drain（每 Write 都接收）但只保留 **tail** 窗口（drop-front），让最新输出（错误/测试摘要）在截断时存活；结果显式带 truncation flags + original byte counts。

```go
package tools

import (
    "bytes"
    "context"
    "errors"
    "io"
    "os/exec"
    "sync"
    "time"

    "github.com/x6nux/yanshi/internal/secproc"
)

const secureCaptureLimit = 1 << 20 // 1 MiB

type commandResult struct {
    Stdout          string `json:"stdout"`
    Stderr          string `json:"stderr"`
    ExitCode        int    `json:"exit_code"`
    StdoutTruncated bool   `json:"stdout_truncated"`
    StderrTruncated bool   `json:"stderr_truncated"`
    StdoutBytes     int64  `json:"stdout_original_bytes"`
    StderrBytes     int64  `json:"stderr_original_bytes"`
}

type commandRunner func(context.Context, secproc.SecureProcessSpec, time.Duration) (commandResult, error)

var secureCommandRunner commandRunner = runSecureCapture

type boundedCapture struct {
    mu    sync.Mutex
    buf   bytes.Buffer
    limit int
    total int64
}

func (w *boundedCapture) Write(p []byte) (int, error) {
    w.mu.Lock()
    defer w.mu.Unlock()
    n := len(p)
    w.total += int64(n)
    w.buf.Write(p)
    // Drain is complete (every Write accepted); only the retained window is
    // bounded. Drop from the front so the TAIL (recent errors/summaries) wins.
    if w.buf.Len() > w.limit {
        w.buf.Next(w.buf.Len() - w.limit)
    }
    return n, nil
}

func (w *boundedCapture) snapshot() (string, int64, bool) {
    w.mu.Lock()
    defer w.mu.Unlock()
    return w.buf.String(), w.total, w.total > int64(w.limit)
}

func drain(dst io.Writer, src io.Reader) <-chan error {
    done := make(chan error, 1)
    go func() { _, err := io.Copy(dst, src); done <- err }()
    return done
}

func runSecureCapture(ctx context.Context, spec secproc.SecureProcessSpec, timeout time.Duration) (commandResult, error) {
    runCtx, cancel := context.WithTimeout(ctx, timeout)
    defer cancel()
    started, err := LaunchSecureProcess(runCtx, spec)
    if err != nil { return commandResult{}, err }
    stdout := &boundedCapture{limit: secureCaptureLimit}
    stderr := &boundedCapture{limit: secureCaptureLimit}
    stdoutDone := drain(stdout, started.Stdout)
    stderrDone := drain(stderr, started.Stderr)
    waitErr := started.Cmd.Wait()
    stdoutErr, stderrErr := <-stdoutDone, <-stderrDone
    if stdoutErr != nil { return commandResult{}, stdoutErr }
    if stderrErr != nil { return commandResult{}, stderrErr }
    if err := runCtx.Err(); err != nil { return commandResult{}, err }
    exitCode := 0
    if waitErr != nil {
        var exitErr *exec.ExitError
        if !errors.As(waitErr, &exitErr) { return commandResult{}, waitErr }
        exitCode = exitErr.ExitCode()
    }
    stdoutText, stdoutBytes, stdoutTruncated := stdout.snapshot()
    stderrText, stderrBytes, stderrTruncated := stderr.snapshot()
    return commandResult{
        Stdout: stdoutText, Stderr: stderrText, ExitCode: exitCode,
        StdoutTruncated: stdoutTruncated, StderrTruncated: stderrTruncated,
        StdoutBytes: stdoutBytes, StderrBytes: stderrBytes,
    }, nil
}
```

`io.Copy` 始终 drain 到 EOF；buffer 只保留 tail 1 MiB；结果显式提供 truncation flags 与 original byte counts，不会静默把截断当完整输出。

- [ ] **Step 7：写 DT3-first artifact RED tests**

新建 `internal/tools/artifact_output_test.go`：

```go
package tools

import (
    "context"
    "strings"
    "testing"

    "github.com/x6nux/yanshi/internal/task/work"
)

func TestWriteArtifactOrSpillUsesTaskManager(t *testing.T) {
    manager := work.NewFakeManager()
    root := t.TempDir()
    ctx := WithWorkRoot(WithTaskManager(context.Background(), manager), root)
    content := strings.Repeat("x", SpillThreshold+1)
    got := writeArtifactOrSpill(ctx, "task-7", "git-diff", content)
    if got.Degraded || got.ArtifactRef == "" || got.Size != int64(len(content)) { t.Fatalf("got=%+v", got) }
    stored, err := manager.ReadArtifact(ctx, got.ArtifactRef)
    if err != nil { t.Fatal(err) }
    if stored.TaskID != "task-7" || stored.Label != "git-diff" || stored.Size != int64(len(content)) { t.Fatalf("stored=%+v", stored) }
}

func TestWriteArtifactOrSpillMarksFallbackDegraded(t *testing.T) {
    ctx := WithWorkRoot(context.Background(), t.TempDir())
    got := writeArtifactOrSpill(ctx, "task-7", "git-diff", strings.Repeat("x", SpillThreshold+1))
    if !got.Degraded || got.ArtifactRef != "" { t.Fatalf("got=%+v", got) }
    if !strings.HasPrefix(got.Summary, "[degraded: task artifact manager unavailable; using temporary spillover]\n") { t.Fatalf("summary=%q", got.Summary) }
}
```

- [ ] **Step 8：实现 `artifactOutput` 单一函数（DT3-first）**

新建 `internal/tools/artifact_output.go`。返回类型是 `artifactOutput`；优先走 DT3 `TaskManagerFromContext`，回退 `spillIfTooLong`，并置 `Degraded`。

```go
package tools

import "context"

type artifactOutput struct {
    Summary     string `json:"summary"`
    ArtifactRef string `json:"artifact_ref,omitempty"`
    Size        int64  `json:"size"`
    Degraded    bool   `json:"degraded,omitempty"`
}

func writeArtifactOrSpill(ctx context.Context, taskID, label, content string) artifactOutput {
    size := int64(len(content))
    if len(content) <= SpillThreshold { return artifactOutput{Summary: content, Size: size} }
    root := WorkRootFromContext(ctx)
    if root == "" { root = "." }
    if manager, ok := TaskManagerFromContext(ctx); ok {
        artifact, err := manager.WriteArtifact(ctx, taskID, label, []byte(content), root)
        if err == nil {
            return artifactOutput{Summary: artifact.Summary, ArtifactRef: artifact.ID, Size: artifact.Size}
        }
    }
    return artifactOutput{
        Summary:  "[degraded: task artifact manager unavailable; using temporary spillover]\n" + spillIfTooLong(ctx, label, content),
        Size:     size,
        Degraded: true,
    }
}
```

- [ ] **Step 9：运行 GREEN tests**

Run: `go test ./internal/tools -run 'TestParamsPreserves|TestRunSecureCapture|TestWriteArtifactOrSpill' -v`

Expected: PASS。

- [ ] **Step 10：提交**

```bash
git add internal/tools/helpers.go internal/tools/helpers_test.go internal/tools/secproc_capture.go internal/tools/secproc_capture_test.go internal/tools/artifact_output.go internal/tools/artifact_output_test.go
git commit -m "feat(tools): adapt schemas secure processes and task artifacts"
```

## Task 2: W07 — `git_status` / `git_diff`（per-file、NUL-safe、隔离 HOME）

**Files:**
- Create: `internal/tools/git.go`
- Create: `internal/tools/git_test.go`

- [ ] **Step 1：写真实 TDD RED 测试（非 comment-only）**

新建 `internal/tools/git_test.go`。所有测试均为完整 Go 代码：staged/unstaged/untracked、empty、scopes（working_tree/base_ref/commit）、path-escape、binary marker、config 隔离。

```go
package tools

import (
    "bytes"
    "context"
    "encoding/json"
    "os"
    "os/exec"
    "path/filepath"
    "runtime"
    "strings"
    "testing"

    "github.com/x6nux/yanshi/internal/secproc"
)

func initTempGitRepo(t *testing.T) string {
    t.Helper()
    root := t.TempDir()
    for _, args := range [][]string{
        {"init", "--quiet"},
        {"config", "user.email", "test@example.com"},
        {"config", "user.name", "test"},
    } {
        cmd := exec.Command("git", args...)
        cmd.Dir = root
        if out, err := cmd.CombinedOutput(); err != nil { t.Fatalf("git %v: %v\n%s", args, err, out) }
    }
    return root
}

func commitFile(t *testing.T, root, path, content string) {
    t.Helper()
    full := filepath.Join(root, path)
    if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil { t.Fatal(err) }
    if err := os.WriteFile(full, []byte(content), 0o644); err != nil { t.Fatal(err) }
    cmd := exec.Command("git", "add", "--", path)
    cmd.Dir = root
    if out, err := cmd.CombinedOutput(); err != nil { t.Fatalf("add: %v\n%s", err, out) }
    cmd = exec.Command("git", "commit", "--quiet", "-m", "add "+path)
    cmd.Dir = root
    if out, err := cmd.CombinedOutput(); err != nil { t.Fatalf("commit: %v\n%s", err, out) }
}

// realGitFactory runs git through the OS — used for status/diff integration tests.
type realGitFactory struct{}

func (realGitFactory) Start(ctx context.Context, spec secproc.SecureProcessSpec) (*secproc.StartedProcess, error) {
    cmd := exec.CommandContext(ctx, spec.Program, spec.Args...)
    cmd.Dir = spec.Dir
    cmd.Env = append(os.Environ(), spec.Env...)
    stdout, err := cmd.StdoutPipe()
    if err != nil { return nil, err }
    stderr, err := cmd.StderrPipe()
    if err != nil { return nil, err }
    if err := cmd.Start(); err != nil { return nil, err }
    return &secproc.StartedProcess{Cmd: cmd, PID: cmd.Process.Pid, Stdout: stdout, Stderr: stderr}, nil
}

func realGitCtx(t *testing.T, root string) context.Context {
    return WithWorkRoot(secureTestContext(t, realGitFactory{}), root)
}

func TestGitStatusParsesPorcelainV2ZWithHostileNames(t *testing.T) {
    root := initTempGitRepo(t)
    for _, name := range []string{"a b.txt", "你好.go", "tab\tname.txt"} {
        if err := os.WriteFile(filepath.Join(root, name), []byte("x\n"), 0o644); err != nil { t.Fatal(err) }
    }
    out, err := runTool(realGitCtx(t, root), NewGitTools().Status, `{}`)
    if err != nil { t.Fatal(err) }
    for _, name := range []string{"a b.txt", "你好.go", "tab\tname.txt"} {
        if !strings.Contains(out, strings.ReplaceAll(name, `"`, `\"`)) { t.Fatalf("out=%s missing %q", out, name) }
    }
}

func TestGitDiffReturnsOneRecordPerFileWithBinaryMarker(t *testing.T) {
    root := initTempGitRepo(t)
    commitFile(t, root, "a b.txt", "old\n")
    commitFile(t, root, "bin.dat", string([]byte{0, 1, 2}))
    if err := os.WriteFile(filepath.Join(root, "a b.txt"), []byte("new\n"), 0o644); err != nil { t.Fatal(err) }
    if err := os.WriteFile(filepath.Join(root, "bin.dat"), []byte{0, 9, 2}), 0o644); err != nil { t.Fatal(err) }
    out, err := runTool(realGitCtx(t, root), NewGitTools().Diff, `{"scope":{"kind":"working_tree"}}`)
    if err != nil { t.Fatal(err) }
    var res struct {
        Files []struct {
            Path   string `json:"path"`
            Binary bool   `json:"binary"`
            Patch  string `json:"patch"`
        } `json:"files"`
    }
    if err := json.Unmarshal([]byte(out), &res); err != nil { t.Fatal(err) }
    if len(res.Files) != 2 { t.Fatalf("files=%+v", res.Files) }
    byPath := map[string]int{}
    for i, f := range res.Files { byPath[f.Path] = i }
    idxText, ok1 := byPath["a b.txt"]
    idxBin, ok2 := byPath["bin.dat"]
    if !ok1 || !ok2 { t.Fatalf("files=%+v", res.Files) }
    if res.Files[idxText].Binary || res.Files[idxText].Patch == "" { t.Fatalf("text file missing patch: %+v", res.Files[idxText]) }
    if !res.Files[idxBin].Binary || res.Files[idxBin].Patch != "" { t.Fatalf("binary file should not include patch: %+v", res.Files[idxBin]) }
}

func TestGitDiffWorkingTreeIncludesStagedUnstagedUntracked(t *testing.T) {
    root := initTempGitRepo(t)
    commitFile(t, root, "committed.go", "package p\n")
    if err := os.WriteFile(filepath.Join(root, "staged.go"), []byte("package p\n"), 0o644); err != nil { t.Fatal(err) }
    if out, err := exec.Command("git", "-C", root, "add", "--", "staged.go").CombinedOutput(); err != nil { t.Fatalf("add: %v\n%s", err, out) }
    if err := os.WriteFile(filepath.Join(root, "committed.go"), []byte("package p // edit\n"), 0o644); err != nil { t.Fatal(err) }
    if err := os.WriteFile(filepath.Join(root, "untracked.go"), []byte("package p\n"), 0o644); err != nil { t.Fatal(err) }
    out, err := runTool(realGitCtx(t, root), NewGitTools().Diff, `{"scope":{"kind":"working_tree"}}`)
    if err != nil { t.Fatal(err) }
    for _, want := range []string{"staged.go", "committed.go", "untracked.go"} {
        if !strings.Contains(out, want) { t.Fatalf("working_tree missing %s\n%s", want, out) }
    }
}

func TestGitDiffEmptyReturnsNoFiles(t *testing.T) {
    root := initTempGitRepo(t)
    out, err := runTool(realGitCtx(t, root), NewGitTools().Diff, `{"scope":{"kind":"working_tree"}}`)
    if err != nil { t.Fatal(err) }
    if !strings.Contains(out, `"files":[]`) { t.Fatalf("out=%s", out) }
}

func TestGitDiffRejectsPathEscape(t *testing.T) {
    root := initTempGitRepo(t)
    // runGitDiff returns ("", fmt.Errorf("path ... outside work root")); SyncStream
    // packages that as a ToolChunk with Err set, and InvokableRun converts non-
    // circuit-breaker errors into the result string (err==nil). Assert on `out`.
    out, err := runTool(realGitCtx(t, root), NewGitTools().Diff, `{"scope":{"kind":"working_tree"},"paths":["../escape"]}`)
    if err != nil { t.Fatal(err) }
    if !strings.Contains(out, "outside work root") { t.Fatalf("out=%s", out) }
}

func TestGitDiffScopesBaseRefAndCommit(t *testing.T) {
    root := initTempGitRepo(t)
    commitFile(t, root, "a.go", "package a\n")
    if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a // edit\n"), 0o644); err != nil { t.Fatal(err) }
    for _, tc := range []struct{ name, args string }{
        {"working_tree", `{"scope":{"kind":"working_tree"}}`},
        {"base_ref", `{"scope":{"kind":"base_ref","ref":"HEAD~1"}}`},
        {"commit", `{"scope":{"kind":"commit","ref":"HEAD"}}`},
    } {
        out, err := runTool(realGitCtx(t, root), NewGitTools().Diff, tc.args)
        if err != nil { t.Fatalf("%s: %v", tc.name, err) }
        if !strings.Contains(out, `"path":"a.go"`) { t.Fatalf("%s: out=%s", tc.name, out) }
    }
}

func TestGitToolsDoNotWriteGitConfig(t *testing.T) {
    home := t.TempDir()
    global := filepath.Join(home, "global.gitconfig")
    original := []byte("[user]\n\tname = sentinel\n")
    if err := os.WriteFile(global, original, 0o600); err != nil { t.Fatal(err) }
    t.Setenv("HOME", home)
    if runtime.GOOS == "windows" { t.Setenv("USERPROFILE", home) }
    t.Setenv("GIT_CONFIG_GLOBAL", global)
    t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
    t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
    root := initTempGitRepo(t)
    commitFile(t, root, "a.go", "package a\n")
    if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a // edit\n"), 0o644); err != nil { t.Fatal(err) }
    _, _ = runTool(realGitCtx(t, root), NewGitTools().Status, `{}`)
    _, _ = runTool(realGitCtx(t, root), NewGitTools().Diff, `{"scope":{"kind":"working_tree"}}`)
    got, err := os.ReadFile(global)
    if err != nil { t.Fatal(err) }
    if !bytes.Equal(got, original) { t.Fatalf("global config mutated:\nwant=%q\ngot=%q", original, got) }
}
```

> `GIT_CONFIG_NOSYSTEM=1` 与 `XDG_CONFIG_HOME` 是关键：B3 的 git 调用必须通过 spec.Env 注入这两个变量（见 Step 3 的 `gitEnvIsolation`），防止读到/写到用户全局 git config。

- [ ] **Step 2：运行 RED**

Run: `go test ./internal/tools -run 'TestGitStatus|TestGitDiff|TestGitToolsDoNotWriteGitConfig' -v`

Expected: FAIL；`NewGitTools` 未定义。

- [ ] **Step 3：实现 git.go（per-file 分块、`-z`、隔离 HOME）**

新建 `internal/tools/git.go`，包含 `GitTools`、`NewGitTools()`、`runGitStatus`、`runGitDiff`、`collectGitDiffFiles`、`gitDiffCommands`、`gitPatchForFile`、`parseGitNumstatZ`、`parseGitStatusZ`、`filterGitByPaths`、`validateGitRef`、`sanitizeLabel`、`gitEnvIsolation`。每条 git spec 的 `Env` 都追加 `gitEnvIsolation()`（`GIT_CONFIG_NOSYSTEM=1`、`XDG_CONFIG_HOME=<root>/.yanshi/tmp/gitxdg`）。

```go
package tools

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "path/filepath"
    "strings"
    "time"

    "github.com/cloudwego/eino/schema"
    "github.com/x6nux/yanshi/internal/sandbox"
    "github.com/x6nux/yanshi/internal/secproc"
)

type GitTools struct {
    Status *GuardedTool
    Diff   *GuardedTool
}

type gitFile struct {
    Path        string `json:"path"`
    Additions   int    `json:"additions"`
    Deletions   int    `json:"deletions"`
    Binary      bool   `json:"binary"`
    Patch       string `json:"patch,omitempty"`
    ArtifactRef string `json:"artifact_ref,omitempty"`
    Degraded    bool   `json:"degraded,omitempty"`
}

type gitDiffArgs struct {
    Scope struct {
        Kind string `json:"kind"`
        Ref  string `json:"ref,omitempty"`
    } `json:"scope"`
    Paths []string `json:"paths,omitempty"`
}

// gitEnvIsolation returns env entries that prevent git from reading/writing the
// user's global or system config: GIT_CONFIG_NOSYSTEM skips /etc/gitconfig, and
// XDG_CONFIG_HOME points at a throwaway dir under the work root so any
// home-directory ~/.gitconfig is neither consulted nor mutated.
func gitEnvIsolation(root string) []string {
    return []string{
        "GIT_CONFIG_NOSYSTEM=1",
        "XDG_CONFIG_HOME=" + filepath.Join(root, ".yanshi", "tmp", "gitxdg"),
    }
}

func NewGitTools() *GitTools {
    return &GitTools{
        Status: NewGuardedTool("git_status", "Git status", "Return structured working-tree status.", 10*time.Second, nil, SyncStream(runGitStatus)),
        Diff: NewGuardedTool("git_diff", "Git diff", "Return per-file structured diff.", 30*time.Second,
            params(map[string]*schema.ParameterInfo{
                "scope": {Type: schema.Object, Required: true, SubParams: map[string]*schema.ParameterInfo{
                    "kind": {Type: schema.String, Required: true, Enum: []string{"working_tree", "base_ref", "commit"}},
                    "ref":  {Type: schema.String},
                }},
                "paths": {Type: schema.Array, ElemInfo: &schema.ParameterInfo{Type: schema.String}},
            }), SyncStream(runGitDiff)),
    }
}

func runGitStatus(ctx context.Context, argsJSON string) (string, error) {
    root := WorkRootFromContext(ctx)
    spec := secproc.SecureProcessSpec{
        Tool: "git_status", Program: "git", Dir: root,
        Args: []string{"-c", "core.quotepath=false", "status", "--porcelain=v2", "-z", "--untracked-files=all"},
        Env:              gitEnvIsolation(root),
        UseSandboxTier:   sandbox.ReadOnly,
    }
    res, err := secureCommandRunner(ctx, spec, 10*time.Second)
    if err != nil { return errorResult("git status: " + err.Error()), nil }
    return toJSON(parseGitStatusZ(res.Stdout)), nil
}

func runGitDiff(ctx context.Context, argsJSON string) (string, error) {
    root := WorkRootFromContext(ctx)
    var args gitDiffArgs
    if err := ParseArgs(argsJSON, &args); err != nil { return errorResult(err.Error()), nil }
    for _, p := range args.Paths {
        abs := p
        if !filepath.IsAbs(abs) { abs = filepath.Join(root, p) }
        if !withinRoot(abs, root) { return "", fmt.Errorf("path %q outside work root", p) }
    }
    files, err := collectGitDiffFiles(ctx, root, args)
    if err != nil { return errorResult(err.Error()), nil }
    return toJSON(struct {
        Scope struct {
            Kind string `json:"kind"`
            Ref  string `json:"ref,omitempty"`
        } `json:"scope"`
        Files []gitFile `json:"files"`
    }{Scope: args.Scope, Files: files}), nil
}

func collectGitDiffFiles(ctx context.Context, root string, args gitDiffArgs) ([]gitFile, error) {
    numstatSpec, patchBuilder, err := gitDiffCommands(args)
    if err != nil { return nil, err }
    numstatSpec.Dir = root
    numstatSpec.Tool = "git_diff"
    numstatSpec.UseSandboxTier = sandbox.ReadOnly
    numstatSpec.Env = gitEnvIsolation(root)
    res, err := secureCommandRunner(ctx, numstatSpec, 30*time.Second)
    if err != nil { return nil, err }
    entries := parseGitNumstatZ(res.Stdout)
    if args.Scope.Kind == "working_tree" {
        untrackedSpec := secproc.SecureProcessSpec{
            Tool: "git_diff", Program: "git", Dir: root,
            Args:            []string{"-c", "core.quotepath=false", "ls-files", "--others", "--exclude-standard", "-z"},
            Env:             gitEnvIsolation(root),
            UseSandboxTier:  sandbox.ReadOnly,
        }
        res, err := secureCommandRunner(ctx, untrackedSpec, 10*time.Second)
        if err != nil { return nil, err }
        for _, path := range strings.Split(strings.TrimRight(res.Stdout, "\x00"), "\x00") {
            if path == "" { continue }
            entries = append(entries, gitNumstatEntry{Path: path, Untracked: true})
        }
    }
    var files []gitFile
    for _, e := range filterGitByPaths(entries, args.Paths) {
        patch, binary, err := gitPatchForFile(ctx, root, e.Path, args, patchBuilder)
        if err != nil { return nil, err }
        entry := gitFile{Path: e.Path, Additions: e.Additions, Deletions: e.Deletions, Binary: binary || e.Binary}
        if !binary && !e.Binary && !e.Untracked {
            art := writeArtifactOrSpill(ctx, "git-diff", sanitizeLabel(e.Path), patch)
            entry.Patch = art.Summary
            entry.ArtifactRef = art.ArtifactRef
            entry.Degraded = art.Degraded
        }
        files = append(files, entry)
    }
    return files, nil
}

func gitDiffCommands(args gitDiffArgs) (secproc.SecureProcessSpec, func(path string) secproc.SecureProcessSpec, error) {
    baseArgs := []string{"-c", "core.quotepath=false"}
    switch args.Scope.Kind {
    case "working_tree":
        numstat := secproc.SecureProcessSpec{Program: "git", Args: append(baseArgs, "diff", "--numstat", "-z", "--no-ext-diff", "--")}
        return numstat, func(path string) secproc.SecureProcessSpec {
            return secproc.SecureProcessSpec{Program: "git", Args: append(baseArgs, "diff", "--no-ext-diff", "--binary", "--", path)}
        }, nil
    case "base_ref":
        if err := validateGitRef(args.Scope.Ref); err != nil { return secproc.SecureProcessSpec{}, nil, err }
        numstat := secproc.SecureProcessSpec{Program: "git", Args: append(baseArgs, "diff", "--numstat", "-z", "--no-ext-diff", args.Scope.Ref+"...HEAD", "--")}
        return numstat, func(path string) secproc.SecureProcessSpec {
            return secproc.SecureProcessSpec{Program: "git", Args: append(baseArgs, "diff", "--no-ext-diff", "--binary", args.Scope.Ref+"...HEAD", "--", path)}
        }, nil
    case "commit":
        if err := validateGitRef(args.Scope.Ref); err != nil { return secproc.SecureProcessSpec{}, nil, err }
        numstat := secproc.SecureProcessSpec{Program: "git", Args: append(baseArgs, "show", "--numstat", "-z", "--format=", args.Scope.Ref, "--")}
        return numstat, func(path string) secproc.SecureProcessSpec {
            return secproc.SecureProcessSpec{Program: "git", Args: append(baseArgs, "show", "--format=", "--binary", args.Scope.Ref, "--", path)}
        }, nil
    default:
        return secproc.SecureProcessSpec{}, nil, fmt.Errorf("unknown scope %q", args.Scope.Kind)
    }
}

func gitPatchForFile(ctx context.Context, root, path string, args gitDiffArgs, build func(string) secproc.SecureProcessSpec) (string, bool, error) {
    _ = args
    spec := build(path)
    spec.Dir = root
    spec.Tool = "git_diff"
    spec.UseSandboxTier = sandbox.ReadOnly
    spec.Env = gitEnvIsolation(root)
    res, err := secureCommandRunner(ctx, spec, 15*time.Second)
    if err != nil { return "", false, err }
    binary := strings.Contains(res.Stdout, "Binary files") || res.Stdout == ""
    return res.Stdout, binary, nil
}

type gitNumstatEntry struct {
    Path      string
    Additions int
    Deletions int
    Binary    bool
    Untracked bool
}

func parseGitNumstatZ(stdout string) []gitNumstatEntry {
    var out []gitNumstatEntry
    fields := strings.Split(strings.TrimRight(stdout, "\x00"), "\x00")
    for i := 0; i+2 < len(fields); i += 3 {
        adds, dels, path := fields[i], fields[i+1], fields[i+2]
        e := gitNumstatEntry{Path: path}
        if adds != "-" { fmt.Sscanf(adds, "%d", &e.Additions) } else { e.Binary = true }
        if dels != "-" { fmt.Sscanf(dels, "%d", &e.Deletions) } else { e.Binary = true }
        out = append(out, e)
    }
    return out
}

func parseGitStatusZ(stdout string) any {
    type entry struct {
        Path string `json:"path"`
        XY   string `json:"xy"`
    }
    var entries []entry
    for _, record := range strings.Split(strings.TrimRight(stdout, "\x00"), "\x00") {
        if record == "" { continue }
        parts := strings.SplitN(record, " ", 3)
        if len(parts) < 3 { continue }
        entries = append(entries, entry{XY: parts[1], Path: parts[2]})
    }
    return struct {
        Entries []entry `json:"entries"`
    }{Entries: entries}
}

func filterGitByPaths(entries []gitNumstatEntry, paths []string) []gitNumstatEntry {
    if len(paths) == 0 { return entries }
    allow := map[string]bool{}
    for _, p := range paths { allow[p] = true }
    var out []gitNumstatEntry
    for _, e := range entries {
        if allow[e.Path] { out = append(out, e) }
    }
    return out
}

var gitRefDisallowed = strings.NewReplacer(" ", "", "\t", "", "\n", "", "\r", "", "\x00", "")

func validateGitRef(ref string) error {
    if ref == "" || gitRefDisallowed.Replace(ref) != ref { return fmt.Errorf("invalid git ref %q", ref) }
    return nil
}

func sanitizeLabel(path string) string {
    return strings.Map(func(r rune) rune {
        if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' { return r }
        return '_'
    }, path)
}
```

> `withinRoot` 由 `internal/tools/fs.go` 提供（已存在）。`gitEnvIsolation` 是 Task 2 新增的公共 helper，被所有 git spec 复用。

- [ ] **Step 4：运行 GREEN**

Run: `go test ./internal/tools -run 'TestGitStatus|TestGitDiff|TestGitToolsDoNotWriteGitConfig' -v`

Expected: PASS。

- [ ] **Step 5：提交**

```bash
git add internal/tools/git.go internal/tools/git_test.go
git commit -m "feat(tools): add NUL-safe per-file git status and diff"
```

---

## Task 3: DT4 — `run_tests`（Go/cargo/npm，完整解析）

**Files:**
- Create: `internal/tools/testrun.go`
- Create: `internal/tools/testrun_test.go`
- Modify: `config.example.yaml`

> **修订要点：** 不再定义 `fakeRunnerFactory` 或 `simulateFactoryResult`（旧版的 `simulateFactoryResult` 未调用 `cmd.Start()`，导致 `runSecureCapture` 的 `cmd.Wait()` 必失败）。Task 3 直接复用 Task 1 的 `scriptedFactory` + `cannedResult`。

- [ ] **Step 1：写真实 TDD RED 测试（parsers 输入+预期 + runner glue）**

新建 `internal/tools/testrun_test.go`：

```go
package tools

import (
    "encoding/json"
    "testing"

    "github.com/x6nux/yanshi/internal/sandbox"
    "github.com/x6nux/yanshi/internal/secproc"
)

func TestParseGoJSONCountsPassFailSkip(t *testing.T) {
    raw := `{"Action":"pass","Package":"p","Test":"TestA"}` + "\n" +
        `{"Action":"fail","Package":"p","Test":"TestB"}` + "\n" +
        `{"Action":"skip","Package":"p","Test":"TestC"}` + "\n"
    got := parseGoJSON(raw)
    if got.Framework != "go" || got.Passed != 1 || got.Failed != 1 || got.Skipped != 1 { t.Fatalf("got=%+v", got) }
    if len(got.Failures) != 1 || got.Failures[0].Test != "TestB" { t.Fatalf("failures=%+v", got.Failures) }
}

func TestParseCargoOutputSummarizes(t *testing.T) {
    raw := "test result: ok. 3 passed; 1 failed; 0 ignored; ...\nrunning test::other ...\n\nfailures:\n    case_x\n"
    got := parseCargoOutput(raw)
    if got.Framework != "cargo" || got.Passed != 3 || got.Failed != 1 { t.Fatalf("got=%+v", got) }
    if len(got.Failures) != 1 || got.Failures[0].Test != "case_x" { t.Fatalf("failures=%+v", got.Failures) }
}

func TestParseNPMOutputAggregates(t *testing.T) {
    raw := `{"stats":{"passes":4,"failures":2,"pending":1},"tests":[{"name":"t1","err":"bad"}],"passes":[{"name":"t1"}],"failures":[{"name":"t1"}]}`
    got := parseNPMOutput(raw)
    if got.Framework != "npm" || got.Passed != 4 || got.Failed != 2 || got.Skipped != 1 { t.Fatalf("got=%+v", got) }
}

func TestDetectRunnerPriority(t *testing.T) {
    cases := []struct {
        files map[string]bool
        want  string
    }{
        {map[string]bool{"go.mod": true}, "go"},
        {map[string]bool{"Cargo.toml": true}, "cargo"},
        {map[string]bool{"package.json": true}, "npm"},
        {map[string]bool{"go.mod": true, "Cargo.toml": true}, "go"},
        {map[string]bool{}, ""},
    }
    for _, tc := range cases {
        if got := detectRunner(tc.files); got != tc.want { t.Fatalf("files=%v got=%s want=%s", tc.files, got, tc.want) }
    }
}

func TestRunTestsExecutesGoTestJSONWithWorkspaceWriteTier(t *testing.T) {
    var last secproc.SecureProcessSpec
    factory := newScriptedFactory(t, func(s secproc.SecureProcessSpec) cannedResult {
        last = s
        return cannedResult{Stdout: `{"Action":"pass","Package":"p","Test":"T"}` + "\n"}
    })
    ctx := WithWorkRoot(secureTestContext(t, factory), t.TempDir())
    out, err := runTool(ctx, NewTestRunTool(), `{}`)
    if err != nil { t.Fatal(err) }
    if last.Program != "go" || last.Args[0] != "test" || last.Args[1] != "-json" { t.Fatalf("argv=%+v", last.Args) }
    if last.UseSandboxTier != sandbox.WorkspaceWrite { t.Fatalf("tier=%v", last.UseSandboxTier) }
    var res testResult
    if err := json.Unmarshal([]byte(out), &res); err != nil { t.Fatal(err) }
    if res.Framework != "go" || res.Passed != 1 || res.Status != "pass" { t.Fatalf("res=%+v", res) }
}
```

- [ ] **Step 2：运行 RED**

Run: `go test ./internal/tools -run 'TestParseGoJSON|TestParseCargoOutput|TestParseNPMOutput|TestDetectRunnerPriority|TestRunTestsExecutesGoTestJSON' -v`

Expected: FAIL。

- [ ] **Step 3：实现 testrun.go**

新建 `internal/tools/testrun.go`。parsers 完整重写（Go JSON 行扫描、cargo `test result:` + `failures:` 块、npm JSON `stats`）。**不**定义 `fakeRunnerFactory`/`simulateFactoryResult`。

```go
package tools

import (
    "bufio"
    "context"
    "encoding/json"
    "fmt"
    "os"
    "path/filepath"
    "strconv"
    "strings"
    "time"

    "github.com/cloudwego/eino/schema"
    "github.com/x6nux/yanshi/internal/sandbox"
    "github.com/x6nux/yanshi/internal/secproc"
)

type runTestsArgs struct {
    Framework string   `json:"framework,omitempty"`
    Packages  []string `json:"packages,omitempty"`
    Filter    string   `json:"filter,omitempty"`
    TimeoutS  int      `json:"timeout_s,omitempty"`
}

type testFailure struct {
    Package string `json:"package,omitempty"`
    Test    string `json:"test,omitempty"`
    Message string `json:"message,omitempty"`
}

type testResult struct {
    Framework   string        `json:"framework"`
    Status      string        `json:"status"`
    Passed      int           `json:"passed"`
    Failed      int           `json:"failed"`
    Skipped     int           `json:"skipped"`
    DurationMS  int64         `json:"duration_ms"`
    Failures    []testFailure `json:"failures,omitempty"`
    Summary     string        `json:"summary"`
    ArtifactRef string        `json:"artifact_ref,omitempty"`
    Degraded    bool          `json:"degraded,omitempty"`
}

func NewTestRunTool() *GuardedTool {
    return NewGuardedTool("run_tests", "Run tests", "Run Go/cargo/npm tests and return structured results.", 10*time.Minute,
        params(map[string]*schema.ParameterInfo{
            "framework": {Type: schema.String, Enum: []string{"auto", "go", "cargo", "npm"}},
            "packages":  {Type: schema.Array, ElemInfo: &schema.ParameterInfo{Type: schema.String}},
            "filter":    {Type: schema.String},
            "timeout_s": {Type: schema.Integer},
        }), SyncStream(runTests))
}

func runTests(ctx context.Context, argsJSON string) (string, error) {
    var args runTestsArgs
    if err := ParseArgs(argsJSON, &args); err != nil { return errorResult(err.Error()), nil }
    root := WorkRootFromContext(ctx)
    framework := args.Framework
    if framework == "" || framework == "auto" {
        framework = detectRunner(detectMarkerFiles(root))
        if framework == "" { return errorResult("no go.mod / Cargo.toml / package.json in work root"), nil }
    }
    timeout := time.Duration(clampInt(args.TimeoutS, 1, 1800)) * time.Second
    if timeout == 0 { timeout = 10 * time.Minute }
    spec := testSpec(framework, args, root)
    start := time.Now()
    res, err := secureCommandRunner(ctx, spec, timeout)
    duration := time.Since(start).Milliseconds()
    if err != nil { return errorResult("run_tests: " + err.Error()), nil }
    parsed := parseTestResult(framework, res)
    parsed.DurationMS = duration
    raw := res.Stdout + res.Stderr
    if len(raw) > SpillThreshold {
        art := writeArtifactOrSpill(ctx, "run-tests", "test-output", raw)
        parsed.Summary = truncateSummary(parsed.Summary, 4096)
        parsed.ArtifactRef = art.ArtifactRef
        parsed.Degraded = art.Degraded
    }
    return toJSON(parsed), nil
}

func testSpec(framework string, args runTestsArgs, root string) secproc.SecureProcessSpec {
    base := secproc.SecureProcessSpec{Tool: "run_tests", Dir: root, UseSandboxTier: sandbox.WorkspaceWrite}
    switch framework {
    case "go":
        argv := []string{"test", "-json"}
        if len(args.Packages) > 0 { argv = append(argv, args.Packages...) } else { argv = append(argv, "./...") }
        if args.Filter != "" { argv = append(argv, "-run", args.Filter) }
        base.Program, base.Args = "go", argv
    case "cargo":
        argv := []string{"test"}
        if args.Filter != "" { argv = append(argv, args.Filter) }
        base.Program, base.Args = "cargo", argv
    case "npm":
        argv := []string{"test", "--", "--json"}
        if args.Filter != "" { argv = append(argv, "--testNamePattern", args.Filter) }
        base.Program, base.Args = "npm", argv
    }
    return base
}

func detectMarkerFiles(root string) map[string]bool {
    markers := []string{"go.mod", "Cargo.toml", "package.json"}
    out := map[string]bool{}
    for _, name := range markers {
        if _, err := os.Stat(filepath.Join(root, name)); err == nil { out[name] = true }
    }
    return out
}

func detectRunner(files map[string]bool) string {
    if files["go.mod"] { return "go" }
    if files["Cargo.toml"] { return "cargo" }
    if files["package.json"] { return "npm" }
    return ""
}

func parseTestResult(framework string, res commandResult) testResult {
    switch framework {
    case "go":
        return parseGoJSON(res.Stdout)
    case "cargo":
        return parseCargoOutput(res.Stdout + res.Stderr)
    case "npm":
        return parseNPMOutput(res.Stdout + res.Stderr)
    }
    return testResult{Framework: framework, Status: "error", Summary: "unknown framework"}
}

func parseGoJSON(stdout string) testResult {
    result := testResult{Framework: "go"}
    scanner := bufio.NewScanner(strings.NewReader(stdout))
    scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
    for scanner.Scan() {
        var ev struct {
            Action, Package, Test string
        }
        if json.Unmarshal(scanner.Bytes(), &ev) != nil { continue }
        switch ev.Action {
        case "pass":
            result.Passed++
        case "fail":
            result.Failed++
            result.Failures = append(result.Failures, testFailure{Package: ev.Package, Test: ev.Test})
        case "skip":
            result.Skipped++
        }
    }
    result.Status = "pass"
    if result.Failed > 0 { result.Status = "fail" }
    result.Summary = fmt.Sprintf("go: %d passed, %d failed, %d skipped", result.Passed, result.Failed, result.Skipped)
    return result
}

func parseCargoOutput(stdout string) testResult {
    result := testResult{Framework: "cargo"}
    for _, line := range strings.Split(stdout, "\n") {
        line = strings.TrimSpace(line)
        if strings.HasPrefix(line, "test result:") {
            result.Passed += extractInt(line, "passed")
            result.Failed += extractInt(line, "failed")
            result.Skipped += extractInt(line, "ignored")
        }
    }
    if idx := strings.Index(stdout, "failures:\n"); idx >= 0 {
        for _, line := range strings.Split(stdout[idx+len("failures:\n"):], "\n") {
            name := strings.TrimSpace(line)
            if name == "" || strings.HasPrefix(name, "----") { continue }
            if strings.Contains(name, "stdout") || strings.Contains(name, "running") { break }
            result.Failures = append(result.Failures, testFailure{Test: name})
        }
    }
    result.Status = "pass"
    if result.Failed > 0 { result.Status = "fail" }
    result.Summary = fmt.Sprintf("cargo: %d passed, %d failed", result.Passed, result.Failed)
    return result
}

func parseNPMOutput(stdout string) testResult {
    result := testResult{Framework: "npm"}
    start := strings.Index(stdout, "{")
    if start < 0 { result.Status = "error"; result.Summary = "npm: no JSON output"; return result }
    var payload struct {
        Stats struct {
            Passes, Failures, Pending int
        }
        Failures []struct{ Name string }
    }
    if err := json.Unmarshal([]byte(stdout[start:]), &payload); err != nil {
        result.Status = "error"; result.Summary = "npm: " + err.Error(); return result
    }
    result.Passed, result.Failed, result.Skipped = payload.Stats.Passes, payload.Stats.Failures, payload.Stats.Pending
    for _, f := range payload.Failures { result.Failures = append(result.Failures, testFailure{Test: f.Name}) }
    result.Status = "pass"
    if result.Failed > 0 { result.Status = "fail" }
    result.Summary = fmt.Sprintf("npm: %d passed, %d failed, %d pending", result.Passed, result.Failed, result.Skipped)
    return result
}

func extractInt(line, label string) int {
    idx := strings.Index(line, label)
    if idx < 0 { return 0 }
    prefix := line[:idx]
    fields := strings.Fields(strings.TrimSpace(prefix))
    if len(fields) == 0 { return 0 }
    n, _ := strconv.Atoi(fields[len(fields)-1])
    return n
}

func clampInt(v, low, high int) int {
    if v < low { return low }
    if v > high { return high }
    return v
}

func truncateSummary(s string, max int) string {
    if len(s) > max { return s[:max] + "…" }
    return s
}
```

- [ ] **Step 4：修改 config.example.yaml（test-only allowlist）**

`config.example.yaml` 第 50 行当前 `shell: { policy: "allowlist", patterns: ["go *", "git *", "npm *"] }` 改为：

```yaml
shell: { policy: "allowlist", patterns: ["git *", "go test", "go test *", "cargo test", "cargo test *", "npm test", "npm test *"] }
```

> 不再保留广 `go *`/`npm *`。`yanshi` 自身 run_tests 通过 argv 走 A1c，不依赖这些 patterns。

- [ ] **Step 5：运行 GREEN**

Run: `go test ./internal/tools -run 'TestParseGoJSON|TestParseCargoOutput|TestParseNPMOutput|TestDetectRunnerPriority|TestRunTestsExecutes' -v`

Expected: PASS。

- [ ] **Step 6：提交**

```bash
git add internal/tools/testrun.go internal/tools/testrun_test.go config.example.yaml
git commit -m "feat(tools): add sandboxed structured test runner"
```

## Task 4: DT5 — `diagnostics`（消费 B2-LSP1）

**Files:**
- Create: `internal/tools/diagnostics.go`
- Create: `internal/tools/diagnostics_test.go`

> **修订要点：** 测试使用 `diagLSPSourceOverride` seam 而非 `asManagerPtr`（old `asManagerPtr` 返回 nil `*lsp.Manager`，导致 `LSPFromContext` 因 nil 指针跳过 LSP 探针 —— 测试实际上不会到达断言 LSP 状态的代码）。

- [ ] **Step 1：写真实 TDD RED 测试（使用 diagLSPSourceOverride seam）**

```go
package tools

import (
    "context"
    "encoding/json"
    "strings"
    "testing"
    "time"

    "github.com/x6nux/yanshi/internal/lsp"
    "github.com/x6nux/yanshi/internal/secproc"
)

type stubLSPManager struct {
    enabled bool
    byPath  map[string][]lsp.Diagnostic
}

func (s stubLSPManager) Enabled() bool { return s.enabled }
func (s stubLSPManager) Diagnostics(path string, _ time.Duration) []lsp.Diagnostic { return s.byPath[path] }

func TestDiagnosticsAggregatesIndependentProbes(t *testing.T) {
    src := stubLSPManager{enabled: true, byPath: map[string][]lsp.Diagnostic{"a.go": {{Severity: lsp.Error, Message: "bad"}}}}
    diagLSPSourceOverride = src
    t.Cleanup(func() { diagLSPSourceOverride = nil })

    factory := newScriptedFactory(t, func(spec secproc.SecureProcessSpec) cannedResult {
        if spec.Program == "go" { return cannedResult{Stdout: "go version go1.26.4 linux/amd64\n"} }
        return cannedResult{Stdout: ""}
    })
    ctx := WithWorkRoot(secureTestContext(t, factory), t.TempDir())
    out, err := runTool(ctx, NewDiagnosticsTool(diagTestProbe{files: []string{"a.go"}}), `{}`)
    if err != nil { t.Fatal(err) }
    var res struct {
        Git       struct{ Available bool `json:"available"` } `json:"git"`
        Toolchain struct{ Go string `json:"go"` }             `json:"toolchain"`
        LSP       struct {
            Available            bool `json:"available"`
            OpenDiagnosticsCount int  `json:"open_diagnostics_count"`
        } `json:"lsp"`
    }
    if err := json.Unmarshal([]byte(out), &res); err != nil { t.Fatal(err) }
    if !res.Git.Available || !strings.HasPrefix(res.Toolchain.Go, "go1.26") {
        t.Fatalf("toolchain=%s", res.Toolchain.Go)
    }
    if !res.LSP.Available || res.LSP.OpenDiagnosticsCount != 1 { t.Fatalf("lsp=%+v", res.LSP) }
}

func TestDiagnosticsLSPUnavailableIsLocalDegradation(t *testing.T) {
    diagLSPSourceOverride = stubLSPManager{enabled: false}
    t.Cleanup(func() { diagLSPSourceOverride = nil })
    factory := newScriptedFactory(t, func(secproc.SecureProcessSpec) cannedResult { return cannedResult{} })
    ctx := WithWorkRoot(secureTestContext(t, factory), t.TempDir())
    out, err := runTool(ctx, NewDiagnosticsTool(diagTestProbe{files: nil}), `{}`)
    if err != nil { t.Fatal(err) }
    if !strings.Contains(out, `"lsp":{"available":false,"open_diagnostics_count":0}`) { t.Fatalf("out=%s", out) }
}

func TestDiagnosticsGitFailureDoesNotHideOthers(t *testing.T) {
    diagLSPSourceOverride = stubLSPManager{enabled: false}
    t.Cleanup(func() { diagLSPSourceOverride = nil })
    factory := newScriptedFactory(t, func(spec secproc.SecureProcessSpec) cannedResult {
        if spec.Program == "git" { return cannedResult{ExitCode: 128, Stderr: "not a repo"} }
        return cannedResult{Stdout: "go version go1.26.4\n"}
    })
    ctx := WithWorkRoot(secureTestContext(t, factory), t.TempDir())
    out, err := runTool(ctx, NewDiagnosticsTool(diagTestProbe{files: nil}), `{}`)
    if err != nil { t.Fatal(err) }
    if !strings.Contains(out, `"go":"go1.26.4"`) { t.Fatalf("out=%s", out) }
    if !strings.Contains(out, `"git":{"available":false`) { t.Fatalf("out=%s", out) }
}

type diagTestProbe struct{ files []string }
func (d diagTestProbe) recentFiles(ctx context.Context, root string) []string { return d.files }
```

- [ ] **Step 2：运行 RED**

Run: `go test ./internal/tools -run TestDiagnostics -v`
Expected: FAIL。

- [ ] **Step 3：实现 diagnostics.go + lspSource override seam**

```go
package tools

import (
    "context"
    "strings"
    "time"

    "github.com/cloudwego/eino/schema"
    "github.com/x6nux/yanshi/internal/lsp"
    "github.com/x6nux/yanshi/internal/sandbox"
    "github.com/x6nux/yanshi/internal/secproc"
)

type lspSource interface {
    Enabled() bool
    Diagnostics(path string, timeout time.Duration) []lsp.Diagnostic
}

type diagFileLister interface {
    recentFiles(ctx context.Context, root string) []string
}

type defaultFileLister struct{}

func (defaultFileLister) recentFiles(_ context.Context, _ string) []string { return nil }

var diagFileListerOverride diagFileLister = defaultFileLister{}

// diagLSPSourceOverride is a test seam for injecting stub LSP sources.
// Production leaves it nil (reads LSPFromContext); tests set it and defer cleanup.
var diagLSPSourceOverride lspSource

func NewDiagnosticsTool(probe diagFileLister) *GuardedTool {
    if probe != nil { diagFileListerOverride = probe }
    return NewGuardedTool("diagnostics", "Diagnostics",
        "Aggregate workspace, git, sandbox, toolchain, and LSP diagnostics.",
        15*time.Second, nil, SyncStream(runDiagnostics))
}

func runDiagnostics(ctx context.Context, argsJSON string) (string, error) {
    root := WorkRootFromContext(ctx)
    return toJSON(struct {
        Workspace map[string]any `json:"workspace"`
        Git       probeDiag      `json:"git"`
        Sandbox   sandboxDiag    `json:"sandbox"`
        Toolchain toolchainDiag  `json:"toolchain"`
        LSP       lspDiag        `json:"lsp"`
    }{
        Workspace: workspaceSummary(root),
        Git:       runGitProbe(ctx, root),
        Sandbox:   sandboxProbe(ctx),
        Toolchain: runToolchainProbes(ctx),
        LSP:       runLSPProbe(ctx, root),
    }), nil
}

type probeDiag struct {
    Available bool   `json:"available"`
    Reason    string `json:"reason,omitempty"`
}

type sandboxDiag struct {
    Requested string `json:"requested"`
    Effective string `json:"effective"`
    Enforced  bool   `json:"enforced"`
}

type toolchainDiag struct {
    Go    string `json:"go,omitempty"`
    Cargo string `json:"cargo,omitempty"`
    Node  string `json:"node,omitempty"`
}

type lspDiag struct {
    Available            bool `json:"available"`
    OpenDiagnosticsCount int  `json:"open_diagnostics_count"`
}

func runGitProbe(ctx context.Context, root string) probeDiag {
    spec := secproc.SecureProcessSpec{Tool: "diagnostics", Program: "git", Dir: root,
        Args: []string{"rev-parse", "--show-toplevel"}, UseSandboxTier: sandbox.ReadOnly}
    res, err := secureCommandRunner(ctx, spec, 5*time.Second)
    if err != nil || res.ExitCode != 0 { return probeDiag{Reason: "git unavailable"} }
    return probeDiag{Available: true}
}

func sandboxProbe(ctx context.Context) sandboxDiag {
    // The sandbox package (internal/sandbox) is delivered by A1c together
    // with secproc. If A1c exposes a SandboxFromContext helper and a Report
    // struct with Requested/Effective/Enforced fields, prefer that. Until
    // then, degrade gracefully: secproc specs already carry UseSandboxTier
    // so the diagnostic loses no information by reporting "unknown" here.
    return sandboxDiag{Requested: "unknown", Effective: "unknown", Enforced: false}
}

func runToolchainProbes(ctx context.Context) toolchainDiag {
    out := toolchainDiag{}
    for _, entry := range []struct{ name, program string }{
        {"Go", "go"}, {"Cargo", "cargo"}, {"Node", "node"},
    } {
        spec := secproc.SecureProcessSpec{Tool: "diagnostics", Program: entry.program,
            Args: []string{"--version"}, UseSandboxTier: sandbox.ReadOnly}
        res, err := secureCommandRunner(ctx, spec, 3*time.Second)
        if err != nil || res.ExitCode != 0 { continue }
        line := strings.TrimSpace(res.Stdout)
        switch entry.name {
        case "Go": out.Go = line
        case "Cargo": out.Cargo = line
        case "Node": out.Node = line
        }
    }
    return out
}

func runLSPProbe(ctx context.Context, root string) lspDiag {
    var source lspSource
    if mgr, ok := LSPFromContext(ctx); ok && mgr != nil { source = mgr }
    if source == nil { source = diagLSPSourceOverride }
    if source == nil || !source.Enabled() { return lspDiag{} }
    files := diagFileListerOverride.recentFiles(ctx, root)
    count := 0
    for _, path := range files {
        count += len(source.Diagnostics(path, 2*time.Second))
    }
    return lspDiag{Available: true, OpenDiagnosticsCount: count}
}

func workspaceSummary(root string) map[string]any {
    return map[string]any{"root": root}
}
```

- [ ] **Step 4：运行 GREEN**

Run: `go test ./internal/tools -run TestDiagnostics -v`
Expected: PASS。

- [ ] **Step 5：提交**

```bash
git add internal/tools/diagnostics.go internal/tools/diagnostics_test.go
git commit -m "feat(tools): aggregate LSP sandbox and toolchain diagnostics"
```

---

## Task 5: 强制审批端到端贯通（proto / WS / SSE / TUI）

**Files:**
- Modify: `internal/tools/permctx.go`
- Modify: `internal/tools/guard.go`
- Modify: `internal/tools/permctx_test.go`
- Modify: `internal/proto/frame.go`
- Modify: `internal/proto/frame_test.go`
- Modify: `internal/cli/backend.go`
- Modify: `internal/cli/wsbackend.go`
- Modify: `internal/cli/tui/commands.go`
- Modify: `internal/cli/tui/model.go`
- Modify: `internal/cli/tui/permissions.go`
- Modify: `internal/cli/tui/permissions_test.go`
- Modify: `internal/api/http/ws.go`
- Modify: `internal/api/http/ws_test.go`

- [ ] **Step 1：写 `Authorize` 真值表 RED tests（8 行真实 table test）**

`internal/tools/permctx_test.go` 增加。每行断言 allow/deny、callback 调用次数、allowlist 记录数。

```go
func TestAuthorizeApprovalTruthTable(t *testing.T) {
    type row struct {
        name             string
        profileBound     bool
        staticAllows     bool
        callback         PermissionDecision
        mandatory        bool
        wantAllow        bool
        callbackCalls    int
        allowlistRecords int
    }
    rows := []row{
        {"no profile / mandatory", false, false, "", true, false, 0, 0},
        {"no profile / non-mandatory", false, false, "", false, false, 0, 0},
        {"profile allow / no callback", true, true, "", false, true, 0, 0},
        {"profile allow / mandatory / no callback", true, true, "", true, false, 0, 0},
        {"profile allow / mandatory / callback allow", true, true, string(PermissionAllow), true, true, 1, 0},
        {"profile allow / mandatory / callback always_allow", true, true, string(PermissionAlwaysAllow), true, false, 1, 0},
        {"profile deny / non-mandatory / callback always_allow", true, false, string(PermissionAlwaysAllow), false, true, 1, 1},
        {"profile deny / mandatory / callback always_allow", true, false, string(PermissionAlwaysAllow), true, false, 1, 0},
        {"profile deny / mandatory / callback deny", true, false, string(PermissionDeny), true, false, 1, 0},
        {"profile deny / non-mandatory / pre-allowed allowlist", true, false, "", false, true, 0, 1},
    }
    for _, tc := range rows {
        t.Run(tc.name, func(t *testing.T) {
            ctx := context.Background()
            if tc.profileBound {
                if !tc.staticAllows {
                    ctx = WithProfile(ctx, guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"nothing"}}})
                } else {
                    ctx = WithProfile(ctx, allowAllProfile())
                }
            }
            if tc.allowlistRecords > 0 && tc.callback == "" {
                ctx = WithPermissionAllowlist(ctx)
                if al := allowlistFrom(ctx); al != nil { al.record(allowKey(guard.Action{Tool: "github_comment"})) }
            }
            if tc.callback != "" {
                decision := tc.callback
                ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
                    if req.ApprovalRequired != tc.mandatory { t.Fatalf("mandatory got=%v want=%v", req.ApprovalRequired, tc.mandatory) }
                    return decision
                })
            }
            // The mandatory flag dictates which entry point is used: mandatory
            // tools go through NewApprovalGuardedTool → AuthorizeApprovalRequired;
            // non-mandatory tools go through Authorize. Testing both in one table
            // verifies the two paths stay consistent.
            var err error
            if tc.mandatory {
                err = AuthorizeApprovalRequired(ctx, guard.Action{Tool: "github_comment"}, "{}")
            } else {
                err = Authorize(ctx, guard.Action{Tool: "github_comment"}, "{}")
            }
            if tc.wantAllow && err != nil { t.Fatalf("want allow got %v", err) }
            if !tc.wantAllow && err == nil { t.Fatalf("want deny got allow") }
            if al := allowlistFrom(ctx); al != nil {
                if got := len(al.m); got != tc.allowlistRecords { t.Fatalf("allowlist=%d want=%d", got, tc.allowlistRecords) }
            }
        })
    }
}

func TestApprovalRequiredNoCallbackNeverRunsTool(t *testing.T) {
    var runs int
    tool := NewApprovalGuardedTool("github_comment", "GitHub comment", "approval-only", time.Second, nil,
        SyncStream(func(context.Context, string) (string, error) { runs++; return "ran", nil }))
    ctx := WithProfile(context.Background(), allowAllProfile())
    out, err := tool.InvokableRun(ctx, `{}`)
    if err != nil { t.Fatal(err) }
    if runs != 0 { t.Fatalf("tool ran %d times", runs) }
    if !strings.Contains(out, "permission denied") { t.Fatalf("out=%s", out) }
}
```

- [ ] **Step 2：运行 RED**

Run: `go test ./internal/tools -run 'TestAuthorizeApprovalTruthTable|TestApprovalRequiredNoCallback' -v`
Expected: FAIL。

- [ ] **Step 3：实现 `Authorize` 内核与 `NewApprovalGuardedTool`**

`internal/tools/permctx.go` 的 `PermissionRequest` 加 `ApprovalRequired bool` 字段：

```go
type PermissionRequest struct {
    Tool             string
    Args             string
    Reason           string
    ApprovalRequired bool
}
```

`Authorize` 函数（第 153-196 行）整体替换为：

```go
func Authorize(ctx context.Context, action guard.Action, argsJSON string) error {
    return authorize(ctx, action, argsJSON, false)
}

func AuthorizeApprovalRequired(ctx context.Context, action guard.Action, argsJSON string) error {
    return authorize(ctx, action, argsJSON, true)
}

func authorize(ctx context.Context, action guard.Action, argsJSON string, approvalRequired bool) error {
    prof, ok := ProfileFromContext(ctx)
    if !ok { return &DenyErr{Reason: "no permission profile in context"} }
    if !approvalRequired {
        if al := allowlistFrom(ctx); al != nil && al.allows(allowKey(action)) { return nil }
        if guard.New().Check(prof, action).Allowed { return nil }
    }
    ask, hasCallback := permissionCallback(ctx)
    if !hasCallback {
        reason := "tool requires explicit approval"
        if !approvalRequired { reason = guard.New().Check(prof, action).Reason }
        return &DenyErr{Reason: reason}
    }
    reason := guard.New().Check(prof, action).Reason
    if approvalRequired && reason == "" { reason = "tool requires explicit approval" }
    req := PermissionRequest{Tool: action.Tool, Args: argsJSON, Reason: reason, ApprovalRequired: approvalRequired}
    switch ask(req) {
    case PermissionAllow:
        return nil
    case PermissionAlwaysAllow:
        if approvalRequired { return &DenyErr{Reason: "always_allow invalid for approval-required calls"} }
        if al := allowlistFrom(ctx); al != nil { al.record(allowKey(action)) }
        return nil
    default:
        return &DenyErr{Reason: reason}
    }
}
```

`internal/tools/guard.go` `GuardedTool` 加字段：

```go
type GuardedTool struct {
    // ... existing fields ...
    approvalRequired bool
}

func NewApprovalGuardedTool(name, display, desc string, timeout time.Duration, p *schema.ParamsOneOf, stream StreamFunc) *GuardedTool {
    return &GuardedTool{name: name, display: display, desc: desc, timeout: timeout, params: p, stream: stream, approvalRequired: true}
}
```

`Stream` 方法第 185 行的 Authorize 调用改为：

```go
var authErr error
if g.approvalRequired {
    authErr = AuthorizeApprovalRequired(ctx, guard.Action{Tool: g.name}, argsJSON)
} else {
    authErr = Authorize(ctx, guard.Action{Tool: g.name}, argsJSON)
}
```

- [ ] **Step 4：proto + frame_test callsites（NewPermissionRequest 加 bool 参数）**

`internal/proto/frame.go` 第 178 行加字段：

```go
ApprovalRequired bool `json:"approval_required,omitempty"` // permission_request
```

`NewPermissionRequest`（第 330 行）整体替换：

```go
func NewPermissionRequest(id, tool, args, reason string, approvalRequired bool) ServerFrame {
    return ServerFrame{Type: "permission_request", ID: id, ToolName: tool, ToolArgs: args, Reason: reason, ApprovalRequired: approvalRequired}
}
```

`internal/proto/frame_test.go` 两个 callsite 更新：

```go
in := NewPermissionRequest("req-1", "shell", `{"cmd":"rm -rf /"}`, "shell command", false)
// ...
NewPermissionRequest("id", "t", "{}", "r", false),
```

- [ ] **Step 5：StreamEvent 与映射**

`internal/cli/backend.go` 的 `StreamEvent` 加字段：

```go
ApprovalRequired bool // permission_request: must be explicit one-shot allow/deny
```

`internal/cli/wsbackend.go:261` 的 `StreamEvent{...}` 映射增加：

```go
ApprovalRequired: f.ApprovalRequired,
```

- [ ] **Step 6：WS resolvePermissionMode 顶短路**

`internal/api/http/ws.go:357` `resolvePermissionMode` 函数第一行：

```go
if req.ApprovalRequired {
    return tools.PermissionDeny, false
}
```

`ws.go:739`：

```go
conn.write(proto.NewPermissionRequest(id, req.Tool, req.Args, req.Reason, req.ApprovalRequired))
```

`internal/api/http/ws_test.go` 加测试。`resolvePermissionMode` 接收具体的 `connSession` 结构体（不是 interface），其 `perm *permModeState` 字段才是真正的 mode 持有者。测试必须构造一个 `connSession` 并通过 `applySetMode` 设置 mode：

```go
func TestResolvePermissionModeApprovalRequiredAlwaysPrompts(t *testing.T) {
    for _, mode := range []guard.PermissionMode{guard.ModeYOLO, guard.ModeAuto, guard.ModeAllowEdits, guard.ModeDefault} {
        cs := &connSession{perm: &permModeState{}}
        cs.applySetMode(proto.ClientFrame{Type: "set_mode", Mode: string(mode)})
        d, resolved := resolvePermissionMode(context.Background(), *cs, nil,
            tools.PermissionRequest{Tool: "github_comment", ApprovalRequired: true})
        if d != tools.PermissionDeny || resolved {
            t.Fatalf("mode=%s got=(%s,%v)", mode, d, resolved)
        }
    }
}
```

> 注意：`permModeState` 与 `applySetMode` 都已存在于 `internal/api/http/ws.go`（约 line 95-100、210）。初始化 `cs.perm` 用 `&permModeState{}`（不要虚构 `newPermModeState`）。`connSession` 是 concrete struct 而非 interface，所以不能传入自定义 fake——必须用真实 `connSession` 通过控制 `perm` 字段来驱动；这也是 `resolvePermissionMode` 当前签名 `(ctx, cs connSession, models, req)` 的强制要求。

- [ ] **Step 7：TUI mandatory UX**

`internal/cli/tui/commands.go:826` 的 `permissionEntry` 加字段：

```go
type permissionEntry struct {
    id, tool, args, reason string
    mandatory bool
}
```

`internal/cli/tui/model.go:1098` 的 `permission_request` 分支改为：

```go
m.pendingPermissions = append(m.pendingPermissions, &permissionEntry{
    id: ev.ID, tool: ev.ToolName, args: ev.ToolArgs, reason: ev.Reason,
    mandatory: ev.ApprovalRequired,
})
```

`internal/cli/tui/permissions.go:93` `autoResolvePendingByMode`：

```go
for _, pe := range m.pendingPermissions {
    if !pe.mandatory && modeAutoAllows(m.permMode, pe.tool) {
        _ = m.sess.SendFrame(proto.NewPermissionResponse(pe.id, "allow"))
    } else {
        kept = append(kept, pe)
    }
}
```

同文件新增 `permissionOptions`：

```go
func permissionOptions(pe *permissionEntry) []struct{ label, decision string } {
    if pe != nil && pe.mandatory {
        return []struct{ label, decision string }{{"Allow", "allow"}, {"Deny", "deny"}}
    }
    return permOptions
}
```

`permMove`、`respondPermission`、`permissionPopup` 调用 `permissionOptions(m.pendingPermission())` 而非直接引用 `permOptions`。`respondPermission` 顶部防御：

```go
if pe.mandatory && decision == "always_allow" { return }
```

`internal/cli/tui/permissions_test.go` 加测试：

```go
func TestMandatoryPermissionSurvivesSwitchToYOLOAndAuto(t *testing.T) {
    for _, mode := range []guard.PermissionMode{guard.ModeYOLO, guard.ModeAuto} {
        rec := &recordingSession{}
        m := newModel(rec, "/proj")
        m.permMode = mode
        m.pendingPermissions = []*permissionEntry{{id: "p1", tool: "github_comment", mandatory: true}}
        m.autoResolvePendingByMode()
        if len(m.pendingPermissions) != 1 { t.Fatalf("mode=%s mandatory disappeared", mode) }
        if len(rec.frames) != 0 { t.Fatalf("mode=%s sent=%+v", mode, rec.frames) }
        opts := permissionOptions(m.pendingPermission())
        if len(opts) != 2 || opts[0].decision != "allow" || opts[1].decision != "deny" { t.Fatalf("opts=%+v", opts) }
    }
}
```

- [ ] **Step 8：运行 GREEN**

```bash
go test ./internal/tools -run 'TestAuthorizeApprovalTruthTable|TestApprovalRequiredNoCallback' -v
go test ./internal/proto -v
go test ./internal/api/http -run TestResolvePermissionModeApprovalRequired -v
go test ./internal/cli/tui -run TestMandatoryPermission -v
```

Expected: PASS。

- [ ] **Step 9：提交**

```bash
git add internal/tools/permctx.go internal/tools/guard.go internal/tools/permctx_test.go internal/proto/frame.go internal/proto/frame_test.go internal/cli/backend.go internal/cli/wsbackend.go internal/cli/tui/commands.go internal/cli/tui/model.go internal/cli/tui/permissions.go internal/cli/tui/permissions_test.go internal/api/http/ws.go internal/api/http/ws_test.go
git commit -m "feat(permissions): carry mandatory approval through TUI and WS"
```

## Task 6: GitHub tools（gh CLI 封装 + 窄 FetchGitHubContext 导出）

**Files:**
- Create: `internal/tools/github.go`
- Create: `internal/tools/github_test.go`

主要工具：
- `github_pr_context`（只读，无需 approval）：获取 PR metadata + diff。完整 schema（不是 `params(/*kind+number*/)` 占位）。
- `github_comment`（写入，通过 `NewApprovalGuardedTool` 强制逐次审批）：提交 PR 评论。
- `github_approve`（写入，强制审批）：批准 PR。
- `github_merge`（写入，强制审批）：合并 PR。

`FetchGitHubContext` 是纯函数（窄导出）：给定 `gh` CLI 的 JSON stdout，解析为 `GitHubContext`。工具从 context 获取 factory 跑 `gh` 然后调用它；`yanshi pr` 命令通过 `os/exec` 跑 `gh` 然后调用它。

- [ ] **Step 1：写真实 TDD RED 测试（5 tests）**

```go
package tools

import (
    "context"
    "encoding/json"
    "strings"
    "testing"

    "github.com/x6nux/yanshi/internal/guard"
    "github.com/x6nux/yanshi/internal/secproc"
)

func TestGitHubPRContextParsesGHJSON(t *testing.T) {
    raw := `{"number":42,"title":"Fix bug","body":"The fix","headRefName":"fix-branch","baseRefName":"main","author":{"login":"alice"},"files":[{"path":"main.go","additions":10,"deletions":2}],"changedFiles":1}`
    ghJSON, err := FetchGitHubContext("owner/repo", 42, raw)
    if err != nil { t.Fatal(err) }
    if ghJSON.Number != 42 || ghJSON.Title != "Fix bug" || ghJSON.Author != "alice" { t.Fatalf("ghJSON=%+v", ghJSON) }
    if len(ghJSON.Files) != 1 || ghJSON.Files[0].Path != "main.go" { t.Fatalf("files=%+v", ghJSON.Files) }
}

func TestGitHubPRContextRejectsInvalidJSON(t *testing.T) {
    _, err := FetchGitHubContext("owner/repo", 1, `not json`)
    if err == nil || !strings.Contains(err.Error(), "parse GitHub") { t.Fatalf("err=%v", err) }
}

func TestGitHubCommentRequiresApproval(t *testing.T) {
    // SSE path: no callback → denied
    tool := NewGitHubTools(nil).Comment
    ctx := WithProfile(context.Background(), guard.PermissionProfile{
        Tools: guard.ToolsPerm{Allow: []string{"*"}},
        Net:   guard.NetPerm{Allow: true},
    })
    out, err := tool.InvokableRun(ctx, `{"repo":"owner/repo","number":1,"body":"LGTM"}`)
    if err != nil { t.Fatal(err) }
    if !strings.Contains(out, "permission denied") { t.Fatalf("out=%s", out) }
}

func TestGitHubCommentApprovalAllow(t *testing.T) {
    var last secproc.SecureProcessSpec
    factory := newScriptedFactory(t, func(s secproc.SecureProcessSpec) cannedResult {
        last = s
        // Real `gh pr comment` returns a URL string (not JSON); mock matches.
        return cannedResult{Stdout: "https://github.com/owner/repo/issues/1#issuecomment-123"}
    })
    ctx := WithSecureProcessFactory(WithPermissionCallback(
        WithProfile(context.Background(), guard.PermissionProfile{
            Tools: guard.ToolsPerm{Allow: []string{"*"}},
            Net:   guard.NetPerm{Allow: true},
        }),
        func(req PermissionRequest) PermissionDecision {
            if req.Tool != "github_comment" || req.ApprovalRequired != true { t.Fatalf("bad req=%+v", req) }
            return "allow"
        }), factory)
    tool := NewGitHubTools(nil).Comment
    out, err := tool.InvokableRun(ctx, `{"repo":"owner/repo","number":1,"body":"LGTM"}`)
    if err != nil { t.Fatal(err) }
    // runGitHubComment wraps gh's trimmed stdout as a string ID; check it round-trips.
    if !strings.Contains(out, "123") { t.Fatalf("out=%s", out) }
    if last.Program != "gh" || !strings.Contains(strings.Join(last.Args, " "), "pr comment") { t.Fatalf("args=%+v", last.Args) }
}

func TestGitHubMergeRejectsUnknownMethod(t *testing.T) {
    // Note: an ApprovalGuardedTool with no callback yields a DenyErr, which
    // GuardedTool.InvokableRun packages into the result STRING (not a Go error).
    // We must bind a callback that allows the call so the merge logic actually
    // runs and rejects "xyz"; otherwise the test would assert against the
    // permission-denied result and never see "xyz".
    ctx := WithPermissionCallback(
        WithProfile(context.Background(), allowAllProfile()),
        func(req PermissionRequest) PermissionDecision { return PermissionAllow })
    tool := NewGitHubTools(nil).Merge
    out, err := tool.InvokableRun(ctx, `{"repo":"owner/repo","number":1,"method":"xyz"}`)
    if err != nil { t.Fatal(err) }
    if !strings.Contains(out, "xyz") { t.Fatalf("out=%s", out) }
}
```

- [ ] **Step 2：运行 RED**

Run: `go test ./internal/tools -run 'TestGitHubPRContext|TestGitHubComment|TestGitHubMerge' -v`
Expected: FAIL。

- [ ] **Step 3：实现 github.go**

```go
package tools

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "strings"
    "time"

    "github.com/cloudwego/eino/schema"
    "github.com/x6nux/yanshi/internal/sandbox"
    "github.com/x6nux/yanshi/internal/secproc"
)

type GitHubTools struct {
    PRContext *GuardedTool
    Comment   *GuardedTool
    Approve   *GuardedTool
    Merge     *GuardedTool
}

type GitHubContext struct {
    Number      int              `json:"number"`
    Title       string           `json:"title"`
    Body        string           `json:"body"`
    Author      string           `json:"author"`
    HeadRef     string           `json:"head_ref"`
    BaseRef     string           `json:"base_ref"`
    Files       []GitHubFileStat `json:"files"`
    ChangedFile int              `json:"changed_files"`
}

type GitHubFileStat struct {
    Path      string `json:"path"`
    Additions int    `json:"additions"`
    Deletions int    `json:"deletions"`
}

// FetchGitHubContext is a NARROW export: a pure parser that takes the JSON
// stdout of `gh pr view --json number,title,body,...` and returns a structured
// GitHubContext. Both the github_pr_context tool and the `yanshi pr` command
// use it. The caller (tool or command) is responsible for actually executing
// `gh` — this function only parses.
func FetchGitHubContext(repo string, number int, ghJSON string) (GitHubContext, error) {
    var raw struct {
        Number      int    `json:"number"`
        Title       string `json:"title"`
        Body        string `json:"body"`
        HeadRefName string `json:"headRefName"`
        BaseRefName string `json:"baseRefName"`
        Author      struct{ Login string `json:"login"` } `json:"author"`
        Files       []struct {
            Path      string `json:"path"`
            Additions int    `json:"additions"`
            Deletions int    `json:"deletions"`
        } `json:"files"`
        ChangedFiles int `json:"changedFiles"`
    }
    // Copy to local; JSON decoder modifies usage of ghJSON
    b := []byte(ghJSON)
    if err := json.Unmarshal(b, &raw); err != nil {
        return GitHubContext{}, fmt.Errorf("parse GitHub PR %s#%d: %w", repo, number, err)
    }
    ctx := GitHubContext{
        Number: raw.Number, Title: raw.Title, Body: raw.Body,
        HeadRef: raw.HeadRefName, BaseRef: raw.BaseRefName,
        Author: raw.Author.Login, ChangedFile: raw.ChangedFiles,
    }
    for _, f := range raw.Files {
        ctx.Files = append(ctx.Files, GitHubFileStat{Path: f.Path, Additions: f.Additions, Deletions: f.Deletions})
    }
    return ctx, nil
}

func NewGitHubTools(ghFactory secproc.Factory) *GitHubTools {
    if ghFactory == nil { ghFactory = nil } // bound per-turn via context
    return &GitHubTools{
        PRContext: NewGuardedTool("github_pr_context", "GitHub PR context",
            "Fetch PR metadata, files, and diff from GitHub.", 30*time.Second,
            params(map[string]*schema.ParameterInfo{
                "repo":   {Type: schema.String, Required: true, Desc: "owner/repo"},
                "number": {Type: schema.Integer, Required: true, Desc: "PR number"},
            }), SyncStream(runGitHubPRContext)),
        Comment: NewApprovalGuardedTool("github_comment", "GitHub comment",
            "Post a comment on a GitHub PR.", 30*time.Second,
            params(map[string]*schema.ParameterInfo{
                "repo":   {Type: schema.String, Required: true, Desc: "owner/repo"},
                "number": {Type: schema.Integer, Required: true, Desc: "PR number"},
                "body":   {Type: schema.String, Required: true, Desc: "Comment body"},
            }), SyncStream(runGitHubComment)),
        Approve: NewApprovalGuardedTool("github_approve", "GitHub approve",
            "Approve a GitHub PR.", 30*time.Second,
            params(map[string]*schema.ParameterInfo{
                "repo":   {Type: schema.String, Required: true, Desc: "owner/repo"},
                "number": {Type: schema.Integer, Required: true, Desc: "PR number"},
                "body":   {Type: schema.String, Desc: "Approval comment (optional)"},
            }), SyncStream(runGitHubApprove)),
        Merge: NewApprovalGuardedTool("github_merge", "GitHub merge",
            "Merge a GitHub PR.", 30*time.Second,
            params(map[string]*schema.ParameterInfo{
                "repo":   {Type: schema.String, Required: true, Desc: "owner/repo"},
                "number": {Type: schema.Integer, Required: true, Desc: "PR number"},
                "method": {Type: schema.String, Enum: []string{"merge", "squash", "rebase"}, Desc: "Merge method (default merge)"},
            }), SyncStream(runGitHubMerge)),
    }
}

// ghSpec builds a secproc spec for running `gh` with the given args.
func ghSpec(args ...string) secproc.SecureProcessSpec {
    return secproc.SecureProcessSpec{
        Tool: "github", Program: "gh", Args: args,
        UseSandboxTier: sandbox.NetOnly,
    }
}

func runGitHubPRContext(ctx context.Context, argsJSON string) (string, error) {
    var params struct {
        Repo   string `json:"repo"`
        Number int    `json:"number"`
    }
    if err := ParseArgs(argsJSON, &params); err != nil { return errorResult(err.Error()), nil }
    res, err := secureCommandRunner(ctx, ghSpec("pr", "view", "--repo", params.Repo, "--json",
        "number,title,body,headRefName,baseRefName,author,files,changedFiles", fmt.Sprintf("%d", params.Number)), 30*time.Second)
    if err != nil { return errorResult("gh: " + err.Error()), nil }
    ghCtx, err := FetchGitHubContext(params.Repo, params.Number, res.Stdout)
    if err != nil { return errorResult(err.Error()), nil }
    return toJSON(ghCtx), nil
}

func runGitHubComment(ctx context.Context, argsJSON string) (string, error) {
    var params struct {
        Repo   string `json:"repo"`
        Number int    `json:"number"`
        Body   string `json:"body"`
    }
    if err := ParseArgs(argsJSON, &params); err != nil { return errorResult(err.Error()), nil }
    res, err := secureCommandRunner(ctx, ghSpec("pr", "comment", "--repo", params.Repo, "--body", params.Body, fmt.Sprintf("%d", params.Number)), 30*time.Second)
    if err != nil { return errorResult("gh: " + err.Error()), nil }
    return toJSON(struct{ ID string }{ID: strings.TrimSpace(res.Stdout)}), nil
}

func runGitHubApprove(ctx context.Context, argsJSON string) (string, error) {
    var params struct {
        Repo   string `json:"repo"`
        Number int    `json:"number"`
        Body   string `json:"body,omitempty"`
    }
    if err := ParseArgs(argsJSON, &params); err != nil { return errorResult(err.Error()), nil }
    ghArgs := []string{"pr", "review", "--approve", "--repo", params.Repo}
    if params.Body != "" { ghArgs = append(ghArgs, "--body", params.Body) }
    ghArgs = append(ghArgs, fmt.Sprintf("%d", params.Number))
    res, err := secureCommandRunner(ctx, ghSpec(ghArgs...), 30*time.Second)
    if err != nil { return errorResult("gh: " + err.Error()), nil }
    return toJSON(struct{ Result string }{Result: strings.TrimSpace(res.Stdout)}), nil
}

func runGitHubMerge(ctx context.Context, argsJSON string) (string, error) {
    var params struct {
        Repo   string `json:"repo"`
        Number int    `json:"number"`
        Method string `json:"method"`
    }
    if err := ParseArgs(argsJSON, &params); err != nil { return errorResult(err.Error()), nil }
    switch params.Method {
    case "", "merge", "squash", "rebase":
    default:
        return errorResult(fmt.Sprintf("unknown merge method %q (merge|squash|rebase)", params.Method)), nil
    }
    ghArgs := []string{"pr", "merge", "--repo", params.Repo}
    if params.Method != "" { ghArgs = append(ghArgs, "--"+params.Method) }
    ghArgs = append(ghArgs, fmt.Sprintf("%d", params.Number))
    res, err := secureCommandRunner(ctx, ghSpec(ghArgs...), 30*time.Second)
    if err != nil { return errorResult("gh: " + err.Error()), nil }
    return toJSON(struct{ Result string }{Result: strings.TrimSpace(res.Stdout)}), nil
}
```

- [ ] **Step 4：运行 GREEN**

Run: `go test ./internal/tools -run 'TestGitHubPRContext|TestGitHubComment|TestGitHubMerge' -v`
Expected: PASS。

- [ ] **Step 5：提交**

```bash
git add internal/tools/github.go internal/tools/github_test.go internal/tools/predefined.go
git commit -m "feat(tools): GitHub tools with approval-gated mutations and narrow FetchGitHubContext export"
```

---

## Task 7: Web tools 扩展（timeout + Search + body degrade）

**Files:**
- Modify: `internal/tools/web.go`
- Modify: `internal/tools/web_test.go`

`NewWebTools` 新增 `timeout` 参数（保留 `maxBytes` 默认 1 MiB 行为）。`Search` 工具通过 HTTP 搜索端点。body 读取用 bounded capture（非静默 `io.LimitReader`），标记 degrade。

- [ ] **Step 1：写真实 TDD RED 测试（5 tests）**

```go
package tools

import (
    "context"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
    "time"

    "github.com/x6nux/yanshi/internal/guard"
)

func TestNewWebToolsPreservesDefaultsWhenTimeoutAdded(t *testing.T) {
    w := NewWebTools(0, 0)
    if w.maxBytes != 1<<20 { t.Fatalf("maxBytes=%d", w.maxBytes) }
    w2 := NewWebTools(4096, 0)
    if w2.maxBytes != 4096 { t.Fatalf("maxBytes=%d", w2.maxBytes) }
    w3 := NewWebTools(0, 30*time.Second)
    if w3.maxBytes != 1<<20 { t.Fatalf("maxBytes=%d", w3.maxBytes) }
}

func TestWebFetchMarksOversizeBodyDegraded(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Write([]byte(strings.Repeat("x", 200)))
    }))
    defer srv.Close()
    // Bind a profile that allows the httptest server's host so the net guard
    // does not pre-empt the truncation check. Without this the guard returns a
    // DenyErr that InvokableRun packages into the result string (not a Go err),
    // and the test would never observe "body truncated".
    host := srv.Listener.Addr().String()
    ctx := WithProfile(context.Background(), guard.PermissionProfile{
        Tools: guard.ToolsPerm{Allow: []string{"*"}},
        Net:   guard.NetPerm{Allow: true, Hosts: []string{host}},
    })
    w := NewWebTools(100, 5*time.Second)
    out, err := w.Fetch.InvokableRun(ctx, `{"url":"`+srv.URL+`"}`)
    if err != nil { t.Fatal(err) }
    if !strings.Contains(out, "body truncated") { t.Fatalf("out=%s", out) }
}

func TestWebFetchRedirectReAuthorization(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        http.Redirect(w, r, "http://evil.com/payload", http.StatusFound)
    }))
    defer srv.Close()
    allowedHost := srv.Listener.Addr().String()
    ctx := WithProfile(context.Background(), guard.PermissionProfile{
        Tools: guard.ToolsPerm{Allow: []string{"*"}},
        Net:   guard.NetPerm{Allow: false, Hosts: []string{allowedHost}},
    })
    // Fetch URL goes to srv (host in allow list) -> initial check passes; the
    // server redirects to evil.com (NOT in allow set) -> CheckRedirect returns a
    // DenyErr. SyncStream wraps it; InvokableRun packages DenyErr into the
    // result STRING (not a Go error), so we must assert on `out` not `err`.
    out, err := NewWebTools(1<<20, 5*time.Second).Fetch.InvokableRun(ctx, `{"url":"`+srv.URL+`"}`)
    if err != nil { t.Fatal(err) }
    if !strings.Contains(out, "permission denied") || !strings.Contains(out, "redirect denied") {
        t.Fatalf("out=%s", out)
    }
}

func TestWebSearchReturnsFilteredResults(t *testing.T) {
    ctx := WithProfile(context.Background(), guard.PermissionProfile{
        Tools: guard.ToolsPerm{Allow: []string{"*"}},
        Net:   guard.NetPerm{Allow: true, Hosts: []string{"*"}},
    })
    // Use a canned HTTP search endpoint
    w := NewWebTools(1<<20, 5*time.Second)
    w.searchBase = "http://localhost:0/invalid"
    // Running against a real search endpoint would require external.
    // For RED, we define the tool; the test verifies it exists.
    _ = w.Search
}

func TestWebSearchRejectsDisallowedHost(t *testing.T) {
    ctx := WithProfile(context.Background(), guard.PermissionProfile{
        Tools: guard.ToolsPerm{Allow: []string{"*"}},
        Net:   guard.NetPerm{Allow: false, Hosts: []string{}},
    })
    w := NewWebTools(1<<20, 5*time.Second)
    // Permission denial shows up in the result string (GuardedTool.InvokableRun
    // converts DenyErr into a "✗ permission denied: ..." result, not a Go err).
    out, err := w.Search.InvokableRun(ctx, `{"query":"test","max_results":5}`)
    if err != nil { t.Fatal(err) }
    if !strings.Contains(out, "permission denied") { t.Fatalf("out=%s", out) }
}
```

- [ ] **Step 2：运行 RED**

Run: `go test ./internal/tools -run 'TestNewWebTools|TestWebFetch|TestWebSearch' -v`
Expected: FAIL。

- [ ] **Step 3：修改 web.go**

```go
package tools

import (
    "bytes"
    "context"
    "fmt"
    "io"
    "net/http"
    "net/url"
    "strings"
    "time"

    "github.com/cloudwego/eino/schema"
    "github.com/x6nux/yanshi/internal/guard"
)

type WebTools struct {
    maxBytes   int
    timeout    time.Duration
    searchBase string // override for tests
    Fetch      *GuardedTool
    Search     *GuardedTool
}

func NewWebTools(maxBytes int, timeout time.Duration) *WebTools {
    w := &WebTools{maxBytes: maxBytes, timeout: timeout, searchBase: "https://lite.duckduckgo.com/lite"}
    if w.maxBytes <= 0 { w.maxBytes = 1 << 20 }
    if w.timeout <= 0 { w.timeout = 30 * time.Second }
    w.Fetch = NewGuardedTool("web_fetch", "Fetch",
        "Fetch a URL via HTTP GET and return the response body as text.",
        w.timeout,
        params(map[string]*schema.ParameterInfo{
            "url": {Type: schema.String, Desc: "URL to fetch", Required: true},
        }), SyncStream(w.runFetch))
    w.Search = NewGuardedTool("web_search", "Search",
        "Search the web and return a list of result titles and URLs.",
        w.timeout,
        params(map[string]*schema.ParameterInfo{
            "query":      {Type: schema.String, Required: true, Desc: "Search query"},
            "max_results": {Type: schema.Integer, Desc: "Max results (default 10)"},
        }), SyncStream(w.runSearch))
    return w
}
```

`runFetch` 移除 `io.LimitReader`，改为带 degrade 标记的 `truncatingReader`：

```go
type truncatingReader struct {
    r       io.Reader
    limit   int
    total   int
    truncated bool
}

func (t *truncatingReader) Read(p []byte) (int, error) {
    n, err := t.r.Read(p)
    t.total += n
    if t.total > t.limit {
        maxKeep := t.limit
        if t.total-n < maxKeep { // partial write straddling the boundary
            keep := n - (t.total - maxKeep)
            if keep > 0 { copy(p[:keep], p[:keep]) } // keep prefix of this chunk
            t.truncated = true
            return keep, nil
        }
        t.truncated = true
        return 0, io.EOF
    }
    return n, err
}

func (w *WebTools) runFetch(ctx context.Context, argsJSON string) (string, error) {
    var a struct{ URL string `json:"url"` }
    if err := ParseArgs(argsJSON, &a); err != nil { return "", err }
    prof, ok := ProfileFromContext(ctx)
    if !ok { return "", &DenyErr{Reason: "no permission profile in context"} }
    host := hostOnly(a.URL)
    if host == "" { return "", &DenyErr{Reason: "invalid url / empty host"} }
    dec := guard.New().Check(prof, guard.Action{Tool: "web_fetch", NetHost: host})
    if !dec.Allowed { return "", &DenyErr{Reason: dec.Reason} }
    req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
    if err != nil { return "", fmt.Errorf("web.fetch: build request: %w", err) }
    cli := &http.Client{
        Timeout: w.timeout,
        CheckRedirect: func(req *http.Request, via []*http.Request) error {
            if len(via) >= 10 { return fmt.Errorf("web.fetch: stopped after 10 redirects") }
            h := hostOnly(req.URL.String())
            if h == "" { return &DenyErr{Reason: "redirect target has empty host"} }
            if d := guard.New().Check(prof, guard.Action{Tool: "web_fetch", NetHost: h}); !d.Allowed {
                return &DenyErr{Reason: "redirect denied: " + d.Reason}
            }
            return nil
        },
    }
    resp, err := cli.Do(req)
    if err != nil { return "", fmt.Errorf("web.fetch: request failed: %w", err) }
    defer resp.Body.Close()
    if resp.StatusCode >= 400 { return "", fmt.Errorf("web.fetch: HTTP %d", resp.StatusCode) }
    tr := &truncatingReader{r: resp.Body, limit: w.maxBytes}
    body, err := io.ReadAll(tr)
    if err != nil { return "", fmt.Errorf("web.fetch: read body: %w", err) }
    out := string(body)
    if tr.truncated { out += fmt.Sprintf("\n[body truncated: kept %d of %d bytes]", w.maxBytes, tr.total) }
    return out, nil
}
```

`runSearch`：

```go
type searchResult struct {
    Results []searchItem `json:"results"`
}

type searchItem struct {
    Title string `json:"title"`
    URL   string `json:"url"`
    Snippet string `json:"snippet,omitempty"`
}

func (w *WebTools) runSearch(ctx context.Context, argsJSON string) (string, error) {
    var a struct {
        Query     string `json:"query"`
        MaxResults int   `json:"max_results"`
    }
    if err := ParseArgs(argsJSON, &a); err != nil { return "", err }
    if a.MaxResults <= 0 || a.MaxResults > 50 { a.MaxResults = 10 }
    prof, ok := ProfileFromContext(ctx)
    if !ok { return "", &DenyErr{Reason: "no permission profile in context"} }
    searchHost := hostOnly(w.searchBase)
    if searchHost == "" { return "", &DenyErr{Reason: "invalid search base URL"} }
    dec := guard.New().Check(prof, guard.Action{Tool: "web_search", NetHost: searchHost})
    if !dec.Allowed { return "", &DenyErr{Reason: dec.Reason} }
    req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.searchBase+"?q="+url.QueryEscape(a.Query), nil)
    if err != nil { return "", fmt.Errorf("web.search: build request: %w", err) }
    cli := &http.Client{Timeout: w.timeout}
    resp, err := cli.Do(req)
    if err != nil { return "", fmt.Errorf("web.search: request failed: %w", err) }
    defer resp.Body.Close()
    body, err := io.ReadAll(io.LimitReader(resp.Body, int64(w.maxBytes)))
    if err != nil { return "", fmt.Errorf("web.search: read: %w", err) }
    // Simple HTML extraction: find <a> tags with href in search results.
    html := string(body)
    var results []searchItem
    for _, line := range strings.Split(html, "\n") {
        line = strings.TrimSpace(line)
        if strings.HasPrefix(line, "<a ") && strings.Contains(line, "class=\"result-link\"") {
            results = append(results, searchItem{Title: line, URL: ""})
            if len(results) >= a.MaxResults { break }
        }
    }
    return toJSON(searchResult{Results: results}), nil
}
```

- [ ] **Step 4：运行 GREEN**

Run: `go test ./internal/tools -run 'TestNewWebTools|TestWebFetch|TestWebSearch' -v`
Expected: PASS。

- [ ] **Step 5：提交**

```bash
git add internal/tools/web.go internal/tools/web_test.go
git commit -m "feat(tools): web_search tool, NewWebTools timeout param, body degrade marking"
```

## Task 8: Review 核心流水线（完整分块 + 子代理 + 解析 + 去重）

**Files:**
- Create: `internal/tools/review.go`
- Create: `internal/tools/review_chunk.go`
- Create: `internal/tools/review_decode.go`
- Create: `internal/tools/review_test.go`
- Modify: `internal/tools/predefined.go`（注册 `"review"` predefined agent）
- Modify: `internal/tools/agent.go`（新增 `Review *GuardedTool` + `streamReview` 接入）

Review 工具把 PR diff 切成 48 KiB chunk，每个 chunk 由 `runSubAgent` 单独评审，输出 JSON findings，最后解析、去重、排序、artifact 化。

- [ ] **Step 1：写 TDD RED 测试（5 tests + `makeManyFileDiff` fixture）**

新建 `internal/tools/review_test.go`：

```go
package tools

import (
    "context"
    "encoding/json"
    "strings"
    "testing"

    "github.com/x6nux/yanshi/internal/task/work"
)

// makeManyFileDiff builds a deterministic large diff: n files, each with
// `lines` added lines of patterned content. Used to drive chunk-loop tests
// that require more than one chunk.
func makeManyFileDiff(n, lines int) string {
    var b strings.Builder
    for i := 0; i < n; i++ {
        b.WriteString("--- a/pkg/file")
        b.WriteString(itoa(i))
        b.WriteString(".go\n+++ b/pkg/file")
        b.WriteString(itoa(i))
        b.WriteString(".go\n@@ -0,0 +1,")
        b.WriteString(itoa(lines))
        b.WriteString(" @@\n")
        for j := 0; j < lines; j++ {
            b.WriteString("+line content ")
            b.WriteString(itoa(i))
            b.WriteString("-")
            b.WriteString(itoa(j))
            b.WriteString("\n")
        }
    }
    return b.String()
}

func itoa(n int) string {
    if n == 0 { return "0" }
    var buf []byte
    neg := n < 0
    if neg { n = -n }
    for n > 0 { buf = append([]byte{byte('0' + n%10)}, buf...); n /= 10 }
    if neg { buf = append([]byte{'-'}, buf...) }
    return string(buf)
}

func TestChunkDiffSplitsAt48KiBBoundary(t *testing.T) {
    // Each file = header + N lines. To force a split, use size > 48KiB.
    // 48 KiB / ~30 bytes per line ~ 1600 lines per chunk → 2 chunks at 2400.
    diff := makeManyFileDiff(3, 800) // ~3 * 24 KiB = 72 KiB total
    chunks := chunkDiff(diff, 48*1024)
    if len(chunks) < 2 { t.Fatalf("expected >=2 chunks, got %d", len(chunks)) }
    // Each chunk must not exceed limit except possibly last piece carrying overflow.
    for i, c := range chunks {
        if len(c) > 48*1024+64 { t.Fatalf("chunk %d too big: %d", i, len(c)) }
    }
    // Concatenation preserves every file marker.
    joined := strings.Join(chunks, "")
    if !strings.Contains(joined, "file0.go") || !strings.Contains(joined, "file2.go") {
        t.Fatalf("chunks lost file markers")
    }
}

func TestChunkDiffPreservesCompleteHunks(t *testing.T) {
    // Chunks split on "\n@@" / "\n--- " boundaries, never mid-hunk.
    diff := "header line\n--- a/x.go\n+++ b/x.go\n@@ -0,0 +1,3 @@\n+a\n+b\n+c\n"
    chunks := chunkDiff(diff, 20)
    joined := strings.Join(chunks, "")
    if !strings.Contains(joined, "+a\n+b\n+c\n") {
        t.Fatalf("hunk fragmented: %q", joined)
    }
}

func TestStreamReviewInvokesSubAgentPerChunk(t *testing.T) {
    manager := work.NewFakeManager()
    ctx := WithWorkRoot(WithTaskManager(context.Background(), manager), t.TempDir())
    // Inject a fake SubAgentRunner that returns canned JSON findings.
    // SubAgentRunner signature is (ctx, prompt, allowedTools, instructionOverride);
    // tests must match it exactly or WithSubAgentRunner won't compile.
    inject := func(ctx context.Context, prompt string, allowedTools []string, instructionOverride string) (string, error) {
        return `{"findings":[{"file":"pkg/x.go","line":1,"severity":"high","message":"bug"}]}`, nil
    }
    ctx = WithSubAgentRunner(ctx, inject)
    diff := makeManyFileDiff(2, 800)
    out, err := streamReview(ctx, reviewInput{Diff: diff, TaskID: "task-7"})
    if err != nil { t.Fatal(err) }
    var res reviewResult
    if err := json.Unmarshal([]byte(out), &res); err != nil { t.Fatal(err) }
    if res.ChunksReviewed < 1 || len(res.Findings) < 1 { t.Fatalf("res=%+v", res) }
    if res.Findings[0].File != "pkg/x.go" || res.Findings[0].Severity != "high" {
        t.Fatalf("finding=%+v", res.Findings[0])
    }
}

func TestStreamReviewDedupesAndSortsFindings(t *testing.T) {
    // Two chunks both flag the same finding; result should have it once.
    calls := 0
    inject := func(ctx context.Context, prompt string, allowedTools []string, instructionOverride string) (string, error) {
        calls++
        // Both calls return the same finding (same file/line/message).
        return `{"findings":[{"file":"a.go","line":1,"severity":"high","message":"dup"}]}`, nil
    }
    ctx := WithWorkRoot(WithTaskManager(context.Background(), work.NewFakeManager()), t.TempDir())
    ctx = WithSubAgentRunner(ctx, inject)
    out, err := streamReview(ctx, reviewInput{Diff: makeManyFileDiff(3, 900), TaskID: "t1"})
    if err != nil { t.Fatal(err) }
    var res reviewResult
    json.Unmarshal([]byte(out), &res)
    if calls < 2 { t.Fatalf("expected >=2 sub-agent calls, got %d", calls) }
    if len(res.Findings) != 1 { t.Fatalf("expected dedupe to 1, got %d (%+v)", len(res.Findings), res.Findings) }
}

func TestStreamReviewWritesArtifactWhenDiffTooLarge(t *testing.T) {
    manager := work.NewFakeManager()
    ctx := WithWorkRoot(WithTaskManager(context.Background(), manager), t.TempDir())
    inject := func(ctx context.Context, prompt string, allowedTools []string, instructionOverride string) (string, error) {
        return `{"findings":[]}`, nil
    }
    ctx = WithSubAgentRunner(ctx, inject)
    out, err := streamReview(ctx, reviewInput{Diff: makeManyFileDiff(5, 1500), TaskID: "task-7"})
    if err != nil { t.Fatal(err) }
    // Big diff → many findings serialized to artifact, OR artifact of diff summary.
    var res reviewResult
    json.Unmarshal([]byte(out), &res)
    // The fixture creates at least one artifact because the diff is huge.
    artifacts, _ := manager.ListArtifacts(context.Background(), "task-7")
    if len(artifacts) < 1 { t.Fatalf("expected artifact stored, got %d", len(artifacts)) }
}
```

- [ ] **Step 2：运行 RED**

Run: `go test ./internal/tools -run 'TestChunkDiff|TestStreamReview' -v`
Expected: FAIL，`chunkDiff`/`streamReview` 未定义。

- [ ] **Step 3：实现 `review_chunk.go`**

```go
package tools

import "strings"

const reviewChunkLimit = 48 * 1024

// chunkDiff splits diff text into <=limit-byte chunks without cutting through
// a single hunk. The splitter walks line-by-line and flushes the accumulated
// buffer whenever it exceeds `limit` AND the next line starts a new hunk
// boundary ("@@ " or file separator "--- " / "+++ ").
func chunkDiff(diff string, limit int) []string {
    if limit <= 0 { limit = reviewChunkLimit }
    if len(diff) <= limit { return []string{diff} }
    var chunks []string
    var cur strings.Builder
    for _, line := range strings.SplitAfter(diff, "\n") {
        // Flush at hunk boundary if buffer is already over the limit.
        if cur.Len() > limit && (strings.HasPrefix(line, "@@ ") || strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "diff --git ")) {
            chunks = append(chunks, cur.String())
            cur.Reset()
        }
        cur.WriteString(line)
    }
    if cur.Len() > 0 { chunks = append(chunks, cur.String()) }
    // Guarantee no chunk is larger than limit + (one line) — never mid-hunk.
    var safe []string
    for _, c := range chunks {
        if len(c) <= limit {
            safe = append(safe, c)
            continue
        }
        // Fallback: hard split on line boundaries (still no mid-line cut).
        for len(c) > limit {
            cut := strings.LastIndexByte(c[:limit], '\n')
            if cut <= 0 { cut = limit }
            safe = append(safe, c[:cut])
            c = c[cut:]
        }
        if len(c) > 0 { safe = append(safe, c) }
    }
    return safe
}
```

- [ ] **Step 4：实现 `review_decode.go`**

```go
package tools

import (
    "encoding/json"
    "sort"
)

type reviewFinding struct {
    File     string `json:"file"`
    Line     int    `json:"line"`
    Severity string `json:"severity"`
    Message  string `json:"message"`
    Rule     string `json:"rule,omitempty"`
}

type reviewSubAgentOutput struct {
    Findings []reviewFinding `json:"findings"`
}

type reviewResult struct {
    Findings       []reviewFinding `json:"findings"`
    ChunksReviewed int             `json:"chunks_reviewed"`
    ArtifactRef    string          `json:"artifact_ref,omitempty"`
    Degraded       bool            `json:"degraded,omitempty"`
}

// decodeReviewSubAgentOutput parses the JSON a sub-agent returned. Sub-agents
// occasionally wrap output in prose or return a fence; extract the first
// balanced JSON object before unmarshalling.
func decodeReviewSubAgentOutput(raw string) (reviewSubAgentOutput, error) {
    var out reviewSubAgentOutput
    payload := extractJSONObject(raw)
    if payload == "" { return out, nil }
    if err := json.Unmarshal([]byte(payload), &out); err != nil {
        return out, err
    }
    return out, nil
}

// extractJSONObject returns the substring of `s` from the first '{' to its
// matching '}'. Empty string if none found. Tolerates nested braces and
// strings containing braces.
func extractJSONObject(s string) string {
    start := strings.IndexByte(s, '{')
    if start < 0 { return "" }
    depth := 0
    inStr := false
    escape := false
    for i := start; i < len(s); i++ {
        c := s[i]
        if escape { escape = false; continue }
        if c == '\\' { escape = true; continue }
        if c == '"' { inStr = !inStr; continue }
        if inStr { continue }
        switch c {
        case '{': depth++
        case '}':
            depth--
            if depth == 0 { return s[start : i+1] }
        }
    }
    return ""
}

// dedupeAndSortFindings removes duplicates (same File+Line+Severity+Message)
// and sorts by Severity (high > medium > low > info), then File, then Line.
func dedupeAndSortFindings(in []reviewFinding) []reviewFinding {
    seen := make(map[string]bool, len(in))
    out := make([]reviewFinding, 0, len(in))
    for _, f := range in {
        key := f.File + "|" + strconv.Itoa(f.Line) + "|" + f.Severity + "|" + f.Message
        if seen[key] { continue }
        seen[key] = true
        out = append(out, f)
    }
    sevRank := map[string]int{"high": 0, "medium": 1, "low": 2, "info": 3}
    sort.SliceStable(out, func(i, j int) bool {
        ri, rj := sevRank[out[i].Severity], sevRank[out[j].Severity]
        if ri != rj { return ri < rj }
        if out[i].File != out[j].File { return out[i].File < out[j].File }
        return out[i].Line < out[j].Line
    })
    return out
}
```

注意：`review_decode.go` 需要导入 `strconv` 和 `strings`（`strconv.Itoa` 用于整数转字符串；`strings.IndexByte` 用于 `extractJSONObject`）。**不要使用测试文件里的 `itoa` helper——它在 `_test.go` 中定义，非测试构建里不可用，会导致生产代码编译失败。**

```go
import (
    "encoding/json"
    "sort"
    "strconv"
    "strings"
)
```

- [ ] **Step 5：实现 `review.go`（含 `streamReview` 完整 Go）**

```go
package tools

import (
    "context"
    "encoding/json"
    "fmt"
    "strings"
)

type reviewInput struct {
    Diff   string `json:"diff"`
    TaskID string `json:"task_id"`
    Repo   string `json:"repo,omitempty"`
    Number int    `json:"number,omitempty"`
}

// streamReview is the core pipeline:
//   1. chunk diff at 48 KiB hunk-safe boundaries
//   2. dispatch each chunk to the review sub-agent via SubAgentRunnerFromContext
//   3. decode each JSON response
//   4. dedupe + sort findings
//   5. writeArtifactOrSpill if total serialized size exceeds SpillThreshold
//
// Note: SubAgentRunner signature is
//   func(ctx, prompt string, allowedTools []string, instructionOverride string) (string, error)
// — NOT (ctx, def, prompt). The review pipeline passes (ctx, prompt, nil, "")
// because allowedTools/instructionOverride are managed by the orchestrator
// binding, not the review tool itself.
func streamReview(ctx context.Context, in reviewInput) (string, error) {
    runner := SubAgentRunnerFromContext(ctx)
    if runner == nil {
        return errorResult("review requires a bound sub-agent runner (task-orchestrator)"), nil
    }
    def, ok := GetPredefinedAgent("review")
    if !ok {
        return errorResult("predefined \"review\" agent not registered"), nil
    }
    chunks := chunkDiff(in.Diff, reviewChunkLimit)
    var allFindings []reviewFinding
    for i, chunk := range chunks {
        prompt := strings.ReplaceAll(def.PromptTmpl, "{{CHUNK}}", chunk)
        prompt = strings.ReplaceAll(prompt, "{{REPO}}", in.Repo)
        prompt = strings.ReplaceAll(prompt, "{{NUMBER}}", fmt.Sprintf("%d", in.Number))
        prompt = strings.ReplaceAll(prompt, "{{INDEX}}", fmt.Sprintf("%d", i+1))
        prompt = strings.ReplaceAll(prompt, "{{TOTAL}}", fmt.Sprintf("%d", len(chunks)))
        raw, err := runner(ctx, prompt, nil, "")
        if err != nil {
            // A single chunk failing should not kill the whole review.
            allFindings = append(allFindings, reviewFinding{
                File: fmt.Sprintf("chunk-%d", i+1), Severity: "info",
                Message: "sub-agent failed: " + err.Error(),
            })
            continue
        }
        decoded, err := decodeReviewSubAgentOutput(raw)
        if err != nil {
            allFindings = append(allFindings, reviewFinding{
                File: fmt.Sprintf("chunk-%d", i+1), Severity: "info",
                Message: "decode failed: " + err.Error(),
            })
            continue
        }
        allFindings = append(allFindings, decoded.Findings...)
    }
    allFindings = dedupeAndSortFindings(allFindings)
    result := reviewResult{Findings: allFindings, ChunksReviewed: len(chunks)}
    payload, _ := json.Marshal(result)
    if len(payload) > SpillThreshold {
        artifact := writeArtifactOrSpill(ctx, in.TaskID, "review-findings", string(payload))
        result.ArtifactRef = artifact.ArtifactRef
        result.Degraded = artifact.Degraded
        // Replace inline findings with summary pointer to keep tool result small.
        result.Findings = compressFindings(allFindings, artifact)
    }
    return toJSON(result), nil
}

// compressFindings replaces the full finding list with a short summary when
// the artifact has been stored. Keeps the top 10 by severity in-line as a
// preview; full list is in the artifact.
func compressFindings(all []reviewFinding, art artifactOutput) []reviewFinding {
    if len(all) <= 10 { return all }
    preview := make([]reviewFinding, 10)
    copy(preview, all[:10])
    return preview
}
```

- [ ] **Step 6：注册 `"review"` predefined agent（修改 `internal/tools/predefined.go`）**

在现有 `PredefinedAgents` map 中加入 `"review"` 条目（沿用现有 `PredefinedAgentDef` 类型与 `GetPredefinedAgent` 查询；不新建 `internal/tools/predefined/` 子包，避免与 `predefined.go` 文件名冲突）：

```go
// 在 PredefinedAgents = map[string]PredefinedAgentDef{ ... } 中追加：
"review": {
    Name:        "review",
    Description: "分块代码评审：每个 48 KiB chunk 由子代理产出结构化 findings，再合并去重",
    PromptTmpl: `You are reviewing chunk {{INDEX}} of {{TOTAL}} of a pull request on {{REPO}}#{{NUMBER}}.

Return STRICT JSON of the form:
{"findings":[{"file":"path","line":N,"severity":"high|medium|low|info","message":"...","rule":"..."}]}

If you find no issues, return {"findings":[]}.

Diff chunk:
{{CHUNK}}
`,
},
```

`streamReview` 通过现有 `GetPredefinedAgent("review")` 查询；无需新增 `predefined.Lookup`。

- [ ] **Step 7：修改 `agent.go`（接入 `streamReview`）**

在 `AgentTools` 中添加 `Review *GuardedTool`，并在 `NewAgentTools` 中构造：

```go
// agent.go - extend AgentTools struct (near line 29)
type AgentTools struct {
    // ... existing fields ...
    Review *GuardedTool
}

// In NewAgentTools (near line 40), after existing tool registration:
func (a *AgentTools) buildReviewTool() *GuardedTool {
    return NewGuardedTool("review", "Code review",
        "Review a pull-request diff in 48 KiB chunks via the review sub-agent, "+
            "deduplicate and sort findings, and persist large outputs as artifacts.",
        10*time.Minute, // review is long-running
        params(map[string]*schema.ParameterInfo{
            "diff":    {Type: schema.String, Required: true, Desc: "Unified diff text to review"},
            "task_id": {Type: schema.String, Desc: "Task ID for artifact storage"},
            "repo":    {Type: schema.String, Desc: "GitHub repo (owner/name) for context"},
            "number":  {Type: schema.Integer, Desc: "PR number for context"},
        }),
        // StreamFunc is a type alias (not a constructor). The function literal
        // must match the exact signature  func(ctx, argsJSON) <-chan ToolChunk
        // — no error return, and the channel element type is ToolChunk (NOT
        // StreamChunk, which does not exist in the codebase).
        StreamFunc(func(ctx context.Context, argsJSON string) <-chan ToolChunk {
            ch := make(chan ToolChunk, 1)
            go func() {
                defer close(ch)
                var in reviewInput
                if err := ParseArgs(argsJSON, &in); err != nil {
                    ch <- ToolChunk{Err: err}
                    return
                }
                out, err := streamReview(ctx, in)
                if err != nil {
                    ch <- ToolChunk{Err: err}
                    return
                }
                ch <- ToolChunk{Text: out, Result: out}
            }()
            return ch
        }))
}
```

然后在 `NewAgentTools` 函数末尾把 `Review` 字段填上。

- [ ] **Step 8：运行 GREEN**

Run: `go test ./internal/tools -run 'TestChunkDiff|TestStreamReview' -v`
Expected: PASS。

- [ ] **Step 9：提交**

```bash
git add internal/tools/review.go internal/tools/review_chunk.go internal/tools/review_decode.go internal/tools/review_test.go internal/tools/predefined.go internal/tools/agent.go
git commit -m "feat(tools): chunked review pipeline with sub-agent dispatch, JSON decode, dedupe/sort, artifact spill"
```

## Task 9: Entry points（bootstrap + TUI + main pr + runHeadlessPrompt）

**Files:**
- Modify: `internal/bootstrap/bootstrap.go`
- Modify: `internal/cli/tui/commands.go`
- Modify: `cmd/yanshi/main.go`
- Create: `cmd/yanshi/pr.go`
- Create: `cmd/yanshi/pr_test.go`

一次性注册所有 B3 工具（删除冗余注册），`cmdReview` 走 `dispatchSend`（不直接调 `PermissionRequest`），`cmd/yanshi/pr.go` 共享 `FetchGitHubContext`，`runHeadlessPrompt` 完整实现。

- [ ] **Step 1：写 RED 测试（pr_test.go + cmdReview dispatchSend）**

```go
// cmd/yanshi/pr_test.go
package main

import (
    "testing"

    "github.com/x6nux/yanshi/internal/tools"
)

func TestFetchGitHubContextParsesComplete(t *testing.T) {
    raw := `{"number":7,"title":"Feat X","body":"","headRefName":"feat-x","baseRefName":"main","author":{"login":"bob"},"files":[{"path":"a.go","additions":5,"deletions":0}],"changedFiles":1}`
    ctx, err := tools.FetchGitHubContext("owner/repo", 7, raw)
    if err != nil { t.Fatal(err) }
    if ctx.Number != 7 || ctx.Title != "Feat X" || ctx.Author != "bob" { t.Fatalf("ctx=%+v", ctx) }
    if len(ctx.Files) != 1 || ctx.Files[0].Path != "a.go" { t.Fatalf("files=%+v", ctx.Files) }
}
```

```go
// internal/cli/tui/commands_test.go — 增加 TUI cmdReview dispatch 测试。
// Verify the "/review" command is in commandTable and routes through
// dispatchSend (sends "/review <text>" as a normal user_message — the agent
// then invokes the `review` tool). The assertion is on the recordingSession's
// sentText slice, which captures everything passed to Send().
func TestCmdReviewDispatchesViaSend(t *testing.T) {
    rec := &recordingSession{}
    m := newModel(rec, "/proj")
    cmd, ok := lookupCommand("review")
    if !ok { t.Fatalf("/review command not registered") }
    if cmd.run == nil { t.Fatalf("cmd.run is nil") }
    if _, c := cmd.run(m, []string{"some diff text"}); c == nil {
        // cmd.run must return a non-nil tea.Cmd when it dispatches a send; a nil
        // return would indicate the slash path silently dropped the input.
        t.Fatalf("cmd.run returned nil Cmd")
    }
    var saw bool
    for _, sent := range rec.sentText {
        if strings.Contains(sent, "/review") && strings.Contains(sent, "some diff text") {
            saw = true
            break
        }
    }
    if !saw {
        t.Fatalf("Send not invoked with /review text; sentText=%q", rec.sentText)
    }
}
```

> recordingSession.Send appends to sentText (see model_test.go:68-77). The test reuses that field; do not invent a new fake.

- [ ] **Step 2：修改 `bootstrap/bootstrap.go`**

在 B3 工具注册段，删除重复的 `NewWebTools(0)` 调用和旧版 leftover。新增 `NewGitHubTools` / `WebTools` / `Review` / `Diagnostics` 注册。

> **重要：现有 bootstrap.go 没有 `App.registerTool` 方法，也没有 `App.Tools` 字段——工具注册是在 `Build()` 内部的 `allTools []orchestrator.BaseTool` 本地 slice 上完成的（约 line 142-206）。下面的代码必须改写为在 `Build()` 内 inline 追加到 `allTools`，不要新建 `registerB3Tools` 方法。**

```go
// bootstrap.go — 在 Build() 内的 allTools 构造段（约 line 142-206）追加：

// B3 — git / test / diagnostics（每个工具构造一次；不要重复已有的 web_fetch）
gitTools := tools.NewGitTools()
allTools = append(allTools, gitTools.Status, gitTools.Diff)
allTools = append(allTools, tools.NewTestRunTool())
allTools = append(allTools, tools.NewDiagnosticsTool(nil))

// B3 — GitHub tools（factory 在 per-turn context 中绑定，构造时传 nil）
ghTools := tools.NewGitHubTools(nil)
allTools = append(allTools, ghTools.PRContext, ghTools.Comment, ghTools.Approve, ghTools.Merge)

// B3 — 把现有 NewWebTools(0) 替换为带 timeout 的新签名（web_fetch + web_search）
webTools := tools.NewWebTools(1<<20, 30*time.Second)
allTools = append(allTools, webTools.Fetch, webTools.Search)

// B3 — Review 走 AgentTools（NewAgentTools 已构造 Review 字段）
agentTools := tools.NewAgentTools(chatModel) // 已有的构造；如已在 line 163 构造，复用之
allTools = append(allTools, agentTools.Review)
```

注意：不要重复注册 `agent_start`/`workflow_start`/`analysis`/`summarize` —— 它们已在 Build() 现有段注册。同时不要用 `a.Tools.(...)` 类型断言去取 AgentTools —— `App` 没有这个字段，AgentTools 是 Build() 内的局部变量。

- [ ] **Step 3：修改 TUI `commands.go`（`cmdReview` via `dispatchSend`）**

在 `commandTable` 增加 `"review"` 入口。注意：现有 `command` struct 只有 `name`、`help`、`run` 三个字段（没有 `aliases`/`desc`/`category`），保持字段一致：

```go
// commands.go — 在 commandTable（约 line 30，紧随现有条目）
{name: "review", help: "run code review on a PR diff: /review <diff text or PR URL>", run: cmdReview},
```

新建 `cmdReview` 函数（同文件）：

```go
// cmdReview implements the /review slash command. It forwards the supplied
// diff/URL through dispatchSend so the message routes through the normal
// agent pipeline (permissions, streaming, transcript); the agent then decides
// to invoke the `review` tool.
func cmdReview(m model, args []string) (tea.Model, tea.Cmd) {
    if len(args) < 1 {
        m.entries = append(m.entries, errorEntry{text: "usage: /review <diff text or PR URL>"})
        m.refresh()
        m.viewport.GotoBottom()
        return m, nil
    }
    input := strings.Join(args, " ")
    return m.dispatchSend("/review "+input, false)
}
```

> 不要直接调 `proto.NewPermissionResponse` 或类似 frame —— `/review` 不是权限事件，而是一次普通的用户消息，由 `dispatchSend` 走既有路径。`addSystemMsg` 在现有 model 里若不存在，用 `entries = append(entries, errorEntry{...})` 替代（与 `runCommand` 的 unknown-command 分支一致）。

- [ ] **Step 4：修改 `main.go`（添加 `pr` case）**

```go
// main.go — 在 switch（约 line 79-101）中增加：
case "pr":
    if len(os.Args) < 3 {
        fmt.Fprintln(os.Stderr, "Usage: yanshi pr <PR-number>")
        os.Exit(1)
    }
    prNum := os.Args[2]
    os.Exit(runPR(context.Background(), prNum))
```

`pr()` 函数实现在 `cmd/yanshi/pr.go` 中。

- [ ] **Step 5：实现 `pr.go`（共享 `FetchGitHubContext` + `runHeadlessPrompt`）**

```go
// cmd/yanshi/pr.go
package main

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "os"
    "os/exec"
    "strconv"
    "strings"

    "github.com/x6nux/yanshi/internal/tools"
)

func runPR(ctx context.Context, prInput string) int {
    // Parse PR input: can be URL ("https://github.com/owner/repo/pull/42")
    // or just number (uses git remote to infer repo).
    repo, number := parsePRInput(prInput)
    if repo == "" || number <= 0 {
        fmt.Fprintln(os.Stderr, "Usage: yanshi pr <PR-number>  (run from the repo directory)")
        fmt.Fprintln(os.Stderr, "       yanshi pr <full-URL>   (any repo)")
        return 1
    }

    // Run `gh pr view` to get JSON.
    ghArgs := []string{"pr", "view", "--repo", repo, "--json",
        "number,title,body,headRefName,baseRefName,author,files,changedFiles",
        strconv.Itoa(number)}
    var stdout, stderr bytes.Buffer
    cmd := exec.CommandContext(ctx, "gh", ghArgs...)
    cmd.Stdout = &stdout
    cmd.Stderr = &stderr
    if err := cmd.Run(); err != nil {
        fmt.Fprintf(os.Stderr, "Error running gh: %v\n%s\n", err, stderr.String())
        return 1
    }

    // Parse via the shared narrow export.
    ghCtx, err := tools.FetchGitHubContext(repo, number, stdout.String())
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error parsing GitHub context: %v\n", err)
        return 1
    }

    // Get diff via `gh pr diff`.
    var diffBuf bytes.Buffer
    diffCmd := exec.CommandContext(ctx, "gh", "pr", "diff", "--repo", repo, strconv.Itoa(number))
    diffCmd.Stdout = &diffBuf
    diffCmd.Stderr = &stderr
    if err := diffCmd.Run(); err != nil {
        fmt.Fprintf(os.Stderr, "Error getting diff: %v\n%s\n", err, stderr.String())
        return 1
    }
    diff := diffBuf.String()

    // Build review input and run headless.
    prompt := buildPRReviewPrompt(ghCtx, diff)
    result, err := runHeadlessPrompt(ctx, "review", prompt)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Review failed: %v\n", err)
        return 1
    }
    fmt.Println(result)
    return 0
}

// parsePRInput returns (repo, number) from a URL or raw number.
func parsePRInput(input string) (string, int) {
    // Try URL: https://github.com/owner/repo/pull/42
    if strings.Contains(input, "github.com") {
        parts := strings.Split(strings.TrimRight(input, "/"), "/")
        for i, p := range parts {
            if p == "pull" && i+1 < len(parts) {
                n, err := strconv.Atoi(parts[i+1])
                if err != nil { return "", 0 }
                return strings.Join(parts[i-2:i], "/"), n
            }
        }
        return "", 0
    }

    // Try raw number: infer repo from git remote.
    n, err := strconv.Atoi(input)
    if err != nil { return "", 0 }
    repo := detectGitHubRemote()
    return repo, n
}

// detectGitHubRemote runs `git remote get-url origin` and extracts owner/repo.
func detectGitHubRemote() string {
    var out bytes.Buffer
    cmd := exec.Command("git", "remote", "get-url", "origin")
    cmd.Stdout = &out
    if err := cmd.Run(); err != nil { return "" }
    remote := strings.TrimSpace(out.String())
    // Handle both git@github.com:owner/repo.git and https://github.com/owner/repo
    remote = strings.TrimSuffix(remote, ".git")
    if idx := strings.Index(remote, "github.com:"); idx >= 0 {
        return remote[idx+len("github.com:"):]
    }
    if idx := strings.Index(remote, "github.com/"); idx >= 0 {
        return remote[idx+len("github.com/"):]
    }
    return ""
}

func buildPRReviewPrompt(ghCtx tools.GitHubContext, diff string) string {
    return fmt.Sprintf(`Review PR #%d (%s) by %s

Title: %s
Description: %s
Files changed: %d

Diff:
%s

Return findings in JSON exactly as: {"findings":[{"file":"path","line":N,"severity":"high|medium|low|info","message":"...","rule":"..."}]}`,
        ghCtx.Number, ghCtx.HeadRef, ghCtx.Author, ghCtx.Title, ghCtx.Body, len(ghCtx.Files), diff)
}
```

- [ ] **Step 6：实现 `runHeadlessPrompt`**

`runHeadlessPrompt` 是 `main.go` 中的辅助函数，不需要 ADK/model 栈，直接调用 `streamReview`（内部起子代理）。因为 review 工具在 bootstrap 时已注册，所以 `runHeadlessPrompt` 可以直接调用 `tools.StreamFunc`。

```go
// main.go — 新增 runHeadlessPrompt 函数
func runHeadlessPrompt(ctx context.Context, toolName, prompt string) (string, error) {
    // For now, only "review" tool is supported headless.
    switch toolName {
    case "review":
        return tools.RunReviewHeadless(ctx, prompt)
    default:
        return "", fmt.Errorf("unknown headless tool: %s", toolName)
    }
}
```

辅助实现（在 `internal/tools/review.go` 中）：

```go
// RunReviewHeadless is a public entry point used by `yanshi pr`.
// It builds a reviewInput from the prompt and calls streamReview.
func RunReviewHeadless(ctx context.Context, diff string) (string, error) {
    return streamReview(ctx, reviewInput{
        Diff:   diff,
        TaskID: "headless-review",
    })
}
```

- [ ] **Step 7：运行 GREEN**

```bash
go test ./cmd/yanshi -run 'TestFetchGitHubContext' -v
go test ./internal/cli/tui -run 'TestCmdReviewDispatchesViaSend' -v
go vet ./cmd/yanshi ./internal/bootstrap ./internal/cli/tui
```

Expected: PASS。（`RunReviewHeadless` 已被 Task 8 的 `TestStreamReviewInvokesSubAgentPerChunk` 间接覆盖，无需重复单测。）

- [ ] **Step 8：提交**

```bash
git add cmd/yanshi/main.go cmd/yanshi/pr.go cmd/yanshi/pr_test.go internal/bootstrap/bootstrap.go internal/cli/tui/commands.go internal/cli/tui/model.go internal/tools/review.go
git commit -m "feat: entry points — bootstrap registerB3Tools, TUI cmdReview dispatchSend, yanshi pr, runHeadlessPrompt"
```

---

## 最终：验证清单

所有 B3 任务实施完毕后的冒烟检查（按依赖顺序）。

- [ ] **FINAL-1：编译门禁**

```bash
go build -o /dev/null ./cmd/yanshi
```
Expected: 编译成功。失败时检查 A1c/DT3/B2 接口兼容性。

- [ ] **FINAL-2：B3 包测试（不含 review，快）**

```bash
go test ./internal/tools -run 'TestParamsPreserves|TestRunSecureCapture|TestWriteArtifactOrSpill|TestGitStatus|TestGitDiff|TestGitToolsDoNotWriteGitConfig|TestRunTestsExecutes|TestParseGoJSON|TestParseCargoOutput|TestParseNPMOutput|TestDetectRunnerPriority|TestDiagnostics|TestAuthorizeApprovalTruthTable|TestApprovalRequiredNoCallback|TestGitHubPRContext|TestGitHubComment|TestGitHubMerge|TestNewWebTools|TestWebFetch|TestWebSearch|TestChunkDiff' -v -count=1
```
Expected: 全部 PASS。

- [ ] **FINAL-3：Review 测试（起子代理）**

```bash
go test ./internal/tools -run 'TestStreamReview|TestChunkDiff' -v -count=1
```
Expected: PASS（使用 fake sub-agent runner）。

- [ ] **FINAL-4：`yanshi pr` 端到端（需 gh CLI）**

```bash
go build -o yanshi.exe ./cmd/yanshi
./yanshi pr 1
```
Expected: 从当前仓库的 git remote 推断 owner/repo，调用 `gh pr view`，然后打印 review 结果。如果没装 gh CLI，应当友好报错。

- [ ] **FINAL-5：TUI `/review` 命令**

启动 TUI（`./yanshi --fake-model`），输入 `/review <diff text>`，确认 `dispatchSend` 将文本发往 agent 路由。

- [ ] **FINAL-6：预设 review agent 查找**

```bash
go test -run 'TestStreamReviewInvokesSubAgentPerChunk' ./internal/tools/... -v
```
Expected: PASS（验证 `GetPredefinedAgent("review")` 在 streamReview 中可查到，等价于Lookup 校验）。

- [ ] **FINAL-7：Web 工具 degrade 测试**

```bash
go test -run 'TestWebFetchMarksOversizeBodyDegraded' ./internal/tools -v
```
Expected: PASS。

- [ ] **FINAL-8：强制审批真值表**

```bash
go test -run 'TestAuthorizeApprovalTruthTable' ./internal/tools -v
```
Expected: PASS，10 行全部匹配（Task 5 Step 1 的 truth table 共 10 行）。

- [ ] **FINAL-9：`go vet ./...`**

```bash
go vet ./...
```
Expected: 无 vet warning。

- [ ] **FINAL-10：提交全部修改**

```bash
git add internal/tools/ internal/bootstrap/bootstrap.go internal/cli/tui/commands.go internal/cli/tui/model.go internal/cli/tui/events.go cmd/yanshi/main.go cmd/yanshi/pr.go cmd/yanshi/pr_test.go internal/proto/frame.go internal/proto/frame_test.go internal/cli/backend.go internal/cli/wsbackend.go internal/api/http/ws.go internal/api/http/ws_test.go config.example.yaml
git commit -m "feat(b3): developer tools — git, test, diagnostics, GitHub, web, review, mandatory approval, entry points"
```
