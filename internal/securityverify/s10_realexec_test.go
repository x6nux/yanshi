package securityverify

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/tools"
)

// canaryBin installs a fake `rm` earlier on PATH than the real one. It deletes
// nothing; it appends its argv to a witness file. If the guard ever lets a
// catastrophic deletion through, the witness records it and the test fails —
// without the test having to actually run the real rm to find out.
//
// This is the difference between asserting "Check returned HardDeny" and
// asserting the process was never launched. The first is a claim about a
// function; the second is a claim about the machine.
func canaryBin(t *testing.T) (dir, witness string) {
	t.Helper()
	dir = t.TempDir()
	witness = filepath.Join(dir, "witness.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + witness + "\nexit 0\n"
	for _, name := range []string{"rm", "dd", "shred"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir, witness
}

// runShellTurn drives a real ReAct turn whose scripted action is shell_run(cmd)
// and returns what the model saw back.
func runShellTurn(t *testing.T, root, cmd string, prof guard.PermissionProfile) string {
	t.Helper()
	args, err := jsonObj(map[string]string{"command": cmd})
	if err != nil {
		t.Fatal(err)
	}
	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "shell_run", Arguments: args}},
	})
	step2 := schema.AssistantMessage("done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2}, nil)
	sh := tools.NewShellTools(root)
	o, err := orchestrator.New(orchestrator.Config{
		Model: mdl, Tools: []orchestrator.BaseTool{sh.Run}, Profile: prof,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out string
	iter := o.Events(context.Background(), "run it")
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

// TestS10_CatastrophicCommandsNeverReachTheProcess is the execution-level
// counterpart of the tier matrix: it proves the dangerous argv is never handed
// to a process, using a canary binary as the witness.
func TestS10_CatastrophicCommandsNeverReachTheProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("canary uses a /bin/sh shebang")
	}
	_, witness := canaryBin(t)
	home := t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	work := filepath.Join(home, "proj")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	prof := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
	}
	attacks := []string{
		`rm -rf /`,
		`bash -c $'\x72\x6d -rf /'`,
		`bash -c "rm -rf /"`,
		`rm -rf ~`,
		`rm -rf $HOME`,
		`rm -rf ~/proj/../`,
		`rm -rf *`,
		`rm -rf /etc`,
		`dd if=/dev/zero of=/dev/disk0`,
	}
	for _, cmd := range attacks {
		out := runShellTurn(t, work, cmd, prof)
		t.Logf("cmd=%q -> %s", cmd, strings.TrimSpace(out))
		if !strings.Contains(strings.ToLower(out), "denied") {
			t.Errorf("attack %q was not denied: %s", cmd, out)
		}
	}
	if b, err := os.ReadFile(witness); err == nil && len(b) > 0 {
		t.Fatalf("CANARY FIRED — a destructive process was launched with:\n%s", b)
	} else {
		t.Logf("canary witness empty: no destructive process was ever launched")
	}
}

// TestS10_LegitimateCommandsStillExecute is the control. The canary proves the
// opposite direction too: an allowed `rm -rf ./build` DOES reach the process,
// so the previous test's empty witness means "blocked", not "shell_run is
// broken in this harness".
func TestS10_LegitimateCommandsStillExecute(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("canary uses a /bin/sh shebang")
	}
	_, witness := canaryBin(t)
	home := t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	t.Setenv("HOME", home)
	work := filepath.Join(home, "proj")
	if err := os.MkdirAll(filepath.Join(work, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	prof := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
	}
	out := runShellTurn(t, work, "rm -rf ./build", prof)
	t.Logf("legitimate rm -rf ./build -> %s", strings.TrimSpace(out))
	b, err := os.ReadFile(witness)
	if err != nil || !strings.Contains(string(b), "build") {
		t.Fatalf("expected the canary rm to be invoked for a legitimate deletion; witness=%q err=%v", b, err)
	}
	t.Logf("canary witness: %q", strings.TrimSpace(string(b)))
}
