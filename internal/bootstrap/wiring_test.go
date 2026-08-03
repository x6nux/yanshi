package bootstrap_test

import (
	"context"
	"encoding/json"
	"path"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/bootstrap"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/tools"
)

// TestDefaultOrchestratorProfileIsStable proves the factory-default profile
// is reachable independently of config, which is what lets GOV5 compare the
// shipped allow list against the shipped tool registry.
func TestDefaultOrchestratorProfileIsStable(t *testing.T) {
	p := bootstrap.DefaultOrchestratorProfile()
	require.NotEmpty(t, p.Tools.Allow, "default profile must name concrete tools, not fail open")
	require.True(t, p.Net.Allow, "default profile allows net (see bootstrap.go comment)")
}

// TestAppExposesToolNames proves a built App reports the tool names actually
// registered with the orchestrator.
func TestAppExposesToolNames(t *testing.T) {
	app := buildMinimalApp(t) // helper from bootstrap_test.go:40
	require.NotEmpty(t, app.ToolNames, "App.ToolNames must list the registered tools")
	require.Contains(t, app.ToolNames, "fs_read", "fs_read is always registered")
}

// TestC1ToolsAreRegistered proves the C1 capability's tools reach the
// orchestrator's registry.
//
// Why this exists alongside GOV4: GOV4 only proves BuildC1 is *called* from
// Build. A call whose return value is dropped on the floor still satisfies it,
// and that is precisely the failure mode this task fixed. Asserting on
// App.ToolNames proves the output was used.
//
// rlm_query is asserted too, and its absence would be a real failure rather
// than a config quirk: buildMinimalApp runs with FakeModel: true, and
// SelectRLMModel returns the fake when batch.rlm_model is unset, so rlm_query
// must register on this path. If it went missing, the fake model never reached
// BuildC1 — and rlm_query would become a phantom name in the default profile,
// which GOV5 forbids with no exemption available (the table is removal-only).
func TestC1ToolsAreRegistered(t *testing.T) {
	app := buildMinimalApp(t)

	registered := make(map[string]bool, len(app.ToolNames))
	for _, n := range app.ToolNames {
		registered[n] = true
	}
	for _, name := range []string{
		"automation_create", "automation_list", "automation_read", "automation_update",
		"automation_pause", "automation_resume", "automation_delete", "automation_run",
		"agent_batch", "rlm_query",
	} {
		require.True(t, registered[name],
			"C1 tool %q is missing from App.ToolNames — BuildC1 ran but its tools were "+
				"never appended to allTools", name)
	}
	require.NotNil(t, app.C1Scheduler,
		"App.C1Scheduler must be set so Shutdown can join the automation tick loop")
}

// toolWiringExceptions maps a tool name present in the default profile's
// allow list but absent from the tool registry, to the work package that
// will register it.
//
// Entries may only be REMOVED, never added. A dead entry — the tool is now
// registered — fails the test.
var toolWiringExceptions = map[string]string{}

