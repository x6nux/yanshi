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
}

// streamReview is the core pipeline:
//  1. chunk diff at 48 KiB hunk-safe boundaries
//  2. dispatch each chunk to the review sub-agent via SubAgentRunnerFromContext
//  3. decode each JSON response
//  4. dedupe + sort findings
//  5. writeArtifactOrSpill if total serialized size exceeds SpillThreshold
func streamReview(ctx context.Context, in reviewInput) (string, error) {
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
