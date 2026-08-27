package loopguard

import (
	"strings"
	"testing"
	"time"
)

// fakeGate is a Gate whose verdict is scripted per iteration. Preferred over a
// mock framework per the repo convention: the whole behaviour is the answers
// slice, so a test reads as a table of iteration -> verdict.
type fakeGate struct {
	name     string
	priority int
	answers  []Result
	seen     []Observation
}

func (f *fakeGate) Name() string  { return f.name }
func (f *fakeGate) Priority() int { return f.priority }
func (f *fakeGate) Check(obs Observation) Result {
	f.seen = append(f.seen, obs)
	if obs.Iteration < len(f.answers) {
		return f.answers[obs.Iteration]
	}
	return Result{}
}

func TestActionString(t *testing.T) {
	cases := []struct {
		action Action
		want   string
	}{
		{ActionContinue, "continue"},
		{ActionModifyPrompt, "modify_prompt"},
		{ActionStop, "stop"},
		{Action(9), "action(9)"},
	}
	for _, tc := range cases {
		if got := tc.action.String(); got != tc.want {
			t.Errorf("Action(%d).String() = %q, want %q", tc.action, got, tc.want)
		}
	}
}

// TestHandlerZeroGatesContinues pins the ONE place this port deliberately
// inverts QwenPaw's default. Copying the reference (no gates -> TERMINATE)
// would stop every turn at iteration zero.
func TestHandlerZeroGatesContinues(t *testing.T) {
	if got := NewHandler().Evaluate(Observation{}); got.Action != ActionContinue {
		t.Fatalf("empty handler: got %v, want continue", got.Action)
	}
	var nilHandler *Handler
	if got := nilHandler.Evaluate(Observation{}); got.Action != ActionContinue {
		t.Fatalf("nil handler: got %v, want continue", got.Action)
	}
	if nilHandler.Len() != 0 || nilHandler.Names() != nil {
		t.Fatalf("nil handler must report zero gates")
	}
}

func TestHandlerDropsNilGates(t *testing.T) {
	h := NewHandler(nil, &fakeGate{name: "a", priority: 1}, nil)
	if h.Len() != 1 {
		t.Fatalf("Len = %d, want 1", h.Len())
	}
}

func TestHandlerSortsByPriority(t *testing.T) {
	h := NewHandler(
		&fakeGate{name: "late", priority: 30},
		&fakeGate{name: "early", priority: 5},
		&fakeGate{name: "mid", priority: 20},
	)
	want := []string{"early", "mid", "late"}
	got := h.Names()
	if len(got) != len(want) {
		t.Fatalf("Names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names = %v, want %v", got, want)
		}
	}
}

func TestHandlerEvaluateReduction(t *testing.T) {
	stop := Result{Action: ActionStop, Reason: "budget"}
	nudge := Result{Action: ActionModifyPrompt, Continuation: "try something else"}
	cont := Result{}

	cases := []struct {
		name      string
		gates     []Gate
		wantAct   Action
		wantGate  string
		wantExtra string
	}{
		{
			name:    "all continue",
			gates:   []Gate{&fakeGate{name: "a", priority: 1, answers: []Result{cont}}},
			wantAct: ActionContinue,
		},
		{
			name:     "single nudge",
			gates:    []Gate{&fakeGate{name: "a", priority: 1, answers: []Result{nudge}}},
			wantAct:  ActionModifyPrompt,
			wantGate: "a",
		},
		{
			// A stop from a LATER gate must beat a nudge from an earlier one:
			// spending another model call to deliver advice a stopped turn
			// will never act on is wasted budget.
			name: "stop beats earlier nudge",
			gates: []Gate{
				&fakeGate{name: "nudger", priority: 1, answers: []Result{nudge}},
				&fakeGate{name: "stopper", priority: 2, answers: []Result{stop}},
			},
			wantAct:  ActionStop,
			wantGate: "stopper",
		},
		{
			name: "first nudge wins over second",
			gates: []Gate{
				&fakeGate{name: "first", priority: 1, answers: []Result{nudge}},
				&fakeGate{name: "second", priority: 2, answers: []Result{nudge}},
			},
			wantAct:  ActionModifyPrompt,
			wantGate: "first",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NewHandler(tc.gates...).Evaluate(Observation{})
			if got.Action != tc.wantAct {
				t.Fatalf("action = %v, want %v", got.Action, tc.wantAct)
			}
			if tc.wantGate != "" && got.Gate != tc.wantGate {
				t.Fatalf("gate = %q, want %q", got.Gate, tc.wantGate)
			}
		})
	}
}

