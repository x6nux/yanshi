package eino

import (
	_ "embed"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// This file is the W-C-01 (INF2) data-driven model capability catalog. It
// replaces two Go literal tables — contextwindow.go's former
// contextWindowCatalog var and pricing.go's former DefaultPricing body —
// with one embedded YAML file (models.yaml) parsed once at package init.
// Adding a model, or repricing an existing one, is now an edit to
// models.yaml; no Go code changes and no rebuild-from-source-tree step.
//
// See models.yaml's header comment for the row schema and the "two kinds of
// row" design (family rows carry context_window, specific rows carry
// pricing). This file only adds the parsing and the three derived indexes
// (context-window catalog, pricing table, auto-compact-threshold table)
// that contextwindow.go, pricing.go and the compaction wiring consume.

//go:embed models.yaml
var modelsYAML []byte

// modelCatalogFile is the top-level shape of models.yaml.
type modelCatalogFile struct {
	Models []modelCatalogRow `yaml:"models"`
}

// modelCatalogRow is one entry in models.yaml — either a family catch-all
// (context_window only) or a specific snapshot (pricing only), per that
// file's "two kinds of row" design. A row may also carry both, or neither
// meaningful field yet (the max_output/modalities/reasoning_efforts/
// priority fields are schema-only in this ticket — parsed so a future W-C
// ticket can populate them without a schema migration, not consumed by any
// Go code yet. truncation_policy USED to be on this list — W-C-09 wired it
// up; see KnownTruncationPolicy for the same "mechanism real, data empty"
// shape RC-3 shipped auto_compact_threshold under, now Ruling RC-9).
type modelCatalogRow struct {
	ID                   string   `yaml:"id"`
	Aliases              []string `yaml:"aliases"`
	ContextWindow        int      `yaml:"context_window"`
	AutoCompactThreshold float64  `yaml:"auto_compact_threshold"`
	// FallbackModels (W-C-10) is an ordered list of model ids this model
	// falls back to when its own compaction summary call fails outright
	// (exhausted retries, quota, overload — the same failure RunSummary
	// reports and W-C-04's pins-only path exists to survive). Declared ids
	// that are not configured providers in THIS deployment are skipped at
	// resolution time, not an error — the catalog is compiled into the
	// binary and cannot know which providers an operator actually
	// configured. See models.yaml's header for Ruling RC-8: this field ships
	// ZERO populated rows in this round, the same "field/parser/resolver are
	// real, only the DATA is missing" shape AutoCompactThreshold shipped
	// under Ruling RC-3 (which explicitly anticipated this: "并且给 C4 批记
	// 一条前置 — W-C-04 / W-C-10 都会假设这张表里有东西").
	FallbackModels   []string         `yaml:"fallback_models"`
	Pricing          *modelPricingRow `yaml:"pricing"`
	MaxOutput        int              `yaml:"max_output"`
	Modalities       []string         `yaml:"modalities"`
	ReasoningEfforts []string         `yaml:"reasoning_efforts"`
	TruncationPolicy string           `yaml:"truncation_policy"`
	Priority         int              `yaml:"priority"`
}

// modelPricingRow is the USD-per-million-token shape inside a
// modelCatalogRow, mirroring ModelPricing's fields under the YAML names
// models.yaml documents.
type modelPricingRow struct {
	InputPerM    float64 `yaml:"input_per_m"`
	CacheHitPerM float64 `yaml:"cache_hit_per_m"`
	OutputPerM   float64 `yaml:"output_per_m"`
}

// modelCatalog is the parsed models.yaml, built once at package init.
// KnownContextWindow, DefaultPricing, KnownAutoCompactThreshold and
// BuildProviders' unknown-model degradation path all read this same parse —
// there is exactly one place the embedded bytes are turned into structs.
var modelCatalog = mustParseModelCatalog(modelsYAML)

// mustParseModelCatalog parses raw models.yaml bytes and panics on a
// malformed file, an empty id, a duplicate id, or a truncation_policy string
// that does not parse.
//
// This is a BUILD-TIME defect check, not the runtime-safe "model not found"
// axis — those are different questions with different answers. A model this
// binary has never heard of must degrade to a safe default and keep the
// server running (see ResolveContextWindow's fallback ladder); a models.yaml
// that fails to parse, or that defines the same id twice with silently
// different values, is a mistake in the data THIS BINARY SHIPS WITH, caught
// at package init exactly like a syntax error would be caught at compile
// time — before any request depends on which of the two rows won.
//
// The truncation_policy check is the same reasoning applied to one field: a
// row that sets it to something ParseTruncationPolicy rejects is a typo in
// data this binary ships with, not a value that reached us from an operator
// (that path — ProviderConfig.TruncationPolicy — degrades instead of
// panicking; see ResolveTruncationPolicy's doc comment for why the two
// sources get different failure modes). The shipped file currently has zero
// rows setting this field (Ruling RC-9), so this branch is presently
// unreachable in production; it exists so the first row that DOES set it
// gets the same load-bearing scrutiny id/duplicate already get.
func mustParseModelCatalog(raw []byte) modelCatalogFile {
	var file modelCatalogFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		panic(fmt.Sprintf("eino: models.yaml is malformed: %v", err))
	}
	seen := make(map[string]bool, len(file.Models))
	for _, row := range file.Models {
		id := strings.ToLower(strings.TrimSpace(row.ID))
		if id == "" {
			panic("eino: models.yaml has a row with an empty id")
		}
		if seen[id] {
			panic(fmt.Sprintf("eino: models.yaml has a duplicate id %q", id))
		}
		if row.TruncationPolicy != "" {
			if _, ok := ParseTruncationPolicy(row.TruncationPolicy); !ok {
				panic(fmt.Sprintf("eino: models.yaml row %q has an unparsable truncation_policy %q (want e.g. \"head=15,tail=10\")", id, row.TruncationPolicy))
			}
		}
		seen[id] = true
	}
	return file
}

