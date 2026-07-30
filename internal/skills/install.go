package skills

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// InstallSource describes a parsed install source. Owner and Repo are required;
// Subdir is optional (selects a subdirectory of the repo as the skill dir).
type InstallSource struct {
	Owner  string
	Repo   string
	Subdir string // "" or "path/to/skill" (no leading/trailing slash)
}

// ownerRepoRe matches "owner/repo" with optional "/subdir/path". Each segment
// is restricted to [A-Za-z0-9._-] (no whitespace, no shell metachars, no path
// separators inside a segment). "." and ".." are rejected by the per-segment
// check in ParseInstallSource (they technically pass this regex).
var ownerRepoRe = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+(/[/A-Za-z0-9._-]+)?$`)

// segmentRe is the per-segment charset: same as validName's skillNameRe but
// also allowing "." (for e.g. "foo.bar"). The caller still rejects "." and "..".
var segmentRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ParseInstallSource parses "github:owner/repo[/subdir]" into its components.
// Each segment must match segmentRe AND must not be "." or "..". The whole
// "owner/repo[/subdir]" must also match ownerRepoRe (defense in depth).
//
// Other prefixes (e.g. "local:/path") can be added later; MVP only supports
// github:.
func ParseInstallSource(src string) (InstallSource, error) {
	if !strings.HasPrefix(src, "github:") {
		return InstallSource{}, fmt.Errorf("skills: unsupported source (only github: supported): %q", src)
	}
	body := strings.TrimPrefix(src, "github:")
	if !ownerRepoRe.MatchString(body) {
		return InstallSource{}, fmt.Errorf("skills: source must be owner/repo[/subdir], got %q", body)
	}
	parts := strings.Split(body, "/")
	for _, seg := range parts {
		if !segmentRe.MatchString(seg) {
			return InstallSource{}, fmt.Errorf("skills: invalid segment %q (allowed: [A-Za-z0-9._-])", seg)
		}
		if seg == "." || seg == ".." {
			return InstallSource{}, fmt.Errorf("skills: segment %q rejected (path traversal)", seg)
		}
	}
	if len(parts) < 2 {
		return InstallSource{}, fmt.Errorf("skills: source must have at least owner/repo")
	}
	out := InstallSource{Owner: parts[0], Repo: parts[1]}
	if len(parts) > 2 {
		out.Subdir = strings.Join(parts[2:], "/")
	}
	return out, nil
}

// CloneImpl is the git-clone abstraction. Production uses realClone (a thin
// wrapper around `git clone --depth 1`); tests inject a stub that copies a
// local "remote" dir. Decoupling lets the test suite exercise install logic
// without network access.
type CloneImpl interface {
	Clone(ctx context.Context, owner, repo, intoDir string) error
}

// realClone runs `git clone --depth 1 https://github.com/<owner>/<repo>.git`.
type realClone struct{}

func (realClone) Clone(ctx context.Context, owner, repo, intoDir string) error {
	url := fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", url, intoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone %s/%s: %w\n%s", owner, repo, err, out)
	}
	return nil
}

// CloneStub is a test-only CloneImpl. AsRemote is a fixture root whose direct
// children are repositories. GB3: descend into the requested repo before
// copying, so source github:fake-remote/evil-scripts produces
// staging/SKILL.md, not staging/evil-scripts/SKILL.md.
type CloneStub struct {
	AsRemote string
}

// Clone copies a repository into the staging directory for install validation.
func (c *CloneStub) Clone(_ context.Context, _, repo, intoDir string) error {
	return os.CopyFS(intoDir, os.DirFS(filepath.Join(c.AsRemote, repo)))
}

