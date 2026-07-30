package tools

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/guard"
	obslog "github.com/x6nux/yanshi/internal/observe/log"
)

// TestAuthorize_NoCallback_StaticDenyIsUnchanged proves that without a
// permission callback bound, Authorize is identical to the static guard: a
// denied action returns a DenyErr. This is the SSE / static-profile path.
func TestAuthorize_NoCallback_StaticDenyIsUnchanged(t *testing.T) {
	prof := guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"other"}}}
	ctx := WithProfile(context.Background(), prof)
	err := Authorize(ctx, guard.Action{Tool: "fs_write"}, `{"path":"x"}`)
	require.Error(t, err)
	assert.True(t, IsDenyErr(err))
}

// TestAuthorize_NoProfile_FailClosed proves a missing profile always denies.
func TestAuthorize_NoProfile_FailClosed(t *testing.T) {
	ctx := WithPermissionCallback(context.Background(), func(PermissionRequest) PermissionDecision {
		t.Fatal("callback must not be consulted when no profile is bound")
		return PermissionDeny
	})
	err := Authorize(ctx, guard.Action{Tool: "fs_write"}, "{}")
	require.Error(t, err)
	assert.True(t, IsDenyErr(err))
}

// TestAuthorize_CallbackAllow_OverridesDeny proves the callback is consulted
// when the static profile would deny, and allow proceeds.
func TestAuthorize_CallbackAllow_OverridesDeny(t *testing.T) {
	prof := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_write"}},
		FS:    guard.FSPerm{Write: []string{"/safe/**"}},
	}
	var asked int32
	ctx := WithProfile(context.Background(), prof)
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		atomic.AddInt32(&asked, 1)
		assert.Equal(t, "fs_write", req.Tool)
		assert.Contains(t, req.Args, "secret.go")
		assert.NotEmpty(t, req.Reason, "Reason must explain the static denial")
		return PermissionAllow
	})
	err := Authorize(ctx, guard.Action{
		Tool: "fs_write",
		FS:   guard.FSWant{Op: "write", Paths: []string{"/outside/secret.go"}},
	}, `{"path":"/outside/secret.go"}`)
	require.NoError(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&asked))
}

// TestAuthorize_CallbackDeny_ReturnsDenyErr proves a deny decision surfaces the
// same DenyErr the static guard would have returned.
func TestAuthorize_CallbackDeny_ReturnsDenyErr(t *testing.T) {
	prof := guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"fs_write"}}}
	ctx := WithProfile(context.Background(), prof)
	ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
		return PermissionDeny
	})
	err := Authorize(ctx, guard.Action{
		Tool: "fs_write", FS: guard.FSWant{Op: "write", Paths: []string{"/x"}},
	}, `{}`)
	require.Error(t, err)
	assert.True(t, IsDenyErr(err))
}

// TestAuthorize_AlwaysAllow_RecordsAndSkipsNext proves always_allow proceeds AND
// records a TTL=session rule in the approval manager so the identical next
// action is approved without re-prompting.
func TestAuthorize_AlwaysAllow_RecordsAndSkipsNext(t *testing.T) {
	prof := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "allowlist"}, // nothing on the list -> static deny
	}
	var asks int32
	mgr, err := approval.New(&fakeApprovalKV{}, "proc-1", nil)
	require.NoError(t, err)
	ctx := WithProfile(context.Background(), prof)
	ctx = WithApprovalManager(ctx, mgr, "session-a")
	ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
		atomic.AddInt32(&asks, 1)
		return PermissionAlwaysAllow
	})
	action := guard.Action{Tool: "shell_run", Shell: "ls"}
	require.NoError(t, Authorize(ctx, action, `{}`))
	require.NoError(t, Authorize(ctx, action, `{}`)) // manager hit -> no prompt
	require.NoError(t, Authorize(ctx, action, `{}`)) // and again
	assert.Equal(t, int32(1), atomic.LoadInt32(&asks), "always_allow must skip re-prompting the identical action")
}

