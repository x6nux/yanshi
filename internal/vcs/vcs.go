// Package vcs implements yanshi's lightweight, SQLite-backed VCS
// (worktrees branched from a main trunk + a git-like change history).
package vcs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/store"
)

// ErrNoChanges is returned by CommitMain/CommitWorktree (via commitScope) when
// the scope has no pending edits to fold into a commit. Callers may distinguish
// "nothing to do" from a genuine store failure via errors.Is(err, vcs.ErrNoChanges).
var ErrNoChanges = errors.New("vcs: no changes to commit")

// ErrConflicts is returned by MergeToMain when both main and the worktree changed
// the same file(s) and force is false. The conflict path list is also returned.
var ErrConflicts = errors.New("vcs: merge conflicts")

var defaultIgnore = []string{
	"node_modules", ".git", ".hg", ".svn", "vendor", "dist", "build",
	".next", ".nuxt", "target", "__pycache__", ".venv", "venv",
	".idea", ".vscode", "*.log", "*.pyc", ".DS_Store",
	// yanshi's own SQLite store: never tracked into the init commit (a tracked
	// db would bloat every tree and record the store's own internal churn).
	"*.db", "yanshi.db",
	// Compiled artifacts: never tracked (a 38MB yanshi.exe was the bulk of the
	// blob store; these rebuild on every `go build` and have no place in history).
	"*.exe", "*.dll", "*.so", "*.dylib", "*.o", "*.a",
}

// VCS is the lightweight version-control core over a store.
type VCS struct {
	store       *store.Store
	ignore      []string
	// ignoreMu protects the shared ignore slice independently of repo lanes.
	// isIgnored copies under RLock, then matches after unlock; loadDotIgnore parses
	// first and appends under Lock, so unrelated repo scans never hold this lock
	// during filesystem or database work.
	ignoreMu    sync.RWMutex
	worktreeDir string
	// treeCache memoizes reconstructed full path→hash trees per commit id.
	// Commit trees are immutable (history is append-only), so a cached entry is
	// always correct for the life of the process. This keeps commitTree O(1) on
	// repeated access instead of O(chain-length) per reconstruction — important
	// because delta storage (§writeCommitInTx) makes a cold reconstruction walk
	// the whole parent chain. Entries are defensive copies; callers may mutate
	// the map returned by commitTree without corrupting the cache.
	//
	// CONCURRENCY: a VCS is shared across HTTP handler goroutines (bootstrap wires
	// one into both the orchestrator and the broker), so concurrent commitTree /
	// AddWorktree / MergeToMain calls read+write this map. Every access MUST go
	// through treeCacheMu (RLock for reads, Lock for writes) — a bare map op is a
	// fatal "concurrent map read and map write" panic. The Lock is never held
	// across a DB call, so it cannot deadlock against the single-conn store pool.
	treeCache   map[string]map[string]string
	treeCacheMu sync.RWMutex

	// repoLocksMu protects ONLY the repoLocks map. Never hold it while waiting
	// on a per-repo mutex: take/create the pointer, release the index mutex, then
	// lock the repo mutex. This serializes DB+FS composites within one repo while
	// allowing independent repositories to progress concurrently (CB1).
	repoLocksMu sync.Mutex
	repoLocks   map[string]*sync.Mutex
}

// lockRepo locks the write lane for key and returns its unlock function.
// repoID is the normal key; InitRepo uses "init:"+canonicalRoot before an id
// exists. Lock entries intentionally live for the VCS lifetime (repo count is
// bounded by the process's opened projects; deleting them would create an ABA
// race with goroutines that already retained the mutex pointer).
func (v *VCS) lockRepo(key string) func() {
	v.repoLocksMu.Lock()
	if v.repoLocks == nil {
		v.repoLocks = make(map[string]*sync.Mutex)
	}
	mu := v.repoLocks[key]
	if mu == nil {
		mu = &sync.Mutex{}
		v.repoLocks[key] = mu
	}
	v.repoLocksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// New builds a VCS. worktreeDir is where worktree working dirs are created.
// Extra ignore patterns are merged with the built-in defaults.
func New(s *store.Store, worktreeDir string, extraIgnore ...string) *VCS {
	ig := append([]string{}, defaultIgnore...)
	ig = append(ig, extraIgnore...)
	return &VCS{store: s, ignore: ig, worktreeDir: worktreeDir, treeCache: map[string]map[string]string{}}
}

// isIgnored snapshots the shared slice under RLock and performs all glob work
// after releasing it, so ignoreMu is never nested with a repo lane for long.
func (v *VCS) isIgnored(rel string) bool {
	v.ignoreMu.RLock()
	patterns := append([]string(nil), v.ignore...)
	v.ignoreMu.RUnlock()

	rel = filepath.ToSlash(rel)
	for _, pat := range patterns {
		if ok, err := guard.MatchGlob(pat, rel); err == nil && ok {
			return true
		}
	}
	for _, seg := range strings.Split(rel, "/") {
		for _, pat := range patterns {
			if !strings.ContainsAny(pat, "*?[") && seg == pat {
				return true
			}
		}
	}
	return false
}

// hashContent returns the sha256 hex digest of content.
func hashContent(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// putBlob stores content in the blob store (dedup'd by hash via
// INSERT OR IGNORE) and returns the content's sha256 hex hash.
func (v *VCS) putBlob(content []byte) string {
	h := hashContent(content)
	_ = v.store.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(
			"INSERT OR IGNORE INTO vcs_blobs (hash, content, size) VALUES (?, ?, ?)",
			h, content, len(content),
		)
		return err
	})
	return h
}

// getBlob returns the stored content for hash. An empty/missing row yields
// the sql.ErrNoRows error from QueryRow.
func (v *VCS) getBlob(h string) ([]byte, error) {
	var content []byte
	err := v.store.DB.QueryRow("SELECT content FROM vcs_blobs WHERE hash = ?", h).Scan(&content)
	return content, err
}

