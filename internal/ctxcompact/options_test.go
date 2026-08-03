// internal/ctxcompact/options_test.go
package ctxcompact_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/x6nux/yanshi/internal/ctxcompact"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// TestModelSummarizer_FakeModelSatisfies is a compile-time check: if FakeModel
// (or any model.BaseChatModel) ever stops satisfying ModelSummarizer, this
// package fails to compile. The test body exists only to make it runnable.
func TestModelSummarizer_FakeModelSatisfies(t *testing.T) {
	var _ ctxcompact.ModelSummarizer = einollm.NewFakeModel(nil, nil)
}

func TestPlanResult_ZeroValue(t *testing.T) {
	var p ctxcompact.PlanResult
	assert.Empty(t, p.PinnedIndices)
	assert.Empty(t, p.SummarizeIndices)
	assert.Empty(t, p.WorkingSetPaths)
}
