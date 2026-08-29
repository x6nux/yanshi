package tools

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/instruct"
)

// FSTools exposes filesystem tools, scoped to a work-root.
type FSTools struct {
	root   string
	Read   *GuardedTool
	Write  *GuardedTool
	Edit   *GuardedTool
	List   *GuardedTool
	Glob   *GuardedTool
	Search *GuardedTool
	// AstSearch is the structural (AST) counterpart of Search, backed by the
	// external ast-grep CLI. It is a distinct tool, not a mode of Search: the
	// two query languages are mutually unintelligible and a model that mixes
	// them gets a confident empty result rather than an error. See
	// fs_astsearch.go.
	AstSearch *GuardedTool
	// Patch is the multi-file apply_patch tool (add/update/delete/move),
	// atomic + dry-run, auto-tracked via the same VCS scope as fs_write/fs_edit.
	Patch *GuardedTool
}

// NewFSTools builds filesystem tools rooted at root (relative paths resolve here).
func NewFSTools(root string) *FSTools {
	if root == "" {
		root = "."
	}
	f := &FSTools{root: root}
	f.Read = NewGuardedTool(
		"fs_read", "Read", "Read a file and return its text content.",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"path":   {Type: schema.String, Desc: "file path (relative to work root)", Required: true},
			"offset": {Type: schema.Integer, Desc: "1-based starting line (default 1)"},
			"end":    {Type: schema.Integer, Desc: "1-based ending line, inclusive"},
		}),
		SyncStream(f.runRead),
	).withVerbatimResult()
	f.Write = NewGuardedTool(
		"fs_write", "Write", "Create or overwrite a file with the given content.",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"path":    {Type: schema.String, Desc: "file path", Required: true},
			"content": {Type: schema.String, Desc: "full file content", Required: true},
		}),
		SyncStream(f.runWrite),
	)
	f.Edit = NewGuardedTool(
		"fs_edit", "Edit", "Replace an exact string in a file. old_string must be unique unless replace_all.",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"path":        {Type: schema.String, Desc: "file path", Required: true},
			"old_string":  {Type: schema.String, Desc: "exact text to find", Required: true},
			"new_string":  {Type: schema.String, Desc: "replacement text", Required: true},
			"replace_all": {Type: schema.Boolean, Desc: "replace every occurrence"},
		}),
		SyncStream(f.runEdit),
	)
	f.List = NewGuardedTool(
		"fs_list", "List", "List directory entries (names, sizes, is_dir).",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"path":    {Type: schema.String, Desc: "directory path", Required: true},
			"pattern": {Type: schema.String, Desc: "optional glob filter on entry names"},
		}),
		SyncStream(f.runList),
	)
	f.Glob = NewGuardedTool(
		"fs_glob", "Glob", "Find files whose path matches a glob pattern (supports **).",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"pattern": {Type: schema.String, Desc: "glob pattern, e.g. **/*.go", Required: true},
			"path":    {Type: schema.String, Desc: "root to search (default work root)"},
		}),
		SyncStream(f.runGlob),
	)
	f.Search = NewGuardedTool(
		"fs_search", "Search", "Search file contents with a regexp (skips .git, node_modules, vendor, .hg, .svn and oversized/binary files).",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"pattern":     {Type: schema.String, Desc: "Go regexp", Required: true},
			"path":        {Type: schema.String, Desc: "root to search (default .)"},
			"glob":        {Type: schema.String, Desc: "file-name glob filter"},
			"output_mode": {Type: schema.String, Desc: "files_with_matches (default) | content | count", Enum: []string{"files_with_matches", "content", "count"}},
		}),
		SyncStream(f.runSearch),
	)
	f.AstSearch = f.NewAstSearchTool()
	f.Patch = NewGuardedTool(
		"apply_patch", "Patch",
		"Apply a multi-file patch (add/update/delete/move) atomically. "+
			"Set dry_run=true to preview a unified diff without writing.",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"patch":   {Type: schema.String, Desc: "patch text: *** Begin Patch ... *** End Patch", Required: true},
			"dry_run": {Type: schema.Boolean, Desc: "if true, return a unified diff without writing"},
		}),
		SyncStream(f.runPatch),
	)
	return f
}

