// internal/llm/eino/cmdauth_test.go
//
// W-C-12: does auth.command actually produce a credential through secproc,
// cache it for refresh_interval, re-run exactly once on a 401, and fail
// closed the same way every other secproc caller does? Four claims, four
// groups of tests below:
//
//   - CommandTokenSource caching/refresh — a fake AuthCommandRunner is enough
//     here: the claim under test is CommandTokenSource's own cache logic, not
//     the spawn underneath it (covered separately).
//   - runAuthCommand's secproc routing — the security-critical half. A
//     denied/absent Authorizer or Factory must fail closed exactly like any
//     other secproc.Launch caller, the SecureProcessSpec fields must match
//     the security contract cmdauth.go's own doc comments promise
//     (Tool/UseSandboxTier/AllowEnv), and — the load-bearing assertion — a
//     REAL OS subprocess's REAL stdout must become the returned token, not a
//     fake Factory's canned return value standing in for one. That last test
//     reuses the self-reinvoking-test-binary pattern established in
//     internal/tools/secproc_capture_test.go (TestSecureCaptureHelperProcess)
//     and internal/tools/secproc_capture_prod_test.go
//     (TestRunSecureCaptureWithProductionFactory), which is the only
//     cross-platform way to prove a real spawn without shelling out to
//     /bin/sh or echo.exe.
//   - authRefreshTransport — header injection (bearer vs raw), and the
//     401-triggers-exactly-one-forced-refresh contract.
//   - authRefreshHTTPTransport — the nil-auth passthrough buildOne relies on
//     to keep an unconfigured provider's transport chain byte-identical to
//     what it was before this feature existed.
package eino

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/ctxcompact"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
	"github.com/x6nux/yanshi/internal/secrets"
	"github.com/x6nux/yanshi/internal/shell"
)

// fakeAuthCommandRunner is a thread-safe AuthCommandRunner test double:
// records every argv it was called with, hands back canned
// (token, err) pairs in call order (the last entry repeats once exhausted),
// and counts calls. Used by every CommandTokenSource test in this file —
// those tests are about the cache's OWN decision of when to call the runner,
// not about what running a real command does (that is runAuthCommand's job,
// tested separately below against the real secproc path).
type fakeAuthCommandRunner struct {
	mu      sync.Mutex
	results []struct {
		tok string
		err error
	}
	calls int
	argvs [][]string
}

func newFakeAuthCommandRunner(tokens ...string) *fakeAuthCommandRunner {
	f := &fakeAuthCommandRunner{}
	for _, tok := range tokens {
		f.results = append(f.results, struct {
			tok string
			err error
		}{tok: tok})
	}
	return f
}

func (f *fakeAuthCommandRunner) run(_ context.Context, argv []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.argvs = append(f.argvs, argv)
	f.calls++
	if len(f.results) == 0 {
		return "", fmt.Errorf("fakeAuthCommandRunner: no canned result for call %d", f.calls)
	}
	idx := f.calls - 1
	if idx >= len(f.results) {
		idx = len(f.results) - 1 // last entry repeats
	}
	r := f.results[idx]
	return r.tok, r.err
}

func (f *fakeAuthCommandRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// allowAuthCommandAuthorizer is a minimal secproc.Authorizer for this file's
// tests. It evaluates the SAME guard.Check runAuthCommand's real caller
// (tools.Authorize, via secproc's registered Authorizer) evaluates, against
// the profile bound by ctx (runAuthCommand always binds authCommandProfile()
// via guard.WithProfile — see that function's doc comment) — it is real
// guard logic, not a fixed "always allow" stub, so a genuinely denied
// profile in a test still comes out denied. The only thing it skips is
// tools.Authorize's extra machinery (toolreg's registered-name check, the
// interactive approval-manager callback): cmdauth.go's fixed, code-defined
// authCommandTool never reaches either of those (see authCommandProfile's
// doc comment for why), so skipping them here does not paper over anything
// this package's own runtime depends on.
func allowAuthCommandAuthorizer(ctx context.Context, action guard.Action, _ string) error {
	prof, ok := guard.ProfileFromContext(ctx)
	if !ok {
		return errors.New("allowAuthCommandAuthorizer: no permission profile bound")
	}
	d := guard.New().Check(prof, action)
	if d.Verdict != guard.Allow {
		return fmt.Errorf("allowAuthCommandAuthorizer: denied (%v): %s", d.Verdict, d.Reason)
	}
	return nil
}

// denyingAuthorizer always refuses, for the fail-closed propagation test.
func denyingAuthorizer(context.Context, guard.Action, string) error {
	return errors.New("denyingAuthorizer: refused by test")
}

// withSwappedAuthorizer installs a for restored automatically at test end,
// via t.Cleanup — every test that touches secproc's process-wide Authorizer
// uses this instead of hand-rolled defer/restore so a t.Fatal partway
// through still leaves the global clean for the next test in this binary.
func withSwappedAuthorizer(t *testing.T, a secproc.Authorizer) {
	t.Helper()
	prev := secproc.SwapAuthorizer(a)
	t.Cleanup(func() { secproc.SwapAuthorizer(prev) })
}

// ---- CommandTokenSource: caching / refresh_interval ----

// TestCommandTokenSource_CachesWithinRefreshWindow pins the "按
// refresh_interval 刷新" acceptance clause's cache-hit half: two Token()
// calls inside one refresh window must not re-run the command.
func TestCommandTokenSource_CachesWithinRefreshWindow(t *testing.T) {
	runner := newFakeAuthCommandRunner("tok-1", "tok-2")
	src := NewCommandTokenSource([]string{"get-token"}, time.Hour, runner.run)

	got1, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("first Token(): %v", err)
	}
	got2, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("second Token(): %v", err)
	}
	if got1 != "tok-1" || got2 != "tok-1" {
		t.Fatalf("got %q, %q — want both calls to return the cached first token", got1, got2)
	}
	if n := runner.callCount(); n != 1 {
		t.Fatalf("runner called %d times, want exactly 1 (second Token() should have hit the cache)", n)
	}
}

