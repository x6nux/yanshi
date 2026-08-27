// Background offload for long-running tools (T3).
//
// # The failure this removes
//
// GuardedTool.Stream puts every tool under context.WithTimeout. When the
// deadline fires the tool's result is a deadline error and NOTHING ELSE: the
// forty minutes of test output the command had already produced go to the
// garbage collector along with the context. The model's only move is to run
// the whole suite again — usually with a larger timeout it cannot set, so it
// runs the same call and gets the same nothing.
//
// The port (QwenPaw tool_calls/_coordinator.py, _begin_offload) keeps the work
// running and hands the model a HANDLE at the deadline instead of an error:
// "moved to the background, keep working, you will be told when it finishes".
// The run finishes on its own schedule and its result is reinjected later.
//
// # Why the notification is not a tool result
//
// QwenPaw is explicit about this and it is not a style choice: its hint message
// (tool_calls/_hint.py) flattens the finished response into an ORDINARY
// assistant message with no ToolResultBlock, precisely so no orphan role=tool
// message reaches the wire. This repository has the same constraint for a
// sharper reason — ctxcompact.EnforceToolCallPairs runs a fixpoint over
// tool_call/tool_result pairing, and a tool result with no matching call is a
// message it cannot pair and providers reject outright. The offload
// acknowledgement IS a tool result (it answers the call that was made), but the
// later completion notice is a USER message appended at an iteration boundary,
// which is the same shape the loop guard's continuation nudge uses.
//
// # Scope
//
// Only tools that declare themselves backgroundable are eligible
// (BackgroundableTools). A backgrounded fs_read would be absurd: it is over
// before the deadline exists, and the handle would cost more tokens than the
// file.
package tools

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
)

// BackgroundHardLimit bounds how long a run may continue AFTER it was moved to
// the background, measured from the moment of offload.
//
// It exists because "moved to the background" must not mean "unbounded": the
// process would accumulate orphaned subprocesses across a long session, and
// Close would have nothing finite to wait for. Thirty minutes is chosen
// against the longest thing a coding agent legitimately starts — a full test
// suite or a release build — and is deliberately far above every tool's own
// foreground timeout, so the offload is a real reprieve rather than a slightly
// later failure.
const BackgroundHardLimit = 30 * time.Minute

// BackgroundCloseGrace bounds how long Close waits for cancelled runs to
// unwind before returning. Process shutdown must terminate; a subprocess that
// ignores its cancel cannot hold the binary open.
const BackgroundCloseGrace = 5 * time.Second

// BackgroundableTools names the tools whose calls may be moved to the
// background when their foreground timeout expires.
//
// The list is short and deliberately hand-picked rather than derived from
// "timeout > N": what makes a tool eligible is that its work has VALUE AFTER
// THE DEADLINE. A test run that needs twelve minutes still produces the answer
// at minute twelve. A web_fetch that has not answered in thirty seconds is not
// going to become useful at minute two, and holding a handle for it would only
// teach the model to collect handles.
//
// Each of these three also has the second required property: it is idempotent
// to OBSERVE. The model can be told "this is still running" without the
// statement changing anything.
func BackgroundableTools() []string {
	return []string{"shell_run", "run_tests", "task_gate_run"}
}

// backgroundable is the lookup form of BackgroundableTools, built once.
var backgroundable = func() map[string]bool {
	m := make(map[string]bool)
	for _, n := range BackgroundableTools() {
		m[n] = true
	}
	return m
}()

// IsBackgroundable reports whether name is in BackgroundableTools.
func IsBackgroundable(name string) bool { return backgroundable[name] }

// BackgroundState is the lifecycle state of one background run.
type BackgroundState string

// Background run states.
const (
	// BackgroundRunning means the tool is still executing.
	BackgroundRunning BackgroundState = "running"
	// BackgroundCompleted means the tool finished and Result holds its output.
	BackgroundCompleted BackgroundState = "completed"
	// BackgroundFailed means the tool reported an error; Error holds it and
	// Result holds whatever partial output arrived first.
	BackgroundFailed BackgroundState = "failed"
	// BackgroundCancelled means the run was cancelled — by the model, by the
	// hard limit, or by process shutdown.
	BackgroundCancelled BackgroundState = "cancelled"
)

