// effect_wiring_test.go — the RE-23/24/25 wiring census (fix-e4a of W-E-tui).
//
// WHY THE TWO EXISTING CENSUS GATES DID NOT CATCH THESE THREE
//
// capability_wiring_test.go and keybindings_wiring_test.go both census a
// SUPPLY side: "which model fields does buildModelForCapability set from the
// detected capability" and "which keys does the keymap bind". Both walk one
// named function's body and both observe a VALUE that ends up on the model.
//
// RE-23/24/25 are all on the other side of that seam. Nothing about them is a
// field: notifyCmd, windowTitleCmd and renderPendingBody are already correct,
// already unit-tested, and already return the right thing. What went missing
// is the CONSUMER — the single production line that calls them. The capability
// census's own doc comment states the limit out loud ("What this does NOT
// catch: a field wired somewhere OTHER than buildModelForCapability"), and
// notifyEnabled's doc comment on the model struct even records that it
// "deliberately has NO entry" in that census. So the two gates were not
// bypassed; the defects simply live on an axis neither gate has ever looked
// at. Adding a third gate on the CONSUMER axis is the fix — three point tests
// would be the fourth patch, not a mechanism.
//
// WHAT THIS CENSUS ENFORCES
//
//  1. Producer discovery is automatic, not hand-listed.
//     TestTerminalEffectProducersMatchCensus walks every non-test .go file in
//     the package and collects every method on `model` whose body calls one of
//     bubbletea's terminal-writing Cmd constructors (teaEffectConstructors),
//     then requires that set to equal the census's producer set. A NEW escape
//     emitter — in a new file, under any name — is discovered here.
//  2. Call sites are censused, both directions.
//     TestEffectCallSitesMatchCensus walks the same files for every
//     `m.<producer>(…)` call and keys it "<enclosing func>/<producer>". That
//     set must EXACTLY equal effectWiringCensus's keys. Add a call to
//     notifyCmd from a fifth place, or move the title call out of Init, and
//     this goes red until the census is edited.
//  3. Every census entry must carry a working differential probe.
//     TestEffectCallSitesAreObservable drives the REAL enclosing production
//     function (Init / dispatchSend / Update / renderBody — no re-implemented
//     copy) and requires `on` to observe the effect and `off` not to. Deleting
//     the call site makes `on` false; removing the guard around it makes `off`
//     true. Both probes are mandatory — a nil either side fails the test, so a
//     new entry cannot be waved through by naming it.
//
// NOT ENFORCED (read this before trusting the table)
//
//   - A probe body that returns a hard-coded true/false observes nothing. The
//     differential (on must be true AND off must be false) makes the trivial
//     one-liner fail in one direction or the other, but a probe that drives
//     the wrong thing and happens to differ still passes. Same class as the
//     "reason column" holes CLAUDE.md documents for the debt tables: review
//     enforces it, the compiler cannot.
//   - teaEffectConstructors is a list of names from the fork, not derived from
//     it. A future escape-emitting constructor (tea.SetClipboard, say) added to
//     third_party/bubbletea and used here would be invisible to discovery
//     until its name is added. Deriving it would mean reaching into another
//     module's unexported renderer switch from this package's tests.
//   - Discovery matches `tea.X` textually on the selector, so a dot-import or
//     a locally aliased constructor would slip past it. The package imports
//     bubbletea as `tea` everywhere; a dot-import would break far more than
//     this gate.
package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/x6nux/yanshi/internal/cli"
)

// teaEffectConstructors are the bubbletea Cmd constructors whose Msg the
// renderer turns into terminal escape bytes. A model method that calls one of
// these is, by definition, a terminal-effect producer — see this file's
// header for why the list is written down rather than derived.
var teaEffectConstructors = []string{"SetWindowTitle", "Notify", "Bell"}

// effectRenderProducers are producers this census tracks that are NOT Cmd
// constructors and therefore cannot be auto-discovered by
// teaEffectConstructors: renderPendingBody composes the streaming assistant
// block as a plain string, so nothing about it reaches the terminal through a
// tea.Cmd. RE-25 is precisely that call site going untested, so it is listed
// explicitly and gets the same call-site + probe treatment as the Cmd
// producers.
var effectRenderProducers = []string{"renderPendingBody"}

// effectWiring is one census entry: a production call site plus a differential
// probe that drives the real enclosing function. Both probes are required.
type effectWiring struct {
	// why records what breaks in the product when this call site disappears.
	why string
	// on drives the enclosing production function in the state where the
	// effect MUST be observable. Deleting the call site makes it false.
	on func(t *testing.T) bool
	// off drives it in the state where the effect MUST NOT appear — the state
	// the guard around the call site exists to produce. Removing that guard
	// makes it true.
	off func(t *testing.T) bool
}

