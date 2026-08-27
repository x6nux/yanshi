package cli

// liverun_v3_test.go — the V3 timeline, checked through a running machine
// rather than through hand-inserted seam rows.
//
// The unit tests for Timeline construct seams and messages directly, which is
// the only way to isolate the ordinal join. What they cannot show is whether
// the two producers the join depends on — the WS handler's seam sealing and its
// message persistence — actually agree in a live process. They are written in
// different files, run at different instants (a pre-turn seam is sealed before
// the user message is durable), and the failure mode named in timeline.go's
// header is that the join silently resolves to the PREVIOUS question. A wrong
// label reads exactly like a right one, so nothing short of driving real turns
// with distinguishable prompts and reading the labels back can find it.
//
// This boots the full composition root, drives turns over the real WebSocket
// backend, and then asks the real VCS for the timeline.

import (
	"context"
	"net"
	"os"
	"path/filepath"

	"testing"

	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/bootstrap"
	"github.com/x6nux/yanshi/internal/vcs"
)

// TestLiveRun_TimelineLabelsRealTurnsWithTheirQuestions drives three turns
// through the assembled app and asserts the timeline names each turn by the
// prompt that opened it — in the right order, with no placeholder entries.
//
// Each prompt is unique and unambiguous, so an off-by-one join (the documented
// failure) surfaces as a label belonging to a different turn rather than as a
// missing one.
func TestLiveRun_TimelineLabelsRealTurnsWithTheirQuestions(t *testing.T) {
	root := t.TempDir()
	// The repo autoVCS tracks is the process working directory, so the app must
	// be built from inside a scratch project or the timeline would describe
	// this checkout.
	proj := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(proj, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(proj, "seed.txt"), []byte("seed\n"), 0o644))

	cfgPath := writeTestConfig(t, root)
	app, err := bootstrap.Build(bootstrap.Options{
		ConfigPath: cfgPath,
		FakeModel:  true,
		WorkRoot:   proj,
	})
	require.NoError(t, err, "bootstrap must assemble")
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })

	require.NotEmpty(t, app.VCSRepoID, "autoVCS must have initialised a repo for the scratch project")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = app.Serve(ln) }()

	ctx := context.Background()
	be, err := newWSBackend(ctx, "ws://"+ln.Addr().String()+"/api/v1/chat/ws")
	require.NoError(t, err)
	t.Cleanup(func() { _ = be.Close() })

	prompts := []string{
		"rename the config loader",
		"add a retry to the http client",
		"delete the unused parser",
	}
	for _, p := range prompts {
		ch, err := be.Send(ctx, p)
		require.NoErrorf(t, err, "send %q", p)
		for ev := range ch {
			if ev.Kind == "error" && ev.Err != nil {
				t.Fatalf("turn %q failed: %v", p, ev.Err)
			}
		}
	}

	entries, err := app.VCS.Timeline(app.VCSRepoID, vcs.TimelineOptions{Limit: 30})
	require.NoError(t, err)
	require.NotEmpty(t, entries, "three real turns must produce timeline entries")

	for _, e := range entries {
		t.Logf("seam=%s kind=%-9s turn=%d files=%d head=%v question=%q",
			short(e.SeamID), e.Kind, e.TurnSeq, e.FilesChanged, e.IsHead, e.Question)
	}

	// Collect the labelled turns, newest first as Timeline returns them.
	labelled := map[int]string{}
	for _, e := range entries {
		if e.Question == "" {
			continue
		}
		if prev, ok := labelled[e.TurnSeq]; ok && prev != e.Question {
			t.Errorf("turn %d carries two different questions: %q and %q",
				e.TurnSeq, prev, e.Question)
		}
		labelled[e.TurnSeq] = e.Question
	}
	if len(labelled) == 0 {
		t.Fatalf("every timeline entry is an unlabelled placeholder; the question join produced nothing")
	}
	for i, want := range prompts {
		turn := i + 1
		got, ok := labelled[turn]
		if !ok {
			t.Errorf("turn %d (%q) has no labelled timeline entry", turn, want)
			continue
		}
		if got != want {
			t.Errorf("turn %d is labelled %q, but the user actually asked %q", turn, got, want)
		}
	}
}

// short truncates an id for log lines.
func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