// repoRow is a lightweight view of a vcs_repos row.
type repoRow struct {
	ID, RootPath, MainHead string
	CreatedAt              int64
}

// getRepo loads a repo row by id.
func (v *VCS) getRepo(id string) (repoRow, error) {
	var r repoRow
	err := v.store.DB.QueryRow(
		"SELECT id, root_path, main_head, created_at FROM vcs_repos WHERE id = ?", id,
	).Scan(&r.ID, &r.RootPath, &r.MainHead, &r.CreatedAt)
	return r, err
}

// Worktree is a lightweight view of a vcs_worktrees row.
type Worktree struct {
	ID, RepoID, Path, BaseCommit string
	CreatedAt                    int64
	// Tip is the id of the newest commit on this worktree's branch (mirrors
	// main_head on vcs_repos), or empty when the branch has no commits yet
	// (the worktree is still at BaseCommit). Tracked inside commitScope's tx so
	// two commits in the same second cannot orphan the newer one.
	Tip string
}

// getWorktree loads a worktree row by id.
func (v *VCS) getWorktree(id string) (Worktree, error) {
	var w Worktree
	err := v.store.DB.QueryRow(
		"SELECT id, repo_id, path, base_commit, created_at, tip FROM vcs_worktrees WHERE id=?", id,
	).Scan(&w.ID, &w.RepoID, &w.Path, &w.BaseCommit, &w.CreatedAt, &w.Tip)
	return w, err
}

// RepoRoot returns the absolute root path of repoID. It is the exported
// counterpart to getRepo for callers (e.g. the vcs_restore tool) that need the
// working-copy dir without reaching into the row struct.
func (v *VCS) RepoRoot(repoID string) (string, error) {
	r, err := v.getRepo(repoID)
	if err != nil {
		return "", err
	}
	return r.RootPath, nil
}

// RepoMainHead returns the current main_head commit id of repoID, or "" + error
// when the repo cannot be loaded. Exported mirror of getRepo.MainHead for
// callers (ws.go handler, sse chat handler) that need to bind confirmations
// to the exact head at request time (B2-RB1 必修项 E: target binding).
func (v *VCS) RepoMainHead(repoID string) (string, error) {
	r, err := v.getRepo(repoID)
	if err != nil {
		return "", err
	}
	return r.MainHead, nil
}

// WorktreePath returns the absolute working-dir path of wtID. It is the
// exported counterpart to getWorktree for callers that need the worktree's
// working copy without reaching into the row struct.
func (v *VCS) WorktreePath(wtID string) (string, error) {
	w, err := v.getWorktree(wtID)
	if err != nil {
		return "", err
	}
	return w.Path, nil
}

// writeCommit stores a commit + its full path→hash tree in its own transaction.
// It is the standalone entry point (used by InitRepo). Commit and MergeToMain use
// writeCommitInTx directly so they can fold the commit-row + tree-row inserts in
// with their side-effect updates (clear changeset, advance head) as one atomic tx.
func (v *VCS) writeCommit(repoID, worktreeID, parentID, mergedFrom, author, message string, tree map[string]string) (string, error) {
	var id string
	err := v.store.WriteTx(context.Background(), func(tx *sql.Tx) error {
		var e error
		id, e = v.writeCommitInTx(tx, repoID, worktreeID, parentID, mergedFrom, author, message, tree)
		return e
	})
	return id, err
}

// writeCommitInTx stores a commit + its DELTA vs the parent's tree inside a
// CALLER-OWNED transaction (it does not Begin/Commit). Returns the commit id.
//
// Delta storage (the O(changes×commits) fix): instead of writing the FULL
// path→hash snapshot per commit (which bloated the db by O(files) per commit
// even when one file changed), writeCommitInTx diffs the new tree against the
// reconstructed parent tree and stores only added/modified/deleted rows (op
// column). commitTree reverses this by walking the parent chain and applying
// deltas root→commit. Callers (InitRepo, commitScope, MergeToMain) still pass
// the FULL new tree — the delta is computed here, so callers are unchanged.
//
// The id is a content hash of (repoID, worktreeID, parentID, mergedFrom, author,
// message, tree) — it depends on the logical tree, not on the storage layout, so
// it is stable across the full-snapshot→delta refactor. mergedFrom is included so
// two merges that produce identical trees from the same parent but differ in
// their source worktree do not collide on the PK. INSERT OR IGNORE makes the id
// stable across replays (same inputs → same rows).
func (v *VCS) writeCommitInTx(tx *sql.Tx, repoID, worktreeID, parentID, mergedFrom, author, message string, tree map[string]string) (string, error) {
	id := commitID(repoID, worktreeID, parentID, mergedFrom, author, message, tree)
	now := time.Now().Unix()
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO vcs_commits (id, repo_id, worktree_id, parent_id, merged_from, author, message, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, repoID, worktreeID, parentID, mergedFrom, author, message, now,
	); err != nil {
		return "", err
	}
	// Reconstruct the parent's full tree to diff against. Trees are immutable so
	// this is safe to read through the tx (the parent chain is already-committed
	// data; the new commit's rows aren't written yet at this point). For a root
	// commit (parentID empty) the parent tree is the empty map → every path is an
	// 'add', matching the old full-snapshot behavior for root commits.
	parentTree := v.reconstructTree(tx, parentID)
	for p := range unionKeys(parentTree, tree) {
		oh, okOld := parentTree[p]
		nh, okNew := tree[p]
		switch {
		case okNew && !okOld: // added in this commit
			if _, err := tx.Exec(
				"INSERT OR IGNORE INTO vcs_tree (commit_id, path, blob_hash, op) VALUES (?, ?, ?, 'add')",
				id, p, nh,
			); err != nil {
				return "", err
			}
		case okNew && okOld && oh != nh: // modified in this commit
			if _, err := tx.Exec(
				"INSERT OR IGNORE INTO vcs_tree (commit_id, path, blob_hash, op) VALUES (?, ?, ?, 'mod')",
				id, p, nh,
			); err != nil {
				return "", err
			}
		case !okNew && okOld: // deleted in this commit
			if _, err := tx.Exec(
				"INSERT OR IGNORE INTO vcs_tree (commit_id, path, blob_hash, op) VALUES (?, ?, ?, 'del')",
				id, p, "",
			); err != nil {
				return "", err
			}
		}
		// unchanged (oh == nh): store NOTHING — the whole point.
	}
	// Cache the just-written commit's full tree so the next commitTree(id) is O(1)
	// (avoids a cold chain reconstruction of the new tip). `tree` is the caller's
	// map; store a defensive copy so any later caller mutation can't corrupt the
	// cache. A tx that rolls back after this leaves a stale entry for an
	// un-persisted id, which is harmless: the id is content-derived and only ever
	// looked up again on a retry that re-caches the same value.
	v.cacheTree(id, tree)
	return id, nil
}

