package approval

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeKV struct {
	value string
	fail  bool
}

func (f *fakeKV) KVGet(string) (string, bool, error) {
	if f.fail {
		return "", false, errors.New("disk full")
	}
	return f.value, f.value != "", nil
}

func (f *fakeKV) KVSet(_ string, value string) error {
	if f.fail {
		return errors.New("disk full")
	}
	f.value = value
	return nil
}

// ----- New -----

func TestNewWithNilKV(t *testing.T) {
	m, err := New(nil, "proc-1", nil)
	if err != nil {
		t.Fatalf("New with nil kv should succeed: %v", err)
	}
	if m == nil {
		t.Fatal("New returned nil manager")
	}
}

func TestNewKVGetError(t *testing.T) {
	kv := &fakeKV{fail: true}
	_, err := New(kv, "proc-1", nil)
	if err == nil {
		t.Fatal("expected error from KVGet failure")
	}
}

func TestNewCorruptPersistentData(t *testing.T) {
	kv := &fakeKV{value: "{not-json"}
	_, err := New(kv, "proc-1", nil)
	if err == nil {
		t.Fatal("expected error from corrupt JSON")
	}
}

func TestNewNonPersistentRuleInStore(t *testing.T) {
	kv := &fakeKV{value: `[{"id":"r1","action":"shell_run","scope":{"tool":"shell_run"},"ttl":"once","source":"user"}]`}
	_, err := New(kv, "proc-1", nil)
	if err == nil {
		t.Fatal("expected error for non-persistent rule in persistent store")
	}
}

