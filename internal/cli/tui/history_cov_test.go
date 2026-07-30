package tui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- LoadHistory ----

// TestCov_LoadHistory_DefaultCap covers the capacity<=0 → default branch.
func TestCov_LoadHistory_DefaultCap(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hist.jsonl")
	require.NoError(t, os.WriteFile(p, []byte(`{"text":"a","ts":"2020-01-01T00:00:00Z"}`+"\n"), 0o644))
	h, err := LoadHistory(p, 0) // capacity<=0 → default (500)
	require.NoError(t, err)
	assert.Len(t, h.Items(), 1)
}

// TestCov_LoadHistory_OpenError covers the non-NotExist open-error path (NUL
// byte in the path → invalid argument, not IsNotExist).
func TestCov_LoadHistory_OpenError(t *testing.T) {
	_, err := LoadHistory("bad\x00name.jsonl", 10)
	assert.Error(t, err)
}

// TestCov_LoadHistory_TrimToCap covers the trim-to-capacity branch: a file with
// more entries than the cap keeps only the most recent cap entries.
func TestCov_LoadHistory_TrimToCap(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hist.jsonl")
	content := `{"text":"a","ts":"2020-01-01T00:00:00Z"}` + "\n" +
		`{"text":"b","ts":"2020-01-01T00:00:01Z"}` + "\n" +
		`{"text":"c","ts":"2020-01-01T00:00:02Z"}` + "\n"
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	h, err := LoadHistory(p, 2) // cap 2, 3 entries → trim to last 2
	require.NoError(t, err)
	items := h.Items()
	require.Len(t, items, 2)
	assert.Equal(t, "b", items[0].Text, "oldest entry dropped")
	assert.Equal(t, "c", items[1].Text)
}

// ---- Save ----

// TestCov_HistorySave_MkdirAllError covers the MkdirAll error branch (a file
// blocks the directory creation).
func TestCov_HistorySave_MkdirAllError(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "sub"), []byte("x"), 0o644))
	h := &History{path: filepath.Join(tmp, "sub", "deep", "hist.jsonl"), cap: 10}
	assert.Error(t, h.Save())
}

// ---- historyPopup ----

// TestCov_HistoryPopup_ScrollWindow covers the cursor>=maxRows scroll branch:
// the popup window starts past the first item so the cursor stays visible.
func TestCov_HistoryPopup_ScrollWindow(t *testing.T) {
	items := make([]historyItem, 12)
	for i := range items {
		items[i] = historyItem{Text: "prompt-" + string(rune('a'+i)), TS: time.Now()}
	}
	m := newModel(&fakeSession{}, "/proj")
	m.width = 80
	m.historySearch = &historyState{visible: true, cursor: 9, items: items}
	out := m.historyPopup()
	assert.Contains(t, out, "History")
	assert.Contains(t, out, "▶", "the cursor row is marked")
}

// ---- historyPath ----

// TestCov_HistoryPath_NoConfigDir covers the UserConfigDir-error path.
func TestCov_HistoryPath_NoConfigDir(t *testing.T) {
	t.Setenv("APPDATA", "")
	t.Setenv("LOCALAPPDATA", "")
	if got := historyPath(); got != "" {
		t.Skipf("UserConfigDir did not error on this platform (got %q)", got)
	}
}
