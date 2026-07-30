package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/skills"
)

// NewSkillUseTool builds the skill_use tool: it loads a skill's body from the
// registry and returns it for the model to follow.
func NewSkillUseTool(reg *skills.Registry) *GuardedTool {
	return NewGuardedTool(
		"skill_use", "Skill", "Load a skill's instructions by name (names listed in Available skills).",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"name": {Type: schema.String, Desc: "skill name", Required: true},
		}),
		SyncStream(func(_ context.Context, argsJSON string) (string, error) {
			var a struct {
				Name string `json:"name"`
			}
			if err := ParseArgs(argsJSON, &a); err != nil {
				return "", err
			}
			s, ok := reg.Get(a.Name)
			if !ok {
				return "", fmt.Errorf("skill_use: unknown skill %q", a.Name)
			}
			if !s.Enabled {
				return "", fmt.Errorf("skill_use: skill %q is disabled (use /skill enable %s)", a.Name, a.Name)
			}
			body, err := reg.Body(s)
			if err != nil {
				return "", err
			}
			return body, nil
		}),
	)
}