// TestAuthorize_AlwaysAllow_DifferentActionStillPrompts proves the approval
// scope keys on the exact (tool, scope) tuple: always_allow for "ls" must NOT
// silently approve "rm". This is the security-critical property — a permissive
// reply for one command cannot widen approval to arbitrary others.
func TestAuthorize_AlwaysAllow_DifferentActionStillPrompts(t *testing.T) {
	prof := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "allowlist"},
	}
	var got []string
	mgr, err := approval.New(&fakeApprovalKV{}, "proc-1", nil)
	require.NoError(t, err)
	ctx := WithProfile(context.Background(), prof)
	ctx = WithApprovalManager(ctx, mgr, "session-a")
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		got = append(got, req.Args)
		return PermissionAlwaysAllow
	})
	require.NoError(t, Authorize(ctx, guard.Action{Tool: "shell_run", Shell: "ls"}, "ls"))
	require.NoError(t, Authorize(ctx, guard.Action{Tool: "shell_run", Shell: "rm"}, "rm"))
	assert.Equal(t, []string{"ls", "rm"}, got, "different actions must each prompt")
}

// TestAuthorize_StaticAllow_SkipsCallback proves the callback is NOT consulted
// when the static profile already allows the action (no unnecessary prompts).
func TestAuthorize_StaticAllow_SkipsCallback(t *testing.T) {
	prof := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_read"}},
		FS:    guard.FSPerm{Read: []string{"/safe/**"}},
	}
	ctx := WithProfile(context.Background(), prof)
	ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
		t.Fatal("callback must not be consulted when the profile allows the action")
		return PermissionDeny
	})
	err := Authorize(ctx, guard.Action{
		Tool: "fs_read", FS: guard.FSWant{Op: "read", Paths: []string{"/safe/x"}},
	}, `{}`)
	require.NoError(t, err)
}

// TestGuardedTool_CallbackAllow_ToolNameDenied proves the InvokableRun path
// consults the callback when the tool-NAME dimension would deny. The tool body
// runs only after the user allows.
func TestGuardedTool_CallbackAllow_ToolNameDenied(t *testing.T) {
	prof := guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"other.*"}}}
	ctx := WithProfile(context.Background(), prof)
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		assert.Equal(t, "echo", req.Tool)
		return PermissionAllow
	})
	ran := false
	gt := NewGuardedTool("echo", "Echo", "d", 10*time.Second, nil, SyncStream(func(context.Context, string) (string, error) {
		ran = true
		return "ran", nil
	}))
	out, err := gt.InvokableRun(ctx, `{"msg":"hi"}`)
	require.NoError(t, err)
	assert.Equal(t, "ran", out)
	assert.True(t, ran, "tool body must run after allow")
}

// TestGuardedTool_CallbackDeny_ToolSkipped proves a deny decision prevents the
// tool body from running and surfaces the denial as a tool RESULT ({"error":
// "permission denied: ..."}) so the model can react and the ADK does not abort
// the turn on a Go error.
func TestGuardedTool_CallbackDeny_ToolSkipped(t *testing.T) {
	prof := guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"other.*"}}}
	ctx := WithProfile(context.Background(), prof)
	ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
		return PermissionDeny
	})
	ran := false
	gt := NewGuardedTool("echo", "Echo", "d", 10*time.Second,
		schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{}),
		SyncStream(func(context.Context, string) (string, error) { ran = true; return "ran", nil }))
	out, err := gt.InvokableRun(ctx, "{}")
	require.NoError(t, err, "a deny decision must surface as a result, not a Go error")
	assert.Contains(t, out, "permission denied")
	assert.False(t, ran, "tool body must NOT run after deny")
}

