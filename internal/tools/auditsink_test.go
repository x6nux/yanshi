package tools

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/toolreg"
)

// recordingSink is a fake (not a mock) PermissionAuditSink: it keeps what it is
// given so a test can assert on the records themselves rather than on call
// counts. failWith makes StoreAuditSink's error path reachable.
type recordingSink struct {
	mu      sync.Mutex
	records []PermissionAuditRecord
}

func (r *recordingSink) Record(_ context.Context, rec PermissionAuditRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec)
}

func (r *recordingSink) all() []PermissionAuditRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]PermissionAuditRecord(nil), r.records...)
}

// installSink registers a sink for the duration of one test and restores the
// previous state afterwards. The sink is process-wide (see auditsink.go), so
// every test that touches it must clean up or it leaks into the next one.
func installSink(t *testing.T, sink PermissionAuditSink) {
	t.Helper()
	SetPermissionAuditSink(sink)
	t.Cleanup(func() { SetPermissionAuditSink(nil) })
}

// authCtx builds the context Authorize needs: a profile, a registered tool set
// (S8 refuses unregistered names), and the identity values. The sink itself is
// installed separately because it is not a context value.
func authCtx(t *testing.T, sink PermissionAuditSink, tools []string, sessionID, agentID string) context.Context {
	t.Helper()
	installSink(t, sink)
	ctx := WithProfile(context.Background(), allowAllProfile())
	ctx = toolreg.WithRegistered(ctx, tools)
	if sessionID != "" {
		ctx = WithApprovalManager(ctx, &approval.Manager{}, sessionID)
	}
	if agentID != "" {
		ctx = WithVCS(ctx, VCSScope{Agent: agentID})
	}
	return ctx
}

// TestAuthorize_ReachesTheAuditSink is the whole point of S6: the record
// auditPermission has always built now arrives somewhere durable. Before the
// sink existed this assertion had nothing to observe — the record went to slog
// and vanished.
func TestAuthorize_ReachesTheAuditSink(t *testing.T) {
	sink := &recordingSink{}
	ctx := authCtx(t, sink, []string{"fs_read"}, "sess-7", "coder")

	require.NoError(t, Authorize(ctx, guard.Action{
		Tool: "fs_read",
		FS:   guard.FSWant{Op: "read", Paths: []string{"/work/main.go"}},
	}, `{"path":"/work/main.go"}`))

	recs := sink.all()
	require.Len(t, recs, 1)
	assert.Equal(t, "fs_read", recs[0].Tool)
	assert.Equal(t, "allow", recs[0].Decision)
	assert.Equal(t, "static_profile", recs[0].Source)
	assert.Equal(t, "sess-7", recs[0].SessionID)
	assert.Equal(t, "coder", recs[0].AgentID)
	assert.Contains(t, recs[0].CmdDigest, "/work/main.go")
}

// TestAuthorize_AuditsDenialsToo: an audit trail that only records approvals
// answers the least interesting half of the question.
func TestAuthorize_AuditsDenials(t *testing.T) {
	cases := []struct {
		name           string
		ctx            func(t *testing.T, sink PermissionAuditSink) context.Context
		action         guard.Action
		wantDecision   string
		wantSource     string
		wantReasonCode string
	}{
		{
			name: "unregistered tool (S8 structural refusal)",
			ctx: func(t *testing.T, sink PermissionAuditSink) context.Context {
				return authCtx(t, sink, []string{"fs_read"}, "s", "a")
			},
			action:         guard.Action{Tool: "fs_mkdir"},
			wantDecision:   "deny",
			wantSource:     "unregistered_tool",
			wantReasonCode: "not_registered",
		},
		{
			name: "no profile bound (fail-closed)",
			ctx: func(t *testing.T, sink PermissionAuditSink) context.Context {
				installSink(t, sink)
				return toolreg.WithRegistered(context.Background(), []string{"fs_read"})
			},
			action:         guard.Action{Tool: "fs_read"},
			wantDecision:   "deny",
			wantSource:     "fail_closed",
			wantReasonCode: "missing_profile",
		},
		{
			// INF1 (ADR-0004 supplement) narrowed the structural class from
			// "any control metacharacter" to "structure the segmenter cannot
			// read", so `ls && rm -rf /tmp/x` is no longer the example: it is
			// now split and graded (and refused, but by the destructive gate).
			// A command substitution still is.
			name: "structural hard deny (unreadable shell structure)",
			ctx: func(t *testing.T, sink PermissionAuditSink) context.Context {
				return authCtx(t, sink, []string{"shell_run"}, "s", "a")
			},
			action:         guard.Action{Tool: "shell_run", Shell: "ls $(whoami)"},
			wantDecision:   "deny",
			wantSource:     "hard_deny",
			wantReasonCode: "firewall",
		},
		{
			// The chained form still audits as a structural refusal, just via
			// the deletion gate rather than the metacharacter scan. Keeping
			// both rows is what proves the class did not shrink.
			name: "structural hard deny (catastrophic segment in a chain)",
			ctx: func(t *testing.T, sink PermissionAuditSink) context.Context {
				return authCtx(t, sink, []string{"shell_run"}, "s", "a")
			},
			action:         guard.Action{Tool: "shell_run", Shell: "ls && rm -rf /"},
			wantDecision:   "deny",
			wantSource:     "hard_deny",
			wantReasonCode: "firewall",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &recordingSink{}
			err := Authorize(tc.ctx(t, sink), tc.action, "{}")
			require.Error(t, err)
			recs := sink.all()
			require.Len(t, recs, 1)
			assert.Equal(t, tc.wantDecision, recs[0].Decision)
			assert.Equal(t, tc.wantSource, recs[0].Source)
			assert.Equal(t, tc.wantReasonCode, recs[0].ReasonCode)
		})
	}
}

