package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/i18n"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
)

// commandKind 区分 palette 条目类型：slash 命令 / MCP 工具 / MCP 分组标题。
type commandKind int

const (
	cmdSlash    commandKind = iota // 普通 / 命令
	cmdMCPTool                     // MCP 工具条目（name=qualified 运行时名）
	cmdMCPGroup                    // 分组标题（不可选）
	cmdAtPath                      // UX4：@path 文件补全条目（name=相对路径）
	// cmdKindSkillRun is a skill-declared command offered under `/skill run`
	// (T11). It is a distinct kind because completion must insert the whole
	// `/skill run <name>` line: inserting the bare name would produce input
	// runCommand then rejects as an unknown top-level command, i.e. the
	// palette would advertise something the parser does not accept. See
	// commands_skillrun.go for why skills do not get top-level names.
	cmdKindSkillRun
)

// command describes one slash command or palette entry. kind distinguishes
// MCP tools/groups from slash commands (default cmdSlash for commandTable
// entries). helpKey is optional: when set, every help surface renders the
// localized text from the i18n.Bundle instead of the static help string.
// Migration is incremental — entries without a helpKey still use help.
type command struct {
	name    string
	help    string
	helpKey string
	kind    commandKind
	// disabled marks an entry that is shown but cannot be used — currently
	// MCP tools whose server is not ready. Filtering them out instead would
	// make a failed server indistinguishable from one that exposes no tools,
	// which is the opposite of what the operator needs to see.
	disabled bool
	run      func(m model, args []string) (tea.Model, tea.Cmd)
}

// commandTable is the ordered list shown by /help. Order matters for help
// rendering; lookupCommand does a linear scan (the list is tiny).
// helpKey is populated for the entries the i18n catalog covers; entries
// without a helpKey still render their static help text (incremental
// migration so the table can land before every help string is ported).
//
// /help is intentionally NOT in commandTable: its handler needs the
// commandTable itself to render, and putting it in the initializer creates
// an init cycle (commandTable → cmdHelp → commandTable). runCommand handles
// /help explicitly before consulting the table.
var commandTable = []command{
	{name: "model", help: "list / switch model", helpKey: "tui.command.help.model", run: cmdModel},
	{name: "think", help: "set reasoning effort (low|medium|high|off)", helpKey: "tui.command.help.think", run: cmdThink},
	{name: "mode", help: "set permission mode (default|allow-edits|yolo|auto)", helpKey: "tui.command.help.mode", run: cmdMode},
	{name: "queue-mode", help: "set/cycle queue mode (queue|single|batch)", helpKey: "tui.command.help.queue_mode", run: cmdQueueMode},
	{name: "clear", help: "reset conversation", helpKey: "tui.command.help.clear", run: cmdClear},
	{name: "config", help: "show active config", helpKey: "tui.command.help.config", run: cmdConfig},
	{name: "cost", help: "token usage this session", helpKey: "tui.command.help.cost", run: cmdCost},
	{name: "stats", help: "token consumption histogram (recent sessions)", helpKey: "tui.command.help.stats", run: cmdStats},
	{name: "compact", help: "compact context (WS only)", helpKey: "tui.command.help.compact", run: cmdCompact},
	{name: "stash", help: "pop last stashed draft (or: stash list | stash drop N)", run: cmdStash},
	{name: "mcp", help: "list MCP servers", helpKey: "tui.command.help.mcp", run: cmdMCP},
	{name: "sessions", help: "list stored sessions", helpKey: "tui.command.help.sessions", run: cmdSessions},
	{name: "features", help: "list / enable / disable feature flags", run: cmdFeatures},
	{name: "restore", help: "restore a stored session by ID", helpKey: "tui.command.help.restore", run: cmdRestore},
	{name: "rename", help: "rename a session: /rename <id> <title>", helpKey: "tui.command.help.rename", run: cmdRename},
	{name: "archive", help: "hide a session: /archive <id>", helpKey: "tui.command.help.archive", run: cmdArchive},
	{name: "unarchive", help: "restore a session: /unarchive <id>", helpKey: "tui.command.help.unarchive", run: cmdUnarchive},
	{name: "archived", help: "list archived sessions", helpKey: "tui.command.help.archived", run: cmdArchived},
	{name: "delete", help: "delete a session: /delete <id> yes", helpKey: "tui.command.help.delete", run: cmdDelete},
	{name: "permissions", help: "list / revoke approval rules", run: cmdPermissions},
	{name: "jobs", help: "list / read / stdin / cancel shell jobs", run: cmdJobs},
	{name: "theme", help: "list / switch colour theme", helpKey: "tui.command.help.theme", run: cmdTheme},
	{name: "plan", help: "enter read-only plan mode (WS only)", run: cmdPlan},
	{name: "plan-off", help: "exit plan mode, restore previous permission mode", run: cmdPlanOff},
	{name: "restore-turn", help: "list main seams or revert to a prior turn", run: cmdRestoreTurn},
	{name: "diff", help: "show pending workspace changes (WS only)", run: cmdDiff},
	{name: "memory", help: "show active memory file path", run: cmdMemory},
	{name: "distill", help: "merge redundant memories", helpKey: "tui.command.help.distill", run: cmdDistill},
	{name: "memory-clear", help: "delete memories: /memory-clear <session|agent <id>|all> yes", run: cmdMemoryClear},
	{name: "checkpoint", help: "list / create / plan / restore a session+memory+file checkpoint", run: cmdCheckpoint},
	{name: "logs", help: "tail the structured log file (or report stderr)", run: cmdLogs},
	{name: "fork", help: "fork this session: /fork [seq] (-1=all, >=0=up to seq)", run: cmdFork},
	{name: "side", help: "start an ephemeral side conversation (V11)", run: cmdSide},
	{name: "btw", help: "alias for /side", run: cmdSide},
	{name: "main", help: "exit current side conversation (discard; keep is future polish)", run: cmdMain},
	{name: "skills", help: "list installed skills", run: cmdSkills},
	{name: "skill", help: "manage: /skill run|install|uninstall|trust|untrust|enable|disable|validate", run: cmdSkill},
	{name: "review", help: "run code review on a PR diff: /review <diff text or PR URL>", run: cmdReview},
	// C15 + I18N1 preference commands. Handlers live in commands_prefs.go so
	// this file stays under the GOV2 cap; only the rows belong here.
	{name: "keymap", help: "show / reset keymap, or print binding diagnostics", helpKey: "tui.command.help.keymap", run: cmdKeymap},
	{name: "vim", help: "toggle vim-style modal editing: /vim on|off", helpKey: "tui.command.help.vim", run: cmdVim},
	{name: "contrast", help: "toggle the high-contrast theme: /contrast on|off", helpKey: "tui.command.help.contrast", run: cmdContrast},
	{name: "locale", help: "show / switch UI language: /locale auto|en|zh-Hans", helpKey: "tui.command.help.locale", run: cmdLocale},
}