// commitID deterministically hashes commit constituents (each value is
// null-terminated so "ab"+"c" != "a"+"bc").
func commitID(parts ...any) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%v\x00", p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// commitTree returns the full path→hash map for a commit (empty map if missing).
//
// Delta storage means vcs_tree holds only the DELTA per commit, so commitTree
// reconstructs the full tree by walking the parent chain root→id and applying
// each commit's deltas in order (add/mod set path→hash; del removes path). The
// result is memoized in v.treeCache (trees are immutable), making repeated calls
// O(1). The returned map is a defensive copy — callers may mutate it freely.
//
// CONCURRENCY: the cache hit path takes treeCacheMu.RLock (many concurrent
// readers OK); a miss reconstructs (no lock held during the DB chain walk — it
// cannot deadlock the single-conn pool) then takes Lock to publish the entry.
// Two misses for the same id both reconstruct and write the same immutable tree;
// the second write is redundant, not corrupting.
func (v *VCS) commitTree(id string) map[string]string {
	if id == "" {
		return map[string]string{}
	}
	v.treeCacheMu.RLock()
	cached, ok := v.treeCache[id]
	v.treeCacheMu.RUnlock()
	if ok {
		return cloneTree(cached)
	}
	tree := v.reconstructTree(nil, id)
	v.treeCacheMu.Lock()
	v.treeCache[id] = tree
	v.treeCacheMu.Unlock()
	return cloneTree(tree)
}

// cloneTree returns a shallow copy of a path→hash tree (callers may mutate the
// result without corrupting v.treeCache, whose entries are never exposed as-is).
func cloneTree(t map[string]string) map[string]string {
	out := make(map[string]string, len(t))
	for p, h := range t {
		out[p] = h
	}
	return out
}

// treeEntry is one row of a commit's delta (the per-commit diff vs its parent).
type treeEntry struct {
	Path, BlobHash, Op string
}

// unionKeys returns the set of keys present in either map (for diffing two
// trees). Order is non-deterministic — callers that need order sort separately.
func unionKeys(a, b map[string]string) map[string]struct{} {
	out := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		out[k] = struct{}{}
	}
	for k := range b {
		out[k] = struct{}{}
	}
	return out
}

// queryRowScoped runs QueryRow via tx when non-nil, else via the DB pool. This
// avoids the single-connection deadlock (SetMaxOpenConns(1)) that would occur if
// a writeCommitInTx path read through v.store.DB while the caller's tx holds the
// only connection.
func (v *VCS) queryRowScoped(tx *sql.Tx, query string, args ...any) *sql.Row {
	if tx != nil {
		return tx.QueryRow(query, args...)
	}
	return v.store.DB.QueryRow(query, args...)
}

// queryScoped is the rows-returning counterpart to queryRowScoped.
func (v *VCS) queryScoped(tx *sql.Tx, query string, args ...any) (*sql.Rows, error) {
	if tx != nil {
		return tx.Query(query, args...)
	}
	return v.store.DB.Query(query, args...)
}

// commitDelta returns the delta rows (path, blob_hash, op) recorded for a commit
// — i.e. exactly the paths that commit changed vs its parent. Read via the DB
// pool (non-tx callers: changedCount, diagnostics).
func (v *VCS) commitDelta(id string) []treeEntry {
	return v.commitDeltaScoped(nil, id)
}

// commitDeltaScoped is the tx-aware core of commitDelta.
func (v *VCS) commitDeltaScoped(tx *sql.Tx, id string) []treeEntry {
	rows, err := v.queryScoped(tx, "SELECT path, blob_hash, op FROM vcs_tree WHERE commit_id=?", id)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []treeEntry
	for rows.Next() {
		var e treeEntry
		if rows.Scan(&e.Path, &e.BlobHash, &e.Op) == nil {
			out = append(out, e)
		}
	}
	_ = rows.Err()
	return out
}

