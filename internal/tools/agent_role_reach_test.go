package tools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/tools"
)

// roleReachTools builds agent tools plus a context whose profile allows
// everything, so the only thing narrowing the sub-agent is the role.
func roleReachTools(t *testing.T) (*tools.AgentTools, context.Context) {
	t.Helper()
	at := tools.NewAgentTools(&einollm.FakeModel{Echo: true})
	return at, tools.WithProfile(context.Background(), allowAll("*"))
}

// TestAgentStartOffersEveryRole is the reachability clause.
//
// The role catalogue, the intersection with the caller's tools, the
// case-insensitive lookup and the unknown-value rejection were all tested — on
// agent_spawn, which DefaultOrchestratorProfile does not permit. Measured:
// guard.Check(DefaultOrchestratorProfile(), Action{Tool: "agent_spawn"}) is "not
// permitted". So every one of those tests was describing a parameter no shipped
// configuration could reach, and "seven selectable roles" was true only of a
// tool nobody can call.
//
// agent_start IS in the factory allow list, so the role now lives there too.
// That is not a privilege change: resolveSpawnRole returns role ∩ caller, so a
// role can only narrow the inherited tool set.
//
// ledger: B1/M05#1 7 角色可选
func TestAgentStartOffersEveryRole(t *testing.T) {
	at, _ := roleReachTools(t)

	info, err := at.StartAgent.Info(context.Background())
	require.NoError(t, err)
	js, err := info.ParamsOneOf.ToJSONSchema()
	require.NoError(t, err)
	require.NotNil(t, js)

	roleParam, ok := js.Properties.Get("role")
	require.True(t, ok, "agent_start has no role parameter, so the roles are unreachable "+
		"from the only delegation tool the factory profile permits")

	declared := map[string]bool{}
	for _, v := range roleParam.Enum {
		if s, isStr := v.(string); isStr {
			declared[s] = true
		}
	}
	names := tools.AgentRoleNames()
	require.Len(t, names, 7, "the catalogue no longer has seven roles")
	for _, n := range names {
		assert.True(t, declared[n],
			"role %q is in the catalogue but not offered by agent_start; the model "+
				"cannot select what it is not shown", n)
	}
	assert.Len(t, roleParam.Enum, len(names),
		"agent_start advertises a role the catalogue does not define")
}

// TestAgentStartRoleNarrowsTheToolSet proves the parameter is applied, not just
// declared.
//
// A parameter that appears in the schema and is dropped on the floor is the
// exact shape of a phantom capability: the model selects "explore", believes it
// is running a read-only sub-agent, and gets one with every tool the parent
// had. The runner records what it was handed.
//
// ledger: B1/M05#1 7 角色可选
func TestAgentStartRoleNarrowsTheToolSet(t *testing.T) {
	at, ctx := roleReachTools(t)

	var handed []string
	ctx = tools.WithSubAgentRunner(ctx, tools.SubAgentRunner(
		func(ic context.Context, prompt string, allowed []string, instr string) (string, error) {
			handed = allowed
			return "done", nil
		}))

	// "explore" is a role with an explicit tool list in the catalogue.
	out, err := at.StartAgent.InvokableRun(ctx,
		`{"prompt":"look around","role":"explore"}`)
	require.NoError(t, err)
	require.NotContains(t, out, "unknown role", out)

	require.NotEmpty(t, handed,
		"the sub-agent inherited the caller's full tool set: the role was declared but never applied")
	for _, name := range handed {
		assert.NotEqual(t, "fs_write", name,
			"the explore role handed the sub-agent a write tool: %v", handed)
	}

	// Control: omitting the role must NOT narrow anything, or every existing
	// caller silently loses tools. An absent role resolves to "general", whose
	// allow list is ["*"] — so the list may be non-empty, but it has to match
	// everything rather than name a subset.
	handed = nil
	_, err = at.StartAgent.InvokableRun(ctx, `{"prompt":"look around"}`)
	require.NoError(t, err)
	if len(handed) > 0 {
		require.Equal(t, []string{"*"}, handed,
			"omitting the role narrowed the tool set to %v; an absent role must inherit everything",
			handed)
	}
}

// TestAgentStartRejectsUnknownRoleAndListsValidOnes mirrors the agent_spawn
// behaviour on the entry point that is actually reachable.
//
// ledger: B1/M05#5 未知值返回可接受集
func TestAgentStartRejectsUnknownRoleAndListsValidOnes(t *testing.T) {
	at, ctx := roleReachTools(t)
	ctx = tools.WithSubAgentRunner(ctx, tools.SubAgentRunner(
		func(context.Context, string, []string, string) (string, error) {
			t.Error("the sub-agent ran despite an unknown role")
			return "", nil
		}))

	out, err := at.StartAgent.InvokableRun(ctx, `{"prompt":"x","role":"nonexistent"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "unknown role")
	// Listing the acceptable values is the clause: a bare rejection leaves the
	// model guessing, and it will guess again.
	for _, n := range tools.AgentRoleNames() {
		assert.Contains(t, out, n, "the rejection does not list %q as a valid role: %s", n, out)
	}
}

// TestAgentStartRoleNameIsCaseInsensitive mirrors the alias clause.
//
// ledger: B1/M05#4 别名大小写不敏感
func TestAgentStartRoleNameIsCaseInsensitive(t *testing.T) {
	at, ctx := roleReachTools(t)
	var ran bool
	ctx = tools.WithSubAgentRunner(ctx, tools.SubAgentRunner(
		func(context.Context, string, []string, string) (string, error) {
			ran = true
			return "done", nil
		}))

	out, err := at.StartAgent.InvokableRun(ctx, `{"prompt":"x","role":"EXPLORE"}`)
	require.NoError(t, err)
	assert.False(t, strings.Contains(out, "unknown role"),
		"an upper-case role name was rejected: %s", out)
	assert.True(t, ran, "the sub-agent never ran")
}

// TestGeneralRoleDoesNotStripEveryTool is the regression guard for a defect the
// role work surfaced.
//
// The "general" role's allow list is ["*"] — the vocabulary every other allow
// list in this repo uses — but selectSubAgentTools compared names exactly, so
// "*" was just a tool name nothing has. A sub-agent spawned with the general
// role got ZERO tools while its allow list said "everything": no error, no log,
// an agent answering from the prompt alone. The defect predates this work
// (agent_spawn's general role took the same path); giving agent_start a role
// parameter is what made it observable.
//
// The assertion is on what the runner is HANDED, because that is the value the
// orchestrator then filters with.
func TestGeneralRoleDoesNotStripEveryTool(t *testing.T) {
	at, ctx := roleReachTools(t)
	var handed []string
	ctx = tools.WithSubAgentRunner(ctx, tools.SubAgentRunner(
		func(ic context.Context, prompt string, allowed []string, instr string) (string, error) {
			handed = allowed
			return "done", nil
		}))

	_, err := at.StartAgent.InvokableRun(ctx, `{"prompt":"x","role":"general"}`)
	require.NoError(t, err)

	// Either nil (inherit everything) or a pattern that actually matches
	// everything. What must not happen is a list that matches nothing.
	if len(handed) > 0 {
		assert.Contains(t, handed, "*",
			"the general role handed down %v, which is neither 'inherit all' nor a "+
				"pattern matching all", handed)
	}
}
