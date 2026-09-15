package ipc

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/lockfile"
)

// TestSocketPathSitsNextToTheLockfile pins the discoverability property: one
// answer to "where is this project's daemon socket", findable from the root.
func TestSocketPathSitsNextToTheLockfile(t *testing.T) {
	root := t.TempDir()
	sock, err := SocketPath(root)
	if err != nil {
		t.Fatalf("SocketPath: %v", err)
	}
	lf, err := lockfile.Path(root)
	if err != nil {
		t.Fatalf("lockfile.Path: %v", err)
	}
	if filepath.Dir(sock) != filepath.Dir(lf) {
		t.Fatalf("socket %s is not beside lockfile %s", sock, lf)
	}
	if !strings.HasSuffix(sock, ".sock") {
		t.Fatalf("socket %s does not end in .sock", sock)
	}
	if len(sock) > maxSocketPath {
		t.Fatalf("socket path is %d bytes, over the %d-byte bound", len(sock), maxSocketPath)
	}
}

// TestSocketPathFallsBackForDeepRoots proves the sun_path bound is handled
// rather than left to the kernel: bind is where an over-long path fails, and
// its error reads like a missing directory.
func TestSocketPathFallsBackForDeepRoots(t *testing.T) {
	deep := "/tmp/" + strings.Repeat("a-very-long-project-directory-name/", 6)
	sock, err := SocketPath(deep)
	if err != nil {
		t.Fatalf("SocketPath: %v", err)
	}
	if len(sock) > maxSocketPath {
		t.Fatalf("socket path is %d bytes, over the %d-byte bound: %s", len(sock), maxSocketPath, sock)
	}
	if !strings.Contains(filepath.Base(sock), "s-") {
		t.Fatalf("expected the hashed fallback name, got %s", sock)
	}
}

// TestListenServeDialRoundTrip is the whole transport in one test.
func TestListenServeDialRoundTrip(t *testing.T) {
	root := t.TempDir()
	ln, err := Listen(root)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = Serve(ctx, ln, func(_ context.Context, c net.Conn) {
			sc := bufio.NewScanner(c)
			for sc.Scan() {
				_, _ = c.Write(append([]byte("echo:"), sc.Bytes()...))
				_, _ = c.Write([]byte("\n"))
			}
		})
	}()

	conn, err := Dial(ctx, root)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.TrimSpace(line) != "echo:hello" {
		t.Fatalf("got %q", line)
	}
}

// TestListenRefusesALiveSocket proves two daemons cannot share one socket: the
// second start gets ErrInUse rather than a bind error that reads like a
// filesystem problem, or worse, a silent second listener.
func TestListenRefusesALiveSocket(t *testing.T) {
	root := t.TempDir()
	ln, err := Listen(root)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, aerr := ln.Accept()
		if aerr == nil {
			_ = c.Close()
		}
	}()

	if _, err := Listen(root); err == nil {
		t.Fatal("second Listen succeeded while the first was live")
	} else if !errors.Is(err, ErrInUse) {
		t.Fatalf("err = %v, want ErrInUse", err)
	}
	<-done
}

// TestListenReplacesAStaleSocket proves a crash is recoverable: the kernel
// leaves the socket file behind, and refusing to start because a dead process's
// inode is in the way would make that crash permanent.
func TestListenReplacesAStaleSocket(t *testing.T) {
	root := t.TempDir()
	path, err := SocketPath(root)
	if err != nil {
		t.Fatalf("SocketPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A plain file where the socket should be: nothing answers on it.
	if err := os.WriteFile(path, []byte("litter"), 0o600); err != nil {
		t.Fatalf("write litter: %v", err)
	}
	ln, err := Listen(root)
	if err != nil {
		t.Fatalf("Listen over litter: %v", err)
	}
	defer ln.Close()
	if fi, err := os.Stat(path); err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("expected a socket at %s, got mode %v err %v", path, fi.Mode(), err)
	}
}

// TestRemoveIsIdempotent proves shutdown can call it unconditionally.
func TestRemoveIsIdempotent(t *testing.T) {
	root := t.TempDir()
	if err := Remove(root); err != nil {
		t.Fatalf("Remove on a missing socket: %v", err)
	}
	ln, err := Listen(root)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	_ = ln.Close()
	if err := Remove(root); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := Remove(root); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
}
