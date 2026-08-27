package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/lockfile"
)

// TestReloadSectionsAreDisjointAndJustified is the honesty contract for the
// reload boundary: no section may be listed as both reloadable and
// restart-required, and every entry must carry the reason it is in its bucket.
// A boundary without reasons is a boundary nobody can review, and the whole
// point of O3's reload is not pretending everything is hot-swappable.
func TestReloadSectionsAreDisjointAndJustified(t *testing.T) {
	for _, name := range ReloadableSections() {
		reason, reloadable, known := ReloadReason(name)
		require.True(t, known)
		require.True(t, reloadable)
		require.NotEmpty(t, reason, "%q must state why it is safe to reload", name)
		_, isAlsoStatic := nonReloadableSections[name]
		require.False(t, isAlsoStatic, "%q is in both buckets", name)
	}
	for _, name := range NonReloadableSections() {
		reason, reloadable, known := ReloadReason(name)
		require.True(t, known)
		require.False(t, reloadable)
		require.NotEmpty(t, reason, "%q must state why a restart is required", name)
	}

	// The two settings the task names explicitly must be on the refusing side.
	for _, mustRestart := range []string{"server.http_addr", "storage.sqlite_path"} {
		_, reloadable, known := ReloadReason(mustRestart)
		require.True(t, known, "%q must be classified, not fall through to the default", mustRestart)
		require.False(t, reloadable, "%q must require a restart", mustRestart)
	}
	// And the ones it names as reloadable must be on the applying side.
	for _, mustReload := range []string{"profiles", "compaction", "observability.log"} {
		_, reloadable, known := ReloadReason(mustReload)
		require.True(t, known)
		require.True(t, reloadable, "%q must be reloadable", mustReload)
	}

	_, _, known := ReloadReason("some.section.nobody.reviewed")
	require.False(t, known)
}

// TestClassifyReloadDefaultsToRefusing pins the fail-safe direction: a section
// nobody has reasoned about is a section whose runtime bindings nobody has
// checked, so it is refused. "Restart to be sure" costs one restart; "reload
// and hope" costs a silently stale process.
func TestClassifyReloadDefaultsToRefusing(t *testing.T) {
	cases := []struct {
		name         string
		changed      []string
		wantApplied  []string
		wantRejected []string
	}{
		{
			name:        "all reloadable",
			changed:     []string{"profiles", "compaction"},
			wantApplied: []string{"compaction", "profiles"},
		},
		{
			name:         "mixed",
			changed:      []string{"profiles", "server.http_addr"},
			wantApplied:  []string{"profiles"},
			wantRejected: []string{"server.http_addr"},
		},
		{
			name:         "unknown section is refused",
			changed:      []string{"brand.new.block"},
			wantRejected: []string{"brand.new.block"},
		},
		{
			name:    "nothing changed",
			changed: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			applied, rejected := ClassifyReload(tc.changed)
			require.Equal(t, tc.wantApplied, applied)
			names := make([]string, 0, len(rejected))
			for _, r := range rejected {
				names = append(names, r.Section)
				require.NotEmpty(t, r.Reason, "a refusal must explain itself")
			}
			if len(tc.wantRejected) == 0 {
				require.Empty(t, names)
				return
			}
			require.Equal(t, tc.wantRejected, names)
		})
	}
}

// TestChangedSectionsDetectsPerSectionEdits proves the diff is per-section, so
// an operator who changed one thing is not told to restart for everything.
func TestChangedSectionsDetectsPerSectionEdits(t *testing.T) {
	base := &config.Config{}
	base.Server.HTTPAddr = "127.0.0.1:8080"
	base.Storage.SQLitePath = "yanshi.db"
	base.Compaction.Threshold = 0.8

	t.Run("identical configs change nothing", func(t *testing.T) {
		same := *base
		require.Empty(t, ChangedSections(base, &same))
	})

	t.Run("a reloadable edit is isolated", func(t *testing.T) {
		next := *base
		next.Compaction.Threshold = 0.6
		require.Equal(t, []string{"compaction"}, ChangedSections(base, &next))
	})

	t.Run("a restart-required edit is isolated", func(t *testing.T) {
		next := *base
		next.Server.HTTPAddr = "0.0.0.0:9999"
		require.Equal(t, []string{"server.http_addr"}, ChangedSections(base, &next))
	})

	t.Run("no baseline reports every section", func(t *testing.T) {
		all := ChangedSections(nil, base)
		require.Len(t, all, len(ReloadableSections())+len(NonReloadableSections()),
			"without a baseline the daemon cannot claim a section is unchanged")
	})

	require.Nil(t, ChangedSections(base, nil))
}

