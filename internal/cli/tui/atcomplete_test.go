package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAtCompletionIsOrderedByFrecency is the first observable consumer of
// Frecency.TopN, which had none.
//
// TopN was written, tested and never called by anything in production. The
// acceptance criterion says recently-used files sort first, and until the
// @path completion existed there was no surface on which that could be true or
// false — which is why this task had to follow UX3 rather than ship beside it.
func TestAtCompletionIsOrderedByFrecency(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"alpha.go", "zeta.go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	m := newModel(&recordingSession{}, root)
	// zeta was touched; alpha was not. Alphabetically alpha wins, so an
	// unsorted implementation produces the opposite order.
	m.frecency.Record(filepath.Join(root, "zeta.go"))

	got := m.atCandidates("")
	if len(got) < 2 {
		t.Fatalf("want at least 2 candidates, got %v", got)
	}
	if got[0] != "zeta.go" {
		t.Errorf("candidates = %v: the recently-used file must sort first, or "+
			"Frecency.TopN still has no consumer that can be observed", got)
	}
}

// TestAtCompletionStaysInsideTheProject pins the defect the audit missed.
//
// frecencyPath declared a root parameter and never used it, while newModel
// earnestly passed one in — so the store looked per-project and was global.
// Two projects share one file, and without a filter project A offers project
// B's paths, which is both wrong and a small information leak between
// unrelated workspaces.
func TestAtCompletionStaysInsideTheProject(t *testing.T) {
	projA := t.TempDir()
	projB := t.TempDir()
	if err := os.WriteFile(filepath.Join(projA, "a.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projB, "b.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newModel(&recordingSession{}, projA)
	m.frecency.Record(filepath.Join(projB, "b.go"))
	m.frecency.Record(filepath.Join(projA, "a.go"))

	got := m.atCandidates("")
	for _, c := range got {
		// A leaked path arrives as "../<other-tmpdir>/b.go", not as "b.go" —
		// atCandidates makes every candidate relative to THIS root. The first
		// version of this assertion checked for the bare name and for an
		// absolute path in projB, and a mutation removing the root filter
		// entirely left it green.
		if strings.HasPrefix(filepath.ToSlash(c), "../") || filepath.IsAbs(c) {
			t.Fatalf("candidate %q escapes the project: %v", c, got)
		}
		if strings.HasSuffix(c, "b.go") {
			t.Fatalf("candidates leak another project's files: %v", got)
		}
	}
	if len(got) == 0 {
		t.Fatal("no candidates at all, so the assertions above are vacuous")
	}
}

// TestFrecencyCanBeDisabled covers the config switch. Off must stop BOTH the
// recording and the ordering: leaving recording on would keep building a
// profile of the user's files that they asked not to have.
func TestFrecencyCanBeDisabled(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	off := false
	m := newModelWithPrefs(&recordingSession{}, root, Preferences{Frecency: &off})
	if m.frecency != nil {
		t.Fatal("frecency store constructed despite tui.frecency: false")
	}
	// Candidates still work — the directory listing is not the frecency
	// feature — they are just unordered by usage.
	if got := m.atCandidates(""); len(got) == 0 {
		t.Error("disabling frecency also disabled @path completion entirely")
	}
	// And recording must be a no-op rather than a panic.
	m.recordFrecency(filepath.Join(root, "a.go"))
}

// TestTypingAtOpensTheCompletionPopup is the wiring half: atCandidates being
// correct proves nothing if no keystroke reaches it.
func TestTypingAtOpensTheCompletionPopup(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newModel(&recordingSession{}, root)
	m.input.SetValue("look at @not")
	m.updatePalette()

	if !m.paletteOpen() {
		t.Fatal("typing @not opened no completion popup")
	}
	if m.paletteItems[0].name != "notes.md" {
		t.Errorf("first candidate = %q, want notes.md", m.paletteItems[0].name)
	}

	// Completing replaces the partial token, keeps the rest of the sentence,
	// and leaves a path extractAttachRefs will recognise.
	m.paletteComplete()
	got := m.input.Value()
	if got != "look at @notes.md " {
		t.Fatalf("input after completion = %q", got)
	}
	if len(extractAttachRefs(got, root)) != 1 {
		t.Error("the completed text does not produce an attachment reference")
	}
}

func TestAtPrefixAtCursorRejectsNonTokens(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"@no", true},
		{"see @sub/dir", true},
		{"", false},
		{"me@example.com", false},      // an email, not an attachment
		{"@done and more text", false}, // the user has moved past the token
	} {
		if _, ok := atPrefixAtCursor(tc.text); ok != tc.want {
			t.Errorf("atPrefixAtCursor(%q) = %v, want %v", tc.text, ok, tc.want)
		}
	}
}
