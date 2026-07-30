package shell

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
)

// recordingFactory captures the env slice that DefaultSecureFactory hands off
// to the OS layer, so we can assert PrepareEnv stripped the inherited vars.
type recordingFactory struct {
	gotEnv []string
}

func (r *recordingFactory) Start(_ context.Context, spec LaunchSpec) (Process, Console, error) {
	r.gotEnv = append([]string(nil), spec.Env...)
	// Return a no-op Process + Console so DefaultSecureFactory can complete.
	return &noopProcess{}, &noopConsole{}, nil
}

type noopProcess struct{}

func (noopProcess) Wait() error                                        { return nil }
func (noopProcess) PID() int                                           { return 1 }
func (noopProcess) Kill() error                                        { return nil }
func (noopProcess) Capabilities() ProcessCapabilities                 { return ProcessCapabilities{CanKillTree: false} }

type noopConsole struct{}

func (noopConsole) Read([]byte) (int, error)           { return 0, errConsoleClosed }
func (noopConsole) Write(p []byte) (int, error)        { return len(p), nil }
func (noopConsole) Close() error                       { return nil }
func (noopConsole) Resize(uint16, uint16) error        { return nil }
func (noopConsole) PTY() bool                          { return false }

var errConsoleClosed = errors.New("console closed")

func TestConsoleReaderPreservesNonEOFError(t *testing.T) {
	_, err := (consoleReader{r: noopConsole{}}).Read(make([]byte, 8))
	if !errors.Is(err, errConsoleClosed) {
		t.Fatalf("non-EOF console error must survive, got %v", err)
	}
}

func TestDefaultSecureFactoryStripsInheritedProxyEnv(t *testing.T) {
	rec := &recordingFactory{}
	f := DefaultSecureFactory{
		OS:       rec,
		Policy:   &netpolicy.Policy{Default: "allow"},
		ProxyURL: "http://127.0.0.1:9090",
		Sandbox:  sandbox.New(sandbox.Config{Enabled: true, Tier: sandbox.ReadOnly}),
	}
	spec := secproc.SecureProcessSpec{
		Tool:    "shell_run",
		Shell:   "go version",
		Program: "go",
		Args:    []string{"version"},
		Env:     []string{"PATH=/usr/bin", "http_proxy=leak", "HTTPS_PROXY=leak"},
	}
	if _, err := f.Start(context.Background(), spec); err != nil {
		t.Fatalf("start: %v", err)
	}
	joined := strings.Join(rec.gotEnv, "\n")
	if strings.Contains(strings.ToLower(joined), "http_proxy=leak") {
		t.Fatalf("inherited proxy var survived: %v", rec.gotEnv)
	}
	if !strings.Contains(joined, "HTTP_PROXY=http://127.0.0.1:9090") {
		t.Fatalf("managed proxy var missing: %v", rec.gotEnv)
	}
}

func TestDefaultSecureFactoryFailsClosedWhenNoOSFactory(t *testing.T) {
	f := DefaultSecureFactory{}
	if _, err := f.Start(context.Background(), secproc.SecureProcessSpec{Program: "go"}); err == nil {
		t.Fatal("missing OS factory must fail closed")
	}
}
