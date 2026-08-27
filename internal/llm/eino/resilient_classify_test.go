package eino

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedErrModel always fails with the same error and counts Generate calls, so
// a test can assert exactly how many attempts the retry loop made.
type fixedErrModel struct {
	err   error
	calls int
}

func (m *fixedErrModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	m.calls++
	return nil, m.err
}

func (m *fixedErrModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.calls++
	return nil, m.err
}

var _ model.BaseChatModel = (*fixedErrModel)(nil)

// generateWith runs one Generate against a single-provider chain with a fast
// backoff, returning the number of attempts the retry loop made.
func generateWith(t *testing.T, err error, maxRetries int) int {
	t.Helper()
	m := &fixedErrModel{err: err}
	r, buildErr := NewResilientModel([]model.BaseChatModel{m}, ResilientConfig{
		MaxRetries: maxRetries,
		BaseDelay:  time.Millisecond,
		MaxDelay:   time.Millisecond,
		// The rate-limit schedule is squashed too, or the 429 rows below would
		// spend the real 5s/10s/20s cooldown. Squashing it is only possible
		// because it is a CONFIG field rather than a constant — which is the
		// same property an operator needs to tune it (see ResilientConfig).
		RateLimitBase: time.Millisecond,
		RateLimitMax:  time.Millisecond,
	})
	require.NoError(t, buildErr)
	_, _ = r.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	return m.calls
}

// TestRetryHonoursClassification_404InBodyOfA500 is the M8 regression, pinned
// at the layer that actually decides.
//
// The old classifier did strings.Contains(err, "404") and short-circuited
// retry, so a transient upstream 500 that happened to quote a 404 from its own
// origin was abandoned after a single attempt. That is a real outage turned
// into a hard failure by a substring. The status is 500; the "404" is body
// text and must not participate.
//
// Both halves are asserted, because a fix that simply stopped recognising 404
// anywhere would pass the first half and silently reintroduce pointless
// retries on genuine 404s.
func TestRetryHonoursClassification_404InBodyOfA500(t *testing.T) {
	t.Run("500 quoting a 404 retries", func(t *testing.T) {
		err := &RetryableModelError{
			Err: errors.New(`anthropic: API error (HTTP 500): {"error":"backend returned 404 from origin"}`),
		}
		assert.Equal(t, 4, generateWith(t, err, 3),
			"a transient 500 must consume all retries even when its body mentions 404")
	})

	t.Run("genuine 404 still does not retry", func(t *testing.T) {
		err := &RetryableModelError{
			Err: errors.New("error, status code: 404, status: 404 Not Found, message: no such model"),
		}
		assert.Equal(t, 1, generateWith(t, err, 3),
			"a real 404 is a config bug; retrying only hides it")
	})
}

// TestRetryClassification_PerClassAttemptCounts states the retry decision as a
// table over the four decided classes, at the resilient layer rather than at
// the classifier. Errors are wrapped in RetryableModelError throughout so the
// only thing that can vary the outcome is the classification — a class that
// leaked through would show up as the wrong attempt count.
func TestRetryClassification_PerClassAttemptCounts(t *testing.T) {
	const maxRetries = 3
	cases := []struct {
		name     string
		errText  string
		attempts int
	}{
		{"5xx retries", "eino/responses: API error (HTTP 502): bad gateway", maxRetries + 1},
		{"529 overloaded retries", "anthropic: API error (HTTP 529): overloaded_error", maxRetries + 1},
		{"mid-stream EOF retries", "failed to receive stream chunk: unexpected EOF", maxRetries + 1},
		{"429 retries", "anthropic: API error (HTTP 429): rate_limit_error", maxRetries + 1},
		{"401 does not retry", "error, status code: 401, status: 401 Unauthorized", 1},
		{"403 does not retry", "error, status code: 403, status: 403 Forbidden", 1},
		{"invalid_api_key does not retry", `{"error":{"code":"invalid_api_key"}}`, 1},
		{"context overflow does not retry", "status code: 400, message: context_length_exceeded", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := &RetryableModelError{Err: errors.New(tc.errText)}
			assert.Equal(t, tc.attempts, generateWith(t, err, maxRetries))
		})
	}
}

