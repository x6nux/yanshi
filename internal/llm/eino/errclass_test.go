package eino

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	goopenai "github.com/meguminnnnnnnnn/go-openai"
)

// TestClassifyError is the M8 table. Every row states the error text or type a
// provider actually produces and the class the retry layer must derive from it.
func TestClassifyError(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		class  ErrorClass
		status int
	}{
		// --- the regression this classifier exists for ----------------------
		{
			// THE bug. The old table did strings.Contains(err, "404") and filed
			// this as a non-retryable client error, so an upstream gateway
			// outage that merely QUOTED a 404 from its own origin gave up
			// without one retry. The status is 500 and the class must be
			// transient; the "404" in the body must not participate.
			name:   "500 whose body mentions 404 is transient, not a client error",
			err:    errors.New(`anthropic: API error (HTTP 500): {"error":"backend returned 404 from origin"}`),
			class:  ClassTransient,
			status: 500,
		},
		{
			name:   "502 whose body quotes a 401 from upstream is still transient",
			err:    fmt.Errorf(`eino/responses: API error (HTTP 502): {"detail":"origin replied 401 unauthorized"}`),
			class:  ClassTransient,
			status: 502,
		},
		{
			// A prose mention with no status-shaped anchor anywhere must not
			// invent a status at all.
			name:   "a bare number in prose is not a status",
			err:    errors.New("the file at line 404 of the transcript could not be parsed"),
			class:  ClassUnknown,
			status: 0,
		},

		// --- typed extraction (exact) ---------------------------------------
		{
			name:   "typed APIError 429",
			err:    &goopenai.APIError{HTTPStatusCode: 429, Message: "slow down"},
			class:  ClassRateLimit,
			status: 429,
		},
		{
			name:   "typed APIError 401",
			err:    &goopenai.APIError{HTTPStatusCode: 401, Message: "bad key"},
			class:  ClassClientError,
			status: 401,
		},
		{
			name:   "typed APIError 503",
			err:    &goopenai.APIError{HTTPStatusCode: 503, Message: "unavailable"},
			class:  ClassTransient,
			status: 503,
		},
		{
			name:   "typed RequestError 500",
			err:    &goopenai.RequestError{HTTPStatusCode: 500, Err: errors.New("boom")},
			class:  ClassTransient,
			status: 500,
		},
		{
			// A typed error wrapped by the eino acl's fmt.Errorf must still be
			// recovered — this is the shape production actually sees.
			name:   "typed error wrapped by the acl still yields its status",
			err:    fmt.Errorf("failed to create chat completion: %w", &goopenai.APIError{HTTPStatusCode: 429, Message: "rl"}),
			class:  ClassRateLimit,
			status: 429,
		},

		// --- anchored status patterns ---------------------------------------
		{
			name:   "go-openai style status phrase",
			err:    errors.New("error, status code: 404, status: 404 Not Found, message: no such model"),
			class:  ClassClientError,
			status: 404,
		},
		{
			name:   "our adapter's HTTP-in-parens form",
			err:    errors.New("anthropic: API error (HTTP 429): rate_limit_error"),
			class:  ClassRateLimit,
			status: 429,
		},
		{
			name:   "bare reason phrase",
			err:    errors.New("503 Service Unavailable"),
			class:  ClassTransient,
			status: 503,
		},
		{
			name:   "408 request timeout is transient",
			err:    errors.New("status code: 408, status: 408 Request Timeout"),
			class:  ClassTransient,
			status: 408,
		},
		{
			name:   "529 anthropic overloaded is transient",
			err:    errors.New("anthropic: API error (HTTP 529): overloaded_error"),
			class:  ClassTransient,
			status: 529,
		},
		{
			name:   "422 is a client error",
			err:    errors.New("status code: 422, status: 422 Unprocessable Entity"),
			class:  ClassClientError,
			status: 422,
		},

		// --- keyword fallback -----------------------------------------------
		{
			name:  "invalid_api_key with no status",
			err:   errors.New(`{"error":{"code":"invalid_api_key"}}`),
			class: ClassClientError,
		},
		{
			name:  "model_not_found with no status",
			err:   errors.New(`{"error":{"code":"model_not_found"}}`),
			class: ClassClientError,
		},
		{
			name:  "mid-stream EOF from the openai acl",
			err:   errors.New("failed to receive stream chunk: unexpected EOF"),
			class: ClassTransient,
		},
		{
			name:  "connection reset",
			err:   errors.New("read tcp 10.0.0.1:443: connection reset by peer"),
			class: ClassTransient,
		},
		{
			name:  "rate limit named without a status",
			err:   errors.New("you have exceeded your rate limit, please slow down"),
			class: ClassRateLimit,
		},
		{
			name:  "transport error recognised by type",
			err:   &classNetErr{},
			class: ClassTransient,
		},
		{
			name:  "truncated read recognised by type",
			err:   fmt.Errorf("reading body: %w", io.ErrUnexpectedEOF),
			class: ClassTransient,
		},

		// --- context overflow ------------------------------------------------
		{
			// Arrives as a 400. Without the overflow check running FIRST it
			// would be filed as a plain client error and the caller would lose
			// the "compact, then retry" signal.
			name:   "context_length_exceeded is overflow, not a plain client error",
			err:    errors.New(`status code: 400, message: This model's maximum context length is 128000 tokens (context_length_exceeded)`),
			class:  ClassContextOverflow,
			status: 400,
		},
		{
			name:   "anthropic prompt-too-long phrasing",
			err:    errors.New("anthropic: API error (HTTP 400): prompt is too long: 250000 tokens > 200000 maximum"),
			class:  ClassContextOverflow,
			status: 400,
		},

		// --- content safety (W-C-13) ------------------------------------------
		{
			// OpenAI's documented APIError.Code for this condition, embedded in
			// the message text an adapter would produce (see error.go's Code
			// field in the vendored go-openai client this package imports —
			// APIError.Error() never prints Code itself, only Message, so a
			// real classifier can only see this if it is echoed into the text,
			// exactly as OpenAI's own Message field does).
			name:   "OpenAI content_policy_violation code",
			err:    errors.New("error, status code: 400, status: 400 Bad Request, message: content_policy_violation: the prompt was rejected"),
			class:  ClassContentSafety,
			status: 400,
		},
		{
			// OpenAI's documented message text for the same rejection.
			name:   "OpenAI safety-system rejection message",
			err:    errors.New("error, status code: 400, status: 400 Bad Request, message: Your request was rejected as a result of our safety system."),
			class:  ClassContentSafety,
			status: 400,
		},
		{
			// Azure OpenAI's documented content-filter message. No status
			// pattern this phrasing anchors on, so this also proves the
			// keyword path (no c.Status > 0) reaches ClassContentSafety.
			name:  "Azure content management policy filter",
			err:   errors.New("The response was filtered due to the prompt triggering Azure OpenAI's content management policy. Please modify your prompt and retry."),
			class: ClassContentSafety,
		},
		{
			// Azure OpenAI's documented InnerError.Code for the same condition
			// (error.go's InnerError.Code field), as it appears once echoed
			// into the outer message text.
			name:   "Azure ResponsibleAIPolicyViolation inner code",
			err:    errors.New(`status code: 400, message: {"innererror":{"code":"ResponsibleAIPolicyViolation"}}`),
			class:  ClassContentSafety,
			status: 400,
		},
		{
			// The exact ambiguity the spec calls out by name: an unrelated 502
			// (gateway trouble) is ClassTransient (see the "500 whose body
			// mentions 404" case above for the same shape at a different
			// status), but a 502 whose body is a gateway FRONTING a content
			// filter must still resolve to ClassContentSafety — status alone
			// cannot tell these two 502s apart, only the marker can.
			name:   "502 fronting a content filter is content_safety, not transient",
			err:    errors.New(`gateway error (HTTP 502): upstream rejected: content_policy_violation`),
			class:  ClassContentSafety,
			status: 502,
		},

		// --- degenerate -------------------------------------------------------
		{name: "nil error", err: nil, class: ClassUnknown},
		{name: "unrecognised error", err: errors.New("something went sideways"), class: ClassUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyError(tc.err)
			assert.Equal(t, tc.class, got.Class, "class")
			assert.Equal(t, tc.status, got.Status, "status")
		})
	}
}

