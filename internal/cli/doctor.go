package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/x6nux/yanshi/internal/acp"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/i18n"
	corekeymap "github.com/x6nux/yanshi/internal/keymap"
	"github.com/x6nux/yanshi/internal/lockfile"
	"github.com/x6nux/yanshi/internal/lsp"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secrets"
	"github.com/x6nux/yanshi/internal/shell"
	"github.com/x6nux/yanshi/internal/store"
)

// DoctorStatus is the tri-state outcome of a single doctor check.
type DoctorStatus string

const (
	// StatusOK means the check passed.
	StatusOK DoctorStatus = "ok"
	// StatusWarn means the check found a non-fatal issue worth flagging.
	StatusWarn DoctorStatus = "warn"
	// StatusFail means the check found a problem that blocks or degrades boot.
	StatusFail DoctorStatus = "fail"
)

// CheckResult is the outcome of one doctor check. Message must never carry
// secrets: API keys are reported as "set"/"not set", never as their value.
type CheckResult struct {
	Name    string       `json:"name"`
	Status  DoctorStatus `json:"status"`
	Message string       `json:"message"`
}

// DoctorReport aggregates the per-check results of a doctor run.
type DoctorReport struct {
	Checks []CheckResult `json:"checks"`
}

// DoctorOptions configures RunDoctor.
type DoctorOptions struct {
	// ConfigPath is the YAML config to validate (same path bootstrap.Build uses).
	ConfigPath string
	// Root is the project root for the lockfile check; defaults to os.Getwd.
	Root string
	// Release promotes release-blocking warns to fails (e.g. port-in-use,
	// config-version anomalies). Used by the release runbook: `yanshi doctor
	// --release` must exit 0 before cutting a release. Docs: upgrade-guide.md.
	Release bool
}

// ExitCode maps the report to a process exit code: 0 when every check is ok, 1
// when at least one is warn (and none fail), 2 when at least one fails.
func (r DoctorReport) ExitCode() int {
	var hasWarn, hasFail bool
	for _, c := range r.Checks {
		switch c.Status {
		case StatusWarn:
			hasWarn = true
		case StatusFail:
			hasFail = true
		}
	}
	switch {
	case hasFail:
		return 2
	case hasWarn:
		return 1
	default:
		return 0
	}
}

// counts tallies checks by status.
func (r DoctorReport) counts() (ok, warn, fail int) {
	for _, c := range r.Checks {
		switch c.Status {
		case StatusOK:
			ok++
		case StatusWarn:
			warn++
		case StatusFail:
			fail++
		}
	}
	return ok, warn, fail
}

// RenderText writes a human-readable, column-aligned report to w. Each line is
// "[TAG] name message" where TAG is OK/WARN/FAIL padded to a fixed width so the
// name column lines up. Ends with a one-line summary. No color, so the output
// is pipe-friendly and deterministic for tests.
func (r DoctorReport) RenderText(w io.Writer) {
	for _, c := range r.Checks {
		tag := "[" + strings.ToUpper(string(c.Status)) + "]"
		fmt.Fprintf(w, "%-6s %-16s %s\n", tag, c.Name, c.Message)
	}
	ok, warn, fail := r.counts()
	fmt.Fprintf(w, "\n%d ok, %d warn, %d fail\n", ok, warn, fail)
}

// jsonReport is the machine-readable shape emitted by RenderJSON.
type jsonReport struct {
	Checks  []CheckResult `json:"checks"`
	Summary struct {
		OK   int `json:"ok"`
		Warn int `json:"warn"`
		Fail int `json:"fail"`
	} `json:"summary"`
}

// RenderJSON writes a machine-readable report to w as indented JSON. The shape
// is {"checks":[{name,status,message}],"summary":{ok,warn,fail}}.
func (r DoctorReport) RenderJSON(w io.Writer) error {
	var jr jsonReport
	jr.Checks = r.Checks
	jr.Summary.OK, jr.Summary.Warn, jr.Summary.Fail = r.counts()
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(jr)
}

