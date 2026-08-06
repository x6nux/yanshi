package eino

import (
	"math"
	"testing"
)

func TestDefaultPricingRealAnthropicModels(t *testing.T) {
	tab := DefaultPricing()
	cases := map[string]ModelPricing{
		"claude-fable-5":    {InputPerM: 10, CacheHitPerM: 1, OutputPerM: 50},
		"claude-mythos-5":   {InputPerM: 10, CacheHitPerM: 1, OutputPerM: 50},
		"claude-opus-4-8":   {InputPerM: 5, CacheHitPerM: 0.5, OutputPerM: 25},
		"claude-opus-4-7":   {InputPerM: 5, CacheHitPerM: 0.5, OutputPerM: 25},
		"claude-opus-4-6":   {InputPerM: 5, CacheHitPerM: 0.5, OutputPerM: 25},
		"claude-sonnet-5":   {InputPerM: 3, CacheHitPerM: 0.3, OutputPerM: 15},
		"claude-sonnet-4-6": {InputPerM: 3, CacheHitPerM: 0.3, OutputPerM: 15},
		"claude-haiku-4-5":  {InputPerM: 1, CacheHitPerM: 0.1, OutputPerM: 5},
	}
	for model, want := range cases {
		got, ok := tab[model]
		if !ok {
			t.Fatalf("model %q missing from DefaultPricing", model)
		}
		if got.InputPerM != want.InputPerM || got.OutputPerM != want.OutputPerM || got.CacheHitPerM != want.CacheHitPerM {
			t.Fatalf("model %s pricing = %+v want %+v", model, got, want)
		}
	}
	for _, forbidden := range []string{"claude-opus-4-5", "claude-sonnet-4-8", "claude-3-7-sonnet"} {
		if _, ok := tab[forbidden]; ok {
			t.Fatalf("forbidden legacy id %q in table", forbidden)
		}
	}
}

// TestCostKnownSplitsCacheAndOutput.
//
// ledger: C4/COST1#4 缓存价区分
func TestCostKnownSplitsCacheAndOutput(t *testing.T) {
	tab := DefaultPricing()
	price := tab["claude-opus-4-8"]
	plain, _ := CostOK(tab, "claude-opus-4-8", Usage{Prompt: 1_000_000, Completion: 1_000_000})
	if math.Abs(plain-(price.InputPerM+price.OutputPerM)) > 1e-9 {
		t.Fatalf("plain cost = %v", plain)
	}
	cached, _ := CostOK(tab, "claude-opus-4-8", Usage{Prompt: 1_000_000, Cached: 1_000_000})
	if cached >= plain {
		t.Fatalf("cached cost %v must be cheaper than %v", cached, plain)
	}
}

func TestCostUnknownModelReportsNA(t *testing.T) {
	tab := DefaultPricing()
	cost, known := CostOK(tab, "acme-proprietary-1", Usage{Prompt: 100, Completion: 50})
	if known || cost != 0 {
		t.Fatalf("unknown model must be (0,false), got (%v,%v)", cost, known)
	}
}

// TestLedgerAccumulatesPerProviderUsage.
//
// ledger: C4/COST1#2 聚合正确
func TestLedgerAccumulatesPerProviderUsage(t *testing.T) {
	tab := DefaultPricing()
	price := tab["claude-sonnet-5"]
	var ledger Ledger
	ledger.Add(Usage{Prompt: 8000, Cached: 6000, Completion: 1200})
	ledger.Add(Usage{Prompt: 12000, Cached: 11000, Completion: 3000})
	cost, known := ledger.Cost(tab, "claude-sonnet-5")
	if !known {
		t.Fatal("sonnet-5 must be known")
	}
	wantInput := (2000 + 1000) / 1_000_000.0 * price.InputPerM
	wantCache := (6000 + 11000) / 1_000_000.0 * price.CacheHitPerM
	wantOutput := (1200 + 3000) / 1_000_000.0 * price.OutputPerM
	if math.Abs(cost-(wantInput+wantCache+wantOutput)) > 1e-9 {
		t.Fatalf("ledger cost = %v want %v", cost, wantInput+wantCache+wantOutput)
	}
	if ledger.Billed.CachedTokens != 17000 || ledger.Billed.InputTokens != 3000 || ledger.Billed.OutputTokens != 4200 {
		t.Fatalf("ledger billed = %+v", ledger.Billed)
	}
}