// TestHandlerStopShortCircuits proves a stop verdict prevents LATER gates from
// even being consulted. Without it a stop would still pay for every remaining
// gate's work on the iteration that ends the turn.
func TestHandlerStopShortCircuits(t *testing.T) {
	later := &fakeGate{name: "later", priority: 9}
	h := NewHandler(
		&fakeGate{name: "stopper", priority: 1, answers: []Result{{Action: ActionStop, Reason: "x"}}},
		later,
	)
	h.Evaluate(Observation{})
	if len(later.seen) != 0 {
		t.Fatalf("later gate was consulted %d times after a stop; want 0", len(later.seen))
	}
}

func TestNewStopError(t *testing.T) {
	if err := NewStopError(Result{Action: ActionContinue}); err != nil {
		t.Fatalf("continue must not produce an error, got %v", err)
	}
	if err := NewStopError(Result{Action: ActionModifyPrompt}); err != nil {
		t.Fatalf("nudge must not produce an error, got %v", err)
	}
	err := NewStopError(Result{Action: ActionStop, Gate: "deadline", Reason: "too slow"})
	if err == nil {
		t.Fatal("stop must produce an error")
	}
	var se *StopError
	if !asStopError(err, &se) {
		t.Fatalf("want *StopError, got %T", err)
	}
	if se.Gate != "deadline" || se.Reason != "too slow" {
		t.Fatalf("StopError = %+v", se)
	}
	if !strings.Contains(err.Error(), "deadline") || !strings.Contains(err.Error(), "too slow") {
		t.Fatalf("Error() = %q, must name the gate and the reason", err.Error())
	}
	unnamed := &StopError{Reason: "anonymous"}
	if !strings.Contains(unnamed.Error(), "anonymous") {
		t.Fatalf("Error() = %q", unnamed.Error())
	}
}

// asStopError is errors.As specialised, kept local so the test file needs no
// import purely for one assertion.
func asStopError(err error, target **StopError) bool {
	se, ok := err.(*StopError)
	if ok {
		*target = se
	}
	return ok
}

func TestHashArgs(t *testing.T) {
	cases := []struct {
		name  string
		a, b  string
		equal bool
	}{
		{"identical", `{"path":"a.go"}`, `{"path":"a.go"}`, true},
		{"different path", `{"path":"a.go"}`, `{"path":"b.go"}`, false},
		// The load-bearing case for L1: the SAME file at a different offset is
		// sequential progress through a long file, not a doom loop. It must
		// hash differently or reading any large file trips the detector.
		{"same file different offset", `{"path":"a.go","offset":0}`, `{"path":"a.go","offset":4000}`, false},
		{"empty vs empty", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HashArgs(tc.a) == HashArgs(tc.b); got != tc.equal {
				t.Fatalf("HashArgs(%q)==HashArgs(%q) = %v, want %v", tc.a, tc.b, got, tc.equal)
			}
		})
	}
	// Truncation is documented behaviour, not an accident: two blobs agreeing
	// on their first hashArgsLimit bytes collide by design.
	long := strings.Repeat("x", hashArgsLimit)
	if HashArgs(long+"A") != HashArgs(long+"B") {
		t.Fatal("args beyond hashArgsLimit must not affect the hash")
	}
	if len(HashArgs("anything")) != 16 {
		t.Fatalf("hash length = %d, want 16", len(HashArgs("anything")))
	}
}

func TestRepetitionSimilarity(t *testing.T) {
	call := func(n, h string) ToolCall { return ToolCall{Name: n, ArgsHash: h} }
	cases := []struct {
		name   string
		window []ToolCall
		want   float64
	}{
		{"empty", nil, 0},
		{"single", []ToolCall{call("a", "1")}, 0},
		{"all identical", []ToolCall{call("a", "1"), call("a", "1"), call("a", "1")}, 1},
		{"all distinct", []ToolCall{call("a", "1"), call("b", "2"), call("c", "3")}, 0},
		{"two of three", []ToolCall{call("a", "1"), call("a", "1"), call("b", "2")}, 0.5},
		// Same tool, different args: NOT repetition.
		{"same tool distinct args", []ToolCall{call("fs_read", "1"), call("fs_read", "2"), call("fs_read", "3")}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := repetitionSimilarity(tc.window); got != tc.want {
				t.Fatalf("similarity = %v, want %v", got, tc.want)
			}
		})
	}
}

