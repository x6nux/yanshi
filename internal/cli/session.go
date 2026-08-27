package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/x6nux/yanshi/internal/bootstrap"
	"github.com/x6nux/yanshi/internal/lockfile"
	obslog "github.com/x6nux/yanshi/internal/observe/log"
	"github.com/x6nux/yanshi/internal/proto"
)

// Options configures Run (and Session) — mirrors the CLI flags.
type Options struct {
	ConfigPath string
	FakeModel  bool
	Server     string // force remote (skip discovery) when non-empty
	InProcess  bool   // force in-process (skip discovery)
	Root       string // project root (cwd); defaults to os.Getwd at Run time
}

// Session resolves and holds a backend, bootstrapping an in-process backend
// (and owning a lockfile) when no live one is found.
type Session struct {
	root       string
	configPath string
	fakeModel  bool

	// forced modes, set by setForced from Options (Server/InProcess). When set
	// they short-circuit Resolve past lockfile discovery.
	forcedRemote string
	forcedInProc bool

	mu       sync.Mutex
	backend  ChatBackend
	owner    bool
	app      *bootstrap.App
	serveErr chan error
}

func newSession(root, configPath string, fakeModel bool) *Session {
	return &Session{root: root, configPath: configPath, fakeModel: fakeModel}
}

// NewSession is the exported Session constructor used by the composition root
// (cmd/yanshi). It builds a session from Options — resolving Root to the
// working directory when empty — and applies the forced-remote / forced-in-process
// flags so a subsequent Resolve(ctx) honors them.
//
// cmd/yanshi uses this (rather than a cli.Run helper) because package cli
// cannot import package tui (the tui package depends on cli.StreamEvent), so the
// cli→tui wiring must happen in package main. runHeadless remains the in-package
// test seam.
func NewSession(opts Options) *Session {
	root := opts.Root
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	s := newSession(root, opts.ConfigPath, opts.FakeModel)
	s.setForced(opts.Server, opts.InProcess)
	return s
}

// setForced configures forced modes (called by Run from Options).
func (s *Session) setForced(remote string, inProcess bool) {
	s.forcedRemote = remote
	s.forcedInProc = inProcess
}

// Resolve discovers a backend (lockfile + readiness probe) and connects; if
// none is live, bootstraps one in-process, binds 127.0.0.1:0, writes a
// lockfile, and marks this session as the owner. Always selects WS->SSE
// transport.
//
// Order: forcedInProc → forcedRemote → discovery (lockfile.Read → Alive &&
// ready → connectRemote; stale → Remove → fall through) → bootstrapOwner.
//
// The probe is READINESS, not liveness: a process that has claimed the
// lockfile but is still assembling its store / VCS answers /healthz with 200
// long before it can serve a turn, so a second window that trusted liveness
// would connect to a backend that is not there yet. See cli.ready for the
// 404 fallback that keeps this working against an older owner.
func (s *Session) Resolve(ctx context.Context) error {
	// 1. Forced in-process: skip discovery, always bootstrap.
	if s.forcedInProc {
		return s.bootstrapOwner(ctx)
	}
	// 2. Forced remote: skip discovery, connect to the given origin.
	if s.forcedRemote != "" {
		return s.connectRemote(ctx, s.forcedRemote)
	}

	// 3. Discovery via lockfile.
	lf, err := lockfile.Read(s.root)
	if err == nil {
		if lf.Alive() && ready(ctx, "http://"+lf.Addr) {
			return s.connectRemote(ctx, "http://"+lf.Addr)
		}
		_ = lockfile.Remove(s.root) // stale: dead PID or unready backend
	}

	// 4. Bootstrap in-process and become the owner.
	return s.bootstrapOwner(ctx)
}

// connectRemote selects a transport (WS primary, SSE fallback) and stores it.
// The session is NOT the owner in this path.
func (s *Session) connectRemote(ctx context.Context, baseURL string) error {
	b, err := newBackend(ctx, baseURL)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.backend = b
	s.owner = false
	s.mu.Unlock()
	return nil
}

// bootstrapOwner builds the app, binds an ephemeral loopback port, atomically
// claims the lockfile, and selects a transport against the local server. If the
// lockfile Acquire loses to a concurrent owner, the local server is torn down
// and we connect to the winner instead.
func (s *Session) bootstrapOwner(ctx context.Context) error {
	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: s.configPath, FakeModel: s.fakeModel, TUIMode: true})
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = app.Shutdown(ctx)
		return err
	}
	addr := ln.Addr().String()

	// Atomically claim the lockfile; if we lose, another owner just started —
	// tear our server down and connect to the winner.
	won, err := lockfile.Acquire(s.root, lockfile.Lockfile{
		PID: os.Getpid(), Addr: addr, Auth: "none", Root: s.root,
	})
	if err != nil {
		_ = ln.Close()
		_ = app.Shutdown(ctx)
		return err
	}
	if !won {
		_ = ln.Close()
		_ = app.Shutdown(ctx)
		winner, rerr := lockfile.Read(s.root)
		if rerr != nil {
			return rerr
		}
		return s.connectRemote(ctx, "http://"+winner.Addr)
	}

	s.serveErr = make(chan error, 1)
	go func() { s.serveErr <- app.Serve(ln) }()

	b, err := newBackend(ctx, "http://"+addr)
	if err != nil {
		_ = app.Shutdown(ctx)
		_ = lockfile.Remove(s.root)
		return err
	}

	s.mu.Lock()
	s.backend = b
	s.owner = true
	s.app = app
	s.mu.Unlock()
	return nil
}

