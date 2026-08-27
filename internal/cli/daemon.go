package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/x6nux/yanshi/internal/lockfile"
)

// ControlPath is the daemon control endpoint. It is a POST-only route on the
// same loopback server the TUI talks to, which is what makes `daemon stop` and
// `daemon reload` work identically on Windows and on Unix: Windows has no
// SIGTERM and no SIGHUP, and a control endpoint needs no platform code at all.
//
// A signal is still the right mechanism for an operator at a shell prompt, and
// `yanshi serve` keeps its SIGINT/SIGTERM handler. This is the mechanism for a
// SECOND process that has to reach the first one by name.
const ControlPath = "/api/v1/daemon/control"

// ControlOp names a daemon control operation.
type ControlOp string

const (
	// ControlStop asks the daemon to shut down gracefully.
	ControlStop ControlOp = "stop"
	// ControlReload asks the daemon to re-read its configuration.
	ControlReload ControlOp = "reload"
)

// ControlRequest is the POST body of a control call.
type ControlRequest struct {
	Op ControlOp `json:"op"`
	// ConfigPath, for reload, names the file to re-read. Empty means the
	// daemon re-reads whatever path it was started with, which is the case an
	// operator means 99% of the time.
	ConfigPath string `json:"config_path,omitempty"`
}

// ControlResponse is the reply to a control call.
type ControlResponse struct {
	// OK is true when the operation was accepted.
	OK bool `json:"ok"`
	// Applied lists the config sections a reload actually re-read.
	Applied []string `json:"applied,omitempty"`
	// Rejected lists the changed sections a reload REFUSED, each with the
	// reason. A reload that silently ignored them would leave the operator
	// believing a listen-address change took effect.
	Rejected []RejectedReload `json:"rejected,omitempty"`
	// Message is a human-readable summary. It never echoes a config value.
	Message string `json:"message,omitempty"`
}

// RejectedReload names one configuration section a reload could not apply.
type RejectedReload struct {
	Section string `json:"section"`
	Reason  string `json:"reason"`
}

// reloadableSections are the configuration areas a running daemon can adopt
// without a restart, each with the reason it is safe.
//
// The list is short on purpose. "Reloadable" is not a property of a YAML key,
// it is a property of what the value is BOUND to at runtime: a value read
// afresh on every use can change under a running process, and a value captured
// into a listener, a file handle or a connection pool cannot.
var reloadableSections = map[string]string{
	"profiles":          "guard consults the profile map per action, so a new map takes effect on the next tool call",
	"compaction":        "thresholds are read per turn when sizing the history",
	"observability.log": "the logger is re-installed as slog.Default, which every later call resolves through",
	"loop_guard":        "budgets are read when a turn starts",
	"features":          "the flag registry is a runtime map with its own mutation path",
	"pricing":           "the price table is consulted when a turn's cost is computed",
}

// nonReloadableSections are the areas that require a restart, each with the
// reason. They are enumerated rather than left as "everything else" because an
// operator who changed one needs to be TOLD, not silently ignored.
//
// Every entry names a resource that was acquired once at boot and is now held
// by something with no re-open path: a bound socket, an open database handle,
// a spawned subprocess, an initialised exporter pipeline.
var nonReloadableSections = map[string]string{
	"server.http_addr":    "the listener is already bound; rebinding would drop every connected client",
	"server.task_addr":    "same as server.http_addr: the socket is bound at boot",
	"storage.sqlite_path": "the database handle is open and holds the session, VCS and task state",
	"storage":             "the SQLite pool and its pragmas are fixed when the handle opens",
	"llm.providers":       "provider clients and the per-model context windows are built at boot",
	"vcs.worktree_dir":    "worktrees under the old root are live working copies",
	"mcp.servers":         "MCP servers are spawned subprocesses with established sessions",
	"lsp":                 "language servers are spawned subprocesses with an indexed workspace",
	"secrets":             "the credential backend is opened once and holds decrypted material",
	"observability.otel":  "the exporter pipeline and its connections are established at boot",
	"token":               "the auth middleware captured the token; changing it mid-flight would strand authenticated clients",
	"security.sandbox":    "the sandbox posture is applied to the process at boot",
}

// ReloadableSections returns the reloadable section names in a stable order.
func ReloadableSections() []string { return sortedSectionNames(reloadableSections) }

// NonReloadableSections returns the restart-required section names in a stable
// order.
func NonReloadableSections() []string { return sortedSectionNames(nonReloadableSections) }

