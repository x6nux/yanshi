// Package tui implements the yanshi interactive terminal UI: a Claude-Code-
// style block transcript (user / assistant / tool / error entries) rendered with
// lipgloss + glamour, driven by a bubbletea model over a cli.Session.
package tui

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// palette holds the hex-RGB equivalents of this file's hue-bearing style
// colors (W-E-01 follow-up: "所有颜色常量改为从 Palette 取"). They replace the
// bare ANSI-256 palette-index strings (lipgloss.Color("39") etc.) that this
// file used before termenv-based profile detection existed.
//
// Why hex and not the index string: termenv.Profile.Convert only ever
// DOWNGRADES an already-typed color (RGBColor→256/16; it never upgrades an
// ANSI256Color/ANSIColor to 24-bit — see capability_test.go's
// TestApplyColorProfile_TrueColorEmits24Bit doc comment). A style declared as
// lipgloss.Color("39") therefore renders identically under ANSI256 and
// TrueColor profiles; only a hex-declared lipgloss.Color("#…") gets genuine
// profile-dependent output, including real 24-bit under COLORTERM=truecolor
// (acceptance criterion 3). Each constant documents the ANSI-256 index it
// replaces (computed via the standard 6×6×6 xterm cube: index = 16 + 36r +
// 6g + 1b, level(n) ∈ {0,95,135,175,215,255}) so ANSI256-profile output is
// byte-identical to before this change — verified by the existing style
// tests, which assert no output changed.
//
// Deliberately excluded: the five grayscale colors this file also uses (245,
// 250, 252, 238, 255) are NOT converted here. xterm-256 has two disjoint
// representations for near-gray colors — the 6×6×6 cube's r=g=b diagonal and
// the dedicated 24-step grayscale ramp (232–255) — and termenv's hex→256
// nearest-match walks both, so round-tripping e.g. "#8a8a8a" (245's exact
// ramp value) back through Profile.Color under ANSI256 is not guaranteed to
// land on index 245 again. Converting the hues (which live solely in the
// cube, so hex→256 round-trips exactly) gets the TrueColor upgrade without
// that risk; the grays stay as index literals.
const (
	hueCyan       = "#00afff" // was "39"  — roleUser
	huePink       = "#ff87ff" // was "213" — roleAsst, footerThinkStyle
	hueBrightCyan = "#87ffff" // was "123" — toolName, stashHeaderStyle
	hueGreen      = "#00d787" // was "42"  — okStyle, diffAddStyle
	hueRed        = "#ff5f5f" // was "203" — errStyle, diffDelStyle, errToastStyle
	hueAmber      = "#d7af5f" // was "179" — warnStyle, warnToastStyle
	hueBrightRed  = "#ff0000" // was "196" — warnStylePerm
	hueLightBlue  = "#87d7ff" // was "117" — thinkingLiveStyle
	hueDarkBlue   = "#005f87" // was "24"  — selPaletteStyle background
)

