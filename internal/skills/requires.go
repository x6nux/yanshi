// internal/skills/requires.go
//
// Declared external dependencies of a skill pack.
//
// A SKILL.md is a set of instructions for the model, and most non-trivial
// packs open by telling it to run a program: `rg`, `gh`, `ast-grep`. Before
// this file there was no way to say so. The absence surfaced as the worst
// possible shape of failure — the model selects the skill because the
// description matches, follows step 1, and shell_run reports "command not
// found" for a binary the skill could have known was missing before the turn
// started. Nothing in /skills, in the model's listing, or in the install
// output said the skill could not run on this machine.
//
// Scope is deliberately one requirement kind: `bin`, resolved with
// exec.LookPath. A version constraint needs a per-program way to ask (and to
// parse the answer), a `python-package` kind needs an interpreter to ask with,
// and neither has a caller today. The frontmatter shape leaves room for them
// (each entry is a mapping, not a bare string) without implementing them, and
// an unknown kind is REJECTED rather than ignored so a pack cannot ship a
// requirement that silently means nothing.
package skills

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// Requirement is one declared external dependency of a skill.
//
// The YAML shape is a list of single-key mappings:
//
//	requires:
//	  - bin: ast-grep
//	  - bin: gh
//
// A mapping rather than a bare string (`- ast-grep`) because the kind must be
// stated at the point of declaration: a later `- python-package: pandas` has
// to be distinguishable from a program name without a lookup table of every
// program yanshi has ever heard of.
type Requirement struct {
	// Bin is the program name to resolve on PATH. Exactly one requirement
	// field must be set per entry; today Bin is the only one.
	Bin string `yaml:"bin"`
}

// String renders the requirement the way it is written in frontmatter, for
// error messages and UI rows.
func (r Requirement) String() string {
	if r.Bin != "" {
		return "bin:" + r.Bin
	}
	return "(empty requirement)"
}

// binNameRe-equivalent check: a program name may not contain a path separator
// or shell-significant punctuation. This is NOT a security boundary —
// ProbeRequirements only calls exec.LookPath, which does not execute anything
// — but a `requires: [{bin: "rm -rf /"}]` entry that silently probes as
// "missing" forever is a pack bug worth naming at install time rather than
// letting it read as a legitimately absent dependency.
func validBinName(n string) bool {
	if n == "" || len(n) > 128 {
		return false
	}
	if strings.ContainsAny(n, "/\\ \t\n\r\"'`$;&|<>*?()[]{}") {
		return false
	}
	return true
}

// ValidateRequirements checks the shape of a parsed `requires:` block and
// returns every problem it finds, joined into one error.
//
// Every problem, not the first: a pack author fixing a typo'd requirement list
// one round-trip per entry is the reason people stop declaring them. The
// errors name the offending entry by index, because two entries can be wrong
// in the same way and "empty requirement" alone does not say which.
func ValidateRequirements(reqs []Requirement) error {
	var problems []string
	for i, r := range reqs {
		switch {
		case r.Bin == "":
			problems = append(problems, fmt.Sprintf(
				"requires[%d]: no recognized requirement key; the only supported "+
					"form is `- bin: <program>`", i))
		case !validBinName(r.Bin):
			problems = append(problems, fmt.Sprintf(
				"requires[%d]: %q is not a bare program name (no path separators, "+
					"whitespace or shell punctuation); write the program as it is "+
					"typed on the command line, e.g. `- bin: ast-grep`", i, r.Bin))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("skills: %s", strings.Join(problems, "; "))
}

// normalizeRequirements drops entries that ValidateRequirements would reject
// and returns the survivors alongside the validation error.
//
// Load uses it to honor its "one bad skill never fails the whole load"
// contract while still refusing to let a malformed entry reach the prober: an
// entry with an empty Bin would resolve as "missing" on every machine and
// suppress the skill everywhere, turning a frontmatter typo into a silent
// uninstall. ValidateSkillDir uses the returned error to say so out loud.
func normalizeRequirements(reqs []Requirement) ([]Requirement, error) {
	err := ValidateRequirements(reqs)
	if err == nil {
		return reqs, nil
	}
	kept := make([]Requirement, 0, len(reqs))
	for _, r := range reqs {
		if ValidateRequirements([]Requirement{r}) == nil {
			kept = append(kept, r)
		}
	}
	if len(kept) == 0 {
		return nil, err
	}
	return kept, err
}

// LookPath is the PATH resolver ProbeRequirements uses. It is a package
// variable so tests can present a PATH they control without mutating the
// process environment — which is shared by every parallel test in the package
// and by any subprocess they spawn.
//
// Production value is exec.LookPath, which resolves without executing: probing
// a requirement must never run the program it is asking about.
var LookPath = exec.LookPath

// ProbeRequirements returns the sorted, de-duplicated names of the programs in
// reqs that could not be resolved on PATH. It returns nil when everything
// resolved, so `len(...) == 0` is the satisfied predicate.
//
// Sorted so the /skills listing and the skill_use refusal name the missing
// programs in the same order every time; de-duplicated because a pack that
// declares the same binary twice should not say "missing rg, rg".
func ProbeRequirements(reqs []Requirement) []string {
	if len(reqs) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(reqs))
	var missing []string
	for _, r := range reqs {
		if r.Bin == "" || seen[r.Bin] {
			continue
		}
		seen[r.Bin] = true
		if _, err := LookPath(r.Bin); err != nil {
			missing = append(missing, r.Bin)
		}
	}
	sort.Strings(missing)
	return missing
}

// MissingRequirementHint renders the model-facing explanation for a skill that
// cannot run here. Shared by skill_use's refusal and the TUI listing so the
// wording — and the "install these" instruction — exists in one place.
//
// Returns "" when nothing is missing, so callers can use it as both the
// predicate and the message.
func MissingRequirementHint(name string, missing []string) string {
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"skill %q declares required programs that are not on PATH: %s. "+
			"Install them (or ask the user to) before using this skill; "+
			"another skill or the general-purpose tools may work in the meantime.",
		name, strings.Join(missing, ", "))
}
