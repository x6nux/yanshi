package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type reviewInput struct {
	Diff   string `json:"diff"`
	TaskID string `json:"task_id"`
	Repo   string `json:"repo,omitempty"`
	Number int    `json:"number,omitempty"`

	// Base selects what to review when diff is omitted: "working_tree",
	// "base_ref" or "commit", with Ref supplying the ref for the latter two.
	//
	// Without this the caller had to produce the diff text itself, which made
	// "review the branch" a two-tool dance the model had to get right — and
	// made the acceptance's "supports three bases" false, even though git_diff
	// had all three scopes the whole time. The diff field still wins when both
	// are given: a caller holding a diff (a PR payload, say) is not asking for
	// a working tree to be read.
	Base string `json:"base,omitempty"`
	Ref  string `json:"ref,omitempty"`
}

// resolveReviewDiff produces the unified diff for a base selection.
//
// It reuses git_diff's scope machinery rather than shelling out separately, so
// the three bases keep producing the three argv shapes that tool already pins
// — a second implementation here would be a second thing to keep correct.
func resolveReviewDiff(ctx context.Context, in reviewInput) (string, error) {
	root := WorkRootFromContext(ctx)
	if root == "" {
		return "", fmt.Errorf("review: no work root bound")
	}
	var args gitDiffArgs
	args.Scope.Kind = in.Base
	args.Scope.Ref = in.Ref
	switch in.Base {
	case "working_tree":
	case "base_ref", "commit":
		if strings.TrimSpace(in.Ref) == "" {
			return "", fmt.Errorf("review: base %q requires a ref", in.Base)
		}
	default:
		return "", fmt.Errorf("review: unknown base %q (want working_tree, base_ref or commit)", in.Base)
	}
	files, err := collectGitDiffFiles(ctx, root, args)
	if err != nil {
		return "", fmt.Errorf("review: collect diff: %w", err)
	}
	var b strings.Builder
	for _, f := range files {
		b.WriteString(f.Patch)
		if !strings.HasSuffix(f.Patch, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String(), nil
}

// streamReview is the core pipeline:
//  1. chunk diff at 48 KiB hunk-safe boundaries
//  2. dispatch each chunk to the review sub-agent via SubAgentRunnerFromContext
//  3. decode each JSON response
//  4. dedupe + sort findings
//  5. writeArtifactOrSpill if total serialized size exceeds SpillThreshold
func streamReview(ctx context.Context, in reviewInput) (string, error) {
	if in.Diff == "" && in.Base != "" {
		diff, err := resolveReviewDiff(ctx, in)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		in.Diff = diff
	}
	runner := SubAgentRunnerFromContext(ctx)
	if runner == nil {
		return errorResult("review requires a bound sub-agent runner (task-orchestrator)"), nil
	}
	def, ok := GetPredefinedAgent("review")
	if !ok {
		return errorResult(`predefined "review" agent not registered`), nil
	}
	chunks := chunkDiff(in.Diff, reviewChunkLimit)
	var allFindings []reviewFinding
	for i, chunk := range chunks {
		prompt := strings.ReplaceAll(def.PromptTmpl, "{{CHUNK}}", chunk)
		prompt = strings.ReplaceAll(prompt, "{{REPO}}", in.Repo)
		prompt = strings.ReplaceAll(prompt, "{{NUMBER}}", fmt.Sprintf("%d", in.Number))
		prompt = strings.ReplaceAll(prompt, "{{INDEX}}", fmt.Sprintf("%d", i+1))
		prompt = strings.ReplaceAll(prompt, "{{TOTAL}}", fmt.Sprintf("%d", len(chunks)))
		raw, err := runner(ctx, prompt, nil, "")
		if err != nil {
			// A single chunk failing should not kill the whole review.
			allFindings = append(allFindings, reviewFinding{
				File: fmt.Sprintf("chunk-%d", i+1), Severity: "info",
				Message: "sub-agent failed: " + err.Error(),
			})
			continue
		}
		decoded, err := decodeReviewSubAgentOutput(raw)
		if err != nil {
			allFindings = append(allFindings, reviewFinding{
				File: fmt.Sprintf("chunk-%d", i+1), Severity: "info",
				Message: "decode failed: " + err.Error(),
			})
			continue
		}
		allFindings = append(allFindings, decoded.Findings...)
	}
	allFindings = dedupeAndSortFindings(allFindings)
	result := reviewResult{Findings: allFindings, ChunksReviewed: len(chunks)}
	payload, _ := json.Marshal(result)
	if len(payload) > SpillThreshold {
		artifact := writeArtifactOrSpill(ctx, in.TaskID, "review-findings", string(payload))
		result.ArtifactRef = artifact.ArtifactRef
		result.Degraded = artifact.Degraded
		// Replace inline findings with a preview; full list is in the artifact.
		result.Findings = compressFindings(allFindings)
	}
	return toJSON(result), nil
}

// compressFindings keeps the top 10 findings by severity as an inline preview
// when the full list has been stored as an artifact.
func compressFindings(all []reviewFinding) []reviewFinding {
	if len(all) <= 10 {
		return all
	}
	preview := make([]reviewFinding, 10)
	copy(preview, all[:10])
	return preview
}

// RunReviewHeadless is a public entry point used by `yanshi pr`.
// It builds a reviewInput from the diff and calls streamReview.
func RunReviewHeadless(ctx context.Context, diff string) (string, error) {
	return streamReview(ctx, reviewInput{
		Diff:   diff,
		TaskID: "headless-review",
	})
}
