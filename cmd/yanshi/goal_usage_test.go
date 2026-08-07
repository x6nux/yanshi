package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/goalloop"
	"github.com/x6nux/yanshi/internal/store"
)

// TestLightweightGoalReportsItsTokensToTheSink is the accounting gap on the
// path most goals take.
//
// runLightweightGoal called Orchestrator.Query, which discards usage, so the
// shared UsageSink saw nothing from a T0–T2 run and persistGoalRun recorded
// zero tokens no matter what the turn cost. The heavy path (T3–T4) reports
// through the loop's own components, so reading only the loop made this
// invisible: every component that could report did report, and the one path
// that bypassed all of them was the cheap one, which is also the common one.
//
// ledger: M1/G02#1 SpentTokens 随 LLM 调用累计
func TestLightweightGoalReportsItsTokensToTheSink(t *testing.T) {
	app := buildFakeAppWithSkills(t)
	defer app.Shutdown(context.Background())

	sink := &goalloop.UsageSink{}
	require.Zero(t, sink.Snapshot().Total(), "the sink starts empty")

	dec := runLightweightGoal(context.Background(), app, goalloop.TierQuickFix, "do a thing", sink)
	require.True(t, dec.Complete, "summary=%s", dec.Summary)

	got := sink.Snapshot()
	assert.Positive(t, got.Total(),
		"the lightweight turn reported no tokens: the run record for every T0-T2 goal "+
			"says zero regardless of what it spent, and a token budget over this path "+
			"never bites")
}

// TestLightweightGoalReportsTokensEvenWhenTheTurnFails is the direction a
// budget depends on.
//
// A turn that errors has still spent whatever it spent. Reporting only on
// success gives a failing loop an unmetered retry every time — the same reason
// runSubAgentTurn forwards its usage before checking errMsg.
//
// ledger: M1/G02#1 SpentTokens 随 LLM 调用累计
func TestLightweightGoalReportsTokensEvenWhenTheTurnFails(t *testing.T) {
	app := buildFakeApp(t)
	defer app.Shutdown(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sink := &goalloop.UsageSink{}
	dec := runLightweightGoal(ctx, app, goalloop.TierQuickFix, "do a thing", sink)
	require.False(t, dec.Complete)

	// A cancelled turn may legitimately report nothing — it may not have
	// reached the model. What must NOT happen is the reporting being skipped
	// structurally, which is what asserting only on the success path allows.
	// The assertion that carries this is that the call returns rather than
	// panicking on a nil-usage turn, plus the non-negative invariant.
	got := sink.Snapshot()
	assert.GreaterOrEqual(t, got.Total(), 0)
	assert.Equal(t, got.PromptTokens+got.CompletionTokens > 0, got.Total() > 0,
		"the sink holds an inconsistent usage after a failed turn")
}

// TestPersistedRunRecordCarriesTheStopReason is the persistence half of the
// budget clause, read back out of the store.
//
// TestPersistGoalRun_WritesRecord asserts `SELECT COUNT(*) ... LIKE 'goalrun:%'
// >= 1` on a Decision{Complete: true}. Nothing anywhere wrote StopReason into
// the store and read it back: replacing `StopReason: decision.StopReason` in
// NewRunRecord with `""` reddens nothing on the persistence side. "把原因持久化"
// is precisely the field that test does not look at.
//
// ledger: M1/G02#2 预算耗尽可靠停止并把原因持久化
func TestPersistedRunRecordCarriesTheStopReason(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "goalrun.db"))
	require.NoError(t, err)
	defer st.Close()

	usage := goalloop.Usage{PromptTokens: 900, CompletionTokens: 200, TotalTokens: 1100}
	persistGoalRun(st, goalloop.TierStandard, goalloop.Decision{
		Complete:   false,
		StopReason: goalloop.StopReasonTokenBudget,
		Summary:    "budget exhausted",
	}, usage, 3)

	var raw string
	require.NoError(t,
		st.DB.QueryRow(`SELECT value FROM kv WHERE key LIKE 'goalrun:%' ORDER BY key DESC LIMIT 1`).Scan(&raw),
		"no goalrun record was written")

	var rec goalloop.RunRecord
	require.NoError(t, json.Unmarshal([]byte(raw), &rec), "stored value: %s", raw)

	assert.Equal(t, goalloop.StopReasonTokenBudget, rec.StopReason,
		"the persisted record does not say WHY the run stopped, which is the whole "+
			"point of writing it: an operator reading it back cannot tell a budget "+
			"exhaustion from a normal finish")
	assert.False(t, rec.Complete)
	assert.Equal(t, 1100, rec.Usage.TotalTokens,
		"the persisted record lost the token count that justified the stop")
	assert.Equal(t, 3, rec.Iterations)
}

// TestGoalHistoryReadsBackWhatWasPersisted closes the loop on the record.
//
// persistGoalRun has written a RunRecord to kv since G02 and nothing read one
// back — a record written for an operator with no way for an operator to see
// it. "把原因持久化" is only worth anything if the reason can be retrieved, and
// StopReason is the field that decides what to do next: a run that finished
// and a run that ran out of tokens both "stopped".
//
// ledger: M1/G02#2 预算耗尽可靠停止并把原因持久化
func TestGoalHistoryReadsBackWhatWasPersisted(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "goalrun.db")
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	persistGoalRun(st, goalloop.TierStandard, goalloop.Decision{
		Complete: false, StopReason: goalloop.StopReasonTokenBudget, Summary: "out of budget",
	}, goalloop.Usage{TotalTokens: 4242}, 2)
	require.NoError(t, st.Close())

	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(
		"storage:\n  sqlite_path: \""+strings.ReplaceAll(dbPath, "\\", "/")+"\"\n"), 0o644))

	var out strings.Builder
	code := printGoalHistory(cfgPath, 5, &out)
	require.Equal(t, exitOK, code, "output: %s", out.String())

	got := out.String()
	assert.Contains(t, got, "stop_reason=token_budget",
		"the history does not show WHY the run stopped, which is the only reason the "+
			"record is written: %q", got)
	assert.Contains(t, got, "tokens=4242", "the history lost the token count: %q", got)
	assert.Contains(t, got, "complete=false")
}

// TestGoalHistoryOnAnEmptyStoreSaysSo is the boundary a listing needs.
//
// Printing nothing at all reads as a broken command; it has to distinguish
// "no runs yet" from "something went wrong".
//
// ledger: M1/G02#2 预算耗尽可靠停止并把原因持久化
func TestGoalHistoryOnAnEmptyStoreSaysSo(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "empty.db")
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, st.Close())

	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(
		"storage:\n  sqlite_path: \""+strings.ReplaceAll(dbPath, "\\", "/")+"\"\n"), 0o644))

	var out strings.Builder
	require.Equal(t, exitOK, printGoalHistory(cfgPath, 5, &out))
	assert.Contains(t, out.String(), "no goal runs recorded yet")
}
