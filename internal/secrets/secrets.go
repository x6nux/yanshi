// Package secrets provides secure credential storage and a unified redactor
// applied at every output boundary (stderr logs, WS frames, SSE frames, SQLite
// writes). The Redactor is the single source of truth for what counts as a
// secret in yanshi's runtime: every secret string is registered once after
// config load, and every boundary calls Redact on the rendered text.
//
// Design decision: SafeError preserves the raw cause via Unwrap so
// errors.Is/As chains stay intact (orchestrator code matches sentinel errors
// by identity). The trade-off is that a caller who re-stringifies the
// unwrapped cause re-leaks the secret — therefore SafeError is only used at
// boundaries that render Error() once and forward the resulting text, never
// as a generic error wrapper inside business logic.
package secrets

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ErrKeyringUnavailable is returned by the OS keyring adapter when no platform
// backend is wired (CGO disabled, no D-Bus on Linux, etc). The Manager treats
// this as a soft-degrade trigger, not a fatal error.
var ErrKeyringUnavailable = errors.New("secrets: OS keyring unavailable")

// ErrSecretNotFound is distinct from ErrKeyringUnavailable: a missing entry
// proves the backend answered and is therefore available. Manager auto mode
// must not misclassify a normal miss as an unavailable keyring.
var ErrSecretNotFound = errors.New("secrets: secret not found")

// Redactor is a concurrency-safe registry of secret substrings. Register each
// secret exactly once after resolution; every output boundary then calls
// Redact on the rendered text. Replacements are stable strings so log grep
// still groups by "which secret leaked".
type Redactor struct {
	mu      sync.RWMutex
	secrets []string
}

// NewRedactor returns an empty Redactor.
func NewRedactor() *Redactor { return &Redactor{} }

// MinSecretLength is the shortest string Register will accept. Values below
// it are dropped.
//
// This trades a theoretical leak for a certain corruption, deliberately.
// Redact does substring replacement, so registering a 3-character value turns
// every incidental occurrence of those 3 characters into "[REDACTED]" —
// across stderr, WS frames, SSE frames, AND SQLite writes. The Store redacts
// before the SQL write, so that damage is written to disk and is not
// recoverable by fixing the config afterwards. A misconfigured env:// ref
// resolving to a one-character value is enough to trigger it.
//
// The leak side of the trade is close to nothing: a credential this short has
// no meaningful entropy, so an attacker who can see the logs did not need
// them. Callers holding a real credential should still notice the drop —
// bootstrap warns when a resolved provider key falls below this bar.
const MinSecretLength = 6

// Register adds a secret substring to the registry. Values shorter than
// MinSecretLength (including the empty string) are ignored; see that
// constant for why dropping is the safer direction. Re-registration is
// idempotent.
func (r *Redactor) Register(secret string) {
	if len(secret) < MinSecretLength {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.secrets {
		if s == secret {
			return
		}
	}
	r.secrets = append(r.secrets, secret)
	sort.Slice(r.secrets, func(i, j int) bool {
		return len(r.secrets[i]) > len(r.secrets[j])
	})
}

// Redact replaces every registered secret substring in s with "[REDACTED]".
// Concurrent-safe. Returns s unchanged if no secrets registered.
func (r *Redactor) Redact(s string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.secrets) == 0 {
		return s
	}
	out := s
	for _, secret := range r.secrets {
		out = strings.ReplaceAll(out, secret, "[REDACTED]")
	}
	return out
}

// SafeError wraps err so that Error() is redacted but Unwrap() returns the
// raw cause (preserving errors.Is/As). See package doc for the re-stringify
// risk. SafeError(nil) returns nil.
func (r *Redactor) SafeError(err error) error {
	if err == nil {
		return nil
	}
	return &safeError{cause: err, text: r.Redact(err.Error())}
}

type safeError struct {
	cause error
	text  string
}

func (e *safeError) Error() string { return e.text }
func (e *safeError) Unwrap() error { return e.cause }