// TestCommandTokenSource_RefetchesAfterRefreshWindow pins the refresh half:
// once refresh has elapsed, the NEXT Token() call re-runs the command rather
// than serving a token that may already be stale.
func TestCommandTokenSource_RefetchesAfterRefreshWindow(t *testing.T) {
	runner := newFakeAuthCommandRunner("tok-1", "tok-2")
	src := NewCommandTokenSource([]string{"get-token"}, 15*time.Millisecond, runner.run)

	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("first Token(): %v", err)
	}
	time.Sleep(40 * time.Millisecond)
	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("second Token(): %v", err)
	}
	if got != "tok-2" {
		t.Fatalf("got %q, want tok-2 — the command should have been re-run once refresh elapsed", got)
	}
	if n := runner.callCount(); n != 2 {
		t.Fatalf("runner called %d times, want exactly 2", n)
	}
}

// TestCommandTokenSource_RefreshBypassesCache pins the 401 path's
// prerequisite: Refresh forces a re-run even when the cached token is still
// within its window — because a 401 means the PROVIDER, not the clock, has
// decided the cached token is no longer good.
func TestCommandTokenSource_RefreshBypassesCache(t *testing.T) {
	runner := newFakeAuthCommandRunner("tok-1", "tok-2")
	src := NewCommandTokenSource([]string{"get-token"}, time.Hour, runner.run)

	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("Token(): %v", err)
	}
	got, err := src.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh(): %v", err)
	}
	if got != "tok-2" {
		t.Fatalf("Refresh() = %q, want tok-2 — a well-within-window cached token must not shadow a forced refresh", got)
	}
	if n := runner.callCount(); n != 2 {
		t.Fatalf("runner called %d times, want exactly 2 (initial fetch + forced refresh)", n)
	}
	// A subsequent plain Token() must now serve the REFRESHED value from
	// cache, not re-run a third time.
	got, err = src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() after Refresh(): %v", err)
	}
	if got != "tok-2" || runner.callCount() != 2 {
		t.Fatalf("Token() after Refresh() = %q (calls=%d), want tok-2 served from cache with no 3rd run",
			got, runner.callCount())
	}
}

// TestCommandTokenSource_RunnerErrorIsNotCached proves a failed run does not
// poison the source: no token is cached (so the field stays empty rather
// than "" masquerading as a fetched-but-empty credential — see
// CommandTokenSource's own doc comment on having no stale-token fallback),
// and the very next call retries rather than replaying the error forever.
func TestCommandTokenSource_RunnerErrorIsNotCached(t *testing.T) {
	calls := 0
	runner := func(context.Context, []string) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("boom: credential helper is down")
		}
		return "tok-recovered", nil
	}
	src := NewCommandTokenSource([]string{"get-token"}, time.Hour, runner)

	if _, err := src.Token(context.Background()); err == nil {
		t.Fatal("first Token(): want an error from the failing runner")
	}
	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("second Token(): %v", err)
	}
	if got != "tok-recovered" || calls != 2 {
		t.Fatalf("got %q (calls=%d), want tok-recovered after exactly 2 runner calls", got, calls)
	}
}

// recordingRegistrar is a SecretRegistrar test double that records every
// secret it is asked to register, for the B-2 assertions below.
type recordingRegistrar struct {
	mu       sync.Mutex
	recorded []string
}

func (r *recordingRegistrar) Register(secret string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorded = append(r.recorded, secret)
}

func (r *recordingRegistrar) has(secret string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.recorded {
		if s == secret {
			return true
		}
	}
	return false
}

