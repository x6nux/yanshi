// internal/llm/eino/retryafter.go
//
// M1, the half that was missing: making a provider's Retry-After header
// SURVIVE the trip out of the OpenAI Chat Completions adapter.
//
// errclass.go already knows how to read the header (RetryAfterFromHeader) and
// the retry loop already prefers a server-requested cooldown over its blind
// exponential (RateLimitBackoffWith). Both were reachable only for the
// anthropic and openai-responses adapters, which own their *http.Response and
// wrap non-200s in a HeaderError themselves. The default "openai" kind — the
// one every OpenAI-compatible gateway, vLLM deployment and local runtime uses
// — could not reach either, because go-openai's handleErrorResp reads the body
// and DROPS resp.Header on the floor: APIError keeps only HTTPStatusCode, and
// RequestError keeps only the body bytes. By the time the error reaches
// ClassifyError there is no header map left to consult, so a 429 that said
// "Retry-After: 3" and a 429 that said nothing at all produced byte-identical
// errors and both got the 5s/10s/20s blind schedule.
//
// That is not a cosmetic gap. The whole justification for M1 is that the server
// knows when its bucket refills and we do not: waiting 5s when it asked for 3
// wastes 2s per retry, and — the case that actually hurts — waiting 5s when it
// asked for 30 spends four more requests against the very bucket we are waiting
// on, which is how a short throttle becomes a long one.
//
// WHY A ROUNDTRIPPER AND NOT A go-openai PATCH. The header exists at exactly
// one moment: inside http.Client.Do, before go-openai converts the response to
// an error. A RoundTripper is the only seam in that window that does not
// require forking the SDK (this repo already vendors one fork, for bubbletea,
// and the bar for a second is high). The transport cannot return the header to
// the caller — it must return the response unchanged or the SDK breaks — so it
// deposits it in a holder carried by the request's own context, and the model
// decorator that installed that holder rejoins header to error afterwards.
//
// WHY THE HOLDER IS PER-CALL. A field on the transport would be shared by every
// concurrent request through the same client: a sub-agent fan-out would attach
// one provider's Retry-After to another provider's error, which is worse than
// having no header at all because it is silently wrong. A fresh holder per
// Generate/Stream, reachable only through that call's context, makes the join
// unambiguous by construction.
package eino

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// respHeaderHolder is the per-call slot a headerCaptureTransport writes the
// failed response's status and headers into.
//
// It keeps the FIRST failure rather than the last. A single model call can
// issue more than one HTTP request (a redirect, or an SDK-internal retry we do
// not control), and the first rejection is the one whose cooldown the caller is
// actually subject to; a later 500 overwriting a 429's Retry-After would drop
// the only useful signal in the pair.
type respHeaderHolder struct {
	mu     sync.Mutex
	set    bool
	status int
	header http.Header
}

// capture records status/header unless a failure was already recorded.
func (h *respHeaderHolder) capture(status int, header http.Header) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.set {
		return
	}
	h.set, h.status, h.header = true, status, header.Clone()
}

