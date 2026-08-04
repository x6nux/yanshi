package tools

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/secproc"
)

// TestShellRunFactoryPathEmitsExitFooter is the regression test for shell_run's
// SecureProcessFactory branch returning as soon as stdout hit EOF.
//
// Two things were missing and this asserts both at once, because the same
// missing call produces both: started.Wait was never invoked, so (a) every
// shell_run left an unreaped child behind and (b) the "── exit N · Xs ──"
// footer the legacy pipe path always emits was absent. Without that footer the
// model cannot tell a failed command from a successful one — stdout and stderr
// arrive merged into one untagged line stream, so "command not found" reads
// exactly like ordinary program output.
//
// The exit code is the proof that Wait ran: 7 is only observable by reaping
// the process, and scriptedFactory hands back the real (*exec.Cmd).Wait.
func TestShellRunFactoryPathEmitsExitFooter(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		code       int
	}{
		{"clean exit", "── exit 0 ·", 0},
		{"non-zero exit", "── exit 7 ·", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			factory := newScriptedFactory(t, func(secproc.SecureProcessSpec) cannedResult {
				return cannedResult{Stdout: "child-stdout-marker\n", ExitCode: tc.code}
			})
			sh := NewShellTools(t.TempDir())
			ctx := WithSecureProcessFactory(WithProfile(context.Background(), allowAllProfile()), factory)
			out, err := runTool(ctx, sh.Run, `{"command":"echo hi"}`)
			if err != nil {
				t.Fatalf("shell_run: %v", err)
			}
			if !strings.Contains(out, "child-stdout-marker") {
				t.Fatalf("child stdout missing from result: %q", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("result %q is missing the %q footer — the model cannot "+
					"distinguish success from failure without it", out, tc.want)
			}
		})
	}
}

// TestShellRunFactoryPathFailsClosedWithoutReaper proves the inlined factory
// branch honors the same no-reaper contract RunSecureCapture does. The branch
// used to skip that check entirely, so a Factory with a nil Wait would have
// been silently accepted here while every other secproc caller rejected it.
func TestShellRunFactoryPathFailsClosedWithoutReaper(t *testing.T) {
	sh := NewShellTools(t.TempDir())
	ctx := WithSecureProcessFactory(WithProfile(context.Background(), allowAllProfile()), reaperlessFactory{})
	out, err := runTool(ctx, sh.Run, `{"command":"echo hi"}`)
	if err != nil {
		t.Fatalf("shell_run: %v", err)
	}
	if !strings.Contains(out, "reaper") {
		t.Fatalf("result = %q, want a fail-closed reaper error", out)
	}
}

// parkedFactory hands back a process whose output stream never reaches EOF, so
// the only way out of shell_run's streaming loop is context cancellation. Wait
// records that it was called and returns immediately — the assertion is about
// whether the caller reaps at all, not about how long reaping takes.
type parkedFactory struct {
	waited  atomic.Bool
	release chan struct{}
}

func (f *parkedFactory) Start(context.Context, secproc.SecureProcessSpec) (*secproc.StartedProcess, error) {
	pr, pw := io.Pipe()
	go func() {
		_, _ = io.WriteString(pw, "child-stdout-marker\n")
		<-f.release
		_ = pw.Close()
	}()
	return &secproc.StartedProcess{
		PID:    4242,
		Stdout: pr,
		Wait: func() error {
			f.waited.Store(true)
			return nil
		},
	}, nil
}

// TestShellRunFactoryPathReapsOnCancel is the regression test for the CANCEL
// half of shell_run's factory branch. TestShellRunFactoryPathEmitsExitFooter
// covers the success half, and the comment next to that branch's nil-Wait
// check reads as if the whole "never call Wait at all, leaking one unreaped
// child per shell_run" bug were fixed. It was not: the streaming-error return
// — which is how every cancelled or timed-out shell_run leaves — still
// bypassed Wait entirely.
//
// A cancelled turn is not a rare path; it is what the TUI's Esc key and every
// per-call timeout do. Each one leaked an unreaped child plus the goroutines
// pumping its pipes, because (*exec.Cmd).Wait is also what closes those pipes.
func TestShellRunFactoryPathReapsOnCancel(t *testing.T) {
	factory := &parkedFactory{release: make(chan struct{})}
	defer close(factory.release)
	sh := NewShellTools(t.TempDir())
	base := WithSecureProcessFactory(WithProfile(context.Background(), allowAllProfile()), factory)
	ctx, cancel := context.WithTimeout(base, 300*time.Millisecond)
	defer cancel()

	if _, err := runTool(ctx, sh.Run, `{"command":"echo hi"}`); err != nil {
		t.Logf("shell_run returned err (expected on cancel): %v", err)
	}
	if !factory.waited.Load() {
		t.Fatal("shell_run returned on cancellation without calling started.Wait — " +
			"the child is never reaped and its pipe pumps leak")
	}
}
