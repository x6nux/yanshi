// Package main implements the yanshi CLI.
//
// Bare `yanshi` (no subcommand) launches a self-contained TUI that discovers a
// running backend for the current project (lockfile + healthz) or embeds one
// in-process. `chat` does the same and accepts --no-tui for the legacy line REPL.
// `serve` starts a shared daemon; `goal` runs the self-driven goal loop; `vcs-mcp`
// runs the autoVCS MCP server on stdio.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/x6nux/yanshi/internal/agent/goalloop"
	"github.com/x6nux/yanshi/internal/auth"
	"github.com/x6nux/yanshi/internal/bootstrap"
	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/cli/tui"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/lockfile"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secrets"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/vcs"
	vcsmcp "github.com/x6nux/yanshi/internal/vcs/mcp"
)

var usage = `yanshi ` + Version + ` — the CLI for the yanshi agent server.

Usage:
  yanshi                                self-contained TUI (discovers or embeds the backend)
  yanshi chat    [--no-tui] [-server URL] [-inprocess] [-fake-model] [-config FILE] [-token TOKEN]
  yanshi chat    [--no-tui] [-p "prompt" | stdin] [--input text|lines|jsonl] [-output text|jsonl] [-timeout 1m] [-resume ID]
  yanshi exec    [-p "prompt" | stdin] [--input text|lines|jsonl] [-output text|jsonl] [-timeout 1m] [-resume ID]
  yanshi serve   [-config config.yaml] [-fake-model] [-addr ADDR]
  yanshi app     [-config config.yaml] [-fake-model]
  yanshi goal    [-config config.yaml] [-fake-model] [-workdir DIR] [-agent claudecode] [-max-iters 5] [-max-tokens 0] [-goal "text"] [-tier auto|t0..t4]
  yanshi vcs-mcp (env-driven; spawned by the ACP adapter — YANSHI_DB_PATH/YANSHI_REPO_ID/YANSHI_WT_ID/YANSHI_AGENT/YANSHI_WORKTREE_DIR)
  yanshi init    [-config FILE] [-template FILE] [-force]
  yanshi daemon  status|stop|reload [-root DIR] [-json] [-config FILE] [-timeout 20s]
  yanshi schedule list|show|pause|resume|run-now|delete [ID] [-root DIR] [-json]
  yanshi provider add|list [-config FILE] [-name N] [-kind K] [-model M] [-api-key K] [-replace] [-json]
  yanshi acp     [-config config.yaml] [-fake-model]
  yanshi doctor [-config FILE] [-json] [-release] [-fix] [-fix-only LIST] [-fix-dry-run]
  yanshi pr      <PR-number> | <full-URL>
  yanshi enqueue [-config FILE] <session-id> <message...> | -list <session-id>
  yanshi auth    status|logout|device [-provider NAME] [-account NAME]
  yanshi auth    mcp-login <server> | mcp-logout <server>

Subcommands:
  (none)   Launch the self-contained TUI. Discovers a running backend for the
           current project via a lockfile, or embeds one in-process. WebSocket is
           the primary transport (multi-turn, tool-aware); SSE is the fallback.
  chat     Same TUI as the bare invocation. --no-tui drops to the shared headless
           runner (defaults to line input so one-prompt-per-line still works).
           The headless path adds JSONL output, resume, timeout, and stable exit
           codes (0/1/2/124/130). -server/-inprocess force a backend.
  exec     Headless single/multi-prompt runner. Reads prompts via -p, --file, or
           stdin (text/lines/jsonl). Prints assistant text to stdout (text mode)
           or one stable JSONL object per event (jsonl mode), with progress and
           session id on stderr. Stable exit codes: 0 ok, 1 runtime error, 2
           usage, 124 timeout, 130 cancelled. --resume continues a prior session
           once; --fake-model needs no API key.
  serve    Start the HTTP server as a shared daemon (SIGINT/SIGTERM to stop).
           Other yanshi invocations in the same project discover it.
  app      Run the JSON-RPC 2.0 app-server on stdio. Drives the same shared
           v1 agent service as HTTP; item streams arrive as item/updated
           notifications (one JSON object per line). Diagnostics go to stderr
           so stdout stays parseable. -fake-model needs no API key.
  goal     Run the self-driven goal loop (plan-implement-evaluate-judge).
  vcs-mcp  Run the autoVCS MCP server on stdio (spawned by the ACP adapter).
  init     Generate a config.yaml from config.example.yaml. Refuses to
           overwrite an existing config unless -force (which backs it up
           first). ${VAR} references are left as references — nothing writes
           a credential to disk — and the summary names every provider
           environment variable that is still unset.
  daemon   Operate an already-running backend for this project, found through
           the same lockfile the TUI uses. status prints pid / address /
           uptime / readiness (exit 0 only when ready). stop asks it to shut
           down and waits for the process to go. reload makes it re-read the
           config, applying what can be applied and REFUSING the rest with a
           reason — a listen address or a database path cannot change under a
           running process, and reload says so rather than pretending.
  schedule  Operate the scheduled automations held by the running daemon: list
           them with their next fire time, show one with its run history,
           pause / resume / run-now / delete. Creating an automation stays a
           model-facing tool; this is the operations surface.
  provider  Add or list the LLM providers in config.yaml. add prompts for
           whatever the flags omit (and needs every value as a flag when no
           terminal is attached, so it scripts). The API key goes into the
           secrets backend and only a secret:// reference is written to the
           config, so the file stays safe to copy and to attach to a report.
           Providers are bound at boot, so a new one needs a restart.
  acp      Speak the Agent Client Protocol as the AGENT on stdio, exposing
           yanshi's own orchestrator to an ACP host such as Zed. Protocol
           frames go to stdout and diagnostics to stderr, the same contract
           the app subcommand uses. This is the reverse of the ACP CLIENT the goal
           loop and acp_delegate use to drive somebody else's agent.
  doctor   One-time self-check of config, database, providers, ACP CLIs,
           lockfile, port, directories, and sandbox status. Prints ok/warn/fail
           per check; -json emits machine-readable output. Exit 0 all ok /
           1 warn / 2 fail. Never prints secrets. -fix additionally performs a
           closed allowlist of repairs (missing directories, missing required
           config blocks, dead lockfiles, over-permissive file modes), backing
           up every file it edits and refusing the file-editing ones when not
           attached to a terminal. It never touches provider credentials and
           never deletes a database.
  pr       Fetch a GitHub pull request into the session as context. Takes a
           PR number (run from the repo directory) or a full URL (any repo).
  enqueue  Queue a user message for a session, connected or not. It is stored
           in the project database and delivered, in enqueue order, the next
           time that session is resumed (exec/chat -resume). -list shows what
           is waiting without consuming it.
  auth     Manage authenticated sessions: RFC 8628 device flow (status /
           logout / device) and MCP OAuth (mcp-login / mcp-logout, the
           authorization_code + PKCE flow for an enterprise MCP server; the
           tokens go to the secrets backend and the refresh token is rotated
           automatically). Never echoes a secret. Provider api_keys are not
           managed here — use "yanshi provider add", which puts the key in the
           secrets backend too.
`

