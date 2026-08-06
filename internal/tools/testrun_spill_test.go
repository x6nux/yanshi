package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/secproc"
)

// goModRoot creates a work root that detectRunner("auto") resolves to "go".
func goModRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestRunTestsSpillsLargeOutputToAnArtifact is the fourth acceptance clause.
//
// A `go test ./...` on a real repository prints far more than a model can hold,
// and inlining it means one tool result consumes the whole context window
// before the model has read a single failure. Above SpillThreshold the raw
// output has to become a reference, with the summary truncated to something
// readable.
//
// The threshold is exercised from BOTH sides: a small run must NOT produce an
// artifact, or "spills when large" would be satisfied by a tool that spills
// everything.
//
// ledger: B3/DT4#4 大输出成 artifact
func TestRunTestsSpillsLargeOutputToAnArtifact(t *testing.T) {
	big := strings.Repeat(`{"Action":"pass","Package":"p","Test":"T"}`+"\n", 4000)
	if len(big) <= SpillThreshold {
		t.Fatalf("fixture is %d bytes, below the %d-byte threshold; this test cannot "+
			"observe spilling", len(big), SpillThreshold)
	}
	factory := newScriptedFactory(t, func(secproc.SecureProcessSpec) cannedResult {
		return cannedResult{Stdout: big}
	})
	ctx := WithWorkRoot(secureTestContext(t, factory), goModRoot(t))

	out, err := runTool(ctx, NewTestRunTool(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var res testResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Summary) > 4096+512 {
		t.Errorf("summary is %d bytes; a large run must be truncated, not inlined",
			len(res.Summary))
	}
	if res.ArtifactRef == "" && !res.Degraded {
		t.Error("a run above SpillThreshold produced neither an artifact reference " +
			"nor a degraded marker: the raw output went into the model's context")
	}

	// Negative control: a small run stays inline.
	small := newScriptedFactory(t, func(secproc.SecureProcessSpec) cannedResult {
		return cannedResult{Stdout: `{"Action":"pass","Package":"p","Test":"T"}` + "\n"}
	})
	sctx := WithWorkRoot(secureTestContext(t, small), goModRoot(t))
	sout, err := runTool(sctx, NewTestRunTool(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var sres testResult
	if err := json.Unmarshal([]byte(sout), &sres); err != nil {
		t.Fatal(err)
	}
	if sres.ArtifactRef != "" || sres.Degraded {
		t.Error("a small run was spilled to an artifact; the threshold is not honoured")
	}
}

// TestRunTestsTimeoutIsReportedNotSwallowed is the third acceptance clause.
//
// A hung runner must end the tool call, and it must end it VISIBLY: reporting
// a timeout as "pass" is the same failure mode as reporting a runner crash as
// pass — the model proceeds believing the suite is green.
//
// timeout_s is clamped to [1, 1800]; 0 means the 10-minute default. That
// default matters: an earlier version clamped 0 → 1, which made every call a
// one-second timeout — fine for a scripted factory, a guaranteed deadline
// under -race where the helper takes longer than that just to start.
//
// ledger: B3/DT4#3 超时/取消干净
func TestRunTestsTimeoutIsReportedNotSwallowed(t *testing.T) {
	factory := newScriptedFactory(t, func(secproc.SecureProcessSpec) cannedResult {
		// Block hangs the helper forever; the timeout is what has to end it.
		return cannedResult{Block: true}
	})
	ctx := WithWorkRoot(secureTestContext(t, factory), goModRoot(t))

	out, err := runTool(ctx, NewTestRunTool(), `{"timeout_s":1}`)
	if err != nil {
		t.Fatal(err)
	}
	var res testResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("timeout produced unparseable output %q: %v", out, err)
	}
	if res.Status == "pass" {
		t.Errorf("a run that timed out reported status=pass: %s", out)
	}

	// Cancellation is the caller-side twin and must not hang the tool.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		cout, _ := runTool(cctx, NewTestRunTool(), `{}`)
		// The tool packages failures as result TEXT, so "did not report
		// success" is the checkable claim — the exact wording is the tool's
		// business, and asserting it would pin prose instead of behaviour.
		if strings.Contains(cout, `"status":"pass"`) {
			t.Errorf("a cancelled run reported success: %s", cout)
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("a cancelled run_tests did not return within 30s")
	}
}
