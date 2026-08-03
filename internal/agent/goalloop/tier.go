package goalloop

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// Tier selects a development-workflow difficulty level.
type Tier int

// Development-workflow difficulty levels, ordered from quickest (T0) to most
// autonomous (T4). Higher tiers engage more evaluators and judgement criteria.
const (
	TierQuickFix   Tier = iota // T0
	TierStandard               // T1
	TierDesigned               // T2
	TierTeam                   // T3
	TierAutonomous             // T4
)

// String returns the canonical lowercase name for the tier.
func (t Tier) String() string {
	switch t {
	case TierQuickFix:
		return "quick-fix"
	case TierStandard:
		return "standard"
	case TierDesigned:
		return "designed"
	case TierTeam:
		return "team"
	case TierAutonomous:
		return "autonomous"
	}
	return "unknown"
}

// Tierer picks a Tier for a task.
type Tierer interface {
	Tier(ctx context.Context, task string) (Tier, error)
}

// RuleTierer uses keyword heuristics to classify a task into a Tier.
// Keywords are checked most-specific (highest tier) first, so a task
// matching multiple tiers resolves to the highest one.
type RuleTierer struct{}

// Tier classifies the task text into a development-workflow Tier.
func (RuleTierer) Tier(_ context.Context, task string) (Tier, error) {
	l := strings.ToLower(task)
	switch {
	case anyKeyword(l, "project", "epic", "autonomous", "build a system", "platform"):
		return TierAutonomous, nil
	case anyKeyword(l, "component", "parallel", "team", "multi-service"):
		return TierTeam, nil
	case anyKeyword(l, "design", "refactor", "api ", "multiple files", "module"):
		return TierDesigned, nil
	case anyKeyword(l, "typo", "config", "tweak", "rename a ", "bump "):
		return TierQuickFix, nil
	case anyKeyword(l, "bug", "test", "fix"):
		return TierStandard, nil
	}
	return TierStandard, nil // safe default
}

