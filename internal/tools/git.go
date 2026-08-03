package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
)

// GitTools bundles guarded git sub-tools (status, diff).
type GitTools struct {
	Status *GuardedTool
	Diff   *GuardedTool
}

type gitFile struct {
	Path        string `json:"path"`
	Additions   int    `json:"additions"`
	Deletions   int    `json:"deletions"`
	Binary      bool   `json:"binary"`
	Patch       string `json:"patch,omitempty"`
	ArtifactRef string `json:"artifact_ref,omitempty"`
	Degraded    bool   `json:"degraded,omitempty"`
}

type gitDiffArgs struct {
	Scope struct {
		Kind string `json:"kind"`
		Ref  string `json:"ref,omitempty"`
	} `json:"scope"`
	Paths []string `json:"paths,omitempty"`
}

// gitEnvIsolation returns env entries that prevent git from reading/writing the
// user's global or system config: GIT_CONFIG_NOSYSTEM skips /etc/gitconfig, and
// XDG_CONFIG_HOME points at a throwaway dir under the work root so any
// home-directory ~/.gitconfig is neither consulted nor mutated.
func gitEnvIsolation(root string) []string {
	return []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"XDG_CONFIG_HOME=" + filepath.Join(root, ".yanshi", "tmp", "gitxdg"),
	}
}

// NewGitTools constructs the git_status and git_diff guarded tools.
func NewGitTools() *GitTools {
	return &GitTools{
		Status: NewGuardedTool("git_status", "Git status", "Return structured working-tree status.", 10*time.Second, nil, SyncStream(runGitStatus)),
		Diff: NewGuardedTool("git_diff", "Git diff", "Return per-file structured diff.", 30*time.Second,
			params(map[string]*schema.ParameterInfo{
				"scope": {Type: schema.Object, Required: true, SubParams: map[string]*schema.ParameterInfo{
					"kind": {Type: schema.String, Required: true, Enum: []string{"working_tree", "base_ref", "commit"}},
					"ref":  {Type: schema.String},
				}},
				"paths": {Type: schema.Array, ElemInfo: &schema.ParameterInfo{Type: schema.String}},
			}), SyncStream(runGitDiff)),
	}
}

func runGitStatus(ctx context.Context, argsJSON string) (string, error) {
	root := WorkRootFromContext(ctx)
	spec := secproc.SecureProcessSpec{
		Tool: "git_status", Program: "git", Dir: root,
		Args:           []string{"-c", "core.quotepath=false", "status", "--porcelain=v2", "-z", "--untracked-files=all"},
		Env:            gitEnvIsolation(root),
		UseSandboxTier: sandbox.ReadOnly,
	}
	res, err := secureCommandRunner(ctx, spec, 10*time.Second)
	if err != nil {
		return errorResult("git status: " + err.Error()), nil
	}
	return toJSON(parseGitStatusZ(res.Stdout)), nil
}

func runGitDiff(ctx context.Context, argsJSON string) (string, error) {
	root := WorkRootFromContext(ctx)
	var args gitDiffArgs
	if err := ParseArgs(argsJSON, &args); err != nil {
		return errorResult(err.Error()), nil
	}
	for _, p := range args.Paths {
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(root, p)
		}
		if !withinRoot(abs, root) {
			return "", fmt.Errorf("path %q outside work root", p)
		}
	}
	files, err := collectGitDiffFiles(ctx, root, args)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	return toJSON(struct {
		Scope struct {
			Kind string `json:"kind"`
			Ref  string `json:"ref,omitempty"`
		} `json:"scope"`
		Files []gitFile `json:"files"`
	}{Scope: args.Scope, Files: files}), nil
}

