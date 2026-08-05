package bootstrap_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/bootstrap"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/shell"
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
// BuildC1.
//
// Note this is the FAKE shape. rlm_query does NOT register in the production
// shape (no fake, no batch.rlm_model) — that asymmetry is precisely why it
// lives in bootstrap.ConditionalProfileTools rather than in the static default
// allow list, and why TestGOV5ProductionProfileHasNoPhantomNames exists.
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
// Entries may only be REMOVED, never added. A dead entry fails the test, in
// both of its shapes: the tool is now registered, OR no profile allow list
// names it any more so there is nothing left to excuse.
var toolWiringExceptions = map[string]string{}

// profileNamedTools is the universe an exemption can legitimately refer to:
// every concrete name the shipped profile machinery can put in front of guard.
//
// ConditionalProfileTools is included because the production-shape assertion
// runs phantomNames against the EFFECTIVE profile, which those names join at
// boot; leaving them out would make a legitimate exemption look vanished.
func profileNamedTools() map[string]bool {
	named := map[string]bool{}
	for _, n := range bootstrap.DefaultOrchestratorProfile().Tools.Allow {
		named[n] = true
	}
	for _, n := range bootstrap.ConditionalProfileTools() {
		named[n] = true
	}
	return named
}

// registeredSet indexes an App's tool registry snapshot for name lookup.
func registeredSet(t *testing.T, app *bootstrap.App) map[string]bool {
	t.Helper()
	set := make(map[string]bool, len(app.ToolNames))
	for _, n := range app.ToolNames {
		set[n] = true
	}
	require.NotEmpty(t, set, "tool registry must not be empty")
	return set
}

// phantomNames returns the sorted allow-list entries that name no registered
// tool, skipping wildcards (unmatchable by exact name) and exempted entries.
//
// Shared by every GOV5 assertion so "what counts as a phantom" has exactly one
// definition: the fake-shape check, the production-shape check, and any future
// config shape must agree, or the strictest one is decorative.
func phantomNames(allowed []string, registered map[string]bool) []string {
	var phantom []string
	for _, name := range allowed {
		if strings.ContainsAny(name, "*?[") {
			continue
		}
		if registered[name] {
			continue
		}
		if _, exempt := toolWiringExceptions[name]; exempt {
			continue
		}
		phantom = append(phantom, name)
	}
	sort.Strings(phantom)
	return phantom
}