// anyKeyword reports whether haystack contains any of the needles.
func anyKeyword(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// LLMTierer asks the model to classify the task; falls back to Fallback
// (usually a RuleTierer) when the model is unavailable or unparseable.
type LLMTierer struct {
	Model    model.BaseChatModel
	Fallback Tierer
	// Sink accumulates each Tier call's token usage (G02). Nil = no-op.
	Sink *UsageSink
}

// tierKeywords maps each Tier to the lowercase keyword the model is asked to
// reply with. None of the keywords is a substring of another, so map iteration
// order does not affect which tier matches.
var tierKeywords = map[Tier]string{
	TierQuickFix:   "quick-fix",
	TierStandard:   "standard",
	TierDesigned:   "designed",
	TierTeam:       "team",
	TierAutonomous: "autonomous",
}

// Tier asks the model to classify the task into a single tier keyword. If the
// model errors or its reply does not contain a known keyword, it falls back to
// Fallback; when no fallback is configured it returns the safe default
// TierStandard.
func (l LLMTierer) Tier(ctx context.Context, task string) (Tier, error) {
	prompt := "Classify the following task into exactly one tier by replying with " +
		"the single word: quick-fix, standard, designed, team, or autonomous.\n\nTask: " + task
	out, err := l.Model.Generate(ctx, []*schema.Message{
		{Role: schema.User, Content: prompt},
	})
	addUsage(l.Sink, out) // G02: count this call's tokens (nil-safe: nil out → no-op)
	if err == nil && out != nil {
		low := strings.ToLower(strings.TrimSpace(out.Content))
		for t, kw := range tierKeywords {
			if strings.Contains(low, kw) {
				return t, nil
			}
		}
	}
	if l.Fallback != nil {
		return l.Fallback.Tier(ctx, task)
	}
	return TierStandard, nil
}

// SkillName returns the SKILL.md registry name for the tier. Each maps to a
// directory under skills/ (dev-quick-fix … dev-autonomous-project). An unknown
// tier falls back to the standard skill (safe default, mirrors RuleTierer).
func (t Tier) SkillName() string {
	switch t {
	case TierQuickFix:
		return "dev-quick-fix"
	case TierStandard:
		return "dev-standard-feature"
	case TierDesigned:
		return "dev-designed-feature"
	case TierTeam:
		return "dev-team-feature"
	case TierAutonomous:
		return "dev-autonomous-project"
	}
	return "dev-standard-feature"
}

// Path returns the execution path the tier uses:
//   - "lightweight" (T0–T2): a single orchestrator+skill turn that follows the
//     tier's SKILL.md via the already-wired fs/shell tools.
//   - "loop" (T3–T4): the full plan-implement-evaluate-judge goal loop with
//     workers and multi-evaluator judging.
//
// The lightweight tiers map to skills whose workflow a single agent turn can
// perform (quick fix, TDD standard, designed-feature spec→plan); the loop tiers
// map to skills that explicitly use the Goal Loop's Lead/Worker/Integrator.
func (t Tier) Path() string {
	if t <= TierDesigned {
		return "lightweight"
	}
	return "loop"
}

// EvaluatorsForTier returns the evaluator set appropriate for the tier:
//   - T0: TestEvaluator only (a quick fix must at least pass its tests).
//   - T1: + IntentEvaluator (did the standard feature meet the goal intent?).
//   - T2–T4: + QualityEvaluator (the designed/team/autonomous bar includes code
//     quality).
//
// A nil model yields only TestEvaluator (the non-LLM one), so callers without a
// chat model still get a meaningful (if shallow) evaluation. sink, when
// non-nil, is stamped onto each LLM-backed evaluator so every Generate call in
// the tier's evaluator set flows into the one shared budget (G02). Used by the
// goal loop path; the lightweight path relies on the orchestrator turn instead.
func EvaluatorsForTier(t Tier, m model.BaseChatModel, sink *UsageSink) []Evaluator {
	base := []Evaluator{TestEvaluator{}}
	if m == nil {
		return base
	}
	switch {
	case t <= TierQuickFix:
		return base
	case t == TierStandard:
		return []Evaluator{TestEvaluator{}, IntentEvaluator{Model: m, Sink: sink}}
	default: // TierDesigned, TierTeam, TierAutonomous
		return []Evaluator{
			TestEvaluator{},
			IntentEvaluator{Model: m, Sink: sink},
			QualityEvaluator{Model: m, Sink: sink},
		}
	}
}

// ResolveTierFlag maps a -tier flag value to a Tier:
//   - "auto": runs RuleTierer over text (forced=false).
//   - "t0".."t4": forces that tier (forced=true).
//   - anything else: returns an error the caller surfaces as a usage error.
//
// Extracted from cmd/yanshi so the mapping is unit-testable (package main is
// not). The caller still owns printing/exiting on error.
func ResolveTierFlag(flagValue, text string) (Tier, bool, error) {
	if flagValue == "auto" {
		t, _ := RuleTierer{}.Tier(context.Background(), text)
		return t, false, nil
	}
	t, ok := ParseForcedTier(flagValue)
	if !ok {
		return TierStandard, false, fmt.Errorf("invalid tier %q (want auto or t0..t4)", flagValue)
	}
	return t, true, nil
}

// tierFlag is the inverse of ParseForcedTier: Tier → "t0".."t4".
var tierFlagTable = [5]string{"t0", "t1", "t2", "t3", "t4"}

func tierFlag(t Tier) string {
	if int(t) >= 0 && int(t) < len(tierFlagTable) {
		return tierFlagTable[t]
	}
	return "t1"
}

// EscalationHint returns a human-readable recommendation to re-run at the next
// higher tier, or "" for TierAutonomous (the top tier has nowhere to escalate).
//
// The dev-* skills each have an "Escalate" section that tells the AGENT when to
// re-tier; EscalationHint is the loop-side counterpart: when a lower tier
// exhausts its budget/iterations, the Decision surfaces this hint instead of
// exiting silently (G03: "升级规则不静默退出").
func EscalationHint(t Tier) string {
	if t >= TierAutonomous {
		return ""
	}
	next := t + 1
	return fmt.Sprintf("consider escalating to tier %s (-tier %s)", next.String(), tierFlag(next))
}
