package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/task/work"
)

// ---- entry renders previously at 0% coverage ----

// TestPermissionsEntry_Render covers the permissions list entry: empty list
// renders "(none)", a populated list renders one row per rule.
func TestPermissionsEntry_Render(t *testing.T) {
	out := permissionsEntry{}.render(80, spinner.Model{})
	assert.Contains(t, out, "(none)")

	out = permissionsEntry{items: []proto.PermissionInfo{
		{ID: "r1", Action: "allow", TTL: "session", Scope: "fs_read"},
	}}.render(80, spinner.Model{})
	assert.Contains(t, out, "r1")
	assert.Contains(t, out, "allow")
	assert.Contains(t, out, "fs_read")
}

// TestSeamsEntry_Render covers the reversible-seams list entry: empty and
// populated.
func TestSeamsEntry_Render(t *testing.T) {
	out := seamsEntry{}.render(80, spinner.Model{})
	assert.Contains(t, out, "no reversible seams")

	out = seamsEntry{items: []proto.SeamInfo{
		{ID: "s1", CommitShort: "abc1234", Label: "before edit"},
		{ID: "s2", CommitShort: "def5678", Label: ""},
	}}.render(80, spinner.Model{})
	assert.Contains(t, out, "s1")
	assert.Contains(t, out, "abc1234")
	assert.Contains(t, out, "before edit")
	assert.Contains(t, out, "(no label)", "empty label falls back")
}

// TestSeamRestoredEntry_Render covers the post-restore confirmation, with and
// without the undo hint.
func TestSeamRestoredEntry_Render(t *testing.T) {
	out := seamRestoredEntry{summary: "3 turns"}.render(80, spinner.Model{})
	assert.Contains(t, out, "restored")
	assert.NotContains(t, out, "undo:")

	out = seamRestoredEntry{undoID: "s9", summary: "3 turns"}.render(80, spinner.Model{})
	assert.Contains(t, out, "undo:")
	assert.Contains(t, out, "s9")
}

// TestSeamRestorePromptEntry_Render covers the confirmation prompt entry.
func TestSeamRestorePromptEntry_Render(t *testing.T) {
	out := seamRestorePromptEntry{seamID: "s7"}.render(80, spinner.Model{})
	assert.Contains(t, out, "s7")
	assert.Contains(t, out, "/restore-turn s7 yes")
}

// TestTaskUpdateEntry_Render covers the task progress entry (uses CompletionPct).
func TestTaskUpdateEntry_Render(t *testing.T) {
	e := taskUpdateEntry{task: work.WorkTask{
		ID: "T1", Status: "in_progress", Title: "do the thing",
		Checklist: work.Checklist{Items: []work.ChecklistItem{
			{Content: "a", Status: work.ChecklistDone},
			{Content: "b", Status: work.ChecklistPending},
		}},
	}}
	out := e.render(80, spinner.Model{})
	assert.Contains(t, out, "T1")
	assert.Contains(t, out, "50%")
	assert.Contains(t, out, "do the thing")
}

// TestPlanAndChecklistUpdateEntry_Render covers planUpdateEntry (the three item
// statuses) and checklistUpdateEntry (which delegates to planUpdateEntry).
func TestPlanAndChecklistUpdateEntry_Render(t *testing.T) {
	cl := work.Checklist{Items: []work.ChecklistItem{
		{Content: "done item", Status: work.ChecklistDone},
		{Content: "active item", Status: work.ChecklistInProgress},
		{Content: "todo item", Status: work.ChecklistPending},
	}}
	out := planUpdateEntry{taskID: "P1", checklist: cl}.render(80, spinner.Model{})
	assert.Contains(t, out, "P1")
	assert.Contains(t, out, "[x] done item")
	assert.Contains(t, out, "[-] active item")
	assert.Contains(t, out, "[ ] todo item")

	// checklistUpdateEntry delegates to planUpdateEntry.
	out2 := checklistUpdateEntry{taskID: "C1", checklist: cl}.render(80, spinner.Model{})
	assert.Contains(t, out2, "C1")
}

// TestSkillsEntry_Render covers the skills list entry: empty and populated.
func TestSkillsEntry_Render(t *testing.T) {
	out := skillsEntry{}.render(80, spinner.Model{})
	assert.Contains(t, out, "no skills installed")

	out = skillsEntry{skills: []proto.SkillInfo{
		{Name: "debug"}, {Name: "plan"},
	}}.render(80, spinner.Model{})
	assert.Contains(t, out, "debug")
	assert.Contains(t, out, "plan")
}

