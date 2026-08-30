package tea

import (
	"bytes"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// titleModel is a minimal model for exercising Program.SetWindowTitle /
// titlePushed across all three exit paths (W-E-05). setTitle controls
// whether Init() emits a SetWindowTitle Cmd at all, so the "never touched
// the title" control case (mirroring TERM=dumb, where the TUI layer never
// emits the Cmd) can be told apart from "touched it, then restored it".
type titleModel struct {
	executed atomic.Value
	setTitle bool
	doPanic  atomic.Bool
}

func (m *titleModel) Init() Cmd {
	if !m.setTitle {
		return nil
	}
	return SetWindowTitle("yanshi — test")
}

func (m *titleModel) Update(msg Msg) (Model, Cmd) {
	switch msg.(type) {
	case KeyMsg:
		if m.doPanic.Load() {
			panic("titleModel: testing panic behavior")
		}
		return m, Quit
	}
	return m, nil
}

func (m *titleModel) View() string {
	m.executed.Store(true)
	return "ok\n"
}

// pushSeq/popSeq are the raw XTWINOPS bytes pushWindowTitle/popWindowTitle
// write — see standard_renderer.go. Asserting on the literal bytes (rather
// than calling ansi.WindowOp again) keeps this test independent of the
// production code it's checking, per the "attacker/oracle must not share a
// parser" shape documented in CLAUDE.md's guard section.
const (
	pushSeq = "\x1b[22;2t"
	popSeq  = "\x1b[23;2t"
)

// TestTitleRestoredOnNormalQuit covers exit path 1/3: a plain Quit (the
// user presses a key that returns tea.Quit from Update, same as pressing
// 'q' in a real session). buf must show the push, then the OSC 2 title, then
// the pop — in that order — proving shutdown() restores the terminal's
// original title rather than leaving the program's title stuck after exit.
func TestTitleRestoredOnNormalQuit(t *testing.T) {
	var buf bytes.Buffer
	var in bytes.Buffer
	in.Write([]byte("q"))

	m := &titleModel{setTitle: true}
	p := NewProgram(m, WithInput(&in), WithOutput(&buf))
	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}

	assertTitlePushSetPopInOrder(t, buf.String())
}

// TestTitleRestoredOnKill covers exit path 2/3: Kill, which cancels the
// program's internal context from a goroutine other than eventLoop's and
// causes Run() to call p.shutdown(killed=true) — distinct from the plain
// Quit test above, where Run() reaches the SAME p.shutdown(...) call at its
// own tail (tea.go:755) after eventLoop returns normally. That single call
// site is also what SIGTERM/SIGINT go through in this fork (their handler
// just sends QuitMsg/InterruptMsg into eventLoop, same as a 'q' keypress —
// there is no separate signal-specific shutdown path), so the Quit test
// already exercises that shape; Kill and panic (below) are the two
// genuinely different call sites (screen.go's Kill() and tea.go's
// recoverFromPanic both call p.shutdown() directly, from a different
// goroutine than the one that set titlePushed) — which is what proves
// titlePushed's atomic.Bool, not a plain bool, is load-bearing here.
func TestTitleRestoredOnKill(t *testing.T) {
	var buf bytes.Buffer
	var in bytes.Buffer

	m := &titleModel{setTitle: true}
	p := NewProgram(m, WithInput(&in), WithOutput(&buf))
	go func() {
		for {
			time.Sleep(time.Millisecond)
			if m.executed.Load() != nil {
				p.Kill()
				return
			}
		}
	}()

	_, err := p.Run()
	if !errors.Is(err, ErrProgramKilled) {
		t.Fatalf("expected %v, got %v", ErrProgramKilled, err)
	}

	assertTitlePushSetPopInOrder(t, buf.String())
}

// TestTitleRestoredOnPanic covers exit path 3/3: a panic inside Update.
// recoverFromPanic calls p.shutdown(true) directly (tea.go) — this test
// proves that call restores the title exactly as the other two exit paths
// do, closing the "verified across three exit paths" requirement.
func TestTitleRestoredOnPanic(t *testing.T) {
	var buf bytes.Buffer
	var in bytes.Buffer
	in.Write([]byte("q"))

	m := &titleModel{setTitle: true}
	m.doPanic.Store(true)
	p := NewProgram(m, WithInput(&in), WithOutput(&buf))

	// Run() itself recovers the panic (see recoverFromPanic) and returns a
	// model/err pair rather than propagating — mirroring TestTeaPanic's use
	// of the same mechanism two exit paths up in this file.
	if _, err := p.Run(); err == nil {
		t.Fatal("expected an error from a panicking Update, got nil")
	}

	assertTitlePushSetPopInOrder(t, buf.String())
}

// TestTitleUntouchedLeavesNoStackNoise is the negative control: a model that
// never calls SetWindowTitle (the TERM=dumb shape at the TUI layer, which
// gates the Cmd at the call site rather than relying on this fork to no-op
// it) must produce zero title-stack bytes. Without this, a regression that
// made shutdown() push+pop unconditionally would pass the three tests above
// and still leak escape noise into every session that never touches the
// title feature at all.
func TestTitleUntouchedLeavesNoStackNoise(t *testing.T) {
	var buf bytes.Buffer
	var in bytes.Buffer
	in.Write([]byte("q"))

	m := &titleModel{setTitle: false}
	p := NewProgram(m, WithInput(&in), WithOutput(&buf))
	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	if strings.Contains(out, pushSeq) || strings.Contains(out, popSeq) {
		t.Fatalf("expected no title-stack sequences when SetWindowTitle was never called, got: %q", out)
	}
}

func assertTitlePushSetPopInOrder(t *testing.T, out string) {
	t.Helper()
	const title = "\x1b]2;yanshi — test\x07"

	pushAt := strings.Index(out, pushSeq)
	titleAt := strings.Index(out, title)
	popAt := strings.Index(out, popSeq)

	if pushAt < 0 {
		t.Fatalf("missing title-stack push %q in output: %q", pushSeq, out)
	}
	if titleAt < 0 {
		t.Fatalf("missing OSC 2 title %q in output: %q", title, out)
	}
	if popAt < 0 {
		t.Fatalf("missing title-stack pop %q in output: %q", popSeq, out)
	}
	if !(pushAt < titleAt && titleAt < popAt) {
		t.Fatalf("expected push < title < pop, got offsets push=%d title=%d pop=%d in: %q", pushAt, titleAt, popAt, out)
	}
}