// effectWiringCensus is the authoritative table described in this file's
// header: one entry per "<enclosing production function>/<producer>" call
// site, keyed exactly as TestEffectCallSitesMatchCensus derives them from the
// AST.
var effectWiringCensus = map[string]effectWiring{
	"Init/windowTitleCmd": {
		why: "RE-24(a): without it the terminal title never leaves whatever the " +
			"previous program left behind — the idle state of W-E-05 is never entered.",
		on:  func(t *testing.T) bool { return probeInitTitle(t, true) },
		off: func(t *testing.T) bool { return probeInitTitle(t, false) },
	},
	"dispatchSend/windowTitleCmd": {
		why: "RE-24(b): without it the title never goes busy, so the '会话状态' half " +
			"of W-E-05's acceptance disappears and the title reads idle for the whole turn.",
		on:  func(t *testing.T) bool { return probeDispatchTitle(t, true) },
		off: func(t *testing.T) bool { return probeDispatchTitle(t, false) },
	},
	"Update/windowTitleCmd": {
		why: "RE-24(c) and (d): without it the title stays busy forever after a turn " +
			"ends; without the `&& !drained` guard it flips to idle on a queue hop that " +
			"immediately starts the next turn.",
		on:  func(t *testing.T) bool { return probeUpdateIdleTitle(t, nil) },
		off: func(t *testing.T) bool { return probeUpdateIdleTitle(t, []string{"queued"}) },
	},
	"Update/notifyCmd": {
		why: "RE-23: the single production call site of W-E-04. Without it the desktop " +
			"notification is silently unplugged and no test in the package notices.",
		on:  func(t *testing.T) bool { return probeUpdateNotify(t, true) },
		off: func(t *testing.T) bool { return probeUpdateNotify(t, false) },
	},
	"renderBody/renderPendingBody": {
		why: "RE-25: the single production call site of W-E-07. Reverting it to " +
			"pendingStyle.Render(m.pending) turns progressive markdown off entirely " +
			"while every pendingmarkdown_test.go case stays green.",
		on:  func(t *testing.T) bool { return probeRenderProgressive(t, true) },
		off: func(t *testing.T) bool { return probeRenderProgressive(t, false) },
	},
}

// ---- observation helpers ----

// cmdFuncName returns the runtime name of a tea.Cmd's underlying function.
// tea.SetWindowTitle/Notify/Bell each return a distinct closure, so the name
// identifies which constructor produced the Cmd WITHOUT running it — which
// matters because the batches under test also carry waitForEvent (blocks on a
// channel) and the fetchInitial* commands (real session I/O).
func cmdFuncName(cmd tea.Cmd) string {
	if cmd == nil {
		return ""
	}
	fn := runtime.FuncForPC(reflect.ValueOf(cmd).Pointer())
	if fn == nil {
		return ""
	}
	return fn.Name()
}

// cmdLeaves flattens a tea.Cmd tree into its leaves. Only the batch/sequence
// wrappers are executed (bubbletea's compactCmds closure just returns its
// []Cmd — no side effect); every other Cmd is returned unrun. A single-element
// tea.Batch is returned unwrapped by compactCmds, so the recursion has to key
// off the wrapper's function name rather than assuming the top node is a batch.
func cmdLeaves(cmd tea.Cmd) []tea.Cmd {
	if cmd == nil {
		return nil
	}
	if !strings.Contains(cmdFuncName(cmd), ".compactCmds[") {
		return []tea.Cmd{cmd}
	}
	rv := reflect.ValueOf(cmd())
	if rv.Kind() != reflect.Slice {
		return []tea.Cmd{cmd}
	}
	var out []tea.Cmd
	for i := 0; i < rv.Len(); i++ {
		child, ok := rv.Index(i).Interface().(tea.Cmd)
		if !ok {
			continue
		}
		out = append(out, cmdLeaves(child)...)
	}
	return out
}

// cmdHasEffect reports whether the Cmd tree contains a leaf produced by the
// named bubbletea constructor.
func cmdHasEffect(cmd tea.Cmd, ctor string) bool {
	for _, leaf := range cmdLeaves(cmd) {
		if strings.Contains(cmdFuncName(leaf), "."+ctor+".func") {
			return true
		}
	}
	return false
}

