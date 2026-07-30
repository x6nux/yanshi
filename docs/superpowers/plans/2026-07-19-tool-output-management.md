# Tool Output Management Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a uniform 64 KiB output cap that spills oversized tool results to temp files, upgrade `fs_read` to `offset`/`end` paging (no spillover, errors when too big), and add a `summarize` sub-agent tool that condenses files.

**Architecture:** A single `spillIfTooLong` choke point in `GuardedTool.InvokableRun` writes oversized results to `<workRoot>/.yanshi/tmp/spillover/` and returns a head+tail preview + path. `fs_read` self-guards (errors, never spills) to avoid read-spill loops. `summarize` mirrors `analysis` as a predefined agent + dedicated tool. The work root reaches the spill layer via a new `WithWorkRoot` context value, injected by the orchestrator alongside `WithProfile`.

**Tech Stack:** Go 1.26.4, Eino ADK, `internal/tools` (GuardedTool, FSTools, AgentTools), `internal/agent/orchestrator`, `internal/bootstrap`. Tests use `github.com/stretchr/testify`, `einollm.FakeModel`/`FakeModel{Echo:true}`, `t.TempDir()`.

**Spec:** `docs/superpowers/specs/2026-07-19-tool-output-management-design.md`

---

## File Structure

| File | Responsibility | Task |
|---|---|---|
| `internal/tools/workroot.go` (new) | `WithWorkRoot` / `WorkRootFromContext` ctx helpers | 1 |
| `internal/tools/workroot_test.go` (new) | ctx round-trip tests | 1 |
| `internal/tools/spillover.go` (new) | `SpillThreshold`, `spillIfTooLong`, `spillPreview`, `capLines`, `humanBytes`, `relPath`, `randSuffix`, `Sweep` | 2 |
| `internal/tools/spillover_test.go` (new) | threshold/preview/sweep tests | 2 |
| `internal/tools/guard.go` | `InvokableRun` success path calls `spillIfTooLong` | 3 |
| `internal/tools/guard_test.go` | end-to-end spill through GuardedTool | 3 |
| `internal/tools/fs.go` | `fs_read` rewrite (`offset`+`end`, streaming, self-guard); drop `fsReadMaxBytes`; add `.yanshi` to ignores; skip `.yanshi` in list/glob | 4, 5 |
| `internal/tools/fs_test.go` | windowing → `end`; oversize-error test; drop truncation tests; `.yanshi` skip tests | 4, 5 |
| `internal/tools/predefined.go` | register `summarize` predefined agent | 6 |
| `internal/tools/agent_test.go` | `summarize` predefined-agent + tool tests | 6, 7 |
| `internal/tools/agent.go` | `Summarize` field + `runSummarize` | 7 |
| `internal/bootstrap/bootstrap.go` | register `Summarize`; pass `WorkRoot` to orchestrator.Config; call `tools.Sweep(workRoot)` | 7, 8 |
| `internal/agent/orchestrator/orchestrator.go` | `Config.WorkRoot`; store; inject `WithWorkRoot` at 4 turn sites | 8 |
| `internal/agent/orchestrator/orchestrator_test.go` | WorkRoot injection test | 8 |

---

## Task 1: WorkRoot context helpers

**Files:**
- Create: `internal/tools/workroot.go`
- Test: `internal/tools/workroot_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/tools/workroot_test.go`:

```go
package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWorkRootRoundTrip(t *testing.T) {
	ctx := context.Background()
	assert.Equal(t, "", WorkRootFromContext(ctx), "unbound ctx yields empty root")

	ctx = WithWorkRoot(ctx, "/some/proj")
	assert.Equal(t, "/some/proj", WorkRootFromContext(ctx), "bound root round-trips")
}

func TestWorkRootEmptyAllowed(t *testing.T) {
	// An empty root is stored verbatim (not conflated with "unbound");
	// spillIfTooLong maps "" → ".". This keeps WithWorkRoot callable
	// unconditionally.
	ctx := WithWorkRoot(context.Background(), "")
	assert.Equal(t, "", WorkRootFromContext(ctx))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tools -run TestWorkRoot -v`
Expected: build failure — `undefined: WithWorkRoot` / `WorkRootFromContext`.

- [ ] **Step 3: Write the implementation**

Create `internal/tools/workroot.go`:

