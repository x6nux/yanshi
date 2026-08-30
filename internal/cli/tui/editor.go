// Package tui — external editor support (W-E-12).
//
// Ctrl+E hands the input box to $VISUAL/$EDITOR (or a platform default) via
// tea.ExecProcess, exactly like `git commit` or `crontab -e` do. The
// terminal-release/restore dance is handled entirely by the vendored
// bubbletea fork's unmodified Exec machinery (third_party/bubbletea/exec.go)
// — this file only has to build the *exec.Cmd and read the result back.
package tui

import (
	"os"
	"os/exec"
	"runtime"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// resolveEditorCommand picks the external editor to run for Ctrl+E, in the
// same precedence git and crontab(1) use: $VISUAL, then $EDITOR, then a
// platform default. The chosen string is split on whitespace so a command
// line like "code -w" invokes the editor with its wait flag.
//
// ponytail: whitespace splitting does not understand quoting, so an editor
// path containing a space (rare outside "C:\Program Files\...") needs a
// wrapper script on PATH instead. Upgrade to a shell-based split if that is
// ever reported.
func resolveEditorCommand() []string {
	for _, name := range []string{"VISUAL", "EDITOR"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return strings.Fields(v)
		}
	}
	if runtime.GOOS == "windows" {
		return []string{"notepad"}
	}
	return []string{"vi"}
}

// externalEditorMsg reports that the external editor process launched by
// startExternalEditor has exited. err is whatever exec.Cmd.Run returned:
// nil on a clean exit, non-nil for a missing editor binary or a non-zero
// exit code. Update handles both non-nil cases identically — discard the
// temp file, leave the input box exactly as the user left it — because a
// spawn failure must never lose already-typed text.
type externalEditorMsg struct {
	tmpFile string
	err     error
}

// startExternalEditor writes text to a temp file and returns a tea.Cmd that
// hands the terminal to the resolved editor via tea.ExecProcess. The
// returned error is only a temp-file failure (disk full, permissions); a
// missing editor binary or a non-zero exit is not known yet at this point —
// it surfaces later, asynchronously, as externalEditorMsg.err once the
// process has actually been tried.
func startExternalEditor(text string) (tea.Cmd, error) {
	tmpFile, err := writeEditorTempFile(text)
	if err != nil {
		return nil, err
	}
	cmd := buildEditorCmd(tmpFile)
	// cmd.Env left nil: INHERITS the parent environment deliberately. This
	// is a user-invoked, synchronous, foreground editor sharing the same
	// terminal as the running yanshi process — the same posture
	// internal/shell/snapshot.go documents for capturing the operator's own
	// login shell. Nothing the editor reads leaves the machine on its own,
	// and stripping PATH/HOME/editor-plugin variables would break the
	// operator's own editor configuration for a purely local, foreground
	// edit. See spawnEnvCensus's entry for this file
	// (internal/archtest/spawnenv_test.go).
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return externalEditorMsg{tmpFile: tmpFile, err: err}
	}), nil
}

// writeEditorTempFile creates the scratch file the editor will open, seeded
// with the input box's current text, and returns its path.
func writeEditorTempFile(text string) (string, error) {
	f, err := os.CreateTemp("", "yanshi-edit-*.md")
	if err != nil {
		return "", err
	}
	tmpFile := f.Name()
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		os.Remove(tmpFile)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpFile)
		return "", err
	}
	return tmpFile, nil
}

// buildEditorCmd resolves $VISUAL/$EDITOR/vi and points it at tmpFile.
// Split out from startExternalEditor so a test can inspect the built
// *exec.Cmd directly instead of unpacking bubbletea's private execMsg.
func buildEditorCmd(tmpFile string) *exec.Cmd {
	parts := resolveEditorCommand()
	args := append(append([]string{}, parts[1:]...), tmpFile)
	return exec.Command(parts[0], args...)
}
