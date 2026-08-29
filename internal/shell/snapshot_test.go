package shell

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// lastEnvValue returns the LAST value for name, which is what exec's dedup gives
// the child.
func lastEnvValue(env []string, name string) (string, bool) {
	value, found := "", false
	for _, item := range env {
		k, v, ok := strings.Cut(item, "=")
		if ok && k == name {
			value, found = v, true
		}
	}
	return value, found
}

// TestZeroSnapshotApplyIsTheIdentity is W-B-21's "还原失败不影响会话" in its
// purest form: every failure path in CaptureSnapshot returns this value, and
// applying it must leave the environment byte-for-byte unchanged.
func TestZeroSnapshotApplyIsTheIdentity(t *testing.T) {
	base := []string{"PATH=/bin", "HOME=/home/x", "WEIRD=1"}
	got := Snapshot{}.Apply(base)
	if len(got) != len(base) {
		t.Fatalf("Apply changed the environment: %v", got)
	}
	for i := range base {
		if got[i] != base[i] {
			t.Fatalf("Apply changed entry %d: %q -> %q", i, base[i], got[i])
		}
	}
}

// TestSnapshotDoesNotOverrideTheHostEnvironment pins the precedence. A value
// yanshi's operator exported for the SERVER has to beat a value their rc file
// happened to set; the snapshot only fills in names the base never mentions.
func TestSnapshotDoesNotOverrideTheHostEnvironment(t *testing.T) {
	snap := Snapshot{Env: map[string]string{
		"HOME":      "/home/from-rc",
		"NVM_DIR":   "/home/x/.nvm",
		"GOPRIVATE": "example.com/*",
	}}
	got := snap.Apply([]string{"HOME=/home/host", "GOPRIVATE="})

	if v, _ := lastEnvValue(got, "HOME"); v != "/home/host" {
		t.Fatalf("HOME = %q, want the host value", v)
	}
	// An empty host value still counts as "the base mentions this name".
	if v, _ := lastEnvValue(got, "GOPRIVATE"); v != "" {
		t.Fatalf("GOPRIVATE = %q, want the host's empty value", v)
	}
	if v, ok := lastEnvValue(got, "NVM_DIR"); !ok || v != "/home/x/.nvm" {
		t.Fatalf("NVM_DIR = %q (present=%v), want the captured value", v, ok)
	}
}

// TestSnapshotMergesPathInsteadOfReplacingIt is the exception, and the whole
// reason the feature exists: a launcher's PATH must stay reachable while the
// operator's toolchain directories become reachable.
func TestSnapshotMergesPathInsteadOfReplacingIt(t *testing.T) {
	sep := string(os.PathListSeparator)
	snap := Snapshot{Env: map[string]string{"PATH": "/opt/brew/bin" + sep + "/usr/bin"}}
	got := snap.Apply([]string{"PATH=/usr/bin" + sep + "/bin"})

	value, ok := lastEnvValue(got, "PATH")
	if !ok {
		t.Fatal("PATH missing")
	}
	parts := strings.Split(value, sep)
	want := []string{"/usr/bin", "/bin", "/opt/brew/bin"}
	if len(parts) != len(want) {
		t.Fatalf("PATH = %q, want %v", value, want)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("PATH = %q, want %v (order is precedence and must not be reshuffled)", value, want)
		}
	}
}

func TestParseEnvDump(t *testing.T) {
	dump := "PATH=/bin\n" +
		"MULTI=first\ncontinued\n" +
		"PWD=/should/be/dropped\n" +
		"SHLVL=3\n" +
		"BASH_FUNC_x()=() {  echo hi\n}\n" +
		"LAST=tail\n"
	env := parseEnvDump(dump)

	if env["PATH"] != "/bin" {
		t.Fatalf("PATH = %q", env["PATH"])
	}
	if env["MULTI"] != "first\ncontinued" {
		t.Fatalf("multi-line value = %q; a continuation line was dropped or mangled", env["MULTI"])
	}
	for _, dropped := range []string{"PWD", "SHLVL"} {
		if _, ok := env[dropped]; ok {
			t.Fatalf("%s reached the child; it names the capture shell's state, not the project's", dropped)
		}
	}
	if env["LAST"] != "tail" {
		t.Fatalf("LAST = %q; the trailing newline was not trimmed", env["LAST"])
	}
}