```go
package tools

import "context"

// workRootKey keys the project work root in the tool-execution context.
type workRootKey struct{}

// WithWorkRoot binds the project work root into ctx so the spillover layer
// (spillIfTooLong) knows where to write oversized tool outputs
// (<root>/.yanshi/tmp/spillover/). The orchestrator installs it alongside
// WithProfile at each turn's tool-execution context. An empty root is stored
// verbatim; spillIfTooLong treats "" as ".".
func WithWorkRoot(ctx context.Context, root string) context.Context {
	return context.WithValue(ctx, workRootKey{}, root)
}

// WorkRootFromContext returns the bound work root, or "" when none is bound
// (spillIfTooLong then falls back to ".").
func WorkRootFromContext(ctx context.Context) string {
	r, _ := ctx.Value(workRootKey{}).(string)
	return r
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tools -run TestWorkRoot -v`
Expected: PASS (both subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/tools/workroot.go internal/tools/workroot_test.go
git commit -m "feat(tools): add WithWorkRoot ctx helper for spillover work-root"
```

---

## Task 2: Spillover core

**Files:**
- Create: `internal/tools/spillover.go`
- Test: `internal/tools/spillover_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/tools/spillover_test.go`:

```go
package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpillIfTooLong_UnderThresholdUnchanged(t *testing.T) {
	ctx := WithWorkRoot(context.Background(), t.TempDir())
	in := strings.Repeat("x", SpillThreshold) // exactly at threshold
	got := spillIfTooLong(ctx, "shell_run", in)
	assert.Equal(t, in, got, "result at threshold must pass through unchanged")
}

func TestSpillIfTooLong_OverThresholdSpills(t *testing.T) {
	root := t.TempDir()
	ctx := WithWorkRoot(context.Background(), root)
	var b strings.Builder
	for i := 0; i < 200; i++ {
		b.WriteString(strings.Repeat("a", 1023) + "\n") // ~1 KiB/line → ~200 KiB
	}
	in := b.String()
	require.Greater(t, len(in), SpillThreshold)

	got := spillIfTooLong(ctx, "shell_run", in)
	assert.Contains(t, got, "[spilled:")
	assert.Contains(t, got, ".yanshi/tmp/spillover/shell_run-", "temp path surfaced to model")
	assert.Contains(t, got, "lines omitted")
	assert.Contains(t, got, "Use summarize(path)")

	// Exactly one temp file written, holding the full original result.
	entries, err := os.ReadDir(filepath.Join(root, spillDir))
	require.NoError(t, err)
	require.Len(t, entries, 1, "exactly one spill file")
	data, err := os.ReadFile(filepath.Join(root, spillDir, entries[0].Name()))
	require.NoError(t, err)
	assert.Equal(t, in, string(data), "spill file holds the full original result")
}

func TestSpillIfTooLong_FallsBackToDot(t *testing.T) {
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(".", spillDir)) })
	in := strings.Repeat("x", SpillThreshold+1)
	got := spillIfTooLong(context.Background(), "web_fetch", in) // no WithWorkRoot
	assert.Contains(t, got, "[spilled:")
	assert.Less(t, len(got), len(in), "preview must be shorter than the original")
}

func TestSpillPreview_HeadAndTail(t *testing.T) {
	pad := strings.Repeat("x", 700)
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, fmt.Sprintf("LINE-%03d %s", i, pad))
	}
	in := strings.Join(lines, "\n")
	require.Greater(t, len(in), SpillThreshold)

	got := spillPreview("shell_run", in, ".yanshi/tmp/spillover/x.txt")
	assert.Contains(t, got, "LINE-000", "head includes first line")
	assert.Contains(t, got, "LINE-014", "head includes 15th line (spillHeadLines)")
	assert.Contains(t, got, "lines omitted")
	assert.Contains(t, got, "LINE-099", "tail includes last line")
	assert.NotContains(t, got, "LINE-050", "middle line must be omitted")
}

func TestSweep_RemovesSpillFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, spillDir)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("y"), 0o644))

	Sweep(root)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "Sweep must remove all spill files")
}

func TestSweep_MissingDirNoOp(t *testing.T) {
	Sweep(t.TempDir()) // must not panic / error
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tools -run "TestSpill|TestSweep" -v`
Expected: build failure — `undefined: SpillThreshold`, `spillIfTooLong`, `spillPreview`, `Sweep`, `spillDir`.

- [ ] **Step 3: Write the implementation**

Create `internal/tools/spillover.go`:

```go
package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SpillThreshold is the single uniform cap on any GuardedTool's output (64 KiB,
// ≈16k tokens). Results at or below this size are returned verbatim; larger
// results are written to a temp file under <workRoot>/.yanshi/tmp/spillover/ and
// replaced with a head+tail preview plus the temp path. fs_read self-guards and
// never returns an oversized string, so it never actually spills (see fs.go).
const SpillThreshold = 64 * 1024

// spillDir is the subpath under the work root where oversized tool outputs land.
const spillDir = ".yanshi/tmp/spillover"

// Preview window shown in place of an oversized result. Head+tail so trailing
// info (shell exit code, final JSON fields) stays visible alongside the head.
// The byte budgets keep the preview well under SpillThreshold even for inputs
// with a few very long lines.
const (
	spillHeadLines  = 15
	spillTailLines  = 10
	spillHeadBudget = 16 * 1024
	spillTailBudget = 8 * 1024
)

// spillIfTooLong returns result unchanged when len(result) <= SpillThreshold.
// Otherwise it writes result to a temp file under
// <workRoot>/.yanshi/tmp/spillover/<tool>-<unixms>-<rand>.txt and returns a
// head+tail preview plus the path and usage guidance. workRoot is read from ctx
// (WithWorkRoot); when empty it falls back to ".". A write failure degrades
// gracefully: the result is truncated to SpillThreshold with a footer noting the
// spill failed — a disk error must never fail the tool call itself.
func spillIfTooLong(ctx context.Context, toolName, result string) string {
	if len(result) <= SpillThreshold {
		return result
	}
	root := WorkRootFromContext(ctx)
	if root == "" {
		root = "."
	}
	dir := filepath.Join(root, spillDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return degradedSpill(result, err)
	}
	name := fmt.Sprintf("%s-%d-%s.txt", toolName, time.Now().UnixMilli(), randSuffix(4))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(result), 0o600); err != nil {
		return degradedSpill(result, err)
	}
	return spillPreview(toolName, result, relPath(root, path))
}

// degradedSpill truncates result to SpillThreshold and appends a footer noting
// the spill failed — used when the temp file cannot be written.
func degradedSpill(result string, cause error) string {
	trunc := result
	if len(trunc) > SpillThreshold {
		trunc = trunc[:SpillThreshold]
	}
	return trunc + fmt.Sprintf("\n\n[... spill to temp file failed (%v); result truncated to %d bytes ...]", cause, SpillThreshold)
}