func main() {
	os.Exit(dispatch(os.Args, os.Stdin, os.Stdout, os.Stderr))
}

// dispatch is the testable top-level router: it mirrors the original main()
// switch but returns an exit code instead of calling os.Exit, so the routing
// logic (version flag, managed-invocation gating, subcommand dispatch, unknown
// subcommand usage error) is unit-testable. argv includes argv[0].
func dispatch(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// The Landlock re-exec helper, FIRST — before --version, before -h, before
	// isManagedInvocation. This process was spawned by yanshi's own Linux
	// sandbox as /proc/self/exe: it must apply the ruleset to itself and then
	// execve the real program. Landlock can only be applied by the process that
	// will be confined, and Go cannot run code between fork and exec, so the
	// re-exec is the only available shape.
	//
	// It is deliberately not a case in the switch below: the token starts with
	// underscores so gendocs' subcommand scanner does not see it (checked by
	// cmd/gendocs's TestSubcommandListMatchesDispatch), and it is not a
	// subcommand — no operator should ever type it.
	//
	// On success RunLandlockHelper does not return; it has replaced this
	// process image. Any return is a failure, and it must be a failure exit:
	// falling through would run the unconfined program that the sandbox was
	// asked to confine.
	if len(argv) > 1 && argv[1] == sandbox.LandlockHelperArg() {
		if err := sandbox.RunLandlockHelper(argv); err != nil {
			fmt.Fprintf(stderr, "yanshi: %v\n", err)
		}
		return exitErr
	}
	if len(argv) == 2 && (argv[1] == "--version" || argv[1] == "-version") {
		fmt.Fprintln(stdout, "yanshi", Version)
		return exitOK
	}
	// `yanshi -h` / `--help` prints the full usage text to stdout and exits 0.
	// Without this, -h falls through to runDefault, whose flag.FlagSet treats
	// -h as flag.ErrHelp and exits 2 with the auto-generated flag list. CI smoke
	// (yanshi -h) and CLAUDE.md both expect a clean exit 0.
	if len(argv) == 2 && (argv[1] == "-h" || argv[1] == "--help") {
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	// S10/O03 managed invocations (auth/doctor with optional leading --config)
	// route through the testable production dispatcher. The dispatcher reads
	// the global --config flag, parses the subcommand, and never calls os.Exit
	// itself — only this top-level main does, based on the returned code.
	if isManagedInvocation(argv[1:]) {
		return runCLI(argv, stdin, stdout, stderr)
	}
	if len(argv) < 2 {
		return runDefault(nil) // bare `yanshi` -> self-contained TUI
	}

	switch argv[1] {
	case "serve":
		return serve(argv[2:])
	case "chat":
		return chatTUI(argv[2:])
	case "exec":
		return runHeadlessCommand(argv[2:], "exec", stdin)
	case "app":
		return runApp(argv[2:], stdin, stdout)
	case "goal":
		return runGoal(argv[2:])
	case "vcs-mcp":
		return vcsMcp(argv[2:])
	case "init":
		return runInit(argv[2:], stdout, stderr)
	case "daemon":
		return runDaemon(argv[2:], stdout, stderr)
	case "schedule":
		return runSchedule(argv[2:], stdout, stderr)
	case "provider":
		return runProvider(argv[2:], stdin, stdout, stderr)
	case "acp":
		return runACPServer(argv[2:], stdin, stdout)
	case "enqueue":
		return runEnqueue(argv[2:], stdout)
	case "pr":
		if len(argv) < 3 {
			fmt.Fprintln(stderr, "Usage: yanshi pr <PR-number>")
			return exitErr
		}
		return runPR(context.Background(), argv[2])
	default:
		// A leading flag (e.g. `yanshi --fake-model`) is treated as a bare
		// invocation with flags, so the TUI launch works either way.
		if strings.HasPrefix(argv[1], "-") {
			return runDefault(argv[1:])
		}
		fmt.Fprintf(stderr, "unknown subcommand: %s\n\n%s", argv[1], usage)
		return exitUsage
	}
}

// authCLIDeps carries the auth-side collaborators a test can inject. The
// zero value is the production shape: real Clock / real Sleeper. Only the
// device-flow loopback E2E test sets these to deterministic fakes.
type authCLIDeps struct {
	Clock   auth.Clock
	Sleeper auth.Sleeper
}

// isManagedInvocation reports whether args (argv minus the program name)
// select a managed subcommand (auth or doctor), allowing a leading --config
// flag in either "--config FILE" or "--config=FILE" form. Used by main to
// decide whether to dispatch to the testable runCLI before the legacy
// per-subcommand switch.
func isManagedInvocation(args []string) bool {
	for len(args) > 0 {
		switch {
		case args[0] == "--config":
			if len(args) < 2 {
				return true // dispatcher prints the precise usage error
			}
			args = args[2:]
		case strings.HasPrefix(args[0], "--config="):
			args = args[1:]
		default:
			return args[0] == "auth" || args[0] == "doctor"
		}
	}
	return false
}

// parseManagedInvocation splits the argv tail into (cfgPath, sub, rest).
// cfgPath defaults to config.yaml when no --config is present. Returns an
// error when --config is given without a value or when no subcommand
// follows the global flags.
func parseManagedInvocation(args []string) (
	cfgPath, sub string,
	rest []string,
	err error,
) {
	cfgPath = "config.yaml"
	for len(args) > 0 {
		switch {
		case args[0] == "--config":
			if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
				return "", "", nil,
					errors.New("--config requires a non-empty path")
			}
			cfgPath = args[1]
			args = args[2:]
		case strings.HasPrefix(args[0], "--config="):
			cfgPath = strings.TrimPrefix(args[0], "--config=")
			if strings.TrimSpace(cfgPath) == "" {
				return "", "", nil,
					errors.New("--config requires a non-empty path")
			}
			args = args[1:]
		default:
			sub = args[0]
			return cfgPath, sub, append([]string(nil), args[1:]...), nil
		}
	}
	return "", "", nil, errors.New("missing subcommand after global flags")
}

// runCLI is the production, testable dispatcher. args includes argv[0].
// stderrOpt is optional; when omitted the dispatcher discards stderr.
func runCLI(
	args []string,
	stdin io.Reader,
	stdout io.Writer,
	stderrOpt ...io.Writer,
) int {
	stderr := io.Writer(io.Discard)
	if len(stderrOpt) > 0 && stderrOpt[0] != nil {
		stderr = stderrOpt[0]
	}
	return runCLIWithAuthDeps(
		args, stdin, stdout, stderr, authCLIDeps{})
}

// runCLIWithAuthDeps is the dispatcher tests use to inject deterministic
// Clock/Sleeper for the device flow. Production callers use runCLI, which
// passes a zero authCLIDeps.
func runCLIWithAuthDeps(
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	deps authCLIDeps,
) int {
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if len(args) == 0 {
		return 2
	}
	cfgPath, sub, rest, err := parseManagedInvocation(args[1:])
	if err != nil {
		secrets.NewSafeLogger(stderr, secrets.NewRedactor()).Printf(
			"yanshi: %v", err)
		return 2
	}
	switch sub {
	case "auth":
		return runAuthSub(
			context.Background(), rest, cfgPath,
			stdin, stdout, stderr, deps,
		)
	case "doctor":
		doctorArgs := append([]string{"-config", cfgPath}, rest...)
		return runDoctor(context.Background(), doctorArgs, stdout, stderr)
	default:
		secrets.NewSafeLogger(stderr, secrets.NewRedactor()).Printf(
			"unknown subcommand: %s", sub)
		return 2
	}
}