// catalogAliases returns the lowercased, trimmed id and aliases a row
// answers for, in the order they should be indexed. Used by the two
// EXACT-match indexes (pricing, auto-compact-threshold) — context-window
// resolution instead matches these same strings as boundary-anchored
// substring patterns (see contextwindow.go's file comment for why the two
// resolution modes differ).
func catalogAliases(row modelCatalogRow) []string {
	out := make([]string, 0, 1+len(row.Aliases))
	if id := strings.ToLower(strings.TrimSpace(row.ID)); id != "" {
		out = append(out, id)
	}
	for _, a := range row.Aliases {
		if a := strings.ToLower(strings.TrimSpace(a)); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// buildContextWindowCatalog projects the rows that carry a context_window
// into the []contextWindowEntry shape contextwindow.go's boundary-substring
// matcher consumes. Rows with no window opinion (ContextWindow <= 0, e.g.
// pricing-only specific-snapshot rows) contribute nothing here — that is
// what lets a family row remain the sole authority on window size for every
// snapshot that does not override it.
func buildContextWindowCatalog(cat modelCatalogFile) []contextWindowEntry {
	var out []contextWindowEntry
	for _, row := range cat.Models {
		if row.ContextWindow <= 0 {
			continue
		}
		for _, pattern := range catalogAliases(row) {
			out = append(out, contextWindowEntry{pattern: pattern, tokens: row.ContextWindow})
		}
	}
	return out
}

// buildPricingCatalog projects the rows that carry pricing into the
// map[string]ModelPricing shape DefaultPricing has always returned. Matching
// is EXACT (map key), not boundary-substring — see models.yaml's header for
// why pricing does not penetrate gateway prefixes the way context_window
// does.
func buildPricingCatalog(cat modelCatalogFile) map[string]ModelPricing {
	out := make(map[string]ModelPricing)
	for _, row := range cat.Models {
		if row.Pricing == nil {
			continue
		}
		price := ModelPricing{
			InputPerM:    row.Pricing.InputPerM,
			CacheHitPerM: row.Pricing.CacheHitPerM,
			OutputPerM:   row.Pricing.OutputPerM,
		}
		for _, alias := range catalogAliases(row) {
			out[alias] = price
		}
	}
	return out
}

// buildAutoCompactThresholds projects the rows that carry
// auto_compact_threshold into an exact-match id/alias -> fraction table,
// mirroring buildPricingCatalog. The value is a FRACTION of the resolved
// context window (e.g. 0.8), never an absolute token count — see
// models.yaml's header comment.
func buildAutoCompactThresholds(cat modelCatalogFile) map[string]float64 {
	out := make(map[string]float64)
	for _, row := range cat.Models {
		if row.AutoCompactThreshold <= 0 {
			continue
		}
		for _, alias := range catalogAliases(row) {
			out[alias] = row.AutoCompactThreshold
		}
	}
	return out
}

// buildFallbackModels projects the rows that carry fallback_models into an
// exact-match id/alias -> ordered-id-list table, mirroring
// buildAutoCompactThresholds. A row's own aliases all answer with the SAME
// fallback list (an alias is a name for the same model, not a different
// model with its own opinion) — consistent with how every other exact-match
// index in this file (pricing, auto-compact-threshold) treats aliases.
func buildFallbackModels(cat modelCatalogFile) map[string][]string {
	out := make(map[string][]string)
	for _, row := range cat.Models {
		if len(row.FallbackModels) == 0 {
			continue
		}
		ids := make([]string, 0, len(row.FallbackModels))
		for _, id := range row.FallbackModels {
			if id := strings.ToLower(strings.TrimSpace(id)); id != "" {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			continue
		}
		for _, alias := range catalogAliases(row) {
			out[alias] = ids
		}
	}
	return out
}

// buildTruncationPolicies projects the rows that carry a (parsable)
// truncation_policy into an exact-match id/alias -> TruncationSpec table,
// mirroring buildAutoCompactThresholds. A row whose string fails to parse
// contributes nothing here — mustParseModelCatalog already panicked on that
// row before this function runs, so in practice every row reaching this loop
// either has an empty TruncationPolicy (skipped, ok==false) or one
// ParseTruncationPolicy accepts.
func buildTruncationPolicies(cat modelCatalogFile) map[string]TruncationSpec {
	out := make(map[string]TruncationSpec)
	for _, row := range cat.Models {
		spec, ok := ParseTruncationPolicy(row.TruncationPolicy)
		if !ok {
			continue
		}
		for _, alias := range catalogAliases(row) {
			out[alias] = spec
		}
	}
	return out
}

// autoCompactThresholds is the catalog-sourced auto-compact-threshold table,
// built once at package init from the same modelCatalog parse everything
// else in this file reads.
var autoCompactThresholds = buildAutoCompactThresholds(modelCatalog)

// fallbackModels is the catalog-sourced fallback-chain table (W-C-10), built
// once at package init from the same modelCatalog parse everything else in
// this file reads.
var fallbackModels = buildFallbackModels(modelCatalog)

// truncationPolicies is the catalog-sourced tool-output truncation-policy
// table (W-C-09), built once at package init from the same modelCatalog
// parse everything else in this file reads.
var truncationPolicies = buildTruncationPolicies(modelCatalog)

// KnownFallbackModels returns the ordered list of model ids modelID falls
// back to when its own compaction summary call fails, and whether the
// catalog had an opinion. A false second return (or an empty, ok=true list —
// callers should treat both the same way) means modelID has no declared
// fallback chain: the caller keeps using modelID alone, exactly the
// behavior that predates W-C-10.
//
// The shipped models.yaml currently populates ZERO fallback_models rows
// (Ruling RC-8, models.yaml's header): the field is live and consumed, it
// just has no data yet — no per-model fallback chain has a documented
// real-world basis (which models are actually suitable, quality-compatible
// stand-ins for which primary model) to ship with. Until a future ticket
// adds rows, this always returns (nil, false) and every caller of this
// function is a no-op, byte-identical to pre-W-C-10 behavior.
//
// Matching is EXACT against id/aliases (see catalogAliases), not the
// boundary-substring matching KnownContextWindow uses — same choice
// KnownAutoCompactThreshold makes and for the same reason: a fallback chain
// is a per-model editorial opinion, not a property that should leak across
// every snapshot sharing a family prefix.
func KnownFallbackModels(modelID string) ([]string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(modelID))
	if normalized == "" {
		return nil, false
	}
	ids, ok := fallbackModels[normalized]
	return ids, ok
}

// ResolveFallbackModels (M-2 / W-C-10) returns modelID's compaction-summary
// fallback chain and whether anything (config or catalog) had an opinion,
// mirroring ResolveTruncationPolicy's "explicit override, then catalog"
// shape — override wins outright when non-empty, exactly like
// ProviderConfig.FallbackModels' own doc comment promises. It does not
// merge override and catalog entries: a provider that sets fallback_models
// gets EXACTLY that chain, not that chain plus whatever the catalog also
// knows, so a deliberately short (or single-entry) override cannot be
// silently lengthened by a future catalog row.
//
// override is unresolved config data (declared model ids, not live
// model.BaseChatModel objects) — bootstrap's buildProviderFallbacks does
// that resolution afterwards, dropping any id that is not a key in the
// registry this deployment actually built. An override entry naming a model
// no configured provider resolves to is therefore a silently shorter chain,
// the same tolerance KnownFallbackModels' catalog path already has (see its
// doc comment) — not a config validation error, because validating a model
// id here would need the very provider registry this function runs before.
func ResolveFallbackModels(override []string, modelID string) ([]string, bool) {
	if len(override) > 0 {
		return override, true
	}
	return KnownFallbackModels(modelID)
}

// KnownAutoCompactThreshold returns the cataloged auto-compact threshold for
// modelID and whether the catalog knew it. A false second return means "not
// in the table" — the caller keeps its own configured/global threshold
// unchanged, exactly like KnownContextWindow's false case.
//
// The shipped models.yaml currently populates ZERO auto_compact_threshold
// rows (F-4/W-C-03): the field is live and consumed (unlike the five
// genuinely schema-only fields models.yaml's header comment names), it just
// has no data yet — no per-model threshold has a documented real-world basis
// to ship with. Until a future W-C ticket adds rows, this function always
// returns (0, false) and the only way to reach a non-zero auto-compact
// threshold is ProviderConfig.AutoCompactThreshold (the config-file rung of
// the ladder, one level above this one).
//
// Matching is EXACT against id/aliases (see catalogAliases), not the
// boundary-substring matching KnownContextWindow uses.
func KnownAutoCompactThreshold(modelID string) (float64, bool) {
	normalized := strings.ToLower(strings.TrimSpace(modelID))
	if normalized == "" {
		return 0, false
	}
	v, ok := autoCompactThresholds[normalized]
	return v, ok
}

// ResolveAutoCompactThreshold returns the auto-compact threshold for one
// provider and whether anything (config or catalog) had an opinion. A false
// second return means the caller's own fallback (CompactionConfig.Threshold)
// applies unchanged — this function never invents a value.
//
// Precedence mirrors ResolveContextWindow: p.AutoCompactThreshold (explicit
// operator override, config.ProviderConfig.AutoCompactThreshold projected
// through ProviderShape) wins outright when non-zero, then the catalog.
//
// A NEGATIVE p.AutoCompactThreshold is an explicit per-provider DISABLE
// (W-C-04 / F-10), not "unset" — it is returned as-is with ok=true, mirroring
// the global CompactionConfig.Threshold convention ("<= 0 disables") at the
// per-provider layer. Before this, 0 and any negative value both meant
// "unset" (ContextWindow's own "<=0 means unset" convention, deliberately —
// see TestAcceptance3_ConfigOverrideOutranksCatalog), which left an operator
// with a catalog/global auto-compact opinion but no way to opt ONE provider
// out of it. Only the sign is special-cased; the caller (thresholdFor /
// wrapCompaction) is what turns a negative resolved value into "off" for
// that provider, exactly like it already does for the global Threshold.
func ResolveAutoCompactThreshold(p ProviderShape) (float64, bool) {
	if p.AutoCompactThreshold > 0 {
		return p.AutoCompactThreshold, true
	}
	if p.AutoCompactThreshold < 0 {
		return p.AutoCompactThreshold, true
	}
	return KnownAutoCompactThreshold(p.Model)
}

// TruncationSpec (W-C-09) is the resolved head/tail line-retention policy
// for an oversized tool result. internal/tools/spillover.go's spillPreview
// is the sole consumer: it keeps the first HeadLines and last TailLines of
// an over-threshold result, with everything between them replaced by an
// explicit "[... N lines omitted ...]" marker (spillover.go already had that
// marker before this ticket — W-C-09's acceptance criterion for it was
// already met, this type only makes the LINE COUNTS configurable).
//
// Byte budgets (spillover.go's spillHeadBudget/spillTailBudget) are
// deliberately NOT part of this struct: they exist to keep a preview well
// under SpillThreshold even when a few lines are pathologically long, a
// safety cap rather than a per-model editorial opinion, so they stay fixed
// constants in the one package that enforces SpillThreshold.
type TruncationSpec struct {
	// HeadLines is how many lines from the start of an oversized result to
	// keep verbatim (before any byte-budget capping).
	HeadLines int
	// TailLines is how many lines from the end of an oversized result to
	// keep verbatim (before any byte-budget capping).
	TailLines int
}

// DefaultTruncationSpec is applied when neither ProviderConfig.TruncationPolicy
// nor the model catalog has an opinion. It reproduces the exact head/tail
// line counts internal/tools/spillover.go hardcoded before this ticket
// (spillHeadLines=15, spillTailLines=10), so an unconfigured deployment's
// tool-output truncation behaves byte-identically to before W-C-09.
var DefaultTruncationSpec = TruncationSpec{HeadLines: 15, TailLines: 10}

// ParseTruncationPolicy parses the "head=<N>,tail=<M>" string format shared
// by ProviderConfig.TruncationPolicy (the operator-facing override) and
// models.yaml's truncation_policy rows (the catalog). Keys are
// case-insensitive, order-independent, and either may be omitted — an
// omitted key keeps DefaultTruncationSpec's value for that side, so
// "head=30" alone only changes the head retention. Whitespace around commas,
// "=", and values is tolerated.
//
// A false second return means s did not describe a policy this function
// understands: empty/blank input, a segment with no "=", an unknown key, or
// a value that is not a positive integer. There is no partial-success case —
// one bad segment invalidates the whole string, rather than silently mixing
// a parsed head with a default tail, because that would make the SAME
// malformed input produce two different-looking but equally silent
// failures depending on which key came first.
//
// The false case is intentionally UNDIFFERENTIATED from "s was empty": both
// mean "this input has no opinion", so callers (KnownTruncationPolicy's
// empty-modelID guard, ResolveTruncationPolicy's override branch) can treat
// "not configured" and "configured wrong" as the same "fall through to the
// next rung" signal — see ResolveTruncationPolicy's doc comment for why an
// operator typo degrades instead of failing closed.
func ParseTruncationPolicy(s string) (TruncationSpec, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return TruncationSpec{}, false
	}
	spec := DefaultTruncationSpec
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, val, ok := strings.Cut(part, "=")
		if !ok {
			return TruncationSpec{}, false
		}
		n, err := strconv.Atoi(strings.TrimSpace(val))
		if err != nil || n <= 0 {
			return TruncationSpec{}, false
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "head":
			spec.HeadLines = n
		case "tail":
			spec.TailLines = n
		default:
			return TruncationSpec{}, false
		}
	}
	return spec, true
}

