package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/memory"
)

// NewRememberTool builds the `remember` tool: appends a timestamped bullet to
// the user or project memory file (MEM1). It is a standard GuardedTool; the
// default profile (Tools.Allow=["*"]) permits it without per-call prompting.
// Ack text is honest: memory is baked into the orchestrator system prompt at
// bootstrap, so the new entry takes effect on the NEXT BACKEND RESTART, not
// the next turn.
//
// The user/project paths are fixed at construction (bootstrap), so the model
// cannot redirect writes by passing arguments — only content and scope
// (user|project) come from args.
func NewRememberTool(userPath, projectPath string) *GuardedTool {
	return NewGuardedTool(
		"remember", "Skill",
		"Append a preference note to the user or project memory file. "+
			"Notes persist across sessions and are injected into future system prompts.",
		5*time.Second,
		params(map[string]*schema.ParameterInfo{
			"content": {Type: schema.String, Desc: "the note to remember", Required: true},
			"scope": {
				Type: schema.String,
				Desc: `"user" (default, ~/.yanshi/memory.md) or "project" (<workRoot>/.yanshi/memory.md)`,
			},
		}),
		SyncStream(func(_ context.Context, argsJSON string) (string, error) {
			var a struct {
				Content string `json:"content"`
				Scope   string `json:"scope"`
			}
			if err := ParseArgs(argsJSON, &a); err != nil {
				return "", err
			}
			if strings.TrimSpace(a.Content) == "" {
				return "", fmt.Errorf("remember: content must be non-empty")
			}
			path := userPath
			switch strings.TrimSpace(a.Scope) {
			case "", "user":
				path = userPath
			case "project":
				path = projectPath
			default:
				return "", fmt.Errorf("remember: scope must be user or project, got %q", a.Scope)
			}
			if path == "" {
				return "", fmt.Errorf("remember: no memory path configured for scope %q", a.Scope)
			}
			if err := memory.Append(path, a.Content); err != nil {
				return "", fmt.Errorf("remember: %w", err)
			}
			return fmt.Sprintf(
				"saved to %s; takes effect after backend restart (memory is baked into the system prompt at bootstrap).",
				filepath.Base(path)), nil
		}),
	)
}