// TestCommandTokenSource_RegistersFetchedTokenWithRegistrar is W-C-12 review
// finding B-2's direct fix verification: before this existed, a token
// auth.command produced was the only dynamic credential in the provider
// stack never handed to a SecretRegistrar, so it would appear verbatim in
// any log line or transcript that happened to echo it back. Each ACTUAL
// runner call (an initial fetch and a forced Refresh — NOT a cache hit,
// which never calls the runner and so has nothing new to register) must
// register the token it produced.
func TestCommandTokenSource_RegistersFetchedTokenWithRegistrar(t *testing.T) {
	runner := newFakeAuthCommandRunner("tok-1", "tok-2")
	reg := &recordingRegistrar{}
	src := NewCommandTokenSource([]string{"get-token"}, time.Hour, runner.run, reg)

	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("Token(): %v", err)
	}
	if !reg.has("tok-1") {
		t.Fatalf("registrar recorded %v, want it to contain tok-1 after the first fetch", reg.recorded)
	}

	// A cache hit must not call the runner again, so it has no new token to
	// register — the registrar's recorded list must stay exactly as it was.
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("second Token(): %v", err)
	}
	if len(reg.recorded) != 1 {
		t.Fatalf("registrar recorded %v after a cache hit, want still just [tok-1]", reg.recorded)
	}

	if _, err := src.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh(): %v", err)
	}
	if !reg.has("tok-2") {
		t.Fatalf("registrar recorded %v, want it to also contain tok-2 after the forced refresh", reg.recorded)
	}
}

// TestCommandTokenSource_NilRegistrarDoesNotPanic proves the optional
// registrar is genuinely optional: NewCommandTokenSource called with no
// trailing SecretRegistrar argument at all (the shape every pre-B-2 call
// site in this file still uses) must not panic on a nil-check miss.
func TestCommandTokenSource_NilRegistrarDoesNotPanic(t *testing.T) {
	runner := newFakeAuthCommandRunner("tok-1")
	src := NewCommandTokenSource([]string{"get-token"}, time.Hour, runner.run)
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("Token(): %v", err)
	}
}

// TestCommandTokenSource_RegisteredTokenIsRedactedWhereverItLaterAppears is
// B-2's integration-level proof: it registers a fetched token with a REAL
// secrets.Redactor (not just a recording test double) and confirms the
// Redactor scrubs that exact text out of an arbitrary later string — the
// property the design actually depends on, since secrets.Redactor.Redact
// matches by substring regardless of which stream (a stdout token vs. a
// stderr diagnostic that happens to echo a stale copy of the same token on
// a later failed refresh) the text originally arrived on. A single
// registration on the successful stdout fetch is therefore enough to
// protect BOTH channels; this test is what makes that claim checkable
// rather than merely asserted in a comment.
func TestCommandTokenSource_RegisteredTokenIsRedactedWhereverItLaterAppears(t *testing.T) {
	const canaryToken = "wc12-b2-canary-secret-4a1f9c"
	runner := newFakeAuthCommandRunner(canaryToken)
	redactor := secrets.NewRedactor()
	src := NewCommandTokenSource([]string{"get-token"}, time.Hour, runner.run, redactor)

	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("Token(): %v", err)
	}

	// The token itself, appearing on what would be the stdout channel.
	if got := redactor.Redact("stdout: " + canaryToken); strings.Contains(got, canaryToken) {
		t.Fatalf("Redact(stdout occurrence) = %q, still contains the canary", got)
	}
	// The SAME token text, now appearing in a simulated stderr diagnostic
	// from a later failed refresh — the crossover scenario B-2 exists for.
	stderrMsg := fmt.Sprintf("auth.command failed: token %s was rejected (401)", canaryToken)
	if got := redactor.Redact(stderrMsg); strings.Contains(got, canaryToken) {
		t.Fatalf("Redact(stderr occurrence) = %q, still contains the canary — one registration must protect both channels", got)
	}
}

// ---- runAuthCommand: the secproc routing contract ----

// TestRunAuthCommand_EmptyArgvErrors is the one input-validation edge
// runAuthCommand owns before it ever touches secproc.
func TestRunAuthCommand_EmptyArgvErrors(t *testing.T) {
	_, err := runAuthCommand(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "no program") {
		t.Fatalf("err = %v, want a \"no program\" error", err)
	}
}

// TestRunAuthCommand_FailsClosedWithoutAuthorizer pins the security floor
// runAuthCommand shares with every other secproc.Launch caller: a process
// that never registered an Authorizer (or, as simulated here, one that has
// been explicitly cleared) must not spawn anything.
func TestRunAuthCommand_FailsClosedWithoutAuthorizer(t *testing.T) {
	withSwappedAuthorizer(t, nil)
	ctx := secproc.WithFactory(context.Background(), &specCapturingFactory{})
	_, err := runAuthCommand(ctx, []string{"get-token"})
	if err == nil || !errors.Is(err, secproc.ErrNoAuthorizer) {
		t.Fatalf("err = %v, want secproc.ErrNoAuthorizer (fail-closed)", err)
	}
}