func TestNewLoadsPersistentRules(t *testing.T) {
	kv := &fakeKV{}
	m, err := New(kv, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Write a persistent rule
	rule := Rule{ID: "r1", Action: "shell_run", Scope: Scope{Tool: "shell_run"}, TTL: TTLPersistent, Source: SourceUser}
	if err := m.Record("s", rule); err != nil {
		t.Fatal(err)
	}
	// Re-create manager from the same KV
	m2, err := New(kv, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	rules := m2.List("s", time.Now())
	if len(rules) != 1 || rules[0].ID != "r1" {
		t.Fatalf("expected one persistent rule loaded, got %#v", rules)
	}
}

// ----- Match -----

func TestMatchPersistentHit(t *testing.T) {
	m, err := New(&fakeKV{}, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Tool: "shell_run", Program: "go"}
	if err := m.Record("s", Rule{ID: "r1", Action: "shell_run", Scope: scope, TTL: TTLPersistent, Source: SourceUser}); err != nil {
		t.Fatal(err)
	}
	hit, rule := m.Match("other-session", scope, time.Now())
	if !hit {
		t.Fatal("persistent rule must hit across sessions")
	}
	if rule.ID != "r1" {
		t.Fatalf("expected rule ID r1, got %s", rule.ID)
	}
}

func TestMatchSessionHit(t *testing.T) {
	m, err := New(&fakeKV{}, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Tool: "web_fetch", Host: "example.com"}
	if err := m.Record("s", Rule{ID: "r1", Action: "web_fetch", Scope: scope, TTL: TTLSession, Source: SourceUser}); err != nil {
		t.Fatal(err)
	}
	hit, rule := m.Match("s", scope, time.Now())
	if !hit {
		t.Fatal("session rule must hit")
	}
	if rule.ID != "r1" {
		t.Fatalf("expected rule ID r1, got %s", rule.ID)
	}
	// Must still be present (TTLSession, not TTLOnce)
	hit, _ = m.Match("s", scope, time.Now())
	if !hit {
		t.Fatal("session rule must survive first match")
	}
}

func TestMatchConsumeOnceWithExpiry(t *testing.T) {
	m, err := New(&fakeKV{}, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Tool: "shell_run", Program: "rm"}
	now := time.Now()
	if err := m.Record("s", Rule{ID: "r1", Action: "shell_run", Scope: scope, TTL: TTLOnce, Source: SourceUser, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// First match consumes it
	hit, rule := m.Match("s", scope, now)
	if !hit {
		t.Fatal("once rule must hit first time")
	}
	if rule.ID != "r1" {
		t.Fatalf("expected r1, got %s", rule.ID)
	}
	// Second match should miss
	hit, _ = m.Match("s", scope, now)
	if hit {
		t.Fatal("once rule must be consumed")
	}
}

func TestMatchNoMatch(t *testing.T) {
	m, err := New(&fakeKV{}, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	hit, rule := m.Match("s", Scope{Tool: "nonexistent"}, time.Now())
	if hit {
		t.Fatal("expected no match")
	}
	if rule != nil {
		t.Fatalf("expected nil rule, got %#v", rule)
	}
}

func TestMatchReturnsEmptyRuleOnMiss(t *testing.T) {
	// verify that the returned *Rule on miss has the scope set
	var capturedScope Scope
	m, err := New(&fakeKV{}, "proc-1", func(e AuditEvent) {
		capturedScope = e.Scope
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Tool: "shell_run", Program: "go"}
	hit, rule := m.Match("s", scope, time.Now())
	if hit {
		t.Fatal("expected miss")
	}
	if rule != nil {
		t.Fatal("expected nil rule on miss")
	}
	if capturedScope.Tool != "shell_run" {
		t.Fatalf("expected scope in audit miss event, got %#v", capturedScope)
	}
}

// ----- Record -----

func TestRecordValidationErrors(t *testing.T) {
	m, err := New(&fakeKV{}, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		rule    Rule
		session string
	}{
		{name: "empty id", rule: Rule{Action: "a", Scope: Scope{Tool: "t"}, TTL: TTLOnce, Source: SourceUser}, session: "s"},
		{name: "empty action", rule: Rule{ID: "r1", Scope: Scope{Tool: "t"}, TTL: TTLOnce, Source: SourceUser}, session: "s"},
		{name: "empty scope tool", rule: Rule{ID: "r1", Action: "a", TTL: TTLOnce, Source: SourceUser}, session: "s"},
		{name: "empty session for once", rule: Rule{ID: "r1", Action: "a", Scope: Scope{Tool: "t"}, TTL: TTLOnce, Source: SourceUser}, session: ""},
		{name: "empty session for session", rule: Rule{ID: "r1", Action: "a", Scope: Scope{Tool: "t"}, TTL: TTLSession, Source: SourceUser}, session: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := m.Record(tc.session, tc.rule); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestRecordUnknownTTL(t *testing.T) {
	m, err := New(&fakeKV{}, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = m.Record("s", Rule{ID: "r1", Action: "a", Scope: Scope{Tool: "t"}, TTL: "invalid", Source: SourceUser})
	if err == nil {
		t.Fatal("expected error for unknown TTL")
	}
}

func TestRecordSetsCreatedAtWhenZero(t *testing.T) {
	m, err := New(&fakeKV{}, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Record("s", Rule{ID: "r1", Action: "shell_run", Scope: Scope{Tool: "shell_run"}, TTL: TTLSession, Source: SourceUser}); err != nil {
		t.Fatal(err)
	}
	rules := m.List("s", time.Now())
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].CreatedAt.IsZero() {
		t.Fatal("CreatedAt should be set automatically")
	}
}

func TestRecordTTLOnceSuccess(t *testing.T) {
	m, err := New(&fakeKV{}, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	rule := Rule{ID: "r1", Action: "shell_run", Scope: Scope{Tool: "shell_run"}, TTL: TTLOnce, Source: SourceUser}
	if err := m.Record("s", rule); err != nil {
		t.Fatal(err)
	}
	rules := m.List("s", time.Now())
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].ProcessID != "proc-1" {
		t.Fatalf("expected process ID proc-1, got %s", rules[0].ProcessID)
	}
}

func TestRecordTTLPersistentSuccess(t *testing.T) {
	kv := &fakeKV{}
	m, err := New(kv, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	rule := Rule{ID: "r1", Action: "shell_run", Scope: Scope{Tool: "shell_run"}, TTL: TTLPersistent, Source: SourceUser}
	if err := m.Record("s", rule); err != nil {
		t.Fatal(err)
	}
	// Check persistent data was written
	raw, ok, err := kv.KVGet(persistentKey)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || raw == "" {
		t.Fatal("persistent data should be written to KV")
	}
}

// ----- List -----

func TestListEmpty(t *testing.T) {
	m, err := New(&fakeKV{}, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	rules := m.List("s", time.Now())
	if len(rules) != 0 {
		t.Fatalf("expected empty list, got %d", len(rules))
	}
}

func TestListPreservesOrder(t *testing.T) {
	m, err := New(&fakeKV{}, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Add rules with created times in the past
	scope1 := Scope{Tool: "tool1"}
	scope2 := Scope{Tool: "tool2"}
	// Use Record which sets CreatedAt; record in reverse order
	if err := m.Record("s", Rule{ID: "r2", Action: "a", Scope: scope2, TTL: TTLSession, Source: SourceUser}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond) // ensure distinct timestamps
	if err := m.Record("s", Rule{ID: "r1", Action: "a", Scope: scope1, TTL: TTLSession, Source: SourceUser}); err != nil {
		t.Fatal(err)
	}
	// Add a persistent rule
	if err := m.Record("s", Rule{ID: "r3", Action: "a", Scope: scope1, TTL: TTLPersistent, Source: SourceUser}); err != nil {
		t.Fatal(err)
	}
	rules := m.List("s", time.Now())
	if len(rules) != 3 {
		t.Fatalf("expected 3 rules, got %d", len(rules))
	}
	// Should be sorted by CreatedAt ascending: r2, r1, r3
	if rules[0].ID != "r2" || rules[1].ID != "r1" || rules[2].ID != "r3" {
		t.Fatalf("expected order r2, r1, r3 got %s, %s, %s", rules[0].ID, rules[1].ID, rules[2].ID)
	}
}

// ----- Revoke -----

func TestRevokeRuleNotFound(t *testing.T) {
	m, err := New(&fakeKV{}, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Revoke("s", "nonexistent"); err == nil {
		t.Fatal("expected error for non-existent rule")
	}
}

func TestRevokePersistentRule(t *testing.T) {
	kv := &fakeKV{}
	m, err := New(kv, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Tool: "shell_run"}
	if err := m.Record("s", Rule{ID: "r1", Action: "a", Scope: scope, TTL: TTLPersistent, Source: SourceUser}); err != nil {
		t.Fatal(err)
	}
	if err := m.Revoke("s", "r1"); err != nil {
		t.Fatal(err)
	}
	// Must be gone from list
	rules := m.List("s", time.Now())
	for _, r := range rules {
		if r.ID == "r1" {
			t.Fatal("revoked rule should not appear in list")
		}
	}
	// Persistent store must be updated
	raw, ok, err := kv.KVGet(persistentKey)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("persistent key should still exist")
	}
	if raw != "[]" {
		t.Fatalf("expected empty array, got %s", raw)
	}
}

func TestRevokePersistentPersistFailure(t *testing.T) {
	kv := &fakeKV{}
	// Create manager without failure so we can seed a persistent rule.
	m, err := New(kv, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Tool: "t"}
	if err := m.Record("s", Rule{ID: "r1", Action: "a", Scope: scope, TTL: TTLPersistent, Source: SourceUser}); err != nil {
		t.Fatal(err)
	}
	// Now make KVSet fail so Revoke's persist attempt errors.
	kv.fail = true

	err = m.Revoke("s", "r1")
	if err == nil {
		t.Fatal("expected persist failure on revoke")
	}
	// The persistent in-memory list must be unchanged after persist failure.
	rules := m.List("s", time.Now())
	if len(rules) != 1 || rules[0].ID != "r1" {
		t.Fatalf("failed persist should leave in-memory rule unchanged, got %#v", rules)
	}
}

func TestRevokeSessionRule(t *testing.T) {
	m, err := New(&fakeKV{}, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Tool: "shell_run"}
	if err := m.Record("s", Rule{ID: "r1", Action: "a", Scope: scope, TTL: TTLSession, Source: SourceUser}); err != nil {
		t.Fatal(err)
	}
	if err := m.Revoke("s", "r1"); err != nil {
		t.Fatal(err)
	}
	rules := m.List("s", time.Now())
	for _, r := range rules {
		if r.ID == "r1" {
			t.Fatal("revoked session rule should not appear in list")
		}
	}
}

// ----- Expiry edge cases -----

func TestExpireRemovesExpiredRules(t *testing.T) {
	m, err := New(&fakeKV{}, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	scope := Scope{Tool: "shell_run"}
	if err := m.Record("s", Rule{ID: "r1", Action: "a", Scope: scope, TTL: TTLSession, Source: SourceUser, ExpiresAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := m.Record("s", Rule{ID: "r2", Action: "a", Scope: scope, TTL: TTLSession, Source: SourceUser}); err != nil {
		t.Fatal(err)
	}
	rules := m.List("s", now)
	if len(rules) != 1 || rules[0].ID != "r2" {
		t.Fatalf("expected only r2 (expired r1 should be gone), got %#v", rules)
	}
}

func TestExpireOnlyDropsExpired(t *testing.T) {
	m, err := New(&fakeKV{}, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	scope := Scope{Tool: "shell_run"}
	if err := m.Record("s", Rule{ID: "r1", Action: "a", Scope: scope, TTL: TTLSession, Source: SourceUser, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := m.Record("s", Rule{ID: "r2", Action: "a", Scope: scope, TTL: TTLSession, Source: SourceUser}); err != nil {
		t.Fatal(err)
	}
	rules := m.List("s", now)
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules (none expired), got %d", len(rules))
	}
}

// ----- Audit bus edge cases -----

func TestAuditBusDropOnFullChannel(t *testing.T) {
	bus := NewAuditBus()
	ch := bus.Subscribe()
	// Fill the buffer (capacity 64)
	event := AuditEvent{Kind: "hit"}
	for i := 0; i < 64; i++ {
		bus.Publish(event)
	}
	// The 65th publish should not block and should be dropped
	bus.Publish(event)
	// Drain
	count := 0
	for {
		select {
		case <-ch:
			count++
		case <-time.After(100 * time.Millisecond):
			// no more events
			if count != 64 {
				t.Fatalf("expected 64 events, got %d", count)
			}
			bus.Unsubscribe(ch)
			return
		}
	}
}

func TestAuditBusUnsubscribeNoop(t *testing.T) {
	bus := NewAuditBus()
	ch := bus.Subscribe()
	bus.Unsubscribe(ch)
	// Second unsubscribe should be a no-op (not panic)
	bus.Unsubscribe(ch)
}

func TestAuditBusSubscribeMultiple(t *testing.T) {
	bus := NewAuditBus()
	ch1 := bus.Subscribe()
	ch2 := bus.Subscribe()
	event := AuditEvent{Kind: "hit", RuleID: "r1"}
	bus.Publish(event)
	select {
	case got := <-ch1:
		if got.Kind != "hit" {
			t.Fatalf("ch1 expected hit, got %s", got.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("ch1 didn't receive event")
	}
	select {
	case got := <-ch2:
		if got.Kind != "hit" {
			t.Fatalf("ch2 expected hit, got %s", got.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("ch2 didn't receive event")
	}
	bus.Unsubscribe(ch1)
	bus.Unsubscribe(ch2)
}

// ----- Concurrent access -----

func TestConcurrentAccess(t *testing.T) {
	m, err := New(&fakeKV{}, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Tool: "shell_run", Program: "go"}
	var wg sync.WaitGroup
	// Concurrent Record calls
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("r%d", i)
			m.Record("s", Rule{ID: id, Action: "shell_run", Scope: scope, TTL: TTLSession, Source: SourceUser})
		}(i)
	}
	wg.Wait()
	// Concurrent Match + Revoke
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Match("s", scope, time.Now())
			m.List("s", time.Now())
		}()
	}
	wg.Wait()
	// Concurrent Revoke
	m.Record("s", Rule{ID: "revokeme", Action: "shell_run", Scope: scope, TTL: TTLSession, Source: SourceUser})
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Revoke("s", "revokeme")
		}()
	}
	wg.Wait()
}

// ----- Session isolation -----

func TestSessionIsolation(t *testing.T) {
	m, err := New(&fakeKV{}, "proc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Tool: "shell_run"}
	m.Record("s1", Rule{ID: "r1", Action: "a", Scope: scope, TTL: TTLSession, Source: SourceUser})
	m.Record("s2", Rule{ID: "r2", Action: "a", Scope: scope, TTL: TTLSession, Source: SourceUser})

	rules1 := m.List("s1", time.Now())
	if len(rules1) != 1 || rules1[0].ID != "r1" {
		t.Fatalf("s1 should only see its own rules, got %#v", rules1)
	}
	rules2 := m.List("s2", time.Now())
	if len(rules2) != 1 || rules2[0].ID != "r2" {
		t.Fatalf("s2 should only see its own rules, got %#v", rules2)
	}
}

// TestRuleTTLGovernsExpiry pins that a rule's TTL class actually bounds its
// lifetime.
//
// expireLocked drops rules whose ExpiresAt has passed and has been correct
// since it was written, but neither production Record call site set the
// field, so it never met a non-zero value: every recorded rule lived until
// the process did. A "session" approval that outlives the session is the
// same object as a persistent one, which is not what the user was asked.
//
// Uses the now parameter Match already takes rather than sleeping, so the
// boundary is exact instead of approximate.
//
// ledger: A1/S07#1 规则含来源/scope/过期
func TestRuleTTLGovernsExpiry(t *testing.T) {
	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	scope := Scope{Tool: "shell_run", Program: "go", Prefix: []string{"test", "./pkg"}}

	m, err := New(nil, "proc", nil)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if err := m.Record("s1", Rule{
		ID: "r1", Action: "shell_run", Scope: scope,
		TTL: TTLSession, Source: SourceUser,
		CreatedAt: base, ExpiresAt: base.Add(time.Hour),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	if hit, _ := m.Match("s1", scope, base.Add(59*time.Minute)); !hit {
		t.Error("a rule inside its TTL must still match, or expiry has become a blanket refusal")
	}
	if hit, _ := m.Match("s1", scope, base.Add(time.Hour)); hit {
		t.Error("a rule must stop matching at its expiry instant, not after it")
	}
	if hit, _ := m.Match("s1", scope, base.Add(2*time.Hour)); hit {
		t.Error("an expired rule must stay expired")
	}
}

// TestRecordedRulesCarryAnExpiry pins the production side of the same
// guarantee: the rules permctx records must arrive with an ExpiresAt.
//
// The manager can only expire what it is told to. Checked at the source
// because reaching those call sites needs a live guard decision, an approval
// context and an interactive callback, and the claim is about which field the
// literal sets.
//
// ledger: A1/S07#1 规则含来源/scope/过期
func TestRecordedRulesCarryAnExpiry(t *testing.T) {
	src, err := os.ReadFile("../tools/permctx.go")
	if err != nil {
		t.Fatalf("read permctx.go: %v", err)
	}
	body := string(src)
	for _, want := range []string{
		"TTL: approval.TTLSession, Source: approval.SourceUser, ExpiresAt:",
		"TTL: approval.TTLPersistent, Source: approval.SourceUser, ExpiresAt:",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("a recorded rule no longer carries an expiry (%q): the manager's "+
				"expiry logic has nothing to act on and approvals live until the process dies", want)
		}
	}
}

// TestScopeMatchIsExactNotPrefix pins that an approval covers the argv it was
// granted for and nothing adjacent to it.
//
// Match compares scopes with reflect.DeepEqual, so `go test ./pkg` approved
// once does not silently cover `go test ./pkg -tags=e2e_real`. That matters
// because it is exactly how a granted approval would become a way around
// execpolicy: the operator approves a benign command, and a longer command
// sharing its prefix inherits the approval without ever reaching the rules
// that would deny it.
//
// ⚠️ The exactness is DELIBERATE, not a prefix tier waiting to be built. If
// someone wants prefix matching, this test is the thing that should stop
// them: before relaxing it they have to show a prefix rule cannot step over
// an execpolicy deny, which is the question this design avoids by never
// having a prefix surface at all. Changing it is a security decision and
// needs its own ADR.
//
// ledger: A1/S07#4 前缀规则有绕过回归测试
func TestScopeMatchIsExactNotPrefix(t *testing.T) {
	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	granted := Scope{Tool: "shell_run", Program: "go", Prefix: []string{"test", "./pkg"}}

	m, err := New(nil, "proc", nil)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if err := m.Record("s1", Rule{
		ID: "r1", Action: "shell_run", Scope: granted,
		TTL: TTLSession, Source: SourceUser,
		CreatedAt: base, ExpiresAt: base.Add(time.Hour),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	cases := []struct {
		name  string
		scope Scope
		want  bool
	}{
		{"the exact argv that was approved", granted, true},
		{"a longer argv sharing the prefix", Scope{Tool: "shell_run", Program: "go",
			Prefix: []string{"test", "./pkg", "-tags=e2e_real"}}, false},
		{"a shorter argv the approval contains", Scope{Tool: "shell_run", Program: "go",
			Prefix: []string{"test"}}, false},
		{"the same argv under a different tool", Scope{Tool: "task_gate_run", Program: "go",
			Prefix: []string{"test", "./pkg"}}, false},
		{"the same argv under a different program", Scope{Tool: "shell_run", Program: "gotest",
			Prefix: []string{"test", "./pkg"}}, false},
	}
	for _, tc := range cases {
		got, _ := m.Match("s1", tc.scope, base)
		if got != tc.want {
			t.Errorf("%s: match=%v, want %v -- scope matching must be exact equality, "+
				"or an approval for a benign command covers longer ones that execpolicy would deny",
				tc.name, got, tc.want)
		}
	}
}
