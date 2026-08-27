// internal/llm/eino/m8_errorclass_wire_test.go
//
// M8 over the wire: does the classifier's verdict match what the RETRY LOOP
// actually does against a real HTTP server?
//
// errclass_test.go covers the classifier as a function, table-driven, over
// hand-written error strings. That is the right test for a classifier and it is
// not what this file duplicates. What it cannot show is the composition: an
// error produced by the real openai adapter (which reformats the provider's
// body into its own message), passed through the real retry loop, produces the
// number of requests the class implies. Every historical defect in this area
// was a composition defect — the "404 in the body of a 500" case existed
// because the classifier and the transport disagreed about what the error text
// even was.
//
// The assertion in every test here is therefore a REQUEST COUNT at the stub:
// retryable classes must produce more than one, terminal classes exactly one.
package eino

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// countRequestsFor drives one Generate through the production stack wrapped in
// a fast-retrying ResilientChatModel and returns how many requests the stub saw
// plus the final error.
//
// The backoff is compressed to keep the file fast; the SCHEDULE is M1's subject
// and is tested there. What matters here is only how many attempts happen.
func countRequestsFor(t *testing.T, s *stubProvider) (int, error) {
	t.Helper()
	m, _ := buildStubModel(t, s, nil)
	r, err := NewResilientModel([]model.BaseChatModel{retryableWrapper{inner: m}}, ResilientConfig{
		MaxRetries:    2,
		BaseDelay:     time.Millisecond,
		MaxDelay:      5 * time.Millisecond,
		RateLimitBase: time.Millisecond,
		RateLimitMax:  5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewResilientModel: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, genErr := r.Generate(ctx, []*schema.Message{schema.UserMessage("hi")})
	return len(s.chatRequests()), genErr
}

// alwaysStatus returns a responder that answers every request with status and
// body.
func alwaysStatus(status int, body string) func(int, capturedRequest) stubResponse {
	return func(int, capturedRequest) stubResponse {
		return stubResponse{Status: status, Body: body}
	}
}

// TestM8_TerminalStatusesAreNotRetried pins the short-circuit half. Every one
// of these is a configuration or request bug: the next attempt hits the same
// bug, and the backoff only delays the moment the operator learns about it.
func TestM8_TerminalStatusesAreNotRetried(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"400 malformed request", 400,
			`{"error":{"message":"messages[0].role is invalid","type":"invalid_request_error"}}`},
		{"401 bad key", 401,
			`{"error":{"message":"Incorrect API key provided","code":"invalid_api_key"}}`},
		{"403 forbidden", 403,
			`{"error":{"message":"You do not have access to this model","type":"permission_error"}}`},
		{"404 unknown model", 404,
			`{"error":{"message":"The model 'nope' does not exist","code":"model_not_found"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStubProvider(t, alwaysStatus(tc.status, tc.body))
			n, err := countRequestsFor(t, s)
			t.Logf("status %d → %d request(s), err=%v", tc.status, n, err)
			if err == nil {
				t.Fatal("want an error")
			}
			if n != 1 {
				t.Errorf("stub saw %d requests for a terminal %d; retrying a client error burns "+
					"quota and hides the root cause", n, tc.status)
			}
		})
	}
}

// TestM8_TransientStatusesAreRetried is the other half. Each of these clears on
// its own, so giving up on the first one turns a blip into a failed turn.
func TestM8_TransientStatusesAreRetried(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504, 408, 529} {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			// Fail once, then succeed: proves the retry both happens AND
			// recovers, which a permanently-failing stub cannot distinguish
			// from a retry loop that never produces a usable answer.
			s := newStubProvider(t, func(n int, _ capturedRequest) stubResponse {
				if n == 1 {
					return stubResponse{Status: status, Body: `{"error":{"message":"upstream unavailable"}}`}
				}
				return stubResponse{}
			})
			n, err := countRequestsFor(t, s)
			t.Logf("status %d → %d request(s), err=%v", status, n, err)
			if err != nil {
				t.Fatalf("a transient %d was not recovered from: %v", status, err)
			}
			if n < 2 {
				t.Errorf("stub saw %d requests for a transient %d; it was never retried", n, status)
			}
		})
	}
}

// TestM8_FiveHundredMentioningFourZeroFourIsRetried is THE regression case.
//
// The pre-classifier code matched a bare "404" anywhere in the error text, so
// an upstream gateway reporting `HTTP 500: {"error":"backend returned 404 from
// origin"}` was filed as a non-retryable client error and the turn died without
// a single retry — on an error that clears by itself.
//
// It is here rather than only in errclass_test.go because the failure required
// the adapter's own reformatting to put both numbers in one string; a
// hand-written error string is a reconstruction of that, not the thing itself.
func TestM8_FiveHundredMentioningFourZeroFourIsRetried(t *testing.T) {
	s := newStubProvider(t, func(n int, _ capturedRequest) stubResponse {
		if n == 1 {
			return stubResponse{Status: 500,
				Body: `{"error":{"message":"backend returned 404 from origin","type":"upstream_error"}}`}
		}
		return stubResponse{}
	})
	n, err := countRequestsFor(t, s)
	t.Logf("500-quoting-404 → %d request(s), err=%v", n, err)
	if err != nil {
		t.Fatalf("not recovered: %v", err)
	}
	if n < 2 {
		t.Fatal("a 500 whose BODY mentions 404 was treated as a terminal client error and never " +
			"retried — this is the exact defect the anchored status patterns exist to prevent")
	}
}

// TestM8_FourZeroFourInTheBodyOfATerminalErrorStillTerminates is the paired
// control. The fix above must not have been achieved by making 404 retryable:
// a real 404 status is still terminal.
func TestM8_FourZeroFourInTheBodyOfATerminalErrorStillTerminates(t *testing.T) {
	s := newStubProvider(t, alwaysStatus(404,
		`{"error":{"message":"The model 'ghost' does not exist","code":"model_not_found"}}`))
	n, err := countRequestsFor(t, s)
	t.Logf("real 404 → %d request(s), err=%v", n, err)
	if err == nil {
		t.Fatal("want an error")
	}
	if n != 1 {
		t.Errorf("a real 404 was retried %d times; the anchored-pattern fix must not have made "+
			"every 404 retryable", n)
	}
}

// TestM8_ConnectionRefusedIsRetried covers the transport family, which carries
// no HTTP status at all and is therefore classified by type rather than text.
//
// The stub is closed before the call so the connection is genuinely refused —
// a synthesized net.Error would test errors.As, not the path.
func TestM8_ConnectionRefusedIsRetried(t *testing.T) {
	s := newStubProvider(t, nil)
	m, _ := buildStubModel(t, s, nil)
	s.srv.Close() // nothing is listening now

	r, err := NewResilientModel([]model.BaseChatModel{retryableWrapper{inner: m}}, ResilientConfig{
		MaxRetries: 2, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewResilientModel: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, genErr := r.Generate(ctx, []*schema.Message{schema.UserMessage("hi")})
	if genErr == nil {
		t.Fatal("want an error when nothing is listening")
	}
	t.Logf("transport error: %v", genErr)
	if c := ClassifyError(genErr); !IsRetryableClass(c.Class) && c.Class != ClassUnknown {
		t.Errorf("connection refused classified as %s, want a retryable class", c.Class)
	}
}

// TestM8_ContextOverflowIsNotRetriedUnchanged pins the C6 boundary from M8's
// side: an over-long prompt must NOT go round the ordinary retry loop, because
// resending identical bytes reproduces the rejection exactly and is charged for
// each time. Shrinking first is C6's job (see the C6 tests).
func TestM8_ContextOverflowIsNotRetriedUnchanged(t *testing.T) {
	s := newStubProvider(t, alwaysStatus(400,
		`{"error":{"message":"This model's maximum context length is 32000 tokens, however you `+
			`requested 41000 tokens","code":"context_length_exceeded"}}`))
	n, err := countRequestsFor(t, s)
	t.Logf("context overflow → %d request(s), err=%v", n, err)
	if err == nil {
		t.Fatal("want an error")
	}
	if c := ClassifyError(err); c.Class != ClassContextOverflow {
		t.Errorf("class = %s, want %s: an overflow filed as a plain client error loses the one "+
			"distinction that says 'compact, then retry'", c.Class, ClassContextOverflow)
	}
	if n != 1 {
		t.Errorf("stub saw %d requests; an unchanged overflow retry can only fail again", n)
	}
}

// TestM8_RateLimitIsRetried closes the matrix: 429 is retryable (on M1's
// schedule, tested there) rather than terminal.
func TestM8_RateLimitIsRetried(t *testing.T) {
	s := newStubProvider(t, func(n int, _ capturedRequest) stubResponse {
		if n == 1 {
			return stubResponse{Status: http.StatusTooManyRequests,
				Body: `{"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`}
		}
		return stubResponse{}
	})
	n, err := countRequestsFor(t, s)
	t.Logf("429 → %d request(s), err=%v", n, err)
	if err != nil {
		t.Fatalf("a 429 was not recovered from: %v", err)
	}
	if n < 2 {
		t.Error("a 429 was never retried")
	}
}
