package approval

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"
)

const persistentKey = "security.approvals.v1"

// Manager owns the rule set for one process. It is goroutine-safe; the
// canonical usage is one Manager per yanshi process (created by bootstrap) and
// many concurrent Match/Record/Revoke callers across WS connections.
//
// Storage layout:
//   - sessions[sessionID] — in-memory only. TTLOnce and TTLSession rules
//     live here; they are dropped when the process exits.
//   - persistent — copy-on-write mirrored to KV. Only TTLPersistent rules
//     live here. The slice is the in-memory projection of the last
//     successfully persisted JSON; a persist failure leaves it unchanged.
type Manager struct {
	mu         sync.Mutex
	kv         KV
	processID  string
	persistent []Rule
	sessions   map[string][]Rule
	emit       func(AuditEvent)
}

// New builds a Manager and (when kv is non-nil) loads any previously-persisted
// TTLPersistent rules. A non-persistent rule in the persistent store is a
// hard error: it indicates a version skew / migration bug and the manager
// refuses to start rather than silently re-classifying rules.
func New(kv KV, processID string, emit func(AuditEvent)) (*Manager, error) {
	m := &Manager{kv: kv, processID: processID, sessions: make(map[string][]Rule), emit: emit}
	if kv == nil {
		return m, nil
	}
	raw, ok, err := kv.KVGet(persistentKey)
	if err != nil {
		return nil, fmt.Errorf("approval: load: %w", err)
	}
	if ok {
		if err := json.Unmarshal([]byte(raw), &m.persistent); err != nil {
			return nil, fmt.Errorf("approval: decode: %w", err)
		}
		for _, r := range m.persistent {
			if r.TTL != TTLPersistent {
				return nil, fmt.Errorf("approval: non-persistent rule %q found in persistent store", r.ID)
			}
		}
	}
	return m, nil
}

// Match reports whether scope is admitted by any Rule visible to sessionID.
// TTLOnce rules are consumed in place when they match (one-shot semantics).
// Order: session rules (so the most recent / specific decision wins) then
// persistent. Each event emits an audit signal: miss / hit / consume.
//
// Returns the matching Rule by value (not pointer) so the caller can inspect
// ID/Action without holding the manager's mutex. The bool is the binary
// outcome for callers that only need that.
func (m *Manager) Match(sessionID string, scope Scope, now time.Time) (bool, *Rule) {
	m.mu.Lock()
	m.expireLocked(sessionID, now)
	for i, r := range m.sessions[sessionID] {
		if reflect.DeepEqual(r.Scope, scope) {
			copyRule := r
			m.auditLocked("hit", sessionID, r, now)
			if r.TTL == TTLOnce {
				rules := append([]Rule(nil), m.sessions[sessionID]...)
				rules = append(rules[:i], rules[i+1:]...)
				m.sessions[sessionID] = rules
				m.auditLocked("consume", sessionID, r, now)
			}
			m.mu.Unlock()
			return true, &copyRule
		}
	}
	for _, r := range m.persistent {
		if reflect.DeepEqual(r.Scope, scope) {
			copyRule := r
			m.auditLocked("hit", sessionID, r, now)
			m.mu.Unlock()
			return true, &copyRule
		}
	}
	m.auditLocked("miss", sessionID, Rule{Scope: scope}, now)
	m.mu.Unlock()
	return false, nil
}

// Record adds rule under sessionID. Validation: ID/Action/Scope.Tool must be
// non-empty; TTL must be one of the known values; TTLOnce/TTLSession require
// a non-empty sessionID. TTLPersistent is mirrored to KV copy-on-write: a
// persist failure returns an error and leaves m.persistent unchanged so a
// later List call can never observe half-written state.
func (m *Manager) Record(sessionID string, rule Rule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rule.ID == "" || rule.Action == "" || rule.Scope.Tool == "" {
		return fmt.Errorf("approval: id/action/scope.tool are required")
	}
	if rule.CreatedAt.IsZero() {
		rule.CreatedAt = time.Now().UTC()
	}
	switch rule.TTL {
	case TTLOnce, TTLSession:
		if sessionID == "" {
			return fmt.Errorf("approval: session id required for %s", rule.TTL)
		}
		rule.ProcessID = m.processID
		copyRules := append([]Rule(nil), m.sessions[sessionID]...)
		m.sessions[sessionID] = append(copyRules, rule)
		return nil
	case TTLPersistent:
		copyRules := append([]Rule(nil), m.persistent...)
		copyRules = append(copyRules, rule)
		if err := m.persistLocked(copyRules); err != nil {
			return err
		}
		m.persistent = copyRules
		return nil
	default:
		return fmt.Errorf("approval: unknown ttl %q", rule.TTL)
	}
}

