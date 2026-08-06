package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestAtPathBecomesAnAttachmentOnTheWire is the TUI half of UX3: typing @file
// has to put a reference on the turn, or the server-side resolver has no
// caller and the feature exists only in tests.
func TestAtPathBecomesAnAttachmentOnTheWire(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := &recordingSession{}
	m := newModel(rec, root)
	m.input.SetValue("summarize @notes.md please")
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(model)

	if len(rec.frames) != 1 {
		t.Fatalf("want one user_message frame, got %v", frameTypes(rec.frames))
	}
	fr := rec.frames[0]
	if len(fr.Attachments) != 1 {
		t.Fatalf("the turn carries no attachment: %+v", fr)
	}
	if !strings.HasSuffix(filepath.ToSlash(fr.Attachments[0].Path), "/notes.md") {
		t.Errorf("attachment path = %q", fr.Attachments[0].Path)
	}
	if fr.Attachments[0].Path == "notes.md" {
		t.Error("the path must be absolute: the server resolves it against the work " +
			"root, and a relative one would depend on the server's cwd")
	}
	// The @token stays in the prompt: the model should see what was asked, and
	// the server prepends the contents ahead of it.
	if !strings.Contains(fr.Text, "@notes.md") {
		t.Errorf("the @token was stripped from the prompt: %q", fr.Text)
	}
}

func TestExtractAttachRefsIgnoresNonFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, text string
		want       int
	}{
		{"a real file is picked up", "see @real.txt", 1},
		{"a nonexistent path is not", "ping @nope.txt", 0},
		{"a directory is not a file", "look in @adir", 0},
		{"an email address is not an attachment", "mail me@real.txt now", 0},
		{"a bare @ is not", "reply @ me", 0},
		{"trailing punctuation is trimmed", "read @real.txt.", 1},
		{"the same file twice yields one ref", "@real.txt and @real.txt", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(extractAttachRefs(tc.text, root)); got != tc.want {
				t.Errorf("got %d refs, want %d", got, tc.want)
			}
		})
	}
}
