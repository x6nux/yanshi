package eino

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// mustParseModelCatalog: build-time defect detection
// ---------------------------------------------------------------------------

// TestMustParseModelCatalog_MalformedYAMLPanics pins the build-time defect
// check documented on mustParseModelCatalog: a models.yaml that does not even
// parse as YAML must panic at package init, not surface as a runtime "model
// not found" for every model.
func TestMustParseModelCatalog_MalformedYAMLPanics(t *testing.T) {
	assert.Panics(t, func() {
		mustParseModelCatalog([]byte("models: [this is not: valid: yaml"))
	})
}

// TestMustParseModelCatalog_EmptyIDPanics covers both the literally-empty and
// the whitespace-only id, since the panic check trims before comparing.
func TestMustParseModelCatalog_EmptyIDPanics(t *testing.T) {
	assert.Panics(t, func() {
		mustParseModelCatalog([]byte("models:\n  - id: \"\"\n    context_window: 1000\n"))
	}, "an empty id")
	assert.Panics(t, func() {
		mustParseModelCatalog([]byte("models:\n  - id: \"   \"\n    context_window: 1000\n"))
	}, "a whitespace-only id")
}

// TestMustParseModelCatalog_DuplicateIDPanics_CaseInsensitive pins that the
// duplicate check normalizes case the same way catalogAliases and the two
// exact-match indexes do — "GPT-4O" and "gpt-4o" are the same row twice, not
// two distinct entries that happen to collide later.
func TestMustParseModelCatalog_DuplicateIDPanics_CaseInsensitive(t *testing.T) {
	assert.Panics(t, func() {
		mustParseModelCatalog([]byte(
			"models:\n  - id: gpt-4o\n    context_window: 1000\n  - id: GPT-4O\n    context_window: 2000\n"))
	})
}

// TestMustParseModelCatalog_WellFormedDoesNotPanic is the control: a
// well-formed file with distinct ids must parse cleanly, which is what makes
// the panic tests above mean "this specific defect", not "any input panics".
func TestMustParseModelCatalog_WellFormedDoesNotPanic(t *testing.T) {
	assert.NotPanics(t, func() {
		mustParseModelCatalog([]byte("models:\n  - id: gpt-4o\n    context_window: 128000\n  - id: gpt-4o-mini\n    context_window: 128000\n"))
	})
}

// TestMustParseModelCatalog_ProductionFileParsedAtInit proves the package-init
// parse of the real, shipped models.yaml already happened without panicking
// (if it had, this test binary would never have reached main) and that it is
// not empty — a models.yaml that got embedded as zero bytes would parse
// "successfully" into an empty slice and silently degrade every model to
// fallback defaults.
func TestMustParseModelCatalog_ProductionFileParsedAtInit(t *testing.T) {
	require.NotEmpty(t, modelCatalog.Models, "the embedded models.yaml must have parsed at least one row")
}

// ---------------------------------------------------------------------------
// Full-field parse round trip
// ---------------------------------------------------------------------------

