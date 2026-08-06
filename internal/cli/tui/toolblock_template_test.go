package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
)

// TestToolBlockRendersFromAFixedTemplate pins the rendered shape byte for byte.
//
// Every existing render test asserts with Contains: that the name appears, that
// the glyph appears, that the result is in there somewhere. All of them stay
// green if the indentation changes, if the ⎿ prefix is dropped, if the trailing
// blank line that separates blocks disappears, or if the summary and the status
// panel swap places. "Renders from a fixed template" is a claim about the exact
// string, so the assertion has to be equality.
//
// Styling is absent here because lipgloss degrades to plain text without a TTY,
// which is what makes a golden feasible at all. The trade is explicit: this
// pins layout, not colour. Colour is toolGlyph's own concern and has its own
// tests.
//
// ledger: M1/SPEC-TOOLIF#5 TUI 固定模板渲染
func TestToolBlockRendersFromAFixedTemplate(t *testing.T) {
	sp := spinner.New()
	for _, tc := range []struct {
		class string
		name  string
		want  string
	}{
		// Silent: header only. Read/List produce no result body because the
		// content is already in the model's answer.
		{"silent", "fs_read", "  Read ✓\n"},
		// Tail: the command's own output, indented, no ⎿ marker — it is a
		// transcript, not a summary.
		{"tail", "shell_run", "  Bash ✓\n    done\n\n"},
		// Normal: the ⎿ summary line plus the expand hint.
		{"normal", "git_status", "  git_status ✓\n    ⎿ done  (ctrl+o expand)\n\n"},
	} {
		t.Run(tc.class, func(t *testing.T) {
			e := &toolEntry{name: tc.name, status: "ok", args: `{}`, result: "done"}
			if got := e.render(80, sp); got != tc.want {
				t.Errorf("render mismatch\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestToolBlockTemplateVariesOnlyByName is the other half of "fixed template".
//
// Equality against a golden proves one tool renders a known string; it does not
// prove the string came from a shared template rather than from that tool's own
// bespoke branch. Two tools of the same display class must produce outputs that
// differ in exactly the name substitution — anything else means a second
// template grew somewhere.
//
// ledger: M1/SPEC-TOOLIF#5 TUI 固定模板渲染
func TestToolBlockTemplateVariesOnlyByName(t *testing.T) {
	sp := spinner.New()
	const other = "some_unregistered_tool"

	a := (&toolEntry{name: "git_status", status: "ok", args: `{}`, result: "done"}).render(80, sp)
	b := (&toolEntry{name: other, status: "ok", args: `{}`, result: "done"}).render(80, sp)

	// Guard against the degenerate pass where both render as the same name.
	if !strings.Contains(a, "git_status") || !strings.Contains(b, other) {
		t.Fatalf("the two blocks do not carry their own names; nothing to compare\n a=%q\n b=%q", a, b)
	}
	if normalised := strings.Replace(b, other, "git_status", 1); normalised != a {
		t.Errorf("two tools of the same display class do not share one template\n"+
			" %q normalises to %q\n want %q", b, normalised, a)
	}
}
