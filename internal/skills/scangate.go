// internal/skills/scangate.go
//
// S7 enforcement: where the scanner's verdict is actually consulted.
//
// The scan itself (scan.go) is a pure function over a directory. This file is
// the half that says NO, and it is deliberately separate because the two have
// different failure modes: a scanner bug produces a wrong finding, a gate bug
// produces no gate at all. The second is invisible without a test that asserts
// the refusal, so the refusals live together where they can be read as a set.
//
// # Two doors, two tiers of consequence
//
// ACQUISITION (install) REFUSES. A pack being installed is not yet relied upon
// by anything; refusing costs the user one command and a diagnostic naming the
// exact line. There is no state to preserve and no workflow to break, so this
// is the door where "no" is cheap and therefore where it is absolute.
//
// LOAD WITHHOLDS. A pack already on disk may be one the user wrote themselves,
// installed before this gate existed, or edited by hand five minutes ago.
// Failing the whole Load would take out every OTHER skill in the same root, and
// Load's standing contract is that one bad skill never fails the load. So the
// unsafe skill is withheld from the registry — the model never sees it and
// never loads its body — and everything else proceeds.
//
// Withholding rather than merely warning is the point. A warning on stderr at
// boot is read once, by nobody, while the pack's text keeps going into the
// system prompt on every turn. The gate has to be the thing that stops the text
// reaching the model, not a note about the text that did.
//
// # The escape hatch, and why it is per-skill and on disk
//
// A blocked pack can be admitted by placing a `.scan-override` marker file in
// its directory (SkillScanOverrideMarker). Three properties made that the
// shape, rather than a global "disable scanning" flag:
//
//   - It is per-skill. Disabling the scanner globally to admit one pack is how
//     a security gate goes from "on" to "off" permanently, because nobody
//     comes back to re-enable it.
//   - It is a WRITE the user performs on the pack they are vouching for, which
//     is the same gesture as `.trusted` and reads the same way in a diff.
//   - Remote packs cannot self-assert it. Install deletes the marker from a
//     downloaded pack before scanning, exactly as it already deletes `.trusted`
//     and `.disabled` — a pack that could ship its own override would make the
//     gate decorative.
//
// The override does not silence the findings; ScanSkillDir still reports them
// and /skill validate still prints them. It only stops them being fatal.

package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SkillScanOverrideMarker is the filename that admits a skill whose content
// scan found blocking issues. See the file header for why the escape hatch is
// a per-skill on-disk marker rather than a global flag.
//
// Install DELETES this marker from a downloaded pack before scanning, together
// with .trusted and .disabled, so a remote pack can never vouch for itself.
const SkillScanOverrideMarker = ".scan-override"

// ErrSkillUnsafe wraps every refusal produced by the content scan, so callers
// can distinguish "this pack is dangerous" from "this pack is malformed"
// without string-matching the message.
var ErrSkillUnsafe = errors.New("skills: content scan refused the pack")

// ScanOverridden reports whether dir carries the override marker.
func ScanOverridden(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, SkillScanOverrideMarker))
	return err == nil
}

// GateSkillDir scans dir and returns an error when the pack carries blocking
// findings and is not overridden.
//
// allowUnsafe is the CALLER's override (a `--allow-unsafe` flag), separate from
// the on-disk marker. Both exist because they answer different questions: the
// flag is "I am installing this one deliberately, right now", the marker is
// "this pack stays admitted across every future load". A flag alone would have
// to be re-passed on every boot; a marker alone would force the user to write a
// file into a pack before they have decided to keep it.
//
// A scan that cannot RUN (unreadable directory) is an error, not a pass. This
// is the fail-closed direction: the alternative — treat an unscannable pack as
// clean — makes "make the scan fail" a general-purpose bypass.
func GateSkillDir(dir string, allowUnsafe bool) (ScanResult, error) {
	res, err := ScanSkillDir(dir)
	if err != nil {
		return res, err
	}
	if res.IsSafe() || allowUnsafe || ScanOverridden(dir) {
		return res, nil
	}
	return res, fmt.Errorf("%w: %s", ErrSkillUnsafe, res.Error())
}

// scanStagedPack is the install-time gate, applied to a pack sitting in staging
// before it is renamed into the loader-scanned root.
//
// It deletes the override marker first. A remote pack that shipped
// `.scan-override` would otherwise arrive pre-approved, which is the same class
// of self-assertion as a remote `.trusted` — and that one has been purged since
// the first version of Install for exactly this reason.
func scanStagedPack(skillDir string, allowUnsafe bool) error {
	if err := os.Remove(filepath.Join(skillDir, SkillScanOverrideMarker)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("skills: remove remote %s: %w", SkillScanOverrideMarker, err)
	}
	_, err := GateSkillDir(skillDir, allowUnsafe)
	return err
}