func collectGitDiffFiles(ctx context.Context, root string, args gitDiffArgs) ([]gitFile, error) {
	numstatSpec, patchBuilder, err := gitDiffCommands(args)
	if err != nil {
		return nil, err
	}
	numstatSpec.Dir = root
	numstatSpec.Tool = "git_diff"
	numstatSpec.UseSandboxTier = sandbox.ReadOnly
	numstatSpec.Env = gitEnvIsolation(root)
	res, err := secureCommandRunner(ctx, numstatSpec, 30*time.Second)
	if err != nil {
		return nil, err
	}
	entries := parseGitNumstatZ(res.Stdout)
	if args.Scope.Kind == "working_tree" {
		// Include staged changes
		cachedSpec := secproc.SecureProcessSpec{
			Tool: "git_diff", Program: "git", Dir: root,
			Args:           []string{"-c", "core.quotepath=false", "diff", "--cached", "--numstat", "-z", "--no-ext-diff", "--"},
			Env:            gitEnvIsolation(root),
			UseSandboxTier: sandbox.ReadOnly,
		}
		res, err := secureCommandRunner(ctx, cachedSpec, 10*time.Second)
		if err != nil {
			return nil, err
		}
		entries = append(entries, parseGitNumstatZ(res.Stdout)...)
		// Include untracked files
		untrackedSpec := secproc.SecureProcessSpec{
			Tool: "git_diff", Program: "git", Dir: root,
			Args:           []string{"-c", "core.quotepath=false", "ls-files", "--others", "--exclude-standard", "-z"},
			Env:            gitEnvIsolation(root),
			UseSandboxTier: sandbox.ReadOnly,
		}
		res, err = secureCommandRunner(ctx, untrackedSpec, 10*time.Second)
		if err != nil {
			return nil, err
		}
		for _, path := range strings.Split(strings.TrimRight(res.Stdout, "\x00"), "\x00") {
			if path == "" {
				continue
			}
			entries = append(entries, gitNumstatEntry{Path: path, Untracked: true})
		}
	}
	files := make([]gitFile, 0)
	for _, e := range filterGitByPaths(entries, args.Paths) {
		patch, binary, err := gitPatchForFile(ctx, root, e.Path, args, patchBuilder)
		if err != nil {
			return nil, err
		}
		entry := gitFile{Path: e.Path, Additions: e.Additions, Deletions: e.Deletions, Binary: binary || e.Binary}
		if !binary && !e.Binary && !e.Untracked {
			art := writeArtifactOrSpill(ctx, "git-diff", sanitizeLabel(e.Path), patch)
			entry.Patch = art.Summary
			entry.ArtifactRef = art.ArtifactRef
			entry.Degraded = art.Degraded
		}
		files = append(files, entry)
	}
	return files, nil
}

func gitDiffCommands(args gitDiffArgs) (secproc.SecureProcessSpec, func(path string) secproc.SecureProcessSpec, error) {
	baseArgs := []string{"-c", "core.quotepath=false"}
	switch args.Scope.Kind {
	case "working_tree":
		numstat := secproc.SecureProcessSpec{Program: "git", Args: append(baseArgs, "diff", "--numstat", "-z", "--no-ext-diff", "--")}
		return numstat, func(path string) secproc.SecureProcessSpec {
			return secproc.SecureProcessSpec{Program: "git", Args: append(baseArgs, "diff", "--no-ext-diff", "--binary", "--", path)}
		}, nil
	case "base_ref":
		if err := validateGitRef(args.Scope.Ref); err != nil {
			return secproc.SecureProcessSpec{}, nil, err
		}
		numstat := secproc.SecureProcessSpec{Program: "git", Args: append(baseArgs, "diff", "--numstat", "-z", "--no-ext-diff", args.Scope.Ref+"...HEAD", "--")}
		return numstat, func(path string) secproc.SecureProcessSpec {
			return secproc.SecureProcessSpec{Program: "git", Args: append(baseArgs, "diff", "--no-ext-diff", "--binary", args.Scope.Ref+"...HEAD", "--", path)}
		}, nil
	case "commit":
		if err := validateGitRef(args.Scope.Ref); err != nil {
			return secproc.SecureProcessSpec{}, nil, err
		}
		numstat := secproc.SecureProcessSpec{Program: "git", Args: append(baseArgs, "show", "--numstat", "-z", "--format=", args.Scope.Ref, "--")}
		return numstat, func(path string) secproc.SecureProcessSpec {
			return secproc.SecureProcessSpec{Program: "git", Args: append(baseArgs, "show", "--format=", "--binary", args.Scope.Ref, "--", path)}
		}, nil
	default:
		return secproc.SecureProcessSpec{}, nil, fmt.Errorf("unknown scope %q", args.Scope.Kind)
	}
}

