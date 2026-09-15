package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/x6nux/yanshi/internal/ipc"
)

// This file is the CLI half of the operator control plane. Each verb is a thin
// client of `internal/ctl` — over the IPC socket when the state being described
// lives in a running daemon, and directly (offline) when it lives in the store.
//
// Why the split is visible in the errors: `jobs`, `mcp`, `skills`, `features`,
// `approvals` and `vcs` describe a PROCESS. A CLI that silently returned an
// empty list when no daemon is running would read as "there is nothing", which
// is the wrong fact to hand an operator debugging a missing job. Those verbs
// fail with "start one with `yanshi -b`". `session` and `usage` read the store
// and work either way.

// ctlUsage is the shared help.
const ctlUsage = `usage: yanshi <verb> [args] [-root DIR] [-json]

  usage     [<session-id>] [-limit N]     token/cost roll-up (offline)
  skills    list | show <name>            loaded skills, their state and body
  features  list | set <key> on|off       runtime feature flags
  approvals list [-session ID] | revoke <rule-id> [-session ID]
  jobs      list | read <id> [-max N] | cancel <id> | write <id> <data>
  mcp       list | enable <name> | disable <name>
  vcs       log [-limit N] | diff <from> [to]
  models    list                         models a session can switch to

Everything except usage/session reads state that lives in a RUNNING daemon and
goes through its IPC socket; start one with "yanshi -b". -json prints the raw
result object instead of a table.
`

// ctlClient is one JSON-RPC call over the project's IPC socket.
//
// It matches responses by id: the daemon may emit notifications (a turn started
// by another client, for instance) and taking the first line back would return
// one of those as if it were the answer.
type ctlClient struct {
	conn net.Conn
	sc   *bufio.Scanner
}

func dialCtl(ctx context.Context, root string) (*ctlClient, error) {
	conn, err := ipc.Dial(ctx, root)
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	return &ctlClient{conn: conn, sc: sc}, nil
}

func (c *ctlClient) Close() { _ = c.conn.Close() }

func (c *ctlClient) call(method string, params any) (json.RawMessage, error) {
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	line, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := c.conn.Write(append(line, '\n')); err != nil {
		return nil, err
	}
	for c.sc.Scan() {
		var resp struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(c.sc.Bytes(), &resp); err != nil {
			continue
		}
		if len(resp.ID) == 0 {
			continue // a notification, not our answer
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s", resp.Error.Message)
		}
		return resp.Result, nil
	}
	if err := c.sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("daemon closed the connection before answering %s", method)
}

// ctlRun opens the socket, runs fn, and maps the "no daemon" case onto advice
// the operator can act on.
func ctlRun(stdout, stderr io.Writer, fn func(*ctlClient) error) int {
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "yanshi: %v\n", err)
		return exitErr
	}
	if wd := ctlRootOverride(); wd != "" {
		root = wd
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c, err := dialCtl(ctx, root)
	if err != nil {
		fmt.Fprintf(stderr, "yanshi: %v\n", err)
		fmt.Fprintln(stderr, "yanshi: this state lives in a running daemon; start one with `yanshi -b`")
		return exitErr
	}
	defer c.Close()
	if err := fn(c); err != nil {
		fmt.Fprintf(stderr, "yanshi: %v\n", err)
		return exitErr
	}
	return exitOK
}

// ctlRootOverride lets a verb address another project's daemon. It reads
// YANSHI_ROOT rather than adding -root to every verb: the platform-level
// socket path already identifies a project, and threading a flag through ten
// verbs is how one of them ends up ignoring it.
func ctlRootOverride() string { return os.Getenv("YANSHI_ROOT") }

// printJSON is the shared -json rendering: the daemon's result object verbatim,
// so a script sees exactly what the protocol said and not a re-rendering.
func printJSON(w io.Writer, raw json.RawMessage) int {
	fmt.Fprintln(w, string(raw))
	return exitOK
}

// wantHelp reports whether a verb was asked for its help. Every verb checks it
// FIRST, before deciding whether the first argument is a verb name: `yanshi
// skills -h` is a help request, not an unknown verb, and the docs generator
// runs exactly that spelling when it captures help snapshots (the first run of
// it recorded `unknown verb "-h"` into docs/user-guide/entrypoints.md).
func wantHelp(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "-h", "--help", "help":
		return true
	}
	return false
}