// RunDoctor executes every doctor check in a fixed order and returns the
// aggregate report. Each check is independent: a config-load failure downgrades
// dependent checks to a "skipped" warn rather than aborting the run, so doctor
// is usable in incomplete environments (e.g. a CI box with no providers and no
// external agent CLIs). Task 2 wires the config/database/providers/directories/
// sandbox checks; Task 3 inserts acp/lockfile/port.
func RunDoctor(ctx context.Context, opts DoctorOptions) DoctorReport {
	root := opts.Root
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	cfg, cfgErr := config.Load(opts.ConfigPath)

	var checks []CheckResult
	checks = append(checks, checkConfig(opts.ConfigPath, cfg, cfgErr))
	checks = append(checks, checkConfigVersion(cfg, cfgErr, opts.Release))
	checks = append(checks, checkDatabase(cfg, cfgErr))
	checks = append(checks, checkProviders(cfg, cfgErr))
	checks = append(checks, checkACP())
	checks = append(checks, checkLockfile(root))
	checks = append(checks, checkPort(cfg, cfgErr, opts.Release))
	checks = append(checks, checkDirectories(cfg, cfgErr))
	checks = append(checks, checkSandbox(cfg, cfgErr, root))
	checks = append(checks, checkPTY())
	checks = append(checks, checkMCP(cfg, cfgErr))
	checks = append(checks, checkLSP(ctx, root))
	// I-1 (C3 remediation): the first production consumer of
	// internal/llm/eino's local-runtime discovery package. See
	// doctorlocalruntimes.go's package comment for why it lives here and
	// why it never fails/warns on its own account.
	checks = append(checks, checkLocalRuntimes(ctx))
	checks = append(checks, checkPermissions(cfg, cfgErr))
	// S3: is the file that decides what the agent may do sitting where the
	// agent can write it? See doctorpolicy.go for why this is a warn and not a
	// startup refusal.
	checks = append(checks, checkPolicyScope(cfg, cfgErr, opts.ConfigPath, root))
	checks = append(checks, checkPolicyFilePerms(opts.ConfigPath))
	// B-2 (2026-08-29 review, RC-10): same question as checkPolicyScope, asked
	// of llm.providers[].auth.command instead of `profiles:`. See
	// checkAuthCommandScope's doc comment for why it needs its own check
	// rather than being folded into checkPolicyScope's PolicyActive branch.
	checks = append(checks, checkAuthCommandScope(cfg, cfgErr))
	// D3 (S10/O03/I18N1/C15) doctor checks: surface locale, keymap, vim, and
	// high-contrast posture. Each check returns a fixed safe string; raw error
	// text or API key values are never echoed because the underlying
	// validators return bounded messages.
	checks = append(checks, checkWAL(cfg, cfgErr))
	checks = append(checks, checkKeyringAvailability(cfg, cfgErr))
	checks = append(checks, checkLocaleConfig(cfg, cfgErr))
	checks = append(checks, checkKeymapConfig(cfg, cfgErr))
	checks = append(checks, checkHighContrastConfig(cfg, cfgErr))
	return DoctorReport{Checks: checks}
}

// skipped returns a warn result noting the check could not run because an
// earlier dependency (the config) failed to load. The raw cfgErr text is
// NOT echoed: a config-load failure often involves a raw api_key value
// (C1's gate rejects them) and that value may itself be a secret. The
// dedicated checkConfig result carries the precise message for operators
// running doctor directly; later checks just say "config not loaded".
func skipped(name string, cfgErr error) CheckResult {
	return CheckResult{Name: name, Status: StatusWarn, Message: "skipped: config not loaded"}
}

// checkConfig validates that the YAML parses. A failure here is fail-level
// because bootstrap.Build rejects an unloadable config outright. The raw
// cfgErr text is deliberately NOT echoed: a config-load failure often
// involves a raw api_key value (C1's gate rejects them) and that value may
// itself be a secret. Operators running doctor see the cfgErr through the
// direct `yanshi serve` / `yanshi auth` startup path; doctor's job here is
// to flag the failure, not reproduce the leak.
func checkConfig(path string, cfg *config.Config, cfgErr error) CheckResult {
	if cfgErr != nil {
		return CheckResult{Name: "config", Status: StatusFail,
			Message: fmt.Sprintf("load %q failed (run `yanshi serve` for the precise error; messages may echo raw config values)", path)}
	}
	addr := cfg.Server.HTTPAddr
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	return CheckResult{
		Name:    "config",
		Status:  StatusOK,
		Message: fmt.Sprintf("%s loaded (http_addr=%s, %d provider(s))", path, addr, len(cfg.LLM.Providers)),
	}
}

