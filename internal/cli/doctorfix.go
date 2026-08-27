package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/x6nux/yanshi/internal/cli/embedded"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/lockfile"
)

// FixAction names one repair the doctor knows how to perform. The set is a
// closed allowlist, which is the first of the three gates this feature is built
// on (allowlist / backup / non-interactive refusal).
//
// A repair tool that can do "whatever the check suggested" is a repair tool
// that will one day rewrite a credential because a check's message changed.
// Naming each action as a constant means adding a repair is a code change with
// a test, not a message edit.
type FixAction string

const (
	// FixCreateDirs creates configured directories that do not exist. Purely
	// additive: it never touches an existing path.
	FixCreateDirs FixAction = "create-dirs"
	// FixConfigDefaults fills REQUIRED config fields that are missing, taking
	// the values from config.example.yaml. It never overwrites a field the
	// operator already set, and never writes anything into a credential field.
	FixConfigDefaults FixAction = "config-defaults"
	// FixStaleLockfile removes a lockfile whose recorded PID is not alive.
	FixStaleLockfile FixAction = "stale-lockfile"
	// FixFilePermissions tightens over-permissive modes on files that hold or
	// may hold credentials (the config file, the secrets file).
	FixFilePermissions FixAction = "file-permissions"
)

// fixAllowlist is the authoritative set of repairs. Anything not in here is
// not repairable, no matter what a check's message says.
//
// Two categories are deliberately absent and must stay absent:
//
//   - Provider credentials. A tool that can write an api_key can also write
//     the WRONG api_key, and the failure mode (silent auth against an account
//     the operator did not intend) is worse than the broken state it fixed.
//   - The database. Deleting or rebuilding a SQLite file destroys the session
//     history, the VCS commits and the task ledger in one step, and no doctor
//     check can distinguish "corrupt" from "written by a newer version".
var fixAllowlist = map[FixAction]string{
	FixCreateDirs:      "create configured directories that do not exist",
	FixConfigDefaults:  "fill missing required config fields from config.example.yaml",
	FixStaleLockfile:   "remove a lockfile whose owner process is gone",
	FixFilePermissions: "tighten world/group-readable modes on credential-bearing files",
}

// dangerousFixes are repairs that MODIFY an existing file rather than creating
// something absent. They are refused when doctor is not attached to a terminal.
//
// The reasoning is about who is watching, not about how risky the edit is in
// isolation: a repair that rewrites the operator's config file is fine when a
// human ran `yanshi doctor -fix` and can read the diff, and is not fine inside
// a CI job or a container entrypoint where the change lands unseen in an image
// and diverges from the repository copy. Creating a missing directory has no
// such asymmetry, so it runs everywhere.
var dangerousFixes = map[FixAction]bool{
	FixConfigDefaults:  true,
	FixFilePermissions: true,
}

// FixOptions configures RunDoctorFix.
type FixOptions struct {
	// ConfigPath is the YAML config to inspect and, for FixConfigDefaults, to
	// rewrite.
	ConfigPath string
	// ExamplePath is the template that supplies default values. Empty means
	// "config.example.yaml" next to the working directory.
	ExamplePath string
	// Root is the project root for the lockfile repair; defaults to os.Getwd.
	Root string
	// Interactive reports whether a human is watching. Production sets it from
	// a TTY probe (see StdinIsTerminal); tests set it directly. When false,
	// every action in dangerousFixes is refused.
	Interactive bool
	// DryRun plans the repairs and reports them without touching the disk.
	DryRun bool
	// Only, when non-empty, restricts the run to these actions. Names outside
	// fixAllowlist are rejected rather than ignored, so a typo fails loudly
	// instead of silently doing nothing.
	Only []FixAction
}

// FixOutcome is the result of attempting one repair.
type FixOutcome struct {
	Action FixAction `json:"action"`
	// Status is "fixed", "skipped" (nothing to do), "refused" (blocked by a
	// gate), or "failed" (attempted and errored).
	Status string `json:"status"`
	// Detail explains what happened. It never echoes a config VALUE, for the
	// same reason the checks do not: a config file is where api keys live.
	Detail string `json:"detail"`
	// Backup is the path of the backup taken before the change, when one was
	// taken. Empty for repairs that created something rather than edited it.
	Backup string `json:"backup,omitempty"`
}