// windowTitlesIn returns the title strings the Cmd tree would push. Only the
// SetWindowTitle leaves are executed, and those are pure closures over the
// title text. setWindowTitleMsg is unexported in package tea, so the payload is
// read via reflect.Value.String() — the same technique title_test.go uses.
func windowTitlesIn(cmd tea.Cmd) []string {
	var out []string
	for _, leaf := range cmdLeaves(cmd) {
		if strings.Contains(cmdFuncName(leaf), ".SetWindowTitle.func") {
			out = append(out, reflect.ValueOf(leaf()).String())
		}
	}
	return out
}

// ---- probes: each drives the real production function, never a copy ----

// probeInitTitle runs the real model.Init and reports whether its batch pushes
// a window title. titleEnabled is the gate windowTitleCmd itself honours, so
// enabled=false is the "must not appear" side.
func probeInitTitle(t *testing.T, enabled bool) bool {
	t.Helper()
	m := newModel(&recordingSession{}, "/proj")
	m.titleEnabled = enabled
	return cmdHasEffect(m.Init(), "SetWindowTitle")
}

// probeDispatchTitle runs the real dispatchSend — the single seam every turn
// (manual submit and queue drain alike) passes through — and reports whether
// its batch pushes a window title.
func probeDispatchTitle(t *testing.T, enabled bool) bool {
	t.Helper()
	m := newModel(&recordingSession{}, "/proj")
	m.titleEnabled = enabled
	_, cmd := m.dispatchSend("hello", false)
	return cmdHasEffect(cmd, "SetWindowTitle")
}

// probeUpdateIdleTitle feeds a real "done" streamMsg to the real Update and
// reports whether the IDLE title specifically is pushed — not merely "some
// title", because on a queue hop dispatchSend pushes the BUSY title from
// inside the same batch. Distinguishing the two is what lets the queue-nonempty
// side pin the `&& !drained` guard: drop that guard and the idle title starts
// appearing alongside the busy one, flipping this probe's off side to true.
func probeUpdateIdleTitle(t *testing.T, queue []string) bool {
	t.Helper()
	m := newModel(&recordingSession{}, "/proj")
	m.titleEnabled = true
	idle := reflect.ValueOf(m.windowTitleCmd(false)()).String()
	m.msgQueue = queue
	_, cmd := m.Update(streamMsg{ev: cli.StreamEvent{Kind: "done"}})
	for _, title := range windowTitlesIn(cmd) {
		if title == idle {
			return true
		}
	}
	return false
}

// probeUpdateNotify feeds a real "done" streamMsg to the real Update and
// reports whether the batch carries W-E-04's OSC 9 notification.
//
// The off side is notifyEnabled=false rather than "queue non-empty" on
// purpose: a queue hop reaches drainQueue -> dispatchSend -> startTurn, which
// resets turnStart, so notifyCmd's long-task threshold already suppresses the
// notification even with the `&& !drained` guard removed. That guard is
// therefore belt-and-braces for the notify half and cannot be pinned from
// here; the title half of the same `if` pins it (see probeUpdateIdleTitle).
func probeUpdateNotify(t *testing.T, enabled bool) bool {
	t.Helper()
	m := newModel(&recordingSession{}, "/proj")
	m.titleEnabled = true
	m.notifyEnabled = enabled
	m.turnStart = time.Now().Add(-time.Hour)
	_, cmd := m.Update(streamMsg{ev: cli.StreamEvent{Kind: "done"}})
	return cmdHasEffect(cmd, "Notify")
}

// probeRenderProgressive runs the real renderBody over a model whose pending
// buffer holds markdown, and reports whether the body shows the RENDERED pass
// rather than the raw source. The "raw source absent" half uses the literal
// "**bold**" the chunk carried — an oracle independent of the code under test,
// so reverting renderBody's call to pendingStyle.Render(m.pending) fails the
// probe on that half alone.
//
// cached=false drops the markdown cache, which is exactly the pre-W-E-07
// state: renderPendingBody falls back to plain, the raw asterisks reappear,
// and the probe must report false.
func probeRenderProgressive(t *testing.T, cached bool) bool {
	t.Helper()
	m := newModel(nil, "/proj")
	m.width = 80
	m = m.applyEvent(cli.StreamEvent{Kind: "agent_chunk", Text: "**bold**"})
	if !cached {
		m.pendingRendered = ""
		m.pendingRenderedText = ""
	}
	body := m.renderBody()
	if strings.Contains(body, "**bold**") {
		return false
	}
	return strings.Contains(body, strings.TrimRight(m.renderPendingBody(), "\n"))
}