// classNetErr is a minimal net.Error fake so the transport branch is exercised
// by TYPE rather than by its message text.
type classNetErr struct{}

func (e *classNetErr) Error() string   { return "dial failed" }
func (e *classNetErr) Timeout() bool   { return true }
func (e *classNetErr) Temporary() bool { return true }

var _ net.Error = (*classNetErr)(nil)

// TestRetryAfterFromHeader covers BOTH forms RFC 9110 §10.2.3 defines. Parsing
// only delta-seconds would silently drop every date-form header (several
// CDN-fronted gateways send only that), leaving those 429s on a blind
// exponential.
func TestRetryAfterFromHeader(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		value string
		want  time.Duration
		ok    bool
	}{
		{"delta-seconds", "30", 30 * time.Second, true},
		{"delta-seconds zero", "0", 0, true},
		{"delta-seconds fractional", "1.5", 1500 * time.Millisecond, true},
		{"delta-seconds with surrounding space", "  45  ", 45 * time.Second, true},
		{"negative delta-seconds clamps to zero", "-10", 0, true},
		{"http-date in the future", "Sat, 08 Aug 2026 12:02:00 GMT", 2 * time.Minute, true},
		{"http-date in the past yields zero, not a parse failure", "Sat, 08 Aug 2026 11:00:00 GMT", 0, true},
		{"http-date rfc850 form", "Saturday, 08-Aug-26 12:01:00 GMT", time.Minute, true},
		{"http-date ansic form", "Sat Aug  8 12:00:30 2026", 30 * time.Second, true},
		{"unparsable value", "soon please", 0, false},
		{"empty value", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.value != "" {
				h.Set("Retry-After", tc.value)
			}
			got, ok := RetryAfterFromHeader(h, now)
			assert.Equal(t, tc.ok, ok)
			if tc.ok {
				assert.Equal(t, tc.want, got)
			}
		})
	}

	t.Run("nil header", func(t *testing.T) {
		_, ok := RetryAfterFromHeader(nil, now)
		assert.False(t, ok)
	})
	t.Run("absent header", func(t *testing.T) {
		_, ok := RetryAfterFromHeader(http.Header{}, now)
		assert.False(t, ok)
	})
}

