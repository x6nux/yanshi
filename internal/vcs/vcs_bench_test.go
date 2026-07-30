package vcs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/x6nux/yanshi/internal/store"
)

// BenchmarkVCSCommit measures CommitMain throughput on small and large working
// trees. Because CommitMain rejects a no-op tree, each iteration adds ONE new
// file (excluded from the timer) and then commits — so the committed path
// measures real commit-object + tree-snapshot cost, not a rejected short-circuit.
// Zero external deps: an in-memory store and a temp dir.
func BenchmarkVCSCommit(b *testing.B) {
	subs := []struct {
		name string
		n    int
	}{
		{name: "SmallTree", n: 10},
		{name: "LargeTree", n: 1000},
	}
	for _, sub := range subs {
		b.Run(sub.name, func(b *testing.B) {
			s, err := store.Open(":memory:")
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			v := New(s, b.TempDir())
			root := b.TempDir()
			repoID, err := v.InitRepo(root)
			if err != nil {
				b.Fatal(err)
			}

			// Pre-populate with N files (excluded from the timer).
			for i := 0; i < sub.n; i++ {
				path := filepath.Join(root, fmt.Sprintf("file_%d.go", i))
				if err := os.WriteFile(path,
					[]byte(fmt.Sprintf("package main\n\nfunc f%d() {}\n", i)), 0o644); err != nil {
					b.Fatal(err)
				}
				if err := v.RecordEditMain(repoID, "bench", path,
					[]byte(fmt.Sprintf("package main\n\nfunc f%d() {}\n", i))); err != nil {
					b.Fatal(err)
				}
			}
			// One initial commit so the pre-populated tree exists in history.
			if _, err := v.CommitMain(repoID, "bench", "seed"); err != nil {
				b.Fatal(err)
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Add one new file each iteration so CommitMain has a real
				// tree delta to snapshot (excluded from the timer).
				b.StopTimer()
				path := filepath.Join(root, fmt.Sprintf("iter_%d.go", i))
				content := []byte(fmt.Sprintf("package main\n\nfunc g%d() {}\n", i))
				if err := os.WriteFile(path, content, 0o644); err != nil {
					b.Fatal(err)
				}
				if err := v.RecordEditMain(repoID, "bench", path, content); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()

				if _, err := v.CommitMain(repoID, "bench", "bench commit"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkDAGApply measures MergeToMain throughput. Two sub-cases exercise
// distinct DAG paths: ModifyFile (a worktree changes an existing file → a real
// three-way merge) and AddFile (a worktree adds a new file → a clean
// fast-forward-ish merge). Because MergeToMain mutates main's head, each
// iteration rebuilds fresh state with the timer stopped.
func BenchmarkDAGApply(b *testing.B) {
	subs := []struct {
		name      string
		modifyFGo bool
	}{
		{name: "AddFile", modifyFGo: false},
		{name: "ModifyFile", modifyFGo: true},
	}
	for _, sub := range subs {
		b.Run(sub.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				s, err := store.Open(":memory:")
				if err != nil {
					b.Fatal(err)
				}
				v := New(s, b.TempDir())
				root := b.TempDir()
				repoID, err := v.InitRepo(root)
				if err != nil {
					b.Fatal(err)
				}
				// base commit with a shared file.
				if err := os.WriteFile(filepath.Join(root, "f.go"),
					[]byte("package main\n\nconst X = 1\n"), 0o644); err != nil {
					b.Fatal(err)
				}
				if err := v.RecordEditMain(repoID, "bench", filepath.Join(root, "f.go"),
					[]byte("package main\n\nconst X = 1\n")); err != nil {
					b.Fatal(err)
				}
				if _, err := v.CommitMain(repoID, "bench", "base"); err != nil {
					b.Fatal(err)
				}

				// ours worktree diverges from base.
				wtO, err := v.AddWorktree(repoID, []string{"bench"})
				if err != nil {
					b.Fatal(err)
				}
				if sub.modifyFGo {
					if err := os.WriteFile(filepath.Join(wtO.Path, "f.go"),
						[]byte("package main\n\nconst X = 2\n"), 0o644); err != nil {
						b.Fatal(err)
					}
					if err := v.RecordEditWorktree(wtO.ID, "bench", filepath.Join(wtO.Path, "f.go"),
						[]byte("package main\n\nconst X = 2\n")); err != nil {
						b.Fatal(err)
					}
				} else {
					if err := os.WriteFile(filepath.Join(wtO.Path, "g.go"),
						[]byte("package main\n\nconst Y = 4\n"), 0o644); err != nil {
						b.Fatal(err)
					}
					if err := v.RecordEditWorktree(wtO.ID, "bench", filepath.Join(wtO.Path, "g.go"),
						[]byte("package main\n\nconst Y = 4\n")); err != nil {
						b.Fatal(err)
					}
				}
				if _, err := v.CommitWorktree(wtO.ID, "bench", "ours"); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()

				// Merge ours → main. ModifyFile exercises the file-level
				// three-way merge (both sides touched f.go from the base);
				// AddFile is a clean merge (no overlapping paths).
				if _, _, err := v.MergeToMain(wtO.ID, "bench", false); err != nil {
					b.Fatal(err)
				}
				s.Close()
			}
		})
	}
}
