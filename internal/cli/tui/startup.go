package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/fsnotify/fsnotify"

	"github.com/x6nux/yanshi/internal/execprobe"
	"github.com/x6nux/yanshi/internal/version"
)

// activityGlyphs is the star sequence cycled each tick to animate the leading
// glyph of the live status line (signals "running" — Claude-Code-style blink).
var activityGlyphs = []string{"✢", "✣", "✤", "✥"}

// activityTickMsg drives the glyph animation of the live status line. It is
// armed in submit()/sendControlFrame() and re-armed on each tick while a turn
// is in flight (m.streamCh != nil); it stops firing once the turn ends.
type activityTickMsg time.Time

// activityTick returns a Cmd that fires an activityTickMsg after ~42ms (~24
// FPS). The ~24 FPS rate is chosen as the sweet spot for the leading-glyph
// animation of the live status line: smooth enough that "✢✣✤✥" reads as a
// continuous spin (the four-frame cycle completes ~6 times per second), cheap
// enough that it adds negligible CPU on top of the transcript reflow. The
// previous 500ms tick made the glyph visibly stutter and the elapsed-time mm:ss
// counter jump in half-second steps (T13/T15).
func activityTick() tea.Cmd {
	return tea.Tick(42*time.Millisecond, func(t time.Time) tea.Msg {
		return activityTickMsg(t)
	})
}

// repaintMsg is the third render-rhythm layer (B1 = 24 FPS activityTick, B1 =
// 16ms input debounce; B2 = 5s repaintTick). It fires a full reflow + viewport
// refresh + tea.Repaint so that:
//
//  1. Non-event-driven time-variant rendering stays correct — e.g. the mm:ss
//     elapsed counter in the activity line could otherwise freeze for the
//     duration of a long tool call that produces no events, and the activity
//     line itself could lag if the diff renderer dropped frames.
//  2. Any latent state that has fallen out of sync (e.g. a layout field that a
//     future patch forgets to reflow) is rebuilt from current state, so the
//     worst case is "1 frame stale" rather than "stuck forever".
//
// Unlike activityTickMsg (24 FPS, glyph-only, armed only while streaming) the
// repaintTick is armed UNCONDITIONALLY for the lifetime of the program — the
// 5s heartbeat is the safety net below all event-driven rendering.
type repaintMsg struct{}

// repaintTick returns a Cmd that fires a repaintMsg after 5s. The 5s interval
// is chosen so that the safety-net reflow runs once every ~5s — well below the
// threshold where a user would notice drift, while adding negligible CPU on
// top of the 24 FPS animation (it represents a ~0.2% overhead vs. a fully idle
// program). Re-armed on every fire in Update's repaintMsg case.
func repaintTick() tea.Cmd {
	return tea.Tick(5*time.Second, func(time.Time) tea.Msg { return repaintMsg{} })
}

// gitRefreshMsg signals that .git/HEAD has changed (detected by the file watcher).
// The Update handler re-reads the branch name and re-arms the watcher.
type gitRefreshMsg struct{}

// watchGitHead sets up a fsnotify watcher on the .git directory and blocks until
// HEAD is modified (e.g. by git checkout in another terminal). It returns a
// gitRefreshMsg so the footer branch display stays in sync. Returns nil when the
// root is not a git repository (or .git is not a directory), so the footer simply
// never gets a branch indicator in non-git projects.
func watchGitHead(rootPath string) tea.Cmd {
	if rootPath == "" {
		return nil
	}
	gitDir := filepath.Join(rootPath, ".git")
	// Only start watching when .git is a real directory (not a worktree file,
	// not absent — detectGitBranch has the same limitation).
	fi, err := os.Stat(gitDir)
	if err != nil || !fi.IsDir() {
		return nil
	}
	return func() tea.Msg {
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			return nil
		}
		defer watcher.Close()
		if err := watcher.Add(gitDir); err != nil {
			return nil
		}
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return nil
				}
				if filepath.Base(event.Name) == "HEAD" &&
					event.Op&fsnotify.Write != 0 {
					return gitRefreshMsg{}
				}
				// Non-HEAD event (e.g. index, objects): keep watching.
			case <-watcher.Errors:
				return nil
			}
		}
	}
}

// gitStatusMsg carries the result of the async git diff-stat and gh PR lookup
// (W-E-09). Both are fetched in a single background tea.Cmd started at
// Init-time and on every gitRefreshMsg so the footer stays live.
type gitStatusMsg struct {
	diffStat string // "+N -M" vs default branch, or "" when on default/no git
	prURL    string // open PR URL for the current branch, or ""
	prTitle  string // PR title, or ""
}