// TestRunAuthCommand_FailsClosedWithoutFactory pins the other half: an
// Authorizer that would allow the spawn is not enough on its own — no
// Factory in the context this function was CALLED with means no Factory
// gets attached to the fresh spawnCtx it builds either (see runAuthCommand's
// doc comment on secproc.FromContext(ctx)), so Launch fails closed exactly
// as it does for any other caller with a missing Factory.
func TestRunAuthCommand_FailsClosedWithoutFactory(t *testing.T) {
	withSwappedAuthorizer(t, allowAuthCommandAuthorizer)
	_, err := runAuthCommand(context.Background(), []string{"get-token"})
	if err == nil || !strings.Contains(err.Error(), "Factory") {
		t.Fatalf("err = %v, want a \"no Factory\" fail-closed error", err)
	}
}

// TestRunAuthCommand_DeniedProfilePropagatesError proves the Authorize
// firewall genuinely gates this spawn rather than being consulted and
// ignored: a denying Authorizer must stop the spawn before any Factory is
// touched, and its reason must survive into the error runAuthCommand
// returns (not be swallowed into a generic failure).
func TestRunAuthCommand_DeniedProfilePropagatesError(t *testing.T) {
	withSwappedAuthorizer(t, denyingAuthorizer)
	factory := &specCapturingFactory{}
	ctx := secproc.WithFactory(context.Background(), factory)
	_, err := runAuthCommand(ctx, []string{"get-token"})
	if err == nil || !strings.Contains(err.Error(), "refused by test") {
		t.Fatalf("err = %v, want it to carry the Authorizer's denial reason", err)
	}
	if factory.started.Load() {
		t.Fatal("Factory.Start was called despite a denying Authorizer — the firewall did not gate the spawn")
	}
}

// specCapturingFactory is an in-memory secproc.Factory test double: it
// records the exact SecureProcessSpec it was asked to start (for asserting
// the security-contract fields runAuthCommand's own doc comments promise —
// Tool/UseSandboxTier/AllowEnv/Env) and hands back a StartedProcess backed
// by in-memory readers, no real process. Distinct from the real-subprocess
// test below on purpose: this one is about what runAuthCommand PUTS INTO the
// spec, which an in-memory fake can assert on directly; the real-subprocess
// test is about what comes back OUT of a genuine spawn, which a fake return
// value cannot prove either way.
type specCapturingFactory struct {
	mu      sync.Mutex
	spec    secproc.SecureProcessSpec
	started atomic.Bool

	stdout   string
	stderr   string
	waitErr  error
	startErr error
}

func (f *specCapturingFactory) Start(_ context.Context, spec secproc.SecureProcessSpec) (*secproc.StartedProcess, error) {
	f.mu.Lock()
	f.spec = spec
	f.mu.Unlock()
	f.started.Store(true)
	if f.startErr != nil {
		return nil, f.startErr
	}
	return &secproc.StartedProcess{
		Wait:   func() error { return f.waitErr },
		PID:    4242,
		Stdout: strings.NewReader(f.stdout),
		Stderr: strings.NewReader(f.stderr),
	}, nil
}

func (f *specCapturingFactory) lastSpec() secproc.SecureProcessSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spec
}

var _ secproc.Factory = (*specCapturingFactory)(nil)

// TestRunAuthCommand_SpecFieldsMatchSecurityContract pins every field
// cmdauth.go's doc comments promise about the SecureProcessSpec an
// auth.command spawn is launched with: Tool is the fixed authCommandTool
// constant (what the profile's allowlist actually names — see
// authCommandProfile), UseSandboxTier is FullAccess (mirrors ghSpec's
// reasoning for reading host credential state), AllowEnv and Env are both
// empty (the credential-fetch program gets none of yanshi's own provider
// keys), and Program/Args come from argv unmodified.
func TestRunAuthCommand_SpecFieldsMatchSecurityContract(t *testing.T) {
	withSwappedAuthorizer(t, allowAuthCommandAuthorizer)
	factory := &specCapturingFactory{stdout: "canary-token"}
	ctx := secproc.WithFactory(context.Background(), factory)

	got, err := runAuthCommand(ctx, []string{"aws-get-token", "--profile", "prod"})
	if err != nil {
		t.Fatalf("runAuthCommand: %v", err)
	}
	if got != "canary-token" {
		t.Fatalf("token = %q, want canary-token", got)
	}

	spec := factory.lastSpec()
	if spec.Tool != authCommandTool {
		t.Errorf("Tool = %q, want %q", spec.Tool, authCommandTool)
	}
	if spec.UseSandboxTier != sandbox.FullAccess {
		t.Errorf("UseSandboxTier = %v, want sandbox.FullAccess", spec.UseSandboxTier)
	}
	if len(spec.AllowEnv) != 0 {
		t.Errorf("AllowEnv = %v, want empty — auth.command must not inherit yanshi's own provider credentials", spec.AllowEnv)
	}
	if len(spec.Env) != 0 {
		t.Errorf("Env = %v, want empty — no explicit env, only the scrubbed host baseline", spec.Env)
	}
	if spec.Program != "aws-get-token" {
		t.Errorf("Program = %q, want aws-get-token", spec.Program)
	}
	if len(spec.Args) != 2 || spec.Args[0] != "--profile" || spec.Args[1] != "prod" {
		t.Errorf("Args = %v, want [--profile prod]", spec.Args)
	}
}

