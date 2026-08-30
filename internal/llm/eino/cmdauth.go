// internal/llm/eino/cmdauth.go
//
// W-C-12: command-based provider token authentication. config.ProviderAuthConfig
// is the config surface (and carries the security rationale for why this must
// go through secproc rather than a raw exec.Command). This file is the
// runtime: a cached, refreshable credential source (CommandTokenSource) and an
// http.RoundTripper (authRefreshTransport) that injects it into every
// outgoing request and retries exactly once on a 401 with a forced refresh.
package eino

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/ctxcompact"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
)

// authCommandTool is the guard.Action.Tool / secproc.SecureProcessSpec.Tool
// name an auth.command spawn is Authorized under. See runAuthCommand's doc
// comment for why this is a fixed, code-defined constant rather than one of
// the model-facing tool names GOV5/toolreg reason about.
const authCommandTool = "provider_auth_command"

// authCommandTimeout bounds one run of auth.command. CommandTokenSource
// serializes refreshes behind a mutex, so a hung command would otherwise
// block every concurrent Generate/Stream call on the same provider forever.
const authCommandTimeout = 30 * time.Second

// placeholderAPIKeyForCommandAuth satisfies the non-empty-APIKey validation in
// NewAnthropicModel / NewOpenAIResponsesModel when the real credential comes
// from auth.command instead of a static APIKey. It never reaches the wire:
// authRefreshTransport overwrites the credential header on every request
// before the RoundTripper it wraps sees it.
const placeholderAPIKeyForCommandAuth = "auth-command-managed"

// AuthCommandRunner runs an auth.command's argv and returns its trimmed
// stdout as the credential. A function type, not an interface, so tests can
// supply a fake without a mock framework.
type AuthCommandRunner func(ctx context.Context, argv []string) (string, error)

// SecretRegistrar records a credential-bearing string with the process-wide
// secrets.Redactor (internal/secrets), so it is scrubbed from every log line,
// WS/SSE frame and SQLite row from that point on — the same protection
// bootstrap.Build already gives every configured api_key and provider header
// (see bootstrap.go's redactor.Register loops for APIKey/Headers). Narrow,
// one-method interface — mirroring internal/ctxcompact.Redactor's precedent —
// so this package accepts *secrets.Redactor structurally without importing
// internal/secrets: nothing here needs any other Redactor method.
//
// W-C-12 review B-2: before this existed, the auth.command-produced token was
// the ONLY dynamic credential in the provider stack never registered — a
// config api_key or a config header value is redacted on sight, but a token
// this package fetched itself was not, so it would appear verbatim in any log
// line or transcript that happened to echo it back (a 401 diagnostic, a
// stack trace from the HTTP client).
type SecretRegistrar interface {
	Register(secret string)
}

// firstRegistrar returns the first non-nil element of regs, or nil.
// BuildProviders and NewCommandTokenSource take an OPTIONAL trailing
// SecretRegistrar (variadic, so the dozens of existing call sites across
// tests that do not care about credential registration keep compiling
// unchanged); buildOne and authRefreshHTTPTransport downstream take a plain
// SecretRegistrar once BuildProviders has already made that choice.
func firstRegistrar(regs []SecretRegistrar) SecretRegistrar {
	for _, r := range regs {
		if r != nil {
			return r
		}
	}
	return nil
}

// DefaultAuthCommandRunner is the production runner: it spawns argv through
// secproc.Launch, the single Authorize-gated entry point every untrusted
// spawn in yanshi must use. It calls secproc.Launch directly rather than
// internal/tools.LaunchSecureProcess (a thin, behavior-identical wrapper
// around the same call — see that function's doc comment) because eino
// cannot import internal/tools at all; see runAuthCommand's doc comment.
var DefaultAuthCommandRunner AuthCommandRunner = runAuthCommand

// authCommandProfile is the purpose-built permission profile the auth.command
// spawn is Authorized against: it allows exactly one tool name and nothing
// else, mirroring bootstrap.agentLaunchProfile's shape for the same reason —
// the ambient turn profile (which may be `strict`, may be plan-mode-readonly,
// or may simply have no opinion about this internal spawn) must not be what
// decides whether a provider may fetch its own credential.
func authCommandProfile() guard.PermissionProfile {
	return guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{authCommandTool}}}
}

