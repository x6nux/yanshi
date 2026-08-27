// internal/skills/author.go
//
// T7: letting the model write a skill.
//
// # Why this exists
//
// The goal loop's whole point is that a run teaches something. Today the only
// place that lesson can land is a one-line memory entry, so the next run
// rediscovers the same thing. A skill is the durable form — it is what the
// model already reads, already selects by description, and already loads on
// demand — and there was no way to produce one except by hand.
//
// # Why the write path is not just os.WriteFile
//
// The content comes FROM THE MODEL, and the model's context contains material
// an attacker may control: a fetched web page, a commit message, a test failure
// quoting a hostile file. So every property that makes an INSTALLED pack safe
// has to hold here too, and for a sharper reason: an installed pack was written
// by a person somewhere, while this one was written by a process that was
// reading attacker-influenced text a moment ago.
//
// Concretely, WriteSkill enforces the same four gates the install path does:
//
//  1. Containment, via os.Root rather than string comparison. See below.
//  2. The frontmatter must be well formed and the name must match the
//     directory (ValidateSkillDir).
//  3. The S7 content scan must pass (GateSkillDir). A model that was talked
//     into writing "ignore all previous instructions" into a skill has written
//     a durable, self-reloading prompt injection — the scan is the only thing
//     between that and next boot's system prompt.
//  4. No overwrite without an explicit flag, so a skill cannot be silently
//     replaced by a later turn.
//
// # Containment is os.Root, not filepath.Join plus a prefix check
//
// This follows internal/vcs/rootwrite.go, and the argument there applies
// verbatim: a lexical check is check-then-use, and between the check and the
// write the same agent that can run shell_run can replace a path component with
// a symlink. os.Root resolves per syscall against a retained directory handle,
// so an escape fails inside the kernel call rather than in a Go-side prediction
// of what the kernel would do. The name is additionally validated by validName,
// which already refuses separators and dots — but that is the belt, and os.Root
// is the braces.
//
// # Staging, for the same reason Install stages
//
// The pack is assembled in a temporary sibling directory and renamed into place
// only after every gate passes. A skill that failed validation must never exist
// at a path the loader scans, not even briefly: Reload can run at any moment,
// and a half-written SKILL.md that happens to parse would be loaded and
// advertised.

package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// MaxSkillBodyBytes caps the markdown body of an authored skill.
//
// The bound exists because this text is loaded into the system prompt on
// demand, so an unbounded skill is an unbounded prompt. 64 KiB is far above any
// legitimate skill (the packs this repository ships are well under 2 KiB) and
// far below the point where one skill could crowd out a context window.
const MaxSkillBodyBytes = 64 << 10

// MaxAuthoredFiles caps how many reference files one authored skill may carry
// beside SKILL.md.
const MaxAuthoredFiles = 32

// SkillDraft is a skill the model wants to write.
//
// Files holds OPTIONAL reference documents keyed by a relative slash-separated
// path. It exists because progressive disclosure is the format's main idea —
// the body stays short and points at references the model reads on demand — and
// a writer that could only produce SKILL.md would push everything into the one
// file that is always loaded.
//
// There is deliberately no field for an executable script. A skill may describe
// commands to run, and the model may write scripts through the ordinary fs
// tools where the normal FS authorization applies; letting the skill WRITER
// place executables would route around that with no gain, since nothing in this
// package ever executes what a skill ships.
type SkillDraft struct {
	Name        string
	Description string
	Body        string
	Files       map[string]string
	// Overwrite permits replacing an existing skill of the same name. Default
	// false: silently replacing a skill would let one turn destroy work from
	// another, and the model has no way to know which it is doing.
	Overwrite bool
	// AllowUnsafe waives the fatality of blocking content-scan findings. It is
	// plumbed through so the tool layer CAN expose an escape hatch, and it is
	// deliberately not the default.
	AllowUnsafe bool
}

// authoredFileRe restricts reference paths to a conservative charset. It is a
// second line behind os.Root, aimed at the shapes that are legal paths but bad
// filenames — leading dots (a hidden file inside a skill is a supply-chain
// finding in its own right), and anything that is not plainly a document.
var authoredFileRe = mustCompileAuthoredFileRe()

