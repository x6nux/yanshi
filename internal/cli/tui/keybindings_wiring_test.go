// Package tui — the RE-15 keyBindings accounting mechanism (fix-e3b of
// W-E-tui).
//
// review-e3.md's RE-15 found the F1 help popup's keyBindings table
// (help.go) claiming Ctrl+E is "toggle history view" — that binding moved to
// Ctrl+E opening $VISUAL/$EDITOR two batches earlier, and Ctrl+T (the
// fullscreen pager) was never added to the table at all. keyBindings' own
// doc comment says "新增 KeyMsg 分支时,同步更新此表" (update this table when
// adding a new KeyMsg branch) but nothing ever checked that anyone did —
// exactly the RE-B/RE-12 shape capability_wiring_test.go's package comment
// describes: a hand-maintained table with no consumer forcing it to stay
// truthful, so it silently drifts as the real dispatch grows around it.
//
// This file is the same mechanism, applied to a different seam:
//
//  1. keyBindingsCensus below is a census: one entry per `tea.Key<X>`
//     identifier that appears as a case value in handleKeyMsg's TOP-LEVEL
//     switch (handlers.go, the `switch msg.Type {` that starts after every
//     modal-guard `if` block — see "what this does NOT catch" for why the
//     scope stops there). Each entry either names the keyBindings Label(s)
//     that document it, or gives an explicit, non-empty reason it is
//     intentionally undocumented.
//  2. TestKeyBindingsCensusMatchesSwitch parses handlers.go with go/ast and
//     asserts the switch's case identifiers are EXACTLY the census's keys —
//     in both directions. Add a new `case tea.KeyWhatever:` without a census
//     entry: red. Delete the census entry for a case that no longer exists:
//     red too (the same "dead entry" direction CLAUDE.md's debt tables
//     enforce elsewhere in this repo).
//  3. TestKeyBindingsCensusLabelsExistInKeyBindings then proves every
//     claimed Label is not just asserted but real — it looks the Label up in
//     the actual keyBindings slice (help.go) and fails if it is missing.
//     This is what would have caught RE-15 directly: a census entry claiming
//     "Ctrl+E: documented as the Label 'Ctrl+E'" fails the moment that Label
//     is absent from keyBindings, which is exactly the state the table was
//     in (Ctrl+E was simply not there under any label).
//  4. TestKeyBindingsCensusExemptEntriesHaveReasons guards the census map
//     itself: every entry must carry either a non-empty Labels list or a
//     non-empty Exempt reason — never both empty (silently unaccounted-for)
//     and never both set (the label lookup would then never run).
//
// What this catches: a new `case tea.Key<X>:` added to handleKeyMsg's
// top-level switch without updating keyBindings (or explicitly exempting it
// with a reason) — the two named gaps (Ctrl+E, Ctrl+T) plus one more this
// census surfaced that review-e3.md didn't name: Shift+Tab (cycles the
// permission mode via m.cycleMode(), handlers.go) had the same gap and is
// fixed in the same commit as RE-15. It also catches a census entry whose
// Label was renamed or removed from keyBindings without updating the census
// (TestKeyBindingsCensusLabelsExistInKeyBindings goes red).
//
// What this does NOT catch:
//
//   - Bindings inside the nested modal-guard `if` blocks at the TOP of
//     handleKeyMsg (pager/help/restore-picker/rollback-picker/action-palette/
//     history-search/interactive-picker, handlers.go lines ~38-312). Their
//     `case tea.Key...` clauses live inside an `if` statement's body, not as
//     a direct statement of the function body, so the AST walk below (which
//     locates ONE specific top-level *ast.SwitchStmt, not "every switch
//     anywhere in the function") never sees them. This is deliberate, not an
//     oversight: those keys are context-local to an already-open popup and
//     are documented by that popup's own on-screen hint (e.g. pagerHint's
//     "q/Esc/Ctrl+T close · ↑↓ PgUp/PgDn Home/End scroll", or the help popup
//     itself), not by the global keyBindings table — folding them in would
//     also multiply-count keys like KeyEscape and KeyUp/KeyDown, which
//     recur in nearly every modal block with entirely different meanings.
//   - The two `if msg.Alt && msg.Type == tea.Key...` guards immediately
//     BEFORE the top-level switch (handlers.go ~255-272: Alt+R opens history
//     search, Alt+Up recalls the last queued message). These are not case
//     clauses of any switch, so this AST walk structurally cannot see them.
//     Both are already present in keyBindings by hand; a THIRD such
//     top-level `if msg.Alt` guard added the same way would land in this
//     blind spot the same way capability_wiring_test.go's census cannot see
//     a field wired outside buildModelForCapability.
//   - m.vimKey(msg) and m.remappedKey(msg) (handlers.go, checked just before
//     the top-level switch): C15's vim-mode and user-configurable keymap
//     dispatch is runtime DATA (parsed from the keymap package / user
//     config), not a static `case` clause — categorically outside what a
//     compile-time AST walk over one switch statement can enumerate.
//   - Whether a Hint string is ACCURATE. Proving a Label exists in
//     keyBindings is not the same as proving its Hint describes current
//     behavior — RE-15's actual reported bug was a wrong Hint on an entry
//     whose Label, if it had existed, would look identical to a correct one
//     to this mechanism. That half is still a human review question, the
//     same caveat capability_wiring_test.go makes for "differs but both
//     wrong" fields.
//   - A second unrelated top-level switch added to handleKeyMsg. The AST
//     walk requires finding EXACTLY one top-level *ast.SwitchStmt in the
//     function body and fails loudly (require, not assert) if that count is
//     ever not 1 — so this is a known, intentional trigger for needing to
//     update this test's search logic, not a silent gap.
package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keyCensusEntry is one census row: either the keyBindings Label(s) that
// document this `tea.Key<X>` case, or — mutually exclusively — a reason it
// is intentionally left out of the global keyBindings table.
type keyCensusEntry struct {
	labels []string
	exempt string
}

