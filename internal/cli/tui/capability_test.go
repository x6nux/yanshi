// Package tui tests for capability.go (W-E-01 / INF5: ApplyColorProfile).
package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// withColorProfile applies p for the duration of the test and restores the
// previous global lipgloss/glamour profile on cleanup. ApplyColorProfile
// mutates package-level state (lipgloss's shared renderer, activeProfile,
// the cached glamour renderer) — see its doc comment — so tests that call it
// must not leak the mutation into unrelated tests. No tui test currently
// runs t.Parallel(), so sequential save/restore is sufficient.
func withColorProfile(t *testing.T, p termenv.Profile) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	ApplyColorProfile(p)
	t.Cleanup(func() { ApplyColorProfile(prev) })
}

// TestApplyColorProfile_AsciiSuppressesColor proves acceptance criterion 1
// (NO_COLOR): once ApplyColorProfile(termenv.Ascii) is applied, rendering a
// color-only style (no bold/italic) emits no ANSI escape sequence at all —
// not just a plain-looking one. diffCtxStyle sets only Foreground, so any
// escape byte in the output can only have come from color.
func TestApplyColorProfile_AsciiSuppressesColor(t *testing.T) {
	withColorProfile(t, termenv.Ascii)
	out := diffCtxStyle.Render("context line")
	if strings.ContainsRune(out, '\x1b') {
		t.Fatalf("Ascii profile: rendered output still contains an ANSI escape byte: %q", out)
	}
	if out != "context line" {
		t.Fatalf("Ascii profile: got %q, want plain text unchanged", out)
	}
}

// TestApplyColorProfile_TrueColorEmits24Bit proves the rendering-layer half
// of acceptance criterion 3 (COLORTERM=truecolor → 24bit): once
// ApplyColorProfile(termenv.TrueColor) is applied, an RGB-declared color
// renders as a genuine 24-bit "38;2;r;g;b" SGR sequence.
//
// This deliberately uses an ad-hoc hex-declared style rather than one of
// package-level style vars above: at this commit those vars are still
// declared as ANSI-256 palette indices (lipgloss.Color("N")), and
// termenv.Profile.Convert never upgrades an already-8-bit color to 24-bit —
// there is no upgrade path, only downgrade paths (RGB→256, RGB→16). What
// this test proves is that the pipeline (ApplyColorProfile → lipgloss's
// shared renderer → termenv) correctly passes an RGB color through as 24-bit
// when given one; whether any *production* style is declared as RGB is a
// separate, later concern (see the "Palette" mechanical-replacement commit,
// which converts the hue-bearing style vars — not the grayscale ones, see
// that commit's own comment — to hex specifically so this upgrade applies to
// them too).
func TestApplyColorProfile_TrueColorEmits24Bit(t *testing.T) {
	withColorProfile(t, termenv.TrueColor)
	rgbStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#00afff"))
	out := rgbStyle.Render("context line")
	if !strings.Contains(out, "38;2;") {
		t.Fatalf("TrueColor profile: expected a 24-bit \"38;2;r;g;b\" sequence, got %q", out)
	}
}

// TestApplyColorProfile_ANSI256EmitsPaletteIndex proves acceptance criterion
// 4's 256-color half: under termenv.ANSI256, the same style renders an 8-bit
// "38;5;N" palette-index sequence, not a 24-bit one.
func TestApplyColorProfile_ANSI256EmitsPaletteIndex(t *testing.T) {
	withColorProfile(t, termenv.ANSI256)
	out := diffCtxStyle.Render("context line")
	if !strings.Contains(out, "38;5;245") {
		t.Fatalf("ANSI256 profile: expected an 8-bit \"38;5;245\" sequence (diffCtxStyle's grey), got %q", out)
	}
	if strings.Contains(out, "38;2;") {
		t.Fatalf("ANSI256 profile: got a 24-bit sequence, want an 8-bit palette index: %q", out)
	}
}

// TestApplyColorProfile_ANSIDegradesTo16Color proves acceptance criterion 4's
// 16-color half: under termenv.ANSI, the palette-index color (245, a
// grayscale extended code with no 16-color analogue) degrades to a plain
// 16-color SGR code ("3" or "9" prefix) instead of an 8-bit index.
func TestApplyColorProfile_ANSIDegradesTo16Color(t *testing.T) {
	withColorProfile(t, termenv.ANSI)
	out := diffCtxStyle.Render("context line")
	if strings.Contains(out, "38;5;") || strings.Contains(out, "38;2;") {
		t.Fatalf("ANSI (16-color) profile: expected degradation to a plain 16-color code, got %q", out)
	}
	if !strings.ContainsRune(out, '\x1b') {
		t.Fatalf("ANSI (16-color) profile: expected a 16-color escape sequence, got none: %q", out)
	}
}

// TestApplyColorProfile_InvalidatesGlamourCache proves renderer() rebuilds
// under a new profile instead of serving a stale cache entry keyed only on
// width (see ApplyColorProfile's doc comment for why this matters).
func TestApplyColorProfile_InvalidatesGlamourCache(t *testing.T) {
	withColorProfile(t, termenv.ANSI256)
	first := renderer(80)
	if first == nil {
		t.Fatal("renderer(80) returned nil under ANSI256")
	}
	ApplyColorProfile(termenv.Ascii)
	t.Cleanup(func() { ApplyColorProfile(termenv.ANSI256) })
	second := renderer(80)
	if second == nil {
		t.Fatal("renderer(80) returned nil under Ascii")
	}
	if first == second {
		t.Fatal("renderer(80) returned the same cached instance after ApplyColorProfile changed the profile")
	}
}
