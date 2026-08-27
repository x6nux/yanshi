package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/lockfile"
)

// fakeScheduleManager is an in-memory ScheduleManager. It records every
// mutation so a test can prove an operation reached the manager (or, for a
// refused request, did not).
type fakeScheduleManager struct {
	items   map[string]automation.Automation
	runs    map[string][]automation.Run
	calls   []string
	failNow error
}

func newFakeScheduleManager() *fakeScheduleManager {
	return &fakeScheduleManager{
		items: map[string]automation.Automation{},
		runs:  map[string][]automation.Run{},
	}
}

func (f *fakeScheduleManager) add(item automation.Automation) {
	f.items[item.ID] = item
}

func (f *fakeScheduleManager) List() ([]automation.Automation, error) {
	f.calls = append(f.calls, "list")
	out := make([]automation.Automation, 0, len(f.items))
	for _, v := range f.items {
		out = append(out, v)
	}
	return out, nil
}

func (f *fakeScheduleManager) Read(id string) (automation.Automation, []automation.Run, error) {
	f.calls = append(f.calls, "read:"+id)
	item, ok := f.items[id]
	if !ok {
		return automation.Automation{}, nil, errors.New("automation " + id + " not found")
	}
	return item, f.runs[id], nil
}

func (f *fakeScheduleManager) Pause(id string) error {
	f.calls = append(f.calls, "pause:"+id)
	item, ok := f.items[id]
	if !ok {
		return errors.New("automation " + id + " not found")
	}
	item.Active = false
	item.NextRunAt = nil
	f.items[id] = item
	return nil
}

func (f *fakeScheduleManager) Resume(id string) error {
	f.calls = append(f.calls, "resume:"+id)
	item, ok := f.items[id]
	if !ok {
		return errors.New("automation " + id + " not found")
	}
	item.Active = true
	next := time.Now().Add(time.Hour)
	item.NextRunAt = &next
	f.items[id] = item
	return nil
}

func (f *fakeScheduleManager) Delete(id string) error {
	f.calls = append(f.calls, "delete:"+id)
	if _, ok := f.items[id]; !ok {
		return errors.New("automation " + id + " not found")
	}
	delete(f.items, id)
	delete(f.runs, id)
	return nil
}

func (f *fakeScheduleManager) RunNow(_ context.Context, id string) (automation.Run, error) {
	f.calls = append(f.calls, "runnow:"+id)
	if f.failNow != nil {
		return automation.Run{}, f.failNow
	}
	if _, ok := f.items[id]; !ok {
		return automation.Run{}, errors.New("automation " + id + " not found")
	}
	run := automation.Run{
		ID: "run-manual", AutomationID: id, Status: automation.RunQueued,
		ScheduledFor: time.Now(),
	}
	f.runs[id] = append(f.runs[id], run)
	return run, nil
}

func postScheduleTo(t *testing.T, h func(http.ResponseWriter, *http.Request), body string) (int, ScheduleResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, SchedulePath, strings.NewReader(body)))
	var resp ScheduleResponse
	if rec.Body.Len() > 0 {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	}
	return rec.Code, resp
}

// TestScheduleHandlerListsWithNextFireTime is the O6 headline: an operator must
// be able to see what exists and when it fires next. Before this endpoint the
// scheduler and its persistence were both present and neither was observable.
func TestScheduleHandlerListsWithNextFireTime(t *testing.T) {
	mgr := newFakeScheduleManager()
	soon := time.Now().Add(5 * time.Minute)
	later := time.Now().Add(2 * time.Hour)
	mgr.add(automation.Automation{
		ID: "auto-late", Name: "nightly", Active: true, NextRunAt: &later,
		Schedule: automation.Schedule{Kind: "cron", Cron: "0 3 * * *"},
	})
	mgr.add(automation.Automation{
		ID: "auto-soon", Name: "poll", Active: true, NextRunAt: &soon,
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 300},
	})
	mgr.add(automation.Automation{
		ID: "auto-paused", Name: "off", Active: false,
		Schedule: automation.Schedule{Kind: "cron", Cron: "@weekly"},
	})

	code, resp := postScheduleTo(t, NewScheduleHandler(mgr), `{"op":"list"}`)
	require.Equal(t, http.StatusOK, code)
	require.True(t, resp.OK)
	require.Len(t, resp.Items, 3)

	require.Equal(t, "auto-soon", resp.Items[0].ID,
		"the answer to \"what runs next\" must be the first line")
	require.Equal(t, "auto-late", resp.Items[1].ID)
	require.Equal(t, "auto-paused", resp.Items[2].ID,
		"a paused automation has no next fire time and sorts last")

	require.Equal(t, "every 300s", resp.Items[0].Schedule)
	require.Equal(t, "cron 0 3 * * *", resp.Items[1].Schedule)
	require.False(t, resp.Items[2].Active)
}