// Tools returns all FS tools once they exist (added in later tasks).
func (f *FSTools) Tools() []*GuardedTool {
	var ts []*GuardedTool
	for _, t := range []*GuardedTool{f.Read, f.Write, f.Edit, f.List, f.Glob, f.Search, f.AstSearch, f.Patch} {
		if t != nil {
			ts = append(ts, t)
		}
	}
	return ts
}

// rootFor resolves the work root a call should resolve paths against: the
// ctx-bound WorkRoot when one is set, else the static root FSTools was
// constructed with. Every top-level turn binds WithWorkRoot(ctx, o.workRoot)
// in bindExecutionContext, and bootstrap constructs FSTools from that exact
// same workRoot value, so for the common case this is a no-op (ctx and f.root
// agree). The two diverge only for an isolated sub-agent turn, whose nested
// Orchestrator is built with Config.WorkRoot pointing at its own VCS
// worktree directory (see acquireSubAgentWorkspace) — FSTools itself is never
// reconstructed per sub-agent (bootstrap builds it once, and runSubAgentTurn
// reuses the same *GuardedTool instances via selectSubAgentTools), so without
// this ctx check every sub-agent, isolated or not, would resolve paths
// against the single process-wide static root and silently write over each
// other's edits regardless of how correctly the VCS worktree lifecycle
// around it is wired.
func (f *FSTools) rootFor(ctx context.Context) string {
	if r := WorkRootFromContext(ctx); r != "" {
		return r
	}
	return f.root
}

// abs resolves a model-supplied path relative to the work root, enforcing the
// path jail: no traversal may escape the root. Paths that stay under the root
// (including harmless "sub/../sub" forms that clean back inside) are returned
// as their cleaned, root-anchored absolute form. Absolute paths that ANCHOR at
// the project root are accepted too — the root prefix is stripped and the path
// is treated as relative from there, so the model may pass either form
// ("reference/x.go" or "D:\code\yanshi\reference\x.go"). Absolute paths OUTSIDE
// the root are rejected. This is the single choke point for model input — every
// fs tool routes its user-supplied paths through here (via absPaths/checkFS), so
// the jail is enforced uniformly. Internally-derived absolute paths (e.g. from
// WalkDir) must NOT go through here; see checkFSSilent. The root itself is
// resolved via rootFor(ctx), not the static f.root field — see its doc comment.
func (f *FSTools) abs(ctx context.Context, p string) (string, error) {
	return resolveWithinRoot(f.rootFor(ctx), p)
}

// resolveWithinRoot is the jail itself, lifted out of FSTools.abs so the one
// other place that has to predict what a later fs call will resolve to runs the
// SAME code instead of a second implementation of it.
//
// That other place is permissionActionFor (requestpermission.go), which records
// an approval scope for the path a model says it will pass to fs_read/fs_write
// later. approval.Manager matches scopes with reflect.DeepEqual, so a scope
// holding the model's raw string while the real call resolves to an absolute
// path is a grant that never matches and never says so — the operator approves,
// the model is told "granted", and the next call prompts again. Three of the
// four ways to spell a target were in exactly that state (W-B-12 Blocking-1).
//
// It also gives that caller the jail's REFUSAL, which matters more: the jail
// runs BEFORE Authorize in every fs tool, so a path outside the root is not a
// permission question at all and no approval can admit it. A caller that shares
// this function finds that out while it can still say so, instead of minting a
// rule for a call that cannot reach the guard.
func resolveWithinRoot(root, p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("fs: empty path")
	}
	cleanRoot := filepath.Clean(root)
	// Allow absolute paths that anchor at the project root — strip the root
	// prefix and treat the result as relative from here. Absolute paths OUTSIDE
	// the root are rejected; any ".." escaping the root is still caught below.
	if isAbsolutePath(p) {
		absRoot, err := filepath.Abs(cleanRoot)
		if err != nil {
			return "", fmt.Errorf("fs: cannot resolve project root: %w", err)
		}
		cleaned := filepath.Clean(p)
		if !withinRoot(cleaned, absRoot) {
			return "", fmt.Errorf("absolute path %q is outside the project root", p)
		}
		rel, err := filepath.Rel(absRoot, cleaned)
		if err != nil {
			return "", fmt.Errorf("absolute path %q is outside the project root", p)
		}
		p = rel
	}
	resolved := filepath.Clean(filepath.Join(root, p))
	// Defense-in-depth: Join+Clean already collapses "..", so verify the result
	// still lives under the root. This is the authoritative check — it admits
	// "sub/../sub/x" (stays inside) and rejects "../foo" and "../../etc/passwd".
	if !withinRoot(resolved, cleanRoot) {
		if hasDotDot(p) {
			return "", fmt.Errorf("path must stay within the project root; '..' is not allowed")
		}
		return "", fmt.Errorf("path must stay within the project root")
	}
	return resolved, nil
}

