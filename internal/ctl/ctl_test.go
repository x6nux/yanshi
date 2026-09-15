package ctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/features"
	"github.com/x6nux/yanshi/internal/skills"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/vcs"
)

// newTestService builds a control plane over a temp store, with a config file
// so Open() has something real to read.
func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf("server:\n  http_addr: \"127.0.0.1:0\"\nstorage:\n  sqlite_path: %q\n",
		filepath.ToSlash(filepath.Join(dir, "test.db")))
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	svc, err := Open(cfgPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(svc.Close)
	return svc, cfgPath
}

// TestSessionsRoundTrip is the stored half: create through the store, read and
// mutate through the control plane, and confirm the mutations landed.
func TestSessionsRoundTrip(t *testing.T) {
	svc, _ := newTestService(t)
	st := svc.deps.Store
	id, err := st.CreateSession("original title")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.AppendMessage(id, 0, "user", "hello"); err != nil {
		t.Fatalf("append: %v", err)
	}

	rows, err := svc.Sessions(10, false)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != id || rows[0].Title != "original title" {
		t.Fatalf("Sessions = %+v", rows)
	}

	one, err := svc.Session(id)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if one.Messages != 1 {
		t.Fatalf("message count = %d, want 1", one.Messages)
	}

	if err := svc.RenameSession(id, "renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := svc.SetSessionArchived(id, true); err != nil {
		t.Fatalf("archive: %v", err)
	}
	active, _ := svc.Sessions(10, false)
	if len(active) != 0 {
		t.Fatalf("archived session still listed as active: %+v", active)
	}
	archived, _ := svc.Sessions(10, true)
	if len(archived) != 1 || archived[0].Title != "renamed" {
		t.Fatalf("archived = %+v", archived)
	}
	if err := svc.SetSessionArchived(id, false); err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	msgs, err := svc.SessionMessages(id, 1)
	if err != nil || len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("SessionMessages = %+v err=%v", msgs, err)
	}
	if err := svc.DeleteSession(id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Session(id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Session after delete: err=%v, want ErrNotFound", err)
	}
}

// TestUsageRollsUpAndFlagsUnknownCost pins the one number that must NOT look
// complete: when any counted session has an unpriced provider, the roll-up says
// so instead of presenting a partial total as the answer.
func TestUsageRollsUpAndFlagsUnknownCost(t *testing.T) {
	svc, _ := newTestService(t)
	id, err := svc.deps.Store.CreateSession("priced")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.deps.Store.UpdateSessionMeta(id, "m", "medium", 100, 20, 1, 0, 0, store.BillingMeta{}); err != nil {
		t.Fatalf("meta: %v", err)
	}
	one, err := svc.Usage(id, 10)
	if err != nil {
		t.Fatalf("Usage(id): %v", err)
	}
	if one.Sessions != 1 || one.TokensIn != 100 || one.TokensOut != 20 {
		t.Fatalf("Usage(id) = %+v", one)
	}
	if one.CostKnown {
		t.Fatal("a session with no pricing entry must report CostKnown=false")
	}

	all, err := svc.Usage("", 10)
	if err != nil {
		t.Fatalf("Usage(all): %v", err)
	}
	if all.TokensIn != 100 {
		t.Fatalf("Usage(all) = %+v", all)
	}
	if all.CostKnown {
		t.Fatal("aggregate must inherit the unknown-cost flag")
	}
}

// TestLiveDomainsReportNeedsDaemon is the honest-failure rule: an operation
// whose state lives in a process must say so, not return an empty list that
// reads like "there is nothing".
func TestLiveDomainsReportNeedsDaemon(t *testing.T) {
	svc, _ := newTestService(t)
	if _, err := svc.Skills(); !errors.Is(err, ErrNeedsDaemon) {
		t.Errorf("Skills err=%v, want ErrNeedsDaemon", err)
	}
	if _, err := svc.Features(); !errors.Is(err, ErrNeedsDaemon) {
		t.Errorf("Features err=%v, want ErrNeedsDaemon", err)
	}
	if _, err := svc.Jobs(); !errors.Is(err, ErrNeedsDaemon) {
		t.Errorf("Jobs err=%v, want ErrNeedsDaemon", err)
	}
	if _, err := svc.MCP(context.Background()); !errors.Is(err, ErrNeedsDaemon) {
		t.Errorf("MCP err=%v, want ErrNeedsDaemon", err)
	}
	if _, err := svc.VCSLog(5); !errors.Is(err, ErrNeedsDaemon) {
		t.Errorf("VCSLog err=%v, want ErrNeedsDaemon", err)
	}
	if err := svc.SetFeature("x", true); !errors.Is(err, ErrNeedsDaemon) {
		t.Errorf("SetFeature err=%v, want ErrNeedsDaemon", err)
	}
	if err := svc.SetSkillEnabled("x", true); !errors.Is(err, ErrNeedsDaemon) {
		t.Errorf("SetSkillEnabled err=%v, want ErrNeedsDaemon", err)
	}
}

// TestFeaturesListAndSet drives the live half with a real registry.
func TestFeaturesListAndSet(t *testing.T) {
	reg := features.NewRegistry(false)
	reg.Register(features.Spec{Key: "observe.cost", Stage: "beta", Default: false, Owner: "obs"})
	svc := New(Deps{Features: reg})

	rows, err := svc.Features()
	if err != nil {
		t.Fatalf("Features: %v", err)
	}
	if len(rows) != 1 || rows[0].Key != "observe.cost" || rows[0].Enabled {
		t.Fatalf("rows = %+v", rows)
	}
	if err := svc.SetFeature("observe.cost", true); err != nil {
		t.Fatalf("SetFeature: %v", err)
	}
	rows, _ = svc.Features()
	if !rows[0].Enabled {
		t.Fatal("SetFeature did not take effect")
	}
}

