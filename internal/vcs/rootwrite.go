// internal/vcs/rootwrite.go
//
// V6: symlink-proof working-copy writes.
//
// Every VCS operation that expands stored history back onto disk (Restore,
// MaterializeMain, RevertToSeam's snapshot/compensation, AddWorktree) used to
// build an absolute path with filepath.Join and hand it to os.WriteFile. Join
// is purely lexical: it collapses "..", but it knows nothing about what the
// path components ARE on disk. A tree path that is perfectly legal after
// validateRelPath — "docs/notes.md", no "..", no leading slash — still writes
// wherever the working copy's "docs" directory happens to POINT.
//
// That precondition is agent-reachable without any VCS bug: shell_run can
// `ln -s /etc "$REPO/docs"`, and the next restore of "docs/notes.md" follows
// it out of the repo. The lexical validation in validateRelPath and
// restoreLocked is necessary but was the ONLY check, so it was also the whole
// defence.
//
// # Why os.Root and not internal/pathjail
//
// pathjail.WithinRootAbs is the repo's canonical root-jail check, and it does
// resolve symlinks — but it cannot be the mechanism here, for two independent
// reasons:
//
//  1. It requires the candidate to EXIST (its doc comment says so: EvalSymlinks
//     stats). Restore and materialize create files that are not there yet, so
//     the check would fail on exactly the writes it is meant to guard.
//  2. It is check-then-use. WithinRootAbs resolves a path and returns a string;
//     the caller then writes through that string on a later syscall. Between
//     the two, the attacker who can create the symlink can also swap it —
//     classic TOCTOU. Validating harder does not close that window; only doing
//     the resolution and the write in ONE syscall does.
//
// os.Root (stdlib, Go 1.24+) does exactly that: every method resolves relative
// to a retained directory handle via openat/statat, and refuses any component
// that leaves the root — a symlink escape fails inside the kernel call, not in
// a Go-side prediction of what the kernel would do. It also carries the
// Windows and case-folding behaviour the hand-rolled checks in this package
// approximate.
//
// Governance note: internal/vcs is a port package, and its GOV1 allowlist
// (archtest deps_test.go portAllowlists) does not include internal/pathjail.
// Reaching for the stdlib is therefore also the option that does not require
// widening a port's dependency surface.
//
// # Belt and braces: in-root symlinks are refused too
//
// os.Root permits symlinks that stay INSIDE the root — following one is not an
// escape. This package refuses them anyway (ensureNoSymlink). A tracked tree
// entry always denotes a REGULAR file (snapshotWorkingFiles already rejects
// anything else with "not a regular file"), so a symlink standing where a
// tracked path should be is never a state this VCS produced. Writing through
// it would silently redirect a restore onto a different tracked file — still
// inside the jail, still wrong. Refusing costs nothing and keeps "the bytes
// land at the path the tree names" true.

package vcs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// errSymlinkComponent reports a working-copy path whose own component is a
// symlink. Callers surface it; it is never retried.
var errSymlinkComponent = errors.New("vcs: refusing to write through a symlink")

// openWorkRoot opens a confined handle on an ACTIVE working copy (a repo root
// or a materialized worktree dir). Every subsequent write goes through the
// returned *os.Root, so a component swapped to a symlink after this call still
// cannot redirect the write: resolution happens per syscall against the
// retained directory handle, not against the string.
//
// The caller must Close the root. Closing releases the descriptor only; it
// never touches the directory contents.
func openWorkRoot(dir string) (*os.Root, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("vcs: open work root %s: %w", dir, err)
	}
	return root, nil
}

// rootRel converts a repo-relative tree path (always slash-separated) into the
// form os.Root expects on this platform.
func rootRel(rel string) string {
	return filepath.FromSlash(rel)
}

// ensureNoSymlink walks every component of rel from the root down and refuses
// the write if any of them is a symlink — including the leaf itself.
//
// A component that does not exist yet is fine: it is about to be created as a
// real directory or file, and creation through os.Root cannot produce a
// symlink. A component that escapes the root fails here as well, because
// os.Root's Lstat reports "path escapes from parent" rather than a mode.
//
// This is the per-segment Lstat sweep, and it runs immediately before the
// write rather than at plan time — but note that it is NOT what makes the
// write safe. os.Root is. This check exists to reject in-root symlink
// redirection (see the file header), which os.Root allows by design; the
// unavoidable gap between this check and the write is therefore not a
// security window, because the write itself is independently confined.
func ensureNoSymlink(root *os.Root, rel string) error {
	rel = filepath.ToSlash(rel)
	segments := strings.Split(rel, "/")
	for i := range segments {
		prefix := rootRel(strings.Join(segments[:i+1], "/"))
		if prefix == "" || prefix == "." {
			continue
		}
		info, err := root.Lstat(prefix)
		if errors.Is(err, fs.ErrNotExist) {
			// Not created yet — nothing can be pointing anywhere.
			return nil
		}
		if err != nil {
			return fmt.Errorf("vcs: inspect %s: %w", prefix, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s (component %q)", errSymlinkComponent, rel, prefix)
		}
	}
	return nil
}

// rootMkdirAll creates rel's PARENT directories inside root, refusing to
// traverse a symlinked component.
func rootMkdirAll(root *os.Root, rel string) error {
	parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(rel)))
	if parent == "." || parent == "" || parent == "/" {
		return nil
	}
	if err := ensureNoSymlink(root, parent); err != nil {
		return err
	}
	if err := root.MkdirAll(rootRel(parent), 0o755); err != nil {
		return fmt.Errorf("vcs: mkdir %s: %w", parent, err)
	}
	return nil
}

