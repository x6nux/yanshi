// internal/proto/frame.go
// Package proto defines the JSON frames exchanged over the chat WebSocket and
// the SSE chat endpoint — both transports share this one event vocabulary. It
// has no dependency on net/http or the cli, so both the server handler and the
// client use it.
package proto

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/task/work"
)

// ClientFrame is a client->server frame.
//
//	type                 meaning
//	user_message         a user turn; Text is the prompt (may start with "/skill …")
//	cancel               cancel the in-flight turn
//	set_model            switch the session model; Name is the provider name
//	set_thinking         set reasoning effort; Effort is low|medium|high|off
//	set_mode             switch the permission mode; Mode is default|allow-edits|yolo|auto
//	clear                reset the conversation history and usage counters
//	list_models          request the available model names (reply: models)
//	get_status           request session status (reply: status)
//	permission_response  answer a permission_request; ID matches the request, Decision is allow|deny|always_allow
//	compact              compact the conversation context
//	session_list         request the stored ACTIVE sessions (reply: sessions)
//	restore_session      restore a prior session by id; ID is the session id (reply: session_restored)
//	rename_session       rename a stored session's title; ID is the session id, Text the new title
//	archive_session      hide a stored session from the active list; ID is the session id
//	unarchive_session    restore an archived session to the active list; ID is the session id
//	delete_session       permanently delete a stored session + its messages; ID is the session id
//	session_list_archived request the ARCHIVED sessions (reply: sessions)
//	features_list        request the runtime feature flag table (reply: features)
//	features_set         toggle one flag; Enabled *bool so false is serialised (reply: features)
type ClientFrame struct {
	Type     string `json:"type"`               // user_message|cancel|set_model|set_thinking|set_mode|clear|list_models|get_status|permission_response|compact|list_mcp|mcp_action
	Text     string `json:"text,omitempty"`     // user_message
	Name     string `json:"name,omitempty"`     // set_model
	Effort   string `json:"effort,omitempty"`   // set_thinking: low|medium|high|off
	Mode     string `json:"mode,omitempty"`     // set_mode: default|allow-edits|yolo|auto
	ID       string `json:"id,omitempty"`       // permission_response
	Decision string `json:"decision,omitempty"` // permission_response: allow|deny|always_allow
	// OutputSchema carries an optional JSON Schema for a user_message turn. When
	// non-empty the server validates the model's final output against it and
	// emits a structured_result frame (A12-core); when empty/absent the turn is
	// plain text (text mode, byte-identical to pre-A12). omitempty keeps legacy
	// clients and the no-schema path unchanged on the wire.
	OutputSchema json.RawMessage `json:"output_schema,omitempty"` // user_message
	// ConfirmedHead carries the FULL main_head id the client observed when listing
	// seams. restore_turn re-sends it so the server can reject a stale target.
	// Empty is invalid and is rejected fail-closed (D6).
	ConfirmedHead string `json:"confirmed_head,omitempty"` // restore_turn
	MCPServer     string `json:"mcp_server,omitempty"`
	MCPAction     string `json:"mcp_action,omitempty"`
	// Seq selects the inclusive upper bound for fork_session (-1 = all messages,
	// >=0 = up to that seq). Other frames leave it zero.
	Seq int `json:"seq,omitempty"`
	// Source carries the install source for skill install requests
	// (e.g. "github:owner/repo"). Other frames leave it empty.
	Source string `json:"source,omitempty"`
	// FeaturesSet carries a /features toggle (OBS3). Enabled is *bool so that
	// false is serialized on the wire — a bare bool with omitempty would drop
	// "off" toggles, making it impossible for the server to distinguish
	// "explicitly disabled" from "absent". Constraint 8 in the C4 plan.
	FeaturesSet *FeaturesSetPayload `json:"features_set,omitempty"`

	// Images carries image attachments for a user_message turn (Tier G).
	// Each entry is one image (id assigned client-side or by the server image store,
	// base64 bytes, fmt, dims). Empty/absent = text-only turn, byte-identical to
	// pre-G on the wire (omitempty drops it). ADDITIVE: existing clients/frames
	// are unchanged.
	Images []ImageAttach `json:"images,omitempty"` // user_message

	// Attachments are @path file references for this turn (UX3). The client
	// sends PATHS ONLY: the server reads the bytes, inside the work root and
	// through the same guard profile a tool call would face.
	//
	// Sending content instead was the rejected design. A client-supplied blob
	// is unauthenticated input that has already escaped every filesystem
	// boundary by the time the server sees it — the server could not tell an
	// attachment the user picked from one an injected prompt fabricated. A
	// path can be checked; bytes cannot.
	//
	// Empty/absent = a turn with no attachments, byte-identical on the wire.
	Attachments []AttachRef `json:"attachments,omitempty"` // user_message
}

// AttachRef is one @path reference. It carries a path and nothing else, by
// design — see ClientFrame.Attachments.
type AttachRef struct {
	Path string `json:"path"`
}

// FeaturesSetPayload is the body of a features_set frame. See ClientFrame.
// FeaturesSet doc for the *bool rationale.
type FeaturesSetPayload struct {
	Key     string `json:"key"`
	Enabled *bool  `json:"enabled"`
}

// ImageAttach is the transport-level representation of one image attachment,
// shared by proto.ClientFrame (WS/SSE) and the v1 Agent API. Field names are
// camelCase on the wire. DataB64 is standard base64 of the raw image bytes;
// ID is the stable image id (img-N); W/H are display dims for the placeholder.
type ImageAttach struct {
	ID      string `json:"id,omitempty"`
	Source  string `json:"source,omitempty"`
	Fmt     string `json:"fmt,omitempty"`
	W       int    `json:"w,omitempty"`
	H       int    `json:"h,omitempty"`
	DataB64 string `json:"dataB64,omitempty"`
}

// NewUserMessage builds a user_message frame.
func NewUserMessage(text string) ClientFrame { return ClientFrame{Type: "user_message", Text: text} }

