package goalloop

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// ledger: M1/G03#2 auto/强制 tier 可测
func TestRuleTierer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"quick-fix typo", "fix the typo in config.yaml", "quick-fix"},
		{"standard bug+test", "fix this bug with a test", "standard"},
		{"designed module", "design and implement a new auth module", "designed"},
		{"team parallel", "build the billing component in parallel with the team", "team"},
		{"autonomous project", "autonomously build the whole analytics project", "autonomous"},
		{"unrecognized defaults to standard", "hello there", "standard"},
		{"empty defaults to standard", "", "standard"},
		{
			"ordering typo+project resolves autonomous",
			"fix the typo in the project",
			"autonomous",
		},
	}
	tt := RuleTierer{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.Tier(context.Background(), c.in)
			require.NoError(t, err)
			assert.Equal(t, c.want, got.String(), c.in)
		})
	}
}

func TestTier_String(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "quick-fix", TierQuickFix.String())
	assert.Equal(t, "standard", TierStandard.String())
	assert.Equal(t, "designed", TierDesigned.String())
	assert.Equal(t, "team", TierTeam.String())
	assert.Equal(t, "autonomous", TierAutonomous.String())
	assert.Equal(t, "unknown", Tier(999).String())
}

// stubTierer returns a fixed Tier so tests can detect whether the fallback
// was invoked (and what it would have returned).
type stubTierer struct{ tier Tier }

func (s stubTierer) Tier(_ context.Context, _ string) (Tier, error) {
	return s.tier, nil
}

func TestLLMTierer_FallbackOnEmpty(t *testing.T) {
	t.Parallel()
	// A fake model that returns an unparseable tier string -> fall back to RuleTierer.
	lt := LLMTierer{
		Model:    einollm.NewFakeModel([]string{"garbage"}, nil),
		Fallback: RuleTierer{},
	}
	got, err := lt.Tier(context.Background(), "fix the typo")
	assert.NoError(t, err)
	assert.Equal(t, TierQuickFix, got) // fallback resolved it
}

func TestLLMTierer_UsesModelAnswerWhenParseable(t *testing.T) {
	t.Parallel()
	// The model returns a VALID tier word ("designed"); the fallback stub would
	// return TierAutonomous. If the model's answer is used, the result is
	// TierDesigned, proving the fallback was NOT consulted.
	lt := LLMTierer{
		Model:    einollm.NewFakeModel([]string{"designed"}, nil),
		Fallback: stubTierer{tier: TierAutonomous},
	}
	got, err := lt.Tier(context.Background(), "refactor the auth module")
	assert.NoError(t, err)
	assert.Equal(t, TierDesigned, got)
}

func TestLLMTierer_FallbackOnModelError(t *testing.T) {
	t.Parallel()
	// The model errors; the fallback's tier is returned and no error propagates.
	lt := LLMTierer{
		Model:    einollm.NewFakeModel(nil, errors.New("boom")),
		Fallback: stubTierer{tier: TierQuickFix},
	}
	got, err := lt.Tier(context.Background(), "fix the typo")
	assert.NoError(t, err)
	assert.Equal(t, TierQuickFix, got)
}

func TestLLMTierer_NilFallbackUnparseable(t *testing.T) {
	t.Parallel()
	// No fallback; an unparseable model reply yields the safe default TierStandard.
	lt := LLMTierer{
		Model: einollm.NewFakeModel([]string{"garbage"}, nil),
	}
	got, err := lt.Tier(context.Background(), "fix the typo")
	assert.NoError(t, err)
	assert.Equal(t, TierStandard, got)
}

func TestLLMTierer_NilFallbackModelError(t *testing.T) {
	t.Parallel()
	// No fallback; a model error yields the safe default TierStandard, no error.
	lt := LLMTierer{
		Model: einollm.NewFakeModel(nil, errors.New("boom")),
	}
	got, err := lt.Tier(context.Background(), "fix the typo")
	assert.NoError(t, err)
	assert.Equal(t, TierStandard, got)
}

// ---------------------------------------------------------------------------
// Tier routing pure functions (G03) + LLMTierer usage (G02)
// ---------------------------------------------------------------------------