func TestLedgerReportsNAForUnknownModel(t *testing.T) {
	var ledger Ledger
	ledger.Add(Usage{Prompt: 500, Completion: 100})
	_, known := ledger.Cost(DefaultPricing(), "unknown-model")
	if known {
		t.Fatal("unknown model must not be known")
	}
}

func TestFormatCostRanges(t *testing.T) {
	cases := map[float64]string{
		0.00001: "<$0.0001",
		0.001:   "$0.0010",
		0.1:     "$0.100",
		12.345:  "$12.35",
	}
	for in, want := range cases {
		if got := FormatCost(in, true); got != want {
			t.Fatalf("FormatCost(%v, known=true) = %q want %q", in, got, want)
		}
	}
	// Constraint 11: known model + zero tokens renders as $0.0000 (not N/A).
	if got := FormatCost(0, true); got != "$0.0000" {
		t.Fatalf("FormatCost(0, true) = %q want $0.0000", got)
	}
	// Unknown model → N/A regardless of cost value.
	if got := FormatCost(0.4231, false); got != "N/A" {
		t.Fatalf("FormatCost known=false must be N/A, got %q", got)
	}
	if got := FormatCost(0, false); got != "N/A" {
		t.Fatalf("FormatCost(0, false) = %q want N/A", got)
	}
}

func TestMergePricingKeepsOverlayAndBase(t *testing.T) {
	base := DefaultPricing()
	overlay := map[string]ModelPricing{
		"custom":          {InputPerM: 2, CacheHitPerM: 0.2, OutputPerM: 8},
		"claude-opus-4-8": {InputPerM: 9, CacheHitPerM: 0.9, OutputPerM: 90},
	}
	merged := MergePricing(base, overlay)
	if merged["custom"].InputPerM != 2 {
		t.Fatalf("overlay custom missing")
	}
	if merged["claude-opus-4-8"].InputPerM != 9 {
		t.Fatalf("overlay must win")
	}
	if merged["claude-haiku-4-5"].InputPerM != 1 {
		t.Fatalf("base entry lost")
	}
	if base["claude-opus-4-8"].InputPerM != 5 {
		t.Fatalf("base mutated")
	}
}

func TestClampNonNeg_ReturnsZeroForNegative(t *testing.T) {
	if got := clampNonNeg(-5); got != 0 {
		t.Fatalf("clampNonNeg(-5) = %d want 0", got)
	}
}

func TestComputeCost_ClampsCachedToPrompt(t *testing.T) {
	price := ModelPricing{InputPerM: 10, CacheHitPerM: 1, OutputPerM: 50}
	cost := computeCost(price, Usage{Prompt: 1000, Cached: 9999, Completion: 500})
	want := 1000.0/1_000_000*1 + 500.0/1_000_000*50
	if math.Abs(cost-want) > 1e-12 {
		t.Fatalf("computeCost = %v want %v", cost, want)
	}
}

func TestLedger_ClampsCachedToPrompt(t *testing.T) {
	var ledger Ledger
	ledger.Add(Usage{Prompt: 1000, Cached: 9999, Completion: 500})
	if ledger.Billed.CachedTokens != 1000 {
		t.Fatalf("CachedTokens should be clamped to Prompt (1000), got %d", ledger.Billed.CachedTokens)
	}
	if ledger.Billed.InputTokens != 0 {
		t.Fatalf("InputTokens should be 0 after full cache hit, got %d", ledger.Billed.InputTokens)
	}
}
