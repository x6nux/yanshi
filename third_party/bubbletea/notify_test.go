package tea

import (
	"bytes"
	"strings"
	"testing"
)

// notifyModel is a minimal model for exercising Program.Notify / Program.Bell
// (W-E-04). Init emits cmd; the resulting notifyMsg/bellMsg is handled
// synchronously by the event loop (p.Notify/p.Bell write to the renderer
// before model.Update is called with that same message — see tea.go's
// eventLoop), so quitting on the very next Update call is enough to prove
// the escape/BEL byte already landed in the output buffer.
type notifyModel struct {
	cmd Cmd
}

func (m *notifyModel) Init() Cmd               { return m.cmd }
func (m *notifyModel) Update(Msg) (Model, Cmd) { return m, Quit }
func (m *notifyModel) View() string            { return "ok\n" }

// TestNotifyEmitsOSC9 guards W-E-04's capable-terminal tier: tea.Notify must
// write the hand-rolled OSC 9 sequence ("\x1b]9;" + text + BEL), not
// termenv's OSC 777 Notify (a narrower-support convention the task
// explicitly rejected — see commands.go's Notify doc comment).
func TestNotifyEmitsOSC9(t *testing.T) {
	var buf bytes.Buffer
	var in bytes.Buffer

	m := &notifyModel{cmd: Notify("build finished")}
	p := NewProgram(m, WithInput(&in), WithOutput(&buf))
	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}

	const want = "\x1b]9;build finished\a"
	if got := buf.String(); !strings.Contains(got, want) {
		t.Fatalf("expected OSC 9 sequence %q in output, got: %q", want, got)
	}
}

// TestBellEmitsPlainBEL guards W-E-04's unsupported-terminal fallback tier:
// tea.Bell must write a bare BEL byte and nothing that could be mistaken for
// an OSC 9 escape (no "\x1b]9;" prefix), since on a terminal that can't be
// trusted to parse OSC 9 that prefix is exactly what would print as garbage.
func TestBellEmitsPlainBEL(t *testing.T) {
	var buf bytes.Buffer
	var in bytes.Buffer

	m := &notifyModel{cmd: Bell()}
	p := NewProgram(m, WithInput(&in), WithOutput(&buf))
	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}

	got := buf.String()
	if !strings.Contains(got, "\a") {
		t.Fatalf("expected a BEL byte in output, got: %q", got)
	}
	if strings.Contains(got, "\x1b]9;") {
		t.Fatalf("Bell must not emit an OSC 9 prefix, got: %q", got)
	}
}
