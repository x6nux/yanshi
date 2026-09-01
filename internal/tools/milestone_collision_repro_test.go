package tools

import (
	"strconv"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/secrets"
	"github.com/x6nux/yanshi/internal/store"
)

// Repro for the windows CI failure of
// TestMilestoneSet_RepeatedIdenticalTextGetsDistinctAddresses:
// "milestone_set: the note was not recorded (duplicate of an existing entry)".
//
// Hypotheses under test:
//  1. the redactor mangles the text so the key's text half no longer matches
//     the stored content, or a nanosecond-timestamp collision makes two
//     consecutive calls derive the SAME key;
//  2. AppendMessages itself mis-handles consecutive single-row appends.
func TestMilestoneKeyCollisionRepro(t *testing.T) {
	s := newHistoryStore(t)
	// Match production: bootstrap always injects a redactor.
	s.SetRedactor(secrets.NewRedactor())
	sid, err := s.CreateSession("repro")
	if err != nil {
		t.Fatal(err)
	}
	const label = "ran the test suite"
	keys := map[string]bool{}
	for i := 0; i < 200; i++ {
		key := "milestone:" + strconv.FormatInt(time.Now().UnixNano(), 10) + ":" + label
		if keys[key] {
			t.Errorf("i=%d: dedup key collision within test: %s", i, key)
		}
		keys[key] = true
		inserted, _, err := s.AppendMessages(sid, []store.Message{
			{Role: RoleMilestone, Content: label, DedupKey: key},
		})
		if err != nil {
			t.Fatalf("i=%d: %v", i, err)
		}
		if inserted != 1 {
			t.Fatalf("i=%d: inserted=%d; dedup key already present: %s", i, inserted, key)
		}
	}
}