// TestGOV5ProfileAllowMatchesToolRegistry verifies the default orchestrator
// profile does not authorize tools that were never registered.
//
// A name in the allow list that has no registered tool is worse than a
// missing feature: anyone reading the profile concludes the capability
// exists. The audit missed this entirely; the 2026-08-03 re-verification
// found nine such names.
//
// This is the FAKE shape (buildMinimalApp runs FakeModel: true), and it is
// deliberately only half the check — a name that registers here but not in
// production is invisible to it. TestGOV5ProductionProfileHasNoPhantomNames
// covers the other half.
func TestGOV5ProfileAllowMatchesToolRegistry(t *testing.T) {
	app := buildMinimalApp(t)

	registered := registeredSet(t, app)

	allowed := bootstrap.DefaultOrchestratorProfile().Tools.Allow

	concrete := make(map[string]bool)
	for _, name := range allowed {
		if !strings.ContainsAny(name, "*?[") {
			concrete[name] = true
		}
	}
	phantom := phantomNames(allowed, registered)
	if len(phantom) > 0 {
		t.Errorf("GOV5: default profile allows %d tool(s) that are NOT registered — "+
			"the profile advertises capabilities that do not exist:\n  %s\n\n"+
			"Fix: register the tools in bootstrap.Build, or remove them from\n"+
			"DefaultOrchestratorProfile. If registration is deferred, add entries to\n"+
			"toolWiringExceptions naming the work package.",
			len(phantom), strings.Join(phantom, "\n  "))
	}

	// Dead-entry check, both halves. Only "now registered" used to be checked,
	// and that condition is vacuously false for a name no profile mentions —
	// so an entry for a tool that was dropped from the allow list (or never
	// existed) survived every run. It is not inert: it silently pre-approves
	// the next appearance of that exact name, which is the phantom-name bug
	// GOV5 exists to catch, arriving with its own exemption already in place.
	named := profileNamedTools()
	var dead []string
	for name := range toolWiringExceptions {
		switch {
		case registered[name]:
			dead = append(dead, name+": now registered")
		case !named[name]:
			dead = append(dead, name+": no profile allow list names this tool, so the "+
				"exemption excuses nothing")
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("GOV5: %d stale toolWiringExceptions entr(ies) — the exempted subject is "+
			"registered or gone, so the exemption must be DELETED:\n  %s",
			len(dead), strings.Join(dead, "\n  "))
	}

	// Advisory only: a registered tool the default profile does not allow is
	// legitimate (a tightened profile is a valid configuration), but listing
	// them makes a forgotten authorization easy to spot in CI logs.
	//
	// Wildcards are re-applied here (they were skipped in the phantom check
	// above) so vcs_commit and friends are not reported as unauthorized when
	// "vcs_*" already covers them.
	//
	// ConditionalProfileTools names are excluded: they are absent from the
	// STATIC list by design and added to the EFFECTIVE profile by
	// extendProfileWithConditionalTools, so reporting them here would be a
	// standing false positive that trains readers to ignore this list.
	for _, n := range bootstrap.ConditionalProfileTools() {
		concrete[n] = true
	}
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

// buildAppWithProviders boots an App in the PRODUCTION model shape and returns
// it: Options.FakeModel is false AND cfg.llm.providers is non-empty, so
// bootstrap.Build takes the real-provider branch and leaves fakeChatModel nil.
//
// That nil is the whole point. SelectRLMModel reads a non-nil fake as
// permission to skip the cheap-provider requirement, so every test that boots
// through buildMinimalApp (FakeModel: true) silently gets an RLM whether or
// not batch.rlm_model is configured — which is exactly how a profile entry for
// a tool that production never registers survived GOV5.
//
// ProviderBuilder is stubbed with fakeProviderBuilder so this needs no API
// keys: it substitutes the provider TRANSPORT, not the fake-model BRANCH, so
// the config-shaped decisions under test are the production ones.
func buildAppWithProviders(t *testing.T, extraYAML string) *bootstrap.App {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfgContent := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + toYAMLPath(filepath.Join(dir, "test.db")) + `"
token: "test-token"
` + extraYAML
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))
	app, err := bootstrap.Build(bootstrap.Options{
		ConfigPath:      cfgPath,
		ProviderBuilder: fakeProviderBuilder,
	})
	require.NoError(t, err)
	require.NotNil(t, app)
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
	return app
}

// productionNoRLMYAML is the shape that broke: a real (non-fake) provider and
// NO batch.rlm_model, which is what a deployment that never opted into RLM
// looks like. BuildC1 then warns and leaves C1Components.RLM nil.
const productionNoRLMYAML = `
llm:
  providers:
    - name: main
      model: main-model
      cost_class: expensive
`

