package http

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/approval"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"

	"net/http/httptest"
)

// TestChatWS_PermissionsListAndRevokeRoundTrip closes the middle of the
// "用户可查看撤销" evidence chain.
//
// Both halves were already covered and neither half touched the join: the TUI
// test asserts /permissions emits permissions_list and permission_revoke
// frames, and approval.Manager's own tests assert List and Revoke behave. The
// two WS-layer tests that existed drove the NIL-manager path, where list is
// empty by construction and revoke is expected to error -- so if the handler
// stopped calling Manager.Revoke entirely, every one of those tests still
// passed and the user could no longer revoke anything.
//
// This drives a real manager end to end: record a rule, see it in the list,
// revoke it over the wire, see it gone.
//
// ledger: A1/S07#3 用户可查看撤销
func TestChatWS_PermissionsListAndRevokeRoundTrip(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)

	mgr, err := approval.New(memKV{}, "test-proc", nil)
	require.NoError(t, err)

	srv := New(Config{Token: "t", Approvals: mgr})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	// A persistent rule is visible to every connection, which is what lets the
	// test record it outside the connection's own session id.
	require.NoError(t, mgr.Record("any-session", approval.Rule{
		ID:        "rule-under-test",
		Action:    "shell_run",
		Scope:     approval.Scope{Tool: "shell_run", Program: "go"},
		TTL:       approval.TTLPersistent,
		Source:    approval.SourceUser,
		ExpiresAt: time.Now().Add(time.Hour),
	}))

	require.NoError(t, c.WriteJSON(proto.NewListPermissions()))
	f := readFrame(t, c)
	require.Equal(t, "permissions", f.Type)
	found := false
	for _, p := range f.Permissions {
		if p.ID == "rule-under-test" {
			found = true
		}
	}
	assert.True(t, found, "a recorded rule must be visible to the user: %+v", f.Permissions)

	require.NoError(t, c.WriteJSON(proto.NewRevokePermission("rule-under-test")))
	f = readFrame(t, c)
	assert.NotEqual(t, "error", f.Type, "revoking an existing rule must not error: %+v", f)

	require.NoError(t, c.WriteJSON(proto.NewListPermissions()))
	f = readFrame(t, c)
	require.Equal(t, "permissions", f.Type)
	for _, p := range f.Permissions {
		assert.NotEqual(t, "rule-under-test", p.ID, "the revoked rule is still listed")
	}
}

// memKV is the smallest thing satisfying approval.KV: a persistent rule needs
// somewhere to persist, and the point of this test is the WS handler, not the
// store. A fake rather than a mock, per the repo convention.
type memKV map[string]string

func (m memKV) KVGet(k string) (string, bool, error) {
	v, ok := m[k]
	return v, ok, nil
}
func (m memKV) KVSet(k, v string) error { m[k] = v; return nil }
