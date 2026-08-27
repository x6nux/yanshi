package vcs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestFreeze_RefusesEveryCooperativeWriter is the V5 table: while a repo is
// frozen, every VCS write entry point must FAIL rather than queue.
//
// "Fail rather than queue" is the whole claim. A writer that blocks on the lane
// resumes after the rollback still holding content it read from the tree the
// rollback just replaced, and commits it — reinstating exactly what the
// operator asked to discard. The test therefore asserts the error, and each
// case additionally runs with a timeout so a regression to blocking shows up as
// a hang caught here rather than in production.
func TestFreeze_RefusesEveryCooperativeWriter(t *testing.T) {
	tests := []struct {
		name string
		call func(v *VCS, repoID, root string) error
	}{
		{
			name: "RecordEditMain",
			call: func(v *VCS, repoID, root string) error {
				return v.RecordEditMain(repoID, "u", filepath.Join(root, "a.txt"), []byte("x"))
			},
		},
		{
			name: "RecordDeleteMain",
			call: func(v *VCS, repoID, root string) error {
				return v.RecordDeleteMain(repoID, "u", filepath.Join(root, "a.txt"))
			},
		},
		{
			name: "CommitMain",
			call: func(v *VCS, repoID, root string) error {
				_, err := v.CommitMain(repoID, "u", "should not happen")
				return err
			},
		},
		{
			name: "RunGC",
			call: func(v *VCS, repoID, root string) error {
				_, err := v.RunGC(repoID, GCOptions{DryRun: true})
				return err
			},
		},
		{
			name: "CleanupOrphanWorktree",
			call: func(v *VCS, repoID, root string) error {
				return v.CleanupOrphanWorktree(repoID, "any")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, repoID, root := setupSeamTestRepo(t)
			thaw, err := v.freezeWorkingCopy(repoID)
			if err != nil {
				t.Fatalf("freeze: %v", err)
			}
			defer thaw()

			done := make(chan error, 1)
			go func() { done <- tc.call(v, repoID, root) }()
			select {
			case err := <-done:
				if !errors.Is(err, ErrWorkingCopyFrozen) {
					t.Fatalf("err = %v, want ErrWorkingCopyFrozen", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("writer blocked on the repo lane instead of failing fast; " +
					"a blocked writer resumes with pre-rollback content and commits it")
			}
		})
	}
}

// TestFreeze_ThawRestoresNormalOperation proves the freeze is not a one-way
// door: once the restore finishes, the same writers work again.
func TestFreeze_ThawRestoresNormalOperation(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	thaw, err := v.freezeWorkingCopy(repoID)
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if !v.WorkingCopyFrozen(repoID) {
		t.Error("WorkingCopyFrozen must report a frozen repo")
	}
	thaw()
	if v.WorkingCopyFrozen(repoID) {
		t.Error("WorkingCopyFrozen must report a thawed repo as unfrozen")
	}
	abs := filepath.Join(root, "after.txt")
	if err := os.WriteFile(abs, []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := v.RecordEditMain(repoID, "u", abs, []byte("ok\n")); err != nil {
		t.Fatalf("write after thaw: %v", err)
	}
}

// TestFreeze_DoesNotNest pins the deliberate refusal to refcount: two concurrent
// rollbacks of one working copy is not a state this package can make coherent.
func TestFreeze_DoesNotNest(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	thaw, err := v.freezeWorkingCopy(repoID)
	if err != nil {
		t.Fatalf("first freeze: %v", err)
	}
	defer thaw()
	if _, err := v.freezeWorkingCopy(repoID); !errors.Is(err, ErrWorkingCopyFrozen) {
		t.Fatalf("second freeze err = %v, want ErrWorkingCopyFrozen", err)
	}
}

// TestFreeze_OtherReposAreUnaffected pins that the freeze is per-repo, not
// global: one project's rollback must not stall another's.
func TestFreeze_OtherReposAreUnaffected(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	other := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	otherID, err := v.InitRepo(other)
	if err != nil {
		t.Fatalf("InitRepo other: %v", err)
	}
	thaw, err := v.freezeWorkingCopy(repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer thaw()

	abs := filepath.Join(other, "f.txt")
	if err := os.WriteFile(abs, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := v.RecordEditMain(otherID, "u", abs, []byte("x\n")); err != nil {
		t.Fatalf("unrelated repo was blocked by another repo's freeze: %v", err)
	}
}

// TestApplyRestore_DetectsAnExternalWriteBetweenPreviewAndApply is the honest
// half of V5: the freeze cannot stop a subprocess, so an apply must NOTICE that
// one moved rather than silently overwrite it.
//
// The external write is simulated with a plain os.WriteFile, which is exactly
// what a compiler or a background worker does — it does not go through this
// package and nothing in this package was consulted.
func TestApplyRestore_DetectsAnExternalWriteBetweenPreviewAndApply(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	target := commitWith(t, v, repoID, root, "target", map[string]string{"f.txt": "v1\n"})
	commitWith(t, v, repoID, root, "advance", map[string]string{"f.txt": "v2\n"})

	plan, err := v.PlanRestore(repoID, target, nil)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}
	// Something outside the VCS writes the same file after the preview.
	const external = "WRITTEN BY A SUBPROCESS AFTER THE PREVIEW\n"
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte(external), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = v.ApplyRestore(repoID, target, nil, plan.ConfirmToken)
	if !errors.Is(err, ErrExternalMutation) {
		t.Fatalf("err = %v, want ErrExternalMutation", err)
	}
	if !strings.Contains(err.Error(), "f.txt") {
		t.Errorf("the error must name the path that moved; got %v", err)
	}
	// Detection means the operator gets to look, so the external content must
	// still be there — overwriting it and THEN reporting would be worse than
	// not checking at all.
	mustFile(t, filepath.Join(root, "f.txt"), external)
}

// TestApplyRestore_CleanWorkingCopyPassesTheFingerprintCheck is the negative
// control for the test above: without an external writer the same code path
// must succeed. Without this, a fingerprint comparison that rejected
// EVERYTHING would look like a working detector.
func TestApplyRestore_CleanWorkingCopyPassesTheFingerprintCheck(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	target := commitWith(t, v, repoID, root, "target", map[string]string{
		"a.go": "package a // v1\n",
		"b.go": "package b // v1\n",
	})
	commitWith(t, v, repoID, root, "advance", map[string]string{
		"a.go": "package a // v2\n",
		"b.go": "package b // v2\n",
	})
	plan, err := v.PlanRestore(repoID, target, nil)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}
	if _, err := v.ApplyRestore(repoID, target, nil, plan.ConfirmToken); err != nil {
		t.Fatalf("clean apply must succeed, got %v", err)
	}
	mustFile(t, filepath.Join(root, "a.go"), "package a // v1\n")
	mustFile(t, filepath.Join(root, "b.go"), "package b // v1\n")
}

// TestFingerprintPaths_DistinguishesAbsentFromEmpty pins the one encoding
// choice a size- or mtime-based fingerprint would get wrong.
func TestFingerprintPaths_DistinguishesAbsentFromEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "content.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := openWorkRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	fp, err := fingerprintPaths(root, []string{"empty.txt", "content.txt", "absent.txt"})
	if err != nil {
		t.Fatalf("fingerprintPaths: %v", err)
	}
	if fp["absent.txt"] != "" {
		t.Errorf("an absent path must record the empty string, got %q", fp["absent.txt"])
	}
	if fp["empty.txt"] == "" {
		t.Error("an EMPTY file must not record the same value as an ABSENT one")
	}
	if fp["empty.txt"] == fp["content.txt"] {
		t.Error("two files with different content share a fingerprint")
	}
}

// TestDiffFingerprints reports drift in both directions.
func TestDiffFingerprints(t *testing.T) {
	tests := []struct {
		name      string
		want, got fingerprint
		expect    []string
	}{
		{name: "identical", want: fingerprint{"a": "1"}, got: fingerprint{"a": "1"}},
		{name: "content changed", want: fingerprint{"a": "1"}, got: fingerprint{"a": "2"},
			expect: []string{"a"}},
		{name: "deleted underneath", want: fingerprint{"a": "1"}, got: fingerprint{"a": ""},
			expect: []string{"a"}},
		{name: "created underneath", want: fingerprint{"a": ""}, got: fingerprint{"a": "1"},
			expect: []string{"a"}},
		{name: "sorted output", want: fingerprint{"b": "1", "a": "1"},
			got: fingerprint{"b": "2", "a": "2"}, expect: []string{"a", "b"}},
		{name: "unexpected extra path", want: fingerprint{}, got: fingerprint{"z": "1"},
			expect: []string{"z"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := diffFingerprints(tc.want, tc.got)
			if strings.Join(got, ",") != strings.Join(tc.expect, ",") {
				t.Errorf("got %v, want %v", got, tc.expect)
			}
		})
	}
}

// TestFreeze_ConcurrentCheckAndFreezeIsRaceFree drives the freeze index from
// many goroutines. It exists for `go test -race`: freezeMu guards a plain map,
// and a missed lock is a fatal concurrent map access rather than a wrong answer.
func TestFreeze_ConcurrentCheckAndFreezeIsRaceFree(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if thaw, err := v.freezeWorkingCopy(repoID); err == nil {
					thaw()
				}
				_ = v.WorkingCopyFrozen(repoID)
				_ = v.checkNotFrozen(repoID)
				_ = v.freezeSnapshot()
			}
		}()
	}
	wg.Wait()
	if v.WorkingCopyFrozen(repoID) {
		t.Error("every freeze was thawed, but the repo still reads frozen")
	}
}

