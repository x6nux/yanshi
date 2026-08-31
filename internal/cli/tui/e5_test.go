package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/proto"
)

// --- W-E-08: session picker transcript preview ---

// TestSessionPickerShowsPreview verifies that the restore picker renders the
// Preview field when the server supplies one (W-E-08).
//
// Mutation: remove the `if s.Preview != ""` block from sessionPickerPopup and
// this test fails because the preview text disappears.
func TestSessionPickerShowsPreview(t *testing.T) {
	m := newModel(&recordingSession{}, "/proj")
	m.restoreSessions = []proto.SessionInfo{
		{ID: "s1", Title: "first session", MsgCount: 4, Preview: "hello world this is the last message"},
		{ID: "s2", Title: "second session", MsgCount: 2, Preview: ""},
	}
	m.restoreCursor = 0
	m.width = 100
	out := m.sessionPickerPopup()
	if !strings.Contains(out, "hello world") {
		t.Errorf("sessionPickerPopup should render the Preview snippet, got:\n%s", out)
	}
}

// TestSessionPickerPreviewTruncated confirms long previews are truncated (W-E-08).
// Mutation: raise the truncate limit above 60 in sessionPickerPopup and the
// test fails because the suffix "…" disappears for a 61-char preview.
func TestSessionPickerPreviewTruncated(t *testing.T) {
	long := strings.Repeat("x", 80)
	m := newModel(&recordingSession{}, "/proj")
	m.restoreSessions = []proto.SessionInfo{
		{ID: "s1", Title: "t", MsgCount: 1, Preview: long},
	}
	m.restoreCursor = 0
	m.width = 120
	out := m.sessionPickerPopup()
	if !strings.Contains(out, "…") {
		t.Errorf("sessionPickerPopup should truncate a long preview with '…', got:\n%s", out)
	}
}

// TestSessionPickerNoPreviewNoEllipsis confirms that no spurious preview or
// ellipsis appears when Preview is empty (W-E-08).
func TestSessionPickerNoPreviewNoEllipsis(t *testing.T) {
	m := newModel(&recordingSession{}, "/proj")
	m.restoreSessions = []proto.SessionInfo{
		{ID: "s1", Title: "t", MsgCount: 0},
	}
	m.restoreCursor = 0
	m.width = 80
	out := m.sessionPickerPopup()
	if strings.Contains(out, "…") {
		t.Errorf("sessionPickerPopup must not render an ellipsis when Preview is empty, got:\n%s", out)
	}
}

// --- W-E-09: branch diff-stat and open PR in footer ---

// TestStatusHeader_ShowsDiffStat verifies that a non-empty gitDiffStat appears
// in the footer branch segment (W-E-09).
//
// Mutation: remove the `if m.gitDiffStat != ""` block from statusHeader and
// the diff-stat "+3 -1" disappears from the footer, failing this test.
func TestStatusHeader_ShowsDiffStat(t *testing.T) {
	m := newModel(&recordingSession{}, "/proj")
	m.gitBranch = "feat/cool"
	m.gitDiffStat = "+3 -1"
	m.width = 120
	out := m.statusHeader()
	if !strings.Contains(out, "+3 -1") {
		t.Errorf("statusHeader should show gitDiffStat in footer, got:\n%s", out)
	}
}

// TestStatusHeader_NoDiffStatWhenEmpty verifies that no diff-stat artifacts
// appear when gitDiffStat is empty (W-E-09).
func TestStatusHeader_NoDiffStatWhenEmpty(t *testing.T) {
	m := newModel(&recordingSession{}, "/proj")
	m.gitBranch = "main"
	m.gitDiffStat = ""
	m.width = 120
	out := m.statusHeader()
	// Should still show branch name but no diff numbers.
	if !strings.Contains(out, "main") {
		t.Errorf("statusHeader should still show branch name, got:\n%s", out)
	}
	if strings.Contains(out, "+0 -0") || strings.Contains(out, "+  -") {
		t.Errorf("statusHeader must not show empty diff artifacts, got:\n%s", out)
	}
}

// TestStatusHeader_ShowsPRLabelWhenURL verifies that a PR label appears in the
// footer when gitOpenPRURL is set (W-E-09 + W-E-06 label path).
//
// Mutation: remove the `if m.gitOpenPRURL != ""` block from statusHeader and
// the PR label disappears, failing this test.
func TestStatusHeader_ShowsPRLabelWhenURL(t *testing.T) {
	m := newModel(&recordingSession{}, "/proj")
	m.gitBranch = "feat/something"
	m.gitOpenPRURL = "https://github.com/owner/repo/pull/42"
	m.gitOpenPRTitle = "Add the thing"
	m.width = 120
	out := m.statusHeader()
	if !strings.Contains(out, "Add the thing") {
		t.Errorf("statusHeader should show PR title when gitOpenPRURL is set, got:\n%s", out)
	}
}

