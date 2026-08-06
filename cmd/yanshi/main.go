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

	"github.com/x6nux/yanshi/internal/agent/goalloop"
	"github.com/x6nux/yanshi/internal/auth"
	"github.com/x6nux/yanshi/internal/bootstrap"
	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/cli/tui"
	"github.com/x6nux/yanshi/internal/config"
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
  yanshi doctor [-config FILE] [-json] [-release]

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
  doctor   One-time self-check of config, database, providers, ACP CLIs,
           lockfile, port, directories, and sandbox status. Prints ok/warn/fail
           per check; -json emits machine-readable output. Exit 0 all ok /
           1 warn / 2 fail. Never prints secrets.
`

func main() {
	os.Exit(dispatch(os.Args, os.Stdin, os.Stdout, os.Stderr))
}

// dispatch is the testable top-level router: it mirrors the original main()
// switch but returns an exit code instead of calling os.Exit, so the routing
// logic (version flag, managed-invocation gating, subcommand dispatch, unknown
// subcommand usage error) is unit-testable. argv includes argv[0].
func dispatch(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
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

// runAuthSub dispatches the `yanshi auth` subcommands (set/status/logout/
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
		bootstrapLog.Println("usage: yanshi auth <set|status|logout|device> ...")
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
	apiKeyStdin := fs.Bool("api-key-stdin", false, "read API key from stdin (no prompt)")
	if err := fs.Parse(rest); err != nil {
		safeLog.Printf("auth %s: %v", sub, err)
		return 2
	}

	switch sub {
	case "set":
		cmd := secrets.AuthCommand{
			Provider:      *provider,
			Account:       *account,
			Backend:       cfg.Secrets.Backend,
			FilePath:      cfg.Secrets.FilePath,
			PassphraseEnv: cfg.Secrets.PassphraseEnv,
			APIKeyStdin:   *apiKeyStdin,
			Manager:       smgr,
			Stdin:         stdin,
			Stdout:        stdout,
		}
		if err := cmd.Run(); err != nil {
			safeLog.Printf("%v", err)
			return 1
		}
		return 0
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
	})
	if err != nil {
		fmt.Fprintf(stderr, "yanshi serve: %v\n", err)
		return exitErr
	}

	if *addr != "" {
		app.Server.Addr = *addr
		app.Addr = *addr
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
	case <-ctx.Done():
		fmt.Fprintln(stderr, "\nyanshi shutting down...")
		if err := app.Shutdown(context.Background()); err != nil {
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
	maxTokens := fs.Int("max-tokens", 0, "token budget for the whole goal run (0 = unlimited)")
	goalText := fs.String("goal", "", "goal text (alternatively, pass as positional arg)")
	tierFlag := fs.String("tier", "auto", `difficulty tier: "auto" (RuleTierer) or t0..t4 (quick-fix, standard, designed, team, autonomous)`)
	if err := fs.Parse(args); err != nil {
		return exitUsage
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
	_ = forced // kept for the (future) "was the tier explicit?" distinction
	fmt.Printf("[tier: %s] path: %s\n", resolvedTier, resolvedTier.Path())

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

		if resolvedTier.Path() == "lightweight" {
			// --- T0-T2: one orchestrator turn with the tier's skill body ---
			decision := runLightweightGoal(ctx, app, resolvedTier, text)
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
func absWorkdir(p string) (string, error) {
	if p == "" || p == "." {
		return os.Getwd()
	}
	if strings.Contains(p, ":") || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return p, nil // already absolute (Windows drive letter or Unix root)
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return wd + string(os.PathSeparator) + p, nil
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

// runLightweightGoal executes the T0-T2 lightweight path: one orchestrator
// turn that follows the tier's SKILL.md (loaded from the app's skill registry)
// and edits files via the orchestrator's bound tools. It returns a Decision
// carrying the assistant summary and, when the tier is below T4, an escalation
// hint so an undersized tier surfaces a next step instead of exiting silently
// (G03).
func runLightweightGoal(ctx context.Context, app *bootstrap.App, tier goalloop.Tier, text string) goalloop.Decision {
	prompt := text
	if skill, ok := app.Skills.Get(tier.SkillName()); ok {
		if body, err := app.Skills.Body(skill); err == nil && body != "" {
			prompt = body + "\n\n---\n\nGoal: " + text
		}
	}
	result, err := app.Orch.Query(ctx, prompt)
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
