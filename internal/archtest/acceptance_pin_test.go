package archtest

// Acceptance pins (GOV8, second half) — the promise itself is nailed down.
//
// WHY THIS EXISTS
//
// status_evidence_test.go makes a terminal verdict pay for every acceptance
// clause. It computes the number of clauses FROM the acceptance text, which
// means the ledger author controls the denominator. Deleting four of five
// clauses from `acceptance:`, deleting the four now-orphaned evidence keys and
// deleting the four now-stale `ledger:` markers is a purely mechanical
// three-step edit that requires no semantic judgement — and it leaves the whole
// gate green while the entry keeps its `done` verdict.
//
// That is not a hypothetical hole; it is the exact shape GOV8 claims to close
// ("lower the bar, then declare victory"), reachable from inside the same file
// GOV8 audits. Nothing else in the repository restates the acceptance text, so
// there was no cross-check: docs/feature-status-audit.md is a dated snapshot
// that records verdicts, not per-item acceptance criteria.
//
// THE PIN
//
// Every ledger entry — not just terminal ones, because an entry can be
// quietly trimmed while it is still `partial` and flipped later — carries a
// row here recording the clause count and a digest of the acceptance text as
// last reviewed. Any edit to acceptance turns this test red, and the only way
// to green it is to write the new count and digest into this table.
//
// This does not make acceptance immutable, and it must not: W5 plans to add a
// scope qualifier to A1/S09, and a criterion that cannot be corrected rots.
// What it makes impossible is a SILENT change. A pin update is a line in the
// diff whose "5 -> 1" is legible to anyone, sits next to the ledger edit that
// caused it, and cannot be produced as a side effect of any other work.
//
// LIMITS (see ADR-0011 "边界")
//
// A digest cannot judge whether a rewritten criterion is weaker or merely
// clearer — only that it changed and that a human wrote down the new shape.
// It also pins `acceptance` alone; `title` is prose and is not pinned.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// acceptancePin records the shape of one ledger entry's acceptance text at the
// moment it was last reviewed.
//
// Clauses is redundant with Digest for detection purposes — any clause change
// moves the digest too — and is stored anyway because it is the field a human
// can read. A digest change says "something moved"; a `Clauses: 5` becoming
// `Clauses: 1` says "four promises were dropped", which is the failure this
// table exists to make visible in review.
type acceptancePin struct {
	// Clauses is the number of conjunctive clauses splitClauses found.
	Clauses int
	// Digest is the first 16 hex characters of the SHA-256 of the
	// whitespace-trimmed acceptance text.
	Digest string
}

// acceptanceDigest is the pinned fingerprint of an acceptance string.
//
// The input is trimmed but NOT otherwise normalised: swapping "；" for ";"
// or reflowing the text changes the digest. That is intentional — those edits
// are still edits to the criterion of record, and a gate that silently
// tolerates "harmless" rewrites has to decide which rewrites are harmless.
func acceptanceDigest(acceptance string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(acceptance)))
	return hex.EncodeToString(sum[:])[:16]
}

