package v1

import (
	"os"
	"strings"
	"testing"
)

// TestTurnOptsCarryCorrelationIDs pins the two fields that let a turn's logs
// and tool-audit records be traced back to the conversation that produced them.
//
// All three turn entry points left ThreadID and TurnID empty, so
// tools.WithThreadLink bound two empty strings and the consumer in
// orchestrator.go read zero values on every production turn. The v1 path is
// the one with real identities available — st.thread.ID and ts.turn.ID — so it
// is the one asserted here.
//
// Checked at the source rather than by running a turn: the fields are consumed
// through a context injector whose effect is only observable in log output,
// and asserting on log text would pin the formatting rather than the wiring.
func TestTurnOptsCarryCorrelationIDs(t *testing.T) {
	src, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	for _, want := range []string{"ThreadID:       st.thread.ID", "TurnID:         ts.turn.ID"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("service.go no longer sets %q — the turn's records lose their link to the thread", want)
		}
	}
}
