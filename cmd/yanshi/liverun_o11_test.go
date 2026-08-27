package main

// liverun_o11_test.go — the stdio ACP server, exercised the way a host like Zed
// exercises it: by spawning the real binary and speaking the protocol over its
// standard streams.
//
// The in-process tests for internal/acpserver drive Serve with an io.Pipe,
// which cannot see the failure that actually breaks a host integration. A host
// parses stdout line by line as JSON-RPC and dies on the first line that is not
// a frame, and the things that write to stdout are not the protocol code: they
// are a stray fmt.Println in some package the composition root pulls in, a
// library banner, a soft-degradation notice that picked the wrong stream. Only
// the assembled binary can be wrong in that way, so only the assembled binary
// can be checked for it.
//
// The binary is built by the test, so there is no external dependency and no
// assumption that someone ran `go build` first.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// builtBinary builds cmd/yanshi once per test binary run and returns its path.
var builtBinary struct {
	once sync.Once
	path string
	err  error
}

// yanshiBinary compiles the CLI into a temp location and returns its path.
//
// Built once per package test run: `go build` of this module takes seconds, and
// every test here wants the same artefact.
func yanshiBinary(t *testing.T) string {
	t.Helper()
	builtBinary.once.Do(func() {
		dir, err := os.MkdirTemp("", "yanshi-bin")
		if err != nil {
			builtBinary.err = err
			return
		}
		name := "yanshi"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		out := filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-o", out, "github.com/x6nux/yanshi/cmd/yanshi")
		if combined, berr := cmd.CombinedOutput(); berr != nil {
			builtBinary.err = errWithOutput(berr, combined)
			return
		}
		builtBinary.path = out
	})
	if builtBinary.err != nil {
		t.Fatalf("build yanshi: %v", builtBinary.err)
	}
	return builtBinary.path
}

// errWithOutput attaches a command's output to its error.
func errWithOutput(err error, out []byte) error {
	return &buildError{err: err, out: string(out)}
}

// buildError carries a failed build's output so the failure is diagnosable.
type buildError struct {
	err error
	out string
}

func (e *buildError) Error() string { return e.err.Error() + ": " + e.out }

// acpProject creates a scratch project with a config the binary can boot from.
func acpProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	db := filepath.ToSlash(filepath.Join(root, "yanshi.db"))
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.yaml"),
		[]byte("storage:\n  sqlite_path: \""+db+"\"\n"), 0o644))
	return root
}

// runACP spawns `yanshi acp`, writes the given lines to its stdin, and returns
// stdout and stderr once it exits.
func runACP(t *testing.T, lines []string) (stdout, stderr string) {
	t.Helper()
	root := acpProject(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, yanshiBinary(t), "acp", "-fake-model",
		"-config", filepath.Join(root, "config.yaml"))
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("yanshi acp exited with %v\nstdout:\n%s\nstderr:\n%s", err, outBuf.String(), errBuf.String())
	}
	return outBuf.String(), errBuf.String()
}

