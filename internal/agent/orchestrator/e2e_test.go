package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/skills"
	"github.com/x6nux/yanshi/internal/tools"
)

// TestE2E_ModelDrivenFSEditMutatesRealFile is the functional-verification gate
// for the M7 tool-execution pipeline. It proves the full path works end to end:
//
//	model emits fs_edit tool call -> ADK ReAct runner -> GuardedTool ->
//	PermissionGuard -> real fs_edit -> REAL file mutation on disk.
//
// The only double is the FakeModel; the guard, the tool, and the filesystem
// are all real. If this test passes, the phase's core contract holds.
func TestE2E_ModelDrivenFSEditMutatesRealFile(t *testing.T) {
	workdir := t.TempDir()
	readme := filepath.Join(workdir, "readme.txt")
	require.NoError(t, os.WriteFile(readme, []byte("Helo world"), 0o644))

	// Scripted model: (1) request an fs_edit that fixes the typo, then
	// (2) emit a final assistant message so the ReAct loop terminates.
	// The Arguments field is a JSON-encoded string per the eino schema.
	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name:      "fs_edit",
			Arguments: `{"path":"readme.txt","old_string":"Helo","new_string":"Hello"}`,
		}},
	})
	step2 := schema.AssistantMessage("done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2}, nil)

	// Real GuardedTool (fs_edit) rooted at the temp workdir.
	fs := tools.NewFSTools(workdir)

	// Real permission profile: allow fs_* tools and writes under the workdir.
	// This is what flows through PermissionGuard at call time.
	profile := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS: guard.FSPerm{
			Read:  []string{workdir + "/**"},
			Write: []string{workdir + "/**"},
		},
	}

	o, err := New(Config{Model: mdl, Tools: []BaseTool{fs.Edit}, Profile: profile})
	require.NoError(t, err)

	out, err := o.Query(context.Background(), "fix the typo in readme.txt")
	require.NoError(t, err)
	assert.Equal(t, "done", out)

	// THE PROOF: the real file on disk was mutated by the model-driven call.
	got, err := os.ReadFile(readme)
	require.NoError(t, err)
	assert.Equal(t, "Hello world", string(got),
		"fs_edit tool call must have mutated the real file through the guard stack")
}

// TestE2E_GuardBlocksFSEditOutsideProfile proves the real PermissionGuard is
// actually gating the tool (not bypassed or permissive). With the tool name
// allowed but the write path outside the profile's FS.Write allowlist, the
// guard must deny the call so the real file is left untouched.
func TestE2E_GuardBlocksFSEditOutsideProfile(t *testing.T) {
	workdir := t.TempDir()
	readme := filepath.Join(workdir, "readme.txt")
	require.NoError(t, os.WriteFile(readme, []byte("Helo world"), 0o644))

	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name:      "fs_edit",
			Arguments: `{"path":"readme.txt","old_string":"Helo","new_string":"Hello"}`,
		}},
	})
	step2 := schema.AssistantMessage("done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2}, nil)

	fs := tools.NewFSTools(workdir)

	// Tool name fs_edit is allowed, but the FS.Write allowlist does NOT cover
	// the workdir, so the guard's checkFS dimension must reject the mutation.
	profile := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS: guard.FSPerm{
			Read:  []string{workdir + "/**"},
			Write: []string{"/nonexistent-allowlist/**"},
		},
	}

	o, err := New(Config{Model: mdl, Tools: []BaseTool{fs.Edit}, Profile: profile})
	require.NoError(t, err)

	// Whether the ADK surfaces the denial as a Query error or threads it back
	// to the model, the load-bearing assertion is the same: the real file must
	// be unchanged because the guard blocked the write.
	_, _ = o.Query(context.Background(), "fix the typo in readme.txt")

	got, err := os.ReadFile(readme)
	require.NoError(t, err)
	assert.Equal(t, "Helo world", string(got),
		"guard must block fs_edit outside the profile's FS.Write allowlist")
}

