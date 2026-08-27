package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/secrets"
)

// ---------------------------------------------------------------------------
// S6: permission decisions get a durable sink
// ---------------------------------------------------------------------------

func TestAppendPermissionAudit_RoundTrip(t *testing.T) {
	s, _ := openTempStore(t)
	require.NoError(t, s.AppendPermissionAudit(PermissionAudit{
		TS: 1000, SessionID: "sess-1", AgentID: "coder",
		Tool: "shell_run", Decision: "allow", Source: "mode_override",
		ReasonCode: "", CmdDigest: "shell: rm -rf build/",
	}))

	got, err := s.QueryPermissionAudit(PermissionAuditQuery{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, int64(1000), got[0].TS)
	assert.Equal(t, "sess-1", got[0].SessionID)
	assert.Equal(t, "coder", got[0].AgentID)
	assert.Equal(t, "shell_run", got[0].Tool)
	assert.Equal(t, "allow", got[0].Decision)
	assert.Equal(t, "mode_override", got[0].Source)
	assert.Equal(t, "shell: rm -rf build/", got[0].CmdDigest)
	assert.NotZero(t, got[0].ID)
}

// TestAppendPermissionAudit_Redacts is the reason the digest field is allowed
// to exist at all. Tool arguments carry credentials often enough that an
// un-redacted audit table would be a credential dump with a timestamp column.
func TestAppendPermissionAudit_Redacts(t *testing.T) {
	s, _ := openTempStore(t)
	r := secrets.NewRedactor()
	r.Register("ghp-tokenvalue123")
	s.SetRedactor(r)

	require.NoError(t, s.AppendPermissionAudit(PermissionAudit{
		Tool: "shell_run", Decision: "allow",
		CmdDigest: "shell: curl -H 'Authorization: ghp-tokenvalue123' https://x",
	}))
	got, err := s.QueryPermissionAudit(PermissionAuditQuery{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.NotContains(t, got[0].CmdDigest, "ghp-tokenvalue123")
	assert.Contains(t, got[0].CmdDigest, "REDACTED")
}

// TestAppendPermissionAudit_TruncatesDigest: the digest is a diagnostic aid,
// not evidence. Without the cap the audit table becomes a second copy of every
// tool argument ever passed.
func TestAppendPermissionAudit_TruncatesDigest(t *testing.T) {
	s, _ := openTempStore(t)
	require.NoError(t, s.AppendPermissionAudit(PermissionAudit{
		Tool: "shell_run", Decision: "deny",
		CmdDigest: "shell: " + strings.Repeat("x", maxDigestBytes*3),
	}))
	got, err := s.QueryPermissionAudit(PermissionAuditQuery{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.LessOrEqual(t, len(got[0].CmdDigest), maxDigestBytes)
}

// TestAppendPermissionAudit_RequiresTool: a row that cannot name the tool it
// judged is not an audit record.
func TestAppendPermissionAudit_RequiresTool(t *testing.T) {
	s, _ := openTempStore(t)
	require.Error(t, s.AppendPermissionAudit(PermissionAudit{Decision: "allow"}))
	got, err := s.QueryPermissionAudit(PermissionAuditQuery{})
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestAppendPermissionAudit_DefaultsTimestamp: a caller that does not supply a
// time still produces a timestamped row, so "when" is never blank.
func TestAppendPermissionAudit_DefaultsTimestamp(t *testing.T) {
	s, _ := openTempStore(t)
	require.NoError(t, s.AppendPermissionAudit(PermissionAudit{Tool: "fs_read", Decision: "allow"}))
	got, err := s.QueryPermissionAudit(PermissionAuditQuery{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.NotZero(t, got[0].TS)
}

// TestAppendPermissionAudit_NoSessionForeignKey: the SSE path has no session,
// and a structural refusal can happen before a session exists. Audit rows must
// not require one — a foreign key here would silently drop exactly the records
// nobody else is capturing.
func TestAppendPermissionAudit_NoSessionForeignKey(t *testing.T) {
	s, _ := openTempStore(t)
	require.NoError(t, s.AppendPermissionAudit(PermissionAudit{
		SessionID: "a-session-that-does-not-exist",
		Tool:      "shell_run", Decision: "deny", Source: "no_callback",
	}))
	got, err := s.QueryPermissionAudit(PermissionAuditQuery{})
	require.NoError(t, err)
	assert.Len(t, got, 1)
}

func seedAudit(t *testing.T, s *Store) {
	t.Helper()
	rows := []PermissionAudit{
		{TS: 100, SessionID: "s1", AgentID: "a1", Tool: "shell_run", Decision: "allow", Source: "static_profile"},
		{TS: 200, SessionID: "s1", AgentID: "a1", Tool: "fs_write", Decision: "deny", Source: "hard_deny"},
		{TS: 300, SessionID: "s2", AgentID: "a2", Tool: "shell_run", Decision: "allow", Source: "mode_override"},
		{TS: 400, SessionID: "s2", AgentID: "a1", Tool: "web_fetch", Decision: "deny", Source: "interactive"},
	}
	for _, r := range rows {
		require.NoError(t, s.AppendPermissionAudit(r))
	}
}

func TestQueryPermissionAudit(t *testing.T) {
	cases := []struct {
		name   string
		q      PermissionAuditQuery
		wantTS []int64
	}{
		{"zero query is newest-first across everything", PermissionAuditQuery{}, []int64{400, 300, 200, 100}},
		{"by session", PermissionAuditQuery{SessionID: "s1"}, []int64{200, 100}},
		{"by agent", PermissionAuditQuery{AgentID: "a1"}, []int64{400, 200, 100}},
		{"by tool", PermissionAuditQuery{Tool: "shell_run"}, []int64{300, 100}},
		{"by decision", PermissionAuditQuery{Decision: "deny"}, []int64{400, 200}},
		{"since is inclusive", PermissionAuditQuery{Since: 300}, []int64{400, 300}},
		{"until is exclusive", PermissionAuditQuery{Until: 300}, []int64{200, 100}},
		{"time window", PermissionAuditQuery{Since: 200, Until: 400}, []int64{300, 200}},
		{"limit truncates the newest end first", PermissionAuditQuery{Limit: 2}, []int64{400, 300}},
		{"combined dimensions", PermissionAuditQuery{SessionID: "s2", Decision: "allow"}, []int64{300}},
		{"no match", PermissionAuditQuery{Tool: "nonexistent"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTempStore(t)
			seedAudit(t, s)
			got, err := s.QueryPermissionAudit(tc.q)
			require.NoError(t, err)
			var ts []int64
			for _, r := range got {
				ts = append(ts, r.TS)
			}
			assert.Equal(t, tc.wantTS, ts)
		})
	}
}

// TestQueryPermissionAudit_LimitIsCapped: an audit query is an operator tool,
// but it is still bounded — an unbounded default would load every decision ever
// made into memory the first time anyone asks.
func TestQueryPermissionAudit_LimitIsCapped(t *testing.T) {
	s, _ := openTempStore(t)
	for i := 0; i < MaxAuditPageSize+10; i++ {
		require.NoError(t, s.AppendPermissionAudit(PermissionAudit{
			TS: int64(i + 1), Tool: "fs_read", Decision: "allow",
		}))
	}
	got, err := s.QueryPermissionAudit(PermissionAuditQuery{Limit: 1 << 20})
	require.NoError(t, err)
	assert.Len(t, got, MaxAuditPageSize)

	got, err = s.QueryPermissionAudit(PermissionAuditQuery{})
	require.NoError(t, err)
	assert.Len(t, got, DefaultAuditPageSize)
}

// TestPermissionAudit_MigratesOntoLegacyDatabase: the table is new, so an
// existing database must grow it on open rather than failing every write.
func TestPermissionAudit_MigratesOntoLegacyDatabase(t *testing.T) {
	path := writeLegacyDB(t)
	s, err := Open(path)
	require.NoError(t, err)
	defer s.Close()

	require.NoError(t, s.AppendPermissionAudit(PermissionAudit{
		Tool: "shell_run", Decision: "allow", Source: "static_profile",
	}))
	got, err := s.QueryPermissionAudit(PermissionAuditQuery{})
	require.NoError(t, err)
	assert.Len(t, got, 1)
}
