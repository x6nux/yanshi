package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/muesli/termenv"
)

// TermCapability is the result of detecting what the attached terminal can
// render (W-E-01 / INF5). It governs two independent decisions: which color
// profile the TUI's lipgloss/glamour rendering targets (see
// tui.ApplyColorProfile), and whether the program may switch into the
// alternate screen buffer at all.
type TermCapability struct {
	// Profile is the terminal color profile to render for: termenv.Ascii (no
	// color), termenv.ANSI (16-color), termenv.ANSI256, or termenv.TrueColor
	// (24-bit).
	Profile termenv.Profile
	// AltScreen reports whether the program may switch into the terminal's
	// alternate screen buffer. False only for TERM=dumb: a dumb terminal has
	// no cursor addressing at all, so the alt-screen switch sequence isn't
	// merely wasted, it's noise the consumer (a log pipe, some CI runners)
	// never asked for.
	AltScreen bool
}

// String renders the capability as a short human-readable summary, e.g.
// "color=ANSI256 alt_screen=true". Used by doctor's terminal-capability
// check so the detection result is visible without reading source.
func (c TermCapability) String() string {
	return fmt.Sprintf("color=%s alt_screen=%v", c.Profile.Name(), c.AltScreen)
}

// DetectCapability inspects the environment (via getenv) and returns the
// TermCapability the caller should render for. getenv is injected rather
// than reading os.Getenv directly so the detection logic is a pure function
// of its input and testable without mutating process-global environment
// state; a nil getenv is treated as an empty environment.
//
// Priority order, most specific first:
//
//  1. TERM=dumb forces Ascii + no alt-screen, overriding every color hint
//     below it — including a stale/inherited COLORTERM=truecolor (some CI
//     images export both). A dumb terminal cannot address the cursor, so
//     alt-screen and color codes are actively harmful, not just wasted.
//  2. NO_COLOR (https://no-color.org/), any non-empty value, forces Ascii.
//     The alt-screen switch is still safe here (unlike TERM=dumb, the
//     terminal itself is assumed capable — the user just opted out of
//     color), so AltScreen stays true.
//  3. COLORTERM=truecolor / COLORTERM=24bit forces TrueColor. This is
//     checked explicitly rather than left to termenv's own ColorProfile,
//     because termenv's Windows implementation does not consult COLORTERM
//     at all (see termenv_windows.go — it uses ConEmuANSI and the Windows
//     build number instead), so relying on it alone would make this
//     criterion pass on POSIX and silently fail on Windows.
//  4. Otherwise, termenv's own TERM-name/terminfo heuristic decides between
//     TrueColor, ANSI256, ANSI (16-color), and Ascii. termenv.WithUnsafe
//     bypasses its isatty/CI gate so this is a pure function of the env
//     vars passed to getenv, independent of whether the calling process's
//     stdout happens to be a real terminal (relevant for tests and for
//     doctor, which runs headless).
func DetectCapability(getenv func(string) string) TermCapability {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	if getenv("TERM") == "dumb" {
		return TermCapability{Profile: termenv.Ascii, AltScreen: false}
	}
	if getenv("NO_COLOR") != "" {
		return TermCapability{Profile: termenv.Ascii, AltScreen: true}
	}
	switch strings.ToLower(getenv("COLORTERM")) {
	case "truecolor", "24bit":
		return TermCapability{Profile: termenv.TrueColor, AltScreen: true}
	}
	out := termenv.NewOutput(io.Discard,
		termenv.WithEnvironment(&getenvEnviron{getenv}),
		termenv.WithUnsafe(),
	)
	return TermCapability{Profile: out.ColorProfile(), AltScreen: true}
}

// getenvEnviron adapts a getenv func to termenv.Environ so DetectCapability
// can hand termenv an injected environment instead of the real os.Environ.
// Environ() (the full variable listing) is never consulted by
// termenv.Output.ColorProfile — only Getenv is — so it returns nil rather
// than reconstructing a real listing from getenv.
type getenvEnviron struct{ getenv func(string) string }

func (e *getenvEnviron) Environ() []string        { return nil }
func (e *getenvEnviron) Getenv(key string) string { return e.getenv(key) }

// checkTerminalCapability reports the terminal capability doctor would
// detect for the current process environment (W-E-01 acceptance: "探测结果
// 可被 -h 或 doctor 显示"). It never fails or warns on its own account — like
// the other D3-era checks, this surfaces posture, not a problem.
func checkTerminalCapability() CheckResult {
	cap := DetectCapability(os.Getenv)
	return CheckResult{Name: "terminal-capability", Status: StatusOK, Message: cap.String()}
}
