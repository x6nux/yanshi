// Package archtest — architecture governance tests for GOV2 pure code line gate.
//
// This file enforces a maximum of 1000 pure code lines (non-comment, non-blank)
// per .go file. Grandfather exceptions are tracked in lineExceptions and must
// be removed when the corresponding refactoring task is completed.
package archtest

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

const pureLineLimit = 1000

// lineExceptions: grandfather-ed overlimit files (tracked, must be removed).
// Initial state = 2 files to be split in Tasks 4/6 (Task 5 completed: agent.go split).
// Task 6 completed: model.go split to state.go.
var lineExceptions = map[string]string{}

// TestPureCodeLineGate verifies that no non-test .go file in the internal/
// or cmd/ directories exceeds 1000 pure code lines. Grandfathered files (in
// lineExceptions) are logged but not failed — unless they now fall below
// the limit, indicating a dead entry that must be removed.
func TestPureCodeLineGate(t *testing.T) {
	root := moduleRoot(t)
	files := goFiles(t,
		filepath.Join(root, "internal"),
		filepath.Join(root, "cmd"),
	)
	var failed []string
	var approaching []string
	for _, f := range files {
		n := pureCodeLines(t, f)
		if reason, ok := lineExceptions[f]; ok {
			// Grandfathered: log current count but don't fail.
			t.Logf("grandfathered %s: %d pure (limit %d) — %s", short(f, root), n, pureLineLimit, reason)
			if n <= pureLineLimit {
				t.Errorf("grandfathered %s is now %d <= %d — remove from lineExceptions (no dead entries)", short(f, root), n, pureLineLimit)
			}
			continue
		}
		if n > pureLineLimit {
			failed = append(failed, fmt.Sprintf("%s: %d pure code lines (limit %d) — split required", short(f, root), n, pureLineLimit))
		} else if n > 900 {
			approaching = append(approaching, fmt.Sprintf("%s: %d pure (approaching limit)", short(f, root), n))
		}
	}
	if len(failed) > 0 {
		t.Fatalf("Pure code line limit (%d) exceeded:\n  %s", pureLineLimit, strings.Join(failed, "\n  "))
	}
	for _, a := range approaching {
		t.Logf("approaching: %s", a)
	}
}