// driveRepetition feeds one call per iteration and returns each iteration's
// verdict.
func driveRepetition(g *RepetitionGate, calls []ToolCall) []Result {
	out := make([]Result, 0, len(calls))
	for i, c := range calls {
		out = append(out, g.Check(Observation{Iteration: i, Calls: []ToolCall{c}}))
	}
	return out
}

func TestRepetitionGateEscalates(t *testing.T) {
	same := ToolCall{Name: "fs_read", ArgsHash: "h1"}
	g := NewRepetitionGate(RepetitionConfig{})
	res := driveRepetition(g, []ToolCall{same, same, same, same})

	// Window is 3, so nothing can fire before the third call.
	if res[0].Action != ActionContinue || res[1].Action != ActionContinue {
		t.Fatalf("first two calls must pass: %v, %v", res[0].Action, res[1].Action)
	}
	if res[2].Action != ActionModifyPrompt {
		t.Fatalf("3rd identical call: got %v, want modify_prompt", res[2].Action)
	}
	if !strings.Contains(res[2].Continuation, "fs_read") {
		t.Fatalf("nudge must name the tool, got %q", res[2].Continuation)
	}
	if res[3].Action != ActionStop {
		t.Fatalf("4th identical call: got %v, want stop", res[3].Action)
	}
	if !strings.Contains(res[3].Reason, "fs_read") {
		t.Fatalf("stop reason must name the tool, got %q", res[3].Reason)
	}
}

// TestRepetitionGateDistinctArgsNeverFires is the mutation-resistant half of
// L1: it is the assertion that fails if HashArgs stops hashing arguments (e.g.
// someone "simplifies" the signature to the tool name alone). Reading a long
// file page by page is normal work.
func TestRepetitionGateDistinctArgsNeverFires(t *testing.T) {
	g := NewRepetitionGate(RepetitionConfig{})
	calls := []ToolCall{
		{Name: "fs_read", ArgsHash: HashArgs(`{"path":"big.go","offset":0}`)},
		{Name: "fs_read", ArgsHash: HashArgs(`{"path":"big.go","offset":2000}`)},
		{Name: "fs_read", ArgsHash: HashArgs(`{"path":"big.go","offset":4000}`)},
		{Name: "fs_read", ArgsHash: HashArgs(`{"path":"big.go","offset":6000}`)},
		{Name: "fs_read", ArgsHash: HashArgs(`{"path":"big.go","offset":8000}`)},
	}
	for i, r := range driveRepetition(g, calls) {
		if r.Action != ActionContinue {
			t.Fatalf("iteration %d: got %v, want continue (distinct offsets are progress)", i, r.Action)
		}
	}
}

// TestRepetitionGateResetsOnProgress: a model that takes the nudge and does
// something different must not be stopped later for that earlier stretch.
func TestRepetitionGateResetsOnProgress(t *testing.T) {
	same := ToolCall{Name: "fs_read", ArgsHash: "h1"}
	other := ToolCall{Name: "shell_run", ArgsHash: "h2"}
	g := NewRepetitionGate(RepetitionConfig{})
	res := driveRepetition(g, []ToolCall{same, same, same, other, same, same})
	if res[2].Action != ActionModifyPrompt {
		t.Fatalf("expected nudge at index 2, got %v", res[2].Action)
	}
	for i := 3; i < len(res); i++ {
		if res[i].Action == ActionStop {
			t.Fatalf("iteration %d stopped after the pattern was broken", i)
		}
	}
}

func TestRepetitionGateConfigClamps(t *testing.T) {
	cases := []struct {
		name       string
		cfg        RepetitionConfig
		wantWindow int
	}{
		{"zero selects default", RepetitionConfig{}, DefaultRepetitionWindow},
		{"one clamps to two", RepetitionConfig{Window: 1}, 2},
		{"negative selects default", RepetitionConfig{Window: -5}, DefaultRepetitionWindow},
		{"explicit honoured", RepetitionConfig{Window: 6}, 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if g := NewRepetitionGate(tc.cfg); g.window != tc.wantWindow {
				t.Fatalf("window = %d, want %d", g.window, tc.wantWindow)
			}
		})
	}
	if g := NewRepetitionGate(RepetitionConfig{}); g.threshold != DefaultRepetitionThreshold {
		t.Fatalf("threshold = %v, want %v", g.threshold, DefaultRepetitionThreshold)
	}
	if g := NewRepetitionGate(RepetitionConfig{}); len(g.stages) != 2 {
		t.Fatalf("stages = %d, want the 2 QwenPaw defaults", len(g.stages))
	}
}