// spillPreview builds the head+tail preview returned in place of an oversized
// result. rel is the path surfaced to the model (relative to the work root so it
// can be passed back to fs_read). Head and tail are byte-capped so the preview
// stays under SpillThreshold even for pathological inputs.
func spillPreview(toolName, result, rel string) string {
	lines := strings.Split(result, "\n")
	total := len(lines)

	headSrc := lines
	if len(headSrc) > spillHeadLines {
		headSrc = headSrc[:spillHeadLines]
	}
	head := capLines(headSrc, spillHeadBudget)

	var tail string
	var omitMsg string
	omit := total - spillHeadLines - spillTailLines
	if omit > 0 {
		omitMsg = fmt.Sprintf("\n[... %d lines omitted ...]\n", omit)
		tail = capLines(lines[total-spillTailLines:], spillTailBudget)
	}

	return fmt.Sprintf("[spilled: %d lines / %s → %s]\n%s%s%s\nUse summarize(path) to condense, or fs_read(path, offset, end) to page.",
		total, humanBytes(len(result)), rel, head, omitMsg, tail)
}

// capLines joins lines with newlines, but stops (truncating the final line) once
// the accumulated size would exceed budget. Guarantees the result is ≤ budget
// bytes for any input where each line is itself ≤ budget.
func capLines(lines []string, budget int) string {
	var b strings.Builder
	for i, ln := range lines {
		sep := ""
		if i > 0 {
			sep = "\n"
		}
		if b.Len()+len(sep)+len(ln) > budget {
			remain := budget - b.Len() - len(sep)
			if remain > 0 {
				b.WriteString(sep)
				b.WriteString(ln[:remain])
			}
			break
		}
		b.WriteString(sep)
		b.WriteString(ln)
	}
	return b.String()
}

// relPath returns path relative to root when possible (so the model gets a
// jail-under-root path it can hand back to fs_read), else path unchanged.
func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

// humanBytes renders n as e.g. "280 KiB" / "1.2 MiB" / "900 B".
func humanBytes(n int) string {
	const k = 1024
	switch {
	case n >= k*k:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(k*k))
	case n >= k:
		return fmt.Sprintf("%.0f KiB", float64(n)/float64(k))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// randSuffix returns n random bytes as a 2n-char hex string; on the rare
// crypto/rand read failure it returns a fixed fallback.
func randSuffix(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b)
}