// Fix status literals, so callers branch on constants rather than on spelling.
const (
	// FixStatusFixed means the repair ran and changed something.
	FixStatusFixed = "fixed"
	// FixStatusSkipped means there was nothing to repair.
	FixStatusSkipped = "skipped"
	// FixStatusRefused means a gate blocked the repair (non-interactive, or
	// not selected).
	FixStatusRefused = "refused"
	// FixStatusFailed means the repair was attempted and errored.
	FixStatusFailed = "failed"
)

// FixReport aggregates the outcomes of a doctor repair run.
type FixReport struct {
	Outcomes []FixOutcome `json:"outcomes"`
	// DryRun mirrors the option, so a reader of the report cannot mistake a
	// plan for a completed run.
	DryRun bool `json:"dry_run"`
}

// Changed reports whether any repair actually modified the system.
func (r FixReport) Changed() bool {
	for _, o := range r.Outcomes {
		if o.Status == FixStatusFixed {
			return true
		}
	}
	return false
}

// ExitCode maps the repair report to a process exit code: 2 when a repair was
// attempted and failed, 0 otherwise. A refusal is not a failure — it is the
// gate working — so it does not change the code.
func (r FixReport) ExitCode() int {
	for _, o := range r.Outcomes {
		if o.Status == FixStatusFailed {
			return 2
		}
	}
	return 0
}

// RenderText writes the repair outcomes in the same column-aligned shape as
// DoctorReport.RenderText, so the two read as one command's output.
func (r FixReport) RenderText(w io.Writer) {
	if r.DryRun {
		fmt.Fprintln(w, "(dry run: nothing was modified)")
	}
	for _, o := range r.Outcomes {
		tag := "[" + strings.ToUpper(o.Status) + "]"
		fmt.Fprintf(w, "%-9s %-18s %s\n", tag, o.Action, o.Detail)
		if o.Backup != "" {
			fmt.Fprintf(w, "%-9s %-18s backup: %s\n", "", "", o.Backup)
		}
	}
}

