package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// SpawnOptions configures Spawn.
type SpawnOptions struct {
	Agent     string // "opencode" | "claudecode" | "codex"
	Cwd       string
	ExtraDirs []string
	Env       []string // optional extra env (e.g. API keys)
	Policy    Policy   // optional; if nil, no gating (fs/terminal caps still advertised)

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

// Spawned is the result of Spawn: a ready Client and the underlying Cmd/process
// so the caller can Wait/cleanup. SessionID is the ID of the session created
// during the spawn handshake (needed for subsequent Prompt calls).
type Spawned struct {
	Client    *Client
	Cmd       *exec.Cmd
	SessionID string
}

// Spawn launches an external ACP agent as a subprocess, wraps its stdin/stdout
// pipes in a Client, and runs the initialize -> session/new handshake.
//
// The returned Spawned.Client is ready for Prompt. The caller is responsible
// for cleanup: call Client.Close, then Cmd.Wait.
//
// The full subprocess+init flow is exercised end-to-end in the optional Task 9
// (real-CLI E2E smoke). Unit tests cover argv/cmd construction and error paths
// without starting a real process.
func Spawn(ctx context.Context, opts SpawnOptions) (*Spawned, error) {
	cmd, err := buildCmd(ctx, opts)
	if err != nil {
		return nil, err
	}

	// Create stdin/stdout pipes for JSON-RPC communication with the agent.
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		stdinPipe.Close()
		return nil, fmt.Errorf("acp: stdout pipe: %w", err)
	}

	// Capture stderr for diagnostics.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("acp: spawn %q: %w", opts.Agent, err)
	}

	// Wrap the pipes in a Client. The agent reads from stdinPipe (client writes)
	// and writes to stdoutPipe (client reads).
	client := NewClient(stdoutPipe, stdinPipe)
	if opts.Policy != nil {
		client.SetPolicy(opts.Policy)
	}
	if opts.WorktreeID != "" {
		client.SetVCSTracking(opts.WorktreeID, opts.Recorder)
	}

	// Advertise fs read/write + terminal capabilities.
	caps := ClientCapabilities{
		FS:       &FSCap{ReadTextFile: true, WriteTextFile: true},
		Terminal: true,
	}
	if _, err := client.Initialize(ctx, caps); err != nil {
		client.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return nil, fmt.Errorf("acp: initialize %q: %w", opts.Agent, err)
	}

	sessionID, err := client.NewSession(ctx, opts.Cwd, opts.ExtraDirs, buildMcpServers(opts))
	if err != nil {
		client.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return nil, fmt.Errorf("acp: session/new %q: %w", opts.Agent, err)
	}

	return &Spawned{Client: client, Cmd: cmd, SessionID: sessionID}, nil
}

// buildMcpServers constructs the mcpServers map sent in session/new. When
// opts.MCPCommand is set, it is marshaled as the "yanshi-vcs" entry (the ACP
// adapter spawns it as a stdio subprocess). The result is always non-nil —
// adapters (e.g. @agentclientprotocol/claude-agent-acp) reject session/new with
// -32602 when mcpServers is absent, so it MUST be present even when empty.
//
// Extracted from Spawn so the mcpServers-building logic can be unit-tested
// directly without spawning an agent subprocess.
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

// buildCmd resolves the argv via LaunchSpec and constructs an *exec.Cmd with
// Dir and Env configured. Pipes and Start are the caller's responsibility.
// This is factored out so tests can verify argv/Dir/Env without starting a
// process.
func buildCmd(ctx context.Context, opts SpawnOptions) (*exec.Cmd, error) {
	argv, err := LaunchSpec(opts.Agent)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	cmd.Env = append(os.Environ(), opts.Env...)
	return cmd, nil
}