// TestLiveRun_O11StdioACPSpeaksProtocolOnStdoutAndDiagnosticsOnStderr runs a
// complete host conversation — initialize, session/new, session/prompt —
// against the real binary.
//
// Two properties are asserted, and the second is the one an in-process test
// cannot reach: every response is a well-formed frame, AND stdout contains
// nothing else. A single diagnostic line on the wrong stream ends the host's
// session with a parse error, which is why the boot-time notices this binary
// emits (LSP disabled, managed proxy, C1 degradation) have to be checked for
// rather than assumed to be on stderr.
func TestLiveRun_O11StdioACPSpeaksProtocolOnStdoutAndDiagnosticsOnStderr(t *testing.T) {
	stdout, stderr := runACP(t, []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"."}}`,
		`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"yanshi-1",` +
			`"prompt":[{"type":"text","text":"hello from a host"}]}}`,
	})

	t.Logf("stdout:\n%s", stdout)
	t.Logf("stderr (%d bytes, first 400): %.400s", len(stderr), stderr)

	// EVERY stdout line must parse as a JSON-RPC 2.0 message. This is the
	// assertion that fails when anything at all prints to stdout.
	type frame struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      *int64          `json:"id"`
		Method  string          `json:"method"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	var frames []frame
	sc := bufio.NewScanner(strings.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var f frame
		if err := json.Unmarshal([]byte(text), &f); err != nil {
			t.Fatalf("stdout line %d is not JSON, so a host's parser dies here: %v\n  %.200s",
				line, err, text)
		}
		if f.JSONRPC != "2.0" {
			t.Errorf("stdout line %d is JSON but not a JSON-RPC frame (jsonrpc=%q): %.200s",
				line, f.JSONRPC, text)
		}
		frames = append(frames, f)
	}
	require.NoError(t, sc.Err())
	require.NotEmpty(t, frames, "the server answered nothing at all")

	// The three requests must each get a reply, and none may be an error.
	replies := map[int64]frame{}
	notifications := 0
	for _, f := range frames {
		if f.ID != nil {
			replies[*f.ID] = f
			continue
		}
		notifications++
		if f.Method == "" {
			t.Errorf("a frame with no id and no method is neither a response nor a notification")
		}
	}
	for _, id := range []int64{1, 2, 3} {
		f, ok := replies[id]
		if !ok {
			t.Errorf("request id=%d got no reply", id)
			continue
		}
		if len(f.Error) > 0 {
			t.Errorf("request id=%d failed: %s", id, f.Error)
		}
		if len(f.Result) == 0 {
			t.Errorf("request id=%d replied with neither result nor error", id)
		}
	}
	t.Logf("%d replies, %d notifications", len(replies), notifications)

	// A prompt must actually stream something back, or the host shows an empty
	// turn: session/update notifications are the only channel for that.
	if notifications == 0 {
		t.Errorf("session/prompt produced no session/update notification; " +
			"the host would render an empty answer")
	}

	// The boot diagnostics have to exist SOMEWHERE — if they are missing
	// entirely, the check above passes for the wrong reason (nothing was
	// logged at all) rather than because the streams are separated correctly.
	if strings.TrimSpace(stderr) == "" {
		t.Errorf("stderr is empty: this binary emits boot notices, so their absence " +
			"means the assertion that stdout is clean proves nothing")
	}
}

// TestLiveRun_O11MalformedInputDoesNotCorruptTheStream feeds the server garbage
// between two valid requests.
//
// A host that mis-frames one message must not lose the session: the reply to
// the following request has to arrive, and every stdout line has to stay
// parseable. Echoing an unparseable line back, or dying, both leave the host
// with a broken pipe and no explanation.
func TestLiveRun_O11MalformedInputDoesNotCorruptTheStream(t *testing.T) {
	stdout, _ := runACP(t, []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`,
		`this is not json at all`,
		`{"jsonrpc":"2.0","id":2,"method":"no/such/method","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"session/new","params":{"cwd":"."}}`,
	})
	t.Logf("stdout after malformed input:\n%s", stdout)

	var ids []int64
	sc := bufio.NewScanner(strings.NewReader(stdout))
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var f struct {
			JSONRPC string `json:"jsonrpc"`
			ID      *int64 `json:"id"`
		}
		if err := json.Unmarshal([]byte(text), &f); err != nil {
			t.Fatalf("stdout line %d stopped being valid JSON after malformed input: %v\n  %.200s",
				line, err, text)
		}
		if f.JSONRPC != "2.0" {
			t.Errorf("stdout line %d is not a JSON-RPC frame: %.200s", line, text)
		}
		if f.ID != nil {
			ids = append(ids, *f.ID)
		}
	}
	t.Logf("replies seen for ids %v", ids)

	// The request AFTER the garbage must still be answered.
	answered := false
	for _, id := range ids {
		if id == 3 {
			answered = true
		}
	}
	if !answered {
		t.Errorf("the request following a malformed line was never answered (ids seen: %v); "+
			"one bad frame ended the session", ids)
	}
}
