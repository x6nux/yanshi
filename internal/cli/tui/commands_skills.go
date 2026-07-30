package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/x6nux/yanshi/internal/proto"
)

// cmdSkills lists all installed skills (visible to skill_use). It sends
// list_skills; the reply (skills_list) renders via applyEvent.
func cmdSkills(m model, _ []string) (tea.Model, tea.Cmd) {
	return m.sendControlFrame(proto.NewListSkills())
}

// cmdSkill is the entry for /skill <subcommand> [args]. Subcommands:
//   install <source>   install a skill from github:owner/repo[/subdir]
//   uninstall <name>   remove an installed skill
//   trust <name>       mark a skill as reviewed (writes .trusted)
//   untrust <name>     revoke trust
//   enable <name>      re-enable a disabled skill
//   disable <name>     hide from MetaPrompt and skill_use
//
// All subcommands are routed through protocol frames so the BACKEND performs
// the git clone / file writes — remote mode (SSE) is unaffected. No /skill
// update in MVP (use uninstall + install).
func cmdSkill(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		m.entries = append(m.entries, errorEntry{
			text: "usage: /skill install|uninstall|trust|untrust|enable|disable ...",
		})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "install":
		if len(rest) == 0 {
			m.entries = append(m.entries, errorEntry{text: "usage: /skill install github:owner/repo[/subdir]"})
			m.refresh()
			m.viewport.GotoBottom()
			return m, nil
		}
		return m.sendControlFrame(proto.NewInstallSkill(strings.Join(rest, " ")))
	case "uninstall":
		return skillNamed(m, rest, proto.NewUninstallSkill)
	case "trust":
		return skillNamed(m, rest, proto.NewTrustSkill)
	case "untrust":
		return skillNamed(m, rest, proto.NewUntrustSkill)
	case "enable":
		return skillNamed(m, rest, proto.NewEnableSkill)
	case "disable":
		return skillNamed(m, rest, proto.NewDisableSkill)
	default:
		m.entries = append(m.entries, errorEntry{text: "unknown /skill subcommand: " + sub})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
}

// skillNamed dispatches the single-name subcommands (uninstall/trust/etc.).
func skillNamed(m model, args []string, ctor func(string) proto.ClientFrame) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		m.entries = append(m.entries, errorEntry{text: "usage: /skill <subcommand> <name>"})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	return m.sendControlFrame(ctor(args[0]))
}