// scanOrUsage maps a flagScanner error onto an exit code, treating -h as a
// successful help request rather than a usage error.
func scanOrUsage(sc *flagScanner, args []string, verb string, stdout, stderr io.Writer) (bool, int) {
	if err := sc.parse(args); err != nil {
		if err == errHelpRequested {
			fmt.Fprint(stdout, ctlUsage)
			return false, exitOK
		}
		fmt.Fprintf(stderr, "yanshi %s: %v\n", verb, err)
		return false, exitUsage
	}
	return true, exitOK
}

// runUsage prints the token/cost roll-up. It reads the store, so it works with
// no daemon: an operator asking "what did this cost" should not need a backend.
func runUsage(args []string, stdout, stderr io.Writer) int {
	if wantHelp(args) {
		fmt.Fprint(stdout, ctlUsage)
		return exitOK
	}
	sc := newFlagScanner([]string{"json"}, []string{"config", "limit"})
	if ok, code := scanOrUsage(sc, args, "usage", stdout, stderr); !ok {
		return code
	}
	limit, err := sc.intValue("limit", 20)
	if err != nil {
		fmt.Fprintf(stderr, "yanshi usage: %v\n", err)
		return exitUsage
	}
	pos := sc.positionals()
	if len(pos) > 1 {
		fmt.Fprint(stderr, ctlUsage)
		return exitUsage
	}
	id := ""
	if len(pos) == 1 {
		id = pos[0]
	}
	svc, err := openCLICtl(sc.value("config", "config.yaml"))
	if err != nil {
		fmt.Fprintf(stderr, "yanshi usage: %v\n", err)
		return exitErr
	}
	defer svc.Close()
	usage, err := svc.Usage(id, limit)
	if err != nil {
		fmt.Fprintf(stderr, "yanshi usage: %v\n", err)
		return exitErr
	}
	if sc.has("json") {
		line, _ := json.Marshal(usage)
		fmt.Fprintln(stdout, string(line))
		return exitOK
	}
	fmt.Fprintf(stdout, "sessions:  %d\n", usage.Sessions)
	fmt.Fprintf(stdout, "turns:     %d\n", usage.Turns)
	fmt.Fprintf(stdout, "tokens:    in=%d out=%d cached=%d reasoning=%d\n",
		usage.TokensIn, usage.TokensOut, usage.CachedIn, usage.Reasoning)
	if usage.CostKnown {
		fmt.Fprintf(stdout, "cost:      $%.4f\n", usage.CostUSD)
	} else {
		fmt.Fprintln(stdout, "cost:      N/A — at least one session used a provider with no pricing entry")
	}
	return exitOK
}

// runSkills lists, shows and toggles skills (live registry: needs a daemon).
func runSkills(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, ctlUsage)
		return exitUsage
	}
	verb := args[0]
	if wantHelp(args) {
		fmt.Fprint(stdout, ctlUsage)
		return exitOK
	}
	sc := newFlagScanner([]string{"json"}, []string{"config"})
	if ok, code := scanOrUsage(sc, args[1:], "skills", stdout, stderr); !ok {
		return code
	}
	pos := sc.positionals()
	switch verb {
	case "list":
		return ctlRun(stdout, stderr, func(c *ctlClient) error {
			raw, err := c.call("skills/list", nil)
			if err != nil {
				return err
			}
			if sc.has("json") {
				fmt.Fprintln(stdout, string(raw))
				return nil
			}
			var payload struct {
				Skills []struct {
					Name, Source, Description string
					Enabled, Trusted          bool
					Missing, Unsafe           []string
				} `json:"skills"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				return err
			}
			if len(payload.Skills) == 0 {
				fmt.Fprintln(stdout, "no skills loaded")
				return nil
			}
			for _, s := range payload.Skills {
				flags := []string{s.Source}
				if !s.Enabled {
					flags = append(flags, "disabled")
				}
				if s.Trusted {
					flags = append(flags, "trusted")
				}
				if len(s.Missing) > 0 {
					flags = append(flags, "missing:"+strings.Join(s.Missing, ","))
				}
				if len(s.Unsafe) > 0 {
					flags = append(flags, "UNSAFE")
				}
				fmt.Fprintf(stdout, "%-28s %s\n", s.Name, strings.Join(flags, " "))
			}
			return nil
		})
	case "show":
		if len(pos) != 1 {
			fmt.Fprint(stderr, ctlUsage)
			return exitUsage
		}
		return ctlRun(stdout, stderr, func(c *ctlClient) error {
			raw, err := c.call("skills/show", map[string]any{"name": pos[0]})
			if err != nil {
				return err
			}
			if sc.has("json") {
				fmt.Fprintln(stdout, string(raw))
				return nil
			}
			var payload struct {
				Skill struct {
					Name, Source, Description string
					Enabled, Trusted          bool
				} `json:"skill"`
				Body string `json:"body"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "name:        %s\n", payload.Skill.Name)
			fmt.Fprintf(stdout, "source:      %s\n", payload.Skill.Source)
			fmt.Fprintf(stdout, "enabled:     %v\n", payload.Skill.Enabled)
			fmt.Fprintf(stdout, "trusted:     %v\n", payload.Skill.Trusted)
			fmt.Fprintf(stdout, "description: %s\n", payload.Skill.Description)
			fmt.Fprintln(stdout, "---")
			fmt.Fprintln(stdout, payload.Body)
			return nil
		})
	case "enable", "disable", "trust", "untrust":
		if len(pos) != 1 {
			fmt.Fprint(stderr, ctlUsage)
			return exitUsage
		}
		return ctlRun(stdout, stderr, func(c *ctlClient) error {
			raw, err := c.call("skills/"+verb, map[string]any{"name": pos[0]})
			if err != nil {
				return err
			}
			fmt.Fprintln(stdout, string(raw))
			return nil
		})
	}
	fmt.Fprintf(stderr, "yanshi skills: unknown verb %q\n", verb)
	fmt.Fprint(stderr, ctlUsage)
	return exitUsage
}

