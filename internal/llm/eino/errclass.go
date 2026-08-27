package eino

import (
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	goopenai "github.com/meguminnnnnnnnn/go-openai"
)

// This file is the error CLASSIFIER (M8) and the rate-limit cooldown source
// (M1). It replaces the pair of flat substring tables the resilient layer used
// to consult with a three-stage pipeline whose stages are ordered by evidence
// quality:
//
//	1. TYPED   — errors.As onto the provider SDK error types, which carry a
//	             real HTTPStatusCode field. This is exact.
//	2. ANCHORED TEXT — regexes anchored on the phrasings providers actually use
//	             to report a status ("status code: 404", "HTTP 404", "404
//	             Not Found"). Anchoring is the whole point: the old table did a
//	             bare strings.Contains(err, "404") and therefore misclassified
//	             a 500 whose BODY happened to mention a 404 — e.g. an upstream
//	             gateway reporting `HTTP 500: {"error":"backend returned 404
//	             from origin"}`. That error is transient and must retry; the old
//	             code short-circuited it as a client error and gave up.
//	3. KEYWORD — last resort, for families that never carry a status at all
//	             (transport errors, mid-stream EOFs) or whose text is the only
//	             signal ("invalid_api_key").
//
// The classes and what the retry loop does with each:
//
//	ClassTransient      → retry with the ordinary exponential backoff.
//	ClassRateLimit      → retry, but on the RATE-LIMIT schedule (M1): honour
//	                      Retry-After when present, otherwise a much longer
//	                      exponential base. Hammering a 429 every 200ms is how
//	                      a short throttle becomes a long one.
//	ClassClientError    → do NOT retry. The next attempt hits the same config
//	                      bug and the backoff only hides the root cause.
//	ClassContextOverflow→ do NOT retry as-is. Retrying an over-long prompt
//	                      reproduces it verbatim; compaction has to shrink the
//	                      context first (C9/C6).
//	ClassUnknown        → no evidence either way; the caller keeps its previous
//	                      behavior (retry only if the error was explicitly
//	                      wrapped as retryable).

// ErrorClass names how the retry layer should treat a provider error.
type ErrorClass string

// Error classes produced by ClassifyError.
const (
	// ClassUnknown means no rule matched; the caller decides.
	ClassUnknown ErrorClass = "unknown"
	// ClassTransient is a retryable failure: 5xx, timeout, connection reset,
	// mid-stream EOF.
	ClassTransient ErrorClass = "transient"
	// ClassRateLimit is a 429 (or a 503 carrying Retry-After) and must be
	// retried on the cooldown schedule rather than the ordinary backoff.
	ClassRateLimit ErrorClass = "rate_limit"
	// ClassClientError is a non-retryable 4xx: bad key, missing model,
	// malformed request.
	ClassClientError ErrorClass = "client_error"
	// ClassContextOverflow means the prompt exceeded the model's window;
	// retrying unchanged reproduces it, so compaction must run first.
	ClassContextOverflow ErrorClass = "context_overflow"
)

// Classification is the full verdict on one provider error.
type Classification struct {
	// Class is how the retry layer should treat the error.
	Class ErrorClass
	// Status is the HTTP status when one was recovered (typed field or an
	// anchored text match), else 0.
	Status int
	// RetryAfter is the server-requested cooldown parsed from a Retry-After
	// header or from the error text, already clamped to MaxRetryAfter. Zero
	// means the server did not ask for a specific delay.
	RetryAfter time.Duration
}

// MaxRetryAfter caps any server-requested cooldown at 5 minutes.
//
// The cap is a safety bound, not a policy preference: Retry-After is
// attacker-or-bug-controlled input. Anthropic's free-tier limit error has been
// observed asking for ~14 hours, and a misconfigured proxy can emit an absurd
// HTTP-date. Sleeping on that value would wedge a turn for the rest of the
// session with no way for the user to tell whether anything is still running.
// Five minutes is long enough for every genuine per-minute or per-hour bucket
// to have refilled at least partially, and short enough that a bogus value
// costs one visible pause rather than a hang.
const MaxRetryAfter = 5 * time.Minute

// RateLimitBaseDelay is the backoff base used for ClassRateLimit when the
// server sent no Retry-After — 5 seconds against the ordinary 200ms.
//
// The ordinary base exists for network blips, where retrying almost
// immediately is right. A 429 is the opposite situation: the server has said
// it is over capacity, and every early retry both fails and counts against the
// same bucket, so a fast schedule actively lengthens the throttle. Starting at
// 5s means the first four retries span roughly 5/10/20/40s, which covers the
// per-minute buckets most providers use.
const RateLimitBaseDelay = 5 * time.Second