// RedactJSON redacts both plain and JSON-escaped spellings of each secret.
// This is required at WS/SSE boundaries: json.Marshal escapes quotes,
// backslashes and control characters, so replacing only the raw spelling
// would miss a key such as `abc"def` after marshal.
func (r *Redactor) RedactJSON(data []byte) []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := string(data)
	for _, secret := range r.secrets {
		quoted := strconv.Quote(secret)
		escaped := quoted[1 : len(quoted)-1]
		out = strings.ReplaceAll(out, secret, "[REDACTED]")
		out = strings.ReplaceAll(out, escaped, "[REDACTED]")
	}
	return []byte(out)
}

// SafeLogger is the only stderr/log sink introduced by D3. It formats first,
// then redacts once before writing. New D3 code must not call fmt.Fprintf on
// os.Stderr directly; bootstrap constructs one process-wide SafeLogger and
// passes it to secrets/auth/device builders.
type SafeLogger struct {
	out      io.Writer
	redactor *Redactor
}

// NewSafeLogger creates a SafeLogger that redacts secrets from log output.
func NewSafeLogger(out io.Writer, r *Redactor) *SafeLogger {
	return &SafeLogger{out: out, redactor: r}
}

// Printf formats and writes a redacted log line.
func (l *SafeLogger) Printf(format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	if l.redactor != nil {
		text = l.redactor.Redact(text)
	}
	_, _ = io.WriteString(l.out, text)
}

// Println writes a redacted log line with a trailing newline.
func (l *SafeLogger) Println(args ...any) {
	l.Printf("%s", fmt.Sprintln(args...))
}

// Absorb registers every secret held by others into r, in place. Nil entries
// are skipped. Used by bootstrap to fold subsystem-level redactors (the
// secrets Manager's, device-auth tokens) into the one process-wide registry.
//
// In place is the whole point, and it replaced a MergeRedactors free function
// that returned a fresh union. That shape was an alias trap: SafeOutput pairs
// a Redactor with a SafeLogger built over THAT pointer, so as soon as
// bootstrap rebound its local `redactor` to the union, every subsequent
// Register(resolvedAPIKey) landed on an object the logger had never heard of
// — SafeLogger emitted provider API keys in plaintext for its entire life,
// while the doc on SafeOutput claimed the two shared a registry. The union
// was correct in isolation and wrong at the seam; returning a new object is
// the bug, so the API no longer offers one.
//
// TestSafeOutputRegistryIsAnAliasNotACopy pins this.
func (r *Redactor) Absorb(others ...*Redactor) {
	for _, other := range others {
		if other == nil || other == r {
			continue
		}
		other.mu.RLock()
		snapshot := make([]string, len(other.secrets))
		copy(snapshot, other.secrets)
		other.mu.RUnlock()
		// Register takes r.mu, so it must be called outside other's lock and
		// outside any lock on r.
		for _, s := range snapshot {
			r.Register(s)
		}
	}
}

// SafeOutput bundles a Redactor and its SafeLogger so callers (bootstrap,
// cli.Session) pass one pointer around instead of two. A nil SafeOutput is
// legal and means "use the defaults"; effectiveSafeOutput in bootstrap
// materializes one process-wide. The Redactor and Logger share the same
// secret registry: registering a secret on Redactor makes SafeLogger redact
// it on the next Printf/Println.
type SafeOutput struct {
	Redactor *Redactor
	Logger   *SafeLogger
}

// NewSafeOutput returns a SafeOutput whose Redactor and Logger share the
// same registry. Pass nil for r to get a fresh empty Redactor; pass nil for
// out to get a logger that writes to io.Discard (tests that don't care
// about warnings). One SafeOutput per process is the intended usage.
func NewSafeOutput(out io.Writer, r *Redactor) *SafeOutput {
	if r == nil {
		r = NewRedactor()
	}
	if out == nil {
		out = io.Discard
	}
	return &SafeOutput{Redactor: r, Logger: NewSafeLogger(out, r)}
}
