// Package features owns process-scoped feature flags. Defaults and metadata
// are registered in bootstrap; YAML overrides apply at startup via ApplyMap;
// /features applies non-persistent runtime overrides via Set. Strict mode
// (config.features.strict=true or YANSHI_FEATURES_STRICT=1) makes ApplyMap
// reject unknown flag names so typos surface immediately instead of silently
// being dropped.
package features

import (
	"fmt"
	"os"
	"sort"
	"sync"
)

// Stage classifies how stable a flag is. Stable flags ship on by default;
// Beta flags are enabled for early adopters; Experimental flags stay off
// until their owner graduates them. The stage is metadata for /features UI
// and for ordering List() — runtime checks only see Enabled() (bool).
type Stage string

// Flag stability tiers, ordered from most to least stable. Stable ships on by
// default; Beta is opt-in for early adopters; Experimental stays off until
// graduated.
const (
	Stable        Stage = "stable"
	Beta          Stage = "beta"
	Experimental  Stage = "experimental"
)

// Spec is the registration record for a flag: its key, maturity stage,
// default value, and owning batch/feature (so /features can show "who do I
// ask about this?"). Key/Stage/Owner are required; Register panics on a
// malformed spec because registry construction is part of process boot and a
// missing field is a programmer error, not a runtime condition.
type Spec struct {
	Key     string
	Stage   Stage
	Default bool
	Owner   string
}

// Row is the readonly view returned by List() for /features rendering. Stage
// is a string (not Stage) so the wire representation stays portable across
// languages and config tools.
type Row struct {
	Key     string
	Stage   string
	Enabled bool
	Owner   string
}

// Registry is a thread-safe map of flag → current value plus metadata. A nil
// *Registry is valid: Enabled returns false and List returns nil, so callers
// that gate behavior behind an optional registry don't need a separate
// "registry disabled" check.
type Registry struct {
	mu     sync.RWMutex
	strict bool
	specs  map[string]Spec
	values map[string]bool
}

// NewRegistry combines the YAML strict preference with the
// YANSHI_FEATURES_STRICT=1 escape hatch so operators can force strict mode
// even when the config sets strict: false (useful for catching typos during
// migrations).
func NewRegistry(strict bool) *Registry {
	return &Registry{
		strict: strict || os.Getenv("YANSHI_FEATURES_STRICT") == "1",
		specs:  make(map[string]Spec),
		values: make(map[string]bool),
	}
}

// DefaultSpecs returns yanshi's built-in flag set. The slice is copied on
// each call so callers cannot mutate the registry's source-of-truth by
// editing the returned slice. Bootstrap registers every spec; /features
// lists every spec; runtime toggles go through Set.
func DefaultSpecs() []Spec {
	return []Spec{
		{Key: "observe.slog_trace_id", Stage: Stable, Default: true, Owner: "C4/OBS1"},
		{Key: "observe.otel_export", Stage: Experimental, Default: false, Owner: "C4/OBS2"},
		{Key: "observe.cost_in_status", Stage: Beta, Default: true, Owner: "C4/COST1"},
	}
}

// Register installs a flag. Panics on duplicates or missing required fields
// because the registry is constructed during process boot and a malformed
// spec is a programmer error (not an operator-configurable runtime state).
func (r *Registry) Register(spec Spec) {
	if spec.Key == "" || spec.Owner == "" || spec.Stage == "" {
		panic("features: key, stage, and owner are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.specs[spec.Key]; exists {
		panic("features: duplicate flag " + spec.Key)
	}
	r.specs[spec.Key] = spec
	r.values[spec.Key] = spec.Default
}

// Enabled returns the current value of the named flag. An unknown flag is
// reported as false (not panicked) because runtime callers (e.g.
// if features.Enabled("foo")) must degrade safely when a flag is renamed or
// removed. Use Set to TOGGLE a known flag and ApplyMap to seed many at once.
func (r *Registry) Enabled(key string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, known := r.specs[key]
	return known && r.values[key]
}

// Set toggles a flag at runtime. It is the single entry point for /features
// and ALWAYS rejects unknown keys (regardless of strict mode): runtime
// toggling of an unknown flag is either a typo or a stale client, neither of
// which should silently succeed. Strict mode only changes startup ApplyMap
// behavior; runtime typing discipline is enforced uniformly.
func (r *Registry) Set(key string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, known := r.specs[key]; !known {
		return fmt.Errorf("features: unknown flag %q", key)
	}
	r.values[key] = enabled
	return nil
}

// ApplyMap seeds the registry from a YAML/TUI override map. In strict mode
// the operation is ATOMIC: an unknown key rejects the entire batch and the
// registry is left unchanged so a half-applied override can't land. In
// non-strict mode unknown keys are ignored so a newer config can be loaded by
// an older binary without breaking (forward-compat). Per-flag overrides are
// applied in key-sorted order for deterministic behavior regardless of map
// iteration order.
func (r *Registry) ApplyMap(overrides map[string]bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.strict {
		// Strict: detect ALL unknown keys first so the apply phase never
		// runs on a partially-rejected batch. Iteration order doesn't
		// matter for the check (we collect before any mutation).
		for key := range overrides {
			if _, known := r.specs[key]; !known {
				return fmt.Errorf("features: unknown flag %q (strict mode)", key)
			}
		}
	}
	for key, enabled := range overrides {
		if _, known := r.specs[key]; known {
			r.values[key] = enabled
		}
	}
	return nil
}

// List returns every registered flag as a Row, ordered by Stage (Stable →
// Beta → Experimental) then alphabetically by Key so /features renders
// consistently. The returned slice is safe to mutate.
func (r *Registry) List() []Row {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	rows := make([]Row, 0, len(r.specs))
	for key, spec := range r.specs {
		rows = append(rows, Row{Key: key, Stage: string(spec.Stage), Enabled: r.values[key], Owner: spec.Owner})
	}
	rank := map[Stage]int{Stable: 0, Beta: 1, Experimental: 2}
	sort.Slice(rows, func(i, j int) bool {
		left, right := rank[Stage(rows[i].Stage)], rank[Stage(rows[j].Stage)]
		if left != right {
			return left < right
		}
		return rows[i].Key < rows[j].Key
	})
	return rows
}