// TestScheduleListOmitsPrompts pins the disclosure posture: a prompt is model
// input, the same category the structured logger redacts wholesale, and a
// listing an operator scans is not the place for it.
func TestScheduleListOmitsPrompts(t *testing.T) {
	mgr := newFakeScheduleManager()
	mgr.add(automation.Automation{
		ID: "auto-1", Name: "n", Active: true,
		Prompt:   "the operator's private instructions with a token sk-live-XYZ in them",
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 60},
	})

	code, resp := postScheduleTo(t, NewScheduleHandler(mgr), `{"op":"list"}`)
	require.Equal(t, http.StatusOK, code)
	require.Empty(t, resp.Items[0].PromptPreview, "list must not carry model input")

	// show carries a BOUNDED preview: enough to tell two automations apart.
	code, resp = postScheduleTo(t, NewScheduleHandler(mgr), `{"op":"show","id":"auto-1"}`)
	require.Equal(t, http.StatusOK, code)
	require.NotEmpty(t, resp.Items[0].PromptPreview)
	require.LessOrEqual(t, len([]rune(resp.Items[0].PromptPreview)), PromptPreviewRunes+1)
}

// TestPreviewPromptBounds is the table for the excerpt clamp.
func TestPreviewPromptBounds(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "short prompt passes through", in: "do the thing", want: "do the thing"},
		{name: "whitespace collapses to one line", in: "do\n  the\tthing\n", want: "do the thing"},
		{name: "empty stays empty", in: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, previewPrompt(tc.in))
		})
	}

	long := strings.Repeat("x", PromptPreviewRunes+50)
	got := previewPrompt(long)
	require.Equal(t, PromptPreviewRunes+1, len([]rune(got)))
	require.True(t, strings.HasSuffix(got, "…"))
}

// TestScheduleMutationsRequireAnID is the guard against the argument an
// operator forgets. "Pause everything because the id was empty" is not a
// mistake with an undo.
func TestScheduleMutationsRequireAnID(t *testing.T) {
	for _, op := range []ScheduleOp{SchedulePause, ScheduleResume, ScheduleRunNow, ScheduleDelete} {
		t.Run(string(op), func(t *testing.T) {
			mgr := newFakeScheduleManager()
			mgr.add(automation.Automation{ID: "auto-1", Active: true})

			code, resp := postScheduleTo(t, NewScheduleHandler(mgr),
				`{"op":"`+string(op)+`"}`)
			require.Equal(t, http.StatusBadRequest, code)
			require.Contains(t, resp.Error, "requires an automation id")
			require.Empty(t, mgr.calls, "a refused request must not reach the manager")
		})
	}

	// show is a read, but an id-less show is still a usage error rather than a
	// silent empty answer.
	mgr := newFakeScheduleManager()
	code, resp := postScheduleTo(t, NewScheduleHandler(mgr), `{"op":"show"}`)
	require.Equal(t, http.StatusBadRequest, code)
	require.Contains(t, resp.Error, "requires an automation id")
}

