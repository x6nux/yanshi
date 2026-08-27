// Package skills loads standard SKILL.md skill packs (agentskills.io format)
// with progressive disclosure: only frontmatter is read at load time.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Skill is a loaded skill's metadata. The body is read lazily via Registry.Body.
type Skill struct {
	Name        string
	Description string
	Dir         string // absolute path to the skill directory
	Source      string // "builtin" | "user" | "plugin:<name>"
	// Enabled gates MetaPrompt listing and skill_use invocation. Go's zero value
	// is false; Loader, not the struct literal, establishes default enabled state
	// by reading the absence of .disabled.
	Enabled bool
	// Trusted records user review only; it never authorizes script execution.
	Trusted bool
	// Requires lists the external programs this skill's instructions tell the
	// model to run. Declared in the SKILL.md frontmatter, resolved against PATH
	// by ProbeRequirements — never used to authorize anything, only to answer
	// "will this skill's first step fail" before the model spends a turn
	// finding out.
	Requires []Requirement
	// Missing names the subset of Requires that ProbeRequirements could not
	// resolve on PATH. Nil when everything resolved and nil when Requires is
	// empty, so `len(Missing) > 0` is the single "this skill is unusable here"
	// predicate for every consumer (MetaPrompt, skill_use, /skills).
	//
	// It is a FIELD rather than a method because the probe hits the filesystem:
	// recomputing it per consumer would make an O(1) list render into one
	// exec.LookPath per skill per call, and the two consumers could disagree
	// about the same skill within one turn if PATH changed between them.
	Missing []string
	// Unsafe records the S7 content-scan findings that BLOCK this skill, and is
	// the load-time half of the gate described in scangate.go. Non-empty means
	// the skill was loaded into the registry but is withheld from the model:
	// MetaPrompt omits it and skill_use refuses it, so its text never reaches
	// the system prompt.
	//
	// The skill is still REGISTERED rather than dropped, and that is the whole
	// design. A dropped skill is indistinguishable from one that was never
	// installed — /skills would show nothing, and the user whose own hand-edited
	// pack just vanished would have no way to learn why. Keeping the entry and
	// carrying the reason is what makes the withholding diagnosable.
	//
	// Empty for every skill that scans clean or carries the .scan-override
	// marker, so `len(Unsafe) > 0` is the single predicate consumers test.
	Unsafe []Finding
}

// Root is a skill search root: a directory whose immediate children are skill dirs.
type Root struct {
	Dir    string
	Source string
}

// Builtin, User, Plugin construct Roots with the conventional source tags.
func Builtin(dir string) Root { return Root{Dir: dir, Source: "builtin"} }

// User constructs a root with the "user" source tag.
func User(dir string) Root { return Root{Dir: dir, Source: "user"} }

// Plugin constructs a root with a plugin-qualified source tag.
func Plugin(name, dir string) Root { return Root{Dir: dir, Source: "plugin:" + name} }

// Registry holds loaded skills keyed by Name (first-seen-wins). Multiple WS
// connections and tool calls may access it concurrently; mu guards the map and
// every mutable Skill stored behind it. Read APIs return snapshots.
type Registry struct {
	mu     sync.RWMutex
	skills map[string]*Skill
	// conflicts records every name that more than one root provided. The
	// resolution stays first-seen-wins — changing it would silently swap which
	// skill runs — but the LOSS is now recorded. It used to be a bare
	// `continue`, so a project skill shadowed by a user-level one of the same
	// name left no trace anywhere: /skills showed only the winner, the name
	// resolved to something the user did not write, and nothing in the product
	// could say so.
	conflicts []Conflict
}

// Conflict is one shadowed skill: the name, who won, and who lost.
//
// Both directories are carried because the actionable question is "which file
// is being ignored", and a source label alone ("user") does not answer it on a
// machine with several user-level roots.
type Conflict struct {
	Name           string
	WinnerSource   string
	WinnerDir      string
	ShadowedSource string
	ShadowedDir    string
}

// Conflicts returns the shadowed-skill records from the last Load.
func (r *Registry) Conflicts() []Conflict {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Conflict, len(r.conflicts))
	copy(out, r.conflicts)
	return out
}

// Loader scans one or more Roots and builds a Registry.
type Loader struct {
	roots []Root
}

// NewLoader creates a Loader that scans the given roots for skills.
func NewLoader(roots ...Root) *Loader { return &Loader{roots: roots} }

// Get returns a cloned skill by name, or (nil, false) if unknown.
func (r *Registry) Get(name string) (*Skill, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.skills[name]
	if !ok {
		return nil, false
	}
	return cloneSkill(s), true
}

