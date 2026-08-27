package main

// liverun_o6_test.go — the schedule operator surface, driven end to end against
// a real assembled daemon.
//
// The existing tests for this endpoint inject a hand-written ScheduleManager,
// which proves the HTTP plumbing and nothing about the thing an operator cares
// about: whether `yanshi schedule pause` actually leaves the automation paused
// in the daemon's own store, and whether `run-now` actually causes a run.
// Those are state questions, and a stub manager answers them by construction.
//
// So this test assembles the app the way `yanshi serve` does, creates a real
// automation through App.Automation, serves the real ops endpoints on a real
// loopback listener, and then calls the same client function the CLI calls —
// re-reading state from the manager after each operation.

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/bootstrap"
	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/lockfile"
)

// o6Daemon is an assembled app served on loopback with the ops endpoints
// mounted, plus the lockfile the CLI discovers it through.
type o6Daemon struct {
	app  *bootstrap.App
	root string
}

// startO6Daemon boots the real composition root over a scratch project and
// publishes it exactly as `yanshi serve` does, so cli.RunSchedule finds it by
// the normal discovery path rather than by being handed an address.
func startO6Daemon(t *testing.T) *o6Daemon {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "yanshi.db")
	cfgPath := filepath.Join(root, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath,
		[]byte("storage:\n  sqlite_path: \""+filepath.ToSlash(dbPath)+"\"\n"), 0o644))

	app, err := bootstrap.Build(bootstrap.Options{
		ConfigPath: cfgPath,
		FakeModel:  true,
		WorkRoot:   root,
	})
	require.NoError(t, err, "the daemon must assemble")
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })

	require.NotNil(t, app.Automation,
		"App.Automation must be wired, or every schedule command answers "+
			"\"this daemon has no scheduler\" no matter what is in the store")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	handler := withOpsEndpoints(app.Server.Handler, opsConfig{
		App:        app,
		ConfigPath: cfgPath,
		Current:    app.BootConfig,
		Stop:       func() {},
	})
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	_, err = lockfile.Acquire(root, lockfile.Lockfile{
		PID: os.Getpid(), Addr: ln.Addr().String(), Auth: "none", Root: root,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lockfile.Remove(root) })

	return &o6Daemon{app: app, root: root}
}

// schedule issues one operator command through the same function the CLI uses.
func (d *o6Daemon) schedule(t *testing.T, op cli.ScheduleOp, id string) cli.ScheduleResponse {
	t.Helper()
	resp, err := cli.RunSchedule(context.Background(), d.root, cli.ScheduleRequest{Op: op, ID: id})
	require.NoErrorf(t, err, "schedule %s", op)
	return resp
}

// state re-reads the automation straight from the daemon's manager, which is
// the only source that can contradict what the endpoint just claimed.
func (d *o6Daemon) state(t *testing.T, id string) (automation.Automation, []automation.Run) {
	t.Helper()
	a, runs, err := d.app.Automation.Read(id)
	require.NoError(t, err)
	return a, runs
}