// authCommandErrf builds an error for a failure in the credential-fetch
// pipeline itself (config or wiring: no secproc.Factory, the program is
// missing, it exited non-zero, its stdout was empty) as opposed to a failure
// from the chat model the credential is FOR. It wraps
// ctxcompact.ErrConfigOrWiring via fmt.Errorf's "%w" (Go 1.20+ allows more
// than one %w in one call, so this composes with an existing "%w" for an
// underlying cause without disturbing it) so internal/ctxcompact's
// isConfigOrWiringFailure can classify the error with errors.Is instead of
// matching this function's wording — see ErrConfigOrWiring's doc comment for
// why that distinction is load-bearing (W-C model-runtime review, finding
// under M-3: the previous string-match classifier broke silently the moment
// this file's wording changed, and no test caught it because the only test
// exercising the classifier constructed its fixture error by hand instead of
// routing a real error through this file).
//
// All six of this file's credential-pipeline error sites go through this
// helper rather than calling fmt.Errorf directly, so there is exactly one
// place that attaches the sentinel — a seventh site that forgot to wrap it
// would be the only way to reopen the gap, and TestCmdAuthErrorsCarryConfigOrWiringSentinel
// (cmdauth_test.go) drives each of them through the real function and checks
// errors.Is on the result, not on hand-written error text.
func authCommandErrf(format string, args ...any) error {
	return fmt.Errorf(format+": %w", append(args, ctxcompact.ErrConfigOrWiring)...)
}

// runAuthCommand is DefaultAuthCommandRunner's implementation, split into a
// named function so the var above stays swappable in tests.
//
// # Why this builds a FRESH context instead of using ctx directly
//
// ctx here is whatever reached buildOne's constructed model's Generate/Stream
// — in production that is the orchestrator turn's context, which carries
// toolreg's registered-name set (bound by bindExecutionContext so
// tools.Authorize can refuse a tool name nothing answers to; see
// internal/toolreg's package doc). That check exists to catch a MODEL-
// reachable phantom name — a hallucinated tool call or a spec-builder bug
// naming a tool that was never registered, the exact bug class that once
// left every github_* tool dead (see ghSpec's doc comment in
// internal/tools/github.go). authCommandTool is not, and structurally cannot
// be, that: it is a fixed constant, and argv comes from
// config.ProviderAuthConfig.Command, resolved once in buildOne, never from
// model output or a tool call. There is no ADK tool.BaseTool for it to be
// registered as (this spawn happens inside the model-calling layer itself,
// not inside a tool handler), so leaving it on the turn's context would make
// every configured auth.command fail closed with "not a registered tool" in
// exactly the shape that bug already took once.
//
// So this spawns against a new context rooted at context.Background(), and
// explicitly re-attaches only the two things it actually needs: the
// secproc.Factory the turn already validated (read back via
// secproc.FromContext — NOT the same shape as the deleted
// "read factory, fall back to raw exec" anti-pattern W-B-02 removed, since
// there is no fallback path here: no factory means Launch fails closed,
// full stop) and this function's own purpose-built profile, bound via
// guard.WithProfile rather than tools.WithProfile — eino cannot import
// internal/tools (that import, tried first, closed an import cycle: tools'
// own coverage test agent_dag_cov_test.go and internal/agent/rlm's
// rlm_cov_test.go — both internal `package tools`/`package rlm` test files,
// not external `_test` packages — import eino for FakeModel, and tools
// imports rlm in production, so eino->tools closed the loop the moment a
// test binary was compiled; `go build ./...` stayed clean throughout because
// it never compiles test files, only `go vet`/`go test` caught it).
// guard.WithProfile/ProfileFromContext (internal/guard/profilectx.go) is the
// same key/getter/setter tools.Authorize reads, moved to the leaf package
// both eino and tools can import without a cycle; tools.WithProfile is now a
// thin re-export of it, so every existing caller of tools.WithProfile is
// unaffected. The cost is real and accepted: a spawn in flight does not
// abort early if the whole turn is cancelled, only after authCommandTimeout.
// Everything guard actually evaluates for this spec — the tool-name
// allowlist, the (absent) FS/shell/net dimensions — is unaffected, and
// AllowEnv/UseSandboxTier come from the spec below, not from context.
func runAuthCommand(ctx context.Context, argv []string) (string, error) {
	if len(argv) == 0 {
		return "", authCommandErrf("eino: auth.command has no program to run")
	}

	spawnCtx, cancel := context.WithTimeout(context.Background(), authCommandTimeout)
	defer cancel()
	if f, ok := secproc.FromContext(ctx); ok {
		spawnCtx = secproc.WithFactory(spawnCtx, f)
	}
	spawnCtx = guard.WithProfile(spawnCtx, authCommandProfile())

	// AllowEnv is deliberately left empty: the credential-fetch program is
	// exactly the class of untrusted external command
	// SecureProcessSpec.AllowEnv documents as getting "no credentials at
	// all" by default — it has no claim on yanshi's own provider API keys.
	//
	// UseSandboxTier is FullAccess: unlike shell_run or a delegated
	// sub-agent, an auth.command is plausibly reading host credential state
	// (~/.aws, ~/.config/gcloud, a keychain socket, a corporate SSO daemon
	// on localhost) that WorkspaceWrite's workspace-confined view would
	// block. It mirrors tools.ghSpec's reasoning for the same tier.
	//
	// secproc.Launch directly (not tools.LaunchSecureProcess): see
	// DefaultAuthCommandRunner's doc comment. The Authorize firewall is still
	// enforced — tools.init registers internal/tools.Authorize as
	// secproc.Launch's Authorizer process-wide, independent of which package
	// calls Launch.
	started, err := secproc.Launch(spawnCtx, secproc.SecureProcessSpec{
		Tool:           authCommandTool,
		Program:        argv[0],
		Args:           argv[1:],
		UseSandboxTier: sandbox.FullAccess,
	})
	if err != nil {
		return "", authCommandErrf("eino: auth.command launch: %w", err)
	}

	// B-1: drain BOTH stdout and stderr to EOF concurrently, BEFORE Wait.
	// The previous version here read stdout in full, called Wait, and only
	// THEN conditionally read stderr on a non-zero exit — exactly the
	// single-stream-first shape secproc.WaitDrained's doc comment names as
	// a deadlock risk (the unread stream's OS pipe buffer fills, the child
	// blocks writing to it, and the child therefore never reaches the exit
	// the stdout read is waiting for) as well as a read-after-close race on
	// a fast exit. internal/tools' runSecureCaptureOnce already had this
	// right; WaitDrained is the shared extraction of that implementation
	// (internal/secproc/capture.go) so this is the second consumer, not a
	// third reimplementation. Bounded with secproc.CaptureLimit for the
	// same reason runSecureCaptureOnce bounds its captures: a runaway
	// auth-command helper must not grow this capture without limit.
	stdout := secproc.NewBoundedCapture(secproc.CaptureLimit)
	stderr := secproc.NewBoundedCapture(secproc.CaptureLimit)
	waitErr, drainErr := secproc.WaitDrained(started, stdout, stderr)
	if drainErr != nil {
		return "", authCommandErrf("eino: auth.command %q: %w", argv[0], drainErr)
	}
	stdoutText, _, _ := stdout.Snapshot()
	stderrText, _, _ := stderr.Snapshot()
	if waitErr != nil {
		return "", authCommandErrf("eino: auth.command %q failed: %w (stderr: %s)",
			argv[0], waitErr, strings.TrimSpace(stderrText))
	}
	token := strings.TrimSpace(stdoutText)
	if token == "" {
		return "", authCommandErrf("eino: auth.command %q produced an empty credential", argv[0])
	}
	return token, nil
}