// List returns snapshots of all registered skills.
func (r *Registry) List() []*Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Skill, 0, len(r.skills))
	for _, s := range r.skills {
		out = append(out, cloneSkill(s))
	}
	return out
}

// MetaPrompt returns a human-readable "Available skills" block for injection
// into the orchestrator system prompt, or "" if no advertisable skill is
// registered. The string is an instantaneous registry snapshot;
// orchestrator.New bakes its return value, so a later Reload does not mutate
// an already-running orchestrator prompt (FN3).
//
// Three classes of skill are omitted, each because listing it would cost the
// model a turn or worse:
//
//   - Disabled skills, so the model does not advertise what the operator
//     switched off.
//   - Skills whose declared `requires:` programs are not on PATH. A skill
//     whose first instruction is "run ast-grep" on a machine without ast-grep
//     is not a capability, it is a scripted failure: the model picks it
//     because the description matches, spends a turn discovering the binary is
//     missing, and has no way to learn that from the listing. Withholding the
//     NAME is what stops that, which is why the filter is here and not only in
//     skill_use — skill_use can refuse, but by then the turn is already spent.
//   - Skills the S7 content scan blocked (len(Unsafe) > 0). Here the reason is
//     stronger than for the other two: this string is CONCATENATED INTO THE
//     SYSTEM PROMPT, so a blocked skill's own description would ride into the
//     prompt on the very listing that was supposed to withhold it. Filtering
//     in skill_use alone would be too late by one full prompt.
//
// This is deliberately the narrow form of "skill state gates what the model
// sees". The broad form — a skill declaring which TOOLS it needs and those
// tools vanishing from the schema — is not implemented and should not be
// inferred from this: skills do not own tool registration in this codebase
// (bootstrap does), no skill declares a tool today, and a mechanism with no
// declared consumers would be a layer that exists to be diagrammed. The skill
// listing is the surface skills actually own, so it is the surface that is
// gated.
func (r *Registry) MetaPrompt() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.skills))
	for n, s := range r.skills {
		if s.Enabled && len(s.Missing) == 0 && len(s.Unsafe) == 0 {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("Available skills (call the skill_use tool to load one):\n")
	for _, n := range names {
		s := r.skills[n]
		b.WriteString("- ")
		b.WriteString(n)
		b.WriteString(": ")
		b.WriteString(s.Description)
		b.WriteString("\n")
	}
	return b.String()
}

// cloneSkill returns a copy of s. Get/List hand these out so callers cannot
// mutate the registry's internal state through the returned pointer (FN2).
//
// The two slice fields are copied, not aliased. A shallow copy leaves Requires
// and Missing pointing at the registry's own backing arrays, so a caller that
// appended to the returned Skill's Missing would write into the live entry — a
// mutation through exactly the pointer this function exists to prevent.
func cloneSkill(s *Skill) *Skill {
	if s == nil {
		return nil
	}
	cp := *s
	if s.Requires != nil {
		cp.Requires = append([]Requirement(nil), s.Requires...)
	}
	if s.Missing != nil {
		cp.Missing = append([]string(nil), s.Missing...)
	}
	if s.Unsafe != nil {
		cp.Unsafe = append([]Finding(nil), s.Unsafe...)
	}
	return &cp
}

// Body returns the skill's markdown body (frontmatter stripped), read lazily.
func (r *Registry) Body(s *Skill) (string, error) {
	_, body, err := readFrontmatter(filepath.Join(s.Dir, "SKILL.md"))
	if err != nil {
		return "", fmt.Errorf("skills: read body of %q: %w", s.Name, err)
	}
	return body, nil
}

// ReadFile reads an on-demand reference file from the skill's directory.
// The relpath is sanitized to reject path traversal outside the skill dir.
func (r *Registry) ReadFile(s *Skill, rel string) (string, error) {
	// Absolute paths (e.g. C:\Windows\win.ini or /etc/passwd) are rejected
	// up front: on Windows filepath.Join does NOT override base for absolute
	// rel values, so the prefix check below would otherwise let them through
	// and the failure would surface only as a confusing os.ReadFile error.
	if filepath.IsAbs(filepath.FromSlash(rel)) {
		return "", fmt.Errorf("skills: path %q must be relative to the skill dir", rel)
	}
	base, err := filepath.Abs(s.Dir)
	if err != nil {
		return "", err
	}
	target := filepath.Clean(filepath.Join(base, filepath.FromSlash(rel)))
	if !strings.HasPrefix(filepath.Clean(target)+string(filepath.Separator), base+string(filepath.Separator)) && target != base {
		return "", fmt.Errorf("skills: path %q escapes skill dir", rel)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return "", fmt.Errorf("skills: read %q: %w", rel, err)
	}
	return string(data), nil
}

var skillNameRe = regexp.MustCompile(`^[a-zA-Z0-9-]+$`)

func validName(n string) bool { return len(n) > 0 && len(n) <= 64 && skillNameRe.MatchString(n) }
func validDesc(d string) bool { return len(d) > 0 && len(d) <= 1024 }

// Load scans every root and registers skills. A dir without a valid SKILL.md is
// skipped; one bad skill never fails the whole load.
func (l *Loader) Load() (*Registry, error) {
	r := &Registry{skills: map[string]*Skill{}}
	for _, root := range l.roots {
		entries, err := os.ReadDir(root.Dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("skills: read root %q: %w", root.Dir, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(root.Dir, e.Name())
			fm, _, err := readFrontmatter(filepath.Join(dir, "SKILL.md"))
			if err != nil || !validName(fm.Name) || !validDesc(fm.Description) {
				continue
			}
			name := fm.Name
			if existing, exists := r.skills[name]; exists {
				// first-seen-wins, but the loss is recorded rather than
				// dropped: see Registry.conflicts.
				r.conflicts = append(r.conflicts, Conflict{
					Name:           name,
					WinnerSource:   existing.Source,
					WinnerDir:      existing.Dir,
					ShadowedSource: root.Source,
					ShadowedDir:    dir,
				})
				continue
			}
			// A malformed requires: block does not drop the skill — Load's
			// contract is "one bad skill never fails the whole load", and a
			// dependency declaration is metadata, not the skill. It is
			// normalized away here and reported by ValidateSkillDir, which is
			// the verb whose job is to say so.
			reqs, _ := normalizeRequirements(fm.Requires)
			r.skills[name] = &Skill{
				Name: name, Description: fm.Description, Dir: dir, Source: root.Source,
				Enabled:  !disabledMarkerExists(dir),
				Trusted:  trustMarkerExists(dir),
				Requires: reqs,
				Missing:  ProbeRequirements(reqs),
				Unsafe:   scanFindingsForLoad(dir),
			}
		}
	}
	return r, nil
}

// scanFindingsForLoad runs the S7 content scan for Load and returns the
// findings that must WITHHOLD this skill from the model, or nil.
//
// Three decisions are compressed here, and each has a failure mode:
//
//   - An override marker returns nil. The user vouched for this pack; the
//     findings are still available from ValidateSkillDir, which is the verb
//     whose job is diagnosis.
//   - A scan that FAILS TO RUN returns nil, and this is the one place in the
//     S7 design that is fail-OPEN rather than fail-closed. The reason is that
//     Load's inputs are directories that already passed a frontmatter parse:
//     an I/O failure here is a broken disk or a permissions problem, not an
//     attack, and refusing every skill in the root because one directory
//     became unreadable would turn a local fault into a total loss of
//     capability at boot. Acquisition is where an unscannable pack is refused
//     (GateSkillDir returns the error), and acquisition is the door that
//     actually faces the network.
//   - Only BLOCKING findings are returned. An advisory finding must not
//     withhold a skill; see scan.go's header for why the tiers are split.
func scanFindingsForLoad(dir string) []Finding {
	if ScanOverridden(dir) {
		return nil
	}
	res, err := ScanSkillDir(dir)
	if err != nil {
		return nil
	}
	blocking := res.Blocking()
	if len(blocking) == 0 {
		return nil
	}
	return blocking
}

// UnsafeSkillHint renders the refusal a MODEL-FACING consumer shows when a
// skill was withheld by the content scan, or "" when the skill is clean.
//
// It is the S7 twin of MissingRequirementHint and exists for the same reason:
// two consumers (skill_use and the /skills listing) must say the same thing
// about the same state, and the way that stops being true is each writing its
// own sentence.
//
// # It names the rule and the location but NOT the matched text
//
// Finding.String() includes the offending line, and every other refusal path
// prints it — those are read by a person deciding whether to override. This one
// is returned to the MODEL, and the offending line is, by construction, the
// prompt injection. Echoing it would hand the model the exact sentence the
// withholding existed to keep away from it, merely wrapped in an apology.
//
// That is not a hypothetical concern about a snippet: it was the first version
// of this function, and a test asserting the refusal does not contain the
// payload is what caught it.
//
// The rule id and the file:line are kept, because they are what makes the
// refusal actionable without reproducing the content. A human reads the actual
// line with /skill validate, which is the verb whose audience can safely see it.
func UnsafeSkillHint(name string, unsafe []Finding) string {
	if len(unsafe) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "skill %q is withheld: its content scan found %d blocking issue(s)", name, len(unsafe))
	for _, f := range unsafe {
		loc := f.File
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		fmt.Fprintf(&b, "\n  [%s] %s at %s", f.Severity, f.RuleID, loc)
	}
	fmt.Fprintf(&b, "\n  The matched text is withheld here because it would be the injection itself; "+
		"run `/skill validate %s` to read it. Its text would otherwise be injected verbatim into the "+
		"system prompt. After reviewing, place a %s marker in the skill directory to admit it.",
		name, SkillScanOverrideMarker)
	return b.String()
}

// frontmatter is the parsed YAML header of a SKILL.md.
//
// It is returned whole rather than destructured into positional results
// because the header keeps growing (Requires was the third field), and every
// added field would otherwise have to be threaded through four call sites as
// one more anonymous string.
type frontmatter struct {
	Name        string        `yaml:"name"`
	Description string        `yaml:"description"`
	Requires    []Requirement `yaml:"requires"`
}

// readFrontmatter parses the YAML frontmatter of a SKILL.md, returning the
// header and the markdown body that follows it.
func readFrontmatter(path string) (fm frontmatter, body string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return frontmatter{}, "", err
	}
	return parseSkillFile(data)
}