// KnownTruncationPolicy returns the cataloged head/tail retention policy for
// modelID and whether the catalog knew it. A false second return means "not
// in the table" — the caller keeps its own fallback (DefaultTruncationSpec),
// exactly like KnownAutoCompactThreshold's false case.
//
// The shipped models.yaml currently populates ZERO truncation_policy rows
// (Ruling RC-9, W-C-09): the field is live and consumed — unlike the four
// genuinely schema-only fields (max_output/modalities/reasoning_efforts/
// priority) models.yaml's header comment names — it just has no data yet,
// the same "mechanism real, data empty" shape RC-3 shipped
// auto_compact_threshold under. Until a future ticket adds rows, this always
// returns (TruncationSpec{}, false) and the only way to reach a non-default
// policy is ProviderConfig.TruncationPolicy.
//
// Matching is EXACT against id/aliases (see catalogAliases), mirroring
// KnownAutoCompactThreshold rather than KnownContextWindow's boundary-
// substring matching — how much tool output a model's own turns should keep
// is a per-model editorial opinion, not something that should leak across a
// whole gateway-prefixed family.
func KnownTruncationPolicy(modelID string) (TruncationSpec, bool) {
	normalized := strings.ToLower(strings.TrimSpace(modelID))
	if normalized == "" {
		return TruncationSpec{}, false
	}
	v, ok := truncationPolicies[normalized]
	return v, ok
}

