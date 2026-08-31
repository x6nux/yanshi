package tea

import (
	"bytes"
	"errors"
	"strings"
	"sync"
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
	// quitAfterTitles appends Quit to Init's Sequence instead of waiting for a
	// keypress. A key arrives from the input reader on its own schedule, which
	// races the title Cmds: with two titles to write (RE-29) the plain-quit
	// test failed 3 runs in 5 with only the second title reaching the buffer.
	// Sequence delivers its msgs into the single eventLoop in order, so
	// quitting from inside it is the only shape here that is actually
	// deterministic.
	quitAfterTitles bool
	// panicAfterTitles panics from Update once the titles are in, replacing an
	// earlier "panic on the first KeyMsg" trigger. Same race as above from the
	// other side: the keypress could arrive before Init's title Cmds had
	// produced anything, and under -race it did — the panic exit path saw a
	// buffer with zero title bytes in 2 runs out of 3.
	panicAfterTitles bool
	// titlesDone is set once eventLoop has processed both title msgs. Because
	// eventLoop handles setWindowTitleMsg synchronously and Sequence feeds it
	// in order, observing this means both titles are already in the output —
	// which is what lets the Kill test fire at a deterministic moment instead
	// of racing the titles from View's first execution.
	titlesDone atomic.Bool
}

// titlesDoneMsg is the Sequence's final step; see titleModel.titlesDone.
type titlesDoneMsg struct{}

// titleA/titleB are the two titles Init emits. TWO, not one, because
// titlePushed's whole reason to exist is that a real yanshi turn sets the
// title repeatedly (W-E-05 sets it once per turn) while shutdown pops exactly
// one level — a single SetWindowTitle cannot tell "push once" apart from
// "push on every call" (RE-29). They differ in text so that
// assertTitlePushSetPopInOrder's search for titleA cannot accidentally match
// the second write: OSC 2 is BEL-terminated, so "…test\x07" is not a prefix
// of "…test 2\x07".
const (
	titleA = "yanshi — test"
	titleB = "yanshi — test 2"
)

func (m *titleModel) Init() Cmd {
	if !m.setTitle {
		return nil
	}
	return Sequence(
		SetWindowTitle(titleA),
		SetWindowTitle(titleB),
		func() Msg { return titlesDoneMsg{} },
	)
}