// FixActions returns the allowlisted repairs in a stable order, each with its
// one-line description. Used by the CLI to document -fix without duplicating
// the table.
func FixActions() []FixAction {
	out := make([]FixAction, 0, len(fixAllowlist))
	for a := range fixAllowlist {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// FixActionDescription returns the allowlist entry's description, or "" for an
// action that is not allowlisted.
func FixActionDescription(a FixAction) string { return fixAllowlist[a] }

// ErrUnknownFixAction is returned by RunDoctorFix when Only names an action
// outside the allowlist.
var ErrUnknownFixAction = errors.New("cli: unknown doctor fix action")

// StdinIsTerminal reports whether f is an interactive terminal, i.e. a human is
// plausibly watching.
//
// It asks the OS whether the descriptor is a TTY (term.IsTerminal, a tcgetattr
// on Unix and a GetConsoleMode on Windows) rather than checking
// os.ModeCharDevice. That distinction is the entire correctness of the gate:
// /dev/null IS a character device, so the mode-bit version returns true for
// `yanshi doctor -fix < /dev/null` -- which is precisely the shape a CI job, a
// container entrypoint and a systemd unit have. The check would have passed in
// every environment it exists to refuse, and it took a test asserting the
// refusal to notice.
func StdinIsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	// A closed descriptor makes Fd() return ^uintptr(0); term.IsTerminal
	// answers false for it rather than panicking, so no separate guard is
	// needed.
	return term.IsTerminal(int(f.Fd()))
}

// RunDoctorFix performs the allowlisted repairs and returns a report.
//
// The three gates, in the order they apply:
//  1. Allowlist — only actions in fixAllowlist can run at all.
//  2. Non-interactive refusal — actions in dangerousFixes are refused when
//     opts.Interactive is false.
//  3. Backup — any repair that modifies an existing file copies it aside first.
//
// It never returns a partial error: each repair's failure is recorded in its
// own outcome so one broken repair does not hide the others. The only returned
// error is a caller mistake (an unknown action in Only).
func RunDoctorFix(ctx context.Context, opts FixOptions) (FixReport, error) {
	_ = ctx // no subprocess or network call; kept for symmetry with RunDoctor
	selected, err := resolveFixSelection(opts.Only)
	if err != nil {
		return FixReport{}, err
	}
	root := opts.Root
	if root == "" {
		if wd, wderr := os.Getwd(); wderr == nil {
			root = wd
		}
	}
	// Left empty when the operator gave none: resolution now happens inside
	// fixConfigDefaults via LoadExampleTemplate, which falls back to the
	// compiled-in copy. Defaulting to the literal "config.example.yaml" here
	// would turn "no preference" into "this exact relative path", and
	// LoadExampleTemplate treats an explicit path as a hard requirement.
	examplePath := opts.ExamplePath

	report := FixReport{DryRun: opts.DryRun}
	for _, action := range FixActions() {
		if !selected[action] {
			report.Outcomes = append(report.Outcomes, FixOutcome{
				Action: action, Status: FixStatusRefused,
				Detail: "not selected for this run",
			})
			continue
		}
		if dangerousFixes[action] && !opts.Interactive {
			report.Outcomes = append(report.Outcomes, FixOutcome{
				Action: action, Status: FixStatusRefused,
				Detail: "modifies an existing file; refused without a terminal " +
					"(rerun interactively, or select it explicitly from a terminal)",
			})
			continue
		}
		report.Outcomes = append(report.Outcomes, runFixAction(action, opts, root, examplePath))
	}
	return report, nil
}

// resolveFixSelection turns Only into a set, rejecting names outside the
// allowlist. An empty Only selects every allowlisted action.
func resolveFixSelection(only []FixAction) (map[FixAction]bool, error) {
	if len(only) == 0 {
		out := make(map[FixAction]bool, len(fixAllowlist))
		for a := range fixAllowlist {
			out[a] = true
		}
		return out, nil
	}
	out := make(map[FixAction]bool, len(only))
	for _, a := range only {
		if _, ok := fixAllowlist[a]; !ok {
			return nil, fmt.Errorf("%w: %q (allowed: %s)",
				ErrUnknownFixAction, a, joinActions(FixActions()))
		}
		out[a] = true
	}
	return out, nil
}

// joinActions renders an action list for an error message.
func joinActions(actions []FixAction) string {
	parts := make([]string, len(actions))
	for i, a := range actions {
		parts[i] = string(a)
	}
	return strings.Join(parts, ", ")
}

// runFixAction dispatches one allowlisted repair.
func runFixAction(action FixAction, opts FixOptions, root, examplePath string) FixOutcome {
	switch action {
	case FixCreateDirs:
		return fixCreateDirs(opts.ConfigPath, opts.DryRun)
	case FixConfigDefaults:
		return fixConfigDefaults(opts.ConfigPath, examplePath, opts.DryRun)
	case FixStaleLockfile:
		return fixStaleLockfile(root, opts.DryRun)
	case FixFilePermissions:
		return fixFilePermissions(opts.ConfigPath, opts.DryRun)
	default:
		// Unreachable: resolveFixSelection rejects unknown actions and
		// FixActions only yields allowlisted ones. Kept so a future action
		// added to the allowlist without a handler fails visibly instead of
		// silently reporting success.
		return FixOutcome{Action: action, Status: FixStatusFailed,
			Detail: "no handler is wired for this allowlisted action"}
	}
}

// backupSuffix stamps a backup with the time it was taken, so repeated runs do
// not overwrite the one copy of the file the operator actually wants back.
func backupSuffix() string {
	return time.Now().UTC().Format(".bak-20060102T150405")
}

// backupFile copies path aside before it is modified and returns the backup
// path. It is the second of the three gates: no repair in this file edits an
// existing file without calling it first.
//
// The copy preserves the source mode, because the permission repair below
// works by comparing modes and a backup written with the process umask would
// look like a second violation on the next run.
func backupFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	mode := os.FileMode(0o600)
	if fi, statErr := os.Stat(path); statErr == nil {
		mode = fi.Mode().Perm()
	}
	backup := path + backupSuffix()
	for i := 1; ; i++ {
		if _, statErr := os.Stat(backup); os.IsNotExist(statErr) {
			break
		}
		backup = fmt.Sprintf("%s%s-%d", path, backupSuffix(), i)
	}
	if err := os.WriteFile(backup, data, mode); err != nil {
		return "", err
	}
	return backup, nil
}