// List returns the union of session-scoped and persistent rules visible to
// sessionID, sorted by CreatedAt. Expired session rules are dropped as a
// side effect (the only place where ExpiresAt is consulted for TTLSession).
func (m *Manager) List(sessionID string, now time.Time) []Rule {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(sessionID, now)
	out := append([]Rule(nil), m.sessions[sessionID]...)
	out = append(out, m.persistent...)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Revoke removes the rule with the given ID. Searches session rules first,
// then persistent. Persistent revocation is COW: a persist failure leaves the
// in-memory set unchanged and returns the error. Audit emits "revoke".
func (m *Manager) Revoke(sessionID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rules := m.sessions[sessionID]; len(rules) > 0 {
		for i, r := range rules {
			if r.ID == id {
				copyRules := append([]Rule(nil), rules...)
				copyRules = append(copyRules[:i], copyRules[i+1:]...)
				m.sessions[sessionID] = copyRules
				m.auditLocked("revoke", sessionID, r, time.Now())
				return nil
			}
		}
	}
	for i, r := range m.persistent {
		if r.ID == id {
			copyRules := append([]Rule(nil), m.persistent...)
			copyRules = append(copyRules[:i], copyRules[i+1:]...)
			if err := m.persistLocked(copyRules); err != nil {
				return err
			}
			m.persistent = copyRules
			m.auditLocked("revoke", sessionID, r, time.Now())
			return nil
		}
	}
	return fmt.Errorf("approval: rule %q not found", id)
}

// expireLocked drops session rules whose ExpiresAt has passed (now >= ExpiresAt)
// and emits an "expire" audit for each. Called by Match/List with m.mu held.
// Persistent rules do not expire on a timer (they are revocation-only).
func (m *Manager) expireLocked(sessionID string, now time.Time) {
	rules := m.sessions[sessionID]
	kept := make([]Rule, 0, len(rules))
	for _, r := range rules {
		if !r.ExpiresAt.IsZero() && !now.Before(r.ExpiresAt) {
			m.auditLocked("expire", sessionID, r, now)
			continue
		}
		kept = append(kept, r)
	}
	m.sessions[sessionID] = kept
}

// persistLocked marshals rules to JSON and writes them to KV under
// persistentKey. Returns the KV error verbatim (wrapped) so the caller can
// surface it without re-translating. Caller MUST hold m.mu.
func (m *Manager) persistLocked(rules []Rule) error {
	if m.kv == nil {
		return fmt.Errorf("approval: persistent store unavailable")
	}
	data, err := json.Marshal(rules)
	if err != nil {
		return fmt.Errorf("approval: encode: %w", err)
	}
	if err := m.kv.KVSet(persistentKey, string(data)); err != nil {
		return fmt.Errorf("approval: persist: %w", err)
	}
	return nil
}

// auditLocked invokes the registered emit callback (if any) with an AuditEvent
// for the given lifecycle signal. Caller MUST hold m.mu (the callback may
// re-enter Manager methods).
func (m *Manager) auditLocked(kind, sessionID string, rule Rule, at time.Time) {
	if m.emit != nil {
		m.emit(AuditEvent{Kind: kind, RuleID: rule.ID, SessionID: sessionID, Action: rule.Action, Scope: rule.Scope, At: at})
	}
}

// AuditBus fans a single approval.Manager emit callback out to N subscribers
// (one per WS connection). The Manager knows nothing about transports; it just
// calls emit on every lifecycle event. bootstrap constructs one bus, hands
// bus.Publish as the emit callback to approval.New, and passes the bus to the
// HTTP server so each WS connection can Subscribe/Unsubscribe and render
// permission_rule_hit frames (Task 9). Without this, the WS layer has no
// thread-safe way to observe manager events (registering a per-connection
// callback on the manager would overwrite other connections' callbacks).
type AuditBus struct {
	mu          sync.RWMutex
	subscribers map[<-chan AuditEvent]chan AuditEvent
}

// NewAuditBus returns an empty bus.
func NewAuditBus() *AuditBus {
	return &AuditBus{subscribers: make(map[<-chan AuditEvent]chan AuditEvent)}
}

// Publish broadcasts e to every subscriber without blocking: each subscriber
// channel is buffered (64) and drops on full so a slow WS connection can never
// stall the approval manager's Match/Record/Revoke paths.
func (b *AuditBus) Publish(e AuditEvent) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subscribers {
		select {
		case ch <- e:
		default:
		}
	}
}

// Subscribe returns a receive-only buffered channel receiving every subsequent
// AuditEvent. The caller MUST pass the same channel to Unsubscribe when its WS
// connection closes.
func (b *AuditBus) Subscribe() <-chan AuditEvent {
	writable := make(chan AuditEvent, 64)
	readonly := (<-chan AuditEvent)(writable)
	b.mu.Lock()
	b.subscribers[readonly] = writable
	b.mu.Unlock()
	return readonly
}

// Unsubscribe removes ch from the bus and closes it. Subsequent calls with the
// same ch are no-ops (the map lookup returns ok=false).
func (b *AuditBus) Unsubscribe(ch <-chan AuditEvent) {
	b.mu.Lock()
	writable, ok := b.subscribers[ch]
	if ok {
		delete(b.subscribers, ch)
		close(writable)
	}
	b.mu.Unlock()
}
