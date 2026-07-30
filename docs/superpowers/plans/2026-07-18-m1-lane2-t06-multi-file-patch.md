# T06: 多文件 Patch 工具（apply_patch runtime）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 提供一个 `apply_patch` 工具，一次调用即可原子地执行 add / update / delete / move 多类多文件操作；`dry_run` 返回 unified-style diff 但不落盘；任一操作校验失败则整批回滚（不留半成品）；成功应用的编辑经 VCS scope 进入 autoVCS tracking；权限按 fs 写规则 + guard 整批校验。

**Architecture:** 在 `internal/tools` 包内新增一个 `apply_patch` GuardedTool（挂在现有 `FSTools` 上，复用其 `root`/`abs`/`checkFS`/`trackEdit` 基础设施，自动经 `bootstrap` 的 `fsTools.Tools()` 注册进 orchestrator）。实现分三层：(1) 纯 parser 把 codex 风格的 patch 文本解析成有序 op 列表；(2) prepare 阶段在内存中模拟整批操作（读取磁盘、按序应用到 in-memory 文件树），全量校验后产出"已暂存变更"列表——这一阶段不写盘，任何校验失败（add 已存在、update 上下文找不到、delete/move 源不存在、路径越界）直接整批 abort；(3) commit 阶段把暂存变更按序落盘，任一 I/O 错误回滚已落盘项。dry-run 只跑 prepare 并把暂存变更渲染成 unified diff 返回。VCS 追踪复用现有 `trackEdit`（add/update/move 目标），并新增对称的 `trackDelete`（delete/move 源）——后者需要给 `internal/vcs` 增加公开的 `RecordDeleteMain`/`RecordDeleteWorktree`（当前 VCS 只能记录内容写入，没有删除入口）。

**Tech Stack:** Go stdlib（无新依赖）；现有 `internal/tools`（FSTools/GuardedTool/checkFS/trackEdit）、`internal/vcs`（RecordEditMain/Worktree）、`internal/guard`（PermissionProfile/FSWant）。复用 `fs_edit.go` 的 `lenientFind`/`previewLines` 做 update 块的宽容匹配与错误提示。

**不变性（路线图风险约束）：**
- **原子性**：prepare 阶段不写盘；commit 阶段任一 I/O 失败回滚已落盘项。一个 `apply_patch` 调用结束后，磁盘要么反映全部 op，要么完全不变（不计 VCS 追踪这种 best-effort 副作用）。
- **dry-run 不落盘**：`dry_run=true` 时绝不调用 `os.WriteFile`/`os.Remove`，也不触发 VCS tracking。
- **不改现有 fs 工具行为**：`fs_read`/`fs_write`/`fs_edit`/… 字节不变；apply_patch 是纯增量。
- **审批粒度 = 整批**：所有受影响路径收进**一个** guard Action（一次 `Authorize`），交互模式下用户对整批 patch 只看到一次询问——与 all-or-nothing 语义一致。

---

## File Structure

- **Create** `internal/vcs/delete.go` — `RecordDeleteMain` / `RecordEditWorktree` 的删除对称方法 + `recordDelete` 内核（把 `op='deleted'` 写进 `vcs_uncommitted`）。独立成文件是因为 `vcs.go` 已 1026 行（超 1000 纯代码行约束）。
- **Create** `internal/vcs/delete_test.go` — 删除追踪单测（main 作用域 + commit 后从 tree 消失 + worktree 作用域）。
- **Create** `internal/tools/fs_patch_parse.go` — patch 文本 parser：op 类型常量、`patchOp`/`updatePair`、`parsePatch`、`takeRawBody`、`parseUpdateBody`。纯函数，无 I/O，可独立 TDD。
- **Create** `internal/tools/fs_patch_parse_test.go` — parser 单测（四类 op + 各种畸形输入）。
- **Create** `internal/tools/fs_diff.go` — `unifiedDiff`（基于 LCS 的行级 diff，O(n·m)，dry-run 渲染用，文件小）+ `splitDiffLines`。
- **Create** `internal/tools/fs_diff_test.go` — diff 单测。
- **Create** `internal/tools/fs_patch.go` — applier 主体：`stagedChange`、`preparePatch`（in-memory 校验+暂存）、`commitPatch`+`applyStaged`+`rollbackStaged`（原子落盘+回滚）、`renderPatchDiff`/`countLines`/`prefixLines`/`stagedRelPaths`、`applyOneEdit`（复用 `lenientFind`）、`opWritePaths`、`trackDelete`、`runPatch`（GuardedTool body）。
- **Create** `internal/tools/fs_patch_test.go` — applier 集成测试（add/update/delete/move、dry-run、原子回滚、权限拒绝、VCS 追踪）。
- **Modify** `internal/tools/fs.go` — 3 处小改：`FSTools` 结构体加 `Patch *GuardedTool` 字段；`NewFSTools` 加构造块；`Tools()` 切片加 `f.Patch`。无需改 `bootstrap.go`（它遍历 `fsTools.Tools()`，自动带上）。

---

## Task 1: VCS 删除追踪（`RecordDeleteMain` / `RecordDeleteWorktree`）

**Files:**
- Create: `internal/vcs/delete.go`
- Create: `internal/vcs/delete_test.go`

> 背景：当前 `vcs.go` 的 `recordEdit`（665 行）只插 `op='added'/'modified'` 的内容行；`commitScope`（771 行）虽会处理 `op=='deleted'`，但**没有公开入口**把删除写进 `vcs_uncommitted`。本任务补齐这个对称缺口，使 apply_patch 的 delete/move 能被 autoVCS 正确记录。放新文件是因为 `vcs.go` 已超 1000 行约束。

- [ ] **Step 1: 写失败测试**