// TestSkillsListShowAndToggle drives the skill half with a real loader over a
// temp skill directory, including the two states a list must not hide: a
// disabled skill and one whose required program is missing.
func TestSkillsListShowAndToggle(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "greeter", "Use when greeting", "Say hello nicely.")
	loader := skills.NewLoader(skills.Builtin(dir))
	reg, err := loader.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	svc := New(Deps{Skills: reg})

	rows, err := svc.Skills()
	if err != nil {
		t.Fatalf("Skills: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "greeter" || !rows[0].Enabled {
		t.Fatalf("rows = %+v", rows)
	}
	if err := svc.SetSkillEnabled("greeter", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	rows, _ = svc.Skills()
	if rows[0].Enabled {
		t.Fatal("disable did not take effect")
	}
	if err := svc.SetSkillTrusted("greeter", true); err != nil {
		t.Fatalf("trust: %v", err)
	}
	view, body, err := svc.Skill("greeter")
	if err != nil {
		t.Fatalf("Skill: %v", err)
	}
	if !view.Trusted || !strings.Contains(body, "Say hello nicely") {
		t.Fatalf("view=%+v body=%q", view, body)
	}
	if _, _, err := svc.Skill("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown skill err=%v, want ErrNotFound", err)
	}
}

// TestApprovalsPersistentRoundTrip proves the offline view is real: a rule
// recorded as persistent is visible to a second Service that opens the same
// store, which is what makes `yanshi approvals list` useful without a daemon.
func TestApprovalsPersistentRoundTrip(t *testing.T) {
	svc, cfgPath := newTestService(t)
	rule := approval.Rule{
		ID:     "rule-1",
		Action: "fs_write",
		Scope:  approval.Scope{Tool: "fs_write", FSOp: "write", Paths: []string{"/tmp/x"}},
		TTL:    approval.TTLPersistent,
		Source: approval.SourceUser,
	}
	if err := svc.deps.Approvals.Record("", rule); err != nil {
		t.Fatalf("record: %v", err)
	}
	rows, err := svc.Approvals("")
	if err != nil {
		t.Fatalf("Approvals: %v", err)
	}
	if len(rows) != 1 || rows[0].TTL != "persistent" {
		t.Fatalf("rows = %+v", rows)
	}

	// A SECOND service over the same database sees the same rule.
	other, err := Open(cfgPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer other.Close()
	again, err := other.Approvals("")
	if err != nil {
		t.Fatalf("Approvals (reopened): %v", err)
	}
	if len(again) != 1 || again[0].ID != rows[0].ID {
		t.Fatalf("reopened rows = %+v, want %+v", again, rows)
	}
	if err := other.RevokeApproval("", rows[0].ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	left, _ := other.Approvals("")
	if len(left) != 0 {
		t.Fatalf("revoke did not remove the rule: %+v", left)
	}
}

func writeSkill(t *testing.T, root, name, description, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}
}

// TestMutationsRejectAnUnknownSession pins the guard the store does not have:
// its UPDATE/DELETE statements report success for zero rows, so without this
// check `session/delete` on a typo'd id answers {"ok":true} and a script cannot
// tell "deleted" from "never existed" — which is exactly what the confirm guard
// exists to prevent.
func TestMutationsRejectAnUnknownSession(t *testing.T) {
	svc, _ := newTestService(t)
	const missing = "does-not-exist"
	if err := svc.RenameSession(missing, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RenameSession err=%v, want ErrNotFound", err)
	}
	if err := svc.SetSessionArchived(missing, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetSessionArchived err=%v, want ErrNotFound", err)
	}
	if err := svc.SetSessionArchived(missing, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("unarchive err=%v, want ErrNotFound", err)
	}
	if err := svc.DeleteSession(missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteSession err=%v, want ErrNotFound", err)
	}
	if _, err := svc.ForkSession(missing, -1); !errors.Is(err, ErrNotFound) {
		t.Errorf("ForkSession err=%v, want ErrNotFound", err)
	}
	if err := svc.DeleteSession(""); err == nil {
		t.Error(`DeleteSession("") must fail`)
	}
}

// TestVCSDiffRejectsAnUnknownRef pins the other silent-lie case: Diff resolves
// a missing commit to an empty tree, so `diff <typo> <head>` answers "every
// file was added" — measured through `yanshi ipc vcs/diff` before the check
// existed (exit 0, three files added).
func TestVCSDiffRejectsAnUnknownRef(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "vcs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	v := vcs.New(st, dir)
	svc := New(Deps{VCS: v, VCSRepoID: "repo-1"})
	if _, err := svc.VCSDiff("deadbeef", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("VCSDiff(unknown ref) err=%v, want ErrNotFound", err)
	}
	// With no VCS wired at all the answer is the daemon hint, not not-found.
	bare := New(Deps{})
	if _, err := bare.VCSDiff("a", "b"); !errors.Is(err, ErrNeedsDaemon) {
		t.Fatalf("VCSDiff(no VCS) err=%v, want ErrNeedsDaemon", err)
	}
}

// TestModelsIsNeverNil pins the wire shape: {"models": null} made a client
// iterating the field get null instead of an empty list.
func TestModelsIsNeverNil(t *testing.T) {
	svc := New(Deps{})
	if got := svc.Models(); got == nil {
		t.Fatal("Models() returned nil; it must marshal as []")
	}
	blob, err := json.Marshal(map[string]any{"models": svc.Models()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(blob) != `{"models":[]}` {
		t.Fatalf("marshalled %s, want {\"models\":[]}", blob)
	}
}