// lookupCommand finds a command by name (the first token after "/").
func lookupCommand(name string) (command, bool) {
	for _, c := range commandTable {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

// hasCommand 报告 name 是否是已注册的斜杠命令（不含前导斜杠）。
//
// 它委托给 lookupCommand 而不是自己再扫一遍 commandTable：两份线性扫描是
// 「重复逻辑必须抽成公共函数」的原样反例，而这个函数当初正是作为「写了但零
// 读者」那类缺陷的修复的一部分被加进来的。
//
// 未导出。第一版导出它，doc 注释说是为了让别的包能对「文档里宣传的命令真的
// 存在吗」做机器判据 —— 但那样的调用方一个都没有，唯一的调用者是本包自己的
// 测试。为一个不存在的消费者导出符号，就是这个包刚修完的那个形状。真出现跨
// 包判据时再导出，那时它有调用点。
func hasCommand(name string) bool {
	_, ok := lookupCommand(name)
	return ok
}

// parseCommand splits "/name arg1 arg2" into ("name", ["arg1","arg2"]). A bare
// "/" yields ("", nil) which routes to the unknown-command branch.
func parseCommand(text string) (name string, args []string) {
	text = strings.TrimPrefix(text, "/")
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return "", nil
	}
	return parts[0], parts[1:]
}

// runCommand routes a "/…"-prefixed submission to its handler, or renders an
// unknown-command error. It never sends a user_message. /help is special-cased
// before lookupCommand because cmdHelp needs commandTable itself to render,
// and putting it in the table initializer would create an init cycle.
func (m model) runCommand(text string) (tea.Model, tea.Cmd) {
	name, args := parseCommand(text)
	if name == "help" {
		return cmdHelp(m, args)
	}
	cmd, ok := lookupCommand(name)
	if !ok {
		m.entries = append(m.entries, errorEntry{text: "unknown command: /" + name + " (try /help)"})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	return cmd.run(m, args)
}

// updatePalette recomputes the command palette from the current input and also
// appends MCP tools when available.
func (m *model) updatePalette() {
	v := m.input.Value()
	if !strings.HasPrefix(v, "/") {
		// UX4: the @path completion shares this popup. The two are mutually
		// exclusive — the input either starts with "/" or it does not — so one
		// popup, one selection index and one Tab handler serve both, instead
		// of a second set that would drift out of step with this one.
		if m.updateAtPalette() {
			return
		}
		m.paletteItems = nil
		m.paletteSel = 0
		return
	}
	// T11: `/skill run <name>` completes installed skill names. It is checked
	// BEFORE the "contains a space means we are past the command name" bail
	// below, which is exactly the branch that would otherwise close the
	// palette on the space after `run`.
	if m.updateSkillRunPalette(v) {
		return
	}
	prefix := strings.TrimPrefix(v, "/")
	if strings.Contains(prefix, " ") {
		m.paletteItems = nil
		return
	}
	var items []command
	for _, c := range commandTable {
		if strings.HasPrefix(c.name, prefix) {
			items = append(items, c)
		}
	}
	items = append(items, m.matchingMCPItems(prefix)...)
	m.paletteItems = items
	if m.paletteSel >= len(items) || m.paletteSel < 0 {
		m.paletteSel = 0
	}
}

// paletteOpen reports whether the command palette is currently shown.
func (m model) paletteOpen() bool { return len(m.paletteItems) > 0 }

// paletteBlock renders the command palette popup (one row per filtered command,
// the selected row highlighted). Returns "" when the palette is closed.
func (m model) paletteBlock() string {
	if len(m.paletteItems) == 0 {
		return ""
	}
	rows := make([]string, 0, len(m.paletteItems))
	for i, c := range m.paletteItems {
		var line string
		switch c.kind {
		case cmdMCPGroup:
			line = "  " + toolMeta.Render(c.name)
		case cmdMCPTool:
			desc := c.help
			if desc == "" {
				desc = "MCP tool"
			}
			if c.disabled {
				// Dimmed AND marked. Colour alone is not the signal: it is
				// absent on a non-TTY, and the row still has to read as
				// unusable in a plain-text transcript or a screenshot.
				line = toolMeta.Render(fmt.Sprintf("    %-7s  %s  (unavailable)", c.name, desc))
			} else {
				line = fmt.Sprintf("    %-7s  %s", c.name, toolMeta.Render(desc))
			}
		case cmdAtPath:
			line = fmt.Sprintf("  @%-30s %s", c.name, toolMeta.Render(c.help))
		case cmdKindSkillRun:
			if c.disabled {
				// Marked as well as dimmed: colour is absent on a non-TTY and
				// in a pasted transcript, and an unusable row has to read as
				// unusable in both.
				line = toolMeta.Render(fmt.Sprintf("  %-16s %s  (unavailable)", c.name, c.help))
			} else {
				line = fmt.Sprintf("  %-16s %s", c.name, toolMeta.Render(c.help))
			}
		default:
			line = fmt.Sprintf("  /%-7s  %s", c.name, toolMeta.Render(localizedHelp(m.bundle, c)))
		}
		if i == m.paletteSel {
			rows = append(rows, selPaletteStyle.Render("▶ "+line))
		} else {
			rows = append(rows, paletteStyle.Render(line))
		}
	}
	return strings.Join(rows, "\n")
}

// paletteMove moves the palette selection by delta (wrapping). No-op when the
// palette is closed.
func (m *model) paletteMove(delta int) {
	if len(m.paletteItems) == 0 {
		return
	}
	n := len(m.paletteItems)
	m.paletteSel = ((m.paletteSel+delta)%n + n) % n
	// Skip group headers (cmdMCPGroup). Bounded by n: a palette that is ALL
	// headers — every configured server exposing zero tools, which is what a
	// fleet of failed servers looks like — would otherwise spin here forever
	// with the UI frozen and no error anywhere.
	for i := 0; i < n && m.paletteItems[m.paletteSel].kind == cmdMCPGroup; i++ {
		m.paletteSel = ((m.paletteSel+delta)%n + n) % n
	}
}

// paletteComplete replaces the input's command-name prefix with the selected
// command (followed by a space, ready for args) and dismisses the palette.
func (m *model) paletteComplete() {
	if len(m.paletteItems) == 0 {
		return
	}
	sel := m.paletteItems[m.paletteSel]
	switch sel.kind {
	case cmdAtPath:
		m.completeAtPath(sel)
		return
	case cmdMCPTool:
		if sel.disabled {
			// Selecting it must not insert the name: the server is not
			// connected, so the call would fail with an "unknown tool" the
			// operator has no way to connect back to the palette row.
			return
		}
		// MCP tools insert the qualified name directly (no "/" prefix).
		m.input.SetValue(sel.name)
	case cmdMCPGroup:
		return // not selectable
	case cmdKindSkillRun:
		if sel.disabled {
			// Same rule as an MCP tool on a dead server: inserting the name
			// would produce a line that fails for a reason the row already
			// stated, with nothing tying the two together.
			return
		}
		m.input.SetValue(skillRunPrefix + sel.name + " ")
	default:
		m.input.SetValue("/" + sel.name + " ")
	}
	m.input.CursorEnd()
	m.paletteItems = nil
	m.growInput()
}

// permissionPopup renders the active permission prompt as a popup ABOVE the
// input: the tool call + the static-deny reason, then the Allow / Always allow
// / Deny options with the selected one highlighted (↑↓ to move, enter to
// confirm; y/a/n shortcuts also work). When more than one request is queued
// (parallel tool calls), a "(1/N)" badge shows the position so the user knows
// resolving this reveals the next. Returns "" when no prompt is pending.
func (m model) permissionPopup() string {
	pe := m.pendingPermission()
	if pe == nil {
		return ""
	}
	args := truncate(strings.TrimSpace(pe.args), 60)
	var b strings.Builder
	b.WriteString("  " + errStyle.Render("✋ permission") + "  " + toolName.Render(toolDisplayName(pe.tool)))
	if args != "" {
		b.WriteString("  " + toolMeta.Render("("+args+")"))
	}
	// Queue depth badge: shows how many prompts are stacked behind this one.
	if n := len(m.pendingPermissions); n > 1 {
		b.WriteString("  " + warnStyle.Render(fmt.Sprintf("(%d queued)", n-1)))
	}
	// Expiry countdown (S5). The server denies an unanswered prompt when its
	// deadline passes, so the popup has to say so: without it the prompt simply
	// vanishes and the tool call fails for no visible reason. The 5s repaint
	// heartbeat (repaintTick) is what keeps this number moving while the user
	// stares at it — no extra timer is armed for the popup.
	if left := pe.secondsLeft(time.Now()); left >= 0 {
		b.WriteString("  " + warnStyle.Render(fmt.Sprintf("%ds left", left)))
	}
	b.WriteString("\n")
	if pe.reason != "" {
		b.WriteString("  " + warnStyle.Render(pe.reason) + "\n")
	}
	curOpts := permissionOptions(pe)
	opts := make([]string, 0, len(curOpts))
	for i, o := range curOpts {
		if i == m.permSel {
			opts = append(opts, selPaletteStyle.Render("▶ "+o.label))
		} else {
			opts = append(opts, paletteStyle.Render("  "+o.label))
		}
	}
	b.WriteString("  " + strings.Join(opts, " ") + "  " + toolMeta.Render("(↑↓ enter · y/a/n · shift+tab mode)") + "\n")
	return b.String()
}

// yoloPopup renders the two-Enter YOLO confirmation warning. Stage 1 (after
// Shift+Tab targets yolo) warns and asks for the first Enter; stage 2 (after
// the first Enter) asks for a second Enter to confirm. Any non-Enter key
// cancels (handled in the KeyMsg switch). Returns "" unless yoloConfirm > 0.
func (m model) yoloPopup() string {
	switch m.yoloConfirm {
	case 1:
		return "  " + warnStylePerm.Render("⚠ YOLO mode auto-approves ALL actions (incl. rm -rf / force push).") +
			"\n  " + toolMeta.Render("Enter to continue · any other key to cancel") + "\n"
	case 2:
		return "  " + warnStylePerm.Render("⚠ Confirm: enter YOLO mode? This cannot be undone for approved calls.") +
			"\n  " + toolMeta.Render("Enter AGAIN to confirm · any other key to cancel") + "\n"
	}
	return ""
}

// sendControlFrame writes a control REQUEST frame and, when the backend returns
// a reply channel, arms waitForEvent so the reply flows into applyEvent. Returns
// the updated model + cmd.
func (m model) sendControlFrame(f proto.ClientFrame) (tea.Model, tea.Cmd) {
	ch := m.sess.SendFrame(f)
	if ch == nil {
		// No reply expected / fire-and-forget (or a fake session in tests).
		m.refresh()
		return m, nil
	}
	m.streamCh = ch
	// Initialize the live activity status line so it doesn't render with a zero
	// turnStart while awaiting the reply, and arm the glyph animation tick.
	m.startTurn()
	// reflow (not just refresh): the status line is now visible, so the viewport
	// must shrink by its 1-line height to keep the JoinVertical total exact.
	m.reflow()
	return m, tea.Batch(m.waitForEvent(), activityTick())
}

// paletteMCPItems returns command entries for MCP tools grouped by server.
// matchingMCPItems returns the MCP entries matching prefix, each server's
// surviving tools preceded by that server's group header.
//
// The header is not a match candidate, and treating it as one is what broke
// this: the previous code ran every entry -- headers included -- through
// HasPrefix(name, prefix), and a header spelled "── files ──" can never start
// with what the user typed. Every header was dropped and the tools arrived as
// one flat list, which is exactly the ambiguity the headers exist to remove
// (MCP tool names are long, and two servers can expose similar ones).
//
// A group with no surviving tools contributes nothing: an empty header is
// worse than no header, because it claims a server matched when none of its
// tools did.
func (m *model) matchingMCPItems(prefix string) []command {
	var out []command
	for _, srv := range m.paletteMCPServers {
		down := srv.Status != "ready"
		var tools []command
		for _, tool := range srv.Tools {
			if strings.HasPrefix(tool.Name, prefix) {
				tools = append(tools, command{name: tool.Name, help: tool.Description,
					kind: cmdMCPTool, disabled: down})
			}
		}
		if len(tools) == 0 {
			continue
		}
		out = append(out, command{name: mcpGroupLabel(srv), kind: cmdMCPGroup})
		out = append(out, tools...)
	}
	return out
}

// mcpGroupLabel renders one server's palette header, appending the status when
// it is anything other than ready so a disabled or failed server is visible
// rather than silently absent.
func mcpGroupLabel(srv proto.MCPServerStatus) string {
	label := "── " + srv.Name + " ──"
	if srv.Status != "ready" {
		label += " [" + srv.Status + "]"
	}
	return label
}

func (m *model) paletteMCPItems() []command {
	var items []command
	for _, srv := range m.paletteMCPServers {
		down := srv.Status != "ready"
		items = append(items, command{name: mcpGroupLabel(srv), kind: cmdMCPGroup})
		for _, tool := range srv.Tools {
			items = append(items, command{name: tool.Name, help: tool.Description,
				kind: cmdMCPTool, disabled: down})
		}
	}
	return items
}

// --- handlers ---------------------------------------------------------------

// cmdHelp renders the command list locally — no server round-trip. The
// localized title + per-command help come from the model's i18n.Bundle,
// pre-rendered once at construction so render() is a cheap string return.
//
// /help is NOT registered in commandTable (it would create an init cycle
// because cmdHelp needs the table to render). runCommand handles /help
// explicitly before lookupCommand; the help text still appears in /help
// output via the synthetic first entry added in newCmdHelpEntry.
func cmdHelp(m model, _ []string) (tea.Model, tea.Cmd) {
	table := append([]command{{
		name: "help", help: "list commands", helpKey: "tui.command.help.help",
	}}, commandTable...)
	m.entries = append(m.entries, newCmdHelpEntry(m.bundle, table))
	m.refresh()
	m.viewport.GotoBottom()
	return m, nil
}

func cmdFeatures(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		return m.sendControlFrame(proto.NewFeaturesList())
	}
	if len(args) != 2 || (args[0] != "enable" && args[0] != "disable") {
		m.entries = append(m.entries, errorEntry{text: "usage: /features [enable|disable <key>]"})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	return m.sendControlFrame(proto.NewFeaturesSet(args[1], args[0] == "enable"))
}

// cmdModel: no arg → interactive picker; one arg → switch to that model.
func cmdModel(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) > 0 && args[0] == "plan" {
		return cmdPlan(m, args[1:])
	}
	if m.permMode == guard.ModePlan && len(args) > 0 && args[0] != "plan" {
		m.prePlanMode = ""
	}
	if len(args) == 0 {
		m.pickerKind = "model"
		return m.sendControlFrame(proto.NewListModels())
	}
	return m.sendControlFrame(proto.NewSetModel(args[0]))
}

// cmdThink: set reasoning effort (low|medium|high|off).
func cmdThink(m model, args []string) (tea.Model, tea.Cmd) {
	effort := ""
	if len(args) > 0 {
		effort = args[0]
	}
	return m.sendControlFrame(proto.NewSetThinking(effort))
}

// cmdMode: set the permission mode. Forms:
//
//	/mode                    interactive picker (lists guard.Modes())
//	/mode strict             confirm EVERY call, including allowed ones (W-B-20)
//	/mode default            ask for every denied call
//	/mode allow-edits        auto-approve fs_write/fs_edit
//	/mode yolo               auto-approve EVERYTHING (no confirmation gate here;
//	                        /mode is an explicit typed command, unlike Shift+Tab)
//	/mode auto               the model risk-assesses each denied call
//	/mode plan               read-only
//
// An unrecognized mode renders a local error and sends nothing. The error text
// enumerates guard.Modes() rather than a hand-written list: the hand-written
// one had already dropped `plan`, and a help string that names fewer modes than
// exist is how a mode ends up unreachable for everyone who did not read the
// source.
func cmdPlan(m model, _ []string) (tea.Model, tea.Cmd) {
	if m.sess.Mode() == "sse" {
		m.entries = append(m.entries, errorEntry{text: "plan mode requires the WebSocket transport; SSE is stateless"})
		m.refresh()
		return m, nil
	}
	if m.permMode != guard.ModePlan {
		m.prePlanMode = m.permMode
		if m.prePlanMode == "" {
			m.prePlanMode = guard.ModeDefault
		}

	}
	m.permMode = guard.ModePlan
	return m.sendMode()
}

func cmdPlanOff(m model, _ []string) (tea.Model, tea.Cmd) {
	if m.sess.Mode() == "sse" {
		m.entries = append(m.entries, errorEntry{text: "plan mode requires the WebSocket transport; SSE is stateless"})
		m.refresh()
		return m, nil
	}
	mode := m.prePlanMode
	if mode == "" || mode == guard.ModePlan {
		mode = guard.ModeDefault
	}
	m.permMode = mode
	m.prePlanMode = ""
	return m.sendMode()
}

// cmdReview implements the /review slash command. It forwards the supplied
// diff/URL through dispatchSend so the message routes through the normal agent
// pipeline (permissions, streaming, transcript); the agent then decides to
// invoke the `review` tool.
func cmdReview(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) < 1 {
		m.entries = append(m.entries, errorEntry{text: "usage: /review <diff text or PR URL>"})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	input := strings.Join(args, " ")
	return m.dispatchSend("/review "+input, false)
}

func cmdMode(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		cur := m.permMode
		if cur == "" {
			cur = guard.ModeDefault
		}
		m.pickerKind = "mode"
		m.pickerItems = nil
		for _, mode := range guard.Modes() {
			m.pickerItems = append(m.pickerItems, pickerItem{
				name:    string(mode),
				current: mode == cur,
			})
		}
		m.pickerCursor = 0
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	pm, ok := guard.NormalizeMode(args[0])
	if !ok {
		names := make([]string, 0, len(guard.Modes()))
		for _, mode := range guard.Modes() {
			names = append(names, string(mode))
		}
		m.entries = append(m.entries, errorEntry{
			text: "unknown mode: " + args[0] + " (" + strings.Join(names, "|") + ")",
		})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	m.permMode = pm
	return m.sendMode()
}

// cmdClear: clear server history+counters AND local entries.
func cmdClear(m model, _ []string) (tea.Model, tea.Cmd) {
	mm, cmd := m.sendControlFrame(proto.NewClear())
	m = mm.(model)
	m.entries = nil
	m.pending = ""
	m.toolsRun = 0
	m.pendingStatus = nil
	m.pendingPermissions = nil
	m.yoloConfirm = 0
	m.refresh()
	m.viewport.GotoBottom()
	return m, cmd
}

// cmdConfig: request status and render it as a block (model + thinking + usage).
func cmdConfig(m model, _ []string) (tea.Model, tea.Cmd) {
	return m.requestStatusBlock()
}

// requestStatusBlock appends a statusEntry (filled when the status reply
// arrives) and sends get_status.
func (m model) requestStatusBlock() (tea.Model, tea.Cmd) {
	se := &statusEntry{}
	m.entries = append(m.entries, se)
	m.pendingStatus = se
	m.refresh()
	m.viewport.GotoBottom()
	return m.sendControlFrame(proto.NewGetStatus())
}

// cmdCost: force a get_status and render the usage fields as a block.
func cmdCost(m model, _ []string) (tea.Model, tea.Cmd) {
	return m.requestStatusBlock()
}

// cmdStats: request the session list and render a per-session token-consumption
// histogram. Reuses the sessions fetch (proto.NewSessionList) — SessionInfo
// already carries TokensIn/TokensOut — so /stats needs no new backend round-trip
// or store field. The reply fills the pending statsEntry (see applyEvent's
// "sessions" case).
func cmdStats(m model, _ []string) (tea.Model, tea.Cmd) {
	se := &statsEntry{}
	m.entries = append(m.entries, se)
	m.pendingStatsEntry = se
	m.refresh()
	m.viewport.GotoBottom()
	return m.sendControlFrame(proto.NewSessionList())
}

// cmdQueueMode: set or cycle the message queue mode. Forms:
//
//	/queue-mode              cycle to the next mode (queue → single → batch → queue)
//	/queue-mode queue        queue follow-ups FIFO; drain one per turn in order
//	/queue-mode single       a follow-up CANCELS the running turn and supersedes the queue
//	/queue-mode batch        merge all queued follow-ups into one turn after the current ends
//
// The mode governs what submit() does when a message arrives mid-turn (see
// enqueue) and how the queue unwinds when the turn ends (see drainQueue, hooked
// in Update's streamMsg case). With no arg the command CYCLES (acceptance:
// "可循环切换") so the user can flip modes without typing; every invocation
// renders the mode list so the change is visible. An unknown arg renders a local
// error and sends nothing.
func cmdQueueMode(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) > 0 {
		qm, ok := parseQueueMode(args[0])
		if !ok {
			m.entries = append(m.entries, errorEntry{
				text: "unknown queue mode: " + args[0] + " (queue|single|batch)",
			})
			m.refresh()
			m.viewport.GotoBottom()
			return m, nil
		}
		m.queueMode = qm
	} else {
		// No-arg form: cycle queue → single → batch → queue.
		m.queueMode = nextQueueMode(m.queueMode)
	}
	m.entries = append(m.entries, errorEntry{text: queueModeHelp(m.queueMode)})
	m.refresh()
	m.viewport.GotoBottom()
	return m, nil
}

// nextQueueMode returns the mode after qm in the cycle queue → single → batch →
// queue. Used by the no-arg /queue-mode form.
func nextQueueMode(qm QueueMode) QueueMode {
	switch qm {
	case QueueModeQueue:
		return QueueModeSingle
	case QueueModeSingle:
		return QueueModeBatch
	default:
		return QueueModeQueue
	}
}

// queueModeHelp renders the three-line mode list with the active one marked, so
// every /queue-mode invocation (cycle or explicit) shows what changed. The
// descriptions match the dispatch behavior in enqueue / drainQueue.
func queueModeHelp(active QueueMode) string {
	modes := []QueueMode{QueueModeQueue, QueueModeSingle, QueueModeBatch}
	rows := make([]string, 0, len(modes))
	for _, qm := range modes {
		marker := "  "
		if qm == active {
			marker = "▶ "
		}
		rows = append(rows, "  "+marker+qm.String()+" — "+queueModeDesc(qm))
	}
	return "queue mode: " + active.String() + "\n" + strings.Join(rows, "\n")
}

// queueModeDesc is the one-line, user-facing description of a mode's effect,
// matching the dispatch in enqueue (mid-turn) and drainQueue (at turn end).
func queueModeDesc(qm QueueMode) string {
	switch qm {
	case QueueModeSingle:
		return "a new message cancels the running turn"
	case QueueModeBatch:
		return "merge all queued messages into one turn"
	default:
		return "queue follow-ups; run one per turn in order"
	}
}

// cmdCompact: request server-side compaction. The status (compacting /
// compacted / progress deltas) is shown ONLY on the activity line, never as a
// transcript entry (bug⑧: compaction is a meta-op, so its status must not
// pollute the conversation flow).
func cmdCompact(m model, _ []string) (tea.Model, tea.Cmd) {
	// bug⑧: compaction status lives on the activity line, not the transcript.
	m.activity = "Compacting context…"
	m.refresh()
	return m.sendControlFrame(proto.NewCompact())
}

// cmdMCP: manage MCP servers. Forms:
//
//	/mcp              list all servers (mcp_action list)
//	/mcp list         list all servers
//	/mcp validate     validate all server connections
//	/mcp enable <s>   enable/reconnect a server
//	/mcp disable <s>  disable/disconnect a server
//	/mcp reload <s>   tear down and restart a server
func cmdMCP(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		return m.sendControlFrame(proto.NewMCPAction("", "list"))
	}
	action := args[0]
	switch action {
	case "list", "validate":
		return m.sendControlFrame(proto.NewMCPAction("", action))
	case "enable", "disable", "reload":
		if len(args) < 2 {
			m.entries = append(m.entries, errorEntry{text: "usage: /mcp " + action + " <server>"})
			m.refresh()
			m.viewport.GotoBottom()
			return m, nil
		}
		return m.sendControlFrame(proto.NewMCPAction(args[1], action))
	default:
		m.entries = append(m.entries, errorEntry{text: "unknown /mcp subcommand: " + action + " (list|validate|enable|disable|reload)"})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
}

// cmdSessions: list stored sessions.
func cmdSessions(m model, _ []string) (tea.Model, tea.Cmd) {
	m.entries = append(m.entries, &sessionsEntry{})
	m.refresh()
	m.viewport.GotoBottom()
	return m.sendControlFrame(proto.NewSessionList())
}

// cmdRestore: restore a stored session by ID, or show an interactive picker when no ID is given.
func cmdRestore(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		m.restoreMode = true
		return m.sendControlFrame(proto.NewSessionList())
	}
	m.entries = append(m.entries, &restoreEntry{})
	m.refresh()
	m.viewport.GotoBottom()
	return m.sendControlFrame(proto.NewRestoreSession(args[0]))
}

