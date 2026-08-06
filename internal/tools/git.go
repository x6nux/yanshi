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

// gitEnvIsolation returns env entries that keep git from reading or writing the
// operator's global and system configuration.
//
// Three variables, because each closes a different file and no two of them
// overlap:
//
//   - GIT_CONFIG_NOSYSTEM=1 skips /etc/gitconfig.
//   - GIT_CONFIG_GLOBAL redirects ~/.gitconfig (git 2.32+).
//   - XDG_CONFIG_HOME redirects $XDG_CONFIG_HOME/git/config.
//
// The middle one used to be missing, and this comment used to claim
// XDG_CONFIG_HOME covered it. It does not: git consults the XDG copy only when
// ~/.gitconfig is ABSENT, so on every machine that has one — which is every
// machine where `git config --global user.email` has ever been run — the
// operator's global config was read in full. Measured with a global
// core.excludesFile of `*.go`: git_status reported a clean tree for a work
// tree with an untracked .go file in it. A config the tool silently obeys can
// suppress the very changes the model is asking about.
//
// On git older than 2.32 the second entry is ignored and behaviour falls back
// to what it was before — no worse, still not isolated. That floor is above
// the 2.24 the base_ref/commit diff scopes need, so it is stated here rather
// than in the tool description: isolation degrades, nothing breaks.
//
// The throwaway paths sit under the work root rather than in a temp dir so a
// process that does write config leaves it somewhere attributable.
func gitEnvIsolation(root string) []string {
	throwaway := filepath.Join(root, ".yanshi", "tmp", "gitxdg")
	return []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + filepath.Join(throwaway, "gitconfig"),
		"XDG_CONFIG_HOME=" + throwaway,
	}
}

// gitCapture runs one git command and folds BOTH failure shapes into err: a
// launch error AND a non-zero exit code.
//
// Every way git can fail — not a repository, corrupt index, unreadable object,
// ambiguous ref, permission denied — writes its reason to stderr, exits
// non-zero and leaves stdout EMPTY. git.go used to read only res.Stdout, and
// the parsers turn an empty stdout into zero records, so git_status answered
// {"entries":null} — a clean working tree — for a repository it could not read
// at all, and git_diff answered {"files":[]}. Both look like a successful
// "nothing changed", which is the one answer that makes the model stop
// looking.
//
// Non-zero is unambiguously a failure for ALL of git.go's call sites, which is
// why the check lives here and not per-caller: a non-zero exit code carries
// normal semantics only under --exit-code / --quiet (`git diff --quiet`
// returns 1 to mean "there are differences"), and none of the commands built
// in this file pass either flag. Their success-with-nothing-to-report case is
// exit 0 with empty stdout — a clean tree, an empty diff, no untracked files —
// which stays a success here. Any future git command in this file that DOES
// use --exit-code must bypass gitCapture and document why.
//
// The error text carries git's own stderr because that is the only place the
// reason exists; res is returned even on failure so callers may still inspect
// partial output.
func gitCapture(ctx context.Context, spec secproc.SecureProcessSpec, timeout time.Duration) (commandResult, error) {
	res, err := secureCommandRunner(ctx, spec, timeout)
	if err != nil {
		return res, fmt.Errorf("%s: %w", gitCommandLabel(spec), err)
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("%s: exited %d: %s", gitCommandLabel(spec), res.ExitCode, commandFailureTail(res))
	}
	return res, nil
}

// gitCommandLabel renders the invocation as "git <subcommand>" for error
// messages, skipping the leading `-c key=value` configuration pairs every spec
// in this file carries so the label names the sub-command the model asked for
// ("git status", "git diff") rather than "git -c".
func gitCommandLabel(spec secproc.SecureProcessSpec) string {
	for i := 0; i < len(spec.Args); i++ {
		if spec.Args[i] == "-c" {
			i++
			continue
		}
		if strings.HasPrefix(spec.Args[i], "-") {
			continue
		}
		return "git " + spec.Args[i]
	}
	return "git"
}

