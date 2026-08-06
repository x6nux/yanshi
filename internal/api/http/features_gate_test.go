package http

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/features"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	obslog "github.com/x6nux/yanshi/internal/observe/log"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/tools"
)

// newTestRegistry builds a registry with the production specs so the flags
// under test carry their real defaults.
func newTestRegistry(t *testing.T) *features.Registry {
	t.Helper()
	reg := features.NewRegistry(false)
	for _, spec := range features.DefaultSpecs() {
		reg.Register(spec)
	}
	return reg
}

// captureLogs redirects the default logger through the production redacting
// handler into a buffer. It has to be the production handler: the trace id is
// injected by that handler from the context, so a plain slog.TextHandler would
// show nothing regardless of how the flag is set, and the test would pass for
// the wrong reason.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(obslog.New(obslog.Config{Level: "info", Writer: &buf}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// runWSTurn drives one complete WS turn against a server and returns nothing;
// callers assert on whatever side channel they installed beforehand.
func runWSTurn(t *testing.T, s *Server, o *orchestrator.Orchestrator, models map[string]model.BaseChatModel) {
	t.Helper()
	s.ChatWS(o, models, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("hi")))
	for {
		if recvFrame(t, c).Type == "done" {
			break
		}
	}
}

func newFlagTestServer(t *testing.T, reg *features.Registry) (*Server, *orchestrator.Orchestrator, map[string]model.BaseChatModel) {
	t.Helper()
	fm := einollm.NewFakeModel([]string{"hello"}, nil)
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)
	s := New(Config{Token: "t", FeaturesReg: reg, PriceTab: map[string]einollm.ModelPricing{
		"fake-1": {InputPerM: 1, OutputPerM: 2},
	}})
	return s, o, map[string]model.BaseChatModel{"fake-1": fm}
}

// TestSlogTraceIDFlagGatesCorrelationIDs pins the first of two flags that were
// registered, listed by /features, toggleable by the user -- and read by
// nothing outside their own package tests. Only observe.otel_export had a real
// consumer, so "new features can be dark-launched" was true for one flag in
// three, and toggling either of the other two changed nothing at all.
//
// ledger: C4/OBS3#3 新功能可灰度
func TestSlogTraceIDFlagGatesCorrelationIDs(t *testing.T) {
	t.Run("on by default: ids reach the log", func(t *testing.T) {
		buf := captureLogs(t)
		reg := newTestRegistry(t)
		s, o, models := newFlagTestServer(t, reg)
		runWSTurn(t, s, o, models)
		if !strings.Contains(buf.String(), "trace_id") {
			t.Fatalf("no trace_id in the turn's log lines: %s", buf.String())
		}
	})

	t.Run("off: no ids anywhere in the turn's logs", func(t *testing.T) {
		buf := captureLogs(t)
		reg := newTestRegistry(t)
		require.NoError(t, reg.Set("observe.slog_trace_id", false))
		s, o, models := newFlagTestServer(t, reg)
		runWSTurn(t, s, o, models)
		for _, key := range []string{"trace_id", "session_id", "turn_id"} {
			if strings.Contains(buf.String(), key) {
				t.Errorf("%s present after the flag was turned off: %s", key, buf.String())
			}
		}
	})
}

// TestCostInStatusFlagGatesTheStatusFrame is the same defect for the second
// flag: turning cost reporting off left every cost field on the wire.
//
// ledger: C4/OBS3#3 新功能可灰度
func TestCostInStatusFlagGatesTheStatusFrame(t *testing.T) {
	run := func(t *testing.T, reg *features.Registry) proto.ServerFrame {
		t.Helper()
		fm := einollm.NewFakeModel([]string{"hello"}, nil)
		o, err := orchestrator.New(orchestrator.Config{Model: fm})
		require.NoError(t, err)
		s := New(Config{Token: "t", FeaturesReg: reg, PriceTab: map[string]einollm.ModelPricing{
			"fake-1": {InputPerM: 1, OutputPerM: 2},
		}})
		s.ChatWS(o, map[string]model.BaseChatModel{"fake-1": fm}, nil)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()
		c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
		defer c.Close()
		require.NoError(t, c.WriteJSON(proto.NewUserMessage("hi")))
		var status proto.ServerFrame
		for {
			f := recvFrame(t, c)
			if f.Type == "status" {
				status = f
			}
			if f.Type == "done" {
				break
			}
		}
		return status
	}

	t.Run("on by default: the cost fields are populated", func(t *testing.T) {
		st := run(t, newTestRegistry(t))
		if !st.CostKnown {
			t.Fatalf("cost_known false with a priced model and the flag on: %+v", st)
		}
	})

	t.Run("off: no cost reaches the client", func(t *testing.T) {
		reg := newTestRegistry(t)
		require.NoError(t, reg.Set("observe.cost_in_status", false))
		st := run(t, reg)
		if st.CostKnown || st.CostUSD != 0 {
			t.Fatalf("cost still on the wire after the flag was turned off: "+
				"cost_known=%v cost_usd=%v", st.CostKnown, st.CostUSD)
		}
	})
}

