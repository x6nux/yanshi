// Package tui — golden test for the Update method.
//
// This file records a deterministic snapshot of model state after a scripted
// sequence of tea.Msg values and compares it against a golden file. It acts
// as a regression guard against accidental behavioral changes in Update.
//
// USE: go test ./internal/cli/tui -run TestUpdateGolden -update   (regenerate)
//      go test ./internal/cli/tui -run TestUpdateGolden           (verify)
package tui

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

var updateGoldenFlag = flag.Bool("update", false, "regenerate update_golden.txt")

// snapshotModel returns a deterministic string key=value snapshot of the model.
// Avoids time-dependent fields (View(), timestamps, etc.).
func snapshotModel(m model) string {
	var b strings.Builder
	b.WriteString("input=")
	b.WriteString(m.input.Value())
	b.WriteString("\n")
	fmt.Fprintf(&b, "width=%d\n", m.width)
	fmt.Fprintf(&b, "height=%d\n", m.height)
	fmt.Fprintf(&b, "entries=%d\n", len(m.entries))
	fmt.Fprintf(&b, "pending=%q\n", m.pending)
	fmt.Fprintf(&b, "helpVisible=%v\n", m.helpVisible)
	fmt.Fprintf(&b, "pickerKind=%q\n", m.pickerKind)
	fmt.Fprintf(&b, "canceling=%v\n", m.canceling)
	fmt.Fprintf(&b, "permMode=%q\n", m.permMode)
	fmt.Fprintf(&b, "queueMode=%s\n", m.queueMode.String())
	fmt.Fprintf(&b, "streamCh=%v\n", m.streamCh != nil)
	fmt.Fprintf(&b, "autoProcessing=%v\n", m.autoProcessing)
	fmt.Fprintf(&b, "theme=%q\n", m.theme)
	return b.String()
}

// scriptedMsgs returns a deterministic sequence of messages for the golden
// test. Each message exercises a different Update path.
func scriptedMsgs() []tea.Msg {
	return []tea.Msg{
		tea.WindowSizeMsg{Width: 80, Height: 24},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")},
		tea.KeyMsg{Type: tea.KeyEnter},
		tea.KeyMsg{Type: tea.KeyEscape},
	}
}

func TestUpdateGolden(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	var sb strings.Builder
	for i, msg := range scriptedMsgs() {
		mm, _ := m.Update(msg)
		m = mm.(model)
		fmt.Fprintf(&sb, "=== step %d (%s) ===\n", i, typeStr(msg))
		sb.WriteString(snapshotModel(m))
		sb.WriteString("\n")
	}
	got := sb.String()
	goldenPath := filepath.Join("testdata", "update_golden.txt")
	if *updateGoldenFlag {
		if err := os.MkdirAll("testdata", 0755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0644); err != nil {
			t.Fatalf("write golden %s: %v", goldenPath, err)
		}
		t.Logf("golden regenerated: %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update first)", goldenPath, err)
	}
	if string(want) != got {
		t.Fatalf("update golden mismatch:\n--- want\n%s\n--- got\n%s", string(want), got)
	}
}

// typeStr returns the short type name of v, stripping common prefixes.
func typeStr(v any) string {
	s := fmt.Sprintf("%T", v)
	s = strings.TrimPrefix(s, "*")
	s = strings.TrimPrefix(s, "tea.")
	s = strings.TrimPrefix(s, "bubbletea.")
	return s
}