// TestSchedulePauseResumeRoundTrip proves the two operations reach the manager
// and that resume reports the recomputed fire time -- an operator resuming a
// long-paused automation needs to know it is not about to fire for every slot
// it missed.
func TestSchedulePauseResumeRoundTrip(t *testing.T) {
	mgr := newFakeScheduleManager()
	next := time.Now().Add(time.Minute)
	mgr.add(automation.Automation{
		ID: "auto-1", Name: "n", Active: true, NextRunAt: &next,
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 60},
	})
	h := NewScheduleHandler(mgr)

	code, resp := postScheduleTo(t, h, `{"op":"pause","id":"auto-1"}`)
	require.Equal(t, http.StatusOK, code)
	require.True(t, resp.OK)
	require.Contains(t, resp.Message, "paused")
	require.False(t, mgr.items["auto-1"].Active)
	require.Nil(t, mgr.items["auto-1"].NextRunAt)

	code, resp = postScheduleTo(t, h, `{"op":"resume","id":"auto-1"}`)
	require.Equal(t, http.StatusOK, code)
	require.True(t, mgr.items["auto-1"].Active)
	require.Len(t, resp.Items, 1)
	require.NotNil(t, resp.Items[0].NextRunAt,
		"resume must report the recomputed fire time")
	require.True(t, resp.Items[0].NextRunAt.After(time.Now()))
}

// TestScheduleRunNowLeavesTheScheduleAlone pins the semantics of a manual
// trigger: it enqueues an extra run, it does not consume or move the next
// scheduled slot.
func TestScheduleRunNowLeavesTheScheduleAlone(t *testing.T) {
	mgr := newFakeScheduleManager()
	next := time.Now().Add(time.Hour)
	mgr.add(automation.Automation{
		ID: "auto-1", Name: "n", Active: true, NextRunAt: &next,
		Schedule: automation.Schedule{Kind: "cron", Cron: "@hourly"},
	})

	code, resp := postScheduleTo(t, NewScheduleHandler(mgr), `{"op":"run-now","id":"auto-1"}`)
	require.Equal(t, http.StatusOK, code)
	require.True(t, resp.OK)
	require.Contains(t, resp.Message, "schedule is unchanged")
	require.Len(t, resp.Runs, 1)
	require.Equal(t, automation.RunQueued, resp.Runs[0].Status)
	require.Equal(t, next.Unix(), mgr.items["auto-1"].NextRunAt.Unix(),
		"a manual run must not move the scheduled slot")
}

// TestScheduleDeleteRemovesRunsToo proves the delete is complete: leaving run
// rows behind for a deleted automation would make the history unjoinable.
func TestScheduleDeleteRemovesRunsToo(t *testing.T) {
	mgr := newFakeScheduleManager()
	mgr.add(automation.Automation{ID: "auto-1", Active: true})
	mgr.runs["auto-1"] = []automation.Run{{ID: "run-1", Status: automation.RunCompleted}}

	code, resp := postScheduleTo(t, NewScheduleHandler(mgr), `{"op":"delete","id":"auto-1"}`)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, resp.Message, "run history")
	require.NotContains(t, mgr.items, "auto-1")
	require.NotContains(t, mgr.runs, "auto-1")

	// A second delete is a clean not-found, not a crash.
	code, resp = postScheduleTo(t, NewScheduleHandler(mgr), `{"op":"delete","id":"auto-1"}`)
	require.Equal(t, http.StatusNotFound, code)
	require.False(t, resp.OK)
	require.Contains(t, resp.Error, "not found")
}

// TestScheduleShowReturnsRunsNewestFirst covers the history ordering an
// operator reads: the run they care about is the last one.
func TestScheduleShowReturnsRunsNewestFirst(t *testing.T) {
	mgr := newFakeScheduleManager()
	mgr.add(automation.Automation{ID: "auto-1", Name: "n", Active: true, Prompt: "p"})
	base := time.Now()
	mgr.runs["auto-1"] = []automation.Run{
		{ID: "run-old", Status: automation.RunCompleted, ScheduledFor: base.Add(-2 * time.Hour)},
		{ID: "run-new", Status: automation.RunFailed, ScheduledFor: base, Error: "boom"},
		{ID: "run-mid", Status: automation.RunCompleted, ScheduledFor: base.Add(-time.Hour)},
	}

	code, resp := postScheduleTo(t, NewScheduleHandler(mgr), `{"op":"show","id":"auto-1"}`)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, resp.Runs, 3)
	require.Equal(t, "run-new", resp.Runs[0].ID)
	require.Equal(t, "run-old", resp.Runs[2].ID)
	require.Equal(t, "boom", resp.Runs[0].Error)
}