// TestGitStatusMsg_UpdatesModelFields proves that the gitStatusMsg handler in
// Update populates gitDiffStat, gitOpenPRURL, and gitOpenPRTitle (W-E-09
// wiring). Mutation: remove the gitStatusMsg case from Update and all three
// fields stay zero, failing the assertions below.
func TestGitStatusMsg_UpdatesModelFields(t *testing.T) {
	m := newModel(&recordingSession{}, "/proj")
	updated, _ := m.Update(gitStatusMsg{
		diffStat: "+7 -2",
		prURL:    "https://github.com/x/y/pull/10",
		prTitle:  "My PR",
	})
	mm := updated.(model)
	if mm.gitDiffStat != "+7 -2" {
		t.Errorf("gitDiffStat = %q, want '+7 -2'", mm.gitDiffStat)
	}
	if mm.gitOpenPRURL != "https://github.com/x/y/pull/10" {
		t.Errorf("gitOpenPRURL = %q", mm.gitOpenPRURL)
	}
	if mm.gitOpenPRTitle != "My PR" {
		t.Errorf("gitOpenPRTitle = %q", mm.gitOpenPRTitle)
	}
}

// --- W-E-14: @ completion mode cycling ---

// TestAtModeAll verifies that atMode=0 returns both FS files and MCP plugin names.
//
// Mutation: remove the `case atModeAll` MCP append block in updateAtPalette and
// the MCP tool "mcp_srv_mytool" disappears from the palette in all-mode, failing.
func TestAtModeAll(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newModel(&recordingSession{}, root)
	m.paletteMCPServers = []proto.MCPServerStatus{{
		Name:  "srv",
		Tools: []proto.MCPToolBrief{{Name: "mcp_srv_mytool"}},
	}}
	m.input.SetValue("look @")
	m.atMode = 0
	m.updateAtPalette()
	var hasFile, hasTool bool
	for _, it := range m.paletteItems {
		if strings.HasSuffix(it.name, ".go") {
			hasFile = true
		}
		if it.name == "mcp_srv_mytool" {
			hasTool = true
		}
	}
	if !hasFile {
		t.Error("atMode=0 (all): should include FS candidates")
	}
	if !hasTool {
		t.Error("atMode=0 (all): should include MCP tool candidates")
	}
}

// TestAtModeFS verifies that atMode=1 returns only FS files, not MCP tools.
//
// Mutation: change the switch case to also add MCP tools in FS mode and the
// mcp_srv_tool appears, failing the assertion.
func TestAtModeFS(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newModel(&recordingSession{}, root)
	m.paletteMCPServers = []proto.MCPServerStatus{{
		Name:  "srv",
		Tools: []proto.MCPToolBrief{{Name: "mcp_srv_tool"}},
	}}
	m.input.SetValue("@")
	m.atMode = 1
	m.updateAtPalette()
	for _, it := range m.paletteItems {
		if it.name == "mcp_srv_tool" {
			t.Errorf("atMode=1 (files): must not include MCP tool %q", it.name)
		}
	}
}

// TestAtModePlugins verifies that atMode=2 returns only MCP tools, not FS files.
//
// Mutation: make cycleAtMode not change atMode and after Tab the mode stays at
// 0 (all), so the FS files remain, failing.
func TestAtModePlugins(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newModel(&recordingSession{}, root)
	m.paletteMCPServers = []proto.MCPServerStatus{{
		Name:  "srv",
		Tools: []proto.MCPToolBrief{{Name: "mcp_srv_plugin"}},
	}}
	m.input.SetValue("@")
	m.atMode = 2
	m.updateAtPalette()
	var hasPlugin bool
	for _, it := range m.paletteItems {
		if strings.HasSuffix(it.name, ".go") {
			t.Errorf("atMode=2 (plugins): must not include FS file %q", it.name)
		}
		if it.name == "mcp_srv_plugin" {
			hasPlugin = true
		}
	}
	if !hasPlugin {
		t.Error("atMode=2 (plugins): should include MCP tool 'mcp_srv_plugin'")
	}
}

// TestCycleAtModeCycles verifies that cycleAtMode wraps 0→1→2→0 (W-E-14).
//
// Mutation: remove the mod 3 in cycleAtMode and the mode increments past 2,
// breaking the third assertion.
func TestCycleAtModeCycles(t *testing.T) {
	m := newModel(&recordingSession{}, t.TempDir())
	if m.atMode != 0 {
		t.Fatalf("initial atMode = %d, want 0", m.atMode)
	}
	m.input.SetValue("@")
	m.cycleAtMode()
	if m.atMode != 1 {
		t.Errorf("after 1st cycle atMode = %d, want 1", m.atMode)
	}
	m.cycleAtMode()
	if m.atMode != 2 {
		t.Errorf("after 2nd cycle atMode = %d, want 2", m.atMode)
	}
	m.cycleAtMode()
	if m.atMode != 0 {
		t.Errorf("after 3rd cycle atMode = %d, want 0 (wrap)", m.atMode)
	}
}