// TestParse_AllSchemaFieldsRoundTrip proves every field the INF2 contract
// documents (id / aliases / context_window / max_output / pricing{in,out,
// cached} / modalities / reasoning_efforts / truncation_policy /
// auto_compact_threshold / priority) parses without being dropped, including
// the four fields (max_output, modalities, reasoning_efforts, priority) no Go
// code consumes yet in this ticket. A yaml-tag typo on any of those would
// silently discard operator data with no test noticing until a LATER ticket
// tries to read it — this test is what makes that later ticket's job "read a
// field", not "discover the tag never worked".
//
// truncation_policy graduated out of that list under W-C-09: it is now
// parsed AND semantically consumed (see KnownTruncationPolicy), so its value
// here must be one mustParseModelCatalog's ParseTruncationPolicy validation
// actually accepts ("head=20,tail=12") rather than an arbitrary placeholder
// string — this file's own row-level validation would panic on a fixture
// carrying "auto", which is what this test used before that validation
// existed.
func TestParse_AllSchemaFieldsRoundTrip(t *testing.T) {
	const raw = `
models:
  - id: catalog-fixture-full
    aliases: [cff-alias-one, cff-alias-two]
    context_window: 200000
    max_output: 8192
    pricing:
      input_per_m: 3.5
      cache_hit_per_m: 0.35
      output_per_m: 15
    modalities: [text, image]
    reasoning_efforts: [low, medium, high]
    truncation_policy: "head=20,tail=12"
    auto_compact_threshold: 0.75
    priority: 7
`
	cat := mustParseModelCatalog([]byte(raw))
	require.Len(t, cat.Models, 1)
	row := cat.Models[0]

	assert.Equal(t, "catalog-fixture-full", row.ID)
	assert.Equal(t, []string{"cff-alias-one", "cff-alias-two"}, row.Aliases)
	assert.Equal(t, 200000, row.ContextWindow)
	assert.Equal(t, 8192, row.MaxOutput)
	require.NotNil(t, row.Pricing)
	assert.Equal(t, 3.5, row.Pricing.InputPerM)
	assert.Equal(t, 0.35, row.Pricing.CacheHitPerM)
	assert.Equal(t, 15.0, row.Pricing.OutputPerM)
	assert.Equal(t, []string{"text", "image"}, row.Modalities)
	assert.Equal(t, []string{"low", "medium", "high"}, row.ReasoningEfforts)
	assert.Equal(t, "head=20,tail=12", row.TruncationPolicy)
	assert.Equal(t, 0.75, row.AutoCompactThreshold)
	assert.Equal(t, 7, row.Priority)
}

// TestCatalogAliases_NormalizesCaseAndWhitespace pins the normalization
// catalogAliases performs, which both exact-match indexes (pricing,
// auto-compact-threshold) and the context-window boundary matcher depend on
// agreeing with.
func TestCatalogAliases_NormalizesCaseAndWhitespace(t *testing.T) {
	row := modelCatalogRow{ID: "  Mixed-Case-ID  ", Aliases: []string{" Alias-One ", "", "   "}}
	got := catalogAliases(row)
	assert.Equal(t, []string{"mixed-case-id", "alias-one"}, got,
		"blank/whitespace-only aliases must be dropped, not indexed as an empty string")
}

// ---------------------------------------------------------------------------
// Acceptance #1 — adding a model is a data-file edit, no Go changes
// ---------------------------------------------------------------------------

// TestAcceptance1_NewModelIsADataFileEditOnly proves the whole point of INF2:
// a brand-new model id, present ONLY in an in-test YAML fixture that no Go
// source in this repo has ever mentioned, resolves correctly through all
// three derived indexes (context window, pricing, auto-compact threshold)
// built from that fixture alone. If adding a model required touching Go code,
// this test would have to hand-write a new catalog entry in a .go file
// instead of a YAML string — it does not.
func TestAcceptance1_NewModelIsADataFileEditOnly(t *testing.T) {
	const raw = `
models:
  - id: acme-nova-9000
    context_window: 512000
    pricing:
      input_per_m: 2
      cache_hit_per_m: 0.2
      output_per_m: 10
    auto_compact_threshold: 0.7
`
	cat := mustParseModelCatalog([]byte(raw))

	windows := buildContextWindowCatalog(cat)
	require.Len(t, windows, 1)
	assert.Equal(t, "acme-nova-9000", windows[0].pattern)
	assert.Equal(t, 512000, windows[0].tokens)

	prices := buildPricingCatalog(cat)
	price, ok := prices["acme-nova-9000"]
	require.True(t, ok, "the new model must resolve in the pricing table built from the fixture")
	assert.Equal(t, 2.0, price.InputPerM)
	assert.Equal(t, 0.2, price.CacheHitPerM)
	assert.Equal(t, 10.0, price.OutputPerM)

	thresholds := buildAutoCompactThresholds(cat)
	th, ok := thresholds["acme-nova-9000"]
	require.True(t, ok, "the new model must resolve in the auto-compact-threshold table built from the fixture")
	assert.Equal(t, 0.7, th)
}