// TestControlHandlerRejectsNonPost pins the method gate. A control endpoint
// reachable by GET is one a browser tab or a curl typo can trigger, and
// "stop the daemon" must have no accidental invocation path.
func TestControlHandlerRejectsNonPost(t *testing.T) {
	var stopped atomic.Bool
	h := NewControlHandler(ControlHooks{Stop: func() { stopped.Store(true) }})

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(method, ControlPath, strings.NewReader(`{"op":"stop"}`)))
		require.Equal(t, http.StatusMethodNotAllowed, rec.Code, "method %s", method)
	}
	require.False(t, stopped.Load(), "no non-POST method may reach the stop hook")
}

// TestControlHandlerStopRepliesBeforeShuttingDown pins the ordering that makes
// `daemon stop` usable: the reply travels over the very server being stopped,
// so it must be written before the hook runs or every successful stop looks
// like a connection error.
func TestControlHandlerStopRepliesBeforeShuttingDown(t *testing.T) {
	stopCalled := make(chan struct{})
	h := NewControlHandler(ControlHooks{Stop: func() { close(stopCalled) }})

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, ControlPath, strings.NewReader(`{"op":"stop"}`)))

	require.Equal(t, http.StatusAccepted, rec.Code)
	var resp ControlResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.True(t, resp.OK)
	require.NotEmpty(t, rec.Body.String(), "the reply must be complete before shutdown starts")

	select {
	case <-stopCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("stop hook was never invoked")
	}
}

// TestControlHandlerReloadAppliesOnlyTheSafeHalf is the O3 behaviour the task
// asks for: reloadable sections take effect, restart-required ones are
// explicitly refused with a reason, and the refusal is visible in the reply.
func TestControlHandlerReloadAppliesOnlyTheSafeHalf(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeFile(t, filepath.Join(dir, "config.yaml"),
		"schema_version: 1\nserver:\n  http_addr: \"0.0.0.0:9999\"\n"+
			"storage:\n  sqlite_path: \"yanshi.db\"\n"+
			"compaction:\n  threshold: 0.5\n")

	running := &config.Config{}
	running.Server.HTTPAddr = "127.0.0.1:8080"
	running.Storage.SQLitePath = "yanshi.db"
	running.Compaction.Threshold = 0.8

	var appliedSections []string
	h := NewControlHandler(ControlHooks{
		ConfigPath:    cfgPath,
		CurrentConfig: func() *config.Config { return running },
		ApplyReload: func(_ *config.Config, sections []string) ([]string, error) {
			appliedSections = append(appliedSections, sections...)
			return sections, nil
		},
	})

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, ControlPath, strings.NewReader(`{"op":"reload"}`)))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp ControlResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.True(t, resp.OK)
	require.Contains(t, resp.Applied, "compaction")
	require.Contains(t, appliedSections, "compaction",
		"the apply hook must actually receive the reloadable section")

	var rejectedNames []string
	for _, r := range resp.Rejected {
		rejectedNames = append(rejectedNames, r.Section)
		require.NotEmpty(t, r.Reason)
	}
	require.Contains(t, rejectedNames, "server.http_addr",
		"a listen-address change must be refused, not silently ignored")
	require.NotContains(t, appliedSections, "server.http_addr",
		"a refused section must never reach the apply hook")
}

