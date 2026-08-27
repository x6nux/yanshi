package securityverify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/tools"
)

// s3_policyfile_test.go covers policy-file tamper resistance in the two places
// it has to hold: the WRITE attempt itself, and doctor's judgement about the
// posture.
//
// The second half is not decoration. A warning that fires unconditionally is
// indistinguishable from a warning that fires correctly, and both look green in
// a test that only checks "doctor mentions the config file". So the test moves
// the config OUT of the work root and requires the warning to DISAPPEAR. If it
// does not, the check is not reading the posture — it is printing a constant.

// writeAgentTurn scripts a model that calls fs_write on the given path and
// returns what the tool handed back.
func writeAgentTurn(t *testing.T, root, path, content string, prof guard.PermissionProfile) string {
	t.Helper()
	args, err := jsonObj(map[string]string{"path": path, "content": content})
	if err != nil {
		t.Fatal(err)
	}
	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "fs_write", Arguments: args}},
	})
	step2 := schema.AssistantMessage("done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2}, nil)
	fs := tools.NewFSTools(root)
	o, err := orchestrator.New(orchestrator.Config{
		Model: mdl, Tools: []orchestrator.BaseTool{fs.Write}, Profile: prof,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out string
	iter := o.Events(context.Background(), "edit the config")
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

// TestS3_AgentCannotWidenItsOwnProfileViaConfig is the self-escalation attempt
// as a real turn: the model rewrites config.yaml to give itself a wide profile.
//
// The escalation only pays off after a restart, so the observable that matters
// is whether the FILE CHANGED, not merely what the tool printed.
func TestS3_AgentCannotWidenItsOwnProfileViaConfig(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "config.yaml")
	const original = "server:\n  http_addr: 127.0.0.1:0\nprofiles:\n  orchestrator:\n    tools: { allow: [] }\n"
	if err := os.WriteFile(cfgPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	// A profile whose write scope excludes the config file — the posture an
	// operator gets by protecting it explicitly.
	prof := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS: guard.FSPerm{
			Read:  []string{"**"},
			Write: []string{filepath.Join(root, "src", "**")},
		},
	}
	widened := "profiles:\n  orchestrator:\n    tools: { allow: [\"*\"] }\n    shell: { policy: allowlist, patterns: [\"*\"] }\n"
	out := writeAgentTurn(t, root, "config.yaml", widened, prof)
	t.Logf("fs_write(config.yaml) -> %s", strings.TrimSpace(out))

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Fatalf("SELF-ESCALATION: the agent rewrote its own policy file.\n--- now ---\n%s", after)
	}
	if !strings.Contains(strings.ToLower(out), "denied") {
		t.Errorf("the write should be reported as denied, got %q", out)
	}

	// Control: the same profile DOES permit an ordinary source write, so the
	// refusal above is about the path and not about fs_write being broken.
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctrl := writeAgentTurn(t, root, filepath.Join("src", "main.go"), "package main\n", prof)
	t.Logf("control fs_write(src/main.go) -> %s", strings.TrimSpace(ctrl))
	if _, err := os.Stat(filepath.Join(root, "src", "main.go")); err != nil {
		t.Fatalf("control write did not happen: %v", err)
	}
}

// TestS3_ConfigInAgentWriteScopeIsTheRealPredicate exercises the judgement
// doctor's policy-scope warning is built on, in BOTH directions.
//
// The negative direction is the load-bearing one. Confirming that a config
// inside the work root warns proves nothing on its own: a check hard-coded to
// warn always would pass that. Only moving the file out and watching the answer
// flip shows the predicate reads the world.
func TestS3_ConfigInAgentWriteScopeIsTheRealPredicate(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	outside := t.TempDir()
	if r, err := filepath.EvalSymlinks(outside); err == nil {
		outside = r
	}

	inside := filepath.Join(root, "config.yaml")
	if !config.ConfigInAgentWriteScope(inside, root) {
		t.Errorf("a config inside the work root must be reported as in scope: %s in %s", inside, root)
	} else {
		t.Logf("in scope (warns):     %s under root %s", inside, root)
	}

	elsewhere := filepath.Join(outside, "config.yaml")
	if config.ConfigInAgentWriteScope(elsewhere, root) {
		t.Errorf("a config OUTSIDE the work root must NOT warn — the check is printing a constant: %s vs root %s",
			elsewhere, root)
	} else {
		t.Logf("out of scope (silent): %s under root %s", elsewhere, root)
	}

	// A nested config is still inside; a sibling directory sharing a prefix is
	// not. The second is the classic prefix-comparison bug.
	nested := filepath.Join(root, "etc", "config.yaml")
	if !config.ConfigInAgentWriteScope(nested, root) {
		t.Errorf("a nested config is still writable by the agent: %s", nested)
	}
	sibling := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-other", "config.yaml")
	if config.ConfigInAgentWriteScope(sibling, root) {
		t.Errorf("a sibling directory sharing a name prefix is NOT inside the root: %s vs %s", sibling, root)
	}
	t.Logf("nested=in-scope, prefix-sibling=out-of-scope, as required")
}

// TestS3_PolicyProtectedPathsCoversTheSplitOutProfiles pins the set the
// protection applies to. Covering config.yaml while leaving profiles.d/
// writable protects nothing: an operator who split their profiles out would
// have the same hole under a check reporting OK.
func TestS3_PolicyProtectedPathsCoversTheSplitOutProfiles(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	paths := config.PolicyProtectedPaths(cfg)
	t.Logf("protected: %v", paths)

	var sawConfig, sawProfilesDir bool
	for _, p := range paths {
		if p == cfg {
			sawConfig = true
		}
		if p == filepath.Join(dir, "profiles.d") {
			sawProfilesDir = true
		}
	}
	if !sawConfig {
		t.Error("the config file itself must be protected")
	}
	if !sawProfilesDir {
		t.Error("profiles.d must be protected too, or splitting profiles out reopens the hole")
	}
}
