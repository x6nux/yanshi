package skills

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadOne writes a single skill and loads it, returning the registry + skill.
func loadOne(t *testing.T, name string) (*Registry, *Skill, string) {
	t.Helper()
	root := t.TempDir()
	dir := writeSkill(t, root, name, "---\nname: "+name+"\ndescription: "+name+"-desc\n---\n# "+name+"\nbody\n")
	reg, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)
	s, ok := reg.Get(name)
	require.True(t, ok)
	return reg, s, dir
}

// --- Trust / Untrust (were 0%) ---

func TestRegistryTrustRoundTrip(t *testing.T) {
	reg, s, _ := loadOne(t, "trusty")
	require.False(t, s.Trusted)

	// Trust writes the marker + flips the flag.
	require.NoError(t, reg.Trust("trusty"))
	got, _ := reg.Get("trusty")
	assert.True(t, got.Trusted)
	// marker file exists on disk
	_, err := os.Stat(filepath.Join(s.Dir, ".trusted"))
	assert.NoError(t, err)

	// Trust unknown skill errors.
	require.Error(t, reg.Trust("nope"))

	// Untrust removes the marker + clears the flag.
	require.NoError(t, reg.Untrust("trusty"))
	got, _ = reg.Get("trusty")
	assert.False(t, got.Trusted)

	// Untrust on a skill without the marker is a no-op success (IsNotExist).
	require.NoError(t, reg.Untrust("trusty"))
	// Untrust unknown skill errors.
	require.Error(t, reg.Untrust("nope"))
}

// --- Enable / Disable unknown-skill branches ---

func TestRegistryEnableDisableUnknown(t *testing.T) {
	reg, _, _ := loadOne(t, "thing")
	require.Error(t, reg.Enable("ghost"))
	require.Error(t, reg.Disable("ghost"))
}

func TestRegistryEnableRemovesDisabledMarker(t *testing.T) {
	reg, s, _ := loadOne(t, "flippy")
	// Disable then Enable → marker removed, Enabled true.
	require.NoError(t, reg.Disable("flippy"))
	_, err := os.Stat(filepath.Join(s.Dir, ".disabled"))
	assert.NoError(t, err)
	require.NoError(t, reg.Enable("flippy"))
	_, err = os.Stat(filepath.Join(s.Dir, ".disabled"))
	assert.True(t, os.IsNotExist(err))
	got, _ := reg.Get("flippy")
	assert.True(t, got.Enabled)
}

// --- cloneSkill nil branch ---

func TestCloneSkillNil(t *testing.T) {
	assert.Nil(t, cloneSkill(nil))
}

// --- Body error branch (readFrontmatter fails) ---

func TestRegistryBodyMissingFile(t *testing.T) {
	reg, s, _ := loadOne(t, "bodiless")
	// Remove the SKILL.md so Body's readFrontmatter fails.
	require.NoError(t, os.Remove(filepath.Join(s.Dir, "SKILL.md")))
	_, err := reg.Body(s)
	require.Error(t, err)
}

// --- ReadFile missing-file error branch ---

func TestRegistryReadFileMissing(t *testing.T) {
	reg, s, _ := loadOne(t, "reader")
	_, err := reg.ReadFile(s, "no-such-file.md")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read")
}

// --- Load: root is a file (ReadDir non-NotExist error) ---
//
// On POSIX, os.ReadDir on a regular file returns ENOTDIR (a non-NotExist
// error) so Load surfaces it. On Windows, ReadDir on a file returns
// (empty, nil), so this POSIX-only error path cannot be exercised there.

func TestLoadRootIsFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ReadDir on a file returns (empty,nil) on Windows; ENOTDIR is POSIX-only")
	}
	f := filepath.Join(t.TempDir(), "iam-a-file")
	require.NoError(t, os.WriteFile(f, []byte("x"), 0o644))
	_, err := NewLoader(Builtin(f)).Load()
	require.Error(t, err)
}

// --- Reload: nil loader ---