// TestRunAuthCommand_NonZeroExitReturnsStderrInError proves a failed
// credential command surfaces as a request failure (per CommandTokenSource's
// no-stale-fallback design) with the child's stderr attached, not a bare
// "exit status 1" that hides why the credential fetch failed.
//
// This spawns a REAL child process rather than using specCapturingFactory's
// strings.Reader-backed Stdout/Stderr (W-C-12 review finding M-1; review
// checklist D3, "fake wider than reality"): a strings.Reader stays readable
// no matter when — or whether — it is read relative to Wait, so a fake built
// on one cannot tell a correct drain-before-Wait implementation apart from a
// buggy read-after-Wait one; neither ordering can make it block or return
// stale data. A real OS pipe can — reading it after the reaper has already
// reaped the child risks "file already closed" instead of the bytes the
// child wrote, which is exactly the bug secproc.WaitDrained's
// drain-before-Wait ordering fixes (see runAuthCommand). Only a genuine
// child process, not an in-memory stand-in, can prove stderr still reaches
// the returned error under real pipe semantics.
func TestRunAuthCommand_NonZeroExitReturnsStderrInError(t *testing.T) {
	const stderrCanary = "aws: not logged in (wc12-real-subprocess-stderr-canary)"
	t.Setenv(cmdauthHelperEnvFlag, "1")
	t.Setenv(cmdauthHelperStdoutFile, "")
	t.Setenv("YANSHI_CMDAUTH_HELPER_STDERR_MSG", stderrCanary)
	t.Setenv("YANSHI_CMDAUTH_HELPER_EXIT", "1")

	withSwappedAuthorizer(t, allowAuthCommandAuthorizer)
	ctx := secproc.WithFactory(context.Background(), shell.UnsandboxedSecureFactory())

	argv := []string{os.Args[0], "-test.run=^TestCmdAuthHelperExitProcess$", "--"}
	_, err := DefaultAuthCommandRunner(ctx, argv)
	if err == nil || !strings.Contains(err.Error(), stderrCanary) {
		t.Fatalf("err = %v, want it to contain the real child's stderr %q", err, stderrCanary)
	}
}

// TestRunAuthCommand_EmptyStdoutErrors: a command that exits clean but
// prints nothing must not be treated as "successfully produced an empty
// credential" — that credential would fail every subsequent request with a
// generic auth error, with no signal pointing back at the real cause.
func TestRunAuthCommand_EmptyStdoutErrors(t *testing.T) {
	withSwappedAuthorizer(t, allowAuthCommandAuthorizer)
	factory := &specCapturingFactory{stdout: "   \n  "} // whitespace-only
	ctx := secproc.WithFactory(context.Background(), factory)

	_, err := runAuthCommand(ctx, []string{"aws-get-token"})
	if err == nil || !strings.Contains(err.Error(), "empty credential") {
		t.Fatalf("err = %v, want an \"empty credential\" error", err)
	}
}

// brokenStreamFactory is a secproc.Factory test double whose Stdout stream
// errors on read. It exists to drive runAuthCommand's WaitDrained/drainErr
// site — the one authCommandErrf call site none of this file's other test
// doubles can reach, because specCapturingFactory's Stdout/Stderr are always
// strings.Reader, which never fails a read.
type brokenStreamFactory struct{}

func (brokenStreamFactory) Start(context.Context, secproc.SecureProcessSpec) (*secproc.StartedProcess, error) {
	return &secproc.StartedProcess{
		Wait:   func() error { return nil },
		Stdout: iotest.ErrReader(errors.New("broken stdout stream (test)")),
		Stderr: strings.NewReader(""),
	}, nil
}

var _ secproc.Factory = brokenStreamFactory{}

