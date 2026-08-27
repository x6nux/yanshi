package securityverify

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	obslog "github.com/x6nux/yanshi/internal/observe/log"
)

// o10_crash_test.go verifies crash-site capture by CRASHING A REAL PROCESS.
//
// The distinction is the entire finding. CrashDumper.Dump can be called
// directly from a test and will faithfully write a report — it did, and every
// unit test of it passed — while the shipped binary had no deferred recover
// anywhere in the turn path and no call to NewCrashDumper at all. A panic took
// the process down with a bare Go traceback and no report was written. The
// capability was complete and unreachable.
//
// So the assertion here is made from OUTSIDE the process: fork a child, make it
// panic, and look at the disk and at its stderr afterwards. That is a claim
// about the program; calling Dump in-process is a claim about a function.

// runPanickingChild re-executes this test binary in a subprocess that installs
// the production crash handler and then panics. It returns the child's stderr
// and the crash directory it was given.
func runPanickingChild(t *testing.T, extraEnv ...string) (stderr, crashDir string) {
	t.Helper()
	crashDir = filepath.Join(t.TempDir(), "crash")
	cmd := exec.Command(os.Args[0], "-test.run=TestO10_CrashChildHelper")
	cmd.Env = append(os.Environ(),
		"YANSHI_O10_CHILD=1",
		"YANSHI_O10_CRASHDIR="+crashDir,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the child was supposed to die from a panic; it exited 0:\n%s", out)
	}
	return string(out), crashDir
}

// TestO10_CrashChildHelper is the child half of the fork. It only does anything
// when the parent set YANSHI_O10_CHILD, so an ordinary `go test` run skips it.
func TestO10_CrashChildHelper(t *testing.T) {
	if os.Getenv("YANSHI_O10_CHILD") == "" {
		t.Skip("child-process helper; driven by TestO10_*")
	}
	dir := os.Getenv("YANSHI_O10_CRASHDIR")
	// This is the production installer, called the way a real entry point
	// calls it. If it does not exist or does not capture, the parent sees it.
	restore := obslog.InstallCrashHandler(obslog.CrashHandlerConfig{
		Dir:      dir,
		Redactor: secretRedactor{secret: os.Getenv("YANSHI_O10_SECRET")},
		Stderr:   os.Stderr,
	})
	defer restore()
	panic("O10 synthetic panic: " + os.Getenv("YANSHI_O10_SECRET"))
}

// secretRedactor stands in for the process redactor with exactly one known
// secret, so the report can be checked for it.
type secretRedactor struct{ secret string }

func (r secretRedactor) Redact(s string) string {
	if r.secret == "" {
		return s
	}
	return strings.ReplaceAll(s, r.secret, "[REDACTED]")
}