// fetchGitStatus runs `git rev-parse --abbrev-ref HEAD@{upstream}` to find the
// upstream tracking branch, then `git diff --shortstat <upstream>...HEAD` to
// get added/removed counts, and `gh pr view --json url,title` for the open PR.
// All shell-outs are gated by execprobe.Run's 3-second deadline so a slow git
// or absent gh never hangs the TUI.
//
// The fallback chain is strictly additive: missing git → no diffStat; missing
// gh or no PR → empty prURL. The caller renders whatever arrives.
func fetchGitStatus(rootPath string) tea.Cmd {
	if rootPath == "" {
		return nil
	}
	return func() tea.Msg {
		msg := gitStatusMsg{}

		// 1. Diff-stat vs upstream. Run in the project root by temporarily
		//    changing to it; execprobe.Run does not take a working directory so
		//    we use exec.Command directly here (gated by the same 3s deadline
		//    pattern as execprobe.Run).
		upstream := strings.TrimSpace(runInDir(rootPath, "git", "rev-parse",
			"--abbrev-ref", "--symbolic-full-name", "@{upstream}"))
		if upstream != "" && !strings.HasPrefix(upstream, "fatal") {
			stat := strings.TrimSpace(runInDir(rootPath, "git", "diff",
				"--shortstat", upstream+"...HEAD"))
			// stat example: " 3 files changed, 47 insertions(+), 12 deletions(-)"
			added := extractGitCount(stat, "insertion")
			deleted := extractGitCount(stat, "deletion")
			if added+deleted > 0 {
				msg.diffStat = fmt.Sprintf("+%d -%d", added, deleted)
			}
		}

		// 2. Open PR via gh. Absent gh or non-zero exit → empty, handled by
		//    runInDir returning "".
		prJSON := strings.TrimSpace(runInDir(rootPath, "gh", "pr", "view",
			"--json", "url,title"))
		if prJSON != "" && strings.Contains(prJSON, `"url"`) {
			msg.prURL = extractJSONString(prJSON, "url")
			msg.prTitle = extractJSONString(prJSON, "title")
		}

		return msg
	}
}

// runInDir runs a command in a given working directory with a 3s deadline.
// Returns the first line of combined output, or "" on any error.
func runInDir(dir, cmd string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, cmd, args...)
	c.Dir = dir
	type result struct {
		out []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := c.CombinedOutput()
		ch <- result{out, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil || ctx.Err() == context.DeadlineExceeded {
			return ""
		}
		lines := strings.SplitN(string(r.out), "\n", 2)
		return strings.TrimSpace(lines[0])
	case <-ctx.Done():
		return ""
	}
}

// extractGitCount pulls the first integer before "insertion" or "deletion"
// from a git --shortstat line. Returns 0 on no match.
func extractGitCount(stat, keyword string) int {
	idx := strings.Index(stat, keyword)
	if idx < 0 {
		return 0
	}
	// Scan backwards past any letters/spaces to find the number.
	part := stat[:idx]
	part = strings.TrimRight(part, " \t(")
	spaceIdx := strings.LastIndexAny(part, " \t,")
	if spaceIdx >= 0 {
		part = part[spaceIdx+1:]
	}
	n := 0
	fmt.Sscanf(strings.TrimSpace(part), "%d", &n)
	return n
}