// checkConfigVersion reports the loaded config's schema_version against
// SupportedSchemaVersion. Two reachable states after T7's Load gate:
//   - config loaded cleanly -> schema_version == supported -> OK
//   - config rejected because schema_version > supported -> cfgErr carries the
//     schema_version message; surface it here (warn normally, fail under
//     --release) rather than the generic "skipped: config not loaded", so the
//     release runbook points operators at the real problem.
//
// A lower-than-supported value is normalized to supported by Load's in-memory
// migration, so it never reaches this check as a mismatch.
func checkConfigVersion(cfg *config.Config, cfgErr error, release bool) CheckResult {
	if cfgErr != nil {
		// A schema_version rejection (too high) is a config-version problem,
		// not a generic config-load problem. checkConfig already echoes the
		// raw load failure as fail; here we add the version-specific angle.
		if strings.Contains(cfgErr.Error(), "schema_version") {
			st := StatusWarn
			if release {
				st = StatusFail
			}
			return CheckResult{Name: "config-version", Status: st,
				Message: fmt.Sprintf("config rejected: %v (upgrade yanshi or lower schema_version)", cfgErr)}
		}
		return skipped("config-version", cfgErr)
	}
	switch {
	case cfg.SchemaVersion == config.SupportedSchemaVersion:
		return CheckResult{Name: "config-version", Status: StatusOK,
			Message: fmt.Sprintf("schema_version=%d (supported)", cfg.SchemaVersion)}
	default:
		// Unreachable in practice (Load normalizes), but kept defensive: if a
		// future code path bypasses Load's migration, do not silently pass.
		st := StatusWarn
		if release {
			st = StatusFail
		}
		return CheckResult{Name: "config-version", Status: st,
			Message: fmt.Sprintf("schema_version=%d != supported=%d",
				cfg.SchemaVersion, config.SupportedSchemaVersion)}
	}
}

// checkWAL reads the SQLite journal_mode (read-only: a SELECT of the current
// mode, never a PRAGMA that changes it). Soft-depends on F1 (WAL1): the store
// opens with journal_mode=WAL, so on a healthy F1 build this is OK. It does
// NOT fail and does NOT flip the mode — it only reports.
func checkWAL(cfg *config.Config, cfgErr error) CheckResult {
	if cfgErr != nil {
		return skipped("wal", cfgErr)
	}
	path := cfg.Storage.SQLitePath
	display := path
	if display == "" {
		display = "<unset>"
	}
	st, err := store.Open(path)
	if err != nil {
		return CheckResult{Name: "wal", Status: StatusWarn,
			Message: fmt.Sprintf("open %q to read journal_mode: %v", display, err)}
	}
	// Read-only probe: SELECT the current mode, do not write/PRAGMA-set.
	// Same pattern as checkDatabase (store.Open + st.DB.QueryRow + st.Close).
	mode := ""
	_ = st.DB.QueryRow("PRAGMA journal_mode").Scan(&mode)
	_ = st.Close()
	if mode == "wal" {
		return CheckResult{Name: "wal", Status: StatusOK,
			Message: "journal_mode=wal" + sidecarSizes(path)}
	}
	return CheckResult{Name: "wal", Status: StatusWarn,
		Message: fmt.Sprintf("journal_mode=%q (WAL not active; expected wal on F1 builds)", mode)}
}

// sidecarSizes reports the size of the -wal and -shm files next to the database,
// as a suffix for the wal check's message.
//
// journal_mode alone answers "is WAL on"; it does not answer "is the WAL under
// control", which is the failure an operator actually needs to see. A WAL that
// never checkpoints grows without bound and the database looks healthy by every
// other measure. Reported after Close, so the numbers reflect the steady state
// rather than this probe's own transaction.
//
// Missing files are normal — SQLite removes both on a clean close — and are
// reported as absent rather than as an error.
func sidecarSizes(dbPath string) string {
	if dbPath == "" {
		return ""
	}
	var parts []string
	for _, suffix := range []string{"-wal", "-shm"} {
		fi, err := os.Stat(dbPath + suffix)
		switch {
		case err == nil:
			parts = append(parts, fmt.Sprintf("%s=%dB", suffix, fi.Size()))
		case os.IsNotExist(err):
			parts = append(parts, suffix+"=absent")
		default:
			parts = append(parts, suffix+"=?")
		}
	}
	return ", " + strings.Join(parts, ", ")
}

// checkKeyringAvailability probes the OS keyring via Available() (a sentinel
// read, never a write). On a -tags nokeyring build Available() returns
// ErrKeyringUnavailable; since nokeyring is the default release variant, that
// is reported as OK (a note), not a failure — secrets simply fall back to the
// encrypted file. Any other availability error is a warn.
func checkKeyringAvailability(cfg *config.Config, cfgErr error) CheckResult {
	if cfgErr != nil {
		return skipped("keyring", cfgErr)
	}
	kr := secrets.NewOSKeyringStore()
	if err := kr.Available(); err != nil {
		if errors.Is(err, secrets.ErrKeyringUnavailable) {
			return CheckResult{Name: "keyring", Status: StatusOK,
				Message: "OS keyring disabled in this build (nokeyring); secrets fall back to file"}
		}
		return CheckResult{Name: "keyring", Status: StatusWarn,
			Message: fmt.Sprintf("OS keyring unavailable: %v", err)}
	}
	return CheckResult{Name: "keyring", Status: StatusOK, Message: "OS keyring available"}
}