// ---- pure helper functions ----

// TestHeadLines covers the head-window helper (used by overwrite-mode panels).
func TestHeadLines(t *testing.T) {
	assert.Equal(t, "", headLines("", 3))
	assert.Equal(t, "", headLines("x", 0))
	assert.Equal(t, "a\nb\nc", headLines("a\nb\nc", 5), "fewer lines than n returns all")
	assert.Equal(t, "a\nb", headLines("a\nb\nc\nd", 2))
	assert.Equal(t, "a", headLines("a\n\n\n", 1), "trailing newlines trimmed first")
}

// TestIsAsciiDigits covers the line-number token check.
func TestIsAsciiDigits(t *testing.T) {
	assert.False(t, isAsciiDigits(""))
	assert.True(t, isAsciiDigits("0"))
	assert.True(t, isAsciiDigits("12345"))
	assert.False(t, isAsciiDigits("12a"))
	assert.False(t, isAsciiDigits(" 1"))
}

// TestFormatBytesThresholds covers the compact byte-count formatter thresholds.
func TestFormatBytesThresholds(t *testing.T) {
	assert.Equal(t, "0 B", formatBytes(0))
	assert.Equal(t, "1023 B", formatBytes(1023))
	assert.Equal(t, "1.0 KB", formatBytes(1024))
	assert.Equal(t, "1.0 MB", formatBytes(1024*1024))
}

// TestPluralCount covers the singular/plural label helper.
func TestPluralCount(t *testing.T) {
	assert.Equal(t, "1 line", pluralCount(1, "line"))
	assert.Equal(t, "3 lines", pluralCount(3, "line"))
}

// TestToolResultOutput covers the shell_run JSON output extractor and its raw
// fallback.
func TestToolResultOutput(t *testing.T) {
	assert.Equal(t, "", toolResultOutput(""))
	assert.Equal(t, "hello", toolResultOutput(`{"output":"hello","exit":0}`))
	assert.Equal(t, "raw", toolResultOutput("raw"), "non-JSON falls back to raw")
}

// TestParseGitHead covers the .git/HEAD parser: branch ref, detached sha, short.
func TestParseGitHead(t *testing.T) {
	assert.Equal(t, "main", parseGitHead("ref: refs/heads/main"))
	assert.Equal(t, ":abc1234", parseGitHead("abc1234deadbeef"))
	assert.Equal(t, "ab", parseGitHead("ab"), "short non-ref string returned as-is")
}

// TestDirName covers the footer directory-name helper.
func TestDirName(t *testing.T) {
	assert.Equal(t, "", dirName(""))
	assert.Equal(t, "proj", dirName("/x/y/proj"))
	assert.Equal(t, "proj", dirName("proj"))
}

// TestScalarToString covers the JSON-decoded scalar stringifier.
func TestScalarToString(t *testing.T) {
	assert.Equal(t, "", scalarToString(nil))
	assert.Equal(t, "hi", scalarToString("hi"))
	assert.Equal(t, "true", scalarToString(true))
	assert.Equal(t, "42", scalarToString(float64(42)))
	assert.Equal(t, "3.14", scalarToString(float64(3.14)))
	assert.Equal(t, "[1 2]", scalarToString([]int{1, 2}), "default falls back to fmt.Sprint")
}

// TestThemeNames covers the comma-separated theme list (used in /theme errors).
func TestThemeNames(t *testing.T) {
	got := themeNames()
	assert.Contains(t, got, "default")
	assert.Contains(t, got, ", ")
	// themeByName lookup for each known theme.
	for _, tn := range []ThemeName{ThemeDefault, ThemeHighContrast, ThemeMuted} {
		_, ok := themeByName(tn)
		assert.True(t, ok, "theme %q found", tn)
	}
	_, ok := themeByName("nonexistent")
	assert.False(t, ok)
}

// TestShortenPath covers the path-shortener edge cases.
func TestShortenPath(t *testing.T) {
	assert.Equal(t, "", shortenPath("", "/root"))
	assert.Equal(t, ".", shortenPath("/root", "/root"))
	assert.Equal(t, "sub/a.go", shortenPath("/root/sub/a.go", "/root"))
	assert.Equal(t, "a.go", shortenPath("/elsewhere/a.go", "/root"), "abs outside root -> basename")
	assert.Equal(t, "rel/path", shortenPath("rel/path", "/root"))
}

