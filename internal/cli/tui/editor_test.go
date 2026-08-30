package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestResolveEditorCommand_Precedence pins the $VISUAL > $EDITOR > platform
// default order (the same order git and crontab(1) use) and the whitespace
// split that lets "code -w" invoke an editor with its wait flag.
func TestResolveEditorCommand_Precedence(t *testing.T) {
	cases := []struct {
		name         string
		visual, edit string
		want         []string
	}{
		{"visual wins over editor", "vim -u NONE", "nano", []string{"vim", "-u", "NONE"}},
		{"editor used when visual unset", "", "nano", []string{"nano"}},
		{"blank visual falls through to editor", "   ", "nano", []string{"nano"}},
		{"platform default when both unset", "", "", nil}, // checked separately below
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("VISUAL", c.visual)
			t.Setenv("EDITOR", c.edit)
			got := resolveEditorCommand()
			if c.want == nil {
				if len(got) == 0 {
					t.Fatalf("resolveEditorCommand() returned no args")
				}
				return
			}
			if len(got) != len(c.want) {
				t.Fatalf("resolveEditorCommand() = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("resolveEditorCommand() = %v, want %v", got, c.want)
				}
			}
		})
	}
}

// TestExternalEditorMsg_MissingBinaryLeavesInputUnchanged proves failure mode
// 1 (the editor binary does not exist): Update must leave the textarea
// exactly as the user left it. The error is a REAL exec.Cmd.Run() failure —
// no fabricated error value — from invoking a binary name that cannot exist.
func TestExternalEditorMsg_MissingBinaryLeavesInputUnchanged(t *testing.T) {
	m := newTestModel(t)
	const original = "draft that must survive a missing editor"
	m.input.SetValue(original)

	tmp := filepath.Join(t.TempDir(), "scratch.md")
	if err := os.WriteFile(tmp, []byte("should never be read back"), 0o644); err != nil {
		t.Fatalf("seed temp file: %v", err)
	}

	runErr := exec.Command("yanshi-editor-that-definitely-does-not-exist-xyz").Run()
	if runErr == nil {
		t.Fatalf("expected a real 'binary not found' error, got nil")
	}

	mm, _ := m.Update(externalEditorMsg{tmpFile: tmp, err: runErr})
	m2 := mm.(model)

	if got := m2.input.Value(); got != original {
		t.Fatalf("input.Value() = %q, want unchanged %q", got, original)
	}
	if _, statErr := os.Stat(tmp); !os.IsNotExist(statErr) {
		t.Fatalf("temp file should have been removed even on failure, stat err = %v", statErr)
	}
}

// TestExternalEditorMsg_NonZeroExitLeavesInputUnchanged proves failure mode 2
// (the editor runs but exits non-zero): same guarantee, driven by a REAL
// non-zero exit from /usr/bin/false (or the shell builtin's PATH-resolved
// equivalent) rather than a fabricated *exec.ExitError.
func TestExternalEditorMsg_NonZeroExitLeavesInputUnchanged(t *testing.T) {
	if _, err := exec.LookPath("false"); err != nil {
		t.Skip("no `false` binary on PATH")
	}
	m := newTestModel(t)
	const original = "draft that must survive a non-zero exit"
	m.input.SetValue(original)

	tmp := filepath.Join(t.TempDir(), "scratch.md")
	if err := os.WriteFile(tmp, []byte("should never be read back"), 0o644); err != nil {
		t.Fatalf("seed temp file: %v", err)
	}

	runErr := exec.Command("false").Run()
	if runErr == nil {
		t.Fatalf("expected a real non-zero exit error, got nil")
	}

	mm, _ := m.Update(externalEditorMsg{tmpFile: tmp, err: runErr})
	m2 := mm.(model)

	if got := m2.input.Value(); got != original {
		t.Fatalf("input.Value() = %q, want unchanged %q", got, original)
	}
	if _, statErr := os.Stat(tmp); !os.IsNotExist(statErr) {
		t.Fatalf("temp file should have been removed even on failure, stat err = %v", statErr)
	}
}

// TestExternalEditorMsg_SuccessRefillsInput is the green complement to the two
// failure tests above: on a clean exit the temp file's content replaces the
// input box (trailing newline trimmed, since editors add one at EOF) and the
// temp file is cleaned up.
func TestExternalEditorMsg_SuccessRefillsInput(t *testing.T) {
	m := newTestModel(t)
	m.input.SetValue("stale text the editor is about to replace")

	tmp := filepath.Join(t.TempDir(), "scratch.md")
	if err := os.WriteFile(tmp, []byte("edited content\n"), 0o644); err != nil {
		t.Fatalf("seed temp file: %v", err)
	}

	mm, _ := m.Update(externalEditorMsg{tmpFile: tmp, err: nil})
	m2 := mm.(model)

	if got, want := m2.input.Value(), "edited content"; got != want {
		t.Fatalf("input.Value() = %q, want %q", got, want)
	}
	if _, statErr := os.Stat(tmp); !os.IsNotExist(statErr) {
		t.Fatalf("temp file should have been removed after a successful read, stat err = %v", statErr)
	}
}