// TestCmdAuthErrorsCarryConfigOrWiringSentinel drives all six of
// authCommandErrf's call sites (see its doc comment for the list) through
// the real production functions that produce them — never a hand-written
// error string standing in for one — and checks two things about each
// resulting error:
//
//  1. errors.Is(err, ctxcompact.ErrConfigOrWiring) — the mechanism
//     internal/ctxcompact's isConfigOrWiringFailure actually classifies on.
//     This is the fix under test: it must survive ANY future rewording of
//     this file's error text, including the exact mutation the W-C
//     model-runtime review used to prove the previous
//     strings.Contains(err.Error(), "auth.command")-based classifier was
//     silently unprotected — renaming every occurrence of the literal
//     "eino: auth.command" in this file to "eino: credential command".
//  2. err.Error() still contains "auth.command" — a wording pin, checked
//     separately from (1) on purpose. Unlike (1), this assertion is NOT
//     meant to survive that mutation: it is supposed to go red the moment
//     someone makes that exact rename, so a human notices the observable
//     error text changed and updates it on purpose rather than by accident.
//     A green (1) with a red (2) after that mutation is the correct
//     outcome for this test, not a regression in it.
func TestCmdAuthErrorsCarryConfigOrWiringSentinel(t *testing.T) {
	check := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("err = nil, want a config-or-wiring error")
		}
		if !errors.Is(err, ctxcompact.ErrConfigOrWiring) {
			t.Errorf("errors.Is(err, ctxcompact.ErrConfigOrWiring) = false for err = %v", err)
		}
		if !strings.Contains(err.Error(), "auth.command") {
			t.Errorf("err.Error() = %q, does not contain %q", err.Error(), "auth.command")
		}
	}

	t.Run("no program to run", func(t *testing.T) {
		_, err := runAuthCommand(context.Background(), nil)
		check(t, err)
	})

	t.Run("launch fails without a Factory", func(t *testing.T) {
		withSwappedAuthorizer(t, allowAuthCommandAuthorizer)
		_, err := runAuthCommand(context.Background(), []string{"get-token"})
		check(t, err)
	})

	t.Run("drain fails on a broken stdout stream", func(t *testing.T) {
		withSwappedAuthorizer(t, allowAuthCommandAuthorizer)
		ctx := secproc.WithFactory(context.Background(), brokenStreamFactory{})
		_, err := runAuthCommand(ctx, []string{"aws-get-token"})
		check(t, err)
	})

	t.Run("non-zero exit", func(t *testing.T) {
		t.Setenv(cmdauthHelperEnvFlag, "1")
		t.Setenv(cmdauthHelperStdoutFile, "")
		t.Setenv("YANSHI_CMDAUTH_HELPER_STDERR_MSG", "boom (sentinel test)")
		t.Setenv("YANSHI_CMDAUTH_HELPER_EXIT", "1")
		withSwappedAuthorizer(t, allowAuthCommandAuthorizer)
		ctx := secproc.WithFactory(context.Background(), shell.UnsandboxedSecureFactory())
		argv := []string{os.Args[0], "-test.run=^TestCmdAuthHelperExitProcess$", "--"}
		_, err := DefaultAuthCommandRunner(ctx, argv)
		check(t, err)
	})

	t.Run("empty stdout", func(t *testing.T) {
		withSwappedAuthorizer(t, allowAuthCommandAuthorizer)
		factory := &specCapturingFactory{stdout: "   \n  "}
		ctx := secproc.WithFactory(context.Background(), factory)
		_, err := runAuthCommand(ctx, []string{"aws-get-token"})
		check(t, err)
	})

	t.Run("token refresh fetch fails", func(t *testing.T) {
		fake := newFakeAuthCommandRunner() // no canned results: run() errors on first call
		source := NewCommandTokenSource([]string{"get-token"}, time.Hour, fake.run)
		transport := &authRefreshTransport{source: source, header: "Authorization", bearer: true}
		req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1.invalid/", nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = transport.attempt(req, nil, http.DefaultTransport, false)
		check(t, err)
	})
}

// ---- runAuthCommand against a REAL OS subprocess ----
//
// cmdauthHelperEnvFlag/cmdauthHelperStdoutFileEnv gate and feed
// TestCmdAuthHelperProcess, the re-exec'd helper the test below uses to
// prove runAuthCommand's spawn is a genuine child process whose genuine
// stdout becomes the returned token — not a fake Factory's canned return
// value standing in for one. Mirrors internal/tools/secproc_capture_test.go
// and secproc_capture_prod_test.go's established pattern: env-flag-gated
// no-op under a normal `go test` run (so this file's own
// TestCmdAuthHelperProcess entry, which every `go test` invocation also
// runs directly, does nothing), payload delivered via a temp file rather
// than the env value itself (avoids Windows' small per-variable env size
// limit), portable across windows/darwin/linux because it never shells out
// to /bin/sh or echo.exe — it just re-execs this same test binary.
const (
	cmdauthHelperEnvFlag    = "YANSHI_CMDAUTH_HELPER"
	cmdauthHelperStdoutFile = "YANSHI_CMDAUTH_HELPER_STDOUT_FILE"
)

// TestCmdAuthHelperProcess is a normal member of this package's test suite
// AND the re-exec'd helper TestWC12_RunAuthCommandSpawnsARealProcess uses.
// Under a plain `go test ./internal/llm/eino/...` (no env flag set) it is a
// no-op, exactly like every other test in this file.
func TestCmdAuthHelperProcess(t *testing.T) {
	if os.Getenv(cmdauthHelperEnvFlag) != "1" {
		return
	}
	if p := os.Getenv(cmdauthHelperStdoutFile); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_, _ = os.Stdout.Write(b)
		}
	}
	os.Exit(0)
}

