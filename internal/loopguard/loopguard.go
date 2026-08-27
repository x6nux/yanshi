// Package loopguard is the pluggable stop-condition framework for yanshi's
// agent loop.
//
// # Why a package and not another branch in the turn loop
//
// Before this package the only thing that could end a runaway turn was the ADK
// iteration cap, and every additional condition anyone wanted ("stop after N
// shell_run calls", "stop after 10 minutes", "stop once this turn has burnt
// 200k tokens") had to be soldered into the WS retry loop, which is already
// the longest function in the transport. A stop condition is policy; the loop
// is mechanism, and mixing them means every new policy edits code that has
// nothing to do with it.
//
// So conditions live here as Gate implementations and the loop only asks a
// Handler what to do next. The structure is taken from QwenPaw's
// src/qwenpaw/loop/gates (StopGate / StopAction / StopHandlerResult /
// StopHandler): gates carry a name and a priority, are evaluated lowest
// priority first, and answer with one of three actions.
//
// # The one place the semantics deliberately differ from the reference
//
// QwenPaw's StopHandler returns TERMINATE when it holds no gates or when every
// gate bypasses. That is correct there because its handler is asked "the model
// stopped emitting — should the loop keep going?", where the safe default is
// to let the turn end.
//
// Here the question is inverted. yanshi's ReAct loop advances on its own and
// Handler.Evaluate is asked "may this iteration proceed?". Copying the
// reference default would stop every turn at iteration zero, so the zero-gate
// and all-bypass answers are both ActionContinue. This is the kind of detail
// that survives a port only if it is written down, hence this paragraph.
//
// # State lives in the gates, gates live in the turn
//
// The reference isolates per-session state inside each gate via a session-id
// keyed map (LoopGate). yanshi has no session-id context variable reaching the
// ADK middleware layer, and a process-wide map keyed by a string is exactly
// how one leaked turn's counters end up throttling the next user's turn. So a
// Handler and its gates are built PER TURN and thrown away with it; nothing in
// this package is shared across turns and nothing needs a mutex for
// cross-turn isolation. Concurrency within one turn is still real (the ADK may
// invoke tool wrappers off the main goroutine), so Handler and the stateful
// gates guard their own fields.
package loopguard

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Action is a gate's verdict for one loop iteration.
type Action uint8

const (
	// ActionContinue means the gate has no opinion; the iteration proceeds.
	// It is the zero value so a gate that forgets to set an action cannot
	// accidentally halt a turn.
	ActionContinue Action = iota
	// ActionModifyPrompt means the loop should keep going but must first
	// inject Result.Continuation as an extra user message, steering the model
	// away from whatever the gate objected to.
	ActionModifyPrompt
	// ActionStop means the turn must end now, with Result.Reason explaining
	// why.
	ActionStop
)

// String renders the action for logs and test failures.
func (a Action) String() string {
	switch a {
	case ActionContinue:
		return "continue"
	case ActionModifyPrompt:
		return "modify_prompt"
	case ActionStop:
		return "stop"
	default:
		return fmt.Sprintf("action(%d)", uint8(a))
	}
}

// ToolCall is one observed tool invocation, reduced to the two things every
// stop condition in this package cares about.
//
// ArgsHash rather than the raw arguments: the arguments of a single fs_write
// can be a whole file, and a gate that keeps a sliding window of them keeps a
// sliding window of file contents in memory for the length of the turn. Hash
// via HashArgs so that identical arguments compare equal without retaining
// them.
type ToolCall struct {
	Name     string
	ArgsHash string
}

// Observation is what a gate sees at one iteration boundary.
//
// Token counts are the LATEST model call's, not a running total: the provider
// reports cumulative-per-call figures, and turning them into a turn total is
// TokenBudgetGate's job, not the caller's. Calls are the tool invocations that
// happened since the previous observation, in invocation order.
type Observation struct {
	// Iteration is the zero-based index of the iteration about to run.
	Iteration int
	// Elapsed is the wall-clock time since the turn started.
	Elapsed time.Duration
	// PromptTokens is the prompt token count reported by the most recent model
	// call, or 0 when the provider reported none.
	PromptTokens int
	// CompletionTokens is the completion token count reported by the most
	// recent model call, or 0 when the provider reported none.
	CompletionTokens int
	// Calls are the tool invocations observed since the previous Observation.
	Calls []ToolCall
}