// TestGOV5ProductionProfileHasNoPhantomNames is GOV5 for the shape the
// fake-model harness structurally cannot see.
//
// TestGOV5ProfileAllowMatchesToolRegistry builds with FakeModel: true, which
// makes SelectRLMModel hand back the fake and register rlm_query no matter
// what the config says. A production deployment that never set
// batch.rlm_model gets no rlm_query at all — so a hard-coded "rlm_query" in
// the default allow list was a phantom name in the majority deployment while
// the governance test stayed green. This asserts on the EFFECTIVE profile
// (what the orchestrator was actually handed) against the EFFECTIVE registry,
// so it cannot be satisfied by filing a name under a different list.
func TestGOV5ProductionProfileHasNoPhantomNames(t *testing.T) {
	app := buildAppWithProviders(t, productionNoRLMYAML)
	registered := registeredSet(t, app)

	// Precondition: this really is the divergent shape. If rlm_query were
	// registered here the test would pass vacuously and prove nothing.
	require.False(t, registered["rlm_query"],
		"harness broken: rlm_query registered without batch.rlm_model, so this is "+
			"not the production shape and the assertion below is vacuous")
	require.True(t, registered["agent_batch"],
		"C1 must still be wired — only rlm_query degrades on a missing cheap provider")

	effective := app.Orch.Profile().Tools.Allow
	if phantom := phantomNames(effective, registered); len(phantom) > 0 {
		t.Errorf("GOV5 (production shape): the orchestrator's EFFECTIVE profile allows "+
			"%d tool(s) that this boot never registered:\n  %s\n\n"+
			"Fix: move the name into bootstrap.ConditionalProfileTools so it is derived "+
			"from the registry snapshot, or register the tool unconditionally. Do NOT "+
			"register an always-failing stub just to occupy the name.",
			len(phantom), strings.Join(phantom, "\n  "))
	}
	require.NotContains(t, effective, "rlm_query",
		"rlm_query must not be authorized in a boot that never registered it")

	// Generic invariant: nothing conditional may be hard-coded in the static
	// default, whatever the set grows to.
	static := bootstrap.DefaultOrchestratorProfile().Tools.Allow
	for _, name := range bootstrap.ConditionalProfileTools() {
		require.NotContainsf(t, static, name,
			"%q is conditionally registered but hard-coded in DefaultOrchestratorProfile — "+
				"that is the phantom-name bug this split exists to prevent", name)
	}
}

// TestGOV5ConditionalToolAuthorizedWhenRegistered is the other direction: the
// conditional split must not silently DE-authorize a tool that is present.
//
// Without this, "delete rlm_query from the allow list" would also pass
// TestGOV5ProductionProfileHasNoPhantomNames — a profile that authorizes
// nothing has no phantoms. Here a cheap provider is configured, so rlm_query
// registers, and the effective profile must name it or the tool is dead on
// arrival: guard denies it and the model gets a permission error for a
// capability the operator explicitly paid to enable.
func TestGOV5ConditionalToolAuthorizedWhenRegistered(t *testing.T) {
	// name == model on purpose: einollm.BuildProviders keys the registry by
	// model id while SelectRLMModel's cost_class check matches provider.Name,
	// so equal values exercise BOTH halves of the selection.
	app := buildAppWithProviders(t, `
llm:
  providers:
    - name: cheap-model
      model: cheap-model
      cost_class: cheap
batch:
  rlm_model: "cheap-model"
`)
	registered := registeredSet(t, app)
	require.True(t, registered["rlm_query"],
		"batch.rlm_model names a cheap provider — rlm_query must register")

	effective := app.Orch.Profile().Tools.Allow
	require.Contains(t, effective, "rlm_query",
		"rlm_query is registered but not authorized — extendProfileWithConditionalTools "+
			"did not run, so the tool is unreachable behind guard")
	require.Empty(t, phantomNames(effective, registered),
		"the extended profile must still name only registered tools")
}

// TestGOV5OperatorProfileIsNotWidened proves the conditional extension applies
// only to the factory default.
//
// An operator who writes profiles.orchestrator has made an explicit
// least-privilege statement. Appending tools they did not name would turn a
// deliberate restriction into a silent grant — the same fail-open posture the
// concrete default profile replaced.
func TestGOV5OperatorProfileIsNotWidened(t *testing.T) {
	app := buildAppWithProviders(t, `
llm:
  providers:
    - name: cheap-model
      model: cheap-model
      cost_class: cheap
batch:
  rlm_model: "cheap-model"
profiles:
  orchestrator:
    tools:
      allow: ["fs_read"]
`)
	require.True(t, registeredSet(t, app)["rlm_query"], "harness: rlm_query must be registered")
	require.Equal(t, []string{"fs_read"}, app.Orch.Profile().Tools.Allow,
		"an operator-declared profile must be used verbatim, never widened")
}