// TestWC12_RunAuthCommandSpawnsARealProcessAndCapturesStdout is this file's
// load-bearing test: it drives DefaultAuthCommandRunner — the exact function
// var buildOne wires into authRefreshHTTPTransport — through the REAL
// production secproc.Factory (shell.UnsandboxedSecureFactory, the same one
// orchestrator.bindExecutionContext falls back to when no factory is
// configured) and a real guard-backed Authorizer, and asserts the token it
// returns is the ACTUAL stdout of an ACTUAL child process this test spawned
// moments ago — not a value any test double supplied. This is the
// mandated verification style for a spec-only claim ("the command's stdout
// becomes the credential"): let a real value flow through the real path,
// then check what came out the other end.
func TestWC12_RunAuthCommandSpawnsARealProcessAndCapturesStdout(t *testing.T) {
	const canaryToken = "wc12-real-subprocess-canary-7e21ac4d"
	outPath := filepath.Join(t.TempDir(), "stdout.txt")
	if err := os.WriteFile(outPath, []byte(canaryToken+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(cmdauthHelperEnvFlag, "1")
	t.Setenv(cmdauthHelperStdoutFile, outPath)

	withSwappedAuthorizer(t, allowAuthCommandAuthorizer)
	ctx := secproc.WithFactory(context.Background(), shell.UnsandboxedSecureFactory())

	argv := []string{os.Args[0], "-test.run=^TestCmdAuthHelperProcess$", "--"}
	got, err := DefaultAuthCommandRunner(ctx, argv)
	if err != nil {
		t.Fatalf("DefaultAuthCommandRunner: %v", err)
	}
	if got != canaryToken {
		t.Fatalf("token = %q, want %q — the real child process's real stdout did not become the credential",
			got, canaryToken)
	}
}

// TestWC12_RunAuthCommandRealProcessNonZeroExitFails is the negative control
// for the test above: a real child process that exits non-zero must fail
// runAuthCommand, proving the exit-status check runs against the genuine
// secproc reaper (StartedProcess.Wait) and not just against an in-memory
// fake. It also asserts the specific exit status (W-C-12 review finding
// N-3) rather than only err != nil: a bare "err is non-nil" assertion would
// stay green even if the real exit code got lost or replaced by a generic
// "auth.command failed" placeholder on the way into the returned error —
// naming "exit status 3" pins that the genuine child's genuine exit status
// survives.
func TestWC12_RunAuthCommandRealProcessNonZeroExitFails(t *testing.T) {
	t.Setenv(cmdauthHelperEnvFlag, "1")
	t.Setenv(cmdauthHelperStdoutFile, "") // no file: helper prints nothing, then exits via the flag below
	t.Setenv("YANSHI_CMDAUTH_HELPER_EXIT", "3")

	withSwappedAuthorizer(t, allowAuthCommandAuthorizer)
	ctx := secproc.WithFactory(context.Background(), shell.UnsandboxedSecureFactory())

	argv := []string{os.Args[0], "-test.run=^TestCmdAuthHelperExitProcess$", "--"}
	_, err := DefaultAuthCommandRunner(ctx, argv)
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("err = %v, want it to name the real exit status (exit status 3)", err)
	}
}

// TestCmdAuthHelperExitProcess is TestWC12_RunAuthCommandRealProcessNonZeroExitFails's
// AND TestRunAuthCommand_NonZeroExitReturnsStderrInError's helper: it exits
// with the code named by YANSHI_CMDAUTH_HELPER_EXIT, first writing
// YANSHI_CMDAUTH_HELPER_STDERR_MSG (if set) to stderr. Leaving the stderr
// message unset (as TestWC12_RunAuthCommandRealProcessNonZeroExitFails does)
// proves the failure path does not depend on stdout OR stderr being
// non-empty; setting it (as TestRunAuthCommand_NonZeroExitReturnsStderrInError
// does) proves a real child's real stderr, read through a real OS pipe,
// survives into runAuthCommand's returned error.
func TestCmdAuthHelperExitProcess(t *testing.T) {
	if os.Getenv(cmdauthHelperEnvFlag) != "1" {
		return
	}
	if msg := os.Getenv("YANSHI_CMDAUTH_HELPER_STDERR_MSG"); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
	code := 0
	if v := os.Getenv("YANSHI_CMDAUTH_HELPER_EXIT"); v != "" {
		switch v {
		case "3":
			code = 3
		default:
			code = 1
		}
	}
	os.Exit(code)
}

// ---- authRefreshTransport: header injection ----

// TestAuthRefreshTransport_InjectsRawHeader pins the "raw" half of the
// header/bearer wiring buildOne uses for the anthropic kind ("x-api-key",
// bearer=false): the token goes on the wire with no "Bearer " prefix.
func TestAuthRefreshTransport_InjectsRawHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	runner := func(context.Context, []string) (string, error) { return "raw-token-1", nil }
	source := NewCommandTokenSource([]string{"get-token"}, time.Hour, runner)
	client := newAuthRefreshClient(nil, source, "x-api-key", false)

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got != "raw-token-1" {
		t.Errorf("x-api-key = %q, want %q — a raw header must carry the bare token, no Bearer prefix", got, "raw-token-1")
	}
}

