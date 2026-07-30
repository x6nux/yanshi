package ctxcompact

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
)

func TestSerializeForSummary_PreservesToolCalls(t *testing.T) {
	// bug① regression: old serialize only wrote Role+Content, dropping ToolCalls/ToolResult
	msgs := []*schema.Message{
		{Role: schema.User, Content: "read compacting.go"},
		{Role: schema.Assistant, Content: "ok", ToolCalls: []schema.ToolCall{
			{ID: "call_1", Function: schema.FunctionCall{Name: "read_file", Arguments: `{"path":"compacting.go"}`}},
		}},
		{Role: schema.Tool, ToolCallID: "call_1", Content: "package eino\n..."},
	}
	got := SerializeForSummary(msgs)
	assert.Contains(t, got, "read_file", "tool name preserved")
	assert.Contains(t, got, "call_1", "tool id preserved")
	assert.Contains(t, got, `"path":"compacting.go"`, "tool args preserved")
	assert.Contains(t, got, "package eino", "tool result content preserved")
}

func TestSerializeForSummary_PreservesReasoning(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.Assistant, Content: "answer", ReasoningContent: "because X"},
	}
	got := SerializeForSummary(msgs)
	assert.Contains(t, got, "because X", "reasoning preserved as [thinking]")
	assert.Contains(t, got, "[thinking]")
}

func TestSerializeForSummary_SkipsEmpty(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.Assistant, Content: ""}, // empty, skipped
		{Role: schema.User, Content: "real"},
	}
	got := SerializeForSummary(msgs)
	assert.NotContains(t, got, "assistant:", "empty msg skipped")
	assert.Contains(t, got, "real")
}

func TestSerializeForSummary_RuneSafeTruncation(t *testing.T) {
	// T3 #1: truncation must not split a multi-byte UTF-8 rune (Chinese tool output).
	long := strings.Repeat("中文", 700) // 1400 runes, multi-byte
	msgs := []*schema.Message{
		{Role: schema.Tool, ToolCallID: "c1", Content: long},
	}
	got := SerializeForSummary(msgs)
	assert.Contains(t, got, "[truncated]")
	assert.True(t, utf8.ValidString(got), "output is valid UTF-8 (no mid-rune cut)")
}

// TestSerializeForSummary_NilMessage covers skipping nil messages.
func TestSerializeForSummary_NilMessage(t *testing.T) {
	msgs := []*schema.Message{nil, {Role: schema.User, Content: "real"}, nil}
	got := SerializeForSummary(msgs)
	assert.NotContains(t, got, "[nil]")
	assert.Contains(t, got, "real")
}
