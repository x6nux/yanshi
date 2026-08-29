package harden

import (
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// helperEnv routes the test binary into the subprocess half of these tests.
//
// The measures Apply installs are IRREVERSIBLE for the process that installs
// them: PT_DENY_ATTACH cannot be undone, and an RLIMIT_CORE hard limit cannot
// be raised again. Running Apply in the test runner would therefore harden
// `go test` itself and, on darwin, would kill the run the moment anyone
// attached a debugger to it. Every assertion that needs the real syscalls runs
// in a child that is allowed to be a one-way street.
const helperEnv = "YANSHI_HARDEN_TEST_HELPER"

// TestHardenHelper is the subprocess half: it applies the real hardening and
// prints the report plus the post-hardening state of the loader variables.
//
// It is a Test function rather than a separate main package because that is
// the only way to reach unexported package state from a spawned process
// without shipping a binary nobody else needs.
func TestHardenHelper(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("subprocess helper; driven by the tests below")
	}
	out := struct {
		Report      Report            `json:"report"`
		LoaderAfter map[string]string `json:"loader_after"`
	}{
		Report:      Apply(),
		LoaderAfter: map[string]string{},
	}
	for _, entry := range os.Environ() {
		if name, value, ok := strings.Cut(entry, "="); ok && hasLoaderPrefix(name) {
			out.LoaderAfter[name] = value
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
	os.Exit(0)
}

// hardenInChild runs the helper with extra environment and returns its report.
func hardenInChild(t *testing.T, extraEnv ...string) (Report, map[string]string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", "^TestHardenHelper$", "-test.v=false")
	cmd.Env = append(append(os.Environ(), helperEnv+"=1"), extraEnv...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("harden helper failed: %v\nstdout: %s", err, out)
	}
	// The JSON object is the last line: `go test` prints its own PASS/ok lines
	// around it, and os.Exit(0) inside the helper means the tail varies.
	var decoded struct {
		Report      Report            `json:"report"`
		LoaderAfter map[string]string `json:"loader_after"`
	}
	var found bool
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		if err := json.Unmarshal([]byte(line), &decoded); err == nil {
			found = true
		}
	}
	if !found {
		t.Fatalf("harden helper printed no report\nstdout: %s", out)
	}
	return decoded.Report, decoded.LoaderAfter
}

// TestApplyReportsEveryStep pins that all three measures run and that none of
// them fails on this host.
//
// A failing step is not automatically a bug in this package — a locked-down CI
// container can legitimately refuse setrlimit — but it IS something a human has
// to look at, and a test that tolerated failures would make the report
// unfalsifiable. The failure message carries the Detail so the reason is in the
// log rather than in a follow-up investigation.
func TestApplyReportsEveryStep(t *testing.T) {
	report, _ := hardenInChild(t)
	if report.Skipped {
		t.Fatalf("hardening was skipped in the helper; is %s set in this environment?", DisableEnv)
	}
	want := map[string]bool{"core-dumps": true, "debugger": true, "loader-env": true}
	for _, step := range report.Steps {
		if !want[step.Name] {
			t.Errorf("unexpected step %q", step.Name)
			continue
		}
		delete(want, step.Name)
		if step.Err != "" {
			t.Errorf("step %q failed on this host: %s (detail: %s)", step.Name, step.Err, step.Detail)
		}
		t.Logf("%-11s %s", step.Name, step.Detail)
	}
	for name := range want {
		t.Errorf("step %q never ran", name)
	}
	// Report.Failed() used to be asserted here. It was deleted: its doc said "so
	// a caller can decide whether the Report is worth printing", and the only
	// caller — WriteFailures — never called it, so the whole of its behaviour
	// was this assertion agreeing with the loop above it. An exported predicate
	// whose sole reader is a test that restates the loop next to it is not a
	// covered feature, it is a second implementation of the loop.
}

// TestCoreDumpsAreVerifiedNotAssumed pins that the core-dump step read the
// limit back rather than trusting setrlimit's return value.
//
// The distinction is the whole point of that step: setrlimit is one of the
// calls a container runtime or seccomp profile can turn into a silent success,
// and a step that reported "applied" there would be the line an operator acts
// on when deciding the host is safe to hold keys.
func TestCoreDumpsAreVerifiedNotAssumed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("RLIMIT_CORE does not exist on windows; the step reports not-applicable there")
	}
	report, _ := hardenInChild(t)
	for _, step := range report.Steps {
		if step.Name != "core-dumps" {
			continue
		}
		if !strings.Contains(step.Detail, "read-back") {
			t.Fatalf("core-dump step did not verify the limit: %q", step.Detail)
		}
		return
	}
	t.Fatal("no core-dumps step in the report")
}

