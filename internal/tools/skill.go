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
			// A skill whose declared `requires:` programs are absent is refused
			// here as well as hidden from MetaPrompt. The listing filter is what
			// stops the model choosing it; this is what stops it being loaded by
			// a name the model already had — from an earlier turn, a baked
			// prompt written before the binary was uninstalled (MetaPrompt is
			// snapshotted at orchestrator construction, FN3), or a user typing
			// the name. Returned as a RESULT, not a Go error, so the model reads
			// which programs are missing and re-plans instead of the turn
			// aborting.
			if hint := skills.MissingRequirementHint(s.Name, s.Missing); hint != "" {
				return errorResult("skill_use: " + hint), nil
			}
			// S7: a skill the content scan blocked is refused here as well as
			// hidden from MetaPrompt, and the split of duties is the same one
			// the requirements check above uses. The listing filter stops the
			// model CHOOSING it; this stops it being loaded by a name the model
			// already had — from a prompt baked before the pack was edited
			// (MetaPrompt is snapshotted at orchestrator construction, FN3),
			// from an earlier turn, or from a user typing the name.
			//
			// This is the last line before reg.Body reads the file, and Body's
			// return value goes straight back to the model as the skill's
			// instructions. Everything the scan objected to is in that text.
			if hint := skills.UnsafeSkillHint(s.Name, s.Unsafe); hint != "" {
				return errorResult("skill_use: " + hint), nil
			}
			body, err := reg.Body(s)
			if err != nil {
				return "", err
			}
			return body, nil
		}),
	)
}