// TestGOV7EditToolsAreRegistered verifies every name in guard's allow-edits
// auto-approval set is a tool that is actually registered.
//
// Why this matters: this is GOV5's consumption-side twin. GOV5 catches phantom
// names on the AUTHORIZATION side (the default profile advertising tools that do
// not exist); this catches them on the CONSUMPTION side. Unlike the TUI's
// display-only tables (label maps, silent-tool case labels), where a dead name
// is merely clutter, guard.editTools carries AUTHORIZATION SEMANTICS: it is the
// set ModeAllowEdits auto-approves WITHOUT prompting the user. A name that
// matches no registered tool occupies a no-prompt auto-approval slot and makes
// the set read as broader than it is — exactly how fs_mkdir, a tool that was
// never registered (FSTools.Tools() ships read/write/edit/list/glob/search/
// apply_patch), survived here long after it was dropped from the default
// profile.
//
// The gate is deliberately ONE-DIRECTIONAL, and the missing direction is not an
// oversight to be closed later. It asks "is every name here registered?", never
// "is every registered write tool here?" — apply_patch is a real, registered
// file-writing tool that is intentionally absent. The reverse check would be a
// gate that FORCES names into an authorization set, i.e. one whose only way to
// go green is to auto-approve more without prompting. Widening this set is a
// deliberate authorization decision; a governance test must never be the thing
// that demands it. See guard.EditToolNames for the set itself.
//
// Why there is no exemption table (unlike GOV5's toolWiringExceptions): after
// the cleanup the set is {fs_write, fs_edit}, both of which FSTools always
// registers, so the violation set is identically empty. An exemption table with
// nothing to exempt would be dead weight — and widening this set is an
// authorization change that belongs in a deliberate work package, not behind a
// governance escape hatch.
func TestGOV7EditToolsAreRegistered(t *testing.T) {
	app := buildMinimalApp(t)

	registered := make(map[string]bool, len(app.ToolNames))
	for _, n := range app.ToolNames {
		registered[n] = true
	}
	require.NotEmpty(t, registered, "tool registry must not be empty")

	editTools := guard.EditToolNames()
	require.NotEmpty(t, editTools, "allow-edits must name concrete tools")

	var phantom []string
	for _, name := range editTools {
		if !registered[name] {
			phantom = append(phantom, name)
		}
	}
	if len(phantom) > 0 {
		t.Errorf("GOV7: guard's allow-edits auto-approval set names %d tool(s) that are "+
			"NOT registered:\n  %s\n\n"+
			"Each phantom name occupies a no-prompt auto-approval slot in a set with "+
			"authorization semantics.\nFix: remove the name from guard.editTools "+
			"(internal/guard/mode.go). Do NOT add an exemption table — a name that "+
			"authorizes nothing has no reason to stay.",
			len(phantom), strings.Join(phantom, "\n  "))
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
			"bindExecutionContext, so the diagnostics tool reports the posture as " +
			"\"unknown\" instead of what the process is actually running under " +
			"(see tools.sandboxProbe). Enforcement itself travels by constructor " +
			"field into shell.SecureLaunchFactory / shell.DefaultSecureFactory, " +
			"not through this context value.")
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

// TestShellRunEndToEndSeesHostPATHAndReportsExitCode drives legacy shell_run
// through a real orchestrator turn built by bootstrap.Build, which is the only
// configuration in which its SecureProcessFactory branch is taken at all.
//
// Two production regressions meet here, both invisible to every test that
// substitutes its own factory:
//
//   - The child environment was built from SecureProcessSpec.Env, which no
//     caller populates, so the process got HTTP_PROXY/HTTPS_PROXY/NO_PROXY and
//     nothing else. `go version` answered "sh: go: command not found" — every
//     PATH-resolved binary (go, node, python3, gh, cargo) was unreachable.
//   - The branch returned at stdout EOF without calling Wait, so there was no
//     "── exit N ──" footer and the model could not tell success from failure.
//
// `go version` is the probe because a Go test binary is running, so the
// toolchain that built it is by construction installed; LookPath still gates
// the case where it is not on PATH (e.g. an oddly packaged CI image).
func TestShellRunEndToEndSeesHostPATHAndReportsExitCode(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go not on PATH: %v", err)
	}
	app := buildMinimalApp(t)
	call := schema.AssistantMessage("", []schema.ToolCall{{
		ID: "s1", Type: "function", Function: schema.FunctionCall{
			Name: "shell_run", Arguments: `{"command":"go version"}`,
		},
	}})
	res := runShellTurn(t, app, []*schema.Message{call})

	out, ok := res["shell_run"]
	require.Truef(t, ok, "no shell_run tool_result (results: %v)", res)
	require.NotContainsf(t, out, "command not found",
		"the spawned child cannot resolve binaries through PATH — its environment was "+
			"rebuilt from an empty spec.Env instead of the host's (result: %s)", out)
	require.Containsf(t, out, "go version",
		"shell_run did not return the command's stdout (result: %s)", out)
	require.Containsf(t, out, "── exit 0 ·",
		"shell_run returned no exit-code footer, so the model cannot distinguish a "+
			"successful command from a failed one (result: %s)", out)
}

