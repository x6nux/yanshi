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
		return "", fmt.Errorf("eino: auth.command has no program to run")
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
		return "", fmt.Errorf("eino: auth.command launch: %w", err)
	}

	out, readErr := io.ReadAll(started.Stdout)
	waitErr := started.Wait()
	if waitErr != nil {
		stderr, _ := io.ReadAll(started.Stderr)
		return "", fmt.Errorf("eino: auth.command %q failed: %w (stderr: %s)",
			argv[0], waitErr, strings.TrimSpace(string(stderr)))
	}
	if readErr != nil {
		return "", fmt.Errorf("eino: auth.command %q: reading stdout: %w", argv[0], readErr)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("eino: auth.command %q produced an empty credential", argv[0])
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
	argv    []string
	refresh time.Duration
	runner  AuthCommandRunner

	mu      sync.Mutex
	token   string
	fetched time.Time
}

// NewCommandTokenSource builds a source for argv, refreshing at most every
// refresh. A nil runner defaults to DefaultAuthCommandRunner; tests pass a
// fake to avoid spawning a real process.
func NewCommandTokenSource(argv []string, refresh time.Duration, runner AuthCommandRunner) *CommandTokenSource {
	if runner == nil {
		runner = DefaultAuthCommandRunner
	}
	return &CommandTokenSource{argv: argv, refresh: refresh, runner: runner}
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
		return nil, fmt.Errorf("eino: fetching auth.command credential: %w", err)
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
// prefix) — see buildOne's three call sites.
func authRefreshHTTPTransport(base http.RoundTripper, auth *config.ProviderAuthConfig, header string, bearer bool) http.RoundTripper {
	if auth == nil {
		return base
	}
	source := NewCommandTokenSource(auth.Command, auth.RefreshInterval, nil)
	return &authRefreshTransport{next: base, source: source, header: header, bearer: bearer}
}
