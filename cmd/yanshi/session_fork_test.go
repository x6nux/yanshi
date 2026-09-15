package main

import (
	"strings"
	"testing"
)

// TestSessionForkCopiesUpToASeq pins the fork contract: the new session id goes
// to stdout (so `id=$(yanshi session fork "$s")` composes), the fork is
// readable, and -json reports the same id in a field.
func TestSessionForkCopiesUpToASeq(t *testing.T) {
	cfg, id := sessionTestEnv(t, "fork me")

	code, out, errs := runSessionCLI(t, "fork", id, "-config", cfg)
	if code != exitOK {
		t.Fatalf("fork exit=%d stderr=%q", code, errs)
	}
	forked := strings.TrimSpace(out)
	if forked == "" || forked == id {
		t.Fatalf("fork id = %q, want a new id", forked)
	}
	if code, out, _ := runSessionCLI(t, "show", forked, "-config", cfg, "-json"); code != exitOK ||
		!strings.Contains(out, "fork me") {
		t.Fatalf("forked session not readable: %q", out)
	}

	// -json must carry the same id, or a script has to parse two formats.
	code, out, _ = runSessionCLI(t, "fork", id, "-config", cfg, "-json")
	if code != exitOK || !strings.Contains(out, `"forkedId"`) {
		t.Fatalf("fork -json output = %q", out)
	}

	// A fork of an unknown session is an error, not a silent empty session.
	if code, _, _ := runSessionCLI(t, "fork", "nope", "-config", cfg); code != exitErr {
		t.Fatalf("fork of an unknown id: exit=%d, want exitErr", code)
	}
}
