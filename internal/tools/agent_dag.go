// Package tools provides agent management, workflow, and analysis tools.
//
// This file contains the DAG workflow engine and related types/functions
// moved from agent.go for the E3 architecture governance split:
//   - WorkflowDef, WorkflowStepDef, ExpandedStep, stepState, dagResult structs
//   - runDAGWorkflow and executeLevel methods
//   - rangeRegex var
//   - expandStepID, expandSteps, expandDeps, resolveDeps functions
//   - topoSortLevels function
//   - interpolatePrompt template function

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// WorkflowDef is the top-level DAG workflow definition.
type WorkflowDef struct {
	Steps []WorkflowStepDef `json:"steps"`
}

// WorkflowStepDef defines one step (possibly with range expansion).
type WorkflowStepDef struct {
	ID     string   `json:"id"`             // e.g., "A1" or "B1-8"
	Prompt string   `json:"prompt"`         // template with {{stepID.output}} etc.
	Deps   []string `json:"deps,omitempty"` // dependency IDs or ranges, e.g. ["A1", "B1-4"]
}

// ExpandedStep is a concrete step after range expansion.
type ExpandedStep struct {
	ID     string   // e.g., "B3"
	Origin string   // original range spec, e.g. "B1-8"
	Prompt string   // original template text
	Deps   []string // concrete dependency IDs (after expanding any ranges)
	Index  int      // 0-based index within the expansion group
	Count  int      // total count in the expansion group (1 for non-ranged)
}

// stepState tracks execution state of one expanded step.
type stepState struct {
	step   ExpandedStep
	result string // populated after successful execution
	err    error  // populated on failure
}