func TestTier_SkillName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tier Tier
		want string
	}{
		{TierQuickFix, "dev-quick-fix"},
		{TierStandard, "dev-standard-feature"},
		{TierDesigned, "dev-designed-feature"},
		{TierTeam, "dev-team-feature"},
		{TierAutonomous, "dev-autonomous-project"},
		{Tier(999), "dev-standard-feature"}, // unknown → safe default
	}
	for _, c := range cases {
		assert.Equal(t, c.want, c.tier.SkillName(), "tier %v", c.tier)
	}
}

func TestTier_Path(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		tier Tier
		want string
	}{
		{TierQuickFix, "lightweight"},
		{TierStandard, "lightweight"},
		{TierDesigned, "lightweight"},
		{TierTeam, "loop"},
		{TierAutonomous, "loop"},
	} {
		assert.Equal(t, c.want, c.tier.Path(), "tier %v", c.tier)
	}
}

func TestEvaluatorsForTier(t *testing.T) {
	t.Parallel()
	m := einollm.NewFakeModel([]string{"x"}, nil)
	assert.Len(t, EvaluatorsForTier(TierQuickFix, m, nil), 1, "T0: test only")
	assert.Len(t, EvaluatorsForTier(TierStandard, m, nil), 2, "T1: test+intent")
	assert.Len(t, EvaluatorsForTier(TierDesigned, m, nil), 3, "T2: test+intent+quality")
	assert.Len(t, EvaluatorsForTier(TierAutonomous, m, nil), 3, "T4: all three")
	// Nil model: only the non-LLM TestEvaluator is returned regardless of tier.
	assert.Len(t, EvaluatorsForTier(TierAutonomous, nil, nil), 1, "nil model → test only")
}

func TestEvaluatorsForTier_StampSink(t *testing.T) {
	t.Parallel()
	m := einollm.NewFakeModel([]string{"x"}, nil)
	sink := &UsageSink{}
	evals := EvaluatorsForTier(TierDesigned, m, sink)
	// The intent + quality evaluators must carry the shared sink so their usage
	// flows into the loop's budget. (TestEvaluator has no Sink field.)
	assert.Equal(t, sink, evals[1].(IntentEvaluator).Sink)
	assert.Equal(t, sink, evals[2].(QualityEvaluator).Sink)
}

// ledger: M1/G03#2 auto/强制 tier 可测
func TestResolveTierFlag(t *testing.T) {
	t.Parallel()
	// auto → RuleTierer (forced=false)
	t0, forced, err := ResolveTierFlag("auto", "fix the typo")
	require.NoError(t, err)
	assert.False(t, forced)
	assert.Equal(t, TierQuickFix, t0)

	// explicit t2 → forced
	t2, forced, err := ResolveTierFlag("t2", "anything")
	require.NoError(t, err)
	assert.True(t, forced)
	assert.Equal(t, TierDesigned, t2)

	// invalid → error (caller prints + exits)
	_, _, err = ResolveTierFlag("t9", "x")
	assert.Error(t, err)
}

func TestEscalationHint(t *testing.T) {
	t.Parallel()
	assert.NotEmpty(t, EscalationHint(TierQuickFix), "T0 gets an upgrade hint")
	assert.Contains(t, EscalationHint(TierStandard), "t2")
	assert.Empty(t, EscalationHint(TierAutonomous), "T4 has nowhere to escalate")
}

func TestLLMTierer_RecordsUsage(t *testing.T) {
	t.Parallel()
	msg := schema.AssistantMessage("designed", nil)
	msg.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{TotalTokens: 7}}
	m := einollm.NewFakeModelWithMessages([]*schema.Message{msg}, nil)
	sink := &UsageSink{}
	lt := LLMTierer{Model: m, Fallback: RuleTierer{}, Sink: sink}
	_, err := lt.Tier(context.Background(), "refactor the module")
	require.NoError(t, err)
	assert.Equal(t, 7, sink.Snapshot().TotalTokens, "LLMTierer must count its Generate call")
}
