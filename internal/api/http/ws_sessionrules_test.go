package http

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/tools"
)

// fakeRuleRecorder counts what recordSessionApproval asks for. Counting rather
// than asserting on a rule set is deliberate: this file's job is the WIRING —
// that an answered prompt reaches the orchestrator at all — and the semantics
// of what the orchestrator then does are pinned in
// internal/agent/orchestrator/sessionrules_test.go against the real guard.
type fakeRuleRecorder struct {
	approved []string
	demoted  []string
	sessions []string
}

func (f *fakeRuleRecorder) ApproveShellForSession(sid, cmd string) bool {
	f.sessions = append(f.sessions, sid)
	f.approved = append(f.approved, cmd)
	return true
}

func (f *fakeRuleRecorder) DemoteShellForSession(sid, cmd string) bool {
	f.sessions = append(f.sessions, sid)
	f.demoted = append(f.demoted, cmd)
	return true
}

// TestRecordSessionApproval_Table is the S9 consumer contract.
//
// internal/guard/generalize.go implemented approval generalization completely
// and NOTHING CALLED IT. This function is the caller; if it stops being
// invoked, or stops distinguishing allow from deny, generalization silently
// reverts to the state it spent its whole life in.
func TestRecordSessionApproval_Table(t *testing.T) {
	cases := []struct {
		name         string
		req          tools.PermissionRequest
		decision     tools.PermissionDecision
		wantApproved []string
		wantDemoted  []string
		why          string
	}{
		{
			name:         "allow widens",
			req:          tools.PermissionRequest{Tool: "shell_run", Shell: "go test ./internal/a"},
			decision:     tools.PermissionAllow,
			wantApproved: []string{"go test ./internal/a"},
			why:          "the user said yes to this command shape",
		},
		{
			name:         "always_allow widens",
			req:          tools.PermissionRequest{Tool: "shell_run", Shell: "go build ./..."},
			decision:     tools.PermissionAlwaysAllow,
			wantApproved: []string{"go build ./..."},
		},
		{
			name:         "allow_session widens",
			req:          tools.PermissionRequest{Tool: "shell_run", Shell: "go vet ./..."},
			decision:     tools.PermissionAllowSession,
			wantApproved: []string{"go vet ./..."},
		},
		{
			name:         "allow_persistent widens",
			req:          tools.PermissionRequest{Tool: "shell_run", Shell: "make check"},
			decision:     tools.PermissionAllowPersistent,
			wantApproved: []string{"make check"},
		},
		{
			name:        "deny demotes",
			req:         tools.PermissionRequest{Tool: "shell_run", Shell: "go test ./internal/b"},
			decision:    tools.PermissionDeny,
			wantDemoted: []string{"go test ./internal/b"},
			why:         "a refusal inside a widened family is evidence the widening was wrong",
		},
		{
			name:        "an unrecognised decision demotes",
			req:         tools.PermissionRequest{Tool: "shell_run", Shell: "go test ./x"},
			decision:    tools.PermissionDecision("gibberish"),
			wantDemoted: []string{"go test ./x"},
			why:         "anything that is not an explicit allow is not an allow",
		},
		{
			name:     "a non-shell tool records nothing",
			req:      tools.PermissionRequest{Tool: "fs_write"},
			decision: tools.PermissionAllow,
			why:      "a generalized rule is an execpolicy rule; it has nothing to say about fs_write",
		},
		{
			name: "a force-prompt tool records nothing",
			req: tools.PermissionRequest{
				Tool: "shell_run", Shell: "go test ./x", ForcePrompt: true,
			},
			decision: tools.PermissionAllow,
			why:      "force-prompt means ask EVERY time; a rule would be a standing grant it must not have",
		},
		{
			name: "an approval-required tool records nothing",
			req: tools.PermissionRequest{
				Tool: "shell_run", Shell: "go test ./x", ApprovalRequired: true,
			},
			decision: tools.PermissionAllow,
			why:      "same",
		},
		{
			name: "a forced destructive request records nothing",
			req: tools.PermissionRequest{
				Tool: "shell_run", Shell: "rm -rf ./build", Force: true,
			},
			decision: tools.PermissionAllow,
			why:      "RequireApproval marks calls that must always reach a human",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &fakeRuleRecorder{}
			recordSessionApproval(rec, "ws-1", tc.req, tc.decision)
			assert.Equal(t, tc.wantApproved, rec.approved, tc.why)
			assert.Equal(t, tc.wantDemoted, rec.demoted, tc.why)
		})
	}
}

// TestRecordSessionApprovalNeedsARecorderAndASession. Both guards exist for
// the same reason: without a place to release the rules from, recording them
// is a leak rather than a feature.
func TestRecordSessionApprovalNeedsARecorderAndASession(t *testing.T) {
	req := tools.PermissionRequest{Tool: "shell_run", Shell: "go test ./x"}

	// A nil recorder must not panic — the WS handler passes the orchestrator,
	// and a test server may not have one.
	recordSessionApproval(nil, "ws-1", req, tools.PermissionAllow)

	rec := &fakeRuleRecorder{}
	recordSessionApproval(rec, "", req, tools.PermissionAllow)
	assert.Empty(t, rec.approved, "an empty session id names no connection to release from")
	assert.Empty(t, rec.demoted)
}

// TestRecordSessionApprovalCarriesTheSessionID. Every connection has its own
// rule set; recording under the wrong id would let one conversation's approval
// authorize another's commands.
func TestRecordSessionApprovalCarriesTheSessionID(t *testing.T) {
	rec := &fakeRuleRecorder{}
	req := tools.PermissionRequest{Tool: "shell_run", Shell: "go test ./x"}
	recordSessionApproval(rec, "ws-alpha", req, tools.PermissionAllow)
	recordSessionApproval(rec, "ws-beta", req, tools.PermissionDeny)
	require.Equal(t, []string{"ws-alpha", "ws-beta"}, rec.sessions)
}
