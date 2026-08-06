package tui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/proto"
)

// replySession returns a fixed channel from SendFrame so the control-frame
// helpers (fetchInitialStatus / syncSavedMode) can exercise their reply-reading
// Cmd. Send is unused.
type replySession struct {
	ch chan cli.StreamEvent
}

// SendTurn routes an attachment-carrying turn through the same path Send
// uses. Required on the interface rather than optional so a session that
// forgets it fails to compile — an optional one would silently drop every
// attachment on that path.
func (r *replySession) SendTurn(fr proto.ClientFrame) <-chan cli.StreamEvent {
	return r.Send(fr.Text)
}

func (r *replySession) Send(_ string) <-chan cli.StreamEvent                 { return nil }
func (r *replySession) SendFrame(_ proto.ClientFrame) <-chan cli.StreamEvent { return r.ch }
func (r *replySession) CancelCurrent() error                                 { return nil }
func (r *replySession) Mode() string                                         { return "ws" }
func (r *replySession) Root() string                                         { return "/proj" }

// statusReplyCh returns a channel delivering one status event then closing.
func statusReplyCh() chan cli.StreamEvent {
	ch := make(chan cli.StreamEvent, 1)
	ch <- cli.StreamEvent{Kind: "status", Model: "m"}
	close(ch)
	return ch
}

// closedNonStatusCh returns a channel delivering one non-status event then
// closing (fetchInitialStatus returns a nil Msg for a non-status first reply).
func closedNonStatusCh() chan cli.StreamEvent {
	ch := make(chan cli.StreamEvent, 1)
	ch <- cli.StreamEvent{Kind: "models"}
	close(ch)
	return ch
}

// closedReplyCh returns an already-closed channel (syncSavedMode drains it).
func closedReplyCh() chan cli.StreamEvent {
	ch := make(chan cli.StreamEvent)
	close(ch)
	return ch
}

// ---- action palette ----

func TestCollectActions_AllSources(t *testing.T) {
	m := wsModel(&recordingSession{})
	m.models = []string{"gpt-4o", "claude-opus-4"}
	items := m.collectActions()
	// /help is always first.
	assert.Equal(t, "/help", items[0].Label)
	// At least one command, mode, model, and theme entry.
	saw := map[string]bool{}
	for _, it := range items {
		saw[it.Source] = true
	}
	for _, src := range []string{"command", "mode", "model", "theme"} {
		assert.True(t, saw[src], "source %q present", src)
	}
}

func TestRankedActions_QueryFilters(t *testing.T) {
	m := wsModel(&recordingSession{})
	// Empty query -> all actions.
	m.action = &actionState{query: ""}
	all := m.rankedActions()
	assert.NotEmpty(t, all)

	// A query that matches nothing yields no results.
	m.action = &actionState{query: "zzzzzzzzz"}
	assert.Empty(t, m.rankedActions())

	// A query matching a command yields a non-empty, filtered set.
	m.action = &actionState{query: "mode"}
	matched := m.rankedActions()
	assert.NotEmpty(t, matched)
	for _, it := range matched {
		assert.Contains(t, it.Label, "mode")
	}
}

func TestActionMoveAndConfirm(t *testing.T) {
	m := wsModel(&recordingSession{})
	// No action open -> no-op.
	m.actionMove(1)

	m.openActionPopup()
	require.NotNil(t, m.action)
	n := len(m.action.items)
	require.Greater(t, n, 1)
	assert.Equal(t, 0, m.action.cursor)
	m.actionMove(1)
	assert.Equal(t, 1, m.action.cursor, "actionMove advances cursor")
	m.actionMove(-1)
	assert.Equal(t, 0, m.action.cursor, "actionMove wraps backwards")

	// Confirm runs the selected item's Action (sets the input for /help).
	m.action.cursor = 0 // /help
	mm, _ := m.actionConfirm()
	m2 := mm.(model)
	assert.Nil(t, m2.action, "confirm closes the popup")
	assert.Contains(t, m2.input.Value(), "/help")

	// Confirm with no action open -> no-op.
	m3 := wsModel(&recordingSession{})
	mm, _ = m3.actionConfirm()
	assert.Nil(t, mm.(model).action)

	// Confirm with cursor out of range -> no-op (no panic).
	m4 := wsModel(&recordingSession{})
	m4.action = &actionState{visible: true, cursor: 999, items: []actionItem{{Label: "x"}}}
	mm, _ = m4.actionConfirm()
	assert.NotNil(t, mm.(model).action, "out-of-range cursor is a no-op")

	// closeActionPopup clears the action state.
	m4.closeActionPopup()
	assert.Nil(t, m4.action)
}