// Palette (256-color; degrades gracefully on minimal terminals).
var (
	roleUser    = lipgloss.NewStyle().Foreground(lipgloss.Color(hueCyan)).Bold(true)       // cyan
	roleAsst    = lipgloss.NewStyle().Foreground(lipgloss.Color(huePink)).Bold(true)       // pink
	toolName    = lipgloss.NewStyle().Foreground(lipgloss.Color(hueBrightCyan)).Bold(true) // bright cyan
	toolMeta    = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))                    // grey
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color(hueGreen)).Bold(true)      // green
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color(hueRed)).Bold(true)        // red
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(hueAmber))                 // amber
	resultStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("250")).PaddingLeft(4)

	// diffAddStyle / diffDelStyle / diffCtxStyle color the per-line sigils of
	// a unified diff rendered inside an fs_edit/fs_write tool block. The hues
	// match the de-facto convention: green for additions, red for removals,
	// and dim grey for unchanged context. They are intentionally shared with
	// the ok/err palette so the diff reads as part of the transcript rather
	// than a foreign element.
	diffAddStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(hueGreen)) // green
	diffDelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(hueRed))   // red
	diffCtxStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))    // grey

	// diffGutterStyle renders the old/new line-number gutter W-E-02 adds in
	// front of each diff line. It is deliberately a step darker (238) than
	// diffCtxStyle's context grey (245) — both live on the 232-255 grayscale
	// ramp, where a lower index is darker — so the four diff roles read as a
	// genuine depth-graded tier (gutter < context < add/del in visual
	// weight), not just three same-weight hues with numbers bolted on. This
	// is the "调色板...按色深分档" requirement the spec calls out by name.
	diffGutterStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))

	inputBorder = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)

	// footerThinkStyle is used by colorizeActivity to render the "Thinking…"
	// activity text in pink (213) so it reads consistently with the live
	// thinkingEntry hue. Despite its "footer" prefix it is NOT a footer-segment
	// style — the footer has no "think" segment at all (RE-H, fix-e1 review of
	// W-E-01: the theme tables below used to carry a "think" key, but
	// statusHeader (view.go) never looked it up, so it was dead data; removed).
	footerThinkStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(huePink)) // magenta/pink
	warnStylePerm    = lipgloss.NewStyle().Foreground(lipgloss.Color(hueBrightRed)).Bold(true)
	// queuePreviewStyle renders the queued-messages preview block above the input
	// composer (C07): dim grey + italic, matching codex's PendingInputPreview so
	// the backlog reads as pending context rather than part of the live transcript.
	queuePreviewStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Italic(true)

	// activityStyle renders the Claude-Code-style live status line shown while a
	// turn is in flight (between the transcript and the input box).
	activityStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("252")).PaddingLeft(2)
	// pasteStyle renders the collapsed "[粘贴 #<id>]" placeholder that long
	// pasted messages are reduced to in the transcript (full text still sent).
	pasteStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Italic(true)
	// thinkingLiveStyle renders a LIVE thinkingEntry's header + body while
	// reasoning is still streaming in. A brighter hue (light blue 117, italic)
	// reads as "in progress, still arriving" and contrasts with the dim grey of
	// thinkingDoneStyle once the block finalizes — so the user can tell at a
	// glance whether the displayed reasoning is actively being produced or has
	// settled. The whole block (header + tail body) shares this style so the
	// state transition is a single visual shift, not just a header-color tweak.
	thinkingLiveStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(hueLightBlue)).Italic(true)
	// thinkingDoneStyle renders a finalized thinkingEntry (collapsed "Thought
	// for Xs" line and expanded markdown body). The dim grey (245) italic
	// matches the historic single thinkingStyle and signals "this reasoning has
	// settled" — older context, no longer changing. Pairing with
	// thinkingLiveStyle gives the live→finalized transition a clear color cue.
	thinkingDoneStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Italic(true)
	// doneSummaryStyle renders the turn-end "Done N tools uses X tokens Y"
	// summary line in the same dim grey (245) as the finalized "Thought for
	// Xs" line so it reads as a transcript annotation, not the assistant's
	// answer.
	doneSummaryStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	// selStyle renders the lines under an in-app mouse selection: a solid
	// background so the dragged range reads clearly as the selection.
	selStyle = lipgloss.NewStyle().Background(lipgloss.Color("238")).Foreground(lipgloss.Color("255"))
	// paletteStyle / selPaletteStyle render the command-palette popup rows; the
	// selected row gets a distinct background + a "▶" marker.
	paletteStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	selPaletteStyle = lipgloss.NewStyle().Background(lipgloss.Color(hueDarkBlue)).Foreground(lipgloss.Color("255")).Bold(true)

	// C2 — UX7 toast styles. Each level keeps a stable colour identity: info
	// is dim grey (recognition without alarm), warn is amber (matches the
	// existing warnStyle so "saved"/"restored" read consistently across the
	// UI), error is red. Only the body uses these styles — the prefix glyphs
	// ("[!]", "[X]") still use warnStyle / errStyle for contrast at the glyph.
	infoToastStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	warnToastStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(hueAmber))
	errToastStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color(hueRed)).Bold(true)

	// C2 — UX5 stash list styles. Header uses the same bold cyan family as
	// other transcript section headers (e.g. "available models"); items stay
	// neutral grey so the preview text reads as data, not styling.
	stashHeaderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(hueBrightCyan)).Bold(true)
	stashItemStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
)

