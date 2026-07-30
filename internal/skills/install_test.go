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

// TestParseInstallSource_RejectsTraversal proves each segment of owner/repo
// rejects "." and "..".
func TestParseInstallSource_RejectsTraversal(t *testing.T) {
	cases := []string{
		"github:../etc",
		"github:./foo",
		"github:foo/..",
		"github:foo/bar/..",
		"github:foo/../../etc",
	}
	for _, src := range cases {
		_, err := ParseInstallSource(src)
		if err == nil {
			t.Errorf("ParseInstallSource(%q) should reject path traversal", src)
		}
	}
}

// TestParseInstallSource_RejectsInvalidChars proves illegal characters are
// rejected.
func TestParseInstallSource_RejectsInvalidChars(t *testing.T) {
	cases := []string{
		"github:foo bar/baz",  // space
		"github:foo;rm -rf /", // semicolon
		"github:foo&bar/baz",  // ampersand
		`github:foo\bar/baz`,  // backslash
	}
	for _, src := range cases {
		_, err := ParseInstallSource(src)
		if err == nil {
			t.Errorf("ParseInstallSource(%q) should reject illegal characters", src)
		}
	}
}

// TestParseInstallSource_AcceptsValid proves a legal source parses correctly.
func TestParseInstallSource_AcceptsValid(t *testing.T) {
	cases := []struct {
		src    string
		owner  string
		repo   string
		subdir string
	}{
		{"github:owner/repo", "owner", "repo", ""},
		{"github:owner/repo/skills/foo", "owner", "repo", "skills/foo"},
		{"github:o-r_n.r/e-p_o/s-d_r", "o-r_n.r", "e-p_o", "s-d_r"},
	}
	for _, tc := range cases {
		got, err := ParseInstallSource(tc.src)
		require.NoError(t, err, "src=%q", tc.src)
		assert.Equal(t, tc.owner, got.Owner, "src=%q", tc.src)
		assert.Equal(t, tc.repo, got.Repo, "src=%q", tc.src)
		assert.Equal(t, tc.subdir, got.Subdir, "src=%q", tc.src)
	}
}

// cloneFunc lets a test materialize an exact staging tree (e.g. preserve a
// symlink, which os.CopyFS intentionally refuses to copy).
type cloneFunc func(context.Context, string, string, string) error

func (f cloneFunc) Clone(ctx context.Context, owner, repo, intoDir string) error {
	return f(ctx, owner, repo, intoDir)
}

// TestInstall_RejectsSymlink proves the installer itself inspects the staged
// tree. A custom cloner creates the symlink directly in intoDir.
func TestInstall_RejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated permission on Windows")
	}
	external := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(external, []byte("secret"), 0o644))
	cloner := cloneFunc(func(_ context.Context, _, _, intoDir string) error {
		require.NoError(t, os.WriteFile(filepath.Join(intoDir, "SKILL.md"),
			[]byte("---\nname: evil\ndescription: d\n---\n"), 0o644))
		return os.Symlink(external, filepath.Join(intoDir, "link"))
	})

	_, err := Install("github:fake-remote/evil-skill", t.TempDir(), cloner)
	require.Error(t, err, "a skill containing a symlink must be rejected")
	assert.Contains(t, strings.ToLower(err.Error()), "symlink",
		"err should clearly mention symlink rejection, got: %v", err)
}

// TestInstall_DeletesRemoteMarkers proves that after CloneStub descends into
// the repo, the forged .trusted/.disabled files are deleted before publishing.
func TestInstall_DeletesRemoteMarkers(t *testing.T) {
	remoteRoot := t.TempDir()
	repoDir := filepath.Join(remoteRoot, "fake-trusted")
	require.NoError(t, os.MkdirAll(repoDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "SKILL.md"),
		[]byte("---\nname: fake-trusted\ndescription: d\n---\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".trusted"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".disabled"), nil, 0o644))

	dstRoot := t.TempDir()
	name, err := Install("github:fake-remote/fake-trusted", dstRoot, &CloneStub{AsRemote: remoteRoot})
	require.NoError(t, err)
	installed := filepath.Join(dstRoot, name)
	for _, marker := range []string{".trusted", ".disabled"} {
		_, statErr := os.Stat(filepath.Join(installed, marker))
		assert.True(t, os.IsNotExist(statErr),
			"remote %s must not survive install; got statErr=%v", marker, statErr)
	}
}

// TestInstall_FixtureRepositoryLayout is the GB3 regression: AsRemote points
// at a parent containing repo directories; CloneStub must descend into repo so
// staging/SKILL.md exists for a source without subdir.
func TestInstall_FixtureRepositoryLayout(t *testing.T) {
	fixtureRoot, err := filepath.Abs("fixtures")
	require.NoError(t, err)
	dstRoot := t.TempDir()
	name, err := Install("github:fake-remote/evil-scripts", dstRoot,
		&CloneStub{AsRemote: fixtureRoot})
	require.NoError(t, err)
	assert.Equal(t, "evil-scripts", name)
	_, err = os.Stat(filepath.Join(dstRoot, name, "SKILL.md"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dstRoot, name, "scripts", "evil.sh"))
	require.NoError(t, err, "script may install as a regular file but must never execute")
	for _, marker := range []string{".trusted", ".disabled"} {
		_, statErr := os.Stat(filepath.Join(dstRoot, name, marker))
		assert.True(t, os.IsNotExist(statErr), "remote marker %s must be purged", marker)
	}
}

// TestInstall_RejectsExistingTarget proves a pre-existing target is refused
// (no overwrite).
func TestInstall_RejectsExistingTarget(t *testing.T) {
	remote := t.TempDir()
	skillDir := filepath.Join(remote, "dupe")
	os.MkdirAll(skillDir, 0o755)
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: dupe\ndescription: d\n---\n"), 0o644)

	dstRoot := t.TempDir()
	// Pre-create the target dir.
	os.MkdirAll(filepath.Join(dstRoot, "dupe"), 0o755)
	_, err := Install("github:fake-remote/dupe", dstRoot, &CloneStub{AsRemote: remote})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "exists"),
		"err should include 'exists', got: %v", err)
}