// runFeatures lists and toggles runtime feature flags.
func runFeatures(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, ctlUsage)
		return exitUsage
	}
	verb := args[0]
	if wantHelp(args) {
		fmt.Fprint(stdout, ctlUsage)
		return exitOK
	}
	sc := newFlagScanner([]string{"json"}, nil)
	if ok, code := scanOrUsage(sc, args[1:], "features", stdout, stderr); !ok {
		return code
	}
	pos := sc.positionals()
	switch verb {
	case "list":
		return ctlRun(stdout, stderr, func(c *ctlClient) error {
			raw, err := c.call("features/list", nil)
			if err != nil {
				return err
			}
			if sc.has("json") {
				fmt.Fprintln(stdout, string(raw))
				return nil
			}
			var payload struct {
				Features []struct {
					Key     string `json:"key"`
					Stage   string `json:"stage"`
					Enabled bool   `json:"enabled"`
				} `json:"features"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				return err
			}
			for _, f := range payload.Features {
				state := "off"
				if f.Enabled {
					state = "on"
				}
				fmt.Fprintf(stdout, "%-32s %-3s %s\n", f.Key, state, f.Stage)
			}
			return nil
		})
	case "set":
		if len(pos) != 2 || (pos[1] != "on" && pos[1] != "off") {
			fmt.Fprintln(stderr, "yanshi features set <key> on|off")
			return exitUsage
		}
		return ctlRun(stdout, stderr, func(c *ctlClient) error {
			raw, err := c.call("features/set", map[string]any{"key": pos[0], "enabled": pos[1] == "on"})
			if err != nil {
				return err
			}
			fmt.Fprintln(stdout, string(raw))
			return nil
		})
	}
	fmt.Fprintf(stderr, "yanshi features: unknown verb %q\n", verb)
	return exitUsage
}

// runApprovals lists and revokes remembered permission rules.
func runApprovals(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, ctlUsage)
		return exitUsage
	}
	verb := args[0]
	if wantHelp(args) {
		fmt.Fprint(stdout, ctlUsage)
		return exitOK
	}
	sc := newFlagScanner([]string{"json"}, []string{"session"})
	if ok, code := scanOrUsage(sc, args[1:], "approvals", stdout, stderr); !ok {
		return code
	}
	pos := sc.positionals()
	sessionID := sc.value("session", "")
	switch verb {
	case "list":
		return ctlRun(stdout, stderr, func(c *ctlClient) error {
			raw, err := c.call("approvals/list", map[string]any{"sessionId": sessionID})
			if err != nil {
				return err
			}
			if sc.has("json") {
				fmt.Fprintln(stdout, string(raw))
				return nil
			}
			var payload struct {
				Rules []struct {
					ID     string `json:"id"`
					TTL    string `json:"ttl"`
					Tool   string `json:"tool"`
					Action string `json:"action"`
				} `json:"rules"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				return err
			}
			if len(payload.Rules) == 0 {
				fmt.Fprintln(stdout, "no remembered approval rules")
				return nil
			}
			for _, r := range payload.Rules {
				fmt.Fprintf(stdout, "%s  ttl=%s tool=%s action=%s\n", r.ID, r.TTL, r.Tool, r.Action)
			}
			return nil
		})
	case "revoke":
		if len(pos) != 1 {
			fmt.Fprint(stderr, ctlUsage)
			return exitUsage
		}
		return ctlRun(stdout, stderr, func(c *ctlClient) error {
			raw, err := c.call("approvals/revoke", map[string]any{"sessionId": sessionID, "id": pos[0]})
			if err != nil {
				return err
			}
			fmt.Fprintln(stdout, string(raw))
			return nil
		})
	}
	fmt.Fprintf(stderr, "yanshi approvals: unknown verb %q\n", verb)
	return exitUsage
}