// reconstructTree returns the full path→hash tree for commit id. It consults
// v.treeCache (under treeCacheMu.RLock) for the fast path but does NOT itself
// publish entries — commitTree owns the cache write for the non-tx read path,
// and cacheTree owns it for the writeCommitInTx pre-warm. tx is used for
// delta/commit-row reads when non-nil (the writeCommitInTx path, which must read
// through its own tx to avoid the single-connection deadlock); nil reads via the
// DB pool. An empty id yields an empty map (root or missing). On a cache miss the
// parent chain is walked id→root, then deltas applied root→id. The RLock is
// released before any DB call, so this cannot deadlock the single-conn pool.
func (v *VCS) reconstructTree(tx *sql.Tx, id string) map[string]string {
	if id == "" {
		return map[string]string{}
	}
	v.treeCacheMu.RLock()
	cached, ok := v.treeCache[id]
	v.treeCacheMu.RUnlock()
	if ok {
		return cached
	}
	// Collect the chain id → root (root last). A missing commit row ends the walk
	// silently (treated as a root whose delta is empty).
	var chain []string
	cur := id
	for cur != "" {
		chain = append(chain, cur)
		c, err := v.getCommitScoped(tx, cur)
		if err != nil {
			break
		}
		cur = c.ParentID
	}
	// Apply deltas root → id (chain is id→root, so iterate reversed).
	tree := map[string]string{}
	for i := len(chain) - 1; i >= 0; i-- {
		for _, e := range v.commitDeltaScoped(tx, chain[i]) {
			if e.Op == "del" {
				delete(tree, e.Path)
			} else { // add | mod
				tree[e.Path] = e.BlobHash
			}
		}
	}
	return tree
}

// cacheTree stores a defensive copy of tree under id (used by writeCommitInTx to
// pre-warm the cache for the just-written commit so its first commitTree read is
// O(1) instead of a cold chain walk). All map access is under treeCacheMu.Lock;
// no DB call is made under the lock, so this is safe to call from inside a tx
// (the Lock cannot deadlock against the single-conn pool held by the tx).
func (v *VCS) cacheTree(id string, tree map[string]string) {
	v.treeCacheMu.Lock()
	defer v.treeCacheMu.Unlock()
	if v.treeCache == nil {
		v.treeCache = map[string]map[string]string{}
	}
	v.treeCache[id] = cloneTree(tree)
}

// Commit is a lightweight view of a vcs_commits row.
type Commit struct {
	ID, RepoID, WorktreeID, ParentID, MergedFrom, Author, Message string
	CreatedAt                                                     int64
	FilesChanged                                                  int
}

func (v *VCS) getCommit(id string) (Commit, error) {
	return v.getCommitScoped(nil, id)
}

// getCommitScoped is the tx-aware core of getCommit (used by reconstructTree
// inside a caller-owned tx to avoid the single-connection deadlock).
func (v *VCS) getCommitScoped(tx *sql.Tx, id string) (Commit, error) {
	var c Commit
	err := v.queryRowScoped(tx,
		`SELECT id, repo_id, worktree_id, parent_id, merged_from, author, message, created_at
		 FROM vcs_commits WHERE id=?`, id,
	).Scan(&c.ID, &c.RepoID, &c.WorktreeID, &c.ParentID, &c.MergedFrom, &c.Author, &c.Message, &c.CreatedAt)
	return c, err
}

// canonicalRepoRoot returns the ONLY root identity used for both locking and
// vcs_repos.root_path. Abs closes relative aliases; EvalSymlinks closes
// junction/symlink aliases; Windows folding closes case aliases on its
// case-insensitive filesystem.
func canonicalRepoRoot(rootPath string) (string, error) {
	abs, err := filepath.Abs(rootPath)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("vcs: canonicalize repo root: %w", err)
	}
	real = filepath.Clean(real)
	if runtime.GOOS == "windows" {
		real = strings.ToLower(real)
	}
	return real, nil
}

func initRepoLockKey(canonicalRoot string) string {
	return "init:" + canonicalRoot
}

// loadDotIgnore parses without a lock, then appends as one protected mutation.
func (v *VCS) loadDotIgnore(repoRoot string) {
	data, err := os.ReadFile(filepath.Join(repoRoot, ".yanshiignore"))
	if err != nil {
		return
	}
	var additions []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			additions = append(additions, line)
		}
	}
	if len(additions) == 0 {
		return
	}
	v.ignoreMu.Lock()
	v.ignore = append(v.ignore, additions...)
	v.ignoreMu.Unlock()
}

// InitRepo first serializes discovery/creation by canonical root. For an
// existing row it then acquires that repo's normal lane in the only permitted
// order (init-key -> repoID), re-queries while locked, and refreshes ignore
// rules. No repoID writer ever acquires an init-key, so no reverse edge exists.
func (v *VCS) InitRepo(rootPath string) (string, error) {
	canonicalRoot, err := canonicalRepoRoot(rootPath)
	if err != nil {
		return "", err
	}
	unlockInit := v.lockRepo(initRepoLockKey(canonicalRoot))
	defer unlockInit()

	var existingID string
	err = v.store.DB.QueryRow(
		"SELECT id FROM vcs_repos WHERE root_path = ?", canonicalRoot,
	).Scan(&existingID)
	switch {
	case err == nil:
		unlockRepo := v.lockRepo(existingID)
		defer unlockRepo()
		var lockedID string
		if err := v.store.DB.QueryRow(
			"SELECT id FROM vcs_repos WHERE root_path = ?", canonicalRoot,
		).Scan(&lockedID); err != nil {
			return "", err
		}
		if lockedID != existingID {
			return "", fmt.Errorf(
				"vcs: repo identity changed for root %s", canonicalRoot)
		}
		v.loadDotIgnore(canonicalRoot)
		return lockedID, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", err
	default:
		return v.initNewRepoLocked(canonicalRoot)
	}
}

