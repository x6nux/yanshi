package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/muesli/termenv"
)

// getenvFrom builds a getenv func from a map, for exercising DetectCapability
// without touching process-global environment variables.
func getenvFrom(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestDetectCapability_NoColor(t *testing.T) {
	cap := DetectCapability(getenvFrom(map[string]string{"NO_COLOR": "1"}))
	if cap.Profile != termenv.Ascii {
		t.Errorf("NO_COLOR=1: got profile %s, want Ascii", cap.Profile.Name())
	}
	if !cap.AltScreen {
		t.Error("NO_COLOR=1: AltScreen should stay true (the terminal itself is not assumed dumb)")
	}
}

func TestDetectCapability_TermDumb(t *testing.T) {
	cap := DetectCapability(getenvFrom(map[string]string{"TERM": "dumb"}))
	if cap.Profile != termenv.Ascii {
		t.Errorf("TERM=dumb: got profile %s, want Ascii", cap.Profile.Name())
	}
	if cap.AltScreen {
		t.Error("TERM=dumb: AltScreen should be false")
	}
}

func TestDetectCapability_TermDumbWinsOverColorterm(t *testing.T) {
	// A stale/inherited COLORTERM=truecolor alongside TERM=dumb (some CI
	// images export both) must not re-enable color — see DetectCapability's
	// doc comment, priority rule 1.
	cap := DetectCapability(getenvFrom(map[string]string{"TERM": "dumb", "COLORTERM": "truecolor"}))
	if cap.Profile != termenv.Ascii {
		t.Errorf("TERM=dumb+COLORTERM=truecolor: got profile %s, want Ascii (dumb must win)", cap.Profile.Name())
	}
	if cap.AltScreen {
		t.Error("TERM=dumb+COLORTERM=truecolor: AltScreen should be false (dumb must win)")
	}
}

func TestDetectCapability_ColortermTruecolor(t *testing.T) {
	for _, v := range []string{"truecolor", "24bit", "TrueColor", "24BIT"} {
		cap := DetectCapability(getenvFrom(map[string]string{"COLORTERM": v}))
		if cap.Profile != termenv.TrueColor {
			t.Errorf("COLORTERM=%s: got profile %s, want TrueColor", v, cap.Profile.Name())
		}
		if !cap.AltScreen {
			t.Errorf("COLORTERM=%s: AltScreen should be true", v)
		}
	}
}

func TestDetectCapability_Fallback256(t *testing.T) {
	cap := DetectCapability(getenvFrom(map[string]string{"TERM": "xterm-256color"}))
	if cap.Profile != termenv.ANSI256 {
		t.Errorf("TERM=xterm-256color: got profile %s, want ANSI256", cap.Profile.Name())
	}
}

func TestDetectCapability_Fallback16(t *testing.T) {
	cap := DetectCapability(getenvFrom(map[string]string{"TERM": "xterm"}))
	if cap.Profile != termenv.ANSI {
		t.Errorf("TERM=xterm: got profile %s, want ANSI (16-color)", cap.Profile.Name())
	}
}

func TestDetectCapability_NilGetenv(t *testing.T) {
	cap := DetectCapability(nil)
	if cap.Profile != termenv.Ascii {
		t.Errorf("nil getenv: got profile %s, want Ascii (empty environment)", cap.Profile.Name())
	}
	if !cap.AltScreen {
		t.Error("nil getenv: AltScreen should default to true (no TERM=dumb signal)")
	}
}

func TestTermCapability_String(t *testing.T) {
	cap := TermCapability{Profile: termenv.ANSI256, AltScreen: true}
	got := cap.String()
	if !strings.Contains(got, "ANSI256") || !strings.Contains(got, "true") {
		t.Errorf("String() = %q, want it to mention the profile name and alt_screen value", got)
	}
}

// TestRunDoctor_IncludesTerminalCapabilityCheck covers acceptance criterion 5
// (W-E-01): "探测结果可被 -h 或 doctor 显示".
func TestRunDoctor_IncludesTerminalCapabilityCheck(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTempConfig(t, `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "`+strings.ReplaceAll(dir, "\\", "/")+`/test.db"
token: "test-token"
`)
	rep := RunDoctor(context.Background(), DoctorOptions{ConfigPath: cfgPath, Root: t.TempDir()})
	c := findCheck(t, rep, "terminal-capability")
	if c.Status != StatusOK {
		t.Errorf("terminal-capability status = %s, want ok", c.Status)
	}
	if !strings.Contains(c.Message, "color=") || !strings.Contains(c.Message, "alt_screen=") {
		t.Errorf("terminal-capability message = %q, want it to mention color and alt_screen", c.Message)
	}
}