// TestLoaderEnvIsRemovedFromTheChildEnvironment is the assertion that matches
// what this measure actually buys.
//
// It checks the environment AFTER hardening inside the hardened process,
// because that environment is the one every child yanshi spawns is built from
// (shell.childLaunchPosture starts at os.Environ()). Asserting only that
// scrubLoaderEnv returned a Step naming the variable would pass just as well if
// os.Unsetenv were never called.
// The injected set deliberately excludes DYLD_INSERT_LIBRARIES: dyld ABORTS a
// process whose insert list names a library that does not exist, so a child
// spawned with it never reaches Go's main and the test measures dyld rather
// than this package. DYLD_LIBRARY_PATH exercises the identical prefix rule and
// is merely ignored when it points nowhere. That the dangerous name is covered
// by the same rule is pinned separately by
// TestLoaderPrefixCoversTheInjectionVariables.
func TestLoaderEnvIsRemovedFromTheChildEnvironment(t *testing.T) {
	injected := []string{
		"LD_PRELOAD=/tmp/yanshi-test-not-a-real-library.so",
		"LD_AUDIT=/tmp/yanshi-test-not-a-real-auditor.so",
		"DYLD_LIBRARY_PATH=/tmp/yanshi-test-nonexistent-libdir",
	}
	report, after := hardenInChild(t, injected...)
	if len(after) != 0 {
		t.Fatalf("loader variables survived hardening: %v", after)
	}
	var detail string
	for _, step := range report.Steps {
		if step.Name == "loader-env" {
			detail = step.Detail
		}
	}
	for _, entry := range injected {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.Contains(detail, name) {
			t.Errorf("the report does not name %s as removed (detail: %q)", name, detail)
		}
	}
}

// TestLoaderPrefixCoversTheInjectionVariables pins the rule itself against the
// names that matter, including the two that cannot be exercised through a live
// child.
//
// It is a pure-function test on purpose: DYLD_INSERT_LIBRARIES and LD_PRELOAD
// are the variables the whole measure exists for, and the alternative — setting
// them for real and spawning something — measures the dynamic loader's
// tolerance for a missing library rather than this package's rule.
func TestLoaderPrefixCoversTheInjectionVariables(t *testing.T) {
	for _, name := range []string{
		"LD_PRELOAD", "LD_AUDIT", "LD_LIBRARY_PATH",
		"DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH", "DYLD_FRAMEWORK_PATH",
		"DYLD_FALLBACK_LIBRARY_PATH",
	} {
		if !hasLoaderPrefix(name) {
			t.Errorf("%s is not recognised as a loader variable", name)
		}
		if loaderEnvKeep[name] {
			t.Errorf("%s is on the keep list, so it would survive the scrub", name)
		}
	}
	// The other direction: the rule must not eat unrelated names. PATH and
	// LDFLAGS both start with characters the prefixes contain, and a rule
	// written as a substring match rather than a prefix would take LDFLAGS.
	for _, name := range []string{"PATH", "LDFLAGS", "CGO_LDFLAGS", "OLD_PATH", "DYLDX"} {
		if hasLoaderPrefix(name) {
			t.Errorf("%s must not be treated as a loader variable", name)
		}
	}
}

// TestDisableEnvSkipsEverything pins the escape hatch, in both directions: the
// report says it was skipped, and nothing was actually applied.
//
// The second half is what makes this more than a flag test. A DisableEnv that
// set Skipped=true and hardened anyway would leave the maintainer's `dlv exec`
// dying with a report that says hardening did not run — the worst possible
// combination, because the one place they would look says they are fine.
func TestDisableEnvSkipsEverything(t *testing.T) {
	report, after := hardenInChild(t,
		DisableEnv+"=1",
		"LD_PRELOAD=/tmp/yanshi-test-not-a-real-library.so",
	)
	if !report.Skipped {
		t.Fatal("Report.Skipped must be true when the disable variable is set")
	}
	if len(report.Steps) != 0 {
		t.Fatalf("steps ran despite the disable variable: %+v", report.Steps)
	}
	if after["LD_PRELOAD"] == "" {
		t.Fatal("LD_PRELOAD was scrubbed even though hardening was disabled")
	}
}

// TestWriteFailuresIsSilentOnSuccess pins that the startup path prints nothing
// when everything worked.
//
// This runs in-process because it touches no syscall: it is pure formatting.
// The property matters because Apply is called on EVERY invocation of a CLI
// whose output is routinely piped, and a banner before each command is how an
// operator learns to ignore the one line that reports a real degradation.
func TestWriteFailuresIsSilentOnSuccess(t *testing.T) {
	var sb strings.Builder
	Report{Steps: []Step{{Name: "core-dumps", Detail: "ok"}}}.WriteFailures(&sb)
	if sb.String() != "" {
		t.Fatalf("a clean report printed %q", sb.String())
	}

	sb.Reset()
	Report{Steps: []Step{{Name: "debugger", Detail: "tried", Err: "boom"}}}.WriteFailures(&sb)
	if !strings.Contains(sb.String(), "debugger") || !strings.Contains(sb.String(), "boom") {
		t.Fatalf("a failed step must name itself and its reason, got %q", sb.String())
	}

	sb.Reset()
	Report{Skipped: true}.WriteFailures(&sb)
	if !strings.Contains(sb.String(), DisableEnv) {
		t.Fatalf("a skipped report must name the variable that skipped it, got %q", sb.String())
	}
}