// initNewRepoLocked creates a previously absent canonical root while its
// init-key lane is held. The row is inserted last, so no repoID writer can
// discover the repo until its initial commit exists.
func (v *VCS) initNewRepoLocked(canonicalRoot string) (string, error) {
	v.loadDotIgnore(canonicalRoot)
	id := newVCSID()
	tree := map[string]string{}
	_ = filepath.WalkDir(canonicalRoot, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		rel, err := filepath.Rel(canonicalRoot, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if v.isIgnored(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if v.isIgnored(rel) {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		tree[rel] = v.putBlob(data)
		return nil
	})
	commitIDVal, err := v.writeCommit(
		id, "", "", "", "orchestrator", "vcs init", tree,
	)
	if err != nil {
		return "", err
	}
	if err := v.store.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.Exec(
			"INSERT INTO vcs_repos (id, root_path, main_head, created_at) VALUES (?, ?, ?, ?)",
			id, canonicalRoot, commitIDVal, time.Now().Unix(),
		)
		return e
	}); err != nil {
		return "", err
	}
	return id, nil
}

// newVCSID returns a fresh random hex id (24 hex chars / 12 bytes), mirroring
// store.newID. Used for repo and (later) worktree ids that aren't content-derived.
func newVCSID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// worktreeTip returns the latest commit on a worktree's branch: the tip column
// on vcs_worktrees when set (mirrors main_head on vcs_repos), else the
// worktree's base_commit. The tip column is advanced inside commitScope's
// transaction, so it is immune to the created_at/id tie-break races that an
// ORDER BY query would hit when two commits land in the same second. Returns ""
// if the worktree row cannot be loaded. A worktree created before the tip
// column existed (empty tip) falls back to base_commit until its next commit.
func (v *VCS) worktreeTip(wtID string) string {
	wt, err := v.getWorktree(wtID)
	if err != nil {
		return ""
	}
	if wt.Tip != "" {
		return wt.Tip
	}
	return wt.BaseCommit
}

// AddWorktree creates an isolated working dir under worktreeDir containing a copy
// of main_head's tree, branched from main_head. The agents list records sharing
// (v1: stored as a comma-joined metadata string; a future vcs_worktree_agents
// table can normalize it). Returns the new worktree.
func (v *VCS) AddWorktree(repoID string, agents []string) (Worktree, error) {
	unlock := v.lockRepo(repoID)
	defer unlock()
	return v.addWorktreeLocked(repoID, agents)
}

func (v *VCS) addWorktreeLocked(repoID string, agents []string) (Worktree, error) {
	r, err := v.getRepo(repoID)
	if err != nil {
		return Worktree{}, err
	}
	id := newVCSID()
	wtPath := filepath.Join(v.worktreeDir, id)
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		return Worktree{}, err
	}
	// materialize main_head's tree into wtPath (repo-relative layout)
	for path, h := range v.commitTree(r.MainHead) {
		content, err := v.getBlob(h)
		if err != nil {
			continue
		}
		dest := filepath.Join(wtPath, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return Worktree{}, err
		}
		if err := os.WriteFile(dest, content, 0o644); err != nil {
			return Worktree{}, err
		}
	}
	wt := Worktree{ID: id, RepoID: repoID, Path: wtPath, BaseCommit: r.MainHead, CreatedAt: time.Now().Unix()}
	err = v.store.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.Exec(
			"INSERT INTO vcs_worktrees (id, repo_id, path, base_commit, created_at, active) VALUES (?, ?, ?, ?, ?, 1)",
			wt.ID, wt.RepoID, wt.Path, wt.BaseCommit, wt.CreatedAt,
		)
		return e
	})
	if err != nil {
		return Worktree{}, err
	}
	// v1: agents recorded only in-API (no separate table). Multi-agent sharing is
	// expressed by multiple sessions/tasks referencing the same worktree ID.
	_ = agents
	return wt, nil
}

// RemoveWorktree marks a worktree inactive (history is retained for audit) and
// removes the materialized working dir from disk to avoid leaks. The working
// dir is only removed when it lives under the configured worktreeDir (so a
// path injected by a test or pointing elsewhere can never be deleted), and
// removal errors are swallowed — the active=0 flip is the load-bearing step.
func (v *VCS) RemoveWorktree(wtID string) error {
	wt, err := v.getWorktree(wtID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	unlock := v.lockRepo(wt.RepoID)
	defer unlock()
	return v.removeWorktreeLocked(wt.RepoID, wtID)
}

func (v *VCS) removeWorktreeLocked(repoID, wtID string) error {
	wt, err := v.getWorktree(wtID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if wt.RepoID != repoID {
		return fmt.Errorf("vcs: worktree %s changed repository", wtID)
	}
	// Guard: only remove the working dir if it is under the configured
	// worktreeDir. This prevents RemoveWorktree from ever deleting an arbitrary
	// path (e.g. a test that points wt.Path at the repo root).
	if wt.Path != "" && v.worktreeDir != "" {
		if rel, rerr := filepath.Rel(v.worktreeDir, wt.Path); rerr == nil && rel != "." && !strings.HasPrefix(filepath.ToSlash(rel), "..") {
			_ = os.RemoveAll(wt.Path)
		}
	}
	err = v.store.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.Exec("UPDATE vcs_worktrees SET active=0 WHERE id=?", wtID)
		return e
	})
	return err
}

// scopeHeadTree returns the path→hash tree of the current head for a scope
// ("main"→repo.MainHead, "worktree"→worktreeTip). Empty/unknown scopes yield an
// empty map.
func (v *VCS) scopeHeadTree(scopeType, scopeID string) map[string]string {
	var head string
	switch scopeType {
	case "main":
		r, err := v.getRepo(scopeID)
		if err != nil {
			return map[string]string{}
		}
		head = r.MainHead
	case "worktree":
		head = v.worktreeTip(scopeID)
	}
	if head == "" {
		return map[string]string{}
	}
	return v.commitTree(head)
}

