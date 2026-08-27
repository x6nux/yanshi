package http

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/tools"
)

// ws_perm_storage_test.go proves the storage-destruction grade added in
// internal/guard/storage.go survives the ONE mode that exists to skip prompts.
//
// The grade itself is asserted in internal/securityverify, but a tier is only
// as good as the mode gate's treatment of it, and those are different files
// with different failure modes: resolvePermissionMode hard-stops
// DestructionCatastrophic and merely PROMPTS everything else, so a storage
// destroyer graded one notch lower would be silently auto-approved here while
// every guard-level test stayed green. That is the gap this file covers.
//
// The commands are run against a canary binary placed ahead of the real one on
// PATH, so a regression is observed as "a process was launched", not as "a
// function returned the wrong enum".

// newDestructiveWSServer builds a WS server whose scripted model calls
// shell_run(cmd) exactly once, under a profile wide enough that the SHELL
// dimension cannot be the thing doing the refusing.
func newDestructiveWSServer(t *testing.T, cmd, workdir string) string {
	t.Helper()
	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name:      "shell_run",
			Arguments: `{"command":` + quoteJSON(cmd) + `}`,
		}},
	})
	step2 := schema.AssistantMessage("ran", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2}, nil)

	sh := tools.NewShellTools(workdir)
	o, err := orchestrator.New(orchestrator.Config{
		Model: mdl,
		Tools: []orchestrator.BaseTool{sh.Run},
		Profile: guard.PermissionProfile{
			Tools: guard.ToolsPerm{Allow: []string{"*"}},
			FS:    guard.FSPerm{Read: []string{"**"}, Write: []string{"**"}},
			Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
		},
	})
	require.NoError(t, err)

	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/chat/ws"
}

// quoteJSON renders s as a JSON string literal.
func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// installCanary puts fake `dd`, `mkfs.ext4` and `rm` binaries ahead of the real
// ones on PATH. They destroy nothing and append their argv to a witness file.
func installCanary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	witness := filepath.Join(dir, "witness.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + witness + "\nexit 0\n"
	for _, name := range []string{"dd", "mkfs.ext4", "rm", "wipefs"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755))
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return witness
}

// TestChatWS_YOLOStillRefusesStorageDestruction is the mode-gate half of the
// storage fix. yolo auto-approves every profile policy without a prompt — that
// is the whole point of the mode — so the only thing standing between a model
// turn and a zeroed disk is the catastrophic grade.
func TestChatWS_YOLOStillRefusesStorageDestruction(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("canary uses a /bin/sh shebang")
	}
	for _, cmd := range []string{
		`dd if=/dev/zero of=/dev/disk0`,
		`mkfs.ext4 /dev/sda1`,
		`wipefs -a /dev/sda`,
		`bash -c "dd if=/dev/zero of=/dev/disk0"`,
	} {
		t.Run(cmd, func(t *testing.T) {
			witness := installCanary(t)
			workdir := t.TempDir()
			c := dial(t, newDestructiveWSServer(t, cmd, workdir))
			defer c.Close()

			require.NoError(t, c.WriteJSON(proto.NewSetMode("yolo")))
			drainUntil(t, c, "status")
			require.NoError(t, c.WriteJSON(proto.NewUserMessage("do it")))

			var sawPrompt bool
			var toolText string
			for {
				f := readFrame(t, c)
				switch f.Type {
				case "permission_request":
					sawPrompt = true
				case "tool_result":
					toolText += f.Text
				}
				if f.Type == "done" || f.Type == "error" {
					goto drained
				}
			}
		drained:
			t.Logf("cmd=%q sawPrompt=%v toolText=%q", cmd, sawPrompt, toolText)

			b, err := os.ReadFile(witness)
			if err == nil && len(b) > 0 {
				t.Fatalf("CANARY FIRED under yolo — %q reached a process with argv: %s", cmd, b)
			}
			assert.Contains(t, strings.ToLower(toolText), "denied",
				"yolo must still refuse storage destruction")
		})
	}
}

// TestChatWS_YOLOStillRunsOrdinaryCommands is the control that gives the empty
// witness above its meaning: under the identical setup, a legitimate command
// DOES reach a process. Without it, "the canary never fired" would be equally
// consistent with "shell_run is broken in this harness".
func TestChatWS_YOLOStillRunsOrdinaryCommands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("canary uses a /bin/sh shebang")
	}
	witness := installCanary(t)
	workdir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workdir, "build"), 0o755))
	c := dial(t, newDestructiveWSServer(t, "rm -rf ./build", workdir))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewSetMode("yolo")))
	drainUntil(t, c, "status")
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("clean")))
	for {
		f := readFrame(t, c)
		if f.Type == "done" || f.Type == "error" {
			break
		}
	}
	b, err := os.ReadFile(witness)
	require.NoError(t, err, "the legitimate command must have reached the canary")
	assert.Contains(t, string(b), "build")
	t.Logf("canary witness: %q", strings.TrimSpace(string(b)))
}
