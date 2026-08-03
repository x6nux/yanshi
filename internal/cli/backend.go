// Package cli holds the yanshi interactive client: backend transport
// selection (WebSocket primary, SSE fallback), session discovery/bootstrap,
// and the TUI entry point.
package cli

import (
	"context"
	"encoding/json"

	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/task/work"
)

// StreamEvent is one item from a backend stream. Kind is one of:
// agent_chunk, thinking, tool_call, tool_result, error, done, structured_result
// (turn events), plus the Phase-10 control replies: models, status, mcp_list,
// permission_request, compact_chunk, sessions, session_restored. The connection
// persists across Send calls (multi-turn) for WS backends; SSE backends replay
// the full client-held history each turn.
type StreamEvent struct {
	Kind       string
	Text       string
	ToolName   string
	ToolArgs   string
	ToolStatus string
	Overwrite  bool
	Err        error

	// Phase-10 control-reply fields (T32-T35). Populated by toStreamEvent for
	// the corresponding server frames; zero for ordinary turn events.
	ID               string                  // permission_request: id to echo back in the response
	Reason           string                  // permission_request: why the static profile would deny
	ApprovalRequired bool                    // permission_request: must be explicit one-shot allow/deny (no always_allow)
	Model            string                  // status / session_restored: active model name
	Thinking         string                  // status / session_restored: active reasoning effort
	PermMode         string                  // status: permission mode (default|allow-edits|yolo|auto)
	AutoThreshold    int                     // status: auto-mode risk ceiling (1-10)
	TokensIn         int                     // status / session_restored: cumulative input tokens
	TokensOut        int                     // status / session_restored: cumulative output tokens
	CachedTokens     int                     // status: cumulative prompt-cache hits
	ReasoningTokens  int                     // status: cumulative reasoning tokens
	Turns            int                     // status / session_restored: turn count
	ContextWindow    int                     // status: compaction context-window budget (tokens)
	Items            []string                // models / mcp_list: the listed names
	MCPServers       []proto.MCPServerStatus `json:"mcp_servers,omitempty"` // mcp_status: server status snapshots

	Compacted    bool
	TokensBefore int
	TokensAfter  int

	RetryAttempt int
	RetryMax     int
	RetryDelayMs int

	Sessions  []proto.SessionInfo `json:"sessions,omitempty"`
	SessionID string              `json:"session_id,omitempty"`
	// MemoryPath is the active memory file path (MEM1), carried on status
	// frames so the TUI can display it.
	MemoryPath string `json:"memory_path,omitempty"`
	// LogPath is the structured-log file path (empty = stderr), carried on
	// status frames so the TUI /logs command tails the right file.
	LogPath string `json:"log_path,omitempty"`
	// SideDepth is the current ephemeral side-conversation depth (V11): 0 = main,
	// 1+ = inside a side. Carried on status / side_state frames.
	SideDepth int `json:"side_depth,omitempty"`
	// Skills carries the list on skills_list frames (E03).
	Skills []proto.SkillInfo `json:"skills,omitempty"`
	// Skill carries one ack subject on skill_ack; may be nil when the target
	// no longer exists (for example, a successful uninstall). CB4: StreamEvent
	// does NOT have a Name field — the ack name comes from Skill.Name.
	Skill    *proto.SkillInfo `json:"skill,omitempty"`
	Action   string           `json:"action,omitempty"`
	Messages []MessageStub    `json:"messages,omitempty"`

	StructuredResult json.RawMessage

	// COST1 fields. CostKnown=false means N/A. Populated from ServerFrame in
	// wsbackend.toStreamEvent; SSE populates these from the status frame's
	// cost_usd / cost_known columns too (so /cost works on both transports).
	CostUSD   float64
	CostKnown bool

	// Features carries the /features reply rows (OBS3). Populated for both
	// the "features" reply frame and status frames (which may carry a current
	// snapshot of the registry on reconnect).
	Features []proto.FeatureRow

	Permissions []proto.PermissionInfo
	Jobs        []proto.JobInfo

	// A2 task/plan/checklist frames.
	Task      *work.WorkTask
	TaskID    string
	Checklist *work.Checklist

	// Seam fields (B2-RB1): populated by toStreamEvent for seams /
	// seam_restored control replies. Seams carries the list payload;
	// CommitShort is display-only; Head is the FULL main_head id the TUI
	// caches as the next restore_turn's ConfirmedHead (D6); UndoSeamID is
	// the seam_restored reply's ID (undo seam pointer).
	Seams       []proto.SeamInfo
	CommitShort string
	Head        string
	UndoSeamID  string
}

// MessageStub is a lightweight message role+content pair for session restoration.
type MessageStub struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatBackend is the TUI's transport abstraction. Send delivers one user turn
// and returns a channel of events closed when the turn ends (done/error).
// SendFrame writes a Phase-10 control frame (set_model / list_models / clear /
// get_status / compact / list_mcp / permission_response): for request frames it
// returns a channel receiving the reply (closed on the reply / error); for
// permission_response (a mid-turn reply with no direct ack) it returns nil.
// Cancel aborts the in-flight turn. Mode reports the transport ("ws"/"sse"/"fake").
type ChatBackend interface {
	Send(ctx context.Context, text string) (<-chan StreamEvent, error)
	SendFrame(ctx context.Context, f proto.ClientFrame) (<-chan StreamEvent, error)
	Cancel() error
	Close() error
	Mode() string
}
