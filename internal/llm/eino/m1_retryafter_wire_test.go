// internal/llm/eino/m1_retryafter_wire_test.go
//
// M1 over the wire: does a provider's Retry-After actually change WHEN the
// retry happens?
//
// This is a regression file with a specific history. errclass_test.go already
// proved every piece: RetryAfterFromHeader parses both RFC 9110 forms, a
// HeaderError carries the map to ClassifyError, and RateLimitBackoffWith
// prefers a server cooldown over its exponential. All green — and the feature
// was still dead for the DEFAULT provider kind, because every one of those
// tests built the http.Header itself and in production nothing did.
// go-openai's handleErrorResp reads the body and drops resp.Header, so the
// openai adapter's 429 reached the classifier with no header to read.
//
// Measured against a stub answering 429 + "Retry-After: 3", the real binary
// retried after 5.0s then 10.0s — the blind exponential, exactly as if the
// header had never been sent.
//
// So these tests assert on OBSERVED ARRIVAL TIMES at an http server. A test
// that inspects a Classification cannot fail for the reason this feature
// actually failed.
package eino

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// The no-Retry-After fallback is shortened for these tests but kept
// deliberately LONGER than the cooldowns the stub asks for. If the header were
// dropped again the test would not merely miss a tight tolerance — it would
// take many times longer and fail by a wide margin.
const (
	rateLimitTestBase = 2 * time.Second
	rateLimitTestMax  = 8 * time.Second
	// stubCooldown is what the stub's Retry-After asks for.
	stubCooldown = 300 * time.Millisecond
)