// NewUserMessageWithSchema builds a user_message frame that also declares a
// per-turn JSON Schema for structured output (A12-core). schemaDoc is a JSON
// Schema document (e.g. `{"type":"object",...}`); when the server receives this
// frame it validates the model's final assistant text against schemaDoc and, on
// success, emits a structured_result frame carrying the parsed JSON. Use
// NewUserMessage for the plain text path — the wire form of a no-schema
// user_message stays byte-identical to pre-A12 (see ClientFrame.OutputSchema).
func NewUserMessageWithSchema(text string, schemaDoc json.RawMessage) ClientFrame {
	return ClientFrame{Type: "user_message", Text: text, OutputSchema: schemaDoc}
}

// NewUserMessageWithImages builds a user_message frame carrying image
// attachments (Tier G). images may be nil/empty (equivalent to NewUserMessage).
// ADDITIVE: the wire form of an images-less user_message is unchanged.
func NewUserMessageWithImages(text string, images []ImageAttach) ClientFrame {
	return ClientFrame{Type: "user_message", Text: text, Images: images}
}

// NewUserMessageWithAttachments builds a user_message carrying @path
// references. Mirrors NewUserMessageWithImages; empty refs produce a frame
// indistinguishable from a plain text turn.
func NewUserMessageWithAttachments(text string, refs []AttachRef) ClientFrame {
	return ClientFrame{Type: "user_message", Text: text, Attachments: refs}
}

// NewCancel builds a cancel frame.
func NewCancel() ClientFrame { return ClientFrame{Type: "cancel"} }

// Phase-10 control-frame constructors.
// NewSetModel builds a frame that switches models mid-session.
func NewSetModel(name string) ClientFrame { return ClientFrame{Type: "set_model", Name: name} }

// NewSetThinking builds a frame that requests extended thinking mode.
func NewSetThinking(effort string) ClientFrame {
	return ClientFrame{Type: "set_thinking", Effort: effort}
}

// NewClear builds a frame that clears the current conversation.
func NewClear() ClientFrame { return ClientFrame{Type: "clear"} }

// NewListModels builds a frame that requests the list of available models.
func NewListModels() ClientFrame { return ClientFrame{Type: "list_models"} }

// NewGetStatus builds a frame that requests backend status.
func NewGetStatus() ClientFrame { return ClientFrame{Type: "get_status"} }

// NewSetMode switches the session's interactive permission mode. mode is
// "default"|"allow-edits"|"yolo"|"auto".
func NewSetMode(mode string) ClientFrame {
	return ClientFrame{Type: "set_mode", Mode: mode}
}

// NewPermissionResponse answers a permission_request; decision is
// "allow"|"deny"|"always_allow".
func NewPermissionResponse(id, decision string) ClientFrame {
	return ClientFrame{Type: "permission_response", ID: id, Decision: decision}
}

// NewCompact requests context compaction of the current history.
func NewCompact() ClientFrame { return ClientFrame{Type: "compact"} }

// NewListMCP requests the MCP server snapshot. The reply is mcp_status
// (NewMCPStatusFrame) -- rich per-server state including tools and resources.
//
// It is NOT mcp_list. That frame type had a constructor and a round-trip test
// and no server ever built one, so no client could receive it; the doc here
// promised it anyway, which is how a reader would have wired a handler for a
// frame that never arrives. Both the constructor and the promise are gone.
//
// Sent by the TUI at launch and by the /mcp command.
func NewListMCP() ClientFrame { return ClientFrame{Type: "list_mcp"} }

// NewSessionList requests the list of stored sessions (reply: sessions).
func NewSessionList() ClientFrame { return ClientFrame{Type: "session_list"} }

// NewRestoreSession requests restoration of a prior session by its ID (reply:
// session_restored). The server loads the session's messages and model config
// and populates the current connSession with them.
func NewRestoreSession(id string) ClientFrame {
	return ClientFrame{Type: "restore_session", ID: id}
}

// NewRenameSession renames a stored session's title. id is the session id; title
// is the new title (server trims/clamps). Reply: session_ack{action:"renamed"}.
func NewRenameSession(id, title string) ClientFrame {
	return ClientFrame{Type: "rename_session", ID: id, Text: title}
}

// NewArchiveSession hides a stored session from the active list (archived=true)
// without deleting it. Reply: session_ack{action:"archived"}.
func NewArchiveSession(id string) ClientFrame {
	return ClientFrame{Type: "archive_session", ID: id}
}

// NewUnarchiveSession restores an archived session to the active list. Reply:
// session_ack{action:"unarchive"}.
func NewUnarchiveSession(id string) ClientFrame {
	return ClientFrame{Type: "unarchive_session", ID: id}
}

// NewDeleteSession permanently deletes a stored session and its messages. The
// TUI gates this behind a confirmation token (/delete <id> yes) — the server
// executes unconditionally. Reply: session_ack{action:"deleted"}.
func NewDeleteSession(id string) ClientFrame {
	return ClientFrame{Type: "delete_session", ID: id}
}

// NewSessionListArchived requests the ARCHIVED sessions (reply: sessions). Used
// by /archived so the user can discover IDs to unarchive. Mirrors NewSessionList
// (which returns active sessions).
func NewSessionListArchived() ClientFrame { return ClientFrame{Type: "session_list_archived"} }

// NewFeaturesList requests the runtime feature flag table (reply: features).
// The server replies with NewFeaturesReply(rows) listing every registered flag
// with its current value, stage, and owner. SSE backends return a distinct
// error (sseBackend.SendFrame) because the SSE transport is stateless and
// cannot service a control frame.
func NewFeaturesList() ClientFrame { return ClientFrame{Type: "features_list"} }

// NewFeaturesSet toggles one flag at runtime. key is the exact flag name
// ("observe.otel_export" etc.). enabled is carried as a *bool on the wire so
// that explicit false survives JSON omitempty (constraint 8 in the C4 plan) —
// otherwise the server could not distinguish "explicitly disabled" from
// "absent" and a toggle-off would silently no-op.
func NewFeaturesSet(key string, enabled bool) ClientFrame {
	e := enabled
	return ClientFrame{Type: "features_set", FeaturesSet: &FeaturesSetPayload{Key: key, Enabled: &e}}
}