// WriteSkill materialises draft into a new directory under root and returns the
// absolute path of the installed skill.
//
// root is the skill directory the caller has decided the model may write to —
// in production the user skills root, never a directory chosen from tool
// arguments. Every path this function touches is resolved through an os.Root
// handle on it.
//
// The order of operations is load-bearing: assemble in staging, validate, scan,
// and only then publish by rename. See the file header.
func WriteSkill(root string, draft SkillDraft) (string, error) {
	if !validName(draft.Name) {
		return "", fmt.Errorf("skills: invalid skill name %q (letters, digits and dashes, 1-64 chars)", draft.Name)
	}
	if !validDesc(draft.Description) {
		return "", fmt.Errorf("skills: skill %q needs a 1-1024 character description; "+
			"the model selects skills by description, so an empty one makes the skill unreachable", draft.Name)
	}
	if strings.TrimSpace(draft.Body) == "" {
		return "", fmt.Errorf("skills: skill %q has an empty body; a skill with no instructions is not a skill", draft.Name)
	}
	if len(draft.Body) > MaxSkillBodyBytes {
		return "", fmt.Errorf("skills: skill %q body is %d bytes, over the %d-byte limit; "+
			"move the detail into a reference file and point at it from the body",
			draft.Name, len(draft.Body), MaxSkillBodyBytes)
	}
	if len(draft.Files) > MaxAuthoredFiles {
		return "", fmt.Errorf("skills: skill %q ships %d reference files, over the limit of %d",
			draft.Name, len(draft.Files), MaxAuthoredFiles)
	}
	for rel, content := range draft.Files {
		if err := validateAuthoredFile(rel, content); err != nil {
			return "", err
		}
	}

	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("skills: mkdir skills root: %w", err)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("skills: abs skills root: %w", err)
	}

	// Staging sits BESIDE the skills root, not inside it, so the loader never
	// sees a partially written pack — and so staging and target share a
	// filesystem, which is what keeps publication a rename rather than a copy.
	staging, err := os.MkdirTemp(filepath.Dir(rootAbs), ".yanshi-skill-write-")
	if err != nil {
		return "", fmt.Errorf("skills: mkstaging: %w", err)
	}
	defer os.RemoveAll(staging)

	// ValidateSkillDir compares the directory name against the frontmatter
	// name, so the staged pack has to carry its declared name already.
	stageDir := filepath.Join(staging, draft.Name)
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return "", fmt.Errorf("skills: mkdir staging pack: %w", err)
	}
	if err := writeDraftFiles(stageDir, draft); err != nil {
		return "", err
	}
	if err := ValidateSkillStructure(stageDir); err != nil {
		return "", err
	}
	// The content gate is run SEPARATELY from the structural validation above,
	// rather than relying on ValidateSkillDir's built-in one, because that one
	// is unconditional: it honours only the on-disk override marker and knows
	// nothing about the caller's AllowUnsafe. Calling it here would make the
	// AllowUnsafe field dead — which is exactly what the first version of this
	// function did, and what TestWriteSkill_AllowUnsafeIsRespected caught.
	//
	// A freshly staged pack cannot carry an override marker either, because
	// writeDraftFiles refuses to write dotfiles, so this call is the ONLY thing
	// that can admit an unsafe draft.
	if _, err := GateSkillDir(stageDir, draft.AllowUnsafe); err != nil {
		return "", err
	}

	// Publish through an os.Root handle on the skills root.
	//
	// HONEST SCOPE OF WHAT THIS BUYS. A mutation probe replaced these three
	// rootHandle calls with os.Lstat/os.RemoveAll on filepath.Join(rootAbs,
	// name) and every test stayed green. That is not a missing test — it is the
	// truth about this particular path:
	//
	//   - validName forbids separators, so the destination has exactly ONE
	//     component below the root. There is no intermediate component for a
	//     symlink to hide in, which is the case os.Root's per-syscall
	//     resolution primarily defends.
	//   - When the FINAL component is itself a symlink, os.RemoveAll and
	//     Root.RemoveAll agree: both unlink the symlink rather than following
	//     it. TestWriteSkill_SymlinkedDestinationCannotRedirectTheWrite passes
	//     under both, and it verifies the property that actually matters (the
	//     target directory is untouched).
	//
	// What the handle still buys is the root itself: it is opened once, so
	// replacing the skills ROOT with a symlink after this point cannot redirect
	// the operations below. That window is real but not deterministically
	// testable, so it is defence in depth rather than a tested guarantee, and
	// saying so here is better than implying a test covers it.
	rootHandle, err := os.OpenRoot(rootAbs)
	if err != nil {
		return "", fmt.Errorf("skills: open skills root: %w", err)
	}
	defer func() { _ = rootHandle.Close() }()

	if _, err := rootHandle.Lstat(draft.Name); err == nil {
		if !draft.Overwrite {
			return "", fmt.Errorf("skills: skill %q already exists; pass overwrite to replace it", draft.Name)
		}
		if err := rootHandle.RemoveAll(draft.Name); err != nil {
			return "", fmt.Errorf("skills: replace existing %q: %w", draft.Name, err)
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("skills: stat destination %q: %w", draft.Name, err)
	}

	dst := filepath.Join(rootAbs, draft.Name)
	if err := os.Rename(stageDir, dst); err != nil {
		return "", fmt.Errorf("skills: publish %q: %w", draft.Name, err)
	}
	return dst, nil
}

