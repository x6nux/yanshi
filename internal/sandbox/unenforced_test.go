package sandbox

import (
	"reflect"
	"strings"
	"testing"
)

// TestUnenforcedFieldsSubtractsTheDeclaration pins the direction of the
// subtraction (W-B-13).
//
// The whole gate is one boolean's worth of arithmetic, and getting it backwards
// produces a list that looks plausible and warns about the wrong half. So both
// directions are asserted on the same config: a backend declaring nothing warns
// about everything requested, and one declaring everything warns about nothing.
func TestUnenforcedFieldsSubtractsTheDeclaration(t *testing.T) {
	cfg := Config{
		Enabled:       true,
		WorkspaceRoot: "/w",
		Tier:          WorkspaceWrite,
		NetworkDeny:   true,
		ProxyURL:      "http://127.0.0.1:8080",
	}
	all := []string{FieldTier, FieldWorkspaceRoot, FieldNetworkDeny, FieldProxyURL}

	if got := UnenforcedFields(cfg); !reflect.DeepEqual(got, all) {
		t.Fatalf("declaring nothing enforced: got %v, want %v", got, all)
	}
	if got := UnenforcedFields(cfg, all...); len(got) != 0 {
		t.Fatalf("declaring everything enforced: got %v, want empty", got)
	}
	// The per-field case: exactly the field left out of the declaration.
	got := UnenforcedFields(cfg, FieldTier, FieldWorkspaceRoot, FieldProxyURL)
	if !reflect.DeepEqual(got, []string{FieldNetworkDeny}) {
		t.Fatalf("partial declaration: got %v, want [%s]", got, FieldNetworkDeny)
	}
}

// TestUnenforcedFieldsIsSilentWhenNothingWasRequested keeps the warning list
// from firing on the configuration that asked for nothing.
//
// full-access with no network denial and no proxy is "the sandbox restricts
// nothing", so a backend that enforces nothing has failed to honour nothing. A
// warning here would appear for every operator running the widest setting,
// which is how a warning list becomes scroll-back.
func TestUnenforcedFieldsIsSilentWhenNothingWasRequested(t *testing.T) {
	cfg := Config{Enabled: true, WorkspaceRoot: "/w", Tier: FullAccess}
	if got := UnenforcedFields(cfg); len(got) != 0 {
		t.Fatalf("full-access config produced warnings %v, want none", got)
	}
	// A disabled sandbox at a restrictive tier DOES warn: the operator wrote
	// a restriction and nothing carries it.
	off := Config{Enabled: false, WorkspaceRoot: "/w", Tier: ReadOnly, NetworkDeny: true}
	if got := New(off).Report().Unenforced; len(got) != 3 {
		t.Fatalf("disabled sandbox report.Unenforced = %v, want tier+workspace_root+network_deny", got)
	}
}

// TestLandlockDeclarationTracksSeccomp is the reason W-B-13 is not satisfied by
// the existing Enforced bool.
//
// Landlock without a seccomp filter reports Enforced=true and Effective=
// os-isolated — both true statements about the filesystem — while
// `network_deny: true` is completely inert. Only a per-field declaration can
// say that, and this asserts the flag actually moves the field.
func TestLandlockDeclarationTracksSeccomp(t *testing.T) {
	withSeccomp := landlockEnforcedFields(true)
	withoutSeccomp := landlockEnforcedFields(false)
	if !contains(withSeccomp, FieldNetworkDeny) {
		t.Fatalf("seccomp present but %s not declared enforced: %v", FieldNetworkDeny, withSeccomp)
	}
	if contains(withoutSeccomp, FieldNetworkDeny) {
		t.Fatalf("no seccomp yet %s declared enforced: %v", FieldNetworkDeny, withoutSeccomp)
	}
	// The filesystem half is enforced either way — a declaration that dropped
	// it when seccomp is missing would over-warn and teach operators to ignore
	// the list.
	for _, f := range []string{FieldTier, FieldWorkspaceRoot} {
		if !contains(withoutSeccomp, f) {
			t.Fatalf("%s must be enforced with or without seccomp: %v", f, withoutSeccomp)
		}
	}
}

// TestWindowsUnenforcedNoteAgreesWithUnenforcedFields holds the prose and the
// machine-readable list to the same facts.
//
// windowsUnenforcedNote predates W-B-13 and says the same thing in a sentence
// bootstrap logs. Two independent renderings of one fact is exactly the shape
// that drifts, and the drift is invisible: the sentence and the list are read
// by different consumers (a human reading a log line, and doctor branching on
// a slice), so neither reader can notice the other has gone stale.
//
// Only the two fields the note covers are compared. Extending the note to
// workspace_root/proxy_url would be a change to the log line, not a bug.
func TestWindowsUnenforcedNoteAgreesWithUnenforcedFields(t *testing.T) {
	cases := []Config{
		{Enabled: true, WorkspaceRoot: "C:/w", Tier: FullAccess},
		{Enabled: true, WorkspaceRoot: "C:/w", Tier: WorkspaceWrite},
		{Enabled: true, WorkspaceRoot: "C:/w", Tier: FullAccess, NetworkDeny: true},
		{Enabled: true, WorkspaceRoot: "C:/w", Tier: ReadOnly, NetworkDeny: true},
	}
	for _, cfg := range cases {
		note := windowsUnenforcedNote(cfg)
		rep := windowsJobReport(cfg, jobProbe{Created: true, LimitsApplied: true, KillOnCloseObserved: true})
		if strings.Contains(note, "tier=") != contains(rep.Unenforced, FieldTier) {
			t.Fatalf("cfg %+v: note %q disagrees with Unenforced %v on %s",
				cfg, note, rep.Unenforced, FieldTier)
		}
		if strings.Contains(note, "network-deny") != contains(rep.Unenforced, FieldNetworkDeny) {
			t.Fatalf("cfg %+v: note %q disagrees with Unenforced %v on %s",
				cfg, note, rep.Unenforced, FieldNetworkDeny)
		}
		// The probe SUCCEEDED in every case above, so this backend is at its
		// most capable — and still enforces no access field. A Job Object is a
		// lifetime control; a report claiming otherwise on the happy path is
		// the failure this whole field exists to prevent.
		if cfg.Tier != FullAccess && !contains(rep.Unenforced, FieldTier) {
			t.Fatalf("cfg %+v: a containing job object claimed to enforce the tier", cfg)
		}
	}
}