func (m *titleModel) Update(msg Msg) (Model, Cmd) {
	switch msg.(type) {
	case titlesDoneMsg:
		if m.panicAfterTitles {
			panic("titleModel: testing panic behavior")
		}
		m.titlesDone.Store(true)
		if m.quitAfterTitles {
			return m, Quit
		}
	case KeyMsg:
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

	m := &titleModel{setTitle: true, quitAfterTitles: true}
	p := NewProgram(m, WithInput(&in), WithOutput(&buf))
	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	assertTitlePushSetPopInOrder(t, out)
	// This is the one exit path that deterministically writes BOTH titles, so
	// it is where the "push exactly once despite N sets" property (RE-29) is
	// actually exercised rather than trivially satisfied.
	if !strings.Contains(out, "\x1b]2;"+titleB+"\x07") {
		t.Fatalf("expected the second SetWindowTitle (%q) to reach the terminal: %q", titleB, out)
	}
}

// TestTitleRestoredOnKill covers exit path 2/3: Kill, which cancels the
// program's internal context so eventLoop returns early and Run reaches its
// tail with killed=true (skipping the final render).
//
// RE-28 rewrote this comment; the previous version got three checkable facts
// wrong, so here they are with the check that establishes each:
//
//   - Kill does NOT call p.shutdown. Program.Kill's entire body is p.cancel()
//     (tea.go — `grep -n 'func (p \*Program) Kill' third_party/bubbletea/*.go`
//     also shows it is in tea.go, not screen.go as claimed). p.shutdown has
//     exactly two call sites in the fork, Run's tail and recoverFromPanic
//     (`grep -n 'p.shutdown(' third_party/bubbletea/tea.go`) — Kill is not
//     one of them, which the old comment's own preceding clause ("causes
//     Run() to call p.shutdown") already contradicted.
//   - The old comment also cited a line number for Run's tail shutdown; that
//     number pointed at a comment line even when written. Symbol references
//     only here, per the review checklist's F3 rule.
//   - Kill and panic are therefore NOT "shutdown from a different goroutine
//     than the one that set titlePushed". eventLoop is called synchronously
//     by Run; recoverFromPanic is a defer inside Run; Run's tail is Run. All
//     three exit paths read titlePushed on the very goroutine that wrote it,
//     so nothing in THIS file requires an atomic — a plain bool passes all
//     four tests here under -race (verified).
//
// atomic.Bool is still the right type, for a reason none of these tests
// exercised until RE-28 added one: Program.SetWindowTitle is exported and
// documents no goroutine restriction, so callers may drive it concurrently.
// TestSetWindowTitleFromConcurrentGoroutines below is what actually holds
// that property.
//
// What this test does still prove: the killed=true path restores the title.
// SIGTERM/SIGINT need no test of their own — their handler just sends
// QuitMsg/InterruptMsg into eventLoop, the same shape as a keypress, so the
// plain-Quit test above already covers them.
func TestTitleRestoredOnKill(t *testing.T) {
	var buf bytes.Buffer
	var in bytes.Buffer

	m := &titleModel{setTitle: true}
	p := NewProgram(m, WithInput(&in), WithOutput(&buf))
	go func() {
		for {
			time.Sleep(time.Millisecond)
			if m.titlesDone.Load() {
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

	m := &titleModel{setTitle: true, panicAfterTitles: true}
	p := NewProgram(m, WithInput(&in), WithOutput(&buf))

	// Run() itself recovers the panic (see recoverFromPanic) and returns a
	// model/err pair rather than propagating — mirroring TestTeaPanic's use
	// of the same mechanism two exit paths up in this file.
	if _, err := p.Run(); err == nil {
		t.Fatal("expected an error from a panicking Update, got nil")
	}

	assertTitlePushSetPopInOrder(t, buf.String())
}

// TestTitleSetBeforeRunIsAlsoRestored covers the fork's one remaining
// unpaired title branch (RE-33): Program.SetWindowTitle called BEFORE Run
// takes the p.renderer == nil path and only records startupTitle, which Run
// used to hand straight to the renderer — setting the title without pushing
// the stack, so shutdown found titlePushed false and popped nothing and the
// title outlived the program.
//
// yanshi itself cannot reach this (it only uses the tea.SetWindowTitle Cmd,
// which by construction runs inside eventLoop with a live renderer), so this
// is the fork's invariant being closed on its own terms rather than a yanshi
// regression test.
func TestTitleSetBeforeRunIsAlsoRestored(t *testing.T) {
	var buf bytes.Buffer
	var in bytes.Buffer

	m := &titleModel{} // setTitle=false: the title comes from before Run
	p := NewProgram(m, WithInput(&in), WithOutput(&buf))
	p.SetWindowTitle(titleA) // renderer is still nil here → startupTitle

	go func() {
		for m.executed.Load() == nil {
			time.Sleep(time.Millisecond)
		}
		p.Quit()
	}()

	if _, err := p.Run(); err != nil {
		t.Fatal(err)
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

// assertTitlePushSetPopInOrder checks the two properties every exit path must
// hold: the push/set/pop ordering, and that the push happened EXACTLY once no
// matter how many times the title was set.
//
// The count is not redundant with the ordering (RE-29): strings.Index finds
// the FIRST occurrence, so with N pushes the first one is still ahead of the
// title and the ordering assertion passes unchanged. A mutation replacing
// screen.go's `if !p.titlePushed.Swap(true)` with an unconditional
// `p.titlePushed.Store(true); p.renderer.pushWindowTitle()` left this whole
// file green before the count existed — and that mutation is exactly the bug
// titlePushed's doc comment warns about, since shutdown pops one level while
// the terminal's title stack grows by one per turn.
// syncBuffer is a mutex-guarded output sink. standardRenderer.execute writes
// straight to the output with no lock of its own, so a plain bytes.Buffer
// would report a data race on the BUFFER as soon as two goroutines set the
// title concurrently — drowning out the race the test below is actually
// looking for. Locking here removes that one, leaving titlePushed as the only
// unsynchronized state in the picture (the "attribution" requirement: an
// assertion that cannot say which mechanism failed proves nothing).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestSetWindowTitleFromConcurrentGoroutines is what makes titlePushed's
// atomic.Bool load-bearing (RE-28). Program.SetWindowTitle is exported and
// promises no goroutine affinity, so a caller may drive it from anywhere;
// with a plain bool, the `if !p.titlePushed { p.titlePushed = true }`
// read-modify-write here is an unsynchronized write racing every other
// goroutine's read.
//
// The deterministic half (push count) and the -race half both matter: the
// count can go above one only when two goroutines interleave, which is
// timing-dependent, whereas the race detector flags the unsynchronized access
// whether or not the interleaving loses. CI runs `go test -race` as a hard
// gate, so the reliable signal is there.
//
// This test drives SetWindowTitle directly rather than through the Cmd path
// because the Cmd path is by construction single-goroutine (eventLoop); the
// exported method is the surface with no such guarantee.
func TestSetWindowTitleFromConcurrentGoroutines(t *testing.T) {
	var out syncBuffer
	var in bytes.Buffer

	m := &titleModel{} // setTitle=false: this test sets the title itself
	p := NewProgram(m, WithInput(&in), WithOutput(&out))

	go func() {
		// Wait for View to run: the atomic store in View happens after Run
		// assigned p.renderer, so loading it here establishes the
		// happens-before edge that makes reading p.renderer below legal.
		for m.executed.Load() == nil {
			time.Sleep(time.Millisecond)
		}
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 25; j++ {
					p.SetWindowTitle(titleA)
				}
			}()
		}
		wg.Wait()
		p.Quit()
	}()

	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}

	assertTitlePushSetPopInOrder(t, out.String())
}

func assertTitlePushSetPopInOrder(t *testing.T, out string) {
	t.Helper()
	const title = "\x1b]2;" + titleA + "\x07"

	if n := strings.Count(out, pushSeq); n != 1 {
		t.Fatalf("title-stack push %q appeared %d times, want exactly 1 (shutdown pops only one level): %q", pushSeq, n, out)
	}
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
