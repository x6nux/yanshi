// Operations subcommands: `yanshi init`, `yanshi daemon` and `yanshi schedule`.
//
// They live in their own file rather than in main.go for the ordinary reason
// (GOV2's 1000-pure-line cap on main.go) and for a second one: these three are
// the only subcommands that talk to an ALREADY-RUNNING yanshi rather than
// starting one, so keeping their flag parsing and exit-code mapping together
// makes that shared shape visible.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/x6nux/yanshi/internal/bootstrap"
	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/config"
)

// runInit implements `yanshi init`: generate a config.yaml from the shipped
// config.example.yaml.
func runInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "config.yaml", "path of the config file to create")
	examplePath := fs.String("template", "", "template to copy (default: config.example.yaml)")
	force := fs.Bool("force", false, "overwrite an existing config (the original is backed up first)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	result, err := cli.RunInit(cli.InitOptions{
		ConfigPath: *configPath, ExamplePath: *examplePath, Force: *force,
	})
	if err != nil {
		fmt.Fprintf(stderr, "yanshi init: %v\n", err)
		if errors.Is(err, cli.ErrConfigExists) {
			// A refusal to clobber is a usage error, not a runtime failure:
			// the operator has to decide, and the exit code should say so.
			return exitUsage
		}
		return exitErr
	}
	cli.RenderInitResult(stdout, result)
	return exitOK
}