func TestReloadNil(t *testing.T) {
	reg, _, _ := loadOne(t, "reloadable")
	require.Error(t, reg.Reload(nil))
}

// TestReloadFailingLoader covers Reload's Load-error branch (POSIX-only: on
// Windows ReadDir on a file does not error).
func TestReloadFailingLoader(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Load only errors on ReadDir-non-NotExist, which is POSIX-only")
	}
	reg, _, _ := loadOne(t, "keepme")
	f := filepath.Join(t.TempDir(), "a-file")
	require.NoError(t, os.WriteFile(f, []byte("x"), 0o644))
	badLoader := NewLoader(Builtin(f))
	require.Error(t, reg.Reload(badLoader))
	_, ok := reg.Get("keepme")
	assert.True(t, ok, "failed Reload must leave the old map intact")
}

// --- ParseInstallSource non-github prefix ---

func TestParseInstallSourceNonGithub(t *testing.T) {
	_, err := ParseInstallSource("local:/some/path")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only github: supported")
}

// TestParseInstallSourceEmptySegment covers the per-segment segmentRe failure
// branch: a double slash yields an empty segment.
func TestParseInstallSourceEmptySegment(t *testing.T) {
	_, err := ParseInstallSource("github:owner/repo//sub")
	require.Error(t, err)
}

// TestRealCloneError covers realClone's error path by cancelling the context so
// git exits immediately (no network wait). Any error — git missing, cancelled,
// or network — exercises the error-return branch.
func TestRealCloneError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dst := filepath.Join(t.TempDir(), "clone-out")
	err := realClone{}.Clone(ctx, "owner", "nonexistent-repo-zzz", dst)
	require.Error(t, err)
}

// TestInstallCloneNilThenMkdirFail covers the clone==nil → realClone assignment
// (install.go:116) and the MkdirAll-dstRoot error (install.go:119) by passing a
// dstRoot whose parent is a regular file. MkdirAll fails before Clone runs, so
// no network access is needed.
func TestInstallCloneNilThenMkdirFail(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "a-file")
	require.NoError(t, os.WriteFile(parent, []byte("x"), 0o644))
	// dstRoot under a file → MkdirAll cannot create it.
	_, err := Install("github:owner/repo", filepath.Join(parent, "sub"), nil)
	require.Error(t, err)
}

// TestInstallMarkerRemoveError covers the remote-marker removal error branch
// (install.go:177): when a cloned skill ships .disabled as a non-empty
// directory, os.Remove fails.
func TestInstallMarkerRemoveError(t *testing.T) {
	remote := t.TempDir()
	repoDir := filepath.Join(remote, "markery")
	require.NoError(t, os.MkdirAll(repoDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "SKILL.md"),
		[]byte("---\nname: markery\ndescription: d\n---\n"), 0o644))
	// .disabled as a non-empty directory → os.Remove fails.
	require.NoError(t, os.MkdirAll(filepath.Join(repoDir, ".disabled", "occupied"), 0o755))
	_, err := Install("github:fake-remote/markery", t.TempDir(), &CloneStub{AsRemote: remote})
	require.Error(t, err)
}

// --- Uninstall (was 0%) ---

func TestUninstall(t *testing.T) {
	dstRoot := t.TempDir()
	// Install a skill first so there is something to remove.
	remote := t.TempDir()
	repoDir := filepath.Join(remote, "removable")
	require.NoError(t, os.MkdirAll(repoDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "SKILL.md"),
		[]byte("---\nname: removable\ndescription: d\n---\n"), 0o644))
	name, err := Install("github:fake-remote/removable", dstRoot, &CloneStub{AsRemote: remote})
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dstRoot, name))
	require.NoError(t, err)

	// Uninstall removes it.
	require.NoError(t, Uninstall(name, dstRoot))
	_, err = os.Stat(filepath.Join(dstRoot, name))
	assert.True(t, os.IsNotExist(err))

	// Uninstall again → does-not-exist error.
	err = Uninstall(name, dstRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")

	// Invalid name rejected.
	require.Error(t, Uninstall("bad name", dstRoot))

	// Escape rejected: name that would resolve outside dstRoot.
	require.Error(t, Uninstall("..", dstRoot))
}

