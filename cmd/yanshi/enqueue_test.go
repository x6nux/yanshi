package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/store"
)

// queueFixture writes a config pointing at a fresh database, creates a session
// in it, and returns the config path and session id.
func queueFixture(t *testing.T) (configPath, sessionID string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "yanshi.db")
	configPath = filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath,
		[]byte("storage:\n  sqlite_path: "+filepath.ToSlash(dbPath)+"\n"), 0o600))

	s, err := store.Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()
	sessionID, err = s.CreateSession("queued")
	require.NoError(t, err)
	return configPath, sessionID
}

// openFixtureStore reopens the database a config points at, through the same
// loader the production path uses — an ad-hoc YAML split gets the quoting wrong
// on the first fixture that quotes its path.
func openFixtureStore(t *testing.T, configPath string) *store.Store {
	t.Helper()
	cfg, err := config.Load(configPath)
	require.NoError(t, err)
	s, err := store.Open(cfg.Storage.SQLitePath)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

// TestRunEnqueue_QueuesAndLists exercises the producer end to end: the message
// reaches the database a separate handle can read, which is the whole claim of
// `yanshi enqueue` (the backend is usually another process).
func TestRunEnqueue_QueuesAndLists(t *testing.T) {
	configPath, sid := queueFixture(t)
	var out bytes.Buffer

	code := runEnqueue([]string{"-config", configPath, sid, "run", "the", "release", "script"}, &out)
	require.Equal(t, exitOK, code)
	require.Contains(t, out.String(), "1 message(s) waiting")

	s := openFixtureStore(t, configPath)
	pending, err := s.PendingQueuedMessages(sid)
	require.NoError(t, err)
	require.Equal(t, []string{"run the release script"}, pending,
		"trailing arguments must be joined with spaces")

	out.Reset()
	code = runEnqueue([]string{"-config", configPath, "-list", sid}, &out)
	require.Equal(t, exitOK, code)
	require.Contains(t, out.String(), "run the release script")

	// -list must not consume: asking what is waiting cannot empty the queue.
	pending, err = s.PendingQueuedMessages(sid)
	require.NoError(t, err)
	require.Len(t, pending, 1)
}

// TestRunEnqueue_BadInvocations: a mistyped session id, a missing message and a
// missing config must each fail loudly rather than parking a message nothing
// will ever drain.
func TestRunEnqueue_BadInvocations(t *testing.T) {
	configPath, sid := queueFixture(t)
	var out bytes.Buffer

	require.Equal(t, exitUsage, runEnqueue([]string{"-config", configPath}, &out))
	require.Equal(t, exitUsage, runEnqueue([]string{"-config", configPath, sid}, &out))
	require.Equal(t, exitErr,
		runEnqueue([]string{"-config", configPath, "no-such-session", "hello"}, &out))
	require.Equal(t, exitErr,
		runEnqueue([]string{"-config", filepath.Join(t.TempDir(), "missing.yaml"), sid, "hi"}, &out))
}

// TestDrainQueue_ConsumesOnResume is the consumer half of acceptance clause 3,
// at the seam runHeadlessCommand actually calls.
//
// Deleting the drainQueue call in runHeadlessCommand does NOT make this red on
// its own — TestRunHeadless_ResumeDrainsTheQueue is the one that covers
// the wiring. This covers the function's own contract: order preserved,
// delivered once.
func TestDrainQueue_ConsumesOnResume(t *testing.T) {
	configPath, sid := queueFixture(t)
	s := openFixtureStore(t, configPath)
	for _, m := range []string{"first", "second", "third"} {
		_, err := s.EnqueueMessage(sid, m)
		require.NoError(t, err)
	}

	got := drainQueue(configPath, sid)
	require.Len(t, got, 3)
	require.Equal(t, "first", got[0].Prompt)
	require.Equal(t, "second", got[1].Prompt)
	require.Equal(t, "third", got[2].Prompt)

	require.Empty(t, drainQueue(configPath, sid), "a drained queue must stay drained")
}

// TestDrainQueue_FailuresYieldNothing: the queue is an addition to the run, not
// a precondition. A missing config or a session with no queue must return an
// empty slice rather than blowing up a prompt the user typed.
func TestDrainQueue_FailuresYieldNothing(t *testing.T) {
	configPath, sid := queueFixture(t)
	require.Empty(t, drainQueue(filepath.Join(t.TempDir(), "missing.yaml"), sid))
	require.Empty(t, drainQueue(configPath, sid))
	require.Empty(t, drainQueue(configPath, "no-such-session"))
}

// TestRunHeadless_ResumeDrainsTheQueue is the WIRING guard for W-D-08's
// consumer: it fails if the drainQueue call is removed from
// runHeadlessCommand.
//
// It drives the real entry point — the same one `yanshi exec -resume` reaches —
// and then asserts the queue is EMPTY. That is the consumption side, which is
// what the last round of this work package kept getting wrong: a test that
// checked drainQueue in isolation, or scanned the source for the call, stays
// green while the value never travels. The resumed session id does not exist in
// the backend, so the run itself fails; that is irrelevant here and deliberately
// unasserted, because the drain happens before the turn either way.
func TestRunHeadless_ResumeDrainsTheQueue(t *testing.T) {
	cfgPath := writeServeConfig(t)
	s := openFixtureStore(t, cfgPath)
	sid, err := s.CreateSession("queued")
	require.NoError(t, err)
	_, err = s.EnqueueMessage(sid, "delivered on resume")
	require.NoError(t, err)

	done := make(chan int, 1)
	go func() {
		done <- runHeadlessCommand([]string{
			"-config", cfgPath, "-fake-model", "-inprocess", "-p", "hi", "-resume", sid,
		}, "exec", strings.NewReader(""))
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runHeadlessCommand -resume did not terminate")
	}

	pending, err := s.PendingQueuedMessages(sid)
	require.NoError(t, err)
	require.Empty(t, pending,
		"resuming a session must consume its queue; the drain call is missing from runHeadlessCommand")
}

// TestRunHeadless_NoResumeLeavesTheQueueAlone is the other direction: a run
// that resumes nothing must not drain somebody else's queue. Without it, a
// drainQueue call placed outside the `if cfg.Resume != ""` branch would pass
// the test above.
func TestRunHeadless_NoResumeLeavesTheQueueAlone(t *testing.T) {
	cfgPath := writeServeConfig(t)
	s := openFixtureStore(t, cfgPath)
	sid, err := s.CreateSession("untouched")
	require.NoError(t, err)
	_, err = s.EnqueueMessage(sid, "still waiting")
	require.NoError(t, err)

	done := make(chan int, 1)
	go func() {
		done <- runHeadlessCommand([]string{
			"-config", cfgPath, "-fake-model", "-inprocess", "-p", "hi",
		}, "exec", strings.NewReader(""))
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runHeadlessCommand did not terminate")
	}

	pending, err := s.PendingQueuedMessages(sid)
	require.NoError(t, err)
	require.Len(t, pending, 1)
}

// TestQueuedFirst_QueueLeadsTypedInput pins the DELIVERY ORDER that both the
// `-h` text and docs/user-guide/entrypoints.md promise.
//
// It is the assertion the concatenation had nowhere to live before: reversing
// it to append the queue AFTER the typed prompt left every existing test green,
// because TestDrainQueue_ConsumesOnResume only checks the order drainQueue
// itself returns and TestRunHeadless_ResumeDrainsTheQueue only checks that the
// queue was emptied.
func TestQueuedFirst_QueueLeadsTypedInput(t *testing.T) {
	configPath, sid := queueFixture(t)
	s := openFixtureStore(t, configPath)
	for _, m := range []string{"queued one", "queued two"} {
		_, err := s.EnqueueMessage(sid, m)
		require.NoError(t, err)
	}

	got := queuedFirst(configPath, sid, []cli.HeadlessInput{{Prompt: "typed on the command line"}})
	require.Len(t, got, 3)
	require.Equal(t, []string{"queued one", "queued two", "typed on the command line"},
		[]string{got[0].Prompt, got[1].Prompt, got[2].Prompt},
		"the queue leads, in enqueue order, ahead of this invocation's own input")

	// A session with nothing queued must leave the caller's input exactly as it
	// was — otherwise a nil-vs-empty slip would show up as a dropped prompt.
	own := []cli.HeadlessInput{{Prompt: "only this"}}
	require.Equal(t, own, queuedFirst(configPath, sid, own))
}

// TestEnqueueUsage_AdvertisesOnlyResumePathsThatParse closes the phantom the
// usage text carried for three releases: it told the user to run
// `yanshi chat -resume <id>`, and plain `chat` has no such flag — it routes to
// the headless entry point only when it sees --no-tui, and its own flag set
// defines config/fake-model/server/token/inprocess and nothing else.
//
// Every `-resume` invocation the text advertises is parsed here through the
// exact path that word reaches, so an advertisement and a flag set cannot drift
// apart again. The interactive form is asserted to be REJECTED, which is the
// half a reader of the usage text cannot check.
func TestEnqueueUsage_AdvertisesOnlyResumePathsThatParse(t *testing.T) {
	var advertised int
	for _, line := range strings.Split(enqueueUsage, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "yanshi ") || !strings.Contains(line, "-resume") {
			continue
		}
		advertised++
		fields := strings.Fields(strings.ReplaceAll(line, `"..."`, "hi"))
		command, args := fields[1], fields[2:]
		noTUI, filtered := splitNoTUI(args)
		require.Truef(t, command == "exec" || noTUI,
			"%q advertises -resume on the interactive TUI, which has no such flag", line)
		cfg, err := parseHeadlessArgs(filtered, command)
		require.NoErrorf(t, err, "advertised invocation does not parse: %q", line)
		require.NotEmptyf(t, cfg.Resume, "advertised invocation does not set Resume: %q", line)
	}
	require.Equal(t, 2, advertised, "both real resume paths must stay advertised")

	// The negative half: the interactive TUI rejects -resume before it starts.
	// It is what makes the "no -resume flag" sentence in the usage text a
	// checked claim rather than a description of another file.
	stderr := os.Stderr
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	require.NoError(t, err)
	os.Stderr = devNull
	code := chatTUI([]string{"-resume", "whatever"})
	os.Stderr = stderr
	require.NoError(t, devNull.Close())
	require.Equal(t, exitUsage, code, "plain `yanshi chat` must reject -resume")
}
