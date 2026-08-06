// Package tui — Bubble Tea TUI model for the Yanshi LLM agent.
//
// This file holds helper types, value types, and stateless functions that were
// extracted from model.go so that model.go stays under the 1000-pure-code-line
// limit enforced by the GOV2 architecture gate (internal/archtest/lines_test.go).
//
// No behavioral changes; this is a pure structural split (Phase A of Task 6).
package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/i18n"
	"github.com/x6nux/yanshi/internal/proto"
)

// QueueMode controls how messages queued while a turn is in flight are handled.
type QueueMode int

// Queue mode values.
const (
	QueueModeQueue QueueMode = iota
	QueueModeSingle
	QueueModeBatch
)

// String returns the canonical name for the queue mode.
func (qm QueueMode) String() string {
	switch qm {
	case QueueModeSingle:
		return "single"
	case QueueModeBatch:
		return "batch"
	default:
		return "queue"
	}
}

func parseQueueMode(s string) (QueueMode, bool) {
	switch s {
	case "", "queue":
		return QueueModeQueue, true
	case "single":
		return QueueModeSingle, true
	case "batch":
		return QueueModeBatch, true
	}
	return QueueModeQueue, false
}

// pickerItem represents a selectable option in an interactive command picker
// (used by /model, /mode, /theme when no argument is given). The user navigates
// with ↑/↓, confirms with Enter, cancels with Escape.
type pickerItem struct {
	name        string
	description string // optional one-line description (shown for themes)
	current     bool   // is this the currently active item
}

// pendingSeamRestoreState (B2-RB1) is the in-flight /restore-turn <id> yes
// request. The next seam_restored (or error) frame resolves it.
type pendingSeamRestoreState struct {
	seamID string
}

// pickerConfirm resolves the active command picker: closes it, performs the
// selected action (set_model / set_mode / set_theme), and renders feedback.
func (m model) pickerConfirm() (tea.Model, tea.Cmd) {
	if len(m.pickerItems) == 0 {
		m.pickerKind = ""
		m.reflow()
		return m, nil
	}
	sel := m.pickerItems[m.pickerCursor]
	kind := m.pickerKind
	m.pickerKind = ""
	m.pickerItems = nil

	switch kind {
	case "model":
		return m.sendControlFrame(proto.NewSetModel(sel.name))
	case "mode":
		pm, ok := guard.NormalizeMode(sel.name)
		if !ok {
			m.entries = append(m.entries, errorEntry{text: "unknown mode: " + sel.name})
			m.refresh()
			m.viewport.GotoBottom()
			return m, nil
		}
		m.permMode = pm
		return m.sendMode()
	case "theme":
		tn := ThemeName(sel.name)
		if _, ok := themeByName(tn); !ok {
			m.entries = append(m.entries, errorEntry{text: "unknown theme: " + sel.name})
			m.refresh()
			return m, nil
		}
		m.theme = tn
		m.entries = append(m.entries, errorEntry{text: okStyle.Render("✓ theme: " + string(tn))})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	return m, nil
}

// defaultBundle returns the English catalog bundled with the binary. Used by
// newModel so the TUI always has a non-nil *i18n.Bundle even when the caller
// did not opt into Preferences. Construction cannot fail because the en
// catalog is go:embed'd — a missing/invalid en.json is a build-time bug, so
// we panic to surface it loudly rather than masking with a fallback.
func defaultBundle() *i18n.Bundle {
	b, err := i18n.NewBundle("en")
	if err != nil {
		panic(fmt.Sprintf("i18n: default en catalog unusable: %v", err))
	}
	return b
}

// dirName returns the basename of root (the directory name without its path),
// or root itself when filepath.Base can't shorten it (e.g. "."). Used for the
// footer's directory indicator.
func dirName(root string) string {
	if root == "" {
		return ""
	}
	if b := filepath.Base(root); b != "" && b != "." && b != string(filepath.Separator) {
		return b
	}
	return root
}

// detectGitBranch returns the current git branch name when root is inside a git
// repository (parsed from .git/HEAD's "ref: refs/heads/<name>" line by walking
// up from root), or "" otherwise. No shell-out: it reads the .git/HEAD file
// directly, so it works without git on PATH. A detached HEAD (raw sha in HEAD)
// returns the short sha prefixed with ":".
func detectGitBranch(root string) string {
	dir := root
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	for d := abs; ; {
		b, err := os.ReadFile(filepath.Join(d, ".git", "HEAD"))
		if err == nil {
			return parseGitHead(strings.TrimSpace(string(b)))
		}
		parent := filepath.Dir(d)
		if parent == d {
			break // reached the filesystem root
		}
		d = parent
	}
	return ""
}

// parseGitHead extracts the branch name from a .git/HEAD body: a
// "ref: refs/heads/<branch>" line yields <branch>; anything else (a detached
// HEAD raw object id) yields a short ":" + first 7 chars so it is distinguishable.
func parseGitHead(s string) string {
	const prefix = "ref: refs/heads/"
	if strings.HasPrefix(s, prefix) {
		return strings.TrimPrefix(s, prefix)
	}
	if len(s) >= 7 {
		return ":" + s[:7]
	}
	return s
}

// fetchInitialStatus sends a get_status control frame and reads a single reply,
// returning it as a streamMsg so applyEvent populates the header (model, context
// window, thinking, usage) immediately on launch. WS replies with one status
// frame then closes; SSE returns ErrSSEControlUnsupported (no control frames),
// in which case the reply is not a status event and we return nil so launch
// stays clean rather than rendering an error block. m.streamCh is intentionally
// NOT set here, so this one-shot fetch does not block subsequent input or arm
// waitForEvent.
func (m model) fetchInitialStatus() tea.Cmd {
	ch := m.sess.SendFrame(proto.NewGetStatus())
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok || ev.Kind != "status" {
			return nil // SSE / error / no reply: keep the header on defaults
		}
		return streamMsg{ev: ev}
	}
}

// fetchInitialMCP asks for the MCP server snapshot at launch.
//
// Nothing did. mcp_status is only sent in reply to list_mcp or mcp_action,
// and the only sender of list_mcp was the /mcp command -- so
// paletteMCPServers stayed empty until the user happened to type /mcp, and
// until then the palette offered no MCP tools at all. A user who never runs
// /mcp (there is no reason to, if the palette is where you discover tools)
// would conclude the servers they configured do nothing.
//
// A nil reply is fine and expected: SSE installs no control-frame path, and a
// build with no MCP servers answers with an empty list.
func (m model) fetchInitialMCP() tea.Cmd {
	ch := m.sess.SendFrame(proto.NewListMCP())
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok || ev.Kind != "mcp_status" {
			return nil
		}
		return streamMsg{ev: ev}
	}
}

// syncSavedMode applies the locally persisted permission mode to the server
// when a new TUI connection is opened. Without this, the footer can show the
// saved mode while the server still evaluates permission requests as default.
func (m model) syncSavedMode() tea.Cmd {
	mode := m.permMode
	if mode == "" {
		mode = guard.ModeDefault
	}
	threshold := m.autoThreshold
	if threshold == 0 && mode == guard.ModeAuto {
		threshold = guard.DefaultAutoThreshold
	}
	ch := m.sess.SendFrame(proto.NewSetMode(string(mode), threshold))
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		for range ch {
		}
		return nil
	}
}