func gitPatchForFile(ctx context.Context, root, path string, args gitDiffArgs, build func(string) secproc.SecureProcessSpec) (string, bool, error) {
	_ = args
	spec := build(path)
	spec.Dir = root
	spec.Tool = "git_diff"
	spec.UseSandboxTier = sandbox.ReadOnly
	spec.Env = gitEnvIsolation(root)
	res, err := secureCommandRunner(ctx, spec, 15*time.Second)
	if err != nil {
		return "", false, err
	}
	binary := strings.Contains(res.Stdout, "Binary files") || res.Stdout == ""
	return res.Stdout, binary, nil
}

type gitNumstatEntry struct {
	Path      string
	Additions int
	Deletions int
	Binary    bool
	Untracked bool
}

func parseGitNumstatZ(stdout string) []gitNumstatEntry {
	var out []gitNumstatEntry
	// git diff --numstat -z outputs records separated by NUL. Each record is:
	//   {additions}TAB{deletions}TAB{path}NUL
	// Split on NUL to get individual records, then split each on TAB.
	for _, record := range strings.Split(strings.TrimRight(stdout, "\x00"), "\x00") {
		if record == "" {
			continue
		}
		parts := strings.Split(record, "\t")
		if len(parts) < 3 {
			continue
		}
		adds, dels, path := parts[0], parts[1], parts[2]
		e := gitNumstatEntry{Path: path}
		if adds != "-" {
			fmt.Sscanf(adds, "%d", &e.Additions)
		} else {
			e.Binary = true
		}
		if dels != "-" {
			fmt.Sscanf(dels, "%d", &e.Deletions)
		} else {
			e.Binary = true
		}
		out = append(out, e)
	}
	return out
}

func parseGitStatusZ(stdout string) any {
	type entry struct {
		Path string `json:"path"`
		XY   string `json:"xy"`
	}
	var entries []entry
	for _, record := range strings.Split(strings.TrimRight(stdout, "\x00"), "\x00") {
		if record == "" {
			continue
		}
		// Untracked: "? <path>"
		if strings.HasPrefix(record, "? ") {
			entries = append(entries, entry{XY: "??", Path: record[2:]})
			continue
		}
		// Tracked: "1 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <path>"
		// or "2 <XY> ... <path>" etc.
		parts := strings.SplitN(record, " ", 3)
		if len(parts) < 2 {
			continue
		}
		if len(parts) == 3 {
			// For "1" or "2" type entries: extract XY from parts[1],
			// extract path from the end of parts[2]
			xy := parts[1]
			tail := parts[2]
			// path is the last field in the tail
			subFields := strings.Split(tail, " ")
			path := subFields[len(subFields)-1]
			entries = append(entries, entry{XY: xy, Path: path})
		}
	}
	return struct {
		Entries []entry `json:"entries"`
	}{Entries: entries}
}

func filterGitByPaths(entries []gitNumstatEntry, paths []string) []gitNumstatEntry {
	if len(paths) == 0 {
		return entries
	}
	allow := map[string]bool{}
	for _, p := range paths {
		allow[p] = true
	}
	var out []gitNumstatEntry
	for _, e := range entries {
		if allow[e.Path] {
			out = append(out, e)
		}
	}
	return out
}

var gitRefDisallowed = strings.NewReplacer(" ", "", "\t", "", "\n", "", "\r", "", "\x00", "")

func validateGitRef(ref string) error {
	if ref == "" || gitRefDisallowed.Replace(ref) != ref {
		return fmt.Errorf("invalid git ref %q", ref)
	}
	return nil
}

func sanitizeLabel(path string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, path)
}