// dagResult contains the final outcome for one step in the DAG.
type dagResult struct {
	ID     string `json:"id"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// runDAGWorkflow executes the DAG workflow defined in workflowJSON.
func (t *AgentTools) runDAGWorkflow(ctx context.Context, workflowJSON json.RawMessage) (string, error) {
	ctx = WithLeafSubAgentTools(ctx)
	// Try parsing as a JSON object with a "steps" array.
	var wf WorkflowDef
	if err := json.Unmarshal(workflowJSON, &wf); err != nil {
		// Maybe it's a JSON string containing the object.
		var raw string
		if uerr := json.Unmarshal(workflowJSON, &raw); uerr != nil {
			return errorResult("workflow must be a JSON object with a \"steps\" array: " + err.Error()), nil
		}
		if err := json.Unmarshal([]byte(raw), &wf); err != nil {
			return errorResult("workflow must be a valid JSON object with a \"steps\" array: " + err.Error()), nil
		}
	}
	if len(wf.Steps) == 0 {
		return errorResult("workflow must have at least one step"), nil
	}
	if t.chatModel == nil {
		return errorResult("no chat model configured; cannot start workflow"), nil
	}

	// Phase 1: Expand ranged steps into concrete steps.
	expanded, err := expandSteps(wf.Steps)
	if err != nil {
		return errorResult("workflow step expansion: " + err.Error()), nil
	}
	if len(expanded) == 0 {
		return errorResult("workflow expanded to zero steps"), nil
	}

	// Phase 2: Resolve and validate dependencies.
	resolvedDeps, stepIDs, err := resolveDeps(expanded)
	if err != nil {
		return errorResult("workflow dependency resolution: " + err.Error()), nil
	}

	// Phase 3: Build step map for topological sort.
	stepMap := make(map[string]ExpandedStep, len(expanded))
	for _, s := range expanded {
		stepMap[s.ID] = s
	}

	// Phase 4: Topological sort into execution levels.
	levels, err := topoSortLevels(stepMap, resolvedDeps)
	if err != nil {
		return errorResult("workflow topological sort: " + err.Error()), nil
	}

	// Phase 5: Execute level by level.
	concurrency := runtime.NumCPU()
	if concurrency < 1 {
		concurrency = 1
	}

	// results stores the output of each step by ID.
	results := make(map[string]string)
	errors := make(map[string]string)

	// Report total step count to WorkflowProgress (if bound) for the Status panel.
	if wp := WorkflowProgressFromContext(ctx); wp != nil && wp.SetTotal != nil {
		wp.SetTotal(len(expanded))
	}

	for _, level := range levels {
		if err := t.executeLevel(ctx, level, concurrency, results, errors); err != nil {
			// If the entire level fails catastrophically (not just individual steps),
			// return the error.
			return "", err
		}
	}

	// Build sorted output.
	sortedIDs := make([]string, 0, len(stepIDs))
	for _, id := range stepIDs {
		sortedIDs = append(sortedIDs, id)
	}
	// Preserve the topological order.
	out := make([]dagResult, 0, len(sortedIDs))
	for _, id := range sortedIDs {
		r := dagResult{ID: id}
		if e, ok := errors[id]; ok {
			r.Error = e
		} else if res, ok := results[id]; ok {
			r.Result = res
		} else {
			r.Error = "step did not complete"
		}
		out = append(out, r)
	}

	var sb strings.Builder
	for _, r := range out {
		if r.Error != "" {
			fmt.Fprintf(&sb, "%s: ✗ %s\n", r.ID, r.Error)
		} else if r.Result != "" {
			fmt.Fprintf(&sb, "%s: %s\n", r.ID, r.Result)
		}
	}
	return sb.String(), nil
}

// executeLevel runs all steps in a level concurrently, limited by concurrency.
// Per-step SubAgentProgress is bound from WorkflowProgress (ctx) so each step's
// tool calls / tokens are tracked individually; StepDone fires after each
// completes to advance the "N/M agents" counter.
func (t *AgentTools) executeLevel(ctx context.Context, level []ExpandedStep, concurrency int, results map[string]string, errors map[string]string) error {
	if len(level) == 0 {
		return nil
	}

	wp := WorkflowProgressFromContext(ctx)

	type stepOutcome struct {
		id  string
		out string
		err error
	}

	sem := make(chan struct{}, concurrency)
	for i := 0; i < concurrency; i++ {
		sem <- struct{}{}
	}

	var wg sync.WaitGroup
	outcomeCh := make(chan stepOutcome, len(level))

	for _, step := range level {
		prompt := interpolatePrompt(step.Prompt, results, step.Index, step.Count, step.ID)
		wg.Add(1)
		step := step // capture
		go func() {
			defer wg.Done()
			<-sem
			defer func() { sem <- struct{}{} }()

			stepCtx := ctx
			if wp != nil && wp.StepCB != nil {
				stepCtx = WithSubAgentProgress(ctx, wp.StepCB(step.ID))
			}
			// Deliberately NOT wrapped in ParentWorkingSetHint: this is an
			// INTERMEDIATE DAG step. Its output is spliced into the prompt of
			// downstream steps, so appending the parent-facing hint would let a
			// sub-agent read a marker meant only for the parent — polluting the
			// intermediate state. Only terminal paths (agent_start, analysis)
			// re-surface EVIDENCE.
			out, err := t.runSubAgent(stepCtx, prompt, nil, "")
			if wp != nil && wp.StepDone != nil {
				wp.StepDone(step.ID, err)
			}
			if err != nil {
				outcomeCh <- stepOutcome{id: step.ID, err: err}
			} else {
				outcomeCh <- stepOutcome{id: step.ID, out: out}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(outcomeCh)
	}()

	for o := range outcomeCh {
		if o.err != nil {
			errors[o.id] = o.err.Error()
		} else {
			results[o.id] = o.out
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Range parsing
// ---------------------------------------------------------------------------

// rangeRegex matches patterns like "A1-8", "B1-4", or simple "A1".
// It captures: prefix (letters), start number, end number (optional).
var rangeRegex = regexp.MustCompile(`^([A-Za-z_]+)(\d+)(?:-(\d+))?$`)

// expandStepID expands a single ID string that may contain a range.
// "A1"    → ["A1"]
// "B1-8"  → ["B1","B2","B3","B4","B5","B6","B7","B8"]
// Returns error for invalid formats.
func expandStepID(spec string) ([]string, error) {
	matches := rangeRegex.FindStringSubmatch(spec)
	if matches == nil {
		return nil, fmt.Errorf("invalid step ID %q: must match pattern like A1 or B1-8", spec)
	}

	prefix := matches[1]
	startStr := matches[2]
	endStr := matches[3]

	start, err := strconv.Atoi(startStr)
	if err != nil {
		return nil, fmt.Errorf("invalid step ID %q: cannot parse start number", spec)
	}

	// No range specified.
	if endStr == "" {
		return []string{fmt.Sprintf("%s%d", prefix, start)}, nil
	}

	end, err := strconv.Atoi(endStr)
	if err != nil {
		return nil, fmt.Errorf("invalid step ID %q: cannot parse end number", spec)
	}

	if start > end {
		return nil, fmt.Errorf("invalid step ID %q: start %d > end %d", spec, start, end)
	}
	if end-start > 1000 {
		return nil, fmt.Errorf("invalid step ID %q: range too large (max 1000)", spec)
	}

	ids := make([]string, 0, end-start+1)
	for i := start; i <= end; i++ {
		ids = append(ids, fmt.Sprintf("%s%d", prefix, i))
	}
	return ids, nil
}

// expandSteps processes all step definitions, expanding ranged IDs into concrete steps.
// It also expands dependency ranges within each step.
func expandSteps(steps []WorkflowStepDef) ([]ExpandedStep, error) {
	// First pass: expand step IDs.
	type expansionGroup struct {
		origin string
		ids    []string
		prompt string
		deps   []string
	}

	groups := make([]expansionGroup, 0, len(steps))
	allIDs := make(map[string]bool) // track used IDs to detect duplicates

	for _, s := range steps {
		if s.ID == "" {
			return nil, fmt.Errorf("step with empty id")
		}
		ids, err := expandStepID(s.ID)
		if err != nil {
			return nil, fmt.Errorf("step %q: %w", s.ID, err)
		}
		for _, id := range ids {
			if allIDs[id] {
				return nil, fmt.Errorf("duplicate step ID %q", id)
			}
			allIDs[id] = true
		}
		groups = append(groups, expansionGroup{
			origin: s.ID,
			ids:    ids,
			prompt: s.Prompt,
			deps:   s.Deps,
		})
	}

	// Second pass: expand deps and build ExpandedStep list.
	var expanded []ExpandedStep
	for _, g := range groups {
		for i, id := range g.ids {
			// Expand dependency ranges for this step.
			deps, err := expandDeps(g.deps)
			if err != nil {
				return nil, fmt.Errorf("step %q: dep expansion: %w", id, err)
			}
			// Verify all deps exist.
			for _, dep := range deps {
				if !allIDs[dep] {
					return nil, fmt.Errorf("step %q depends on %q which does not exist", id, dep)
				}
			}
			expanded = append(expanded, ExpandedStep{
				ID:     id,
				Origin: g.origin,
				Prompt: g.prompt,
				Deps:   deps,
				Index:  i,
				Count:  len(g.ids),
			})
		}
	}
	return expanded, nil
}

// expandDeps expands each element of deps; elements may be simple IDs like "A1"
// or ranges like "B1-8".
func expandDeps(deps []string) ([]string, error) {
	if len(deps) == 0 {
		return nil, nil
	}
	var expanded []string
	seen := make(map[string]bool)
	for _, d := range deps {
		ids, err := expandStepID(d)
		if err != nil {
			return nil, fmt.Errorf("dep %q: %w", d, err)
		}
		for _, id := range ids {
			if !seen[id] {
				expanded = append(expanded, id)
				seen[id] = true
			}
		}
	}
	return expanded, nil
}

// resolveDeps builds a concrete dependency map and returns the ordered list of step IDs.
func resolveDeps(expanded []ExpandedStep) (map[string][]string, []string, error) {
	depsMap := make(map[string][]string, len(expanded))
	stepIDs := make([]string, 0, len(expanded))
	for _, s := range expanded {
		if _, exists := depsMap[s.ID]; exists {
			return nil, nil, fmt.Errorf("duplicate step ID %q", s.ID)
		}
		depsMap[s.ID] = s.Deps
		stepIDs = append(stepIDs, s.ID)
	}
	return depsMap, stepIDs, nil
}

// ---------------------------------------------------------------------------
// Topological sort
// ---------------------------------------------------------------------------

// topoSortLevels performs topological sort and returns levels with full step info.
func topoSortLevels(stepMap map[string]ExpandedStep, deps map[string][]string) ([][]ExpandedStep, error) {
	stepIDs := make([]string, 0, len(stepMap))
	for id := range stepMap {
		stepIDs = append(stepIDs, id)
	}
	// Sort for determinism.
	sort.Strings(stepIDs)

	// Build reverse dependencies (who depends on me).
	reverse := make(map[string][]string)
	for _, id := range stepIDs {
		for _, dep := range deps[id] {
			reverse[dep] = append(reverse[dep], id)
		}
	}

	// In-degree.
	inDegree := make(map[string]int, len(stepIDs))
	for _, id := range stepIDs {
		inDegree[id] = len(deps[id])
	}

	// Queue of ready steps.
	ready := make([]string, 0)
	for _, id := range stepIDs {
		if inDegree[id] == 0 {
			ready = append(ready, id)
		}
	}

	var levels [][]ExpandedStep
	processed := 0

	for len(ready) > 0 {
		level := make([]ExpandedStep, 0, len(ready))
		nextReady := make([]string, 0)

		for _, id := range ready {
			level = append(level, stepMap[id])
			processed++

			// Decrease in-degree of dependents.
			for _, dependent := range reverse[id] {
				inDegree[dependent]--
				if inDegree[dependent] == 0 {
					nextReady = append(nextReady, dependent)
				}
			}
		}

		levels = append(levels, level)
		ready = nextReady
	}

	if processed != len(stepIDs) {
		return nil, fmt.Errorf("cycle detected in workflow dependency graph (%d of %d steps processed)", processed, len(stepIDs))
	}

	return levels, nil
}

// ---------------------------------------------------------------------------
// Template interpolation
// ---------------------------------------------------------------------------

// interpolatePrompt replaces {{stepID.output}} with the stored result for
// stepID, {{self.index}}, {{self.count}}, and {{self.id}} with the expanded
// step's metadata.
func interpolatePrompt(tmpl string, results map[string]string, index, count int, id string) string {
	// Replace {{self.*}} first.
	s := tmpl
	s = strings.ReplaceAll(s, "{{self.index}}", strconv.Itoa(index))
	s = strings.ReplaceAll(s, "{{self.count}}", strconv.Itoa(count))
	s = strings.ReplaceAll(s, "{{self.id}}", id)

	// Replace {{stepID.output}}.
	re := regexp.MustCompile(`\{\{([A-Za-z_]\w*)\.output\}\}`)
	s = re.ReplaceAllStringFunc(s, func(match string) string {
		submatch := re.FindStringSubmatch(match)
		if len(submatch) < 2 {
			return match
		}
		stepID := submatch[1]
		if res, ok := results[stepID]; ok {
			return res
		}
		// If the dependency hasn't been resolved yet (shouldn't happen in correct
		// DAG execution), leave a placeholder so the model can still understand.
		return fmt.Sprintf("[output of %s not yet available]", stepID)
	})

	return s
}