// TestRealShippedCatalogReachesPublicEntryPoints closes the gap
// TestAcceptance1_NewModelIsADataFileEditOnly leaves open (F-6): that test
// only calls the internal build* functions against an in-test-only fixture
// string, never the three public functions callers actually use, and never
// touches the real embedded models.yaml. This test calls KnownContextWindow,
// DefaultPricing and KnownAutoCompactThreshold — the real, exported,
// package-init-wired entry points — against real ids that ship in
// models.yaml today, proving the embed -> parse -> package-init -> public-API
// chain genuinely works end to end for the shipped file.
func TestRealShippedCatalogReachesPublicEntryPoints(t *testing.T) {
	// gpt-4.1 carries its own context_window row.
	window, ok := KnownContextWindow("gpt-4.1")
	require.True(t, ok, "gpt-4.1 must resolve in the real shipped models.yaml")
	assert.Equal(t, 1047576, window)

	// claude-opus-4-8 has its own pricing row but no context_window row of
	// its own (see models.yaml) — it must fall through to the "claude"
	// family row (200000), proving family-row fallthrough works against the
	// real shipped file, not just a synthetic fixture.
	claudeWindow, ok := KnownContextWindow("claude-opus-4-8")
	require.True(t, ok, "claude-opus-4-8 must resolve via the claude family row")
	assert.Equal(t, 200000, claudeWindow)

	prices := DefaultPricing()
	price, ok := prices["claude-opus-4-8"]
	require.True(t, ok, "claude-opus-4-8 must resolve in DefaultPricing() built from the real shipped file")
	assert.Equal(t, 5.0, price.InputPerM)
	assert.Equal(t, 0.5, price.CacheHitPerM)
	assert.Equal(t, 25.0, price.OutputPerM)

	// F-4/W-C-03: the shipped file currently has ZERO auto_compact_threshold
	// rows. KnownAutoCompactThreshold must honestly report "no opinion"
	// (0, false) for a real model — a true here would mean either the
	// shipped data changed (update this test to match) or resolution is
	// reading something it should not.
	threshold, ok := KnownAutoCompactThreshold("claude-opus-4-8")
	assert.False(t, ok, "the shipped catalog has zero auto_compact_threshold rows (W-C-03)")
	assert.Equal(t, float64(0), threshold)
}

// ---------------------------------------------------------------------------
// Acceptance #2 — unlisted models degrade to a safe default, never block startup
// ---------------------------------------------------------------------------

// TestAcceptance2_UnknownModelResolvesToSafeDefaults proves that a model id
// absent from the catalog never panics or errors anywhere along the
// resolution chain a provider actually walks at startup — it resolves to the
// documented conservative fallback instead.
func TestAcceptance2_UnknownModelResolvesToSafeDefaults(t *testing.T) {
	const unknown = "totally-unheard-of-model-xyz"

	w, ok := KnownContextWindow(unknown)
	assert.False(t, ok, "an unlisted model must not be reported as known")
	assert.Equal(t, 0, w)

	// The full resolution ladder a provider actually walks (ResolveContextWindow)
	// must still hand back a positive, usable window — not zero, not an error.
	resolved, source := ResolveContextWindow(ProviderShape{Model: unknown}, 0)
	assert.Greater(t, resolved, 0, "an unknown cloud model must still resolve to a positive fallback window")
	assert.Equal(t, WindowFromDefault, source)

	_, priced := DefaultPricing()[unknown]
	assert.False(t, priced, "an unlisted model has no cost opinion — CostOK's caller must treat this as unknown, not $0")

	th, known := KnownAutoCompactThreshold(unknown)
	assert.False(t, known, "an unlisted model must not be reported as having a catalog threshold")
	assert.Equal(t, float64(0), th)

	rTh, rKnown := ResolveAutoCompactThreshold(ProviderShape{Model: unknown})
	assert.False(t, rKnown, "ResolveAutoCompactThreshold must defer to the caller's own fallback for an unknown model")
	assert.Equal(t, float64(0), rTh)
}