// cmdRename: /rename <id> <title...>. args[0] is the session id; the rest (joined
// by space) is the new title. Missing id or title renders a local error and sends
// nothing. Reply (session_ack) renders via applyEvent.
func cmdRename(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) < 2 {
		m.entries = append(m.entries, errorEntry{text: "usage: /rename <id> <title>"})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	id := args[0]
	title := strings.Join(args[1:], " ")
	return m.sendControlFrame(proto.NewRenameSession(id, title))
}

// cmdArchive: /archive <id>. Hides the session from the active list.
func cmdArchive(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) < 1 {
		m.entries = append(m.entries, errorEntry{text: "usage: /archive <id>"})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	return m.sendControlFrame(proto.NewArchiveSession(args[0]))
}

// cmdUnarchive: /unarchive <id>. Restores an archived session. Find ids via
// /archived.
func cmdUnarchive(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) < 1 {
		m.entries = append(m.entries, errorEntry{text: "usage: /unarchive <id>"})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	return m.sendControlFrame(proto.NewUnarchiveSession(args[0]))
}

// cmdArchived: /archived. Lists archived sessions (reply: sessions frame, reused
// render path). Mirrors cmdSessions.
func cmdArchived(m model, _ []string) (tea.Model, tea.Cmd) {
	m.entries = append(m.entries, &sessionsEntry{})
	m.refresh()
	m.viewport.GotoBottom()
	return m.sendControlFrame(proto.NewSessionListArchived())
}