// TestShellV2TaskJobIsControllableWithTheIDItReturns spawns a REAL long-lived
// process through the tools bootstrap registers, then drives the two job tools
// with the id task_shell_start handed back.
//
// task_shell_stdin and task_shell_cancel used to pass that id to
// Manager.Write/Manager.Cancel, which only ever consult the session map, so
// both answered "shell: session/job not found" for the one id the model ever
// sees. A background job could be started and never stopped — and none of the
// four task_shell_* tools had a single test.
//
// It has to be a real process: a fake that exits immediately would let a
// cancel that does nothing look identical to a cancel that works.
func TestShellV2TaskJobIsControllableWithTheIDItReturns(t *testing.T) {
	app := buildMinimalApp(t)

	// A command that stays alive long enough to be canceled, on both families
	// of platform shell. ping is the classic Windows sleep and, unlike
	// `timeout /t`, tolerates redirected stdin.
	longRunning := "sleep 30"
	if runtime.GOOS == "windows" {
		longRunning = "ping -n 30 127.0.0.1"
	}
	start := schema.AssistantMessage("", []schema.ToolCall{{
		ID: "j1", Type: "function", Function: schema.FunctionCall{
			Name: "task_shell_start", Arguments: `{"command":"` + longRunning + `"}`,
		},
	}})
	startRes := runShellTurn(t, app, []*schema.Message{start})

	raw, ok := startRes["task_shell_start"]
	require.Truef(t, ok, "no task_shell_start tool_result (results: %v)", startRes)
	var job struct {
		ID        string `json:"id"`
		SessionID string `json:"session_id"`
		PID       int    `json:"pid"`
	}
	require.NoErrorf(t, json.Unmarshal([]byte(raw), &job),
		"task_shell_start did not return a job; got %q", raw)
	require.NotEmpty(t, job.ID, "job id missing from task_shell_start result")
	require.NotZerof(t, job.PID, "job PID is 0 — no OS process was spawned (result: %s)", raw)
	require.NotEqualf(t, job.ID, job.SessionID,
		"job id and session id must differ, otherwise this test cannot detect the namespace bug")

	// Both tools are driven with job.ID — the ONLY id the model ever receives.
	stdin := schema.AssistantMessage("", []schema.ToolCall{{
		ID: "j2", Type: "function", Function: schema.FunctionCall{
			Name: "task_shell_stdin", Arguments: `{"id":"` + job.ID + `","data":"noop\n"}`,
		},
	}})
	cancel := schema.AssistantMessage("", []schema.ToolCall{{
		ID: "j3", Type: "function", Function: schema.FunctionCall{
			Name: "task_shell_cancel", Arguments: `{"id":"` + job.ID + `"}`,
		},
	}})
	res := runShellTurn(t, app, []*schema.Message{stdin, cancel})

	// task_shell_stdin resolves the id and reaches the console. It cannot
	// succeed on this platform yet for an unrelated and much older reason:
	// StartPTYProcess returns ErrPTYUnavailable everywhere in Phase 0, so every
	// session gets a pipeConsole whose Write is documented read-only (stdin is
	// not wired for non-PTY spawns — process.go). What this asserts is exactly
	// what the id fix is responsible for: the call no longer dies at the lookup.
	// Manager.WriteJob's success path is covered against a writable console in
	// TestManagerJobMethodsResolveJobIDsNotSessionIDs.
	stdinRaw, ok := res["task_shell_stdin"]
	require.Truef(t, ok, "no task_shell_stdin tool_result (results: %v)", res)
	require.NotContainsf(t, stdinRaw, "session/job not found",
		"task_shell_stdin rejected the id task_shell_start returned (result: %s)", stdinRaw)
	require.Truef(t,
		strings.Contains(stdinRaw, `"written"`) || strings.Contains(stdinRaw, "pipe console is read-only"),
		"task_shell_stdin neither wrote nor hit the known Phase 0 pipe-console limit (result: %s)", stdinRaw)

	cancelRaw, ok := res["task_shell_cancel"]
	require.Truef(t, ok, "no task_shell_cancel tool_result (results: %v)", res)
	require.NotContainsf(t, cancelRaw, "session/job not found",
		"task_shell_cancel rejected the id task_shell_start returned, so a background "+
			"job cannot be stopped once started (result: %s)", cancelRaw)
	require.Containsf(t, cancelRaw, `"canceled":true`, "unexpected cancel result: %s", cancelRaw)

	// The manager's own view must agree: the process was really killed, not
	// merely reported as canceled by a tool that returned early.
	jobs := app.ShellManager.ListJobs()
	require.Len(t, jobs, 1, "expected exactly one job in the table")
	require.Equalf(t, shell.StateCanceled, jobs[0].State,
		"job state = %s, want canceled", jobs[0].State)
	require.Equalf(t, shell.StateCanceled, app.ShellManager.Snapshot(job.SessionID).State,
		"the underlying session was not canceled")
}

