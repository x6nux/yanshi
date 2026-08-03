//go:build e2e_real

// Real-CLI E2E test for the ACP integration. Not part of the default suite —
// run with: YANSHI_E2E=1 go test -tags=e2e_real -run TestE2E_Real -timeout 300s ./internal/acp/
// It spawns real coding agents (claudecode/codex) via ACP and checks they can
// write a file under a permissive profile. Requires the agent CLIs + auth.
package acp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
)

func workProfile(t *testing.T, work string) guard.PermissionProfile {
	t.Helper()
	wt := strings.ReplaceAll(filepath.Join(work, "**"), string(filepath.Separator), "/")
	return guard.PermissionProfile{
		FS:    guard.FSPerm{Read: []string{wt}, Write: []string{wt}},
		Tools: guard.ToolsPerm{Allow: []string{"fs.*", "shell.*"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"go *", "git *", "npm *", "python *", "echo *", "cat *", "ls *"}},
		Net:   guard.NetPerm{Allow: false},
	}
}

func runRealAgent(t *testing.T, agent, marker string) {
	if os.Getenv("YANSHI_E2E") == "" {
		t.Skip("set YANSHI_E2E=1 to run real-CLI E2E")
	}
	if runtime.GOOS == "windows" && agent == "codex" {
		t.Logf("note: codex-acp may be slow to fetch via npx on first run")
	}

	work, err := os.MkdirTemp("", "acp-e2e-"+agent+"-*")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(work) })
	t.Logf("workdir: %s", work)

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	spawned, err := Spawn(ctx, SpawnOptions{
		Agent:  agent,
		Cwd:    work,
		Policy: NewGuardPolicy(workProfile(t, work)),
	})
	if err != nil {
		t.Fatalf("spawn %q failed: %v", agent, err)
	}
	t.Logf("spawned %q ok (session=%s)", agent, spawned.SessionID)
	defer func() {
		spawned.Client.Close()
		spawned.Cmd.Process.Kill()
		spawned.Cmd.Wait()
	}()

	prompt := "Create a file named hello.txt in the current working directory " +
		"containing exactly this text on one line (no quotes): " + marker +
		". Use your file-write tool. Then stop."
	var events int
	stop, err := spawned.Client.Prompt(ctx, spawned.SessionID, prompt, func(ev Event) {
		events++
		switch ev.Kind {
		case "agent_message_chunk":
			if ev.Text != "" {
				t.Logf("[msg] %s", strings.TrimSpace(ev.Text))
			}
		case "tool_call", "tool_call_update":
			t.Logf("[tool] %s status=%s title=%q", ev.Kind_, ev.Status, ev.Title)
		default:
			t.Logf("[ev] %s title=%q status=%s", ev.Kind, ev.Title, ev.Status)
		}
	})
	require.NoError(t, err, "prompt failed")
	t.Logf("stopReason=%s events=%d", stop, events)

	got, err := os.ReadFile(filepath.Join(work, "hello.txt"))
	if err != nil {
		t.Errorf("hello.txt was NOT created: %v", err)
		return
	}
	gotS := strings.TrimSpace(string(got))
	if !strings.Contains(gotS, marker) {
		t.Errorf("hello.txt content mismatch: got %q want marker %q", gotS, marker)
	} else {
		t.Logf("SUCCESS: hello.txt = %q", gotS)
	}
}

func TestE2E_RealClaudeCode(t *testing.T) {
	runRealAgent(t, "claudecode", "hello from claudecode via acp")
}
func TestE2E_RealCodex(t *testing.T) { runRealAgent(t, "codex", "hello from codex via acp") }