// checkDatabase verifies the configured SQLite path opens and migrations apply,
// mirroring what bootstrap.Build does on every boot. Opening the prod DB is a
// safe, idempotent side effect (CREATE TABLE IF NOT EXISTS + ADD COLUMN IF
// MISSING); the connection is closed immediately. SQLite file locking handles
// the brief window during which doctor holds the connection alongside a running
// server.
func checkDatabase(cfg *config.Config, cfgErr error) CheckResult {
	if cfgErr != nil {
		return skipped("database", cfgErr)
	}
	path := cfg.Storage.SQLitePath
	display := path
	if display == "" {
		display = "<unset>"
	}
	st, err := store.Open(path)
	if err != nil {
		return CheckResult{Name: "database", Status: StatusFail, Message: fmt.Sprintf("open %q: %v", display, err)}
	}
	var jm string
	_ = st.DB.QueryRow("PRAGMA journal_mode").Scan(&jm)
	_ = st.Close()
	extra := ""
	if jm == "wal" {
		walSize := fileSize(path + "-wal")
		shmSize := fileSize(path + "-shm")
		extra = fmt.Sprintf(" (%s, -wal=%s -shm=%s)", jm, walSize, shmSize)
	} else if jm != "" {
		extra = fmt.Sprintf(" (%s)", jm)
	}
	return CheckResult{Name: "database", Status: StatusOK, Message: fmt.Sprintf("%s opened (migrations applied)%s", display, extra)}
}

func fileSize(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "absent"
	}
	return fmt.Sprintf("%dKB", fi.Size()/1024)
}

// validProviderKinds mirrors the kinds bootstrap/einollm know how to build.
var validProviderKinds = map[string]bool{
	"openai":           true,
	"openai-responses": true,
	"anthropic":        true,
}

// checkProviders validates the llm.providers block. No providers is a warn (the
// app still boots with the fake model); a provider missing kind/model/api_key is
// a fail (that provider will error at call time). API keys are reported as
// "set"/"not set" only — never printed.
func checkProviders(cfg *config.Config, cfgErr error) CheckResult {
	if cfgErr != nil {
		return skipped("providers", cfgErr)
	}
	provs := cfg.LLM.Providers
	if len(provs) == 0 {
		return CheckResult{Name: "providers", Status: StatusWarn, Message: "no providers configured (fake model will be used; set llm.providers in config)"}
	}
	var problems []string
	withKey := 0
	for i, p := range provs {
		name := p.Name
		if name == "" {
			name = fmt.Sprintf("providers[%d]", i)
		}
		if p.Kind == "" {
			problems = append(problems, name+": missing kind")
			continue
		}
		if !validProviderKinds[p.Kind] {
			problems = append(problems, fmt.Sprintf("%s: unknown kind %q", name, p.Kind))
			continue
		}
		if p.Model == "" {
			problems = append(problems, name+": missing model")
		}
		if p.APIKey == "" {
			problems = append(problems, name+": api_key not set")
		} else {
			withKey++
		}
	}
	if len(problems) > 0 {
		return CheckResult{Name: "providers", Status: StatusFail, Message: fmt.Sprintf("%d provider(s): %s", len(provs), strings.Join(problems, "; "))}
	}
	return CheckResult{Name: "providers", Status: StatusOK, Message: fmt.Sprintf("%d provider(s) ok, %d with api_key", len(provs), withKey)}
}

// checkACP reports whether each known external agent CLI can be launched. ACP
// agents are optional for chat (only goal/external-agent flows spawn them), so
// a missing binary is a warn, not a fail. claudecode/codex launch via `npx`, so
// the probe checks npx presence — a faithful "can we spawn it?" proxy — but
// note this does NOT verify the npm package itself is installed.
func checkACP() CheckResult {
	names := acp.AgentNames()
	var lines []string
	missing := 0
	for _, name := range names {
		argv, err := acp.LaunchSpec(name)
		if err != nil {
			continue // AgentNames returns only known names; guard anyway.
		}
		bin := argv[0]
		if _, err := exec.LookPath(bin); err != nil {
			lines = append(lines, fmt.Sprintf("%s: %q not in PATH", name, bin))
			missing++
		} else {
			lines = append(lines, fmt.Sprintf("%s: %q ok", name, bin))
		}
	}
	status := StatusOK
	if missing > 0 {
		status = StatusWarn
	}
	return CheckResult{Name: "acp", Status: status, Message: strings.Join(lines, "; ")}
}

