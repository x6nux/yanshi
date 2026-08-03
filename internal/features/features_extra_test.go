package features

import "testing"

// TestRegisterPanicsOnMissingRequiredFields covers the programmer-error path:
// a spec missing key/stage/owner is a boot-time bug and must panic rather than
// silently register an incomplete flag.
func TestRegisterPanicsOnMissingRequiredFields(t *testing.T) {
	cases := []Spec{
		{Key: "", Stage: Stable, Owner: "o"},  // missing key
		{Key: "k", Stage: "", Owner: "o"},     // missing stage
		{Key: "k", Stage: Stable, Owner: ""},  // missing owner
		{Key: "k", Stage: Stable, Owner: "o"}, // valid (control)
	}
	for i, spec := range cases {
		missing := spec.Key == "" || spec.Stage == "" || spec.Owner == ""
		r := NewRegistry(false)
		panicked := false
		func() {
			defer func() {
				if recover() != nil {
					panicked = true
				}
			}()
			r.Register(spec)
		}()
		if missing != panicked {
			t.Fatalf("case %d %+v: panicked=%v want %v", i, spec, panicked, missing)
		}
	}
}

// TestRegisterPanicsOnDuplicate covers the duplicate-key guard so two specs
// cannot shadow each other during boot.
func TestRegisterPanicsOnDuplicate(t *testing.T) {
	r := NewRegistry(false)
	r.Register(Spec{Key: "dup", Stage: Stable, Default: false, Owner: "o"})
	panicked := false
	func() {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		r.Register(Spec{Key: "dup", Stage: Beta, Default: true, Owner: "o2"})
	}()
	if !panicked {
		t.Fatal("duplicate registration must panic")
	}
}

// TestNilRegistryAccessors covers the nil-safe contract so callers that gate
// behavior behind an optional *Registry do not need a separate disabled check.
func TestNilRegistryAccessors(t *testing.T) {
	var r *Registry
	if r.Enabled("anything") {
		t.Fatal("nil registry must report every flag disabled")
	}
	if rows := r.List(); rows != nil {
		t.Fatalf("nil registry must return nil rows, got %+v", rows)
	}
}

// TestListSortsByStageThenKey covers the alphabetical tiebreak inside the
// sort closure: when two flags share a stage, Key ordering decides. Registering
// two Stable flags forces the tiebreak branch that a single-flag-per-stage
// registry never reaches.
func TestListSortsByStageThenKey(t *testing.T) {
	r := NewRegistry(false)
	r.Register(Spec{Key: "z.same.stage", Stage: Stable, Default: false, Owner: "o"})
	r.Register(Spec{Key: "a.same.stage", Stage: Stable, Default: true, Owner: "o"})
	r.Register(Spec{Key: "m.beta", Stage: Beta, Default: false, Owner: "o"})
	r.Register(Spec{Key: "n.experimental", Stage: Experimental, Default: false, Owner: "o"})

	rows := r.List()
	// Stable group first, alphabetically; then Beta; then Experimental.
	wantKeys := []string{"a.same.stage", "z.same.stage", "m.beta", "n.experimental"}
	if len(rows) != len(wantKeys) {
		t.Fatalf("rows=%+v", rows)
	}
	for i, row := range rows {
		if row.Key != wantKeys[i] {
			t.Fatalf("pos %d = %q want %q; full order %+v", i, row.Key, wantKeys[i], rows)
		}
	}
	// Enabled state is carried through verbatim.
	enabledByName := map[string]bool{}
	for _, row := range rows {
		enabledByName[row.Key] = row.Enabled
	}
	if !enabledByName["a.same.stage"] || enabledByName["z.same.stage"] {
		t.Fatalf("default values not reflected: %+v", enabledByName)
	}
}