// TestAuthRefreshTransport_InjectsBearerHeader pins the "bearer" half, used
// by buildOne for the openai / openai-responses kinds ("Authorization",
// bearer=true).
func TestAuthRefreshTransport_InjectsBearerHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	runner := func(context.Context, []string) (string, error) { return "bearer-token-1", nil }
	source := NewCommandTokenSource([]string{"get-token"}, time.Hour, runner)
	client := newAuthRefreshClient(nil, source, "Authorization", true)

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if want := "Bearer bearer-token-1"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}

// TestAuthRefreshTransport_401TriggersExactlyOneForcedRefresh is the core
// contract authRefreshTransport's doc comment promises: a 401 forces exactly
// one Refresh (bypassing CommandTokenSource's cache) and exactly one retry,
// and the retry's request carries the freshly-fetched token, not the stale
// one that just got refused.
func TestAuthRefreshTransport_401TriggersExactlyOneForcedRefresh(t *testing.T) {
	var reqCount atomic.Int32
	var mu sync.Mutex
	var headers []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := reqCount.Add(1)
		mu.Lock()
		headers = append(headers, r.Header.Get("Authorization"))
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	fake := newFakeAuthCommandRunner("stale-token", "fresh-token")
	source := NewCommandTokenSource([]string{"get-token"}, time.Hour, fake.run)
	client := newAuthRefreshClient(nil, source, "Authorization", true)

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("final status = %d, want 200 after the forced refresh", resp.StatusCode)
	}
	if got := reqCount.Load(); got != 2 {
		t.Fatalf("server saw %d requests, want exactly 2 (one 401 + one forced-refresh retry)", got)
	}
	if fake.callCount() != 2 {
		t.Fatalf("runner called %d times, want exactly 2 (initial cache-miss fetch + forced refresh)", fake.callCount())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(headers) != 2 || headers[0] != "Bearer stale-token" || headers[1] != "Bearer fresh-token" {
		t.Fatalf("headers seen by server = %v, want [%q %q] — the retry must carry the refreshed token, not repeat the refused one",
			headers, "Bearer stale-token", "Bearer fresh-token")
	}
}

// TestAuthRefreshTransport_DoesNotRetryATwiceRefusedToken is the negative
// control for the test above: when even the refreshed token is refused, the
// 401 must surface to the caller rather than retrying forever.
func TestAuthRefreshTransport_DoesNotRetryATwiceRefusedToken(t *testing.T) {
	var reqCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	fake := newFakeAuthCommandRunner("token-1", "token-2", "token-3")
	source := NewCommandTokenSource([]string{"get-token"}, time.Hour, fake.run)
	client := newAuthRefreshClient(nil, source, "Authorization", true)

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("final status = %d, want 401 — a still-refused refreshed token must surface, not be swallowed", resp.StatusCode)
	}
	if got := reqCount.Load(); got != 2 {
		t.Fatalf("server saw %d requests, want exactly 2 — RoundTrip must not attempt a third time", got)
	}
	if fake.callCount() != 2 {
		t.Fatalf("runner called %d times, want exactly 2 — no third fetch when the retry itself is refused", fake.callCount())
	}
}

// ---- authRefreshHTTPTransport: buildOne's per-kind wiring point ----

// TestAuthRefreshHTTPTransport_NilAuthReturnsBaseUnchanged pins the
// unconfigured-provider path: with auth == nil, buildOne's transport chain
// must be byte-identical (literally the same value) to what it was before
// W-C-12 existed — not merely "an equivalent transport".
func TestAuthRefreshHTTPTransport_NilAuthReturnsBaseUnchanged(t *testing.T) {
	base := &http.Transport{}
	got := authRefreshHTTPTransport(base, nil, "x-api-key", false, nil)
	if got != http.RoundTripper(base) {
		t.Fatalf("authRefreshHTTPTransport(base, nil, ...) returned a different value than base; want it unchanged")
	}
}

// TestAuthRefreshHTTPTransport_NonNilAuthWrapsBase confirms the configured
// path preserves base as the wrapped transport's next hop and carries the
// header/bearer wiring through unchanged.
func TestAuthRefreshHTTPTransport_NonNilAuthWrapsBase(t *testing.T) {
	base := &http.Transport{}
	auth := &config.ProviderAuthConfig{Command: []string{"get-token"}, RefreshInterval: time.Minute}
	got := authRefreshHTTPTransport(base, auth, "x-api-key", false, nil)
	wrapped, ok := got.(*authRefreshTransport)
	if !ok {
		t.Fatalf("authRefreshHTTPTransport(base, non-nil auth, ...) = %T, want *authRefreshTransport", got)
	}
	if wrapped.next != http.RoundTripper(base) {
		t.Fatal("wrapped.next != base — the configured base transport was not preserved as the next hop")
	}
	if wrapped.header != "x-api-key" || wrapped.bearer != false {
		t.Fatalf("wrapped header/bearer = %q/%v, want %q/%v", wrapped.header, wrapped.bearer, "x-api-key", false)
	}
}
