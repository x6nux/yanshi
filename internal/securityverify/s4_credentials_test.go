package securityverify

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/shell"
	"github.com/x6nux/yanshi/internal/tools"
)

// s4_credentials_test.go verifies S4 the only way that settles it: launch a
// real child process, have it print its OWN environment, and read the result.
//
// The distinction matters because every layer in the chain has its own copy of
// the environment. Asserting that ScrubEnv returns the right slice says nothing
// about whether the slice reaches exec, and the failure mode this feature
// exists to prevent — `printenv` inside shell_run putting a provider key into
// the transcript, which then travels to the provider on the next request — is
// observable only at the child.
//
// The two directions are equally load-bearing. Over-stripping is not a safe
// failure: a child with no PATH cannot resolve `go`, and a child with no HOME
// cannot read a config. That is not a hardened subprocess, it is a broken one,
// and the operator's bug report says "yanshi cannot run anything".

// credentialVars are the parent-process variables that must NOT reach a child.
// Values are distinctive so a leak is unambiguous in the child's output.
var credentialVars = map[string]string{
	"ANTHROPIC_API_KEY":     "sk-ant-LEAKCANARY0001",
	"OPENAI_API_KEY":        "sk-LEAKCANARY0002",
	"AWS_SECRET_ACCESS_KEY": "LEAKCANARY0003awssecret",
	"AWS_SESSION_TOKEN":     "LEAKCANARY0004session",
	"GH_TOKEN":              "ghp_LEAKCANARY0005",
	"GITHUB_TOKEN":          "ghp_LEAKCANARY0006",
	"DATABASE_PASSWORD":     "LEAKCANARY0007pw",
	"NPM_TOKEN":             "npm_LEAKCANARY0008",
	"SLACK_WEBHOOK_SECRET":  "LEAKCANARY0009",
}

// structuralVars must survive: without them the child cannot function.
var structuralVars = []string{"PATH", "HOME", "LANG"}

// seedParentEnv puts the canary credentials into THIS process's environment,
// which is the baseline every child inherits from.
func seedParentEnv(t *testing.T) {
	t.Helper()
	for k, v := range credentialVars {
		t.Setenv(k, v)
	}
	t.Setenv("LANG", "en_US.UTF-8")
}

// parseEnvOutput turns `env` output into a map.
func parseEnvOutput(s string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if ok {
			out[k] = v
		}
	}
	return out
}

// launchEnvChild runs /usr/bin/env through the production shell launch factory
// — the same object bootstrap builds — and returns what the child saw.
func launchEnvChild(t *testing.T, allowEnv []string) map[string]string {
	t.Helper()
	f := shell.NewSecureLaunchFactory(shell.SecureLaunchFactory{})
	dir := t.TempDir()
	proc, console, err := f.Start(context.Background(), shell.LaunchSpec{
		Program: "/usr/bin/env", Dir: dir, AllowEnv: allowEnv,
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	out, _ := io.ReadAll(console)
	_ = proc.Wait()
	_ = console.Close()
	return parseEnvOutput(string(out))
}

// TestS4_ChildProcessNeverSeesCredentials is the leak direction: a real child
// prints its environment and no canary value is in it.
func TestS4_ChildProcessNeverSeesCredentials(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/usr/bin/env is POSIX")
	}
	seedParentEnv(t)
	// Sanity: the parent really does hold them, so an empty child env below
	// means "stripped", not "never set".
	for k, want := range credentialVars {
		if os.Getenv(k) != want {
			t.Fatalf("test setup: parent %s not seeded", k)
		}
	}
	got := launchEnvChild(t, nil)
	t.Logf("child saw %d variables", len(got))
	for k := range credentialVars {
		if v, present := got[k]; present {
			t.Errorf("LEAK: child inherited %s=%q", k, v)
		}
	}
	// Value-level check too: a variable renamed on the way through would pass
	// the name check above and still carry the secret.
	for _, v := range got {
		if strings.Contains(v, "LEAKCANARY") {
			t.Errorf("LEAK: a canary value reached the child under some name: %q", v)
		}
	}
}

// TestS4_ChildProcessKeepsStructuralVariables is the over-stripping direction.
// A child without PATH cannot resolve a program; without HOME it cannot read a
// config. Both make the subprocess useless rather than safe.
func TestS4_ChildProcessKeepsStructuralVariables(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/usr/bin/env is POSIX")
	}
	seedParentEnv(t)
	got := launchEnvChild(t, nil)
	for _, k := range structuralVars {
		if got[k] == "" {
			t.Errorf("child lost structural variable %s — the subprocess is broken, not hardened", k)
		} else {
			t.Logf("child kept %s=%s", k, got[k])
		}
	}
}