// keyBindingsCensus is the authoritative census described in this file's
// package comment: one entry per `tea.Key<X>` identifier used as a case
// value in handleKeyMsg's top-level switch (handlers.go).
var keyBindingsCensus = map[string]keyCensusEntry{
	"KeyEscape":    {labels: []string{"Esc"}},
	"KeyShiftTab":  {labels: []string{"Shift+Tab"}},
	"KeyCtrlC":     {labels: []string{"Ctrl+C"}},
	"KeyPgUp":      {labels: []string{"PgUp/PgDn"}},
	"KeyPgDown":    {labels: []string{"PgUp/PgDn"}},
	"KeyTab":       {labels: []string{"Tab"}},
	"KeyEnter":     {labels: []string{"Enter"}},
	"KeyCtrlEnter": {labels: []string{"Ctrl+Enter"}},
	"KeyCtrlV":     {labels: []string{"Ctrl+V"}},
	"KeyCtrlO":     {labels: []string{"Ctrl+O"}},
	"KeyCtrlK":     {labels: []string{"Ctrl+K"}},
	"KeyCtrlS":     {labels: []string{"Ctrl+S"}},
	"KeyCtrlE":     {labels: []string{"Ctrl+E"}},
	"KeyCtrlT":     {labels: []string{"Ctrl+T"}},
	"KeyF1":        {labels: []string{"F1"}},
	"KeyUp": {
		exempt: "only acts within an already-open modal (permission prompt " +
			"or the /command autocomplete dropdown, both gated by " +
			"m.pendingPermission()/m.paletteOpen() inside this case); outside " +
			"those it falls through unhandled to the textarea's own default " +
			"arrow-key cursor movement, not a yanshi-defined shortcut. Still " +
			"documented for the user under the combined 'Up/Down' Label.",
	},
	"KeyDown": {
		exempt: "same as KeyUp — modal-gated navigation, documented under " +
			"the combined 'Up/Down' Label.",
	},
	"KeyRunes": {
		exempt: "dispatches to (a) the permission prompt's y/a/n, which " +
			"model.go's permSel field comment documents directly ('Up/Down + " +
			"Enter (or y/a/n) resolves it'), and (b) default character/paste " +
			"handling, which is the textarea's baseline typing behavior, not " +
			"a distinct shortcut a single Label could name.",
	},
}