// runJobs operates the daemon's background jobs.
func runJobs(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, ctlUsage)
		return exitUsage
	}
	verb := args[0]
	if wantHelp(args) {
		fmt.Fprint(stdout, ctlUsage)
		return exitOK
	}
	sc := newFlagScanner([]string{"json"}, []string{"max"})
	if ok, code := scanOrUsage(sc, args[1:], "jobs", stdout, stderr); !ok {
		return code
	}
	maxBytes, err := sc.intValue("max", 8192)
	if err != nil {
		fmt.Fprintf(stderr, "yanshi jobs: %v\n", err)
		return exitUsage
	}
	pos := sc.positionals()
	switch verb {
	case "list":
		return ctlRun(stdout, stderr, func(c *ctlClient) error {
			raw, err := c.call("jobs/list", nil)
			if err != nil {
				return err
			}
			if sc.has("json") {
				fmt.Fprintln(stdout, string(raw))
				return nil
			}
			var payload struct {
				Jobs []struct {
					ID        string `json:"id"`
					State     string `json:"state"`
					Command   string `json:"command"`
					ExitCode  int    `json:"exitCode"`
					OutputLen int    `json:"outputLen"`
				} `json:"jobs"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				return err
			}
			if len(payload.Jobs) == 0 {
				fmt.Fprintln(stdout, "no background jobs")
				return nil
			}
			for _, j := range payload.Jobs {
				fmt.Fprintf(stdout, "%s  %-9s exit=%d out=%dB  %s\n", j.ID, j.State, j.ExitCode, j.OutputLen, j.Command)
			}
			return nil
		})
	case "read":
		if len(pos) != 1 {
			fmt.Fprint(stderr, ctlUsage)
			return exitUsage
		}
		return ctlRun(stdout, stderr, func(c *ctlClient) error {
			raw, err := c.call("jobs/read", map[string]any{"id": pos[0], "max": maxBytes})
			if err != nil {
				return err
			}
			if sc.has("json") {
				fmt.Fprintln(stdout, string(raw))
				return nil
			}
			var payload struct {
				Output string `json:"output"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				return err
			}
			fmt.Fprint(stdout, payload.Output)
			return nil
		})
	case "write":
		if len(pos) < 2 {
			fmt.Fprint(stderr, ctlUsage)
			return exitUsage
		}
		data := strings.Join(pos[1:], " ")
		return ctlRun(stdout, stderr, func(c *ctlClient) error {
			raw, err := c.call("jobs/write", map[string]any{"id": pos[0], "data": data})
			if err != nil {
				return err
			}
			fmt.Fprintln(stdout, string(raw))
			return nil
		})
	case "cancel":
		if len(pos) != 1 {
			fmt.Fprint(stderr, ctlUsage)
			return exitUsage
		}
		return ctlRun(stdout, stderr, func(c *ctlClient) error {
			raw, err := c.call("jobs/cancel", map[string]any{"id": pos[0]})
			if err != nil {
				return err
			}
			fmt.Fprintln(stdout, string(raw))
			return nil
		})
	}
	fmt.Fprintf(stderr, "yanshi jobs: unknown verb %q\n", verb)
	return exitUsage
}