// cmdDelete: /delete <id> [yes]. Deletion is irreversible, so the client requires
// an explicit "yes" token as the second arg before sending the frame — this is a
// stateless, protocol-free confirmation (no new frame, no model state). Without
// "yes" a confirmation prompt is rendered and nothing is sent.
func cmdDelete(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) < 1 {
		m.entries = append(m.entries, errorEntry{text: "usage: /delete <id> yes"})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	id := args[0]
	if len(args) < 2 || args[1] != "yes" {
		m.entries = append(m.entries, errorEntry{
			text: "⚠ delete is irreversible. To confirm, run: /delete " + id + " yes",
		})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	return m.sendControlFrame(proto.NewDeleteSession(id))
}

// cmdRestoreTurn implements /restore-turn (B2-RB1):
//
//	/restore-turn              — list recent pre-turn seams
//	/restore-turn <id>         — show a confirmation prompt
//	/restore-turn <id> yes     — send restore_turn (target-bound by lastKnownHead)
//
// Numeric N (by-offset) is NOT supported (必修项 J: selector is exact seam_id).
// SSE is rejected because it has no interactive callback / stateful history.
func cmdRestoreTurn(m model, args []string) (tea.Model, tea.Cmd) {
	if m.sess.Mode() == "sse" {
		m.entries = append(m.entries, errorEntry{
			text: "/restore-turn requires the WebSocket transport (SSE is stateless)",
		})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	switch len(args) {
	case 0:
		return m.sendControlFrame(proto.NewListSeams())
	case 1:
		m.entries = append(m.entries, seamRestorePromptEntry{seamID: args[0]})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	default:
		if len(args) != 2 || args[1] != "yes" {
			m.entries = append(m.entries, errorEntry{
				text: "usage: /restore-turn [<id> [yes]]",
			})
			m.refresh()
			m.viewport.GotoBottom()
			return m, nil
		}
		if m.lastKnownHead == "" {
			m.entries = append(m.entries, errorEntry{
				text: "restore-turn has no full head binding; run /restore-turn first",
			})
			m.refresh()
			m.viewport.GotoBottom()
			return m, nil
		}
		m.pendingSeamRestore = &pendingSeamRestoreState{seamID: args[0]}
		return m.sendControlFrame(proto.NewRestoreTurn(args[0], m.lastKnownHead))
	}
}

// cmdDiff implements /diff (W-E-13): shows the pending (uncommitted) main-scope
// workspace changeset without switching windows — the reply is rendered by
// workspaceDiffEntry, which reuses W-E-02's renderColoredDiff. Takes no
// arguments. SSE is rejected for the same reason cmdRestoreTurn rejects it:
// list_workspace_diff is a control frame with no SSE-side handling (SSE has
// no persistent server-side history/session state to query).
func cmdDiff(m model, args []string) (tea.Model, tea.Cmd) {
	if m.sess.Mode() == "sse" {
		m.entries = append(m.entries, errorEntry{
			text: "/diff requires the WebSocket transport (SSE is stateless)",
		})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	if len(args) != 0 {
		m.entries = append(m.entries, errorEntry{text: "usage: /diff"})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	return m.sendControlFrame(proto.NewListWorkspaceDiff())
}

// cmdPermissions: /permissions                  → list all approval rules
//
//	/permissions revoke <rule-id> → revoke one rule
//
// (Task 9). The list reply arrives as a "permissions" StreamEvent that
// applyEvent routes to permissionsEntry; the revoke reply arrives as a
// permission_rule_hit with Kind="revoke" (rendered as a one-line summary).
func cmdPermissions(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		return m.sendControlFrame(proto.NewListPermissions())
	}
	if len(args) == 2 && args[0] == "revoke" {
		return m.sendControlFrame(proto.NewRevokePermission(args[1]))
	}
	m.entries = append(m.entries, errorEntry{text: "usage: /permissions [revoke <rule-id>]"})
	m.refresh()
	m.viewport.GotoBottom()
	return m, nil
}

// cmdTheme lists available themes or switches to a named theme.
func cmdTheme(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		m.pickerKind = "theme"
		m.pickerItems = nil
		for _, t := range themeList {
			m.pickerItems = append(m.pickerItems, pickerItem{
				name:        string(t.Name),
				description: t.Description,
				current:     t.Name == m.theme,
			})
		}
		m.pickerCursor = 0
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	tn := ThemeName(args[0])
	if _, ok := themeByName(tn); !ok {
		m.entries = append(m.entries, errorEntry{
			text: "unknown theme: " + args[0] + " (themes: " + themeNames() + ")",
		})
		m.refresh()
		return m, nil
	}
	// Session-only, and recorded as such: remerge() rebuilds theme from the
	// persisted cascade, so without themeOverride the next /vim or /contrast
	// would quietly put the old colours back.
	m.theme = tn
	m.themeOverride = tn
	m.entries = append(m.entries, errorEntry{text: okStyle.Render("✓ theme: " + string(tn))})
	m.refresh()
	m.viewport.GotoBottom()
	return m, nil
}

// --- entry types ------------------------------------------------------------

// helpEntry renders the slash-command table. Purely local (no server data).
// Kept as a thin alias for backward-compatibility with cmdHelp; new code
// should use newCmdHelpEntry so the rows are pre-rendered through the
// process-wide i18n.Bundle.
type helpEntry = cmdHelpEntry

// cmdHelpEntry replaces the bare helpEntry struct. It pre-renders the
// slash-command table at construction time so render() is a plain string
// return — this keeps the render(width, spinner) signature shared by ~20
// entry types unchanged. Pre-rendering localizes the i18n change to one
// entry type; the table is static per startup (locale change requires
// restart, matching existing /theme behavior).
//
// A zero-value cmdHelpEntry (rows == "") falls back to the English-default
// live render so existing tests that construct helpEntry{} directly still
// see the command table without having to thread a bundle through.
type cmdHelpEntry struct {
	rows string
}

// newCmdHelpEntry renders the localized slash-command table. The title and
// each row's help text come from the bundle so /help follows the active
// locale; commands without a helpKey fall back to their static help text
// (the table still has many entries that haven't been ported to catalog
// keys, and that migration is intentionally incremental).
func newCmdHelpEntry(b *i18n.Bundle, table []command) cmdHelpEntry {
	if b == nil {
		b = defaultBundle()
	}
	var sb strings.Builder
	sb.WriteString(roleAsst.Render("▌ "+b.Get("tui.command.help.title")) + "\n")
	var rows []string
	for _, c := range table {
		help := c.help
		if c.helpKey != "" {
			help = b.Get(c.helpKey)
		}
		rows = append(rows, fmt.Sprintf("  /%-7s  %s", c.name, toolMeta.Render(help)))
	}
	sb.WriteString(strings.Join(rows, "\n") + "\n\n")
	// Keyboard shortcuts section (C3 UX: makes shortcuts discoverable without
	// reading docs — especially helpful for new users).
	sb.WriteString(roleAsst.Render("▌ Keyboard shortcuts") + "\n")
	shortcuts := []struct{ key, action string }{
		{"Enter", "send message"},
		{"Ctrl+Enter", "newline"},
		{"Ctrl+C", "cancel running turn / copy"},
		{"Ctrl+O", "toggle expand tool block"},
		{"Ctrl+E", "toggle history view"},
		{"Ctrl+K", "clear input"},
		{"Ctrl+S", "toggle spinner/sound"},
		{"Up/Dn", "scroll transcript"},
		{"Tab", "autocomplete /command"},
		{"Esc", "interrupt / close palette"},
	}
	for _, sc := range shortcuts {
		sb.WriteString(fmt.Sprintf("  %-13s  %s\n", sc.key, toolMeta.Render(sc.action)))
	}
	sb.WriteString("\n")
	return cmdHelpEntry{rows: sb.String()}
}

func (e cmdHelpEntry) render(_ int, _ spinner.Model) string {
	if e.rows != "" {
		return e.rows
	}
	// Zero-value fallback: render the English table without a bundle. This
	// path is only reached by tests that construct helpEntry{} directly; the
	// production cmdHelp path always supplies a pre-rendered rows string.
	return newCmdHelpEntry(defaultBundle(), lookupCommandTableForFallback()).rows
}

// lookupCommandTableForFallback is split from lookupCommandTable so the
// zero-value render path doesn't introduce a new init-cycle surface — it
// is only called at render time, after package init has completed.
func lookupCommandTableForFallback() []command {
	return append([]command{{
		name: "help", help: "list commands", helpKey: "tui.command.help.help",
	}}, commandTable...)
}

// modelsEntry renders the available-models list returned by list_models.
type modelsEntry struct{ names []string }

func (e modelsEntry) render(_ int, _ spinner.Model) string {
	var b strings.Builder
	b.WriteString("  " + toolName.Render("available models") + "\n")
	if len(e.names) == 0 {
		b.WriteString("    " + warnStyle.Render("(none configured)") + "\n\n")
		return b.String()
	}
	for _, n := range e.names {
		b.WriteString("    " + okStyle.Render("•") + " " + n + "\n")
	}
	b.WriteString("    " + toolMeta.Render("use /model <name> to switch") + "\n\n")
	return b.String()
}

// statusEntry renders the session status (model + thinking + usage). It is a
// pointer (*statusEntry) so the reply can fill it in place: /config appends it
// before sending get_status, and applyEvent populates the fields when the status
// frame arrives.
type statusEntry struct {
	model, thinking               string
	tokensIn, tokensOut, turns    int
	cachedTokens, reasoningTokens int
	costUSD                       float64
	costKnown                     bool
}

func (e *statusEntry) render(_ int, _ spinner.Model) string {
	var b strings.Builder
	b.WriteString("  " + toolName.Render("status") + "\n")
	modelVal := e.model
	if modelVal == "" {
		modelVal = toolMeta.Render("(default)")
	}
	thinkingVal := e.thinking
	if thinkingVal == "" {
		thinkingVal = toolMeta.Render("off")
	}
	b.WriteString(fmt.Sprintf("    model: %s\n", modelVal))
	b.WriteString(fmt.Sprintf("    thinking: %s\n", thinkingVal))
	// Token breakdown (Task B8 / T8b): the raw in/out totals hide two real $
	// drivers — prompt-cache hits (tokens spared from re-billing by the
	// provider's cache) and reasoning tokens (tokens the reasoning model
	// produced internally while thinking). Surface them as parenthetical /
	// trailing annotations so /cost reads as a single line:
	//   "tokens: 12000 in (cache 8000) · 1500 out · think 1.2k"
	// cache and think only appear when the server reported a non-zero count
	// (older turns without the A6 wiring stay in the legacy in/out shape).
	b.WriteString(fmt.Sprintf("    tokens: %d in", e.tokensIn))
	if e.cachedTokens > 0 {
		b.WriteString(fmt.Sprintf(" (cache %d)", e.cachedTokens))
	}
	b.WriteString(fmt.Sprintf(" · %d out", e.tokensOut))
	if e.reasoningTokens > 0 {
		b.WriteString(fmt.Sprintf(" · think %d", e.reasoningTokens))
	}
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("    turns: %d\n", e.turns))
	// One formatter for every cost surface. This was $%.6f while /stats used
	// nothing at all and einollm.FormatCost -- the banded formatter written
	// for exactly this -- had zero production callers. Two surfaces showing
	// the same number in two precisions is how a user concludes one of them
	// is wrong.
	b.WriteString("    estimated cost: " + einollm.FormatCost(e.costUSD, e.costKnown) + "\n")
	b.WriteString("\n")
	return b.String()
}

// mcpStatusEntry renders the MCP server status snapshot returned by mcp_status.
type mcpStatusEntry struct {
	servers []proto.MCPServerStatus
}

func (e mcpStatusEntry) render(_ int, _ spinner.Model) string {
	var b strings.Builder
	b.WriteString("  " + toolName.Render("MCP servers") + "\n")
	if len(e.servers) == 0 {
		b.WriteString("    " + warnStyle.Render("(none configured)") + "\n\n")
		return b.String()
	}
	for _, s := range e.servers {
		marker := " ○"
		switch s.Status {
		case "ready":
			marker = " ●"
		case "failed":
			marker = " ✗"
		case "starting":
			marker = " ◌"
		}
		line := fmt.Sprintf("    %s %s (%s) %d tools", marker, s.Name, s.Transport, s.ToolCount)
		if s.Error != "" {
			line += " " + warnStyle.Render(s.Error)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
	return b.String()
}

type featuresEntry struct {
	rows []proto.FeatureRow
}

func (e featuresEntry) render(_ int, _ spinner.Model) string {
	var b strings.Builder
	b.WriteString("  " + toolName.Render("features") + "\n")
	if len(e.rows) == 0 {
		b.WriteString("    " + warnStyle.Render("(none registered)") + "\n\n")
		return b.String()
	}
	for _, row := range e.rows {
		state := warnStyle.Render("disabled")
		if row.Enabled {
			state = okStyle.Render("enabled")
		}
		b.WriteString(fmt.Sprintf("    %-32s %-12s %-10s %s\n", row.Key, row.Stage, state, toolMeta.Render(row.Owner)))
	}
	b.WriteString("\n")
	return b.String()
}

// sessionsEntry renders the stored sessions list. Filled by the server reply.
type sessionsEntry struct {
	sessions []proto.SessionInfo
	// note is the server's word about rows it did not send. The list is one
	// page, and a prefix rendered as the whole list is how a bounded reply gets
	// reported as a lost session.
	note string
}

func (e *sessionsEntry) render(_ int, _ spinner.Model) string {
	var b strings.Builder
	b.WriteString("  " + toolName.Render("stored sessions") + "\n")
	if len(e.sessions) == 0 {
		b.WriteString("    " + warnStyle.Render("(none)") + "\n\n")
		return b.String()
	}
	if e.note != "" {
		b.WriteString("    " + warnStyle.Render(e.note) + "\n")
	}
	for _, s := range e.sessions {
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		created := time.Unix(s.CreatedAt, 0).Format("Jan 02 15:04")
		b.WriteString(fmt.Sprintf("    %s  %s\n", okStyle.Render("•"), toolMeta.Render(s.ID)))
		b.WriteString(fmt.Sprintf("      %s  %s  %d msgs\n", title, created, s.MsgCount))
	}
	b.WriteString("\n")
	return b.String()
}

// restoreEntry renders the restore-session result. Filled by the server reply.
type restoreEntry struct {
	sessionID string
	count     int
	err       string
}

func (e *restoreEntry) render(_ int, _ spinner.Model) string {
	var b strings.Builder
	if e.err != "" {
		b.WriteString("  " + errStyle.Render("✗ restore: "+e.err) + "\n\n")
		return b.String()
	}
	b.WriteString("  " + okStyle.Render("✓ restored session") + " " + toolMeta.Render(e.sessionID) + "\n")
	b.WriteString(fmt.Sprintf("    %s %d messages loaded\n", okStyle.Render("•"), e.count))
	b.WriteString("    " + warnStyle.Render("use /clear to reset and start fresh") + "\n\n")
	return b.String()
}

// ackEntry renders a one-line session-mutation acknowledgement (session_ack).
// Rendered green for renamed/archived/unarchive, red for deleted.
type ackEntry struct{ text string }

func (e ackEntry) render(_ int, _ spinner.Model) string {
	return "  " + e.text + "\n\n"
}

// formatSessionAck renders the human line for a session_ack event.
func formatSessionAck(action, id, title string) string {
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	switch action {
	case "renamed":
		return okStyle.Render("✓ session " + short + " renamed to " + strconv.Quote(title))
	case "archived":
		return okStyle.Render("✓ session " + short + " archived (use /unarchive to restore)")
	case "unarchive":
		return okStyle.Render("✓ session " + short + " restored to active list")
	case "deleted":
		return warnStyle.Render("✗ session " + short + " deleted")
	default:
		return toolMeta.Render("session " + short + " " + action)
	}
}

// compactEntry was a transcript block that rendered "↻ compacting…" +
// summary text. Removed in bug⑧: compaction is a META-OP whose status
// (compacting / compacted / progress deltas) lives on the activity line,
// not the transcript. Summary content is model context, not conversational.

// permissionEntry is the data behind an interactive approval prompt
// (permission_request). It is held in the m.pendingPermissions queue (the front
// is the visible popup) and rendered as a POPUP above the input (see
// permissionPopup), not as a transcript entry.
//
// mandatory means "only an explicit per-call decision counts": it is set from
// EITHER approval_required (mandatory-approval tools) OR force_prompt
// (force-prompt tools and RequireApproval's destructive actions). It suppresses
// both auto-resolution on a mode switch and the sticky-allow popup options.
type permissionEntry struct {
	id, tool, args, reason string
	mandatory              bool
	// expiresAt is when the SERVER will give up on this prompt and deny it
	// (S5). Zero means the server advertised no deadline — an older backend,
	// or a policy with none — and the popup then shows no countdown rather
	// than inventing one.
	//
	// It is stored as a local instant computed from the timeout the frame
	// carried, not from the server's absolute clock value: the two are the same
	// host in the normal case, but a countdown driven by someone else's clock
	// jumps when they disagree, and a wrong number here is worse than none.
	expiresAt time.Time
}

// permissionDeadline converts the wire countdown into a local expiry instant.
// now is the receipt time. A non-positive timeout yields the zero time, which
// secondsLeft reports as "no countdown".
func permissionDeadline(now time.Time, timeoutSecs int) time.Time {
	if timeoutSecs <= 0 {
		return time.Time{}
	}
	return now.Add(time.Duration(timeoutSecs) * time.Second)
}

// secondsLeft returns whole seconds until the prompt expires, or -1 when no
// deadline was advertised. It clamps at 0 rather than going negative: an
// expired prompt is one the server has already denied, and a popup counting
// down past zero would suggest the user still has time.
func (pe *permissionEntry) secondsLeft(now time.Time) int {
	if pe == nil || pe.expiresAt.IsZero() {
		return -1
	}
	d := pe.expiresAt.Sub(now)
	if d <= 0 {
		return 0
	}
	return int(d.Round(time.Second) / time.Second)
}

// thinkingEntry is a Claude.ai-style reasoning block. While the model streams
// reasoning (schema.Message.ReasoningContent) it renders live as
// "✻ Thinking… (Xs)" with the dimmed reasoning underneath; once the first
// non-thinking event arrives (agent_chunk/tool_call/tool_result/error/done) it
// finalizes — live=false — and collapses to one line "✻ Thought for Xs
// (ctrl+o to expand)". ctrl+o (toggleExpand) flips expanded on the most
// recent finalized block to reveal the full reasoning.
//
// It is a pointer (*thinkingEntry) so applyEvent can append deltas and finalize
// it in place. text is a PLAIN STRING, not a strings.Builder: the model value is
// copied through bubbletea's value-receiver Update/applyEvent, and a copied
// non-zero Builder panics on the next WriteString — the same class of bug that
// hit the pending field (commit 788e579). Concatenation is cheap for
// chat-sized reasoning text.
type thinkingEntry struct {
	text      string
	live      bool
	expanded  bool
	startedAt time.Time
	endedAt   time.Time
}

// render draws the block in one of three states: live (streaming, shows elapsed
// time + a tail of the most recent reasoning), collapsed (one "Thought for Xs"
// line with the ctrl+o hint), or expanded (header + the full markdown-rendered
// reasoning).
//
// Coloring and live tail cap (Task B6 / T4+T5):
//   - live uses thinkingLiveStyle (light blue italic) for BOTH header and body
//     so the whole block reads as "in progress" at a glance, distinct from the
//     dim grey thinkingDoneStyle used once it settles.
//   - live body is capped at the last 10 raw lines (tailLines) and rendered as
//     PLAIN TEXT, not markdown. Streaming chunks arrive many times per second
//     and glamour-rendering on each tick is the dominant CPU cost (see
//     pendingStyle for the same trade-off in the assistant block); capping also
//     keeps long reasoning from pushing prior transcript context off screen,
//     matching the shell_run tail window (entries.go renderTail). Markdown
//     rendering is deferred to the finalized expanded view, where the text is
//     immutable and cached.
//   - collapsed/expanded use thinkingDoneStyle for the header (and the markdown
//     body in expanded is rendered normally — the dim grey already differentiates
//     from the assistant answer).
func (e *thinkingEntry) render(width int, _ spinner.Model) string {
	var b strings.Builder
	if e.live {
		dur := formatDuration(time.Since(e.startedAt))
		header := fmt.Sprintf("✻ Thinking… (%s · %d行)", dur, lineCount(e.text))
		b.WriteString("  " + thinkingLiveStyle.Render(header) + "\n")
		if e.text != "" {
			// Tail-only plain text: see the doc comment above for the rationale
			// (CPU + screen real estate). PaddingLeft(4) matches the indentation
			// of the markdown-rendered expanded body so the live→finalized
			// transition does not shift the left margin.
			tail := tailLines(e.text, 10)
			if tail != "" {
				b.WriteString(lipgloss.NewStyle().PaddingLeft(4).Render(thinkingLiveStyle.Render(tail)))
				b.WriteString("\n")
			}
		}
		b.WriteString("\n")
		return b.String()
	}
	dur := formatDuration(e.endedAt.Sub(e.startedAt))
	if e.expanded {
		b.WriteString("  " + thinkingDoneStyle.Render("✻ Thought for "+dur) + "\n")
		if e.text != "" {
			b.WriteString(lipgloss.NewStyle().PaddingLeft(4).Render(renderMarkdown(width, e.text)))
		}
		b.WriteString("\n\n")
		return b.String()
	}
	// Collapsed (default once thinking ends): one line + the expand hint.
	b.WriteString("  " + thinkingDoneStyle.Render("✻ Thought for "+dur+" (ctrl+o to expand)") + "\n\n")
	return b.String()
}

// localizedHelp resolves a command's help text through the catalog, falling
// back to the static English when the entry has no key or the key is missing.
//
// Both help surfaces go through it. Before this, the doc comment on `command`
// claimed /help and the palette rendered the localized string and NEITHER did:
// helpKey had exactly one consumer (the /help transcript entry), so switching
// to zh-Hans produced a half-translated UI — the transcript in Chinese, the F1
// panel and the command palette still in English.
func localizedHelp(b *i18n.Bundle, c command) string {
	if b == nil || c.helpKey == "" {
		return c.help
	}
	if s := b.Get(c.helpKey); s != "" && s != c.helpKey {
		return s
	}
	return c.help
}