// TestScheduleHandlerProtocolErrors covers the non-happy paths of the endpoint.
func TestScheduleHandlerProtocolErrors(t *testing.T) {
	mgr := newFakeScheduleManager()
	h := NewScheduleHandler(mgr)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, SchedulePath, nil))
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)

	code, _ := postScheduleTo(t, h, `not json`)
	require.Equal(t, http.StatusBadRequest, code)

	code, resp := postScheduleTo(t, h, `{"op":"drop-everything"}`)
	require.Equal(t, http.StatusBadRequest, code)
	require.Contains(t, resp.Error, "unknown schedule op")

	// A daemon built without the scheduler says so rather than reporting an
	// empty list, which would tell an operator their schedules are gone.
	code, resp = postScheduleTo(t, NewScheduleHandler(nil), `{"op":"list"}`)
	require.Equal(t, http.StatusNotImplemented, code)
	require.Contains(t, resp.Error, "without the automation scheduler")
	require.Empty(t, mgr.calls)
}

// TestScheduleRunNowFailureIsReported asserts a queue failure surfaces rather
// than being reported as a successful enqueue.
func TestScheduleRunNowFailureIsReported(t *testing.T) {
	mgr := newFakeScheduleManager()
	mgr.add(automation.Automation{ID: "auto-1", Active: true})
	mgr.failNow = errors.New("broker unreachable")

	code, resp := postScheduleTo(t, NewScheduleHandler(mgr), `{"op":"run-now","id":"auto-1"}`)
	require.Equal(t, http.StatusInternalServerError, code)
	require.False(t, resp.OK)
	require.Contains(t, resp.Error, "broker unreachable")
	require.Empty(t, resp.Runs, "a zero Run carries nothing worth showing")
}

// TestRunScheduleRejectsUnknownOpBeforeTouchingTheNetwork proves a typo is
// caught client-side, so `yanshi schedule paws auto-1` fails with a usage
// message instead of a dial error.
func TestRunScheduleRejectsUnknownOpBeforeTouchingTheNetwork(t *testing.T) {
	_, err := RunSchedule(context.Background(), t.TempDir(), ScheduleRequest{Op: "paws"})
	require.ErrorIs(t, err, ErrUnknownScheduleOp)
	require.Contains(t, err.Error(), "pause", "the error must list the real operations")
}

// TestRunScheduleRequiresALiveDaemon pins the deliberate refusal to read the
// store directly. A listing produced behind the scheduler's back is correct
// only until the next tick, and an operator has no way to tell a stale answer
// from a current one.
func TestRunScheduleRequiresALiveDaemon(t *testing.T) {
	t.Run("no lockfile", func(t *testing.T) {
		_, err := RunSchedule(context.Background(),
			filepath.Join(t.TempDir(), "nothing"), ScheduleRequest{Op: ScheduleList})
		require.ErrorIs(t, err, ErrNoDaemon)
		require.Contains(t, err.Error(), "yanshi serve")
	})

	t.Run("dead pid", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "stale")
		require.NoError(t, lockfile.Write(root, lockfile.Lockfile{PID: 999999, Root: root}))
		t.Cleanup(func() { _ = lockfile.Remove(root) })

		_, err := RunSchedule(context.Background(), root, ScheduleRequest{Op: ScheduleList})
		require.ErrorIs(t, err, ErrNoDaemon)
	})
}

