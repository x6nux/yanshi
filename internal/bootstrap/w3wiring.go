// Package bootstrap — Wave 3 composition-root wiring.
//
// This file holds the assembly for the capabilities Wave 3 added: T3
// background offload, T7 skill authoring, C7 milestones, T9 ACP delegation and
// the T10 MCP token store, plus buildMCPManager itself. It lives apart from
// bootstrap.go for the reason c1wiring.go and profile.go give: bootstrap.go is
// hard against the 1000-code-line GOV2 ceiling and lineExceptions is an empty,
// removal-only map, so there is no exemption left to spend on it.
//
// Every function here is called by Build. That matters more than it looks:
// each of these components shipped complete, tested, and with zero non-test
// callers, which is the failure mode GOV4 exists for and the one this repo
// keeps rediscovering. A tool that is constructed but never appended to
// allTools is refused at runtime by toolreg.Check (S8) with a message about an
// unregistered name, and no unit test in the owning package can see it.
package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/mcp"
	"github.com/x6nux/yanshi/internal/secrets"
	"github.com/x6nux/yanshi/internal/skills"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
)

// BuildBackgroundManager returns the process-wide manager for tool calls that
// were moved to the background when they hit their foreground deadline (T3).
//
// One per process, not one per turn: a run cancelled when its turn ends is not
// a background run, it is the same timeout with extra steps. The lifetime is
// the binary's, which is why App.Shutdown must Close it — otherwise a wedged
// subprocess outlives the process that spawned it.
func BuildBackgroundManager() *tools.BackgroundManager {
	return tools.NewBackgroundManager()
}

// BuildBackgroundTools returns the three model-facing query tools over the
// background manager: background_list, background_result, background_cancel.
//
// They read the manager from the turn context (tools.WithBackgroundManager,
// bound by the orchestrator from Config.Background) rather than closing over
// one, so a sub-agent with its own manager is never served the parent's runs.
// Registering them is not optional decoration: the offload notice hands the
// model a run id, and without a way to spend that id the handle is a token the
// model is told to remember and can never use.
func BuildBackgroundTools() []orchestrator.BaseTool {
	var out []orchestrator.BaseTool
	for _, t := range tools.NewBackgroundTools().Tools() {
		out = append(out, t)
	}
	return out
}

// BuildMilestoneTools returns the C7 milestone_set tool, which lets the model
// label its own work as it goes so compaction can keep those labels when the
// underlying turns are evicted.
func BuildMilestoneTools(st *store.Store) []orchestrator.BaseTool {
	var out []orchestrator.BaseTool
	for _, t := range tools.NewMilestoneTools(st).Tools() {
		out = append(out, t)
	}
	return out
}

// BuildSkillWriteTool returns the T7 skill_write tool, or nil when there is
// nowhere to write.
//
// The nil case is the reason this is a function rather than a line in Build.
// With no user skills directory the tool would exist and refuse every call,
// which is a phantom capability the model pays schema tokens for on every
// request. Because registration is conditional, "skill_write" must live in
// ConditionalProfileTools rather than the static allow list — a name hard-coded
// there would be a phantom in exactly the deployments where the condition
// fails, which is the shape GOV5 structurally cannot see.
func BuildSkillWriteTool(userSkillsDir string, reg *skills.Registry, loader *skills.Loader) orchestrator.BaseTool {
	if userSkillsDir == "" {
		return nil
	}
	return tools.NewSkillWriteTool(userSkillsDir, reg, loader)
}

// BuildACPDelegateTool returns the T9 acp_delegate tool, which hands one
// self-contained subtask to an external agent CLI.
//
// Registered but deliberately NOT added to DefaultOrchestratorProfile. It runs
// a third-party binary that executes code of its own choosing, making it the
// highest-capability tool in the registry, and the guard's response to a name
// that misses every glob is Prompt rather than HardDeny. So leaving it out
// means WS asks the user each time and SSE — which has no permission callback
// — fails closed. That is the correct gradient for this tool, and it is not a
// phantom: GOV5 checks that every allowed name is registered, not the converse.
func BuildACPDelegateTool(vcsDBPath, worktreeDir string) orchestrator.BaseTool {
	return tools.NewACPDelegateTool(tools.ACPDelegateConfig{
		VCSDBPath:   vcsDBPath,
		WorktreeDir: worktreeDir,
	})
}