// SessionInfo carries one session for the sessions list response (type "sessions").
type SessionInfo struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	MsgCount  int    `json:"msg_count"`
	Model     string `json:"model,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	TokensIn  int    `json:"tokens_in,omitempty"`
	TokensOut int    `json:"tokens_out,omitempty"`
	// CachedTokens / ReasoningTokens (Task A6) mirror the stored session row so
	// the /sessions list can render the same usage breakdown as the footer.
	CachedTokens    int `json:"cached_tokens,omitempty"`
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
	Turns           int `json:"turns,omitempty"`
	// CostUSD / CostKnown (C4 COST1) mirror the stored session row's cumulative
	// USD ledger. CostKnown=false means the session used at least one model
	// absent from the pricing table; renderers must show "N/A", not "$0".
	CostUSD   float64 `json:"cost_usd,omitempty"`
	CostKnown bool    `json:"cost_known,omitempty"`
}

// SeamInfo carries one seam row for the seams list response (type "seams").
// ID is the exact seam id (B2-RB1 必修项 J: selector 只用 seam_id, 没有 by-N /
// turn_id). CommitShort is the first 8 hex of the referenced commit, for
// display only.
type SeamInfo struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	TurnSeq     int    `json:"turn_seq,omitempty"`
	HistoryLen  int    `json:"history_len,omitempty"`
	CommitShort string `json:"commit_short,omitempty"`
	Label       string `json:"label,omitempty"`
	CreatedAt   int64  `json:"created_at,omitempty"`
}

// ServerFrame is a server->client frame.
//
//	type                fields
//	agent_chunk         Text (assistant text delta for this turn)
//	thinking            Text (one reasoning delta from a reasoning model — schema.Message.ReasoningContent)
//	tool_call           ToolName, ToolArgs, Status("running"|"ok"|"error")
//	tool_result         ToolName, Text(result), Status
//	error               Text
//	done                (turn complete)
//	models              Names (available provider names; reply to list_models)
//	status              Model, Thinking, TokensIn, TokensOut, CachedTokens, ReasoningTokens, Turns, ContextWindow (reply to get_status / sent after control frames)
//	permission_request  ID, ToolName, ToolArgs, Reason (asks the user to approve a tool call)
//	mcp_list            Servers (MCP server names)
//	compact_chunk       Text (one delta of an in-progress compaction summary — Task 35b)
//	history_replaced    Messages (compacted history for SSE clients to adopt — Task 35b)
//	sessions            Sessions (list of stored sessions; reply to session_list)
//	session_restored    Messages, Model, Thinking, TokensIn, TokensOut, Turns, SessionID (restored session state)
//	session_ack         Action, SessionID, Text (ack for rename/archive/unarchive/delete)
//	nested_usage        Tokens (sub-agent's total token spend — emitted by runSubAgentTurn so the
//	                     TUI can render "Done (… · Nk tokens · …)" on the Analysis block's summary)
//	nested_progress     Done, Total (workflow sub-agent completion progress — emitted by the
//	                     workflow_start tool after each task's runSubAgent returns, so the TUI can
//	                     render "Done (N/M agents · …)" on the workflow block's summary)
//	structured_result   StructuredResult (validated JSON for a schema-constrained turn — A12-core)
//	task_update         Task (full WorkTask snapshot) — A2: emitted by durable task tools
//	plan_update         TaskID, Checklist (replaced checklist) — A2: emitted by update_plan
//	checklist_update    TaskID, Checklist (appended/patched checklist) — A2: emitted by add/update
type ServerFrame struct {
	Type      string   `json:"type"`
	Text      string   `json:"text,omitempty"`
	ToolName  string   `json:"tool_name,omitempty"`
	ToolArgs  string   `json:"tool_args,omitempty"`
	Status    string   `json:"status,omitempty"`
	Overwrite bool     `json:"overwrite,omitempty"`
	Names     []string `json:"names,omitempty"`    // models
	Model     string   `json:"model,omitempty"`    // status / session_restored
	Thinking  string   `json:"thinking,omitempty"` // status / session_restored
	// PermMode is the session's interactive permission mode (default|allow-edits|
	// yolo|auto), carried on status frames so the client footer reflects it.
	PermMode  string `json:"perm_mode,omitempty"`  // status
	TokensIn  int    `json:"tokens_in,omitempty"`  // status / session_restored
	TokensOut int    `json:"tokens_out,omitempty"` // status / session_restored
	// CachedTokens / ReasoningTokens (Task A6) break out the API's
	// prompt-cache-hit and reasoning-model spend so /cost can surface them
	// separately. Sources: schema.TokenUsage.PromptTokenDetails.CachedTokens and
	// CompletionTokensDetails.ReasoningTokens respectively. omitempty keeps older
	// clients (and frames emitted before usage arrives) unchanged.
	CachedTokens    int `json:"cached_tokens,omitempty"`    // status
	ReasoningTokens int `json:"reasoning_tokens,omitempty"` // status
	Turns           int `json:"turns,omitempty"`            // status / session_restored
	// ContextWindow is the session's compaction context-window budget (tokens),
	// carried on a status frame so the client can render "ctx: <in>/<window>" in
	// its header. Comes from the server's configured Compaction.ContextWindow.
	ContextWindow    int    `json:"context_window,omitempty"`
	ID               string `json:"id,omitempty"`                // permission_request
	Reason           string `json:"reason,omitempty"`            // permission_request
	ApprovalRequired bool   `json:"approval_required,omitempty"` // permission_request: must be explicit one-shot allow/deny
	// ForcePrompt is the wire half of the server's two force-a-prompt flags:
	// tools.PermissionRequest.ForcePrompt (the forcePromptTools list, e.g.
	// task_cancel) and tools.PermissionRequest.Force (RequireApproval, e.g.
	// revert_turn). Both mean the same thing to a client: this request may NOT
	// be auto-approved or pre-approved — only an explicit per-call decision
	// counts. Without it on the wire the server's refusal to auto-resolve is
	// silently undone client-side (the TUI's autoResolvePendingByMode answered
	// "allow" on the user's behalf the moment they switched to YOLO).
	ForcePrompt bool     `json:"force_prompt,omitempty"` // permission_request: cannot be auto-approved or pre-approved
	Servers     []string `json:"servers,omitempty"`      // mcp_list
	// Compaction fields (Task 35b). Compacted is set on a status frame after a
	// compaction completed; TokensBefore/After carry the estimated token counts
	// across the compaction so the client can render "compacted (X → Y tokens)".
	Compacted    bool             `json:"compacted,omitempty"`
	TokensBefore int              `json:"tokens_before,omitempty"`
	TokensAfter  int              `json:"tokens_after,omitempty"`
	Messages     []schema.Message `json:"messages,omitempty"` // history_replaced / session_restored
	// Retry fields: carried by a "retry" frame announcing a transient-error
	// retry (e.g. a mid-stream "unexpected EOF") so the client can render
	// "↻ retry N/M…" in its activity line. Text carries the triggering error.
	RetryAttempt int `json:"retry_attempt,omitempty"`
	RetryMax     int `json:"retry_max,omitempty"`
	RetryDelayMs int `json:"retry_delay_ms,omitempty"`
	// Sessions is the list of stored sessions (reply to session_list).
	Sessions []SessionInfo `json:"sessions,omitempty"`
	// SessionID is the restored session id (carried on session_restored).
	SessionID string `json:"session_id,omitempty"`
	// MemoryPath carries the active memory file path (MEM1) on a status frame
	// so the TUI can display it in the footer / /memory ack. Empty when MEM1 is
	// disabled or no user/project path is configured.
	MemoryPath string `json:"memory_path,omitempty"` // status
	// LogPath is the structured-log file path on a status frame (empty when
	// logs go to stderr). The TUI /logs command tails this file so the user
	// can inspect runtime logs without leaving the app.
	LogPath string `json:"log_path,omitempty"` // status
	// SideDepth carries the current ephemeral side-conversation depth (V11) on
	// a status frame: 0 (main / omitted) or 1..maxSideDepth. The TUI renders
	// "in side (N)" when > 0.
	SideDepth int `json:"side_depth,omitempty"` // status / side_state
	// Skills (E03) carries the skill list on a skills_list frame. Each entry
	// has Name/Description/Source/Enabled/Trusted. The TUI renders the list
	// for /skills and uses Enabled/Trusted to mark disabled/untrusted skills.
	Skills []SkillInfo `json:"skills,omitempty"` // skills_list
	// Skill carries a single skill's ack info on a skill_ack frame (e.g.
	// install/uninstall/trust/enable/disable result).
	Skill *SkillInfo `json:"skill,omitempty"` // skill_ack
	// Action (V10) carries the mutation kind on a session_ack frame:
	// "renamed" | "archived" | "unarchive" | "deleted". Pair with SessionID and,
	// for rename, Text (the post-rename title).
	Action string `json:"action,omitempty"` // session_ack
	// StructuredResult carries the validated JSON the model produced for a turn
	// that declared an output schema (ClientFrame.OutputSchema, A12-core). Emitted
	// as a dedicated structured_result frame AFTER the turn's agent_chunk stream
	// and BEFORE done, so a consumer (headless exec --output-schema, API client,
	// later the TUI) can take the parsed value directly instead of re-parsing the
	// streamed text. Empty/absent on text-mode turns. Uses json.RawMessage so the
	// payload is embedded as raw JSON on the wire, not an escaped string.
	StructuredResult json.RawMessage `json:"structured_result,omitempty"` // structured_result
	// Seams is the list of recent seams for the current session, reply to
	// list_seams (B2-RB1 必修项 I). Each entry has its own CommitShort for display.
	Seams []SeamInfo `json:"seams,omitempty"`
	// CommitShort carries the short (first-8-hex) hash of the current main_head
	// on status / seams / seam_restored frames, for DISPLAY only.
	CommitShort string `json:"commit_short,omitempty"`
	// Head carries the FULL main_head commit id on seams / seam_restored / status
	// frames. The TUI caches it and re-sends it as the restore_turn frame's
	// ConfirmedHead; the server compares the FULL id (not the short hash) to bind
	// the restore target (B2-RB1 必修项 E / D6: short-hash collision is a real risk
	// across long histories, so the binding must use the untruncated id). Empty
	// when VCS is unconfigured.
	Head string `json:"head,omitempty"`
	// Permissions carries the active approval rules on a "permissions" frame
	// (Task 9 — reply to permissions_list). Each PermissionInfo mirrors one
	// approval.Rule visible to the current session.
	Permissions []PermissionInfo `json:"permissions,omitempty"` // permissions
	// Jobs carries the shell v2 job table on a "jobs" frame (Task 22 — reply
	// to jobs_list). Each JobInfo mirrors one shell.Job the Manager currently
	// tracks (live OR stale after RestoreJobs).
	Jobs Jobs `json:"jobs,omitempty"` // jobs
	// MCPServers carries the mcp_status frame.
	MCPServers []MCPServerStatus `json:"mcp_servers,omitempty"`

	// A2 task/plan/checklist 帧字段。
	Task      *work.WorkTask  `json:"task,omitempty"`
	TaskID    string          `json:"task_id,omitempty"`
	Checklist *work.Checklist `json:"checklist,omitempty"`
	// Subagent lifecycle event fields (B1).
	AgentID     string `json:"agent_id,omitempty"`
	AgentRole   string `json:"agent_role,omitempty"`
	Event       string `json:"event,omitempty"`
	AgentStatus string `json:"agent_status,omitempty"`
	// CostUSD is the per-session cumulative USD estimate (COST1). CostKnown=false
	// indicates the pricing table lacked at least one used model; renderers must
	// show "N/A" rather than "$0.00". omitempty keeps the legacy shape when
	// unset (constraint 11 in the C4 plan: known-and-zero renders as "$0.0000"
	// because the caller also sets CostKnown=true).
	CostUSD float64 `json:"cost_usd,omitempty"`
	// CostKnown pairs with CostUSD: false means "N/A", true means "the CostUSD
	// field is the authoritative estimate".
	CostKnown bool `json:"cost_known,omitempty"`
	// Features carries the OBS3 feature flag rows. Populated on the "features"
	// reply frame (NewFeaturesReply) and optionally on status frames so a
	// reconnecting client can render the current snapshot. omitempty keeps the
	// legacy status shape when no flag table is configured.
	Features []FeatureRow `json:"features,omitempty"`
}

// FeatureRow is the on-the-wire shape of one entry in the features table
// (reply to features_list / features_set). Stage is "stable"|"beta"|
// "experimental" (see internal/features.Stage). Owner is the batch/feature
// that owns the flag so /features can render "ask X about this flag".
type FeatureRow struct {
	Key     string `json:"key"`
	Stage   string `json:"stage"`
	Enabled bool   `json:"enabled"`
	Owner   string `json:"owner,omitempty"`
}

// NewFeaturesReply builds a "features" frame carrying the full flag table.
// Emitted as the reply to features_list AND as the ack for features_set (the
// row reflects the post-toggle value). Emitted as a single-frame control
// reply, so isControlReply closes the client's reply channel on it.
func NewFeaturesReply(rows []FeatureRow) ServerFrame {
	return ServerFrame{Type: "features", Features: rows}
}

// PermissionInfo is the on-the-wire shape of one approval rule. It is a
// flattened / serialized view of approval.Rule: the structured Scope is
// rendered to a JSON string (so the wire form is stable across clients) and
// CreatedAt/ExpiresAt are unix-second integers (zero = unset).
type PermissionInfo struct {
	ID        string `json:"id"`
	Action    string `json:"action"`
	Scope     string `json:"scope"`
	TTL       string `json:"ttl"`
	Source    string `json:"source"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

