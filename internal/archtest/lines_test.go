// Package archtest — architecture governance tests for GOV2 pure code line gate.
//
// This file enforces a maximum of 1000 pure code lines (non-comment, non-blank)
// per .go file. Grandfather exceptions are tracked in lineExceptions and must
// be removed when the corresponding refactoring task is completed.
package archtest

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const pureLineLimit = 1000

// lineExceptions: grandfather-ed overlimit files (tracked, must be removed).
// Keys are ABSOLUTE paths (use abs("internal/…")), matching what goFiles yields.
// Initial state = 2 files to be split in Tasks 4/6 (Task 5 completed: agent.go split).
// Task 6 completed: model.go split to state.go.
//
// Only removal, never addition. An entry is dead — and fails the test — both
// when its file is back under the limit AND when the file is no longer scanned
// at all (renamed, split, deleted, moved outside internal//cmd/).
var lineExceptions = map[string]string{}

// TestPureCodeLineGate verifies that no non-test .go file in the internal/
// or cmd/ directories exceeds 1000 pure code lines. Grandfathered files (in
// lineExceptions) are logged but not failed — unless they now fall below
// the limit, or no longer exist, either of which is a dead entry that must be
// removed.
func TestPureCodeLineGate(t *testing.T) {
	root := moduleRoot(t)
	files := goFiles(t,
		filepath.Join(root, "internal"),
		filepath.Join(root, "cmd"),
	)
	var failed []string
	var approaching []string
	scanned := make(map[string]bool, len(files))
	for _, f := range files {
		scanned[f] = true
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
	// Dead-entry check, "subject has vanished" half. The loop above can only
	// judge an exemption whose file it actually visits, so an entry naming a
	// deleted or renamed file was structurally unfalsifiable: the condition
	// "still over the limit" is vacuously satisfied by a file that is never
	// counted. Such an entry is a standing pre-authorisation — recreate the
	// path and it is grandfathered on arrival, having never been reviewed.
	var vanished []string
	for f := range lineExceptions {
		if !scanned[f] {
			vanished = append(vanished, short(f, root))
		}
	}
	sort.Strings(vanished)
	if len(vanished) > 0 {
		t.Errorf("GOV2: %d lineExceptions entr(ies) name a file that is no longer scanned "+
			"under internal/ or cmd/ — the exemption pre-authorises whatever comes back "+
			"under that path and must be DELETED:\n  %s", len(vanished), strings.Join(vanished, "\n  "))
	}

	if len(failed) > 0 {
		t.Fatalf("Pure code line limit (%d) exceeded:\n  %s", pureLineLimit, strings.Join(failed, "\n  "))
	}
	for _, a := range approaching {
		t.Logf("approaching: %s", a)
	}
}