// isAbsolutePath reports whether p is absolute under either POSIX or Windows
// conventions, regardless of host OS. filepath.IsAbs is host-specific (a POSIX
// "/x" path is not absolute on Windows), but model-supplied paths may use
// either form, so both are checked.
func isAbsolutePath(p string) bool {
	if filepath.IsAbs(p) {
		return true
	}
	pf := strings.ReplaceAll(p, "\\", "/")
	return strings.HasPrefix(pf, "/") || (len(pf) >= 3 && pf[1] == ':' && pf[2] == '/')
}

// hasDotDot reports whether any element of p (split on '/' or '\\') is "..".
func hasDotDot(p string) bool {
	pf := strings.ReplaceAll(p, "\\", "/")
	for _, el := range strings.Split(pf, "/") {
		if el == ".." {
			return true
		}
	}
	return false
}

// withinRoot reports whether resolved is equal to root or a descendant of it.
// Both inputs must be cleaned.
func withinRoot(resolved, root string) bool {
	if resolved == root {
		return true
	}
	return strings.HasPrefix(resolved, root+string(filepath.Separator))
}

// checkFS authorizes an FS op for the acting agent's profile (fail-closed).
// It routes through Authorize so that, when the static profile would deny AND a
// permission callback is bound in ctx (WS interactive mode), the user is asked
// and the decision honored. argsJSON is the tool call's raw args blob, carried
// in the PermissionRequest for display.
func (f *FSTools) checkFS(ctx context.Context, op, tool, argsJSON string, rawPaths ...string) ([]string, error) {
	absPaths, err := f.absPaths(ctx, rawPaths)
	if err != nil {
		return nil, err
	}
	if err := Authorize(ctx, guard.Action{
		Tool: tool,
		FS:   guard.FSWant{Op: op, Paths: absPaths},
	}, argsJSON); err != nil {
		return nil, err
	}
	return absPaths, nil
}

// checkFSSilent authorizes an FS op against the STATIC profile only, without
// consulting the permission callback. It is used for the per-file re-check
// inside fs_search, where a denial must skip the file silently rather than
// prompt (or fail the whole search). It mirrors the pre-interactive guard
// behavior exactly.
func (f *FSTools) checkFSSilent(ctx context.Context, op, tool string, rawPaths ...string) error {
	prof, ok := ProfileFromContext(ctx)
	if !ok {
		return &DenyErr{Reason: "no permission profile in context"}
	}
	// rawPaths are already-resolved absolute paths (from filepath.WalkDir over
	// the jailed root), so clean them directly. Do NOT route through abs()/
	// absPaths(), which would reject absolute paths as part of the model-input
	// jail — these paths are inherently safe (derived from the jailed root).
	paths := make([]string, len(rawPaths))
	for i, rp := range rawPaths {
		paths[i] = filepath.Clean(rp)
	}
	d := guard.New().Check(prof, guard.Action{Tool: tool, FS: guard.FSWant{Op: op, Paths: paths}})
	if !d.IsAllowed() {
		return &DenyErr{Reason: d.Reason}
	}
	return nil
}

// absPaths resolves each raw path relative to the work root and cleans it.
func (f *FSTools) absPaths(ctx context.Context, rawPaths []string) ([]string, error) {
	absPaths := make([]string, 0, len(rawPaths))
	for _, rp := range rawPaths {
		ap, err := f.abs(ctx, rp)
		if err != nil {
			return nil, err
		}
		absPaths = append(absPaths, ap)
	}
	return absPaths, nil
}