// runAuthSub dispatches the `yanshi auth` subcommands (status/logout/
// device). It constructs secrets.Manager + auth.Manager + SQLite metadata
// adapter per invocation, registers configured device providers when
// device_auth_enabled is true, then routes by the first remaining arg.
// Errors are surfaced through the SafeLogger (so they are redacted); exit
// codes follow the conventional 0/1/2 (ok/runtime/usage).
func runAuthSub(
	ctx context.Context,
	args []string,
	cfgPath string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	deps authCLIDeps,
) int {
	bootstrapLog := secrets.NewSafeLogger(stderr, secrets.NewRedactor())
	if len(args) == 0 {
		bootstrapLog.Println("usage: yanshi auth <status|logout|device> ...")
		return 2
	}
	cfg, err := loadConfigForAuth(cfgPath)
	if err != nil {
		bootstrapLog.Printf("load config: %v", err)
		return 1
	}
	// Construct secrets.Manager from cfg. PassphraseEnv is the canonical
	// environment reference. The CLI never copies its value into config or
	// output; FileStore reads it through secrets.Manager.
	smgr, err := secrets.NewManager(secrets.Config{
		Backend:       cfg.Secrets.Backend,
		FilePath:      cfg.Secrets.FilePath,
		PassphraseEnv: cfg.Secrets.PassphraseEnv,
		Stderr:        stderr,
	})
	if err != nil {
		bootstrapLog.Printf("secrets: %v", err)
		return 1
	}
	defer smgr.Close()
	safeLog := secrets.NewSafeLogger(stderr, smgr.Redactor())
	amgr := auth.NewManager(smgr)
	authDB, err := store.Open(cfg.Storage.SQLitePath)
	if err != nil {
		safeLog.Println("auth: open metadata store failed")
		return 1
	}
	defer authDB.Close()
	amgr.SetMetadataStore(store.AuthMetadataFromDB(authDB))
	if deps.Clock != nil {
		amgr.SetClock(deps.Clock)
	}
	if deps.Sleeper != nil {
		amgr.SetSleeper(deps.Sleeper)
	}

	// Register device providers from cfg only when device auth is enabled.
	// HTTPS-only validation runs at construction; a bad endpoint fails the
	// CLI with a clear message rather than silently exiting 0.
	if cfg.Auth.Device.DeviceAuthEnabled {
		seen := make(map[string]bool, len(cfg.Auth.Device.Providers))
		for _, dp := range cfg.Auth.Device.Providers {
			if strings.TrimSpace(dp.ID) == "" || seen[dp.ID] {
				safeLog.Println("auth: device provider ids must be non-empty and unique")
				return 1
			}
			seen[dp.ID] = true
			clientID := strings.TrimSpace(dp.ClientID)
			if clientID == "" {
				clientID = strings.TrimSpace(cfg.Auth.Device.ClientID)
			}
			gdp, derr := auth.NewGenericRFC8628Provider(auth.GenericRFC8628Config{
				ClientID:  clientID,
				DeviceURL: dp.DeviceURL,
				TokenURL:  dp.TokenURL,
				Scopes:    append([]string(nil), dp.Scopes...),
				Redactor:  smgr.Redactor(),
			})
			if derr != nil {
				// Constructor errors are fixed strings and contain no endpoint.
				safeLog.Printf("auth: device provider %s: %v", dp.ID, derr)
				return 1
			}
			amgr.RegisterDeviceProvider(dp.ID, gdp)
		}
	}

	sub := args[0]
	rest := args[1:]
	fs := flag.NewFlagSet("auth "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard) // tests require no stderr leak from flag.Parse
	provider := fs.String("provider", "", "provider name (e.g. openai)")
	account := fs.String("account", "main", "account name (default: main)")
	if err := fs.Parse(rest); err != nil {
		safeLog.Printf("auth %s: %v", sub, err)
		return 2
	}

	switch sub {
	case "status":
		st, err := amgr.Status(*provider, *account)
		if err != nil {
			safeLog.Printf("%v", err)
			return 1
		}
		expiresAt := ""
		if !st.ExpiresAt.IsZero() {
			expiresAt = st.ExpiresAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(stdout,
			"Provider: %s\nAccount: %s\nAuthenticated: %v\nSource: %s\nExpiresAt: %s\n",
			st.Provider, st.Account, st.Authenticated, st.Source, expiresAt)
		return 0
	case "logout":
		// Missing credentials are a cleanup failure, not a silent success:
		// propagate ErrSecretNotFound and exit 1 so automation can detect
		// that nothing was deleted. Any other Delete error also exits 1.
		if err := amgr.Logout(*provider, *account); err != nil {
			safeLog.Printf("%v", err)
			return 1
		}
		fmt.Fprintf(stdout, "logged out %s/%s\n", *provider, *account)
		return 0
	case "device":
		if *provider == "" {
			safeLog.Println("auth device: --provider <id> is required")
			return 2
		}
		if _, err := amgr.RunDeviceFlow(ctx, *provider, *account, stdout); err != nil {
			safeLog.Printf("auth device: %v", err)
			return 1
		}
		return 0
	case "mcp-login", "mcp-logout":
		// T10: MCP OAuth lives under `auth` because it is the same kind of
		// thing `auth device` is — a credential a human approves in a browser
		// — and two commands for one concept diverge in their flags, their
		// exit codes and their refusal messages.
		return runMCPAuth(ctx, sub, cfg, fs.Args(), smgr, stdout, safeLog)
	default:
		safeLog.Printf("unknown auth subcommand: %s", sub)
		return 2
	}
}

// loadConfigForAuth is a thin wrapper around config.Load that test code can
// also call directly. It exists so the auth subcommand does not have to
// duplicate the config-loading path of `serve`/`chat`.
func loadConfigForAuth(cfgPath string) (*config.Config, error) {
	return config.Load(cfgPath)
}

// serve starts the yanshi HTTP server and blocks until SIGINT/SIGTERM. It is a
// thin signal-handling wrapper around the testable runServe core; the only thing
// it adds is the signal context and exit-code translation.
func serve(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runServe(ctx, args, os.Stderr)
}

// runServe is the testable core of `yanshi serve`: it parses flags, builds the
// bootstrap App, and runs the HTTP server until ctx is cancelled or the server
// errors. It returns the exit code (0 clean shutdown / 1 bootstrap or serve
// error) so tests can drive it with a cancelled context or a bad config without
// spawning or exiting.
func runServe(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "config.yaml", "path to configuration file")
	fakeModel := fs.Bool("fake-model", false, "use a deterministic fake model (no API keys needed)")
	addr := fs.String("addr", "", "override the config's HTTP listen address")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	app, err := bootstrap.Build(bootstrap.Options{
		ConfigPath: *configPath,
		FakeModel:  *fakeModel,
		// The daemon IS the backend for this project — it owns the database
		// for as long as it runs, and refusing to start leaves the user with
		// no yanshi and no tool to repair the file. See Options.SelfHeal for
		// why the other Build callers leave this false.
		SelfHeal: true,
	})
	if err != nil {
		fmt.Fprintf(stderr, "yanshi serve: %v\n", err)
		return exitErr
	}
	// Reload diffs against the config the daemon actually booted with. A
	// failure to re-read it here leaves the baseline nil, which the control
	// handler treats as "no baseline" and reports every section as changed --
	// noisy, but never a false claim that something was already applied.
	bootedConfig, _ := config.Load(*configPath)

	if *addr != "" {
		app.Server.Addr = *addr
		app.Addr = *addr
	}

	// O3: expose the daemon control and schedule endpoints so a second
	// invocation (`yanshi daemon stop|reload`, `yanshi schedule …`) can reach
	// this process. Wired here, in package main, because App exposes the
	// assembled *http.Server rather than the mux underneath it.
	serveCtx, cancelServe := context.WithCancel(ctx)
	defer cancelServe()
	app.Server.Handler = withOpsEndpoints(app.Server.Handler, opsConfig{
		ConfigPath: *configPath,
		Current:    bootedConfig,
		App:        app,
		Stop:       cancelServe,
	})

	// Claim the project lockfile. `yanshi serve` calls itself a SHARED daemon
	// and its help text promises "other yanshi invocations in the same project
	// discover it" — but discovery reads the lockfile, and until now only
	// cli.Session.bootstrapOwner (the TUI's embedded backend) ever wrote one.
	// Measured: with `serve` running and answering /healthz and /readyz with
	// 200, `yanshi daemon status` reported "no daemon lockfile" and
	// `yanshi schedule list` reported "no running daemon" — so O3 and O6 were
	// structurally unreachable against a real daemon, and a TUI in the same
	// project would silently boot a SECOND backend instead of joining this one.
	//
	// Losing the race is not an error: another owner is already serving this
	// project, which is exactly the state the lockfile exists to produce. We
	// keep serving on our own address (the operator explicitly asked for this
	// process) but say so, and do not remove a lockfile we do not own.
	ownsLock := false
	if root, rerr := os.Getwd(); rerr == nil {
		won, lerr := lockfile.Acquire(root, lockfile.Lockfile{
			PID: os.Getpid(), Addr: app.Addr, Auth: "none", Root: root,
		})
		switch {
		case lerr != nil:
			// Soft-degrade, consistent with the rest of bootstrap: an
			// unwritable cache dir must not stop the server from serving.
			fmt.Fprintf(stderr, "yanshi serve: could not claim the project lockfile: %v "+
				"(daemon/schedule subcommands will not find this process)\n", lerr)
		case !won:
			fmt.Fprintf(stderr, "yanshi serve: another backend already owns %s; "+
				"serving anyway on %s, but daemon/schedule subcommands will address the owner\n",
				root, app.Addr)
		default:
			ownsLock = true
			defer func() {
				if ownsLock {
					_ = lockfile.Remove(root)
				}
			}()
		}
	}

	errCh := make(chan error, 1)
	go func() {
		fmt.Fprintf(stderr, "yanshi serving on %s\n", app.Addr)
		err := app.Start()
		if err != nil && err != http.ErrServerClosed {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-serveCtx.Done():
		fmt.Fprintln(stderr, "\nyanshi shutting down...")
		// A bounded shutdown, not context.Background(). An unbounded one lets a
		// wedged in-flight request hold the process open forever, which is
		// precisely the state `yanshi daemon stop` exists to escape: its own
		// wait would then time out and report failure for a shutdown that had
		// in fact been accepted.
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), daemonStopGrace)
		defer cancelShutdown()
		if err := app.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(stderr, "yanshi shutdown error: %v\n", err)
		}
		return exitOK
	case err := <-errCh:
		if err != nil {
			fmt.Fprintf(stderr, "yanshi serve: %v\n", err)
			return exitErr
		}
		return exitOK
	}
}

