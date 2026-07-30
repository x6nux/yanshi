package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/version"
)

// ---- renderBlockLogo ----

// TestCov_RenderBlockLogo_NoGlyphs covers the empty-glyphs early return: a
// string with no renderable letters (blockFont only defines Y/A/N/S/H/I).
func TestCov_RenderBlockLogo_NoGlyphs(t *testing.T) {
	assert.Equal(t, "", renderBlockLogo("zzz"))
	assert.Equal(t, "", renderBlockLogo(""))
}

// ---- startupEntry.render width clamps + version stamp ----

// TestCov_Render_WidthClamps covers the narrow (< min) and medium (< max)
// terminal-width clamp branches.
func TestCov_Render_WidthClamps(t *testing.T) {
	e := &startupEntry{info: "OS: x"}
	// Narrow: avail = width-margin-4 < startupBoxMin → inner clamped to min.
	out := e.render(40, newSpinner())
	assert.Contains(t, out, "OS: x")
	// Medium: avail in [min, max) → inner = avail.
	out = e.render(55, newSpinner())
	assert.Contains(t, out, "OS: x")
}

// TestCov_Render_VersionStamp covers the BuildStamp + GitHash subtitle
// branches (both are package vars injected via ldflags/VCS at build time).
func TestCov_Render_VersionStamp(t *testing.T) {
	origStamp, origHash := version.BuildStamp, version.GitHash
	defer func() {
		version.BuildStamp = origStamp
		version.GitHash = origHash
	}()
	version.BuildStamp = "2607230117"
	version.GitHash = "abc1234"

	out := (&startupEntry{info: "x"}).render(120, newSpinner())
	assert.Contains(t, out, "2607230117", "BuildStamp appended to the version line")
	assert.Contains(t, out, "abc1234", "GitHash appended after ·")
}

// ---- detectShellEnv ----

// TestCov_DetectShellEnv covers the SHELL-env early return and the Windows
// branches (bash / powershell / cmd) via env manipulation.
func TestCov_DetectShellEnv(t *testing.T) {
	// SHELL env wins outright.
	t.Setenv("SHELL", "/usr/local/bin/zsh")
	assert.Equal(t, "/usr/local/bin/zsh", detectShellEnv())

	// SHELL unset + Windows: LookPath("bash") branch (covered when bash is on
	// PATH; the call itself runs regardless).
	t.Setenv("SHELL", "")
	got := detectShellEnv()
	if _, err := exec.LookPath("bash"); err == nil {
		assert.Equal(t, "bash (Git Bash)", got)
	}

	// SHELL unset, PATH emptied, PSModulePath set → powershell.
	t.Setenv("SHELL", "")
	t.Setenv("PATH", "")
	t.Setenv("PSModulePath", "something")
	assert.Equal(t, "powershell", detectShellEnv())

	// SHELL unset, PATH emptied, PSModulePath empty → cmd.
	t.Setenv("SHELL", "")
	t.Setenv("PATH", "")
	t.Setenv("PSModulePath", "")
	assert.Equal(t, "cmd", detectShellEnv())
}

// TestCov_RepaintTick covers the repaint-tick closure body by firing the
// returned cmd (the closure only executes when the cmd runs, ~5s later).
func TestCov_RepaintTick(t *testing.T) {
	cmd := repaintTick()
	require.NotNil(t, cmd)
	_, ok := cmd().(repaintMsg)
	assert.True(t, ok, "the repaint-tick closure yields a repaintMsg")
}

// ---- watchGitHead ----

// TestCov_WatchGitHead covers the empty-root and no-.git early returns, plus
// the fsnotify goroutine: a non-HEAD write is ignored and a HEAD write fires
// gitRefreshMsg.
func TestCov_WatchGitHead(t *testing.T) {
	// Empty root → nil.
	assert.Nil(t, watchGitHead(""))

	// No .git directory → nil.
	bare := t.TempDir()
	assert.Nil(t, watchGitHead(bare))

	// Set up a real .git/HEAD to watch.
	gitDir := filepath.Join(bare, ".git")
	require.NoError(t, os.Mkdir(gitDir, 0o755))
	headPath := filepath.Join(gitDir, "HEAD")
	require.NoError(t, os.WriteFile(headPath, []byte("ref: refs/heads/main\n"), 0o644))
	indexPath := filepath.Join(gitDir, "index")
	require.NoError(t, os.WriteFile(indexPath, []byte("idx"), 0o644))

	cmd := watchGitHead(bare)
	require.NotNil(t, cmd)
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()

	// Let the watcher arm, then nudge a non-HEAD file (ignored) and finally
	// HEAD (fires gitRefreshMsg).
	time.Sleep(250 * time.Millisecond)
	_ = os.WriteFile(indexPath, []byte("idx2"), 0o644) // non-HEAD → keep watching
	time.Sleep(150 * time.Millisecond)
	require.NoError(t, os.WriteFile(headPath, []byte("ref: refs/heads/dev\n"), 0o644))

	select {
	case msg := <-done:
		_, ok := msg.(gitRefreshMsg)
		assert.True(t, ok, "HEAD write fires gitRefreshMsg")
	case <-time.After(3 * time.Second):
		t.Fatal("watchGitHead did not fire on HEAD write")
	}
}