// CommandTokenSource caches the credential auth.command produces, re-running
// the command when the cache is older than refresh or when a caller forces a
// refresh (the 401 path in authRefreshTransport).
//
// It has no stale-token fallback on a runner error, by design: a broken
// credential helper should surface as a request failure, not a silently
// stale (and possibly already-revoked) token retried forever.
type CommandTokenSource struct {
	argv      []string
	refresh   time.Duration
	runner    AuthCommandRunner
	registrar SecretRegistrar

	mu      sync.Mutex
	token   string
	fetched time.Time
}

// NewCommandTokenSource builds a source for argv, refreshing at most every
// refresh. A nil runner defaults to DefaultAuthCommandRunner; tests pass a
// fake to avoid spawning a real process. registrars is optional (variadic so
// the many existing call sites that do not exercise B-2 keep compiling
// unchanged) — the first non-nil element, if any, has every fetched token
// registered with it; see SecretRegistrar's doc comment.
func NewCommandTokenSource(argv []string, refresh time.Duration, runner AuthCommandRunner, registrars ...SecretRegistrar) *CommandTokenSource {
	if runner == nil {
		runner = DefaultAuthCommandRunner
	}
	return &CommandTokenSource{argv: argv, refresh: refresh, runner: runner, registrar: firstRegistrar(registrars)}
}

// Token returns the cached credential if it is within refresh of when it was
// last fetched, otherwise runs the command again.
func (c *CommandTokenSource) Token(ctx context.Context) (string, error) {
	return c.fetch(ctx, false)
}

// Refresh unconditionally re-runs the command, bypassing the cache. It is
// what authRefreshTransport calls after a 401 — the cached token might still
// be within its refresh window, but the provider has just said it no longer
// accepts it.
func (c *CommandTokenSource) Refresh(ctx context.Context) (string, error) {
	return c.fetch(ctx, true)
}