// ---------------------------------------------------------------------------
// Acceptance #3 — ProviderConfig's own field outranks the catalog
// ---------------------------------------------------------------------------

// withAutoCompactThresholds swaps the package-level catalog-derived table for
// the duration of one test and restores the original afterwards. The shipped
// models.yaml currently populates zero auto_compact_threshold rows — no
// per-model value has a documented real-world basis to ship with yet
// (F-4/W-C-03; see KnownAutoCompactThreshold's doc comment and models.yaml's
// own header for the explanation, both updated alongside this one) — so this
// is the only way to exercise ResolveAutoCompactThreshold's catalog-hit
// branch against a known value instead of always taking the "nothing had an
// opinion" branch.
//
// Precedented by internal/llm/eino/m5m6_adaptive_wire_test.go's
// slog.SetDefault save/restore/t.Cleanup idiom for the same reason: the thing
// under test reads a package var, not an injected parameter.
func withAutoCompactThresholds(t *testing.T, fixture map[string]float64) {
	t.Helper()
	prev := autoCompactThresholds
	autoCompactThresholds = fixture
	t.Cleanup(func() { autoCompactThresholds = prev })
}

// TestAcceptance3_ConfigOverrideOutranksCatalog pins the three-way precedence
// ResolveAutoCompactThreshold's doc comment documents: an explicit
// ProviderShape.AutoCompactThreshold (which projects
// config.ProviderConfig.AutoCompactThreshold) wins outright over a catalog
// hit; a catalog hit is used only when the config field is unset; and when
// NEITHER has an opinion the function reports false so the caller's own
// fallback (CompactionConfig.Threshold) applies unchanged.
func TestAcceptance3_ConfigOverrideOutranksCatalog(t *testing.T) {
	withAutoCompactThresholds(t, map[string]float64{"catalog-model": 0.5})

	// Both config and catalog have an opinion: config wins.
	v, ok := ResolveAutoCompactThreshold(ProviderShape{Model: "catalog-model", AutoCompactThreshold: 0.9})
	require.True(t, ok)
	assert.Equal(t, 0.9, v, "an explicit config value must outrank the catalog, not average or defer to it")

	// Only the catalog has an opinion: catalog wins.
	v, ok = ResolveAutoCompactThreshold(ProviderShape{Model: "catalog-model"})
	require.True(t, ok)
	assert.Equal(t, 0.5, v)

	// Neither has an opinion: caller's own fallback applies (signaled by ok=false).
	v, ok = ResolveAutoCompactThreshold(ProviderShape{Model: "nobody-has-an-opinion"})
	assert.False(t, ok)
	assert.Equal(t, float64(0), v)

	// A config value present but <= 0 counts as unset, mirroring ContextWindow's
	// "<= 0 means unset" convention (ProviderShape.AutoCompactThreshold's doc
	// comment) — it must fall through to the catalog, not win as "0".
	v, ok = ResolveAutoCompactThreshold(ProviderShape{Model: "catalog-model", AutoCompactThreshold: 0})
	require.True(t, ok)
	assert.Equal(t, 0.5, v, "a non-positive config value must defer to the catalog, not be treated as an explicit 0 threshold")
}