// TestAuthorize_AuditsAutoApprovedShellCommand is the record the S6 motivating
// question needs: under yolo/auto nobody is asked, so the digest of the command
// is the only way to answer "which rm did it approve".
func TestAuthorize_AuditsAutoApprovedShellCommand(t *testing.T) {
	sink := &recordingSink{}
	ctx := authCtx(t, sink, []string{"shell_run"}, "night-session", "autopilot")

	require.NoError(t, Authorize(ctx, guard.Action{
		Tool: "shell_run", Shell: "rm -rf build/", Workdir: "/work",
	}, `{"cmd":"rm -rf build/"}`))

	recs := sink.all()
	require.Len(t, recs, 1)
	assert.Equal(t, "allow", recs[0].Decision)
	assert.Contains(t, recs[0].CmdDigest, "rm -rf build/")
	assert.Equal(t, "night-session", recs[0].SessionID)
	assert.Equal(t, "autopilot", recs[0].AgentID)
}

// TestAuthorize_WithoutSinkStillWorks: the sink is optional and its absence is
// the pre-S6 behaviour. A sub-agent or an SSE call with no sink must not fail.
func TestAuthorize_WithoutSinkStillWorks(t *testing.T) {
	installSink(t, nil)
	ctx := WithProfile(context.Background(), allowAllProfile())
	ctx = toolreg.WithRegistered(ctx, []string{"fs_read"})
	require.NoError(t, Authorize(ctx, guard.Action{Tool: "fs_read"}, "{}"))
}

// TestSetPermissionAuditSink_NilClears: passing nil restores the pre-S6 state
// (slog only) rather than installing a typed-nil that would panic on the first
// decision. Clearing must be expressible — a deployment without a store is a
// supported configuration, not an error.
func TestSetPermissionAuditSink_NilClears(t *testing.T) {
	installSink(t, &recordingSink{})
	assert.True(t, PermissionAuditSinkInstalled())
	SetPermissionAuditSink(nil)
	assert.False(t, PermissionAuditSinkInstalled())

	// And Authorize still works with nothing installed.
	ctx := WithProfile(context.Background(), allowAllProfile())
	ctx = toolreg.WithRegistered(ctx, []string{"fs_read"})
	require.NoError(t, Authorize(ctx, guard.Action{Tool: "fs_read"}, "{}"))
}

// TestSetPermissionAuditSink_SwapsImplementations: atomic.Value panics on a
// type change unless the value is boxed, and swapping a production sink for a
// test fake is exactly that. This is the assertion that catches the unboxed
// form, which would only fail on the SECOND install.
func TestSetPermissionAuditSink_SwapsImplementations(t *testing.T) {
	assert.NotPanics(t, func() {
		installSink(t, &recordingSink{})
		SetPermissionAuditSink(&StoreAuditSink{Append: func(PermissionAuditRecord) error { return nil }})
		SetPermissionAuditSink(&recordingSink{})
	})
}

func TestAuditDigest(t *testing.T) {
	cases := []struct {
		name   string
		action guard.Action
		want   string
	}{
		{"shell wins", guard.Action{Shell: "go test ./...",
			FS: guard.FSWant{Op: "read", Paths: []string{"/x"}}}, "shell: go test ./..."},
		{"fs paths", guard.Action{FS: guard.FSWant{Op: "write", Paths: []string{"/a", "/b"}}}, "write: /a /b"},
		{"fs paths with no op", guard.Action{FS: guard.FSWant{Paths: []string{"/a"}}}, "fs: /a"},
		{"net host", guard.Action{NetHost: "example.com"}, "net: example.com"},
		// A plain tool-name check has nothing to summarise, and "" says so
		// honestly rather than inventing a digest.
		{"nothing to summarise", guard.Action{Tool: "time_now"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, auditDigest(tc.action))
		})
	}
}