// ReloadReason returns the recorded justification for a section being
// reloadable or not, and whether the section is known at all.
func ReloadReason(section string) (reason string, reloadable, known bool) {
	if r, ok := reloadableSections[section]; ok {
		return r, true, true
	}
	if r, ok := nonReloadableSections[section]; ok {
		return r, false, true
	}
	return "", false, false
}

// sortedSectionNames returns a map's keys in a stable order.
func sortedSectionNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ClassifyReload splits the changed section names into those a running daemon
// can adopt and those that need a restart.
//
// An UNKNOWN section is classified as non-reloadable. That default is the
// point: a section nobody has reasoned about is a section whose runtime
// bindings nobody has checked, and "restart to be sure" costs one restart
// while "reload and hope" costs a silently stale process.
func ClassifyReload(changed []string) (applied []string, rejected []RejectedReload) {
	for _, section := range changed {
		if _, ok := reloadableSections[section]; ok {
			applied = append(applied, section)
			continue
		}
		reason, ok := nonReloadableSections[section]
		if !ok {
			reason = "not classified as reloadable; restart to apply " +
				"(an unreviewed section is treated as bound at boot)"
		}
		rejected = append(rejected, RejectedReload{Section: section, Reason: reason})
	}
	sort.Strings(applied)
	sort.Slice(rejected, func(i, j int) bool { return rejected[i].Section < rejected[j].Section })
	return applied, rejected
}

