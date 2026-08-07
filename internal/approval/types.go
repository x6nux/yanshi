// Package approval records and matches per-session / per-process persistent
// permission decisions (the "allow_persistent" / "allow_session" outcomes
// surfaced by Task 8's Authorize). The Manager is the single source of truth
// for which (action, scope) pairs the user already approved so the
// interactive callback layer does not re-prompt for the same call.
//
// Three lifetime classes:
//   - TTLOnce: admitted exactly once in the originating session, then dropped.
//     Used for one-off approvals where the user did not pick "session" or
//     "persistent".
//   - TTLSession: lives until the session ends (TTL bounded by ExpiresAt when
//     set; otherwise dropped at process exit since session rules are
//     in-memory only).
//   - TTLPersistent: durable across process restarts. Persisted to the KV
//     store with copy-on-write semantics — a persist failure leaves the
//     in-memory state untouched so partial state can never be observed.
package approval

import "time"

// TTL names a Rule's lifetime class.
type TTL string

// Rule lifetime classes.
const (
	TTLOnce       TTL = "once"
	TTLSession    TTL = "session"
	TTLPersistent TTL = "persistent"
)

// Source records who created the rule. SourceUser came from an interactive
// permission popup; SourceMode came from the active permission mode
// (allow-edits / yolo) automatically.
type Source string

// Sources that create a permission rule.
const (
	SourceUser Source = "user"
	SourceMode Source = "mode"
)

// Scope is the (tool, args-shape) tuple a Rule admits. It is the same shape as
// guard.Action's "what is being authorized" minus Verdict bookkeeping, so a
// Rule matches a guard check via reflect.DeepEqual after the caller constructs
// a Scope from the action. Optional fields are populated based on Tool:
// Program/Prefix for shell_run; FSOp/Paths for fs_*; Host for web_fetch.
type Scope struct {
	Tool    string   `json:"tool"`
	Program string   `json:"program,omitempty"`
	Prefix  []string `json:"prefix,omitempty"`
	FSOp    string   `json:"fs_op,omitempty"`
	Paths   []string `json:"paths,omitempty"`
	Host    string   `json:"host,omitempty"`

	// ScriptHash is the SHA-256 of the script a shell command executes, set
	// only when the command runs a script FILE (`sh x.sh`, `./x.sh`,
	// `node x.js`). It is what makes a rule about running a script safe to
	// keep: Program+Prefix alone would admit `sh install.sh` forever, so
	// rewriting install.sh would silently inherit the approval. Because the
	// hash participates in the DeepEqual match, editing the script by one
	// byte produces a different scope and the user is asked again.
	//
	// Empty means "not a script execution, or the file could not be read".
	// The second case deliberately produces a scope that will never be
	// recorded, so an unreadable script is re-asked rather than blanket-
	// approved.
	ScriptHash string `json:"script_hash,omitempty"`
}

// Rule is one recorded permission decision. ID is unique within the scope of a
// session/process and is the handle the UI uses for revoke. Action echoes the
// tool name (kept separate from Scope.Tool because the UI may classify
// actions into friendlier groups like "shell", "network").
type Rule struct {
	ID        string    `json:"id"`
	Action    string    `json:"action"`
	Scope     Scope     `json:"scope"`
	TTL       TTL       `json:"ttl"`
	Source    Source    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	ProcessID string    `json:"process_instance_id,omitempty"`
}

// AuditEvent is the payload Manager emits on every rule lifecycle event. The
// five kinds — miss/hit/consume/expire/revoke — are surfaced to operators via
// the permission_rule_hit WS frame (Task 9) so they can audit exactly which
// rule admitted which call.
type AuditEvent struct {
	Kind      string
	RuleID    string
	SessionID string
	Action    string
	Scope     Scope
	At        time.Time
}

// KV is the small storage interface approval.Manager depends on. *store.Store
// satisfies it; tests use a fake. The keys are stable opaque strings chosen
// by the manager (currently "security.approvals.v1").
type KV interface {
	KVGet(string) (string, bool, error)
	KVSet(string, string) error
}