`internal/vcs/delete_test.go`:
```go
package vcs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRecordDelete_MarksPathDeleted proves RecordDeleteMain writes a row with
// op="deleted" and that a subsequent CommitMain folds the deletion into the
// commit tree (the path is gone). This is the autoVCS contract for a deleted
// file: it must NOT persist in the committed snapshot.
func TestRecordDelete_MarksPathDeleted(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("package main"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	// a.go is tracked in main_head (InitRepo snapshots existing files).
	require.NoError(t, v.RecordDeleteMain(repoID, "orchestrator", filepath.Join(root, "a.go")))

	// The uncommitted changeset carries a.go with op="deleted".
	pending := v.Uncommitted("main", repoID)
	assert.Contains(t, pending, "a.go", "deleted path must be in the changeset")

	var op string
	row := v.store.DB.QueryRow(
		"SELECT op FROM vcs_uncommitted WHERE scope_type='main' AND scope_id=? AND path='a.go'",
		repoID)
	require.NoError(t, row.Scan(&op))
	assert.Equal(t, "deleted", op)

	// Committing folds the delete into the tree: a.go must be absent.
	commitID, err := v.CommitMain(repoID, "orchestrator", "delete a.go")
	require.NoError(t, err)
	assert.NotContains(t, v.commitTree(commitID), "a.go",
		"committed tree must not contain the deleted file")
}

// TestRecordDelete_WorktreeScope proves the worktree-scoped delete is symmetric
// with RecordEditWorktree and does not leak into main.
func TestRecordDelete_WorktreeScope(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	repo, _ := v.getRepo(repoID)
	// Insert a worktree row manually (AddWorktree materialization is not needed
	// to test the changeset recording path; mirror TestRecordEdit_WorktreeScope).
	wtID := "wt-del"
	_, err = v.store.DB.Exec(
		"INSERT INTO vcs_worktrees (id, repo_id, path, base_commit, created_at, active) VALUES (?, ?, ?, ?, ?, 1)",
		wtID, repoID, root, repo.MainHead, time.Now().Unix())
	require.NoError(t, err)

	require.NoError(t, v.RecordDeleteWorktree(wtID, "worker-1", filepath.Join(root, "a.go")))
	assert.Contains(t, v.Uncommitted("worktree", wtID), "a.go")
	assert.Empty(t, v.Uncommitted("main", repoID), "worktree delete must not touch main")
}

// TestRecordDelete_OutsideRepoSkipped mirrors RecordEdit's silent-skip behavior:
// a path outside the repo root is a no-op (no error, no row).
func TestRecordDelete_OutsideRepoSkipped(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)
	require.NoError(t, v.RecordDeleteMain(repoID, "orchestrator", filepath.Join(other, "external.go")))
	assert.Empty(t, v.Uncommitted("main", repoID))
}
```

- [ ] **Step 2: 运行测试确认失败**

Run:
```sh
go test ./internal/vcs -run TestRecordDelete -v
```
Expected: FAIL（`v.RecordDeleteMain` / `v.RecordDeleteWorktree` 未定义）。

- [ ] **Step 3: 实现 `delete.go`**

`internal/vcs/delete.go`:
```go
package vcs

import (
	"path/filepath"
	"strings"
)

// RecordDeleteMain records a deletion on the main scope: it upserts the path
// into vcs_uncommitted with op="deleted" so the next CommitMain removes it from
// the tree (commitScope deletes op="deleted" rows from the in-memory tree).
// agent is accepted for API symmetry with RecordEditMain; attribution is applied
// at Commit time. A path outside the repo root or ignored is silently skipped
// (mirroring recordEdit), so a delete outside the tracked area is a no-op.
//
// This is the autoVCS entry point for file deletions (used by apply_patch's
// delete/move operations); before this, only content writes (add/modify) could
// be auto-tracked, so a deleted file would have wrongly persisted in commits.
func (v *VCS) RecordDeleteMain(repoID, agent, absPath string) error {
	r, err := v.getRepo(repoID)
	if err != nil {
		return err
	}
	return v.recordDelete("main", repoID, r.RootPath, absPath)
}

// RecordDeleteWorktree records a deletion on a worktree scope. absPath is
// resolved against the worktree's working dir (wt.Path), exactly like
// RecordEditWorktree; a path outside wt.Path is silently skipped.
func (v *VCS) RecordDeleteWorktree(wtID, agent, absPath string) error {
	wt, err := v.getWorktree(wtID)
	if err != nil {
		return err
	}
	return v.recordDelete("worktree", wtID, wt.Path, absPath)
}

// recordDelete is the shared delete-track core: resolve absPath to a repo-relative
// path, skip silently if outside the repo root or ignored, and upsert the path
// into the scope's vcs_uncommitted with op="deleted" and an empty blob_hash (the
// blob is irrelevant — commitScope drops op="deleted" rows from the tree
// regardless of blob_hash). On conflict (path was added/modified then deleted in
// the same scope) op is overwritten to "deleted" so the net effect is a deletion.
func (v *VCS) recordDelete(scopeType, scopeID, repoRoot, absPath string) error {
	if repoRoot == "" {
		return nil
	}
	rel, err := filepath.Rel(repoRoot, absPath)
	if err != nil || strings.HasPrefix(filepath.ToSlash(rel), "..") {
		return nil // outside repo → skip silently
	}
	rel = filepath.ToSlash(rel)
	if v.isIgnored(rel) {
		return nil
	}
	_, err = v.store.DB.Exec(
		"INSERT INTO vcs_uncommitted (scope_type, scope_id, path, blob_hash, op) VALUES (?, ?, ?, '', 'deleted')\n"+
			"ON CONFLICT(scope_type, scope_id, path) DO UPDATE SET blob_hash='', op='deleted'",
		scopeType, scopeID, rel)
	return err
}
```

- [ ] **Step 4: 运行测试确认通过**

Run:
```sh
go test ./internal/vcs -run TestRecordDelete -v
```
Expected: PASS。

- [ ] **Step 5: 提交**

```sh
git add internal/vcs/delete.go internal/vcs/delete_test.go
git commit -m "feat(vcs): add RecordDelete{Main,Worktree} for autoVCS delete tracking (T06)"
```

---

## Task 2: Patch 文本 parser

**Files:**
- Create: `internal/tools/fs_patch_parse.go`
- Create: `internal/tools/fs_patch_parse_test.go`

> 纯函数、无 I/O，先 TDD。patch 格式（codex 风格信封 + 自定义块体，见函数 doc）：Add 体原样取（模型可整段粘贴文件内容）；Update 体为 unified 风格（空格=上下文、`-`=删、`+`=增），整块编译成一对 (old, new) 做单次搜索替换；Delete 无体；Move 后跟 `*** To:`。

- [ ] **Step 1: 写失败测试**