// TestNilRegistryFallsBackToRegisteredDefaults is the trap that makes the
// naive fix worse than no fix.
//
// Registry.Enabled returns false for a nil receiver -- correct for a flag
// whose default is off, and silently WRONG for observe.slog_trace_id, whose
// default is true. s.featuresReg is nil on every path that builds a Server
// without one (every existing test in this package, and any embedder that
// doesn't pass FeaturesReg), so gating on Enabled directly would have turned
// off a stable, default-on feature for those callers and left no trace of why.
//
// ledger: C4/OBS3#3 新功能可灰度
func TestNilRegistryFallsBackToRegisteredDefaults(t *testing.T) {
	var nilReg *features.Registry
	for _, tc := range []struct {
		key  string
		want bool
	}{
		{"observe.slog_trace_id", true},   // Stable, Default: true
		{"observe.cost_in_status", true},  // Beta, Default: true
		{"observe.otel_export", false},    // Experimental, Default: false
		{"observe.does_not_exist", false}, // unknown: safe default
	} {
		if got := nilReg.EnabledOrDefault(tc.key); got != tc.want {
			t.Errorf("nil registry: EnabledOrDefault(%q) = %v, want the registered default %v",
				tc.key, got, tc.want)
		}
	}

	// A real registry still wins over the default -- otherwise the helper
	// would be a constant and the flags would be unusable in the other
	// direction.
	reg := newTestRegistry(t)
	require.NoError(t, reg.Set("observe.slog_trace_id", false))
	if reg.EnabledOrDefault("observe.slog_trace_id") {
		t.Error("an explicit off was overridden by the registered default")
	}
}

// TestSlogTraceIDOffSurvivesTheOrchestratorBoundary is the half the subtests
// above could not see, and the reason the first fix for this was wrong.
//
// Declining to call WithIDs is not the same as switching correlation off. The
// orchestrator fills in a fresh trace and turn id for any turn arriving without
// them (ensureTurnIDs, which exists so the goal loop, ACP and headless entry
// points still emit correlated logs), so a bare context comes back correlated a
// few frames later. With the flag off an operator would then see ids on every
// line downstream of that boundary -- and ids with no session.id, which look
// joinable and are not.
//
// Asserting it needs a turn that actually logs past that boundary. The guard
// emits one line per authorized tool call, so a scripted fs_write is the
// cheapest real downstream log line available.
func TestSlogTraceIDOffSurvivesTheOrchestratorBoundary(t *testing.T) {
	buf := captureLogs(t)
	workdir := t.TempDir()

	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name:      "fs_write",
			Arguments: `{"path":"out.txt","content":"hello"}`,
		}},
	})
	step2 := schema.AssistantMessage("written", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2}, nil)

	fs := tools.NewFSTools(workdir)
	o, err := orchestrator.New(orchestrator.Config{
		Model: mdl,
		Tools: []orchestrator.BaseTool{fs.Write},
		Profile: guard.PermissionProfile{
			Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
			FS:    guard.FSPerm{Write: []string{filepath.Join(workdir, "**")}},
		},
	})
	require.NoError(t, err)

	reg := newTestRegistry(t)
	require.NoError(t, reg.Set("observe.slog_trace_id", false))
	s := New(Config{Token: "t", FeaturesReg: reg})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("hi")))
	for {
		if recvFrame(t, c).Type == "done" {
			break
		}
	}

	logs := buf.String()
	if !strings.Contains(logs, "tool") && !strings.Contains(logs, "turn") {
		t.Fatalf("no log line was produced at all, so the assertion below is vacuous:\n%s", logs)
	}
	for _, key := range []string{"trace_id", "session_id", "turn_id"} {
		if strings.Contains(logs, key) {
			t.Errorf("%s reappeared downstream of the orchestrator boundary "+
				"with the flag off:\n%s", key, logs)
		}
	}
}