// TestProviderWindowsReachTheOrchestrator pins the composition root's half of
// the per-model compaction window.
//
// The orchestrator resolves a turn's window from CompactionConfig.ProviderWindows,
// and internal/agent/orchestrator has its own tests for that lookup. Neither
// side notices if bootstrap stops filling the map: windowFor returns 0 for
// every model, wrapCompaction reads 0 as "use the configured window", and every
// provider silently shares the global fallback again -- which is exactly the
// defect W4 removed, restored without a single test going red. Measured: with
// this assignment set to nil, bootstrap, orchestrator and archtest all stay
// green.
//
// Checked at the source because the map is not reachable from App: it goes
// into orchestrator.Config, which Build consumes and does not retain. A test
// that reconstructed the whole Build to observe it would be pinning the
// assembly harness rather than this line.
func TestProviderWindowsReachTheOrchestrator(t *testing.T) {
	src, err := os.ReadFile("bootstrap.go")
	if err != nil {
		t.Fatalf("read bootstrap.go: %v", err)
	}
	if !strings.Contains(string(src), "ProviderWindows:   providerWindows,") {
		t.Error("the orchestrator's CompactionConfig no longer receives providerWindows: " +
			"every model would fall back to the global context window, so a provider " +
			"with a smaller one gets a compaction threshold it can never reach")
	}
}