// TestControlHandlerCanOnlyUnderClaimWhatWasApplied is the anti-lie contract.
// Classification answers "is this section safe to reload in principle"; only
// the daemon knows whether it has a wired apply path for it TODAY. A section
// that is classified reloadable, reported reloaded and has no consumer is the
// "written but nobody reads it" shape, and from the operator's side it is
// indistinguishable from a reload that worked.
func TestControlHandlerCanOnlyUnderClaimWhatWasApplied(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeFile(t, filepath.Join(dir, "config.yaml"),
		"schema_version: 1\nserver:\n  http_addr: \"127.0.0.1:8080\"\n"+
			"storage:\n  sqlite_path: \"yanshi.db\"\n")

	t.Run("a hook that adopts nothing cannot report a reload", func(t *testing.T) {
		h := NewControlHandler(ControlHooks{
			ConfigPath: cfgPath,
			ApplyReload: func(*config.Config, []string) ([]string, error) {
				return nil, nil // wired up, adopts nothing
			},
		})
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, ControlPath, strings.NewReader(`{"op":"reload"}`)))

		var resp ControlResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.Empty(t, resp.Applied,
			"a hook that adopted nothing must not produce an Applied list")
		var rejectedNames []string
		for _, r := range resp.Rejected {
			rejectedNames = append(rejectedNames, r.Section)
			require.NotEmpty(t, r.Reason)
		}
		for _, section := range ReloadableSections() {
			require.Contains(t, rejectedNames, section,
				"%q was classified safe but never adopted; it must be demoted", section)
		}
	})

	t.Run("a hook adopting a subset gets exactly that subset", func(t *testing.T) {
		h := NewControlHandler(ControlHooks{
			ConfigPath: cfgPath,
			ApplyReload: func(*config.Config, []string) ([]string, error) {
				return []string{"profiles"}, nil
			},
		})
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, ControlPath, strings.NewReader(`{"op":"reload"}`)))

		var resp ControlResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.Equal(t, []string{"profiles"}, resp.Applied)
		require.NotEmpty(t, resp.Rejected)
	})

	t.Run("no hook at all demotes everything", func(t *testing.T) {
		h := NewControlHandler(ControlHooks{ConfigPath: cfgPath})
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, ControlPath, strings.NewReader(`{"op":"reload"}`)))

		var resp ControlResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.Empty(t, resp.Applied,
			"a daemon with no apply path must not claim it reloaded anything")
		require.NotEmpty(t, resp.Rejected)
	})
}

// TestReconcileAppliedNeverPromotes pins the one-way direction. A section the
// hook adopted but that was never classified safe must NOT appear in Applied:
// promoting it would let a wiring mistake in the composition root silently
// widen the reload boundary this package exists to state.
func TestReconcileAppliedNeverPromotes(t *testing.T) {
	applied, rejected := reconcileApplied(
		[]string{"profiles"},
		[]string{"profiles", "server.http_addr", "invented.section"},
		nil,
	)
	require.Equal(t, []string{"profiles"}, applied,
		"only classified-safe sections may be reported as applied")
	require.Empty(t, rejected)

	applied, rejected = reconcileApplied([]string{"profiles", "compaction"}, nil, nil)
	require.Empty(t, applied)
	require.Len(t, rejected, 2)
}

// TestControlHandlerReloadKeepsRunningConfigOnBadFile proves a broken config
// cannot take a working daemon down with it, and that the raw load error is not
// echoed (a rejected config often names a raw api_key value, and this reply
// crosses a network boundary).
func TestControlHandlerReloadKeepsRunningConfigOnBadFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeFile(t, filepath.Join(dir, "config.yaml"),
		"schema_version: 99999\napi_key: \"sk-live-LEAK\"\n")

	applied := false
	h := NewControlHandler(ControlHooks{
		ConfigPath:  cfgPath,
		ApplyReload: func(_ *config.Config, s []string) ([]string, error) { applied = true; return s, nil },
	})

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, ControlPath, strings.NewReader(`{"op":"reload"}`)))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.False(t, applied, "a config that did not load must not be applied")
	require.Contains(t, rec.Body.String(), "unchanged")
	require.NotContains(t, rec.Body.String(), "sk-live-LEAK",
		"the raw load error must not be echoed across the network boundary")
}

// TestControlHandlerReloadReportsApplyFailure asserts a failing apply is
// surfaced rather than reported as success.
func TestControlHandlerReloadReportsApplyFailure(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeFile(t, filepath.Join(dir, "config.yaml"),
		"schema_version: 1\nserver:\n  http_addr: \"127.0.0.1:8080\"\n"+
			"storage:\n  sqlite_path: \"yanshi.db\"\n")

	h := NewControlHandler(ControlHooks{
		ConfigPath: cfgPath,
		ApplyReload: func(*config.Config, []string) ([]string, error) {
			return nil, errApplyFailed
		},
	})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, ControlPath, strings.NewReader(`{"op":"reload"}`)))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Contains(t, rec.Body.String(), "previous settings")
}