// ---- toast queue ----

// TestToastQueue_DismissAndHasError covers dismissLastError (removes the newest
// error) and hasErrorToast (reports error presence).
func TestToastQueue_DismissAndHasError(t *testing.T) {
	var q toastQueue
	require.False(t, q.hasErrorToast())
	q.push(toast{Level: "info", Text: "i"})
	q.push(toast{Level: "error", Text: "e1"})
	q.push(toast{Level: "warn", Text: "w"})
	q.push(toast{Level: "error", Text: "e2"})
	require.True(t, q.hasErrorToast())

	q.dismissLastError() // removes e2 (newest error)
	assert.True(t, q.hasErrorToast(), "e1 still present")
	// Verify e1 is now the last error: dismiss again removes it.
	q.dismissLastError()
	assert.False(t, q.hasErrorToast())
}

// TestToastQueue_BlockHeight covers the stack-height calculation.
func TestToastQueue_BlockHeight(t *testing.T) {
	var q toastQueue
	assert.Equal(t, 0, q.blockHeight())
	q.push(toast{Level: "info", Text: "i"})
	assert.Equal(t, 2, q.blockHeight(), "1 item -> 1 line + 1 padding")
}

// TestToastQueue_Render covers the render path (the switch over levels).
func TestToastQueue_Render(t *testing.T) {
	var q toastQueue
	assert.Equal(t, "", q.render(60), "empty queue renders nothing")
	q.push(toast{Level: "info", Text: "info-text"})
	q.push(toast{Level: "warn", Text: "warn-text"})
	q.push(toast{Level: "error", Text: "err-text"})
	q.push(toast{Level: "weird", Text: "other"}) // default branch
	out := q.render(60)
	for _, want := range []string{"info-text", "warn-text", "err-text", "other"} {
		assert.Contains(t, out, want)
	}
}

// ---- debouncer ----

// TestDebouncer_ScheduleConsume proves schedule arms at most one tick and
// consume re-arms it for the next schedule.
func TestDebouncer_ScheduleConsume(t *testing.T) {
	var d debouncer
	require.False(t, d.pending)
	cmd1 := d.schedule()
	require.NotNil(t, cmd1, "first schedule arms a tick")
	assert.True(t, d.pending)

	cmd2 := d.schedule()
	assert.Nil(t, cmd2, "second schedule while pending is a no-op")

	d.consume()
	assert.False(t, d.pending, "consume clears the pending flag")
	cmd3 := d.schedule()
	assert.NotNil(t, cmd3, "after consume, schedule arms a fresh tick")
}

// ---- preferences ----

// TestOptionalBool covers the env-bool parser: empty, valid, invalid.
func TestOptionalBool(t *testing.T) {
	v, err := optionalBool("")
	require.NoError(t, err)
	assert.Nil(t, v)

	v, err = optionalBool("true")
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.True(t, *v)

	v, err = optionalBool("0")
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.False(t, *v)

	_, err = optionalBool("maybe")
	require.Error(t, err)
}

// TestPreferencesFromEnv covers the env layer constructor: populated values,
// valid bools, and an invalid bool that fails.
func TestPreferencesFromEnv(t *testing.T) {
	env := map[string]string{
		"YANSHI_UI_LOCALE":     " zh-CN ",
		"YANSHI_THEME":         "muted",
		"YANSHI_KEYMAP":        "default",
		"YANSHI_HIGH_CONTRAST": "true",
		"YANSHI_VIM":           "false",
	}
	p, err := PreferencesFromEnv(func(k string) string { return env[k] })
	require.NoError(t, err)
	assert.Equal(t, "zh-CN", p.UILocale)
	assert.Equal(t, "muted", p.ThemeName)
	assert.Equal(t, "default", p.KeymapName)
	require.NotNil(t, p.HighContrast)
	assert.True(t, *p.HighContrast)
	require.NotNil(t, p.Vim)
	assert.False(t, *p.Vim)

	_, err = PreferencesFromEnv(func(k string) string {
		if k == "YANSHI_VIM" {
			return "nope"
		}
		return ""
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "YANSHI_VIM")
}

// TestPreferencesPath proves the path helper returns a non-empty prefs path.
func TestPreferencesPath(t *testing.T) {
	p := preferencesPath()
	assert.NotEmpty(t, p)
	assert.True(t, strings.HasSuffix(p, "prefs.json"), "path = %q", p)
}