// runMcp lists and toggles MCP servers.
func runMcp(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, ctlUsage)
		return exitUsage
	}
	verb := args[0]
	if wantHelp(args) {
		fmt.Fprint(stdout, ctlUsage)
		return exitOK
	}
	sc := newFlagScanner([]string{"json"}, nil)
	if ok, code := scanOrUsage(sc, args[1:], "mcp", stdout, stderr); !ok {
		return code
	}
	pos := sc.positionals()
	switch verb {
	case "list":
		return ctlRun(stdout, stderr, func(c *ctlClient) error {
			raw, err := c.call("mcp/list", nil)
			if err != nil {
				return err
			}
			if sc.has("json") {
				fmt.Fprintln(stdout, string(raw))
				return nil
			}
			var payload struct {
				Servers []struct {
					Name      string   `json:"name"`
					Transport string   `json:"transport"`
					Status    string   `json:"status"`
					Tools     []string `json:"tools"`
					Error     string   `json:"error"`
				} `json:"servers"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				return err
			}
			if len(payload.Servers) == 0 {
				fmt.Fprintln(stdout, "no MCP servers configured")
				return nil
			}
			for _, s := range payload.Servers {
				fmt.Fprintf(stdout, "%-20s %-10s %-12s tools=%d %s\n", s.Name, s.Transport, s.Status, len(s.Tools), s.Error)
			}
			return nil
		})
	case "enable", "disable":
		if len(pos) != 1 {
			fmt.Fprint(stderr, ctlUsage)
			return exitUsage
		}
		return ctlRun(stdout, stderr, func(c *ctlClient) error {
			raw, err := c.call("mcp/"+verb, map[string]any{"name": pos[0]})
			if err != nil {
				return err
			}
			fmt.Fprintln(stdout, string(raw))
			return nil
		})
	}
	fmt.Fprintf(stderr, "yanshi mcp: unknown verb %q\n", verb)
	return exitUsage
}

// runVcs reads the autoVCS history.
func runVcs(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, ctlUsage)
		return exitUsage
	}
	verb := args[0]
	if wantHelp(args) {
		fmt.Fprint(stdout, ctlUsage)
		return exitOK
	}
	sc := newFlagScanner([]string{"json"}, []string{"limit"})
	if ok, code := scanOrUsage(sc, args[1:], "vcs", stdout, stderr); !ok {
		return code
	}
	limit, err := sc.intValue("limit", 20)
	if err != nil {
		fmt.Fprintf(stderr, "yanshi vcs: %v\n", err)
		return exitUsage
	}
	pos := sc.positionals()
	switch verb {
	case "log":
		return ctlRun(stdout, stderr, func(c *ctlClient) error {
			raw, err := c.call("vcs/log", map[string]any{"limit": limit})
			if err != nil {
				return err
			}
			if sc.has("json") {
				fmt.Fprintln(stdout, string(raw))
				return nil
			}
			var payload struct {
				Commits []struct {
					ID           string `json:"id"`
					Author       string `json:"author"`
					Message      string `json:"message"`
					CreatedAt    string `json:"createdAt"`
					FilesChanged int    `json:"filesChanged"`
				} `json:"commits"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				return err
			}
			if len(payload.Commits) == 0 {
				fmt.Fprintln(stdout, "no commits")
				return nil
			}
			for _, c := range payload.Commits {
				fmt.Fprintf(stdout, "%s  %s  %-12s files=%d  %s\n", c.ID, c.CreatedAt, c.Author, c.FilesChanged, firstLine(c.Message))
			}
			return nil
		})
	case "diff":
		if len(pos) < 1 || len(pos) > 2 {
			fmt.Fprint(stderr, ctlUsage)
			return exitUsage
		}
		to := ""
		if len(pos) == 2 {
			to = pos[1]
		}
		return ctlRun(stdout, stderr, func(c *ctlClient) error {
			raw, err := c.call("vcs/diff", map[string]any{"from": pos[0], "to": to})
			if err != nil {
				return err
			}
			if sc.has("json") {
				fmt.Fprintln(stdout, string(raw))
				return nil
			}
			var payload struct {
				Files []struct {
					Path string `json:"path"`
					Op   string `json:"op"`
				} `json:"files"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				return err
			}
			for _, f := range payload.Files {
				fmt.Fprintf(stdout, "%-9s %s\n", f.Op, f.Path)
			}
			return nil
		})
	}
	fmt.Fprintf(stderr, "yanshi vcs: unknown verb %q\n", verb)
	return exitUsage
}

// runModels lists the models a session can switch to.
func runModelsList(args []string, stdout, stderr io.Writer) int {
	if wantHelp(args) {
		fmt.Fprint(stdout, ctlUsage)
		return exitOK
	}
	sc := newFlagScanner([]string{"json"}, nil)
	if ok, code := scanOrUsage(sc, args, "models", stdout, stderr); !ok {
		return code
	}
	return ctlRun(stdout, stderr, func(c *ctlClient) error {
		raw, err := c.call("models/list", nil)
		if err != nil {
			return err
		}
		if sc.has("json") {
			fmt.Fprintln(stdout, string(raw))
			return nil
		}
		var payload struct {
			Models []string `json:"models"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return err
		}
		for _, m := range payload.Models {
			fmt.Fprintln(stdout, m)
		}
		return nil
	})
}

// firstLine clips a commit message to its subject.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

var _ = time.Second