// trackEdit records a write to the active VCS scope (best-effort; errors
// swallowed). Called after a successful fs_write/fs_edit: if a VCSScope is
// bound in context with a non-nil VCS, the edit is routed to the worktree
// changeset when WorktreeID is set, else to main. A tracking failure must not
// fail the edit (the file write already succeeded), so the error is dropped.
func trackEdit(ctx context.Context, absPath string, content []byte) {
	sc, ok := VCSScopeFromContext(ctx)
	if !ok || sc.VCS == nil {
		return
	}
	var err error
	switch {
	case sc.WorktreeID != "":
		err = sc.VCS.RecordEditWorktree(sc.WorktreeID, sc.Agent, absPath, content)
	case sc.RepoID != "":
		err = sc.VCS.RecordEditMain(sc.RepoID, sc.Agent, absPath, content)
	}
	_ = err // best-effort: a tracking failure must not fail the edit
}

type fsReadArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	End    int    `json:"end"`
}

const fsSearchMaxBytes = 1 << 20 // 1 MiB per file in fs_search; larger files are skipped

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
	// Tier G entry C: an image file never returns its raw bytes as "text". When
	// the turn's model is multimodal the bytes go out of band, onto the turn's
	// image sink, and the model SEES the picture; otherwise there is nowhere for
	// an image part to go, so we keep the original hint and let the model route
	// through image_describe (the auxiliary multimodal model).
	if IsImagePath(a.Path) {
		if imageAttachEnabled(ctx) {
			return attachFileImage(ctx, absPath, a.Path), nil
		}
		return fmt.Sprintf("[image file: %s -- call image_describe with this path to understand it]", a.Path), nil
	}
	fi, err := os.Stat(absPath)
	if err != nil {
		return "", fmt.Errorf("fs_read: %w", err)
	}
	totalBytes := fi.Size()

	offset := a.Offset
	if offset < 1 {
		offset = 1
	}
	if a.End > 0 && a.End < offset {
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
		if totalLines >= offset && (a.End <= 0 || totalLines <= a.End) {
			// bufio.Scanner's default ScanLines already drops a trailing '\r'
			// (CRLF); the TrimSuffix is defense-in-depth in case the split is
			// ever changed.
			collected = append(collected, strings.TrimSuffix(sc.Text(), "\r"))
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("fs_read: scan: %w", err)
	}
	end := a.End
	if end <= 0 {
		end = totalLines
	}
	if end > totalLines {
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
	// summarize. Apply withInstructions FIRST, then guard the final string
	// (the hint must not push a sub-threshold window over the limit). Because
	// this returns a short errorResult, the uniform spillIfTooLong in
	// GuardedTool never fires for fs_read.
	final := f.withInstructions(ctx, absPath, result)
	if len(final) > SpillThreshold {
		return errorResult(fmt.Sprintf(
			"fs_read: result %s (%d lines %d–%d of %d, file %s) exceeds %s limit; narrow offset/end, or summarize(path)",
			humanBytes(len(final)), len(collected), offset, end, totalLines,
			humanBytes(int(totalBytes)), humanBytes(SpillThreshold))), nil
	}

	return final, nil
}

// withInstructions prepends any nested AGENTS.md instructions for absPath's
// directory to result, so the agent sees the directory-specific project rules
// when it reads a file. Root-level instructions are excluded (NestedInstructions)
// because they are already baked into the orchestrator's system prompt — this
// avoids duplication. Returns result unchanged when no nested instruction file
// exists (the common case), keeping fs_read output byte-identical to pre-A11 for
// directories without AGENTS.md.
func (f *FSTools) withInstructions(ctx context.Context, absPath, result string) string {
	hint := instruct.NestedInstructions(f.rootFor(ctx), filepath.Dir(absPath))
	if hint == "" {
		return result
	}
	return "# Project instructions (AGENTS.md chain for this directory)\n" + hint +
		"\n# --- file content ---\n" + result
}

type fsWriteArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (f *FSTools) runWrite(ctx context.Context, argsJSON string) (string, error) {
	var a fsWriteArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	paths, err := f.checkFS(ctx, "write", "fs_write", argsJSON, a.Path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(paths[0]), 0o755); err != nil {
		return "", fmt.Errorf("fs_write: mkdir: %w", err)
	}
	if err := os.WriteFile(paths[0], []byte(a.Content), 0o644); err != nil {
		return "", fmt.Errorf("fs_write: %w", err)
	}
	trackEdit(ctx, paths[0], []byte(a.Content))

	result := map[string]any{"wrote": paths[0], "bytes": len(a.Content)}
	if d := diagFor(ctx, paths[0], a.Content); d != "" {
		result["diagnostics"] = d
	}
	return toJSON(result), nil
}

type fsEditArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func (f *FSTools) runEdit(ctx context.Context, argsJSON string) (string, error) {
	var a fsEditArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	if a.OldString == "" {
		return "", fmt.Errorf("fs_edit: old_string must be non-empty")
	}
	paths, err := f.checkFS(ctx, "write", "fs_edit", argsJSON, a.Path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		return "", fmt.Errorf("fs_edit: %w", err)
	}
	src := string(data)
	count := strings.Count(src, a.OldString)
	var updated string
	if count >= 1 {
		// Exact match (the common path).
		if count > 1 && !a.ReplaceAll {
			return "", fmt.Errorf("fs_edit: old_string has multiple matches (%d); set replace_all to replace all", count)
		}
		if a.ReplaceAll {
			updated = strings.ReplaceAll(src, a.OldString, a.NewString)
		} else {
			updated = strings.Replace(src, a.OldString, a.NewString, 1)
		}
	} else {
		// No exact match: fall back to whitespace-lenient, line-oriented
		// matching (tabs vs spaces, trailing whitespace, \r\n vs \n) so a
		// near-miss old_string still lands. Returns ranges in original byte
		// offsets; ambiguous matches (more than one site) are treated the same
		// as the exact path treats them.
		ranges := lenientFind(data, a.OldString)
		if len(ranges) == 0 {
			return "", fmt.Errorf("fs_edit: %w", editNotFoundError(paths[0], data))
		}
		if len(ranges) > 1 && !a.ReplaceAll {
			return "", fmt.Errorf("fs_edit: old_string has multiple matches (%d, after whitespace normalization); set replace_all to replace all", len(ranges))
		}
		if a.ReplaceAll {
			// Replace every range, back-to-front so earlier offsets stay valid.
			updated = src
			for i := len(ranges) - 1; i >= 0; i-- {
				r := ranges[i]
				updated = updated[:r.Start] + a.NewString + updated[r.End:]
			}
			count = len(ranges)
		} else {
			r := ranges[0]
			updated = src[:r.Start] + a.NewString + src[r.End:]
			count = 1
		}
	}
	if err := os.WriteFile(paths[0], []byte(updated), 0o644); err != nil {
		return "", fmt.Errorf("fs_edit: %w", err)
	}
	trackEdit(ctx, paths[0], []byte(updated))

	result := map[string]any{"edited": paths[0], "replacements": count}
	if d := diagFor(ctx, paths[0], updated); d != "" {
		result["diagnostics"] = d
	}
	return toJSON(result), nil
}

type fsListArgs struct {
	Path    string `json:"path"`
	Pattern string `json:"pattern"`
}

func (f *FSTools) runList(ctx context.Context, argsJSON string) (string, error) {
	var a fsListArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	paths, err := f.checkFS(ctx, "read", "fs_list", argsJSON, a.Path)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(paths[0])
	if err != nil {
		return "", fmt.Errorf("fs_list: %w", err)
	}
	type entry struct {
		Name  string `json:"name"`
		Size  int64  `json:"size"`
		IsDir bool   `json:"is_dir"`
	}
	out := make([]entry, 0, len(entries))
	for _, e := range entries {
		if e.Name() == ".yanshi" {
			continue // hide the spillover scratch dir from listings
		}
		if a.Pattern != "" {
			if ok, err := guard.MatchGlob(a.Pattern, e.Name()); err != nil || !ok {
				continue
			}
		}
		fi, _ := e.Info()
		var sz int64
		if fi != nil {
			sz = fi.Size()
		}
		out = append(out, entry{Name: e.Name(), Size: sz, IsDir: e.IsDir()})
	}
	return toJSON(out), nil
}

type fsGlobArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

func (f *FSTools) runGlob(ctx context.Context, argsJSON string) (string, error) {
	var a fsGlobArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	root := a.Path
	if root == "" {
		root = "."
	}
	if _, err := f.checkFS(ctx, "read", "fs_glob", argsJSON, root); err != nil {
		return "", err
	}
	absRoot, err := f.abs(ctx, root)
	if err != nil {
		return "", err
	}
	matches := make([]string, 0)
	_ = filepath.WalkDir(absRoot, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return nil // skip unreadable
		}
		if d.IsDir() {
			if fsSearchIgnore[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(absRoot, p)
		if rerr != nil {
			rel = p
		}
		rel = filepath.ToSlash(rel)
		if ok, merr := guard.MatchGlob(a.Pattern, rel); merr == nil && ok {
			matches = append(matches, rel)
		}
		return nil
	})
	return toJSON(matches), nil
}

var fsSearchIgnore = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".hg": true, ".svn": true,
	".yanshi": true,
}

type fsSearchArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	OutputMode string `json:"output_mode"`
}

func (f *FSTools) runSearch(ctx context.Context, argsJSON string) (string, error) {
	var a fsSearchArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return "", fmt.Errorf("fs_search: bad regexp: %w", err)
	}
	root := a.Path
	if root == "" {
		root = "."
	}
	if _, err := f.checkFS(ctx, "read", "fs_search", argsJSON, root); err != nil {
		return "", err
	}
	absRoot, err := f.abs(ctx, root)
	if err != nil {
		return "", err
	}
	mode := a.OutputMode
	if mode == "" {
		mode = "files_with_matches"
	}
	type hit struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Text string `json:"text"`
	}
	files := make([]string, 0)
	content := make([]hit, 0)
	count := 0
	_ = filepath.WalkDir(absRoot, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return nil // skip unreadable
		}
		if d.IsDir() {
			if fsSearchIgnore[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(absRoot, p)
		if rerr != nil {
			rel = p
		}
		rel = filepath.ToSlash(rel)
		if a.Glob != "" {
			if ok, merr := guard.MatchGlob(a.Glob, filepath.Base(rel)); merr != nil || !ok {
				return nil
			}
		}
		// Per-file authorization: re-check the read-list for each file so a
		// profile granting only the root dir (not dir/**) can't have its
		// subtree read via a single root-level check. Skip silently rather
		// than failing the whole search (consistent with the binary/oversized
		// skips below), mirroring web_fetch's per-redirect re-check.
		if err := f.checkFSSilent(ctx, "read", "fs_search", p); err != nil {
			return nil
		}
		fc, oerr := os.Open(p)
		if oerr != nil {
			return nil
		}
		data, rerr := io.ReadAll(io.LimitReader(fc, fsSearchMaxBytes+1))
		fc.Close()
		if rerr != nil {
			return nil
		}
		if len(data) > fsSearchMaxBytes {
			return nil // oversized file — skip
		}
		if isBinary(data) {
			return nil
		}
		lines := strings.Split(string(data), "\n")
		matched := false
		for i, ln := range lines {
			if re.MatchString(ln) {
				matched = true
				count++
				if mode == "content" {
					content = append(content, hit{Path: rel, Line: i + 1, Text: strings.TrimSpace(ln)})
				}
			}
		}
		if matched {
			files = append(files, rel)
		}
		return nil
	})
	switch mode {
	case "content":
		return toJSON(content), nil
	case "count":
		return toJSON(map[string]int{"matches": count, "files": len(files)}), nil
	default:
		return toJSON(files), nil
	}
}

// isBinary heuristically reports whether data looks non-textual (contains a NUL byte).
func isBinary(data []byte) bool {
	for _, b := range data {
		if b == 0 {
			return true
		}
	}
	return false
}