// fixCreateDirs creates configured directories that are missing.
//
// Additive only: an existing path is left alone whatever it is, so this can
// never clobber a directory the operator populated. A path that exists as a
// FILE is reported rather than replaced — deleting it to make room is exactly
// the kind of "helpful" step that loses data.
func fixCreateDirs(configPath string, dryRun bool) FixOutcome {
	out := FixOutcome{Action: FixCreateDirs}
	cfg, err := config.Load(configPath)
	if err != nil {
		out.Status = FixStatusSkipped
		out.Detail = "config not loaded; nothing to create"
		return out
	}

	wanted := []string{}
	for _, dir := range []string{
		cfg.Skills.UserDir,
		cfg.Skills.PluginDir,
		cfg.VCS.WorktreeDir,
	} {
		if strings.TrimSpace(dir) != "" {
			wanted = append(wanted, expandHomeDir(dir))
		}
	}
	if cfg.VCS.WorktreeDir == "" {
		wanted = append(wanted, expandHomeDir("~/.yanshi/worktrees"))
	}

	var created, blocked []string
	for _, dir := range wanted {
		fi, statErr := os.Stat(dir)
		switch {
		case statErr == nil && fi.IsDir():
			continue
		case statErr == nil:
			blocked = append(blocked, dir+" (exists as a file; not replaced)")
			continue
		}
		if dryRun {
			created = append(created, dir)
			continue
		}
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			blocked = append(blocked, fmt.Sprintf("%s (%v)", dir, mkErr))
			continue
		}
		created = append(created, dir)
	}

	switch {
	case len(blocked) > 0:
		out.Status = FixStatusFailed
		out.Detail = fmt.Sprintf("%d created, %d could not be: %s",
			len(created), len(blocked), strings.Join(blocked, "; "))
	case len(created) == 0:
		out.Status = FixStatusSkipped
		out.Detail = "every configured directory already exists"
	default:
		out.Status = FixStatusFixed
		out.Detail = fmt.Sprintf("%d director(ies): %s",
			len(created), strings.Join(created, ", "))
	}
	return out
}

// requiredConfigKeys are the top-level YAML keys a usable config must carry.
// Each maps to the example file's block, which is where the default value
// comes from.
//
// llm.providers is NOT here on purpose: a provider block without a working
// api_key is worse than an absent one, because the app boots, every call
// fails at request time, and the operator has a config that looks complete.
var requiredConfigKeys = []string{"server", "storage", "profiles"}

// fixConfigDefaults copies missing required top-level blocks from the example
// file into the operator's config.
//
// It works at the raw-YAML level rather than through config.Config because the
// point is to leave everything else byte-identical: a marshal/unmarshal round
// trip would reformat the file, drop every comment, and materialise every
// default as an explicit setting, so the next upgrade could not tell an
// operator's choice from a value doctor happened to write.
//
// Credential fields are never written. The example's api_key entries are
// ${ENV} references, and a block containing one is copied verbatim — which
// means the operator still has to supply the environment variable, exactly as
// if they had copied the example by hand.
func fixConfigDefaults(configPath, examplePath string, dryRun bool) FixOutcome {
	out := FixOutcome{Action: FixConfigDefaults}

	current, err := os.ReadFile(configPath)
	if err != nil {
		out.Status = FixStatusSkipped
		out.Detail = fmt.Sprintf("config %q is not readable; run `yanshi init` to create one", configPath)
		return out
	}
	// Same reason as `yanshi init`: doctor -fix runs in the operator's project,
	// not in the yanshi source tree, so a bare os.ReadFile here made the
	// config-defaults repair permanently unavailable to everyone it was written
	// for. An explicit -template that cannot be read is still an error, which
	// LoadExampleTemplate preserves.
	exampleText, exampleSource, err := LoadExampleTemplate(configPath, examplePath)
	if err != nil {
		out.Status = FixStatusSkipped
		out.Detail = fmt.Sprintf("%v; nothing to copy defaults from", err)
		return out
	}
	example := []byte(exampleText)
	examplePath = exampleSource

	missing := missingTopLevelKeys(string(current), requiredConfigKeys)
	if len(missing) == 0 {
		out.Status = FixStatusSkipped
		out.Detail = "every required config block is present"
		return out
	}

	var additions []string
	var unavailable []string
	for _, key := range missing {
		block := extractTopLevelBlock(string(example), key)
		if block == "" {
			unavailable = append(unavailable, key)
			continue
		}
		additions = append(additions, block)
	}
	if len(additions) == 0 {
		out.Status = FixStatusFailed
		out.Detail = fmt.Sprintf("missing %s, and the template has no block for %s",
			strings.Join(missing, ", "), strings.Join(unavailable, ", "))
		return out
	}

	added := missing[:0:0]
	for _, key := range missing {
		if extractTopLevelBlock(string(example), key) != "" {
			added = append(added, key)
		}
	}
	if dryRun {
		out.Status = FixStatusFixed
		out.Detail = fmt.Sprintf("would add %s from %s", strings.Join(added, ", "), examplePath)
		return out
	}

	backup, err := backupFile(configPath)
	if err != nil {
		out.Status = FixStatusFailed
		out.Detail = fmt.Sprintf("could not back up %s before editing: %v", configPath, err)
		return out
	}
	out.Backup = backup

	merged := strings.TrimRight(string(current), "\n") + "\n\n" +
		"# added by `yanshi doctor -fix` from " + examplePath + "\n" +
		strings.Join(additions, "\n")
	mode := os.FileMode(0o600)
	if fi, statErr := os.Stat(configPath); statErr == nil {
		mode = fi.Mode().Perm()
	}
	if werr := os.WriteFile(configPath, []byte(merged), mode); werr != nil {
		out.Status = FixStatusFailed
		out.Detail = fmt.Sprintf("write %s: %v (original preserved at %s)", configPath, werr, backup)
		return out
	}
	out.Status = FixStatusFixed
	out.Detail = fmt.Sprintf("added %s from %s", strings.Join(added, ", "), examplePath)
	if len(unavailable) > 0 {
		out.Detail += fmt.Sprintf("; template has no block for %s", strings.Join(unavailable, ", "))
	}
	return out
}