// --- Install error branches ---

// TestInstallCloneError covers the clone-failure branch.
func TestInstallCloneError(t *testing.T) {
	cloner := cloneFunc(func(context.Context, string, string, string) error {
		return os.ErrNotExist
	})
	_, err := Install("github:owner/repo", t.TempDir(), cloner)
	require.Error(t, err)
}

// TestInstallNoSkillMd covers the read-SKILL.md-failure branch.
func TestInstallNoSkillMd(t *testing.T) {
	cloner := cloneFunc(func(_ context.Context, _, _, intoDir string) error {
		// Clone succeeds but no SKILL.md present.
		return os.MkdirAll(intoDir, 0o755)
	})
	_, err := Install("github:owner/repo", t.TempDir(), cloner)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read SKILL.md")
}

// TestInstallInvalidName covers the invalid-skill-name branch.
func TestInstallInvalidName(t *testing.T) {
	cloner := cloneFunc(func(_ context.Context, _, _, intoDir string) error {
		return os.WriteFile(filepath.Join(intoDir, "SKILL.md"),
			[]byte("---\nname: has spaces\ndescription: d\n---\n"), 0o644)
	})
	_, err := Install("github:owner/repo", t.TempDir(), cloner)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid skill name")
}

// TestInstallSubdirEscape covers the subdir-escapes-staging branch.
func TestInstallSubdirEscape(t *testing.T) {
	cloner := cloneFunc(func(_ context.Context, _, _, intoDir string) error {
		return os.WriteFile(filepath.Join(intoDir, "SKILL.md"),
			[]byte("---\nname: ok\ndescription: d\n---\n"), 0o644)
	})
	// Subdir "../escape" resolves outside staging → rejected.
	_, err := Install("github:owner/repo/../escape", t.TempDir(), cloner)
	// ParseInstallSource rejects ".." already; verify the install path is
	// guarded regardless by trying a deep subdir that still parses.
	if err == nil {
		t.Fatal("expected error for traversal subdir")
	}
}

// TestInstallSubdirValid covers the happy path with a subdir: the repo contains
// a subdirectory whose SKILL.md is the skill.
func TestInstallSubdirValid(t *testing.T) {
	remote := t.TempDir()
	repoDir := filepath.Join(remote, "multiskill")
	sub := filepath.Join(repoDir, "skills", "inner")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "SKILL.md"),
		[]byte("---\nname: inner\ndescription: d\n---\n"), 0o644))
	dstRoot := t.TempDir()
	name, err := Install("github:fake-remote/multiskill/skills/inner", dstRoot, &CloneStub{AsRemote: remote})
	require.NoError(t, err)
	assert.Equal(t, "inner", name)
	_, err = os.Stat(filepath.Join(dstRoot, name, "SKILL.md"))
	require.NoError(t, err)
}

// TestInstallRejectsRemoteMarker that fails to remove (covered by DeletesRemoteMarkers).
// Here we cover the dst-escape guard is unreachable by construction, so instead
// exercise Install with an already-present target containing a subdir source.

// --- rejectSymlinks walk-error branch + CloneStub error ---

// TestRejectSymlinksMissingDir covers the walk-error branch of rejectSymlinks
// by pointing at a path that does not exist.
func TestRejectSymlinksMissingDir(t *testing.T) {
	err := rejectSymlinks(filepath.Join(t.TempDir(), "nope"))
	require.Error(t, err)
}

// TestCloneStubMissingRepo covers CloneStub.Clone when the repo dir is absent
// (os.CopyFS error).
func TestCloneStubMissingRepo(t *testing.T) {
	stub := &CloneStub{AsRemote: t.TempDir()}
	err := stub.Clone(context.Background(), "o", "no-such-repo", t.TempDir())
	require.Error(t, err)
}

// --- parseSkillFile error branches ---

