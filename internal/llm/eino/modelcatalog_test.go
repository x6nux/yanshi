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
// the five fields (max_output, modalities, reasoning_efforts,
// truncation_policy, priority) no Go code consumes yet in this ticket. A
// yaml-tag typo on any of those would silently discard operator data with no
// test noticing until a LATER ticket tries to read it — this test is what
// makes that later ticket's job "read a field", not "discover the tag never
// worked".
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
    truncation_policy: auto
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
	assert.Equal(t, "auto", row.TruncationPolicy)
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
// models.yaml currently populates zero auto_compact_threshold rows (see
// buildAutoCompactThresholds' doc comment), so this is the only way to
// exercise ResolveAutoCompactThreshold's catalog-hit branch against a known
// value instead of always taking the "nothing had an opinion" branch.
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
	assert.LessOrEqual(t, th, 1.0, "a ratio for a plausible operator input must stay in [0,1]; a value > 1 here "+
		"would mean something upstream started treating this as an absolute count")
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