// TestLiveRun_O6ScheduleWritePathChangesRealState drives list / show / pause /
// resume / run-now / delete against a live daemon and checks the manager's own
// state after each one.
//
// Each assertion is on state read back AFTER the call, not on the response
// body: an endpoint that returns {"ok":true} and mutates nothing produces an
// identical transcript, and that is precisely the failure this run exists to
// exclude.
func TestLiveRun_O6ScheduleWritePathChangesRealState(t *testing.T) {
	d := startO6Daemon(t)

	created, err := d.app.Automation.Create(automation.CreateInput{
		Name:     "nightly digest",
		Prompt:   "summarise what changed today",
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 3600},
		Cwds:     []string{d.root},
	})
	require.NoError(t, err, "creating an automation is the precondition, not the subject")
	t.Logf("created automation %s (active=%v next=%v)", created.ID, created.Active, created.NextRunAt)

	// --- list: the operator must be able to SEE it.
	list := d.schedule(t, cli.ScheduleList, "")
	require.Len(t, list.Items, 1, "list must return the automation that exists")
	require.Equal(t, created.ID, list.Items[0].ID)
	require.Equal(t, "nightly digest", list.Items[0].Name)
	t.Logf("list: id=%s name=%q active=%v next=%q",
		list.Items[0].ID, list.Items[0].Name, list.Items[0].Active, list.Items[0].NextRunAt)

	// --- show: one automation plus its (still empty) history.
	show := d.schedule(t, cli.ScheduleShow, created.ID)
	require.Len(t, show.Items, 1)
	require.Equal(t, created.ID, show.Items[0].ID)
	t.Logf("show: %d run(s) in history", len(show.Runs))

	// --- pause: state must actually change in the manager.
	d.schedule(t, cli.SchedulePause, created.ID)
	after, _ := d.state(t, created.ID)
	t.Logf("after pause: active=%v next=%v", after.Active, after.NextRunAt)
	if after.Active {
		t.Errorf("pause returned success but the automation is still active in the daemon's store")
	}

	// --- resume: and back, with a recomputed next fire time.
	d.schedule(t, cli.ScheduleResume, created.ID)
	after, _ = d.state(t, created.ID)
	t.Logf("after resume: active=%v next=%v", after.Active, after.NextRunAt)
	if !after.Active {
		t.Errorf("resume returned success but the automation is still paused in the daemon's store")
	}
	if after.NextRunAt == nil {
		t.Errorf("resume left no next fire time; a resumed automation that never fires is still paused")
	} else if after.NextRunAt.Before(time.Now().Add(-time.Minute)) {
		t.Errorf("resume left a next fire time in the past (%v); it must be recomputed from now",
			after.NextRunAt)
	}

	// --- run-now: the load-bearing one. A run must EXIST afterwards.
	_, runsBefore := d.state(t, created.ID)
	d.schedule(t, cli.ScheduleRunNow, created.ID)
	_, runsAfter := d.state(t, created.ID)
	t.Logf("run-now: history went from %d run(s) to %d", len(runsBefore), len(runsAfter))
	if len(runsAfter) <= len(runsBefore) {
		t.Fatalf("run-now reported success but produced no run: history is still %d entries. "+
			"An operator pressing run-now and seeing nothing happen is the whole defect class here",
			len(runsAfter))
	}
	newest := runsAfter[0]
	t.Logf("run-now produced run %s status=%q scheduled_for=%s task=%q",
		newest.ID, newest.Status, newest.ScheduledFor.Format(time.RFC3339), newest.TaskID)
	if newest.Status == "" {
		t.Errorf("the produced run carries no status")
	}

	// run-now must not disturb the schedule: that is what distinguishes it from
	// a manual reschedule.
	afterRun, _ := d.state(t, created.ID)
	if afterRun.NextRunAt == nil || !afterRun.NextRunAt.Equal(*after.NextRunAt) {
		t.Errorf("run-now moved the next fire time from %v to %v; it must run out of band",
			after.NextRunAt, afterRun.NextRunAt)
	}

	// And it is visible through the operator surface, not only through the API.
	show = d.schedule(t, cli.ScheduleShow, created.ID)
	if len(show.Runs) == 0 {
		t.Errorf("show reports no run history even though a run exists in the store")
	}

	// --- delete: gone from both the listing and the store.
	d.schedule(t, cli.ScheduleDelete, created.ID)
	list = d.schedule(t, cli.ScheduleList, "")
	t.Logf("after delete: %d automation(s) listed", len(list.Items))
	if len(list.Items) != 0 {
		t.Errorf("delete returned success but the automation is still listed")
	}
	if _, _, err := d.app.Automation.Read(created.ID); err == nil {
		t.Errorf("delete returned success but the automation is still readable from the store")
	}
}

// TestLiveRun_O6ScheduleRefusesMutationWithoutAnID pins the guard that keeps a
// forgotten argument from becoming a fleet-wide pause. It runs against the same
// live daemon rather than a stub, because the check has to hold on the path an
// operator actually reaches.
func TestLiveRun_O6ScheduleRefusesMutationWithoutAnID(t *testing.T) {
	d := startO6Daemon(t)
	created, err := d.app.Automation.Create(automation.CreateInput{
		Name:     "keep me",
		Prompt:   "do not pause me by accident",
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 3600},
	})
	require.NoError(t, err)

	for _, op := range []cli.ScheduleOp{
		cli.SchedulePause, cli.ScheduleResume, cli.ScheduleRunNow, cli.ScheduleDelete,
	} {
		resp, err := cli.RunSchedule(context.Background(), d.root, cli.ScheduleRequest{Op: op})
		t.Logf("%s with no id -> err=%v error=%q", op, err, resp.Error)
		if err == nil && resp.Error == "" {
			t.Errorf("%s with no id was accepted; a forgotten argument must not act on everything", op)
		}
	}
	still, _ := d.state(t, created.ID)
	if !still.Active {
		t.Errorf("an id-less mutation reached the automation: it is no longer active")
	}
}
