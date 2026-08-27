package securityverify

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	obslog "github.com/x6nux/yanshi/internal/observe/log"
	"github.com/x6nux/yanshi/internal/secrets"
)

// o10_unregistered_test.go extends the crash-report redaction check to the
// credentials nobody registered.
//
// TestO10_ReportIsRedacted uses a stand-in redactor primed with the exact
// canary it then searches for. That is the right way to prove the redactor is
// WIRED — it isolates the plumbing from the detection rules — and it is
// necessarily silent about which secrets the real redactor recognises.
//
// The gap that leaves is the same one measured in the audit table (see
// s6_unregistered_test.go): the process redactor holds what yanshi RESOLVED,
// while a crash report holds whatever the code was handling when it died. A
// panic mid-way through a `curl` tool call, or a provider error echoing a
// request header, carries a token this process never resolved and therefore
// never registered.
//
// The crash report is also the sink with the worst disclosure profile of the
// three: it is a file an operator is explicitly invited to attach to a bug
// report and mail to a maintainer.
//
// The child here installs the handler with a REAL secrets.Redactor whose
// registry is EMPTY, so anything caught is caught by shape alone.

// TestO10_UnregisteredCredentialChildHelper panics carrying a credential the
// redactor was never told about. It runs only when the parent asks.
func TestO10_UnregisteredCredentialChildHelper(t *testing.T) {
	if os.Getenv("YANSHI_O10U_CHILD") == "" {
		t.Skip("child-process helper; driven by TestO10_UnregisteredCredentialsAreRedacted")
	}
	restore := obslog.InstallCrashHandler(obslog.CrashHandlerConfig{
		Dir: os.Getenv("YANSHI_O10U_DIR"),
		// A real redactor with NOTHING registered. This is the configuration a
		// process is in for every credential it did not resolve itself.
		Redactor: secrets.NewRedactor(),
		Stderr:   os.Stderr,
	})
	defer restore()
	panic("request failed: " + os.Getenv("YANSHI_O10U_SECRET"))
}

// TestO10_UnregisteredCredentialsAreRedacted runs a real panicking child and
// greps the bytes that landed on disk.
func TestO10_UnregisteredCredentialsAreRedacted(t *testing.T) {
	cases := []struct{ name, secret string }{
		{"github token", "ghp_O10UNREGaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"openai key", "sk-proj-O10UNREG0123456789abcdefghijklmnop"},
		{"jwt", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJvMTB1bnJlZyJ9.QQQQQQQQQQQQQQQQ"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "crash")
			cmd := exec.Command(os.Args[0], "-test.run=TestO10_UnregisteredCredentialChildHelper")
			cmd.Env = append(os.Environ(),
				"YANSHI_O10U_CHILD=1",
				"YANSHI_O10U_DIR="+dir,
				"YANSHI_O10U_SECRET="+c.secret,
			)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("the child was supposed to die from a panic; it exited 0:\n%s", out)
			}

			entries, cerr := obslog.CrashDirEntries(dir)
			if cerr != nil || len(entries) == 0 {
				t.Fatalf("no crash report was written (err=%v) — nothing was verified", cerr)
			}
			raw, rerr := os.ReadFile(entries[0])
			if rerr != nil {
				t.Fatal(rerr)
			}
			if strings.Contains(string(raw), c.secret) {
				t.Fatalf("LEAK: an unregistered %s is in the crash report at %s — "+
					"this is the file operators are invited to mail to a maintainer", c.name, entries[0])
			}
			if !strings.Contains(string(raw), "[REDACTED]") {
				t.Errorf("the secret is neither present nor redacted; check the panic text "+
					"reached the report at all:\n%s", truncate(string(raw), 600))
			}
			// The report must still be diagnosable. Redacting the whole panic
			// message would satisfy the assertion above and destroy the report.
			if !strings.Contains(string(raw), "request failed") {
				t.Error("redaction consumed the panic message; the report no longer says what happened")
			}
		})
	}
}