// JobInfo is the on-the-wire shape of one shell.Job. Mirrors the field set
// the Manager exposes (id/session_id/command/state/output/error/exit_code/pid
// /started_at/ended_at). ExitCode does NOT use omitempty: 0 is a meaningful
// clean-exit value (mirrors shell.Job in Task 16). EndedAt IS omitempty
// because a zero unix value means "not yet ended".
type JobInfo struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Command   string `json:"command"`
	State     string `json:"state"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
	ExitCode  int    `json:"exit_code"` // NOT omitempty (mirrors shell.Job)
	PID       int    `json:"pid"`
	StartedAt int64  `json:"started_at"`
	EndedAt   int64  `json:"ended_at,omitempty"`
}

// Jobs is a slice of JobInfo. Declared as a named type so the ServerFrame
// field can carry it without a pointer indirection and so callers can build
// the slice literal inline.
type Jobs []JobInfo

// NewAgentChunk builds an agent_chunk frame (assistant text delta).
func NewAgentChunk(text string) ServerFrame { return ServerFrame{Type: "agent_chunk", Text: text} }

// NewThinking builds a thinking frame carrying one delta of the model's
// reasoning content (from schema.Message.ReasoningContent, populated by the
// openai acl from the response's reasoning/reasoning_content field). The TUI
// accumulates these into a live "Thinking…" block that collapses to
// "Thought for Xs (ctrl+o to expand)" once reasoning ends. Streaming reasoning
// arrives as a delta per chunk (chat_model.go:1222 — choice.Delta.ReasoningContent,
// parallel to choice.Delta.Content), so one frame per chunk is correct; the TUI
// appends. If a model returns no reasoning, no thinking frames are emitted and
// the block simply never appears (graceful).
func NewThinking(text string) ServerFrame { return ServerFrame{Type: "thinking", Text: text} }

// NewToolCall builds a tool_call frame (ToolArgs is the raw JSON arguments blob).
func NewToolCall(name, args, status string) ServerFrame {
	return ServerFrame{Type: "tool_call", ToolName: name, ToolArgs: args, Status: status}
}

// NewToolResult builds a tool_result frame (Text is the tool's result text).
func NewToolResult(name, text, status string) ServerFrame {
	return ServerFrame{Type: "tool_result", ToolName: name, Text: text, Status: status}
}

// NewToolProgress builds a tool_progress frame carrying one line of a tool's
// live output (e.g. a shell_run stdout line) so the TUI can render a scrolling
// window while the tool runs. text is a single line without a trailing newline.
func NewToolProgress(name, text string) ServerFrame {
	return ServerFrame{Type: "tool_progress", ToolName: name, Text: text}
}

// NewError builds an error frame.
func NewError(text string) ServerFrame { return ServerFrame{Type: "error", Text: text} }

// NewDone builds a done frame (turn complete).
func NewDone() ServerFrame { return ServerFrame{Type: "done"} }

// NewRetry builds a retry frame announcing a transient-error retry of the model
// call (attempt is 1-based, max the cap, delayMs the backoff about to elapse,
// err the triggering error). Emitted by the server before each retry sleep so
// the TUI can render "↻ retry N/M …" in its activity line.
func NewRetry(attempt, max, delayMs int, errMsg string) ServerFrame {
	return ServerFrame{Type: "retry", RetryAttempt: attempt, RetryMax: max, RetryDelayMs: delayMs, Text: errMsg}
}

// Phase-10 server-frame constructors.

// NewModels builds a models frame listing the available provider Names.
func NewModels(names []string) ServerFrame { return ServerFrame{Type: "models", Names: names} }

// NewStatus builds a status frame describing the active session configuration
// and accumulated usage. contextWindow is the compaction context-window budget
// (tokens) so the client can show "ctx: <in>/<window>"; pass 0 to omit.
// permMode/autoThreshold carry the interactive permission mode so the client
// footer can render it (pass "" and 0 to omit).
func NewStatus(model, thinking string, in, out, turns, contextWindow int) ServerFrame {
	return ServerFrame{
		Type:          "status",
		Model:         model,
		Thinking:      thinking,
		TokensIn:      in,
		TokensOut:     out,
		Turns:         turns,
		ContextWindow: contextWindow,
	}
}

// NewStatusWithMode is NewStatus plus the permission mode fields. Used by the WS
// handler's statusFrame so the client footer always shows the active mode.
func NewStatusWithMode(model, thinking string, in, out, turns, contextWindow int, permMode string) ServerFrame {
	return ServerFrame{
		Type:          "status",
		Model:         model,
		Thinking:      thinking,
		PermMode:      permMode,
		TokensIn:      in,
		TokensOut:     out,
		Turns:         turns,
		ContextWindow: contextWindow,
	}
}

// NewPermissionRequest builds a permission_request frame asking the user to
// approve a tool call (ToolArgs is the raw JSON arguments blob). approvalRequired
// is true for mandatory-approval tools (e.g. GitHub mutations) that cannot be
// auto-approved or pre-allowed. forcePrompt carries the same "explicit decision
// only" contract for the server's other two flags (force-prompt tools such as
// task_cancel, and RequireApproval's destructive actions such as revert_turn);
// both booleans must be forwarded, because a client that never sees them will
// happily answer on the user's behalf.
func NewPermissionRequest(id, tool, args, reason string, approvalRequired, forcePrompt bool) ServerFrame {
	return ServerFrame{Type: "permission_request", ID: id, ToolName: tool, ToolArgs: args, Reason: reason, ApprovalRequired: approvalRequired, ForcePrompt: forcePrompt}
}

// NewCompactChunk builds a compact_chunk frame carrying one delta of an
// in-progress compaction summary (streamed by the server during auto-compaction,
// Task 35b). The TUI accumulates these into a live "compacting…" block.
func NewCompactChunk(text string) ServerFrame { return ServerFrame{Type: "compact_chunk", Text: text} }

// NewHistoryReplaced builds a history_replaced frame carrying the compacted
// conversation history (Task 35b). SSE holds history client-side, so after the
// server compacts it must publish the new slice for sseBackend to adopt before
// the next turn. The WebSocket transport holds history server-side and ignores
// this frame. Messages is the full post-compaction history (summary + recent),
// including the user turn that triggered the compaction.
func NewHistoryReplaced(msgs []schema.Message) ServerFrame {
	return ServerFrame{Type: "history_replaced", Messages: msgs}
}

// NewSessions builds a sessions frame listing stored sessions.
func NewSessions(sessions []SessionInfo) ServerFrame {
	return ServerFrame{Type: "sessions", Sessions: sessions}
}

// NewSessionRestored builds a session_restored frame carrying the restored
// session state: the full message history, model/config, and token counters.
func NewSessionRestored(sessionID string, msgs []schema.Message, model, thinking string, tokensIn, tokensOut, turns int) ServerFrame {
	return ServerFrame{
		Type:      "session_restored",
		SessionID: sessionID,
		Messages:  msgs,
		Model:     model,
		Thinking:  thinking,
		TokensIn:  tokensIn,
		TokensOut: tokensOut,
		Turns:     turns,
	}
}

// NewSessionAck builds a session_ack frame acknowledging a session mutation
// (rename/archive/unarchive/delete). action is "renamed"|"archived"|
// "unarchive"|"deleted"; id is the session id; title is the post-rename title
// (pass "" for non-rename actions). Emitted as a single-frame control reply
// (like sessions), so isControlReply closes the client's reply channel on it.
func NewSessionAck(action, id, title string) ServerFrame {
	return ServerFrame{Type: "session_ack", Action: action, SessionID: id, Text: title}
}

// NewStructuredResult builds a structured_result frame carrying the validated
// JSON for a schema-constrained turn (A12-core). data is the raw JSON bytes
// that passed ValidateStructuredOutput against the turn's ClientFrame.OutputSchema.
// Emitted by the WS and SSE handlers AFTER the turn's agent_chunk stream and
// BEFORE the done frame, so a consumer never has to re-parse streamed text.
func NewStructuredResult(data json.RawMessage) ServerFrame {
	return ServerFrame{Type: "structured_result", StructuredResult: data}
}

// NewListPermissions builds a permissions_list control frame: the client asks
// the server for all approval rules visible to the current session. Reply is a
// "permissions" frame (NewPermissions).
func NewListPermissions() ClientFrame { return ClientFrame{Type: "permissions_list"} }

// NewRevokePermission builds a permission_revoke control frame asking the
// server to remove the rule with the given ID. The server replies with a
// permission_rule_hit frame carrying Kind="revoke" on success, or an error
// frame if the rule does not exist / the approval manager is unavailable.
func NewRevokePermission(id string) ClientFrame {
	return ClientFrame{Type: "permission_revoke", ID: id}
}

// NewPermissions builds a "permissions" frame carrying the active approval
// rules. Emitted as the reply to permissions_list (Task 9). Each item is a
// flattened approval.Rule with the structured Scope rendered as a JSON string
// for client portability.
func NewPermissions(items []PermissionInfo) ServerFrame {
	return ServerFrame{Type: "permissions", Permissions: items}
}

// NewPermissionRuleHit builds a permission_rule_hit frame announcing one
// approval-lifecycle event (hit/miss/consume/expire/revoke). The TUI renders
// these as a one-line activity entry so the operator can audit which rule
// admitted which call. ruleID is the rule (empty for miss); action/scope echo
// the rule's action and the JSON scope string for context; result is the
// lifecycle kind from approval.AuditEvent.Kind.
func NewPermissionRuleHit(ruleID, action, scope, result string) ServerFrame {
	return ServerFrame{Type: "permission_rule_hit", ID: ruleID, ToolName: action, Text: scope, Status: result}
}

// unixOrZero returns t.Unix() when t is non-zero, else 0. Used by the WS handler
// when building PermissionInfo so an unset ExpiresAt serializes as 0 (not the
// negative value time.Time{}.Unix() would produce on some platforms).
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// UnixOrZero is the exported wrapper for callers outside the proto package
// (notably internal/api/http/ws.go building PermissionInfo from approval.Rule).
func UnixOrZero(t time.Time) int64 { return unixOrZero(t) }

// SSEEvent returns the SSE wire form of a frame: the event name (the frame
// Type) and the data payload (the frame JSON). The SSE handler emits
// `event: <name>\ndata: <data>\n\n`; sseBackend parses it back. This keeps SSE
// and WS on one shared event vocabulary (agent_chunk/tool_call/tool_result/
// error/done).
func (f ServerFrame) SSEEvent() (event string, data []byte) {
	event = f.Type
	data, _ = json.Marshal(f)
	return event, data
}

// --- Job control frames (Task 22) ---
//
// The /jobs TUI command and the WS shell control path exchange these frames.
// Every job control frame reuses the existing ClientFrame fields (Type, ID,
// Text) so the wire form stays consistent with the permissions control frames
// (Task 9) and no new top-level field is needed.

// NewJobsList builds a jobs_list control frame: the client asks for the full
// job table visible to the current process. Reply is a "jobs" frame.
func NewJobsList() ClientFrame { return ClientFrame{Type: "jobs_list"} }

// NewJobRead builds a job_read control frame. ID is the job id; Text carries
// the max-bytes hint as a decimal string (strconv.Itoa) so we don't add a new
// ClientFrame field for one integer. The WS handler parses it back via
// strconv.Atoi; missing/garbage values default to 4096.
func NewJobRead(id string, maxBytes int) ClientFrame {
	return ClientFrame{Type: "job_read", ID: id, Text: strconv.Itoa(maxBytes)}
}

// NewJobWrite builds a job_write control frame: stdin for the job identified
// by ID. Text is the raw bytes to write (the Manager hands it to Console.Write
// verbatim — no shell interpolation).
func NewJobWrite(id, data string) ClientFrame {
	return ClientFrame{Type: "job_write", ID: id, Text: data}
}

// NewJobCancel builds a job_cancel control frame. ID reuses the existing
// ClientFrame.ID field so the wire form stays consistent with permission_revoke
// (Task 9).
func NewJobCancel(id string) ClientFrame {
	return ClientFrame{Type: "job_cancel", ID: id}
}

// NewJobs builds a "jobs" frame carrying the full job table. Emitted as the
// reply to jobs_list (Task 22). Each item is a flattened shell.Job.
func NewJobs(jobs Jobs) ServerFrame {
	return ServerFrame{Type: "jobs", Jobs: jobs}
}

// NewJobEvent builds a "job_event" frame announcing a per-job lifecycle event
// (read output, stdin ack, cancel ack). The TUI renders these as a one-line
// summary so the operator can audit /jobs interactions.
func NewJobEvent(job JobInfo) ServerFrame {
	return ServerFrame{Type: "job_event", ID: job.ID, Status: job.State, Text: job.Output}
}

// --- A2 task/plan/checklist frame constructors ---

// NewTaskUpdate builds a task_update frame carrying a full WorkTask snapshot.
// Emitted by task_create / task_cancel / task_gate_run after a mutation.
func NewTaskUpdate(task *work.WorkTask) ServerFrame {
	if task == nil {
		return ServerFrame{}
	}
	return ServerFrame{Type: "task_update", Task: task, TaskID: task.ID}
}

// NewPlanUpdate builds a plan_update frame. rows 为 nil 时 Checklist.Items 是
// empty-slice（非 nil），让 TUI 能区分"清空 checklist"与"未发过 checklist 事件"。
func NewPlanUpdate(taskID string, rows []work.ChecklistItem) ServerFrame {
	checklist := &work.Checklist{Items: append([]work.ChecklistItem(nil), rows...)}
	return ServerFrame{Type: "plan_update", TaskID: taskID, Checklist: checklist}
}

// NewChecklistUpdate builds a checklist_update frame（追加/修改后的完整清单）。
func NewChecklistUpdate(taskID string, rows []work.ChecklistItem) ServerFrame {
	checklist := &work.Checklist{Items: append([]work.ChecklistItem(nil), rows...)}
	return ServerFrame{Type: "checklist_update", TaskID: taskID, Checklist: checklist}
}

// SubagentEventType is the ServerFrame type for subagent lifecycle events.
const SubagentEventType = "subagent_event"

// NewSubagentEvent builds a frame relaying a subagent lifecycle event.
func NewSubagentEvent(agentID, role, event, status, text string) ServerFrame {
	return ServerFrame{
		Type: SubagentEventType, AgentID: agentID, AgentRole: role,
		Event: event, AgentStatus: status, Text: text,
	}
}

// MCPServerStatus is the on-the-wire shape of one MCP server snapshot for
// mcp_status frames. Transport is "stdio" or "http"; Status is the connection
// state ("starting"|"ready"|"failed"|"stopped"); Error carries the failure
// reason when status is "failed"; ToolCount is the number of discovered tools;
// Tools carries the full tool list (for palette population).
type MCPServerStatus struct {
	Name      string         `json:"name"`
	Transport string         `json:"transport"`
	Status    string         `json:"status"`
	Error     string         `json:"error,omitempty"`
	ToolCount int            `json:"tool_count"`
	Tools     []MCPToolBrief `json:"tools,omitempty"`
}

// MCPToolBrief is a palette-friendly summary of one MCP tool. Name is the
// qualified runtime name (mcp_<server>_<tool>); Description is the tool's
// description string.
type MCPToolBrief struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// NewMCPAction builds a ClientFrame for /mcp commands.
func NewMCPAction(server, action string) ClientFrame {
	return ClientFrame{Type: "mcp_action", MCPServer: server, MCPAction: action}
}

// NewMCPStatusFrame builds a ServerFrame carrying MCP server statuses.
func NewMCPStatusFrame(servers []MCPServerStatus) ServerFrame {
	return ServerFrame{Type: "mcp_status", MCPServers: servers}
}

// --- B2-RB1 turn-rollback seam frames (Task 6) ---
//
// list_seams / restore_turn are client→server control frames; seams /
// seam_restored are server→client control replies. The selector is always the
// exact seam_id (必修项 J). ConfirmedHead carries the FULL main_head id the
// client cached from a prior list / status / seam_restored frame so the server
// can reject a stale target (D6: full id, not short hash).

// NewListSeams requests the recent seams for the current session (reply: seams).
// selector is exact seam_id (B2-RB1 必修项 J: no N/turn_id selector).
func NewListSeams() ClientFrame { return ClientFrame{Type: "list_seams"} }

// NewRestoreTurn requests reverting main to the state captured by seamID.
// confirmedHead is the FULL main_head commit id the client observed at list
// time (D6: full id, not the short hash); the server rejects when the head has
// since changed (必修项 E). Empty is rejected by the server (D6: require
// non-empty).
func NewRestoreTurn(seamID, confirmedHead string) ClientFrame {
	return ClientFrame{Type: "restore_turn", ID: seamID, ConfirmedHead: confirmedHead}
}

// NewSeams replies with the recent seams + the current main_head. commitShort
// is the display short hash; head is the FULL commit id the client binds into
// the next restore_turn's ConfirmedHead (D6).
func NewSeams(items []SeamInfo, commitShort, head string) ServerFrame {
	return ServerFrame{Type: "seams", Seams: items, CommitShort: commitShort, Head: head}
}

// NewSeamRestored replies with the undo seam id (pointing at the PRE-revert
// main_head) and the post-revert head. commitShort is for display; head is the
// FULL post-revert commit id for the next binding (D6). text carries a
// human-readable summary for the TUI entry.
func NewSeamRestored(undoSeamID, commitShort, head, text string) ServerFrame {
	return ServerFrame{Type: "seam_restored", ID: undoSeamID, CommitShort: commitShort, Head: head, Text: text}
}

// --- V09 fork frames ---

// NewForkSession requests a fork of the current session (V09). seq=-1 forks
// the entire history; seq>=0 forks messages[0..seq] (inclusive); seq out of
// range is rejected by the server. Reply: session_forked{session_id: forkID}.
func NewForkSession(seq int) ClientFrame {
	return ClientFrame{Type: "fork_session", Seq: seq}
}

// NewSessionForked is the reply to fork_session, carrying the new session id.
// Emitted as a single-frame control reply, so isControlReply closes the
// client's reply channel on it.
func NewSessionForked(forkID string) ServerFrame {
	return ServerFrame{Type: "session_forked", SessionID: forkID}
}

// --- V11 side conversation frames ---

// NewEnterSide requests entering an ephemeral side conversation (V11). The
// server pushes the current history+sessionID+counters onto an in-memory stack
// (no DB writes while inside). Reply: side_state{depth}.
func NewEnterSide() ClientFrame { return ClientFrame{Type: "enter_side"} }

// NewExitSide requests exiting the current side conversation (V11). The server
// pops the stack and restores the snapshotted state. Reply: side_state{depth:0}.
func NewExitSide() ClientFrame { return ClientFrame{Type: "exit_side"} }

// NewSideState is the reply to enter_side / exit_side, carrying the new depth
// (0 = main, 1+ = inside a side). Emitted as a single-frame control reply.
func NewSideState(depth int) ServerFrame {
	return ServerFrame{Type: "side_state", SideDepth: depth}
}

// --- E03 skill management frames ---

// SkillInfo is one skill entry in a skills_list reply (E03).
type SkillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source,omitempty"`
	Enabled     bool   `json:"enabled"`
	Trusted     bool   `json:"trusted"`
	// Shadowed lists the same-named skills this one won over (E03). Load
	// resolves duplicates first-seen-wins and used to drop the losers without
	// a trace, so a project skill hidden by a user-level one of the same name
	// was invisible: the name resolved to something the user did not write and
	// nothing in the product could say so. Empty on the common no-conflict
	// path, and omitempty keeps it off the wire there.
	Shadowed []ShadowedSkill `json:"shadowed,omitempty"`
}

