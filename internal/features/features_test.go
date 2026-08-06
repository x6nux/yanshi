package features

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRegistryDefaultsRuntimeSetAndList.
//
// ledger: C4/OBS3#1 flag 注册/切换
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

// TestRegistryStrictRejectsUnknownByNameAtomically.
//
// ledger: C4/OBS3#2 strict mode 报错未知 flag
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

// TestExampleConfigNamesEveryFlag keeps the operator-facing surface honest.
//
// config.example.yaml is what people copy to make their config.yaml, so it is
// the layer closest to the user. It carried `overrides: {}` and never named a
// single flag, which meant the only way to discover what could go in there was
// to read DefaultSpecs in Go source. That gap was harmless while the flags did
// nothing; it stopped being harmless the moment two of them acquired real
// consumption points.
//
// Reading the file rather than asserting a hand-written list is the point: a
// fourth flag added to DefaultSpecs and forgotten here fails immediately.
func TestExampleConfigNamesEveryFlag(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	specs := DefaultSpecs()
	if len(specs) == 0 {
		t.Fatal("no specs: the assertion below would be vacuous")
	}
	for _, spec := range specs {
		if !strings.Contains(body, spec.Key) {
			t.Errorf("config.example.yaml never mentions %q: an operator filling in "+
				"features.overrides has no way to learn the name exists", spec.Key)
		}
	}
}