`internal/tools/fs_patch_parse_test.go`:
```go
package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePatch_AddUpdateDeleteMove(t *testing.T) {
	src := "*** Begin Patch\n" +
		"*** Add File: new.txt\n" +
		"hello\n" +
		"world\n" +
		"*** Update File: app.go\n" +
		" line1\n" +
		"-func A() {}\n" +
		"+func A() { return }\n" +
		" line3\n" +
		"*** Delete File: old.txt\n" +
		"*** Move File: src/a.go\n" +
		"*** To: src/b.go\n" +
		"*** End Patch"
	ops, err := parsePatch(src)
	require.NoError(t, err)
	require.Len(t, ops, 4)

	assert.Equal(t, opAdd, ops[0].kind)
	assert.Equal(t, "new.txt", ops[0].path)
	assert.Equal(t, "hello\nworld", ops[0].addBody)

	assert.Equal(t, opUpdate, ops[1].kind)
	assert.Equal(t, "app.go", ops[1].path)
	assert.Equal(t, "line1\nfunc A() {}\nline3", ops[1].updOld)
	assert.Equal(t, "line1\nfunc A() { return }\nline3", ops[1].updNew)

	assert.Equal(t, opDelete, ops[2].kind)
	assert.Equal(t, "old.txt", ops[2].path)

	assert.Equal(t, opMove, ops[3].kind)
	assert.Equal(t, "src/a.go", ops[3].from)
	assert.Equal(t, "src/b.go", ops[3].path)
}

func TestParsePatch_UpdatePureInsert(t *testing.T) {
	// Pure insertion (no removal): old=context, new=context+add.
	src := "*** Begin Patch\n*** Update File: f.go\n line1\n+inserted\n*** End Patch"
	ops, err := parsePatch(src)
	require.NoError(t, err)
	require.Len(t, ops, 1)
	assert.Equal(t, "line1", ops[0].updOld)
	assert.Equal(t, "line1\ninserted", ops[0].updNew)
}

func TestParsePatch_UpdateBlankContextLine(t *testing.T) {
	// A blank line inside an Update body is a context line with empty text.
	src := "*** Begin Patch\n*** Update File: f.go\n a\n\n-b\n+c\n*** End Patch"
	ops, err := parsePatch(src)
	require.NoError(t, err)
	assert.Equal(t, "a\n\nb", ops[0].updOld, "blank line preserved as context")
	assert.Equal(t, "a\n\nc", ops[0].updNew)
}

func TestParsePatch_Errors(t *testing.T) {
	tests := []struct {
		name string
		patch string
	}{
		{"missing envelope", "*** Add File: x\nhi\n*** End Patch"},
		{"missing end", "*** Begin Patch\n*** Add File: x\nhi\n"},
		{"empty add path", "*** Begin Patch\n*** Add File: \nhi\n*** End Patch"},
		{"move missing To", "*** Begin Patch\n*** Move File: a\n*** End Patch"},
		{"move empty dest", "*** Begin Patch\n*** Move File: a\n*** To: \n*** End Patch"},
		{"bad update line", "*** Begin Patch\n*** Update File: f\nbadline\n*** End Patch"},
		{"empty update body", "*** Begin Patch\n*** Update File: f\n*** End Patch"},
		{"stray line", "*** Begin Patch\nhello\n*** End Patch"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parsePatch(tc.patch)
			assert.Error(t, err)
		})
	}
}

func TestParsePatch_EmptyHasNoOpsButIsValid(t *testing.T) {
	ops, err := parsePatch("*** Begin Patch\n*** End Patch")
	require.NoError(t, err)
	assert.Empty(t, ops)
}
```

- [ ] **Step 2: 运行测试确认失败**

Run:
```sh
go test ./internal/tools -run TestParsePatch -v
```
Expected: FAIL（`parsePatch` / op 常量未定义）。

- [ ] **Step 3: 实现 `fs_patch_parse.go`**

`internal/tools/fs_patch_parse.go`:
```go
package tools

import (
	"fmt"
	"strings"
)

// patchOp kinds.
const (
	opAdd = iota
	opUpdate
	opDelete
	opMove
)

// patchOp is one parsed operation from a patch. path is the target for
// add/update/delete and the DESTINATION for move; from is the move source.
type patchOp struct {
	kind    int
	path    string
	from    string // move source
	addBody string // add: raw file content (verbatim)
	updOld  string // update: search text (context + removed lines joined by \n)
	updNew  string // update: replacement text (context + added lines joined by \n)
}

// parsePatch parses patch text into an ordered op list. Format (codex-style
// envelope; bodies are self-defined):
//
//	*** Begin Patch
//	*** Add File: <path>
//	<raw content lines until the next "*** " header>
//	*** Update File: <path>
//	 <context line>      (a single leading space)
//	-<removed line>
//	+<added line>
//	*** Delete File: <path>
//	*** Move File: <from>
//	*** To: <to>
//	*** End Patch
//
// Add bodies are taken VERBATIM (no per-line prefix) so the model can paste file
// content directly. Update bodies are unified-style: a leading space is context
// (kept on both sides), "-" is a removal (old side only), "+" is an addition
// (new side only); a completely blank line is treated as a context line with
// empty text so blank lines in the matched region are preserved. The whole block
// becomes ONE search/replace pair (old = context+removed joined by "\n", new =
// context+added joined by "\n"); for multiple disjoint edits in one file use
// multiple Update blocks (each is applied in order). A line within a block that
// starts with "*** " terminates the block (so file content starting with "*** "
// cannot appear inside an Add/Update body — a documented v1 limitation).
func parsePatch(text string) ([]patchOp, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // drop trailing empty from final newline
	}
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "*** Begin Patch" {
		return nil, fmt.Errorf("patch must start with %q", "*** Begin Patch")
	}
	var ops []patchOp
	i := 1
	for i < len(lines) {
		line := lines[i]
		switch {
		case strings.HasPrefix(line, "*** End Patch"):
			return ops, nil
		case strings.HasPrefix(line, "*** Add File: "):
			path := strings.TrimSpace(strings.TrimPrefix(line, "*** Add File: "))
			if path == "" {
				return nil, fmt.Errorf("Add File: empty path at line %d", i+1)
			}
			body, next := takeRawBody(lines, i+1)
			ops = append(ops, patchOp{kind: opAdd, path: path, addBody: body})
			i = next
		case strings.HasPrefix(line, "*** Update File: "):
			path := strings.TrimSpace(strings.TrimPrefix(line, "*** Update File: "))
			if path == "" {
				return nil, fmt.Errorf("Update File: empty path at line %d", i+1)
			}
			body, next := takeRawBody(lines, i+1)
			old, neu, err := parseUpdateBody(body, i+1)
			if err != nil {
				return nil, err
			}
			ops = append(ops, patchOp{kind: opUpdate, path: path, updOld: old, updNew: neu})
			i = next
		case strings.HasPrefix(line, "*** Delete File: "):
			path := strings.TrimSpace(strings.TrimPrefix(line, "*** Delete File: "))
			if path == "" {
				return nil, fmt.Errorf("Delete File: empty path at line %d", i+1)
			}
			ops = append(ops, patchOp{kind: opDelete, path: path})
			i++
		case strings.HasPrefix(line, "*** Move File: "):
			from := strings.TrimSpace(strings.TrimPrefix(line, "*** Move File: "))
			if from == "" {
				return nil, fmt.Errorf("Move File: empty source at line %d", i+1)
			}
			if i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "*** To: ") {
				return nil, fmt.Errorf("Move File: missing %q at line %d", "*** To:", i+2)
			}
			to := strings.TrimSpace(strings.TrimPrefix(lines[i+1], "*** To: "))
			if to == "" {
				return nil, fmt.Errorf("Move File: empty destination at line %d", i+2)
			}
			ops = append(ops, patchOp{kind: opMove, path: to, from: from})
			i += 2
		default:
			return nil, fmt.Errorf("unexpected line %d: %q (expected a *** header)", i+1, line)
		}
	}
	return nil, fmt.Errorf("patch missing %q", "*** End Patch")
}

// takeRawBody collects lines until the next "*** " header (or end of input),
// returning the joined body and the index of the terminating header line (to
// resume parsing from). Used for both Add bodies (verbatim) and Update bodies
// (prefix-encoded).
func takeRawBody(lines []string, start int) (string, int) {
	var body []string
	i := start
	for i < len(lines) && !strings.HasPrefix(lines[i], "*** ") {
		body = append(body, lines[i])
		i++
	}
	return strings.Join(body, "\n"), i
}

// parseUpdateBody decodes a unified-style update body into one (old, new)
// search/replace pair. startLine is the 1-based line of the body's first line
// (for error messages).
func parseUpdateBody(body string, startLine int) (old, neu string, err error) {
	if body == "" {
		return "", "", fmt.Errorf("Update File: empty body at line %d", startLine)
	}
	var oldParts, newParts []string
	for idx, raw := range strings.Split(body, "\n") {
		switch {
		case raw == "":
			oldParts = append(oldParts, "")
			newParts = append(newParts, "")
		case strings.HasPrefix(raw, " "):
			oldParts = append(oldParts, raw[1:])
			newParts = append(newParts, raw[1:])
		case strings.HasPrefix(raw, "-"):
			oldParts = append(oldParts, strings.TrimPrefix(raw, "-"))
		case strings.HasPrefix(raw, "+"):
			newParts = append(newParts, strings.TrimPrefix(raw, "+"))
		default:
			return "", "", fmt.Errorf("Update File: line %d has no context/-/+ prefix: %q", startLine+idx, raw)
		}
	}
	old = strings.Join(oldParts, "\n")
	neu = strings.Join(newParts, "\n")
	if old == "" {
		return "", "", fmt.Errorf("Update File: body has no context or removed lines at line %d", startLine)
	}
	return old, neu, nil
}
```