func parseSkillFile(data []byte) (fm frontmatter, body string, err error) {
	text := string(data)
	lines := strings.Split(text, "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "---" {
		return frontmatter{}, "", fmt.Errorf("skills: missing frontmatter opening delimiter")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return frontmatter{}, "", fmt.Errorf("skills: missing frontmatter closing delimiter")
	}
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &fm); err != nil {
		return frontmatter{}, "", fmt.Errorf("skills: parse frontmatter: %w", err)
	}
	body = strings.TrimSpace(strings.Join(lines[end+1:], "\n"))
	return fm, body, nil
}

// trustMarkerExists reports whether dir contains a .trusted marker file.
func trustMarkerExists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".trusted"))
	return err == nil
}

// disabledMarkerExists reports whether dir contains a .disabled marker file.
func disabledMarkerExists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".disabled"))
	return err == nil
}

// Reload re-runs the ORIGINAL Loader (which owns builtin+user+plugin roots)
// and replaces the map only after a successful scan. Holding r.mu for the full
// load prevents concurrent Enable/Disable/Trust/Untrust from being overwritten
// by a stale marker snapshot. Registry consumers see the change immediately;
// an already-baked orchestrator SkillMetaPrompt still needs restart (FN3).
func (r *Registry) Reload(l *Loader) error {
	if l == nil {
		return fmt.Errorf("skills: reload: nil loader")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	newReg, err := l.Load()
	if err != nil {
		return err // old map remains intact
	}
	r.skills = newReg.skills
	return nil
}

// Enable removes the .disabled marker (if any) and flips Enabled to true.
func (r *Registry) Enable(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.skills[name]
	if !ok {
		return fmt.Errorf("skills: enable: unknown skill %q", name)
	}
	if err := os.Remove(filepath.Join(s.Dir, ".disabled")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("skills: enable: %w", err)
	}
	s.Enabled = true
	return nil
}

// Disable writes a .disabled marker and flips Enabled to false.
func (r *Registry) Disable(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.skills[name]
	if !ok {
		return fmt.Errorf("skills: disable: unknown skill %q", name)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, ".disabled"), nil, 0o644); err != nil {
		return fmt.Errorf("skills: disable: %w", err)
	}
	s.Enabled = false
	return nil
}

// Trust writes a .trusted marker and flips Trusted to true. Review only;
// never authorizes script execution.
func (r *Registry) Trust(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.skills[name]
	if !ok {
		return fmt.Errorf("skills: trust: unknown skill %q", name)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, ".trusted"), nil, 0o644); err != nil {
		return fmt.Errorf("skills: trust: %w", err)
	}
	s.Trusted = true
	return nil
}

// Untrust removes the .trusted marker and flips Trusted to false.
func (r *Registry) Untrust(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.skills[name]
	if !ok {
		return fmt.Errorf("skills: untrust: unknown skill %q", name)
	}
	if err := os.Remove(filepath.Join(s.Dir, ".trusted")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("skills: untrust: %w", err)
	}
	s.Trusted = false
	return nil
}