// checkLockfile reports the per-project backend lockfile state. Absent is ok
// (no backend running — the normal idle state); alive is ok (a backend is up);
// stale is a warn (a dead PID still owns the lockfile, to be reclaimed on the
// next launch by lockfile.Acquire).
func checkLockfile(root string) CheckResult {
	lf, err := lockfile.Read(root)
	if err == lockfile.ErrNotFound {
		return CheckResult{Name: "lockfile", Status: StatusOK, Message: fmt.Sprintf("no lockfile for %s (no backend running)", root)}
	}
	if err != nil {
		return CheckResult{Name: "lockfile", Status: StatusWarn, Message: fmt.Sprintf("read lockfile: %v", err)}
	}
	if lf.Alive() {
		return CheckResult{Name: "lockfile", Status: StatusOK, Message: fmt.Sprintf("backend running (pid=%d addr=%s)", lf.PID, lf.Addr)}
	}
	return CheckResult{Name: "lockfile", Status: StatusWarn, Message: fmt.Sprintf("stale lockfile (pid=%d not alive; reclaimed on next launch)", lf.PID)}
}

// checkPort verifies the configured HTTP address can be bound. "Address in use"
// is a warn, not a fail: another backend may legitimately be listening there
// (discovery would connect to it), or a stale process may hold it.
func checkPort(cfg *config.Config, cfgErr error, release bool) CheckResult {
	if cfgErr != nil {
		return skipped("port", cfgErr)
	}
	addr := cfg.Server.HTTPAddr
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// Release-blocking: the configured port must be free before shipping,
		// since a port held by a stale backend means the release binary cannot
		// bind it cleanly. Without --release this is only a warn (a running
		// backend during dev is expected).
		st := StatusWarn
		if release {
			st = StatusFail
		}
		return CheckResult{Name: "port", Status: st, Message: fmt.Sprintf("%s not bindable: %v (a backend may already be running)", addr, err)}
	}
	_ = ln.Close()
	return CheckResult{Name: "port", Status: StatusOK, Message: fmt.Sprintf("%s is free", addr)}
}

// checkDirectories validates the configured skill/VCS directories. All are
// advisory (warn): skills.Loader.Load skips a missing root dir (os.IsNotExist ->
// continue), and plugin discovery is non-fatal in bootstrap, so a missing dir
// does not block boot. The worktree dir is created (MkdirAll) because bootstrap
// writes worktrees into it.
func checkDirectories(cfg *config.Config, cfgErr error) CheckResult {
	if cfgErr != nil {
		return skipped("directories", cfgErr)
	}
	var problems []string
	checked := 0

	builtin := cfg.Skills.BuiltinDir
	if builtin == "" {
		builtin = "skills"
	}
	checked++
	if _, err := os.Stat(builtin); err != nil {
		problems = append(problems, fmt.Sprintf("skills.builtin_dir %q: %v", builtin, err))
	}
	for _, entry := range []struct{ label, dir string }{
		{"skills.user_dir", cfg.Skills.UserDir},
		{"skills.plugin_dir", cfg.Skills.PluginDir},
	} {
		if entry.dir == "" {
			continue
		}
		checked++
		if _, err := os.Stat(expandHomeDir(entry.dir)); err != nil {
			problems = append(problems, fmt.Sprintf("%s %q: %v", entry.label, entry.dir, err))
		}
	}
	wt := cfg.VCS.WorktreeDir
	if wt == "" {
		wt = "~/.yanshi/worktrees"
	}
	checked++
	if err := os.MkdirAll(expandHomeDir(wt), 0o755); err != nil {
		problems = append(problems, fmt.Sprintf("vcs.worktree_dir %q: %v", wt, err))
	}

	if len(problems) > 0 {
		return CheckResult{Name: "directories", Status: StatusWarn, Message: fmt.Sprintf("%d checked: %s", checked, strings.Join(problems, "; "))}
	}
	return CheckResult{Name: "directories", Status: StatusOK, Message: fmt.Sprintf("%d director(ies) ok", checked)}
}

