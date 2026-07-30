// Package tools provides agent management, workflow, and analysis tools.
//
// This file contains the workflow execution, summarization, and progress
// tracking functions moved from agent.go for the E3 architecture governance split:
//   - makeWorkflowProgress (shared by analysis and workflow_start)
//   - streamStartWorkflow method
//   - summarizeArgs struct + streamSummarize method
//   - workflowStartArgs struct + runStartWorkflow method
//   - runFlatWorkflow method (backwards-compatible flat mode)
//   - workflowTaskResult struct

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// makeWorkflowProgress creates a WorkflowProgress that pushes Status
// ("<done>/<total> agents <X>k <Y>s") and Text (per-step "Agent(tool)" panel,
// overwrite) to ch. Shared by streamStartWorkflow and streamAnalysis workflow
// mode. Mutex-guarded — workflow steps run concurrently.
//
// It returns a stop function the caller MUST defer before its own defer
// close(ch): the returned func signals the per-second refresh ticker to exit and
// BLOCKS until that goroutine has fully drained, so the caller's close(ch) can
// never race a still-in-flight pushPanel send (which would panic on a closed
// channel). The caller's defer stack is LIFO, so `defer stop()` registered after
// `defer close(ch)` runs first — exactly the order we need.
func makeWorkflowProgress(ch chan<- ToolChunk) (*WorkflowProgress, func()) {
	type stepState struct {
		tool    string
		calls   int
		tokens  int
		started time.Time
		ended   time.Time // zero while running; set on StepDone to freeze the duration
		status  string    // "" running | "done" | "error"
	}
	var mu sync.Mutex
	states := make(map[string]*stepState)
	var total, done, totalTokens int
	start := time.Now()
	finished := make(chan struct{})
	tickerDone := make(chan struct{})
	var finishOnce sync.Once

	pushPanel := func() {
		mu.Lock()
		defer mu.Unlock()
		// Non-blocking sends: Status/Text are live UI updates, not model data
		// (the model collects only Result, which pushPanel never emits). Dropping
		// a tick when the channel is full is harmless — the next event/tick
		// refreshes within a second. Crucially, non-blocking sends keep the
		// per-second ticker goroutine from ever getting stuck on a full channel,
		// which would otherwise hang stop() (it waits for the ticker to exit) and
		// deadlock the tool's own defer close(ch).
		send := func(c ToolChunk) {
			select {
			case ch <- c:
			default:
			}
		}
		if total > 0 {
			send(ToolChunk{Status: fmt.Sprintf("%d/%d agents %s %s", done, total, formatTokens(totalTokens), formatDur(time.Since(start)))})
		}
		var b strings.Builder
		ids := make([]string, 0, len(states))
		for id := range states {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool {
			si, sj := states[ids[i]], states[ids[j]]
			ri := si.status == "" // running
			rj := sj.status == ""
			if ri != rj {
				return ri // running first
			}
			return naturalIDLess(ids[i], ids[j])
		})
		for _, id := range ids {
			s := states[id]
			tool := s.tool
			prefix := ""
			switch s.status {
			case "done":
				prefix = "✓ "
				tool = ""
			case "error":
				prefix = "✗ "
				tool = ""
			default:
				if tool == "" {
					tool = "Thinking..."
				}
			}
			// Duration is frozen at StepDone for finished steps so the per-second
			// ticker doesn't keep inflating completed agents' elapsed time.
			dur := time.Since(s.started)
			if !s.ended.IsZero() {
				dur = s.ended.Sub(s.started)
			}
			fmt.Fprintf(&b, "%sAgent-%s(%s) %d tools %s %s\n", prefix, id, tool, s.calls, formatTokens(s.tokens), formatDur(dur))
		}
		if b.Len() > 0 {
			send(ToolChunk{Text: b.String(), Overwrite: true})
		}
	}

	// Per-second panel refresh so durations tick even without new events. Exits
	// when `finished` is closed (all steps done, or the caller's stop func) and
	// signals tickerDone so stop can wait for a full drain before close(ch).
	go func() {
		defer close(tickerDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				pushPanel()
			case <-finished:
				return
			}
		}
	}()

	stop := func() {
		finishOnce.Do(func() { close(finished) })
		<-tickerDone
	}

	return &WorkflowProgress{
		SetTotal: func(n int) {
			mu.Lock()
			total = n
			mu.Unlock()
			pushPanel()
		},
		StepCB: func(stepID string) func(SubAgentEvent) {
			// Eagerly create the state when the step STARTS (not lazily on the
			// first SubAgentEvent) so steps that have launched runSubAgent but
			// haven't yet called a tool still appear in the panel as a running
			// row — otherwise bounded-concurrency workflows show only finished
			// steps while in-flight ones are invisible, defeating "running first".
			mu.Lock()
			if states[stepID] == nil {
				states[stepID] = &stepState{started: time.Now()}
			}
			mu.Unlock()
			pushPanel()
			return func(ev SubAgentEvent) {
				mu.Lock()
				s := states[stepID]
				if s == nil {
					s = &stepState{started: time.Now()}
					states[stepID] = s
				}
				switch ev.Kind {
				case SubAgentToolStart:
					s.tool = ev.ToolDisplay
					s.calls++
				case SubAgentToolEnd:
					s.tool = ""
				case SubAgentTokens:
					s.tokens += ev.Tokens
					totalTokens += ev.Tokens
				}
				mu.Unlock()
				pushPanel()
			}
		},
		StepDone: func(stepID string, err error) {
			mu.Lock()
			done++
			s := states[stepID]
			if s != nil {
				s.tool = ""
				s.ended = time.Now()
				if err != nil {
					s.status = "error"
				} else {
					s.status = "done"
				}
			}
			allDone := total > 0 && done >= total
			mu.Unlock()
			if allDone {
				finishOnce.Do(func() { close(finished) })
			}
			pushPanel()
		},
	}, stop
}

