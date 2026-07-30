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
	"time"

	"github.com/x6nux/yanshi/internal/acp"
	"github.com/x6nux/yanshi/internal/config"
	corekeymap "github.com/x6nux/yanshi/internal/keymap"
	"github.com/x6nux/yanshi/internal/i18n"
	"github.com/x6nux/yanshi/internal/lockfile"
	"github.com/x6nux/yanshi/internal/secrets"
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
	checks = append(checks, checkSandbox())
	checks = append(checks, checkMCP(cfg, cfgErr))
	checks = append(checks, checkLSP(ctx, root))
	checks = append(checks, checkPermissions(cfg, cfgErr))
	// D3 (S10/O03/I18N1/C15) doctor checks: surface credential-ref, locale,
	// keymap, vim, and high-contrast posture. Each check returns a fixed
	// safe string; raw error text or API key values are never echoed because
	// the underlying validators return bounded messages.
	checks = append(checks, checkSecretsRefs(cfg, cfgErr))
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
		return CheckResult{Name: "wal", Status: StatusOK, Message: "journal_mode=wal"}
	}
	return CheckResult{Name: "wal", Status: StatusWarn,
		Message: fmt.Sprintf("journal_mode=%q (WAL not active; expected wal on F1 builds)", mode)}
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

// checkSandbox is a placeholder. Full sandbox verification arrives with S08
// (M2); until then doctor flags it as a known gap so the report is honest
// rather than silently omitting it.
func checkSandbox() CheckResult {
	return CheckResult{Name: "sandbox", Status: StatusWarn, Message: "sandbox verification not implemented yet (arrives with S08 in M2)"}
}

func checkMCP(cfg *config.Config, cfgErr error) CheckResult {
	if cfgErr != nil {
		return skipped("mcp", cfgErr)
	}
	return CheckResult{
		Name:    "mcp",
		Status:  StatusOK,
		Message: "no mcp servers exposed via chat (vcs-mcp serves the ACP path)",
	}
}

func checkLSP(ctx context.Context, root string) CheckResult {
	probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	bin := "gopls"
	if _, err := exec.LookPath(bin); err != nil {
		return CheckResult{Name: "lsp", Status: StatusWarn, Message: fmt.Sprintf("lsp: %q not in PATH (optional; install for code intelligence)", bin)}
	}
	cmd := exec.CommandContext(probeCtx, bin, "-rpc.trace")
	cmd.Dir = root
	if err := cmd.Start(); err != nil {
		return CheckResult{Name: "lsp", Status: StatusWarn, Message: fmt.Sprintf("lsp: start %q: %v", bin, err)}
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return CheckResult{Name: "lsp", Status: StatusOK, Message: fmt.Sprintf("lsp: %q present", bin)}
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

// checkSecretsRefs verifies every provider api_key is either empty, a
// secret:// / env:// reference, or accepted under the legacy_insecure opt-in.
// This is the second gate (after the auth Manager's ParseCredentialRef): it
// catches raw literals that arrived via ${VAR} expansion in config.Load.
// Errors are deliberately bounded — a rejected raw literal may itself be a
// secret, so the message must not echo it.
func checkSecretsRefs(cfg *config.Config, cfgErr error) CheckResult {
	if cfgErr != nil {
		return skipped("secrets", cfgErr)
	}
	checked := 0
	for _, provider := range cfg.LLM.Providers {
		if provider.APIKey == "" {
			continue
		}
		checked++
		if _, err := secrets.ParseCredentialRef(provider.APIKey, cfg.Auth.LegacyInsecure); err != nil {
			return CheckResult{Name: "secrets", Status: StatusFail,
				Message: fmt.Sprintf("provider %q has an invalid credential reference", provider.Name)}
		}
	}
	return CheckResult{Name: "secrets", Status: StatusOK,
		Message: fmt.Sprintf("%d credential reference(s) valid", checked)}
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
// binding overrides against the keymap core's Builder. Diagnostic detail is
// intentionally not echoed — a raw key or action in YAML is untrusted text
// and may carry attacker-controlled content; the TUI's /keymap diagnostics
// render the typed breakdown.
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
			Message: "unsupported keymap name"}
	}
	if _, err := corekeymap.NewDefaultBuilder(cfg.TUI.Bindings).Build(); err != nil {
		return CheckResult{Name: "keymap", Status: StatusFail,
			Message: "key bindings are invalid; use /keymap diagnostics"}
	}
	return CheckResult{Name: "keymap", Status: StatusOK,
		Message: fmt.Sprintf("default keymap, %d override(s), no conflicts",
			len(cfg.TUI.Bindings))}
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