// runDefault is the bare `yanshi` invocation: a self-contained TUI that
// discovers a running backend for the current project (lockfile + healthz) or
// embeds one in-process. Reached by `yanshi` with no subcommand, or by a
// flag-only invocation like `yanshi --fake-model`. Returns the exit code; the
// TUI launch itself is interactive (alt-screen bubbletea) and not unit-tested,
// but the flag wiring and error mapping are exercised through dispatch.
func runDefault(args []string) int {
	fs := flag.NewFlagSet("yanshi", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "config.yaml", "path to configuration file")
	fakeModel := fs.Bool("fake-model", false, "use a deterministic fake model")
	server := fs.String("server", "", "force connect to this server URL (skip discovery)")
	inProcess := fs.Bool("inprocess", false, "force in-process backend (skip discovery)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runTUI(ctx, cli.Options{
		ConfigPath: *configPath, FakeModel: *fakeModel,
		Server: *server, InProcess: *inProcess,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "yanshi: %v\n", err)
		return exitErr
	}
	return exitOK
}

// chatTUI runs the TUI for `yanshi chat`. --no-tui drops to the shared headless
// runner (the same path `exec` takes), defaulting to line input so existing
// scripts that pipe one prompt per line keep working. -server/-inprocess force
// a backend mode.
//
// Implementation note: --no-tui must be detected BEFORE the legacy chat flag
// set is parsed, because the headless flag set (--input, --output, --file,
// --timeout, --resume) overlaps with what the outer parser would reject as
// unknown. We scan args for the literal "--no-tui" token and route to the
// shared runner directly when present.
func chatTUI(args []string) int {
	noTUI, filtered := splitNoTUI(args)
	if noTUI {
		return runHeadlessCommand(filtered, "chat", os.Stdin)
	}

	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "config.yaml", "path to configuration file")
	fakeModel := fs.Bool("fake-model", false, "use a deterministic fake model")
	server := fs.String("server", "", "force connect to this server URL")
	token := fs.String("token", "", "bearer token (ignored for loopback)")
	inProcess := fs.Bool("inprocess", false, "force in-process backend")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	_ = token // accepted for backwards compat; loopback does not require it

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runTUI(ctx, cli.Options{
		ConfigPath: *configPath, FakeModel: *fakeModel,
		Server: *server, InProcess: *inProcess,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "yanshi chat: %v\n", err)
		return exitErr
	}
	return exitOK
}

// splitNoTUI detects the --no-tui/-no-tui token in args and returns whether it
// was present plus the args with every such token removed. Extracted from
// chatTUI so the detection/filtering is unit-testable independently of the TUI
// launch.
func splitNoTUI(args []string) (noTUI bool, filtered []string) {
	for _, a := range args {
		if a == "--no-tui" || a == "-no-tui" {
			noTUI = true
		}
	}
	if !noTUI {
		return false, args
	}
	filtered = make([]string, 0, len(args))
	for _, a := range args {
		if a != "--no-tui" && a != "-no-tui" {
			filtered = append(filtered, a)
		}
	}
	return true, filtered
}