// DaemonStatus is what `yanshi daemon status` reports.
type DaemonStatus struct {
	// Found is false when no lockfile exists for the project, i.e. no daemon
	// has ever claimed it.
	Found bool `json:"found"`
	// PID, Addr and StartedAt come from the lockfile.
	PID       int       `json:"pid,omitempty"`
	Addr      string    `json:"addr,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
	// Alive reports whether the recorded PID is a running process. A PID that
	// is gone with a lockfile still present is the "stale lockfile" state
	// doctor -fix repairs.
	Alive bool `json:"alive"`
	// Ready reports whether the daemon answers the readiness probe. Alive
	// without Ready is a process that is up but still assembling -- exactly
	// the distinction O7 introduced, surfaced here so an operator can see it
	// rather than infer it from a hung window.
	Ready bool `json:"ready"`
	// Uptime is now-StartedAt, or zero when no lockfile was found.
	Uptime time.Duration `json:"uptime_ns,omitempty"`
	// Root is the project root the status was resolved for.
	Root string `json:"root"`
}

// RunDaemonStatus reads the project lockfile and probes the recorded address.
//
// It reports both liveness and readiness because they answer different
// questions and an operator debugging "my second window will not connect"
// needs to tell them apart: a live-but-unready daemon is starting up, a
// live-and-unready-forever daemon is wedged, and an alive=false lockfile is
// litter.
func RunDaemonStatus(ctx context.Context, root string) DaemonStatus {
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	status := DaemonStatus{Root: root}
	lf, err := lockfile.Read(root)
	if err != nil {
		return status
	}
	status.Found = true
	status.PID = lf.PID
	status.Addr = lf.Addr
	status.StartedAt = lf.StartedAt
	status.Alive = lf.Alive()
	if !lf.StartedAt.IsZero() {
		status.Uptime = time.Since(lf.StartedAt)
	}
	if status.Alive && lf.Addr != "" {
		status.Ready = ready(ctx, "http://"+lf.Addr)
	}
	return status
}

// RenderDaemonStatus writes the human-readable status.
func RenderDaemonStatus(w io.Writer, s DaemonStatus) {
	if !s.Found {
		fmt.Fprintf(w, "no daemon lockfile for %s\n", s.Root)
		return
	}
	state := "stale (pid not running)"
	switch {
	case s.Alive && s.Ready:
		state = "ready"
	case s.Alive:
		state = "starting (alive but not ready)"
	}
	fmt.Fprintf(w, "root       %s\n", s.Root)
	fmt.Fprintf(w, "pid        %d\n", s.PID)
	fmt.Fprintf(w, "addr       %s\n", s.Addr)
	if !s.StartedAt.IsZero() {
		fmt.Fprintf(w, "started    %s\n", s.StartedAt.Format(time.RFC3339))
		fmt.Fprintf(w, "uptime     %s\n", s.Uptime.Round(time.Second))
	}
	fmt.Fprintf(w, "state      %s\n", state)
}

// ErrNoDaemon is returned by the control commands when no live daemon owns the
// project.
var ErrNoDaemon = errors.New("cli: no running daemon for this project")

// DaemonStopTimeout bounds how long `daemon stop` waits for the process to
// disappear after the control call is accepted.
const DaemonStopTimeout = 20 * time.Second

// RunDaemonStop asks the project's daemon to shut down and waits for its PID to
// stop existing.
//
// Waiting rather than firing and forgetting is what makes the command usable in
// a script: `yanshi daemon stop && yanshi serve` has to be safe, and it is only
// safe if the first command returns after the port is free.
//
// The lockfile is removed on success. The daemon removes it too on a clean
// exit; doing it here as well covers the case where it died between accepting
// the control call and running its own teardown, which would otherwise leave
// litter that makes the next window think a backend is running.
func RunDaemonStop(ctx context.Context, root string, timeout time.Duration) error {
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	lf, err := lockfile.Read(root)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrNoDaemon, root)
	}
	if !lf.Alive() {
		_ = lockfile.Remove(root)
		return fmt.Errorf("%w (stale lockfile for pid %d removed)", ErrNoDaemon, lf.PID)
	}
	if _, err := postControl(ctx, "http://"+lf.Addr, ControlRequest{Op: ControlStop}); err != nil {
		return err
	}
	if timeout <= 0 {
		timeout = DaemonStopTimeout
	}
	if err := waitForExit(ctx, lf, timeout); err != nil {
		return err
	}
	_ = lockfile.Remove(root)
	return nil
}

// waitForExit polls the recorded PID until it is gone or the deadline passes.
//
// Polling rather than waiting on the process handle is deliberate: the daemon
// is not this process's child (that is the whole reason it needs a lockfile),
// so there is no handle to wait on, and the PID-liveness check is the same one
// discovery already trusts on both platforms.
func waitForExit(ctx context.Context, lf lockfile.Lockfile, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if !lf.Alive() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon pid %d did not exit within %s", lf.PID, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// RunDaemonReload asks the project's daemon to re-read its configuration and
// returns what it applied and what it refused.
func RunDaemonReload(ctx context.Context, root, configPath string) (ControlResponse, error) {
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	lf, err := lockfile.Read(root)
	if err != nil {
		return ControlResponse{}, fmt.Errorf("%w: %s", ErrNoDaemon, root)
	}
	if !lf.Alive() {
		return ControlResponse{}, fmt.Errorf("%w (lockfile names pid %d, which is not running)",
			ErrNoDaemon, lf.PID)
	}
	return postControl(ctx, "http://"+lf.Addr, ControlRequest{
		Op: ControlReload, ConfigPath: configPath,
	})
}

// controlTimeout bounds a single control call. A daemon that cannot answer a
// POST in this long is not going to answer it at all, and the operator needs
// their prompt back.
const controlTimeout = 10 * time.Second

// postControl issues the control POST and decodes the reply.
//
// A 404 is translated into ErrNoDaemon with an upgrade hint rather than a raw
// status: during an upgrade the running daemon is an older binary with no
// control route, and "404" tells the operator nothing about what to do.
func postControl(ctx context.Context, baseURL string, req ControlRequest) (ControlResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return ControlResponse{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(callCtx, "POST", baseURL+ControlPath,
		strings.NewReader(string(body)))
	if err != nil {
		return ControlResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return ControlResponse{}, fmt.Errorf("control call to %s: %w", baseURL, err)
	}
	defer resp.Body.Close()

	if isMissingRoute(resp) {
		return ControlResponse{}, fmt.Errorf(
			"%w: the running daemon has no control endpoint (it predates `yanshi daemon`; "+
				"stop it with Ctrl-C or a signal, then restart)", ErrNoDaemon)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return ControlResponse{}, fmt.Errorf("control call returned %s", resp.Status)
	}
	var out ControlResponse
	if derr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); derr != nil {
		return ControlResponse{}, fmt.Errorf("decode control reply: %w", derr)
	}
	return out, nil
}

// RenderControlResponse writes the human-readable result of a reload.
//
// The rejected list is printed even though it is the boring half, because it is
// the half that prevents the failure this command exists to avoid: an operator
// who changed http_addr, ran reload, saw "ok", and spent an afternoon wondering
// why the daemon is still on the old port.
func RenderControlResponse(w io.Writer, r ControlResponse) {
	if r.Message != "" {
		fmt.Fprintln(w, r.Message)
	}
	if len(r.Applied) > 0 {
		fmt.Fprintf(w, "reloaded: %s\n", strings.Join(r.Applied, ", "))
	}
	for _, rej := range r.Rejected {
		fmt.Fprintf(w, "NOT reloaded: %s — %s\n", rej.Section, rej.Reason)
	}
	if len(r.Rejected) > 0 {
		fmt.Fprintln(w, "restart the daemon to apply the sections above")
	}
	if len(r.Applied) == 0 && len(r.Rejected) == 0 {
		fmt.Fprintln(w, "config re-read; nothing changed")
	}
}