// missingTopLevelKeys returns which of keys have no top-level mapping entry in
// the YAML text.
//
// A top-level key is one starting at column zero followed by a colon; anything
// indented belongs to a parent block. Commented-out lines do not count, which
// is the case that matters: config.example.yaml ships several blocks commented
// out, and treating those as present would leave the operator with the same
// missing block plus a doctor that claims it is there.
func missingTopLevelKeys(yamlText string, keys []string) []string {
	present := map[string]bool{}
	for _, line := range strings.Split(yamlText, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' || line[0] == '-' {
			continue
		}
		name, _, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		present[strings.TrimSpace(name)] = true
	}
	var missing []string
	for _, k := range keys {
		if !present[k] {
			missing = append(missing, k)
		}
	}
	return missing
}

// extractTopLevelBlock returns the YAML text of one top-level key including
// every indented line under it, or "" when the key is absent.
//
// Comment lines immediately preceding the key are included: in this repo the
// comments ARE the documentation for a block, and a config assembled without
// them is materially worse than the one the operator would have copied.
func extractTopLevelBlock(yamlText, key string) string {
	lines := strings.Split(yamlText, "\n")
	start := -1
	for i, line := range lines {
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' || line[0] == '-' {
			continue
		}
		name, _, found := strings.Cut(line, ":")
		if found && strings.TrimSpace(name) == key {
			start = i
			break
		}
	}
	if start < 0 {
		return ""
	}
	// Walk backwards over the contiguous comment block above the key.
	first := start
	for first > 0 && strings.HasPrefix(strings.TrimSpace(lines[first-1]), "#") {
		first--
	}
	end := start + 1
	for end < len(lines) {
		line := lines[end]
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '-' {
			end++
			continue
		}
		break
	}
	// Trim trailing blank lines so blocks concatenate cleanly.
	for end > start+1 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return strings.Join(lines[first:end], "\n") + "\n"
}

// fixStaleLockfile removes a lockfile whose owner process is gone.
//
// No backup is taken: a lockfile is derived state, its entire content is a PID
// and an address that are both already false, and lockfile.Acquire reclaims a
// stale one on the next launch anyway. Backing it up would leave litter in the
// cache directory that nothing ever reads.
//
// A LIVE lockfile is never removed. Deleting it would not stop the running
// backend, it would only make every window in the project unable to find it.
func fixStaleLockfile(root string, dryRun bool) FixOutcome {
	out := FixOutcome{Action: FixStaleLockfile}
	lf, err := lockfile.Read(root)
	if errors.Is(err, lockfile.ErrNotFound) {
		out.Status = FixStatusSkipped
		out.Detail = "no lockfile for this project"
		return out
	}
	if err != nil {
		out.Status = FixStatusFailed
		out.Detail = fmt.Sprintf("read lockfile: %v", err)
		return out
	}
	if lf.Alive() {
		out.Status = FixStatusSkipped
		out.Detail = fmt.Sprintf("backend is running (pid=%d); lockfile is not stale", lf.PID)
		return out
	}
	if dryRun {
		out.Status = FixStatusFixed
		out.Detail = fmt.Sprintf("would remove stale lockfile (pid=%d)", lf.PID)
		return out
	}
	if rerr := lockfile.Remove(root); rerr != nil {
		out.Status = FixStatusFailed
		out.Detail = fmt.Sprintf("remove lockfile: %v", rerr)
		return out
	}
	out.Status = FixStatusFixed
	out.Detail = fmt.Sprintf("removed stale lockfile (pid=%d was not alive)", lf.PID)
	return out
}

