package goalloop

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// Intent classifies the user's input text.
type Intent int

// Intent classifications for user input text. Determines whether the goal
// loop should be triggered.
const (
	IntentSimpleQA Intent = iota // a question looking for a quick answer
	IntentGoalImpl               // a feature-implementation or bug-fix goal
	IntentOther                  // neither of the above
)

// Classifier determines the Intent of a user message.
// RuleClassifier is a heuristic placeholder; an LLM-backed classifier can
// satisfy this interface for smarter classification.
type Classifier interface {
	Classify(ctx context.Context, text string) (Intent, error)
}

// RuleClassifier uses simple keyword heuristics to classify intent.
//
//nolint:revive // exported method name matches interface
type RuleClassifier struct{}

// Classify returns the Intent for the given text.
// Heuristics (checked in order):
//  1. Contains goal-implementation keywords → IntentGoalImpl
//  2. Looks like a question → IntentSimpleQA
//  3. Otherwise → IntentOther
func (RuleClassifier) Classify(_ context.Context, text string) (Intent, error) {
	lower := strings.ToLower(text)

	goalKeywords := []string{"实现", "implement", "add feature", "build", "fix bug"}
	for _, kw := range goalKeywords {
		if strings.Contains(lower, kw) {
			return IntentGoalImpl, nil
		}
	}

	// Question heuristics: contains "?" or starts with a question word.
	trimmed := strings.TrimSpace(lower)
	if strings.Contains(text, "?") {
		return IntentSimpleQA, nil
	}
	questionStarts := []string{"what", "why", "how"}
	for _, qw := range questionStarts {
		if strings.HasPrefix(trimmed, qw+" ") || trimmed == qw {
			return IntentSimpleQA, nil
		}
	}

	return IntentOther, nil
}

// Confirmer asks the user for a yes/no confirmation.
type Confirmer interface {
	Confirm(ctx context.Context, prompt string) (bool, error)
}

// AutoConfirmer returns a pre-set result without prompting. Useful for tests
// and non-interactive mode.
type AutoConfirmer struct {
	Result bool
}

// Confirm implements Confirmer by returning the configured Result.
func (a AutoConfirmer) Confirm(_ context.Context, _ string) (bool, error) {
	return a.Result, nil
}

// OSConfirmer reads a y/n line from stdin. It is best-effort: any read error
// or unrecognised answer defaults to false.
type OSConfirmer struct {
	in io.Reader // defaults to os.Stdin when nil
}

// Confirm prints the prompt to stderr and reads a line from stdin.
func (c OSConfirmer) Confirm(_ context.Context, prompt string) (bool, error) {
	r := c.in
	if r == nil {
		r = os.Stdin
	}
	fmt.Fprint(os.Stderr, prompt+" [y/N]: ")
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return false, err
		}
		return false, nil
	}
	resp := strings.TrimSpace(strings.ToLower(scanner.Text()))
	return resp == "y" || resp == "yes", nil
}

// Trigger decides whether to enter the self-driven Goal Loop for a given
// user message.
type Trigger struct {
	clf  Classifier
	conf Confirmer
	tier Tierer // defaults to RuleTierer
}

// NewTrigger creates a Trigger with the given classifier and confirmer.
// The tier selector defaults to RuleTierer; callers may override it via
// WithTierer.
func NewTrigger(clf Classifier, conf Confirmer) *Trigger {
	return &Trigger{clf: clf, conf: conf, tier: RuleTierer{}}
}

// WithTierer sets the Tierer used for auto-tier selection in DecideTier and
// returns the same Trigger (it mutates the receiver, it does not copy). It is
// safe to pass nil to restore the RuleTierer default.
func (t *Trigger) WithTierer(tier Tierer) *Trigger {
	if tier == nil {
		tier = RuleTierer{}
	}
	t.tier = tier
	return t
}

// Decide returns whether the Goal Loop should be entered and a human-readable
// reason string:
//   - "/goal ..." prefix → enter true, reason "explicit"
//   - Classified as goal-implementation → Confirm; reason "auto+confirm"
//   - Otherwise → enter false, reason "not a goal"
func (t *Trigger) Decide(ctx context.Context, text string) (bool, string) {
	if strings.HasPrefix(text, "/goal ") {
		return true, "explicit"
	}

	intent, err := t.clf.Classify(ctx, text)
	if err != nil || intent != IntentGoalImpl {
		return false, "not a goal"
	}

	confirmed, err := t.conf.Confirm(ctx, "Detected a feature-implementation goal. Enter self-driven mode?")
	if err != nil {
		return false, "auto+confirm"
	}
	return confirmed, "auto+confirm"
}

// DecideTier is like Decide but also returns the selected Tier.
//   - "/dev t2 <task>" → (true, TierDesigned, "explicit")   (forced tier)
//   - "/dev <task>"    → (true, <auto>, "explicit")          (auto-tiered)
//   - goal-impl intent → (confirm; on yes) (true, <auto>, "auto+confirm")
//   - otherwise        → (false, TierStandard, <reason>)
func (t *Trigger) DecideTier(ctx context.Context, text string) (bool, Tier, string) {
	if t.tier == nil {
		t.tier = RuleTierer{} // defend zero-value Trigger{}
	}
	if rest, ok := strings.CutPrefix(text, "/dev "); ok {
		rest = strings.TrimSpace(rest)
		if len(rest) >= 2 && rest[0] == 't' {
			if tier, ok := ParseForcedTier(rest[:2]); ok {
				return true, tier, "explicit"
			}
		}
		tier, _ := t.tier.Tier(ctx, rest)
		return true, tier, "explicit"
	}
	enter, reason := t.Decide(ctx, text)
	if !enter {
		return false, TierStandard, reason
	}
	tier, _ := t.tier.Tier(ctx, text)
	return true, tier, reason
}

// ParseForcedTier maps a short tier token ("t0".."t4") to its Tier. It returns
// ok=false for anything else. Exported so the CLI can share the mapping.
func ParseForcedTier(s string) (Tier, bool) {
	switch s {
	case "t0":
		return TierQuickFix, true
	case "t1":
		return TierStandard, true
	case "t2":
		return TierDesigned, true
	case "t3":
		return TierTeam, true
	case "t4":
		return TierAutonomous, true
	}
	return 0, false
}