// TestGOV5ProfileAllowMatchesToolRegistry verifies the default orchestrator
// profile does not authorize tools that were never registered.
//
// A name in the allow list that has no registered tool is worse than a
// missing feature: anyone reading the profile concludes the capability
// exists. The audit missed this entirely; the 2026-08-03 re-verification
// found nine such names.
//
// The phantom set depends on the config buildMinimalApp uses — several tools
// register conditionally (that config yields 59). A different config can
// shift the set, so treat the exemption table as tied to this harness.
func TestGOV5ProfileAllowMatchesToolRegistry(t *testing.T) {
	app := buildMinimalApp(t)

	registered := make(map[string]bool, len(app.ToolNames))
	for _, n := range app.ToolNames {
		registered[n] = true
	}
	require.NotEmpty(t, registered, "tool registry must not be empty")

	allowed := bootstrap.DefaultOrchestratorProfile().Tools.Allow

	var phantom []string
	concrete := make(map[string]bool)
	for _, name := range allowed {
		if strings.ContainsAny(name, "*?[") {
			continue // wildcard entries cannot be checked by exact name
		}
		concrete[name] = true
		if registered[name] {
			continue
		}
		if _, exempt := toolWiringExceptions[name]; exempt {
			continue
		}
		phantom = append(phantom, name)
	}
	sort.Strings(phantom)
	if len(phantom) > 0 {
		t.Errorf("GOV5: default profile allows %d tool(s) that are NOT registered — "+
			"the profile advertises capabilities that do not exist:\n  %s\n\n"+
			"Fix: register the tools in bootstrap.Build, or remove them from\n"+
			"DefaultOrchestratorProfile. If registration is deferred, add entries to\n"+
			"toolWiringExceptions naming the work package.",
			len(phantom), strings.Join(phantom, "\n  "))
	}

	// Dead-entry check: an exempted name that is now registered has been
	// wired up, so its exemption must be deleted.
	var dead []string
	for name := range toolWiringExceptions {
		if registered[name] {
			dead = append(dead, name)
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("GOV5: %d stale toolWiringExceptions entr(ies) — these tools are now "+
			"registered and their exemptions must be DELETED:\n  %s",
			len(dead), strings.Join(dead, "\n  "))
	}

	// Advisory only: a registered tool the default profile does not allow is
	// legitimate (a tightened profile is a valid configuration), but listing
	// them makes a forgotten authorization easy to spot in CI logs.
	//
	// Wildcards are re-applied here (they were skipped in the phantom check
	// above) so vcs_commit and friends are not reported as unauthorized when
	// "vcs_*" already covers them.
	var unauthorized []string
	for _, n := range app.ToolNames {
		if concrete[n] {
			continue
		}
		covered := false
		for _, pat := range allowed {
			if ok, _ := path.Match(pat, n); ok {
				covered = true
				break
			}
		}
		if !covered {
			unauthorized = append(unauthorized, n)
		}
	}
	sort.Strings(unauthorized)
	if len(unauthorized) > 0 {
		t.Logf("GOV5 (advisory): %d registered tool(s) are not named in the default "+
			"profile's allow list — verify this is intentional tightening and not a "+
			"forgotten authorization:\n  %s",
			len(unauthorized), strings.Join(unauthorized, "\n  "))
	}
}

// TestOrchestratorReceivesSecuritySubsystems asserts the orchestrator BUILT BY
// bootstrap.Build actually holds all five security subsystems.
//
// Why this test exists separately from TestBuild_MinimalApp: that test asserts
// App.Sandbox / App.NetworkPolicy / App.Approvals / App.ShellManager /
// App.SecureFactory, and App is a struct literal assembled from the SAME local
// variables — so it is non-nil no matter what the orchestrator received. It is
// structurally incapable of catching a wiring break between those locals and
// orchestrator.Config. orchestrator.Config is taken BY VALUE by New and the
// package exposes no setters, so any assignment made after New is silently
// discarded. Only reading back from the orchestrator proves the injection.
//
// Each nil field disables one tools.With* injection in bindExecutionContext,
// which means the corresponding subsystem never reaches a real tool call.
func TestOrchestratorReceivesSecuritySubsystems(t *testing.T) {
	app := buildMinimalApp(t)
	require.NotNil(t, app.Orch, "orchestrator must be built")

	rep := app.Orch.SecurityReport()
	if rep.Sandbox == nil {
		t.Error("orchestrator.Sandbox is nil — tools.WithSandbox is never called in " +
			"bindExecutionContext, so every tool runs outside the sandbox posture")
	}
	if rep.NetworkPolicy == nil {
		t.Error("orchestrator.NetworkPolicy is nil — tools.WithNetworkPolicy is never " +
			"called, so net_fetch and friends see no host allow/deny policy")
	}
	if rep.Approvals == nil {
		t.Error("orchestrator.Approvals is nil — tools.WithApprovalManager is never " +
			"called, so persistent/session approval rules are never consulted")
	}
	if rep.ShellManager == nil {
		t.Error("orchestrator.ShellManager is nil — tools.WithShellManager is never " +
			"called, so the shell v2 tools have no session manager to attach to")
	}
	if rep.SecureFactory == nil {
		t.Error("orchestrator.SecureFactory is nil — tools.WithSecureProcessFactory is " +
			"never called, so processes launch without the secure launch pipeline")
	}
}

// runShellTurn drives one orchestrator turn with a scripted model and returns
// every tool_result observed, keyed by tool name.
//
// The permission callback is not optional decoration: the default profile
// leaves security.shell.policy empty, so guard.Check returns Prompt for
// shell_start. Without a callback bound in ctx the GuardedTool layer has no
// one to ask and the turn stalls on a decision that never arrives — so the
// end-to-end assertion below would time out for a reason that has nothing to
// do with the wiring it is meant to prove.
func runShellTurn(t *testing.T, app *bootstrap.App, msgs []*schema.Message) map[string]string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ctx = tools.WithPermissionCallback(ctx, func(tools.PermissionRequest) tools.PermissionDecision {
		return tools.PermissionAllow
	})

	mdl := einollm.NewFakeModelWithMessages(msgs, nil)
	// EventsWithHistoryOpts returns the iterator alone — failures arrive as
	// events on it, not as a second return value.
	iter := app.Orch.EventsWithHistoryOpts(ctx,
		[]*schema.Message{schema.UserMessage("run the shell probe")},
		orchestrator.TurnOpts{Model: mdl})

	results := map[string]string{}
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
		if mv.Role != schema.Tool {
			continue
		}
		msg, err := materializeMessage(mv)
		if err != nil {
			t.Fatalf("drain tool_result stream: %v", err)
		}
		if msg != nil {
			results[mv.ToolName] = msg.Content
		}
	}
	return results
}