var errApplyFailed = errTestOnly("apply failed")

type errTestOnly string

func (e errTestOnly) Error() string { return string(e) }

// TestControlHandlerRejectsMalformedAndUnknownOps covers the protocol errors.
func TestControlHandlerRejectsMalformedAndUnknownOps(t *testing.T) {
	h := NewControlHandler(ControlHooks{Stop: func() {}})

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, ControlPath, strings.NewReader(`not json`)))
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, ControlPath, strings.NewReader(`{"op":"self-destruct"}`)))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "unknown control op")

	// A daemon assembled without a stop hook says so instead of pretending.
	noStop := NewControlHandler(ControlHooks{})
	rec = httptest.NewRecorder()
	noStop(rec, httptest.NewRequest(http.MethodPost, ControlPath, strings.NewReader(`{"op":"stop"}`)))
	require.Equal(t, http.StatusNotImplemented, rec.Code)

	// A reload with no path anywhere is a usage error, not a crash.
	noPath := NewControlHandler(ControlHooks{})
	rec = httptest.NewRecorder()
	noPath(rec, httptest.NewRequest(http.MethodPost, ControlPath, strings.NewReader(`{"op":"reload"}`)))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "no config path")
}

// TestRunDaemonStatusDistinguishesAliveFromReady is the operator-facing half of
// O7: a live-but-unready daemon is starting up, and an operator debugging "my
// second window will not connect" has to be able to see that rather than infer
// it from a hung window.
func TestRunDaemonStatusDistinguishesAliveFromReady(t *testing.T) {
	t.Run("no lockfile", func(t *testing.T) {
		s := RunDaemonStatus(context.Background(), filepath.Join(t.TempDir(), "unused"))
		require.False(t, s.Found)
		require.False(t, s.Alive)
		require.False(t, s.Ready)
	})

	t.Run("alive and ready", func(t *testing.T) {
		ts, _, _ := readyFixture{readyzStatus: 200}.server(t)
		root := filepath.Join(t.TempDir(), "ready-proj")
		require.NoError(t, lockfile.Write(root, lockfile.Lockfile{
			PID: os.Getpid(), Addr: strings.TrimPrefix(ts.URL, "http://"), Root: root,
			StartedAt: time.Now().Add(-90 * time.Second),
		}))
		t.Cleanup(func() { _ = lockfile.Remove(root) })

		s := RunDaemonStatus(context.Background(), root)
		require.True(t, s.Found)
		require.True(t, s.Alive)
		require.True(t, s.Ready)
		require.Positive(t, s.Uptime)
	})

	t.Run("alive but still assembling", func(t *testing.T) {
		ts, _, _ := readyFixture{readyzStatus: 503, healthzStatus: 200}.server(t)
		root := filepath.Join(t.TempDir(), "starting-proj")
		require.NoError(t, lockfile.Write(root, lockfile.Lockfile{
			PID: os.Getpid(), Addr: strings.TrimPrefix(ts.URL, "http://"), Root: root,
		}))
		t.Cleanup(func() { _ = lockfile.Remove(root) })

		s := RunDaemonStatus(context.Background(), root)
		require.True(t, s.Alive)
		require.False(t, s.Ready, "a process that is up but not serving is not ready")
	})

	t.Run("stale lockfile", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "stale-proj")
		require.NoError(t, lockfile.Write(root, lockfile.Lockfile{
			PID: 999999, Addr: "127.0.0.1:1", Root: root,
		}))
		t.Cleanup(func() { _ = lockfile.Remove(root) })

		s := RunDaemonStatus(context.Background(), root)
		require.True(t, s.Found)
		require.False(t, s.Alive)
		require.False(t, s.Ready, "a dead pid is never probed")
	})
}