func TestActionPopup_Render(t *testing.T) {
	m := wsModel(&recordingSession{})
	assert.Equal(t, "", m.actionPopup(), "closed popup renders nothing")

	m.openActionPopup()
	m.action.query = "test"
	out := m.actionPopup()
	assert.Contains(t, out, "Actions")
	assert.Contains(t, out, "test")

	// Empty result set renders the "no matching actions" line.
	m2 := wsModel(&recordingSession{})
	m2.action = &actionState{visible: true, query: "zzzzzzz", items: nil}
	out = m2.actionPopup()
	assert.Contains(t, out, "no matching actions")
}

// ---- startup helpers ----

func TestFormatDurationWords(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{30 * time.Second, "30s"},
		{65 * time.Second, "1m 5s"},
		{time.Hour + time.Minute + 40*time.Second, "1h 1m 40s"},
		{26 * time.Hour, "1d 2h 0m 0s"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, formatDurationWords(tc.d))
	}
}

func TestFormatDurationMMSS(t *testing.T) {
	assert.Equal(t, "0:00", formatDuration(0))
	assert.Equal(t, "1:12", formatDuration(72*time.Second))
}

func TestDetectShellEnv_NonEmpty(t *testing.T) {
	// DetectShellEnv always returns a non-empty shell name on every platform.
	assert.NotEmpty(t, detectShellEnv())
}

func TestProbeLine_EmptySpec(t *testing.T) {
	assert.Equal(t, "", probeLine(""), "empty spec returns empty")
	assert.Equal(t, "", probeLine("   "), "whitespace-only spec returns empty")
	// A real but possibly-absent tool: returns "" or a version string; either is fine.
	_ = probeLine("git --version")
}

func TestTicks_ReturnCmds(t *testing.T) {
	assert.NotNil(t, activityTick())
	assert.NotNil(t, repaintTick())
}

func TestWatchGitHead_NonGitReturnsNil(t *testing.T) {
	// Empty root -> nil.
	assert.Nil(t, watchGitHead(""))
	// A temp dir with no .git -> nil.
	assert.Nil(t, watchGitHead(t.TempDir()))
}

func TestWatchGitHead_GitDirReturnsCmd(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	cmd := watchGitHead(root)
	require.NotNil(t, cmd, "a real .git directory arms the watcher")
	// The Cmd blocks until HEAD changes; do NOT execute it here.
}

func TestProbeStartupTools_ReturnsMsg(t *testing.T) {
	cmd := probeStartupTools()
	require.NotNil(t, cmd)
	msg := cmd()
	_, ok := msg.(startupToolsMsg)
	assert.True(t, ok, "probeStartupTools returns a startupToolsMsg")
}

// ---- /stash ----