// runTUI is the composition root: it resolves a backend (discovery or in-process
// bootstrap via cli.NewSession) and launches the bubbletea TUI. This lives in
// package main because package cli cannot import package tui (the tui package
// depends on cli.StreamEvent), so the cli→tui wiring must happen here.
func runTUI(ctx context.Context, opts cli.Options) error {
	// The interactive TUI is the one entry allowed to heal an unreadable
	// database, and it is set HERE rather than inside cli so that exec and
	// headless — which build the same Session type — keep the safe default.
	// This is the scenario healing exists for: without it a corrupt yanshi.db
	// means the TUI never starts and the user has no other tool to repair it.
	opts.SelfHeal = true
	sess := cli.NewSession(opts)
	if err := sess.Resolve(ctx); err != nil {
		return err
	}
	defer sess.Close()

	// C15 + I18N1: the project preference layer. config.Load failing is not
	// fatal here — the TUI is the thing an operator would use to diagnose a
	// broken config, so it starts on built-in defaults and the doctor
	// subcommand reports the parse error.
	var project tui.Preferences
	if cfg, err := config.Load(opts.ConfigPath); err == nil {
		project = tui.Preferences{
			UILocale:     cfg.I18N.UILocale,
			ThemeName:    cfg.TUI.Theme,
			KeymapName:   cfg.TUI.KeymapName,
			HighContrast: cfg.TUI.HighContrast,
			Vim:          cfg.TUI.Vim,
			Frecency:     cfg.TUI.Frecency,
		}
		tui.SetProjectBindings(cfg.TUI.Bindings)
	}

	prog := tui.NewProgram(sess, sess.Root(), project)

	// Wire the signal ctx (SIGINT/SIGTERM) to prog.Quit so that SIGTERM
	// triggers a graceful exit — bubbletea only handles Ctrl-C internally.
	// Without this, SIGTERM cancels ctx while prog.Run() blocks, so
	// defer sess.Close() never runs (in-process server/store/lockfile leak).
	go func() {
		<-ctx.Done()
		prog.Quit()
	}()

	_, err := prog.Run()
	return err
}

// chatLegacy/sendChatLegacy removed: `chat --no-tui` and `exec` now share the
// headless runner (cmd/yanshi/headless.go). The legacy SSE-only line REPL is
// superseded; cli.RunHeadless preserves the one-prompt-per-line default through
// HeadlessInputLines, and the shared runner adds JSONL output, resume, timeout,
// and stable exit codes the legacy path never had.

// ---------------------------------------------------------------------------
// goal subcommand
// ---------------------------------------------------------------------------

// counterEvaluator fails until its call count reaches passAt, then passes.
// This drives the fake-model demo loop: fail once (iteration 1) then pass
// (iteration 2), so the loop exercises two full cycles and terminates.
type counterEvaluator struct {
	passAt int
	mu     sync.Mutex
	calls  int
}

func (e *counterEvaluator) Evaluate(_ context.Context, _ goalloop.Goal, _ goalloop.Plan, _ string) (goalloop.EvalVerdict, error) {
	e.mu.Lock()
	e.calls++
	n := e.calls
	e.mu.Unlock()

	if n >= e.passAt {
		return goalloop.EvalVerdict{
			Evaluator: "counter",
			Pass:      true,
			Evidence:  fmt.Sprintf("call %d >= passAt %d", n, e.passAt),
		}, nil
	}
	return goalloop.EvalVerdict{
		Evaluator: "counter",
		Pass:      false,
		Evidence:  fmt.Sprintf("call %d < passAt %d", n, e.passAt),
		Gaps:      []string{fmt.Sprintf("counter: call %d < passAt %d", n, e.passAt)},
	}, nil
}

