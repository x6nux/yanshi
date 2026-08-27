package securityverify

import (
	"context"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/secrets"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
)

// s6_unregistered_test.go covers the half of the audit-redaction question that
// s6_audit_test.go could not reach.
//
// That file registers the canary with app.Redactor and then shows it does not
// appear on disk. That proves the plumbing between the store and the redactor,
// and it is silent about the population of credentials the redactor has never
// heard of — which is most of them, from the audit table's point of view.
//
// The distinction is not academic. The registry holds what YANSHI resolved:
// provider keys, OAuth tokens, keyring entries. The audit table records what
// the AGENT ran. A token the agent pasted into a curl command, read out of a
// CI secret, or received in a tool result was never resolved by this process
// and therefore never registered — and the audit digest is, by design, the one
// field that echoes caller text verbatim.
//
// Measured before internal/secrets/patternredact.go existed: authorizing
//
//	curl -H 'Authorization: Bearer sk-proj-…' https://example.com
//
// under a real bootstrap App wrote that row to SQLite with the key intact.

// unregisteredCredentials are vendor-shaped tokens that NOTHING registers. The
// values are semantically worthless: each one matches the credentialPatterns
// regex for its vendor, which is what the redaction pass keys on, but the Slack
// and Stripe rows deliberately do NOT reproduce their vendor's real internal
// structure. Our regexes are loose minimum-length ones (`xox[baprs]-.{10,}`,
// `[rs]k_(?:live|test)_[A-Za-z0-9]{16,}`); the vendors' real shapes are far
// narrower, so these bodies sit in the gap — long enough for us, too short and
// too unstructured for the vendor. GitHub's push protection matches the narrow
// shape and blocked this file twice while these rows were faithful to it — a
// fixture only has to be recognisable to OUR redactor, and one that is
// indistinguishable from a live credential costs a manual unblock on every
// push and trains reviewers to wave secret alerts through.
//
// The set spans the shapes with different structures — prefix+body, fixed
// length, dotted segments, a multi-line block, and an inline URL password —
// because a redaction pass that handles one shape says nothing about the rest.
var unregisteredCredentials = []struct {
	name  string
	token string
	// keep, when non-empty, is a substring that must SURVIVE redaction. It
	// exists for the URL case, where the host is the diagnostic value of the
	// record and only the password is the secret.
	keep string
}{
	{name: "openai project key", token: "sk-proj-A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0"},
	{name: "anthropic key", token: "sk-ant-api03-aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789"},
	{name: "github classic pat", token: "ghp_1234567890abcdefghijklmnopqrstuvwxyz"},
	{name: "github fine-grained pat", token: "github_pat_11ABCDEFG0abcdefghijklmnopqrstuvwxyz012345"},
	{name: "gitlab pat", token: "glpat-xxxxxxxxxxxxxxxxxxxx"},
	{name: "slack bot token", token: "xoxb-EXAMPLE-NOT-A-REAL-SLACK-TOKEN"},
	{name: "aws access key id", token: "AKIAIOSFODNN7EXAMPLE"},
	{name: "google api key", token: "AIzaSyA1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q"},
	{name: "stripe live key", token: "sk_live_NOTAREALSTRIPEKEY"},
	{name: "npm automation token", token: "npm_abcdefghijklmnopqrstuvwxyz0123456789"},
	{name: "jwt", token: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk"},
	{name: "postgres url with inline password", token: "postgres://appuser:hunter2SuperSecret@db.internal:5432/app", keep: "db.internal"},
}

// TestS6_UnregisteredCredentialShapesAreRedactedInTheAuditTable is the
// end-to-end proof: a real App, a real Authorize, a real SQLite read-back.
func TestS6_UnregisteredCredentialShapesAreRedactedInTheAuditTable(t *testing.T) {
	app := buildAuditApp(t)
	ctx := tools.WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
	})

	for _, c := range unregisteredCredentials {
		cmd := "curl -H 'Authorization: Bearer " + c.token + "' https://example.com"
		if err := tools.Authorize(ctx, guard.Action{Tool: "shell_run", Shell: cmd}, "{}"); err != nil {
			t.Fatalf("%s: setup, the command should have been allowed: %v", c.name, err)
		}
	}

	rows, err := app.Store.QueryPermissionAudit(store.PermissionAuditQuery{Tool: "shell_run"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < len(unregisteredCredentials) {
		t.Fatalf("expected %d rows on disk, got %d — nothing was verified",
			len(unregisteredCredentials), len(rows))
	}
	all := ""
	for _, r := range rows {
		all += r.CmdDigest + "\n"
	}
	for _, c := range unregisteredCredentials {
		if strings.Contains(all, c.token) {
			t.Errorf("LEAK: %s reached SQLite verbatim; the audit table is a credential dump for every token yanshi did not resolve itself", c.name)
		}
		if c.keep != "" && !strings.Contains(all, c.keep) {
			t.Errorf("%s: redaction removed %q as well; the record no longer says which host was contacted", c.name, c.keep)
		}
	}
	// The command shell itself must survive: a digest redacted down to nothing
	// records that SOMETHING happened, which is not an audit trail.
	if !strings.Contains(all, "curl") {
		t.Error("redaction consumed the command word; the trail can no longer name the action")
	}
}

// TestS6_OrdinaryTextIsNotRedacted is the false-positive control, and it is the
// reason the patterns are prefix-anchored with minimum body lengths rather than
// an entropy heuristic.
//
// A false positive here is not a cosmetic problem. Redact runs on the path into
// SQLite (audit digests, message logs, session titles), so a pattern that eats
// ordinary text writes that damage to disk, permanently, and fixing the pattern
// afterwards does not recover it. This is the same trade secrets.MinSecretLength
// documents for short registered values, arrived at from the other direction.
func TestS6_OrdinaryTextIsNotRedacted(t *testing.T) {
	ordinary := []string{
		// Prose and identifiers that contain a pattern prefix as a substring.
		"the task-force met to discuss sk-learn and scikit-learn",
		"git checkout ghost-branch && ghc --version",
		"go test ./internal/... -run TestGhostWriter",
		"npm_config_registry is an environment variable name",
		"see https://example.com/AIzaSy for the docs page",
		// Real command lines with no credential in them.
		"curl -sSL https://example.com/install.sh",
		"psql postgres://localhost:5432/app",
		"git clone https://github.com/x6nux/yanshi.git",
		"docker run -e MODE=test alpine:3.20 sh -c 'echo hi'",
		// Hashes and ids, which an entropy-based rule would have destroyed.
		"commit e359584c9f1a2b3d4e5f60718293a4b5c6d7e8f9",
		"sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		"trace_id=4bf92f3577b34da6a3ce929d0e0e4736",
	}
	for _, s := range ordinary {
		if got := secrets.RedactPatterns(s); got != s {
			t.Errorf("FALSE POSITIVE: %q\n          became %q\nordinary text written into SQLite would be corrupted irrecoverably", s, got)
		}
	}
}

// TestS6_PatternRedactionIsIndependentOfRegistration states the property
// directly, without a database in the way.
//
// It uses a FRESH redactor with nothing registered, which is the configuration
// where the pre-fix code returned its input untouched — the early return on an
// empty registry was correct when registration was the only mechanism and would
// have made the pattern pass dead in exactly the processes that register
// nothing.
func TestS6_PatternRedactionIsIndependentOfRegistration(t *testing.T) {
	r := secrets.NewRedactor() // nothing registered, ever
	for _, c := range unregisteredCredentials {
		in := "prefix " + c.token + " suffix"
		out := r.Redact(in)
		if strings.Contains(out, c.token) {
			t.Errorf("%s: Redact on an EMPTY registry left the token intact: %q", c.name, out)
		}
		if !strings.Contains(out, "prefix ") || !strings.Contains(out, " suffix") {
			t.Errorf("%s: surrounding text was destroyed: %q", c.name, out)
		}
	}
}

// TestS6_RedactJSONAlsoCoversShapes pins the WS/SSE boundary. RedactJSON is a
// separate method with its own loop, so a fix applied only to Redact would
// leave every streamed frame unprotected — and frames are where a tool result
// containing a token reaches the operator's screen and the transcript.
func TestS6_RedactJSONAlsoCoversShapes(t *testing.T) {
	r := secrets.NewRedactor()
	const token = "ghp_1234567890abcdefghijklmnopqrstuvwxyz"
	got := string(r.RedactJSON([]byte(`{"content":"export GH_TOKEN=` + token + `"}`)))
	if strings.Contains(got, token) {
		t.Errorf("RedactJSON left the token in a wire frame: %s", got)
	}
	if !strings.Contains(got, `"content"`) {
		t.Errorf("RedactJSON damaged the frame structure: %s", got)
	}
}
