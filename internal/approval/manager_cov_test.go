package approval

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// errorKV lets a test drive specific failure modes for KVGet/KVSet.
type errorKV struct {
	getErr    error
	setErr    error
	getValue  string
	getOK     bool
	setCalled bool
	setValue  string
}

func (e *errorKV) KVGet(string) (string, bool, error) { return e.getValue, e.getOK, e.getErr }
func (e *errorKV) KVSet(_ string, value string) error {
	e.setCalled = true
	e.setValue = value
	return e.setErr
}

func TestNewNilKV(t *testing.T) {
	m, err := New(nil, "proc-1", nil)
	if err != nil {
		t.Fatalf("nil kv must not error: %v", err)
	}
	if m == nil || m.kv != nil {
		t.Fatalf("unexpected manager: %#v", m)
	}
	// persistLocked with nil kv surfaces the unavailable error.
	if err := m.persistLocked(nil); err == nil {
		t.Fatal("persistLocked must fail with nil kv")
	}
}

func TestNewKVGetErrorCov(t *testing.T) {
	_, err := New(&errorKV{getErr: errors.New("io")}, "proc-1", nil)
	if err == nil || !strings.Contains(err.Error(), "approval: load") {
		t.Fatalf("expected load error, got %v", err)
	}
}

func TestNewDecodeError(t *testing.T) {
	_, err := New(&errorKV{getValue: "{not-json", getOK: true}, "proc-1", nil)
	if err == nil || !strings.Contains(err.Error(), "approval: decode") {
		t.Fatalf("expected decode error, got %v", err)
	}
}

func TestNewNonPersistentInStoreIsError(t *testing.T) {
	bad, _ := json.Marshal([]Rule{{ID: "r1", Action: "shell_run", Scope: Scope{Tool: "shell_run"}, TTL: TTLSession}})
	_, err := New(&errorKV{getValue: string(bad), getOK: true}, "proc-1", nil)
	if err == nil || !strings.Contains(err.Error(), "non-persistent rule") {
		t.Fatalf("expected non-persistent error, got %v", err)
	}
}

func TestNewLoadsPersistentRulesCov(t *testing.T) {
	stored, _ := json.Marshal([]Rule{{ID: "r1", Action: "web_fetch", Scope: Scope{Tool: "web_fetch", Host: "h"}, TTL: TTLPersistent}})
	m, err := New(&errorKV{getValue: string(stored), getOK: true}, "proc-1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rules := m.List("any", time.Now())
	if len(rules) != 1 || rules[0].ID != "r1" {
		t.Fatalf("loaded rules = %#v", rules)
	}
}

func TestRecordValidationErrorsCov(t *testing.T) {
	m, _ := New(&fakeKV{}, "proc-1", nil)
	cases := []struct {
		name     string
		session  string
		rule     Rule
		contains string
	}{
		{"missing id", "s", Rule{Action: "shell_run", Scope: Scope{Tool: "shell_run"}, TTL: TTLOnce}, "required"},
		{"missing action", "s", Rule{ID: "r", Scope: Scope{Tool: "shell_run"}, TTL: TTLOnce}, "required"},
		{"missing scope tool", "s", Rule{ID: "r", Action: "shell_run", TTL: TTLOnce}, "required"},
		{"once needs session", "", Rule{ID: "r", Action: "shell_run", Scope: Scope{Tool: "shell_run"}, TTL: TTLOnce}, "session id required"},
		{"session needs session", "", Rule{ID: "r", Action: "shell_run", Scope: Scope{Tool: "shell_run"}, TTL: TTLSession}, "session id required"},
		{"unknown ttl", "s", Rule{ID: "r", Action: "shell_run", Scope: Scope{Tool: "shell_run"}, TTL: "bogus"}, "unknown ttl"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := m.Record(c.session, c.rule)
			if err == nil || !strings.Contains(err.Error(), c.contains) {
				t.Fatalf("expected error containing %q, got %v", c.contains, err)
			}
		})
	}
}

func TestRecordSetsCreatedAtWhenZeroCov(t *testing.T) {
	m, _ := New(&fakeKV{}, "proc-1", nil)
	rule := Rule{ID: "r", Action: "shell_run", Scope: Scope{Tool: "shell_run"}, TTL: TTLSession}
	if err := m.Record("s", rule); err != nil {
		t.Fatal(err)
	}
	listed := m.List("s", time.Now())
	if len(listed) != 1 || listed[0].CreatedAt.IsZero() {
		t.Fatalf("CreatedAt not set: %#v", listed)
	}
	if listed[0].ProcessID != "proc-1" {
		t.Fatalf("ProcessID not set: %#v", listed)
	}
}

func TestMatchHitsPersistentRule(t *testing.T) {
	m, _ := New(&fakeKV{}, "proc-1", nil)
	scope := Scope{Tool: "web_fetch", Host: "example.com"}
	if err := m.Record("s", Rule{ID: "rp", Action: "web_fetch", Scope: scope, TTL: TTLPersistent}); err != nil {
		t.Fatal(err)
	}
	// Persistent rule visible from a different session.
	if hit, rule := m.Match("other-session", scope, time.Now()); !hit || rule == nil || rule.ID != "rp" {
		t.Fatalf("persistent match failed: hit=%v rule=%#v", hit, rule)
	}
}