func TestRepetitionGateCustomStages(t *testing.T) {
	same := ToolCall{Name: "x", ArgsHash: "h"}
	// Window 2 with a stop at 2 means the second identical call ends the turn.
	g := NewRepetitionGate(RepetitionConfig{
		Window: 2,
		Stages: []RepetitionStage{{After: 2, Stop: true, Prompt: "custom stop"}},
	})
	res := driveRepetition(g, []ToolCall{same, same})
	if res[1].Action != ActionStop || res[1].Reason != "custom stop" {
		t.Fatalf("got %v/%q, want stop/custom stop", res[1].Action, res[1].Reason)
	}
}

// TestRepetitionGateStagesAboveHitsBypass covers the branch where repetition is
// detected but no stage has been reached yet (window smaller than the first
// stage threshold).
func TestRepetitionGateStagesAboveHitsBypass(t *testing.T) {
	same := ToolCall{Name: "x", ArgsHash: "h"}
	g := NewRepetitionGate(RepetitionConfig{Window: 2, Stages: []RepetitionStage{{After: 99}}})
	for i, r := range driveRepetition(g, []ToolCall{same, same, same}) {
		if r.Action != ActionContinue {
			t.Fatalf("iteration %d: got %v, want continue (stage never reached)", i, r.Action)
		}
	}
}

func TestRepetitionGateIdentity(t *testing.T) {
	g := NewRepetitionGate(RepetitionConfig{})
	if g.Name() != "repetition" {
		t.Fatalf("Name = %q", g.Name())
	}
	if g.Priority() != 5 {
		t.Fatalf("Priority = %d, want 5 (QwenPaw DoomLoopGate)", g.Priority())
	}
}

// TestRepetitionGateHistoryBounded proves the sliding window does not grow
// without bound over a long turn.
func TestRepetitionGateHistoryBounded(t *testing.T) {
	g := NewRepetitionGate(RepetitionConfig{Window: 3})
	for i := 0; i < 500; i++ {
		g.Check(Observation{Iteration: i, Calls: []ToolCall{{Name: "t", ArgsHash: string(rune('a' + i%26))}}})
	}
	if len(g.history) > 6 {
		t.Fatalf("history len = %d, want <= window*2 = 6", len(g.history))
	}
}

func TestTokenBudgetGate(t *testing.T) {
	if NewTokenBudgetGate(0) != nil {
		t.Fatal("zero budget must produce no gate")
	}
	if NewTokenBudgetGate(-1) != nil {
		t.Fatal("negative budget must produce no gate")
	}
	g := NewTokenBudgetGate(1000)
	if g.Name() != "token-budget" || g.Priority() != 20 {
		t.Fatalf("identity = %q/%d", g.Name(), g.Priority())
	}
}

// TestTokenBudgetGateCountsPromptGrowth is the assertion that distinguishes a
// correct accumulator from the naive one. Providers report the prompt count
// CUMULATIVELY, so summing raw prompt counts over-counts a long turn; only the
// growth is new spend.
func TestTokenBudgetGateCountsPromptGrowth(t *testing.T) {
	g := NewTokenBudgetGate(100000)
	// Three calls whose prompt grows 1000 -> 1500 -> 2000 and which emit 100
	// completion tokens each. New spend is 2000 prompt + 300 completion.
	obs := []Observation{
		{Iteration: 0, PromptTokens: 1000, CompletionTokens: 100},
		{Iteration: 1, PromptTokens: 1500, CompletionTokens: 100},
		{Iteration: 2, PromptTokens: 2000, CompletionTokens: 100},
	}
	for _, o := range obs {
		if r := g.Check(o); r.Action != ActionContinue {
			t.Fatalf("unexpected %v under a 100k budget", r.Action)
		}
	}
	// Naive summing would give 1000+1500+2000+300 = 4800.
	if got := g.Used(); got != 2300 {
		t.Fatalf("Used = %d, want 2300 (prompt growth 2000 + completion 300)", got)
	}
}