// acceptancePins pins every entry in docs/feature-status.yaml.
//
// Unlike the exemption tables elsewhere in this package this map does not
// shrink over time — it is a mirror of the ledger, so it has exactly one row
// per entry, and both a missing row and an extra row are errors.
var acceptancePins = map[string]acceptancePin{
	"A1/S06":         {Clauses: 3, Digest: "88cbb3dc5b591ae8"},
	"A1/S07":         {Clauses: 4, Digest: "fd6da443a4756fa4"},
	"A1/S09":         {Clauses: 4, Digest: "251aa30f8fa18956"},
	"A1/T07/T08":     {Clauses: 5, Digest: "763522e7105826af"},
	"A2/DT1":         {Clauses: 4, Digest: "5d7202e3605abaa8"},
	"A2/DT2":         {Clauses: 4, Digest: "2778fd2f7c5598f0"},
	"A2/G05":         {Clauses: 4, Digest: "5634cfc901b378e7"},
	"A3/C13":         {Clauses: 3, Digest: "9874d795bae570cf"},
	"A3/MCP1":        {Clauses: 3, Digest: "28ec4c7efc8cc012"},
	"A3/V16":         {Clauses: 4, Digest: "82f40219a92ecc1e"},
	"B0/TD1":         {Clauses: 5, Digest: "d43d62685df771eb"},
	"B1/M04":         {Clauses: 4, Digest: "5dfb906e8470c2e2"},
	"B1/M04b":        {Clauses: 4, Digest: "a3795cf7939dedca"},
	"B1/M05":         {Clauses: 5, Digest: "96e8ada05d7a2f49"},
	"B2/LSP1":        {Clauses: 4, Digest: "c567847c5f4772d3"},
	"B3/DT4":         {Clauses: 4, Digest: "0f07f355dd6bee69"},
	"B3/DT5":         {Clauses: 3, Digest: "46f18c75814f5269"},
	"B3/GH1":         {Clauses: 5, Digest: "30efab4864db0b4d"},
	"B3/T11":         {Clauses: 4, Digest: "172773fbd05b049a"},
	"B3/V13":         {Clauses: 4, Digest: "3b79223ea2aa0670"},
	"B3/W07":         {Clauses: 4, Digest: "def8741b188e1644"},
	"C1/AU1":         {Clauses: 5, Digest: "de8741de9487380c"},
	"C1/M07":         {Clauses: 3, Digest: "e7b1c5ce8cea7c57"},
	"C1/RLM1":        {Clauses: 4, Digest: "0f7442ee2132f8f4"},
	"C2/UX1":         {Clauses: 4, Digest: "fe6b29cc5e34b4c1"},
	"C2/UX2":         {Clauses: 3, Digest: "25ec809d65d4bf97"},
	"C2/UX3":         {Clauses: 4, Digest: "ba99c3606aae158c"},
	"C2/UX4":         {Clauses: 3, Digest: "b619391373f3a949"},
	"C2/UX8":         {Clauses: 4, Digest: "6b1ba09b02d510bc"},
	"C3/E03":         {Clauses: 4, Digest: "fa545434149aae66"},
	"C4/COST1":       {Clauses: 4, Digest: "f3256177dd8b58b6"},
	"C4/O07":         {Clauses: 3, Digest: "d2331122f13a4823"},
	"C4/OBS1":        {Clauses: 4, Digest: "6d9c15eb4e6e3e0e"},
	"C4/OBS2":        {Clauses: 4, Digest: "39ed618a5f3a9ab8"},
	"C4/OBS3":        {Clauses: 3, Digest: "8612a7f335f07dab"},
	"D1/APS1":        {Clauses: 3, Digest: "8d2ae7e4cb7547ac"},
	"D1/V12":         {Clauses: 4, Digest: "84723d634f3afc79"},
	"D1/V14":         {Clauses: 3, Digest: "28677574c517a503"},
	"D2/O12":         {Clauses: 2, Digest: "b070e9ce4b3b6937"},
	"D2/V15":         {Clauses: 3, Digest: "98a6c5fd6c639e35"},
	"D3/C15":         {Clauses: 4, Digest: "4ea3a2325adc9036"},
	"D3/I18N1":       {Clauses: 3, Digest: "b981330ae6beb9d1"},
	"D3/S10":         {Clauses: 3, Digest: "3a834fcf695a2d56"},
	"E1/COV2":        {Clauses: 4, Digest: "23147979f03269b2"},
	"E1/COV3":        {Clauses: 3, Digest: "f5a5eeff4f07bfa6"},
	"E2/PROP1":       {Clauses: 3, Digest: "bcb2d111a62591eb"},
	"F1/WAL1":        {Clauses: 5, Digest: "b311ecf8be79aeb8"},
	"F2/BENCH1":      {Clauses: 3, Digest: "cd9e51a95c693492"},
	"F2/CCL1":        {Clauses: 3, Digest: "544655c9557a227a"},
	"F2/LEAK2":       {Clauses: 4, Digest: "44613157096273c2"},
	"F2/LEAK3":       {Clauses: 3, Digest: "750bfb5174956085"},
	"G/VISION":       {Clauses: 3, Digest: "95261e23de252f63"},
	"G/VISION-TOOL":  {Clauses: 4, Digest: "c14f1a527ba12d7c"},
	"H1/PKG1":        {Clauses: 4, Digest: "a818e63e6ec171b0"},
	"H1/VER1":        {Clauses: 3, Digest: "46c507a531bdb275"},
	"H2/APIREF1":     {Clauses: 3, Digest: "6b30793a95cda9f3"},
	"H2/CONTRIB1":    {Clauses: 3, Digest: "8736f9828a718fce"},
	"H2/EX1":         {Clauses: 3, Digest: "1c528d7c9ab29f03"},
	"H2/UDOC1":       {Clauses: 3, Digest: "e54bdd29d182b456"},
	"M1/G02":         {Clauses: 2, Digest: "bff447164b8d8654"},
	"M1/G03":         {Clauses: 3, Digest: "a6834ab7e7bb0e96"},
	"M1/O07":         {Clauses: 5, Digest: "c81cefe6b2ca3d3d"},
	"M1/SPEC-TOOLIF": {Clauses: 5, Digest: "b1da30a62eaad970"},
}

