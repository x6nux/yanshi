package acp

import "encoding/json"

// SpawnOptions configures SpawnSecure.
type SpawnOptions struct {
	Agent     string // "opencode" | "claudecode" | "codex"
	Cwd       string
	ExtraDirs []string
	// Env carries extra environment entries for the child. It is NOT a way to
	// hand it credentials: secproc.Launch scrubs every credential-shaped
	// variable out of the spec before the factory sees it, and the ACP spawn
	// declares no AllowEnv, so a provider key placed here is dropped (and the
	// drop is logged by name). An agent CLI that needs to authenticate does so
	// from its own on-disk login. This field's remaining production use is the
	// delegation-depth marker acp_delegate sets.
	Env    []string
	Policy Policy // optional; if nil, no gating (fs/terminal caps still advertised)

	// WorktreeID, when non-empty with a Recorder, auto-tracks agent file edits
	// into a VCS worktree. Typed as a callback to keep this package free of a
	// vcs import.
	WorktreeID string
	Recorder   func(worktreeID, agent, absPath string, content []byte) error

	// MCPCommand, when non-nil, is serialized as the "yanshi-vcs" entry in the
	// session/new mcpServers map. It must describe the stdio command the ACP
	// adapter spawns, per the MCP stdio config shape:
	//
	//	{"command": "<bin>", "args": ["vcs-mcp"], "env": {"YANSHI_DB_PATH": "...", ...}}
	//
	// The CALLER constructs this map (the caller knows the db path, repo id, and
	// worktree dir); Spawn stays decoupled from vcs/store internals. When nil,
	// the mcpServers map is sent as {} (present-but-empty, which adapters
	// require).
	MCPCommand map[string]any
}

// buildMcpServers constructs the mcpServers map sent in session/new. When
// opts.MCPCommand is set, it is marshaled as the "yanshi-vcs" entry (the ACP
// adapter spawns it as a stdio subprocess). The result is always non-nil —
// adapters (e.g. @agentclientprotocol/claude-agent-acp) reject session/new with
// -32602 when mcpServers is absent, so it MUST be present even when empty.
//
// Extracted from the spawn path so the mcpServers-building logic can be
// unit-tested directly without spawning an agent subprocess.
func buildMcpServers(opts SpawnOptions) map[string]json.RawMessage {
	servers := map[string]json.RawMessage{}
	if opts.MCPCommand == nil {
		return servers
	}
	entry, err := json.Marshal(opts.MCPCommand)
	if err != nil {
		// A marshal failure of a caller-built map is a programming error; fall
		// back to sending no entry rather than aborting the whole spawn.
		return servers
	}
	servers["yanshi-vcs"] = entry
	return servers
}
