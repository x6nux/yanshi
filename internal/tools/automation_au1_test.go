package tools_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
)

// readAutomation runs automation_read and decodes the result.
//
// It exists because the tool layer reports refusals in the RESULT STRING with a
// nil error — GuardedTool folds a denial into the payload rather than returning
// a Go error — so `require.NoError` on these calls asserts nothing about
// whether the operation happened.
func readAutomation(t *testing.T, set *tools.AutomationTools, ctx context.Context, id string) (automation.Automation, string) {
	t.Helper()
	raw, err := set.Read.InvokableRun(ctx, mustJSON(t, map[string]any{"id": id}))
	require.NoError(t, err)
	var env struct {
		Automation automation.Automation `json:"automation"`
	}
	if jerr := json.Unmarshal([]byte(raw), &env); jerr != nil {
		return automation.Automation{}, raw
	}
	return env.Automation, raw
}

// TestAutomationLifecycleIsObservedNotJustInvoked replaces an assertion that
// could not fail.
//
// TestAutomationCreateReadUpdatePauseResumeDeleteRun calls update, pause,
// resume and delete with a bare require.NoError and discards every return
// value. GuardedTool.InvokableRun writes refusals into the result string and
// returns err == nil — the same file's TestAutomationToolsDeniedWithoutProfile
// depends on exactly that — so those four steps are structurally blind to the
// operation not having happened at all. Measured: gutting Manager.Update and
// Manager.Delete to `if true { return }` leaves it PASS.
//
// Each step here reads the state back and asserts the change landed.
//
// ledger: C1/AU1#3 生命周期可控
func TestAutomationLifecycleIsObservedNotJustInvoked(t *testing.T) {
	set, _ := setupAutomation(t)
	ctx := withApprovingUser(tools.WithProfile(context.Background(), allowAll(
		"automation_create", "automation_read", "automation_update",
		"automation_pause", "automation_resume", "automation_delete",
	)))

	createResp, err := set.Create.InvokableRun(ctx, mustJSON(t, map[string]any{
		"name": "nightly", "prompt": "do X",
		"schedule_kind": "interval", "schedule": "60",
	}))
	require.NoError(t, err)
	var created automation.Automation
	require.NoError(t, json.Unmarshal([]byte(createResp), &created))
	require.NotEmpty(t, created.ID)

	got, raw := readAutomation(t, set, ctx, created.ID)
	require.Equal(t, "do X", got.Prompt, "read did not return what create stored: %s", raw)
	require.True(t, got.Active, "a freshly created automation is not active, so pause has nothing to change")

	// update — the prompt must actually change.
	_, err = set.Update.InvokableRun(ctx, mustJSON(t, map[string]any{
		"id": created.ID, "prompt": "do Y",
	}))
	require.NoError(t, err)
	got, raw = readAutomation(t, set, ctx, created.ID)
	assert.Equal(t, "do Y", got.Prompt,
		"update reported success but the stored prompt is unchanged: %s", raw)

	// pause — the status must actually change.
	_, err = set.Pause.InvokableRun(ctx, mustJSON(t, map[string]any{"id": created.ID}))
	require.NoError(t, err)
	paused, raw := readAutomation(t, set, ctx, created.ID)
	assert.False(t, paused.Active,
		"pause reported success but the automation is still active: %s", raw)

	// resume — back to where it started.
	_, err = set.Resume.InvokableRun(ctx, mustJSON(t, map[string]any{"id": created.ID}))
	require.NoError(t, err)
	resumed, raw := readAutomation(t, set, ctx, created.ID)
	assert.True(t, resumed.Active,
		"resume did not restore the active state: %s", raw)

	// delete — the record must be gone. Reading a deleted id has to fail; a
	// delete that reports success and leaves the row is the failure this whole
	// test exists for.
	_, err = set.Delete.InvokableRun(ctx, mustJSON(t, map[string]any{"id": created.ID}))
	require.NoError(t, err)
	after, raw := readAutomation(t, set, ctx, created.ID)
	if after.ID == created.ID {
		t.Errorf("delete reported success but the automation is still readable: %s", raw)
	}
}

// TestAutomationsSurviveReopeningTheDatabase is the persistence clause.
//
// TestRepositorySaveLoadRoundTrip saves and loads through ONE Repository on ONE
// :memory: store. That proves the encoder and decoder agree with each other; it
// cannot distinguish "written to the database" from "kept in a field", because
// the same process, the same object and the same in-memory database serve both
// halves. :memory: in particular is discarded when the handle closes, so the
// test could not have observed a restart even if it tried.
//
// This one writes through one handle, CLOSES it, and reopens the same file.
//
// ledger: C1/AU1#4 持久化
func TestAutomationsSurviveReopeningTheDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "automations.db")

	var (
		wantID      string
		wantPrompt  = "survive the restart"
		wantNextRun int64
		wantSched   automation.Schedule
	)

	// First process: create and persist.
	func() {
		s, err := store.Open(path)
		require.NoError(t, err)
		defer func() { require.NoError(t, s.Close()) }()

		repo := automation.NewRepository(s)
		m, err := automation.NewManager(repo, &fakeQueueStub{}, nil)
		require.NoError(t, err)

		a, err := m.Create(automation.CreateInput{
			Name: "nightly", Prompt: wantPrompt,
			Schedule: automation.Schedule{Kind: "interval", IntervalSec: 60},
		})
		require.NoError(t, err)
		wantID, wantSched = a.ID, a.Schedule
		require.NotNil(t, a.NextRunAt, "NextRunAt was never computed, so its survival proves nothing")
		wantNextRun = a.NextRunAt.Unix()
	}()

	// Second process: a fresh store, repository and manager over the same file.
	s, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	m, err := automation.NewManager(automation.NewRepository(s), &fakeQueueStub{}, nil)
	require.NoError(t, err)

	got, _, err := m.Read(wantID)
	require.NoError(t, err, "the automation did not survive reopening the database")
	assert.Equal(t, wantPrompt, got.Prompt)
	assert.Equal(t, wantSched, got.Schedule)
	// The schedule STATE matters as much as the definition: an automation that
	// comes back with a nil NextRunAt either never fires again or fires
	// immediately on the next tick, and both are worse than losing it outright.
	require.NotNil(t, got.NextRunAt, "NextRunAt was lost across the restart")
	assert.Equal(t, wantNextRun, got.NextRunAt.Unix(),
		"NextRunAt was not preserved across the restart")
	assert.True(t, got.Active, "the active flag was not preserved across the restart")
}