func (t *AgentTools) streamStartWorkflow(ctx context.Context, argsJSON string) <-chan ToolChunk {
	ch := make(chan ToolChunk, 32)
	go func() {
		defer close(ch)
		wp, stopTicker := makeWorkflowProgress(ch)
		defer stopTicker()
		sctx := WithWorkflowProgress(ctx, wp)
		result, err := t.runStartWorkflow(sctx, argsJSON)
		if err != nil {
			pushErrChunk(ch, err)
			return
		}
		ch <- ToolChunk{Result: result}
	}()
	return ch
}

type summarizeArgs struct {
	Path     string `json:"path"`
	Focus    string `json:"focus"`
	MaxLines int    `json:"max_lines"`
}

// runSummarize runs the predefined "summarize" agent over a target file. The
// sub-agent is restricted to fs_read only (it must page through large files
// rather than mutate them). focus and max_lines are interpolated into the
// predefined prompt; max_lines defaults to 50.
func (t *AgentTools) streamSummarize(ctx context.Context, argsJSON string) <-chan ToolChunk {
	ch := make(chan ToolChunk, 16)
	go func() {
		defer close(ch)
		var a summarizeArgs
		if err := ParseArgs(argsJSON, &a); err != nil {
			pushErrChunk(ch, err)
			return
		}
		if a.Path == "" {
			pushErrChunk(ch, fmt.Errorf("path must not be empty"))
			return
		}
		if t.chatModel == nil {
			pushErrChunk(ch, fmt.Errorf("no chat model configured; cannot summarize"))
			return
		}
		def, ok := GetPredefinedAgent("summarize")
		if !ok {
			pushErrChunk(ch, fmt.Errorf("internal error: summarize predefined agent not found"))
			return
		}
		maxLines := a.MaxLines
		if maxLines <= 0 {
			maxLines = 50
		}
		focusLine := ""
		if f := strings.TrimSpace(a.Focus); f != "" {
			focusLine = "重点关注: " + f
		}
		prompt := FillPrompt(def.PromptTmpl, map[string]string{
			"target":     a.Path,
			"focus_line": focusLine,
			"max_lines":  strconv.Itoa(maxLines),
		})
		sctx, finalize := bindSubAgentProgress(ctx, ch, "")
		result, err := t.runSubAgent(WithLeafSubAgentTools(sctx), prompt, []string{"fs_read"}, "")
		finalize()
		if err != nil {
			pushErrChunk(ch, err)
			return
		}
		ch <- ToolChunk{Result: result}
	}()
	return ch
}

