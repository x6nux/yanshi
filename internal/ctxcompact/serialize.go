package ctxcompact

import (
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
)

// toolResultSnippetChars caps each tool_result's content in the transcript so a
// single huge file read can't blow the summary model's window from one message.
const toolResultSnippetChars = 1200

// SerializeForSummary flattens msgs into a text transcript for the summarization
// prompt. Unlike the prior Role+Content-only serializer, it preserves ToolCalls
// (name+id+arguments), tool results (id+content), and reasoning — without these
// the summary model literally cannot see what tools did (bug①). Empty messages
// (no Content, no ToolCalls, no ReasoningContent) are skipped.
func SerializeForSummary(msgs []*schema.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		if m == nil {
			continue
		}
		writeMessageTranscript(&b, m)
	}
	return b.String()
}

func writeMessageTranscript(b *strings.Builder, m *schema.Message) {
	role := string(m.Role)
	hasContent := m.Content != ""
	hasReasoning := m.ReasoningContent != ""
	hasToolCalls := len(m.ToolCalls) > 0
	isToolResult := m.Role == schema.Tool && m.ToolCallID != ""
	if !hasContent && !hasReasoning && !hasToolCalls && !isToolResult {
		return // skip blank turns
	}
	if hasContent {
		b.WriteString("[" + role + "]: " + m.Content + "\n")
	}
	if hasReasoning {
		b.WriteString("[thinking] " + m.ReasoningContent + "\n")
	}
	for _, tc := range m.ToolCalls {
		b.WriteString("  [tool_call: " + tc.Function.Name + " id=" + tc.ID + "] " + tc.Function.Arguments + "\n")
	}
	if isToolResult {
		snippet := m.Content
		// rune-safe truncation: byte-slicing could split a multi-byte UTF-8
		// sequence (yanshi is Chinese-friendly; tool results often carry
		// non-ASCII), producing invalid UTF-8 that confuses the summary model.
		if utf8.RuneCountInString(snippet) > toolResultSnippetChars {
			snippet = string([]rune(snippet)[:toolResultSnippetChars]) + "\n…[truncated]"
		}
		b.WriteString("[tool_result: " + m.ToolCallID + "] " + snippet + "\n")
	}
}
