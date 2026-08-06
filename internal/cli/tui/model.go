package tui

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/ctxcompact"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/i18n"
	"github.com/x6nux/yanshi/internal/keymap"
	"github.com/x6nux/yanshi/internal/proto"
)

// tuiSession is the subset of *cli.Session the TUI uses (avoids an import
// cycle cli -> tui -> cli). The concrete *cli.Session satisfies it; tests use
// fakeSession / scriptedSession / recordingSession.
type tuiSession interface {
	Send(text string) <-chan cli.StreamEvent
	// SendTurn delivers a turn that carries attachments or images. Distinct
	// from SendFrame: that one is for CONTROL frames, and on the WS backend it
	// closes the stream on the first control reply instead of on done.
	SendTurn(f proto.ClientFrame) <-chan cli.StreamEvent
	// SendFrame writes a Phase-10 control frame and returns the reply stream
	// (nil for permission_response, which carries no direct ack). Used by the
	// slash-command handlers (T32–T35).
	SendFrame(f proto.ClientFrame) <-chan cli.StreamEvent
	CancelCurrent() error
	Mode() string
	Root() string
}

// model is the Bubble Tea model holding the session, transcript entries, input
// box, viewport, and all per-session UI state. Its methods are split across
// files by concern (this file = struct + lifecycle: construct, Init/Update,
// submit, applyEvent; see entries.go / events.go / view.go / permissions.go /
// startup.go for the rest).
type model struct {
	sess    tuiSession
	entries []entry
	// startupBanner is the *startupEntry at entries[0]. Held on the model so
	// Update can append the async tool-probe rows to its info in place once
	// startupToolsMsg arrives — without scanning the slice or type-asserting.
	startupBanner *startupEntry
	input         textarea.Model
	viewport      viewport.Model
	spinner       spinner.Model
	width         int
	height        int
	streamCh      <-chan cli.StreamEvent
	// pending holds the in-flight assistant text. It is a plain string (NOT a
	// strings.Builder): the model is passed by value through bubbletea's
	// value-receiver Update/applyEvent, and a copied non-zero Builder panics on
	// the next WriteString. Concatenation is cheap for chat-sized text.
	pending string
	// assistantContinuation suppresses repeated "assistant:" labels when one
	// ReAct turn produces multiple assistant text blocks around tool calls.
	// It is reset for each user turn and set after the first flushAssistant.
	assistantContinuation bool
	canceling             bool
	toolsRun              int
	status                string

	// I18N1 + C15: bundle localizes the visible TUI surfaces; prefs is the
	// USER layer as read from disk (what /vim and friends mutate and write
	// back), project is the layer that came from config.yaml, effective is the
	// merged result, and prefsPath is the file prefs came from.
	//
	// Keeping prefs and project separate is load-bearing for persistence: only
	// the user layer may be written back, so a preference that arrived from
	// project config must not be re-persisted as if the user had chosen it —
	// that would silently pin a value the project could otherwise change.
	//
	// keys is the resolved keymap. It dispatches BEFORE the hardcoded key
	// switch in handlers.go, so a user override in tui.bindings actually takes
	// effect; the built-in defaults deliberately mirror what that switch
	// already does, or wiring the map would have moved keys nobody asked to
	// move (see keymap.NewDefaultBuilder).
	bundle    *i18n.Bundle
	prefs     Preferences
	project   Preferences
	effective EffectivePreferences
	prefsPath string
	keys      *keymap.Map
	// themeOverride is a session-only /theme choice. It exists because
	// remerge() recomputes theme from the persisted cascade, and without this
	// "/theme muted" followed by any preference command would silently revert
	// the colours — a cross-command interference with no visible cause.
	// /theme is documented as session-only, so the override is deliberately
	// NOT written to prefs.json.
	themeOverride ThemeName
	// vim is the modal state machine, non-nil exactly when vim mode is on.
	// Nil is the normal editing path, byte-identical to pre-C15.
	vim *keymap.VimMachine

	// Live activity status line (shown while m.streamCh != nil). activity is the
	// current step ("Thinking…" / "Running <tool>…"); turnStart is when the turn
	// began (for the mm:ss elapsed counter); glyphFrame advances each tick to
	// animate the leading star glyph.
	activity   string
	turnStart  time.Time
	glyphFrame int

	// lastEventAt is the wall-clock instant the most recent NON-thinking event
	// arrived (tool_result / agent_chunk / tool_call / submit / done / control-
	// reply flushes). applyEvent updates it at the top of every non-thinking
	// branch; submit sets it when a turn starts. appendThinkingDelta reads it to
	// stamp a new thinkingEntry.startedAt — see events.go for why this is more
	// accurate than time.Now() at the first reasoning delta.
	lastEventAt time.Time

	// retryAttempt/retryMax carry the latest transient-error retry of the model
	// call (a "retry" frame), so the activity line can show "↻ retry N/M…". They
	// are non-zero only between a retry frame and the next content/tool frame
	// (the retry resolved); retryErr is the triggering error for display.
	retryAttempt int
	retryMax     int
	retryErr     string

	// workDir is the basename of the project root (shown in the footer so the
	// user knows which directory the agent is working in); gitBranch is the
	// current branch when the root is inside a git repo ("" otherwise). Both are
	// detected at startup; gitBranch is refreshed by the fsnotify file watcher
	// whenever .git/HEAD changes (e.g. another terminal switched branches).
	workDir   string
	gitBranch string
	// rootPath is the full filesystem path of the project root, stored so that
	// gitBranch can be re-detected on each gitRefreshMsg without shelling out.
	rootPath string

	// In-app mouse text selection (the terminal's native selection is disabled
	// while mouse reporting is on, so the app implements selection itself).
	// selecting is true during a left-button drag. selAnchor*/selLine* are 2D
	// endpoints — {screen row, screen column}, (0,0) = top-left of the rendered
	// screen — so a drag can start and end mid-line and cover any region (the
	// transcript, the footer, even the input box). Carrying the column (not just
	// the row, as in T9) is what lets the selection be a character range rather
	// than whole lines, so an arbitrary fragment can be copied on release via
	// OSC 52 (T10). On motion, dragging to the top/bottom viewport edge also
	// auto-scrolls so the range can extend into off-screen content.
	selecting    bool
	selAnchorRow int
	selAnchorCol int
	selLineRow   int
	selLineCol   int

	// Command palette: when the input starts with "/" (and has no args yet),
	// paletteItems holds the filtered commands and paletteSel the highlighted
	// row. Up/Down moves the selection, Tab completes the name into the input,
	// Enter runs the selected command (when no args are typed).
	paletteItems []command
	paletteSel   int
	// paletteMCPServers is the MCP server status snapshot used to populate
	// palette MCP tool entries. Updated when an mcp_status frame arrives.
	paletteMCPServers []proto.MCPServerStatus

	// Phase-10 session state (T33), populated by status replies and shown in
	// the header.
	modelName     string
	thinking      string
	tokensIn      int
	tokensOut     int
	turns         int
	contextWindow int
	// cachedTokens/reasoningTokens break out two cost drivers that the raw
	// tokensIn/tokensOut totals hide (Task B8 / T8b): cachedTokens is the
	// cumulative count of prompt-cache hits (tokens spared from re-billing
	// by the provider's prompt cache), and reasoningTokens is the cumulative
	// count of tokens the model produced internally while reasoning (visible
	// spend on reasoning-class models). Both surface in the footer think/cache
	// segments and the /cost breakdown so the user can tell at a glance whether
	// they're paying for cache misses or for the reasoning model's thinking.
	cachedTokens    int
	reasoningTokens int

	// C4 COST1: cumulative USD estimate for the session. costKnown=false means
	// the session used at least one model absent from the pricing table; the
	// renderer shows "N/A" rather than $0.0000.
	costUSD   float64
	costKnown bool

	// pendingStatus is a *statusEntry appended by /config or /cost that is filled
	// in place when the status reply arrives (then cleared).
	pendingStatus *statusEntry
	// pendingStatsEntry is the *statsEntry appended by /stats, filled in place
	// when the session-list reply arrives (the same fetch /sessions uses). nil
	// means the next "sessions" frame routes to lastSessionsEntry instead.
	pendingStatsEntry *statsEntry
	// pendingSeamRestore (B2-RB1) is the active /restore-turn <id> request;
	// while non-nil, the next seam_restored / error frame resolves it.
	pendingSeamRestore *pendingSeamRestoreState
	// lastKnownHead caches the FULL main_head id from the last status / seams /
	// seam_restored frame, so the next /restore-turn <id> yes can re-bind it
	// without round-tripping (D6: full id, never the display short hash).
	lastKnownHead string
	// sessionID is the DB session id (carried on status frames). /fork updates
	// it to the new fork id; /clear resets to "". FN5 fix: case "session_restored"
	// ALSO assigns it (was missing in v2, so /restore left m.sessionID stale).
	sessionID string
	// memoryPath is the active memory file path (MEM1), surfaced on status
	// frames. /memory renders it in the footer.
	memoryPath string
	// logPath is the structured-log file path (empty = stderr), surfaced on
	// status frames. /logs tails this file in a popup.
	logPath string
	// sideDepth is the current ephemeral side-conversation depth (V11): 0 =
	// main, 1+ = inside a side. Footer renders "in side (N)".
	sideDepth int
	// pendingPermission is the active permission prompt; while non-nil a popup
	// renders above the input (tool/args/reason + Allow/Always allow/Deny) and
	// Up/Down + Enter (or y/a/n) resolves it. permSel is the highlighted option.
	pendingPermissions []*permissionEntry
	permSel            int

	permMode         guard.PermissionMode
	autoThreshold    int
	prePlanMode      guard.PermissionMode
	prePlanThreshold int
	yoloConfirm      int
	msgQueue         []string
	queueMode        QueueMode
	autoProcessing   bool

	// restoreSessions holds the session list when the restore picker is active.
	// Non-nil means the picker is open and Up/Down/Enter/Escape are captured.
	restoreSessions []proto.SessionInfo
	restoreCursor   int
	// restoreMode is a transient flag: when true, the next "sessions" frame reply
	// enters picker mode (instead of rendering a sessionsEntry).
	restoreMode bool

	// sessionsCache is the last session list the server sent, kept so the
	// Ctrl+K action palette can offer sessions without blocking on a round
	// trip. It mirrors the models cache exactly, including the staleness
	// problem: opening the palette refetches even when the cache is warm,
	// because a session created in another window is otherwise invisible.
	//
	// It is filled on EVERY "sessions" frame, whatever that frame was
	// requested for (/sessions, /stats, the restore picker). Routing the
	// display side and the cache side off the same reply is what keeps the
	// palette current without a second request per open.
	sessionsCache []proto.SessionInfo

	// Interactive command picker state for "/model", "/mode", "/theme" etc.
	// When pickerKind != "" the picker popup is shown and Up/Down/Enter/Escape
	// are captured for navigation. "model-loading" is a transient state while
	// waiting for the backend models list reply.
	pickerKind   string // "", "model", "mode", "theme"
	pickerItems  []pickerItem
	pickerCursor int

	// reflowDeb coalesces rapid input-driven reflow requests into a single
	// ~16ms-deferred reflow (see debounce.go). Only the bottom-of-Update
	// "input changed" path uses it; WindowSizeMsg / applyEvent / submit reflow
	// immediately because they represent discrete layout-changing events.
	reflowDeb debouncer
	// countReflow is a test-only hook: when non-nil, reflow() bumps *countReflow
	// on entry so TestInputDebounceCoalescesReflow can assert that a burst of
	// KeyMsgs is coalesced rather than driving one reflow per keystroke. It is
	// always nil in production.
	countReflow *int

	// inputPasted is set by the bracketed-paste / bulk-rune detection in Update
	// (tea.KeyMsg.Paste, or a single tea.KeyRunes with >50 runes — the
	// terminal's "I dropped a big paste" signal). submit() reads it to mark the
	// resulting userEntry as pasted (so render() collapses long pastes to
	// "[粘贴 #id]") and then resets it. A typed long message leaves it false,
	// so the message renders in full. See userEntry doc for the why.
	inputPasted bool

	// theme selects the footer colour theme (default / high-contrast / muted),
	// switchable at runtime via /theme. The default is used when empty.
	theme ThemeName

	// C2 — UX4 (frecency) + UX5/UX6/UX7 plumbing shared across the new
	// popups. frecency records file-write frequency for future UX sources;
	// models is the cached model list (populated lazily by the first /model
	// reply and consumed by the Ctrl+K action palette); saveQueue is the
	// single-worker save channel (see frecency.go for the design rationale).
	frecency  *Frecency
	models    []string
	saveQueue chan saveCmd

	// C2 — UX7: stacked toast notifications. toasts holds the visible
	// stack (FIFO ≤5); toastTickActive gates the independent toast tick
	// chain so multiple pushes in quick succession only start ONE tick
	// (the chain self-cleans when the queue drains).
	toasts          toastQueue
	toastTickActive bool

	// C2 — UX1: global action palette (Ctrl+K). action holds the popup
	// state (query + cursor + ranked items); actionLoadingModels is a
	// dedicated "list_models in flight" flag — NOT overloading pickerKind,
	// which would collide with the existing model picker's n==0 panic guard.
	action              *actionState
	actionLoadingModels bool

	// C2 — UX2: F1 help panel. helpVisible gates the popup; helpQuery is
	// the fuzzy filter (matches against Label+Hint). Read-only: no cursor,
	// no Enter action — the help panel exists for discovery, not invocation.
	// helpRendered is a cache of the popup's rendered string (refreshed on
	// every help-state mutation); reflow reads it to size the viewport
	// without going through helpPopup (which would form an init cycle:
	// commandTable → cmdModel → sendControlFrame → reflow → helpPopup →
	// ... → commandTable).
	helpVisible  bool
	helpQuery    string
	helpRendered string
	// helpCursor is the index into the ranked entry list that the visible
	// window is anchored on. Without it the panel rendered all 60-odd entries
	// and let bubbletea's renderer keep the LAST terminal-height lines, so the
	// title and the first ~35 commands were unreachable — the panel absorbs
	// every key except printable search characters, so there was nothing to
	// scroll with either.
	helpCursor int

	// C2 — UX5: draft stash (Ctrl+S / /stash). LIFO stack of multiline
	// drafts persisted as JSONL via the shared saveQueue (single worker).
	stash *Stash

	// C2 — UX6: prompt history. History is the FIFO of actually-sent
	// prompts (recorded by dispatchSend, NOT by submit/enqueue, so
	// queued-but-unsent text stays editable via Alt+↑ without polluting
	// history). historySearch is the popup state for Alt+R fuzzy search.
	history       *History
	historySearch *historyState

	// Tier G entry-A: clipboard image paste (Ctrl+V). pendingImages holds
	// attachments grabbed from the clipboard but not yet sent — they ride
	// the NEXT user turn and are cleared by it (see clipboard.go).
	// clipImage is the injectable grabber; nil means the real OS clipboard
	// (internal/clipimg). Tests substitute a fake to stay off the desktop.
	pendingImages []proto.ImageAttach
	clipImage     clipImageFunc
}