// TestAcceptance3_NegativeConfigValueIsAnExplicitDisable pins F-10/W-C-04: a
// NEGATIVE ProviderShape.AutoCompactThreshold is NOT "unset" (unlike 0, which
// TestAcceptance3_ConfigOverrideOutranksCatalog pins as falling through to the
// catalog) — it is an explicit per-provider disable that outranks a catalog
// hit exactly like a positive override does, and is reported with ok=true so
// the caller (thresholdFor / wrapCompaction) can turn it into "off" for that
// one provider without touching the operator's global CompactionConfig.Threshold.
func TestAcceptance3_NegativeConfigValueIsAnExplicitDisable(t *testing.T) {
	withAutoCompactThresholds(t, map[string]float64{"catalog-model": 0.5})

	v, ok := ResolveAutoCompactThreshold(ProviderShape{Model: "catalog-model", AutoCompactThreshold: -1})
	require.True(t, ok, "a negative config value must be reported as an opinion (ok=true), not treated as unset")
	assert.Equal(t, -1.0, v, "the negative sentinel must pass through unchanged so the caller can gate on its sign")
}

// ---------------------------------------------------------------------------
// Acceptance #4 — the threshold is a window RATIO, sourced from the table
// ---------------------------------------------------------------------------

// TestAcceptance4_ThresholdIsARatioNotAnAbsoluteTokenCount proves
// buildAutoCompactThresholds passes the configured fraction through unchanged
// (it is not scaled by, or combined with, any context-window value at build
// time — the ratio-vs-window-size math happens downstream, in the compaction
// callers that multiply it against a resolved window) and that this is
// entirely table-sourced: values come only from the parsed fixture, with no
// Go-coded per-model constant anywhere in the path.
func TestAcceptance4_ThresholdIsARatioNotAnAbsoluteTokenCount(t *testing.T) {
	const raw = `
models:
  - id: ratio-model
    context_window: 128000
    auto_compact_threshold: 0.85
`
	cat := mustParseModelCatalog([]byte(raw))
	got := buildAutoCompactThresholds(cat)
	th, ok := got["ratio-model"]
	require.True(t, ok)
	assert.Equal(t, 0.85, th, "the table must carry the raw fraction from the file, not context_window * threshold "+
		"or any other pre-multiplied absolute token count")
	// F-8: this used to also assert assert.LessOrEqual(t, th, 1.0) against
	// the hardcoded 0.85 fixture above — tautological, since 0.85 <= 1.0
	// regardless of what buildAutoCompactThresholds does, and it passed for
	// every implementation including a broken one. The catalog layer this
	// test covers never enforces the >1 bound at all (buildAutoCompactThresholds
	// only excludes <= 0, see TestAcceptance4_NonPositiveThresholdExcludedNotZero
	// below); the bound belongs to, and is pinned by, the CONFIG layer's
	// validate() — see internal/config's
	// TestValidate_AutoCompactThresholdAboveOneIsRejected (F-3).
}

// TestAcceptance4_NonPositiveThresholdExcludedNotZero proves a row with
// auto_compact_threshold <= 0 (unset, or explicitly zeroed) is left OUT of the
// derived table entirely rather than appearing as a false "compact at 0% of
// the window" entry — which would fire compaction on every single turn for
// that model. This mirrors buildPricingCatalog's and
// buildContextWindowCatalog's "opinion vs. no opinion" convention for the
// same reason KnownAutoCompactThreshold's false return exists: silence, not a
// zero value, means "no opinion".
func TestAcceptance4_NonPositiveThresholdExcludedNotZero(t *testing.T) {
	const raw = `
models:
  - id: unset-threshold-model
    context_window: 128000
  - id: explicit-zero-model
    context_window: 128000
    auto_compact_threshold: 0
`
	cat := mustParseModelCatalog([]byte(raw))
	got := buildAutoCompactThresholds(cat)
	_, ok := got["unset-threshold-model"]
	assert.False(t, ok, "a row that never set auto_compact_threshold must not appear in the derived table")
	_, ok = got["explicit-zero-model"]
	assert.False(t, ok, "an explicit 0 must be excluded, not stored as a literal 0% threshold")
}

// ---------------------------------------------------------------------------
// W-C-09 — ParseTruncationPolicy / KnownTruncationPolicy / ResolveTruncationPolicy
// ---------------------------------------------------------------------------