// IsTerminal reports whether no further transition is possible.
func (s BackgroundState) IsTerminal() bool { return s != BackgroundRunning }

// BackgroundRun is the model-visible snapshot of one background run.
//
// Result is the tool's accumulated Result text, already passed through the
// spillover cap — a background run is exactly the kind that produces a
// megabyte of test output, and reinjecting that verbatim would undo the window
// saving the offload just bought.
type BackgroundRun struct {
	ID        string          `json:"id"`
	Tool      string          `json:"tool"`
	Args      string          `json:"args"`
	State     BackgroundState `json:"state"`
	StartedAt time.Time       `json:"started_at"`
	EndedAt   time.Time       `json:"ended_at,omitempty"`
	Result    string          `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// backgroundEntry is a run plus the machinery only the manager touches.
type backgroundEntry struct {
	run    BackgroundRun
	cancel context.CancelFunc
	done   chan struct{}
	// cancelRequested records that Cancel (or Close) already fired this run's
	// cancel. It lives here rather than on BackgroundRun because it is not a
	// state the model should see: between the request and the goroutine
	// unwinding, the run is still genuinely running, and a model told
	// "cancelled" at that moment would conclude a build had stopped while it
	// was still writing files. finish reads it to pick BackgroundCancelled
	// over BackgroundFailed, since a cancelled run's error is always the
	// cancellation and reporting that as a failure would invite a retry.
	cancelRequested bool
}

// BackgroundManager owns every offloaded tool run in the process.
//
// One manager per backend process, bound into every turn context by the
// orchestrator. It is NOT per-turn: the whole point is that a run outlives the
// turn that started it, and a per-turn manager would be cancelled at exactly
// the moment the offload was supposed to help.
//
// # Why not internal/task
//
// internal/task.Broker dispatches PROMPTS to agent workers and internal/shell's
// job manager owns persistent shell SESSIONS. Neither can hold "the second half
// of an in-flight GuardedTool.Stream": the broker's unit of work is a string
// prompt for an LLM, and the shell manager's is a process it launched itself.
// A backgrounded run_tests is neither — it is a half-consumed ToolChunk channel
// belonging to a tool the guard already authorized. Routing it through either
// would mean re-authorizing under a different identity, which is the kind of
// reuse that quietly widens a permission boundary.
type BackgroundManager struct {
	mu      sync.Mutex
	entries map[string]*backgroundEntry
	// active maps an idempotency key (tool + args) to the id of the RUNNING
	// entry holding it. Terminal runs are removed so a re-run after completion
	// is a fresh call rather than a permanent refusal.
	active map[string]string
	// notices holds finished runs whose completion has not yet been reinjected
	// into a conversation. DrainNotices empties it.
	notices []BackgroundRun
	seq     int
	closed  bool
	wg      sync.WaitGroup
}

// NewBackgroundManager returns an empty manager ready for use.
func NewBackgroundManager() *BackgroundManager {
	return &BackgroundManager{
		entries: make(map[string]*backgroundEntry),
		active:  make(map[string]string),
	}
}

// backgroundKey is the idempotency key: a call is "the same call" when the tool
// and the exact argument blob match.
//
// Byte equality on the raw JSON rather than a semantic comparison. The stricter
// form would need a per-tool notion of which arguments matter, which is a
// second schema to keep in step with the first; and the failure mode of being
// too strict here is one duplicated run, while being too loose would refuse a
// call the model legitimately meant to repeat.
func backgroundKey(tool, args string) string { return tool + "\x00" + args }

// Active returns the RUNNING background run for this exact call, if one exists.
//
// GuardedTool.Stream consults it before executing, which is what makes the
// offload idempotent: a model that re-issues `run_tests` while the previous
// one is still going is told the handle it already has, not given a second
// suite running against the same tree.
func (m *BackgroundManager) Active(tool, args string) (BackgroundRun, bool) {
	if m == nil {
		return BackgroundRun{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.active[backgroundKey(tool, args)]
	if !ok {
		return BackgroundRun{}, false
	}
	e, ok := m.entries[id]
	if !ok || e.run.State != BackgroundRunning {
		return BackgroundRun{}, false
	}
	return e.run, true
}

// Adopt registers a call that has just been moved to the background and returns
// its handle. cancel is the run's own cancel function; the manager calls it on
// Cancel and on Close.
//
// It returns nil when the manager is closed (process shutting down), in which
// case the caller must not offload: the run would never be waited for.
func (m *BackgroundManager) Adopt(tool, args string, cancel context.CancelFunc) *BackgroundHandle {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.seq++
	id := "bg-" + strconv.Itoa(m.seq)
	e := &backgroundEntry{
		run: BackgroundRun{
			ID: id, Tool: tool, Args: args,
			State: BackgroundRunning, StartedAt: time.Now(),
		},
		cancel: cancel,
		done:   make(chan struct{}),
	}
	m.entries[id] = e
	m.active[backgroundKey(tool, args)] = id
	m.wg.Add(1)
	return &BackgroundHandle{mgr: m, id: id}
}

// BackgroundHandle is the writer side of one adopted run. Only the goroutine
// that owns the tool's channel holds one.
type BackgroundHandle struct {
	mgr  *BackgroundManager
	id   string
	once sync.Once
}

// ID returns the run id the model was handed.
func (h *BackgroundHandle) ID() string {
	if h == nil {
		return ""
	}
	return h.id
}

// Finish records the run's outcome and queues a completion notice.
//
// Safe to call more than once; only the first call counts. That matters
// because the drain goroutine's normal exit and the hard-limit cancel can
// race, and a second Finish would otherwise overwrite a real result with the
// cancellation.
func (h *BackgroundHandle) Finish(result string, err error) {
	if h == nil {
		return
	}
	h.once.Do(func() { h.mgr.finish(h.id, result, err) })
}

// finish transitions an entry to its terminal state and enqueues its notice.
func (m *BackgroundManager) finish(id, result string, err error) {
	m.mu.Lock()
	e, ok := m.entries[id]
	if !ok || e.run.State != BackgroundRunning {
		m.mu.Unlock()
		return
	}
	e.run.EndedAt = time.Now()
	e.run.Result = result
	switch {
	case e.cancelRequested:
		e.run.State = BackgroundCancelled
	case err != nil:
		e.run.State = BackgroundFailed
		e.run.Error = err.Error()
	default:
		e.run.State = BackgroundCompleted
	}
	delete(m.active, backgroundKey(e.run.Tool, e.run.Args))
	m.notices = append(m.notices, e.run)
	close(e.done)
	m.mu.Unlock()
	m.wg.Done()
}

// Get returns a snapshot of one run.
func (m *BackgroundManager) Get(id string) (BackgroundRun, bool) {
	if m == nil {
		return BackgroundRun{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	if !ok {
		return BackgroundRun{}, false
	}
	return e.run, true
}

// List returns every run, newest first.
func (m *BackgroundManager) List() []BackgroundRun {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]BackgroundRun, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, e.run)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}

// Cancel stops a running background run. It returns false when the id is
// unknown or the run already reached a terminal state.
//
// The state does NOT flip to cancelled here — it flips when the drain
// goroutine actually unwinds and calls Finish, which is the only moment the
// process really stopped doing the work. Reporting cancelled before that would
// let the model conclude a build had stopped while it was still writing files.
func (m *BackgroundManager) Cancel(id string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	e, ok := m.entries[id]
	if !ok || e.run.State != BackgroundRunning {
		m.mu.Unlock()
		return false
	}
	e.cancelRequested = true
	cancel := e.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}

// DrainNotices returns and clears every completion notice not yet delivered.
//
// The orchestrator's notifier middleware calls it at each ReAct iteration
// boundary. Draining (rather than reading) is what keeps a finished run from
// being announced on every subsequent iteration, and the queue is manager-wide
// rather than per-turn on purpose: a run that finishes between turns must
// still be announced to the next turn, which is the common case for a
// twelve-minute test suite.
func (m *BackgroundManager) DrainNotices() []BackgroundRun {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.notices) == 0 {
		return nil
	}
	out := m.notices
	m.notices = nil
	return out
}

// Close cancels every running background run and waits, bounded by
// BackgroundCloseGrace, for their goroutines to unwind. After Close, Adopt
// refuses, so a tool call racing shutdown runs to its foreground deadline and
// fails there rather than being adopted by a manager nobody will wait for.
//
// It reports whether every run unwound within the grace period. A false return
// means at least one subprocess outlived the wait; the caller (the composition
// root's shutdown path) can log it. Returning rather than blocking forever is
// the deliberate choice: a wedged child must not stop the binary from exiting.
func (m *BackgroundManager) Close() bool {
	if m == nil {
		return true
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return true
	}
	m.closed = true
	cancels := make([]context.CancelFunc, 0, len(m.entries))
	for _, e := range m.entries {
		if e.run.State == BackgroundRunning {
			e.cancelRequested = true
			if e.cancel != nil {
				cancels = append(cancels, e.cancel)
			}
		}
	}
	m.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	waited := make(chan struct{})
	go func() { m.wg.Wait(); close(waited) }()
	select {
	case <-waited:
		return true
	case <-time.After(BackgroundCloseGrace):
		return false
	}
}

// backgroundManagerKey keys the manager in a tool-execution context.
type backgroundManagerKey struct{}

// WithBackgroundManager binds m into ctx. A nil manager leaves ctx unchanged,
// so "no manager wired" and "manager explicitly absent" are the same state:
// GuardedTool.Stream then keeps its plain WithTimeout behaviour and nothing
// is offloaded. Every reader must therefore handle ok=false.
func WithBackgroundManager(ctx context.Context, m *BackgroundManager) context.Context {
	if m == nil {
		return ctx
	}
	return context.WithValue(ctx, backgroundManagerKey{}, m)
}

// BackgroundManagerFromContext returns the bound manager, if any.
func BackgroundManagerFromContext(ctx context.Context) (*BackgroundManager, bool) {
	m, ok := ctx.Value(backgroundManagerKey{}).(*BackgroundManager)
	return m, ok && m != nil
}

// OffloadNotice renders the tool RESULT handed to the model at the moment of
// offload. It is a success result, not an error: the call did what it could and
// the work continues.
//
// The wording carries three instructions because all three were needed in
// QwenPaw: keep working, do NOT re-run this, you will be told. Dropping the
// middle one produces a model that immediately re-issues the call — which the
// idempotency check then refuses, costing a round trip to learn what this
// sentence could have said.
func OffloadNotice(id, tool string, after time.Duration) string {
	return fmt.Sprintf(
		"Tool %q exceeded its %s foreground budget and was moved to the background (id=%s). "+
			"It is STILL RUNNING. Continue with other work; do not re-run the same call. "+
			"You will be notified with the result when it finishes, and you can check on it "+
			"with background_list or background_result(id=%q).",
		tool, after, id, id)
}

// AlreadyRunningNotice renders the result for a call the model repeated while
// the previous one is still in the background.
func AlreadyRunningNotice(run BackgroundRun) string {
	return fmt.Sprintf(
		"This exact call is ALREADY running in the background (id=%s, started %s ago) and was "+
			"NOT started a second time. Wait for its notification, or check it with "+
			"background_result(id=%q).",
		run.ID, time.Since(run.StartedAt).Truncate(time.Second), run.ID)
}

// CompletionNotice renders the USER-role message reinjected into the
// conversation when a background run finishes.
//
// The <system-notification> envelope is QwenPaw's (tool_calls/_hint.py) and
// serves the same purpose here: the text arrives on the user turn, so without a
// marker the model reads a test log as something the human typed.
func CompletionNotice(run BackgroundRun) string {
	body := run.Result
	if run.Error != "" {
		if body != "" {
			body += "\n"
		}
		body += "error: " + run.Error
	}
	if body == "" {
		body = "(no output)"
	}
	return fmt.Sprintf(
		"<system-notification>\nBackground tool call %q (id=%s) finished with state=%s after %s. "+
			"Result below.\n</system-notification>\n%s",
		run.Tool, run.ID, run.State, run.duration(), body)
}

// duration renders how long the run took, for the completion notice.
func (r BackgroundRun) duration() time.Duration {
	if r.EndedAt.IsZero() {
		return 0
	}
	return r.EndedAt.Sub(r.StartedAt).Truncate(time.Second)
}