var (
	mdRenderer   *glamour.TermRenderer
	mdWidth      int
	mdRendererMu sync.Mutex

	// activeProfile is the terminal color profile applied via
	// ApplyColorProfile (see that function's doc comment). It defaults to
	// ANSI256 — the profile this file hardcoded before W-E-01 — so any code
	// path that renders before NewProgram calls ApplyColorProfile (tests,
	// helpers invoked outside a running program) keeps today's behavior.
	activeProfile = termenv.ANSI256
)

// ApplyColorProfile sets the terminal color profile used by every lipgloss
// style declared in this package and by the glamour markdown renderer built
// by renderer(). Call it once at startup, before any rendering happens (see
// model.NewProgram, which derives the profile from cli.DetectCapability).
//
// lipgloss styles here are package-level vars, but Style.Render reads the
// color profile from lipgloss's shared renderer at render time rather than
// at var-declaration time, so a single lipgloss.SetColorProfile call
// retroactively governs all of them without touching each var individually.
//
// Changing the profile also invalidates the cached glamour renderer:
// renderer() keys its cache on width only, so without this the cache would
// keep serving markdown rendered under the previous profile after a switch.
// In production there is exactly one call site (NewProgram, once per
// session) so this never fires there; the switch this guards against is
// tests that probe more than one profile in the same process
// (TestApplyColorProfile_InvalidatesGlamourCache pins it) — RE-G (fix-e1
// review of W-E-01): an earlier draft of this comment named "/model changes
// the detected capability" as a second scenario, but /model (commands.go's
// cmdModel) never touches terminal capability or calls ApplyColorProfile —
// that scenario does not exist in this codebase.
func ApplyColorProfile(p termenv.Profile) {
	lipgloss.SetColorProfile(p)
	mdRendererMu.Lock()
	defer mdRendererMu.Unlock()
	if activeProfile != p {
		activeProfile = p
		mdRenderer = nil
	}
}

// currentColorProfile returns the profile most recently applied via
// ApplyColorProfile, safe for concurrent use. renderFooter (view.go) writes
// raw ANSI escapes directly rather than through a lipgloss.Style — it never
// goes through lipgloss's shared renderer, so it needs its own read of the
// active profile to honor NO_COLOR / TERM=dumb / COLORTERM (W-E-01).
func currentColorProfile() termenv.Profile {
	mdRendererMu.Lock()
	defer mdRendererMu.Unlock()
	return activeProfile
}

func renderer(width int) *glamour.TermRenderer {
	mdRendererMu.Lock()
	defer mdRendererMu.Unlock()
	if mdRenderer != nil && mdWidth == width {
		return mdRenderer
	}
	if width <= 0 {
		width = 80
	}
	// v1.0.0's dark style shows "## "/"### " heading markers as a prefix and
	// paints inline `code` red (203). Start from dark, clear the heading
	// markers, and recolor inline code cyan so it reads calmly.
	style := styles.DarkStyleConfig
	for _, h := range []*string{
		&style.H2.StylePrimitive.Prefix,
		&style.H3.StylePrimitive.Prefix,
		&style.H4.StylePrimitive.Prefix,
		&style.H5.StylePrimitive.Prefix,
		&style.H6.StylePrimitive.Prefix,
	} {
		*h = ""
	}
	style.Code.StylePrimitive.Color = strPtr("51")
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithColorProfile(activeProfile),
		glamour.WithWordWrap(width),
		glamour.WithChromaFormatter(chromaFormatterFor(activeProfile)),
	)
	if err != nil {
		return nil
	}
	mdRenderer = r
	mdWidth = width
	return r
}

// strPtr returns a pointer to s, for glamour's *string style fields.
func strPtr(s string) *string { return &s }