// TestParseEnvDumpHandlesCRLF covers the PowerShell path, whose output is
// CRLF-terminated. Without the normalisation every captured value would end in
// a stray carriage return.
func TestParseEnvDumpHandlesCRLF(t *testing.T) {
	env := parseEnvDump("Path=C:\\bin\r\nUSERPROFILE=C:\\Users\\x\r\n")
	if env["Path"] != "C:\\bin" {
		t.Fatalf("Path = %q", env["Path"])
	}
	if env["USERPROFILE"] != "C:\\Users\\x" {
		t.Fatalf("USERPROFILE = %q", env["USERPROFILE"])
	}
}

func TestSnapshotArgvPerShell(t *testing.T) {
	for _, name := range []string{"bash", "zsh", "sh"} {
		program, args, err := snapshotArgv(name)
		if err != nil {
			t.Fatalf("snapshotArgv(%q): %v", name, err)
		}
		if program != name {
			t.Fatalf("program = %q, want %q", program, name)
		}
		if strings.Join(args, " ") != "-l -c env" {
			t.Fatalf("%s args = %v", name, args)
		}
	}
	program, args, err := snapshotArgv("powershell")
	if err != nil {
		t.Fatalf("snapshotArgv(powershell): %v", err)
	}
	if program != "powershell" || !strings.Contains(strings.Join(args, " "), "Get-ChildItem Env:") {
		t.Fatalf("powershell argv = %q %v", program, args)
	}
	// -NoProfile would defeat the entire point: the profile is what sets the
	// operator's toolchain up.
	if strings.Contains(strings.Join(args, " "), "-NoProfile") {
		t.Fatal("the PowerShell capture skips the profile it exists to read")
	}
	if _, _, err := snapshotArgv("cmd"); err == nil {
		t.Fatal("cmd was accepted; it has no startup environment to capture")
	}
	if _, _, err := snapshotArgv("fish"); err == nil {
		t.Fatal("an unknown shell was accepted")
	}
}

// TestCaptureSnapshotFailureYieldsTheZeroValue pins the fail-safe contract at
// its source: an error never comes with a partial snapshot, so a caller that
// uses the value regardless (as bootstrap does) cannot apply half a capture.
func TestCaptureSnapshotFailureYieldsTheZeroValue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := CaptureSnapshot(ctx, "definitely-not-a-shell")
	if err == nil {
		t.Fatal("CaptureSnapshot accepted an unknown shell")
	}
	if !got.Empty() {
		t.Fatalf("a failed capture returned %+v, want the zero Snapshot", got)
	}
}

// TestCaptureSnapshotHonoursItsContext pins the bound. This runs on the path
// to the first prompt, and an rc file that blocks on a network call or a
// keypress would otherwise hold the whole start-up there.
//
// The context is canceled BEFORE the call rather than given a short deadline:
// `sh -l -c env` finishes in milliseconds on a healthy machine, so a deadline
// race would pass for the wrong reason (the shell won) far more often than it
// would test anything. exec.CommandContext.Start returns the context error
// when the context is already done, which makes this deterministic.
func TestCaptureSnapshotHonoursItsContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the POSIX capture argv is not what runs on windows")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no sh on PATH: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := CaptureSnapshot(ctx, "sh")
	if err == nil {
		t.Fatal("a canceled capture reported success")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want it to wrap context.Canceled", err)
	}
	if !got.Empty() {
		t.Fatalf("a canceled capture returned %+v, want the zero Snapshot", got)
	}
}

// TestCaptureSnapshotReadsTheLoginShell is the end-to-end half: a real shell,
// really executed, really parsed. Without it every test above could pass over
// a capture that has never once produced a variable.
func TestCaptureSnapshotReadsTheLoginShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the POSIX capture argv is not what runs on windows")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no sh on PATH: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	snap, err := CaptureSnapshot(ctx, "sh")
	if err != nil {
		t.Fatalf("CaptureSnapshot(sh): %v", err)
	}
	if snap.Empty() {
		t.Fatal("captured an empty environment from a working shell")
	}
	if _, ok := snap.Env["PATH"]; !ok {
		t.Fatalf("no PATH in the captured environment: %v", snapshotEnvKeys(snap.Env))
	}
	if _, ok := snap.Env["PWD"]; ok {
		t.Fatal("PWD survived the skip list")
	}
}

func snapshotEnvKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