// get returns the captured status and headers, and whether anything was
// captured at all.
func (h *respHeaderHolder) get() (int, http.Header, bool) {
	if h == nil {
		return 0, nil, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status, h.header, h.set
}

// respHeaderKey is the context key for the per-call respHeaderHolder.
type respHeaderKey struct{}

// withRespHeaderHolder returns ctx carrying a fresh holder, plus the holder.
func withRespHeaderHolder(ctx context.Context) (context.Context, *respHeaderHolder) {
	h := &respHeaderHolder{}
	return context.WithValue(ctx, respHeaderKey{}, h), h
}

// respHeaderHolderFrom returns the holder bound to ctx, or nil.
func respHeaderHolderFrom(ctx context.Context) *respHeaderHolder {
	h, _ := ctx.Value(respHeaderKey{}).(*respHeaderHolder)
	return h
}

// headerCaptureTransport is an http.RoundTripper that copies the headers of
// failed responses into the holder bound to the request's context.
//
// It is deliberately inert when no holder is bound: the transport is installed
// on the provider's http.Client unconditionally, and a call made outside a
// HeaderAwareModel (an SDK-internal request, a caller that built the adapter
// directly) must behave exactly as it did before this file existed.
type headerCaptureTransport struct {
	// base is the wrapped transport. nil means http.DefaultTransport.
	base http.RoundTripper
}

// RoundTrip forwards to the base transport and records the headers of any
// response with a >=400 status.
//
// The response itself is returned completely untouched — body unread, headers
// unmodified. Reading the body here would consume it before the SDK could
// parse the error message out of it, turning a precise "invalid_api_key" into
// an empty one.
func (t *headerCaptureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode < 400 {
		return resp, err
	}
	respHeaderHolderFrom(req.Context()).capture(resp.StatusCode, resp.Header)
	return resp, err
}

// newHeaderCaptureClient returns an http.Client whose transport captures failed
// responses' headers. It is what BuildProviders installs on the openai adapter.
//
// The client still has NO Timeout and NO transport-level retry, for the reason
// spelled out in BuildProviders: ResilientChatModel is the single retry
// authority. Capturing a header is observation, not policy.
func newHeaderCaptureClient() *http.Client {
	return &http.Client{Transport: &headerCaptureTransport{base: http.DefaultTransport}}
}

// HeaderAwareModel wraps a model adapter whose SDK discards response headers,
// re-attaching them to the error so ClassifyError can read Retry-After from the
// authoritative source.
//
// It is the consumer half of headerCaptureTransport, and the two are useless
// apart: the transport writes into a holder only this wrapper installs, and
// this wrapper finds a holder populated only by that transport. Both are
// installed together by buildOne for the "openai" kind.
//
// It is NOT needed for the anthropic and openai-responses adapters. Those hold
// the *http.Response themselves and already wrap their non-200 errors in a
// HeaderError; wrapping them again would be a no-op at best and would double
// the error's depth for no gain.
type HeaderAwareModel struct {
	// Inner is the wrapped adapter.
	Inner model.BaseChatModel
}

// NewHeaderAwareModel wraps inner. A nil inner yields nil, matching
// NewAdaptiveModel's convention so a builder that failed produces a nil-pointer
// panic at its own call site rather than a silent do-nothing model.
func NewHeaderAwareModel(inner model.BaseChatModel) *HeaderAwareModel {
	if inner == nil {
		return nil
	}
	return &HeaderAwareModel{Inner: inner}
}

// compile-time interface check.
var _ model.BaseChatModel = (*HeaderAwareModel)(nil)

// attach rejoins a captured header map to err.
//
// It returns err unchanged when nothing was captured, when err is nil, or when
// err is ALREADY a HeaderError. The last case matters for the openai-responses
// adapter, which wraps its own errors: re-wrapping would put a transport-
// captured header (possibly from a different request in the same call) in front
// of the one the adapter deliberately attached.
func attach(h *respHeaderHolder, err error) error {
	if err == nil {
		return nil
	}
	status, header, ok := h.get()
	if !ok || header == nil {
		return err
	}
	var existing *HeaderError
	if errors.As(err, &existing) {
		return err
	}
	return &HeaderError{StatusCode: status, Header: header, Err: err}
}

// Generate installs a holder, calls the inner model, and re-attaches the failed
// response's headers to any error.
func (m *HeaderAwareModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (
	*schema.Message, error) {
	ctx, h := withRespHeaderHolder(ctx)
	msg, err := m.Inner.Generate(ctx, in, opts...)
	if err != nil {
		return nil, attach(h, err)
	}
	return msg, nil
}

// Stream installs a holder and re-attaches headers to BOTH the synchronous open
// error and any error the returned reader later surfaces.
//
// Both, because which one carries a 429 is an SDK implementation detail: the
// eino openai adapter reports some setup rejections from Stream itself and
// others from the first Recv, and a fix that covered only the synchronous path
// would work or not work depending on a decision made in a dependency. The
// holder outlives the call precisely so the reader can still consult it.
//
// A mid-stream drop leaves the holder empty (there was no 4xx response), so
// attach returns those errors untouched and the ordinary transient schedule
// still applies to them.
func (m *HeaderAwareModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (
	*schema.StreamReader[*schema.Message], error) {
	ctx, h := withRespHeaderHolder(ctx)
	sr, err := m.Inner.Stream(ctx, in, opts...)
	if err != nil {
		return nil, attach(h, err)
	}
	return wrapStreamErrors(sr, h), nil
}

// wrapStreamErrors returns a reader forwarding sr unchanged, with every non-EOF
// error passed through attach.
func wrapStreamErrors(sr *schema.StreamReader[*schema.Message], h *respHeaderHolder) *schema.StreamReader[*schema.Message] {
	out, ow := schema.Pipe[*schema.Message](1)
	go func() {
		defer func() {
			ow.Close()
			sr.Close()
		}()
		for {
			msg, err := sr.Recv()
			if err != nil {
				if isEOF(err) {
					return
				}
				ow.Send(nil, attach(h, err))
				return
			}
			if ow.Send(msg, nil) {
				return
			}
		}
	}()
	return out
}
