// Package execbroker intercepts privilege-elevation programs invoked from
// INSIDE a running child and gets each one adjudicated on its own.
//
// # The gap it closes
//
// guard judges the command a tool was asked to run. It does not — and cannot —
// see what that command does once it is running. A model that writes
//
//	#!/bin/sh
//	make build
//	sudo make install        # line 3
//	./run-tests
//
// and then asks to run the script has one thing approved: the script. Line 3
// executes with whatever privileges the operator's sudoers file grants, and the
// only record of it is in the output. The outer approval is not evidence about
// the inner command, because at approval time the inner command had not been
// written down anywhere the guard could read.
//
// The interception is a shim directory prepended to the child's PATH containing
// one symlink per elevation program, each pointing at the yanshi binary. When
// the child runs `sudo`, it gets the shim; the shim asks the parent over a unix
// socket; the parent runs the SAME guard pipeline it would have run for a
// top-level shell_run, including the interactive approval callback; and the shim
// either execs the real program or exits without doing anything. The script does
// not restart, and nothing else in it is re-approved — which is the property
// that makes this usable rather than an interruption.
//
// # What it does not catch, stated rather than implied
//
// PATH interposition is defeated by an absolute path. A script that runs
// /usr/bin/sudo, or that resets PATH itself, never sees the shim. This is
// therefore a control over ACCIDENTAL and ordinary elevation, not a boundary
// against a child that is actively evading it — for that, the sandbox tier and
// the seccomp filter are the mechanisms, because they are enforced by the
// kernel rather than by a lookup rule the child controls.
//
// Windows is not covered: the shims are symlinks, which need a privilege a
// normal user does not have there. Listen reports that honestly rather than
// installing something that half works.
//
// # Fail-closed
//
// Every error path in the shim exits WITHOUT exec'ing: no broker address in the
// environment, a socket that will not dial, a bad token, a malformed reply, a
// server that went away mid-request. The reason is that all of those are
// indistinguishable, from inside the shim, from "the parent is not there to
// approve this" — and a shim that ran the program when it could not ask is a
// shim that an attacker removes an environment variable to disable.
package execbroker

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// SocketEnv names the environment variable carrying the broker's unix socket
// path, TokenEnv the per-launch shared secret, and ShimDirEnv the directory the
// shims live in.
//
// ShimDirEnv is not redundant with PATH: the shim has to resolve the REAL
// program, and the only reliable way to skip itself is to know which directory
// to ignore. Deriving it from os.Args[0] would work until something invoked the
// shim through a second symlink.
const (
	SocketEnv  = "YANSHI_EXEC_BROKER"
	TokenEnv   = "YANSHI_EXEC_BROKER_TOKEN"
	ShimDirEnv = "YANSHI_EXEC_SHIM_DIR"
)

// InterceptedPrograms are the names a shim is installed for.
//
// The list is elevation, not "dangerous": these four are the standard ways a
// process asks the OS for privileges it was not started with. Adding a program
// here means every child that runs it pays an approval round-trip, so the bar
// is "this changes who the command runs as", not "this can do damage" — a
// shim for `rm` would intercept every build system on earth and teach operators
// to approve without reading.
var InterceptedPrograms = []string{"sudo", "doas", "su", "pkexec"}

// Request is what a shim sends. Dir is the shim's working directory, which is
// the working directory of the script line that invoked it — not the outer
// command's, which may have cd'd since.
type Request struct {
	Token   string   `json:"token"`
	Program string   `json:"program"`
	Args    []string `json:"args"`
	Dir     string   `json:"dir"`
}

// Response is the parent's verdict. Reason is non-empty on a denial and is
// printed by the shim, so the operator reading the child's output learns why a
// line failed rather than seeing a bare non-zero exit.
type Response struct {
	Allow  bool   `json:"allow"`
	Reason string `json:"reason,omitempty"`
}

// Decider is the seam that keeps this package out of the tools/guard import
// graph: it returns nil to allow and an error to deny, with the error text
// travelling back to the child.
//
// It is a function rather than an interface for the same reason
// secproc.Authorizer is: the real implementation lives in a package that
// already depends on this one, and inverting through a value is what stops the
// cycle.
type Decider func(ctx context.Context, req Request) error

// Server is one launch's broker: a unix socket, a shim directory, and the
// goroutine that answers.
type Server struct {
	listener net.Listener
	shimDir  string
	token    string
	decide   Decider
	ctx      context.Context

	closeOnce sync.Once
}

// ErrUnsupported is returned by Listen on a platform where the shims cannot be
// installed. Callers treat it as "no interception here" and carry on: it is a
// reduced posture, identical to the behaviour before this package existed, and
// refusing to spawn anything at all on Windows would be a much larger
// regression than the control is worth.
var ErrUnsupported = fmt.Errorf("execbroker: shim interception is not supported on this platform")