// TestSSEClientIDsAreSanitizedBeforeTheyBecomeSpanAttributes is the wiring
// half of obslog.SanitizeID: the helper is only worth anything at the one
// ingress that reads an identifier off the wire.
func TestSSEClientIDsAreSanitizedBeforeTheyBecomeSpanAttributes(t *testing.T) {
	exp := recordSpans(t)

	fm := einollm.NewFakeModel([]string{"hello"}, nil)
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.Chat(o, map[string]model.BaseChatModel{"fake-1": fm}, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	huge := strings.Repeat("A", 5000)
	body := `{"message":"hi","thread_id":"` + huge + `","turn_id":"a b\nc"}`
	req, err := http.NewRequest("POST", ts.URL+"/api/v1/chat", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	span := turnSpan(t, exp)
	if got := spanAttr(span, "session.id"); len(got) > obslog.MaxIDLength {
		t.Errorf("session.id is %d chars: the client sets it, and it is repeated on "+
			"every log line and span attribute of the request", len(got))
	}
	if got := spanAttr(span, "turn.id"); strings.ContainsAny(got, " \n") {
		t.Errorf("turn.id %q kept whitespace: a newline forges a log line in text format", got)
	}
}

// costWriteRe matches a write to either cost field: "x.CostUSD =",
// "CostKnown:" in a struct literal, "{CostUSD:" in a one-line literal. The
// leading class excludes identifier characters so it cannot match a longer
// name that merely ends in CostUSD.
var costWriteRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])Cost(?:USD|Known)\s*[:=][^=]`)

// TestCostFlagCoversEverySurfaceThatShipsCost is the completeness half.
//
// The first version of the cost gate covered statusFrame on both transports
// and stopped there -- while cost also reaches the client through the session
// list (which is what /stats renders) and through session_restored. Turning
// cost reporting off left it visible in two surfaces out of four, which is the
// same "honoured on one surface only" failure the gate was written to avoid.
//
// The static half is deliberate: a runtime test can only cover the frames it
// happens to drive, and the defect was a surface nobody thought to drive. This
// walks every production assignment to the two cost fields and requires the
// ENCLOSING FUNCTION to consult the flag. Function scope, not a byte window:
// the natural way to write a loop is to read the flag once above it.
func TestCostFlagCoversEverySurfaceThatShipsCost(t *testing.T) {
	// Assignments that are NOT wire-bound, and must stay ungated: turning a
	// display off must not lose accounting. connSession.billingMeta feeds the
	// stored ledger, and handleRestoreSession restores in-memory state, so
	// switching the flag back on shows the full history.
	ungated := map[string]bool{
		"billingMeta":          true,
		"handleRestoreSession": true, // gated for the FRAME; cs.* assignment is not
	}

	fset := token.NewFileSet()
	pkg, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}

	gatedFuncs := map[string]bool{}
	assigners := map[string][]string{} // func name -> selector texts

	for _, p := range pkg {
		for path, af := range p.Files {
			ast.Inspect(af, func(n ast.Node) bool {
				fd, ok := n.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					return true
				}
				var buf strings.Builder
				if err := printer.Fprint(&buf, fset, fd.Body); err != nil {
					t.Fatal(err)
				}
				body := buf.String()
				name := fd.Name.Name
				if strings.Contains(body, `EnabledOrDefault("observe.cost_in_status")`) {
					gatedFuncs[name] = true
				}
				for _, line := range strings.Split(body, "\n") {
					l := strings.TrimSpace(line)
					// Any WRITE to CostUSD / CostKnown: a plain assignment, or a
					// struct-literal field. The boundary is "not an identifier
					// character", not "line start or dot" -- a one-line composite
					// literal puts "{CostUSD:" mid-line, and the narrower pattern
					// let exactly that shape through when it was probed.
					if costWriteRe.MatchString(l) {
						assigners[name] = append(assigners[name], filepath.Base(path)+": "+l)
					}
				}
				return true
			})
		}
	}

	if len(assigners) == 0 {
		t.Fatal("found no cost assignments at all: the scan is broken, not the code")
	}
	for fn, sites := range assigners {
		if ungated[fn] || gatedFuncs[fn] {
			continue
		}
		t.Errorf("%s ships cost without consulting observe.cost_in_status:\n  %s\n"+
			"either gate it or add it to the ungated list with a reason",
			fn, strings.Join(sites, "\n  "))
	}
}
