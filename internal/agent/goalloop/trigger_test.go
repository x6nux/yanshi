package goalloop

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuleClassifier(t *testing.T) {
	t.Parallel()
	clf := RuleClassifier{}
	ctx := context.Background()

	tests := []struct {
		name string
		text string
		want Intent
	}{
		{"english implement", "implement the login feature", IntentGoalImpl},
		{"chinese implement", "实现用户登录", IntentGoalImpl},
		{"add feature", "add feature for dark mode", IntentGoalImpl},
		{"build", "build a REST API", IntentGoalImpl},
		{"fix bug", "fix bug in parser", IntentGoalImpl},
		{"fix bug uppercase", "Fix Bug in parser", IntentGoalImpl},
		{"question what", "what is 2+2", IntentSimpleQA},
		{"question why", "why does it crash", IntentSimpleQA},
		{"question how", "how do I configure this", IntentSimpleQA},
		{"question mark", "is this correct?", IntentSimpleQA},
		{"other", "random statement", IntentOther},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := clf.Classify(ctx, tc.text)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestDecide_ExplicitGoal(t *testing.T) {
	t.Parallel()
	tr := NewTrigger(RuleClassifier{}, AutoConfirmer{Result: false})
	enter, reason := tr.Decide(context.Background(), "/goal implement X")
	assert.True(t, enter)
	assert.Equal(t, "explicit", reason)
}

func TestDecide_SimpleQA(t *testing.T) {
	t.Parallel()
	tr := NewTrigger(RuleClassifier{}, AutoConfirmer{Result: true})
	enter, reason := tr.Decide(context.Background(), "what is 2+2")
	assert.False(t, enter)
	assert.Equal(t, "not a goal", reason)
}

func TestDecide_GoalImpl_ConfirmYes(t *testing.T) {
	t.Parallel()
	tr := NewTrigger(RuleClassifier{}, AutoConfirmer{Result: true})
	enter, reason := tr.Decide(context.Background(), "implement the login feature")
	assert.True(t, enter)
	assert.Equal(t, "auto+confirm", reason)
}

func TestDecide_GoalImpl_ConfirmNo(t *testing.T) {
	t.Parallel()
	tr := NewTrigger(RuleClassifier{}, AutoConfirmer{Result: false})
	enter, reason := tr.Decide(context.Background(), "implement the login feature")
	assert.False(t, enter)
	assert.Equal(t, "auto+confirm", reason)
}

func TestTrigger_DevPrefixReturnsTier(t *testing.T) {
	t.Parallel()
	tr := NewTrigger(RuleClassifier{}, AutoConfirmer{Result: true})
	enter, tier, reason := tr.DecideTier(context.Background(), "/dev fix typo")
	assert.True(t, enter)
	assert.Equal(t, TierQuickFix, tier)
	assert.Equal(t, "explicit", reason)
}

func TestTrigger_DevPrefixForcedTier(t *testing.T) {
	t.Parallel()
	tr := NewTrigger(RuleClassifier{}, AutoConfirmer{Result: true})
	// "/dev t2 ..." forces TierDesigned regardless of what RuleTierer would pick
	// ("implement login" alone would be TierStandard).
	enter, tier, reason := tr.DecideTier(context.Background(), "/dev t2 implement login")
	assert.True(t, enter)
	assert.Equal(t, TierDesigned, tier)
	assert.Equal(t, "explicit", reason)
}

func TestTrigger_DevPrefixAutoTier(t *testing.T) {
	t.Parallel()
	tr := NewTrigger(RuleClassifier{}, AutoConfirmer{Result: true})
	// No forced tier; RuleTierer sees "platform" -> TierAutonomous.
	enter, tier, reason := tr.DecideTier(context.Background(), "/dev build the whole platform")
	assert.True(t, enter)
	assert.Equal(t, TierAutonomous, tier)
	assert.Equal(t, "explicit", reason)
}

func TestTrigger_GoalPrefixDelegatesToDecide(t *testing.T) {
	t.Parallel()
	tr := NewTrigger(RuleClassifier{}, AutoConfirmer{Result: true})
	// "/goal ..." is not "/dev ", so DecideTier falls through to Decide which
	// recognizes the /goal prefix; the tier is auto-resolved on the full text.
	enter, tier, reason := tr.DecideTier(context.Background(), "/goal build the analytics project")
	assert.True(t, enter)
	assert.Equal(t, TierAutonomous, tier) // "project" -> autonomous
	assert.Equal(t, "explicit", reason)
}

func TestTrigger_DecideTier_NotAGoal(t *testing.T) {
	t.Parallel()
	tr := NewTrigger(RuleClassifier{}, AutoConfirmer{Result: true})
	enter, tier, reason := tr.DecideTier(context.Background(), "what is go?")
	assert.False(t, enter)
	assert.Equal(t, TierStandard, tier)
	assert.Equal(t, "not a goal", reason)
}

func TestParseForcedTier(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want Tier
		ok   bool
	}{
		{"t0", TierQuickFix, true},
		{"t1", TierStandard, true},
		{"t2", TierDesigned, true},
		{"t3", TierTeam, true},
		{"t4", TierAutonomous, true},
		{"t5", 0, false},
		{"tx", 0, false},
		{"", 0, false},
		{"T2", 0, false}, // case-sensitive
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			got, ok := ParseForcedTier(c.in)
			assert.Equal(t, c.ok, ok, c.in)
			if ok {
				assert.Equal(t, c.want, got, c.in)
			}
		})
	}
}