// runGoal runs the self-driven goal loop: plan -> implement -> evaluate -> judge.
// It is the testable core of the `goal` subcommand and returns the exit code
// (0 complete / 1 runtime error / 2 usage error) instead of calling os.Exit, so
// the self-contained --fake-model path (FakePlanner + FakeImplementer +
// counterEvaluator, no API keys or CLIs) is unit-testable end-to-end.
func runGoal(args []string) int {
	fs := flag.NewFlagSet("goal", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "config.yaml", "path to configuration file")
	fakeModel := fs.Bool("fake-model", false, "use fake planner/implementer/evaluator (no API keys or CLIs needed)")
	workdir := fs.String("workdir", ".", "working directory for implementation")
	agent := fs.String("agent", "claudecode", "external agent for implementation (real path)")
	maxIters := fs.Int("max-iters", 5, "maximum goal loop iterations")
	maxTokens := fs.Int("max-tokens", 0, "token budget for the whole goal run (0 = unlimited); when resuming, a value typed here replaces the stored one")
	goalText := fs.String("goal", "", "goal text (alternatively, pass as positional arg)")
	tierFlag := fs.String("tier", "auto", `difficulty tier: "auto" (model classifies, keyword table as fallback) or t0..t4 (quick-fix, standard, designed, team, autonomous)`)
	history := fs.Int("history", 0, "print the last N goal run records and exit (0 = run a goal)")
	reset := fs.Bool("reset", false, "discard the saved resume point for -workdir and exit, so the next run starts over with a full budget")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	if *history > 0 {
		return printGoalHistory(*configPath, *history, os.Stdout)
	}
	// Ahead of the goal-text check: clearing a resume point is about the
	// working directory, not about any particular goal, so requiring the
	// operator to retype the goal text they are trying to abandon would be
	// backwards.
	if *reset {
		return resetGoalRun(*configPath, *workdir, os.Stdout)
	}

	// Goal text: -goal flag takes priority, then first positional arg.
	text := *goalText
	if text == "" {
		rest := fs.Args()
		if len(rest) > 0 {
			text = strings.Join(rest, " ")
		}
	}
	if text == "" {
		fmt.Fprintln(os.Stderr, "yanshi goal: goal text is required (use -goal or a positional argument)")
		return exitUsage
	}

	// Resolve the development tier. "auto" uses the RuleTierer; t0..t4 forces a
	// specific tier. G03: every tier now enters a real execution path — the
	// silent "lightweight path" print-and-return for forced T0-T2 is removed.
	resolvedTier, forced, err := resolveGoalTier(*tierFlag, text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yanshi goal: %v\n", err)
		return exitUsage
	}

	// Budget sources: the goal: config block, overridden by any flag actually
	// typed. Config is read leniently — the fake path is designed to run with
	// no config at all, so an unreadable file means "no goal block", not a
	// startup failure.
	var goalCfg config.GoalConfig
	if c, err := config.Load(*configPath); err == nil {
		goalCfg = c.Goal
	}
	budget := resolveGoalBudget(fs, *maxTokens, *maxIters, goalCfg)

	wd, err := absWorkdir(*workdir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yanshi goal: bad workdir: %v\n", err)
		return exitErr
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		loop *goalloop.Loop
		// Allocated before the branch so BOTH paths accumulate usage. Leaving
		// it nil on the fake path made the demo silently unmeterable: Loop.spent
		// falls back to its internal counter, so a budget could not be observed
		// to work there at all.
		loopSink  = &goalloop.UsageSink{}
		loopStore *store.Store
	)

	if *fakeModel {
		announceTier(resolvedTier)
		// Self-contained demo: FakePlanner + FakeImplementer(fail once) +
		// CounterEvaluator(fail once then pass) + AggregateJudge.
		// This runs two iterations without any external dependencies.
		planner := goalloop.FakePlanner{
			Tests: []goalloop.AcceptanceTest{{Name: "smoke", Command: "go version"}},
			Steps: []string{"step1"},
		}
		impl := &goalloop.FakeImplementer{Result: "done", FailFirst: 1}
		eval := &counterEvaluator{passAt: 2}

		loop = goalloop.New(goalloop.Config{
			Planner:     planner,
			Implementer: impl,
			Evaluators:  []goalloop.Evaluator{eval},
			Judge:       goalloop.AggregateJudge{},
			Budget:      budget,
			Sink:        loopSink,
			// Tier is load-bearing even on the demo path: EscalationHint reads it
			// to name the next tier up, so leaving it zero made `-tier t3` end by
			// advising a DOWNGRADE to t1 (TierQuickFix+1) — measured, not feared.
			Tier: resolvedTier,
		})
	} else {
		// Real path: build the app to get the LLM model + orchestrator + store,
		// then dispatch by tier. G03: T0-T2 (lightweight) run a single
		// orchestrator+skill turn that edits files via the bound fs/shell
		// tools; T3-T4 (loop) run the full plan-implement-evaluate-judge cycle.
		app, err := bootstrap.Build(bootstrap.Options{
			ConfigPath: *configPath,
			FakeModel:  false,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "yanshi goal: bootstrap: %v\n", err)
			return exitErr
		}
		defer app.Shutdown(ctx)
		loopStore = app.Store

		// Shared token sink (G02): every LLM-calling component adds to it; the
		// loop drives its budget from it and the persisted record carries it.

		// -tier auto asks the MODEL, not just the keyword table. The rule pass
		// above already ran (it is what validates the flag and what the fake
		// path uses), and it stays on as LLMTierer's fallback, so a model error
		// or an unparseable reply lands exactly where the old behaviour was.
		//
		// This has to happen here rather than next to resolveGoalTier: the
		// classifier needs app.Model, which does not exist until Build, and
		// Build only runs on this branch. It also has to happen BEFORE the
		// Path() dispatch below — the tier is what chooses between the
		// lightweight turn and the full loop, so refining it afterwards would
		// change the label without changing the execution path.
		if !forced {
			resolvedTier = refineTierWithModel(ctx, app.Model, text, resolvedTier, loopSink)
		}
		announceTier(resolvedTier)

		if resolvedTier.Path() == "lightweight" {
			// --- T0-T2: one orchestrator turn with the tier's skill body ---
			decision := runLightweightGoal(ctx, app, resolvedTier, text, loopSink)
			persistGoalRun(loopStore, resolvedTier, decision, loopSink.Snapshot(), 1)
			fmt.Printf("decision: complete=%v, summary=%s\n", decision.Complete, decision.Summary)
			if decision.Complete {
				return exitOK
			}
			return exitErr
		}

		// --- T3-T4: full goal loop ---
		chatModel := app.Model
		planner := goalloop.LLMPlanner{Model: chatModel, Sink: loopSink}
		impl := &goalloop.ACPImplementer{Agent: *agent, Sink: loopSink}
		// M8: when the VCS + repo are available, wire the autoVCS worker path
		// (worktree branch + merge) and deliver the vcs MCP server to spawned
		// ACP agents. Gated on VCSRepoID so a failed InitRepo leaves the worker
		// on the git-worktree fallback.
		if app.VCSRepoID != "" {
			impl = impl.WithVCS(app.VCS, app.VCSRepoID, app.VCSDBPath, app.WorktreeDir)
		}
		evals := goalloop.EvaluatorsForTier(resolvedTier, chatModel, loopSink)

		loop = goalloop.New(goalloop.Config{
			Planner:     planner,
			Implementer: impl,
			Evaluators:  evals,
			Judge:       goalloop.AggregateJudge{},
			Budget:      budget,
			Sink:        loopSink,
			Tier:        resolvedTier,
			// W-D-16: the store doubles as the goal loop's resume point, so a
			// crashed or Ctrl-C'd run restarts at the next iteration with the
			// tokens it already spent still spent. Only the real path gets one
			// — the fake path is a self-contained demo whose whole point is to
			// run identically every time.
			State: loopStore,
			// Which limits the operator typed, so a resumed run can tell a
			// deliberate new budget from a config default reasserting itself.
			BudgetExplicit: explicitBudgetFlags(fs),
		})
	}

	g := goalloop.Goal{Text: text, Workdir: wd}

	decision, err := loop.Run(ctx, g, func(e goalloop.Event) {
		fmt.Printf("[iter %d] %s: %s\n", e.Iteration, e.Phase, e.Detail)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "yanshi goal: %v\n", err)
		return exitErr
	}

	// Persist the run record on the real loop path (T3-T4). The fake path leaves
	// loopStore nil so this is a no-op; the lightweight path already persisted
	// and returned inside its own branch.
	if loopStore != nil {
		persistGoalRun(loopStore, resolvedTier, decision, loopSink.Snapshot(), loop.Iterations())
	}

	fmt.Printf("decision: complete=%v, summary=%s\n", decision.Complete, decision.Summary)
	if decision.Complete {
		return exitOK
	}
	return exitErr
}

// absWorkdir converts a relative path to absolute.
//
// The result is cleaned because it is not only a path any more: the goal loop
// keys its resume point on it. Without this, `-workdir /repo` and
// `-workdir /repo/` are two different keys, so a trailing slash typed on the
// second run makes the first run's progress and spent budget silently vanish —
// a fresh start with a fresh budget and no message saying why.
func absWorkdir(p string) (string, error) {
	if p == "" || p == "." {
		return os.Getwd()
	}
	if strings.Contains(p, ":") || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return filepath.Clean(p), nil // already absolute (Windows drive letter or Unix root)
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Clean(wd + string(os.PathSeparator) + p), nil
}

// resolveGoalTier maps the -tier flag to a Tier via the shared goalloop mapping.
// "auto" runs the RuleTierer over text (forced=false); "t0".."t4" force a tier
// (forced=true). An unrecognized value returns an error so the caller can map it
// to a usage exit. The mapping logic lives in goalloop.ResolveTierFlag so it is
// unit-testable (package main is not).
func resolveGoalTier(flagValue, text string) (goalloop.Tier, bool, error) {
	tier, forced, err := goalloop.ResolveTierFlag(flagValue, text)
	if err != nil {
		return goalloop.TierStandard, false, err
	}
	return tier, forced, nil
}

// announceTier prints the resolved tier and the execution path it selects.
// Both goal paths call it once the tier is final — the real path only knows
// that after refineTierWithModel, and printing earlier would name a tier the
// run may not use.
func announceTier(t goalloop.Tier) {
	fmt.Printf("[tier: %s] path: %s\n", t, t.Path())
}

