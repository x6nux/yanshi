package features

import (
	"strings"
	"testing"
)

func TestRegistryDefaultsRuntimeSetAndList(t *testing.T) {
	r := NewRegistry(false)
	for _, spec := range DefaultSpecs() {
		r.Register(spec)
	}
	if !r.Enabled("observe.slog_trace_id") {
		t.Fatal("stable default must be enabled")
	}
	if r.Enabled("observe.otel_export") {
		t.Fatal("experimental default must be disabled")
	}
	if err := r.Set("observe.otel_export", true); err != nil {
		t.Fatal(err)
	}
	if !r.Enabled("observe.otel_export") {
		t.Fatal("runtime set did not apply")
	}
	rows := r.List()
	if len(rows) != len(DefaultSpecs()) {
		t.Fatalf("rows = %d", len(rows))
	}
	for _, row := range rows {
		if row.Owner == "" || row.Stage == "" {
			t.Fatalf("incomplete row: %+v", row)
		}
	}
}

func TestRegistryStrictRejectsUnknownByNameAtomically(t *testing.T) {
	r := NewRegistry(true)
	r.Register(Spec{Key: "known", Stage: Stable, Default: false, Owner: "test"})
	err := r.ApplyMap(map[string]bool{"known": true, "typo_flag": true})
	if err == nil || !strings.Contains(err.Error(), "typo_flag") {
		t.Fatalf("expected named unknown error, got %v", err)
	}
	if r.Enabled("known") {
		t.Fatal("strict batch must be atomic")
	}
}

func TestRegistryNonStrictIgnoresUnknown(t *testing.T) {
	r := NewRegistry(false)
	r.Register(Spec{Key: "known", Stage: Beta, Default: false, Owner: "test"})
	if err := r.ApplyMap(map[string]bool{"known": true, "ignored": true}); err != nil {
		t.Fatal(err)
	}
	if !r.Enabled("known") || r.Enabled("ignored") {
		t.Fatalf("unexpected state: %+v", r.List())
	}
}

func TestRegistrySetAlwaysRejectsUnknown(t *testing.T) {
	r := NewRegistry(false)
	if err := r.Set("missing", true); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("runtime set must reject unknown key: %v", err)
	}
}