// runDaemon implements `yanshi daemon <status|stop|reload>`.
//
// All three reach the running backend through the project lockfile, so they
// work from any directory inside the project and need no address argument.
func runDaemon(args []string, stdout, stderr io.Writer) int {
	if isHelpArg(args) {
		fmt.Fprint(stdout, daemonUsage)
		return exitOK
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: yanshi daemon <status|stop|reload> [flags]")
		return exitUsage
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("daemon "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "project root (default: working directory)")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON instead of human-readable text")
	configPath := fs.String("config", "", "config file to re-read (reload only; default: the daemon's own)")
	timeout := fs.Duration("timeout", cli.DaemonStopTimeout, "how long to wait for the daemon to exit (stop only)")
	if err := fs.Parse(rest); err != nil {
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch sub {
	case "status":
		return daemonStatus(ctx, *root, *asJSON, stdout)
	case "stop":
		if err := cli.RunDaemonStop(ctx, *root, *timeout); err != nil {
			fmt.Fprintf(stderr, "yanshi daemon stop: %v\n", err)
			return exitErr
		}
		fmt.Fprintln(stdout, "daemon stopped")
		return exitOK
	case "reload":
		resp, err := cli.RunDaemonReload(ctx, *root, *configPath)
		if err != nil {
			fmt.Fprintf(stderr, "yanshi daemon reload: %v\n", err)
			return exitErr
		}
		if *asJSON {
			return emitJSON(stdout, stderr, resp)
		}
		cli.RenderControlResponse(stdout, resp)
		// A reload that refused something exits 1, not 0. The operator asked
		// for a config to take effect and part of it did not; an exit 0 here
		// is what lets a provisioning script believe the new listen address is
		// live when it is not.
		if len(resp.Rejected) > 0 {
			return exitErr
		}
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown daemon subcommand %q (want status, stop or reload)\n", sub)
		return exitUsage
	}
}

// daemonStatus renders the daemon state and maps it to an exit code.
//
// The exit code is the scriptable half: 0 ready, 1 anything else. A monitoring
// check that has to grep the text output for the word "ready" is a check that
// breaks the first time the wording improves.
func daemonStatus(ctx context.Context, root string, asJSON bool, stdout io.Writer) int {
	status := cli.RunDaemonStatus(ctx, root)
	if asJSON {
		if err := json.NewEncoder(stdout).Encode(status); err != nil {
			return exitErr
		}
	} else {
		cli.RenderDaemonStatus(stdout, status)
	}
	if status.Ready {
		return exitOK
	}
	return exitErr
}

// runSchedule implements `yanshi schedule <list|show|pause|resume|run-now|delete>`.
//
// Creation is deliberately absent: automations are created by the model
// through the approval-guarded automation_create tool, where the prompt is
// authored in context. This command is the OPERATIONS surface -- see what
// exists, stop it, start it, trigger it, remove it -- which is exactly the set
// that was previously unreachable from outside a chat session.
func runSchedule(args []string, stdout, stderr io.Writer) int {
	if isHelpArg(args) {
		fmt.Fprint(stdout, scheduleUsage)
		return exitOK
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: yanshi schedule <list|show|pause|resume|run-now|delete> [ID] [flags]")
		return exitUsage
	}
	op := cli.ScheduleOp(args[0])
	rest := args[1:]

	// The id is positional so `yanshi schedule pause auto-123` reads the way
	// an operator types it. Flags may follow it.
	id := ""
	if len(rest) > 0 && len(rest[0]) > 0 && rest[0][0] != '-' {
		id, rest = rest[0], rest[1:]
	}

	fs := flag.NewFlagSet("schedule "+string(op), flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "project root (default: working directory)")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON instead of a table")
	if err := fs.Parse(rest); err != nil {
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	resp, err := cli.RunSchedule(ctx, *root, cli.ScheduleRequest{Op: op, ID: id})
	if err != nil {
		fmt.Fprintf(stderr, "yanshi schedule: %v\n", err)
		if errors.Is(err, cli.ErrUnknownScheduleOp) {
			return exitUsage
		}
		return exitErr
	}
	if *asJSON {
		return emitJSON(stdout, stderr, resp)
	}
	cli.RenderScheduleResponse(stdout, resp)
	return exitOK
}

// isHelpArg reports whether argv asks for this subcommand's own usage text.
//
// The three ops subcommands route on a positional verb, so a bare `-h` would
// otherwise be parsed as an unknown verb and answered with an error on stderr
// plus a usage exit code. That is the wrong answer for a user who typed -h,
// and it is also what cmd/gendocs captures into the committed help snapshots,
// so the docs would show an error message as the command's documentation.
func isHelpArg(args []string) bool {
	return len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "-help")
}

// daemonUsage is the `yanshi daemon -h` text. It is a literal rather than a
// flag.FlagSet dump because the verb is positional: the flags differ per verb
// (only stop takes -timeout, only reload takes -config) and a single FlagSet
// dump would advertise every flag as valid for every verb.
const daemonUsage = `Usage: yanshi daemon <status|stop|reload> [flags]

Operate the backend already running for this project, discovered through the
same lockfile the TUI uses. Run it from anywhere inside the project.

Verbs:
  status   Print pid, address, uptime and readiness. Exits 0 only when the
           daemon reports ready, so a monitoring check needs no text grep.
  stop     Ask the daemon to shut down and wait for the process to go.
  reload   Make the daemon re-read its config, applying the sections that can
           be applied and refusing the rest with a reason. Exits 1 when any
           section was refused.

Flags:
  -root string      project root (default: working directory)
  -json             emit machine-readable JSON instead of human-readable text
  -config string    config file to re-read (reload only; default: the daemon's own)
  -timeout duration how long to wait for the daemon to exit (stop only)
`

// scheduleUsage is the `yanshi schedule -h` text. See daemonUsage for why it
// is a literal.
const scheduleUsage = `Usage: yanshi schedule <list|show|pause|resume|run-now|delete> [ID] [flags]

Operate the scheduled automations held by the running daemon. Creating an
automation stays a model-facing tool (automation_create, approval-guarded);
this is the operations surface for what already exists.

Verbs:
  list             List every automation with its next fire time.
  show ID          Show one automation with its recent run history.
  pause ID         Stop firing it, keeping its definition.
  resume ID        Resume a paused automation.
  run-now ID       Trigger one run immediately, off-schedule.
  delete ID        Remove it permanently.

Flags:
  -root string     project root (default: working directory)
  -json            emit machine-readable JSON instead of a table
`

// emitJSON writes v as a single JSON document, mapping an encoding failure to
// the runtime exit code.
func emitJSON(stdout, stderr io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(stderr, "yanshi: render json: %v\n", err)
		return exitErr
	}
	return exitOK
}

// daemonStopGrace is the shutdown budget the serve loop gives in-flight
// requests when a control call asks it to stop. Long enough for a streaming
// turn to finish its current frame, short enough that `daemon stop` returns
// while the operator is still watching.
const daemonStopGrace = 10 * time.Second

// opsConfig carries what withOpsEndpoints needs from the composition root.
type opsConfig struct {
	// ConfigPath is the file `serve` was started with, so a reload with no
	// explicit path re-reads the right one.
	ConfigPath string
	// Current is the config the daemon booted with, used as the reload
	// baseline so only genuinely changed sections are reported.
	Current *config.Config
	// App is the assembled application. The scheduler and the feature registry
	// are read from it rather than passed separately so a build where a
	// subsystem failed to assemble yields nil -- which the handler reports
	// honestly instead of showing an empty schedule list.
	App *bootstrap.App
	// Scheduler is the running automation manager, or nil when this build has
	// none.
	//
	// Left as an explicit field rather than always reading App.Automation so a
	// test can drive the endpoint without assembling a whole App. When it is
	// nil, schedulerFor falls back to App.Automation — which is the production
	// path, and the one that was missing: App did not expose the C1 manager at
	// all, so every `yanshi schedule` call against a real daemon answered "this
	// daemon was assembled without the automation scheduler".
	Scheduler cli.ScheduleManager
	// Stop cancels the serve loop's context, which runs the same graceful
	// teardown a SIGTERM would.
	Stop func()
}

// withOpsEndpoints layers the daemon control and schedule routes in front of
// the application handler.
//
// It wraps rather than registering on the server's mux because App exposes the
// assembled http.Handler, not the mux underneath. Wrapping also makes the
// precedence explicit: these two paths are handled here and nothing else is
// touched, so no existing route can be shadowed by accident.
//
// The loopback restriction is deliberately STRICTER than the server's own auth
// middleware, which admits a non-loopback client that presents the bearer
// token. "Stop this process" and "delete this automation" are a different class
// of operation from "send a chat turn": the daemon is discovered through a
// per-project lockfile that only a local process can read, so there is no
// legitimate remote caller, and a leaked token should not be enough to
// terminate someone's backend.
// schedulerFor resolves the automation manager the schedule endpoint operates
// through: an explicitly injected one wins, otherwise the assembled App's.
//
// The nil check is on the concrete pointer, not the interface. Assigning a nil
// *automation.Manager into cli.ScheduleManager yields a non-nil interface
// holding a nil pointer, so NewScheduleHandler's own "no scheduler" guard would
// not fire and the first List() call would dereference nil — a panic inside the
// daemon instead of the honest 501 the handler is written to return.
func schedulerFor(cfg opsConfig) cli.ScheduleManager {
	if cfg.Scheduler != nil {
		return cfg.Scheduler
	}
	if cfg.App != nil && cfg.App.Automation != nil {
		return cfg.App.Automation
	}
	return nil
}

func withOpsEndpoints(next http.Handler, cfg opsConfig) http.Handler {
	control := cli.NewControlHandler(cli.ControlHooks{
		ConfigPath:    cfg.ConfigPath,
		CurrentConfig: func() *config.Config { return cfg.Current },
		ApplyReload:   applyReloadTo(cfg.App),
		Stop: func() {
			if cfg.Stop != nil {
				cfg.Stop()
			}
		},
	})
	schedule := cli.NewScheduleHandler(schedulerFor(cfg))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case cli.ControlPath, cli.SchedulePath:
			if !requestIsLoopback(r.RemoteAddr) {
				http.Error(w, "daemon operations are loopback-only", http.StatusForbidden)
				return
			}
			if r.URL.Path == cli.ControlPath {
				control(w, r)
				return
			}
			schedule(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// requestIsLoopback reports whether a request came from this machine.
//
// A parse failure is treated as NOT loopback. That direction is the whole
// value of the check: an address shape this code does not understand is one it
// cannot vouch for, and the cost of being wrong is an operator using a
// different mechanism rather than a stranger stopping their daemon.
func requestIsLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// A bare address with no port still deserves an answer; net.ParseIP
		// rejects anything that is not an address, which is the fail-closed
		// outcome we want.
		host = remoteAddr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// applyReloadTo adopts the reloadable sections of a new config and returns the
// ones it ACTUALLY adopted.
//
// Only the sections with a real runtime path are returned. Everything else is
// demoted to a refusal by the control handler, which is why this function can
// be honest about the current state of the wiring instead of having to claim
// coverage it does not have: as apply paths are added, they are added here and
// the operator-visible answer improves on its own.
//
// `features` is here because features.Registry.ApplyMap is a genuine runtime
// mutation path that already exists. `observability.log` is NOT, despite being
// classified reloadable: re-installing the logger needs the file writer that
// bootstrap resolved at boot, which App does not expose today. Rather than
// re-deriving the writer here (and quietly opening a SECOND handle to the same
// log file, which is how interleaved half-lines happen), the section is left
// unadopted and the operator is told to restart. That is the honest answer
// until App carries the writer.
func applyReloadTo(app *bootstrap.App) func(*config.Config, []string) ([]string, error) {
	if app == nil {
		return nil
	}
	return func(cfg *config.Config, sections []string) ([]string, error) {
		var adopted []string
		for _, section := range sections {
			if section != "features" || app.Features == nil {
				continue
			}
			if err := app.Features.ApplyMap(cfg.Features.Overrides); err != nil {
				return adopted, err
			}
			adopted = append(adopted, section)
		}
		return adopted, nil
	}
}
