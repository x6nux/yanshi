// Package tui — the RE-12 wiring mechanism (fix-e3a of W-E-tui).
//
// E1's RE-B already established that a field NewProgram sets can be mutated
// away to a constant with the full suite staying green, because
// *tea.Program hides the model it wraps (third_party/bubbletea's
// Program.initialModel is unexported) — nothing downstream of NewProgram can
// observe what it built. That batch fixed the two fields present at the
// time (color profile, via a process-global side effect; the alt-screen
// ProgramOption set, via the extracted programOptions function). RE-12 found
// a THIRD field — m.mouseEnabled — added two batches later in the very same
// function, with the exact same blind spot: `m.mouseEnabled = false` left
// `go test ./...` at 0 FAIL / 68 ok.
//
// Patching mouseEnabled alone would repeat the batch-scoped fix that already
// failed once. The mechanism here instead makes the SHAPE of the bug
// structurally impossible to add silently:
//
//  1. model.go's NewProgram now delegates every capability-derived field
//     assignment to buildModelForCapability(sess, root, project, cap) — a
//     function that takes cap as a parameter and returns the model, so a
//     test can call the real production code and inspect the result.
//  2. capabilityWiredFields below is a census: one entry per field
//     buildModelForCapability sets, each with a low/high TermCapability pair
//     and an accessor.
//  3. TestCapabilityWiredFieldsMatchCensus parses buildModelForCapability's
//     body with go/ast and asserts its `m.<field> = …` assignments are
//     EXACTLY the census's keys — in both directions. Add a new
//     `m.newField = cap.Whatever` line without a census entry: this test
//     goes red. Remove the census entry for a field that's no longer
//     assigned: this test goes red too (the same "dead entry" direction
//     CLAUDE.md's debt tables enforce elsewhere in this repo).
//  4. TestBuildModelForCapability_FieldsFollowCapability then calls the real
//     buildModelForCapability with each entry's low and high capability and
//     asserts the field's value actually differs — the same shape of proof
//     RE-12 found was missing for mouseEnabled (a mutation to a constant is
//     caught because low and high stop differing).
//
// What this catches: any future field wired into buildModelForCapability
// without both a census entry AND a working low/high pair (the field would
// either fail the AST census, or fail the differential assertion by being
// equal under both capabilities — which is exactly what "mouseEnabled
// always false" looks like).
//
// What this does NOT catch: a field wired somewhere OTHER than
// buildModelForCapability (the AST walk only looks at that one function's
// body) — that would be a new instance of the RE-B/RE-12 shape living
// outside the seam this mechanism guards, not a gap in the mechanism
// itself. It also does not catch a census entry whose low/high pair happens
// to produce different-but-both-wrong values (e.g. inverted logic) — the
// differential assertion proves "depends on cap", not "wired to the
// correct field of cap". That second half is what
// TestBuildModelForCapability_FieldsFollowCapability's per-field name
// documents (mouseEnabled ties to AltScreen specifically), not something an
// automated census can derive.
package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/cli"
)

// capabilityWiredFields is the authoritative census described in this file's
// package comment: one entry per model field buildModelForCapability (model.go)
// sets from the detected TermCapability, each with a capability pair that
// must produce two different values for that field.
var capabilityWiredFields = map[string]struct {
	low, high cli.TermCapability
	get       func(model) any
}{
	"mouseEnabled": {
		low:  cli.TermCapability{AltScreen: false},
		high: cli.TermCapability{AltScreen: true},
		get:  func(m model) any { return m.mouseEnabled },
	},
	"titleEnabled": {
		low:  cli.TermCapability{AltScreen: false},
		high: cli.TermCapability{AltScreen: true},
		get:  func(m model) any { return m.titleEnabled },
	},
}

// TestCapabilityWiredFieldsMatchCensus parses buildModelForCapability's body
// and requires its `m.<field> = …` assignments to exactly match
// capabilityWiredFields' keys — see the package comment for what each
// direction of mismatch means.
func TestCapabilityWiredFieldsMatchCensus(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "model.go", nil, 0)
	require.NoError(t, err, "parse model.go")

	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "buildModelForCapability" {
			fn = fd
			break
		}
	}
	require.NotNil(t, fn, "model.go must declare buildModelForCapability — it is the seam this census walks")

	assigned := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range assign.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			if recv, ok := sel.X.(*ast.Ident); ok && recv.Name == "m" {
				assigned[sel.Sel.Name] = true
			}
		}
		return true
	})

	var got []string
	for name := range assigned {
		got = append(got, name)
	}
	var censused []string
	for name := range capabilityWiredFields {
		censused = append(censused, name)
	}
	sort.Strings(got)
	sort.Strings(censused)

	assert.Equal(t, censused, got,
		"buildModelForCapability's m.<field> assignments must exactly match capabilityWiredFields' keys — "+
			"add a census entry (with a low/high pair and accessor) for every new field, and delete the "+
			"entry for any field that no longer gets assigned there")
}

// TestBuildModelForCapability_FieldsFollowCapability proves each censused
// field actually depends on the capability it claims to — calling real
// buildModelForCapability with the entry's low and high TermCapability and
// requiring the field to differ. This is what would have caught RE-12: a
// `m.mouseEnabled = false` mutation makes low and high produce the same
// (false) value, and the assertion fails.
func TestBuildModelForCapability_FieldsFollowCapability(t *testing.T) {
	for name, probe := range capabilityWiredFields {
		t.Run(name, func(t *testing.T) {
			// buildModelForCapability also calls ApplyColorProfile(cap.Profile)
			// (a process-global lipgloss side effect); the low/high pairs above
			// leave Profile at its zero value, which is termenv.TrueColor, not
			// Ascii — so without this restore every later test in the package
			// that renders plain text and expects no ANSI escapes starts
			// failing the moment this test runs first.
			prev := lipgloss.ColorProfile()
			t.Cleanup(func() { ApplyColorProfile(prev) })

			lowM := buildModelForCapability(&cli.Session{}, "/proj", Preferences{}, probe.low)
			highM := buildModelForCapability(&cli.Session{}, "/proj", Preferences{}, probe.high)
			lowVal := probe.get(lowM)
			highVal := probe.get(highM)
			assert.NotEqual(t, lowVal, highVal,
				"%s must actually depend on the detected capability (got %v for both low=%+v and high=%+v)",
				name, lowVal, probe.low, probe.high)
		})
	}
}
