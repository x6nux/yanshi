package goalloop

import (
	"context"
	"fmt"
	"strings"
)

// AggregateJudge combines all evaluator verdicts into a single Decision.
// Complete = all verdicts pass; Gaps = concatenation of all failing verdicts' gaps.
type AggregateJudge struct{}

// Judge returns a Decision based on the aggregate of all verdicts.
func (AggregateJudge) Judge(_ context.Context, verdicts []EvalVerdict) (Decision, error) {
	passCount := 0
	var gaps []string

	for _, v := range verdicts {
		if v.Pass {
			passCount++
		} else {
			gaps = append(gaps, v.Gaps...)
		}
	}

	total := len(verdicts)
	if total == 0 {
		return Decision{
			Complete: false,
			Summary:  "no evaluators configured",
		}, nil
	}

	complete := passCount == total

	var summary string
	if complete {
		summary = fmt.Sprintf("all %d evaluators passed", total)
	} else {
		summary = fmt.Sprintf("%d/%d evaluators passed, gaps: %s", passCount, total, strings.Join(gaps, "; "))
	}

	return Decision{
		Complete: complete,
		Gaps:     gaps,
		Summary:  summary,
	}, nil
}