// Install fetches the source into a staging dir, validates it, removes any
// remote marker files, and atomically renames it into dstRoot. Returns the
// installed skill's name (the dir name under dstRoot).
//
// Security invariants:
//   - Source parsing rejects ".", "..", whitespace, shell metachars.
//   - Staging is a tempdir; any symlink in the clone is rejected (Walk Lstat).
//   - Remote .trusted / .disabled markers are removed (never trust remote
//     assertions).
//   - Rename is atomic on POSIX; target must not exist (refuse overwrite).
//   - Containment check: the final path must be inside dstRoot.
//
// `clone` may be nil — production passes nil to use realClone; tests pass a
// CloneStub.
func Install(src string, dstRoot string, clone CloneImpl) (string, error) {
	parsed, err := ParseInstallSource(src)
	if err != nil {
		return "", err
	}
	if clone == nil {
		clone = realClone{}
	}
	if err := os.MkdirAll(dstRoot, 0o755); err != nil {
		return "", fmt.Errorf("skills: mkdir dstRoot: %w", err)
	}
	rootAbs, err := filepath.Abs(dstRoot)
	if err != nil {
		return "", fmt.Errorf("skills: abs dstRoot: %w", err)
	}

	// Put staging beside dstRoot (outside the Loader-scanned root) so staging
	// and final target are on the same filesystem. This preserves rename-as-
	// publish semantics and avoids cross-volume failures (notably on Windows).
	staging, err := os.MkdirTemp(filepath.Dir(rootAbs), ".yanshi-skill-")
	if err != nil {
		return "", fmt.Errorf("skills: mkstaging: %w", err)
	}
	defer os.RemoveAll(staging)

	ctx := context.Background()
	if err := clone.Clone(ctx, parsed.Owner, parsed.Repo, staging); err != nil {
		return "", err
	}

	// Reject symlinks across the ENTIRE clone before resolving subdir or
	// reading SKILL.md. This prevents a symlinked subdir/SKILL.md from
	// escaping staging during validation.
	if err := rejectSymlinks(staging); err != nil {
		return "", err
	}

	// Locate the skill dir inside staging: subdir if specified, else staging
	// root (a single-skill repo).
	skillDir := staging
	if parsed.Subdir != "" {
		skillDir = filepath.Join(staging, filepath.FromSlash(parsed.Subdir))
	}
	stagingAbs, err := filepath.Abs(staging)
	if err != nil {
		return "", fmt.Errorf("skills: abs staging: %w", err)
	}
	skillAbs, err := filepath.Abs(skillDir)
	if err != nil || !isWithin(skillAbs, stagingAbs) {
		return "", fmt.Errorf("skills: subdir escapes staging")
	}
	skillDir = skillAbs

	// Validate SKILL.md exists and frontmatter name is safe.
	mdPath := filepath.Join(skillDir, "SKILL.md")
	name, _, _, err := readFrontmatter(mdPath)
	if err != nil {
		return "", fmt.Errorf("skills: read SKILL.md: %w", err)
	}
	if !validName(name) {
		return "", fmt.Errorf("skills: invalid skill name %q", name)
	}

	// Remove any remote marker files — never trust remote assertions about
	// trust/enabled state.
	for _, marker := range []string{".trusted", ".disabled"} {
		if err := os.Remove(filepath.Join(skillDir, marker)); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("skills: remove remote %s: %w", marker, err)
		}
	}

	// Containment + target-exists check.
	dst := filepath.Join(dstRoot, name)
	dstAbs, err := filepath.Abs(dst)
	if err != nil {
		return "", fmt.Errorf("skills: abs dst: %w", err)
	}
	if !isWithin(dstAbs, rootAbs) {
		return "", fmt.Errorf("skills: dst %q escapes dstRoot %q", dstAbs, rootAbs)
	}
	if _, err := os.Stat(dstAbs); err == nil {
		return "", fmt.Errorf("skills: target %q already exists (use /skill uninstall first)", dstAbs)
	}

	// Atomic rename into place.
	if err := os.Rename(skillDir, dstAbs); err != nil {
		return "", fmt.Errorf("skills: rename to final: %w", err)
	}
	return name, nil
}

// Uninstall removes the skill's directory from dstRoot. Refuses to traverse
// outside dstRoot.
func Uninstall(name, dstRoot string) error {
	if !validName(name) {
		return fmt.Errorf("skills: uninstall: invalid name %q", name)
	}
	target := filepath.Join(dstRoot, name)
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	rootAbs, err := filepath.Abs(dstRoot)
	if err != nil {
		return err
	}
	if !isWithin(targetAbs, rootAbs) {
		return fmt.Errorf("skills: uninstall: target escapes dstRoot")
	}
	if _, err := os.Stat(targetAbs); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("skills: uninstall: target %q does not exist", targetAbs)
		}
		return fmt.Errorf("skills: uninstall: stat target: %w", err)
	}
	if err := os.RemoveAll(targetAbs); err != nil {
		return fmt.Errorf("skills: uninstall: %w", err)
	}
	return nil
}

// rejectSymlinks walks dir and returns an error if any entry is a symlink.
// Skill packs must not contain symlinks (they could escape the skill dir on
// ReadFile or be used to smuggle trusted marker files).
func rejectSymlinks(dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Walk already calls Lstat (not Stat), so info is the lstat result.
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("skills: symlink rejected at %q", path)
		}
		return nil
	})
}

// isWithin reports whether child is equal to root or a descendant. Both inputs
// must be cleaned (filepath.Abs cleans).
func isWithin(child, root string) bool {
	if child == root {
		return true
	}
	return strings.HasPrefix(child, root+string(filepath.Separator))
}