func TestCmdStash_ListDropPop(t *testing.T) {
	// /stash list on an empty stash -> info toast.
	m := wsModel(&recordingSession{})
	m.stash = newTestStash(t)
	mm, _ := runCommandOn(model(m), "/stash list")
	_ = mm.(model)

	// Push a draft, then /stash list renders an entry.
	m = wsModel(&recordingSession{})
	m.stash = newTestStash(t)
	m.stash.Push("draft one")
	mm, _ = runCommandOn(model(m), "/stash list")
	m = mm.(model)
	var sawList bool
	for _, e := range m.entries {
		if _, ok := e.(*stashListEntry); ok {
			sawList = true
		}
	}
	assert.True(t, sawList, "/stash list renders a stash list entry")

	// /stash drop <N> with a bad index -> error toast, no panic.
	m = wsModel(&recordingSession{})
	m.stash = newTestStash(t)
	m.stash.Push("x")
	mm, _ = runCommandOn(model(m), "/stash drop 9")
	_ = mm.(model)

	// /stash drop not-a-number -> usage error toast.
	m = wsModel(&recordingSession{})
	m.stash = newTestStash(t)
	mm, _ = runCommandOn(model(m), "/stash drop abc")
	_ = mm.(model)

	// /stash (pop) on empty -> warn toast.
	m = wsModel(&recordingSession{})
	m.stash = newTestStash(t)
	mm, _ = runCommandOn(model(m), "/stash")
	_ = mm.(model)

	// /stash (pop) with a draft -> restores into the input.
	m = wsModel(&recordingSession{})
	m.stash = newTestStash(t)
	m.stash.Push("restored draft")
	mm, _ = runCommandOn(model(m), "/stash")
	m = mm.(model)
	assert.Equal(t, "restored draft", m.input.Value())
}

func TestStash_SaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stash.jsonl")
	s := &Stash{path: path}
	s.Push("alpha")
	s.Push("beta")
	require.NoError(t, s.Save())

	loaded, err := LoadStash(path)
	require.NoError(t, err)
	items := loaded.List()
	require.Len(t, items, 2)
	assert.Equal(t, "alpha", items[0].Text)
	assert.Equal(t, "beta", items[1].Text)
}

// ---- picker / state ----

func TestPickerConfirm_AllKinds(t *testing.T) {
	// Empty picker -> closes, no-op.
	m := wsModel(&recordingSession{})
	m.pickerKind = "model"
	mm, _ := m.pickerConfirm()
	assert.Equal(t, "", mm.(model).pickerKind, "empty picker closes")

	// model picker -> sends set_model.
	rec := &recordingSession{}
	m = wsModel(rec)
	m.pickerKind = "model"
	m.pickerItems = []pickerItem{{name: "gpt-4o"}}
	mm, _ = m.pickerConfirm()
	_ = mm.(model)
	require.Contains(t, frameTypes(rec.frames), "set_model")

	// mode picker -> sends set_mode.
	rec = &recordingSession{}
	m = wsModel(rec)
	m.pickerKind = "mode"
	m.pickerItems = []pickerItem{{name: "allow-edits"}}
	mm, _ = m.pickerConfirm()
	m = mm.(model)
	require.Contains(t, frameTypes(rec.frames), "set_mode")
	assert.Equal(t, "allow-edits", string(m.permMode))

	// mode picker with an unknown mode -> local error.
	rec = &recordingSession{}
	m = wsModel(rec)
	m.pickerKind = "mode"
	m.pickerItems = []pickerItem{{name: "bogus"}}
	mm, _ = m.pickerConfirm()
	_ = mm.(model)
	assert.Empty(t, rec.frames, "unknown mode sends no frame")

	// theme picker -> sets theme locally.
	rec = &recordingSession{}
	m = wsModel(rec)
	m.pickerKind = "theme"
	m.pickerItems = []pickerItem{{name: "muted"}}
	mm, _ = m.pickerConfirm()
	m = mm.(model)
	assert.Equal(t, ThemeMuted, m.theme)

	// theme picker with an unknown theme -> local error.
	rec = &recordingSession{}
	m = wsModel(rec)
	m.pickerKind = "theme"
	m.pickerItems = []pickerItem{{name: "nope"}}
	mm, _ = m.pickerConfirm()
	_ = mm.(model)

	// unknown picker kind -> no-op.
	rec = &recordingSession{}
	m = wsModel(rec)
	m.pickerKind = "unknown"
	m.pickerItems = []pickerItem{{name: "x"}}
	mm, _ = m.pickerConfirm()
	_ = mm.(model)
	assert.Empty(t, rec.frames)
}