// runResilientGenerate drives one Generate through the FULL production stack
// (BuildProviders → HeaderAwareModel → openai adapter → http) wrapped in a
// ResilientChatModel on the shortened rate-limit schedule.
func runResilientGenerate(t *testing.T, s *stubProvider) {
	t.Helper()
	m, _ := buildStubModel(t, s, nil)
	// A retryable wrapper is required for the Generate retry loop to engage at
	// all; production's CompactingModel supplies the equivalent. It does NOT
	// alter classification, so the schedule chosen is still the real one.
	r, err := NewResilientModel([]model.BaseChatModel{retryableWrapper{inner: m}}, ResilientConfig{
		MaxRetries:    3,
		RateLimitBase: rateLimitTestBase,
		RateLimitMax:  rateLimitTestMax,
	})
	if err != nil {
		t.Fatalf("NewResilientModel: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := r.Generate(ctx, []*schema.Message{schema.UserMessage("hi")}); err != nil {
		t.Fatalf("Generate through the stub failed: %v", err)
	}
}

// retryableWrapper marks provider errors as retryable so ResilientChatModel's
// Generate loop engages, mirroring what the production CompactingModel layer
// does. It does NOT alter classification: the wrapped error is still the real
// one the http stack produced, so the schedule chosen is the real one.
type retryableWrapper struct{ inner model.BaseChatModel }

// Generate forwards to the inner model, wrapping any error as retryable.
func (w retryableWrapper) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (
	*schema.Message, error) {
	msg, err := w.inner.Generate(ctx, in, opts...)
	if err != nil {
		return nil, &RetryableModelError{Err: err}
	}
	return msg, nil
}

// Stream forwards to the inner model unchanged.
func (w retryableWrapper) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (
	*schema.StreamReader[*schema.Message], error) {
	return w.inner.Stream(ctx, in, opts...)
}

// TestM1_RetryAfterHeaderIsHonouredOverTheWire is the regression test for the
// dropped-header defect: the stub answers 429 with a short Retry-After, and the
// NEXT request must arrive at roughly that delay rather than at the much longer
// blind fallback.
func TestM1_RetryAfterHeaderIsHonouredOverTheWire(t *testing.T) {
	s := newStubProvider(t, func(n int, _ capturedRequest) stubResponse {
		if n == 1 {
			return stubResponse{
				Status: http.StatusTooManyRequests,
				Header: map[string]string{"Retry-After": "0.3"},
				Body:   `{"error":{"message":"slow down","type":"rate_limit_error"}}`,
			}
		}
		return stubResponse{}
	})
	runResilientGenerate(t, s)

	gaps := s.gaps()
	if len(gaps) != 1 {
		t.Fatalf("want exactly 1 retry gap, got %d (chat requests=%d)", len(gaps), len(s.chatRequests()))
	}
	t.Logf("observed retry gap = %v (server asked %v; blind fallback is %v)",
		gaps[0], stubCooldown, rateLimitTestBase)
	if gaps[0] < stubCooldown/2 {
		t.Errorf("retried after %v, sooner than the %v the server asked for", gaps[0], stubCooldown)
	}
	if gaps[0] >= rateLimitTestBase {
		t.Errorf("retry gap %v reached the blind fallback %v — the Retry-After header was ignored, "+
			"which is the exact defect this test exists for", gaps[0], rateLimitTestBase)
	}
}

// TestM1_RetryAfterHTTPDateIsHonouredOverTheWire covers the second RFC 9110
// form. Several CDN-fronted gateways send only this one, so parsing just
// delta-seconds would silently drop half the signal in production while every
// delta-seconds test stayed green.
//
// HTTP-date has one-second granularity and no sub-second representation, so the
// assertion is bounded on both sides rather than pinned: a +1s date must
// produce a wait that is clearly not the multi-second blind fallback.
func TestM1_RetryAfterHTTPDateIsHonouredOverTheWire(t *testing.T) {
	s := newStubProvider(t, func(n int, _ capturedRequest) stubResponse {
		if n == 1 {
			return stubResponse{
				Status: http.StatusTooManyRequests,
				Header: map[string]string{
					"Retry-After": time.Now().UTC().Add(1 * time.Second).Format(http.TimeFormat),
				},
				Body: `{"error":{"message":"slow down","type":"rate_limit_error"}}`,
			}
		}
		return stubResponse{}
	})
	runResilientGenerate(t, s)

	gaps := s.gaps()
	if len(gaps) != 1 {
		t.Fatalf("want exactly 1 retry gap, got %d", len(gaps))
	}
	t.Logf("observed retry gap for an HTTP-date Retry-After = %v", gaps[0])
	if gaps[0] >= rateLimitTestBase {
		t.Errorf("retry gap %v reached the blind fallback %v — the HTTP-date form was not parsed",
			gaps[0], rateLimitTestBase)
	}
}

// TestM1_AbsurdRetryAfterIsClampedNotObeyed pins the safety bound from the
// OTHER side of the wire.
//
// Retry-After is attacker- or bug-controlled input: a misconfigured gateway
// asking for 99999 seconds would, if obeyed, wedge the turn for 27 hours with
// no indication anything was still running. MaxRetryAfter caps it at 5 minutes.
//
// The test cannot wait five minutes, so it asserts the CLAMP rather than the
// sleep, through the real error the real http stack produced — which is the
// part that could regress. It deliberately does not run the retry.
func TestM1_AbsurdRetryAfterIsClampedNotObeyed(t *testing.T) {
	s := newStubProvider(t, func(int, capturedRequest) stubResponse {
		return stubResponse{
			Status: http.StatusTooManyRequests,
			Header: map[string]string{"Retry-After": "99999"},
			Body:   `{"error":{"message":"slow down","type":"rate_limit_error"}}`,
		}
	})
	m, _ := buildStubModel(t, s, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := m.Generate(ctx, []*schema.Message{schema.UserMessage("hi")})
	if err == nil {
		t.Fatal("want the stub's 429 to surface as an error")
	}
	c := ClassifyError(err)
	t.Logf("class=%s status=%d retryAfter=%v", c.Class, c.Status, c.RetryAfter)
	if c.Class != ClassRateLimit {
		t.Errorf("class = %s, want %s", c.Class, ClassRateLimit)
	}
	if c.RetryAfter != MaxRetryAfter {
		t.Errorf("retryAfter = %v, want it clamped to %v; an unclamped 99999s would wedge the "+
			"process for over a day on one bad header", c.RetryAfter, MaxRetryAfter)
	}
	if d := RateLimitBackoff(1, c.RetryAfter); d != MaxRetryAfter {
		t.Errorf("backoff = %v, want %v", d, MaxRetryAfter)
	}
}

// TestM1_HeaderReachesClassifierFromTheOpenAIAdapter is the narrowest statement
// of the defect, with no timing in it: a 429 from the DEFAULT provider kind
// must arrive at ClassifyError carrying its Retry-After.
//
// It is separate from the timing tests because it is the assertion that names
// the root cause. If go-openai ever regains header propagation, or the capture
// transport is removed, this fails immediately and unambiguously while the
// timing tests would merely get slower.
func TestM1_HeaderReachesClassifierFromTheOpenAIAdapter(t *testing.T) {
	s := newStubProvider(t, func(int, capturedRequest) stubResponse {
		return stubResponse{
			Status: http.StatusTooManyRequests,
			Header: map[string]string{"Retry-After": "7"},
			// A body that mentions NO cooldown, so the text-scraping fallback
			// in retryAfterFromError cannot be what makes this pass. The header
			// must be the source.
			Body: `{"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`,
		}
	})
	m, _ := buildStubModel(t, s, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := m.Generate(ctx, []*schema.Message{schema.UserMessage("hi")})
	if err == nil {
		t.Fatal("want an error")
	}
	t.Logf("provider error text: %v", err)
	var he *HeaderError
	if !errors.As(err, &he) {
		t.Fatalf("no HeaderError in the chain: the openai adapter's response headers were dropped, "+
			"so Retry-After is unrecoverable (err=%v)", err)
	}
	if got := he.Header.Get("Retry-After"); got != "7" {
		t.Errorf("captured Retry-After = %q, want %q", got, "7")
	}
	if c := ClassifyError(err); c.RetryAfter != 7*time.Second {
		t.Errorf("ClassifyError.RetryAfter = %v, want 7s", c.RetryAfter)
	}
}

// TestM1_SuccessfulCallCapturesNoHeader pins the negative direction: the
// capture transport must be inert on success.
//
// Without this, a transport that captured every response would attach a stale
// 200's headers to a later unrelated error, and a Retry-After absent from the
// failing response could appear out of a previous one.
func TestM1_SuccessfulCallCapturesNoHeader(t *testing.T) {
	s := newStubProvider(t, nil) // always succeeds
	m, _ := buildStubModel(t, s, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	msg, err := m.Generate(ctx, []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if msg.Content != "STUB-OK" {
		t.Errorf("content = %q, want STUB-OK", msg.Content)
	}
}
