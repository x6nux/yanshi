// Package registry tracks available sub-agents (local, external, remote) and
// provides a process-wide runtime Manager for spawning, running, and persisting
// sub-agents across turns.
package registry

import (
	"context"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// Runtime status
// ---------------------------------------------------------------------------

// Status represents the lifecycle state of a managed sub-agent.
type Status string

// Lifecycle statuses for a managed sub-agent.
const (
	StatusPending     Status = "pending"     // allocated but not yet running
	StatusRunning     Status = "running"     // goroutine active
	StatusCompleted   Status = "completed"   // finished successfully
	StatusFailed      Status = "failed"      // finished with a runner error
	StatusCancelled   Status = "cancelled"   // cancelled via Cancel() or parent cancellation
	StatusInterrupted Status = "interrupted" // was Running on disk when loaded by a new process
)

// Terminal returns true for statuses that represent an end state.
func (s Status) Terminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusCancelled, StatusInterrupted:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Usage
// ---------------------------------------------------------------------------

// Usage accumulates token and call counts for a managed sub-agent.
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	ModelCalls       int64 `json:"model_calls"`
}

// Add returns a new Usage that is the sum of u and other.
func (u Usage) Add(other Usage) Usage {
	return Usage{
		PromptTokens:     u.PromptTokens + other.PromptTokens,
		CompletionTokens: u.CompletionTokens + other.CompletionTokens,
		TotalTokens:      u.TotalTokens + other.TotalTokens,
		ModelCalls:       u.ModelCalls + other.ModelCalls,
	}
}

// ---------------------------------------------------------------------------
// Events (lifecycle events emitted by the Manager)
// ---------------------------------------------------------------------------

// EventType categorises a lifecycle event.
type EventType string

// Lifecycle event types emitted by the Manager.
const (
	EventStarted           EventType = "started"
	EventCompleted         EventType = "completed"
	EventFailed            EventType = "failed"
	EventCancelled         EventType = "cancelled"
	EventResumed           EventType = "resumed"
	EventInputQueued       EventType = "input_queued"
	EventAssigned          EventType = "assigned"
	EventPersistenceFailed EventType = "persistence_failed"
)