// Backend returns the resolved backend.
func (s *Session) Backend() ChatBackend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.backend
}

// IsOwner reports whether this session bootstrapped the backend.
func (s *Session) IsOwner() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owner
}

// Reconnect re-resolves a backend after the current one dropped. If we were the
// owner we keep going (our own server should still be up); otherwise we drop the
// dead backend and re-run Resolve — and if no live backend remains we bootstrap
// (becoming the new owner). This is the multi-window self-heal path: on
// owner-exit, the first disconnected client to find no live backend re-bootstraps.
func (s *Session) Reconnect(ctx context.Context) error {
	s.mu.Lock()
	wasOwner := s.owner
	s.mu.Unlock()

	if wasOwner {
		// Our own server should still be up; nothing to do.
		return nil
	}
	// Drop the old (dead) backend and re-resolve.
	s.mu.Lock()
	if s.backend != nil {
		_ = s.backend.Close()
		s.backend = nil
	}
	s.mu.Unlock()
	return s.Resolve(ctx)
}

// Close releases the backend; if owner, stops the server and removes lockfile.
func (s *Session) Close() error {
	s.mu.Lock()
	b := s.backend
	owner := s.owner
	app := s.app
	s.mu.Unlock()

	if b != nil {
		_ = b.Close()
	}
	if owner {
		if app != nil {
			_ = app.Shutdown(context.Background())
		}
		_ = lockfile.Remove(s.root)
	}
	// Surface a failed in-process server (serveErr is written by the serve
	// goroutine in bootstrapOwner). Non-blocking: Shutdown has returned, so the
	// serve goroutine has typically already posted its result.
	if s.serveErr != nil {
		select {
		case err := <-s.serveErr:
			if err != nil {
				obslog.WarnErr(context.Background(), "in-process server stopped", err)
			}
		default:
		}
	}
	return nil
}

// --- TUI-facing helpers (used by the TUI via its tuiSession interface). ---
// These delegate to the backend; runHeadless uses Backend().Send directly.

// Send delivers one user turn and returns its event stream (closed on
// done/error). It is the TUI-facing entry: no context/error so it matches the
// tuiSession interface the bubbletea model depends on.
func (s *Session) Send(text string) <-chan StreamEvent {
	s.mu.Lock()
	b := s.backend
	s.mu.Unlock()
	if b == nil {
		ch := make(chan StreamEvent)
		close(ch)
		return ch
	}
	ch, err := b.Send(context.Background(), text)
	if err != nil {
		out := make(chan StreamEvent, 1)
		out <- StreamEvent{Kind: "error", Err: err}
		close(out)
		return out
	}
	return ch
}

// SendTurn delivers one user turn as a full frame, so it can carry @path
// attachments or images.
//
// It is NOT SendFrame. On the WS backend SendFrame sets controlMode, which
// closes the stream on the first control reply rather than on done — an image
// turn sent that way ends before the model has answered. That is what the
// image path was doing.
func (s *Session) SendTurn(f proto.ClientFrame) <-chan StreamEvent {
	s.mu.Lock()
	b := s.backend
	s.mu.Unlock()
	if b == nil {
		ch := make(chan StreamEvent)
		close(ch)
		return ch
	}
	ch, err := b.SendTurn(context.Background(), f)
	if err != nil {
		out := make(chan StreamEvent, 1)
		out <- StreamEvent{Kind: "error", Err: err}
		close(out)
		return out
	}
	return ch
}

// SendFrame writes a Phase-10 control frame (set_model / list_models / clear /
// get_status / compact / list_mcp / permission_response) and returns the reply
// stream. For request frames the channel receives the server's single-frame
// reply then closes; for permission_response (a mid-turn reply with no ack) the
// backend returns nil and so does this. A nil/error backend yields an empty or
// error channel — matching Send's contract so the TUI treats both uniformly.
func (s *Session) SendFrame(f proto.ClientFrame) <-chan StreamEvent {
	s.mu.Lock()
	b := s.backend
	s.mu.Unlock()
	if b == nil {
		ch := make(chan StreamEvent)
		close(ch)
		return ch
	}
	ch, err := b.SendFrame(context.Background(), f)
	if err != nil {
		out := make(chan StreamEvent, 1)
		out <- StreamEvent{Kind: "error", Err: err}
		close(out)
		return out
	}
	if ch == nil {
		return nil // permission_response: no reply expected
	}
	return ch
}

// CancelCurrent aborts the in-flight turn.
func (s *Session) CancelCurrent() error {
	s.mu.Lock()
	b := s.backend
	s.mu.Unlock()
	if b == nil {
		return nil
	}
	return b.Cancel()
}

// Mode reports the transport of the resolved backend ("ws"/"sse"/"fake"), or
// "" before Resolve succeeds.
func (s *Session) Mode() string {
	s.mu.Lock()
	b := s.backend
	s.mu.Unlock()
	if b == nil {
		return ""
	}
	return b.Mode()
}

// Root returns the project root this session is bound to.
func (s *Session) Root() string { return s.root }