// TestLedgerAcceptanceIsPinned fails whenever an acceptance criterion is
// added, removed or edited without updating acceptancePins.
//
// The failure text carries the replacement row verbatim so that fixing it is
// copy-paste, not arithmetic: the cost this gate imposes is deliberately the
// cost of SAYING what changed, not the cost of computing a hash.
func TestLedgerAcceptanceIsPinned(t *testing.T) {
	entries := loadLedger(t)

	var problems []string
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.ID == "" {
			continue // reported by TestFeatureStatusLedgerIntegrity
		}
		seen[e.ID] = true
		now := acceptancePin{
			Clauses: len(splitClauses(e.Acceptance)),
			Digest:  acceptanceDigest(e.Acceptance),
		}
		row := fmt.Sprintf("%q: {Clauses: %d, Digest: %q},", e.ID, now.Clauses, now.Digest)

		pin, ok := acceptancePins[e.ID]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: acceptance is not pinned — "+
				"add\n\t%s\n    to acceptancePins so a later edit to this criterion "+
				"cannot pass unremarked", e.ID, row))
			continue
		}
		if pin.Digest == now.Digest && pin.Clauses == now.Clauses {
			continue
		}
		shrink := ""
		if now.Clauses < pin.Clauses {
			shrink = fmt.Sprintf(" THIS DROPS %d PROMISE(S).", pin.Clauses-now.Clauses)
		}
		problems = append(problems, fmt.Sprintf("%s: acceptance changed since it was "+
			"pinned — was %d clause(s)/%s, now %d clause(s)/%s.%s\n    Current text: %q\n"+
			"    If the new wording is the criterion you intend to be held to, replace "+
			"the pin with\n\t%s\n    and say why in the commit message. Reducing a "+
			"criterion is a scope decision, not a cleanup.",
			e.ID, pin.Clauses, pin.Digest, now.Clauses, now.Digest, shrink,
			strings.TrimSpace(e.Acceptance), row))
	}

	var dead []string
	for id := range acceptancePins {
		if !seen[id] {
			dead = append(dead, id)
		}
	}
	sort.Strings(dead)
	for _, id := range dead {
		problems = append(problems, fmt.Sprintf("%s: acceptancePins has a row but the "+
			"ledger has no such entry — the pin table mirrors the ledger, so delete "+
			"the row (and check the entry was not dropped by accident)", id))
	}

	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("acceptance pins do not match docs/feature-status.yaml (%d problem(s)):\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}