// TestParseTruncationPolicy_ValidFormats pins the accepted "head=<N>,tail=<M>"
// shapes: both keys, either key alone (the other keeps DefaultTruncationSpec's
// value), case-insensitive keys, whitespace tolerance around commas/"="/
// values, and an empty segment from a stray comma being skipped rather than
// rejecting the whole string.
func TestParseTruncationPolicy_ValidFormats(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want TruncationSpec
	}{
		{"both keys", "head=15,tail=10", TruncationSpec{HeadLines: 15, TailLines: 10}},
		{"head only keeps default tail", "head=30", TruncationSpec{HeadLines: 30, TailLines: DefaultTruncationSpec.TailLines}},
		{"tail only keeps default head", "tail=5", TruncationSpec{HeadLines: DefaultTruncationSpec.HeadLines, TailLines: 5}},
		{"whitespace tolerated", " head = 20 , tail = 8 ", TruncationSpec{HeadLines: 20, TailLines: 8}},
		{"case-insensitive keys", "HEAD=12,TAIL=6", TruncationSpec{HeadLines: 12, TailLines: 6}},
		{"empty segment skipped", "head=10,,tail=5", TruncationSpec{HeadLines: 10, TailLines: 5}},
		{"later key wins over earlier same key", "head=1,head=2", TruncationSpec{HeadLines: 2, TailLines: DefaultTruncationSpec.TailLines}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseTruncationPolicy(tc.in)
			require.True(t, ok, "input %q should have parsed", tc.in)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestParseTruncationPolicy_InvalidRejected pins the doc comment's "no
// partial success" rule: empty/blank input, a segment with no "=", an
// unparsable or non-positive value, and an unknown key all report false
// (and the zero TruncationSpec) rather than a spec with some fields parsed
// and others silently defaulted.
func TestParseTruncationPolicy_InvalidRejected(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty string", ""},
		{"blank string", "   "},
		{"no equals sign", "auto"},
		{"non-numeric value", "head=abc"},
		{"zero is not positive", "head=0"},
		{"negative value", "head=-5"},
		{"unknown key", "foo=10"},
		{"bare key no equals mixed with valid", "head=10,bogus"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseTruncationPolicy(tc.in)
			assert.False(t, ok, "input %q should have been rejected", tc.in)
			assert.Equal(t, TruncationSpec{}, got, "a rejected input must report the zero value, not a partially-applied spec")
		})
	}
}

// withTruncationPolicies swaps the package-level catalog-derived table for
// the duration of one test and restores the original afterwards, mirroring
// withAutoCompactThresholds above for the same reason: the shipped
// models.yaml currently populates ZERO truncation_policy rows (Ruling RC-9),
// so this is the only way to exercise KnownTruncationPolicy's /
// ResolveTruncationPolicy's catalog-hit branch against a known value instead
// of always taking the "nothing had an opinion" branch.
func withTruncationPolicies(t *testing.T, fixture map[string]TruncationSpec) {
	t.Helper()
	prev := truncationPolicies
	truncationPolicies = fixture
	t.Cleanup(func() { truncationPolicies = prev })
}

// withFallbackModelsCatalog swaps the package-level catalog-derived table
// for the duration of one test and restores the original afterwards,
// mirroring withTruncationPolicies above for the same reason: the shipped
// models.yaml currently populates ZERO fallback_models rows (Ruling RC-8),
// so this is the only way to exercise KnownFallbackModels' /
// ResolveFallbackModels' catalog-hit branch against a known value instead of
// always taking the "nothing had an opinion" branch.
func withFallbackModelsCatalog(t *testing.T, fixture map[string][]string) {
	t.Helper()
	prev := fallbackModels
	fallbackModels = fixture
	t.Cleanup(func() { fallbackModels = prev })
}

