// Package llm defines yanshi's LLM client abstraction.
// In M1 the abstraction is provider-agnostic; Eino ChatModel adapters
// implement Client in M2.
package llm

import "context"

// Role is the role of a chat message.
type Role string

// Chat message roles.
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is a single chat message.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

// Response is the result of a chat completion.
type Response struct {
	Content string
}

// Client is the LLM abstraction every provider implements.
type Client interface {
	// Chat performs a single (non-streaming) completion turn.
	Chat(ctx context.Context, messages []Message) (Response, error)
	// Name identifies the provider (e.g. "openai", "claude").
	Name() string
}

// RetryableError marks an error as transient (rate limit, timeout, 5xx).
// ResilientClient retries and fails over on these.
type RetryableError struct{ Err error }

// Error returns the wrapped error message.
func (e *RetryableError) Error() string { return e.Err.Error() }

// Unwrap returns the wrapped error for errors.Is/As support.
func (e *RetryableError) Unwrap() error { return e.Err }

// Retryable wraps err as a retryable error.
func Retryable(err error) error { return &RetryableError{Err: err} }