// ResolveTruncationPolicy returns the truncation policy for one model and
// whether anything (the operator override or the catalog) had an opinion. A
// false second return means the caller's own fallback (DefaultTruncationSpec)
// applies unchanged — this function never invents a value, matching every
// other Resolve* function in this file.
//
// override is ProviderConfig.TruncationPolicy verbatim (NOT projected
// through ProviderShape, unlike ResolveContextWindow/
// ResolveAutoCompactThreshold): this function only ever reads two scalars
// (a string and a model id), so threading Kind/BaseURL/Local through a
// ProviderShape just to satisfy the sibling functions' call shape would bind
// unused fields for no reason — see the file's other Resolve* functions for
// where those fields actually matter (the local/cloud heuristic).
//
// A malformed override (ParseTruncationPolicy's false case) is NOT a load
// error: it falls through to the catalog exactly like an EMPTY override
// would. This differs from ProviderConfig.AutoCompactThreshold, which IS
// validated at config load (Config.validateProfiles rejects a value > 1) —
// that check is a plain numeric range test with no dependency on this
// package. A truncation_policy string's validity depends on
// ParseTruncationPolicy, and internal/config cannot import this package
// (internal/llm/eino already imports internal/config for provider shaping;
// the reverse import would be a cycle). The bootstrap call site
// (internal/bootstrap/bootstrap.go) logs a warning when override is
// non-empty but this function's second return is false, so a typo is still
// observable — just at boot-time log level, not a refused start.
func ResolveTruncationPolicy(override string, modelID string) (TruncationSpec, bool) {
	if spec, ok := ParseTruncationPolicy(override); ok {
		return spec, true
	}
	return KnownTruncationPolicy(modelID)
}