- [ ] **Step 4: 运行测试确认通过**

Run:
```sh
go test ./internal/tools -run TestParsePatch -v
```
Expected: PASS（全部 5 个测试函数）。

- [ ] **Step 5: 提交**

```sh
git add internal/tools/fs_patch_parse.go internal/tools/fs_patch_parse_test.go
git commit -m "feat(tools): add apply_patch text parser (T06)"
```

---

## Task 3: 行级 unified diff（dry-run 渲染用）

**Files:**
- Create: `internal/tools/fs_diff.go`
- Create: `internal/tools/fs_diff_test.go`

> dry-run 要返回 unified-style diff。update 的 old/new 已在 Task 2 拆成行对，但要把"原文件 → 新文件"渲染成 `+`/`-` 行需要一条最小行 diff。这里实现一个基于 LCS DP 的行 diff（O(n·m) 时空，dry-run 场景文件小，可接受），不引入新依赖。`splitDiffLines` 同时被 Task 4 的 `prefixLines` 复用。

- [ ] **Step 1: 写失败测试**

`internal/tools/fs_diff_test.go`:
```go
package tools

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnifiedDiff_Modify(t *testing.T) {
	a := "line1\nold\nline3\n"
	b := "line1\nnew\nline3\n"
	got := unifiedDiff(a, b)
	// context lines omitted; only the change shows as -old / +new.
	assert.Contains(t, got, "-old\n")
	assert.Contains(t, got, "+new\n")
	assert.NotContains(t, got, "line1", "unchanged lines are omitted")
	assert.NotContains(t, got, "line3")
}

func TestUnifiedDiff_AllAdded(t *testing.T) {
	got := unifiedDiff("", "a\nb\n")
	require.True(t, strings.HasPrefix(got, "+a\n"), "got %q", got)
	assert.Contains(t, got, "+b\n")
}

func TestUnifiedDiff_AllRemoved(t *testing.T) {
	got := unifiedDiff("a\nb\n", "")
	assert.Contains(t, got, "-a\n")
	assert.Contains(t, got, "-b\n")
	assert.NotContains(t, got, "+")
}

func TestUnifiedDiff_NoChange(t *testing.T) {
	assert.Equal(t, "", unifiedDiff("same\nsame\n", "same\nsame\n"))
}

func TestSplitDiffLines(t *testing.T) {
	assert.Nil(t, splitDiffLines(""))
	assert.Equal(t, []string{"a", "b"}, splitDiffLines("a\nb\n"))
	assert.Equal(t, []string{"a", "b"}, splitDiffLines("a\nb")) // no trailing newline
}
```

- [ ] **Step 2: 运行测试确认失败**

Run:
```sh
go test ./internal/tools -run "TestUnifiedDiff|TestSplitDiffLines" -v
```
Expected: FAIL（`unifiedDiff` / `splitDiffLines` 未定义）。

- [ ] **Step 3: 实现 `fs_diff.go`**

`internal/tools/fs_diff.go`:
```go
package tools

import "strings"

// unifiedDiff returns a unified-style line diff between a (original) and b
// (modified) as a sequence of "-" / "+" lines. Unchanged lines are OMITTED to
// keep dry-run output compact (callers render per-file ---/+++ headers). It uses
// a classic LCS dynamic program (O(n·m) time and space) — acceptable for dry-run
// rendering where the files touched by a patch are small. Line endings are
// normalized (\r\n and lone \r fold to \n) before comparing.
func unifiedDiff(a, b string) string {
	la := splitDiffLines(a)
	lb := splitDiffLines(b)
	n, m := len(la), len(lb)
	// dp[i][j] = length of the longest common subsequence of la[i:] and lb[j:].
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if la[i] == lb[j] {
				dp[i][j] = dp[i+1][j+1] + 1
				continue
			}
			dp[i][j] = dp[i+1][j]
			if dp[i][j+1] > dp[i][j] {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var out strings.Builder
	i, j := 0, 0
	for i < n && j < m {
		if la[i] == lb[j] {
			i++
			j++
			continue
		}
		// Prefer removing from a when the LCS via (i+1,j) is at least as long;
		// otherwise add from b. Either way unchanged lines are skipped.
		if dp[i+1][j] >= dp[i][j+1] {
			out.WriteString("-" + la[i] + "\n")
			i++
		} else {
			out.WriteString("+" + lb[j] + "\n")
			j++
		}
	}
	for i < n {
		out.WriteString("-" + la[i] + "\n")
		i++
	}
	for j < m {
		out.WriteString("+" + lb[j] + "\n")
		j++
	}
	return out.String()
}

// splitDiffLines splits s into lines for diffing, folding \r\n and lone \r to \n
// first and dropping a trailing empty element produced by a final newline. An
// empty input returns nil (zero lines).
func splitDiffLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
```

- [ ] **Step 4: 运行测试确认通过**

Run:
```sh
go test ./internal/tools -run "TestUnifiedDiff|TestSplitDiffLines" -v
```
Expected: PASS。

- [ ] **Step 5: 提交**

```sh
git add internal/tools/fs_diff.go internal/tools/fs_diff_test.go
git commit -m "feat(tools): add LCS line diff helper for dry-run rendering (T06)"
```

---

## Task 4: apply_patch applier + 工具注册（原子应用、dry-run、VCS 追踪）

**Files:**
- Create: `internal/tools/fs_patch.go`
- Create: `internal/tools/fs_patch_test.go`
- Modify: `internal/tools/fs.go`（3 处：结构体字段、`NewFSTools` 构造、`Tools()` 切片）

