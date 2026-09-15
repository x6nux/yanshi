//go:build e2e_live

// Live end-to-end tests for the yanshi CLI against a REAL provider.
//
// Why a build tag: these tests need (a) a config file whose llm.providers point
// at a reachable endpoint, (b) network access, and (c) API budget. None of that
// belongs in `go test ./...`, which must stay hermetic. They are the harness for
// "every feature is drivable from the CLI" — the unit tests next to them assert
// the wire shapes, and these assert that a real model actually drives a real
// turn through the same dispatch() the binary uses.
//
// Run with:
//
//	YANSHI_E2E_CONFIG=/tmp/yanshi-e2e/config.yaml \
//	  go test -tags e2e_live ./cmd/yanshi -run TestLiveCLI -v -timeout 15m
//
// The config must set every provider's api_key (a ${VAR} reference is expanded
// by internal/config, so exporting the variable is enough), and its
// profiles.orchestrator must permit the tools each case exercises — a headless
// run has no client to answer a permission prompt, so a denied tool shows up as
// the model being told "permission denied" rather than as a test failure.
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// liveConfig returns the operator-supplied config path, skipping when unset.
func liveConfig(t *testing.T) string {
	t.Helper()
	p := os.Getenv("YANSHI_E2E_CONFIG")
	if p == "" {
		t.Skip("YANSHI_E2E_CONFIG not set: live CLI tests need a config pointing at a real provider")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("YANSHI_E2E_CONFIG=%s: %v", p, err)
	}
	return p
}

// runCLI drives one command through the same router the binary uses.
func runLiveCLI(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := dispatch(append([]string{"yanshi"}, args...), strings.NewReader(stdin), &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// TestLiveCLI_ExecTextReachesTheModel is the smallest possible proof that the
// headless CLI can complete a real turn: one prompt, one answer, exit 0.
func TestLiveCLI_ExecTextReachesTheModel(t *testing.T) {
	cfg := liveConfig(t)
	code, out, errs := runLiveCLI(t, "",
		"exec", "-config", cfg, "-inprocess",
		"-p", "Reply with exactly this token and nothing else: PONG",
		"-timeout", "180s")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s stdout=%s", code, errs, out)
	}
	if !strings.Contains(out, "PONG") {
		t.Fatalf("model answer missing PONG: stdout=%q stderr=%q", out, errs)
	}
}

// TestLiveCLI_ExecJSONLToolLoop drives a file write + read through the JSONL
// stream and asserts both the events and the side effect. A stream that reports
// a tool call the tool never performed is the failure this case exists to catch.
func TestLiveCLI_ExecJSONLToolLoop(t *testing.T) {
	cfg := liveConfig(t)
	dir := t.TempDir()
	var target string
	// A headless run's tools are jailed to the server's work root, and the work
	// root is the process cwd for an in-process run. Writing to an absolute path
	// outside it is a structural refusal no mode can approve, so the case writes
	// a RELATIVE path inside a temp cwd — which is also what an operator's own
	// script would do.
	withWorkdir(t, dir)
	target = "probe.txt"
	prompt := "Create the file probe.txt in the current directory containing exactly the single line e2e-ok, " +
		"then read it back and confirm the contents."

	code, out, errs := runLiveCLI(t, "",
		"exec", "-config", cfg, "-inprocess", "-mode", "yolo",
		"-p", prompt, "-output", "jsonl", "-timeout", "300s")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s stdout=%s", code, errs, out)
	}

	kinds := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("stream line is not JSON: %v (%q)", err, line)
		}
		kinds[ev.Type]++
	}
	for _, want := range []string{"tool_call", "tool_result", "agent_chunk", "done"} {
		if kinds[want] == 0 {
			t.Errorf("jsonl stream has no %q event: %v\nstream:\n%s", want, kinds, out)
		}
	}

	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the model reported a write the filesystem never saw: %v\nstream:\n%s", err, out)
	}
	if !strings.Contains(string(body), "e2e-ok") {
		t.Fatalf("file content = %q, want it to contain e2e-ok", string(body))
	}
}

// withWorkdir runs fn with the process cwd moved to dir, restoring it after.
//
// The in-process backend derives its work root from the process cwd, so this is
// the only way to point a live turn at a scratch directory without adding a flag
// for it. It is safe here because the live suite is build-tagged and runs by
// itself; nothing in the default `go test ./...` run calls it.
func withWorkdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

// TestLiveCLI_UsageExitCodes pins the stable exit codes the help text promises,
// because scripts depend on them and nothing else checks the live binary's.
func TestLiveCLI_UsageExitCodes(t *testing.T) {
	cfg := liveConfig(t)
	if code, _, _ := runLiveCLI(t, "", "exec", "-config", cfg, "-no-such-flag"); code != exitUsage {
		t.Errorf("unknown flag exit=%d, want exitUsage=%d", code, exitUsage)
	}
}
