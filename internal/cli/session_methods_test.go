package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	apihttp "github.com/x6nux/yanshi/internal/api/http"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/lockfile"
	"github.com/x6nux/yanshi/internal/proto"
)

// liveWSServer stands up a real apihttp server with a WS chat endpoint and a
// 200-OK healthz, returning its base URL. Used so Resolve/Reconnect can connect
// remote (non-owner) without bootstrapping or lockfiles.
func liveWSServer(t *testing.T) *httptest.Server {
	t.Helper()
	o, _ := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"r"}, nil)})
	srv := apihttp.New(apihttp.Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	srv.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// ctlBackend is a configurable ChatBackend for exercising Session's TUI-facing
// helpers (Send/SendFrame/CancelCurrent/Mode/Root). It can simulate a nil reply
// channel (permission_response), an error on Send or SendFrame, and records
// whether Cancel/Close were called.
type ctlBackend struct {
	mode     string
	sendErr  error
	frameCh  <-chan StreamEvent
	frameErr error
	canceled bool
	closed   bool
}

// SendTurn delegates to Send: these fakes exercise the text path only.
// It is on the interface rather than an optional capability so a backend
// that forgets it fails to compile — an optional one would silently drop
// every attachment.
func (b *ctlBackend) SendTurn(ctx context.Context, fr proto.ClientFrame) (<-chan StreamEvent, error) {
	return b.Send(ctx, fr.Text)
}

func (b *ctlBackend) Send(_ context.Context, _ string) (<-chan StreamEvent, error) {
	if b.sendErr != nil {
		return nil, b.sendErr
	}
	ch := make(chan StreamEvent, 1)
	ch <- StreamEvent{Kind: "done"}
	close(ch)
	return ch, nil
}

func (b *ctlBackend) SendFrame(_ context.Context, _ proto.ClientFrame) (<-chan StreamEvent, error) {
	if b.frameErr != nil {
		return nil, b.frameErr
	}
	return b.frameCh, nil
}

func (b *ctlBackend) Cancel() error { b.canceled = true; return nil }
func (b *ctlBackend) Close() error  { b.closed = true; return nil }
func (b *ctlBackend) Mode() string  { return b.mode }

// newCtlSession builds a Session and injects b as its resolved backend so the
// TUI-facing helpers can be driven without bootstrapping a real server.
func newCtlSession(b ChatBackend) *Session {
	s := newSession("/proj", "", false)
	s.backend = b
	return s
}

// TestSession_NilBackendHelpersCloseChannel proves the TUI-facing helpers behave
// gracefully when no backend is resolved yet (the nil-backend contract): Send and
// SendFrame return a closed channel (drained as empty), CancelCurrent is a no-op,
// and Mode is "".
func TestSession_NilBackendHelpersCloseChannel(t *testing.T) {
	s := newSession("/proj", "", false) // backend == nil

	ch := s.Send("hi")
	_, ok := <-ch
	assert.False(t, ok, "Send on nil backend returns a closed (empty) channel")

	fch := s.SendFrame(proto.NewGetStatus())
	_, ok = <-fch
	assert.False(t, ok, "SendFrame on nil backend returns a closed channel")

	require.NoError(t, s.CancelCurrent(), "CancelCurrent on nil backend is a no-op")
	assert.Equal(t, "", s.Mode(), "Mode on nil backend is empty")
	assert.Equal(t, "/proj", s.Root(), "Root is always the bound root")
}

// TestSession_SendSurfacesBackendError proves Send wraps a backend error as a
// single error event on a closed channel (the TUI treats it uniformly with a
// server error frame).
func TestSession_SendSurfacesBackendError(t *testing.T) {
	b := &ctlBackend{sendErr: errors.New("dropped")}
	s := newCtlSession(b)

	ch := s.Send("hi")
	var ev StreamEvent
	for e := range ch {
		ev = e
	}
	assert.Equal(t, "error", ev.Kind)
	require.NotNil(t, ev.Err)
	assert.Contains(t, ev.Err.Error(), "dropped")
}

// TestSession_SendFrameHappyPathAndErrors covers the three SendFrame outcomes:
// a reply channel passes through unchanged, a backend error becomes an error
// event channel, and a nil channel (permission_response) is returned as nil so
// the caller knows no reply is expected.
func TestSession_SendFrameHappyPathAndErrors(t *testing.T) {
	// 1. Reply channel passes through.
	reply := make(chan StreamEvent, 1)
	reply <- StreamEvent{Kind: "status"}
	close(reply)
	s := newCtlSession(&ctlBackend{mode: "ws", frameCh: reply})
	out := s.SendFrame(proto.NewGetStatus())
	got, ok := <-out
	require.True(t, ok)
	assert.Equal(t, "status", got.Kind)
	assert.Equal(t, "ws", s.Mode())

	// 2. Backend error -> single error event.
	s2 := newCtlSession(&ctlBackend{frameErr: errors.New("boom")})
	out2 := s2.SendFrame(proto.NewGetStatus())
	ev := <-out2
	assert.Equal(t, "error", ev.Kind)
	assert.Contains(t, ev.Err.Error(), "boom")

	// 3. Nil channel (permission_response) -> nil return.
	s3 := newCtlSession(&ctlBackend{})
	assert.Nil(t, s3.SendFrame(proto.NewPermissionResponse("id", "allow")))
}

// TestSession_CancelCurrentDelegatesToBackend proves CancelCurrent forwards to
// the backend's Cancel.
func TestSession_CancelCurrentDelegatesToBackend(t *testing.T) {
	b := &ctlBackend{}
	s := newCtlSession(b)
	require.NoError(t, s.CancelCurrent())
	assert.True(t, b.canceled, "CancelCurrent must call backend.Cancel")
}

// TestSession_NewSessionResolvesEmptyRoot proves NewSession resolves an empty
// Root to the working directory (not left empty), and setForced records both
// forced modes.
func TestSession_NewSessionResolvesEmptyRoot(t *testing.T) {
	s := NewSession(Options{Server: "http://x", InProcess: true})
	assert.NotEmpty(t, s.Root(), "empty Root resolves to the cwd")
	assert.True(t, s.forcedInProc)
	assert.Equal(t, "http://x", s.forcedRemote)

	s2 := NewSession(Options{Root: "/explicit"})
	assert.Equal(t, "/explicit", s2.Root())
}

// TestSession_ReconnectNonOwnerDropsBackend proves the non-owner Reconnect path
// closes the dead backend and re-resolves (re-connecting to the live remote).
func TestSession_ReconnectNonOwnerDropsBackend(t *testing.T) {
	ts := liveWSServer(t)
	root := t.TempDir()
	s := newSession(root, "", true)
	s.setForced(ts.URL, false) // forcedRemote -> Resolve connects (non-owner)
	require.NoError(t, s.Resolve(context.Background()))
	t.Cleanup(func() { _ = s.Close() })
	require.False(t, s.IsOwner(), "forced remote connects as non-owner")

	// Simulate the backend going dead: swap in a recording ctlBackend.
	dead := &ctlBackend{}
	s.mu.Lock()
	s.backend = dead
	s.mu.Unlock()

	// Reconnect must close the dead backend and re-resolve (re-connect remote).
	require.NoError(t, s.Reconnect(context.Background()))
	assert.True(t, dead.closed, "dead backend was closed on reconnect")
	assert.False(t, s.IsOwner(), "reconnected remote stays non-owner")
	assert.Equal(t, "ws", s.Mode(), "a fresh live backend was re-resolved")
}

// TestSession_ReconnectOwnerIsNoop proves an owner's Reconnect is a no-op that
// keeps the existing backend (its own server should still be up).
func TestSession_ReconnectOwnerIsNoop(t *testing.T) {
	root := t.TempDir()
	s := newSession(root, writeTestConfig(t, t.TempDir()), true)
	s.setForced("", true)
	require.NoError(t, s.Resolve(context.Background()))
	t.Cleanup(func() { _ = s.Close() })

	before := s.Backend()
	require.NoError(t, s.Reconnect(context.Background()))
	assert.Equal(t, before, s.Backend(), "owner Reconnect keeps the same backend")
}

// TestBootstrapOwner_LosesToAliveOwner exercises the !won branch: when
// lockfile.Acquire finds an existing LIVE owner, bootstrapOwner tears its server
// down and connects to the winner. discovery would normally connect to the live
// owner first, so this calls bootstrapOwner directly to reach the race path.
func TestBootstrapOwner_LosesToAliveOwner(t *testing.T) {
	ts := liveWSServer(t)
	root := t.TempDir()
	// Pre-claim the lockfile with THIS process's (alive) PID pointing at the live
	// test server so Acquire loses and bootstrapOwner connectRemote's to it.
	require.NoError(t, lockfile.Write(root, lockfile.Lockfile{
		PID: os.Getpid(), Addr: ts.Listener.Addr().String(), Auth: "none", Root: root,
	}))
	s := newSession(root, writeTestConfig(t, t.TempDir()), true)
	require.NoError(t, s.bootstrapOwner(context.Background()))
	t.Cleanup(func() { _ = s.Close() })
	assert.False(t, s.IsOwner(), "lost the Acquire race -> connected to the winner")
	assert.Equal(t, "ws", s.Mode())
}

// TestBootstrapOwner_BadConfigReturnsError proves a config that fails to load
// surfaces a wrapped "bootstrap:" error from bootstrapOwner.
func TestBootstrapOwner_BadConfigReturnsError(t *testing.T) {
	root := t.TempDir()
	s := newSession(root, filepath.Join(t.TempDir(), "missing-config.yaml"), false)
	err := s.bootstrapOwner(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bootstrap")
}

// TestRunHeadless_NoInputReturnsError proves RunHeadless rejects an empty input
// list before resolving a backend.
func TestRunHeadless_NoInputReturnsError(t *testing.T) {
	_, err := RunHeadless(context.Background(), Options{}, HeadlessRunOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no input")
}

// TestRunHeadless_NilBackendReturnsError proves runHeadlessWithBackend rejects a
// nil backend with a clear error.
func TestRunHeadless_NilBackendReturnsError(t *testing.T) {
	_, err := runHeadlessWithBackend(context.Background(), nil, HeadlessRunOptions{
		Inputs: []HeadlessInput{{Prompt: "hi"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

// TestProjectHeadlessEvent_MapsErrToString proves projectHeadlessEvent surfaces
// ev.Err as the Error string and carries through the stable fields.
func TestProjectHeadlessEvent_MapsErrToString(t *testing.T) {
	ev := StreamEvent{Kind: "error", Err: errors.New("conn reset"), Text: "ignored"}
	out := projectHeadlessEvent(ev)
	assert.Equal(t, "error", out.Type)
	assert.Equal(t, "conn reset", out.Error, "Err.Error() surfaces as the Error string")
}

// TestRenderHeadlessEvent_TextModeDelegates proves text mode renders agent_chunk
// to stdout and tool activity to stderr (delegating to renderExecEvent).
func TestRenderHeadlessEvent_TextModeDelegates(t *testing.T) {
	var stdout, stderr bytes.Buffer
	renderHeadlessEvent(&stdout, &stderr, ExecOutputText, StreamEvent{
		Kind: "agent_chunk", Text: "hello",
	})
	assert.Contains(t, stdout.String(), "hello")

	var out2, err2 bytes.Buffer
	renderHeadlessEvent(&out2, &err2, ExecOutputText, StreamEvent{
		Kind: "tool_call", ToolName: "fs_read", ToolArgs: "{}",
	})
	assert.Contains(t, err2.String(), "fs_read")
}

// TestExecEventText_PrefersErr proves execEventText prefers ev.Err over ev.Text.
func TestExecEventText_PrefersErr(t *testing.T) {
	assert.Equal(t, "boom",
		execEventText(StreamEvent{Kind: "error", Err: errors.New("boom"), Text: "ignored"}))
	assert.Equal(t, "server said",
		execEventText(StreamEvent{Kind: "error", Text: "server said"}))
	assert.Equal(t, "",
		execEventText(StreamEvent{Kind: "error"}))
}

// TestRunHeadless_FakeModelTwoPrompts is the composition proof: RunHeadless
// resolves an in-process fake-model backend and drives two prompts, surfacing a
// non-empty session id and Completed==2.
func TestRunHeadless_FakeModelTwoPrompts(t *testing.T) {
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	res, err := RunHeadless(context.Background(),
		Options{Root: root, ConfigPath: writeTestConfig(t, root), FakeModel: true},
		HeadlessRunOptions{
			Inputs: []HeadlessInput{{Prompt: "one"}, {Prompt: "two"}},
			Output: ExecOutputText,
			Stdout: &stdout,
			Stderr: &stderr,
		})
	require.NoError(t, err)
	assert.Equal(t, 2, res.Completed)
	assert.NotEmpty(t, res.SessionID)
}