// RateLimitMaxDelay caps the no-Retry-After rate-limit backoff at 2 minutes,
// so the exponential does not run past the point of usefulness while still
// leaving headroom below MaxRetryAfter for an explicitly requested wait.
const RateLimitMaxDelay = 2 * time.Minute

// statusPatterns are the anchored forms in which providers report an HTTP
// status inside an error string. Each has exactly one capture group holding the
// three digits.
//
// Anchoring is what separates this from the old bare-substring table: the digits
// must be introduced by a status-shaped phrase, so a body that merely mentions
// "404" somewhere in prose cannot be mistaken for the status of the response
// that carried it. Ordering is irrelevant — the first match wins and the forms
// are mutually exclusive in practice.
var statusPatterns = []*regexp.Regexp{
	// go-openai / most SDK wrappers: "error, status code: 429, status: ..."
	regexp.MustCompile(`(?i)status code:\s*(\d{3})\b`),
	// our own adapters: "anthropic: API error (HTTP 429): ..."
	regexp.MustCompile(`(?i)\(?http[/ ](?:status\s*)?(\d{3})\)?`),
	// bare reason-phrase forms: "429 Too Many Requests", "503 Service Unavailable"
	regexp.MustCompile(`(?i)\b(\d{3})\s+(?:too many requests|service unavailable|bad gateway|gateway time-?out|not found|unauthorized|forbidden|internal server error)\b`),
	// explicit labels: "status: 500", "statusCode=500", "http_status 500"
	regexp.MustCompile(`(?i)status_?(?:code)?[=:\s]\s*(\d{3})\b`),
}

// retryAfterHeaderPatterns extract a Retry-After value from an error string.
// This is the FALLBACK used when the SDK gave us no headers: our own adapters
// embed the response body in the error text, and several gateways report the
// requested cooldown only in that body. Group 1 is the value.
var retryAfterHeaderPatterns = []*regexp.Regexp{
	// header echoed into the message: "Retry-After: 30" / "retry-after 30"
	regexp.MustCompile(`(?i)retry[-_ ]?after["' :=]+\s*([0-9]+(?:\.[0-9]+)?)`),
	// JSON body field: "retry_after_seconds": 30
	regexp.MustCompile(`(?i)retry_after_seconds["' :=]+\s*([0-9]+(?:\.[0-9]+)?)`),
	// prose: "try again in 30 seconds" / "please retry in 30s"
	regexp.MustCompile(`(?i)(?:try again|retry) in\s+([0-9]+(?:\.[0-9]+)?)\s*(?:s\b|sec|second)`),
}

// contextOverflowMarkers are lowercase substrings that mark a prompt-too-long
// rejection. These stay keyword-based on purpose: providers report the
// condition as a 400 with no distinguishing code, so the text IS the only
// signal, and the phrasings below are specific enough not to collide with
// ordinary prose ("context" alone would, which is why it is not listed).
var contextOverflowMarkers = []string{
	"context_length_exceeded",
	"context length exceeded",
	"maximum context length",
	"maximum context",
	"context window",
	"too many tokens",
	"prompt is too long",
	"reduce the length of the messages",
	"input length and `max_tokens` exceed",
}

// clientErrorMarkers are lowercase substrings that identify a non-retryable
// client error by NAME rather than by status. They cover the families whose
// status is often absent from the wrapped text (a provider returning a bare
// `{"error":{"code":"invalid_api_key"}}` body), and each is a full error code
// or a distinctive phrase — never a bare number.
var clientErrorMarkers = []string{
	"invalid_api_key",
	"incorrect api key",
	"invalid x-api-key",
	"model_not_found",
	"invalid_request_error",
	"authentication_error",
	"permission_error",
	"insufficient_quota",
	"account is not active",
}

// transientMarkers are lowercase substrings that identify a retryable failure
// with no recoverable status: transport-level breakage and the openai acl's
// mid-stream wrapper text.
var transientMarkers = []string{
	"unexpected eof",
	"failed to receive stream chunk",
	"request canceled",
	"connection reset",
	"broken pipe",
	"connection refused",
	"no such host",
	"server returned non-200",
	"overloaded_error",
	"timeout",
	"timed out",
}

// rateLimitMarkers are lowercase substrings that identify a throttle when no
// status was recoverable.
var rateLimitMarkers = []string{
	"rate limit",
	"rate_limit",
	"too many requests",
	"quota exceeded",
	"429",
}