// TestRunScheduleAgainstALiveServer drives the full client-to-handler round
// trip, so the wire format is exercised rather than assumed.
func TestRunScheduleAgainstALiveServer(t *testing.T) {
	mgr := newFakeScheduleManager()
	next := time.Now().Add(time.Minute)
	mgr.add(automation.Automation{
		ID: "auto-1", Name: "nightly", Active: true, NextRunAt: &next,
		Schedule: automation.Schedule{Kind: "cron", Cron: "@daily"},
	})

	mux := http.NewServeMux()
	mux.HandleFunc(SchedulePath, NewScheduleHandler(mgr))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	root := filepath.Join(t.TempDir(), "proj")
	require.NoError(t, lockfile.Write(root, lockfile.Lockfile{
		PID: os.Getpid(), Addr: strings.TrimPrefix(ts.URL, "http://"), Root: root,
	}))
	t.Cleanup(func() { _ = lockfile.Remove(root) })

	resp, err := RunSchedule(context.Background(), root, ScheduleRequest{Op: ScheduleList})
	require.NoError(t, err)
	require.True(t, resp.OK)
	require.Len(t, resp.Items, 1)
	require.Equal(t, "nightly", resp.Items[0].Name)

	// A failing op surfaces as a Go error rather than a silent OK=false.
	_, err = RunSchedule(context.Background(), root,
		ScheduleRequest{Op: ScheduleDelete, ID: "does-not-exist"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

// TestRunScheduleAgainstAnOldBackend covers the upgrade window, where the
// running daemon predates this endpoint and a raw 404 would tell the operator
// nothing about what to do.
func TestRunScheduleAgainstAnOldBackend(t *testing.T) {
	ts := httptest.NewServer(http.NewServeMux())
	t.Cleanup(ts.Close)

	root := filepath.Join(t.TempDir(), "old")
	require.NoError(t, lockfile.Write(root, lockfile.Lockfile{
		PID: os.Getpid(), Addr: strings.TrimPrefix(ts.URL, "http://"), Root: root,
	}))
	t.Cleanup(func() { _ = lockfile.Remove(root) })

	_, err := RunSchedule(context.Background(), root, ScheduleRequest{Op: ScheduleList})
	require.ErrorIs(t, err, ErrNoDaemon)
	require.Contains(t, err.Error(), "predates")
}

// TestMissingRouteIsNotADomainNotFound is the distinction the status code
// alone cannot make. Both cases are HTTP 404: "this daemon is too old to have
// the endpoint" tells the operator to restart, while "no automation with that
// id" tells them they typed the wrong id. Collapsing them sends the operator
// to restart a perfectly current daemon over a typo.
func TestMissingRouteIsNotADomainNotFound(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		contentType string
		want        bool
	}{
		{
			name:   "ServeMux built-in 404 is a missing route",
			status: http.StatusNotFound, contentType: "text/plain; charset=utf-8",
			want: true,
		},
		{
			name:   "our JSON 404 is a domain answer",
			status: http.StatusNotFound, contentType: "application/json",
			want: false,
		},
		{
			name:   "a 404 with no content type is a missing route",
			status: http.StatusNotFound,
			want:   true,
		},
		{
			name:   "a non-404 is never a missing route",
			status: http.StatusInternalServerError, contentType: "text/plain",
			want: false,
		},
		{
			name:   "a 200 is never a missing route",
			status: http.StatusOK, contentType: "application/json",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tc.status, Header: http.Header{}}
			if tc.contentType != "" {
				resp.Header.Set("Content-Type", tc.contentType)
			}
			require.Equal(t, tc.want, isMissingRoute(resp))
		})
	}
}

