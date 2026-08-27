// internal/tools/toolbatch.go
//
// T12: tool_batch — one tool call that runs a short, fixed sequence of other
// tool calls, where a later step may reference an earlier step's output.
//
// # This is a data structure, not a language
//
// The obvious way to build "let the model chain calls" is an expression
// evaluator, and it is the wrong one. An evaluator is a new parser reachable
// by model-authored text, running inside the process that holds the operator's
// credentials, and every feature it grows (arithmetic, calls, indexing by
// computed key) is a new way to express something the guard layer never gets
// to see. The capability actually needed is much smaller:
//
//	run these N registered tools in order; a later step's argument may contain
//	the text of an earlier step's result, or of one field inside it.
//
// That is a JSON array plus a substitution rule. There is no evaluator here,
// no operators, no functions, no computed field names — `$0` and `$0.a.b.0`
// are the entire grammar, and resolveRef is a map/slice walk over already
// parsed JSON. Nothing the model writes is ever compiled or evaluated.
//
// # Every step is authorized individually. This is the load-bearing property.
//
// A batch tool is the natural shape of a guard bypass: authorize the wrapper
// once, then let it do N things nobody looked at. That is NOT what happens
// here. Each step is dispatched through the registered tool's own Stream,
// which is GuardedTool.Stream, which calls Authorize (or
// AuthorizeApprovalRequired) exactly as it would for a direct call. The
// batch's own authorization covers the batch and nothing else.
//
// The denial is detected TYPED, via ToolChunk.Err and IsDenyErr, not by
// pattern-matching the result text — a text match would silently stop
// detecting denials the day the message is reworded, and the observable
// consequence would be a batch that kept running past a refusal.
//
// A refused step STOPS the batch. Continuing would hand the model a partial
// result set to reason over, and reasoning over half a result is how a model
// concludes that a file it was refused access to is empty.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// MaxBatchSteps caps how many tool calls one tool_batch may contain.
//
// The cap exists because the batch runs under a SINGLE tool timeout while each
// step may spawn a process, and because a long batch amplifies the cost of the
// model being wrong about step 1 — every later step then runs on a premise
// that no longer holds. Eight is enough for the sequences this exists to
// collapse (read, edit, run the test, read the failure) and short enough that
// a human reading the audit trail can still follow what happened.
const MaxBatchSteps = 8

// batchStepTimeout bounds one step. It is separate from the batch's own
// timeout so a single wedged step cannot consume the whole budget and starve
// the steps behind it of any chance to run — which would produce the worst
// possible transcript: a batch that reports N-1 steps "not attempted" with no
// explanation of which one hung.
const batchStepTimeout = 120 * time.Second

// ToolBatchTool is the tool_batch surface plus its dispatch table.
//
// The table is populated AFTER construction (Bind) because the batch tool must
// itself be in the registry it dispatches over, and a constructor cannot take
// a list it is going to be a member of. Until Bind is called the tool refuses
// every batch with a wiring error rather than silently finding no tools and
// reporting eight "unknown tool" failures, which would read like a model
// mistake instead of a missing line in the composition root.
type ToolBatchTool struct {
	// Tool is the registered GuardedTool. Callers append this to the registry.
	Tool *GuardedTool
	// table maps tool name -> dispatchable tool. Written once by Bind before
	// the server starts serving, read by every batch afterwards.
	table map[string]Tool
	bound bool
}

// BatchStep is one entry of the batch program.
type BatchStep struct {
	// Tool is the registered tool name to call.
	Tool string `json:"tool"`
	// Args is the argument object handed to that tool, after reference
	// substitution. Kept as json.RawMessage so substitution operates on the
	// decoded structure rather than on the raw text — substituting into raw
	// JSON text would let a result containing a quote character reshape the
	// argument object, which is injection by another name.
	Args json.RawMessage `json:"args"`
}

