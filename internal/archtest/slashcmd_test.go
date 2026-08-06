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
// All four original entries GRADUATED in W8: /keymap, /vim, /contrast and
// /locale are now registered in commandTable, the preference cascade they
// mutate is wired, and the documentation that advertised them is finally
// telling the truth. The graduation direction below is what forced this map to
// be emptied in the same change — an implemented command left listed here
// would make the gate forbid documenting a real feature.
//
// The map stays, with the mechanism intact, because it is a permanent denylist
// and not a debt table: the next phantom gets added here rather than
// rediscovered across four carriers a fourth time.
var phantomSlashCommands = map[string]string{}

// phantomDenialMarkers are the phrases that mark a mention as a DENIAL ("this
// command does not exist") rather than an advertisement. A mention sitting next
// to one is allowed to name a phantom, which is what lets the codebase keep
// saying out loud that these commands are not real — the doc comments on
// tui.Model.prefs and doctor.checkKeymapConfig exist precisely to say that.
//
// The match is scoped to a WINDOW OF LINES around the mention, not to the whole
// file, and the file-scoped version this replaces was worse than no exemption
// at the two carriers it mattered most for. docs/user-guide/tui.md and
// docs/user-guide/configuration.md are where the retired phantoms were
// advertised in the first place; both were then fixed by writing the denial
// down, and a file-scoped marker turned each of those fixes into a permanent
// blanket immunity for the whole file. Appending a brand-new advertisement to
// either one passed silently, i.e. the gate was guaranteed not to catch its own
// defect recurring in situ, while the same line appended to any other document
// reddened immediately.
//
// Distinguishing assertion from denial in the general case still needs a parser
// for prose rather than a substring scan; what a window buys is that the
// concession has to be re-earned at every mention instead of once per file.
var phantomDenialMarkers = []string{
	"not registered", "no such command", "never registered",
	"从未注册", "未注册", "并不存在", "不存在的命令",
	"命令不存在", "命令都不存在",
}

// phantomDenialWindow is how many lines away from a mention a denial marker may
// sit and still cover it. Zero (same line only) is too tight for real prose:
// wrapped Go comments and multi-sentence Markdown routinely put the name and
// the "…is not registered" clause on neighbouring lines. Three is the smallest
// value that covers every legitimate denial in the tree today, and it is
// deliberately small enough that a marker somewhere else in the same section
// does not launder an advertisement.
const phantomDenialWindow = 3

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
//
// ledger: H2/UDOC1#3 与实际不漂移
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
		// an absolute filesystem path and a URL whose last segment merely
		// starts with the name out, while catching the command written bare
		// with an argument after it, or wrapped in backticks.
		patterns[name] = regexp.MustCompile(`(^|[^A-Za-z0-9_./:\-])/` + name + `([^A-Za-z0-9_/.\-]|$)`)
	}

	type finding struct {
		rel, name string
		line      int
	}
	var findings []finding

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(mustRel(t, root, path))
		if d.IsDir() {
			switch {
			case isNestedCheckoutDir(rel, d.Name()),
				rel == "third_party", rel == "reference",
				rel == "docs/archive", rel == "docs/superpowers/plans",
				rel == "docs/superpowers/specs", rel == "docs/superpowers/notes":
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
		lines := strings.Split(string(data), "\n")
		denials := denialLines(lines)
		for i, line := range lines {
			for name, re := range patterns {
				if !re.MatchString(line) || nearDenial(denials, i) {
					continue
				}
				findings = append(findings, finding{rel, name, i + 1})
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
		if findings[i].line != findings[j].line {
			return findings[i].line < findings[j].line
		}
		return findings[i].name < findings[j].name
	})
	for _, f := range findings {
		t.Errorf("%s:%d advertises /%s, which commandTable does not register (%s). "+
			"Delete the claim, or — if the line needs to discuss the phantom — say "+
			"plainly within %d lines of it that the command is not registered (one "+
			"of %v). A denial elsewhere in the file no longer covers this line.",
			f.rel, f.line, f.name, phantomSlashCommands[f.name],
			phantomDenialWindow, phantomDenialMarkers)
	}
}

// denialLines reports which line indices carry a denial marker.
func denialLines(lines []string) map[int]bool {
	out := map[int]bool{}
	for i, line := range lines {
		for _, marker := range phantomDenialMarkers {
			if strings.Contains(line, marker) {
				out[i] = true
				break
			}
		}
	}
	return out
}

// nearDenial reports whether a denial marker sits within phantomDenialWindow
// lines of index i.
func nearDenial(denials map[int]bool, i int) bool {
	for d := -phantomDenialWindow; d <= phantomDenialWindow; d++ {
		if denials[i+d] {
			return true
		}
	}
	return false
}
