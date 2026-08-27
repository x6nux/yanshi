// internal/tools/skill_write.go
//
// T7: the skill_write tool.
//
// This is the tool layer over skills.WriteSkill. It owns three things the
// skills package deliberately does not: the guard authorization, the choice of
// which directory the model may write to, and the reload that makes a written
// skill usable without a restart.
//
// # The destination is bound at construction, never taken from arguments
//
// NewSkillWriteTool captures the skills root. There is no `path` or `root`
// parameter, and adding one would defeat the whole design: skills.WriteSkill
// jails writes to whatever root it is HANDED, so a model-supplied root would be
// a jail around a directory the model chose. The same reasoning is why
// NewRememberTool fixes its memory paths at construction.
//
// # Authorization is a real FS write on a real path
//
// The tool Authorizes guard.Action{Tool: "skill_write", FS: write on the
// destination directory} before writing. That the path is outside the project
// tree is the point rather than a problem: an operator whose profile writes
// "**" over the project still gets a prompt for a write into ~/.yanshi/skills,
// which is the correct amount of friction for creating something that will be
// loaded into the system prompt on every subsequent boot.
//
// # Reload, and why the failure to reload is not a failure to write
//
// A written skill that the registry has not seen is invisible until restart,
// which for a goal loop means the lesson it just recorded cannot be used by the
// next iteration — the exact gap this tool exists to close. So the tool reloads.
// If the reload fails the WRITE still succeeded, and the result says so:
// reporting failure would invite the model to write the skill again, and the
// second attempt would fail on the already-exists check.

package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/skills"
)

// NewSkillWriteTool builds the skill_write tool: it lets the model record a
// reusable procedure as a standard SKILL.md pack in the user skills root.
//
// dstRoot is the directory skills are written to (the same root Install
// publishes into, so a written skill and an installed one are the same kind of
// thing). reg and loader may be nil — the tool then writes without refreshing
// the registry, which is the correct degradation for a caller that has no
// registry rather than a reason to refuse the write.
func NewSkillWriteTool(dstRoot string, reg *skills.Registry, loader *skills.Loader) *GuardedTool {
	return NewGuardedTool(
		"skill_write", "Write Skill",
		"Record a reusable procedure as a skill so future sessions can load it by name. "+
			"Use after solving something whose method would apply again. "+
			"The body is markdown instructions written for a model to follow.",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"name": {
				Type:     schema.String,
				Desc:     "skill name: letters, digits and dashes only, e.g. 'debug-flaky-tests'",
				Required: true,
			},
			"description": {
				Type: schema.String,
				Desc: "one sentence saying what this skill does and when to use it; " +
					"skills are selected by description, so be specific",
				Required: true,
			},
			"body": {
				Type:     schema.String,
				Desc:     "the skill's markdown instructions (no YAML frontmatter; it is generated)",
				Required: true,
			},
			"overwrite": {
				Type: schema.Boolean,
				Desc: "replace an existing skill of the same name (default false)",
			},
		}),
		SyncStream(func(ctx context.Context, argsJSON string) (string, error) {
			var a struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Body        string `json:"body"`
				Overwrite   bool   `json:"overwrite"`
			}
			if err := ParseArgs(argsJSON, &a); err != nil {
				return "", err
			}
			if dstRoot == "" {
				return errorResult("skill_write: no writable skills directory is configured"), nil
			}

			// Authorize a real FS write on the real destination. Done BEFORE
			// any staging directory is created, so a denied call leaves nothing
			// behind on disk.
			dst := skillDestination(dstRoot, a.Name)
			if err := Authorize(ctx, guard.Action{
				Tool: "skill_write",
				FS:   guard.FSWant{Op: "write", Paths: []string{dst}},
			}, argsJSON); err != nil {
				return "", err
			}

			path, err := skills.WriteSkill(dstRoot, skills.SkillDraft{
				Name:        a.Name,
				Description: a.Description,
				Body:        a.Body,
				Overwrite:   a.Overwrite,
			})
			if err != nil {
				// Returned as a RESULT rather than a Go error so the model
				// reads WHY and rewrites, instead of the turn aborting. The
				// content-scan refusal in particular names the offending line,
				// and that text is only useful if it reaches the model.
				return errorResult("skill_write: " + err.Error()), nil
			}

			var b strings.Builder
			fmt.Fprintf(&b, "Wrote skill %q to %s", a.Name, path)
			if reg != nil && loader != nil {
				if err := reg.Reload(loader); err != nil {
					// The write succeeded; say so. Reporting failure here would
					// invite a retry that the already-exists check would then
					// refuse, turning a cosmetic problem into a dead end.
					fmt.Fprintf(&b, "\n(warning: the skill registry could not be refreshed (%v); "+
						"the skill is on disk and will be available after a restart)", err)
				} else {
					b.WriteString("\nThe skill is loaded and can be used now via skill_use.")
				}
			}
			return b.String(), nil
		}),
	)
}

// skillDestination renders the path a skill of this name would occupy, for the
// authorization request.
//
// It does NOT validate the name — skills.WriteSkill owns that rule, and
// duplicating it here would create a second copy that can disagree with the
// one that actually gates the write. A hostile name simply produces a path
// string that the guard sees and WriteSkill then rejects.
func skillDestination(root, name string) string {
	return strings.TrimSuffix(root, "/") + "/" + name
}
