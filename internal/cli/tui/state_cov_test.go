package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/proto"
)

// chanSession is a test tuiSession whose SendFrame returns a caller-controlled
// channel, so the closures returned by fetchInitialStatus / syncSavedMode can
// be exercised directly.
type chanSession struct {
	ch chan cli.StreamEvent
}

func (s *chanSession) Send(string) <-chan cli.StreamEvent                   { return nil }
func (s *chanSession) SendFrame(_ proto.ClientFrame) <-chan cli.StreamEvent { return s.ch }
func (s *chanSession) CancelCurrent() error                                 { return nil }
func (s *chanSession) Mode() string                                         { return "fake" }
func (s *chanSession) Root() string                                         { return "/proj" }

// ---- dirName ----

// TestCov_DirName covers the empty + unshortenable fallback (Base is "." / sep)
// + the happy path.
func TestCov_DirName(t *testing.T) {
	assert.Equal(t, "", dirName(""))
	assert.Equal(t, ".", dirName("."), "unshortenable root returns itself")
	assert.Equal(t, "proj", dirName(filepath.Join("a", "proj")))
}

// ---- detectGitBranch ----

// TestCov_DetectGitBranch covers the empty-root → "." walk and a real .git/HEAD
// read (the parseGitHead success path).
func TestCov_DetectGitBranch(t *testing.T) {
	// Empty root → dir defaults to "."; covers the "" → "." branch regardless of
	// whether the cwd happens to be inside a git repo.
	_ = detectGitBranch("")

	// A temp dir with a real .git/HEAD → deterministic branch read.
	tmp := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(tmp, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, ".git", "HEAD"),
		[]byte("ref: refs/heads/feature/x"), 0o644))
	assert.Equal(t, "feature/x", detectGitBranch(tmp))

	// A bare SHA (detached HEAD) → ":" + first 7 chars.
	require.NoError(t, os.WriteFile(filepath.Join(tmp, ".git", "HEAD"),
		[]byte("0123456789abcdef"), 0o644))
	assert.Equal(t, ":0123456", detectGitBranch(tmp))
}

// ---- fetchInitialStatus ----

// TestCov_FetchInitialStatus covers both closure outcomes: a status reply
// becomes a streamMsg, and a closed/non-status reply yields nil.
func TestCov_FetchInitialStatus(t *testing.T) {
	// Status reply → streamMsg.
	ch := make(chan cli.StreamEvent, 1)
	ch <- cli.StreamEvent{Kind: "status", Text: "ok"}
	m := newModel(&chanSession{ch: ch}, "/proj")
	cmd := m.fetchInitialStatus()
	require.NotNil(t, cmd)
	_, ok := cmd().(streamMsg)
	assert.True(t, ok, "status reply → streamMsg")

	// Closed channel → nil.
	closed := make(chan cli.StreamEvent)
	close(closed)
	m2 := newModel(&chanSession{ch: closed}, "/proj")
	cmd2 := m2.fetchInitialStatus()
	require.NotNil(t, cmd2)
	assert.Nil(t, cmd2(), "closed reply channel → nil Msg")
}

// ---- syncSavedMode ----

// TestCov_SyncSavedMode covers the empty-mode default, the Auto+zero-threshold
// default, and the drain closure body.
func TestCov_SyncSavedMode(t *testing.T) {
	// Empty mode → ModeDefault; drain closure returns nil.
	closed := make(chan cli.StreamEvent)
	close(closed)
	m := newModel(&chanSession{ch: closed}, "/proj")
	m.permMode = ""
	cmd := m.syncSavedMode()
	require.NotNil(t, cmd)
	assert.Nil(t, cmd(), "sync closure drains + returns nil")

	// Auto + threshold 0 → default threshold resolved for the wire.
	m2 := newModel(&chanSession{ch: closed}, "/proj")
	m2.permMode = guard.ModeAuto
	m2.autoThreshold = 0
	cmd2 := m2.syncSavedMode()
	require.NotNil(t, cmd2)
	assert.Nil(t, cmd2())
}