// NewGitTools constructs the git_status and git_diff guarded tools.
func NewGitTools() *GitTools {
	return &GitTools{
		Status: NewGuardedTool("git_status", "Git status", "Return structured working-tree status.", 10*time.Second, nil, SyncStream(runGitStatus)),
		Diff: NewGuardedTool("git_diff", "Git diff", "Return per-file structured diff. The base_ref and commit scopes require git 2.24+ (they pass --end-of-options); working_tree has no such requirement.", 30*time.Second,
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
	res, err := gitCapture(ctx, spec, 10*time.Second)
	if err != nil {
		return errorResult(err.Error()), nil
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
	res, err := gitCapture(ctx, numstatSpec, 30*time.Second)
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
		res, err := gitCapture(ctx, cachedSpec, 10*time.Second)
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
		res, err = gitCapture(ctx, untrackedSpec, 10*time.Second)
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
	for _, e := range filterGitByPaths(mergeNumstatByPath(entries), args.Paths) {
		// Untracked files are skipped entirely rather than probed. `git diff`
		// renders no content for a path git does not track, so the probe was
		// guaranteed to come back empty -- and the empty-output heuristic
		// below reads empty as "binary". Every untracked text file was
		// therefore reported to the model as a binary blob, at the cost of one
		// pointless subprocess each.
		if e.Untracked {
			files = append(files, gitFile{Path: e.Path, Additions: e.Additions, Deletions: e.Deletions, Binary: e.Binary})
			continue
		}
		patch, binary, err := gitPatchForFile(ctx, root, e.Path, args, patchBuilder)
		if err != nil {
			return nil, err
		}
		entry := gitFile{Path: e.Path, Additions: e.Additions, Deletions: e.Deletions, Binary: binary || e.Binary}
		if !binary && !e.Binary {
			art := writeArtifactOrSpill(ctx, "git-diff", sanitizeLabel(e.Path), patch)
			entry.Patch = art.Summary
			entry.ArtifactRef = art.ArtifactRef
			entry.Degraded = art.Degraded
		}
		files = append(files, entry)
	}
	return files, nil
}

// gitEndOfOptions is git's own end-of-option-parsing marker (git 2.24+, Nov
// 2019): every argv element after it is read as a revision or pathspec, never
// as an option, no matter what it starts with.
//
// It is the SECOND layer under validateGitRef, and the two are deliberately
// not redundant. validateGitRef is enforced by yanshi and therefore still runs
// on the git versions that predate this marker; gitEndOfOptions is enforced by
// git and therefore survives a future call site here that forgets to validate
// — which is exactly how the ref hole got in.
//
// Say the pre-2.24 half precisely, because "validateGitRef still covers those
// versions" is true and misleading at the same time: on git < 2.24 the marker
// is not a no-op that leaves a validated-but-unmarked argv, it is an argument
// git does not know. `git diff --end-of-options <ref>` there fails outright,
// and the two scopes fail in two different voices because they run two
// different commands: the base_ref scope's `git diff` prints its usage block
// and exits non-zero, while `git show` (the commit scope) goes through
// setup_revisions and answers `fatal: unrecognized argument:
// --end-of-options`. Both are measurable today by substituting any unknown
// option for the marker in the exact argv this file builds.
//
// Note which voice base_ref does NOT get, because the obvious guess is wrong:
// `git diff` also has an `error: invalid option: <opt>` line, but it comes
// from the no-revision path (builtin_diff_files), and base_ref always passes
// `<ref>...HEAD`, so that line is structurally unreachable here. Predicting it
// in a bug report would send the reader looking for a message git never prints.
//
// So git_diff's base_ref and commit scopes do not degrade on those versions,
// they stop working — the tool returns the git error for every call. Only the
// working_tree scope, which emits no marker, keeps running. That makes git 2.24
// (Nov 2019) a hard floor for two of the three scopes, and the git_diff tool
// description says so where the operator and the model can actually see it. Do
// not "fix" a pre-2.24 report by dropping the marker: that reopens the ref hole
// for everyone.
//
// Note the placement rule: `--end-of-options` must come after all real options
// and before the first revision, and `--` still separates revisions from
// pathspecs after it.
//
// Pathspec operands are NOT protected by this marker and do not need to be:
// every path in this file is already emitted after the `--` separator, where
// git's own parser has stopped looking for options.
const gitEndOfOptions = "--end-of-options"

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
		numstat := secproc.SecureProcessSpec{Program: "git", Args: append(baseArgs, "diff", "--numstat", "-z", "--no-ext-diff", gitEndOfOptions, args.Scope.Ref+"...HEAD", "--")}
		return numstat, func(path string) secproc.SecureProcessSpec {
			return secproc.SecureProcessSpec{Program: "git", Args: append(baseArgs, "diff", "--no-ext-diff", "--binary", gitEndOfOptions, args.Scope.Ref+"...HEAD", "--", path)}
		}, nil
	case "commit":
		if err := validateGitRef(args.Scope.Ref); err != nil {
			return secproc.SecureProcessSpec{}, nil, err
		}
		numstat := secproc.SecureProcessSpec{Program: "git", Args: append(baseArgs, "show", "--numstat", "-z", "--format=", gitEndOfOptions, args.Scope.Ref, "--")}
		return numstat, func(path string) secproc.SecureProcessSpec {
			return secproc.SecureProcessSpec{Program: "git", Args: append(baseArgs, "show", "--format=", "--binary", gitEndOfOptions, args.Scope.Ref, "--", path)}
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
	res, err := gitCapture(ctx, spec, 15*time.Second)
	if err != nil {
		return "", false, err
	}
	// An empty patch here means git had nothing textual to print for a file the
	// numstat pass already listed as changed: a binary blob (git only emits the
	// literal patch under --binary for tracked binaries it can encode) or an
	// untracked file, whose content git diff never renders. Reachable only
	// after gitCapture confirmed exit 0 — before that, this line also swallowed
	// every git failure as "binary", since a crashed git prints nothing.
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

// splitPorcelainV2 pulls the XY field and the path out of a porcelain v2
// record whose path begins after exactly nFields space-separated fields.
//
// Counting fields is the whole point. The previous implementation took the
// LAST space-separated field as the path, which silently truncated every
// tracked path containing a space to its final word -- and paths with spaces
// are ordinary. Porcelain v2's layout before the path is fixed-width in
// FIELDS (not bytes), so SplitN with a known count is exact, and everything
// after it is the path verbatim, spaces included.
func splitPorcelainV2(record string, nFields int) (xy, path string, ok bool) {
	parts := strings.SplitN(record, " ", nFields+1)
	if len(parts) != nFields+1 {
		return "", "", false
	}
	return parts[1], parts[nFields], true
}

func parseGitStatusZ(stdout string) any {
	type entry struct {
		Path string `json:"path"`
		XY   string `json:"xy"`
	}
	var entries []entry
	// Porcelain v2 is NUL-delimited, and a rename record is followed by a
	// SEPARATE NUL-delimited field carrying the original path. That field is
	// not a record, so the loop consumes it explicitly rather than letting the
	// next iteration mistake it for one and invent a phantom entry.
	records := strings.Split(strings.TrimRight(stdout, "\x00"), "\x00")
	for i := 0; i < len(records); i++ {
		record := records[i]
		if record == "" {
			continue
		}
		switch {
		case strings.HasPrefix(record, "? "):
			// Untracked: "? <path>"
			entries = append(entries, entry{XY: "??", Path: record[2:]})
		case strings.HasPrefix(record, "1 "):
			// 1 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <path>
			if xy, path, ok := splitPorcelainV2(record, 8); ok {
				entries = append(entries, entry{XY: xy, Path: path})
			}
		case strings.HasPrefix(record, "2 "):
			// 2 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <score> <path>
			// followed by a separate field holding the original path.
			if xy, path, ok := splitPorcelainV2(record, 9); ok {
				entries = append(entries, entry{XY: xy, Path: path})
			}
			i++ // skip the original-path field
		}
		// "u" (unmerged) and "#" (header) records are deliberately not
		// reported here: git_status's contract is the working-tree entry list,
		// and an unmerged path needs a richer shape than {path, xy}.
	}
	return struct {
		Entries []entry `json:"entries"`
	}{Entries: entries}
}

// mergeNumstatByPath folds the three sources collectGitDiffFiles queries for a
// working-tree diff -- unstaged numstat, --cached numstat, and ls-files
// --others -- into one entry per path, preserving first-seen order.
//
// Without this a file that is BOTH staged and modified again in the working
// tree appears twice, because the two numstat runs were simply appended. The
// duplicate is not cosmetic: each entry drives its own gitPatchForFile call,
// so the model receives the same path twice with two different partial diffs
// and no indication that they are halves of one change.
//
// Line counts add, which matches what the caller asked for: "how much did this
// file change", not "how much did it change in the index". Untracked is
// sticky because a path reported by ls-files --others has no diff at all and
// its patch must stay suppressed.
func mergeNumstatByPath(entries []gitNumstatEntry) []gitNumstatEntry {
	merged := make([]gitNumstatEntry, 0, len(entries))
	idx := make(map[string]int, len(entries))
	for _, e := range entries {
		if at, ok := idx[e.Path]; ok {
			merged[at].Additions += e.Additions
			merged[at].Deletions += e.Deletions
			merged[at].Binary = merged[at].Binary || e.Binary
			merged[at].Untracked = merged[at].Untracked || e.Untracked
			continue
		}
		idx[e.Path] = len(merged)
		merged = append(merged, e)
	}
	return merged
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

// validateGitRef screens a model-supplied revision before it is spliced into
// git's argv.
//
// The leading-dash rule comes from validateArgvOperand and is the security
// property: this function used to strip-compare whitespace and NUL only, which
// let `--output=/etc/anything` through as a "ref" and turned the ReadOnly
// git_diff tool into an arbitrary-file-write primitive (git happily accepts
// `--output` after the sub-command and writes the diff there). See
// validateArgvOperand for why the fix is a shape rule and not a flag blacklist.
//
// The whitespace rule is kept on top of it as a plain sanity check, not a
// security boundary: git-check-ref-format forbids space, tab, newline and NUL
// in a ref name, so a value carrying one is a caller bug worth naming early
// rather than letting git reject it three processes later.
func validateGitRef(ref string) error {
	if err := validateArgvOperand("git ref", ref); err != nil {
		return err
	}
	if gitRefDisallowed.Replace(ref) != ref {
		return fmt.Errorf("invalid git ref %q: contains whitespace", ref)
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