// recordEdit is the shared auto-track core: resolve absPath to a repo-relative
// path, skip silently if outside the repo root or ignored, store the content as
// a blob, and upsert the path into the scope's pending vcs_uncommitted changeset.
// absPath is expected to be absolute (from fs tools / ACP); repoRoot is the
// repo's root_path.
func (v *VCS) recordEdit(scopeType, scopeID, repoRoot, absPath string, content []byte) error {
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
	h := v.putBlob(content)
	op := v.deriveOp(scopeType, scopeID, rel, h)
	err = v.store.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.Exec(
			"INSERT INTO vcs_uncommitted (scope_type, scope_id, path, blob_hash, op) VALUES (?, ?, ?, ?, ?)\n"+
				"ON CONFLICT(scope_type, scope_id, path) DO UPDATE SET blob_hash=excluded.blob_hash, op=excluded.op",
			scopeType, scopeID, rel, h, op,
		)
		return e
	})
	return err
}

// deriveOp returns "added" if rel is not present in the scope's head tree, else
// "modified".
func (v *VCS) deriveOp(scopeType, scopeID, rel, newHash string) string {
	head := v.scopeHeadTree(scopeType, scopeID)
	if _, ok := head[rel]; !ok {
		return "added"
	}
	return "modified"
}

// Uncommitted returns the pending path→blob_hash map for a scope (exported as
// a test/diagnostic accessor). Returns an empty map for an unknown scope or on
// query error.
func (v *VCS) Uncommitted(scopeType, scopeID string) map[string]string {
	rows, err := v.store.DB.Query("SELECT path, blob_hash FROM vcs_uncommitted WHERE scope_type=? AND scope_id=?", scopeType, scopeID)
	if err != nil {
		return map[string]string{}
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, h string
		if rows.Scan(&p, &h) == nil {
			out[p] = h
		}
	}
	if rerr := rows.Err(); rerr != nil {
		return map[string]string{}
	}
	return out
}

// RecordEditMain records an edit on the main scope (serialized per repo).
func (v *VCS) RecordEditMain(repoID, agent, absPath string, content []byte) error {
	unlock := v.lockRepo(repoID)
	defer unlock()
	return v.recordEditMainLocked(repoID, agent, absPath, content)
}

func (v *VCS) recordEditMainLocked(repoID, agent, absPath string, content []byte) error {
	r, err := v.getRepo(repoID)
	if err != nil {
		return err
	}
	return v.recordEdit("main", repoID, r.RootPath, absPath, content)
}

// RecordEditWorktree records an edit on a worktree scope (serialized per repo).
func (v *VCS) RecordEditWorktree(wtID, agent, absPath string, content []byte) error {
	wt, err := v.getWorktree(wtID)
	if err != nil {
		return err
	}
	unlock := v.lockRepo(wt.RepoID)
	defer unlock()
	return v.recordEditWorktreeLocked(wt.RepoID, wtID, agent, absPath, content)
}

func (v *VCS) recordEditWorktreeLocked(
	repoID, wtID, agent, absPath string, content []byte,
) error {
	wt, err := v.getWorktree(wtID)
	if err != nil {
		return err
	}
	if wt.RepoID != repoID {
		return fmt.Errorf("vcs: worktree %s changed repository", wtID)
	}
	return v.recordEdit("worktree", wtID, wt.Path, absPath, content)
}