// TestS4_AllowlistIsAGrantThatWorks proves the escape hatch is real. `gh`
// genuinely needs GH_TOKEN, and a scrub with no way to grant an exception
// would be worked around by turning the scrub off.
func TestS4_AllowlistIsAGrantThatWorks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/usr/bin/env is POSIX")
	}
	seedParentEnv(t)
	got := launchEnvChild(t, []string{"GH_TOKEN"})
	if got["GH_TOKEN"] != credentialVars["GH_TOKEN"] {
		t.Fatalf("allowlisted GH_TOKEN did not reach the child: %q", got["GH_TOKEN"])
	}
	t.Logf("allowlisted GH_TOKEN reached the child")
	// And the allowlist is a grant of ONE name, not an amnesty.
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "AWS_SECRET_ACCESS_KEY"} {
		if v, present := got[k]; present {
			t.Errorf("allowlisting GH_TOKEN also leaked %s=%q", k, v)
		}
	}
	if got["GITHUB_TOKEN"] != "" {
		t.Errorf("allowlist must be exact-name: GITHUB_TOKEN leaked alongside GH_TOKEN")
	}
}

// TestS4_ShellRunCannotPrintCredentials is the end-to-end shape of the actual
// incident: a model calls shell_run("env") and the output becomes a tool result
// it can read — and which is then re-sent to the provider with every subsequent
// request in the conversation.
func TestS4_ShellRunCannotPrintCredentials(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/usr/bin/env is POSIX")
	}
	seedParentEnv(t)
	work := t.TempDir()

	args, err := jsonObj(map[string]string{"command": "/usr/bin/env"})
	if err != nil {
		t.Fatal(err)
	}
	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "shell_run", Arguments: args}},
	})
	step2 := schema.AssistantMessage("done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2}, nil)
	sh := tools.NewShellTools(work)
	o, err := orchestrator.New(orchestrator.Config{
		Model: mdl,
		Tools: []orchestrator.BaseTool{sh.Run},
		Profile: guard.PermissionProfile{
			Tools: guard.ToolsPerm{Allow: []string{"*"}},
			Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
		},
		SecureFactory: shell.DefaultSecureFactory{OS: shell.OSProcessFactory{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var toolOut string
	iter := o.Events(context.Background(), "print the environment")
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
			toolOut += msg.Content
		}
	}
	if !strings.Contains(toolOut, "PATH=") {
		t.Fatalf("harness check: the child did not actually run `env`; got %q", toolOut)
	}
	if strings.Contains(toolOut, "LEAKCANARY") {
		for _, line := range strings.Split(toolOut, "\n") {
			if strings.Contains(line, "LEAKCANARY") {
				t.Errorf("LEAK into the model transcript: %s", line)
			}
		}
	} else {
		t.Logf("shell_run(env) returned %d bytes and no canary", len(toolOut))
	}
}

// TestS4_ScrubIsNameBasedNotValueGuessing pins a property the header claims:
// the allowlist names variables, and an ordinary variable whose VALUE happens
// to look like a token is judged on that value rather than waved through.
// Recording which direction this actually goes is the point — a scrub that
// only matched names would let a leak ride in PROJECT_NOTE.
func TestS4_ScrubIsNameBasedNotValueGuessing(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"PROJECT_NOTE=sk-ant-api03-" + strings.Repeat("A", 60),
		"BUILD_NUMBER=1234",
	}
	kept, dropped := netpolicy.ScrubCredentials(env, netpolicy.CredentialPolicy{})
	t.Logf("kept=%v dropped=%v", kept, dropped)
	joined := strings.Join(kept, " ")
	if !strings.Contains(joined, "PATH=/usr/bin") {
		t.Error("PATH must survive")
	}
	if !strings.Contains(joined, "BUILD_NUMBER=1234") {
		t.Error("an ordinary variable must survive")
	}
	if strings.Contains(joined, "sk-ant-api03-") {
		t.Errorf("a credential-shaped VALUE survived under an innocent name: %v", kept)
	}
}

// TestS4_ProcessSelfEnvironIsAlsoDenied closes the obvious detour: if the child
// cannot be given the credentials, it can try to read the PARENT's. On Linux
// /proc/<pid>/environ is exactly that file, and it is in the sensitive-path
// denylist for this reason.
func TestS4_ProcessSelfEnvironIsAlsoDenied(t *testing.T) {
	entry, hit := guard.IsSensitivePath("/proc/self/environ", "")
	t.Logf("IsSensitivePath(/proc/self/environ) -> %q %v", entry, hit)
	if !hit {
		t.Fatal("/proc/self/environ must be on the sensitive denylist: it IS the credential set")
	}
	// The workdir-relative spelling must not be a way around it.
	if _, hit := guard.IsSensitivePath("/proc/self/../self/environ", ""); !hit {
		t.Error("a traversal spelling of /proc/self/environ escaped the denylist")
	}
	_ = filepath.Separator
}