// ClassifyError is the single classification entry point. It returns the class,
// the recovered HTTP status (0 when none), and any server-requested cooldown.
//
// The stages run typed → anchored-status → keyword, and the first stage that
// produces a class wins. A nil error classifies as ClassUnknown.
func ClassifyError(err error) Classification {
	if err == nil {
		return Classification{Class: ClassUnknown}
	}
	text := strings.ToLower(err.Error())
	c := Classification{RetryAfter: retryAfterFromError(err, text)}

	c.Status = statusFromError(err, text)

	// Context overflow is checked before the status branch: it arrives as a
	// 400, which the status branch would file as a plain client error, losing
	// the one distinction that tells the caller "compact, then try again"
	// instead of "this configuration is broken".
	if containsAny(text, contextOverflowMarkers) {
		c.Class = ClassContextOverflow
		return c
	}

	if c.Status > 0 {
		c.Class = classForStatus(c.Status, c.RetryAfter)
		return c
	}

	// No status anywhere: fall back to keywords. Client-error names are checked
	// first because "invalid_request_error" is more specific than any transient
	// marker that might co-occur in the same message.
	switch {
	case containsAny(text, clientErrorMarkers):
		c.Class = ClassClientError
	case containsAny(text, rateLimitMarkers):
		c.Class = ClassRateLimit
	case containsAny(text, transientMarkers), isTransportError(err):
		c.Class = ClassTransient
	default:
		c.Class = ClassUnknown
	}
	return c
}

// classForStatus maps a recovered HTTP status onto a class.
//
// 503 is split by evidence: a plain 503 is an ordinary transient outage worth
// an immediate-ish retry, but a 503 that carries Retry-After is the server
// explicitly scheduling us, which is the rate-limit contract under a different
// code (several gateways report throttling this way). 529 is Anthropic's
// "overloaded" code and behaves like a 503.
func classForStatus(status int, retryAfter time.Duration) ErrorClass {
	switch {
	case status == http.StatusTooManyRequests:
		return ClassRateLimit
	case status == http.StatusServiceUnavailable && retryAfter > 0:
		return ClassRateLimit
	case status == 529: // Anthropic "overloaded_error"
		return ClassTransient
	case status >= 500:
		return ClassTransient
	case status == http.StatusRequestTimeout: // 408
		return ClassTransient
	case status >= 400:
		return ClassClientError
	}
	return ClassUnknown
}

// statusFromError recovers an HTTP status: first from the SDK's typed errors
// (exact), then from an anchored text pattern (approximate but not fooled by a
// status-shaped number appearing in a response body).
//
// text is passed in already lowercased so callers that need it for other stages
// do not lowercase twice.
func statusFromError(err error, text string) int {
	var apiErr *goopenai.APIError
	if errors.As(err, &apiErr) && isHTTPStatus(apiErr.HTTPStatusCode) {
		return apiErr.HTTPStatusCode
	}
	var reqErr *goopenai.RequestError
	if errors.As(err, &reqErr) && isHTTPStatus(reqErr.HTTPStatusCode) {
		return reqErr.HTTPStatusCode
	}
	for _, re := range statusPatterns {
		if m := re.FindStringSubmatch(text); m != nil {
			if n, convErr := strconv.Atoi(m[1]); convErr == nil && isHTTPStatus(n) {
				return n
			}
		}
	}
	return 0
}

// isHTTPStatus reports whether n is inside the valid HTTP status range, so a
// stray three-digit number captured by a pattern cannot become a status.
func isHTTPStatus(n int) bool { return n >= 100 && n <= 599 }

// retryAfterFromError extracts the server-requested cooldown, clamped to
// MaxRetryAfter.
//
// The header is the authoritative source, so it is tried first via the SDK's
// typed error (go-openai's APIError does not surface headers, but our own
// adapters wrap a HeaderError that does — see RetryAfterFromHeader). When no
// headers are reachable the text fallback runs, because our adapters embed the
// response body in the error string and several gateways report the cooldown
// only there. QwenPaw uses the same header-then-text ladder for the same
// reason.
func retryAfterFromError(err error, text string) time.Duration {
	var he *HeaderError
	if errors.As(err, &he) && he.Header != nil {
		if d, ok := RetryAfterFromHeader(he.Header, time.Now()); ok {
			return clampRetryAfter(d)
		}
	}
	for _, re := range retryAfterHeaderPatterns {
		m := re.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		secs, convErr := strconv.ParseFloat(m[1], 64)
		if convErr != nil || secs < 0 {
			continue
		}
		return clampRetryAfter(time.Duration(secs * float64(time.Second)))
	}
	return 0
}

// clampRetryAfter bounds a requested cooldown to [0, MaxRetryAfter]. See
// MaxRetryAfter for why the ceiling is not optional.
func clampRetryAfter(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d > MaxRetryAfter {
		return MaxRetryAfter
	}
	return d
}