// TestApplyRestore_DetectsAnExternalWriteDURINGTheExpansion is the other half
// of the honest-detection claim, and the harder one.
//
// The preview→apply check (above) only covers a writer that moved BEFORE the
// operation started. This covers the window the freeze flag most obviously
// cannot close: a subprocess writing while the tree is half-expanded. The hook
// stands in for that subprocess so the timing is deterministic — see
// restoreHook's doc comment for why a real racing goroutine would be a worse
// test, not a better one.
//
// The compensation assertion is the part that matters. Detecting the race and
// then leaving a half-restored tree behind would be no better than not
// checking: the operator would be told something went wrong and handed a
// working copy that is neither the old state nor the new one.
func TestApplyRestore_DetectsAnExternalWriteDURINGTheExpansion(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	target := commitWith(t, v, repoID, root, "target", map[string]string{
		"first.txt":  "target first\n",
		"second.txt": "target second\n",
	})
	commitWith(t, v, repoID, root, "advance", map[string]string{
		"first.txt":  "current first\n",
		"second.txt": "current second\n",
	})

	plan, err := v.PlanRestore(repoID, target, nil)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}
	unlock := v.lockRepo(repoID)
	defer unlock()

	const external = "A SUBPROCESS WROTE THIS MID-RESTORE\n"
	err = v.applyRestorePlanLockedWithHook(plan, false, func(path string) error {
		// Clobber an ALREADY-written path, the way a compiler emitting output
		// into a half-restored tree would.
		if path == "second.txt" {
			return os.WriteFile(filepath.Join(root, "first.txt"), []byte(external), 0o644)
		}
		return nil
	})
	if !errors.Is(err, ErrExternalMutation) {
		t.Fatalf("err = %v, want ErrExternalMutation", err)
	}
	if !strings.Contains(err.Error(), "during apply") {
		t.Errorf("the error must say WHEN the drift was seen; got %v", err)
	}
	if !strings.Contains(err.Error(), "first.txt") {
		t.Errorf("the error must name the clobbered path; got %v", err)
	}
	// Compensated: neither file may be left holding restored content.
	mustFile(t, filepath.Join(root, "second.txt"), "current second\n")
	if data, rerr := os.ReadFile(filepath.Join(root, "first.txt")); rerr != nil {
		t.Fatalf("read first.txt: %v", rerr)
	} else if string(data) == "target first\n" {
		t.Error("a detected mid-apply race left a half-restored tree behind")
	}
}
