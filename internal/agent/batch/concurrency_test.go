package batch_test

import (
	"context"
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
// The peak is sampled from inside the spawn function, which is the only place
// that observes actual overlap; counting spawns after the fact cannot tell
// sequential from concurrent.
//
// ledger: C1/M07#2 限并发
func TestRunnerHoldsTheCapAndStillFinishesEveryRow(t *testing.T) {
	const cap, rows = 2, 6

	var mu sync.Mutex
	live, peak := 0, 0

	// Inside-spawn barrier: the first `cap` rows block here until all `cap`
	// of them are simultaneously inside spawn, making genuine overlap a
	// precondition instead of a timing hope. On a slow or coarsely scheduled
	// CI runner (windows), a fixed time.Sleep lets row 1 finish before row 2
	// even spawns, and the peak-observed control degenerates to 1 (seen
	// 2026-09-01). Each waiting row has its own watchdog so a runner bug that
	// serializes spawns surfaces as this barrier timing out — which fails the
	// same assertion as before, with a clearer story — rather than hanging.
	arrived := make(chan struct{}, cap)
	release := make(chan struct{})
	arrivals := 0
	var once sync.Once
	spawn := func(ctx context.Context, prompt string, allowed []string, instr string) (string, error) {
		mu.Lock()
		live++
		if live > peak {
			peak = live
		}
		mu.Unlock()

		// First wave: hold each row inside spawn until `cap` rows are here
		// simultaneously — overlap by construction, not by timing. Retry
		// entries after the wave must not block (they would park while
		// holding registry slots and wedge the run), hence the non-blocking
		// arrival and the release-gate that is already open for them.
		select {
		case arrived <- struct{}{}:
			// one of the first `cap` entries
			mu.Lock()
			arrivals++
			full := arrivals >= cap
			mu.Unlock()
			if full {
				once.Do(func() { close(release) })
			}
			select {
			case <-release:
			case <-time.After(10 * time.Second):
				// not enough rows arrived: runner is effectively serial;
				// fall through, the peak assertion reports it.
			}
		default:
			// wave already released (retry entry): walk through
			once.Do(func() { close(release) })
			<-release
		}

		mu.Lock()
		live--
		mu.Unlock()
		return "done:" + prompt, nil
	}

	runner := batch.Runner{
		Spawn:         spawn,
		Manager:       newRegistryManager(t, cap),
		WaitTimeout:   10 * time.Second,
		CappedBackoff: 5 * time.Millisecond,
		CappedRetries: 50,
	}

	in := batch.Input{Prompt: "handle {{q}}"}
	for i := 0; i < rows; i++ {
		in.Rows = append(in.Rows, batch.Row{Index: i, Values: map[string]string{"q": string(rune('a' + i))}})
	}

	report, err := runner.Run(context.Background(), in)
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
	// Positive control on the fixture itself: if the peak never exceeded 1 the
	// rows ran sequentially, and "never exceeded the cap" would be true for a
	// runner with no concurrency at all.
	assert.Greater(t, gotPeak, 1,
		"rows never overlapped, so the cap was never exercised; this assertion "+
			"would hold for a runner that spawns strictly one at a time")
}
