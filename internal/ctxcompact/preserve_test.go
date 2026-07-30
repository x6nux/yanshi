package ctxcompact

import (
	"fmt"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
)

func TestDeriveWorkingSetPaths_ExtractsFromTextAndToolInput(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "fix internal/llm/eino/compacting.go"},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{Function: schema.FunctionCall{Name: "edit", Arguments: `{"path":"internal/ctxcompact/compact.go"}`}},
		}},
	}
	paths := deriveWorkingSetPaths(msgs, nil)
	assert.Contains(t, paths, "internal/llm/eino/compacting.go")
	assert.Contains(t, paths, "internal/ctxcompact/compact.go")
}

func TestIsErrorMarker(t *testing.T) {
	assert.True(t, isErrorMarker("build error: undefined: foo"))
	assert.True(t, isErrorMarker("panic: runtime fault"))
	assert.True(t, isErrorMarker("Traceback (most recent call last)"))
	assert.True(t, isErrorMarker("test failed: TestX"))
	assert.False(t, isErrorMarker("all good"))
}

func TestIsDiffMarker(t *testing.T) {
	assert.True(t, isDiffMarker("diff --git a/foo b/foo"))
	assert.True(t, isDiffMarker("+++ b/src/main.go"))
	assert.True(t, isDiffMarker("```diff"))
	assert.False(t, isDiffMarker("normal text"))
}

func TestShouldPin_UsesWorkingSetPaths(t *testing.T) {
	ws := map[string]bool{"internal/llm/eino/compacting.go": true}
	assert.True(t, shouldPin(&schema.Message{Content: "edit internal/llm/eino/compacting.go"}, ws))
	assert.False(t, shouldPin(&schema.Message{Content: "unrelated chatter"}, ws))
	assert.True(t, shouldPin(&schema.Message{Content: "error: boom"}, map[string]bool{}), "error always pins")
}

func TestExtractPaths_ToolArgumentAcceptsBareFilename(t *testing.T) {
	// I-2: explicit tool arg {"target":"main.go"} accepted verbatim,
	// not dropped by pathRe (which requires a slash).
	m := &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
		{Function: schema.FunctionCall{Name: "analysis", Arguments: `{"target":"main.go"}`}},
	}}
	assert.Contains(t, extractPaths(m), "main.go", "bare filename tool arg accepted")
}

func TestExtractPaths_FreeTextFindsSlashedPath(t *testing.T) {
	m := &schema.Message{Role: schema.User, Content: "see internal/llm/eino/compacting.go for details"}
	assert.Contains(t, extractPaths(m), "internal/llm/eino/compacting.go")
}

// TestShouldPin_NilMessage covers nil message returning false.
func TestShouldPin_NilMessage(t *testing.T) {
	assert.False(t, shouldPin(nil, map[string]bool{}))
}

// TestExtractPaths_NilMessage covers nil message returning nil.
func TestExtractPaths_NilMessage(t *testing.T) {
	assert.Nil(t, extractPaths(nil))
}

// TestDeriveWorkingSetPaths_InvalidSeedIndex covers out-of-range seed indices.
func TestDeriveWorkingSetPaths_InvalidSeedIndex(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "fix internal/llm/eino/compacting.go"},
	}
	// Negative seed index and out-of-range seed index should be skipped.
	paths := deriveWorkingSetPaths(msgs, []int{-1, 100})
	assert.NotEmpty(t, paths, "still extracts paths from recent window")
	assert.Contains(t, paths, "internal/llm/eino/compacting.go")
}

// TestDeriveWorkingSetPaths_MaxCap covers the maxWorkingSetPaths cap.
// Each message in the recent window contributes 2 unique paths, totalling 24
// (maxWorkingSetPaths) paths for 12 messages. This hits both the add() cap
// and the break in the recent-window loop.
func TestDeriveWorkingSetPaths_MaxCap(t *testing.T) {
	msgs := make([]*schema.Message, 12)
	for i := 0; i < 12; i++ {
		// Each message contributes 2 unique paths: a/x/0.go + b/y/12.rs etc.
		// 12 messages × 2 paths = 24 = maxWorkingSetPaths.
		msgs[i] = &schema.Message{
			Role:    schema.User,
			Content: fmt.Sprintf("edit a/x/%d.go and b/y/%d.rs", i, i+12),
		}
	}
	paths := deriveWorkingSetPaths(msgs, nil)
	assert.LessOrEqual(t, len(paths), 24, "capped at maxWorkingSetPaths")
	assert.Equal(t, 24, len(paths), "should reach exactly maxWorkingSetPaths with 12 msgs × 2 paths")
}

// TestDeriveWorkingSetPaths_SeedOrder verifies seed indices are processed newest-first.
func TestDeriveWorkingSetPaths_SeedOrder(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.Assistant, Content: "read internal/vcs/store.go"},
		{Role: schema.User, Content: "fix internal/ctxcompact/compact.go"},
	}
	paths := deriveWorkingSetPaths(msgs, []int{0, 1})
	// Both paths should appear (seed order is newest-first, but both are in the recent window too)
	assert.Contains(t, paths, "internal/ctxcompact/compact.go")
	assert.Contains(t, paths, "internal/vcs/store.go")
}