// materializeMessage collapses the streaming/non-streaming split in adk's
// MessageVariant so callers can read Content uniformly. The orchestrator runs
// with EnableStreaming, so tool results arrive as streams in practice.
func materializeMessage(mv *adk.MessageVariant) (*schema.Message, error) {
	if mv.IsStreaming && mv.MessageStream != nil {
		return mv.GetMessage()
	}
	return mv.Message, nil
}

// TestShellV2EndToEndSpawnsRealProcess proves the shell v2 tools registered by
// bootstrap.Build can actually spawn a process and return its stdout, driving
// shell_start -> shell_wait -> shell_read through a real orchestrator turn.
//
// Why this test exists, and why it asserts on stdout rather than on wiring:
// every previous shell test substituted its own shell.Config.Factory. That is
// exactly why bootstrap could omit Factory for so long without anyone
// noticing — the code path that fails in production was never the code path
// under test. A registry assertion would not have caught it either: the tools
// register fine, then fail at call time with "no process factory configured".
// Only running the process and reading its output covers the gap, which is the
// class of test spec §6.1 names.
//
// The two failure strings it discriminates against are the two ways this
// wiring breaks: "shell: runtime unavailable" (no shell.Manager bound in the
// turn context) and "no process factory configured" (Manager present but
// Config.Factory nil).
func TestShellV2EndToEndSpawnsRealProcess(t *testing.T) {
	app := buildMinimalApp(t)

	// echo is a builtin of both sh and cmd.exe, and shell.ShellArgv wraps the
	// command in the platform shell, so this needs no external binary.
	const marker = "yanshi-shell-v2-alive"
	start := schema.AssistantMessage("", []schema.ToolCall{{
		ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name:      "shell_start",
			Arguments: `{"command":"echo ` + marker + `"}`,
		},
	}})
	startRes := runShellTurn(t, app, []*schema.Message{start})

	raw, ok := startRes["shell_start"]
	require.Truef(t, ok, "no shell_start tool_result — the tool never ran (results: %v)", startRes)
	require.NotContains(t, raw, "runtime unavailable",
		"orchestrator.Config.ShellManager never reached the tool context")
	require.NotContains(t, raw, "no process factory configured",
		"shell.Config.Factory is nil in bootstrap.Build — shell v2 cannot spawn anything")

	var sess struct {
		ID  string `json:"id"`
		PID int    `json:"pid"`
	}
	require.NoErrorf(t, json.Unmarshal([]byte(raw), &sess),
		"shell_start did not return a session; got %q", raw)
	require.NotEmpty(t, sess.ID, "session id missing from shell_start result")
	require.NotZerof(t, sess.PID, "session PID is 0 — no OS process was spawned (result: %s)", raw)

	// Second turn: wait for exit, then read the buffered output. Two scripted
	// responses means the ReAct loop really iterates (model -> tool -> model).
	wait := schema.AssistantMessage("", []schema.ToolCall{{
		ID: "c2", Type: "function", Function: schema.FunctionCall{
			Name:      "shell_wait",
			Arguments: `{"id":"` + sess.ID + `"}`,
		},
	}})
	read := schema.AssistantMessage("", []schema.ToolCall{{
		ID: "c3", Type: "function", Function: schema.FunctionCall{
			Name:      "shell_read",
			Arguments: `{"id":"` + sess.ID + `"}`,
		},
	}})
	res := runShellTurn(t, app, []*schema.Message{wait, read})

	waitRaw, ok := res["shell_wait"]
	require.Truef(t, ok, "no shell_wait tool_result (results: %v)", res)
	var exited struct {
		State    string `json:"state"`
		ExitCode int    `json:"exit_code"`
	}
	require.NoErrorf(t, json.Unmarshal([]byte(waitRaw), &exited),
		"shell_wait did not return a session; got %q", waitRaw)
	require.Equal(t, "exited", exited.State, "process did not reach a terminal state")
	require.Equal(t, 0, exited.ExitCode, "echo exited non-zero — the launch pipeline mangled the command")

	readRaw, ok := res["shell_read"]
	require.Truef(t, ok, "no shell_read tool_result (results: %v)", res)
	var out struct {
		Output string `json:"output"`
	}
	require.NoErrorf(t, json.Unmarshal([]byte(readRaw), &out),
		"shell_read did not return output; got %q", readRaw)
	require.Containsf(t, out.Output, marker,
		"shell_read returned %q — the real stdout of the spawned process never made it back", out.Output)
}