func TestRevokePersistentAndNotFound(t *testing.T) {
	m, _ := New(&fakeKV{}, "proc-1", nil)
	scope := Scope{Tool: "web_fetch", Host: "example.com"}
	if err := m.Record("s", Rule{ID: "rp", Action: "web_fetch", Scope: scope, TTL: TTLPersistent}); err != nil {
		t.Fatal(err)
	}
	if err := m.Revoke("s", "rp"); err != nil {
		t.Fatalf("revoke persistent: %v", err)
	}
	if got := m.List("s", time.Now()); len(got) != 0 {
		t.Fatalf("persistent rule survived revoke: %#v", got)
	}
	if err := m.Revoke("s", "missing"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}

func TestRecordTTLPersistentPersistFail(t *testing.T) {
	kv := &errorKV{setErr: errors.New("disk full")}
	kv.getValue = `[]`
	kv.getOK = true
	m, err := New(kv, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	rule := Rule{ID: "r1", Action: "web_fetch", Scope: Scope{Tool: "web_fetch", Host: "example.com"}, TTL: TTLPersistent}
	if err := m.Record("s", rule); err == nil {
		t.Fatal("expected persist failure")
	}
	// In-memory persistent rules must remain unchanged after a failed persist.
	if len(m.persistent) != 0 {
		t.Fatal("persistent rules changed after failed persist")
	}
}

func TestRevokePersistentPersistFailureCov(t *testing.T) {
	kv := &errorKV{setErr: errors.New("disk full")}
	// Seed a persistent rule by giving it a valid get, then make sets fail.
	stored, _ := json.Marshal([]Rule{{ID: "rp", Action: "web_fetch", Scope: Scope{Tool: "web_fetch"}, TTL: TTLPersistent}})
	kv.getValue = string(stored)
	kv.getOK = true
	kv.setErr = errors.New("disk full")
	m, err := New(kv, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Revoke("s", "rp"); err == nil {
		t.Fatal("revoke must surface persist failure")
	}
	// In-memory state must remain unchanged after persist failure.
	if got := m.List("s", time.Now()); len(got) != 1 {
		t.Fatalf("rule disappeared after failed persist: %#v", got)
	}
}

func TestListExpiresSessionRules(t *testing.T) {
	m, _ := New(&fakeKV{}, "proc-1", nil)
	now := time.Now()
	if err := m.Record("s", Rule{ID: "exp", Action: "web_fetch", Scope: Scope{Tool: "web_fetch"}, TTL: TTLSession, ExpiresAt: now.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := m.Record("s", Rule{ID: "live", Action: "web_fetch", Scope: Scope{Tool: "web_fetch", Host: "x"}, TTL: TTLSession, ExpiresAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	got := m.List("s", now)
	if len(got) != 1 || got[0].ID != "live" {
		t.Fatalf("List did not expire: %#v", got)
	}
}

func TestListSortsByCreatedAt(t *testing.T) {
	m, _ := New(&fakeKV{}, "proc-1", nil)
	now := time.Now()
	// Session rule created now.
	if err := m.Record("s", Rule{ID: "session-first", Action: "shell_run", Scope: Scope{Tool: "shell_run"}, TTL: TTLSession}); err != nil {
		t.Fatal(err)
	}
	// Persistent rule with earlier timestamp.
	early := now.Add(-10 * time.Minute)
	if err := m.Record("s", Rule{ID: "persistent-older", Action: "web_fetch", Scope: Scope{Tool: "web_fetch", Host: "example.com"}, TTL: TTLPersistent, CreatedAt: early}); err != nil {
		t.Fatal(err)
	}
	got := m.List("s", now)
	if len(got) != 2 {
		t.Fatalf("expected 2 rules, got %#v", got)
	}
	// Must be sorted ascending: persistent-older (earlier) then session-first (later).
	if got[0].ID != "persistent-older" || got[1].ID != "session-first" {
		t.Fatalf("List is not sorted by CreatedAt: %#v", got)
	}
}

func TestAuditBusDropsOnFull(t *testing.T) {
	bus := NewAuditBus()
	ch := bus.Subscribe()
	// Buffer is 64; fill it then publish one more to exercise the drop path.
	for i := 0; i < 64; i++ {
		bus.Publish(AuditEvent{Kind: "hit"})
	}
	bus.Publish(AuditEvent{Kind: "miss"}) // dropped, must not block
	// Drain the 64 buffered events; the 65th must not be present.
	count := 0
	drain := time.After(100 * time.Millisecond)
	for {
		select {
		case <-ch:
			count++
		case <-drain:
			if count != 64 {
				t.Fatalf("expected 64 buffered events, got %d", count)
			}
			bus.Unsubscribe(ch)
			return
		}
	}
}

func TestAuditBusUnsubscribeIdempotent(t *testing.T) {
	bus := NewAuditBus()
	ch := bus.Subscribe()
	bus.Unsubscribe(ch)
	bus.Unsubscribe(ch) // must not panic on double unsubscribe
}

func TestRuleActionGroupsByEcho(t *testing.T) {
	// Sanity: Action is independent of Scope.Tool (UI grouping hook).
	r := Rule{ID: "r", Action: "shell", Scope: Scope{Tool: "shell_run"}}
	if r.Action == r.Scope.Tool {
		t.Fatal("Action and Scope.Tool should be distinguishable fields")
	}
}
