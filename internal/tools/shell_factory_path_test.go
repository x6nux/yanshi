package tools

import (
	"context"
	"strings"
	"testing"

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
