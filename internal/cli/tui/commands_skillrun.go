package tui

import (
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/x6nux/yanshi/internal/proto"
)

// Skill-provided slash commands, and why they all live under `/skill run`.
//
// A skill is a directory with a SKILL.md; making one advertise a TOP-LEVEL
// slash command (`/deploy`) was the obvious design and is the wrong one, for
// two reasons that compound:
//
//   - internal/archtest/slashcmd_test.go parses `commandTable` out of
//     commands.go to derive the set of real commands, and forbids any text
//     carrier from advertising a name that is not in it. That gate exists
//     because one phantom command (/keymap) escaped three separate cleanups
//     across four carriers. A registry that can mint top-level names at
//     runtime makes the compile-time table an incomplete answer, and the only
//     ways to keep the gate meaningful would be to teach it about installed
//     skills — which vary per machine — or to weaken it into an assertion that
//     is true by construction.
//   - `/deploy` gives a user no way to tell a built-in from a third-party
//     directory that arrived with `skill install github:someone/pack`. The
//     names also collide with each other and with future built-ins, and the
//     resolution order would be invisible.
//
// `/skill run <name>` costs one extra word and removes both problems: the
// static table stays the whole truth about top-level commands, and the prefix
// says out loud where the behaviour comes from. Completion still makes the
// names discoverable — the palette offers installed skill names after
// `/skill run `, which is the discoverability the top-level form was for.

// skillCommand is one skill-declared command surfaced under `/skill run`.
type skillCommand struct {
	// Name is the skill name, which is also the token typed after `run`.
	Name string
	// Help is the skill's description, shown in the palette and by
	// `/skill run` with no argument.
	Help string
	// Unavailable is non-empty when the skill exists but cannot be used —
	// disabled, missing a required program, or withheld by the content scan.
	// The entry is still LISTED: a skill that silently vanishes is
	// indistinguishable from one that was never installed, which is the exact
	// confusion the /skills listing exists to prevent.
	Unavailable string
}