// TestRenderDaemonStatus covers the three states an operator reads.
func TestRenderDaemonStatus(t *testing.T) {
	cases := []struct {
		name   string
		status DaemonStatus
		want   string
	}{
		{
			name:   "absent",
			status: DaemonStatus{Root: "/proj"},
			want:   "no daemon lockfile",
		},
		{
			name:   "ready",
			status: DaemonStatus{Found: true, Alive: true, Ready: true, PID: 7, Addr: "a:1", Root: "/proj"},
			want:   "ready",
		},
		{
			name:   "starting",
			status: DaemonStatus{Found: true, Alive: true, PID: 7, Addr: "a:1", Root: "/proj"},
			want:   "starting",
		},
		{
			name:   "stale",
			status: DaemonStatus{Found: true, PID: 7, Addr: "a:1", Root: "/proj"},
			want:   "stale",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sb strings.Builder
			RenderDaemonStatus(&sb, tc.status)
			require.Contains(t, sb.String(), tc.want)
		})
	}

	var sb strings.Builder
	RenderDaemonStatus(&sb, DaemonStatus{
		Found: true, Alive: true, Ready: true, PID: 7, Addr: "a:1", Root: "/proj",
		StartedAt: time.Now().Add(-time.Minute), Uptime: time.Minute,
	})
	require.Contains(t, sb.String(), "uptime")
}

// TestRunDaemonStopAndReloadWithoutDaemon asserts both control commands fail
// with a recognisable sentinel rather than a raw dial error, and that stop
// clears the litter a dead owner left behind.
func TestRunDaemonStopAndReloadWithoutDaemon(t *testing.T) {
	t.Run("no lockfile at all", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "nothing")
		require.ErrorIs(t, RunDaemonStop(context.Background(), root, time.Second), ErrNoDaemon)
		_, err := RunDaemonReload(context.Background(), root, "")
		require.ErrorIs(t, err, ErrNoDaemon)
	})

	t.Run("stop removes a stale lockfile", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "stale")
		require.NoError(t, lockfile.Write(root, lockfile.Lockfile{PID: 999999, Root: root}))
		t.Cleanup(func() { _ = lockfile.Remove(root) })

		err := RunDaemonStop(context.Background(), root, time.Second)
		require.ErrorIs(t, err, ErrNoDaemon)
		_, rerr := lockfile.Read(root)
		require.ErrorIs(t, rerr, lockfile.ErrNotFound,
			"a stale lockfile must be cleared, or the next window still thinks a backend is up")
	})

	t.Run("reload against a dead pid", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "stale-reload")
		require.NoError(t, lockfile.Write(root, lockfile.Lockfile{PID: 999999, Root: root}))
		t.Cleanup(func() { _ = lockfile.Remove(root) })

		_, err := RunDaemonReload(context.Background(), root, "")
		require.ErrorIs(t, err, ErrNoDaemon)
	})
}

// TestRunDaemonReloadAgainstALiveServer drives the whole client-to-handler
// round trip against a real HTTP server, so the wire format is exercised rather
// than assumed.
func TestRunDaemonReloadAgainstALiveServer(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeFile(t, filepath.Join(dir, "config.yaml"),
		"schema_version: 1\nserver:\n  http_addr: \"0.0.0.0:7777\"\n"+
			"storage:\n  sqlite_path: \"yanshi.db\"\n")

	running := &config.Config{}
	running.Server.HTTPAddr = "127.0.0.1:8080"
	running.Storage.SQLitePath = "yanshi.db"

	mux := http.NewServeMux()
	mux.HandleFunc(ControlPath, NewControlHandler(ControlHooks{
		ConfigPath:    cfgPath,
		CurrentConfig: func() *config.Config { return running },
		ApplyReload:   func(_ *config.Config, s []string) ([]string, error) { return s, nil },
	}))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	root := filepath.Join(dir, "proj")
	require.NoError(t, lockfile.Write(root, lockfile.Lockfile{
		PID: os.Getpid(), Addr: strings.TrimPrefix(ts.URL, "http://"), Root: root,
	}))
	t.Cleanup(func() { _ = lockfile.Remove(root) })

	resp, err := RunDaemonReload(context.Background(), root, "")
	require.NoError(t, err)
	require.True(t, resp.OK)

	var rejected []string
	for _, r := range resp.Rejected {
		rejected = append(rejected, r.Section)
	}
	require.Contains(t, rejected, "server.http_addr")
}