// newModel builds a model with no project preference layer. Kept as the
// package's default constructor because almost every test wants exactly that.
func newModel(sess tuiSession, root string) model {
	return newModelWithPrefs(sess, root, Preferences{})
}

// newModelWithPrefs resolves the four-layer preference cascade and builds the
// model from the result.
//
// The layers are defaults < project config < user prefs.json < env < flags
// (flags is empty: the TUI has no preference flags today). Everything the
// cascade decides — UI locale, theme, high contrast, vim, keymap — was
// previously hardcoded here: prefs was literally Preferences{}, prefsPath was
// "", and defaultBundle pinned English, so neither cfg.I18N.UILocale nor
// YANSHI_UI_LOCALE could change a single string on screen.
//
// A malformed prefs.json or a bad env boolean degrades to the lower layers
// rather than refusing to start: the TUI is the thing you would use to FIX a
// broken preference, so failing to open it is the worst available outcome.
func newModelWithPrefs(sess tuiSession, root string, project Preferences) model {
	vp := viewport.New(80, 10)
	sp := spinner.New()
	sp.Spinner = spinner.Dot

	prefsPath := preferencesPathFn()
	user, _ := loadPreferences(prefsPath)
	env, _ := PreferencesFromEnv(os.Getenv)
	eff := mergeTUIPrefs(Preferences{}, env, user, project)

	bundle, err := i18n.NewBundle(eff.UILocale)
	if err != nil {
		bundle = defaultBundle()
	}
	m := model{
		sess:          sess,
		input:         newInput(),
		viewport:      vp,
		spinner:       sp,
		status:        root,
		workDir:       dirName(root),
		gitBranch:     detectGitBranch(root),
		rootPath:      root,
		permMode:      loadSavedMode(),
		autoThreshold: loadSavedThreshold(),
		queueMode:     QueueModeQueue,
		theme:         themeForPrefs(eff),
		bundle:        bundle,
		prefs:         user,
		prefsPath:     prefsPath,
		project:       project,
		effective:     eff,
	}
	m.effective.UILocale = eff.UILocale
	m.keys = buildKeymap(eff, project)
	if eff.Vim {
		m.vim = keymap.NewVimMachine()
	}
	m.input.Placeholder = bundle.Get("tui.input.placeholder")
	// Startup banner: header (OS/Shell/Go/Date) renders instantly; tool rows
	// are appended asynchronously by probeStartupTools (see Init/Update) so the
	// TUI is interactive immediately instead of blocking boot on exec probes.
	m.startupBanner = &startupEntry{info: buildStartupHeader()}
	m.entries = append(m.entries, m.startupBanner)
	// C2 — frecency + saveQueue (single-worker serialised JSONL persistence
	// for Frecency/Stash/History; see frecency.go).
	// tui.frecency: false leaves m.frecency nil, and every consumer treats a
	// nil store as "no ranking" — which also stops the recording, because a
	// user who switched the feature off did not ask for a quieter version of
	// the same file-usage profile.
	if eff.Frecency {
		m.frecency, _ = LoadFrecency(frecencyPath())
	}
	m.stash, _ = LoadStash(stashPath())
	m.history, _ = LoadHistory(historyPath(), defaultHistoryCap)
	m.saveQueue = make(chan saveCmd, 16)
	m.refresh()
	return m
}