// credentialFileMode is the mode a file that may hold a secret must not exceed.
// 0600: owner read/write, nothing for group or other.
const credentialFileMode os.FileMode = 0o600

// fixFilePermissions tightens modes on files that hold or may hold credentials.
//
// It only ever REMOVES bits. A repair that could widen a mode would be a
// privilege escalation with a friendly name, and there is no state a doctor
// check can observe that justifies making a credential file more readable.
//
// On Windows the POSIX mode bits are not meaningful, so the repair reports
// "skipped" rather than pretending to have hardened anything.
func fixFilePermissions(configPath string, dryRun bool) FixOutcome {
	out := FixOutcome{Action: FixFilePermissions}
	targets := []string{configPath}
	if cfg, err := config.Load(configPath); err == nil && cfg.Secrets.FilePath != "" {
		targets = append(targets, expandHomeDir(cfg.Secrets.FilePath))
	}

	var tightened, problems []string
	for _, path := range targets {
		if strings.TrimSpace(path) == "" {
			continue
		}
		fi, err := os.Stat(path)
		if err != nil {
			continue // absent file is not an over-permissive file
		}
		perm := fi.Mode().Perm()
		if perm&^credentialFileMode == 0 {
			continue
		}
		if dryRun {
			tightened = append(tightened, fmt.Sprintf("%s (%04o -> %04o)", path, perm, credentialFileMode))
			continue
		}
		if cerr := os.Chmod(path, credentialFileMode); cerr != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", path, cerr))
			continue
		}
		tightened = append(tightened, fmt.Sprintf("%s (%04o -> %04o)", path, perm, credentialFileMode))
	}

	switch {
	case len(problems) > 0:
		out.Status = FixStatusFailed
		out.Detail = strings.Join(problems, "; ")
	case len(tightened) == 0:
		out.Status = FixStatusSkipped
		out.Detail = "no credential-bearing file is group- or world-accessible"
	default:
		out.Status = FixStatusFixed
		out.Detail = strings.Join(tightened, "; ")
	}
	return out
}

// DefaultExamplePath returns the template path doctor -fix and `yanshi init`
// read defaults from: config.example.yaml next to the config file when one is
// there, falling back to the working directory.
//
// It can return a path that does not exist. That is deliberate and callers must
// handle it — see LoadExampleTemplate, which is what they should call instead
// of reading this path directly.
func DefaultExamplePath(configPath string) string {
	if dir := filepath.Dir(configPath); dir != "" && dir != "." {
		candidate := filepath.Join(dir, "config.example.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "config.example.yaml"
}

// LoadExampleTemplate returns the config template to generate from, and the
// human-facing name of where it came from.
//
// An explicit path (operator passed -template) is a hard requirement: if it
// cannot be read that is an error, never a silent fall back to the embedded
// copy, because the operator asked for THAT file and quietly substituting a
// different one produces a config they did not ask for.
//
// With no explicit path, an on-disk config.example.yaml wins over the embedded
// copy — in the source tree that file is the one being edited, and generating
// from a stale compiled-in snapshot while the author stares at the updated file
// is a confusing failure. Everywhere else there is no such file and the
// embedded copy is used, which is the case that made this function necessary:
// `yanshi init` previously just failed outside the source tree.
func LoadExampleTemplate(configPath, explicitPath string) (template string, source string, err error) {
	if strings.TrimSpace(explicitPath) != "" {
		b, rerr := os.ReadFile(explicitPath)
		if rerr != nil {
			return "", "", fmt.Errorf("read template %q: %w", explicitPath, rerr)
		}
		return string(b), explicitPath, nil
	}
	onDisk := DefaultExamplePath(configPath)
	if b, rerr := os.ReadFile(onDisk); rerr == nil {
		return string(b), onDisk, nil
	}
	return embedded.ExampleConfig, "built-in config.example.yaml", nil
}