// TestClassifyError_RetryAfterFromHeaderError proves the authoritative path:
// when the adapter wrapped the response in a HeaderError, the cooldown comes
// from the real header rather than from text scraping.
func TestClassifyError_RetryAfterFromHeaderError(t *testing.T) {
	resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"42"}}}
	err := NewHeaderError(resp, errors.New("anthropic: API error (HTTP 429): rate_limit_error"))

	got := ClassifyError(err)
	assert.Equal(t, ClassRateLimit, got.Class)
	assert.Equal(t, 429, got.Status)
	assert.Equal(t, 42*time.Second, got.RetryAfter)
}

// TestClassifyError_RetryAfterTextFallback proves the fallback QwenPaw also
// carries: when no headers survived the wrapping, the cooldown is recovered
// from the message body our adapters embed.
func TestClassifyError_RetryAfterTextFallback(t *testing.T) {
	cases := []struct {
		name string
		text string
		want time.Duration
	}{
		{"header echoed in the body", `API error (HTTP 429): {"retry-after": 30}`, 30 * time.Second},
		{"header with colon-space", "status code: 429, Retry-After: 12", 12 * time.Second},
		{"json retry_after_seconds field", `429 Too Many Requests {"retry_after_seconds": 90}`, 90 * time.Second},
		{"prose form", "429 Too Many Requests: please retry in 20 seconds", 20 * time.Second},
		{"no cooldown named", "429 Too Many Requests", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyError(errors.New(tc.text))
			assert.Equal(t, ClassRateLimit, got.Class)
			assert.Equal(t, tc.want, got.RetryAfter)
		})
	}
}

// TestRetryAfterIsClamped pins MaxRetryAfter. Anthropic's free-tier limit error
// has been seen asking for ~14 hours; honouring that would wedge the turn for
// the rest of the session with nothing on screen to explain it.
func TestRetryAfterIsClamped(t *testing.T) {
	resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"51496"}}}
	err := NewHeaderError(resp, errors.New("API error (HTTP 429): free usage limit"))

	got := ClassifyError(err)
	assert.Equal(t, MaxRetryAfter, got.RetryAfter, "an absurd cooldown must be clamped, not honoured")

	// The text fallback is clamped by the same ceiling.
	got = ClassifyError(errors.New("429 Too Many Requests, retry-after: 99999"))
	assert.Equal(t, MaxRetryAfter, got.RetryAfter)
}

// TestClassifyError_503WithRetryAfterIsRateLimit pins the split: a plain 503 is
// an ordinary outage worth a fast retry, but a 503 CARRYING Retry-After is the
// server scheduling us, which is the rate-limit contract under another code.
func TestClassifyError_503WithRetryAfterIsRateLimit(t *testing.T) {
	plain := ClassifyError(errors.New("anthropic: API error (HTTP 503): upstream unavailable"))
	assert.Equal(t, ClassTransient, plain.Class)

	resp := &http.Response{StatusCode: 503, Header: http.Header{"Retry-After": []string{"60"}}}
	scheduled := ClassifyError(NewHeaderError(resp, errors.New("API error (HTTP 503): backend busy")))
	assert.Equal(t, ClassRateLimit, scheduled.Class)
	assert.Equal(t, 60*time.Second, scheduled.RetryAfter)
}