func TestFetchInitialStatus_NilAndReply(t *testing.T) {
	// fakeSession.SendFrame returns nil -> nil cmd.
	m := wsModel(&fakeSession{})
	assert.Nil(t, m.fetchInitialStatus())

	// A replySession returns a channel; fetchInitialStatus returns a Cmd that
	// reads one status event (or nil for a non-status reply).
	m = wsModel(&replySession{ch: statusReplyCh()})
	cmd := m.fetchInitialStatus()
	require.NotNil(t, cmd)

	// A reply whose first event is NOT status -> the Cmd returns nil (keep header
	// on defaults), but the Cmd itself is still non-nil.
	m = wsModel(&replySession{ch: closedNonStatusCh()})
	require.NotNil(t, m.fetchInitialStatus())
}

func TestSyncSavedMode_NilAndDrain(t *testing.T) {
	// nil channel (fakeSession) -> nil cmd.
	m := wsModel(&fakeSession{})
	assert.Nil(t, m.syncSavedMode())

	// A replySession returns a channel; syncSavedMode drains it and returns a cmd.
	m = wsModel(&replySession{ch: closedReplyCh()})
	cmd := m.syncSavedMode()
	require.NotNil(t, cmd)
}

// ---- /skill ----

func TestCmdSkill_AllSubcommands(t *testing.T) {
	// No args -> usage error.
	m := wsModel(&recordingSession{})
	mm, _ := runCommandOn(model(m), "/skill")
	_ = mm.(model)

	// install without source -> usage error.
	m = wsModel(&recordingSession{})
	mm, _ = runCommandOn(model(m), "/skill install")
	_ = mm.(model)

	// install with source -> install_skill frame.
	rec := &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/skill install github:foo/bar")
	_ = mm.(model)
	require.NotEmpty(t, rec.frames)
	assert.Equal(t, "install_skill", rec.frames[0].Type)

	// Each single-name subcommand without a name -> usage error.
	for _, sub := range []string{"uninstall", "trust", "untrust", "enable", "disable"} {
		rec := &recordingSession{}
		m := wsModel(rec)
		mm, _ := runCommandOn(model(m), "/skill "+sub)
		_ = mm.(model)
		assert.Empty(t, rec.frames, "/skill %s without name sends nothing", sub)
	}

	// With a name -> the matching frame.
	for _, sub := range []struct{ in, typ string }{
		{"uninstall", "uninstall_skill"}, {"trust", "trust_skill"},
		{"untrust", "untrust_skill"}, {"enable", "enable_skill"}, {"disable", "disable_skill"},
	} {
		rec := &recordingSession{}
		m := wsModel(rec)
		mm, _ := runCommandOn(model(m), "/skill "+sub.in+" myskill")
		_ = mm.(model)
		require.NotEmpty(t, rec.frames, "/skill %s sends a frame", sub.in)
		assert.Equal(t, sub.typ, rec.frames[0].Type)
	}

	// Unknown subcommand -> error.
	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/skill bogus")
	_ = mm.(model)
	assert.Empty(t, rec.frames)
}

// ---- events ----

func TestLastNestedTool(t *testing.T) {
	m := wsModel(&recordingSession{})
	assert.Nil(t, m.lastNestedTool())

	// A non-nested tool is not returned.
	t1 := &toolEntry{name: "fs_read"}
	m.entries = append(m.entries, t1)
	assert.Nil(t, m.lastNestedTool())

	// A nested tool is returned (the most recent one).
	t2 := &toolEntry{name: "analysis", nested: true}
	m.entries = append(m.entries, t2)
	got := m.lastNestedTool()
	require.NotNil(t, got)
	assert.True(t, got.nested)
}

// newTestStash builds an in-memory stash backed by a temp path so cmdStash write
// paths (which call enqueueSave -> Save) do not touch the developer's real file.
func newTestStash(t *testing.T) *Stash {
	t.Helper()
	return &Stash{path: filepath.Join(t.TempDir(), "stash.jsonl")}
}