// chromaFormatterFor maps a termenv color profile to the chroma terminal
// formatter name glamour should use for code-block syntax highlighting —
// RE-I (fix-e1 review of W-E-01): glamour v1.0.0 hardcodes the formatter to
// "terminal256" (ansi/codeblock.go's chromaFormatter const) and never
// consults the renderer's own color profile, so a 16-color (termenv.ANSI)
// terminal was receiving 8-bit palette codes ("38;5;N") it can't render —
// measured at 17 occurrences in a single fenced code block under that
// profile. This is a display-fidelity gap, not a no-color leak: glamour
// skips chroma highlighting (and therefore the formatter) entirely under
// termenv.Ascii (codeblock.go's own "ColorProfile != termenv.Ascii" gate),
// so Ascii/TERM=dumb sessions were never affected by this. TrueColor and
// ANSI256 both keep "terminal256" rather than TrueColor jumping to chroma's
// "terminal16m": this package's own lipgloss styles don't distinguish
// ANSI256 from TrueColor either (see the palette doc comment above), so
// giving only code blocks finer resolution than every surrounding style
// would make them the one element in the transcript that looks like a
// different terminal.
func chromaFormatterFor(p termenv.Profile) string {
	if p == termenv.ANSI {
		return "terminal16"
	}
	return "terminal256"
}

func renderMarkdown(width int, md string) string {
	r := renderer(width)
	if r == nil {
		return md
	}
	out, err := r.Render(md)
	if err != nil {
		return md
	}
	if currentColorProfile() == termenv.Ascii {
		// glamour.WithColorProfile(activeProfile) (see renderer() above) is
		// correctly wired, but it doesn't reach every escape glamour v1.0.0
		// emits: its bold/underline styling goes through termenv.String(s)
		// (ansi/baseelement.go's renderText), and termenv@v0.16.0's
		// Style.String() constructor hardcodes profile=ANSI internally — so
		// Styled()'s own "if t.profile == Ascii { return s }" early-return
		// never fires, and \x1b[1m / \x1b[4m / degenerate empty-parameter SGR
		// sequences survive regardless of the profile passed to glamour. This
		// is the third instance of the same "Ascii drops ALL styling" rule
		// being violated by a path outside lipgloss's shared renderer (the
		// other two: the footer's independent ANSI path, and the footer bold
		// flag not downgrading — see footerSGRParts in view.go). Stripping
		// here rather than waiting on upstream keeps the same guarantee
		// TestApplyColorProfile_AsciiSuppressesColor already pins for
		// lipgloss-rendered styles: zero \x1b bytes under Ascii, not just
		// plain-looking ones.
		out = stripANSI(out)
	}
	return out
}

// entryRenderCache memoizes expensive markdown-rendered transcript blocks keyed
// by a content+width fingerprint (fnv64, computed by the caller). The cache
// lets renderBody skip the glamour pass on finalized entries whose markdown is
// invariant between frames — important because renderBody runs on every
// debounced input reflow and on every 5s repaintTick, so without caching the
// WHOLE transcript would be glamour-rendered on every keystroke even though
// historical entries never change.
//
// Scope: only finalized assistantEntry blocks go through the cache today (they
// are the dominant cost — full glamour markdown rendering of a long answer).
// Lighter entries (user/tool/error/…) are rendered directly; if profiling
// later shows another entry type is hot, the same cache applies.
//
// Bounds: a sync.Map does not evict on its own, and the key incorporates width
// (so resizing the terminal fans out a fresh key per distinct column count).
// Without a cap, a long session + a few resize events could grow this map
// without limit. entryCacheCap bounds the number of cached renders; once the
// cap is reached, cachedEntryRender stops storing new entries (existing hits
// still serve, and the render still runs — only the memoization is skipped).
// The cap is large enough that a normal session never reaches it: 1024 unique
// (text × width) fingerprints covers dozens of multi-KB assistant turns across
// a handful of width buckets with headroom to spare.
//
// The cache is process-global: entries are content-addressed so an identical
// block (e.g. the same assistant answer re-rendered after a width change) hits
// regardless of which transcript it lives in.
var (
	entryRenderCache sync.Map // key=uint64 → string
	entryCacheCount  atomic.Int64
)