// TestBackoffFor_RateLimitUsesTheCooldownSchedule is the M1 assertion at the
// point of use: the delay chosen for a 429 comes from the rate-limit schedule,
// not from cfg.BaseDelay.
//
// The config here is the pathological one the bug report describes — a 1ms
// base and a 5ms cap. If backoffFor consulted it for a 429, the server's
// "wait 30 seconds" would be clamped to 5ms and the retry loop would fire ten
// requests inside the throttle window, lengthening it.
func TestBackoffFor_RateLimitUsesTheCooldownSchedule(t *testing.T) {
	r, err := NewResilientModel(
		[]model.BaseChatModel{&fixedErrModel{err: errors.New("x")}},
		ResilientConfig{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
	)
	require.NoError(t, err)

	t.Run("transient keeps the configured schedule", func(t *testing.T) {
		got := r.backoffFor(1, errors.New("failed to receive stream chunk: unexpected EOF"))
		assert.Equal(t, time.Millisecond, got)
	})

	t.Run("rate limit without a header uses the slow base", func(t *testing.T) {
		got := r.backoffFor(1, errors.New("anthropic: API error (HTTP 429): rate_limit_error"))
		assert.Equal(t, RateLimitBaseDelay, got,
			"cfg.MaxDelay must not clamp a rate-limit wait; that is how a short throttle becomes a long one")
	})

	t.Run("rate limit with Retry-After honours the server", func(t *testing.T) {
		resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"30"}}}
		wrapped := NewHeaderError(resp, errors.New("anthropic: API error (HTTP 429): rate_limit_error"))
		assert.Equal(t, 30*time.Second, r.backoffFor(1, wrapped))
	})

	t.Run("rate limit with an absurd Retry-After is clamped", func(t *testing.T) {
		resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"51496"}}}
		wrapped := NewHeaderError(resp, errors.New("API error (HTTP 429): free usage limit"))
		assert.Equal(t, MaxRetryAfter, r.backoffFor(1, wrapped))
	})

	t.Run("nil error falls through to the ordinary schedule", func(t *testing.T) {
		assert.Equal(t, time.Millisecond, r.backoffFor(1, nil))
	})
}

// TestIsRetryableStreamErr_UnknownFallsBackToLegacyMarkers proves the
// ClassUnknown floor. The legacy markers are looser than the classifier's, and
// keeping them means this change can only ADD retries relative to the previous
// behavior — never silently remove one that used to happen.
func TestIsRetryableStreamErr_UnknownFallsBackToLegacyMarkers(t *testing.T) {
	ctx := context.Background()
	for _, text := range []string{
		"some wrapper: transport closed",
		"stream ended with EOF",
		"gateway asked us to retry",
	} {
		t.Run(text, func(t *testing.T) {
			err := errors.New(text)
			require.Equal(t, ClassUnknown, ClassifyError(err).Class,
				"fixture must actually be unclassified or it tests the wrong path")
			assert.True(t, isRetryableStreamErr(ctx, err))
		})
	}
}

// TestIsRetryableStreamErr_UserCancelStillWins pins the ordering: a real user
// cancel suppresses retry even for an error the classifier calls transient.
// The classifier runs after that check, so adding it must not have moved the
// cancel gate.
func TestIsRetryableStreamErr_UserCancelStillWins(t *testing.T) {
	transient := errors.New("anthropic: API error (HTTP 503): unavailable")
	assert.True(t, isRetryableStreamErr(context.Background(), transient))

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	ctx := WithUserCancelCtx(context.Background(), cancelled)
	assert.False(t, isRetryableStreamErr(ctx, transient),
		"a user cancel must win over any classification")
}

// TestIsRetryableStreamErr_ErrStreamIdleIsAlwaysRetryable pins the W-A-06
// stream watchdog's error onto the actual Stream-path retry gate, not just
// IsRetryableModelErr (the Generate-path helper the watchdog's own test
// asserts against). isRetryableStreamErr is what runStream really consults on
// a streamErr outcome, and it does not call IsRetryableModelErr at all — a
// change that made ErrStreamIdle satisfy the latter without also reaching
// this switch would leave the goal loop's actual retry decision unfixed.
//
// The identity check is asserted directly (not merely "the keyword classifier
// happens to match 'timeout' in the text") so a future rewording of
// ErrStreamIdle's message, or a pruning of transientMarkers, cannot silently
// stop this from retrying.
func TestIsRetryableStreamErr_ErrStreamIdleIsAlwaysRetryable(t *testing.T) {
	assert.True(t, isRetryableStreamErr(context.Background(), ErrStreamIdle))
}