> 核心任务。prepare 阶段在内存文件树上按序应用所有 op 并校验，产出 `[]stagedChange`（每项含原始字节快照与最终字节）；dry-run 只跑 prepare 再渲染 diff；commit 阶段按序落盘，任一 I/O 失败用快照回滚已落盘项。成功后对 add/update/move-dst 调 `trackEdit`、对 delete/move-src 调（本任务新增的）`trackDelete`。权限收进一次 `checkFS`（整批）。`bootstrap.go` 无需改动——它遍历 `fsTools.Tools()`，把 `f.Patch` 一并注册。

- [ ] **Step 1: 写失败测试**

`internal/tools/fs_patch_test.go`:
```go
package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/autocode/internal/guard"
	"github.com/x6nux/autocode/internal/store"
	"github.com/x6nux/autocode/internal/vcs"
)

// patchCtx builds a context that allows fs_* + apply_patch full read/write under
// dir. Note apply_patch is the tool NAME, so a profile granting only "fs_*" would
// NOT match it — it must be listed explicitly (or "*").
func patchCtx(dir string) context.Context {
	return WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*", "apply_patch"}},
		FS:    guard.FSPerm{Write: []string{dir + "/**"}, Read: []string{dir + "/**"}},
	})
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
}

func readBack(t *testing.T, fs *FSTools, ctx context.Context, name string) string {
	t.Helper()
	out, err := runTool(ctx, fs.Read, toJSON(map[string]any{"path": name}))
	require.NoError(t, err)
	return out
}

func TestApplyPatch_AddUpdateDeleteMove(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	ctx := patchCtx(dir)
	writeFile(t, dir, "upd.go", "line1\nfunc A() {}\nline3\n")
	writeFile(t, dir, "del.go", "bye\n")
	writeFile(t, dir, "mvsrc.go", "moving\n")

	patch := "*** Begin Patch\n" +
		"*** Add File: new.go\n" +
		"package new\n" +
		"*** Update File: upd.go\n" +
		" line1\n" +
		"-func A() {}\n" +
		"+func A() { return }\n" +
		" line3\n" +
		"*** Delete File: del.go\n" +
		"*** Move File: mvsrc.go\n" +
		"*** To: mvdst.go\n" +
		"*** End Patch"

	out, err := runTool(ctx, fs.Patch, toJSON(map[string]any{"patch": patch}))
	require.NoError(t, err)
	assert.Contains(t, out, "applied")

	// add
	assert.Contains(t, readBack(t, fs, ctx, "new.go"), "package new")
	// update
	assert.Contains(t, readBack(t, fs, ctx, "upd.go"), "func A() { return }")
	assert.NotContains(t, readBack(t, fs, ctx, "upd.go"), "func A() {}")
	// delete
	_, derr := os.Stat(filepath.Join(dir, "del.go"))
	assert.True(t, os.IsNotExist(derr), "del.go must be removed")
	// move: src gone, dst has content
	_, serr := os.Stat(filepath.Join(dir, "mvsrc.go"))
	assert.True(t, os.IsNotExist(serr), "mvsrc.go must be removed")
	assert.Contains(t, readBack(t, fs, ctx, "mvdst.go"), "moving")
}

func TestApplyPatch_DryRunNoWrite(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	ctx := patchCtx(dir)

	patch := "*** Begin Patch\n*** Add File: ghost.txt\nboo\n*** End Patch"
	out, err := runTool(ctx, fs.Patch, toJSON(map[string]any{"patch": patch, "dry_run": true}))
	require.NoError(t, err)
	assert.Contains(t, out, "dry_run")
	assert.Contains(t, out, "+++ ghost.txt", "dry-run must return a unified-style diff")
	// The file must NOT exist on disk (dry-run never writes).
	_, ferr := os.Stat(filepath.Join(dir, "ghost.txt"))
	assert.True(t, os.IsNotExist(ferr), "dry-run must not create files")
}

func TestApplyPatch_AtomicRollbackOnBadOp(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	ctx := patchCtx(dir)
	writeFile(t, dir, "a.go", "original\n")
	// Good update on a.go, then a delete of a file that does not exist. The
	// missing-file op must fail prepare and NOTHING is written — a.go is unchanged.
	patch := "*** Begin Patch\n" +
		"*** Update File: a.go\n" +
		" original\n" +
		"-original\n" +
		"+changed\n" +
		"*** Delete File: missing.go\n" +
		"*** End Patch"
	out, err := runTool(ctx, fs.Patch, toJSON(map[string]any{"patch": patch}))
	require.NoError(t, err, "validation failure surfaces as a result, not a Go error")
	assert.Contains(t, out, "error")
	assert.Contains(t, out, "does not exist")

	data, rerr := os.ReadFile(filepath.Join(dir, "a.go"))
	require.NoError(t, rerr)
	assert.Equal(t, "original\n", string(data), "a.go must be unchanged (atomic: no half-applied patch)")
}

func TestApplyPatch_PermissionDenied(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	// Profile grants write only under dir/allowed/**; targeting dir/secret.go
	// must be denied at the batch guard check — nothing is written.
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*", "apply_patch"}},
		FS:    guard.FSPerm{Write: []string{dir + "/allowed/**"}, Read: []string{dir + "/**"}},
	})
	patch := "*** Begin Patch\n*** Add File: secret.go\ns3cr3t\n*** End Patch"
	out, err := runTool(ctx, fs.Patch, toJSON(map[string]any{"patch": patch}))
	require.NoError(t, err)
	assert.Contains(t, out, "permission denied")
	_, ferr := os.Stat(filepath.Join(dir, "secret.go"))
	assert.True(t, os.IsNotExist(ferr), "denied patch must not write")
}

// TestApplyPatch_TracksToVCS proves successful edits flow into autoVCS: add /
// update / move-destination are tracked as content (blob present), and delete /
// move-source are tracked via the new trackDelete path (op="deleted"). The op
// value itself is asserted in the vcs-package test (Task 1); here we assert the
// rows exist in the changeset (Uncommitted returns the path keys, including
// deleted ones whose blob_hash is empty).
func TestApplyPatch_TracksToVCS(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "upd.go", "line1\nfunc A() {}\nline3\n")
	writeFile(t, root, "del.go", "bye\n")
	writeFile(t, root, "mvsrc.go", "moving\n")

	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	v := vcs.New(st, t.TempDir())
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	fs := NewFSTools(root)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*", "apply_patch"}},
		FS:    guard.FSPerm{Write: []string{root + "/**"}, Read: []string{root + "/**"}},
	})
	ctx = WithVCS(ctx, VCSScope{VCS: v, RepoID: repoID, Agent: "orchestrator"})

	patch := "*** Begin Patch\n" +
		"*** Add File: new.go\n" +
		"package new\n" +
		"*** Update File: upd.go\n" +
		" line1\n" +
		"-func A() {}\n" +
		"+func A() { return }\n" +
		" line3\n" +
		"*** Delete File: del.go\n" +
		"*** Move File: mvsrc.go\n" +
		"*** To: mvdst.go\n" +
		"*** End Patch"
	out, err := runTool(ctx, fs.Patch, toJSON(map[string]any{"patch": patch}))
	require.NoError(t, err)
	assert.Contains(t, out, "applied")

	pending := v.Uncommitted("main", repoID)
	// content-tracked (add / update / move destination)
	for _, p := range []string{"new.go", "upd.go", "mvdst.go"} {
		assert.Contains(t, pending, p, "%s must be tracked", p)
	}
	// delete-tracked (Uncommitted returns deleted paths with empty blob_hash;
	// their key presence proves trackDelete fired).
	for _, p := range []string{"del.go", "mvsrc.go"} {
		assert.Contains(t, pending, p, "%s must be tracked as deleted", p)
	}
}

func TestApplyPatch_EmptyPatchIsError(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	ctx := patchCtx(dir)
	out, err := runTool(ctx, fs.Patch, toJSON(map[string]any{"patch": "*** Begin Patch\n*** End Patch"}))
	require.NoError(t, err)
	assert.Contains(t, out, "error")
}
```

