package shell

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// stderrFloodBytes is comfortably larger than any platform's pipe buffer
// (64 KiB on Linux, 8-64 KiB on macOS, ~4 KiB for Windows anonymous pipes), so
// a child that writes it before touching stdout is guaranteed to BLOCK on the
// stderr write until someone drains that pipe.
const stderrFloodBytes = 512 << 10

// stdoutMarker is written after the flood, so it can only appear if the reader
// drained stderr concurrently.
const stdoutMarker = "stdout-marker\n"

// TestStreamSplitHelper is both a no-op test and the subprocess helper for the
// two tests below: it floods stderr FIRST and only then writes stdout, which
// is the ordering that turns a concatenating reader into a deadlock. os.Exit
// keeps the testing framework from appending its own "PASS" to stdout.
func TestStreamSplitHelper(t *testing.T) {
	if os.Getenv("YANSHI_STREAM_SPLIT_HELPER") != "1" {
		return
	}
	_, _ = os.Stderr.Write(bytes.Repeat([]byte("E"), stderrFloodBytes))
	_, _ = os.Stdout.WriteString(stdoutMarker)
	os.Exit(0)
}

// streamSplitSpec re-execs the test binary into TestStreamSplitHelper.
func streamSplitSpec(separate bool) LaunchSpec {
	return LaunchSpec{
		Program:        os.Args[0],
		Args:           []string{"-test.run=TestStreamSplitHelper"},
		Env:            append(os.Environ(), "YANSHI_STREAM_SPLIT_HELPER=1"),
		SeparateStderr: separate,
	}
}

// readAllBounded reads r to EOF, failing the test instead of hanging forever
// when the read cannot complete. The bound is what lets these tests assert the
// ABSENCE of a deadlock: a regression fails in seconds with a useful message
// rather than being reported as a package-wide 10-minute timeout.
func readAllBounded(t *testing.T, what string, r io.Reader) []byte {
	t.Helper()
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(r)
		done <- result{b, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("read %s: %v", what, res.err)
		}
		return res.data
	case <-time.After(30 * time.Second):
		t.Fatalf("reading %s did not finish: the child is blocked writing stderr "+
			"while the reader waits on stdout — the streams are being concatenated, not merged", what)
		return nil
	}
}

// TestOSProcessFactoryMergesStreamsConcurrently pins the display contract AND
// the deadlock fix in one shot.
//
// The merged console used to be io.MultiReader(stdout, stderr), which does not
// read stderr until stdout reports EOF. A child that fills its stderr buffer
// before closing stdout therefore blocks on write while the reader blocks on
// read, and the pair is only broken up when the caller's timeout kills the
// process — shell_run reporting a 30s timeout for a command that finished
// instantly.
func TestOSProcessFactoryMergesStreamsConcurrently(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // kills the child if a regression leaves it wedged
	proc, console, err := OSProcessFactory{}.Start(ctx, streamSplitSpec(false))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer console.Close()

	got := readAllBounded(t, "merged console", console)
	if !strings.Contains(string(got), strings.TrimSpace(stdoutMarker)) {
		t.Fatalf("merged stream is missing the stdout marker (len=%d)", len(got))
	}
	if n := bytes.Count(got, []byte("E")); n != stderrFloodBytes {
		t.Fatalf("merged stream carried %d stderr bytes, want %d", n, stderrFloodBytes)
	}
	if err := proc.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// TestOSProcessFactorySeparatesStderrOnRequest pins the parsing contract:
// with LaunchSpec.SeparateStderr the console carries stdout and NOTHING else,
// so a porcelain/JSON parser reading it can never see a diagnostic line.
func TestOSProcessFactorySeparatesStderrOnRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proc, console, err := OSProcessFactory{}.Start(ctx, streamSplitSpec(true))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer console.Close()

	sc, ok := console.(StderrConsole)
	if !ok {
		t.Fatal("SeparateStderr console must satisfy StderrConsole, else DefaultSecureFactory silently falls back to an empty stderr")
	}
	errStream := sc.Stderr()
	if errStream == nil {
		t.Fatal("Stderr() returned nil for a SeparateStderr spawn")
	}

	// Both halves must be drained concurrently: the child blocks on the stderr
	// flood, so reading stdout first would hang no matter how the factory is
	// implemented.
	var wg sync.WaitGroup
	var outBytes, errBytes []byte
	wg.Add(2)
	go func() { defer wg.Done(); outBytes = readAllBounded(t, "stdout", console) }()
	go func() { defer wg.Done(); errBytes = readAllBounded(t, "stderr", errStream) }()
	wg.Wait()

	if strings.TrimSpace(string(outBytes)) != strings.TrimSpace(stdoutMarker) {
		t.Fatalf("stdout = %q, want only the marker — any extra byte here is stderr text a parser would read as data", outBytes)
	}
	if n := bytes.Count(errBytes, []byte("E")); n != stderrFloodBytes {
		t.Fatalf("stderr carried %d flood bytes, want %d", n, stderrFloodBytes)
	}
	if err := proc.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}