func TestTokenBudgetGateStops(t *testing.T) {
	g := NewTokenBudgetGate(500)
	if r := g.Check(Observation{PromptTokens: 400, CompletionTokens: 50}); r.Action != ActionContinue {
		t.Fatalf("450 of 500: got %v, want continue", r.Action)
	}
	r := g.Check(Observation{Iteration: 1, PromptTokens: 460, CompletionTokens: 50})
	if r.Action != ActionStop {
		t.Fatalf("560 of 500: got %v, want stop", r.Action)
	}
	if !strings.Contains(r.Reason, "500") {
		t.Fatalf("reason must state the limit, got %q", r.Reason)
	}
}

// TestTokenBudgetGateIgnoresMissingUsage: providers that omit usage (FakeModel,
// some streaming paths) must not be treated as zero-cost resets of lastPrompt,
// which would make the NEXT real report count as growth from zero all over
// again.
func TestTokenBudgetGateIgnoresMissingUsage(t *testing.T) {
	g := NewTokenBudgetGate(100000)
	g.Check(Observation{Iteration: 0, PromptTokens: 5000, CompletionTokens: 10})
	g.Check(Observation{Iteration: 1}) // provider reported nothing
	g.Check(Observation{Iteration: 2, PromptTokens: 5200, CompletionTokens: 10})
	if got := g.Used(); got != 5220 {
		t.Fatalf("Used = %d, want 5220 (5000 + 200 growth + 20 completion)", got)
	}
}

func TestTokenBudgetGateNilSafe(t *testing.T) {
	var g *TokenBudgetGate
	if r := g.Check(Observation{PromptTokens: 1 << 30}); r.Action != ActionContinue {
		t.Fatalf("nil gate must continue, got %v", r.Action)
	}
	if g.Used() != 0 {
		t.Fatal("nil gate Used must be 0")
	}
}

func TestDeadlineGate(t *testing.T) {
	if NewDeadlineGate(0) != nil || NewDeadlineGate(-time.Second) != nil {
		t.Fatal("non-positive limit must produce no gate")
	}
	g := NewDeadlineGate(30 * time.Second)
	if g.Name() != "deadline" || g.Priority() != 30 {
		t.Fatalf("identity = %q/%d", g.Name(), g.Priority())
	}
	cases := []struct {
		elapsed time.Duration
		want    Action
	}{
		{0, ActionContinue},
		{29 * time.Second, ActionContinue},
		{30 * time.Second, ActionStop},
		{5 * time.Minute, ActionStop},
	}
	for _, tc := range cases {
		if got := g.Check(Observation{Elapsed: tc.elapsed}); got.Action != tc.want {
			t.Fatalf("elapsed %s: got %v, want %v", tc.elapsed, got.Action, tc.want)
		}
	}
	var nilGate *DeadlineGate
	if got := nilGate.Check(Observation{Elapsed: time.Hour}); got.Action != ActionContinue {
		t.Fatalf("nil deadline gate must continue")
	}
}

// TestDeadlineGateReasonExplainsBoundaryChecking keeps the "why it is soft"
// explanation attached to the message the user actually sees. Someone whose
// turn ran 40s past a 30s limit will otherwise file it as a bug.
func TestDeadlineGateReasonExplainsBoundaryChecking(t *testing.T) {
	r := NewDeadlineGate(time.Second).Check(Observation{Elapsed: 90 * time.Second})
	if !strings.Contains(r.Reason, "boundary") {
		t.Fatalf("reason = %q, must explain boundary checking", r.Reason)
	}
}

func TestToolBudgetNilForNoLimits(t *testing.T) {
	cases := []struct {
		name string
		cfg  ToolBudgetConfig
		nil_ bool
	}{
		{"empty", ToolBudgetConfig{}, true},
		{"zero limits only", ToolBudgetConfig{PerTool: map[string]int{"a": 0}}, true},
		{"blank name only", ToolBudgetConfig{PerTool: map[string]int{"": 5}}, true},
		{"negative total only", ToolBudgetConfig{MaxTotal: -3}, true},
		{"real per-tool", ToolBudgetConfig{PerTool: map[string]int{"a": 1}}, false},
		{"real total", ToolBudgetConfig{MaxTotal: 1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NewToolBudget(tc.cfg)
			if (got == nil) != tc.nil_ {
				t.Fatalf("nil = %v, want %v", got == nil, tc.nil_)
			}
		})
	}
}