- [ ] **Step 2: 运行测试确认失败**

Run:
```sh
go test ./internal/tools -run TestApplyPatch -v
```
Expected: FAIL（`fs.Patch` 未定义——还没在 `NewFSTools` 注册；`runPatch`/`preparePatch`/… 未定义）。

- [ ] **Step 3a: 修改 `internal/tools/fs.go`（3 处接线）**

(a) 在 `FSTools` 结构体（约 17–25 行，`Search *GuardedTool` 字段之后）加字段：
```go
	// Patch is the multi-file apply_patch tool (add/update/delete/move),
	// atomic + dry-run, auto-tracked via the same VCS scope as fs_write/fs_edit.
	Patch *GuardedTool
```

(b) 在 `NewFSTools`（`f.Edit` 构造块之后、`f.List` 之前，约 59 行后）插入：
```go
	f.Patch = NewGuardedTool(
		"apply_patch",
		"Apply a multi-file patch (add/update/delete/move) atomically. "+
			"Set dry_run=true to preview a unified diff without writing.",
		params(map[string]*schema.ParameterInfo{
			"patch":   {Type: schema.String, Desc: "patch text: *** Begin Patch ... *** End Patch", Required: true},
			"dry_run": {Type: schema.Boolean, Desc: "if true, return a unified diff without writing"},
		}),
		f.runPatch,
	)
```

(c) 在 `Tools()`（约 92 行）的字面量里加 `f.Patch`：
```go
	for _, t := range []*GuardedTool{f.Read, f.Write, f.Edit, f.List, f.Glob, f.Search, f.Patch} {
```

- [ ] **Step 3b: 实现 `internal/tools/fs_patch.go`**