// ShadowedSkill is one skill that lost a name collision. The directory is
// carried because "which file is being ignored" is the actionable question,
// and a source label alone does not answer it when several roots share one.
type ShadowedSkill struct {
	Source string `json:"source,omitempty"`
	Dir    string `json:"dir,omitempty"`
}

// Skill-list / mutation frames (E03). These all run on the backend — the TUI
// never git-clones locally, so remote-mode WS backends stay correct.

// NewListSkills builds a client frame requesting the skill list.
func NewListSkills() ClientFrame { return ClientFrame{Type: "list_skills"} }

// NewSkillsList builds a server frame listing installed skills.
func NewSkillsList(s []SkillInfo) ServerFrame {
	return ServerFrame{Type: "skills_list", Skills: s}
}

// NewValidateSkill builds a client frame asking the server to re-run the
// install-time checks against an already-installed skill. An empty name means
// "every installed skill".
func NewValidateSkill(name string) ClientFrame {
	return ClientFrame{Type: "validate_skill", Name: name}
}

// NewInstallSkill builds a client frame requesting skill installation.
func NewInstallSkill(source string) ClientFrame {
	return ClientFrame{Type: "install_skill", Source: source}
}

// NewUninstallSkill builds a client frame requesting skill removal.
func NewUninstallSkill(name string) ClientFrame {
	return ClientFrame{Type: "uninstall_skill", Name: name}
}

// NewTrustSkill builds a client frame marking a skill as trusted.
func NewTrustSkill(name string) ClientFrame {
	return ClientFrame{Type: "trust_skill", Name: name}
}

// NewUntrustSkill builds a client frame marking a skill as untrusted.
func NewUntrustSkill(name string) ClientFrame {
	return ClientFrame{Type: "untrust_skill", Name: name}
}

// NewEnableSkill builds a client frame enabling a skill.
func NewEnableSkill(name string) ClientFrame {
	return ClientFrame{Type: "enable_skill", Name: name}
}

// NewDisableSkill builds a client frame disabling a skill.
func NewDisableSkill(name string) ClientFrame {
	return ClientFrame{Type: "disable_skill", Name: name}
}

// NewSkillAck acknowledges a skill mutation (E03). action is one of
// "installed"|"uninstalled"|"trusted"|"untrusted"|"enabled"|"disabled".
// On install the Skill pointer carries the freshly-loaded entry; otherwise it
// may be nil. errText carries the error message on failure (the TUI renders it
// as an error entry).
func NewSkillAck(action string, skill *SkillInfo, errText string) ServerFrame {
	return ServerFrame{Type: "skill_ack", Action: action, Skill: skill, Text: errText}
}