// refineTierWithModel upgrades an "auto" tier decision from keyword matching to
// model classification, returning ruleTier unchanged when no model is wired.
//
// The keyword table (goalloop.RuleTierer) reads the goal text for words like
// "refactor" or "typo". That is a reasonable prior and a poor classifier: it
// cannot tell "fix the typo in the migration that corrupts every row" from
// "fix the typo in the README", and the two belong at opposite ends of the
// range. Since the tier picks the execution path, evaluator set and skill
// body, a misread here is not cosmetic.
//
// sink is threaded through so this classification call counts against the same
// token budget as the rest of the run — an unmetered call would make the budget
// under-report by exactly the amount nobody was watching.
func refineTierWithModel(
	ctx context.Context, m model.BaseChatModel, text string,
	ruleTier goalloop.Tier, sink *goalloop.UsageSink,
) goalloop.Tier {
	if m == nil {
		return ruleTier
	}
	t, err := goalloop.LLMTierer{
		Model:    m,
		Fallback: goalloop.RuleTierer{},
		Sink:     sink,
	}.Tier(ctx, text)
	if err != nil {
		return ruleTier
	}
	return t
}

// runLightweightGoal executes the T0-T2 lightweight path: one orchestrator
// turn that follows the tier's SKILL.md (loaded from the app's skill registry)
// and edits files via the orchestrator's bound tools. It returns a Decision
// carrying the assistant summary and, when the tier is below T4, an escalation
// hint so an undersized tier surfaces a next step instead of exiting silently
// (G03).
func runLightweightGoal(ctx context.Context, app *bootstrap.App, tier goalloop.Tier, text string, sink *goalloop.UsageSink) goalloop.Decision {
	prompt := text
	if skill, ok := app.Skills.Get(tier.SkillName()); ok {
		if body, err := app.Skills.Body(skill); err == nil && body != "" {
			prompt = body + "\n\n---\n\nGoal: " + text
		}
	}
	result, u, err := app.Orch.QueryWithUsage(ctx, prompt)
	// Report BEFORE the error branch: the tokens were spent either way, and a
	// budget that only counts successful turns is a budget a failing loop runs
	// straight past. This path used to call Query, which discards usage, so
	// every T0-T2 run record said zero tokens regardless of what it cost.
	if sink != nil {
		sink.Add(goalloop.Usage{
			PromptTokens:     u.PromptTokens,
			CompletionTokens: u.CompletionTokens,
			TotalTokens:      u.TotalTokens,
		})
	}
	if err != nil {
		return goalloop.Decision{
			Complete: false,
			Summary:  "lightweight turn error: " + err.Error(),
		}
	}
	summary := result
	if hint := goalloop.EscalationHint(tier); hint != "" {
		// Non-silent: even a finished low-tier turn advertises the upgrade path.
		summary = result + " (" + hint + ")"
	}
	return goalloop.Decision{Complete: true, Summary: summary}
}

// printGoalHistory prints the most recent goal run records, newest first.
//
// persistGoalRun has written a RunRecord to kv under "goalrun:<unix>" since
// G02, and until this existed nothing read them back — a record written for an
// operator to inspect, with no way for an operator to inspect it. StopReason
// is the column that matters: "the run stopped" and "the run stopped because
// it ran out of tokens" are different facts, and only the second tells you
// whether to raise the budget.
// resetGoalRun discards the goal loop's saved resume point for workdir, so the
// next run of that goal starts from iteration 1 with its budget whole again.
//
// It opens the store directly rather than going through bootstrap.Build, the
// same way printGoalHistory does: this touches one kv row and has no reason to
// need a model provider or an agent CLI to be configured.
func resetGoalRun(configPath, workdir string, out io.Writer) int {
	wd, err := absWorkdir(workdir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yanshi goal -reset: bad workdir: %v\n", err)
		return exitErr
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yanshi goal -reset: config: %v\n", err)
		return exitErr
	}
	st, err := store.Open(cfg.Storage.SQLitePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yanshi goal -reset: open store: %v\n", err)
		return exitErr
	}
	defer st.Close()

	if err := goalloop.ResetGoalState(st, wd); err != nil {
		fmt.Fprintf(os.Stderr, "yanshi goal -reset: %v\n", err)
		return exitErr
	}
	fmt.Fprintf(out, "goal state cleared for %s\n", wd)
	return exitOK
}

func printGoalHistory(configPath string, limit int, out io.Writer) int {
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yanshi goal -history: config: %v\n", err)
		return exitErr
	}
	st, err := store.Open(cfg.Storage.SQLitePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yanshi goal -history: open store: %v\n", err)
		return exitErr
	}
	defer st.Close()

	rows, err := st.DB.Query(
		`SELECT value FROM kv WHERE key LIKE 'goalrun:%' ORDER BY key DESC LIMIT ?`, limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yanshi goal -history: query: %v\n", err)
		return exitErr
	}
	defer func() { _ = rows.Close() }()

	var n int
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			fmt.Fprintf(os.Stderr, "yanshi goal -history: scan: %v\n", err)
			return exitErr
		}
		var rec goalloop.RunRecord
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			// A record written by an older schema is worth reporting, not
			// worth aborting the listing for.
			fmt.Fprintf(out, "(unreadable record: %v)\n", err)
			continue
		}
		reason := rec.StopReason
		if reason == "" {
			reason = "-"
		}
		fmt.Fprintf(out, "tier=%s complete=%v stop_reason=%s iterations=%d tokens=%d\n",
			rec.Tier, rec.Complete, reason, rec.Iterations, rec.Usage.Total())
		n++
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "yanshi goal -history: %v\n", err)
		return exitErr
	}
	if n == 0 {
		fmt.Fprintln(out, "no goal runs recorded yet")
	}
	return exitOK
}

// persistGoalRun writes a RunRecord for the finished goal run into the store's
// kv table (G02: persist why the run ended). Failures are best-effort: a
// persistence error is logged to stderr but never fails the goal command.
func persistGoalRun(st *store.Store, tier goalloop.Tier, decision goalloop.Decision, usage goalloop.Usage, iterations int) {
	if st == nil {
		return
	}
	rec := goalloop.NewRunRecord(tier, decision, usage, iterations)
	data, err := json.Marshal(rec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yanshi goal: encode run record: %v\n", err)
		return
	}
	key := fmt.Sprintf("goalrun:%d", time.Now().Unix())
	if err := st.KVSet(key, string(data)); err != nil {
		fmt.Fprintf(os.Stderr, "yanshi goal: persist run record: %v\n", err)
	}
}

// ---------------------------------------------------------------------------
// vcs-mcp subcommand
// ---------------------------------------------------------------------------