// rootWriteFile writes data to rel inside root, creating parent directories.
// It is the confined replacement for os.WriteFile on a working copy.
func rootWriteFile(root *os.Root, rel string, data []byte, mode os.FileMode) error {
	if err := rootMkdirAll(root, rel); err != nil {
		return err
	}
	if err := ensureNoSymlink(root, rel); err != nil {
		return err
	}
	if err := root.WriteFile(rootRel(rel), data, mode); err != nil {
		return fmt.Errorf("vcs: write %s: %w", rel, err)
	}
	return nil
}

// rootReadFile reads rel from inside root. A missing file surfaces as an error
// satisfying errors.Is(err, fs.ErrNotExist), so existing callers keep their
// "did it exist?" branch.
func rootReadFile(root *os.Root, rel string) ([]byte, error) {
	if err := ensureNoSymlink(root, rel); err != nil {
		return nil, err
	}
	return root.ReadFile(rootRel(rel))
}

// rootRemove deletes rel from inside root. A missing file is reported with
// fs.ErrNotExist so callers can keep treating it as success.
func rootRemove(root *os.Root, rel string) error {
	if err := ensureNoSymlink(root, rel); err != nil {
		return err
	}
	return root.Remove(rootRel(rel))
}

// rootLstat stats rel WITHOUT following a final symlink, so the caller sees the
// link itself rather than its target. snapshotWorkingFiles relies on this to
// classify a symlink as a non-regular file instead of silently snapshotting
// (and later rewriting) whatever it points at.
func rootLstat(root *os.Root, rel string) (fs.FileInfo, error) {
	return root.Lstat(rootRel(rel))
}

// rootReplaceFile is the confined twin of replaceFile: same-directory temp file
// plus rename, with every step resolved through the root handle.
//
// The temp file is created with O_EXCL under a name derived from the target, so
// a pre-planted file at the temp path cannot be written through either. Windows
// still needs the delete-before-rename step; as in replaceFile, the caller's
// snapshot compensation repairs the resulting gap on a process error.
func rootReplaceFile(root *os.Root, rel string, data []byte, mode os.FileMode) (err error) {
	if err := rootMkdirAll(root, rel); err != nil {
		return err
	}
	if err := ensureNoSymlink(root, rel); err != nil {
		return err
	}
	tmpRel := filepath.ToSlash(rel) + ".yanshi-tmp-" + newVCSID()
	f, err := root.OpenFile(rootRel(tmpRel), os.O_RDWR|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("vcs: create temp for %s: %w", rel, err)
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = root.Remove(rootRel(tmpRel))
		}
	}()
	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("vcs: write temp for %s: %w", rel, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("vcs: close temp for %s: %w", rel, err)
	}
	// Chmod after close: OpenFile's perm argument is masked by umask, so the
	// mode the snapshot recorded would otherwise not survive a restore.
	if err = root.Chmod(rootRel(tmpRel), mode); err != nil {
		return fmt.Errorf("vcs: chmod temp for %s: %w", rel, err)
	}
	if err = removeForRename(root, rel); err != nil {
		return err
	}
	if err = root.Rename(rootRel(tmpRel), rootRel(rel)); err != nil {
		return fmt.Errorf("vcs: rename onto %s: %w", rel, err)
	}
	return nil
}

// removeForRename deletes an existing destination before a rename on the
// platforms that need it. POSIX rename replaces atomically and must NOT unlink
// first (that would open a window where the path does not exist); Windows
// rejects a rename onto an existing name, so there the delete is mandatory.
// Mirrors the runtime.GOOS branch in replaceFile.
func removeForRename(root *os.Root, rel string) error {
	if runtime.GOOS != "windows" {
		return nil
	}
	if err := root.Remove(rootRel(rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("vcs: clear destination %s: %w", rel, err)
	}
	return nil
}