// Listen creates the shim directory and starts the broker.
//
// exe must be the absolute path of the yanshi binary, resolved by the caller
// from os.Executable rather than os.Args[0]: argv[0] is attacker-influenced and
// choosing WHICH BINARY answers as sudo based on a caller-supplied string is
// the whole game.
//
// The socket lives inside the same 0700 directory as the shims, so the token
// check below is a second lock on a door that is already closed to other local
// users. It is still checked, because the directory mode is a property of the
// filesystem the temp dir landed on and the token is a property of this code.
func Listen(ctx context.Context, exe string, decide Decider) (*Server, error) {
	if runtime.GOOS == "windows" {
		return nil, ErrUnsupported
	}
	if decide == nil {
		return nil, fmt.Errorf("execbroker: a nil Decider would allow everything (fail-closed)")
	}
	if !filepath.IsAbs(exe) {
		return nil, fmt.Errorf("execbroker: exe %q must be absolute", exe)
	}
	dir, err := os.MkdirTemp("", "yanshi-execshim-")
	if err != nil {
		return nil, fmt.Errorf("execbroker: shim dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("execbroker: shim dir mode: %w", err)
	}
	for _, name := range InterceptedPrograms {
		if err := os.Symlink(exe, filepath.Join(dir, name)); err != nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("execbroker: shim for %s: %w", name, err)
		}
	}
	// The socket path is bounded by the platform's sun_path limit (104 bytes on
	// darwin, 108 on linux). A temp dir under /tmp plus this name is far inside
	// it, but the failure mode if it were not — bind returning "invalid
	// argument" — is opaque enough to be worth naming here.
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("execbroker: listen: %w", err)
	}
	token, err := newToken()
	if err != nil {
		_ = ln.Close()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	s := &Server{listener: ln, shimDir: dir, token: token, decide: decide, ctx: ctx}
	go s.serve()
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()
	return s, nil
}

// newToken mints the per-launch shared secret.
func newToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("execbroker: token: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// ShimDir is the directory to prepend to the child's PATH.
func (s *Server) ShimDir() string { return s.shimDir }

// Env returns the entries a child needs to reach this broker, EXCLUDING PATH —
// the caller owns PATH because it has to merge, not replace.
func (s *Server) Env() []string {
	return []string{
		SocketEnv + "=" + s.listener.Addr().String(),
		TokenEnv + "=" + s.token,
		ShimDirEnv + "=" + s.shimDir,
	}
}

// Close stops the broker and removes the shim directory. Idempotent, and safe
// to call concurrently with the ctx watchdog that also calls it.
//
// # The order is load-bearing, and so is what it cannot fix
//
// The socket goes BEFORE the shims — net.Listener.Close unlinks a unix socket
// itself, so the ordering below is the property rather than an accident to
// preserve. It matters because the two removals have opposite outcomes for a
// child that is mid-invocation: a shim whose socket has gone fails closed, it
// cannot ask and so does not run the program; a shim that has been DELETED does
// not fail at all, because the child's PATH falls through to the next entry and
// finds the real /usr/bin/sudo with no interception and no trace.
//
// The residue is real and is bounded rather than eliminated: Close runs when the
// launched process is REAPED, so anything still able to reach the shim
// afterwards is an orphan the launch left behind. Such a process gets the
// pre-interception behaviour, which is what every child had before this package
// existed — a smaller exposure than the one this control removes, not a new
// one. Keeping the directory alive instead would mean leaking a directory of
// symlinks named `sudo` per launch for the life of the server, and no way to
// decide when it stopped being needed.
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		err = s.listener.Close()
		if rmErr := os.RemoveAll(s.shimDir); err == nil {
			err = rmErr
		}
	})
	return err
}

// serve accepts until the listener closes.
func (s *Server) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

// handle answers one shim.
//
// One request per connection, and the connection is closed afterwards: a shim
// asks exactly once and then either execs or exits, so a connection that stayed
// open would only be a handle for something else to reuse.
func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		writeResponse(conn, Response{Reason: "malformed request"})
		return
	}
	// Constant-time, because the alternative leaks the token one byte at a time
	// to anything that can dial the socket and measure.
	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.token)) != 1 {
		writeResponse(conn, Response{Reason: "bad broker token"})
		return
	}
	req.Token = ""
	if err := s.decide(s.ctx, req); err != nil {
		writeResponse(conn, Response{Reason: err.Error()})
		return
	}
	writeResponse(conn, Response{Allow: true})
}

// writeResponse sends one newline-terminated JSON verdict.
func writeResponse(conn net.Conn, resp Response) {
	raw, err := json.Marshal(resp)
	if err != nil {
		return
	}
	_, _ = conn.Write(append(raw, '\n'))
}

// CommandLine renders an intercepted argv as a POSIX shell command string.
//
// The round trip exists because guard judges a COMMAND STRING: Action.Shell is
// what the segmenter, the destructive classifier and the execpolicy rules all
// read. The shim has exact argv, which is strictly more information, and
// throwing it away to re-parse a string looks like a step backwards — but the
// alternative is a second decision path with its own idea of what `sudo rm -rf
// /` is, and two classifiers that disagree is the failure this project has
// already paid for elsewhere.
//
// Quoting is single-quote-everything rather than "quote only when necessary".
// The cheap version needs a table of which characters the guard's lexer treats
// as special, and that table is exactly what must not be duplicated here: a
// character this function believed safe and the lexer treated as a separator
// would split one argument into two, changing the command the operator is shown
// from the command that runs.
func CommandLine(program string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, shellQuote(program))
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// shellQuote wraps s in single quotes, closing and reopening around any single
// quote it contains — the standard POSIX idiom, because a single-quoted string
// has no escape character at all.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// PrependShimDir returns env with dir prepended to PATH, preserving every other
// entry.
//
// PATH is matched case-insensitively because Windows spells it Path — this
// package does not run there, but the same env slices are built by code that
// does, and a helper that quietly appended a second PATH entry would be a
// hazard waiting for the first cross-platform caller.
//
// A child with no PATH at all gets one containing only the shim dir. That is
// deliberate: it cannot resolve anything else either way, and inventing a
// default search path here would hand it programs its caller chose not to.
func PrependShimDir(env []string, dir string) []string {
	out := make([]string, 0, len(env)+1)
	found := false
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(name, "PATH") && !found {
			found = true
			out = append(out, name+"="+dir+string(os.PathListSeparator)+value)
			continue
		}
		out = append(out, entry)
	}
	if !found {
		out = append(out, "PATH="+dir)
	}
	return out
}
