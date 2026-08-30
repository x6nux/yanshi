package bootstrap_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/bootstrap"
	"github.com/x6nux/yanshi/internal/config"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
)

// This file verifies the loop guard (L1, L2) over the WHOLE chain an operator
// actually traverses: a loop_guard block in a config FILE, parsed by the real
// loader, assembled by the real composition root, enforced on a real turn that
// calls REAL registered tools and leaves REAL files on disk.
//
// The orchestrator package already drives these gates end to end, but it
// constructs orchestrator.Config in Go and counts calls on a purpose-built
// probe tool. That leaves two links unverified, and they are the ones this
// repo keeps breaking:
//
//   - whether the YAML an operator writes reaches the struct field the gate
//     reads. A dropped field in bootstrap's mapping or a mistyped yaml tag
//     leaves every orchestrator-level test green while the operator's budget
//     does nothing.
//   - whether a refused call is refused BEFORE its side effect. A counter on a
//     test tool cannot tell "the tool ran and its result was discarded" from
//     "the tool never ran"; a file that exists on disk can.
//
// A tool-calling fake model is what makes this checkable at all: the CLI's
// fake emits text only, so `yanshi exec --fake-model` can never make a turn
// call a tool, and no budget is observable through it.

// buildAppWithLoopGuard writes a real config file containing the given
// loop_guard YAML, builds a real App from it rooted at workRoot, and returns
// the App.
//
// The YAML is a string rather than a config.Config literal on purpose: passing
// Options.Cfg would skip the loader, and the loader is half of what is being
// verified. A yaml tag that does not match the key an operator writes is
// invisible to any test that builds the struct in Go.
func buildAppWithLoopGuard(t *testing.T, workRoot, loopGuardYAML string, mdl model.BaseChatModel) *bootstrap.App {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf(`
llm:
  providers:
    - name: probe
      model: probe-model
storage:
  sqlite_path: %q
profiles:
  orchestrator:
    tools: { allow: ["fs_*"] }
    fs:
      read: ["**"]
      write: ["**"]
%s
`, filepath.Join(dir, "test.db"), loopGuardYAML)
	require.NoError(t, os.WriteFile(cfgPath, []byte(body), 0o644))

	app, err := bootstrap.Build(bootstrap.Options{
		ConfigPath: cfgPath,
		WorkRoot:   workRoot,
		ProviderBuilder: func(*config.Config, ...einollm.SecretRegistrar) (map[string]model.BaseChatModel, []model.BaseChatModel, map[string]int, map[string]float64, map[string]einollm.TruncationSpec, error) {
			return map[string]model.BaseChatModel{"probe-model": mdl},
				[]model.BaseChatModel{mdl}, nil, nil, nil, nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
	return app
}

// writeCallMsg builds an assistant message asking fs_write to create name.
func writeCallMsg(id, name string) *schema.Message {
	return schema.AssistantMessage("", []schema.ToolCall{{
		ID: id, Type: "function",
		Function: schema.FunctionCall{
			Name:      "fs_write",
			Arguments: fmt.Sprintf(`{"path":%q,"content":"x"}`, name),
		},
	}})
}

// runTurn drives one real turn through the App's orchestrator and returns the
// concatenated error-frame text.
func runTurn(t *testing.T, app *bootstrap.App, prompt string) string {
	t.Helper()
	var errs strings.Builder
	orchestrator.ClassifyEvents(app.Orch.Events(context.Background(), prompt),
		func(f proto.ServerFrame) {
			if f.Type == "error" {
				errs.WriteString(f.Text)
			}
		})
	return errs.String()
}

// filesIn lists the regular files directly under dir.
func filesIn(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestLoopGuardConfigReachesARealTurn_TotalToolBudget is the end-to-end L2
// check: `max_tool_calls` written in a config FILE must stop a real turn's
// tool calls at that number, and must stop them before they touch the disk.
//
// The model asks for five distinct files; the budget allows two. The files
// that exist afterwards are the assertion. A budget enforced only in the
// reported result -- refusing in the transcript while the write still landed
// -- would pass a call-count check and fail this one.
func TestLoopGuardConfigReachesARealTurn_TotalToolBudget(t *testing.T) {
	workRoot := t.TempDir()
	msgs := []*schema.Message{
		writeCallMsg("c1", "a.txt"),
		writeCallMsg("c2", "b.txt"),
		writeCallMsg("c3", "c.txt"),
		writeCallMsg("c4", "d.txt"),
		writeCallMsg("c5", "e.txt"),
		schema.AssistantMessage("done", nil),
	}
	app := buildAppWithLoopGuard(t, workRoot, `
loop_guard:
  max_tool_calls: 2
`, einollm.NewFakeModelWithMessages(msgs, nil))

	runTurn(t, app, "write five files")

	got := filesIn(t, workRoot)
	require.ElementsMatch(t, []string{"a.txt", "b.txt"}, got,
		"max_tool_calls: 2 in the config FILE must stop the third fs_write before it "+
			"reaches the disk; got %v", got)
}

// TestLoopGuardConfigReachesARealTurn_ZeroConfigIsOff is the control, and it is
// what makes the test above mean anything.
//
// Without it, a build that installed a budget unconditionally -- ignoring the
// config entirely -- would pass. It also pins the documented promise that an
// operator who writes no loop_guard block keeps the previous behaviour: all
// five writes land.
func TestLoopGuardConfigReachesARealTurn_ZeroConfigIsOff(t *testing.T) {
	workRoot := t.TempDir()
	msgs := []*schema.Message{
		writeCallMsg("c1", "a.txt"),
		writeCallMsg("c2", "b.txt"),
		writeCallMsg("c3", "c.txt"),
		writeCallMsg("c4", "d.txt"),
		writeCallMsg("c5", "e.txt"),
		schema.AssistantMessage("done", nil),
	}
	// No loop_guard block at all.
	app := buildAppWithLoopGuard(t, workRoot, ``, einollm.NewFakeModelWithMessages(msgs, nil))

	runTurn(t, app, "write five files")

	got := filesIn(t, workRoot)
	require.ElementsMatch(t, []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"}, got,
		"with no loop_guard block every write must land; if some are missing, a gate "+
			"is installing itself unasked and the budget test above proves nothing")
}

// TestLoopGuardConfigReachesARealTurn_RepetitionStops is the end-to-end L1
// check: `repetition_enabled: true` in a config FILE must end a real
// doom-looping turn with a reason naming the gate and the tool.
func TestLoopGuardConfigReachesARealTurn_RepetitionStops(t *testing.T) {
	workRoot := t.TempDir()
	// The SAME write, forever: identical name and identical arguments, which
	// is what the repetition gate keys on.
	mdl := einollm.NewFakeModelWithMessages(
		[]*schema.Message{writeCallMsg("c1", "same.txt")}, nil)
	mdl.Repeat = true
	app := buildAppWithLoopGuard(t, workRoot, `
loop_guard:
  repetition_enabled: true
`, mdl)

	errText := runTurn(t, app, "write the same file over and over")

	require.Contains(t, strings.ToLower(errText), "repetition",
		"repetition_enabled: true in the config FILE must stop the turn and name the gate")
	require.Contains(t, errText, "fs_write", "the stop reason must name the offending tool")
}

// TestLoopGuardConfigReachesARealTurn_DistinctArgsAreNotRepetition is the
// false-positive control for L1 against a REAL tool.
//
// Five different filenames are five different argument blobs, so the gate must
// not fire -- and every file must land. A repetition detector keyed on the
// tool name alone would stop this turn, turning "write the files I asked for"
// into a truncated turn with some of them missing.
func TestLoopGuardConfigReachesARealTurn_DistinctArgsAreNotRepetition(t *testing.T) {
	workRoot := t.TempDir()
	msgs := []*schema.Message{
		writeCallMsg("c1", "a.txt"),
		writeCallMsg("c2", "b.txt"),
		writeCallMsg("c3", "c.txt"),
		writeCallMsg("c4", "d.txt"),
		writeCallMsg("c5", "e.txt"),
		schema.AssistantMessage("done", nil),
	}
	app := buildAppWithLoopGuard(t, workRoot, `
loop_guard:
  repetition_enabled: true
`, einollm.NewFakeModelWithMessages(msgs, nil))

	errText := runTurn(t, app, "write five different files")

	require.NotContains(t, strings.ToLower(errText), "repetition",
		"distinct arguments are progress, not a doom loop")
	require.ElementsMatch(t, []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"},
		filesIn(t, workRoot))
}
