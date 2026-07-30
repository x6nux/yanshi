package vcs

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/store"
)

// newSeamRaceRepo builds a VCS + repo + initial commit for race tests.
func newSeamRaceRepo(t *testing.T) (*VCS, string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	st, err := store.Open(filepath.Join(base, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	v := New(st, filepath.Join(base, "worktrees"))
	repoID, err := v.InitRepo(root)
	if err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	seedPath := filepath.Join(root, "counter.txt")
	if err := os.WriteFile(seedPath, []byte("0"), 0o644); err != nil {
		t.Fatalf("seed WriteFile: %v", err)
	}
	if err := v.RecordEditMain(repoID, "test", seedPath, []byte("0")); err != nil {
		t.Fatalf("seed RecordEditMain: %v", err)
	}
	if _, err := v.CommitMain(repoID, "test", "seed"); err != nil {
		t.Fatalf("seed CommitMain: %v", err)
	}
	return v, repoID, root
}

// publicRepoWriters is the lock-coverage contract. Tasks 3/4/5 append their new
// writers before their GREEN run: SealMainTurnSeam, MaterializeMain,
// ResetMainHead, RevertToSeam.
var publicRepoWriters = []string{
	"InitRepo", "AddWorktree", "RemoveWorktree", "Restore",
	"RecordEditMain", "RecordEditWorktree",
	"CommitMain", "CommitWorktree", "MergeToMain",
	"SealMainTurnSeam",
	"MaterializeMain", "ResetMainHead",
	"RevertToSeam",
}

// TestPublicRepoWritersAcquireRepoLane parses production files and requires every
// public writer wrapper to call lockRepo. This deterministically catches an
// omitted lane even when a timing-dependent race does not reproduce.
func TestPublicRepoWritersAcquireRepoLane(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	dir := filepath.Dir(thisFile)
	pkgs, err := parser.ParseDir(token.NewFileSet(), dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("ParseDir: %v", err)
	}
	for _, name := range publicRepoWriters {
		found, locks := false, false
		for _, pkg := range pkgs {
			for _, file := range pkg.Files {
				for _, decl := range file.Decls {
					fn, ok := decl.(*ast.FuncDecl)
					if !ok || fn.Recv == nil || fn.Name.Name != name {
						continue
					}
					found = true
					ast.Inspect(fn.Body, func(n ast.Node) bool {
						call, ok := n.(*ast.CallExpr)
						if !ok {
							return true
						}
						sel, ok := call.Fun.(*ast.SelectorExpr)
						if ok && sel.Sel.Name == "lockRepo" {
							locks = true
						}
						return true
					})
				}
			}
		}
		if !found {
			t.Errorf("public writer %s not found", name)
		} else if !locks {
			t.Errorf("public writer %s does not acquire lockRepo", name)
		}
	}
}

// TestRepoMu_ConcurrentRecordEditMain drives the same public writer from many
// goroutines. Each path is unique, so no goroutine's later CommitMain can consume
// another goroutine's pending set; one final commit verifies all edits survived.
func TestRepoMu_ConcurrentRecordEditMain(t *testing.T) {
	v, repoID, root := newSeamRaceRepo(t)
	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			path := filepath.Join(root, fmt.Sprintf("counter-%02d.txt", i))
			content := []byte(fmt.Sprintf("g%d", i))
			if err := v.RecordEditMain(repoID, "race", path, content); err != nil {
				t.Errorf("RecordEditMain[%d]: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if _, err := v.CommitMain(repoID, "race", "all concurrent edits"); err != nil {
		t.Fatalf("final CommitMain: %v", err)
	}
}

// TestRepoMu_DifferentReposProgressIndependently holds repo A's lane directly
// and proves a public writer for repo B can still complete. A single global
// mutex would time out here (CB1).
func TestRepoMu_DifferentReposProgressIndependently(t *testing.T) {
	v, repoA, rootA := newSeamRaceRepo(t)
	rootB := filepath.Join(filepath.Dir(rootA), "repo-b")
	if err := os.MkdirAll(rootB, 0o755); err != nil {
		t.Fatalf("MkdirAll repo-b: %v", err)
	}
	repoB, err := v.InitRepo(rootB)
	if err != nil {
		t.Fatalf("InitRepo repo-b: %v", err)
	}

	unlockA := v.lockRepo(repoA)
	done := make(chan error, 1)
	go func() {
		done <- v.RecordEditMain(repoB, "race",
			filepath.Join(rootB, "independent.txt"), []byte("b"))
	}()
	select {
	case err := <-done:
		unlockA()
		if err != nil {
			t.Fatalf("repo B writer: %v", err)
		}
	case <-time.After(2 * time.Second):
		unlockA()
		t.Fatal("repo B writer blocked behind unrelated repo A lane")
	}
}

// TestInitRepo_UsesCanonicalRootAsStoredIdentity locks the alias contract:
// canonicalization is not merely a mutex key; the canonical root is also the
// root_path queried and persisted, so aliases cannot create distinct repo rows.
func TestInitRepo_UsesCanonicalRootAsStoredIdentity(t *testing.T) {
	v, repoID, root := newSeamRaceRepo(t)
	alias := filepath.Join(root, ".")
	canonical, err := canonicalRepoRoot(alias)
	if err != nil {
		t.Fatalf("canonicalRepoRoot: %v", err)
	}
	gotID, err := v.InitRepo(alias)
	if err != nil {
		t.Fatalf("InitRepo alias: %v", err)
	}
	if gotID != repoID {
		t.Fatalf("InitRepo(alias) id = %s, want existing %s", gotID, repoID)
	}
	var stored string
	if err := v.store.DB.QueryRow(
		"SELECT root_path FROM vcs_repos WHERE id=?", repoID,
	).Scan(&stored); err != nil {
		t.Fatalf("query root_path: %v", err)
	}
	if stored != canonical {
		t.Fatalf("stored root_path = %q, want canonical %q", stored, canonical)
	}
}

// TestRepoMu_ConcurrentInitDifferentRepos drives separate init lanes while both
// roots contribute .yanshiignore patterns. With -race, this also locks the
// shared ignore slice's copy-on-read/append synchronization.
func TestRepoMu_ConcurrentInitDifferentRepos(t *testing.T) {
	base := t.TempDir()
	st, err := store.Open(filepath.Join(base, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	v := New(st, filepath.Join(base, "worktrees"))

	roots := []string{filepath.Join(base, "repo-a"), filepath.Join(base, "repo-b")}
	for i, root := range roots {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatalf("MkdirAll[%d]: %v", i, err)
		}
		if err := os.WriteFile(filepath.Join(root, ".yanshiignore"),
			[]byte(fmt.Sprintf("private-%d\n", i)), 0o644); err != nil {
			t.Fatalf("WriteFile .yanshiignore[%d]: %v", i, err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(roots))
	for _, root := range roots {
		root := root
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := v.InitRepo(root)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent InitRepo: %v", err)
		}
	}
}

// TestRestore_TracksMainWrite proves Restore's os.WriteFile and recordEdit are
// one serialized operation: restored bytes become a pending main edit (CB1).
func TestRestore_TracksMainWrite(t *testing.T) {
	v, repoID, root := newSeamRaceRepo(t)
	path := filepath.Join(root, "counter.txt")
	oldHead, err := v.RepoMainHead(repoID)
	if err != nil {
		t.Fatalf("RepoMainHead: %v", err)
	}
	if err := os.WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := v.RecordEditMain(repoID, "test", path, []byte("new")); err != nil {
		t.Fatalf("RecordEditMain: %v", err)
	}
	if _, err := v.CommitMain(repoID, "test", "new version"); err != nil {
		t.Fatalf("CommitMain: %v", err)
	}

	if err := v.Restore(oldHead, "counter.txt", root); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "0" {
		t.Fatalf("restored bytes = %q, err=%v; want 0", got, err)
	}
	pending := v.Uncommitted("main", repoID)
	if pending["counter.txt"] != hashContent([]byte("0")) {
		t.Fatalf("pending restore hash = %q, want restored blob hash", pending["counter.txt"])
	}
}

// TestRestore_TracksWorktreeWrite proves destDir resolution selects the active
// worktree scope rather than accidentally recording the edit on main.
func TestRestore_TracksWorktreeWrite(t *testing.T) {
	v, repoID, _ := newSeamRaceRepo(t)
	head, err := v.RepoMainHead(repoID)
	if err != nil {
		t.Fatalf("RepoMainHead: %v", err)
	}
	wt, err := v.AddWorktree(repoID, []string{"agent-a"})
	if err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	path := filepath.Join(wt.Path, "counter.txt")
	if err := os.WriteFile(path, []byte("dirty"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := v.Restore(head, "counter.txt", wt.Path); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := v.Uncommitted("worktree", wt.ID)["counter.txt"]; got != hashContent([]byte("0")) {
		t.Fatalf("worktree pending hash = %q, want restored blob hash", got)
	}
	if _, ok := v.Uncommitted("main", repoID)["counter.txt"]; ok {
		t.Fatal("worktree restore must not create a main pending edit")
	}
}

// TestRepoMu_ConcurrentMixedWrites drives RecordEditMain + SealMainTurnSeam
// concurrently to prove the seam writer's "flush pending → read head → insert"
// sequence is not raced by concurrent edits (Task 5 appends a RevertToSeam call).
func TestRepoMu_ConcurrentMixedWrites(t *testing.T) {
	v, repoID, root := newSeamRaceRepo(t)
	preID, err := v.SealMainTurnSeam(repoID, "race-session", 0, 0, SeamPreTurn, "pre-race")
	if err != nil {
		t.Fatalf("SealMainTurnSeam: %v", err)
	}
	const goroutines = 8
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*2)
	wg.Add(goroutines * 2)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			path := filepath.Join(root, fmt.Sprintf("mixed-%02d.txt", i))
			if err := v.RecordEditMain(repoID, "race", path,
				[]byte(fmt.Sprintf("m%d", i))); err != nil {
				errs <- fmt.Errorf("RecordEditMain[%d]: %w", i, err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			if _, err := v.SealMainTurnSeam(repoID, "race-session", i+1, i+1,
				SeamPostTurn, fmt.Sprintf("post-%d", i)); err != nil {
				errs <- fmt.Errorf("SealMainTurnSeam[%d]: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	// Task 5: prove RevertToSeam is on the same lane as the writers above.
	if _, err := v.RevertToSeam(repoID, preID, "race undo", 0, 0, nil); err != nil {
		t.Errorf("RevertToSeam after concurrent writes: %v", err)
	}
}