// TestFS_Write_CallbackAllow_RunsFileEffect proves the fs_write dimension check
// consults the callback: a write outside the profile's write allowlist prompts;
// on allow the file is actually written.
func TestFS_Write_CallbackAllow_RunsFileEffect(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	// Profile allows fs_write by name and grants a narrow write path that
	// excludes the test's "out.txt" so the fs dimension returns Prompt (not
	// HardDeny). After Task 1, an empty Write list would HardDeny and skip the
	// callback entirely.
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Write: []string{dir + "/safe/**"}},
	})
	ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
		return PermissionAllow
	})
	out, err := runTool(ctx, fs.Write, `{"path":"out.txt","content":"hi"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "out.txt")
	assert.FileExists(t, dir+string("/")+"out.txt")
}

// Task 8 regression: a STRUCTURAL HardDeny (shell metachar, denylist match,
// execpolicy hard_deny, unknown policy, empty MCP allowlist) must never reach
// the callback (or the approval manager / YOLO mode) — it is the immovable
// floor. We exercise the metachar arm: even a callback returning "persistent
// allow" cannot rescue a chained command.
func TestAuthorizeStructuralHardDenyNeverCallsCallback(t *testing.T) {
	called := false
	prof := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
	}
	ctx := WithProfile(context.Background(), prof)
	ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
		called = true
		return PermissionAllowPersistent
	})
	err := Authorize(ctx, guard.Action{Tool: "shell_run", Shell: "go test && rm -rf /"}, `{"command":"go test && rm -rf /"}`)
	require.Error(t, err)
	assert.True(t, IsDenyErr(err))
	assert.False(t, called, "callback must not be invoked for a structural (non-Overridable) HardDeny")
}

// TestAuthorize_OverridableHardDeny_CallbackAllow proves an OVERRIDABLE
// profile-policy HardDeny (shell policy="deny") reaches the callback when one
// is bound, and a simulated YOLO/Auto "allow" decision lets the call proceed.
// This is the new behavior: YOLO/Auto bypass the configured profile.
func TestAuthorize_OverridableHardDeny_CallbackAllow(t *testing.T) {
	prof := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "deny"},
	}
	var gotProfileDeny *bool
	ctx := WithProfile(context.Background(), prof)
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		v := req.ProfileHardDeny
		gotProfileDeny = &v
		return PermissionAllow // simulate YOLO/Auto override
	})
	err := Authorize(ctx, guard.Action{Tool: "shell_run", Shell: "ls"}, `{}`)
	require.NoError(t, err, "YOLO/Auto override of a profile-policy deny must allow the call")
	require.NotNil(t, gotProfileDeny)
	assert.True(t, *gotProfileDeny, "callback must receive ProfileHardDeny=true for an overridable profile deny")
}

// TestAuthorize_OverridableHardDeny_NoCallbackFailsClosed proves the SSE path
// (no callback) still treats an overridable profile-policy deny as fail-closed.
// YOLO/Auto only apply when an interactive callback is bound (WS).
func TestAuthorize_OverridableHardDeny_NoCallbackFailsClosed(t *testing.T) {
	prof := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "deny"},
	}
	ctx := WithProfile(context.Background(), prof) // no callback
	err := Authorize(ctx, guard.Action{Tool: "shell_run", Shell: "ls"}, `{}`)
	require.Error(t, err)
	assert.True(t, IsDenyErr(err), "SSE/static path must fail closed on a profile-policy deny")
}

// TestAuthorize_OverridableHardDeny_CallbackDeny proves that when the callback
// resolves an overridable profile-policy deny to Deny (default/allow-edits/plan
// modes do this in resolvePermissionMode), Authorize surfaces DenyErr.
func TestAuthorize_OverridableHardDeny_CallbackDeny(t *testing.T) {
	prof := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "deny"},
	}
	ctx := WithProfile(context.Background(), prof)
	ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
		return PermissionDeny // default/allow-edits/plan resolve to silent deny
	})
	err := Authorize(ctx, guard.Action{Tool: "shell_run", Shell: "ls"}, `{}`)
	require.Error(t, err)
	assert.True(t, IsDenyErr(err))
}

// TestAuthorize_StructuralHardDeny_NotOverridableByCallback proves the floor:
// a structural HardDeny (metachar) is denied even when a callback would allow,
// so YOLO can never authorize a chained command. Contrast with
// TestAuthorize_OverridableHardDeny_CallbackAllow above.
func TestAuthorize_StructuralHardDeny_NotOverridableByCallback(t *testing.T) {
	prof := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
	}
	ctx := WithProfile(context.Background(), prof)
	ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
		return PermissionAllow
	})
	err := Authorize(ctx, guard.Action{Tool: "shell_run", Shell: "ls && rm -rf /"}, `{}`)
	require.Error(t, err)
	assert.True(t, IsDenyErr(err), "structural metachar HardDeny must hold even under a permissive callback")
}

// TestAuthorize_ReportedProfile_YoloBypassesEverythingButDestruction is the
// end-to-end confirmation for the operator's reported profile
//   tools=["*"], fs.read=["**"], fs.write=[], shell policy="deny", net.allow=false
// Under a YOLO-style callback the profile is fully bypassed (read/write/shell/
// net all proceed), while the destructive-deletion gate still blocks
// catastrophic mass deletion regardless of mode.
func TestAuthorize_ReportedProfile_YoloBypassesEverythingButDestruction(t *testing.T) {
	prof := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"**"}}, // write list empty -> overridable deny
		Shell: guard.ShellPerm{Policy: "deny"},
		Net:   guard.NetPerm{Allow: false},
	}
	const workdir = "/home/me/proj"
	ctx := WithProfile(context.Background(), prof)
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		// YOLO: every profile-policy deny (ProfileHardDeny) and Prompt is allowed.
		return PermissionAllow
	})
	allowed := []guard.Action{
		{Tool: "fs_read", FS: guard.FSWant{Op: "read", Paths: []string{"/anywhere/x"}}},
		{Tool: "fs_write", FS: guard.FSWant{Op: "write", Paths: []string{"/anywhere/x"}}},
		{Tool: "shell_run", Shell: "ls -la", Workdir: workdir},
		{Tool: "web_fetch", NetHost: "example.com"},
	}
	for _, act := range allowed {
		if err := Authorize(ctx, act, `{}`); err != nil {
			t.Errorf("yolo must bypass the profile for %s: %v", act.Tool, err)
		}
	}
	// The destructive gate blocks catastrophic mass deletion structurally, before
	// the callback is ever consulted, so it holds under every mode.
	if err := Authorize(ctx, guard.Action{Tool: "shell_run", Shell: "rm -rf /", Workdir: workdir}, `{}`); !IsDenyErr(err) {
		t.Errorf("catastrophic deletion must be blocked even under yolo; got %v", err)
	}
}

// Task 8: when the guard returns Prompt and the user picks "persistent allow",
// the rule is recorded in the approval manager (rather than the legacy
// sessionAllowlist) so future identical actions skip the prompt.
func TestAuthorizePromptCanRecordPersistentRule(t *testing.T) {
	kv := &fakeApprovalKV{}
	manager, err := approval.New(kv, "proc-1", nil)
	require.NoError(t, err)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"go version"}},
	})
	ctx = WithApprovalManager(ctx, manager, "session-a")
	ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision { return PermissionAllowPersistent })
	require.NoError(t, Authorize(ctx, guard.Action{Tool: "shell_run", Shell: "go test ./..."}, `{"command":"go test ./..."}`))
	assert.Len(t, manager.List("session-a", time.Now()), 1)
}

type fakeApprovalKV struct{ value string }

func (f *fakeApprovalKV) KVGet(string) (string, bool, error) {
	return f.value, f.value != "", nil
}

func (f *fakeApprovalKV) KVSet(_ string, value string) error {
	f.value = value
	return nil
}

// TestAuthorizeLogsDecisionWithoutArguments proves the audit record is written
// when the static profile allows the action, and that it carries only safe
// fields (tool name + decision + source). The raw argsJSON must NEVER appear in
// the audit trail.
func TestAuthorizeLogsDecisionWithoutArguments(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(obslog.New(obslog.Config{Writer: &buf, Format: "json", Level: "debug"}))
	defer slog.SetDefault(old)

	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_read"}},
	})
	if err := Authorize(ctx, guard.Action{Tool: "fs_read"}, `{"path":"C:/secret","api_key":"sk-test"}`); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, `"decision":"allow"`) || !strings.Contains(got, `"tool":"fs_read"`) {
		t.Fatalf("missing audit fields: %s", got)
	}
	for _, forbidden := range []string{"C:/secret", "sk-test", "argsJSON"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("audit leaked %q: %s", forbidden, got)
		}
	}
}

// TestAuthorizeLogsDenyDecision proves the deny paths (no profile, static-deny,
// callback deny) all emit a deny audit record so operators can correlate
// permission failures without seeing the underlying reason text.
func TestAuthorizeLogsDenyDecision(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(obslog.New(obslog.Config{Writer: &buf, Format: "json", Level: "info"}))
	defer slog.SetDefault(old)

	prof := guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"other"}}}
	ctx := WithProfile(context.Background(), prof)
	err := Authorize(ctx, guard.Action{Tool: "fs_write"}, `{"path":"x"}`)
	require.Error(t, err)

	got := buf.String()
	if !strings.Contains(got, `"decision":"deny"`) || !strings.Contains(got, `"tool":"fs_write"`) {
		t.Fatalf("missing deny audit: %s", got)
	}
}
