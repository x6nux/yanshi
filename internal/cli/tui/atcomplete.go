package tui

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// atCandidateLimit caps how many paths the @ completion offers. The popup is a
// few rows tall; enumerating a large repo would cost more than it helps.
const atCandidateLimit = 12

// atWalkLimit caps how many directory entries the fallback scan visits.
// Without it, typing "@" at the root of a large repository walks the whole
// tree on a keystroke.
const atWalkLimit = 4000

// atCandidates returns the @path completion candidates for a typed prefix,
// most-recently-used first.
//
// Ordering is the point. Frecency.TopN had no production consumer at all, so
// "recently-used files sort first" could not be observed anywhere — which is
// why this had to land after the attachment path existed rather than beside
// it. Frecency-ranked paths come first, then a plain directory scan fills the
// rest so a file you have never touched is still reachable.
//
// Paths are returned RELATIVE to the work root: that is what gets inserted
// into the prompt, and extractAttachRefs resolves it against the same root.
func (m model) atCandidates(prefix string) []string {
	root := m.rootPath
	if root == "" {
		return nil
	}
	prefix = filepath.ToSlash(strings.TrimSpace(prefix))

	seen := map[string]bool{}
	var out []string
	add := func(rel string) bool {
		rel = filepath.ToSlash(rel)
		if rel == "" || seen[rel] {
			return false
		}
		if prefix != "" && !strings.Contains(strings.ToLower(rel), strings.ToLower(prefix)) {
			return false
		}
		seen[rel] = true
		out = append(out, rel)
		return len(out) >= atCandidateLimit
	}

	// Frecency first. A nil store (tui.frecency: false) returns nil, so the
	// scan below simply becomes the whole list.
	head := 0
	for _, abs := range m.frecency.TopNUnder(root, 0) {
		if rel, err := filepath.Rel(root, abs); err == nil {
			if _, statErr := os.Stat(abs); statErr == nil {
				if add(rel) {
					return out
				}
				head = len(out)
			}
		}
	}

	// Then a bounded scan, alphabetical, so an untouched file is still
	// reachable. Dotted directories and the usual build sinks are skipped:
	// nobody attaches a file from .git, and walking it is most of the cost.
	visited := 0
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || visited > atWalkLimit {
			return fs.SkipAll
		}
		visited++
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules" ||
				name == "vendor" || name == "target") {
				return fs.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		if add(rel) {
			return fs.SkipAll
		}
		return nil
	})
	sortTail(out, head)
	return out
}

// sortTail alphabetizes the scan-derived tail while leaving the
// frecency-ranked head in score order.
//
// head is the number of frecency-derived entries. The first version computed
// it as len(seen)-len(out), which is always zero — the two maps hold the same
// entries — so the tail was never sorted and the argument was decorative.
func sortTail(out []string, head int) {
	if head <= 0 || head >= len(out) {
		return
	}
	sort.Strings(out[head:])
}

// recordFrecency notes a path as used. A nil store (tui.frecency: false) makes
// it a no-op, so callers never branch — and, more importantly, disabling the
// feature stops the RECORDING too. Leaving that on would keep building a
// profile of the user's files that they explicitly asked not to have.
func (m *model) recordFrecency(absPath string) {
	if m.frecency == nil || absPath == "" {
		return
	}
	m.frecency.Record(absPath)
	m.enqueueSave(m.frecency.Save)
}

// atPrefixAtCursor returns the partial @path token the user is currently
// typing, and whether there is one.
//
// It looks at the LAST @token in the text and only offers completion when the
// text ends inside it — completing an @token the cursor has already moved past
// would rewrite something the user finished with. The token may not contain
// whitespace, matching attachTokenRe so the completion inserts a path the
// extractor will actually recognise.
func atPrefixAtCursor(text string) (string, bool) {
	at := strings.LastIndexByte(text, '@')
	if at < 0 {
		return "", false
	}
	if at > 0 && !isSpaceByte(text[at-1]) {
		return "", false // an email address, not an attachment
	}
	tok := text[at+1:]
	if strings.ContainsAny(tok, " \t\n") {
		return "", false // the user has moved on
	}
	return tok, true
}

func isSpaceByte(b byte) bool { return b == ' ' || b == '\t' || b == '\n' }

// updateAtPalette recomputes the @path completion list from the current input.
//
// It mirrors updatePalette: same field, same key handling, same rendering. The
// two cannot both be open — the input either starts with "/" or it does not —
// so sharing paletteItems keeps one popup, one selection index and one Tab
// handler instead of a second set that would drift.
//
// W-E-14: atMode (model.atMode) selects the filter applied to candidates:
//
//	0 = all (FS + plugins)
//	1 = FS only
//	2 = plugins only (MCP tool names from m.paletteMCPServers)
//
// Tab inside the popup cycles the mode. Mode label is injected into the
// command's help text so the popup header stays informative.
func (m *model) updateAtPalette() bool {
	prefix, ok := atPrefixAtCursor(m.input.Value())
	if !ok {
		return false
	}

	const (
		atModeAll     = 0
		atModeFS      = 1
		atModePlugins = 2
		atModeLast    = 2
	)

	modeLabel := []string{"all", "files", "plugins"}[m.atMode]

	var items []command

	switch m.atMode {
	case atModeAll, atModeFS:
		cands := m.atCandidates(prefix)
		for _, c := range cands {
			items = append(items, command{name: c, help: "attach file [" + modeLabel + "]", kind: cmdAtPath})
		}
		if m.atMode == atModeAll {
			// Also add plugin (MCP tool) names.
			items = append(items, atPluginCandidates(m, prefix, modeLabel)...)
		}
	case atModePlugins:
		items = atPluginCandidates(m, prefix, modeLabel)
	}

	m.paletteItems = items
	if m.paletteSel >= len(items) || m.paletteSel < 0 {
		m.paletteSel = 0
	}
	return true
}

// atPluginCandidates returns MCP tool names as @-completion candidates.
// Each entry has kind=cmdAtPath so the existing Tab-complete machinery works.
func atPluginCandidates(m *model, prefix, modeLabel string) []command {
	var out []command
	prefix = strings.ToLower(prefix)
	for _, srv := range m.paletteMCPServers {
		for _, tool := range srv.Tools {
			name := tool.Name
			if prefix != "" && !strings.Contains(strings.ToLower(name), prefix) {
				continue
			}
			out = append(out, command{name: name, help: "MCP tool [" + modeLabel + "]", kind: cmdAtPath})
		}
	}
	return out
}

// cycleAtMode advances m.atMode to the next filter and rebuilds the palette.
// Called by the Tab handler when the @ popup is open.
func (m *model) cycleAtMode() {
	m.atMode = (m.atMode + 1) % 3
	m.updateAtPalette()
	m.paletteSel = 0
}

// completeAtPath replaces the partial @token with the selected path.
func (m *model) completeAtPath(sel command) {
	v := m.input.Value()
	at := strings.LastIndexByte(v, '@')
	if at < 0 {
		return
	}
	m.input.SetValue(v[:at+1] + sel.name + " ")
	m.input.CursorEnd()
	m.paletteItems = nil
	m.atMode = 0 // reset for the next @
	m.growInput()
}