// Event is one lifecycle event emitted by the Manager. Consumers (e.g. the
// subagent_relay in the HTTP package) map these to proto.ServerFrame.
type Event struct {
	Type      EventType `json:"type"`
	AgentID   string    `json:"agent_id"`
	ParentID  string    `json:"parent_id,omitempty"`
	Role      string    `json:"role"`
	Status    Status    `json:"status"`
	Text      string    `json:"text,omitempty"`
	Usage     Usage     `json:"usage,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// EventSink receives lifecycle events. Implementations must not block or lock
// the Manager (see locked design item #6: emit is called outside all Manager
// locks).
type EventSink func(Event)

// ---------------------------------------------------------------------------
// Custom role
// ---------------------------------------------------------------------------

// CustomRole holds the caller-supplied role configuration for a "custom" role
// sub-agent.
type CustomRole struct {
	Name          string   `json:"name"`
	PromptPrefix  string   `json:"prompt_prefix"`
	AllowedTools  []string `json:"allowed_tools"`
	ReadOnlyShell bool     `json:"read_only_shell"`
}

// ---------------------------------------------------------------------------
// Record
// ---------------------------------------------------------------------------

// Record captures the full state of a managed sub-agent at a point in time.
// It is persisted to disk and returned from Result / List / Wait.
type Record struct {
	ID              string      `json:"id"`
	SessionBootID   string      `json:"session_boot_id"`
	AgentType       string      `json:"agent_type"`
	Role            string      `json:"role"`
	Nickname        string      `json:"nickname,omitempty"`
	ParentID        string      `json:"parent_id,omitempty"`
	Depth           int         `json:"depth"`
	Status          Status      `json:"status"`
	Prompt          string      `json:"prompt"`
	Assignment      string      `json:"assignment,omitempty"`
	Instruction     string      `json:"instruction,omitempty"`
	AllowedTools    []string    `json:"allowed_tools,omitempty"`
	ModelOverride   string      `json:"model_override,omitempty"`
	ReasoningEffort string      `json:"reasoning_effort,omitempty"`
	Custom          *CustomRole `json:"custom,omitempty"`
	Result          string      `json:"result,omitempty"`
	Error           string      `json:"error,omitempty"`
	Usage           Usage       `json:"usage"`
	StartedAt       time.Time   `json:"started_at"`
	EndedAt         time.Time   `json:"ended_at,omitempty"`
}

// ---------------------------------------------------------------------------
// SpawnRequest
// ---------------------------------------------------------------------------

// SpawnRequest describes the parameters for spawning a new managed sub-agent.
type SpawnRequest struct {
	AgentType       string
	ParentID        string
	Role            string
	Custom          *CustomRole
	Nickname        string
	Prompt          string
	AllowedTools    []string
	Instruction     string
	ModelOverride   string
	ReasoningEffort string
	Runner          Runner
	Emit            EventSink
}

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

// Runner executes one sub-agent turn. The Manager calls Run on the goroutine
// it spawns; agentID and assignment are provided so the runner can report
// usage and receive follow-up input.
type Runner interface {
	Run(ctx context.Context, agentID, assignment string) (string, error)
}

// RunnerFunc is a function adapter that implements Runner.
type RunnerFunc func(ctx context.Context, agentID, assignment string) (string, error)

// Run implements Runner.
func (f RunnerFunc) Run(ctx context.Context, agentID, assignment string) (string, error) {
	return f(ctx, agentID, assignment)
}

// ---------------------------------------------------------------------------
// ResumeRequest
// ---------------------------------------------------------------------------

// ResumeRequest passes the runner and optional prompt override for resuming a
// previously interrupted / completed sub-agent.
type ResumeRequest struct {
	Runner Runner
	Emit   EventSink
	Prompt string
}

// ---------------------------------------------------------------------------
// WaitOpts
// ---------------------------------------------------------------------------

// WaitOpts controls the Wait call.
type WaitOpts struct {
	Timeout time.Duration
}

// ---------------------------------------------------------------------------
// ListResult
// ---------------------------------------------------------------------------

// ListResult is returned by Manager.List.
type ListResult struct {
	Agents  []Record `json:"agents"`
	Running int      `json:"running"`
	Limit   int      `json:"limit"`
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// SpawnErrCap is returned when the concurrency limit has been reached. C1 uses
// errors.As to detect this and schedule a retry.
type SpawnErrCap struct {
	Cap int
}

// Error returns a human-readable concurrency-limit message.
func (e *SpawnErrCap) Error() string {
	return fmt.Sprintf("subagent concurrency limit reached (cap=%d)", e.Cap)
}

// Sentinel errors.
var (
	ErrNotFound    = fmt.Errorf("subagent not found")
	ErrClosed      = fmt.Errorf("subagent manager is closed")
	ErrNotRunning  = fmt.Errorf("subagent is not running")
	ErrMailboxFull = fmt.Errorf("subagent mailbox is full")
	ErrTooDeep     = fmt.Errorf("subagent nesting depth exceeds maximum")
)

// cloneRecord returns a shallow copy of rec with slices deep-copied so that
// callers cannot mutate the Manager-internal record.
func cloneRecord(rec Record) Record {
	out := rec
	if rec.AllowedTools != nil {
		out.AllowedTools = make([]string, len(rec.AllowedTools))
		copy(out.AllowedTools, rec.AllowedTools)
	}
	if rec.Custom != nil {
		c := *rec.Custom
		if rec.Custom.AllowedTools != nil {
			c.AllowedTools = make([]string, len(rec.Custom.AllowedTools))
			copy(c.AllowedTools, rec.Custom.AllowedTools)
		}
		out.Custom = &c
	}
	return out
}