`internal/tools/fs_patch.go`:
```go
package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// stagedChange is one file's net change computed by preparePatch: the original
// on-disk state (for rollback + diff) and the final state (nil final ⇒ delete).
// rel is the raw model-supplied path used for diff display; abs is the resolved
// jail-anchored path used for I/O.
type stagedChange struct {
	rel         string
	abs         string
	origExisted bool
	orig        []byte
	final       []byte // nil ⇒ the file should be deleted
}

// runPatch is the apply_patch tool body. It parses, authorizes the WHOLE batch
// in one guard action (patch-level interactive approval, matching atomic
// semantics), validates everything in memory (preparePatch), then either renders
// a diff (dry_run) or commits atomically and records edits into autoVCS.
func (f *FSTools) runPatch(ctx context.Context, argsJSON string) (string, error) {
	var a struct {
		Patch  string `json:"patch"`
		DryRun bool   `json:"dry_run"`
	}
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	if a.Patch == "" {
		return "", fmt.Errorf("apply_patch: patch must be non-empty")
	}
	ops, err := parsePatch(a.Patch)
	if err != nil {
		return "", fmt.Errorf("apply_patch: %w", err)
	}
	if len(ops) == 0 {
		return "", fmt.Errorf("apply_patch: patch has no operations")
	}

	// Patch-level authorization: every affected path goes into ONE guard action
	// so an interactive prompt (when bound) is a single approval for the whole
	// patch — coherent with all-or-nothing atomic semantics. A jail violation or
	// profile deny on ANY path aborts the batch before any file is touched.
	if _, err := f.checkFS(ctx, "write", "apply_patch", argsJSON, opWritePaths(ops)...); err != nil {
		return "", err
	}

	staged, err := f.preparePatch(ops)
	if err != nil {
		return "", fmt.Errorf("apply_patch: %w", err)
	}
	if len(staged) == 0 {
		return toJSON(map[string]any{"applied": 0, "note": "patch resulted in no net changes"}), nil
	}

	if a.DryRun {
		return toJSON(map[string]any{
			"dry_run": true,
			"files":   len(staged),
			"diff":    renderPatchDiff(staged),
		}), nil
	}

	if err := commitPatch(staged); err != nil {
		return "", fmt.Errorf("apply_patch: %w", err)
	}

	// Best-effort autoVCS tracking (mirrors the fs_write/fs_edit hook). Add /
	// update / move-destination carry final content → trackEdit. Delete /
	// move-source carry no content → trackDelete (op="deleted"). Errors are
	// swallowed: the disk write already succeeded and tracking must not fail it.
	for _, c := range staged {
		if c.final != nil {
			trackEdit(ctx, c.abs, c.final)
		} else {
			trackDelete(ctx, c.abs)
		}
	}

	return toJSON(map[string]any{
		"applied": len(staged),
		"files":   stagedRelPaths(staged),
	}), nil
}

// opWritePaths returns every path a patch touches under a "write" intent (the
// move source is included because deleting it is a write to that location).
func opWritePaths(ops []patchOp) []string {
	var ps []string
	for _, o := range ops {
		if o.kind == opMove {
			ps = append(ps, o.from, o.path)
			continue
		}
		ps = append(ps, o.path)
	}
	return ps
}

// preparePatch validates every op and computes the net per-file changes WITHOUT
// touching disk. It maintains an in-memory view of touched files (seeded lazily
// from disk), applies ops in order, and diffs the final in-memory state against
// each file's original on-disk state. Any validation failure (add exists, update
// context not found, delete/move source missing, move destination exists) aborts
// the whole batch — this is the prepare half of atomicity (no file is written
// until every op validates).
func (f *FSTools) preparePatch(ops []patchOp) ([]stagedChange, error) {
	type entry struct {
		rel         string // first raw path that referenced this abs (display)
		origExisted bool
		orig        []byte // snapshot from disk at first touch
		existed     bool   // live existence (an add/move sets this true)
		content     []byte // live content
		deleted     bool   // live deleted flag
	}
	mem := map[string]*entry{}
	var order []string // abs paths in first-touch order (stable output)
	ensure := func(abs, rel string) *entry {
		if e, ok := mem[abs]; ok {
			return e
		}
		e := &entry{rel: rel}
		if data, err := os.ReadFile(abs); err == nil {
			e.origExisted = true
			e.orig = data
			e.existed = true
			e.content = data
		}
		mem[abs] = e
		order = append(order, abs)
		return e
	}
	resolve := f.abs

	for i, op := range ops {
		switch op.kind {
		case opAdd:
			abs, err := resolve(op.path)
			if err != nil {
				return nil, fmt.Errorf("op %d (add): %w", i+1, err)
			}
			e := ensure(abs, op.path)
			if e.existed && !e.deleted {
				return nil, fmt.Errorf("op %d (add): %s already exists", i+1, op.path)
			}
			e.content = []byte(op.addBody)
			e.existed, e.deleted = true, false
		case opUpdate:
			abs, err := resolve(op.path)
			if err != nil {
				return nil, fmt.Errorf("op %d (update): %w", i+1, err)
			}
			e := ensure(abs, op.path)
			if !e.existed || e.deleted {
				return nil, fmt.Errorf("op %d (update): %s does not exist", i+1, op.path)
			}
			next, err := applyOneEdit(string(e.content), op.updOld, op.updNew)
			if err != nil {
				return nil, fmt.Errorf("op %d (update %s): %w", i+1, op.path, err)
			}
			e.content = []byte(next)
		case opDelete:
			abs, err := resolve(op.path)
			if err != nil {
				return nil, fmt.Errorf("op %d (delete): %w", i+1, err)
			}
			e := ensure(abs, op.path)
			if !e.existed || e.deleted {
				return nil, fmt.Errorf("op %d (delete): %s does not exist", i+1, op.path)
			}
			e.deleted = true
		case opMove:
			src, err := resolve(op.from)
			if err != nil {
				return nil, fmt.Errorf("op %d (move): %w", i+1, err)
			}
			dst, err := resolve(op.path)
			if err != nil {
				return nil, fmt.Errorf("op %d (move): %w", i+1, err)
			}
			es := ensure(src, op.from)
			if !es.existed || es.deleted {
				return nil, fmt.Errorf("op %d (move): %s does not exist", i+1, op.from)
			}
			ed := ensure(dst, op.path)
			if ed.existed && !ed.deleted {
				return nil, fmt.Errorf("op %d (move): %s already exists", i+1, op.path)
			}
			ed.content = es.content
			ed.existed, ed.deleted = true, false
			es.deleted = true
		default:
			return nil, fmt.Errorf("op %d: unknown kind", i+1)
		}
	}

	// Build staged changes: compare each touched file's live state to its
	// original on-disk state; skip net no-ops (created-then-deleted, or unchanged).
	var staged []stagedChange
	for _, abs := range order {
		e := mem[abs]
		if e.deleted && !e.origExisted {
			continue // created then deleted within the same patch
		}
		if !e.deleted && e.origExisted && bytes.Equal(e.orig, e.content) {
			continue // no change
		}
		var final []byte
		if !e.deleted {
			final = e.content
		}
		staged = append(staged, stagedChange{
			rel:         e.rel,
			abs:         abs,
			origExisted: e.origExisted,
			orig:        e.orig,
			final:       final,
		})
	}
	return staged, nil
}

// applyOneEdit performs one search/replace of old→new in src, mirroring fs_edit's
// two-tier matching: an exact byte substring first (rejecting multiple sites),
// then whitespace-lenient line matching via lenientFind. A miss surfaces the
// actual file content (previewLines) so the model can self-correct.
func applyOneEdit(src, old, new string) (string, error) {
	if old == "" {
		return "", fmt.Errorf("empty context/remove block")
	}
	if c := strings.Count(src, old); c >= 1 {
		if c > 1 {
			return "", fmt.Errorf("context matches %d sites; add surrounding context to make it unique", c)
		}
		return strings.Replace(src, old, new, 1), nil
	}
	ranges := lenientFind([]byte(src), old)
	if len(ranges) == 0 {
		return "", fmt.Errorf("context not found:\n%s", previewLines([]byte(src), 15))
	}
	if len(ranges) > 1 {
		return "", fmt.Errorf("context matches %d sites (after whitespace normalization); add surrounding context to make it unique", len(ranges))
	}
	r := ranges[0]
	return src[:r.Start] + new + src[r.End:], nil
}

// commitPatch applies staged changes to disk in order. On any I/O error it rolls
// back already-applied changes (in reverse) using their original snapshots, then
// returns the error — the commit half of atomicity. A newly-created file is
// removed on rollback; a pre-existing file is restored to its original bytes.
func commitPatch(staged []stagedChange) error {
	for i, c := range staged {
		if err := applyStaged(c); err != nil {
			for j := i - 1; j >= 0; j-- {
				rollbackStaged(staged[j])
			}
			return fmt.Errorf("apply failed at %s: %w (rolled back)", c.rel, err)
		}
	}
	return nil
}

// applyStaged writes one staged change to disk: Remove for a delete, else
// MkdirAll(dir) + WriteFile(final).
func applyStaged(c stagedChange) error {
	if c.final == nil {
		if err := os.Remove(c.abs); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(c.abs, c.final, 0o644)
}

// rollbackStaged restores one applied change to its pre-patch state. Best-effort
// (errors dropped): rollback itself failing is unrecoverable, but the caller's
// error already names the failing file so the user can inspect.
func rollbackStaged(c stagedChange) {
	if !c.origExisted {
		_ = os.Remove(c.abs) // was newly created → remove to restore "did not exist"
		return
	}
	_ = os.MkdirAll(filepath.Dir(c.abs), 0o755)
	_ = os.WriteFile(c.abs, c.orig, 0o644)
}

// trackDelete records a deletion to the active VCS scope (best-effort; errors
// swallowed). Symmetric with trackEdit: delete/move-source carry no content, so
// they route to RecordDelete{Main,Worktree} (op="deleted") instead of
// RecordEdit. A scope with nil VCS is a no-op (tracking not configured).
func trackDelete(ctx context.Context, absPath string) {
	sc, ok := VCSScopeFromContext(ctx)
	if !ok || sc.VCS == nil {
		return
	}
	var err error
	switch {
	case sc.WorktreeID != "":
		err = sc.VCS.RecordDeleteWorktree(sc.WorktreeID, sc.Agent, absPath)
	case sc.RepoID != "":
		err = sc.VCS.RecordDeleteMain(sc.RepoID, sc.Agent, absPath)
	}
	_ = err // best-effort: a tracking failure must not fail the (already-written) edit
}

// stagedRelPaths returns the display paths of a staged set, in order.
func stagedRelPaths(staged []stagedChange) []string {
	paths := make([]string, len(staged))
	for i, c := range staged {
		paths[i] = c.rel
	}
	return paths
}

// renderPatchDiff renders staged changes as a compact unified-style diff: add ⇒
// /dev/null → new file; delete ⇒ file → /dev/null; update/move ⇒ unifiedDiff of
// old vs new bytes. Hunk headers carry global line counts (a single @@ per file)
// which is sufficient for a dry-run preview.
func renderPatchDiff(staged []stagedChange) string {
	var b strings.Builder
	for _, c := range staged {
		switch {
		case !c.origExisted && c.final != nil:
			fmt.Fprintf(&b, "--- /dev/null\n+++ %s\n@@ -0,0 +1,%d @@\n", c.rel, countLines(c.final))
			b.WriteString(prefixLines("+", c.final))
		case c.final == nil:
			fmt.Fprintf(&b, "--- %s\n+++ /dev/null\n@@ -1,%d +0,0 @@\n", c.rel, countLines(c.orig))
			b.WriteString(prefixLines("-", c.orig))
		default:
			fmt.Fprintf(&b, "--- %s\n+++ %s\n@@ -1,%d +1,%d @@\n", c.rel, c.rel, countLines(c.orig), countLines(c.final))
			b.WriteString(unifiedDiff(string(c.orig), string(c.final)))
		}
	}
	return b.String()
}

// countLines is the number of lines in b (a trailing newline is not an extra line).
func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, []byte("\n"))
	if !bytes.HasSuffix(b, []byte("\n")) {
		n++ // final line without a trailing newline
	}
	return n
}

// prefixLines prefixes every line of b with prefix and rejoins with a trailing
// newline (each line becomes one +/- entry in the diff body).
func prefixLines(prefix string, b []byte) string {
	lines := splitDiffLines(string(b))
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n") + "\n"
}
```

