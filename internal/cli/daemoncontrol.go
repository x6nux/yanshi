package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"

	"github.com/x6nux/yanshi/internal/config"
)

// ControlHooks are the actions a daemon exposes to the control endpoint. The
// composition root supplies them; this package supplies the protocol, the
// classification and the refusal semantics, so the two are testable apart.
type ControlHooks struct {
	// Stop initiates a graceful shutdown. It MUST return promptly and do the
	// actual teardown asynchronously: the reply has to reach the client before
	// the server it travelled over stops accepting connections, or every stop
	// looks like a connection error to the caller.
	Stop func()
	// ConfigPath is the file the daemon was started with, used when a reload
	// request names none.
	ConfigPath string
	// ApplyReload adopts the reloadable parts of a freshly loaded config and
	// returns the sections it ACTUALLY adopted.
	//
	// The return value is the point. Classification answers "is this section
	// safe to reload in principle"; only the daemon knows whether it has a
	// wired apply path for it today. A hook that returned nothing but an error
	// would let the reply claim `profiles` was applied while the daemon
	// quietly did nothing with it -- a section that is classified reloadable,
	// reported reloaded, and has no consumer is the exact "written but nobody
	// reads it" shape, and it would be invisible from the operator's side.
	// Sections omitted from the return value are demoted to refusals with a
	// reason, so the reply can only ever under-claim.
	//
	// It runs after classification has already decided what is safe, so it
	// never has to make that judgement itself.
	ApplyReload func(cfg *config.Config, sections []string) ([]string, error)
	// CurrentConfig returns the config the daemon is running with, so a reload
	// can diff against it and report only what actually changed. Nil means
	// "no baseline": every reloadable section is then reported as applied,
	// which is honest -- without a baseline the daemon genuinely cannot tell a
	// changed section from an unchanged one.
	CurrentConfig func() *config.Config
}

// NewControlHandler returns the HTTP handler for ControlPath.
//
// It is POST-only. A control endpoint reachable by GET is one a browser tab, a
// link preview or a curl typo can trigger, and "stop the daemon" is not an
// operation that should have an accidental invocation path.
//
// Authentication is the server's job, not this handler's: the route sits behind
// the same middleware as every other API route, which trusts loopback and
// requires a bearer token otherwise. Adding a second, different check here
// would create two places where "who may stop the daemon" is decided.
func NewControlHandler(hooks ControlHooks) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeControl(w, http.StatusMethodNotAllowed, ControlResponse{
				Message: "the daemon control endpoint accepts POST only",
			})
			return
		}
		var req ControlRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeControl(w, http.StatusBadRequest, ControlResponse{
				Message: "malformed control request",
			})
			return
		}
		switch req.Op {
		case ControlStop:
			handleControlStop(w, hooks)
		case ControlReload:
			handleControlReload(w, req, hooks)
		default:
			writeControl(w, http.StatusBadRequest, ControlResponse{
				Message: fmt.Sprintf("unknown control op %q (want %q or %q)",
					req.Op, ControlStop, ControlReload),
			})
		}
	}
}

// handleControlStop acknowledges the request and then triggers shutdown.
//
// The reply is written BEFORE Stop runs, and Stop runs on its own goroutine,
// because the response travels over the very server being shut down. Calling
// Stop first would race the flush and hand the operator a connection error for
// an operation that succeeded.
func handleControlStop(w http.ResponseWriter, hooks ControlHooks) {
	if hooks.Stop == nil {
		writeControl(w, http.StatusNotImplemented, ControlResponse{
			Message: "this daemon was assembled without a stop hook",
		})
		return
	}
	writeControl(w, http.StatusAccepted, ControlResponse{
		OK: true, Message: "shutting down",
	})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go hooks.Stop()
}

// handleControlReload loads the named config, classifies what changed, applies
// the reloadable half and reports the rest as refused.
func handleControlReload(w http.ResponseWriter, req ControlRequest, hooks ControlHooks) {
	path := req.ConfigPath
	if path == "" {
		path = hooks.ConfigPath
	}
	if path == "" {
		writeControl(w, http.StatusBadRequest, ControlResponse{
			Message: "no config path: the daemon was started without one and the request named none",
		})
		return
	}
	cfg, err := config.Load(path)
	if err != nil {
		// The raw load error is NOT echoed, for the same reason doctor's
		// config check does not echo it: a rejected config often names a raw
		// api_key value, and this reply crosses a network boundary.
		writeControl(w, http.StatusBadRequest, ControlResponse{
			Message: fmt.Sprintf("reload rejected: %s did not load; the running "+
				"configuration is unchanged", path),
		})
		return
	}

	var changed []string
	if hooks.CurrentConfig != nil {
		changed = ChangedSections(hooks.CurrentConfig(), cfg)
	} else {
		changed = ReloadableSections()
	}
	applied, rejected := ClassifyReload(changed)

	if len(applied) > 0 {
		if hooks.ApplyReload == nil {
			// No apply path at all: every classified-safe section is demoted.
			// Reporting them as applied would be a lie the operator cannot
			// detect from the outside.
			for _, section := range applied {
				rejected = append(rejected, RejectedReload{Section: section,
					Reason: "this daemon has no apply path for it; restart to adopt"})
			}
			applied = nil
		} else {
			adopted, aerr := hooks.ApplyReload(cfg, applied)
			if aerr != nil {
				writeControl(w, http.StatusInternalServerError, ControlResponse{
					Rejected: rejected,
					Message:  "reload failed while applying; the daemon kept its previous settings",
				})
				return
			}
			applied, rejected = reconcileApplied(applied, adopted, rejected)
		}
	}
	sortRejected(rejected)
	writeControl(w, http.StatusOK, ControlResponse{
		OK: true, Applied: applied, Rejected: rejected,
		Message: fmt.Sprintf("re-read %s", path),
	})
}

