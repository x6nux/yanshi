package securityverify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/bootstrap"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
)

// s6_audit_test.go verifies the audit trail against a REAL bootstrap App, not
// against store.AppendPermissionAudit directly.
//
// That choice is the whole point. The S6 finding was never "there is no record
// type" — auditPermission had built a full structured record since it was
// written — it was that the record had exactly one consumer, slog, so the
// answer to "which rm did it approve last night" died with the terminal
// buffer. A test that calls AppendPermissionAudit itself re-creates the sink
// it is supposed to be checking for, and would have passed on the day the bug
// existed. Only bootstrap.Build installs the production sink, so only a real
// App can show that a guard decision reaches SQLite.

// buildAuditApp boots a real App on a temp SQLite file and returns it with the
// live Store. The config is the smallest one Build accepts.
func buildAuditApp(t *testing.T) *bootstrap.App {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "server:\n  http_addr: 127.0.0.1:0\nstorage:\n  sqlite_path: \"" +
		filepath.ToSlash(filepath.Join(dir, "yanshi.db")) + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	app, err := bootstrap.Build(bootstrap.Options{
		ConfigPath: cfgPath, FakeModel: true, WorkRoot: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
	if app.Store == nil {
		t.Fatal("App has no Store; the audit sink cannot be wired")
	}
	return app
}

// TestS6_GuardDecisionsReachSQLite drives Authorize under a real App and reads
// the rows back out of the database.
func TestS6_GuardDecisionsReachSQLite(t *testing.T) {
	app := buildAuditApp(t)
	if !tools.PermissionAuditSinkInstalled() {
		t.Fatal("bootstrap.Build did not install the permission audit sink")
	}

	ctx := tools.WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"git status"}},
	})
	// One allow and one deny, so the trail has to distinguish them.
	allowErr := tools.Authorize(ctx, guard.Action{Tool: "shell_run", Shell: "git status"}, "{}")
	denyErr := tools.Authorize(ctx, guard.Action{Tool: "shell_run", Shell: "curl evil.example"}, "{}")
	t.Logf("allow -> %v; deny -> %v", allowErr, denyErr)
	if allowErr != nil {
		t.Fatalf("the allowlisted command must be allowed: %v", allowErr)
	}
	if denyErr == nil {
		t.Fatal("a non-allowlisted command with no callback must be refused")
	}

	rows, err := app.Store.QueryPermissionAudit(store.PermissionAuditQuery{Tool: "shell_run"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("permission_audit rows: %d", len(rows))
	for _, r := range rows {
		t.Logf("  ts=%d tool=%s decision=%s source=%s reason=%s digest=%q",
			r.TS, r.Tool, r.Decision, r.Source, r.ReasonCode, r.CmdDigest)
	}
	if len(rows) < 2 {
		t.Fatalf("expected both decisions on disk, got %d rows", len(rows))
	}
	var sawAllow, sawDeny bool
	for _, r := range rows {
		switch r.Decision {
		case "allow":
			sawAllow = true
			if !strings.Contains(r.CmdDigest, "git status") {
				t.Errorf("allow row lost the command: %q", r.CmdDigest)
			}
		case "deny":
			sawDeny = true
			if r.ReasonCode == "" {
				t.Error("a denial with no reason code cannot be investigated")
			}
		}
		if r.TS == 0 {
			t.Error("row has no timestamp; the trail cannot be ordered")
		}
	}
	if !sawAllow || !sawDeny {
		t.Errorf("trail must record both outcomes (allow=%v deny=%v)", sawAllow, sawDeny)
	}
}

// TestS6_AuditDigestIsRedacted is the property that decides whether this table
// is an audit trail or a second credential dump. Tool arguments carry tokens
// routinely, and the digest is the one audit field that echoes caller text.
func TestS6_AuditDigestIsRedacted(t *testing.T) {
	app := buildAuditApp(t)

	// Register the secret with the process redactor the same way a real
	// credential becomes known: through the store's redactor, which bootstrap
	// already aliased to the process-wide one.
	const secret = "ghp_S6REDACTIONCANARY0123456789abcdef"
	if app.Redactor == nil {
		t.Fatal("App.Redactor is nil; redaction cannot be wired")
	}
	app.Redactor.Register(secret)

	ctx := tools.WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
	})
	cmd := "curl -H " + secret + " https://example.com"
	if err := tools.Authorize(ctx, guard.Action{Tool: "shell_run", Shell: cmd}, "{}"); err != nil {
		t.Fatalf("setup: the command should have been allowed: %v", err)
	}

	rows, err := app.Store.QueryPermissionAudit(store.PermissionAuditQuery{Tool: "shell_run"})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range rows {
		t.Logf("digest=%q", r.CmdDigest)
		if strings.Contains(r.CmdDigest, "curl") {
			found = true
			if strings.Contains(r.CmdDigest, secret) {
				t.Fatalf("LEAK: the raw credential is in the audit table: %q", r.CmdDigest)
			}
		}
	}
	if !found {
		t.Fatal("the command was not recorded at all; nothing was verified")
	}
}

// TestS6_AuditFailureCannotChangeAVerdict pins the direction of the dependency:
// the archive records what the guard decided and must never be able to veto it.
// A full disk that started denying tool calls would be a denial of service
// wearing an audit trail's clothes.
func TestS6_AuditFailureCannotChangeAVerdict(t *testing.T) {
	prev := tools.PermissionAuditSinkInstalled()
	t.Cleanup(func() {
		if !prev {
			tools.SetPermissionAuditSink(nil)
		}
	})
	tools.SetPermissionAuditSink(&tools.StoreAuditSink{
		Append: func(tools.PermissionAuditRecord) error {
			return os.ErrPermission // every write fails, as on a full disk
		},
	})
	ctx := tools.WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"git status"}},
	})
	if err := tools.Authorize(ctx, guard.Action{Tool: "shell_run", Shell: "git status"}, "{}"); err != nil {
		t.Fatalf("a failing audit sink must not turn an allow into a denial: %v", err)
	}
	t.Log("allow survived a sink whose every write fails")
}