const entryCacheCap int64 = 1024

// cachedEntryRender returns the cached render for key if one exists; otherwise
// it invokes render(), stores the result, and returns it. Storing on first miss
// means subsequent identical renders are O(map-lookup) instead of O(glamour).
//
// Once entryCacheCap entries have been stored, new entries are no longer cached
// (the render still runs and is returned to the caller) — this bounds memory
// for unbounded sessions without invalidating the warm working set already in
// the cache. See entryRenderCache docs for the rationale.
func cachedEntryRender(key uint64, render func() string) string {
	if v, ok := entryRenderCache.Load(key); ok {
		return v.(string)
	}
	s := render()
	// Bounded cache: once we've hit the cap, stop accepting new entries. The
	// Load+Store here is racy across goroutines (two callers may both Store the
	// same key), but the racy Stores are idempotent (same key → same value) and
	// the count overshoots by at most a few goroutines, which is acceptable for
	// a soft cap whose only purpose is preventing unbounded growth.
	if entryCacheCount.Load() < entryCacheCap {
		if _, loaded := entryRenderCache.LoadOrStore(key, s); !loaded {
			entryCacheCount.Add(1)
		}
	}
	return s
}

// resetEntryRenderCacheForTest clears the cache and counter. Production code
// never needs to reset the cache (it is content-addressed and bounded), but
// tests need a clean slate to assert cap behavior deterministically.
func resetEntryRenderCacheForTest() {
	entryRenderCache = sync.Map{}
	entryCacheCount.Store(0)
}

// pendingStyle renders the streaming (not-yet-finalized) assistant text as
// plain text — NO glamour markdown rendering. Streaming chunks arrive many
// times per second, and rendering markdown per chunk is the dominant CPU cost
// of streaming; the plain-text path lets the UI keep up with the model's token
// rate. Once the turn finalizes (flushAssistant), the accumulated text becomes
// an assistantEntry and is rendered through markdown exactly once (and cached
// via entryRenderCache) — so the user sees raw markdown briefly during
// streaming, then the fully-rendered version on completion.
var pendingStyle = lipgloss.NewStyle()