// TestAuditDigest_Truncates: an unbounded digest would copy every tool argument
// ever passed through the sink interface.
func TestAuditDigest_Truncates(t *testing.T) {
	long := make([]byte, maxAuditDigestBytes*3)
	for i := range long {
		long[i] = 'x'
	}
	got := auditDigest(guard.Action{Shell: string(long)})
	assert.Len(t, got, maxAuditDigestBytes)
}

func TestAuditIdentity(t *testing.T) {
	cases := []struct {
		name              string
		ctx               context.Context
		wantSess, wantAgt string
	}{
		{"nothing bound", context.Background(), "", ""},
		{"approval session", WithApprovalManager(context.Background(), &approval.Manager{}, "s1"), "s1", ""},
		{"thread link fallback", WithThreadLink(context.Background(), "t1", "turn"), "t1", ""},
		{"vcs agent", WithVCS(context.Background(), VCSScope{Agent: "a1"}), "", "a1"},
		{
			name: "approval beats thread link",
			ctx: WithThreadLink(
				WithApprovalManager(context.Background(), &approval.Manager{}, "s1"), "t1", "turn"),
			wantSess: "s1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess, agt := auditIdentity(tc.ctx)
			assert.Equal(t, tc.wantSess, sess)
			assert.Equal(t, tc.wantAgt, agt)
		})
	}
}

// TestStoreAuditSink_SwallowsWriteFailures is the invariant that keeps the
// archive from becoming a new failure mode: a full disk must not turn an
// allowed action into a denied one. The guard's verdict is already made by the
// time Record runs, and Record has no way to change it — which is the point.
func TestStoreAuditSink_SwallowsWriteFailures(t *testing.T) {
	calls := 0
	sink := &StoreAuditSink{Append: func(PermissionAuditRecord) error {
		calls++
		return errors.New("disk full")
	}}
	installSink(t, sink)
	ctx := WithProfile(context.Background(), allowAllProfile())
	ctx = toolreg.WithRegistered(ctx, []string{"fs_read"})

	// The failing sink must not change the verdict.
	require.NoError(t, Authorize(ctx, guard.Action{Tool: "fs_read"}, "{}"))
	assert.Equal(t, 1, calls, "the sink must actually have been consulted")
}

// TestStoreAuditSink_NilAppendIsSafe: a partially-constructed sink must not
// panic inside Authorize.
func TestStoreAuditSink_NilAppendIsSafe(t *testing.T) {
	var nilSink *StoreAuditSink
	assert.NotPanics(t, func() {
		nilSink.Record(context.Background(), PermissionAuditRecord{Tool: "x"})
		(&StoreAuditSink{}).Record(context.Background(), PermissionAuditRecord{Tool: "x"})
	})
}

// TestStoreAuditSink_ForwardsTheRecord closes the adapter loop: what Authorize
// produced is what the store is asked to persist, field for field.
func TestStoreAuditSink_ForwardsTheRecord(t *testing.T) {
	var got PermissionAuditRecord
	sink := &StoreAuditSink{Append: func(rec PermissionAuditRecord) error {
		got = rec
		return nil
	}}
	ctx := authCtx(t, sink, []string{"shell_run"}, "s9", "a9")
	require.NoError(t, Authorize(ctx, guard.Action{
		Tool: "shell_run", Shell: "ls -la",
	}, `{"cmd":"ls -la"}`))

	assert.Equal(t, "shell_run", got.Tool)
	assert.Equal(t, "allow", got.Decision)
	assert.Equal(t, "s9", got.SessionID)
	assert.Equal(t, "a9", got.AgentID)
	assert.Equal(t, "shell: ls -la", got.CmdDigest)
}

// TestAuthorize_AuditNeverCarriesRawArgs: argsJSON is deliberately absent from
// the record. The digest is derived from the ACTION's structured fields, not
// from the model-supplied blob, so a tool that stuffs a credential into an
// unrelated argument does not get it written to the archive.
func TestAuthorize_AuditNeverCarriesRawArgs(t *testing.T) {
	sink := &recordingSink{}
	ctx := authCtx(t, sink, []string{"fs_read"}, "s", "a")
	require.NoError(t, Authorize(ctx, guard.Action{
		Tool: "fs_read",
		FS:   guard.FSWant{Op: "read", Paths: []string{"/work/a.go"}},
	}, `{"path":"/work/a.go","api_key":"sk-should-never-be-recorded"}`))

	recs := sink.all()
	require.Len(t, recs, 1)
	assert.NotContains(t, recs[0].CmdDigest, "sk-should-never-be-recorded")
}