func (c *CommandTokenSource) fetch(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.token != "" && time.Since(c.fetched) < c.refresh {
		return c.token, nil
	}
	tok, err := c.runner(ctx, c.argv)
	if err != nil {
		return "", err
	}
	// B-2: register BEFORE returning to any caller. This single registration
	// protects the token wherever its exact text later appears — including a
	// stderr-derived error from a LATER failed refresh (a stale or just-
	// revoked copy of this same token can show up in the helper's own
	// diagnostic output on a 401 retry) — because Redactor.Redact matches
	// substrings regardless of which stream the text originally arrived on;
	// there is no need to separately register anything read from stderr.
	if c.registrar != nil {
		c.registrar.Register(tok)
	}
	c.token = tok
	c.fetched = time.Now()
	return c.token, nil
}

// authRefreshTransport injects a CommandTokenSource-managed credential into
// every outgoing request's header (bearer prefixes "Bearer "; raw does not —
// see the per-kind wiring in provider.go's buildOne) and retries a request
// exactly once, with a force-refreshed token, when the wrapped RoundTripper
// answers 401.
//
// The retry lives here rather than in ResilientChatModel (the chat-completion
// retry authority, see resilient.go) on purpose: a 401 from a STATIC bad API
// key is correctly non-retryable (isNonRetryableClientErr classifies it as a
// real client error), but a 401 from a STALE command-sourced token is a
// completely different failure that a fresh token fixes. Handling it inside
// the transport keeps that distinction invisible to every caller above it —
// go-openai, anthropic.go, responses.go and ResilientChatModel all see one
// final response per logical call — and the retry does not consume
// MaxRetries or wait out the backoff schedule, because a stale token is not a
// transient failure to wait out.
type authRefreshTransport struct {
	next   http.RoundTripper
	source *CommandTokenSource
	header string
	bearer bool
}

// RoundTrip implements http.RoundTripper.
func (t *authRefreshTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}

	// Buffer the body upfront so it can be replayed on the 401 retry — Body
	// is a single-use stream, and req.Clone does not duplicate its content.
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("eino: buffering request body for auth refresh: %w", err)
		}
		body = b
	}

	resp, err := t.attempt(req, body, next, false)
	if err != nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	_ = resp.Body.Close()
	return t.attempt(req, body, next, true)
}

// attempt clones req, sets the credential header from a (possibly forced)
// token fetch, restores the buffered body, and delegates to next.
func (t *authRefreshTransport) attempt(req *http.Request, body []byte, next http.RoundTripper, force bool) (*http.Response, error) {
	tok, err := t.source.fetch(req.Context(), force)
	if err != nil {
		return nil, authCommandErrf("eino: fetching auth.command credential: %w", err)
	}
	clone := req.Clone(req.Context())
	if body != nil {
		clone.Body = io.NopCloser(bytes.NewReader(body))
		clone.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		clone.ContentLength = int64(len(body))
	}
	v := tok
	if t.bearer {
		v = "Bearer " + tok
	}
	clone.Header.Set(t.header, v)
	return next.RoundTrip(clone)
}

// newAuthRefreshClient builds an *http.Client whose transport wraps next with
// authRefreshTransport. next may be nil (defaults to http.DefaultTransport
// inside RoundTrip).
func newAuthRefreshClient(next http.RoundTripper, source *CommandTokenSource, header string, bearer bool) *http.Client {
	return &http.Client{Transport: &authRefreshTransport{next: next, source: source, header: header, bearer: bearer}}
}

// authRefreshHTTPTransport is buildOne's per-kind wiring point: it wraps base
// in an authRefreshTransport sourced from auth (W-C-12) when auth is
// non-nil, and returns base UNCHANGED when it is nil — so a provider that
// does not configure auth.command gets a transport chain byte-identical to
// the one buildOne built before this feature existed. header/bearer encode
// where and how each adapter kind carries its credential (anthropic:
// "x-api-key", raw; openai / openai-responses: "Authorization", "Bearer "
// prefix) — see buildOne's three call sites. registrar (B-2) is forwarded to
// NewCommandTokenSource unchanged; nil disables registration, matching every
// other soft-degradation default in this package.
func authRefreshHTTPTransport(base http.RoundTripper, auth *config.ProviderAuthConfig, header string, bearer bool, registrar SecretRegistrar) http.RoundTripper {
	if auth == nil {
		return base
	}
	source := NewCommandTokenSource(auth.Command, auth.RefreshInterval, nil, registrar)
	return &authRefreshTransport{next: base, source: source, header: header, bearer: bearer}
}