// skillCommandsFrom projects a skills_list frame into the runnable command set,
// sorted by name so the palette order does not depend on map iteration.
//
// Availability is computed HERE rather than filtered at the source because the
// TUI has to render the unavailable ones differently, not omit them. The three
// reasons are ordered by which one the user must fix first: a disabled skill
// stays disabled no matter what PATH holds, and a scan-blocked skill is not
// worth reporting a missing program for.
func skillCommandsFrom(skills []proto.SkillInfo) []skillCommand {
	out := make([]skillCommand, 0, len(skills))
	for _, sk := range skills {
		out = append(out, skillCommand{
			Name:        sk.Name,
			Help:        sk.Description,
			Unavailable: skillUnavailableReason(sk),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// skillUnavailableReason returns why a skill cannot be run, or "".
func skillUnavailableReason(sk proto.SkillInfo) string {
	if !sk.Enabled {
		return "disabled"
	}
	if len(sk.Missing) > 0 {
		return "missing: " + strings.Join(sk.Missing, ", ")
	}
	return ""
}

// matchingSkillRunItems returns palette entries for `/skill run <prefix>`.
//
// The entries carry kind cmdKindSkillRun rather than cmdSlash so paletteComplete
// inserts the full `/skill run <name>` line instead of treating the skill name
// as a top-level command. Getting that wrong would put a name in the input that
// runCommand then rejects as unknown — the palette would be advertising
// something the parser does not accept.
func matchingSkillRunItems(skills []skillCommand, prefix string) []command {
	var out []command
	for _, sk := range skills {
		if !strings.HasPrefix(sk.Name, prefix) {
			continue
		}
		help := sk.Help
		if sk.Unavailable != "" {
			help = "(" + sk.Unavailable + ") " + help
		}
		out = append(out, command{
			name: sk.Name, help: help, kind: cmdKindSkillRun,
			disabled: sk.Unavailable != "",
		})
	}
	return out
}

// skillRunPrefix is what the input must start with for the palette to offer
// skill names. The trailing space matters: `/skill run` with no space is still
// the subcommand being typed, and completing a skill name there would replace
// the word the user is halfway through.
const skillRunPrefix = "/skill run "

// updateSkillRunPalette populates the palette with skill names when the input
// is a `/skill run …` line. Reports whether it took over the palette.
func (m *model) updateSkillRunPalette(input string) bool {
	if !strings.HasPrefix(input, skillRunPrefix) {
		return false
	}
	arg := strings.TrimPrefix(input, skillRunPrefix)
	if strings.Contains(arg, " ") {
		// The name is complete and free-text arguments follow; there is
		// nothing left to complete.
		m.paletteItems = nil
		return true
	}
	m.paletteItems = matchingSkillRunItems(m.skillCommands, arg)
	if m.paletteSel >= len(m.paletteItems) || m.paletteSel < 0 {
		m.paletteSel = 0
	}
	return true
}

// cmdSkillRun handles `/skill run <name> [args…]`: it resolves the name against
// the installed skills and sends the invocation as an ordinary user turn.
//
// It is a user turn rather than a control frame because running a skill IS a
// model turn — the skill body becomes instructions the model follows. Sending
// it as a control frame would clobber the stream the same way sendControlFrame
// does for /mcp, and nothing would ever answer.
func cmdSkillRun(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		return m.skillRunUsage()
	}
	name := args[0]
	sk, found := m.lookupSkillCommand(name)
	if !found {
		m.entries = append(m.entries, errorEntry{
			text: "unknown skill: " + name + " (try /skills to list installed skills)",
		})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	if sk.Unavailable != "" {
		// Refusing here rather than sending is what keeps the failure legible.
		// The backend would refuse too, but as a tool error inside a turn the
		// user has already paid for.
		m.entries = append(m.entries, errorEntry{
			text: "skill " + name + " cannot run: " + sk.Unavailable,
		})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	mm, cmd := m.dispatchSend(skillInvocation(name, args[1:]), false)
	return mm, cmd
}

// skillInvocation renders the user-turn text for a skill run.
//
// It is phrased as an instruction to use the skill rather than pasted skill
// content, because the skill body lives on the BACKEND: in remote mode the TUI
// may not even be on the same machine as the skill directory. The model
// retrieves the body through skill_use, which is also where the enable /
// requires / content-scan gates are enforced.
func skillInvocation(name string, args []string) string {
	line := "Use the " + name + " skill."
	if rest := strings.TrimSpace(strings.Join(args, " ")); rest != "" {
		line += " " + rest
	}
	return line
}

// lookupSkillCommand finds an installed skill by exact name.
func (m model) lookupSkillCommand(name string) (skillCommand, bool) {
	for _, sk := range m.skillCommands {
		if sk.Name == name {
			return sk, true
		}
	}
	return skillCommand{}, false
}

// skillRunUsage renders the list of runnable skills, or the reason there are
// none. "No skills installed" and "the TUI has not been told yet" are different
// states and the message says which one it is, because the second one resolves
// itself and the first one needs `/skill install`.
func (m model) skillRunUsage() (tea.Model, tea.Cmd) {
	if len(m.skillCommands) == 0 {
		if !m.skillsLoaded {
			m.entries = append(m.entries, errorEntry{
				text: "the skill list has not arrived from the backend yet; run /skills and try again",
			})
		} else {
			m.entries = append(m.entries, errorEntry{
				text: "no skills installed (see /skill install)",
			})
		}
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	var b strings.Builder
	b.WriteString("usage: /skill run <name> [instructions]\n\n  runnable skills:\n")
	for _, sk := range m.skillCommands {
		b.WriteString("    " + sk.Name)
		if sk.Unavailable != "" {
			b.WriteString("  [" + sk.Unavailable + "]")
		} else if sk.Help != "" {
			b.WriteString("  " + sk.Help)
		}
		b.WriteString("\n")
	}
	m.entries = append(m.entries, ackEntry{text: b.String()})
	m.refresh()
	m.viewport.GotoBottom()
	return m, nil
}
