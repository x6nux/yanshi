package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/x6nux/yanshi/internal/difflib"
	"github.com/x6nux/yanshi/internal/lsp"
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

	staged, err := f.preparePatch(ctx, ops)
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

	// 评审 #11:诊断 seam 在 runPatch 函数体,commitPatch 成功之后、return 之前。
	// 对每个写盘项(final != nil,即 add/update/move-destination;delete 跳过)查诊断,
	// 汇总进 "diagnostics" 字段。共享总预算 2s,避免 N 文件 × timeout 串行阻塞(评审 #12)。
	result := map[string]any{
		"applied": len(staged),
		"files":   stagedRelPaths(staged),
	}
	if d := diagForStaged(ctx, staged); d != "" {
		result["diagnostics"] = d
	}
	return toJSON(result), nil
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
func (f *FSTools) preparePatch(ctx context.Context, ops []patchOp) ([]stagedChange, error) {
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
	resolve := func(p string) (string, error) { return f.abs(ctx, p) }

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
	lines := difflib.SplitLines(string(b))
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n") + "\n"
}

// patchDiagBudget 是 patch 多文件诊断查询的共享总预算(评审 #12)。每文件均分,
// 串行调用(诊断查询是等 channel,无 CPU 开销,errgroup 无收益且引复杂度)。
const patchDiagBudget = LSPDiagBudget

// diagForStaged 对 staged 中所有写盘项(final != nil)各查一次诊断,拼接渲染。
// 无 Manager / 全无诊断 → 空串(调用方据此省略 JSON 字段)。每文件的超时取自
// (剩余预算 / 待查文件数),保证总和 ≤ patchDiagBudget。
func diagForStaged(ctx context.Context, staged []stagedChange) string {
	mgr, ok := LSPFromContext(ctx)
	if !ok || !mgr.Enabled() {
		return ""
	}
	// 只查有内容(写盘)且语言可识别的项。
	type pending struct {
		abs     string
		content string
	}
	var todo []pending
	for _, c := range staged {
		if c.final == nil {
			continue // delete / move-source:无新内容,跳过
		}
		todo = append(todo, pending{abs: c.abs, content: string(c.final)})
	}
	if len(todo) == 0 {
		return ""
	}

	deadline := time.Now().Add(patchDiagBudget)
	var b strings.Builder
	for i, p := range todo {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break // 预算耗尽,后续文件跳过(空串)
		}
		perFile := remaining / time.Duration(len(todo)-i)
		mgr.DidChange(p.abs, p.content)
		diags := diagnosticsWithin(mgr, p.abs, perFile)
		if text := lsp.FormatDiags(filepath.Base(p.abs), diags); text != "" {
			b.WriteString(text)
			b.WriteString("\n")
		}
	}
	return strings.TrimSpace(b.String())
}