func TestToolBudgetPerTool(t *testing.T) {
	b := NewToolBudget(ToolBudgetConfig{PerTool: map[string]int{"shell_run": 2}})
	for i := 0; i < 2; i++ {
		if ok, _ := b.Consume("shell_run"); !ok {
			t.Fatalf("call %d must be allowed under a budget of 2", i+1)
		}
	}
	ok, refusal := b.Consume("shell_run")
	if ok {
		t.Fatal("3rd call must be refused")
	}
	if !strings.Contains(refusal, "shell_run") || !strings.Contains(refusal, "2") {
		t.Fatalf("refusal must name the tool and the limit, got %q", refusal)
	}
	// An unbudgeted tool is unaffected.
	if ok, _ := b.Consume("fs_read"); !ok {
		t.Fatal("unbudgeted tool must remain allowed")
	}
	// Refused calls do not inflate the counter.
	b.Consume("shell_run")
	b.Consume("shell_run")
	if got := b.Used("shell_run"); got != 2 {
		t.Fatalf("Used = %d, want 2 (refusals must not count)", got)
	}
}

func TestToolBudgetTotal(t *testing.T) {
	b := NewToolBudget(ToolBudgetConfig{MaxTotal: 3})
	for _, name := range []string{"a", "b", "c"} {
		if ok, _ := b.Consume(name); !ok {
			t.Fatalf("%s must be allowed", name)
		}
	}
	ok, refusal := b.Consume("d")
	if ok {
		t.Fatal("4th call must be refused by the total cap")
	}
	if !strings.Contains(refusal, "3") || !strings.Contains(refusal, "d") {
		t.Fatalf("refusal = %q", refusal)
	}
	if b.Total() != 3 {
		t.Fatalf("Total = %d, want 3", b.Total())
	}
}

// TestToolBudgetPerToolBeatsTotal: when both would refuse, the message must
// name the specific tool's limit — that is the actionable one.
func TestToolBudgetPerToolBeatsTotal(t *testing.T) {
	b := NewToolBudget(ToolBudgetConfig{PerTool: map[string]int{"x": 1}, MaxTotal: 1})
	b.Consume("x")
	_, refusal := b.Consume("x")
	if !strings.Contains(refusal, "per-turn budget") {
		t.Fatalf("refusal = %q, want the per-tool message", refusal)
	}
}

func TestToolBudgetRefusalIsModelFacing(t *testing.T) {
	b := NewToolBudget(ToolBudgetConfig{PerTool: map[string]int{"shell_run": 1}})
	b.Consume("shell_run")
	_, refusal := b.Consume("shell_run")
	// The refusal is a TOOL RESULT the model reads, so it has to tell the
	// model what to do instead. A bare "denied" produces an immediate retry.
	if !strings.Contains(refusal, "Do not retry") {
		t.Fatalf("refusal must instruct the model not to retry, got %q", refusal)
	}
}

func TestToolBudgetNilSafe(t *testing.T) {
	var b *ToolBudget
	if ok, refusal := b.Consume("anything"); !ok || refusal != "" {
		t.Fatal("nil budget must allow everything")
	}
	if b.Used("x") != 0 || b.Total() != 0 || b.Limits() != nil {
		t.Fatal("nil budget accessors must be zero-valued")
	}
	if b.Describe() != "no tool budget" {
		t.Fatalf("Describe = %q", b.Describe())
	}
}

func TestToolBudgetDescribeIsStable(t *testing.T) {
	b := NewToolBudget(ToolBudgetConfig{
		PerTool:  map[string]int{"shell_run": 5, "fs_write": 3, "agent_spawn": 1},
		MaxTotal: 40,
	})
	want := "total<=40 agent_spawn<=1 fs_write<=3 shell_run<=5"
	for i := 0; i < 5; i++ {
		if got := b.Describe(); got != want {
			t.Fatalf("Describe = %q, want %q (map iteration must be sorted away)", got, want)
		}
	}
	limits := b.Limits()
	if len(limits) != 3 || limits["shell_run"] != 5 {
		t.Fatalf("Limits = %v", limits)
	}
}

func TestToolBudgetConcurrent(t *testing.T) {
	b := NewToolBudget(ToolBudgetConfig{MaxTotal: 50})
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 20; j++ {
				b.Consume("t")
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if got := b.Total(); got != 50 {
		t.Fatalf("Total = %d, want exactly 50 (the cap held under concurrency)", got)
	}
}
