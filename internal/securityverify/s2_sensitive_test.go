package securityverify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/acp"
	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/tools"
)

// codingProfile mirrors config.example.yaml's `coding` profile: the widest
// FS grant an operator gets by copying the shipped example.
func codingProfile() guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"git *"}},
		Net:   guard.NetPerm{Allow: true, Hosts: []string{"*"}},
	}
}

// fakeHome makes HOME point at a temp dir holding a real private key, so the
// denylist's home-relative suffixes resolve against a tree this test owns.
func fakeHome(t *testing.T) (home, key string) {
	t.Helper()
	home = t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	key = filepath.Join(home, ".ssh", "id_rsa")
	body := "-----BEGIN OPENSSH PRIVATE KEY-----\nSECRETKEYMATERIAL\n-----END OPENSSH PRIVATE KEY-----\n"
	if err := os.WriteFile(key, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home, key
}

// runReadTurn drives a full ReAct turn whose only scripted action is
// fs_read(path), and returns the tool output the model would have received.
// No permission callback is bound, so any escalation fails closed — which is
// exactly the headless/SSE posture.
func runReadTurn(t *testing.T, root, path string, prof guard.PermissionProfile) string {
	t.Helper()
	// jsonObj, not string concatenation: on Windows the temp-dir path is full of
	// backslashes, and `{"path":"C:\Users\…"}` is invalid JSON (`\U` is not an
	// escape), so the tool's args parser refused the call before the guard ever
	// saw a path — all four S2 assertions then failed on a parse error instead
	// of a verdict. Measured on the CI windows leg; s3/s4/s10 already go through
	// this helper for exactly that reason.
	args, err := jsonObj(map[string]string{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name: "fs_read", Arguments: args,
		}},
	})
	step2 := schema.AssistantMessage("done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2}, nil)
	fs := tools.NewFSTools(root)
	o, err := orchestrator.New(orchestrator.Config{
		Model: mdl, Tools: []orchestrator.BaseTool{fs.Read}, Profile: prof,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out string
	iter := o.Events(context.Background(), "read it")
	for {
		ev, ok := iter.Next()
		if !ok {
			break
		}
		if ev.Output == nil || ev.Output.MessageOutput == nil {
			continue
		}
		mv := ev.Output.MessageOutput
		var msg *schema.Message
		if mv.IsStreaming && mv.MessageStream != nil {
			msg, _ = mv.GetMessage()
		} else {
			msg = mv.Message
		}
		if msg != nil && msg.Role == schema.Tool {
			out += msg.Content
		}
	}
	return out
}

// TestS2_AgentRunInHomeDirCannotReadSSHKey is the reachable fs-tool shape of
// the S2 incident: the work root IS the home directory (an operator who ran
// yanshi from ~), so the project jail does not stand between the model and
// ~/.ssh/id_rsa and the guard's FS dimension is the only thing left.
func TestS2_AgentRunInHomeDirCannotReadSSHKey(t *testing.T) {
	home, key := fakeHome(t)
	out := runReadTurn(t, home, key, codingProfile())
	t.Logf("tool output: %q", out)
	if strings.Contains(out, "SECRETKEYMATERIAL") {
		t.Fatalf("LEAK: key material reached the model: %s", out)
	}
	if !strings.Contains(strings.ToLower(out), "denied") {
		t.Fatalf("expected a denial, got %q", out)
	}
}

// TestS2_TraversalSpellingIsAlsoDenied proves the denylist matches the
// normalised path, not the spelling: ~/.ssh/../.ssh/id_rsa is the same file.
func TestS2_TraversalSpellingIsAlsoDenied(t *testing.T) {
	home, _ := fakeHome(t)
	sneaky := filepath.Join(home, ".ssh", "..", ".ssh", "id_rsa")
	out := runReadTurn(t, home, sneaky, codingProfile())
	t.Logf("tool output: %q", out)
	if strings.Contains(out, "SECRETKEYMATERIAL") {
		t.Fatalf("LEAK via traversal spelling: %s", out)
	}
	if !strings.Contains(strings.ToLower(out), "denied") {
		t.Fatalf("expected a denial, got %q", out)
	}
}

// TestS2_LiteralGrantIsTheEscapeHatch proves the gate is grantable: a profile
// that spells the credential path out literally reads the file. Without this
// the denylist would be a capability no operator can turn on, and the pressure
// would go into widening the profile globally instead.
func TestS2_LiteralGrantIsTheEscapeHatch(t *testing.T) {
	home, key := fakeHome(t)
	prof := codingProfile()
	prof.FS.Read = []string{"**", key}
	out := runReadTurn(t, home, key, prof)
	t.Logf("tool output: %q", out)
	if !strings.Contains(out, "SECRETKEYMATERIAL") {
		t.Fatalf("literal grant must allow the read, got %q", out)
	}
}

// TestS2_OrdinaryProjectFileIsUntouched is the false-positive control. A gate
// that also refuses ordinary files is a gate operators switch off.
func TestS2_OrdinaryProjectFileIsUntouched(t *testing.T) {
	home, _ := fakeHome(t)
	src := filepath.Join(home, "main.go")
	if err := os.WriteFile(src, []byte("package main // ORDINARY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := runReadTurn(t, home, src, codingProfile())
	t.Logf("tool output: %q", out)
	if !strings.Contains(out, "ORDINARY") {
		t.Fatalf("ordinary file must be readable, got %q", out)
	}
}

// TestS2_ACPExternalAgentCannotReadSSHKey covers the OTHER reachable FS
// surface, and the one with no jail at all: acp.GuardPolicy.OnFSRead takes the
// path straight from an external agent CLI over the wire. Nothing upstream of
// it constrains the path to the project.
func TestS2_ACPExternalAgentCannotReadSSHKey(t *testing.T) {
	_, key := fakeHome(t)
	gp := acp.NewGuardPolicy(codingProfile())
	err := gp.OnFSRead(key)
	t.Logf("OnFSRead(%s) -> %v", key, err)
	if err == nil {
		t.Fatal("external ACP agent read a private key under the shipped coding profile")
	}
	// The control: an ordinary path is still readable through the same door.
	if err := gp.OnFSRead(filepath.Join(t.TempDir(), "main.go")); err != nil {
		t.Fatalf("ordinary path must pass: %v", err)
	}
}