// TestDomainNotFoundReachesTheOperatorAsSuch is the end-to-end half of the
// distinction above: a bad id against a CURRENT daemon must produce the
// manager's message, never the "restart your daemon" advice.
func TestDomainNotFoundReachesTheOperatorAsSuch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(SchedulePath, NewScheduleHandler(newFakeScheduleManager()))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	root := filepath.Join(t.TempDir(), "current")
	require.NoError(t, lockfile.Write(root, lockfile.Lockfile{
		PID: os.Getpid(), Addr: strings.TrimPrefix(ts.URL, "http://"), Root: root,
	}))
	t.Cleanup(func() { _ = lockfile.Remove(root) })

	_, err := RunSchedule(context.Background(), root,
		ScheduleRequest{Op: ScheduleShow, ID: "auto-typo"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
	require.NotContains(t, err.Error(), "predates",
		"a typo must not send the operator off to restart a current daemon")
	require.NotErrorIs(t, err, ErrNoDaemon)
}

// TestFormatNextRun covers the "when does this fire" rendering, including the
// overdue case: printing a negative duration is the difference between an
// operator seeing a scheduler that is behind and one that looks broken.
func TestFormatNextRun(t *testing.T) {
	require.Equal(t, "-", formatNextRun(nil))

	soon := time.Now().Add(90 * time.Second)
	got := formatNextRun(&soon)
	require.Contains(t, got, "in 1m30s")
	require.Contains(t, got, soon.Format(time.RFC3339))

	past := time.Now().Add(-2 * time.Minute)
	got = formatNextRun(&past)
	require.Contains(t, got, "overdue by")
	require.NotContains(t, got, "-2m", "a negative duration must not be printed")
}

// TestDescribeSchedule covers the recurrence rendering, including the shapes an
// operator can reach with a hand-edited store.
func TestDescribeSchedule(t *testing.T) {
	cases := []struct {
		name string
		in   automation.Schedule
		want string
	}{
		{name: "cron", in: automation.Schedule{Kind: "cron", Cron: "0 9 * * 1-5"}, want: "cron 0 9 * * 1-5"},
		{name: "interval", in: automation.Schedule{Kind: "interval", IntervalSec: 300}, want: "every 300s"},
		{name: "absent", in: automation.Schedule{}, want: "(none)"},
		{name: "unknown kind is shown as-is", in: automation.Schedule{Kind: "solar"}, want: "solar"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, describeSchedule(tc.in))
		})
	}
}

// TestRenderScheduleResponse covers the console surface an operator reads.
func TestRenderScheduleResponse(t *testing.T) {
	next := time.Now().Add(time.Minute)
	var sb strings.Builder
	RenderScheduleResponse(&sb, ScheduleResponse{
		OK: true,
		Items: []ScheduleItem{{
			ID: "auto-1", Name: "nightly", Active: true,
			Schedule: "cron @daily", NextRunAt: &next, PromptPreview: "run the report",
		}},
		Runs: []ScheduleRun{{
			ID: "run-1", Status: automation.RunFailed,
			ScheduledFor: time.Now(), Error: "queue full",
		}},
	})
	out := sb.String()
	require.Contains(t, out, "auto-1")
	require.Contains(t, out, "nightly")
	require.Contains(t, out, "active")
	require.Contains(t, out, "run the report")
	require.Contains(t, out, "queue full")

	sb.Reset()
	RenderScheduleResponse(&sb, ScheduleResponse{OK: true})
	require.Contains(t, sb.String(), "no scheduled tasks")

	sb.Reset()
	RenderScheduleResponse(&sb, ScheduleResponse{Error: "automation auto-9 not found"})
	require.Contains(t, sb.String(), "auto-9 not found")
}

// TestScheduleOpsIsTheCompleteSet guards the CLI usage text and the validator
// against drifting apart from the handler's switch.
func TestScheduleOpsIsTheCompleteSet(t *testing.T) {
	for _, op := range ScheduleOps() {
		require.True(t, isKnownScheduleOp(op))
		mgr := newFakeScheduleManager()
		mgr.add(automation.Automation{ID: "auto-1", Active: true})
		code, resp := postScheduleTo(t, NewScheduleHandler(mgr),
			`{"op":"`+string(op)+`","id":"auto-1"}`)
		require.NotEqual(t, http.StatusBadRequest, code,
			"%q is advertised but the handler does not implement it: %s", op, resp.Error)
	}
	require.False(t, isKnownScheduleOp("create"),
		"creation stays a model-facing tool; the CLI is an operations surface")
}
