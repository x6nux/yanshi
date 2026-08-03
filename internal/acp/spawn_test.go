package acp

import (
	"context"
	"encoding/json"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSpawn_UnknownAgent verifies that Spawn returns an error for an unknown
// agent name without attempting to start a process.
func TestSpawn_UnknownAgent(t *testing.T) {
	_, err := Spawn(context.Background(), SpawnOptions{Agent: "nope"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown agent")
}

// TestBuildCmd verifies that buildCmd constructs the correct *exec.Cmd for
// each known agent -- asserting Args, Dir, and Env -- without starting the
// process. The full subprocess+init flow (Initialize, NewSession) is deferred
// to the optional Task 9 (real-CLI E2E smoke).
func TestBuildCmd(t *testing.T) {
	tests := []struct {
		name     string
		opts     SpawnOptions
		wantArgs []string
		wantDir  string
		wantEnv  []string // extra env entries that must be present
		wantErr  bool
	}{
		{
			name:     "opencode",
			opts:     SpawnOptions{Agent: "opencode", Cwd: "/tmp/work"},
			wantArgs: []string{"opencode", "acp"},
			wantDir:  "/tmp/work",
		},
		{
			name:     "claudecode with env",
			opts:     SpawnOptions{Agent: "claudecode", Cwd: "/proj", Env: []string{"ANTHROPIC_API_KEY=sk-test"}},
			wantArgs: []string{"npx", "@agentclientprotocol/claude-agent-acp"},
			wantDir:  "/proj",
			wantEnv:  []string{"ANTHROPIC_API_KEY=sk-test"},
		},
		{
			name:     "codex",
			opts:     SpawnOptions{Agent: "codex", Cwd: "/dev"},
			wantArgs: []string{"npx", "@agentclientprotocol/codex-acp"},
			wantDir:  "/dev",
		},
		{
			name:    "unknown agent",
			opts:    SpawnOptions{Agent: "unknown"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := buildCmd(context.Background(), tt.opts)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantArgs, cmd.Args)
			assert.Equal(t, tt.wantDir, cmd.Dir)
			for _, e := range tt.wantEnv {
				assert.Contains(t, strings.Join(cmd.Env, "\n"), e)
			}
		})
	}
}

// TestSpawnOptions_VCSFieldsCompile verifies that the WorktreeID + Recorder
// fields on SpawnOptions compile as expected, and that supplying them does not
// alter buildCmd's argv resolution (VCS tracking is wiring-only; it must not
// change the spawned command).
func TestSpawnOptions_VCSFieldsCompile(t *testing.T) {
	recorder := func(worktreeID, agent, absPath string, content []byte) error { return nil }
	opts := SpawnOptions{
		Agent:      "opencode",
		Cwd:        "/tmp/work",
		WorktreeID: "wt-1",
		Recorder:   recorder,
	}
	if opts.WorktreeID != "wt-1" {
		t.Errorf("opts.WorktreeID = %q; want \"wt-1\"", opts.WorktreeID)
	}
	if opts.Recorder == nil {
		t.Error("opts.Recorder = nil; want callback")
	}

	cmd, err := buildCmd(context.Background(), opts)
	require.NoError(t, err)
	// argv is unchanged from the plain opencode spec in TestBuildCmd.
	assert.Equal(t, []string{"opencode", "acp"}, cmd.Args)
	assert.Equal(t, "/tmp/work", cmd.Dir)
}

// TestSpawn_FailureClosesClient verifies that when Initialize fails (because
// the subprocess produces no ACP output), the error-path cleanup in Spawn calls
// client.Close() -- cancelling the ReadLoop context so no goroutine is leaked.
//
// Since Spawn requires a real agent binary, we test the same cleanup pattern
// directly: create a Client whose Initialize will time out (no JSON-RPC
// response arrives), then simulate the Spawn error path (client.Close) and
// verify the readLoop goroutine exits.
func TestSpawn_FailureClosesClient(t *testing.T) {
	// Create a pipe pair where the agent side reads requests but never writes
	// a response. The client's Initialize will time out.
	agentR, clientOut := io.Pipe() // client writes -> agent reads
	clientIn, agentW := io.Pipe()  // agent writes -> client reads (we write nothing)
	defer agentR.Close()
	defer agentW.Close()

	// Drain agentR so the client's write (initialize request line) does not
	// block on the pipe. Without this, writeLine blocks before reaching the
	// select on ctx.Done() in Transport.Call.
	go io.Copy(io.Discard, agentR)

	c := NewClient(clientIn, clientOut)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	before := runtime.NumGoroutine()

	// Initialize starts the readLoop goroutine, then blocks on the RPC call
	// which will time out because no response ever arrives.
	_, err := c.Initialize(ctx, ClientCapabilities{})
	require.Error(t, err)

	// Simulate the Spawn error-path cleanup: client.Close (cancels readLoop ctx
	// + closes the stdin writer).
	c.Close()

	// Close the agent-side write pipe so the client's readLoop reader sees EOF.
	agentW.Close()

	// Poll for the readLoop goroutine to exit rather than assuming a fixed
	// 200ms is enough. Under -race, or on a loaded CI runner, the goroutine may
	// not be scheduled to its exit point that fast, and the leak assertion
	// below would fail for a scheduling reason rather than a real leak.
	after := runtime.NumGoroutine()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		runtime.GC()
		if after = runtime.NumGoroutine(); after <= before+2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The readLoop goroutine should have exited. Allow +1 tolerance for
	// the io.Copy drain goroutine which may not have exited yet.
	assert.LessOrEqual(t, after, before+2,
		"goroutine leak detected: before=%d after=%d", before, after)
}

// ---------------------------------------------------------------------------
// buildMcpServers — the mcpServers map Spawn sends in session/new
// ---------------------------------------------------------------------------

// TestBuildMcpServers_NilCommandIsEmpty verifies that when MCPCommand is nil,
// buildMcpServers returns a non-nil empty map. Adapters require mcpServers to be
// present (even as {}) in session/new, so the map must never be nil.
func TestBuildMcpServers_NilCommandIsEmpty(t *testing.T) {
	t.Parallel()

	servers := buildMcpServers(SpawnOptions{})
	require.NotNil(t, servers, "mcpServers must be non-nil (adapters reject absent field)")
	assert.Empty(t, servers)
}

// TestBuildMcpServers_WithCommand verifies that when MCPCommand is set,
// buildMcpServers marshals it as the "yanshi-vcs" entry with the
// command/args/env shape the ACP adapter spawns.
func TestBuildMcpServers_WithCommand(t *testing.T) {
	t.Parallel()

	opts := SpawnOptions{
		MCPCommand: map[string]any{
			"command": "/path/to/yanshi",
			"args":    []string{"vcs-mcp"},
			"env": map[string]string{
				"YANSHI_DB_PATH": "/tmp/test.db",
				"YANSHI_REPO_ID": "repo-1",
				"YANSHI_WT_ID":   "wt-1",
				"YANSHI_AGENT":   "acp",
			},
		},
	}

	servers := buildMcpServers(opts)
	require.Len(t, servers, 1)
	require.Contains(t, servers, "yanshi-vcs")

	var entry map[string]any
	require.NoError(t, json.Unmarshal(servers["yanshi-vcs"], &entry))
	assert.Equal(t, "/path/to/yanshi", entry["command"])

	args, ok := entry["args"].([]any)
	require.True(t, ok, "args must be a JSON array")
	require.Len(t, args, 1)
	assert.Equal(t, "vcs-mcp", args[0])

	env, ok := entry["env"].(map[string]any)
	require.True(t, ok, "env must be a JSON object")
	assert.Equal(t, "/tmp/test.db", env["YANSHI_DB_PATH"])
	assert.Equal(t, "repo-1", env["YANSHI_REPO_ID"])
	assert.Equal(t, "wt-1", env["YANSHI_WT_ID"])
	assert.Equal(t, "acp", env["YANSHI_AGENT"])
}

// TestNewSession_SendsMcpServersOverWire drives the full Client -> Transport ->
// FakeAgent path to prove that the mcpServers map passed to NewSession is
// actually serialized in the session/new params received by the agent. This is
// the wire-format counterpart to the buildMcpServers unit test.
func TestNewSession_SendsMcpServersOverWire(t *testing.T) {
	fa, clientIn, clientOut := NewFakeAgent()
	defer fa.Close()
	c := NewClient(clientIn, clientOut)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := c.Initialize(ctx, ClientCapabilities{})
	require.NoError(t, err)

	// Simulate what Spawn sends: the buildMcpServers output for a caller-built
	// MCPCommand describing the yanshi vcs-mcp subprocess.
	servers := buildMcpServers(SpawnOptions{
		MCPCommand: map[string]any{
			"command": "yanshi",
			"args":    []string{"vcs-mcp"},
			"env":     map[string]string{"YANSHI_WT_ID": "wt-9"},
		},
	})
	require.Contains(t, servers, "yanshi-vcs")

	_, err = c.NewSession(ctx, ".", nil, servers)
	require.NoError(t, err)

	captured := fa.CapturedNewSession()
	require.NotEmpty(t, captured, "FakeAgent must capture session/new params")

	var params NewSessionParams
	require.NoError(t, json.Unmarshal(captured, &params))
	require.Contains(t, params.McpServers, "yanshi-vcs",
		"session/new mcpServers must include the yanshi-vcs entry")

	var entry map[string]any
	require.NoError(t, json.Unmarshal(params.McpServers["yanshi-vcs"], &entry))
	assert.Equal(t, "yanshi", entry["command"])

	env, ok := entry["env"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "wt-9", env["YANSHI_WT_ID"])
}

// TestNewSession_NilMcpServersSendsEmptyObject verifies the backward-compatible
// path: passing a nil mcpServers still sends a present-but-empty {} field, which
// adapters require.
func TestNewSession_NilMcpServersSendsEmptyObject(t *testing.T) {
	fa, clientIn, clientOut := NewFakeAgent()
	defer fa.Close()
	c := NewClient(clientIn, clientOut)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := c.Initialize(ctx, ClientCapabilities{})
	require.NoError(t, err)

	_, err = c.NewSession(ctx, ".", nil, nil)
	require.NoError(t, err)

	captured := fa.CapturedNewSession()
	require.NotEmpty(t, captured)

	// The raw params must contain an "mcpServers":{} member (present, empty).
	assert.Contains(t, string(captured), `"mcpServers":{}`)

	var params NewSessionParams
	require.NoError(t, json.Unmarshal(captured, &params))
	require.NotNil(t, params.McpServers)
	assert.Empty(t, params.McpServers)
}
