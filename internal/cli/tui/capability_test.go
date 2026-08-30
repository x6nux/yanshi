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

// TestRenderMarkdown_AsciiStripsAllEscapes pins RE-A: glamour.WithColorProfile
// correctly drops COLOR under Ascii, but glamour v1.0.0's bold/underline
// styling goes through termenv.String(s).Styled(), and termenv@v0.16.0's
// Style.String() constructor hardcodes profile=ANSI internally — so Ascii's
// "suppress all styling" early-return in Styled() never fires for glamour,
// regardless of the profile passed to glamour.WithColorProfile. renderMarkdown
// now strips the result post-hoc; this test is the regression pin for that.
func TestRenderMarkdown_AsciiStripsAllEscapes(t *testing.T) {
	withColorProfile(t, termenv.Ascii)
	out := renderMarkdown(80, "**bold** and _italic_ text")
	if strings.ContainsRune(out, '\x1b') {
		t.Fatalf("Ascii profile: glamour-rendered markdown still contains an ANSI escape byte: %q", out)
	}
}

// TestRenderMarkdown_ColorProfilePropagates pins RE-C: renderer(width)'s
// glamour.WithColorProfile(activeProfile) call (styles.go) actually reaches
// glamour's rendered output — not just the cache-pointer-identity that
// TestApplyColorProfile_InvalidatesGlamourCache above pins. Mutating that
// call back to a hardcoded profile (e.g. glamour.WithColorProfile(termenv.
// ANSI256)) leaves every other test in this file green; this one goes red
// because the ANSI case would then still emit "38;5;" (an 8-bit palette
// index has no business surviving a 16-color downgrade).
//
// ANSI256 and TrueColor are asserted identical here (both "38;5;") — that is
// not a bug in this propagation path: glamour's own style colors
// (styles.DarkStyleConfig, and this file's "51" code-span override in
// renderer()) are declared as bare ANSI-256 index strings, not hex, and
// termenv.Profile.Convert never upgrades an already-8-bit color to 24-bit
// (see TestApplyColorProfile_TrueColorEmits24Bit's doc comment) — it is
// glamour's own color literals staying 8-bit under TrueColor, tracked
// separately as RE-F.
func TestRenderMarkdown_ColorProfilePropagates(t *testing.T) {
	cases := []struct {
		profile termenv.Profile
		want256 bool
	}{
		{termenv.ANSI, false},
		{termenv.ANSI256, true},
		{termenv.TrueColor, true},
	}
	for _, c := range cases {
		t.Run(c.profile.Name(), func(t *testing.T) {
			withColorProfile(t, c.profile)
			out := renderMarkdown(80, "`code span`")
			if has := strings.Contains(out, "38;5;"); has != c.want256 {
				t.Errorf("%s: \"38;5;\" present=%v, want %v: %q", c.profile.Name(), has, c.want256, out)
			}
			if !strings.ContainsRune(out, '\x1b') {
				t.Errorf("%s: expected an ANSI escape sequence, got none: %q", c.profile.Name(), out)
			}
		})
	}
}