// checkSandbox reports the posture the process will actually run under, by
// building the same sandbox bootstrap builds and reading its CapabilityReport.
//
// It used to return a fixed "not implemented yet" warning regardless of
// configuration -- which was honest about ITSELF but useless about the system:
// an operator who set tier: full-access and one who left the default got the
// same line, and neither learned whether OS isolation was actually enforced.
// A doctor check that cannot distinguish those two is not reporting on the
// sandbox at all.
//
// It goes through sandbox.ParseTier and sandbox.New rather than reading the
// config strings directly, because the question is not "what did the operator
// type" but "what will the runtime do with it" -- a tier typo silently falls
// back to read-only, and doctor has to show the fallback, not the typo.
func checkSandbox(cfg *config.Config, cfgErr error, workRoot string) CheckResult {
	if cfgErr != nil {
		return skipped("sandbox", cfgErr)
	}
	enabled := cfg.Security.Sandbox.Enabled == nil || *cfg.Security.Sandbox.Enabled
	if !enabled {
		return CheckResult{Name: "sandbox", Status: StatusWarn,
			Message: "disabled by config (security.sandbox.enabled: false): subprocesses run with this process's privileges"}
	}
	sb := sandbox.New(sandbox.Config{
		Enabled:       true,
		WorkspaceRoot: workRoot,
		Tier:          sandbox.ParseTier(cfg.Security.Sandbox.Tier),
		NetworkDeny:   cfg.Security.Sandbox.NetworkDeny,
	})
	return sandboxCheckResult(sb.Report())
}

// sandboxCheckResult renders one CapabilityReport as the doctor row.
//
// Split from checkSandbox so it can be driven with a SYNTHESISED report. The
// row that matters most for W-B-13 — a backend that is Enforced and OSIsolated
// and still leaves network_deny inert — is the Landlock-without-seccomp shape,
// which no developer machine and only one CI leg can produce. A test that could
// only observe the local host's real backend would silently assert nothing
// about it, which is the position the per-field warning exists to fix one layer
// down.
func sandboxCheckResult(rep sandbox.CapabilityReport) CheckResult {
	msg := fmt.Sprintf("tier %s, effective %s, backend %s", rep.Requested, rep.Effective, rep.Backend)
	if rep.Reason != "" {
		msg += " (" + rep.Reason + ")"
	}
	// W-B-13: the per-field warnings come BEFORE the Enforced branch, because
	// the case they exist for is the one where Enforced is TRUE. A Landlock
	// backend without seccomp reports os-isolated and enforced, and
	// `network_deny: true` does nothing on it; folding the warning into the
	// !Enforced arm would hide it in exactly that configuration.
	if len(rep.Unenforced) > 0 {
		msg += fmt.Sprintf("; WARNING: configured but NOT enforced by this backend: %s",
			strings.Join(rep.Unenforced, ", "))
	}
	if !rep.Enforced {
		// Warn rather than fail: this is the documented Phase-0 posture, not a
		// broken install. Saying OK here is what would be wrong -- the guard
		// layer is the containment boundary and the operator needs to know it.
		return CheckResult{Name: "sandbox", Status: StatusWarn,
			Message: msg + "; OS/network isolation NOT enforced — guard is the boundary"}
	}
	if len(rep.Unenforced) > 0 {
		// Enforced, but not all of it. StatusOK here would be the report saying
		// the configuration is fine while naming the part of it that is inert.
		return CheckResult{Name: "sandbox", Status: StatusWarn, Message: msg}
	}
	return CheckResult{Name: "sandbox", Status: StatusOK, Message: msg}
}

// checkPTY reports whether this host can give a shell session a real terminal.
//
// It is a doctor check rather than an error the operator meets at the first
// `shell_start {"pty": true}` because the failure is a property of the HOST, not
// of the request: a container built without /dev/ptmx, a devpts that is not
// mounted, a Windows older than 1809. Each of those is a one-line fix the
// operator would never derive from "shell: open /dev/ptmx: no such file or
// directory" arriving mid-turn as a tool result.
//
// Warn rather than fail when unavailable: everything except interactive
// sessions works fine on a host with no PTY — non-PTY spawns use the pipe
// console — so this is a reduced capability, not a broken install.
//
// It calls shell.PlatformPTYCapability, which is the SAME probe the spawn path
// consults, rather than deriving an answer from runtime.GOOS. Two independent
// answers to "can we do PTYs here" is how a report ends up disagreeing with the
// thing it reports on.
func checkPTY() CheckResult {
	cap := shell.PlatformPTYCapability()
	msg := fmt.Sprintf("backend %s (%s)", cap.Backend, cap.Reason)
	if !cap.Available {
		return CheckResult{Name: "pty", Status: StatusWarn,
			Message: msg + "; interactive sessions (shell_start pty=true) will fail on this host"}
	}
	return CheckResult{Name: "pty", Status: StatusOK, Message: msg}
}

