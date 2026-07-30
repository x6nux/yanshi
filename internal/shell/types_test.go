package shell

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestJobSerializesSnakeCaseAndOmitsEmptyEndedAt(t *testing.T) {
	job := Job{ID: "job-1", SessionID: "s-1", Command: "go test", State: StateRunning, ExitCode: -1, PID: 12, StartedAt: time.Unix(1700000000, 0)}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{`"session_id"`, `"started_at"`, `"exit_code":-1`, `"state":"running"`, `"pid":12`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in %s", want, s)
		}
	}
	if strings.Contains(s, `"ended_at"`) {
		t.Fatalf("zero EndedAt must be omitted: %s", s)
	}
}

func TestSessionExitedZeroKeepsExitCodeField(t *testing.T) {
	sess := Session{ID: "s-1", State: StateExited, ExitCode: 0}
	data, _ := json.Marshal(sess)
	// exit_code is NOT omitempty: zero is a meaningful value (clean exit) and
	// must survive the round trip, unlike EndedAt where zero means unknown.
	if !strings.Contains(string(data), `"exit_code":0`) {
		t.Fatalf("exit_code=0 must serialize: %s", data)
	}
}

func TestStateStringRoundTrip(t *testing.T) {
	for _, s := range []State{StateStarting, StateRunning, StateExited, StateCanceled, StateStale} {
		if s.String() != string(s) {
			t.Fatalf("State.String mismatch: %q vs %q", s.String(), string(s))
		}
	}
}