// TestRateLimitBackoff covers M1's schedule: an explicit cooldown wins, and the
// no-header path starts far above the ordinary transient base so ten retries do
// not all land inside the same throttle bucket.
func TestRateLimitBackoff(t *testing.T) {
	cases := []struct {
		name       string
		attempt    int
		retryAfter time.Duration
		want       time.Duration
	}{
		{"server cooldown wins over the exponential", 1, 30 * time.Second, 30 * time.Second},
		{"server cooldown wins at a late attempt too", 6, 3 * time.Second, 3 * time.Second},
		{"absurd server cooldown is clamped", 1, 14 * time.Hour, MaxRetryAfter},
		{"no cooldown: first retry", 1, 0, RateLimitBaseDelay},
		{"no cooldown: second retry doubles", 2, 0, 2 * RateLimitBaseDelay},
		{"no cooldown: third retry doubles again", 3, 0, 4 * RateLimitBaseDelay},
		{"no cooldown: capped", 20, 0, RateLimitMaxDelay},
		{"attempt zero is treated as the first", 0, 0, RateLimitBaseDelay},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, RateLimitBackoff(tc.attempt, tc.retryAfter))
		})
	}
}

// TestRateLimitBaseIsMuchSlowerThanTransient pins the RELATIONSHIP rather than
// the constants: the whole M1 argument is that a 429 must not be retried on the
// blip schedule. If someone lowers RateLimitBaseDelay to "make retries feel
// snappier", this fails and points at that argument.
func TestRateLimitBaseIsMuchSlowerThanTransient(t *testing.T) {
	const ordinaryBase = 200 * time.Millisecond // NewResilientModel's default
	assert.GreaterOrEqual(t, RateLimitBaseDelay, 10*ordinaryBase,
		"a 429 retried on the transient schedule lengthens the throttle it is waiting out")
	assert.Greater(t, RateLimitMaxDelay, RateLimitBaseDelay)
	assert.Greater(t, MaxRetryAfter, RateLimitMaxDelay,
		"an explicitly requested wait must be allowed to exceed the blind exponential's ceiling")
}

// TestIsRetryableClass pins the retry decision per class, notably that
// ClassUnknown is NOT retryable on its own — absence of evidence is not
// evidence of transience, and the caller still has its explicit
// RetryableModelError wrapper.
func TestIsRetryableClass(t *testing.T) {
	cases := []struct {
		class ErrorClass
		want  bool
	}{
		{ClassTransient, true},
		{ClassRateLimit, true},
		{ClassClientError, false},
		{ClassContextOverflow, false},
		{ClassContentSafety, false},
		{ClassUnknown, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.class), func(t *testing.T) {
			assert.Equal(t, tc.want, IsRetryableClass(tc.class))
		})
	}
}

// TestNewHeaderError covers the wrapper's degenerate inputs and its errors.As
// reachability through an extra fmt.Errorf layer (the shape production sees
// once the resilient layer wraps the chain error).
func TestNewHeaderError(t *testing.T) {
	assert.Nil(t, NewHeaderError(nil, nil))

	inner := errors.New("boom")
	assert.Same(t, inner, NewHeaderError(nil, inner), "a nil response must leave the error untouched")

	resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"7"}}}
	wrapped := fmt.Errorf("eino: model chain exhausted: %w", NewHeaderError(resp, inner))

	var he *HeaderError
	require.True(t, errors.As(wrapped, &he))
	assert.Equal(t, 429, he.StatusCode)
	assert.Equal(t, "boom", he.Error())
	assert.Same(t, inner, he.Unwrap())
}

// TestStatusFromError_RejectsOutOfRangeCaptures proves a three-digit number
// captured by an anchored pattern is still range-checked, so a malformed
// message cannot install a nonsense status that then drives classForStatus.
func TestStatusFromError_RejectsOutOfRangeCaptures(t *testing.T) {
	// "999" matches the label pattern but is not a valid HTTP status.
	got := ClassifyError(errors.New("status code: 999, message: ???"))
	assert.Equal(t, 0, got.Status)
	assert.Equal(t, ClassUnknown, got.Class)
}