// truncate clips s to n bytes with an ellipsis.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// clamp constrains n to the inclusive [lo, hi] range.
func clamp(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// --- Theme system for footer segments ----------------------------------------
//
// A Theme defines the foreground/background/bold for each information segment in
// the Powerline-style footer bar. Three built-in themes give the user control
// over visual density and contrast without changing the information layout.

// ThemeName identifies a named color theme.
type ThemeName string

// Named color themes.
const (
	ThemeDefault      ThemeName = "default"
	ThemeHighContrast ThemeName = "high-contrast"
	ThemeMuted        ThemeName = "muted"
)

// segmentColors defines the foreground colour, background colour, and bold flag
// for one footer segment. Colour values are ANSI 256-color palette codes ("0"–
// "255"); the caller must also know the default footer background ("236") for
// passages that should not render as a coloured pill.
//
// ansiBg is an optional override consulted only under termenv.ANSI (real
// 16-color terminals) — RE-E (fix-e1 review of W-E-01): bg is fed through
// termenv's Profile.Convert on every profile, including ANSI, and that
// conversion is a mechanical nearest-256-to-16 lookup table, not a palette
// anyone designed for 16 colors. Measured (env -u COLORTERM TERM=xterm-16color,
// tuidbg raster compare against the 256-color render): "ctx" (bg 58, dark
// olive) downgrades to SGR 43 (yellow) — white-on-yellow, low contrast;
// "perm_auto" (bg 94, dark brown) downgrades to the same SGR 43; "total" (bg
// 130, dark orange) downgrades to SGR 101 (bright red) — readable but
// semantically backwards, since red is this same footer's error/yolo color
// (see "perm_yolo"). When set, ansiBg is itself just another ANSI-256 index —
// it goes through the identical Convert path, so its value is chosen for
// where *it* lands under ANSI, not for how it would look under 256-color
// (which never sees it). Left empty for every segment ansiBg doesn't name:
// the mechanical downgrade is unreviewed there too (see RE-E's review note on
// A31, a full 16-color-aware palette, as future work) but wasn't measured as
// failing the "distinguishable, not misleading" bar this fix targets.
type segmentColors struct {
	fg     string // ANSI 256-colour foreground code e.g. "255"
	bg     string // ANSI 256-colour background code e.g. "17"
	bold   bool
	ansiBg string // optional ANSI-256 index used instead of bg when profile == termenv.ANSI
}

// Theme defines a named footer colour theme with per-segment colour mappings.
type Theme struct {
	Name        ThemeName
	Description string
	Colors      map[string]segmentColors // keyed by segment id
}

// themeList is the ordered list of built-in themes. The first entry is the
// default; a typo in /theme falls back to the first entry.
var themeList = []Theme{themeDefault, themeHighContrast, themeMuted}

// themeByName returns the theme with the given name, or false if not found.
func themeByName(name ThemeName) (Theme, bool) {
	for _, t := range themeList {
		if t.Name == name {
			return t, true
		}
	}
	return Theme{}, false
}

// themeDefault uses white-on-dark-background pills for maximum readability.
// The background colour carries the categorical meaning (navy=brand, green=
// active, olive=warning, purple=git/think, amber=caution); white foreground
// guarantees contrast on any terminal that supports 256 colours. Inspired by
// CCometixLine's Powerline segment bar.
var themeDefault = Theme{
	Name:        ThemeDefault,
	Description: "white text on coloured dark backgrounds",
	Colors: map[string]segmentColors{
		"mode":         {fg: "255", bg: "22", bold: false},                // white on dark green
		"dir":          {fg: "255", bg: "24", bold: false},                // white on dark blue
		"git":          {fg: "255", bg: "53", bold: false},                // white on dark purple
		"model":        {fg: "255", bg: "23", bold: false},                // white on dark teal
		"ctx":          {fg: "255", bg: "58", bold: false, ansiBg: "18"},  // white on dark olive; RE-E: 58 alone degrades to yellow(43) under 16-color, low contrast — 18 degrades to blue(44) instead
		"total":        {fg: "255", bg: "130", bold: false, ansiBg: "87"}, // white on dark orange (consumption tally); RE-E: 130 alone degrades to bright-red(101) — reads as an error, but this pill is a cost tally, not perm_yolo's error color — 87 degrades to bright-cyan(106) instead
		"perm_default": {fg: "245", bg: "236", bold: false},
		"perm_edits":   {fg: "255", bg: "28", bold: false},               // RE-H: was bg 22, identical to "mode" — same pill, two meanings
		"perm_auto":    {fg: "255", bg: "94", bold: false, ansiBg: "95"}, // RE-E: 94 alone degrades to yellow(43), same low-contrast defect as "ctx" — 95 degrades to bright-black(100) instead
		"perm_yolo":    {fg: "255", bg: "52", bold: true},
		"tools":        {fg: "245", bg: "235", bold: false},
		"queue":        {fg: "255", bg: "94", bold: true, ansiBg: "134"}, // RE-E: shared the same source (94) as "perm_auto" pre-fix — both degraded to identical yellow(43) under 16-color even though the two pills can render side by side (permission mode + queue depth); 134 degrades to bright-magenta(105), distinct from perm_auto's new bright-black(100)
	},
}

// themeHighContrast uses brighter backgrounds and bold text everywhere for users
// who need extra visual clarity or work on less-accurate displays.
var themeHighContrast = Theme{
	Name:        ThemeHighContrast,
	Description: "brighter backgrounds, bold text",
	Colors: map[string]segmentColors{
		"mode":         {fg: "255", bg: "28", bold: true},
		"dir":          {fg: "255", bg: "26", bold: true},
		"git":          {fg: "255", bg: "55", bold: true},
		"model":        {fg: "255", bg: "30", bold: true},
		"ctx":          {fg: "255", bg: "59", bold: true},                // RE-E: measured — 59 degrades to bright-black(100) under 16-color, still high contrast with bold white text, left as-is
		"total":        {fg: "255", bg: "130", bold: true, ansiBg: "87"}, // RE-E: same 130→bright-red(101) defect as themeDefault's "total" — see that entry's comment
		"perm_default": {fg: "255", bg: "236", bold: true},
		"perm_edits":   {fg: "255", bg: "34", bold: true},                // RE-H: was bg 28, identical to "mode" — same pill, two meanings
		"perm_auto":    {fg: "255", bg: "100", bold: true, ansiBg: "95"}, // RE-E: 100 also degrades to yellow(43) — same defect as themeDefault's "perm_auto", different source index, same downgrade table entry
		"perm_yolo":    {fg: "15", bg: "88", bold: true},
		"tools":        {fg: "255", bg: "237", bold: true},
		"queue":        {fg: "255", bg: "63", bold: true}, // RE-H: was bg 100, identical to "perm_auto" — same pill, two meanings
	},
}

// themeMuted strips the coloured pills and renders text-only on the default
// footer background. Each segment keeps its semantic foreground colour but the
// line gains the feel of a traditional terminal status bar.
var themeMuted = Theme{
	Name:        ThemeMuted,
	Description: "minimal, text-only on plain background",
	Colors: map[string]segmentColors{
		"mode":         {fg: "42", bg: "236", bold: false},
		"dir":          {fg: "75", bg: "236", bold: false},
		"git":          {fg: "141", bg: "236", bold: false},
		"model":        {fg: "51", bg: "236", bold: false},
		"ctx":          {fg: "221", bg: "236", bold: false},
		"total":        {fg: "215", bg: "236", bold: false},
		"perm_default": {fg: "245", bg: "236", bold: false},
		"perm_edits":   {fg: "82", bg: "236", bold: false}, // RE-H: was fg 42, identical to "mode" — same text colour, two meanings
		"perm_auto":    {fg: "179", bg: "236", bold: false},
		"perm_yolo":    {fg: "203", bg: "236", bold: true},
		"tools":        {fg: "250", bg: "236", bold: false}, // RE-H: was fg 245, identical to "perm_default" — same text colour, two meanings
		"queue":        {fg: "179", bg: "236", bold: true},
	},
}

// themeNames returns a comma-separated list of available theme names (for
// error messages in /theme).
func themeNames() string {
	var b strings.Builder
	for i, t := range themeList {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(string(t.Name))
	}
	return b.String()
}

// --- tool-call display helpers ----------------------------------------------

// toolDisplayNames maps raw tool names to short friendly aliases for the
// transcript. Tools not listed here are handled by toolDisplayName's prefix
// rules (vcs_*, memory_*) or returned verbatim.
var toolDisplayNames = map[string]string{
	"fs_read":        "Read",
	"fs_write":       "Write",
	"fs_edit":        "Edit",
	"fs_list":        "List",
	"fs_glob":        "Glob",
	"fs_search":      "Search",
	"shell_run":      "Bash",
	"time_now":       "Time",
	"web_fetch":      "Fetch",
	"skill_use":      "Skill",
	"agent_start":    "Agent",
	"workflow_start": "Workflow",
	"analysis":       "Analysis",
}

// toolDisplayName maps a raw tool name to a friendly display name: known tools
// get a short alias; vcs_* tools drop the prefix and Title-case each segment
// (vcs_commit → Commit, vcs_git_status → GitStatus); memory_* tools collapse to
// "Memory"; anything else is returned unchanged.
func toolDisplayName(name string) string {
	if pretty, ok := toolDisplayNames[name]; ok {
		return pretty
	}
	if rest := strings.TrimPrefix(name, "vcs_"); rest != name && rest != "" {
		return titleCaseSegments(rest)
	}
	if strings.HasPrefix(name, "memory_") {
		return "Memory"
	}
	return name
}

// titleCaseSegments Title-cases each underscore-separated segment of s and
// joins them without separators (e.g. "git_status" → "GitStatus", "commit" →
// "Commit"). Tool-name segments are ASCII identifiers, so byte-level casing is
// safe here.
func titleCaseSegments(s string) string {
	parts := strings.Split(s, "_")
	for i, seg := range parts {
		if seg == "" {
			continue
		}
		parts[i] = strings.ToUpper(seg[:1]) + seg[1:]
	}
	return strings.Join(parts, "")
}

// fsArgKeys is the preference order for the key field of an fs_* tool's args.
var fsArgKeys = []string{"path", "pattern", "glob"}

// toolArgSummary renders a compact argument summary for a tool call as
// "(value)" — the key field's value for the tool family (path/pattern/glob for
// fs tools, command for shell_run, first key otherwise). Path-like values are
// shortened relative to root where possible (else basename); patterns are
// quoted; the result is truncated to ~40 chars. Returns "" when there is no
// useful argument (the caller then omits the (...) entirely).
func toolArgSummary(name, argsJSON, root string) string {
	argsJSON = strings.TrimSpace(argsJSON)
	if argsJSON == "" || argsJSON == "{}" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
		// Not valid JSON — show the raw text so something useful is rendered.
		return "(" + truncate(argsJSON, 40) + ")"
	}
	key := pickArgKey(name, m)
	if key == "" {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	s := scalarToString(v)
	if s == "" {
		return ""
	}
	switch key {
	case "path", "glob":
		s = shortenPath(s, root)
	case "command", "cmd":
		// shell command: shown verbatim
	case "pattern":
		s = strconv.Quote(s)
	default:
		// other string values: quote to distinguish from bare identifiers
		if _, isStr := v.(string); isStr {
			s = strconv.Quote(s)
		}
	}
	return "(" + truncate(s, 40) + ")"
}

// pickArgKey selects the most relevant arg key for the tool family. fs tools
// prefer path/pattern/glob; shell_run prefers command/cmd; anything else uses
// the first (lexicographically smallest) key present.
func pickArgKey(name string, m map[string]any) string {
	if strings.HasPrefix(name, "fs_") {
		for _, k := range fsArgKeys {
			if _, ok := m[k]; ok {
				return k
			}
		}
		return firstKey(m)
	}
	if name == "shell_run" {
		for _, k := range []string{"command", "cmd"} {
			if _, ok := m[k]; ok {
				return k
			}
		}
		return firstKey(m)
	}
	return firstKey(m)
}

// firstKey returns the lexicographically smallest key in m, or "" if empty.
// (Go map iteration order is randomized, so this keeps the fallback
// deterministic.)
func firstKey(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys[0]
}

// scalarToString renders a JSON-decoded scalar as a string. JSON numbers arrive
// as float64 and are formatted without a trailing exponent; nil renders as "".
func scalarToString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}

// shortenPath makes a path more compact for display: relative to the work root
// when it lives under it (forward-slash normalized so this works regardless of
// the separator the caller used), else the basename when it is an absolute
// path, else the path unchanged. The caller is responsible for truncating.
func shortenPath(path, root string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return path
	}
	pr := filepath.ToSlash(path)
	if root != "" {
		rr := strings.TrimSuffix(filepath.ToSlash(root), "/")
		if rr != "" {
			if pr == rr {
				return "."
			}
			if strings.HasPrefix(pr, rr+"/") {
				return pr[len(rr)+1:]
			}
		}
	}
	if isAbsPath(pr) {
		if base := filepath.Base(pr); base != "" && base != "/" && base != "." {
			return base
		}
	}
	return pr
}

// isAbsPath reports whether pr (already forward-slash normalized) is absolute
// under either POSIX (leading "/") or Windows ("C:/…") conventions. Tool-arg
// paths may use either form regardless of the host OS, and filepath.IsAbs is
// OS-specific (a POSIX path is not absolute on Windows), so both are checked.
func isAbsPath(pr string) bool {
	if strings.HasPrefix(pr, "/") {
		return true
	}
	return len(pr) >= 3 && pr[1] == ':' && pr[2] == '/'
}