// Result is a gate's answer for one iteration.
type Result struct {
	// Action is the verdict. The zero value is ActionContinue.
	Action Action
	// Reason explains an ActionStop (and is carried on ActionModifyPrompt for
	// logs). It reaches the user, so write it for a user.
	Reason string
	// Continuation is the user message injected on ActionModifyPrompt. Empty
	// on any other action.
	Continuation string
	// Gate is the name of the gate that produced a non-continue action. Filled
	// by Handler.Evaluate, not by the gate itself.
	Gate string
}

// Gate is one stop condition.
//
// Check is called once per iteration, in priority order, and must be cheap:
// it sits directly in front of every model call. A gate that needs to see tool
// invocations reads Observation.Calls rather than reaching into the agent
// state, which is what keeps this package free of any eino or yanshi
// dependency.
type Gate interface {
	// Name is the gate's stable identifier, used in stop reasons and tests.
	Name() string
	// Priority orders evaluation; lower runs first. The convention copied from
	// the reference: repetition 5, token budget 20, deadline 30.
	Priority() int
	// Check evaluates the condition for one iteration.
	Check(obs Observation) Result
}

// Handler evaluates an ordered set of gates.
//
// Evaluation is not short-circuit-on-first-opinion in one respect worth
// stating: an ActionStop from ANY gate wins immediately, even if an earlier
// (lower priority) gate asked for ActionModifyPrompt. Stopping is the
// stronger, safer verdict, and a turn that is over budget should not spend
// another model call delivering a nudge it will never act on.
type Handler struct {
	mu    sync.Mutex
	gates []Gate
}

// NewHandler builds a Handler over gates, sorted by priority. Nil gates are
// dropped so a caller can build the slice with conditional entries
// (`append(g, maybeNil())`) without guarding each one.
func NewHandler(gates ...Gate) *Handler {
	h := &Handler{}
	for _, g := range gates {
		if g != nil {
			h.gates = append(h.gates, g)
		}
	}
	sort.SliceStable(h.gates, func(i, j int) bool {
		return h.gates[i].Priority() < h.gates[j].Priority()
	})
	return h
}

// Len reports how many gates are registered. Used by callers that skip the
// whole monitoring path when nothing is configured.
func (h *Handler) Len() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.gates)
}

// Names lists the registered gate names in evaluation order.
func (h *Handler) Names() []string {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.gates))
	for _, g := range h.gates {
		out = append(out, g.Name())
	}
	return out
}

// Evaluate runs every gate for one iteration and reduces their answers to a
// single verdict.
//
// A nil Handler and an empty gate set both answer ActionContinue — see the
// package doc for why this is the opposite of the reference implementation's
// default.
func (h *Handler) Evaluate(obs Observation) Result {
	if h == nil {
		return Result{}
	}
	h.mu.Lock()
	gates := make([]Gate, len(h.gates))
	copy(gates, h.gates)
	h.mu.Unlock()

	var nudge Result
	for _, g := range gates {
		res := g.Check(obs)
		switch res.Action {
		case ActionStop:
			res.Gate = g.Name()
			return res
		case ActionModifyPrompt:
			if nudge.Action == ActionContinue {
				res.Gate = g.Name()
				nudge = res
			}
		}
	}
	return nudge
}

// StopError is the error a turn ends with when a gate returns ActionStop.
//
// It is an error rather than a synthetic "you are out of budget, summarise and
// stop" user message on purpose. That alternative costs one more model call —
// paid out of the very budget that just ran out — and the model is free to
// call another tool instead of summarising, so the hard stop would not be
// hard. An error ends the run immediately at an iteration boundary, which is
// the only point where the message history is guaranteed to have no dangling
// tool_call awaiting a tool_result.
type StopError struct {
	// Gate is the name of the gate that stopped the turn.
	Gate string
	// Reason is the gate's user-facing explanation.
	Reason string
}

// Error implements error.
func (e *StopError) Error() string {
	if e.Gate == "" {
		return "loop guard stopped the turn: " + e.Reason
	}
	return "loop guard (" + e.Gate + ") stopped the turn: " + e.Reason
}

// NewStopError builds a StopError from a stop Result. Returns nil when res is
// not an ActionStop, so callers can write `if err := NewStopError(res); err !=
// nil` without re-testing the action.
func NewStopError(res Result) error {
	if res.Action != ActionStop {
		return nil
	}
	return &StopError{Gate: res.Gate, Reason: res.Reason}
}
