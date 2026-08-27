package secrets

import (
	"regexp"
	"strings"
)

// patternredact.go adds SHAPE-BASED redaction to the Redactor, alongside the
// registry of secrets somebody explicitly called Register on.
//
// WHY BOTH, AND WHY THE REGISTRY ALONE WAS NOT ENOUGH.
//
// The registry answers "did yanshi resolve this credential itself" — provider
// API keys, OAuth tokens, keyring entries. It is exact, it is zero-false-
// positive, and it is blind to the entire second population of credentials:
// the ones the AGENT encounters at runtime. Those are never registered, because
// nothing in this process ever resolved them.
//
// Measured, not reasoned. Running a real bootstrap App and authorizing
//
//	curl -H 'Authorization: Bearer sk-proj-A1b2C3…S9t0' https://example.com
//
// put this row in SQLite:
//
//	digest="shell: curl -H 'Authorization: Bearer sk-proj-A1b2C3…S9t0' https://…"
//
// verbatim. The permission_audit table's own doc comment says the digest is
// redacted "because tool arguments carry API keys, tokens and connection
// strings often enough that 'the audit table' and 'the credential dump' would
// otherwise be the same table" — and for every key the agent handles rather
// than yanshi resolves, they were the same table.
//
// The same Redact call is the redaction step for three sinks with three
// different lifetimes, so the gap was the same size in all three:
//
//   - store.AppendPermissionAudit / AppendMessage — writes to disk, permanent,
//     and NOT recoverable by fixing anything later.
//   - observe/log.CrashDumper — the report an operator mails to a maintainer.
//   - ctxcompact.redactForSummary — text sent to the summary model, which then
//     becomes a PINNED message and is re-sent to the provider on every
//     subsequent turn, forever.
//
// WHY THIS IS CONSERVATIVE BY CONSTRUCTION. Redact does substring replacement
// on text that is then written to SQLite, so a false positive is corruption
// that survives the fix — the same trade MinSecretLength documents, and the
// reason that constant exists at all. So the patterns here are not "high
// entropy string" heuristics. Every one requires a VENDOR-SPECIFIC PREFIX plus
// a minimum body length, which is the property that makes them unambiguous:
// "sk-" alone appears inside ordinary English ("task-force"), while "sk-"
// followed by twenty base62 characters does not appear by accident in prose,
// code or paths. A credential shape that needs a general entropy test to
// recognise is deliberately left to the registry.
//
// The prefix list is intentionally the same population that
// observe/log.sensitiveValuePrefixes already recognises for slog attributes.
// That table proves the shapes are worth catching; it just could not help here,
// because it inspects structured log ATTRIBUTES and these three sinks pass
// free-form strings through a different path entirely.

// credentialPatterns are the token shapes redacted regardless of registration.
//
// Each entry is anchored on a vendor prefix and a minimum body length. The
// bodies use explicit character classes rather than \S+ so a token at the end
// of a sentence does not swallow the punctuation after it — replacing
// "sk-abc…xyz." including the period would corrupt the surrounding text, and
// corrupting text is the failure mode this file is most obliged to avoid.
var credentialPatterns = []*regexp.Regexp{
	// OpenAI / Anthropic and the many providers that copied the spelling.
	// Covers sk-, sk-proj-, sk-ant-, sk-or-v1-: the body class admits the
	// internal hyphens, so one pattern serves all of them.
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),

	// GitHub: classic PAT, OAuth, user-to-server, server-to-server, refresh,
	// and fine-grained. The fine-grained form is much longer but shares the
	// underscore-delimited shape.
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),

	// GitLab personal / project / group tokens.
	regexp.MustCompile(`glpat-[A-Za-z0-9_-]{16,}`),

	// Slack bot / user / app / refresh tokens.
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),

	// AWS access key ids. Fixed 20-character length, so this one can be exact.
	// The secret ACCESS KEY has no prefix and is plain base64 — deliberately
	// NOT matched here, because nothing distinguishes it from an ordinary
	// base64 blob and a pattern for it would redact checksums and git hashes.
	regexp.MustCompile(`(?:AKIA|ASIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA|ABIA|ACCA)[A-Z0-9]{16}`),

	// Google API keys: fixed prefix, fixed length.
	regexp.MustCompile(`AIza[A-Za-z0-9_-]{35}`),

	// Stripe live/test secret and restricted keys.
	regexp.MustCompile(`[rs]k_(?:live|test)_[A-Za-z0-9]{16,}`),

	// npm automation tokens.
	regexp.MustCompile(`npm_[A-Za-z0-9]{30,}`),

	// JSON Web Tokens. Three base64url segments, and the leading "eyJ" is the
	// base64 of `{"` — a JWT header always starts there. The minimum segment
	// lengths keep it off short dotted identifiers.
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),

	// PEM private key blocks: the whole body, not just the header. A report
	// that redacted the BEGIN line and printed the key underneath it would be
	// worse than one that redacted nothing, because it would look handled.
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),

	// Inline credentials in a URL: scheme://user:password@host. The password
	// is the target; the rest of the URL is left intact so the log still names
	// which host was contacted. DATABASE_URL is the everyday case.
	regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]+):[^\s/@]+@`),
}

// patternPrefixes are cheap substring probes, one per pattern family, used to
// skip the regex scan entirely for text that cannot possibly match.
//
// Redact runs on every message written to SQLite and on every crash-report
// field, so the common case — ordinary prose and source code — must not pay for
// eleven regex traversals. A miss here would silently disable the whole file,
// so the entries are prefixes of the patterns above rather than an independent
// list, and TestPatternPrefixesCoverEveryPattern checks the correspondence by
// running each pattern's own sample through the probe.
var patternPrefixes = []string{
	"sk-", "gh", "glpat-", "xox", "AKIA", "ASIA", "AGPA", "AIDA", "AROA",
	"AIPA", "ANPA", "ANVA", "ABIA", "ACCA", "AIza", "k_live_", "k_test_",
	"npm_", "eyJ", "-----BEGIN", "://",
}

// mayContainCredentialPattern reports whether s is worth scanning.
func mayContainCredentialPattern(s string) bool {
	for _, p := range patternPrefixes {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// RedactPatterns replaces vendor-shaped credential tokens in s with the same
// "[REDACTED]" marker Redact uses for registered secrets.
//
// It is exported so a caller that handles text OUTSIDE a Redactor's lifetime
// can apply the same rules, and so the shapes can be exercised directly. Redact
// calls it on every input, so ordinary callers need not.
//
// The inline-URL-credential pattern keeps its scheme-and-user prefix
// (postgres://user:[REDACTED]@db/app) because the host is the diagnostic value
// of the line and the password is the only part that is a secret.
func RedactPatterns(s string) string {
	if s == "" || !mayContainCredentialPattern(s) {
		return s
	}
	out := s
	for i, re := range credentialPatterns {
		// The last pattern captures the part to KEEP; every other pattern
		// matches only the secret itself.
		if i == len(credentialPatterns)-1 {
			out = re.ReplaceAllString(out, "$1:[REDACTED]@")
			continue
		}
		out = re.ReplaceAllString(out, redactedMarker)
	}
	return out
}

// redactedMarker is the single replacement string, shared with Redact so a
// consumer grepping logs for leaks has one token to look for.
const redactedMarker = "[REDACTED]"