// ---- the gates ----

// prodFilesInPackage parses every non-test .go file in this package. The
// census walks source rather than reflection because a call site that has been
// DELETED leaves nothing behind for reflection to find — the whole point.
func prodFilesInPackage(t *testing.T) []*ast.File {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package sources: %v", err)
	}
	fset := token.NewFileSet()
	var out []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		t.Fatal("no non-test .go files found — the census would pass vacuously")
	}
	return out
}

// isModelMethod reports whether fd is a method on model (value or pointer).
func isModelMethod(fd *ast.FuncDecl) bool {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return false
	}
	typ := fd.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	id, ok := typ.(*ast.Ident)
	return ok && id.Name == "model"
}

// censusProducers splits the census keys into the producer names they name.
func censusProducers() map[string]bool {
	out := map[string]bool{}
	for key := range effectWiringCensus {
		if i := strings.IndexByte(key, '/'); i >= 0 {
			out[key[i+1:]] = true
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestTerminalEffectProducersMatchCensus discovers every model method that
// emits a terminal escape via one of teaEffectConstructors and requires the
// result to equal the Cmd-producer half of effectWiringCensus. A new escape
// emitter added anywhere in the package — any file, any name — lands here
// first, before its call site can go untested.
func TestTerminalEffectProducersMatchCensus(t *testing.T) {
	ctors := map[string]bool{}
	for _, name := range teaEffectConstructors {
		ctors[name] = true
	}

	found := map[string]bool{}
	for _, f := range prodFilesInPackage(t) {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || !isModelMethod(fd) || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "tea" || !ctors[sel.Sel.Name] {
					return true
				}
				found[fd.Name.Name] = true
				return true
			})
		}
	}

	want := map[string]bool{}
	for name := range censusProducers() {
		want[name] = true
	}
	for _, name := range effectRenderProducers {
		delete(want, name)
	}

	if got, exp := sortedKeys(found), sortedKeys(want); !reflect.DeepEqual(got, exp) {
		t.Fatalf("terminal-effect producers = %v, census names %v — every model method that "+
			"calls one of %v must have at least one effectWiringCensus entry (and every censused "+
			"Cmd producer must still be one)", got, exp, teaEffectConstructors)
	}
}

// TestEffectCallSitesMatchCensus is the consumer-axis gate: every production
// call of a censused producer, keyed "<enclosing func>/<producer>", must match
// effectWiringCensus's keys exactly in both directions. Adding a wiring point
// without a census entry goes red here; deleting one and leaving the entry
// behind goes red too (the dead-entry direction CLAUDE.md's debt tables use).
func TestEffectCallSitesMatchCensus(t *testing.T) {
	producers := censusProducers()

	found := map[string]bool{}
	for _, f := range prodFilesInPackage(t) {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !producers[sel.Sel.Name] {
					return true
				}
				// Receiver must be a plain identifier (m, mm, …) — a
				// selector like x.y.notifyCmd would be a different type.
				if _, ok := sel.X.(*ast.Ident); !ok {
					return true
				}
				found[fd.Name.Name+"/"+sel.Sel.Name] = true
				return true
			})
		}
	}

	want := map[string]bool{}
	for key := range effectWiringCensus {
		want[key] = true
	}
	if got, exp := sortedKeys(found), sortedKeys(want); !reflect.DeepEqual(got, exp) {
		t.Fatalf("production call sites = %v, census keys %v — add an effectWiringCensus entry "+
			"(with a why and BOTH probes) for every new wiring point, and delete the entry for "+
			"any call site that is gone", got, exp)
	}
}

// TestEffectCallSitesAreObservable runs each census entry's differential probe
// against the real production function. This is the half that turns the AST
// bookkeeping into a test: without it a new entry could be registered with no
// coverage at all.
func TestEffectCallSitesAreObservable(t *testing.T) {
	for key, w := range effectWiringCensus {
		t.Run(key, func(t *testing.T) {
			if w.why == "" || w.on == nil || w.off == nil {
				t.Fatalf("%s: census entry needs a why plus BOTH probes — a call site with only "+
					"one direction cannot tell 'wiring deleted' from 'guard removed'", key)
			}
			if !w.on(t) {
				t.Fatalf("%s: the effect is NOT observable through the real production function. %s", key, w.why)
			}
			if w.off(t) {
				t.Fatalf("%s: the effect IS observable in the state that must suppress it — the "+
					"guard around this call site is gone. %s", key, w.why)
			}
		})
	}
}