// findHandleKeyMsgSwitch parses handlers.go, locates handleKeyMsg, and
// returns the ONE top-level *ast.SwitchStmt in its body — see this file's
// package comment for why "top-level" (not "anywhere in the function", which
// ast.Inspect would give for free but would also pull in every nested
// modal-guard switch this census deliberately excludes).
func findHandleKeyMsgSwitch(t *testing.T) *ast.SwitchStmt {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "handlers.go", nil, 0)
	require.NoError(t, err, "parse handlers.go")

	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "handleKeyMsg" {
			fn = fd
			break
		}
	}
	require.NotNil(t, fn, "handlers.go must declare handleKeyMsg — it is the seam this census walks")

	var switches []*ast.SwitchStmt
	for _, stmt := range fn.Body.List {
		if sw, ok := stmt.(*ast.SwitchStmt); ok {
			switches = append(switches, sw)
		}
	}
	require.Len(t, switches, 1,
		"expected exactly one top-level switch in handleKeyMsg's body — if this "+
			"changed on purpose, findHandleKeyMsgSwitch's search needs updating too")
	return switches[0]
}

// switchCaseKeyIdents extracts the `tea.Key<X>` identifier name from every
// case value in sw. Fails the test if a case expression is not a plain
// `tea.Key<X>` selector (there is no `default:` clause in the switch this
// census targets, and every case value there is a bubbletea KeyType
// constant — anything else means the switch changed shape and this
// extraction needs to change with it).
func switchCaseKeyIdents(t *testing.T, sw *ast.SwitchStmt) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, stmt := range sw.Body.List {
		cc, ok := stmt.(*ast.CaseClause)
		require.True(t, ok, "switch body must contain only case clauses")
		require.NotEmpty(t, cc.List, "no default: clause expected in this switch")
		for _, expr := range cc.List {
			sel, ok := expr.(*ast.SelectorExpr)
			require.True(t, ok, "case value %v is not a tea.Key<X> selector", expr)
			xIdent, ok := sel.X.(*ast.Ident)
			require.True(t, ok && xIdent.Name == "tea", "case value %v is not rooted at the tea package", expr)
			out[sel.Sel.Name] = true
		}
	}
	return out
}

// TestKeyBindingsCensusMatchesSwitch asserts handleKeyMsg's top-level switch
// case identifiers exactly match keyBindingsCensus' keys — see the package
// comment for what each direction of mismatch means.
func TestKeyBindingsCensusMatchesSwitch(t *testing.T) {
	sw := findHandleKeyMsgSwitch(t)
	found := switchCaseKeyIdents(t, sw)

	var got []string
	for name := range found {
		got = append(got, name)
	}
	var censused []string
	for name := range keyBindingsCensus {
		censused = append(censused, name)
	}
	sort.Strings(got)
	sort.Strings(censused)

	assert.Equal(t, censused, got,
		"handleKeyMsg's top-level switch case identifiers must exactly match "+
			"keyBindingsCensus' keys — add a census entry (labels or an exempt "+
			"reason) for every new case, and delete the entry for any case that "+
			"no longer exists")
}

// TestKeyBindingsCensusLabelsExistInKeyBindings proves every censused Label
// is real, not just claimed — this is what directly catches RE-15's shape:
// a census entry naming "Ctrl+E" as documented fails here the moment
// keyBindings (help.go) does not actually contain that Label.
func TestKeyBindingsCensusLabelsExistInKeyBindings(t *testing.T) {
	present := map[string]bool{}
	for _, kb := range keyBindings {
		present[kb.Label] = true
	}
	for name, entry := range keyBindingsCensus {
		for _, label := range entry.labels {
			assert.True(t, present[label],
				"census entry %s claims Label %q is documented in keyBindings, but it is not there",
				name, label)
		}
	}
}

// TestKeyBindingsCensusExemptEntriesHaveReasons guards the census map's own
// shape: every entry must carry either labels or a reason, never neither
// (silently unaccounted-for) and never both (the reason would then be dead
// weight, since the label lookup above already covers it).
func TestKeyBindingsCensusExemptEntriesHaveReasons(t *testing.T) {
	for name, entry := range keyBindingsCensus {
		hasLabels := len(entry.labels) > 0
		hasReason := entry.exempt != ""
		assert.True(t, hasLabels || hasReason,
			"census entry %s has neither labels nor an exempt reason — it is unaccounted for", name)
		assert.False(t, hasLabels && hasReason,
			"census entry %s has both labels and an exempt reason — pick one", name)
	}
}
