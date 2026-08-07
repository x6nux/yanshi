package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// frameTypesNotRendered lists the ServerFrame types applyEvent deliberately has
// no branch for, with the reason.
//
// Read the worked example on internal/cli::streamEventNotCarried before adding
// an entry. The short version: a table like this enforces its mechanism, never
// its justification, so an entry whose reason quietly stops being true is
// indistinguishable from one that was always right. Both entries below are
// claims about REACHABILITY — the frame cannot arrive at this switch — which is
// the only kind of reason that does not rot when someone widens a profile or
// wires a new path. "The TUI shows it another way" is not that kind of reason,
// and that is exactly the one that expired.
var frameTypesNotRendered = map[string]string{
	"history_replaced": "SSE-only (only api/http/chat.go constructs it) and consumed in " +
		"internal/cli/ssebackend.go before a StreamEvent is ever built",
	"structured_result": "requires TurnOpts.OutputSchema, which the TUI never sets — " +
		"this is the headless/SDK path",
}

// TestEveryServerFrameTypeIsRenderedOrDeclared is the TYPE-level twin of
// internal/cli::TestToStreamEventCarriesEveryServerFrameField.
//
// The field-level gate asks "does the payload survive the hop"; this one asks
// "does anything downstream do something with this KIND of frame at all". Three
// separate defects in this repo lived in that gap — plan_update, task_update
// and subagent_event each reached the client and were dropped on the floor,
// each with passing tests on both sides of the hop that nothing joined. Fixing
// one only moved the wall one frame type downstream, which is why this is a
// gate and not a checklist item.
func TestEveryServerFrameTypeIsRenderedOrDeclared(t *testing.T) {
	emitted := serverFrameTypes(t)
	require.NotEmpty(t, emitted, "no ServerFrame constructors found: the scan is broken, not the code")
	handled := applyEventCases(t)
	require.NotEmpty(t, handled, "no cases found in applyEvent: the scan is broken, not the code")

	var missing []string
	for typ := range emitted {
		if handled[typ] {
			continue
		}
		if _, ok := frameTypesNotRendered[typ]; ok {
			continue
		}
		missing = append(missing, typ)
	}
	sort.Strings(missing)
	require.Emptyf(t, missing,
		"these ServerFrame types reach the client and applyEvent does nothing with them: %v\n"+
			"either add a case, or add an entry to frameTypesNotRendered explaining why the "+
			"frame cannot arrive here", missing)

	// Dead entries: a declared exemption for a type nothing constructs any more
	// is a permanent pre-authorisation for that name to come back unhandled.
	for typ := range frameTypesNotRendered {
		require.Truef(t, emitted[typ],
			"frameTypesNotRendered names %q but no proto constructor produces it", typ)
		require.Falsef(t, handled[typ],
			"%q is declared not-rendered but applyEvent has a case for it", typ)
	}
}

// serverFrameTypes returns every Type string set inside a function that returns
// a proto.ServerFrame. Reading the constructors rather than the struct's
// doc comment is the point: a frame type that exists only in prose cannot be
// sent, and one added without prose still can.
func serverFrameTypes(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile("../../proto/frame.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "frame.go", src, 0)
	require.NoError(t, err)

	// Type: SomeConst — resolve through the file's own const declarations.
	consts := map[string]string{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			if lit, ok := vs.Values[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if v, err := strconv.Unquote(lit.Value); err == nil {
					consts[vs.Names[0].Name] = v
				}
			}
		}
	}

	out := map[string]bool{}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil || !returnsServerFrame(fn) {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			if id, ok := kv.Key.(*ast.Ident); !ok || id.Name != "Type" {
				return true
			}
			switch v := kv.Value.(type) {
			case *ast.BasicLit:
				if s, err := strconv.Unquote(v.Value); err == nil {
					out[s] = true
				}
			case *ast.Ident:
				if s, ok := consts[v.Name]; ok {
					out[s] = true
				}
			}
			return true
		})
	}
	return out
}

func returnsServerFrame(fn *ast.FuncDecl) bool {
	if fn.Type.Results == nil {
		return false
	}
	for _, r := range fn.Type.Results.List {
		if id, ok := r.Type.(*ast.Ident); ok && id.Name == "ServerFrame" {
			return true
		}
	}
	return false
}

var caseLine = regexp.MustCompile(`^\tcase ((?:"[a-z_]+"(?:,\s*)?)+):`)

// applyEventCases returns the frame kinds applyEvent switches on. It reads the
// source rather than driving the model because a case that exists but is
// unreachable is a different defect, and this gate is about the vocabulary.
func applyEventCases(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile("model.go")
	require.NoError(t, err)
	lines := strings.Split(string(src), "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "func (m model) applyEvent(") {
			start = i
			break
		}
	}
	require.GreaterOrEqual(t, start, 0, "applyEvent not found in model.go: the scan is broken")
	out := map[string]bool{}
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "func ") {
			break
		}
		if m := caseLine.FindStringSubmatch(lines[i]); m != nil {
			for _, q := range regexp.MustCompile(`"([a-z_]+)"`).FindAllStringSubmatch(m[1], -1) {
				out[q[1]] = true
			}
		}
	}
	return out
}