- [ ] **Step 4: 运行测试确认通过**

Run:
```sh
go test ./internal/tools -run TestApplyPatch -v
```
Expected: PASS（全部 6 个 TestApplyPatch_* 测试）。

再跑 tools 包全量，确认没碰坏既有 fs 工具：
```sh
go test ./internal/tools -v
```
Expected: 全 PASS（含既有 fs_test / fs_edit_test / guard_test 等）。

- [ ] **Step 5: 提交**

```sh
git add internal/tools/fs_patch.go internal/tools/fs_patch_test.go internal/tools/fs.go
git commit -m "feat(tools): add atomic multi-file apply_patch tool (T06)"
```

---

## Task 5: 全量回归 + 构建验证

**Files:** 无新增；跑全量测试 + vet + build。

- [ ] **Step 1: 全量测试**

Run:
```sh
go test ./...
```
Expected: 全 PASS。允许 CLAUDE.md 记载的预期 `t.Skip`：`e2e_real`（无 `ACODE_E2E=1` 或 PATH 上无 `codex`/`claudecode`）、部分 `internal/llm/eino` 与 `internal/bootstrap` 在 openai provider 不可用时 skip。

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
Expected: 成功（apply_patch 经 `bootstrap.go` 的 `fsTools.Tools()` 自动注册，无需改 bootstrap）。

- [ ] **Step 4: 文件行数自检（CLAUDE.md 约定）**

Run:
```sh
wc -l internal/tools/fs.go internal/tools/fs_patch.go internal/tools/fs_patch_parse.go internal/tools/fs_diff.go internal/vcs/delete.go
```
Expected: `fs.go` 约 605 行（+12：字段/doc、构造块、Tools 切片）；各新文件均在 1000 行纯代码以下；`vcs.go` 未被改动（仍 1026 行——这是既有状态，本 plan 不恶化它，删除逻辑独立在 `delete.go`）。

- [ ] **Step 5: 提交（若有零散小修）**

```sh
git add -A
git commit -m "test: T06 apply_patch regression green" || echo "nothing to commit"
```

---

## Self-Review（写完后自查结果）

1. **Spec 覆盖**：roadmap T06 / 任务书验收——「add/update/delete/move 四类」(Task 2 parser + Task 4 prepare 各分支) ✅；「dry-run 返回 diff 不落盘」(Task 3 diff + Task 4 `dry_run` 分支 + `TestApplyPatch_DryRunNoWrite` 断言文件未创建) ✅；「任一失败整批回滚」(prepare 不写盘 + commit 回滚 + `TestApplyPatch_AtomicRollbackOnBadOp`) ✅；「patch 级审批」(Task 4 `runPatch` 单次 `checkFS` 收全部路径 + `TestApplyPatch_PermissionDenied`) ✅；「成功编辑进 autoVCS」(Task 1 `RecordDelete*` + Task 4 `trackEdit`/`trackDelete` + `TestApplyPatch_TracksToVCS` 断言 5 个路径) ✅；「fs 写规则 + guard 校验」(走现有 `checkFS`/`Authorize`，jail + profile 双重) ✅。覆盖完整。

2. **Placeholder 扫描**：每个 step 都有完整可编译代码或精确命令；无 TBD/TODO/"类似 Task N"。Task 4 的 fs.go 三处接线给了精确行号 + 完整代码片段。

3. **类型/命名一致性**：`patchOp{kind,path,from,addBody,updOld,updNew}` 在 Task 2 定义、Task 4 `preparePatch`/`opWritePaths` 消费，字段名一致 ✅。`stagedChange{rel,abs,origExisted,orig,final}` 在 Task 4 内部自洽，`runPatch`→`preparePatch`→`commitPatch`→`applyStaged`/`rollbackStaged` 调用链签名匹配 ✅。`trackDelete`/`trackEdit` 对称，`RecordDeleteMain`/`RecordDeleteWorktree` 与 `RecordEditMain`/`RecordEditWorktree` 对称 ✅。`runPatch` 注册名 `"apply_patch"` 与测试 profile 的 `Allow: ["fs_*","apply_patch"]`、`TestApplyPatch_*` 一致 ✅。

4. **关键设计决策（写明供执行者参考）**：
   - **整批审批**：所有路径进**一个** guard Action，交互模式只问一次（与 all-or-nothing 一致）。
   - **prepare 不写盘**：内存模拟 → 校验失败零副作用；commit 阶段才落盘，I/O 失败用原始快照回滚。
   - **move = 内存复制+标删**：`es.content → ed`，`es.deleted=true`；落盘 = 写 dst + 删 src；VCS = trackEdit(dst)+trackDelete(src)。
   - **update 复用 fs_edit 匹配**：`applyOneEdit` 走 exact-then-`lenientFind`，多匹配拒绝（要求加上下文），未找到带文件预览（`previewLines`）。
   - **删除追踪要扩 VCS**：现有 VCS 无删除入口，Task 1 新增 `RecordDelete{Main,Worktree}`（放 `delete.go` 因 `vcs.go` 已超 1000 行）。

5. **已知 v1 限制（非 placeholder）**：Add/Update 块体内出现以 `"*** "` 开头的行会被误判为块结束（文档化的 parser 限制）；dry-run 的 hunk 头用全局行数（单 `@@` 每 file），不做精确 hunk 边界；commit 阶段回滚自身失败属不可恢复（错误已点名文件）；profile 仅给 `"fs_*"` 不会匹配 `"apply_patch"`，受限 profile 需显式加 `apply_patch`/`*`（bootstrap 默认 `*`，开箱可用）。

## 执行交接

Plan complete and saved to `docs/superpowers/plans/2026-07-18-m1-lane2-t06-multi-file-patch.md`. 两种执行方式：

1. **Subagent-Driven（推荐）** — 每个任务派一个新 subagent，任务间 review。Task 1→2→3 可并行（互相无依赖），Task 4 依赖 1/2/3，Task 5 最后。
2. **Inline Execution** — 本会话内按 executing-plans 批次执行 + checkpoint。