// Sweep removes all regular files under <root>/.yanshi/tmp/spillover/. Call at
// process start to clear leftovers from a previous run. A missing directory is a
// no-op; per-file removal errors are ignored (best-effort). Subdirectories are
// left in place (the spillover dir is flat).
func Sweep(root string) {
	if root == "" {
		root = "."
	}
	dir := filepath.Join(root, spillDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tools -run "TestSpill|TestSweep" -v`
Expected: all 6 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tools/spillover.go internal/tools/spillover_test.go
git commit -m "feat(tools): add uniform 64KiB spillover layer (spillIfTooLong + Sweep)"
```

---

## Task 3: Wire spillover into GuardedTool

**Files:**
- Modify: `internal/tools/guard.go` (the success return in `InvokableRun`)
- Test: `internal/tools/guard_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/tools/guard_test.go`. First ensure its import block contains `os`, `path/filepath`, and `strings` — add any that are missing (current imports are `context`, `testing`, `schema`, `assert`, `require`, `guard`).

```go
func TestGuardedTool_SpillsOversizedResult(t *testing.T) {
	root := t.TempDir()
	big := NewGuardedTool("big", "returns a lot", nil,
		func(ctx context.Context, argsJSON string) (string, error) {
			return strings.Repeat("z", SpillThreshold+1), nil
		})
	ctx := WithWorkRoot(WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	}), root)

	out, err := big.InvokableRun(ctx, `{}`)
	require.NoError(t, err)
	assert.Contains(t, out, "[spilled:")
	assert.Contains(t, out, ".yanshi/tmp/spillover/big-")

	entries, err := os.ReadDir(filepath.Join(root, spillDir))
	require.NoError(t, err)
	require.Len(t, entries, 1, "oversized result must spill to one temp file")
}

func TestGuardedTool_SmallResultUnchanged(t *testing.T) {
	small := NewGuardedTool("small", "returns a little", nil,
		func(ctx context.Context, argsJSON string) (string, error) {
			return "ok", nil
		})
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	out, err := small.InvokableRun(ctx, `{}`)
	require.NoError(t, err)
	assert.Equal(t, "ok", out, "small result must pass through unchanged")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tools -run "TestGuardedTool_SpillsOversizedResult|TestGuardedTool_SmallResultUnchanged" -v`
Expected: `TestGuardedTool_SpillsOversizedResult` FAILs — currently `InvokableRun` returns the oversized string verbatim, so `out` lacks `[spilled:` and no temp file is created. (`SmallResultUnchanged` should already PASS.)

- [ ] **Step 3: Wire spillIfTooLong into the success path**

In `internal/tools/guard.go`, `InvokableRun` currently ends with:

```go
	if c := getErrCounter(ctx); c != nil {
		*c = 0
	}
	return out, nil
}
```

Change the final `return` to route through `spillIfTooLong`:

```go
	if c := getErrCounter(ctx); c != nil {
		*c = 0
	}
	return spillIfTooLong(ctx, g.name, out), nil
}
```

Leave the `errorResult` branches above unchanged (errors are small and must stay visible verbatim).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tools -run "TestGuardedTool_" -v`
Expected: all GuardedTool tests PASS, including the two new spill tests.

- [ ] **Step 5: Commit**

```bash
git add internal/tools/guard.go internal/tools/guard_test.go
git commit -m "feat(tools): spill oversized GuardedTool results to temp files"
```

---

## Task 4: Rewrite fs_read (offset + end, streaming, self-guard)

**Files:**
- Modify: `internal/tools/fs.go` — replace `fsReadArgs`, `runRead`; remove `fsReadMaxBytes`; add `"bufio"` to imports
- Test: `internal/tools/fs_test.go` — rewrite `TestFS_ReadWindowing`; replace `TestFS_ReadSignalsTruncation` + `TestFS_ReadNoTruncationMarkerForSmallFile`

- [ ] **Step 1: Write the failing tests (replace the windowing + truncation tests)**

In `internal/tools/fs_test.go`:

(a) Replace the entire `TestFS_ReadWindowing` function (lines ~63–98) with:

```go
func TestFS_ReadWindowing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lines.txt")
	require.NoError(t, os.WriteFile(p, []byte("line1\nline2\nline3\nline4\nline5\n"), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir + "/**"}},
	})

	t.Run("offset within range starts at requested line", func(t *testing.T) {
		out, err := runTool(ctx, fs.Read, `{"path":"lines.txt","offset":2}`)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(out, "2\tline2"), "got %q", out)
		assert.NotContains(t, out, "1\tline1")
	})

	t.Run("end only caps line range", func(t *testing.T) {
		out, err := runTool(ctx, fs.Read, `{"path":"lines.txt","end":2}`)
		require.NoError(t, err)
		assert.Equal(t, "1\tline1\n2\tline2", out)
	})

	t.Run("offset and end both respected", func(t *testing.T) {
		out, err := runTool(ctx, fs.Read, `{"path":"lines.txt","offset":2,"end":3}`)
		require.NoError(t, err)
		assert.Equal(t, "2\tline2\n3\tline3", out)
	})

	t.Run("offset beyond EOF returns empty without panic", func(t *testing.T) {
		out, err := runTool(ctx, fs.Read, `{"path":"lines.txt","offset":1000}`)
		require.NoError(t, err)
		assert.Equal(t, "", out)
	})

	t.Run("end less than offset errors", func(t *testing.T) {
		out, err := runTool(ctx, fs.Read, `{"path":"lines.txt","offset":3,"end":2}`)
		require.NoError(t, err)
		assert.Contains(t, out, "error")
		assert.Contains(t, out, "end")
	})
}
```

(b) Replace the entire `TestFS_ReadSignalsTruncation` function (lines ~100–136) with:

```go
func TestFS_Read_OversizeWindowErrors(t *testing.T) {
	dir := t.TempDir()
	// Build a file well over SpillThreshold (64 KiB): ~200 lines of ~697 B.
	var b strings.Builder
	for i := 0; i < 200; i++ {
		b.WriteString("L" + strings.Repeat("x", 695) + "\n")
	}
	content := b.String()
	require.Greater(t, len(content), SpillThreshold)

	p := filepath.Join(dir, "big.txt")
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir + "/**"}},
	})

	// No narrow window → whole file → oversized → errorResult, NOT spilled.
	out, err := runTool(ctx, fs.Read, `{"path":"big.txt"}`)
	require.NoError(t, err, "oversize must surface as a result, not a Go error")
	assert.Contains(t, out, "error")
	assert.Contains(t, out, "exceeds")
	assert.Contains(t, out, "narrow offset/end")
	// Crucially: fs_read never spills to a temp file.
	_, derr := os.ReadDir(filepath.Join(dir, ".yanshi", "tmp", "spillover"))
	assert.True(t, os.IsNotExist(derr), "fs_read must not create a spill dir")

	// A narrow window on the same big file succeeds and returns just those lines.
	out, err = runTool(ctx, fs.Read, `{"path":"big.txt","offset":1,"end":3}`)
	require.NoError(t, err)
	assert.NotContains(t, out, "error")
	assert.True(t, strings.HasPrefix(out, "1\tL"), "narrow window returns content, got %q", out)
}
```

(c) Delete the `TestFS_ReadNoTruncationMarkerForSmallFile` function entirely (small-file behavior is already covered by `TestFS_Read`).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tools -run "TestFS_ReadWindowing|TestFS_Read_OversizeWindowErrors" -v`
Expected: build/test failure — `fs_read` still uses `limit`/`fsReadMaxBytes`; `end` arg and `SpillThreshold` reference in fs.go not yet implemented.

- [ ] **Step 3: Rewrite fs_read in `internal/tools/fs.go`**

(a) Add `"bufio"` to the import block (after `"context"` the imports read `context, fmt, io, os, ...`; insert `"bufio"` first).

(b) Delete the `fsReadMaxBytes` constant (line ~276):
```go
const fsReadMaxBytes = 256 * 1024 // 256 KiB cap to protect context
```
Keep `fsSearchMaxBytes` (used by fs_search).

(c) Replace the `fsReadArgs` struct + `runRead` method (lines ~270–327) with:

```go
type fsReadArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	End    int    `json:"end"`
}

func (f *FSTools) runRead(ctx context.Context, argsJSON string) (string, error) {
	var a fsReadArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	paths, err := f.checkFS(ctx, "read", "fs_read", argsJSON, a.Path)
	if err != nil {
		return "", err
	}
	absPath := paths[0]
	fi, err := os.Stat(absPath)
	if err != nil {
		return "", fmt.Errorf("fs_read: %w", err)
	}
	totalBytes := fi.Size()

	offset := a.Offset
	if offset < 1 {
		offset = 1
	}
	if a.End != 0 && a.End < offset {
		return errorResult(fmt.Sprintf(
			"fs_read: end (%d) must be >= offset (%d)", a.End, offset)), nil
	}

	// Single streaming pass: count total lines and collect [offset, end].
	// Memory is bounded by the window (only collected lines are held); the whole
	// file is scanned once so the oversize error can report totalLines for
	// paging guidance.
	fc, err := os.Open(absPath)
	if err != nil {
		return "", fmt.Errorf("fs_read: %w", err)
	}
	defer fc.Close()

	var collected []string
	totalLines := 0
	sc := bufio.NewScanner(fc)
	sc.Buffer(make([]byte, 64*1024), 1024*1024) // tolerate long lines up to 1 MiB
	for sc.Scan() {
		totalLines++
		if totalLines >= offset && (a.End == 0 || totalLines <= a.End) {
			// bufio.Scanner.ScanLines strips the trailing '\n' but leaves a
			// trailing '\r' on CRLF files — drop it so output is newline-clean.
			collected = append(collected, strings.TrimSuffix(sc.Text(), "\r"))
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("fs_read: scan: %w", err)
	}
	end := a.End
	if end == 0 {
		end = totalLines
	}

	// offset beyond EOF → empty window.
	if offset > totalLines {
		return "", nil
	}

	out := make([]string, 0, len(collected))
	for i, text := range collected {
		out = append(out, fmt.Sprintf("%d\t%s", offset+i, text))
	}
	result := strings.Join(out, "\n")

	// Self-guard: fs_read never returns an oversized string. If it spilled to a
	// temp file, the model would fs_read that temp file and spill again — an
	// unbounded loop. Error instead so the model narrows offset/end or calls
	// summarize. Because this returns a short errorResult, the uniform
	// spillIfTooLong in GuardedTool never fires for fs_read.
	if len(result) > SpillThreshold {
		return errorResult(fmt.Sprintf(
			"fs_read: result window %s (%d lines %d–%d of %d, file %s) exceeds %s limit; narrow offset/end, or summarize(path)",
			humanBytes(len(result)), len(collected), offset, end, totalLines,
			humanBytes(int(totalBytes)), humanBytes(SpillThreshold))), nil
	}

	return f.withInstructions(absPath, result), nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tools -run "TestFS_Read" -v`
Expected: all `TestFS_Read*` tests PASS (including rewritten windowing + oversize-error tests; the CRLF and nested-AGENTS.md tests pass unchanged).

- [ ] **Step 5: Run the full tools package to catch regressions**

Run: `go test ./internal/tools ./internal/agent/orchestrator -count=1`
Expected: PASS. (Orchestrator tests call fs_read indirectly; the removed `fsReadMaxBytes` / changed arg name must not break them — fix any call site that sent `limit` if one surfaces.)

- [ ] **Step 6: Commit**

```bash
git add internal/tools/fs.go internal/tools/fs_test.go
git commit -m "feat(tools): fs_read offset+end paging, streaming, self-guard on oversize"
```

---

## Task 5: Exclude `.yanshi` from list / search / glob

**Files:**
- Modify: `internal/tools/fs.go` — add `.yanshi` to `fsSearchIgnore`; skip `.yanshi` in `runList` and `runGlob`
- Test: `internal/tools/fs_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/tools/fs_test.go`:

```go
func TestFS_SearchIgnoresYanshiScratch(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".yanshi", "tmp", "spillover"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".yanshi", "tmp", "spillover", "shell_run-x.txt"), []byte("TODO leaked scratch\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("TODO visible\n"), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir, dir + "/**"}},
	})

	out, err := runTool(ctx, fs.Search, `{"pattern":"TODO","path":".","output_mode":"files_with_matches"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "main.go")
	assert.NotContains(t, out, "shell_run-x.txt", "spillover scratch must be hidden from search")
}

func TestFS_ListSkipsYanshiDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".yanshi"), 0o755))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir, dir + "/**"}},
	})
	out, err := runTool(ctx, fs.List, `{"path":"."}`)
	require.NoError(t, err)
	assert.Contains(t, out, "a.go")
	assert.NotContains(t, out, ".yanshi", "list must hide the .yanshi scratch dir")
}