// checkMCP reports the configured MCP servers and which of them are enabled.
//
// It used to return a fixed "no mcp servers exposed via chat" regardless of
// configuration -- a statement that was false for anyone who configured one,
// since the tools bridge registers every ready server's tools into the chat
// tool set. An operator debugging a server that would not connect got a line
// telling them none were expected.
//
// It deliberately does NOT start the servers. doctor runs before/alongside a
// live instance, and spawning stdio servers here would fork processes the
// operator did not ask for, race the real manager for stdio, and make a
// diagnostic command have side effects. Reachability is the health loop's job
// (internal/mcp/health.go); what doctor can answer honestly is what the
// configuration says and whether it is coherent.
func checkMCP(cfg *config.Config, cfgErr error) CheckResult {
	if cfgErr != nil {
		return skipped("mcp", cfgErr)
	}
	if len(cfg.MCP.Servers) == 0 {
		return CheckResult{Name: "mcp", Status: StatusOK, Message: "no mcp servers configured"}
	}
	var enabled, disabled, broken []string
	for name, sc := range cfg.MCP.Servers {
		switch {
		case sc.Transport == "http" && sc.URL == "":
			broken = append(broken, name+" (http transport with no url)")
		case sc.Transport != "http" && sc.Command == "":
			broken = append(broken, name+" (stdio transport with no command)")
		case !sc.Enabled:
			disabled = append(disabled, name)
		default:
			enabled = append(enabled, name)
		}
	}
	sort.Strings(enabled)
	sort.Strings(disabled)
	sort.Strings(broken)

	msg := fmt.Sprintf("%d configured: %d enabled", len(cfg.MCP.Servers), len(enabled))
	if len(enabled) > 0 {
		msg += " (" + strings.Join(enabled, ", ") + ")"
	}
	if len(disabled) > 0 {
		msg += fmt.Sprintf("; %d disabled (%s)", len(disabled), strings.Join(disabled, ", "))
	}
	if len(broken) > 0 {
		// A server that can never start is a configuration error, not a
		// preference: it will fail at every boot and the failure text names
		// the transport rather than the missing field.
		return CheckResult{Name: "mcp", Status: StatusFail,
			Message: msg + "; unusable: " + strings.Join(broken, ", ")}
	}
	return CheckResult{Name: "mcp", Status: StatusOK, Message: msg}
}

// checkLSP reports which language servers will actually start in this
// workspace, by asking lsp.New the same question bootstrap asks it.
//
// It used to probe one hardcoded binary (gopls) and report "present" or "not
// in PATH". That answered a question nobody has: gopls being installed says
// nothing about whether yanshi will use it, and it says nothing at all about
// the other five languages in the table. Worse, since W6 there is a SECOND
// gate -- a workspace marker file -- so "gopls present" and "gopls will run
// here" are now genuinely different facts, and only the second one is useful
// to someone whose diagnostics are empty.
//
// Going through lsp.New rather than re-deriving the rules is the point: two
// implementations of "will this server start" would disagree the first time
// either gate changes, and doctor's whole value is that it agrees with the
// runtime.
func checkLSP(ctx context.Context, root string) CheckResult {
	_ = ctx // no subprocess is spawned; kept for signature symmetry with the other checks
	mgr := lsp.New(lsp.Config{WorkRoot: root, Languages: lsp.DefaultLanguages()})
	usable := mgr.Languages()
	if len(usable) == 0 {
		return CheckResult{Name: "lsp", Status: StatusWarn,
			Message: "no language server will start here: none of the configured commands are on PATH, " +
				"or this workspace has no marker file (go.mod, Cargo.toml, tsconfig.json, ...)"}
	}
	names := make([]string, 0, len(usable))
	for lang, ls := range usable {
		names = append(names, lang+" ("+ls.Command+")")
	}
	sort.Strings(names)
	return CheckResult{Name: "lsp", Status: StatusOK,
		Message: fmt.Sprintf("%d language server(s) active here: %s", len(names), strings.Join(names, ", "))}
}

func checkPermissions(cfg *config.Config, cfgErr error) CheckResult {
	if cfgErr != nil {
		return skipped("permissions", cfgErr)
	}
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return CheckResult{Name: "permissions", Status: StatusWarn, Message: "no profiles configured (guard default-denies all tools)"}
	}
	details := make([]string, 0, len(names))
	for _, name := range names {
		prof := cfg.Profiles[name]
		tools := "no tools"
		if len(prof.Tools.Allow) > 0 {
			tools = fmt.Sprintf("%d tools allowed", len(prof.Tools.Allow))
		}
		details = append(details, fmt.Sprintf("%s (%s)", name, tools))
	}
	return CheckResult{Name: "permissions", Status: StatusOK, Message: strings.Join(details, "; ")}
}