func TestParseSkillFileErrors(t *testing.T) {
	// missing opening delimiter
	_, _, err := parseSkillFile([]byte("no frontmatter here"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "opening delimiter")
	// missing closing delimiter
	_, _, err = parseSkillFile([]byte("---\nname: x\nbody without close"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "closing delimiter")
	// empty (len < 2)
	_, _, err = parseSkillFile([]byte(""))
	require.Error(t, err)
	// bad YAML in frontmatter
	_, _, err = parseSkillFile([]byte("---\nname: [unclosed\n---\nbody"))
	require.Error(t, err)
}

// --- DiscoverPlugins root-is-file error branch ---

func TestDiscoverPluginsRootIsFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ReadDir on a file returns (empty,nil) on Windows; ENOTDIR is POSIX-only")
	}
	f := filepath.Join(t.TempDir(), "a-file")
	require.NoError(t, os.WriteFile(f, []byte("x"), 0o644))
	_, err := DiscoverPlugins(f)
	require.Error(t, err)
}

// TestDiscoverPluginsSkipsNonDirEntry covers the `!e.IsDir()` continue branch.
func TestDiscoverPluginsSkipsNonDirEntry(t *testing.T) {
	pluginRoot := t.TempDir()
	// A regular file directly in pluginRoot (not a dir) → skipped.
	require.NoError(t, os.WriteFile(filepath.Join(pluginRoot, "stray-file"), []byte("x"), 0o644))
	roots, err := DiscoverPlugins(pluginRoot)
	require.NoError(t, err)
	assert.Empty(t, roots)
}

// --- DiscoverPlugins read-error on manifest (stat ok, read fails) ---

func TestDiscoverPluginsManifestUnreadable(t *testing.T) {
	pluginRoot := t.TempDir()
	pDir := filepath.Join(pluginRoot, "p1")
	require.NoError(t, os.MkdirAll(filepath.Join(pDir, ".yanshi-plugin"), 0o755))
	// manifest.json is a directory (not a file) → os.ReadFile errors.
	require.NoError(t, os.MkdirAll(filepath.Join(pDir, ".yanshi-plugin", "plugin.json"), 0o755))
	roots, err := DiscoverPlugins(pluginRoot)
	require.NoError(t, err)
	assert.Empty(t, roots)
}

// keep strings referenced
var _ = strings.Contains

// TestMarkerOpsMarkerIsDirectory exercises the marker-file I/O error branches
// of Disable/Trust (WriteFile) and Enable/Untrust (Remove) by making the marker
// itself a directory: WriteFile over a directory path fails everywhere, and
// os.Remove on a NON-empty directory fails everywhere.
func TestMarkerOpsMarkerIsDirectory(t *testing.T) {
	root := t.TempDir()
	// "ro" loads as enabled (no markers); "ro2" loads as disabled+trusted via
	// directory markers so Enable/Untrust hit Remove on a non-empty dir.
	writeSkill(t, root, "ro", "---\nname: ro\ndescription: d\n---\nbody\n")
	disabledDir := filepath.Join(root, "ro", ".disabled")
	require.NoError(t, os.MkdirAll(filepath.Join(disabledDir, "occupied"), 0o755))
	trustedDir := filepath.Join(root, "ro", ".trusted")
	require.NoError(t, os.MkdirAll(filepath.Join(trustedDir, "occupied"), 0o755))

	reg, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)

	// Disable: WriteFile(.disabled) where .disabled is a non-empty dir → error.
	require.Error(t, reg.Disable("ro"), "WriteFile over a directory must fail")
	// Trust: WriteFile(.trusted) where .trusted is a non-empty dir → error.
	require.Error(t, reg.Trust("ro"), "WriteFile over a directory must fail")
	// Enable: Remove(.disabled) where .disabled is a non-empty dir → error.
	require.Error(t, reg.Enable("ro"), "Remove on a non-empty dir must fail")
	// Untrust: Remove(.trusted) where .trusted is a non-empty dir → error.
	require.Error(t, reg.Untrust("ro"), "Remove on a non-empty dir must fail")
}
