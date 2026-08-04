package archtest

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Phantom slash commands (all text carriers, not just Markdown)
// ---------------------------------------------------------------------------

// phantomSlashCommands are TUI slash commands that documentation has claimed
// exist and that commandTable has never registered.
//
// The reason this gate scans every text carrier rather than one is that
// /keymap escaped three separate cleanups, each of which swept exactly one
// carrier: 5eb5869 cleared it out of the user-facing Markdown, cf088f7 cleared
// it out of the Go doc comments, and BOTH times config.example.yaml — the file
// an operator literally copies to make config.yaml, i.e. the carrier closest
// to the user — sat outside the scan surface. GOV5, GOV7 and GOV9 all reason
// about Go symbols and none of them can see a slash command inside a YAML
// comment.
//
// This is a permanent denylist, NOT a debt table, so it does not shrink
// monotonically and it has no "dead entry" rule for absence of mentions:
// zero mentions anywhere is the goal state, not a stale entry. The one dead
// direction that does apply is a name graduating into commandTable — if the
// command gets implemented it is no longer a phantom, and the entry must go.
var phantomSlashCommands = map[string]string{
	"keymap":   "config.example.yaml advertised `/keymap reset` writing a KeymapReset tombstone; no such command is registered and no preferences file is loaded",
	"vim":      "docs/user-guide/configuration.md advertised runtime `/vim`; internal/cli/tui/model.go records that it was never registered",
	"contrast": "docs/user-guide/configuration.md advertised runtime `/contrast`; use /theme high-contrast, which does exist",
	"locale":   "advertised alongside /vim and /contrast as a runtime preference toggle; never registered",
}

// phantomDenialMarkers are the phrases that mark a mention as a DENIAL ("this
// command does not exist") rather than an advertisement. A file carrying one
// is allowed to name a phantom, which is what lets the codebase keep saying
// out loud that these commands are not real — the doc comments on
// tui.Model.prefs and doctor.checkKeymapConfig exist precisely to say that.
//
// The check is file-scoped, so a file that both advertises and denies passes.
// That is the same concession removal_test.go makes for the D2/O12 tombstone
// and for the same reason: telling assertion from denial in the general case
// needs a parser for prose, not a substring scan. The scope this gate is
// actually defending is "no carrier advertises a phantom while being silent
// about its non-existence", and a file-level marker covers that.
var phantomDenialMarkers = []string{
	"not registered", "no such command", "never registered",
	"从未注册", "未注册", "并不存在", "不存在的命令",
	"命令不存在", "命令都不存在",
}

// slashCarrierExts are the text carriers scanned. The point of the list is
// that it is longer than one: .yaml is here because that is the carrier the
// previous two cleanups missed.
var slashCarrierExts = map[string]bool{
	".md": true, ".yaml": true, ".yml": true, ".go": true, ".json": true,
}

// commandTableName matches a commandTable entry's name field.
var commandTableName = regexp.MustCompile(`name:\s*"([a-z][a-z0-9-]*)"`)

// liveSlashCommands reads the slash commands actually registered in
// internal/cli/tui/commands.go. Deriving the live set from source (rather than
// listing it here) is what makes the graduation direction below self-updating.
func liveSlashCommands(t *testing.T) map[string]bool {
	t.Helper()
	path := filepath.Join(moduleRoot(t), "internal", "cli", "tui", "commands.go")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read commands.go: %v", err)
	}
	body := string(data)
	start := strings.Index(body, "var commandTable")
	if start < 0 {
		t.Fatal("commandTable not found in internal/cli/tui/commands.go — this gate " +
			"derives the live command set from it and cannot run blind")
	}
	end := strings.Index(body[start:], "\n}")
	if end < 0 {
		t.Fatal("commandTable has no closing brace at column 0")
	}
	live := map[string]bool{}
	for _, m := range commandTableName.FindAllStringSubmatch(body[start:start+end], -1) {
		live[m[1]] = true
	}
	if len(live) == 0 {
		t.Fatal("parsed zero names out of commandTable: the gate would be vacuous")
	}
	return live
}

// TestPhantomSlashCommandsNotAdvertised fails when any text carrier presents a
// never-registered slash command as if an operator could type it.
func TestPhantomSlashCommandsNotAdvertised(t *testing.T) {
	root := moduleRoot(t)
	live := liveSlashCommands(t)

	// Graduation direction: a phantom that got implemented is not a phantom.
	for name := range phantomSlashCommands {
		if live[name] {
			t.Errorf("/%s is now registered in commandTable but is still listed in "+
				"phantomSlashCommands — delete the entry; the gate is otherwise "+
				"forbidding documentation of a real command", name)
		}
	}

	patterns := map[string]*regexp.Regexp{}
	for name := range phantomSlashCommands {
		// A left boundary that is not a path/URL character, and a right
		// boundary that is not a further path segment: this is what keeps
		// "/var/log/..." and "https://host/keymap-ish" out while catching
		// "/keymap reset" and "`/keymap`".
		patterns[name] = regexp.MustCompile(`(^|[^A-Za-z0-9_./:\-])/` + name + `([^A-Za-z0-9_/.\-]|$)`)
	}

	type finding struct{ rel, name string }
	var findings []finding

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(mustRel(t, root, path))
		if d.IsDir() {
			switch {
			case rel == ".git", rel == "third_party", rel == "reference",
				rel == "docs/archive", rel == "docs/superpowers/plans",
				rel == "docs/superpowers/specs", rel == "docs/superpowers/notes",
				rel == "node_modules", strings.HasSuffix(rel, "/node_modules"):
				return filepath.SkipDir
			}
			return nil
		}
		if !slashCarrierExts[strings.ToLower(filepath.Ext(rel))] {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		body := string(data)
		denied := false
		for _, marker := range phantomDenialMarkers {
			if strings.Contains(body, marker) {
				denied = true
				break
			}
		}
		if denied {
			return nil
		}
		for name, re := range patterns {
			if re.MatchString(body) {
				findings = append(findings, finding{rel, name})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].rel != findings[j].rel {
			return findings[i].rel < findings[j].rel
		}
		return findings[i].name < findings[j].name
	})
	for _, f := range findings {
		t.Errorf("%s advertises /%s, which commandTable does not register (%s). "+
			"Delete the claim, or — if the file needs to discuss the phantom — say "+
			"plainly that it is not registered (one of %v).",
			f.rel, f.name, phantomSlashCommands[f.name], phantomDenialMarkers)
	}
}
