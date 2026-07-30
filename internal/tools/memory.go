package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/store"
)

// MemoryTools exposes memory_search / memory_recall / memory_write as GuardedTools.
type MemoryTools struct {
	store  *store.Store
	Search *GuardedTool
	Recall *GuardedTool
	Write  *GuardedTool
}

// NewMemoryTools builds memory tools backed by s.
func NewMemoryTools(s *store.Store) *MemoryTools {
	mt := &MemoryTools{store: s}

	mt.Search = NewGuardedTool(
		"memory_search", "Memory Search", "Full-text search over stored memories.",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"query": {Type: schema.String, Desc: "search terms", Required: true},
			"limit": {Type: schema.Integer, Desc: "max results (default 10)"},
		}),
		SyncStream(mt.runSearch),
	)

	mt.Recall = NewGuardedTool(
		"memory_recall", "Memory Recall", "Return most-recent memories.",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"limit": {Type: schema.Integer, Desc: "max results (default 10)"},
		}),
		SyncStream(mt.runRecall),
	)

	mt.Write = NewGuardedTool(
		"memory_write", "Memory Write", "Store a memory for later recall.",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"content": {Type: schema.String, Desc: "memory text", Required: true},
			"kind":    {Type: schema.String, Desc: "category, e.g. note/pref/fact"},
		}),
		SyncStream(mt.runWrite),
	)

	return mt
}

// Tools returns all memory tools as a slice for convenience.
func (m *MemoryTools) Tools() []*GuardedTool {
	return []*GuardedTool{m.Search, m.Recall, m.Write}
}

// --- arg types ---

type searchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

type recallArgs struct {
	Limit int `json:"limit"`
}

type writeArgs struct {
	Content string `json:"content"`
	Kind    string `json:"kind"`
}

// --- run functions ---

func (m *MemoryTools) runSearch(_ context.Context, argsJSON string) (string, error) {
	var a searchArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	ms, err := m.store.SearchMemory(a.Query, a.Limit)
	if err != nil {
		return "", err
	}
	return formatMemories(ms), nil
}

func (m *MemoryTools) runRecall(_ context.Context, argsJSON string) (string, error) {
	var a recallArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	ms, err := m.store.RecallMemory(a.Limit)
	if err != nil {
		return "", err
	}
	return formatMemories(ms), nil
}

func (m *MemoryTools) runWrite(_ context.Context, argsJSON string) (string, error) {
	var a writeArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	kind := a.Kind
	if kind == "" {
		kind = "note"
	}
	id, err := m.store.WriteMemory(kind, a.Content)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Stored as %s [%s]", id, kind), nil
}

// formatMemories renders a slice of Memory as human-readable text. Each entry
// shows kind, date, and content lines. Multiple entries are separated by a blank
// line. The caller (TUI transcript) already wraps the result in a tool-entry
// block with expand/collapse (ctrl+o), so we return the full text here and let
// the rendering layer handle truncation.
func formatMemories(ms []store.Memory) string {
	if len(ms) == 0 {
		return "No memories found."
	}
	var b strings.Builder
	for i, m := range ms {
		if i > 0 {
			b.WriteByte('\n')
		}
		// Header line: [kind] 2006-01-02
		ts := time.Unix(m.CreatedAt, 0).Format("2006-01-02")
		b.WriteString(fmt.Sprintf("[%s] %s\n", m.Kind, ts))
		// Content body: indented, preserves line breaks
		body := strings.TrimRight(m.Content, "\n")
		if body != "" {
			indented := "    " + strings.ReplaceAll(body, "\n", "\n    ")
			b.WriteString(indented)
		}
	}
	return b.String()
}