type workflowStartArgs struct {
	Tasks    json.RawMessage `json:"tasks"`    // flat mode: JSON array of prompts
	Workflow json.RawMessage `json:"workflow"` // DAG mode: JSON with "steps" array
}

func (t *AgentTools) runStartWorkflow(ctx context.Context, argsJSON string) (string, error) {
	var a workflowStartArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}

	// DAG mode takes precedence.
	if a.Workflow != nil && len(a.Workflow) > 0 {
		return t.runDAGWorkflow(ctx, a.Workflow)
	}

	// Flat mode (backwards compatible).
	if a.Tasks != nil && len(a.Tasks) > 0 {
		return t.runFlatWorkflow(ctx, a.Tasks)
	}

	return errorResult("provide either \"tasks\" (flat array) or \"workflow\" (DAG definition with \"steps\")"), nil
}

// ---------------------------------------------------------------------------
// Flat workflow (backwards compatible)
// ---------------------------------------------------------------------------

func (t *AgentTools) runFlatWorkflow(ctx context.Context, tasksJSON json.RawMessage) (string, error) {
	ctx = WithLeafSubAgentTools(ctx)
	var prompts []string
	if err := json.Unmarshal(tasksJSON, &prompts); err != nil {
		// Fall back: treat it as a JSON string that contains a JSON array.
		var raw string
		if uerr := json.Unmarshal(tasksJSON, &raw); uerr != nil {
			return errorResult("tasks must be a JSON array of strings or a JSON string containing a JSON array: " + err.Error()), nil
		}
		if err := json.Unmarshal([]byte(raw), &prompts); err != nil {
			return errorResult("tasks must be a valid JSON array of strings: " + err.Error()), nil
		}
	}
	if len(prompts) == 0 {
		return errorResult("tasks array must not be empty"), nil
	}
	if t.chatModel == nil {
		return errorResult("no chat model configured; cannot start workflow"), nil
	}

	concurrency := runtime.NumCPU()
	if concurrency < 1 {
		concurrency = 1
	}

	type taskUnit struct {
		index  int
		prompt string
	}

	type taskOutcome struct {
		index  int
		result string
		err    error
	}

	taskCount := len(prompts)
	taskCh := make(chan taskUnit, taskCount)
	outcomeCh := make(chan taskOutcome, taskCount)

	// Report total task count to WorkflowProgress (if bound).
	wp := WorkflowProgressFromContext(ctx)
	if wp != nil && wp.SetTotal != nil {
		wp.SetTotal(taskCount)
	}

	for i, p := range prompts {
		taskCh <- taskUnit{index: i, prompt: p}
	}
	close(taskCh)

	sem := make(chan struct{}, concurrency)
	for i := 0; i < concurrency; i++ {
		sem <- struct{}{}
	}

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range taskCh {
				<-sem
				func() {
					defer func() { sem <- struct{}{} }()
					taskCtx := ctx
					if wp != nil && wp.StepCB != nil {
						taskCtx = WithSubAgentProgress(ctx, wp.StepCB(fmt.Sprintf("%d", task.index)))
					}
					r, err := t.runSubAgent(taskCtx, task.prompt, nil, "")
					if wp != nil && wp.StepDone != nil {
						wp.StepDone(fmt.Sprintf("%d", task.index), err)
					}
					if err != nil {
						outcomeCh <- taskOutcome{index: task.index, err: err}
					} else {
						outcomeCh <- taskOutcome{index: task.index, result: r}
					}
				}()
			}
		}()
	}

	go func() {
		wg.Wait()
		close(outcomeCh)
	}()

	results := make([]*workflowTaskResult, taskCount)
	for i := 0; i < taskCount; i++ {
		results[i] = &workflowTaskResult{Index: i}
	}
	for o := range outcomeCh {
		if o.err != nil {
			results[o.index].Error = o.err.Error()
		} else {
			results[o.index].Result = o.result
		}
	}

	var sb strings.Builder
	for _, r := range results {
		if r.Error != "" {
			fmt.Fprintf(&sb, "%d: ✗ %s\n", r.Index, r.Error)
		} else if r.Result != "" {
			fmt.Fprintf(&sb, "%d: %s\n", r.Index, r.Result)
		}
	}
	return sb.String(), nil
}

type workflowTaskResult struct {
	Index  int    `json:"index"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}