// TestKnownFallbackModels_ExactMatchOnly pins the empty-modelID guard and the
// EXACT (not boundary-substring) matching KnownFallbackModels' doc comment
// claims, mirroring TestKnownTruncationPolicy_ExactMatchOnly below.
func TestKnownFallbackModels_ExactMatchOnly(t *testing.T) {
	withFallbackModelsCatalog(t, map[string][]string{
		"gpt-5-preview": {"gpt-5-mini"},
	})

	ids, ok := KnownFallbackModels("gpt-5-preview")
	require.True(t, ok)
	assert.Equal(t, []string{"gpt-5-mini"}, ids)

	// Case/whitespace normalization still applies, exactly like the lookup key.
	ids, ok = KnownFallbackModels("  GPT-5-Preview  ")
	require.True(t, ok)
	assert.Equal(t, []string{"gpt-5-mini"}, ids)

	// A substring of a known id must not match: this table is exact-match,
	// not boundary-substring.
	_, ok = KnownFallbackModels("gpt-5")
	assert.False(t, ok, "a prefix of a cataloged id must not match under exact-match semantics")

	// Empty/blank modelID is guarded before the map lookup.
	_, ok = KnownFallbackModels("")
	assert.False(t, ok)
	_, ok = KnownFallbackModels("   ")
	assert.False(t, ok)

	// A model with no opinion in the table.
	_, ok = KnownFallbackModels("nobody-has-an-opinion")
	assert.False(t, ok)
}

// TestResolveFallbackModels_OverrideOutranksCatalog pins the precedence
// ResolveFallbackModels' doc comment documents (M-2 / W-C-10): a non-empty
// override wins outright over a catalog hit, an empty override falls
// through to the catalog, and when neither has an opinion the function
// reports false so the caller keeps using modelID alone — mirroring
// TestResolveTruncationPolicy_OverrideOutranksCatalog above. Unlike
// truncation_policy there is no "malformed" middle case to test: the field
// is already a []string, so there is nothing for ResolveFallbackModels
// itself to fail parsing (config.go's own doc comment covers the
// unresolvable-id case, which is bootstrap's buildProviderFallbacks'
// concern, not this function's).
func TestResolveFallbackModels_OverrideOutranksCatalog(t *testing.T) {
	withFallbackModelsCatalog(t, map[string][]string{
		"catalog-model": {"catalog-fallback"},
	})

	// Both override and catalog have an opinion: override wins, verbatim —
	// not merged with the catalog's entry.
	ids, ok := ResolveFallbackModels([]string{"override-fallback"}, "catalog-model")
	require.True(t, ok)
	assert.Equal(t, []string{"override-fallback"}, ids, "a non-empty override must outrank the catalog, not merge with it")

	// Only the catalog has an opinion (empty override): catalog wins.
	ids, ok = ResolveFallbackModels(nil, "catalog-model")
	require.True(t, ok)
	assert.Equal(t, []string{"catalog-fallback"}, ids)

	// Neither has an opinion: caller's own fallback applies (signaled by ok=false).
	ids, ok = ResolveFallbackModels(nil, "nobody-has-an-opinion")
	assert.False(t, ok)
	assert.Empty(t, ids)
}

// TestKnownTruncationPolicy_ExactMatchOnly pins the empty-modelID guard and
// the EXACT (not boundary-substring) matching KnownTruncationPolicy's doc
// comment claims: a family-prefix substring of a known id must NOT match,
// unlike KnownContextWindow's boundary-substring behavior.
func TestKnownTruncationPolicy_ExactMatchOnly(t *testing.T) {
	withTruncationPolicies(t, map[string]TruncationSpec{
		"gpt-5-preview": {HeadLines: 25, TailLines: 15},
	})

	v, ok := KnownTruncationPolicy("gpt-5-preview")
	require.True(t, ok)
	assert.Equal(t, TruncationSpec{HeadLines: 25, TailLines: 15}, v)

	// Case/whitespace normalization still applies, exactly like the lookup key.
	v, ok = KnownTruncationPolicy("  GPT-5-Preview  ")
	require.True(t, ok)
	assert.Equal(t, TruncationSpec{HeadLines: 25, TailLines: 15}, v)

	// A substring of a known id must not match: this table is exact-match,
	// not boundary-substring.
	_, ok = KnownTruncationPolicy("gpt-5")
	assert.False(t, ok, "a prefix of a cataloged id must not match under exact-match semantics")

	// Empty/blank modelID is guarded before the map lookup.
	_, ok = KnownTruncationPolicy("")
	assert.False(t, ok)
	_, ok = KnownTruncationPolicy("   ")
	assert.False(t, ok)

	// A model with no opinion in the table.
	_, ok = KnownTruncationPolicy("nobody-has-an-opinion")
	assert.False(t, ok)
}

