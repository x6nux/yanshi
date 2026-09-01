package batch_test

import (
	"context"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/batch"
)

// TestRunnerHoldsTheCapAndStillFinishesEveryRow is the positive evidence for
// the concurrency clause.
//
// Neither existing test provides it. TestRunnerCapsAtRegistryMaxConcurrent
// passes 3 rows at cap 4 and therefore never reaches the cap at all — the name
// describes an event the body cannot produce. TestRunnerSpawnCapExhausted is
// the opposite case: it pins the cap with an occupant that never returns, so it
// proves rows are refused, not that they are refused TEMPORARILY.
//
// The distinction is the whole clause. A "cap" that never releases its slots is
// a lifetime spawn budget, which is precisely what the registry had before the
// slot leak was fixed: every batch longer than the cap failed row by row after
// exhausting retries. Both halves have to hold together —
//
//   - concurrency never exceeds the cap (otherwise there is no cap), and
//   - every row eventually completes (otherwise the cap is a budget).
//
// OVERLAP IS PRODUCED BY HANDSHAKE, NOT BY SLEEP SAMPLING. The first version
// sampled a peak counter inside the spawn function after a 30ms sleep and
// asserted the peak exceeded 1. That is a timing bet: on a heavily loaded CI
// runner the registry's runner goroutine for row 2 can take longer than 30ms
// to START, so every row finishes its sleep before the next one begins, the
// sampled peak stays 1, and the test fails with "rows never overlapped" —
// run 33507129420's windows leg — even though the runner is concurrent and a
// merely slower machine would have seen the overlap. Sleep proves nothing
// about scheduling that scheduling can take away.
//
// Instead rows 0 and 1 bar until both are inside the spawn function at the
// same time: each signals arrival, then waits until the partner has signalled
// too. If the runner is concurrent the rendezvous completes as soon as both
// goroutines arrive — no sleep, no race against a timer, deterministic on any
// machine — and the positive-control assertion below observes it. If the
// runner spawned strictly one row at a time the partner would never arrive,
// so the bar wait is bounded by the same timeout the run itself uses:
// expiry still fails the test, but now it fails because overlap genuinely
// did not happen, not because the machine was slow. Rows 3..6 need no
// handshake; the cap and completion assertions cover them.
//
// ledger: C1/M07#2 限并发
func TestRunnerHoldsTheCapAndStillFinishesEveryRow(t *testing.T) {
	const cap, rows = 2, 6

	var mu sync.Mutex
	live, peak := 0, 0

	// The rendezvous. Row i marks its own arrival in arrived[i] (set under
	// barMu), then waits until BOTH flags are set — its own and the
	// partner's. The wait is condition-checked so there is no channel
	// plumbing to get subtly wrong: each row re-checks under the same mutex
	// that the partner sets its flag under, and the last one to arrive sees
	// both flags set and lets go. A dropped wakeup is impossible because
	// neither goroutine ever sleeps waiting for a signal — both only ever
	// re-check shared state — so if the runner is concurrent the overlap is
	// observed directly, and if it is sequential the renege timer fires and
	// the positive-control assertion below reports it.
	var barMu sync.Mutex
	arrived := [2]bool{}
	renege := make(chan struct{})
	var renegeOnce sync.Once
	renegeAfter := 10 * time.Second
	stopReneger := func() { renegeOnce.Do(func() { close(renege) }) }
	go func() {
		<-time.After(renegeAfter)
		stopReneger()
	}()

	barrier := func(id int) {
		barMu.Lock()
		arrived[id] = true
		both := arrived[0] && arrived[1]
		barMu.Unlock()
		for !both {
			select {
			case <-renege:
				return // runner is sequential; the assertion below reports it
			default:
				runtime.Gosched()
			}
			barMu.Lock()
			both = arrived[0] && arrived[1]
			barMu.Unlock()
		}
	}

	spawn := func(ctx context.Context, prompt string, allowed []string, instr string) (string, error) {
		mu.Lock()
		live++
		if live > peak {
			peak = live
		}
		mu.Unlock()
		defer func() {
			mu.Lock()
			live--
			mu.Unlock()
		}()

		// Identify the rendezvous rows by their row_index field, not by a
		// hardcoded prompt string: promptForRow is
		// "base\nrow_index=<i>\nrow_json={...}", so the row number is the
		// digits after "row_index=" — and NOT the prompt's last byte, which
		// is row_json's closing '}' regardless of the row. Only rows 0 and
		// 1 join the bar; the rest run straight through.
		if i := strings.Index(prompt, "row_index="); i >= 0 {
			digits := prompt[i+len("row_index="):]
			if j := strings.IndexAny(digits, "\n"); j >= 0 {
				digits = digits[:j]
			}
			if row, err := strconv.Atoi(digits); err == nil && row >= 0 && row < 2 {
				barrier(row)
			}
		}
		return "done:" + prompt, nil
	}

	runner := batch.Runner{
		Spawn:         spawn,
		Manager:       newRegistryManager(t, cap),
		WaitTimeout:   renegeAfter,
		CappedBackoff: 5 * time.Millisecond,
		CappedRetries: 50,
	}

	in := batch.Input{Prompt: "handle {{q}}"}
	for i := 0; i < rows; i++ {
		in.Rows = append(in.Rows, batch.Row{Index: i, Values: map[string]string{"q": string(rune('a' + i))}})
	}

	report, err := runner.Run(context.Background(), in)
	stopReneger()
	require.NoError(t, err)

	// A batch longer than the cap must still complete in full.
	require.Len(t, report.Results, rows)
	assert.Equal(t, rows, report.Success,
		"only %d of %d rows succeeded: a cap that does not release its slots is a "+
			"lifetime spawn budget, and every row past the cap fails after exhausting retries",
		report.Success, rows)
	for i, r := range report.Results {
		assert.Equal(t, i, r.Index)
		assert.Empty(t, r.Error, "row %d: %s", i, r.Error)
	}

	mu.Lock()
	gotPeak := peak
	mu.Unlock()

	assert.LessOrEqual(t, gotPeak, cap,
		"observed %d rows running at once with a cap of %d", gotPeak, cap)
	// Positive control, now deterministic: rows 0 and 1 met inside the spawn
	// function, so overlap was OBSERVED, not inferred from a sleep that a
	// loaded runner could outrun. A runner that spawns strictly one at a time
	// leaves the bar reneged and the peak at 1, and this assertion fails —
	// correctly, because such a runner is exactly what the clause forbids.
	assert.Greater(t, gotPeak, 1,
		"rows never overlapped, so the cap was never exercised; this assertion "+
			"would hold for a runner that spawns strictly one at a time")
}