// TestO10_PanicWritesAReportAndAnnouncesIt is the primary claim: a process that
// dies from a panic leaves a report behind AND says where it is. A report
// nobody is told about is a file that gets deleted with the temp directory.
func TestO10_PanicWritesAReportAndAnnouncesIt(t *testing.T) {
	stderr, crashDir := runPanickingChild(t)
	t.Logf("child stderr:\n%s", indent(stderr))

	entries, err := obslog.CrashDirEntries(crashDir)
	if err != nil {
		t.Fatalf("crash dir unreadable: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("the process panicked and no crash report was written to %s", crashDir)
	}
	t.Logf("crash reports written: %v", entries)

	if !strings.Contains(stderr, "crash report") {
		t.Error("the crash path was never announced on stderr; nobody will find the file")
	}
	if !strings.Contains(stderr, entries[0]) {
		t.Errorf("stderr does not name the report that was written (%s)", entries[0])
	}

	// The panic must still propagate: swallowing it would turn a crash into a
	// silent wrong answer, which is strictly worse than crashing.
	if !strings.Contains(stderr, "O10 synthetic panic") {
		t.Error("the panic was swallowed rather than re-raised after capture")
	}
}

// TestO10_ReportOmitsBodiesByDefault pins the disclosure posture. The normal
// log path prints only an error TYPE precisely because provider errors carry
// prompts and credentials, and a crash report that flipped that by default
// would be a leak with extra steps.
func TestO10_ReportOmitsBodiesByDefault(t *testing.T) {
	_, crashDir := runPanickingChild(t)
	rep := readOneReport(t, crashDir)

	if rep.BodiesIncluded {
		t.Error("bodies_included must default to false")
	}
	for i, m := range rep.Messages {
		if m.Body != "" {
			t.Errorf("message %d carries a body by default: %q", i, m.Body)
		}
	}
	// The report still has to be USEFUL, or the safe default has simply
	// produced an empty file.
	if rep.Stack == "" {
		t.Error("a report with no stack cannot diagnose anything")
	}
	if rep.Kind != "panic" {
		t.Errorf("kind = %q, want panic", rep.Kind)
	}
	t.Logf("report: kind=%s error_type=%s stack=%d bytes messages=%d bodies=%v",
		rep.Kind, rep.ErrorType, len(rep.Stack), len(rep.Messages), rep.BodiesIncluded)
}

// TestO10_ReportIsRedacted proves the redactor is actually applied to the
// on-disk bytes. A panic value routinely contains whatever the code was
// handling when it died.
func TestO10_ReportIsRedacted(t *testing.T) {
	const secret = "sk-ant-O10CANARY-0123456789abcdef"
	crashDir := filepath.Join(t.TempDir(), "crash")
	cmd := exec.Command(os.Args[0], "-test.run=TestO10_CrashChildHelper")
	cmd.Env = append(os.Environ(),
		"YANSHI_O10_CHILD=1",
		"YANSHI_O10_CRASHDIR="+crashDir,
		"YANSHI_O10_SECRET="+secret,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("child should have panicked:\n%s", out)
	}

	entries, cerr := obslog.CrashDirEntries(crashDir)
	if cerr != nil || len(entries) == 0 {
		t.Fatalf("no report written (err=%v)", cerr)
	}
	raw, rerr := os.ReadFile(entries[0])
	if rerr != nil {
		t.Fatal(rerr)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("LEAK: the raw secret is in the crash report at %s", entries[0])
	}
	if !strings.Contains(string(raw), "[REDACTED]") {
		t.Errorf("the secret was neither present nor redacted — is the redactor wired at all?\n%s",
			truncate(string(raw), 800))
	}
	t.Logf("report redacted; %d bytes on disk", len(raw))
}

// readOneReport parses the newest report in dir.
func readOneReport(t *testing.T, dir string) obslog.CrashReport {
	t.Helper()
	entries, err := obslog.CrashDirEntries(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no crash report in %s (err=%v)", dir, err)
	}
	raw, err := os.ReadFile(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	var rep obslog.CrashReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("report is not valid JSON: %v\n%s", err, truncate(string(raw), 500))
	}
	return rep
}

// indent prefixes each line, so a child's output is distinguishable from the
// parent test's own in the log.
func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}

// truncate bounds a string for a failure message.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TestO10_WithoutTheInstallerNothingIsWritten is the CONTROL that gives every
// assertion above its meaning, and it is also the measurement that found the
// gap in the first place.
//
// It re-runs the identical panic in a child that does NOT install the handler —
// which is exactly what the shipped binary did before crashinstall.go existed,
// since no entry point called NewCrashDumper and no turn path had a deferred
// recover. The crash directory comes back empty and stderr carries only a bare
// Go traceback.
//
// Keeping this alongside the positive tests means "a report was written" can
// never be satisfied by something else in the environment writing files there.
func TestO10_WithoutTheInstallerNothingIsWritten(t *testing.T) {
	crashDir := filepath.Join(t.TempDir(), "crash")
	if err := os.MkdirAll(crashDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestO10_UninstalledChildHelper")
	cmd.Env = append(os.Environ(),
		"YANSHI_O10_CHILD=1",
		"YANSHI_O10_CRASHDIR="+crashDir,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("child should have panicked:\n%s", out)
	}
	t.Logf("uninstalled child stderr:\n%s", indent(string(out)))

	entries, cerr := obslog.CrashDirEntries(crashDir)
	if cerr != nil {
		t.Fatal(cerr)
	}
	if len(entries) != 0 {
		t.Fatalf("a report appeared without the installer being called: %v", entries)
	}
	if strings.Contains(string(out), "crash report ->") {
		t.Error("stderr announced a report that was never written")
	}
	t.Log("confirmed: with no installer, a panic leaves no report — this was the shipped behaviour")
}

// TestO10_UninstalledChildHelper panics WITHOUT installing the handler.
func TestO10_UninstalledChildHelper(t *testing.T) {
	if os.Getenv("YANSHI_O10_CHILD") == "" {
		t.Skip("child-process helper; driven by TestO10_*")
	}
	panic("O10 synthetic panic: uninstalled")
}
