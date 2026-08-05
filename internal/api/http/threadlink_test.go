package http

import (
	"os"
	"strings"
	"testing"
)

// TestWSTurnCarriesThreadID pins the WS side of the correlation wiring.
//
// The v1 path got a test when its IDs were filled; this one did not, and W3
// review round 10 found the asymmetry by blanking cs.sessionID and watching
// nothing redden. An empty ThreadID makes WithThreadLink bind an empty string,
// and every log line and tool-audit record for the turn loses its way back to
// the conversation — silently, since nothing downstream requires the field.
//
// Checked at the source: the field is consumed through a context injector
// whose only observable effect is log content, and asserting on log text would
// pin the formatting rather than the wiring.
func TestWSTurnCarriesThreadID(t *testing.T) {
	src, err := os.ReadFile("ws.go")
	if err != nil {
		t.Fatalf("read ws.go: %v", err)
	}
	if !strings.Contains(string(src), "ThreadID:            cs.sessionID,") {
		t.Error("the WS turn no longer passes ThreadID: its logs and audit records " +
			"lose the link to the conversation that produced them")
	}
}

// TestSSETurnDeliberatelyHasNoThreadID guards a decision, not an omission.
//
// SSE is stateless — the client holds the history and replays it every
// request — so there is no server-side conversation to correlate against.
// Minting an id would produce a fresh one per request, which is worse than
// none because it looks like a thread. If someone later "fixes" the gap, this
// says why it is not one.
func TestSSETurnDeliberatelyHasNoThreadID(t *testing.T) {
	src, err := os.ReadFile("chat.go")
	if err != nil {
		t.Fatalf("read chat.go: %v", err)
	}
	body := string(src)
	// Scoped to the TurnOpts literal: chat.go's REQUEST struct legitimately has
	// a thread_id field (clients may echo one back), and a bare "ThreadID:"
	// search matches that too. Narrowing the window is the difference between
	// a guard and a tripwire that fires on the wrong thing.
	opts := strings.Index(body, "opts := orchestrator.TurnOpts{")
	if opts < 0 {
		t.Fatal("chat.go no longer builds a TurnOpts literal; this guard needs rewriting")
	}
	end := strings.Index(body[opts:], "\n\t}")
	if end < 0 {
		t.Fatal("could not find the end of the TurnOpts literal")
	}
	// Comment lines are excluded: the note explaining the absence necessarily
	// spells the field name, and a guard that fires on its own rationale is a
	// guard nobody keeps.
	var code []string
	for _, line := range strings.Split(body[opts:opts+end], "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "//") {
			code = append(code, line)
		}
	}
	if strings.Contains(strings.Join(code, "\n"), "ThreadID:") {
		t.Error("chat.go's TurnOpts now sets a ThreadID. SSE has no conversation " +
			"identity, so whatever this is, it is per-request — read the comment " +
			"above the literal before treating it as a fix")
	}
	if !strings.Contains(body, "// No ThreadID: SSE is stateless.") {
		t.Error("the note explaining why SSE has no ThreadID is gone; without it the " +
			"absence reads as an oversight")
	}
}

// TestCompactNowSizesChunksAgainstTheSummaryModel pins which model's window
// the manual /compact path budgets against.
//
// compactNow summarises with compactionModel(...), which is the configured
// compaction.model when set -- typically a small fast model. Sizing
// RunSummary's chunk budget against the SESSION model's window instead hands
// that small model a chunk it cannot accept and the provider rejects the
// compaction outright. ForceCompact's own doc says the window must be the
// summary model's; the caller was passing the session's.
//
// Source-level: reaching compactNow needs a live ws conn, a session with
// history and two registered models, and the assertion is about which of two
// names is looked up -- a harness that heavy would pin the harness.
func TestCompactNowSizesChunksAgainstTheSummaryModel(t *testing.T) {
	src, err := os.ReadFile("ws_compaction.go")
	if err != nil {
		t.Fatalf("read ws_compaction.go: %v", err)
	}
	body := string(src)
	if !strings.Contains(body, "windowOwner := cs.model") ||
		!strings.Contains(body, `if s.compaction.Model != "" {`) {
		t.Error("compactNow no longer prefers the summary model's window: a configured " +
			"compaction.model gets chunks budgeted for the session model's window")
	}
	if !strings.Contains(body, "cw := contextWindowFor(windowOwner, s.compaction)") {
		t.Error("the resolved window is no longer the one handed to ForceCompact")
	}
}