// TestWriteEditorTempFile_ContainsInputText proves the scratch file the
// editor is pointed at actually contains what was in the input box.
func TestWriteEditorTempFile_ContainsInputText(t *testing.T) {
	tmpFile, err := writeEditorTempFile("hello from the input box")
	if err != nil {
		t.Fatalf("writeEditorTempFile: %v", err)
	}
	defer os.Remove(tmpFile)
	data, readErr := os.ReadFile(tmpFile)
	if readErr != nil {
		t.Fatalf("read temp file: %v", readErr)
	}
	if string(data) != "hello from the input box" {
		t.Fatalf("temp file content = %q, want %q", string(data), "hello from the input box")
	}
}

// TestBuildEditorCmd_RunsAndLeavesFileIntact drives buildEditorCmd's *exec.Cmd
// through a REAL Run() (EDITOR=true, a no-op that exits 0 without touching
// its argument) end to end: no error, and the temp file this census entry
// (internal/archtest/spawnenv_test.go's editor.go row) says inherits the
// parent environment actually runs to completion.
func TestBuildEditorCmd_RunsAndLeavesFileIntact(t *testing.T) {
	if _, err := exec.LookPath("true"); err != nil {
		t.Skip("no `true` binary on PATH")
	}
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "true")
	tmpFile, err := writeEditorTempFile("hello from the input box")
	if err != nil {
		t.Fatalf("writeEditorTempFile: %v", err)
	}
	defer os.Remove(tmpFile)

	cmd := buildEditorCmd(tmpFile)
	if runErr := cmd.Run(); runErr != nil {
		t.Fatalf("editor command failed: %v", runErr)
	}
	data, readErr := os.ReadFile(tmpFile)
	if readErr != nil {
		t.Fatalf("read temp file: %v", readErr)
	}
	if string(data) != "hello from the input box" {
		t.Fatalf("temp file content = %q, want unchanged %q", string(data), "hello from the input box")
	}
}

// TestStartExternalEditor_ReturnsNonNilCmd is a thin smoke test over the
// composed constructor: it must succeed and hand back something to run. The
// substantive coverage (content round-trip, both failure modes) lives in the
// tests above and in the externalEditorMsg tests, which exercise the pieces
// startExternalEditor composes without needing to unpack bubbletea's private
// execMsg wrapper.
func TestStartExternalEditor_ReturnsNonNilCmd(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "true")
	before, _ := filepath.Glob(filepath.Join(os.TempDir(), "yanshi-edit-*"))
	existed := map[string]bool{}
	for _, p := range before {
		existed[p] = true
	}

	cmd, err := startExternalEditor("draft")
	if err != nil {
		t.Fatalf("startExternalEditor: %v", err)
	}
	if cmd == nil {
		t.Fatalf("startExternalEditor returned a nil tea.Cmd")
	}

	// startExternalEditor's temp file is only reachable through the private
	// tea.execMsg the returned Cmd wraps (see the doc comment above), so
	// clean it up by diffing the scratch-file glob rather than unpacking
	// bubbletea internals.
	after, _ := filepath.Glob(filepath.Join(os.TempDir(), "yanshi-edit-*"))
	for _, p := range after {
		if !existed[p] {
			os.Remove(p)
		}
	}
}

// TestCtrlE_BlockedWhileModalOpen proves Ctrl+E is a no-op while a popup owns
// the keyboard, mirroring Ctrl+S's guard, instead of tearing down the
// terminal out from under an open picker.
func TestCtrlE_BlockedWhileModalOpen(t *testing.T) {
	m := newTestModel(t)
	m.helpVisible = true
	const original = "unchanged while help is open"
	m.input.SetValue(original)

	mm, cmd, handled := m.handleKeyMsg(tea.KeyMsg{Type: tea.KeyCtrlE})
	if !handled {
		t.Fatalf("Ctrl+E should be handled (consumed) while a modal is open")
	}
	if cmd != nil {
		t.Fatalf("Ctrl+E should be a pure no-op while a modal is open, got a non-nil Cmd")
	}
	if got := mm.input.Value(); got != original {
		t.Fatalf("input.Value() = %q, want unchanged %q", got, original)
	}
}