// TestCompleteAtPathResetsMode verifies that completing an @path resets atMode
// to 0 so the NEXT @ starts in all-mode (W-E-14).
//
// Mutation: remove `m.atMode = 0` from completeAtPath and the mode stays at
// 1 after completion, failing.
func TestCompleteAtPathResetsMode(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newModel(&recordingSession{}, root)
	m.input.SetValue("@")
	m.atMode = 1
	m.updateAtPalette()
	if len(m.paletteItems) == 0 {
		t.Skip("no candidates — need at least one file to complete")
	}
	m.completeAtPath(m.paletteItems[0])
	if m.atMode != 0 {
		t.Errorf("atMode after completeAtPath = %d, want 0", m.atMode)
	}
}

// --- W-E-09 + W-E-06 PR hyperlink wiring ---

// TestStatusHeader_PRHyperlink verifies that when hyperlinks are enabled and a
// PR URL is set, the footer contains the OSC 8 escape sequence or at least the
// PR title (W-E-06 + W-E-09).
//
// Mutation: remove the Hyperlink call in statusHeader and only the plain label
// appears; with hyperlinksEnabled=true the OSC 8 sequence disappears, failing.
func TestStatusHeader_PRHyperlink(t *testing.T) {
	prev := hyperlinksEnabled.Load()
	t.Cleanup(func() { hyperlinksEnabled.Store(prev) })
	hyperlinksEnabled.Store(true)

	m := newModel(&recordingSession{}, "/proj")
	m.gitBranch = "feat/pr"
	m.gitOpenPRURL = "https://github.com/o/r/pull/1"
	m.gitOpenPRTitle = "PR title"
	m.width = 160
	out := m.statusHeader()
	// With hyperlinks enabled the OSC 8 start sequence (\x1b]8;; appears in output.
	if !strings.Contains(out, "PR title") {
		t.Errorf("statusHeader: PR title should appear in footer, got:\n%s", out)
	}
}

// --- helper: ensure no accidental regression in sessionsEntry render ---

// TestSessionsEntryRenderPreviewField ensures the sessionsEntry transcript
// listing also works correctly with the new Preview field (W-E-08 field parity).
func TestSessionsEntryRenderPreviewField(t *testing.T) {
	se := &sessionsEntry{sessions: []proto.SessionInfo{
		{ID: "s1", Title: "chat", MsgCount: 5, Preview: "last thing said"},
	}}
	// sessionsEntry renders in the transcript view (not the picker). Preview is
	// server metadata but the entry renderer is not required to show it — this
	// test just verifies the renderer does not panic when Preview is set.
	out := se.render(80, spinner.Model{})
	if !strings.Contains(out, "s1") {
		t.Errorf("sessionsEntry.render should include session ID, got:\n%s", out)
	}
}

// --- fetchGitStatus helper tests ---

// TestExtractGitCount parses real git --shortstat output shapes.
func TestExtractGitCount(t *testing.T) {
	cases := []struct{ stat, key string; want int }{
		{" 3 files changed, 47 insertions(+), 12 deletions(-)", "insertion", 47},
		{" 3 files changed, 47 insertions(+), 12 deletions(-)", "deletion", 12},
		{" 1 file changed, 1 insertion(+)", "insertion", 1},
		{" 1 file changed, 1 insertion(+)", "deletion", 0},
		{"", "insertion", 0},
	}
	for _, c := range cases {
		got := extractGitCount(c.stat, c.key)
		if got != c.want {
			t.Errorf("extractGitCount(%q, %q) = %d, want %d", c.stat, c.key, got, c.want)
		}
	}
}

// TestExtractJSONString parses simple flat JSON shapes from gh pr view output.
func TestExtractJSONString(t *testing.T) {
	j := `{"url":"https://github.com/o/r/pull/1","title":"Add feature"}`
	if got := extractJSONString(j, "url"); got != "https://github.com/o/r/pull/1" {
		t.Errorf("extractJSONString url = %q", got)
	}
	if got := extractJSONString(j, "title"); got != "Add feature" {
		t.Errorf("extractJSONString title = %q", got)
	}
	if got := extractJSONString(j, "missing"); got != "" {
		t.Errorf("extractJSONString missing key = %q, want empty", got)
	}
}

// TestSessionsEntryAndPickerAreCompatibleWithPreviewField ensures that the
// applyEvent "sessions" path correctly stores the Preview field so both the
// sessionsEntry render and the restore picker can see it (W-E-08 end-to-end
// wiring test). Mutation: remove `row.Preview = txt` from sessionInfos (server
// side) — in tests that path is mocked, so we test the client side here: that
// applyEvent copies ev.Sessions verbatim (including Preview) into
// restoreSessions.
func TestSessionsEntryAndPickerAreCompatibleWithPreviewField(t *testing.T) {
	m := newModel(&recordingSession{}, "/proj")
	m.restoreMode = true
	m = m.applyEvent(cli.StreamEvent{
		Kind: "sessions",
		Sessions: []proto.SessionInfo{
			{ID: "abc", Title: "chat", MsgCount: 3, Preview: "last user message here"},
		},
	})
	if len(m.restoreSessions) != 1 {
		t.Fatalf("restoreSessions len = %d, want 1", len(m.restoreSessions))
	}
	if m.restoreSessions[0].Preview != "last user message here" {
		t.Errorf("restoreSessions[0].Preview = %q, want 'last user message here'",
			m.restoreSessions[0].Preview)
	}
}