// TestRunDaemonReloadAgainstAnOldBackend covers the upgrade window: a running
// daemon built before `yanshi daemon` existed has no control route, and a raw
// "404" tells the operator nothing about what to do.
func TestRunDaemonReloadAgainstAnOldBackend(t *testing.T) {
	ts := httptest.NewServer(http.NewServeMux())
	t.Cleanup(ts.Close)

	root := filepath.Join(t.TempDir(), "old-proj")
	require.NoError(t, lockfile.Write(root, lockfile.Lockfile{
		PID: os.Getpid(), Addr: strings.TrimPrefix(ts.URL, "http://"), Root: root,
	}))
	t.Cleanup(func() { _ = lockfile.Remove(root) })

	_, err := RunDaemonReload(context.Background(), root, "")
	require.ErrorIs(t, err, ErrNoDaemon)
	require.Contains(t, err.Error(), "predates")
}

// TestRunDaemonStopWaitsForExit is the scriptability contract: `yanshi daemon
// stop && yanshi serve` is only safe if the first command returns after the
// port is free, so stop must not return while the PID still exists.
func TestRunDaemonStopWaitsForExit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(ControlPath, NewControlHandler(ControlHooks{Stop: func() {}}))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	root := filepath.Join(t.TempDir(), "wont-exit")
	// The lockfile names THIS process, which obviously never exits, so the
	// wait must time out rather than report a successful stop.
	require.NoError(t, lockfile.Write(root, lockfile.Lockfile{
		PID: os.Getpid(), Addr: strings.TrimPrefix(ts.URL, "http://"), Root: root,
	}))
	t.Cleanup(func() { _ = lockfile.Remove(root) })

	err := RunDaemonStop(context.Background(), root, 200*time.Millisecond)
	require.Error(t, err)
	require.Contains(t, err.Error(), "did not exit")

	_, rerr := lockfile.Read(root)
	require.NoError(t, rerr, "a stop that timed out must not remove a live lockfile")
}

// TestWaitForExitReturnsWhenPidIsGone covers the success side of the poll loop
// and its context cancellation path.
func TestWaitForExitReturnsWhenPidIsGone(t *testing.T) {
	require.NoError(t, waitForExit(context.Background(),
		lockfile.Lockfile{PID: 999999}, time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := waitForExit(ctx, lockfile.Lockfile{PID: os.Getpid()}, time.Minute)
	require.ErrorIs(t, err, context.Canceled)
}

// TestRenderControlResponseNamesRefusals asserts the console surface prints the
// boring half. An operator who changed http_addr, ran reload, saw "ok" and
// spent an afternoon wondering why the port did not move is exactly the failure
// this output prevents.
func TestRenderControlResponseNamesRefusals(t *testing.T) {
	var sb strings.Builder
	RenderControlResponse(&sb, ControlResponse{
		OK: true, Message: "re-read config.yaml",
		Applied:  []string{"profiles"},
		Rejected: []RejectedReload{{Section: "server.http_addr", Reason: "listener already bound"}},
	})
	out := sb.String()
	require.Contains(t, out, "reloaded: profiles")
	require.Contains(t, out, "NOT reloaded: server.http_addr")
	require.Contains(t, out, "listener already bound")
	require.Contains(t, out, "restart the daemon")

	sb.Reset()
	RenderControlResponse(&sb, ControlResponse{OK: true})
	require.Contains(t, sb.String(), "nothing changed")
}

// TestSectionFingerprintIsStableForUnknownNames asserts an unknown section
// never invents a change: two configs must always agree about a section the
// differ does not know how to read.
func TestSectionFingerprintIsStableForUnknownNames(t *testing.T) {
	a := &config.Config{}
	a.Server.HTTPAddr = "a"
	b := &config.Config{}
	b.Server.HTTPAddr = "b"
	require.Equal(t, sectionFingerprint(a, "no.such.section"),
		sectionFingerprint(b, "no.such.section"))
	require.Empty(t, sectionFingerprint(nil, "profiles"))
	require.NotEqual(t, sectionFingerprint(a, "server.http_addr"),
		sectionFingerprint(b, "server.http_addr"))
}