// TestIsNonRetryableClientErr_CoversOverflow pins that the Generate-path
// short-circuit and the Stream-path decision agree on both non-retryable
// classes, which is the property that lets one classifier serve both.
func TestIsNonRetryableClientErr_CoversOverflow(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"401", errors.New("status code: 401, status: 401 Unauthorized"), true},
		{"overflow", errors.New("status code: 400, message: maximum context length is 128000"), true},
		{"500 quoting 404", errors.New("API error (HTTP 500): origin said 404"), false},
		{"transient EOF", errors.New("unexpected EOF"), false},
		{"429", errors.New("API error (HTTP 429): slow down"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isNonRetryableClientErr(tc.err))
		})
	}
}

// TestRetryStopsOnRateLimitBudget proves the rate-limit path still terminates:
// a 429 is retryable, so without a working attempt cap it would loop against
// the cooldown schedule forever. Retry-After is set to 0 seconds so the test
// does not actually sleep for the real cooldown.
func TestRetryStopsOnRateLimitBudget(t *testing.T) {
	resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"0"}}}
	err := &RetryableModelError{
		Err: NewHeaderError(resp, fmt.Errorf("anthropic: API error (HTTP 429): rate_limit_error")),
	}
	assert.Equal(t, 3, generateWith(t, err, 2), "1 initial + 2 retries")
}

// TestResilientConfig_RateLimitScheduleDefaults proves the new fields default
// to the package schedule, so an existing caller that never heard of them gets
// the M1 behavior without changing its construction.
//
// The second half is the reason they are fields at all: BaseDelay and
// RateLimitBase must stay independent, or squashing one for a fast test (or
// tightening one in production) silently changes the other.
func TestResilientConfig_RateLimitScheduleDefaults(t *testing.T) {
	m := &fixedErrModel{err: errors.New("x")}

	r, err := NewResilientModel([]model.BaseChatModel{m}, ResilientConfig{})
	require.NoError(t, err)
	assert.Equal(t, RateLimitBaseDelay, r.cfg.RateLimitBase)
	assert.Equal(t, RateLimitMaxDelay, r.cfg.RateLimitMax)

	custom, err := NewResilientModel([]model.BaseChatModel{m}, ResilientConfig{
		BaseDelay:     time.Millisecond,
		MaxDelay:      time.Millisecond,
		RateLimitBase: 250 * time.Millisecond,
		RateLimitMax:  time.Second,
	})
	require.NoError(t, err)
	rateLimited := errors.New("anthropic: API error (HTTP 429): rate_limit_error")
	assert.Equal(t, 250*time.Millisecond, custom.backoffFor(1, rateLimited))
	assert.Equal(t, 500*time.Millisecond, custom.backoffFor(2, rateLimited))
	assert.Equal(t, time.Second, custom.backoffFor(9, rateLimited), "capped by RateLimitMax")
	assert.Equal(t, time.Millisecond, custom.backoffFor(1, errors.New("unexpected EOF")),
		"the transient schedule must be unaffected by the rate-limit one")
}

// TestRateLimitBackoffWith_DegenerateSchedules pins the guards on the
// caller-supplied schedule: non-positive values fall back to the package
// defaults, an inverted (max < base) pair does not produce a delay shorter than
// the base, and the doubling loop cannot overflow into a negative duration.
func TestRateLimitBackoffWith_DegenerateSchedules(t *testing.T) {
	assert.Equal(t, RateLimitBaseDelay, RateLimitBackoffWith(1, 0, 0, 0),
		"a zero base falls back to the package default")
	assert.Equal(t, RateLimitBaseDelay, RateLimitBackoffWith(1, 0, -time.Second, -time.Second))
	assert.Equal(t, 2*time.Second, RateLimitBackoffWith(5, 0, 2*time.Second, time.Second),
		"an inverted pair must not return less than the base")
	assert.Equal(t, time.Hour, RateLimitBackoffWith(500, 0, time.Second, time.Hour),
		"a huge attempt count must saturate at max, not overflow")
	assert.Equal(t, 30*time.Second, RateLimitBackoffWith(9, 30*time.Second, time.Millisecond, time.Millisecond),
		"a server cooldown is not bounded by the local max")
}