func TestFS_GlobSkipsYanshiDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".yanshi", "tmp", "spillover"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".yanshi", "tmp", "spillover", "shell_run-1.txt"), []byte("x"), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir, dir + "/**"}},
	})
	out, err := runTool(ctx, fs.Glob, `{"pattern":"**/*.txt"}`)
	require.NoError(t, err)
	assert.NotContains(t, out, "shell_run-1.txt", "glob must not descend into .yanshi")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tools -run "TestFS_SearchIgnoresYanshiScratch|TestFS_ListSkipsYanshiDir|TestFS_GlobSkipsYanshiDir" -v`
Expected: all three FAIL (search/list/glob currently surface `.yanshi` contents).

- [ ] **Step 3: Add `.yanshi` to the ignore set and skip it in list/glob**

In `internal/tools/fs.go`:

(a) Update `fsSearchIgnore`:

```go
var fsSearchIgnore = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".hg": true, ".svn": true,
	".yanshi": true,
}
```

(b) In `runList`, inside the loop over `entries`, skip `.yanshi` at the root. Replace the loop body's start:

```go
	for _, e := range entries {
		if a.Pattern != "" {
```
with:

```go
	for _, e := range entries {
		if e.Name() == ".yanshi" {
			continue // hide the spillover scratch dir from listings
		}
		if a.Pattern != "" {
```

(c) In `runGlob`, inside the `WalkDir` callback, skip `.yanshi` directories. Replace:

```go
		if d.IsDir() {
			return nil
		}
```
with:

```go
		if d.IsDir() {
			if fsSearchIgnore[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
```

(This mirrors the existing `runSearch` ignore behavior, so glob now also prunes `.git`/`node_modules`/`.yanshi`/etc.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tools -run "TestFS_SearchIgnoresYanshiScratch|TestFS_ListSkipsYanshiDir|TestFS_GlobSkipsYanshiDir" -v`
Expected: all three PASS.

- [ ] **Step 5: Run full tools package for regressions**

Run: `go test ./internal/tools -count=1`
Expected: PASS (the existing `TestFS_SearchIgnoresDotGit` and friends still pass; `.yanshi` is additive).

- [ ] **Step 6: Commit**

```bash
git add internal/tools/fs.go internal/tools/fs_test.go
git commit -m "feat(tools): hide .yanshi scratch dir from fs_list/fs_search/fs_glob"
```

---

## Task 6: Register `summarize` predefined agent

**Files:**
- Modify: `internal/tools/predefined.go` — add `"summarize"` entry to `PredefinedAgents`
- Test: `internal/tools/agent_test.go` (where `TestAnalysis_PredefinedAgentDefinition` lives)

- [ ] **Step 1: Write the failing test**

Append to `internal/tools/agent_test.go`:

```go
func TestSummarize_PredefinedAgentDefinition(t *testing.T) {
	def, ok := GetPredefinedAgent("summarize")
	require.True(t, ok, "summarize predefined agent must exist")
	assert.Equal(t, "summarize", def.Name)
	assert.NotEmpty(t, def.Description)
	assert.Contains(t, def.PromptTmpl, "{{target}}")
	assert.Contains(t, def.PromptTmpl, "{{max_lines}}")
	assert.Contains(t, def.PromptTmpl, "{{focus_line}}")
	assert.Nil(t, def.Workflow, "summarize is single-agent, no workflow")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tools -run TestSummarize_PredefinedAgentDefinition -v`
Expected: FAIL — `GetPredefinedAgent("summarize")` returns `ok=false`.

- [ ] **Step 3: Register the summarize agent**

In `internal/tools/predefined.go`, inside the `PredefinedAgents` map (after the `"analysis"` entry, before the closing `}`), add:

```go
	// -----------------------------------------------------------------------
	// Summarize – read a file and condense it into a structured summary
	// -----------------------------------------------------------------------
	"summarize": {
		Name:        "summarize",
		Description: "读取文件并产出结构化总结（支持大文件分页读取）",
		PromptTmpl: `你是内容总结专家。请阅读目标文件并产出结构化总结。

目标文件: {{target}}
{{focus_line}}
要求:
- 用 fs_read 分页读取（offset/end），不要假设一次能读完。
- 产出: ① 核心要点 ② 结构/章节 ③ 关键片段（必要时引用行号）。
- 总结不超过 {{max_lines}} 行。
- 若是日志/输出，提取异常、错误、关键时间线。
- 若是代码，概述职责、公开符号、依赖关系。`,
	},
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tools -run "TestSummarize_PredefinedAgentDefinition|TestPredefinedAgents_" -v`
Expected: PASS. (`TestPredefinedAgents_List` uses `Contains` and still passes.)

- [ ] **Step 5: Commit**

```bash
git add internal/tools/predefined.go internal/tools/agent_test.go
git commit -m "feat(tools): register summarize predefined agent"
```

---

## Task 7: `summarize` tool + bootstrap registration

**Files:**
- Modify: `internal/tools/agent.go` — add `Summarize` field + build it in `NewAgentTools` + `runSummarize`
- Modify: `internal/bootstrap/bootstrap.go` — register `agentTools.Summarize`
- Test: `internal/tools/agent_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/tools/agent_test.go`:

```go
func TestSummarize_Basic(t *testing.T) {
	at, ctx := newAgentToolsEcho(t)
	result, err := at.Summarize.InvokableRun(ctx, `{"path":"main.go"}`)
	require.NoError(t, err)
	var m map[string]string
	require.NoError(t, json.Unmarshal([]byte(result), &m))
	assert.Contains(t, m["result"], "main.go", "target interpolated into prompt (echo model)")
	assert.Contains(t, m["result"], "核心要点", "summarize prompt body present")
	assert.Contains(t, m["result"], "不超过 50", "default max_lines applied")
	assert.Equal(t, "main.go", m["target"])
}

func TestSummarize_FocusAndMaxLines(t *testing.T) {
	at, ctx := newAgentToolsEcho(t)
	result, err := at.Summarize.InvokableRun(ctx, `{"path":"x.go","focus":"error handling","max_lines":20}`)
	require.NoError(t, err)
	var m map[string]string
	require.NoError(t, json.Unmarshal([]byte(result), &m))
	assert.Contains(t, m["result"], "重点关注: error handling")
	assert.Contains(t, m["result"], "不超过 20 行")
}

func TestSummarize_EmptyPath(t *testing.T) {
	at, ctx := newAgentTools(t)
	result, err := at.Summarize.InvokableRun(ctx, `{"path":""}`)
	require.NoError(t, err)
	var m map[string]string
	require.NoError(t, json.Unmarshal([]byte(result), &m))
	assert.Contains(t, m["error"], "path must not be empty")
}

func TestSummarize_RestrictsToFsRead(t *testing.T) {
	at, ctx := newAgentTools(t)
	var captured []string
	runner := func(ic context.Context, prompt string, allowed []string, instr string) (string, error) {
		captured = allowed
		return "summary", nil
	}
	ctx = WithSubAgentRunner(ctx, SubAgentRunner(runner))
	_, err := at.Summarize.InvokableRun(ctx, `{"path":"x.go"}`)
	require.NoError(t, err)
	assert.Equal(t, []string{"fs_read"}, captured, "summarize sub-agent may only use fs_read")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tools -run TestSummarize_ -v`
Expected: build failure — `at.Summarize` undefined.

- [ ] **Step 3: Add the Summarize tool to AgentTools**

In `internal/tools/agent.go`:

(a) Add the field to the `AgentTools` struct (after `Analysis *GuardedTool`):

```go
	Summarize *GuardedTool
```

(b) In `NewAgentTools`, after the `t.Analysis = NewGuardedTool(...)` block and before `return t, nil`, add:

```go
	t.Summarize = NewGuardedTool(
		"summarize",
		"Read a file and produce a structured summary. Handles large files by paging "+
			"with fs_read. Use this to condense a long file (or a spilled tool output under "+
			".yanshi/tmp/spillover/) instead of reading it whole.\n"+
			"  • path — file to summarize (project file or spilled temp file).\n"+
			"  • focus (optional) — what to emphasize, e.g. \"error handling\".\n"+
			"  • max_lines (optional, default 50) — target summary length.",
		params(map[string]*schema.ParameterInfo{
			"path":      {Type: schema.String, Desc: "file path to summarize", Required: true},
			"focus":     {Type: schema.String, Desc: "optional focus, e.g. \"error handling\""},
			"max_lines": {Type: schema.Integer, Desc: "target max summary length in lines (default 50)"},
		}),
		t.runSummarize,
	)
```

(c) Add `runSummarize` and its args struct. Place it right after the `runAnalysisAgent` function:

```go
type summarizeArgs struct {
	Path     string `json:"path"`
	Focus    string `json:"focus"`
	MaxLines int    `json:"max_lines"`
}

// runSummarize runs the predefined "summarize" agent over a target file. The
// sub-agent is restricted to fs_read only (it must page through large files
// rather than mutate them). focus and max_lines are interpolated into the
// predefined prompt; max_lines defaults to 50.
func (t *AgentTools) runSummarize(ctx context.Context, argsJSON string) (string, error) {
	var a summarizeArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	if a.Path == "" {
		return errorResult("path must not be empty"), nil
	}
	if t.chatModel == nil {
		return errorResult("no chat model configured; cannot summarize"), nil
	}
	def, ok := GetPredefinedAgent("summarize")
	if !ok {
		return errorResult("internal error: summarize predefined agent not found"), nil
	}
	maxLines := a.MaxLines
	if maxLines <= 0 {
		maxLines = 50
	}
	focusLine := ""
	if f := strings.TrimSpace(a.Focus); f != "" {
		focusLine = "重点关注: " + f
	}
	prompt := FillPrompt(def.PromptTmpl, map[string]string{
		"target":     a.Path,
		"focus_line": focusLine,
		"max_lines":  strconv.Itoa(maxLines),
	})
	result, err := t.runSubAgent(WithLeafSubAgentTools(ctx), prompt, []string{"fs_read"}, "")
	if err != nil {
		return "", err
	}
	return toJSON(map[string]string{"result": result, "target": a.Path}), nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tools -run TestSummarize_ -v`
Expected: all four summarize tests PASS.

- [ ] **Step 5: Register summarize in bootstrap**

In `internal/bootstrap/bootstrap.go`, change the agent-tools registration line:

```go
	allTools = append(allTools, agentTools.StartAgent, agentTools.StartWorkflow, agentTools.Analysis)
```
to:

```go
	allTools = append(allTools, agentTools.StartAgent, agentTools.StartWorkflow, agentTools.Analysis, agentTools.Summarize)
```

- [ ] **Step 6: Build + commit**

Run: `go build ./...`
Expected: builds cleanly.

```bash
git add internal/tools/agent.go internal/tools/agent_test.go internal/bootstrap/bootstrap.go
git commit -m "feat(tools): add summarize tool (fs_read-only sub-agent) + register in bootstrap"
```

---

## Task 8: Orchestrator WorkRoot injection + bootstrap Sweep + final verification

**Files:**
- Modify: `internal/agent/orchestrator/orchestrator.go` — `Config.WorkRoot` field; store `workRoot`; inject `tools.WithWorkRoot` at the 4 turn entry points
- Modify: `internal/bootstrap/bootstrap.go` — pass `WorkRoot` to `orchestrator.Config`; call `tools.Sweep(workRoot)` after orchestrator build
- Test: `internal/agent/orchestrator/orchestrator_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/agent/orchestrator/orchestrator_test.go`:

```go
// TestOrchestrator_InjectsWorkRoot proves the orchestrator injects its
// configured WorkRoot into every turn's tool-execution context: a tool that
// returns an oversized result triggers spillIfTooLong, which reads
// WorkRootFromContext and writes under <root>/.yanshi/tmp/spillover/. The spill
// file landing under the configured root is the observable proof.
func TestOrchestrator_InjectsWorkRoot(t *testing.T) {
	root := t.TempDir()

	big := tools.NewGuardedTool("big", "returns a lot", nil,
		func(ctx context.Context, argsJSON string) (string, error) {
			return strings.Repeat("z", tools.SpillThreshold+1), nil
		})

	tc1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name: "big", Arguments: `{}`,
		}},
	})
	tc2 := schema.AssistantMessage("done", nil)
	model := einollm.NewFakeModelWithMessages([]*schema.Message{tc1, tc2}, nil)

	o, err := New(Config{Model: model, Tools: []BaseTool{big}, WorkRoot: root})
	require.NoError(t, err)

	out, err := o.Query(context.Background(), "call big")
	require.NoError(t, err)
	assert.Equal(t, "done", out)

	entries, err := os.ReadDir(filepath.Join(root, ".yanshi", "tmp", "spillover"))
	require.NoError(t, err, "spill file must exist under the configured WorkRoot")
	assert.Len(t, entries, 1, "oversized tool result spilled to one file")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent/orchestrator -run TestOrchestrator_InjectsWorkRoot -v`
Expected: build failure — `Config.WorkRoot` undefined.

- [ ] **Step 3: Add WorkRoot to Config, store it, and inject at the 4 turn sites**

In `internal/agent/orchestrator/orchestrator.go`:

(a) Add a field to `Config` (place it right after the `VCSScope` field):

```go
	// WorkRoot is the project work root, injected into the tool-execution
	// context (tools.WithWorkRoot) so the spillover layer knows where to write
	// oversized tool outputs (<root>/.yanshi/tmp/spillover/). May be empty;
	// spillIfTooLong then falls back to ".".
	WorkRoot string
```

(b) Add a field to the `Orchestrator` struct (after `vcsScope tools.VCSScope`):

```go
	workRoot string
```

(c) In `New()`, set it on the returned struct. In the `return &Orchestrator{...}` literal, add:

```go
		workRoot:       cfg.WorkRoot,
```

(d) Inject `WithWorkRoot` at all four turn entry points. Each currently begins:

```go
	ctx = tools.WithProfile(ctx, o.profile)
	ctx = o.bindSubAgentRunner(ctx)
```

Add one line after the `WithProfile` line at all four sites (`Query`, `Events`, `EventsWithHistory`, `EventsWithHistoryOpts`):

```go
	ctx = tools.WithProfile(ctx, o.profile)
	ctx = tools.WithWorkRoot(ctx, o.workRoot)
	ctx = o.bindSubAgentRunner(ctx)
```

- [ ] **Step 4: Run orchestrator test to verify it passes**

Run: `go test ./internal/agent/orchestrator -run TestOrchestrator_InjectsWorkRoot -v`
Expected: PASS — the spill file appears under `root`.

- [ ] **Step 5: Wire WorkRoot + Sweep into bootstrap**

In `internal/bootstrap/bootstrap.go`:

(a) Add `WorkRoot` to the `orchConfig` literal (inside `orchConfig := orchestrator.Config{...}`, e.g. after `Instruction:`):

```go
		WorkRoot:         workRoot,
```

(b) After the orchestrator build succeeds, sweep stale spill files. Find:

```go
	orch, err := orchestrator.New(orchConfig)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("bootstrap: orchestrator: %w", err)
	}
```

Add immediately after that block:

```go
	// Clear leftover oversized-output temp files from a previous run before any
	// tool can write new ones. Best-effort: a missing dir is a no-op.
	tools.Sweep(workRoot)
```

- [ ] **Step 6: Full verification**

Run each command and confirm the expected result:

```bash
go build ./...
go vet ./...
go test ./internal/tools ./internal/agent/orchestrator ./internal/bootstrap -count=1
```
Expected: all PASS, no vet warnings.

Then the startup smoke (per CLAUDE.md) — confirm Sweep + tool registration don't break boot:

```bash
go build -o yanshi ./cmd/yanshi && timeout 5 ./yanshi --fake-model -inprocess
```
Expected: starts without panic, prints normal startup (alt-screen TUI; timeout exits 124 after 5s, which is expected for a TUI that doesn't self-terminate). Confirm no `bootstrap:` error on stderr.

- [ ] **Step 7: Commit**

```bash
git add internal/agent/orchestrator/orchestrator.go internal/agent/orchestrator/orchestrator_test.go internal/bootstrap/bootstrap.go
git commit -m "feat(orchestrator): inject WorkRoot for spillover + sweep scratch on boot"
```

---

## Self-Review Notes

**Spec coverage:** Every spec section maps to a task — §1 spillover layer → Task 2; §2 WithWorkRoot → Task 1; §3 GuardedTool wiring → Task 3; §4 fs_read → Task 4; §5 summarize (predefined + tool) → Tasks 6–7; §6 `.yanshi` exclusion → Task 5; orchestrator/bootstrap wiring → Task 8. The fs_read self-guard (no spillover) is implemented inline in Task 4 step 3.

**Type consistency:** `SpillThreshold` / `spillIfTooLong` / `spillPreview` / `spillDir` / `humanBytes` / `Sweep` (package `tools`) are defined in Task 2 and referenced unchanged in Tasks 3, 4, 8. `WithWorkRoot` / `WorkRootFromContext` (Task 1) referenced unchanged in Tasks 2, 3, 8. `summarizeArgs` / `runSummarize` / `Summarize` field (Task 7) match the test usage. `Config.WorkRoot` / `o.workRoot` (Task 8) match across Config, struct, New(), and the 4 injection sites.

**Known edge cases handled:** spill write failure → degraded truncate (Task 2); fs_read oversize → errorResult, no spill, no loop (Task 4); CRLF normalization preserved (Task 4, `TrimSuffix "\r"`); `.yanshi` hidden from search/list/glob (Task 5); summarize with empty focus → `{{focus_line}}` filled as `""` not left literal (Task 7).