// vcsMcp runs the autoVCS MCP server on stdio. It is the spawnable entry point
// the ACP adapter launches as a stdio subprocess (configured via env vars in the
// session/new mcpServers entry) so an external agent can drive vcs_* tools
// through MCP.
//
// Configuration is entirely from env vars (the ACP adapter spawns this binary
// with the right env):
//
//	YANSHI_DB_PATH       SQLite path (default "yanshi.db")
//	YANSHI_REPO_ID       repo id (the vcs_repos.id; required for real use)
//	YANSHI_WT_ID         worktree id (the vcs_worktrees.id; required for real use)
//	YANSHI_AGENT         author attribution stamped on commits/merges (default "acp")
//	YANSHI_WORKTREE_DIR  worktree working-dir root (default "~/.yanshi/worktrees")
//
// SQLite contention note: this opens the SAME sqlite file the main yanshi
// server has open. modernc.org/sqlite serializes writes within a process;
// cross-process access relies on SQLite's file locking. For v1 this is
// acceptable: the MCP server is read-mostly (log/diff) plus occasional
// vcs_commit/vcs_merge writes, all serialized by SQLite. WAL is intentionally
// NOT enabled here (out of scope; enabling it would need coordination with the
// main server's connection).
func vcsMcp(_ []string) int {
	dbPath := envDefault("YANSHI_DB_PATH", "yanshi.db")
	repoID := os.Getenv("YANSHI_REPO_ID")
	wtID := os.Getenv("YANSHI_WT_ID")
	agent := envDefault("YANSHI_AGENT", "acp")
	worktreeDir := expandHome(envDefault("YANSHI_WORKTREE_DIR", "~/.yanshi/worktrees"))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runVCSMCP(ctx, dbPath, repoID, wtID, agent, worktreeDir, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "yanshi vcs-mcp: %v\n", err)
		return exitErr
	}
	return exitOK
}

// runVCSMCP is the testable core of the vcs-mcp subcommand: it opens the store
// at dbPath, builds the VCS, and runs the MCP server's Serve loop on r/w. It is
// split from vcsMcp so tests can drive it with a temp db and in-memory pipes
// without spawning a subprocess.
func runVCSMCP(ctx context.Context, dbPath, repoID, wtID, agent, worktreeDir string, r io.Reader, w io.Writer) error {
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	v := vcs.New(st, worktreeDir)
	// The MCP server is a short-lived subprocess spawned once per ACP agent, so
	// it is exactly the shape that used to leave a lock file behind on every
	// invocation. Close reclaims the ones nobody else holds.
	defer func() { _ = v.Close() }()
	srv := vcsmcp.New(v, repoID, wtID, agent)
	return srv.Serve(ctx, r, w)
}

// envDefault returns the named env var, or dflt when it is unset/empty.
func envDefault(key, dflt string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return dflt
}

// expandHome resolves a leading "~" to the user's home directory. Empty input
// returns empty. This mirrors bootstrap.expandHome, which is unexported and so
// cannot be reused from package main.
func expandHome(p string) string {
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

// ---------------------------------------------------------------------------
// doctor subcommand
// ---------------------------------------------------------------------------

// doctor is dispatched through the managed-invocation path (isManagedInvocation
// → runCLI → runDoctor), so it has no standalone entry point here; runDoctor is
// the testable core and is exercised directly by tests.

// runDoctor is the testable core of the doctor subcommand: it parses flags,
// runs the checks via cli.RunDoctor, renders to out (text by default, indented
// JSON with -json), and returns the exit code (0 ok / 1 warn / 2 fail). errOut
// receives diagnostics (e.g. a JSON encode failure or flag usage). Split from
// doctor so tests can drive it with buffer pipes without spawning or exiting.
func runDoctor(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(errOut)
	configPath := fs.String("config", "config.yaml", "path to configuration file")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON instead of human-readable text")
	release := fs.Bool("release", false, "promote release-blocking warns to fails (release runbook; see docs/upgrade-guide.md)")
	fix := fs.Bool("fix", false, "repair an allowlisted set of problems after reporting (see -fix-only for the list)")
	fixOnly := fs.String("fix-only", "", "comma-separated subset of repairs to run (default: all allowlisted)")
	fixDryRun := fs.Bool("fix-dry-run", false, "with -fix, report what would be repaired without touching anything")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	report := cli.RunDoctor(ctx, cli.DoctorOptions{ConfigPath: *configPath, Release: *release})
	if *asJSON {
		if err := report.RenderJSON(out); err != nil {
			fmt.Fprintf(errOut, "yanshi doctor: render json: %v\n", err)
			return 2
		}
	} else {
		report.RenderText(out)
	}
	if !*fix {
		return report.ExitCode()
	}
	return runDoctorFix(ctx, *configPath, *fixOnly, *fixDryRun, *asJSON, report, out, errOut)
}

// runDoctorFix performs the repair half of `yanshi doctor -fix` and folds its
// exit code into the check report's.
//
// Interactivity is probed from os.Stdin rather than taken as a flag, because
// the gate is about who is WATCHING, not about what the caller claims: a CI
// job that passed -interactive would defeat the whole point, while a human at
// a terminal never has to pass anything.
//
// The two exit codes are combined by taking the worse of them. A repair that
// failed must not be masked by checks that happened to pass, and checks that
// failed must not be masked by repairs that succeeded — the second is the
// likelier direction, since a -fix run that repaired the directories but left
// a broken provider config would otherwise exit 0.
func runDoctorFix(
	ctx context.Context,
	configPath, fixOnly string,
	dryRun, asJSON bool,
	report cli.DoctorReport,
	out, errOut io.Writer,
) int {
	var only []cli.FixAction
	for _, name := range strings.Split(fixOnly, ",") {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			only = append(only, cli.FixAction(trimmed))
		}
	}
	fixReport, err := cli.RunDoctorFix(ctx, cli.FixOptions{
		ConfigPath:  configPath,
		Interactive: cli.StdinIsTerminal(os.Stdin),
		DryRun:      dryRun,
		Only:        only,
	})
	if err != nil {
		fmt.Fprintf(errOut, "yanshi doctor -fix: %v\n", err)
		return 2
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if encErr := enc.Encode(fixReport); encErr != nil {
			fmt.Fprintf(errOut, "yanshi doctor -fix: render json: %v\n", encErr)
			return 2
		}
	} else {
		fmt.Fprintln(out)
		fixReport.RenderText(out)
	}
	if code := fixReport.ExitCode(); code > report.ExitCode() {
		return code
	}
	return report.ExitCode()
}

// ---------------------------------------------------------------------------
// exec / chat --no-tui subcommand (V12): the shared headless runner lives in
// cmd/yanshi/headless.go. The exit codes + mapExecError helper remain here so
// headless.go and any future caller share one mapping.
// ---------------------------------------------------------------------------

// Exit codes for the headless subcommands. They are stable so scripts can
// branch on them. 0 success, 1 runtime error, 2 usage error, 124 timeout (the
// conventional coreutils timeout code), 130 cancelled (128 + SIGINT(2)).
const (
	exitOK      = 0
	exitErr     = 1
	exitUsage   = 2
	exitTimeout = 124
	exitCancel  = 130
)

// mapExecError maps a cli.RunHeadless error to a stable exit code. nil -> 0;
// ctx DeadlineExceeded -> 124 (timeout); ctx Canceled -> 130 (interrupt);
// anything else -> 1 (runtime error). Split out so it is unit-testable.
func mapExecError(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, context.DeadlineExceeded):
		return exitTimeout
	case errors.Is(err, context.Canceled):
		return exitCancel
	default:
		return exitErr
	}
}
