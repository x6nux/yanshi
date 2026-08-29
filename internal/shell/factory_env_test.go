package shell

import (
	"context"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/secproc"
)

// TestDefaultSecureFactoryChildInheritsHostEnv asserts against the PRODUCTION
// factory that a child launched through it gets the host environment, not just
// the proxy variables.
//
// The bug this guards was invisible for a long time because the test double
// used elsewhere (git_test.go's realGitFactory) did
// `cmd.Env = append(os.Environ(), spec.Env...)` while the production path
// started from spec.Env alone -- which no secproc caller populates. The fake
// was MORE capable than the real thing, so every test passed while a real
// child got three proxy variables and nothing else: no PATH, no HOME, no
// GOMODCACHE. run_tests answered "pass" on a toolchain it could not even
// start, and gh wrote its state into the repository because HOME was unset.
//
// So this test must use DefaultSecureFactory itself. Anything that constructs
// its own exec.Cmd re-introduces exactly the divergence being tested for.
func TestDefaultSecureFactoryChildInheritsHostEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /usr/bin/env to dump the child environment")
	}
	const sentinelKey = "YANSHI_ENV_INHERIT_PROBE"
	t.Setenv(sentinelKey, "sentinel-value")

	f := DefaultSecureFactory{OS: OSProcessFactory{}, ProxyURL: "http://127.0.0.1:9999"}
	proc, err := f.Start(context.Background(), secproc.SecureProcessSpec{
		Program: "/usr/bin/env",
		Dir:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	out, _ := io.ReadAll(proc.Stdout)
	_ = proc.Wait()
	env := string(out)

	// PATH is the one whose absence breaks every tool that shells out.
	if !strings.Contains(env, "PATH=") {
		t.Errorf("child has no PATH; env was:\n%s", env)
	}
	// A variable set only in this process proves inheritance rather than some
	// hardcoded minimal set that happens to include PATH.
	if !strings.Contains(env, sentinelKey+"=sentinel-value") {
		t.Errorf("child did not inherit %s; env was:\n%s", sentinelKey, env)
	}
	if os.Getenv("HOME") != "" && !strings.Contains(env, "HOME=") {
		t.Errorf("child has no HOME (gh writes its state into CWD without it); env was:\n%s", env)
	}
	// Inheriting must not have cost us the proxy injection: both halves have
	// to hold at once, which is the whole reason PrepareEnvFor exists rather
	// than a plain append.
	if !strings.Contains(env, "HTTP_PROXY=http://127.0.0.1:9999") {
		t.Errorf("proxy variable lost while inheriting host env; env was:\n%s", env)
	}
}
