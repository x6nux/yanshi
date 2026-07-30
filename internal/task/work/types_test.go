package work

import "testing"

func TestDomainRules(t *testing.T) {
	if err := StatusCompleted.CanTransitionTo(StatusRunning); err == nil {
		t.Fatal("terminal status must reject transitions")
	}
	if err := StatusPending.CanTransitionTo(StatusRunning); err != nil {
		t.Fatal(err)
	}
	if got := (Checklist{Items: []ChecklistItem{
		{ID: 1, Status: ChecklistDone},
		{ID: 2, Status: ChecklistPending},
		{ID: 3, Status: ChecklistInProgress},
		{ID: 4, Status: ChecklistDone},
	}}).CompletionPct(); got != 50 {
		t.Fatalf("CompletionPct=%d want 50", got)
	}
	if ClassificationFromExitCode(0) != "pass" || ClassificationFromExitCode(1) != "fail" || ClassificationFromExitCode(-1) != "error" {
		t.Fatal("unexpected evidence classification")
	}
}