// NewProgram builds the bubbletea program for a session. Mouse cell-motion
// capture is ON so BOTH work with a plain mouse:
//   - the WHEEL scrolls the viewport (routed in the MouseMsg handler), and
//   - a LEFT-BUTTON DRAG selects text in-app (the terminal's native selection
//     is disabled while mouse reporting is on, so the app implements selection
//     itself: it tracks the drag over transcript lines, highlights them, and on
//     release copies the selected text to the system clipboard via OSC 52).
//
// Keyboard scrolling (PgUp/PgDn) is unaffected.
// project is the preference layer read from config.yaml (tui.* and
// i18n.ui_locale). It is the LOWEST layer above built-in defaults: prefs.json,
// the environment and flags all override it. Passing it explicitly rather than
// having the TUI load config itself keeps package tui free of internal/config,
// which is what lets cmd/yanshi stay the only place that knows both halves.
func NewProgram(sess *cli.Session, root string, project Preferences) *tea.Program {
	m := newModelWithPrefs(sess, root, project)
	return tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
}

func (m model) Init() tea.Cmd {
	// repaintTick is batched in alongside the startup Cmds so the 5s safety-net
	// heartbeat begins at launch (B2). It re-arms itself on every fire for the
	// lifetime of the program — see Update's repaintMsg case.
	// probeStartupTools runs the tool --version probes concurrently so the banner
	// populates without blocking boot (one probe hangs ~3s on Windows aliases).
	// C2: waitSave arms the single-worker saveQueue listener once at startup;
	// each saveCmd handler re-arms it, keeping the consumer chain alive for the
	// lifetime of the program (see frecency.go).
	return tea.Batch(
		waitSave(m.saveQueue),
		textarea.Blink,
		m.spinner.Tick,
		m.fetchInitialStatus(),
		m.fetchInitialMCP(),
		m.syncSavedMode(),
		repaintTick(),
		watchGitHead(m.rootPath),
		probeStartupTools(),
	)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case saveCmd:
		// C2 — single-worker save consumer. Execute fn, then unconditionally
		// re-arm waitSave so the queue always has a reader (consumer re-arm).
		// No select/default: that would let the queue briefly drain without a
		// reader, violating the "single serialised worker" guarantee.
		if msg.fn != nil {
			_ = msg.fn()
		}
		return m, waitSave(m.saveQueue)

	case toastTickMsg:
		// C2 — UX7: prune expired toasts, then reflow (blockHeight may have
		// changed). Re-arm the tick only while toasts remain; once the queue
		// is empty, drop toastTickActive so the next pushToast starts a fresh
		// chain. Note this is in Update (not applyEvent) because toast state
		// is independent of stream events.
		m.toasts.prune(time.Now())
		m.reflow()
		if len(m.toasts.items) > 0 {
			return m, tea.Tick(toastTickInterval, func(time.Time) tea.Msg {
				return toastTickMsg{}
			})
		}
		m.toastTickActive = false
		return m, nil

	case spinner.TickMsg:
		// Only animate while at least one tool is running.
		if m.anyToolRunning() {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
		return m, nil

	case activityTickMsg:
		// Advance the status-line glyph each tick while a turn is in flight.
		// No reflow needed: the status line is a constant 1 line, and returning
		// from Update triggers a View() redraw that picks up the new glyph and
		// the refreshed elapsed time. Re-arm only while still streaming.
		if m.streamCh != nil {
			m.glyphFrame++
			return m, activityTick()
		}
		return m, nil

	case startupToolsMsg:
		// Async tool probes resolved — append the rows beneath the instant
		// header (OS/Shell/Go/Date) that already rendered at boot. The banner
		// grows by a few lines, so reflow keeps the viewport aligned. Empty rows
		// (no tool probe succeeded) is a no-op.
		if m.startupBanner != nil && msg.rows != "" {
			m.startupBanner.info += "\n" + msg.rows
			m.reflow()
		}
		return m, nil

	case repaintMsg:
		// 5s safety-net heartbeat (B2): a full reflow + viewport refresh +
		// tea.Repaint so non-event-driven time-variant state (mm:ss elapsed
		// counter drift during long quiet tool calls, latent out-of-sync state
		// from any future patch that forgets to reflow) is rebuilt from current
		// state at most 5s stale. Re-arm unconditionally — the heartbeat runs
		// for the lifetime of the program, unlike activityTick which only fires
		// while streaming.
		m.reflow()
		m.refresh()
		return m, tea.Batch(repaintTick(), tea.Repaint)

	case gitRefreshMsg:
		// Refresh the git branch display when .git/HEAD changes (detected by the
		// fsnotify file watcher). Re-arm the watcher immediately so the next
		// branch switch is caught.
		m.gitBranch = detectGitBranch(m.rootPath)
		return m, watchGitHead(m.rootPath)

	case debounceMsg:
		// The deferred input reflow has come due. consume() first so the next
		// input mutation can arm a fresh tick, then reflow once with all the
		// keystrokes accumulated during the ~16ms coalescing window. growInput
		// and updatePalette are re-run because the deferred path skipped them
		// for coalesced keystrokes (they only ran on the first schedule()).
		// Returning nil Cmd is fine: there is nothing more to wait on.
		m.reflowDeb.consume()
		m.growInput()
		m.updatePalette()
		m.reflow()
		return m, nil

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// Size the input width first so reflow() measures the rendered block at
		// the new width; growInput() then syncs the textarea height to its
		// content and reflow() sizes the viewport to EXACTLY the remaining
		// height (terminal - header - status line - input) so the JoinVertical
		// total equals msg.Height with no overflow.
		m.input.SetWidth(msg.Width - 4) // border(2) + padding(2) horizontal chrome
		m.growInput()
		m.reflow()
		return m, nil

	case streamMsg:
		m = m.applyEvent(msg.ev)
		var cmds []tea.Cmd
		// C07: when a turn ends with queued messages waiting, drain per
		// queueMode. drainQueue's Cmd (from dispatchSend) arms waitForEvent +
		// activityTick for the next turn, so on a real drain we skip the
		// default waitForEvent arming below to avoid reading the stream twice.
		drained := false
		if msg.ev.Kind == "done" && len(m.msgQueue) > 0 {
			dm, dcmd := m.drainQueue()
			m = dm
			if dcmd != nil {
				cmds = append(cmds, dcmd)
				drained = true
			}
		}
		if !drained && m.streamCh != nil {
			cmds = append(cmds, m.waitForEvent())
		}
		if m.anyToolRunning() {
			cmds = append(cmds, m.spinner.Tick)
		}
		return m, tea.Batch(cmds...)

	case tea.KeyMsg:
		mm, cmd, handled := m.handleKeyMsg(msg)
		if handled {
			return mm, cmd
		}
		m = mm

	case tea.MouseMsg:
		// WithMouseCellMotion delivers wheel + left-button events. Wheel scrolls
		// the viewport; a left-button drag selects transcript text in-app (the
		// terminal's native selection is off while mouse reporting is on, so
		// the app implements selection: it highlights the dragged lines and, on
		// release, copies them to the system clipboard via OSC 52).
		switch msg.Button {
		case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
			// Wheel while selecting would scramble the line mapping; let the
			// active drag win (drop the wheel).
			if m.selecting {
				return m, nil
			}
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			// Force a full repaint so the footer/input don't go stale after the
			// viewport scrolls (the diff renderer can otherwise leave the bottom
			// unredrawn).
			return m, tea.Batch(cmd, tea.Repaint)
		case tea.MouseButtonLeft:
			return m, m.handleSelectMouse(msg)
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	// Sync the textarea height to its content (1–3 lines, scrolling past 3) and
	// refresh the command palette against the new input. Both are cheap and run
	// on every input mutation.
	m.growInput()
	m.updatePalette()
	// reflow is the expensive part (three blockHeight measurements + a
	// viewport.SetContent over the whole transcript), so it is debounced: the
	// first input mutation arms a ~16ms tick, subsequent ones within that
	// window are coalesced, and the tick handler does the actual reflow once.
	// This keeps pasting/deleting smooth (T9/T12) without delaying discrete
	// layout events (WindowSizeMsg / submit / applyEvent reflow immediately).
	if cmdDeb := m.reflowDeb.schedule(); cmdDeb != nil {
		return m, tea.Batch(cmd, cmdDeb)
	}
	return m, cmd
}

// submit handles an Enter-keyed input. An empty input is a no-op. A turn in
// flight routes to enqueue (C07): plain messages buffer per queueMode (single
// also cancels the running turn), and slash commands stay no-ops because control
// frames would clobber the in-flight streamCh via sendControlFrame. When idle,
// slash commands run as control frames and plain text starts a user turn via the
// shared dispatchSend kernel.
func (m model) submit() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return m, nil
	}
	if m.streamCh != nil {
		if strings.HasPrefix(text, "/") {
			return m, nil
		}
		return m.enqueue(text)
	}
	m.input.Reset()
	m.paletteItems = nil
	// Sync the textarea height back to 1 line now that it is empty, so the
	// post-submit layout doesn't keep the grown height.
	m.growInput()
	if strings.HasPrefix(text, "/") {
		return m.runCommand(text)
	}
	pasted := m.inputPasted
	// Reset the paste flag so the next turn starts clean — inputPasted must be
	// re-armed by a fresh bracketed-paste / bulk-rune event, not leak from the
	// previous turn.
	m.inputPasted = false
	mm, cmd := m.dispatchSend(text, pasted)
	return mm, cmd
}