// writeDraftFiles materialises SKILL.md and the reference files into stageDir
// through an os.Root handle.
//
// The handle is opened on the staging pack rather than on the skills root
// because that is the directory being filled; containment for the PUBLISH step
// is a separate handle in WriteSkill. Two narrow handles rather than one wide
// one means neither step can write outside the directory it is meant to touch.
func writeDraftFiles(stageDir string, draft SkillDraft) error {
	handle, err := os.OpenRoot(stageDir)
	if err != nil {
		return fmt.Errorf("skills: open staging pack: %w", err)
	}
	defer func() { _ = handle.Close() }()

	if err := handle.WriteFile("SKILL.md", []byte(RenderSkillMarkdown(draft)), 0o644); err != nil {
		return fmt.Errorf("skills: write SKILL.md: %w", err)
	}

	// Sorted so the operation is deterministic: two identical drafts must
	// produce byte-identical directories, which is what makes a failure
	// reproducible.
	rels := make([]string, 0, len(draft.Files))
	for rel := range draft.Files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		clean := filepath.FromSlash(rel)
		if dir := filepath.Dir(clean); dir != "." {
			if err := handle.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("skills: mkdir %q: %w", rel, err)
			}
		}
		if err := handle.WriteFile(clean, []byte(draft.Files[rel]), 0o644); err != nil {
			return fmt.Errorf("skills: write %q: %w", rel, err)
		}
	}
	return nil
}

// validateAuthoredFile checks one reference file's path and size.
//
// The path rules are stricter than "does not escape", because os.Root already
// guarantees that. What these add is that the result is a REVIEWABLE document:
// no hidden files (a dotfile in a skill pack is itself a supply-chain finding),
// no absolute paths, no traversal, and a bounded size.
func validateAuthoredFile(rel, content string) error {
	if rel == "" {
		return fmt.Errorf("skills: reference file has an empty path")
	}
	if strings.EqualFold(rel, "SKILL.md") {
		return fmt.Errorf("skills: reference file %q collides with the generated SKILL.md; "+
			"put the instructions in the body instead", rel)
	}
	if filepath.IsAbs(filepath.FromSlash(rel)) || strings.HasPrefix(rel, "/") {
		return fmt.Errorf("skills: reference path %q must be relative to the skill directory", rel)
	}
	if !authoredFileRe.MatchString(rel) {
		return fmt.Errorf("skills: reference path %q is not allowed; use relative slash-separated "+
			"paths of letters, digits, dash, underscore and dot, with no leading dot in any segment", rel)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("skills: reference path %q contains an empty or traversal segment", rel)
		}
	}
	if len(content) > MaxSkillBodyBytes {
		return fmt.Errorf("skills: reference file %q is %d bytes, over the %d-byte limit",
			rel, len(content), MaxSkillBodyBytes)
	}
	return nil
}

// RenderSkillMarkdown renders a draft as a standard SKILL.md document.
//
// The frontmatter is emitted through the YAML marshaller rather than by string
// concatenation. A description containing a colon, a quote or a newline would
// otherwise produce a file whose frontmatter does not parse — and the model
// writes the description, so that input is not hypothetical.
func RenderSkillMarkdown(draft SkillDraft) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString(marshalFrontmatterField("name", draft.Name))
	b.WriteString(marshalFrontmatterField("description", draft.Description))
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(draft.Body))
	b.WriteString("\n")
	return b.String()
}

// marshalFrontmatterField renders one frontmatter key through the YAML
// marshaller, so a value containing a colon, a quote, a newline or a leading
// "#" is quoted or block-scalared as that value requires.
//
// Hand-rolling the quoting was the obvious alternative and is the bug: the
// description comes from the model, and a description like `Fix: the "auth"
// bug` produces frontmatter that does not parse, which Load then skips
// silently — the model would write a skill and find it absent with no
// diagnosis anywhere.
func marshalFrontmatterField(key, value string) string {
	out, err := yaml.Marshal(map[string]string{key: value})
	if err != nil {
		// yaml.Marshal of a map[string]string cannot fail; if that ever stops
		// being true, a well-formed but useless field beats a panic in a tool
		// call, and ValidateSkillDir will reject the result downstream.
		return key + ": \"\"\n"
	}
	return string(out)
}

// mustCompileAuthoredFileRe compiles the reference-path charset.
//
// Each segment must start with an alphanumeric, which is what excludes hidden
// files, "." and "..". The extension list is restricted to document formats:
// the writer must not be able to place a script, since that would be a write
// with no FS authorization behind it (see the SkillDraft.Files doc comment).
func mustCompileAuthoredFileRe() *regexp.Regexp {
	return regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*\.(?:md|markdown|txt|json|yaml|yml|csv)$`)
}