// extractJSONString extracts the value of a simple string key from a JSON
// object without importing encoding/json (which would pull in a much larger
// dependency surface here). Handles only flat {"key":"value"} shapes.
func extractJSONString(j, key string) string {
	needle := `"` + key + `":"`
	idx := strings.Index(j, needle)
	if idx < 0 {
		return ""
	}
	rest := j[idx+len(needle):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// blockFont maps uppercase letters to 5-row × 5-column block glyphs (█ = full
// cell, space = empty). Only the letters in "YANSHI" are defined; unknown runes
// are skipped so a rename degrades to a partial logo instead of crashing.
var blockFont = map[rune][]string{
	'Y': {"█   █", " █ █ ", "  █  ", "  █  ", "  █  "},
	'A': {" ██  ", "█   █", "█████", "█   █", "█   █"},
	'N': {"█   █", "██  █", "█ █ █", "█  ██", "█   █"},
	'S': {" ███ ", "█    ", " ███ ", "    █", " ███ "},
	'H': {"█   █", "█   █", "█████", "█   █", "█   █"},
	'I': {"█████", "  █  ", "  █  ", "  █  ", "█████"},
}

// logoRowColors is a 5-step vertical gradient (cyan → teal → lilac → magenta →
// pink) applied row-by-row across the block logo. The endpoints tie back to the
// existing palette: top ≈ toolName cyan, bottom ≈ roleAsst pink, so the banner
// reads as part of the transcript rather than a foreign splash.
var logoRowColors = []lipgloss.Color{
	lipgloss.Color("39"),
	lipgloss.Color("44"),
	lipgloss.Color("177"),
	lipgloss.Color("207"),
	lipgloss.Color("213"),
}

// renderBlockLogo returns the multi-line block-text logo for s (uppercased),
// each of the 5 rows colored by the gradient, with a 1-cell gutter between
// letters. Returns "" when s has no renderable letters. Caller is responsible
// for gating on terminal width (the logo is len(letters)×6−1 cells wide).
func renderBlockLogo(s string) string {
	const rows = 5
	var glyphs [][]string
	for _, ch := range strings.ToUpper(s) {
		if g, ok := blockFont[ch]; ok && len(g) == rows {
			glyphs = append(glyphs, g)
		}
	}
	if len(glyphs) == 0 {
		return ""
	}
	var b strings.Builder
	for row := 0; row < rows; row++ {
		var line strings.Builder
		for _, g := range glyphs {
			line.WriteString(g[row])
			line.WriteByte(' ')
		}
		styled := lipgloss.NewStyle().Foreground(logoRowColors[row]).Render(strings.TrimRight(line.String(), " "))
		b.WriteString(styled)
		if row < rows-1 {
			b.WriteByte('\n') // no trailing newline — caller controls joining
		}
	}
	return b.String()
}

// startupEntry is the banner shown once when the TUI starts. It is a POINTER
// entry (like *toolEntry) so the async tool-probe result can mutate info in
// place: the header (OS/Shell/Go/Date) renders INSTANTLY at model creation, and
// the tool rows are appended once the concurrent probes resolve — the TUI is
// interactive immediately instead of blocking boot on exec probes (one of which
// hangs ~3s on Windows App Execution Aliases).
type startupEntry struct{ info string }

// startupBoxMin/Max clamp the banner's inner content width (excluding border +
// padding). Min fits the 35-cell logo with breathing room; Max keeps it from
// stretching across a huge terminal. The box left margin (startupBoxMargin)
// insets the frame from the terminal edge — visible but "not too far".
const (
	startupBoxMin    = 44
	startupBoxMax    = 60
	startupBoxMargin = 2
)

func (e *startupEntry) render(width int, _ spinner.Model) string {
	// Clamp inner content width to [min, max] within the available terminal
	// width (terminal − left margin − border − padding). width == 0 (pre
	// WindowSizeMsg) falls back to Max optimistically.
	inner := startupBoxMax
	if width > 0 {
		avail := width - startupBoxMargin - 4 // margin + 2 borders
		switch {
		case avail < startupBoxMin:
			inner = startupBoxMin
		case avail < startupBoxMax:
			inner = avail
		}
	}

	// Logo: centered. We DON'T use lipgloss Align(Center) here because it
	// centers each row independently by that row's measured width — and the
	// block glyphs have unequal trailing whitespace per row (the "I" letter's
	// middle rows are "  █  "), so rows measure 33–35 wide and shift by a
	// column or two, making the letters zigzag. Instead we left-pad every row
	// by the SAME amount, computed once from the logo's nominal width, so the
	// left edge is a straight line (trailing differences are invisible inside
	// the box).
	logo := renderBlockLogo("yanshi")
	if logo == "" || inner < 40 {
		logo = roleAsst.Render("yanshi")
	}
	logoWidth := len([]rune("yanshi"))*6 - 1 // 6 letters × (5 glyph + 1 gutter) − 1 trailing gutter
	pad := 0
	if inner > logoWidth {
		pad = (inner - logoWidth) / 2
	}
	leftPad := strings.Repeat(" ", pad)
	logoLines := strings.Split(logo, "\n")
	for i, l := range logoLines {
		logoLines[i] = leftPad + l
	}
	logoBlock := strings.Join(logoLines, "\n")

	// Version subtitle: centered, dim. Build stamp (YYMMDDHHMM) is appended to
	// the version when ldflags injected one at compile; git hash follows after "·".
	ver := "v" + version.Version
	if version.BuildStamp != "" {
		ver += "." + version.BuildStamp
	}
	if h := version.GitHash; h != "" {
		ver += "  ·  " + h
	}
	versionBlock := toolMeta.Copy().Width(inner).Align(lipgloss.Center).Render(ver)

	// Info table: left-aligned with a 2-cell indent so it isn't flush to the
	// left border.
	infoBlock := toolMeta.Copy().Width(inner).Align(lipgloss.Left).PaddingLeft(2).
		Render(strings.TrimRight(e.info, "\n"))

	// Stack with blank spacer lines (full inner width) for vertical breathing
	// room. Every line is exactly `inner` wide so the rounded border stays a
	// clean rectangle (no jagged right edge).
	blank := strings.Repeat(" ", inner)
	content := strings.Join([]string{
		blank, logoBlock, blank, versionBlock, blank, infoBlock, blank,
	}, "\n")

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("99")). // soft purple frame
		MarginLeft(startupBoxMargin).
		Render(content) + "\n"
}

// startupToolsMsg carries the tool rows assembled by probeStartupTools. Update
// appends them to the banner's info so the header is never blanked — rows just
// appear beneath it once the probes land.
type startupToolsMsg struct{ rows string }

// buildStartupHeader returns the probe-free banner rows (OS, Shell, Go, Date).
// These come from the Go runtime and env vars, so they cost zero exec and render
// the instant the model is created.
func buildStartupHeader() string {
	var b strings.Builder
	add := func(key, val string) {
		fmt.Fprintf(&b, "%-8s %s\n", key+":", val)
	}
	add("OS", fmt.Sprintf("%s %s", runtime.GOOS, runtime.GOARCH))
	add("Shell", detectShellEnv())
	add("Go", runtime.Version()[2:])
	add("Date", time.Now().Format("2006-01-02"))
	return strings.TrimRight(b.String(), "\n")
}

// probeStartupTools returns a Cmd that runs every tool --version probe
// CONCURRENTLY and returns the assembled rows as a startupToolsMsg. Concurrent
// execution means the batch resolves in max(probe), not sum(probe); each probe
// is bounded by execprobe.Run's 3s deadline (and Windows App Execution Aliases
// are skipped outright), so the banner populates without blocking boot.
func probeStartupTools() tea.Cmd {
	tools := []struct{ label, spec string }{
		{"Node.js", "node --version"},
		{"Python", "python --version"},
		{"Rust", "rustc --version"},
		{"Git", "git --version"},
		{"npm", "npm --version"},
	}
	return func() tea.Msg {
		type cell struct{ label, val string }
		cells := make([]cell, len(tools))
		var wg sync.WaitGroup
		for i, t := range tools {
			wg.Add(1)
			go func(i int, t struct{ label, spec string }) {
				defer wg.Done()
				cells[i] = cell{t.label, probeLine(t.spec)}
			}(i, t)
		}
		wg.Wait()
		var b strings.Builder
		for _, c := range cells {
			if c.val == "" {
				continue
			}
			fmt.Fprintf(&b, "%-8s %s\n", c.label+":", c.val)
		}
		return startupToolsMsg{rows: strings.TrimRight(b.String(), "\n")}
	}
}

func probeLine(spec string) string {
	parts := strings.Fields(spec)
	if len(parts) == 0 {
		return ""
	}
	// Delegates to execprobe.Run for a hard deadline that also covers a
	// CreateProcess hang (Windows App Execution Aliases like python3.exe block
	// non-interactively); the previous context-only timeout couldn't kill a
	// process that hadn't been created yet, so a stalled probe hung TUI boot.
	return execprobe.Run(parts[0], parts[1:]...)
}

func detectShellEnv() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	if runtime.GOOS == "windows" {
		if _, err := exec.LookPath("bash"); err == nil {
			return "bash (Git Bash)"
		}
		if os.Getenv("PSModulePath") != "" {
			return "powershell"
		}
		return "cmd"
	}
	return "sh"
}

// formatDuration renders a duration as mm:ss (e.g. 1m12s → "1:12").
func formatDuration(d time.Duration) string {
	sec := int(d.Seconds())
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}

// formatDurationWords renders a duration as "Xm Ys" (e.g. 17m55s → "17m 55s"),
// the word-delimited counterpart to formatDuration's "MM:SS". Used by the
// Analysis block's done summary where the word form reads more naturally
// alongside the "N tool uses · Nk tokens" segments than a bare "17:55" would.
// Sub-minute durations render as "Ns"; scales up through Nm/Nh/Nd so a 65-minute
// Analysis reads "1h 5m 0s" and a 30-hour one reads "1d 6h 0m 0s" rather than
// an ambiguous "65m 0s" / "30h 0m 0s".
func formatDurationWords(d time.Duration) string {
	totalSec := int(d.Seconds())
	if totalSec < 60 {
		return fmt.Sprintf("%ds", totalSec)
	}
	m, s := totalSec/60, totalSec%60
	if m < 60 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h, m := m/60, m%60
	if h < 24 {
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	}
	days := h / 24
	h = h % 24
	return fmt.Sprintf("%dd %dh %dm %ds", days, h, m, s)
}
