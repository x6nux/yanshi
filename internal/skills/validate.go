// internal/skills/validate.go
//
// Post-installation validation of a skill directory, extracted so the rules
// that gate an install can also be re-run against a skill already on disk.
//
// Before this, every check lived INLINE in Install: frontmatter parsing, name
// and description validity, and the symlink ban. A skill hand-edited after
// installation — which is the normal way people iterate on one — could not be
// checked by anything, and /skill had no verb that would have looked.

package skills

import (
	"fmt"
	"os"
	"path/filepath"
)

// ValidateSkillDir re-applies the install-time rules to a directory that is
// already in place.
//
// It shares readFrontmatter / validName / validDesc / rejectSymlinks with
// Install rather than restating them: two copies of a security rule drift, and
// the copy that drifts is the one that is not the gate.
//
// The symlink ban is re-checked, not assumed. Install strips symlinks from the
// pack it unpacks, but a directory on disk can acquire one afterwards, and the
// reason the ban exists — a symlink can read outside the skill dir or smuggle
// in a .trusted marker — applies just as much to an edited skill as to a
// freshly downloaded one.
func ValidateSkillDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("skills: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("skills: %q is not a directory", dir)
	}
	if err := rejectSymlinks(dir); err != nil {
		return err
	}
	name, desc, _, err := readFrontmatter(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return fmt.Errorf("skills: read SKILL.md: %w", err)
	}
	if !validName(name) {
		return fmt.Errorf("skills: invalid skill name %q (letters, digits and dashes, 1-64 chars)", name)
	}
	if !validDesc(desc) {
		return fmt.Errorf("skills: skill %q has an empty or over-long description "+
			"(1-1024 chars); the model selects skills by description, so an empty "+
			"one makes the skill unreachable", name)
	}
	// The directory name is what Load scans and what Uninstall removes, so a
	// mismatch means the two disagree about which skill this is.
	if base := filepath.Base(dir); base != name {
		return fmt.Errorf("skills: directory %q holds a skill named %q; Load keys on "+
			"the frontmatter name and Uninstall removes by directory, so the two "+
			"would disagree", base, name)
	}
	return nil
}