// TestResolveTruncationPolicy_OverrideOutranksCatalog pins the precedence
// ResolveTruncationPolicy's doc comment documents: a valid override string
// wins outright over a catalog hit; a malformed or empty override falls
// through to the catalog (NOT a load error — see the doc comment's cycle
// explanation); and when neither has an opinion the function reports false
// so the caller's own fallback (DefaultTruncationSpec) applies unchanged.
func TestResolveTruncationPolicy_OverrideOutranksCatalog(t *testing.T) {
	withTruncationPolicies(t, map[string]TruncationSpec{
		"catalog-model": {HeadLines: 40, TailLines: 20},
	})

	// Both override and catalog have an opinion: override wins.
	v, ok := ResolveTruncationPolicy("head=5,tail=5", "catalog-model")
	require.True(t, ok)
	assert.Equal(t, TruncationSpec{HeadLines: 5, TailLines: 5}, v, "a valid override must outrank the catalog")

	// Only the catalog has an opinion (empty override): catalog wins.
	v, ok = ResolveTruncationPolicy("", "catalog-model")
	require.True(t, ok)
	assert.Equal(t, TruncationSpec{HeadLines: 40, TailLines: 20}, v)

	// A malformed override degrades to the catalog rather than failing:
	// "configured wrong" and "not configured" are the same signal.
	v, ok = ResolveTruncationPolicy("not-a-policy", "catalog-model")
	require.True(t, ok, "a malformed override must fall through to the catalog, not report false")
	assert.Equal(t, TruncationSpec{HeadLines: 40, TailLines: 20}, v)

	// Neither has an opinion: caller's own fallback applies (signaled by ok=false).
	v, ok = ResolveTruncationPolicy("", "nobody-has-an-opinion")
	assert.False(t, ok)
	assert.Equal(t, TruncationSpec{}, v)

	// A malformed override AND no catalog opinion: still false, not a partial
	// application of the malformed override.
	v, ok = ResolveTruncationPolicy("garbage", "nobody-has-an-opinion")
	assert.False(t, ok)
	assert.Equal(t, TruncationSpec{}, v)
}

// TestBuildTruncationPolicies_TableSourcedFromCatalog proves
// buildTruncationPolicies is driven entirely by the parsed fixture (no
// Go-coded per-model constant), mirroring
// TestAcceptance4_ThresholdIsARatioNotAnAbsoluteTokenCount's shape for
// auto_compact_threshold, and that a row with an unparsable
// truncation_policy would have already panicked in mustParseModelCatalog
// before reaching this function (so every row this function sees is either
// empty or valid, per its own doc comment).
func TestBuildTruncationPolicies_TableSourcedFromCatalog(t *testing.T) {
	const raw = `
models:
  - id: policy-model
    context_window: 128000
    truncation_policy: "head=22,tail=11"
  - id: no-policy-model
    context_window: 128000
`
	cat := mustParseModelCatalog([]byte(raw))
	got := buildTruncationPolicies(cat)
	spec, ok := got["policy-model"]
	require.True(t, ok)
	assert.Equal(t, TruncationSpec{HeadLines: 22, TailLines: 11}, spec)

	_, ok = got["no-policy-model"]
	assert.False(t, ok, "a row that never set truncation_policy must not appear in the derived table")
}