// reconcileApplied intersects what was classified reloadable with what the
// daemon reports it actually adopted, demoting the difference to refusals.
//
// The direction is one-way on purpose: a section the hook adopted but that was
// never classified safe is NOT promoted into Applied. Doing so would let a
// wiring mistake in the composition root silently widen the reload boundary
// this package exists to state.
func reconcileApplied(classified, adopted []string, rejected []RejectedReload) ([]string, []RejectedReload) {
	adoptedSet := make(map[string]bool, len(adopted))
	for _, s := range adopted {
		adoptedSet[s] = true
	}
	var applied []string
	for _, section := range classified {
		if adoptedSet[section] {
			applied = append(applied, section)
			continue
		}
		rejected = append(rejected, RejectedReload{Section: section,
			Reason: "the daemon has no apply path wired for it; restart to adopt"})
	}
	return applied, rejected
}

// sortRejected keeps the refusal list deterministic for output and tests.
func sortRejected(rejected []RejectedReload) {
	sort.Slice(rejected, func(i, j int) bool { return rejected[i].Section < rejected[j].Section })
}

// writeControl serialises a control reply.
func writeControl(w http.ResponseWriter, status int, resp ControlResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

// ChangedSections reports which named configuration sections differ between
// two loaded configs.
//
// The comparison is on the MARSHALLED form of each section rather than on
// struct equality, because several sections contain maps and slices that Go
// cannot compare with == at all, and a reflect.DeepEqual over the whole Config
// would not tell the caller WHICH section moved.
//
// A nil old config means "no baseline", and every section is reported changed:
// without something to diff against, claiming a section is unchanged would be
// a guess, and the consequence of guessing wrong is a daemon running settings
// nobody can see.
func ChangedSections(old, next *config.Config) []string {
	if next == nil {
		return nil
	}
	names := append(ReloadableSections(), NonReloadableSections()...)
	if old == nil {
		return names
	}
	var changed []string
	for _, name := range names {
		if sectionFingerprint(old, name) != sectionFingerprint(next, name) {
			changed = append(changed, name)
		}
	}
	return changed
}

// sectionFingerprint renders one named section of a config to a comparable
// string. An unknown name yields "", so two configs always agree about it --
// which keeps ChangedSections from inventing a change for a section it does
// not know how to read.
func sectionFingerprint(cfg *config.Config, section string) string {
	if cfg == nil {
		return ""
	}
	var value any
	switch section {
	case "profiles":
		value = cfg.Profiles
	case "compaction":
		value = cfg.Compaction
	case "observability.log":
		value = cfg.Observability.Log
	case "observability.otel":
		value = cfg.Observability.OTel
	case "loop_guard":
		value = cfg.LoopGuard
	case "features":
		value = cfg.Features
	case "pricing":
		value = cfg.Pricing
	case "server.http_addr":
		value = cfg.Server.HTTPAddr
	case "server.task_addr":
		value = cfg.Server.TaskAddr
	case "storage":
		value = cfg.Storage
	case "storage.sqlite_path":
		value = cfg.Storage.SQLitePath
	case "llm.providers":
		value = cfg.LLM.Providers
	case "vcs.worktree_dir":
		value = cfg.VCS.WorktreeDir
	case "mcp.servers":
		value = cfg.MCP.Servers
	case "lsp":
		value = cfg.LSP
	case "secrets":
		value = cfg.Secrets
	case "token":
		value = cfg.Token
	case "security.sandbox":
		value = cfg.Security.Sandbox
	default:
		return ""
	}
	data, err := json.Marshal(value)
	if err != nil {
		// An unmarshallable section is treated as "always different", so a
		// reload errs toward re-applying rather than toward silently skipping.
		return fmt.Sprintf("unmarshallable:%v", err)
	}
	return string(data)
}