// expandHomeDir resolves a leading "~" to the user's home directory. This
// mirrors bootstrap.expandHome and main.expandHome, both of which are
// unexported and therefore not reusable across package boundaries; keep this
// local copy in sync with them.
func expandHomeDir(p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// checkLocaleConfig verifies that cfg.I18N.UILocale resolves through the
// i18n.Bundle (LC_ALL/LANG detection for "auto", explicit value otherwise).
// An unsupported locale is a fail because the TUI silently falls back to
// English; the operator should fix the spelling.
func checkLocaleConfig(cfg *config.Config, cfgErr error) CheckResult {
	if cfgErr != nil {
		return skipped("locale", cfgErr)
	}
	b, err := i18n.NewBundle(cfg.I18N.UILocale)
	if err != nil {
		return CheckResult{Name: "locale", Status: StatusFail, Message: err.Error()}
	}
	return CheckResult{Name: "locale", Status: StatusOK,
		Message: fmt.Sprintf("persistent=%s effective=%s", b.Persistent(), b.Effective())}
}

// checkKeymapConfig validates the configured TUI keymap name and the raw
// binding overrides against the keymap core's Builder.
//
// The remedy this check points at is the YAML itself — `tui.bindings` in the
// config file named by DoctorOptions.ConfigPath — and that is deliberate
// rather than a fallback. There is no runtime surface that repairs a keymap:
// internal/keymap has exactly one production consumer, this function, and the
// TUI never builds a Map from config at all. An earlier message told the
// operator to run a `/keymap diagnostics` command; no such command is
// registered, so the one actionable instruction the product gave typed
// straight into "unknown command".
//
// Raw keys and actions are still NOT echoed: they are untrusted YAML text and
// may carry attacker-controlled content. Only the diagnostic Kind is surfaced,
// which internal/keymap draws from a closed set of four literals, so the
// operator learns WHICH class of mistake to look for without the message
// becoming an echo channel.
func checkKeymapConfig(cfg *config.Config, cfgErr error) CheckResult {
	if cfgErr != nil {
		return skipped("keymap", cfgErr)
	}
	name := cfg.TUI.KeymapName
	if name == "" {
		name = "default"
	}
	if name != "default" {
		return CheckResult{Name: "keymap", Status: StatusFail,
			Message: `unsupported keymap name; set tui.keymap to "default" in the config file`}
	}
	if m, err := corekeymap.NewDefaultBuilder(cfg.TUI.Bindings).Build(); err != nil {
		return CheckResult{Name: "keymap", Status: StatusFail,
			Message: "invalid key bindings; edit tui.bindings in the config file (" +
				keymapDiagSummary(m.Diagnostics()) + ")"}
	}
	return CheckResult{Name: "keymap", Status: StatusOK,
		Message: fmt.Sprintf("default keymap, %d override(s), no conflicts",
			len(cfg.TUI.Bindings))}
}

// keymapDiagSummary renders a per-kind tally of keymap diagnostics, e.g.
// "1 conflict, 2 unknown_action".
//
// Only Diagnostic.Kind is read. Kind is produced by internal/keymap from a
// closed set of four literals and never contains user input, whereas Key and
// RawKeys are verbatim YAML — see checkKeymapConfig for why that distinction
// is what makes this summary safe to print.
func keymapDiagSummary(ds []corekeymap.Diagnostic) string {
	counts := map[string]int{}
	for _, d := range ds {
		counts[d.Kind]++
	}
	kinds := make([]string, 0, len(counts))
	for k := range counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
	}
	if len(parts) == 0 {
		// Build only errors when it produced diagnostics, so this is
		// unreachable today; it keeps the message grammatical if that
		// invariant is ever loosened.
		return "no diagnostic detail"
	}
	return strings.Join(parts, ", ")
}

// checkHighContrastConfig surfaces the *bool high-contrast posture so the
// operator can confirm accessibility settings. Always StatusOK — there is no
// wrong value, only an unset (default false) or explicit true/false.
func checkHighContrastConfig(cfg *config.Config, cfgErr error) CheckResult {
	if cfgErr != nil {
		return skipped("high-contrast", cfgErr)
	}
	enabled := cfg.TUI.HighContrast != nil && *cfg.TUI.HighContrast
	return CheckResult{Name: "high-contrast", Status: StatusOK,
		Message: fmt.Sprintf("enabled=%v", enabled)}
}