// BatchStepResult is what one executed step contributes to the report.
type BatchStepResult struct {
	Index  int    `json:"index"`
	Tool   string `json:"tool"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
	// Denied marks a step the guard refused, as opposed to one that ran and
	// failed. The model's next move differs completely between the two, and
	// without this field both arrive as an "error" string it has to guess at.
	Denied bool `json:"denied,omitempty"`
}

// BatchReport is tool_batch's result.
type BatchReport struct {
	Steps []BatchStepResult `json:"steps"`
	// Completed is the number of steps that ran to completion. Less than
	// len(program) means the batch stopped early; Stopped says why.
	Completed int    `json:"completed"`
	Stopped   string `json:"stopped,omitempty"`
}

// NewToolBatchTool constructs tool_batch. Bind must be called with the final
// registry before the tool can dispatch anything.
func NewToolBatchTool() *ToolBatchTool {
	b := &ToolBatchTool{}
	b.Tool = NewGuardedTool(
		"tool_batch", "Batch Tools",
		"Run up to 8 registered tool calls in one round trip, in order. "+
			"A later step may reference an earlier step's output: any string in a step's "+
			"args may contain \"$N\" (step N's whole result) or \"$N.field.sub.0\" "+
			"(one field inside step N's result, when that result is a JSON object/array). "+
			"There is no expression language: no arithmetic, no function calls, no computed "+
			"field names — substitution of a prior result into a string is the only operation.\n"+
			"Every step is permission-checked individually exactly as a direct call would be; "+
			"batching grants nothing. If any step is denied or fails, the batch STOPS and "+
			"reports what ran.\n"+
			"Input: {\"steps\":[{\"tool\":\"fs_read\",\"args\":{...}}, ...]}",
		10*time.Minute,
		params(map[string]*schema.ParameterInfo{
			"steps": {
				Type:     schema.String,
				Desc:     "JSON array of {\"tool\":\"<name>\",\"args\":{...}} objects, max 8",
				Required: true,
			},
		}),
		SyncStream(b.run),
	)
	return b
}

// Bind installs the dispatch table from the assembled registry.
//
// Members that do not satisfy the tools.Tool contract are SKIPPED rather than
// adapted: dispatch reads ToolChunk.Err to tell a permission denial from an
// ordinary failure, and a bare tool.InvokableTool has no channel to carry it.
// Adapting one would mean a denial inside a batch became an untyped string,
// which is exactly the detection this file refuses to do.
//
// tool_batch itself is excluded, so a batch cannot contain a batch. The reason
// is not recursion depth (MaxBatchSteps bounds the width, not the depth) but
// that a nested batch multiplies the step cap by itself while presenting the
// operator with one audit entry for the outer call.
func (b *ToolBatchTool) Bind(registry []tool.InvokableTool) {
	table := make(map[string]Tool, len(registry))
	for _, t := range registry {
		ct, ok := t.(Tool)
		if !ok {
			continue
		}
		info, err := ct.Info(context.Background())
		if err != nil || info == nil || info.Name == "" || info.Name == "tool_batch" {
			continue
		}
		table[info.Name] = ct
	}
	b.table = table
	b.bound = true
}

// Bound reports whether Bind has been called. Exposed so the composition root
// (and its wiring test) can assert the second half of the two-step
// registration actually happened — a tool_batch in the registry with no table
// is a tool that answers every call with a wiring error.
func (b *ToolBatchTool) Bound() bool { return b.bound }

// Tools returns the tool slice, matching the shape every other tool group in
// this package exposes so the composition root needs no special case.
func (b *ToolBatchTool) Tools() []*GuardedTool {
	if b.Tool == nil {
		return nil
	}
	return []*GuardedTool{b.Tool}
}

type toolBatchArgs struct {
	Steps json.RawMessage `json:"steps"`
}

// run is tool_batch's body. Errors that describe a malformed program are
// returned as Go errors (the model gets them as a result and can fix the
// program); errors from the steps themselves become report entries, because a
// batch where step 3 failed still has the output of steps 1 and 2 to report.
func (b *ToolBatchTool) run(ctx context.Context, argsJSON string) (string, error) {
	if !b.bound {
		return "", fmt.Errorf("tool_batch: dispatch table not bound (composition root must call Bind after assembling the registry)")
	}
	var outer toolBatchArgs
	if err := ParseArgs(argsJSON, &outer); err != nil {
		return "", err
	}
	program, err := parseBatchProgram(outer.Steps)
	if err != nil {
		return "", err
	}
	report := BatchReport{Steps: make([]BatchStepResult, 0, len(program))}
	results := make([]string, 0, len(program))
	for i, step := range program {
		target, ok := b.table[step.Tool]
		if !ok {
			report.Stopped = fmt.Sprintf("step %d: %q is not a registered tool of this agent", i, step.Tool)
			report.Steps = append(report.Steps, BatchStepResult{
				Index: i, Tool: step.Tool, Error: report.Stopped,
			})
			break
		}
		stepArgs, err := substituteRefs(step.Args, results)
		if err != nil {
			report.Stopped = fmt.Sprintf("step %d: %v", i, err)
			report.Steps = append(report.Steps, BatchStepResult{
				Index: i, Tool: step.Tool, Error: report.Stopped,
			})
			break
		}
		out, denied, runErr := b.dispatch(ctx, target, string(stepArgs))
		entry := BatchStepResult{Index: i, Tool: step.Tool, Result: out, Denied: denied}
		if runErr != nil {
			entry.Error = runErr.Error()
		}
		report.Steps = append(report.Steps, entry)
		if runErr != nil || denied {
			if denied {
				report.Stopped = fmt.Sprintf("step %d (%s) was denied by the permission layer; "+
					"the batch stopped so later steps do not run on a missing result", i, step.Tool)
			} else {
				report.Stopped = fmt.Sprintf("step %d (%s) failed; the batch stopped", i, step.Tool)
			}
			break
		}
		report.Completed++
		results = append(results, out)
	}
	return toJSON(report), nil
}

// dispatch runs one step through the target tool's own Stream, which is where
// that tool's Authorize lives.
//
// The three returns separate the three outcomes the caller must treat
// differently: output, "the guard said no" (denied), and "the tool broke"
// (err). Collapsing the last two — which a naive `InvokableRun` call does,
// since it folds both into the result string — is what would make a batch
// unable to tell a refusal from a failure.
func (b *ToolBatchTool) dispatch(ctx context.Context, target Tool, args string) (string, bool, error) {
	stepCtx, cancel := context.WithTimeout(ctx, batchStepTimeout)
	defer cancel()
	var out strings.Builder
	var runErr error
	for c := range target.Stream(stepCtx, args) {
		if c.Result != "" {
			out.WriteString(c.Result)
		}
		if c.Err != nil {
			runErr = c.Err
		}
	}
	if runErr != nil {
		if IsDenyErr(runErr) {
			return out.String(), true, nil
		}
		return out.String(), false, runErr
	}
	return out.String(), false, nil
}

// parseBatchProgram decodes and validates the step array.
//
// Validation is up front and total: a program with a bad step 6 runs none of
// steps 0-5. Half-running a program the model got wrong is worse than running
// none of it, because the side effects of the first half are real and the
// model's plan for them was based on a whole it never got.
func parseBatchProgram(raw json.RawMessage) ([]BatchStep, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("tool_batch: steps is required")
	}
	steps, err := decodeSteps(raw)
	if err != nil {
		return nil, err
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("tool_batch: steps is empty")
	}
	if len(steps) > MaxBatchSteps {
		return nil, fmt.Errorf("tool_batch: %d steps exceeds the limit of %d", len(steps), MaxBatchSteps)
	}
	for i := range steps {
		if strings.TrimSpace(steps[i].Tool) == "" {
			return nil, fmt.Errorf("tool_batch: step %d has no tool name", i)
		}
		if steps[i].Tool == "tool_batch" {
			return nil, fmt.Errorf("tool_batch: step %d cannot be tool_batch (nesting is refused)", i)
		}
		if len(steps[i].Args) == 0 {
			steps[i].Args = json.RawMessage("{}")
		}
	}
	return steps, nil
}

// decodeSteps accepts the array either as JSON directly or as a JSON string
// containing the array.
//
// Both shapes are accepted because both are produced in practice: schema.String
// declares the parameter as a string, and models split about evenly between
// emitting a nested array and emitting an escaped string of one. Rejecting
// either costs a wasted turn to teach the model a distinction that carries no
// meaning.
func decodeSteps(raw json.RawMessage) ([]BatchStep, error) {
	var steps []BatchStep
	if err := json.Unmarshal(raw, &steps); err == nil {
		return steps, nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err != nil {
		return nil, fmt.Errorf("tool_batch: steps must be a JSON array of {tool,args} objects")
	}
	if err := json.Unmarshal([]byte(asString), &steps); err != nil {
		return nil, fmt.Errorf("tool_batch: steps must be a JSON array of {tool,args} objects: %w", err)
	}
	return steps, nil
}