// TestE2E_SkillUseReturnsBodyThroughOrchestrator proves the skill-invocation
// pipeline end to end: the model emits a skill_use tool call, the ADK runs the
// real GuardedTool (which loads the skill body from the real Registry), and the
// skill body surfaces as a tool-result event. The unique marker in the skill
// body is the proof that the real body — not a scripted reply — traveled the
// full pipeline.
func TestE2E_SkillUseReturnsBodyThroughOrchestrator(t *testing.T) {
	// Build a real skill registry with one skill whose body has a unique marker.
	skillRoot := t.TempDir()
	skillDir := filepath.Join(skillRoot, "dev-quick-fix")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: dev-quick-fix\ndescription: fast fix playbook\n---\n# Dev Quick Fix\nUNIQUE_SKILL_MARKER_42"),
		0o644,
	))
	reg, err := skills.NewLoader(skills.Builtin(skillRoot)).Load()
	require.NoError(t, err)

	// Scripted model: (1) call skill_use for dev-quick-fix, (2) final text.
	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name:      "skill_use",
			Arguments: `{"name":"dev-quick-fix"}`,
		}},
	})
	step2 := schema.AssistantMessage("loaded the skill", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2}, nil)

	skillTool := tools.NewSkillUseTool(reg)
	profile := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"skill_*"}},
	}

	o, err := New(Config{Model: mdl, Tools: []BaseTool{skillTool}, Profile: profile})
	require.NoError(t, err)

	// Drive the orchestrator through Events so we can observe the tool-result
	// event (Role == schema.Tool, ToolName == "skill_use"), whose Message.Content
	// is the real skill body returned by the GuardedTool.
	iter := o.Events(context.Background(), "load the dev-quick-fix skill")
	var toolResult string
	var sawSkillEvent bool
	var finalAssistant string
	for {
		ev, ok := iter.Next()
		if !ok {
			break
		}
		if ev.Err != nil {
			t.Fatalf("unexpected agent error: %v", ev.Err)
		}
		if ev.Output == nil || ev.Output.MessageOutput == nil {
			continue
		}
		mv := ev.Output.MessageOutput
		// Streaming variant (EnableStreaming): materialize the message by
		// concatenating its stream so the role/content checks below still work.
		if mv.IsStreaming && mv.MessageStream != nil {
			msg, err := mv.GetMessage()
			if err != nil {
				t.Fatalf("drain stream: %v", err)
			}
			if msg == nil {
				continue
			}
			if mv.Role == schema.Tool && mv.ToolName == "skill_use" {
				toolResult = msg.Content
				sawSkillEvent = true
			}
			if mv.Role == schema.Assistant && msg.Content != "" {
				finalAssistant = msg.Content
			}
			continue
		}
		msg := mv.Message
		if msg == nil {
			continue
		}
		if msg.Role == schema.Tool && mv.ToolName == "skill_use" {
			toolResult = msg.Content
			sawSkillEvent = true
		}
		if msg.Role == schema.Assistant && msg.Content != "" {
			finalAssistant = msg.Content
		}
	}

	require.True(t, sawSkillEvent, "expected a skill_use tool-result event from the ADK runner")
	assert.Contains(t, toolResult, "UNIQUE_SKILL_MARKER_42",
		"skill_use tool result must carry the real skill body loaded by the GuardedTool")
	assert.Equal(t, "loaded the skill", finalAssistant)
}

// TestE2E_ToolFailureFeedsBackAndTurnContinues proves the load-bearing retry
// property: when a tool call FAILS, the failure is fed back to the model as a
// tool RESULT ({"error":...}), not a Go error, so the ADK ReAct loop continues
// instead of aborting on a NodeRunError. The scripted model "retries" after the
// failure: step 1 reads a missing file (tool fails), step 2 reads the real file
// (succeeds), step 3 emits the final answer. Before the GuardedTool fix, step 1
// returned a Go error → NodeRunError → ev.Err aborted the iterator before step 2
// ever ran. After the fix, both tool calls run and the model gets the error.
func TestE2E_ToolFailureFeedsBackAndTurnContinues(t *testing.T) {
	workdir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "real.txt"), []byte("payload"), 0o644))

	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name: "fs_read", Arguments: `{"path":"missing.txt"}`,
		}},
	})
	step2 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c2", Type: "function", Function: schema.FunctionCall{
			Name: "fs_read", Arguments: `{"path":"real.txt"}`,
		}},
	})
	step3 := schema.AssistantMessage("done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2, step3}, nil)

	fs := tools.NewFSTools(workdir)
	profile := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{workdir + "/**"}},
	}
	o, err := New(Config{Model: mdl, Tools: []BaseTool{fs.Read}, Profile: profile})
	require.NoError(t, err)

	var toolResults []string
	iter := o.Events(context.Background(), "read the file")
	for {
		ev, ok := iter.Next()
		if !ok {
			break
		}
		require.NoError(t, ev.Err, "a tool failure must NOT abort the turn (no NodeRunError)")
		if ev.Output == nil || ev.Output.MessageOutput == nil {
			continue
		}
		mv := ev.Output.MessageOutput
		var msg *schema.Message
		if mv.IsStreaming && mv.MessageStream != nil {
			m, err := mv.GetMessage()
			if err != nil || m == nil {
				continue
			}
			msg = m
		} else {
			msg = mv.Message
		}
		if msg == nil {
			continue
		}
		if msg.Role == schema.Tool {
			toolResults = append(toolResults, msg.Content)
		}
	}

	require.Len(t, toolResults, 2,
		"both tool calls must run — the turn continued past the first failure (model gets the error and retries, capped by MaxIterations)")
	assert.Contains(t, toolResults[0], "✗", "the failed read must feed an error result back to the model")
	assert.Contains(t, toolResults[1], "payload", "the model's retry must read the real file")
}