// RetryAfterFromHeader parses the Retry-After header per RFC 9110 §10.2.3,
// which defines TWO forms and requires both to be accepted:
//
//	Retry-After: 120                                (delta-seconds)
//	Retry-After: Wed, 21 Oct 2026 07:28:00 GMT      (HTTP-date)
//
// Real providers use both — OpenAI sends delta-seconds, several CDN-fronted
// gateways send the date form — so parsing only the integer would silently
// drop half the signal and fall back to a blind exponential.
//
// now is injected rather than read from the clock so the date branch is
// deterministically testable. A date in the past yields (0, true): the server
// named a moment that has already arrived, which is a real answer meaning "no
// wait", not a parse failure. The returned duration is NOT clamped — clamping
// belongs to the caller (clampRetryAfter) so this function stays a faithful
// parser.
func RetryAfterFromHeader(h http.Header, now time.Time) (time.Duration, bool) {
	if h == nil {
		return 0, false
	}
	raw := strings.TrimSpace(h.Get("Retry-After"))
	if raw == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(raw, 64); err == nil {
		if secs < 0 {
			return 0, true
		}
		return time.Duration(secs * float64(time.Second)), true
	}
	if t, err := http.ParseTime(raw); err == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// HeaderError attaches the response headers of a failed request to the error,
// so the retry layer can read Retry-After from the authoritative source rather
// than scraping it out of a message.
//
// It exists because neither eino nor go-openai surfaces response headers on a
// failed call: go-openai's APIError carries HTTPStatusCode but drops the header
// map, and the eino acl wraps that error in fmt.Errorf. Our own adapters
// (anthropic.go, responses.go) hold the *http.Response themselves, so they can
// and do wrap their non-200 errors in this type — which is why the header path
// works for them and the text fallback exists for everything else.
type HeaderError struct {
	// StatusCode is the HTTP status of the failed response.
	StatusCode int
	// Header is the response header map; may be nil.
	Header http.Header
	// Err is the underlying error carrying the message and body.
	Err error
}

// Error returns the wrapped error message.
func (e *HeaderError) Error() string { return e.Err.Error() }

// Unwrap returns the wrapped error for errors.Is/As support.
func (e *HeaderError) Unwrap() error { return e.Err }

// NewHeaderError wraps err with the status and headers of resp. A nil resp
// leaves the error unwrapped so callers can pass a response they may not have.
func NewHeaderError(resp *http.Response, err error) error {
	if err == nil {
		return nil
	}
	if resp == nil {
		return err
	}
	return &HeaderError{StatusCode: resp.StatusCode, Header: resp.Header, Err: err}
}

// isTransportError reports whether err is a transport-level failure with no
// HTTP semantics at all — a net.Error, or a truncated read. These never carry a
// status, so they are recognised by type rather than by text.
func isTransportError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF)
}

// containsAny reports whether s contains any of the (lowercase) markers. s must
// already be lowercased.
func containsAny(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// RateLimitBackoff returns how long to wait before retrying a rate-limited
// call on the DEFAULT schedule. attempt is 1-based. It is
// RateLimitBackoffWith bound to RateLimitBaseDelay / RateLimitMaxDelay.
func RateLimitBackoff(attempt int, retryAfter time.Duration) time.Duration {
	return RateLimitBackoffWith(attempt, retryAfter, RateLimitBaseDelay, RateLimitMaxDelay)
}

// RateLimitBackoffWith returns how long to wait before retrying a rate-limited
// call, on a caller-supplied schedule. attempt is 1-based; non-positive base or
// max fall back to the package defaults.
//
// A server-requested cooldown wins outright: the provider knows when its bucket
// refills and guessing shorter just burns another request against the same
// limit. It is still clamped to MaxRetryAfter, which is a safety bound on
// untrusted input rather than a schedule choice, so `max` does not apply to it
// — clamping a server's explicit "wait 30s" down to a local ceiling would
// reintroduce the hammering this function exists to stop.
//
// Absent a cooldown, the delay is base * 2^(attempt-1), capped at max.
func RateLimitBackoffWith(attempt int, retryAfter, base, max time.Duration) time.Duration {
	if retryAfter > 0 {
		return clampRetryAfter(retryAfter)
	}
	if base <= 0 {
		base = RateLimitBaseDelay
	}
	if max <= 0 {
		max = RateLimitMaxDelay
	}
	if max < base {
		max = base
	}
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= max || d <= 0 { // d <= 0 guards the doubling overflowing
			return max
		}
	}
	return d
}

// IsRetryableClass reports whether a class should be retried at all.
// ClassUnknown returns false: absence of evidence is not evidence of
// transience, and the caller still has its own explicit RetryableModelError
// wrapper to fall back on.
func IsRetryableClass(c ErrorClass) bool {
	return c == ClassTransient || c == ClassRateLimit
}
