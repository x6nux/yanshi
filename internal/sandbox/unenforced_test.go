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
// contains reports whether xs holds want. Shared by the untagged tests and
// by the !windows bwrap/landlock argv tests (the windows leg compiles this
// file but not those: their assertions describe the linux implementations,
// and windows builds only stubs).
func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
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
	all := []string{FieldTier, FieldWorkspaceRoot, FieldNetworkDeny}

	if got := UnenforcedFields(cfg); !reflect.DeepEqual(got, all) {
		t.Fatalf("declaring nothing enforced: got %v, want %v", got, all)
	}
	if got := UnenforcedFields(cfg, all...); len(got) != 0 {
		t.Fatalf("declaring everything enforced: got %v, want empty", got)
	}
	// The per-field case: exactly the field left out of the declaration.
	got := UnenforcedFields(cfg, FieldTier, FieldWorkspaceRoot)
	if !reflect.DeepEqual(got, []string{FieldNetworkDeny}) {
		t.Fatalf("partial declaration: got %v, want [%s]", got, FieldNetworkDeny)
	}
}

// TestRequestedFieldsIgnoresProxyURL is the fix for W-B fix-b57 finding 2:
// doctor's checkSandbox builds a sandbox.Config without ProxyURL (it never
// starts the managed proxy) while bootstrap's real Config always carries it
// once the proxy is up, for a config the operator wrote identically in both
// places. ProxyURL used to be read by requestedFields, so those two Configs
// produced different Unenforced lists — the runtime warned "configured but
// NOT enforced by this backend: proxy_url" and doctor stayed silent — for a
// key that was never in doctor's control (`proxy_url` is not a
// security.sandbox YAML key at all; config.example.yaml only has
// enabled/tier/network_deny under it).
//
// This is checked at the requestedFields level rather than by re-deriving
// doctor's and bootstrap's two Config literals here: any future field with
// the same "internal wiring, not an operator setting" shape would recreate
// the disagreement the same way, and the fix is the same in both cases —
// requestedFields must be blind to it.
func TestRequestedFieldsIgnoresProxyURL(t *testing.T) {
	base := Config{Enabled: true, Tier: WorkspaceWrite, WorkspaceRoot: "/w", NetworkDeny: true}
	withProxy := base
	withProxy.ProxyURL = "http://127.0.0.1:8080"

	got, want := requestedFields(withProxy), requestedFields(base)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requestedFields differs by ProxyURL alone: with-proxy=%v without=%v — "+
			"this is exactly why doctor (never sets ProxyURL) and bootstrap (always does) "+
			"disagreed about the SAME operator configuration", got, want)
	}
	for _, f := range got {
		if f == FieldProxyURL {
			t.Fatalf("proxy_url must never appear in requestedFields: %v", got)
		}
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
