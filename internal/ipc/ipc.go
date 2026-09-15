// Package ipc serves the JSON-RPC 2.0 app-server protocol over a unix domain
// socket, so a local tool can drive a RUNNING yanshi daemon without going
// through the loopback HTTP port.
//
// Why a socket exists at all, given HTTP already works: the HTTP port is a TCP
// listener with a bearer token, which means anything that can reach loopback
// can try to talk to it, and the client has to discover an address AND a
// credential. A unix socket is protected by the filesystem: it lives in the
// per-user cache directory, it is created 0600, and the kernel refuses a
// connect from another user before a single byte is parsed. It also cannot
// collide with a port, cannot be reached from off-machine, and needs no token
// on the wire — which is exactly the property that makes it safe to point a
// script at.
//
// The PROTOCOL is internal/appserver's, unchanged: one JSON-RPC request per
// line in, responses and item/updated notifications out. Reusing it rather
// than inventing an IPC vocabulary means `yanshi ipc` and `yanshi app` expose
// the same methods, and a client written against one works against the other —
// the transport differs, the surface does not.
package ipc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/x6nux/yanshi/internal/lockfile"
)

// ErrInUse reports that another daemon already answers on the socket. It is a
// distinct error because it is not a failure to START one — it is the answer
// "there is already an owner", which a caller reports rather than retries.
var ErrInUse = errors.New("ipc: socket already served by another process")

// maxSocketPath bounds the socket path we are willing to hand to the kernel.
// A unix socket address is a fixed-size field (104 bytes on darwin, 108 on
// linux) and an over-long path fails at bind time with a message that reads
// like a missing directory. The lockfile-derived name is already short, but a
// deeply nested project root can push it over, so SocketPath falls back to a
// hash-only name instead of failing.
const maxSocketPath = 100

// SocketPath returns the IPC socket path for a project root. It sits next to
// the project's lockfile, so "where is this daemon's socket" has one answer
// that can be found from the root the daemon was started in. The name is
// derived from the lockfile's own name so the two cannot drift apart.
func SocketPath(root string) (string, error) {
	lfPath, err := lockfile.Path(root)
	if err != nil {
		return "", fmt.Errorf("ipc: %w", err)
	}
	base := strings.TrimSuffix(filepath.Base(lfPath), ".lock")
	dir := filepath.Dir(lfPath)
	path := filepath.Join(dir, base+".sock")
	if len(path) <= maxSocketPath {
		return path, nil
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(dir, "s-"+hex.EncodeToString(sum[:6])+".sock"), nil
}

// Listen binds the project's socket, replacing a stale one.
//
// A socket FILE left behind by a crashed daemon is the normal case, not an
// error: the kernel does not remove it, and refusing to start because a dead
// process's inode is in the way would make a crash permanent. The decision is
// made by CONNECTING rather than by stat'ing, because a file is exactly what a
// live daemon leaves too — the only question that matters is whether anything
// answers on it.
func Listen(root string) (net.Listener, error) {
	path, err := SocketPath(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("ipc: socket dir: %w", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		conn, dialErr := net.DialTimeout("unix", path, 500*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%w: %s", ErrInUse, path)
		}
		// Nothing answered: the file is litter from a process that is gone.
		if rmErr := os.Remove(path); rmErr != nil {
			return nil, fmt.Errorf("ipc: remove stale socket %s: %w", path, rmErr)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("ipc: listen %s: %w", path, err)
	}
	// 0600 is the access control for everything below. On platforms whose
	// unix sockets have no permission bits (Windows AF_UNIX) this is a no-op,
	// and the protection there is the per-user directory the socket lives in —
	// said out loud in the -b help rather than implied.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("ipc: chmod %s: %w", path, err)
	}
	return ln, nil
}

// Remove deletes a socket file this process created. Errors are returned so a
// shutdown path can report a socket it could not clean up, but callers are
// expected to treat it as best-effort: a stale socket is recoverable (Listen
// replaces it), a failed shutdown is not.
func Remove(root string) error {
	path, err := SocketPath(root)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Handler serves one accepted connection. It returns when the client is done.
type Handler func(ctx context.Context, conn net.Conn)

// Serve accepts connections until ctx is cancelled or the listener is closed,
// running each on its own goroutine.
//
// One goroutine per connection rather than a serial loop: the protocol allows
// several long-lived clients (an editor and a script, say), and a turn stream
// holds its connection open for the whole turn. Reaping is what Serve waits
// for, so a cancellation does not return while a handler is mid-write.
func Serve(ctx context.Context, ln net.Listener, h Handler) error {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = ln.Close()
		case <-done:
		}
	}()
	defer close(done)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("ipc: accept: %w", err)
		}
		go func(c net.Conn) {
			defer c.Close()
			h(ctx, c)
		}(conn)
	}
}

// Dial connects to a project's socket and returns the connection. The caller
// owns the connection.
func Dial(ctx context.Context, root string) (net.Conn, error) {
	path, err := SocketPath(root)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("ipc: dial %s: %w", path, err)
	}
	return conn, nil
}