// streamMsg wraps one backend event into a tea.Msg.
type streamMsg struct{ ev cli.StreamEvent }

func (m model) waitForEvent() tea.Cmd {
	ch := m.streamCh
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return streamMsg{ev: cli.StreamEvent{Kind: "done"}}
		}
		return streamMsg{ev: ev}
	}
}

func (m model) applyEvent(ev cli.StreamEvent) model {
	// lastEventAt threads the "previous non-thinking event" timestamp through to
	// appendThinkingDelta, which stamps a new thinkingEntry.startedAt from it. It
	// is updated HERE (for every non-thinking event) rather than enumerated in
	// each branch so the rule "non-thinking event ⇒ lastEventAt advances" stays
	// a single invariant. The thinking branch is the sole exception: thinking
	// itself is not a "previous non-thinking event", and stamping it would steal
	// the anchor from the next reasoning phase. See appendThinkingDelta for why
	// lastEventAt is more accurate than both time.Now() (misses TTFT) and
	// turnStart (includes cross-tool accumulated time).
	if ev.Kind != "thinking" {
		m.lastEventAt = time.Now()
	}
	switch ev.Kind {
	case "thinking":
		// Streaming reasoning delta (schema.Message.ReasoningContent). Append to
		// the last entry if it's a live thinkingEntry; otherwise start a new one.
		// Does not finalize on its own — the first non-thinking event does.
		//
		// Exception: when a nested agent tool (agent_start/workflow_start/
		// analysis/skill_use) is running, the nested agent's ReAct loop streams
		// its own "thinking" deltas back through us. Route those into the tool
		// block's nestedThought instead of opening a standalone thinkingEntry
		// above the call — otherwise the parent → child relationship would read
		// inverted (the child's reasoning would appear above the parent's call).
		m.retryAttempt = 0 // a pending retry resolved (content is flowing again)
		if t := m.lastRunningNestedTool(); t != nil {
			t.nestedThought += ev.Text
		} else {
			m.appendThinkingDelta(ev.Text)
		}
		m.activity = "Thinking…"
	case "agent_chunk":
		// Content arrived → reasoning (if any) has ended. Finalize the live
		// thinking block first so it collapses above the incoming answer.
		m.retryAttempt = 0 // a pending retry resolved (content is flowing again)
		m.finalizeLiveThinking()
		// Sub-agent chunk: a nested tool (Analysis/Agent/…) is running and the
		// child's ReAct loop is streaming its own text output back through us.
		// Route each line into the nested block's progress log instead of
		// appending to the PARENT's pending answer — otherwise the child's
		// output would read as the parent's, inverting the relationship.
		if nt := m.lastRunningNestedTool(); nt != nil {
			// Accumulate as continuous text (natural \n splits lines), not one
			// progress line per chunk — chunks are short text deltas, so
			// per-chunk lines produced "two chars per line" breakage.
			nt.nestedText += ev.Text
		} else {
			m.pending += ev.Text
			// Assistant text is streaming → we're thinking, not running a tool.
			m.activity = "Thinking…"
		}
	case "tool_call":
		m.retryAttempt = 0 // a pending retry resolved
		// Sub-agent tool_call: while a nested agent tool is running, any
		// tool_call arriving belongs to the CHILD's ReAct loop (the parent's
		// own call already created this nested entry). Route it inline as
		// "→ Read(foo.go)" rather than creating a top-level entry, so the
		// sub-agent's activity reads as indented activity under the call.
		// Bump nestedToolUses so the done summary can report how many tools
		// the sub-agent invoked ("N tool uses").
		if nt := m.lastRunningNestedTool(); nt != nil {
			// C3 format: ToolName(args) ◌ (running)
			disp := toolDisplayName(ev.ToolName)
			nt.nestedProgress = append(nt.nestedProgress,
				"Agent("+disp+") ◌")
			nt.nestedToolUses++
		} else {
			m.flushAssistant()
			// Flag agent-class tools so applyEvent's "thinking" branch can
			// route the nested agent's reasoning deltas into nestedThought.
			// startedAt brackets the Analysis block's lifetime for the done
			// summary's "Mm Ss" duration (endedAt is stamped when the parent's
			// tool_result resolves the entry).
			nested := toolDisplayFor(ev.ToolName) == toolDispAgent
			m.entries = append(m.entries, &toolEntry{
				name:      ev.ToolName,
				args:      ev.ToolArgs,
				root:      m.sess.Root(),
				status:    ev.ToolStatus,
				nested:    nested,
				startedAt: time.Now(),
			})
			if ev.ToolStatus == "running" {
				m.toolsRun++
				// A tool is now executing → reflect it in the status line.
				m.activity = "Running " + toolDisplayName(ev.ToolName) + "…"
			}
		}
	case "tool_result":
		// Parent tool's own result resolves the matching running entry first
		// (the name match distinguishes Analysis's own tool_result from the
		// child's). This catches the nested tool's own completion before the
		// sub-agent fallback below, so Analysis{ok} resolves the Analysis
		// block instead of appending to its nestedProgress.
		if t := m.lastRunningTool(ev.ToolName); t != nil {
			m.flushAssistant()
			t.result = ev.Text
			t.status = ev.ToolStatus
			// Stamp endedAt to bracket the block's lifetime for the done
			// summary's "Mm Ss" duration (startedAt was set on the tool_call).
			t.endedAt = time.Now()
			// C2 — UX4: record successful fs writes for frecency. Reads are
			// excluded (too noisy, and only writes signal "actually editing").
			// enqueueSave is non-blocking; applyEvent still returns no Cmd.
			if ev.ToolStatus != "running" {
				m.recordToolFrecency(t.name, t.args)
			}
		} else if m.lastRunningNestedTool() != nil {
			// C3 format: Agent(ToolName) N tools Nk tokens duration
			nt := m.lastRunningNestedTool()
			if len(nt.nestedProgress) > 0 {
				last := len(nt.nestedProgress) - 1
				line := nt.nestedProgress[last]
				// Replace "◌" with per-tool summary
				if strings.HasSuffix(line, " ◌") {
					prefix := strings.TrimSuffix(line, " ◌")
					var segs []string
					if nt.nestedToolUses > 0 {
						segs = append(segs, fmt.Sprintf("%d tools", nt.nestedToolUses))
					}
					if nt.nestedTokens > 0 {
						segs = append(segs, fmt.Sprintf("%dk", (nt.nestedTokens+500)/1000))
					}
					if !nt.startedAt.IsZero() {
						d := time.Since(nt.startedAt).Round(time.Second)
						if d > 0 {
							segs = append(segs, d.String())
						}
					}
					if len(segs) == 0 {
						segs = append(segs, "done")
					}
					nt.nestedProgress[last] = prefix + " " + strings.Join(segs, " ")
				}
			}
		} else {
			m.flushAssistant()
			// Standalone result (no preceding tool_call — e.g. out-of-order
			// delivery). Preserve the nested flag for agent-class tools so any
			// late-arriving thinking deltas still route into nestedThought.
			m.entries = append(m.entries, &toolEntry{
				name:      ev.ToolName,
				root:      m.sess.Root(),
				result:    ev.Text,
				status:    ev.ToolStatus,
				nested:    toolDisplayFor(ev.ToolName) == toolDispAgent,
				endedAt:   time.Now(),
				startedAt: time.Now(),
			})
		}
	case "tool_progress":
		if t := m.lastRunningTool(ev.ToolName); t != nil {
			t.progress = append(t.progress, ev.Text)
		}
	case "tool_chunk":
		// Stream chunk from a tool's Stream channel (Lane 4). Text is appended to
		// the tool block's progress (Overwrite=true replaces it). Status (right-
		// side panel) is stored on statusPanel and rendered in the header; Err
		// marks the block failed (status="error") via the existing tool_result
		// error path.
		if t := m.lastRunningTool(ev.ToolName); t != nil {
			if ev.Overwrite {
				t.progress = []string{ev.Text}
				t.progressOverwrite = true
			} else if ev.Text != "" {
				t.progress = append(t.progress, ev.Text)
			}
			if ev.ToolStatus != "" {
				t.statusPanel = ev.ToolStatus
			}
		}
	case "error":
		m.flushAssistant()
		// B2-RB1 必修项 I: clear any pending /restore-turn state so the UI does
		// not leave the user in a "reverting…" limbo when the server rejects.
		if m.pendingSeamRestore != nil {
			m.pendingSeamRestore = nil
		}
		m.entries = append(m.entries, errorEntry{text: ev.Text})
	case "models":
		// Reply to /model (no arg): when the model picker is open, populate
		// its items interactively; otherwise render the static list.
		// C2 — UX1: Ctrl+K action palette refresh path. We mirror the reply
		// into m.models (the palette's model source) regardless of which
		// path opened the request, and clear actionLoadingModels here (the
		// single source of truth — no premature "loaded" Msg in Update).
		// When Ctrl+K is the trigger (actionLoadingModels=true), the reply
		// MUST NOT append a transcript modelsEntry — the user did not ask
		// /model for a visible list, so a popup-only refresh is correct.
		m.flushAssistant()
		actionRefresh := m.actionLoadingModels
		m.models = append([]string(nil), ev.Items...)
		if m.pickerKind == "model" {
			m.pickerItems = make([]pickerItem, 0, len(ev.Items))
			for _, name := range ev.Items {
				m.pickerItems = append(m.pickerItems, pickerItem{
					name:    name,
					current: name == m.modelName,
				})
			}
			m.pickerCursor = 0
			m.refresh()
			m.viewport.GotoBottom()
		} else if !actionRefresh {
			m.entries = append(m.entries, modelsEntry{names: ev.Items})
		}
		m.actionLoadingModels = false
		if m.action != nil {
			// Active popup: recompute items (now including the fresh model
			// cache) and reset the cursor so the user's next keypress acts
			// on the new top-ranked row.
			m.action.items = m.rankedActions()
			m.action.cursor = 0
		}
	case "mcp_status":
		// Reply to /mcp or mcp_action: render MCP server status.
		m.flushAssistant()
		m.entries = append(m.entries, mcpStatusEntry{servers: ev.MCPServers})
		m.paletteMCPServers = ev.MCPServers
	case "sessions":
		// Reply to /sessions: fill the pending sessionsEntry in place,
		// OR enter restore picker mode when restoreMode is set,
		// OR fill the pending statsEntry (/stats reuses this fetch).
		m.flushAssistant()
		// The cache is updated before the routing switch, not inside a branch:
		// this frame answers four different requests and the palette wants the
		// list from all of them.
		m.sessionsCache = ev.Sessions
		switch {
		case m.restoreMode:
			m.restoreMode = false
			m.restoreSessions = ev.Sessions
			m.restoreCursor = 0
		case m.pendingStatsEntry != nil:
			m.pendingStatsEntry.sessions = ev.Sessions
			m.pendingStatsEntry = nil
		case m.lastSessionsEntry() != nil:
			m.lastSessionsEntry().sessions = ev.Sessions
		}
	case "session_restored":
		// Reply to /restore: fill the pending restoreEntry, re-render
		// the conversation, and update model state.
		m.flushAssistant()
		m.restoreSessions = nil // close picker if open

		// Re-render the conversation from restored messages.
		m.entries = nil
		for _, msg := range ev.Messages {
			// bug⑧: a compaction summary (user role + sentinel) is model
			// context, not conversational content — skip it so it doesn't
			// render as a chat bubble on session restore.
			if msg.Role == "user" && strings.HasPrefix(msg.Content, ctxcompact.SummarySentinel) {
				continue
			}
			if msg.Role == "user" {
				m.entries = append(m.entries, &userEntry{text: msg.Content})
			} else if msg.Role == "assistant" {
				m.entries = append(m.entries, assistantEntry{text: msg.Content})
			}
		}
		// Append restore confirmation.
		m.entries = append(m.entries, &restoreEntry{
			sessionID: ev.SessionID,
			count:     len(ev.Messages),
		})

		if ev.Model != "" {
			m.modelName = ev.Model
		}
		if ev.Thinking != "" {
			m.thinking = ev.Thinking
		}
		m.tokensIn = ev.TokensIn
		m.tokensOut = ev.TokensOut
		m.turns = ev.Turns
		// C4 COST1: mirror the restored cost so /cost right after /restore
		// shows the persisted spend, not a default $0.0000.
		m.costUSD = ev.CostUSD
		m.costKnown = ev.CostKnown
		// FN5 fix: v2 forgot to sync m.sessionID here, so after /restore the
		// TUI kept showing the pre-restore id. session_restored carries the
		// restored id explicitly; mirror it.
		m.sessionID = ev.SessionID
	case "session_ack":
		// Reply to /rename /archive /unarchive /delete: render a one-line ack.
		m.flushAssistant()
		m.entries = append(m.entries, ackEntry{
			text: formatSessionAck(ev.Action, ev.SessionID, ev.Text),
		})
	case "session_forked":
		// Reply to /fork: Task 7 already switched the SAME server connSession
		// to forkID before sending this frame. Mirror that active id locally;
		// no extra /restore is needed and the next turn persists only to the
		// fork.
		m.flushAssistant()
		m.entries = append(m.entries, ackEntry{
			text: "forked and switched to " + ev.SessionID,
		})
		m.sessionID = ev.SessionID
	case "side_state":
		// Reply to /side /btw /main (V11): update the depth indicator. The
		// footer renders "in side (N)" when sideDepth > 0 (see view.go).
		// applyStatus also handles this field for status frames, but side_state
		// arrives as a standalone control reply so it needs its own branch.
		m.flushAssistant()
		m.sideDepth = ev.SideDepth
		if ev.SideDepth > 0 {
			m.entries = append(m.entries, ackEntry{
				text: "entered side conversation (depth " + strconv.Itoa(ev.SideDepth) + ") — changes are not persisted",
			})
		} else {
			m.entries = append(m.entries, ackEntry{
				text: "returned to main thread (side discarded)",
			})
		}
	case "skills_list":
		// E03: reply to /skills. Render the registry snapshot as a transcript
		// block. flushAssistant first so the list does not glue onto a pending
		// assistant chunk.
		m.flushAssistant()
		m.entries = append(m.entries, skillsEntry{skills: ev.Skills})
	case "features":
		// OBS3: reply to /features. Render the feature flag table as a transcript
		// block. The features frame is a control reply (see isControlReply), so it
		// arrives even mid-turn. flushAssistant first (defensive).
		m.flushAssistant()
		m.entries = append(m.entries, featuresEntry{rows: ev.Features})
	case "skill_ack":
		// E03: reply to /skill install|uninstall|trust|untrust|enable|disable.
		// CB4: name comes only from Skill; StreamEvent has no Name field.
		m.flushAssistant()
		if ev.Text != "" {
			m.entries = append(m.entries, errorEntry{text: ev.Text})
			break
		}
		name := ""
		if ev.Skill != nil && ev.Skill.Name != "" {
			name = " " + ev.Skill.Name
		}
		text := strings.TrimSpace("skill" + name + " " + ev.Action)
		switch ev.Action {
		case "installed", "uninstalled", "enabled", "disabled":
			// FN3: registry changed immediately, baked model discovery did not.
			text += "; restart backend to refresh automatic skill discovery"
		}
		m.entries = append(m.entries, ackEntry{text: text})
	case "permissions":
		// Task 9: reply to /permissions. Render as a single transcript block
		// (header + one line per rule). flushAssistant first so the rule list
		// does not get glued onto a pending assistant chunk.
		m.flushAssistant()
		m.entries = append(m.entries, permissionsEntry{items: ev.Permissions})
	case "permission_rule_hit":
		// Task 9: audit event from the approval manager — hit/miss/consume/
		// expire/revoke. ev.ID is the rule id (empty for miss); ev.ToolStatus
		// is the lifecycle kind. One-line summary so the operator can audit
		// which rule admitted which call.
		m.flushAssistant()
		m.entries = append(m.entries, summaryEntry{
			text: fmt.Sprintf("permission rule %s: %s", ev.ID, ev.ToolStatus),
		})
	case "jobs":
		// Task 22: reply to /jobs. Render as a transcript block with one line
		// per job (id, pid, state, command).
		m.flushAssistant()
		m.entries = append(m.entries, jobsEntry{items: ev.Jobs})
	case "job_event":
		// Task 22: per-job lifecycle event (read output, stdin ack, cancel
		// ack). ev.ID is the job id; ev.ToolStatus is the state; ev.Text is
		// the output (for read). One-line summary so the operator can audit
		// /jobs interactions.
		m.flushAssistant()
		text := fmt.Sprintf("job %s: %s", ev.ID, ev.ToolStatus)
		if ev.Text != "" {
			text += " — " + ev.Text
		}
		m.entries = append(m.entries, summaryEntry{text: text})
	case "status":
		// Reply to get_status / set_model / set_thinking / clear / compact.
		// Always update header fields; fill a pending status block (/cost,
		// /config) if one is waiting; reset the activity line on a compaction
		// reply (history was replaced).
		m.applyStatus(ev)
		// B2-RB1 D6: refresh the full main_head binding so the next
		// /restore-turn <id> yes can re-bind it without another round-trip.
		if ev.Head != "" {
			m.lastKnownHead = ev.Head
		}
	case "seams":
		// B2-RB1: /restore-turn list reply. Cache the FULL security binding
		// (D6), and surface the pre-turn + pre-revert seams so the user can
		// revert to a prior turn OR undo a prior revert. post-turn / post-
		// revert seams are hidden (audit-only / not selector targets).
		m.flushAssistant()
		m.lastKnownHead = ev.Head
		preTurn := make([]proto.SeamInfo, 0, len(ev.Seams))
		for _, s := range ev.Seams {
			if s.Kind == "pre-turn" || s.Kind == "pre-revert" {
				preTurn = append(preTurn, s)
			}
		}
		m.entries = append(m.entries, seamsEntry{items: preTurn})
	case "seam_restored":
		// B2-RB1: /restore-turn <id> yes reply. The reply closes the control
		// channel, so do not rely on a later status frame to deliver the new
		// binding — cache its FULL Head now.
		m.flushAssistant()
		if ev.Head != "" {
			m.lastKnownHead = ev.Head
		}
		text := ev.Text
		if m.pendingSeamRestore != nil {
			m.pendingSeamRestore = nil
		}
		m.entries = append(m.entries, seamRestoredEntry{
			undoID:  ev.UndoSeamID,
			summary: text,
		})
	case "compact_chunk":
		// bug⑧: compaction is a meta-op; its status lives on the activity line
		// (the "Running…" row rendered separately from the transcript), NOT as a
		// transcript entry. Summary deltas are not shown as chat content.
		m.activity = "Compacting context…"
	case "retry":
		// Transient-error retry of the model call (a mid-stream "unexpected EOF"
		// etc.). Overwrite: discard the partial output from the failed attempt
		// (pending assistant text + the last thinking block) so the regenerated
		// stream replaces it rather than appending to it. retryAttempt/retryMax
		// drive the "↻ retry N/M…" activity segment; cleared on the next
		// content/tool frame, which means the retry resolved.
		m.pending = ""
		m.discardLastThinking()
		m.retryAttempt = ev.RetryAttempt
		m.retryMax = ev.RetryMax
		m.retryErr = ev.Text
	case "permission_request":
		// A tool call needs interactive approval. Show it as a popup above the
		// input (Allow / Always allow / Deny, navigable with Up/Down + Enter or
		// the y/a/n shortcuts) rather than a transcript entry.
		m.flushAssistant()
		// mandatory ORs the two server-side "explicit decision only" flags:
		// ApprovalRequired (github mutations) and ForcePrompt (task_cancel via
		// forcePromptTools, revert_turn via RequireApproval). Both must keep the
		// popup alive across a mode switch and both must hide the sticky-allow
		// options — the server discards a sticky answer for them anyway.
		m.pendingPermissions = append(m.pendingPermissions, &permissionEntry{id: ev.ID, tool: ev.ToolName, args: ev.ToolArgs, reason: ev.Reason, mandatory: ev.ApprovalRequired || ev.ForcePrompt})
		m.permSel = 0
	case "done":
		m.flushAssistant()
		// Turn-end summary ("Done N tools uses X tokens Y") rides on the
		// done frame's Text. Append it as a summaryEntry (dim grey, like the
		// "Thought for Xs" line) so it reads as a transcript annotation,
		// not as the assistant's answer.
		if ev.Text != "" {
			m.entries = append(m.entries, summaryEntry{text: ev.Text})
		}
		m.streamCh = nil
		m.canceling = false
		// Clear the live activity status line state now that the turn is over.
		m.activity = ""
		m.glyphFrame = 0
		m.retryAttempt = 0
		// Drop any still-pending interactive state: a stream ending (normally or
		// via a server-side permission timeout) means no more replies are coming.
		// A lingering pendingStatus would be filled by an unrelated later status
		// frame; a lingering pendingPermission would send a stale response.
		m.pendingStatus = nil
		m.pendingPermissions = nil
	}
	// reflow (not just refresh): the status line appears/disappears across
	// events (submit→streaming, done→idle), and the viewport must track its
	// 1-line height so the JoinVertical total stays exact. Cheap: three string
	// measurements, then refresh.
	m.reflow()
	m.viewport.GotoBottom()
	return m
}