// commitScope folds a scope's vcs_uncommitted into a new commit on its head,
// clears the scope's uncommitted, and advances the head. For "main" the head is
// the repo's main_head; for "worktree" the head is the worktree's tip column —
// both updated inside the same transaction as the commit. Returns ErrNoChanges
// if the scope has no pending edits.
func (v *VCS) commitScope(scopeType, scopeID, repoID, worktreeID, author, message string) (string, error) {
	tree := v.scopeHeadTree(scopeType, scopeID)
	var parentID string
	switch scopeType {
	case "main":
		r, _ := v.getRepo(repoID)
		parentID = r.MainHead
	case "worktree":
		parentID = v.worktreeTip(worktreeID)
	}
	rows, err := v.store.DB.Query("SELECT path, blob_hash, op FROM vcs_uncommitted WHERE scope_type=? AND scope_id=?", scopeType, scopeID)
	if err != nil {
		return "", err
	}
	var applied int
	for rows.Next() {
		var p, h, op string
		if rows.Scan(&p, &h, &op) != nil {
			continue
		}
		if op == "deleted" {
			delete(tree, p)
		} else {
			tree[p] = h
		}
		applied++
	}
	rows.Close()
	if rerr := rows.Err(); rerr != nil {
		return "", rerr
	}
	if applied == 0 {
		return "", ErrNoChanges
	}
	var cid string
	if err := v.store.WriteTx(context.Background(), func(tx *sql.Tx) error {
		var e error
		cid, e = v.writeCommitInTx(tx, repoID, worktreeID, parentID, "", author, message, tree)
		if e != nil {
			return e
		}
		if _, e := tx.Exec("DELETE FROM vcs_uncommitted WHERE scope_type=? AND scope_id=?", scopeType, scopeID); e != nil {
			return e
		}
		if scopeType == "main" {
			if _, e := tx.Exec("UPDATE vcs_repos SET main_head=? WHERE id=?", cid, repoID); e != nil {
				return e
			}
		} else if scopeType == "worktree" {
			if _, e := tx.Exec("UPDATE vcs_worktrees SET tip=? WHERE id=?", cid, worktreeID); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		return "", err
	}
	return cid, nil
}

// CommitMain folds main's pending changeset into a new commit (serialized).
func (v *VCS) CommitMain(repoID, author, message string) (string, error) {
	unlock := v.lockRepo(repoID)
	defer unlock()
	return v.commitScope("main", repoID, repoID, "", author, message)
}

// CommitWorktree folds a worktree's pending changeset into a new commit
// (serialized per repo).
func (v *VCS) CommitWorktree(wtID, author, message string) (string, error) {
	wt, err := v.getWorktree(wtID)
	if err != nil {
		return "", err
	}
	unlock := v.lockRepo(wt.RepoID)
	defer unlock()
	return v.commitWorktreeLocked(wt.RepoID, wtID, author, message)
}

func (v *VCS) commitWorktreeLocked(
	repoID, wtID, author, message string,
) (string, error) {
	wt, err := v.getWorktree(wtID)
	if err != nil {
		return "", err
	}
	if wt.RepoID != repoID {
		return "", fmt.Errorf("vcs: worktree %s changed repository", wtID)
	}
	return v.commitScope("worktree", wtID, repoID, wtID, author, message)
}

// logFrom walks parent_id from head newest-first, up to limit (limit<=0 → all).
// A missing commit row ends the walk silently (treated as end-of-chain).
func (v *VCS) logFrom(head string, limit int) ([]Commit, error) {
	var out []Commit
	cur := head
	for cur != "" && (limit <= 0 || len(out) < limit) {
		c, err := v.getCommit(cur)
		if err != nil {
			return out, nil
		}
		c.FilesChanged = v.changedCount(c.ID, c.ParentID)
		out = append(out, c)
		cur = c.ParentID
	}
	return out, nil
}

// changedCount returns the number of paths a commit changed vs its parent
// (added + modified + deleted). With delta storage the commit's delta rows ARE
// exactly its changed paths, so this is just len(commitDelta) — no need to
// reconstruct two full trees per log entry. For a root commit (parentID empty)
// the delta is all 'add' rows = the total file count. parentID is retained for
// signature stability but is not needed (the stored delta is already vs the
// correct parent).
func (v *VCS) changedCount(commitID, parentID string) int {
	_ = parentID
	return len(v.commitDelta(commitID))
}

// LogMain returns the commit history of main (newest-first), up to limit entries
// (limit<=0 → all). Returns an error if the repo cannot be loaded.
func (v *VCS) LogMain(repoID string, limit int) ([]Commit, error) {
	r, err := v.getRepo(repoID)
	if err != nil {
		return nil, err
	}
	return v.logFrom(r.MainHead, limit)
}

// LogWorktree returns the commit history of a worktree branch (newest-first),
// up to limit entries (limit<=0 → all).
func (v *VCS) LogWorktree(wtID string, limit int) ([]Commit, error) {
	return v.logFrom(v.worktreeTip(wtID), limit)
}

// FileDiff describes a single path change between two trees.
type FileDiff struct {
	Path    string
	Op      string // added | modified | deleted
	OldHash string
	NewHash string
}

// Diff returns the path-level changes from refA's tree to refB's tree
// (path in B not A → added; in A not B → deleted; hash differs → modified).
// Sorted by path. repoID is accepted for symmetry with other VCS methods and
// for future authorization/audit hooks; it is not currently used to gate the
// lookup because commit ids are already content-derived and repo-scoped.
func (v *VCS) Diff(repoID, refA, refB string) ([]FileDiff, error) {
	a := v.commitTree(refA)
	b := v.commitTree(refB)
	seen := map[string]struct{}{}
	for p := range a {
		seen[p] = struct{}{}
	}
	for p := range b {
		seen[p] = struct{}{}
	}
	var out []FileDiff
	for p := range seen {
		ah, okA := a[p]
		bh, okB := b[p]
		switch {
		case !okA:
			out = append(out, FileDiff{Path: p, Op: "added", NewHash: bh})
		case !okB:
			out = append(out, FileDiff{Path: p, Op: "deleted", OldHash: ah})
		case ah != bh:
			out = append(out, FileDiff{Path: p, Op: "modified", OldHash: ah, NewHash: bh})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Restore writes the historical blob into an ACTIVE working copy and records
// the same bytes in that scope's pending changeset before releasing the repo
// lane (CB1). The public signature is unchanged; commit ownership identifies
// repoID and destDir must exactly match that repo's root or an active worktree.
func (v *VCS) Restore(commitID, path, destDir string) error {
	repoID, err := v.commitRepoID(commitID)
	if err != nil {
		return err
	}
	unlock := v.lockRepo(repoID)
	defer unlock()
	return v.restoreLocked(repoID, commitID, path, destDir)
}

func (v *VCS) commitRepoID(commitID string) (string, error) {
	var repoID string
	if err := v.store.DB.QueryRow(
		"SELECT repo_id FROM vcs_commits WHERE id=?", commitID,
	).Scan(&repoID); err != nil {
		return "", fmt.Errorf("vcs: resolve commit %s: %w", commitID, err)
	}
	return repoID, nil
}

func (v *VCS) restoreLocked(repoID, commitID, path, destDir string) error {
	// Re-read ownership while holding the repo lane: closes lookup→lock TOCTOU.
	lockedRepoID, err := v.commitRepoID(commitID)
	if err != nil {
		return err
	}
	if lockedRepoID != repoID {
		return fmt.Errorf("vcs: commit %s changed repository", commitID)
	}
	scopeType, scopeID, scopeRoot, err := v.restoreScopeLocked(repoID, destDir)
	if err != nil {
		return err
	}

	cleanRel := filepath.Clean(filepath.FromSlash(path))
	relSlash := filepath.ToSlash(cleanRel)
	if cleanRel == "." || filepath.IsAbs(cleanRel) || relSlash == ".." ||
		strings.HasPrefix(relSlash, "../") {
		return fmt.Errorf("vcs: unsafe restore path %q", path)
	}
	tree := v.commitTree(commitID)
	h, ok := tree[relSlash]
	if !ok {
		return fmt.Errorf("vcs: %s not in commit %s", relSlash, commitID)
	}
	content, err := v.getBlob(h)
	if err != nil {
		return fmt.Errorf("vcs: read restore blob %s: %w", h, err)
	}
	dest := filepath.Join(scopeRoot, cleanRel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	// Snapshot the destination so a recordEdit failure cannot leave an untracked
	// filesystem mutation. Blob insertion on a failed upsert is harmless dedup data.
	old, readErr := os.ReadFile(dest)
	existed := readErr == nil
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if err := os.WriteFile(dest, content, 0o644); err != nil {
		return err
	}
	if err := v.recordEdit(scopeType, scopeID, scopeRoot, dest, content); err != nil {
		var rollbackErr error
		if existed {
			rollbackErr = os.WriteFile(dest, old, 0o644)
		} else {
			rollbackErr = os.Remove(dest)
			if errors.Is(rollbackErr, os.ErrNotExist) {
				rollbackErr = nil
			}
		}
		return errors.Join(fmt.Errorf("vcs: track restored file: %w", err), rollbackErr)
	}
	return nil
}

func (v *VCS) restoreScopeLocked(repoID, destDir string) (string, string, string, error) {
	destAbs, err := filepath.Abs(destDir)
	if err != nil {
		return "", "", "", err
	}
	destAbs = filepath.Clean(destAbs)
	if runtime.GOOS == "windows" {
		// Canonicalize case to match the stored root_path identity (canonicalRepoRoot
		// lower-cases Windows roots). Without this, a TempDir-derived destDir with a
		// different casing than the repo's stored root_path would fail the equality
		// check even when pointing at the same directory.
		if real, rerr := filepath.EvalSymlinks(destAbs); rerr == nil {
			destAbs = strings.ToLower(filepath.Clean(real))
		} else {
			destAbs = strings.ToLower(destAbs)
		}
	}
	r, err := v.getRepo(repoID)
	if err != nil {
		return "", "", "", err
	}
	rootAbs := r.RootPath
	if runtime.GOOS == "windows" {
		rootAbs = strings.ToLower(rootAbs)
	}
	if destAbs == rootAbs {
		return "main", repoID, r.RootPath, nil
	}
	rows, err := v.store.DB.Query(
		"SELECT id, path FROM vcs_worktrees WHERE repo_id=? AND active=1", repoID)
	if err != nil {
		return "", "", "", err
	}
	defer rows.Close()
	for rows.Next() {
		var wtID, wtPath string
		if err := rows.Scan(&wtID, &wtPath); err != nil {
			return "", "", "", err
		}
		wtAbs := wtPath
		if runtime.GOOS == "windows" {
			wtAbs = strings.ToLower(wtAbs)
		}
		if destAbs == wtAbs {
			return "worktree", wtID, wtPath, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", "", "", err
	}
	return "", "", "", fmt.Errorf(
		"vcs: restore destination %s is not an active working copy for repo %s",
		destAbs, repoID)
}

// MergeToMain integrates a worktree's tip into main via a tree-level 3-way merge
// (serialized per repo).
// base = worktree's base_commit; ours = main_head; theirs = worktree tip.
// Returns (mergeCommitID, conflicts, err). On conflict without force: no commit,
// err == ErrConflicts, conflicts lists the both-side-modified paths. With force:
// theirs wins on conflicted paths and the merge proceeds.
//
// Fast-forward: when main_head == worktree.base_commit (main hasn't moved since
// the branch), ours==base, so every change is unambiguously theirs and there can
// be no conflict — the merged tree is exactly theirs.
func (v *VCS) MergeToMain(wtID, author string, force bool) (string, []string, error) {
	wt, err := v.getWorktree(wtID)
	if err != nil {
		return "", nil, err
	}
	unlock := v.lockRepo(wt.RepoID)
	defer unlock()
	return v.mergeToMainLocked(wt.RepoID, wtID, author, force)
}

func (v *VCS) mergeToMainLocked(
	repoID, wtID, author string, force bool,
) (string, []string, error) {
	wt, err := v.getWorktree(wtID)
	if err != nil {
		return "", nil, err
	}
	if wt.RepoID != repoID {
		return "", nil, fmt.Errorf("vcs: worktree %s changed repository", wtID)
	}
	r, err := v.getRepo(repoID)
	if err != nil {
		return "", nil, err
	}
	base := v.commitTree(wt.BaseCommit)
	ours := v.commitTree(r.MainHead)
	theirs := v.commitTree(v.worktreeTip(wtID))

	merged := map[string]string{}
	paths := map[string]struct{}{}
	for _, tree := range []map[string]string{base, ours, theirs} {
		for path := range tree {
			paths[path] = struct{}{}
		}
	}
	var conflicts []string
	for path := range paths {
		baseHash := base[path]
		oursHash := ours[path]
		theirsHash := theirs[path]
		switch {
		case oursHash == theirsHash:
			if oursHash != "" {
				merged[path] = oursHash
			}
		case oursHash == baseHash:
			if theirsHash != "" {
				merged[path] = theirsHash
			}
		case theirsHash == baseHash:
			if oursHash != "" {
				merged[path] = oursHash
			}
		default:
			conflicts = append(conflicts, path)
			if force {
				if theirsHash != "" {
					merged[path] = theirsHash
				}
			} else if oursHash != "" {
				merged[path] = oursHash
			}
		}
	}
	sort.Strings(conflicts)
	if len(conflicts) > 0 && !force {
		return "", conflicts, ErrConflicts
	}

	var cid string
	if err := v.store.WriteTx(context.Background(), func(tx *sql.Tx) error {
		var e error
		cid, e = v.writeCommitInTx(
			tx, r.ID, "", r.MainHead, wtID, author,
			"merge worktree "+wtID, merged,
		)
		if e != nil {
			return e
		}
		if _, e := tx.Exec(
			"UPDATE vcs_repos SET main_head=? WHERE id=?", cid, r.ID,
		); e != nil {
			return e
		}
		return nil
	}); err != nil {
		return "", conflicts, err
	}
	return cid, conflicts, nil
}