// bindMCPTokenStore binds the credential-backed token store to the MCP manager
// so authorization_code servers can read the tokens `yanshi auth mcp-login`
// wrote (T10).
//
// It must run BEFORE StartAll: the token source is constructed once per server
// at start, so a store bound afterwards is read by nothing — the classic
// written-but-never-read shape, and one that fails silently because the server
// simply falls back to its configured bearer.
//
// A missing secrets backend is a warning, not a boot failure. It disables only
// the authorization_code grant; client_credentials and bearer servers are
// unaffected, and refusing to boot the whole MCP subsystem over a credential
// backend most deployments never configure would be the wrong trade.
func bindMCPTokenStore(mgr *mcp.Manager, secretMgr *secrets.Manager) {
	store, err := mcp.NewSecretsTokenStore(secretMgr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yanshi: MCP authorization_code servers disabled: %v\n", err)
		return
	}
	mgr.SetTokenStore(store)
}

// mcpOAuthFromConfig projects the YAML oauth block onto the runtime shape.
//
// Two structs exist on purpose — config.MCPOAuthConfig is the operator surface,
// mcp.OAuthConfig is the wire/runtime shape — and this is the single point that
// joins them. Before it existed, mcp.ServerConfig.OAuth was reachable from Go
// and from no YAML, so the whole authorization_code flow was dead on any real
// deployment.
//
// Grant is passed through rather than normalised here: the empty string is a
// meaningful alias for client_credentials that every pre-T10 config relies on,
// and mcp.NormalizeGrant owns that decision.
func mcpOAuthFromConfig(oc *config.MCPOAuthConfig) *mcp.OAuthConfig {
	if oc == nil {
		return nil
	}
	return &mcp.OAuthConfig{
		TokenURL:         oc.TokenURL,
		AuthorizationURL: oc.AuthorizationURL,
		ClientID:         oc.ClientID,
		ClientSecret:     oc.ClientSecret,
		Scopes:           oc.Scopes,
		Grant:            oc.Grant,
	}
}

// buildMCPManager projects config.MCP.Servers onto the mcp package's shape,
// binds the token store, starts every server and installs the health loop.
//
// It moved here from bootstrap.go for GOV2 headroom — that file had reached
// 999 of its 1000 permitted code lines and lineExceptions is an empty,
// removal-only map. This is its natural home anyway: the OAuth projection and
// the token-store binding it depends on are both in this file.
//
// cmd/yanshi.mcpServerConfigs is a second, deliberately narrower projection for
// `auth mcp-login`, which must work while a server is unreachable. Both must
// carry the oauth block: the CLI reads authorization_url/client_id from its
// copy and the agent reads token_url from this one, so projecting in only one
// place yields a login that succeeds and a server that never uses the token.
func buildMCPManager(cfg *config.Config, secretMgr *secrets.Manager) *mcp.Manager {
	servers := make(map[string]*mcp.ServerConfig, len(cfg.MCP.Servers))
	for name, sc := range cfg.MCP.Servers {
		d := 30 * time.Second
		if sc.Timeout != "" {
			if parsed, err := time.ParseDuration(sc.Timeout); err == nil {
				d = parsed
			}
		}
		transport := mcp.TransportStdio
		if sc.Transport == "http" {
			transport = mcp.TransportHTTP
		}
		servers[name] = &mcp.ServerConfig{
			Name: name, Enabled: sc.Enabled, Transport: transport,
			Command: sc.Command, Args: sc.Args, Env: sc.Env,
			URL: sc.URL, Bearer: sc.Bearer, Timeout: d, Reconnect: sc.Reconnect,
			OAuth: mcpOAuthFromConfig(sc.OAuth),
		}
	}
	mgr := mcp.NewManager(servers)
	// T10: bind the token store BEFORE StartAll — the per-server token source
	// is built at start, so a store bound afterwards is read by nothing.
	bindMCPTokenStore(mgr, secretMgr)
	for _, st := range mgr.StartAll(context.Background()) {
		if st.Status == mcp.StatusFailed {
			slog.Warn("mcp server failed to start", "server", st.Name, "error", st.Error)
		}
	}
	// internal/mcp/health.go shipped complete, tested, and with ZERO non-test
	// callers: SetHealthConfig, StartHealthLoop and CallToolRetry were all
	// written and never wired. A server that died after StartAll stayed Ready
	// forever, its tools kept being advertised to the model, and every call to
	// them failed with no reconnect attempted.
	//
	// GOV4 does not catch this shape. It asserts bootstrap's exported Build*
	// functions are reachable from Build, and buildMCPManager IS reachable;
	// what went unwired is a method on a component the composition root
	// already holds -- one level below where the gate looks.
	mgr.SetHealthConfig(mcp.DefaultHealthConfig())
	return mgr
}
