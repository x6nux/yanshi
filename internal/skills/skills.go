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
// into the orchestrator system prompt, or "" if no Enabled skills are
// registered. Disabled skills are omitted so the model does not advertise
// them. The string is an instantaneous registry snapshot; orchestrator.New
// bakes its return value, so a later Reload does not mutate an already-running
// orchestrator prompt (FN3).
func (r *Registry) MetaPrompt() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.skills))
	for n, s := range r.skills {
		if s.Enabled {
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

// cloneSkill returns a shallow copy of s. Get/List hand these out so callers
// cannot mutate the registry's internal state through the returned pointer
// (FN2).
func cloneSkill(s *Skill) *Skill {
	if s == nil {
		return nil
	}
	cp := *s
	return &cp
}

// Body returns the skill's markdown body (frontmatter stripped), read lazily.
func (r *Registry) Body(s *Skill) (string, error) {
	_, _, body, err := readFrontmatter(filepath.Join(s.Dir, "SKILL.md"))
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
			name, desc, _, err := readFrontmatter(filepath.Join(dir, "SKILL.md"))
			if err != nil || !validName(name) || !validDesc(desc) {
				continue
			}
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
			r.skills[name] = &Skill{
				Name: name, Description: desc, Dir: dir, Source: root.Source,
				Enabled: !disabledMarkerExists(dir),
				Trusted: trustMarkerExists(dir),
			}
		}
	}
	return r, nil
}

type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// readFrontmatter parses the YAML frontmatter of a SKILL.md. Returns the body
// too (used by Registry.Body in a later task).
func readFrontmatter(path string) (name, desc, body string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", "", err
	}
	return parseSkillFile(data)
}

func parseSkillFile(data []byte) (name, desc, body string, err error) {
	text := string(data)
	lines := strings.Split(text, "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", "", fmt.Errorf("skills: missing frontmatter opening delimiter")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return "", "", "", fmt.Errorf("skills: missing frontmatter closing delimiter")
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &fm); err != nil {
		return "", "", "", fmt.Errorf("skills: parse frontmatter: %w", err)
	}
	body = strings.TrimSpace(strings.Join(lines[end+1:], "\n"))
	return fm.Name, fm.Description, body, nil
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
