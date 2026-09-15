package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/x6nux/yanshi/internal/ctl"
)

// sessionUsage is the help for `yanshi session`, printed on -h and on a bad
// verb. Kept next to the code that implements it so the two cannot drift.
const sessionUsage = `usage: yanshi session <verb> [args] [-config FILE] [-json]

  list   [-archived] [-limit N]   stored sessions, newest first
  show   <id> [-tail N]           one session: metadata, token ledger, last turns
  rename <id> <title>             set the session title
  fork   <id> [-upto N]           copy a session (N = stop after that seq; default all)
  archive <id> | unarchive <id>   hide / restore a session
  delete <id> yes                 delete a session and its messages

Every verb takes -config FILE (default config.yaml) and, where it prints a
result, -json for one machine-readable object instead of text.

These are the operations the TUI exposes as /sessions, /rename, /archive,
/unarchive, /archived and /delete. They run OFFLINE against the project's
SQLite file (no daemon required), which is what makes them usable in a script
and usable when the backend is wedged — the same reason yanshi doctor never
needs it either.
`

// sessionFlags are the flags every verb shares.
type sessionFlags struct {
	config   string
	jsonOut  bool
	archived bool
	limit    int
	tail     int
	upto     int
}

// runSession implements `yanshi session`, the offline session-management
// surface: the CLI half of what the TUI does with /sessions, /rename,
// /archive and /delete.
//
// Why offline rather than over the daemon's control frames: the TUI already has
// those frames, but a script that wants to list sessions should not need a live
// backend, and the operations themselves are pure store calls. The counter-case
// — a session with an ACTIVE turn being renamed out from under it — is not a
// correctness problem (the store serialises writes) and the daemon picks up the
// new title on its next read.
func runSession(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, sessionUsage)
		return exitUsage
	}
	verb := args[0]
	if verb == "-h" || verb == "--help" || verb == "help" {
		fmt.Fprint(stdout, sessionUsage)
		return exitOK
	}

	// Flags are accepted BEFORE or AFTER the positional arguments, so both
	// `yanshi session show <id> -json` and `yanshi session show -json <id>`
	// work. flag.FlagSet cannot do that: it stops at the first non-flag
	// argument, so the first form silently parsed the id as a positional and
	// then rejected -json as an unknown positional — measured, and the reason
	// this is a hand-rolled scan rather than a FlagSet.
	sf, rest, perr := parseSessionArgs(args[1:])
	if perr != nil {
		fmt.Fprintf(stderr, "yanshi session %s: %v\n", verb, perr)
		fmt.Fprint(stderr, sessionUsage)
		return exitUsage
	}

	switch verb {
	case "list":
		return sessionList(sf, stdout, stderr)
	case "show":
		if len(rest) != 1 {
			fmt.Fprint(stderr, sessionUsage)
			return exitUsage
		}
		return sessionShow(sf, rest[0], stdout, stderr)
	case "fork":
		if len(rest) != 1 {
			fmt.Fprint(stderr, sessionUsage)
			return exitUsage
		}
		return sessionFork(sf, rest[0], stdout, stderr)
	case "rename":
		if len(rest) < 2 {
			fmt.Fprint(stderr, sessionUsage)
			return exitUsage
		}
		return sessionMutate(sf, "rename", rest[0], strings.Join(rest[1:], " "), stdout, stderr)
	case "archive", "unarchive":
		if len(rest) != 1 {
			fmt.Fprint(stderr, sessionUsage)
			return exitUsage
		}
		return sessionMutate(sf, verb, rest[0], "", stdout, stderr)
	case "delete":
		// The TUI requires a literal "yes" (/delete <id> yes). Mirrored here
		// rather than replaced by -force: the same muscle memory should work in
		// both places, and a destructive verb that runs on a bare id is exactly
		// the shape a mistyped session id survives.
		if len(rest) != 2 || rest[1] != "yes" {
			fmt.Fprint(stderr, sessionUsage)
			fmt.Fprintln(stderr, "session delete requires the literal word: yanshi session delete <id> yes")
			return exitUsage
		}
		return sessionMutate(sf, "delete", rest[0], "", stdout, stderr)
	default:
		fmt.Fprintf(stderr, "yanshi session: unknown verb %q\n", verb)
		fmt.Fprint(stderr, sessionUsage)
		return exitUsage
	}
}

// openSessionStore resolves the config and opens the project database. Both
// failures are reported with the path that caused them, because a script's two
// realistic problems here are "wrong -config" and "wrong working directory",
// and a bare "no such table" tells the operator neither.
// openSessionStore is openCLICtl: `yanshi session` reads the store through the
// shared control plane so the CLI, the TUI and the JSON-RPC surface cannot
// disagree about what a session looks like.
func openSessionStore(configPath string) (*ctl.Service, error) {
	return openCLICtl(configPath)
}

// sessionRow is an alias for the control plane's view, kept so the rendering
// below reads the same as before the refactor.
type sessionRow = ctl.SessionView

/*
type sessionRow struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Archived  bool    `json:"archived"`
	UpdatedAt string  `json:"updatedAt"`
	Model     string  `json:"model,omitempty"`
	Thinking  string  `json:"thinking,omitempty"`
	Turns     int     `json:"turns"`
	TokensIn  int     `json:"tokensIn"`
	TokensOut int     `json:"tokensOut"`
	CachedIn  int     `json:"cachedTokens,omitempty"`
	Reasoning int     `json:"reasoningTokens,omitempty"`
	CostUSD   float64 `json:"costUsd,omitempty"`
	CostKnown bool    `json:"costKnown"`
	Messages  int     `json:"messages,omitempty"`
}

*/

func sessionList(sf sessionFlags, stdout, stderr io.Writer) int {
	st, err := openSessionStore(sf.config)
	if err != nil {
		fmt.Fprintf(stderr, "yanshi session: %v\n", err)
		return exitErr
	}
	defer st.Close()
	rows, err := st.Sessions(sf.limit, sf.archived)
	if err != nil {
		fmt.Fprintf(stderr, "yanshi session list: %v\n", err)
		return exitErr
	}
	out := rows
	if sf.jsonOut {
		return writeSessionJSON(stdout, stderr, map[string]any{"sessions": out})
	}
	if len(out) == 0 {
		fmt.Fprintln(stdout, "no sessions")
		return exitOK
	}
	for _, r := range out {
		title := r.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(stdout, "%s  %s  turns=%d tokens=%d/%d  %s\n",
			r.ID, title, r.Turns, r.TokensIn, r.TokensOut, r.UpdatedAt)
	}
	return exitOK
}

func sessionShow(sf sessionFlags, id string, stdout, stderr io.Writer) int {
	st, err := openSessionStore(sf.config)
	if err != nil {
		fmt.Fprintf(stderr, "yanshi session: %v\n", err)
		return exitErr
	}
	defer st.Close()
	row, err := st.Session(id)
	if err != nil {
		fmt.Fprintf(stderr, "yanshi session show %s: %v\n", id, err)
		return exitErr
	}

	tail := []map[string]any{}
	if sf.tail > 0 {
		msgs, merr := st.SessionMessages(id, sf.tail)
		if merr != nil {
			fmt.Fprintf(stderr, "yanshi session show: %v\n", merr)
			return exitErr
		}
		for _, m := range msgs {
			tail = append(tail, map[string]any{
				"seq": m.Seq, "role": m.Role, "content": clip(m.Content, 400),
			})
		}
	}
	if sf.jsonOut {
		return writeSessionJSON(stdout, stderr, map[string]any{"session": row, "tail": tail})
	}
	fmt.Fprintf(stdout, "id:        %s\n", row.ID)
	fmt.Fprintf(stdout, "title:     %s\n", row.Title)
	fmt.Fprintf(stdout, "archived:  %v\n", row.Archived)
	fmt.Fprintf(stdout, "updated:   %s\n", row.UpdatedAt)
	fmt.Fprintf(stdout, "model:     %s\n", row.Model)
	fmt.Fprintf(stdout, "turns:     %d\n", row.Turns)
	fmt.Fprintf(stdout, "tokens:    in=%d out=%d cached=%d reasoning=%d\n",
		row.TokensIn, row.TokensOut, row.CachedIn, row.Reasoning)
	if row.CostKnown {
		fmt.Fprintf(stdout, "cost:      $%.4f\n", row.CostUSD)
	} else {
		fmt.Fprintln(stdout, "cost:      N/A (no pricing entry for every provider used)")
	}
	fmt.Fprintf(stdout, "messages:  %d\n", row.Messages)
	for _, m := range tail {
		fmt.Fprintf(stdout, "  [%v %v] %v\n", m["seq"], m["role"], m["content"])
	}
	return exitOK
}

// sessionFork copies a session and prints the new id.
//
// The new id goes to STDOUT and nothing else does, so `id=$(yanshi session fork
// "$s" -json | jq -r .forkedId)` and the plain-text form both work.
func sessionFork(sf sessionFlags, id string, stdout, stderr io.Writer) int {
	st, err := openSessionStore(sf.config)
	if err != nil {
		fmt.Fprintf(stderr, "yanshi session: %v\n", err)
		return exitErr
	}
	defer st.Close()
	newID, err := st.ForkSession(id, sf.upto)
	if err != nil {
		fmt.Fprintf(stderr, "yanshi session fork %s: %v\n", id, err)
		return exitErr
	}
	if sf.jsonOut {
		return writeSessionJSON(stdout, stderr, map[string]any{"ok": true, "id": id, "forkedId": newID})
	}
	fmt.Fprintln(stdout, newID)
	return exitOK
}

func sessionMutate(sf sessionFlags, verb, id, arg string, stdout, stderr io.Writer) int {
	st, err := openSessionStore(sf.config)
	if err != nil {
		fmt.Fprintf(stderr, "yanshi session: %v\n", err)
		return exitErr
	}
	defer st.Close()
	switch verb {
	case "rename":
		err = st.RenameSession(id, arg)
	case "archive":
		err = st.SetSessionArchived(id, true)
	case "unarchive":
		err = st.SetSessionArchived(id, false)
	case "delete":
		err = st.DeleteSession(id)
	}
	if err != nil {
		fmt.Fprintf(stderr, "yanshi session %s %s: %v\n", verb, id, err)
		return exitErr
	}
	if sf.jsonOut {
		return writeSessionJSON(stdout, stderr, map[string]any{"ok": true, "verb": verb, "id": id})
	}
	fmt.Fprintf(stdout, "%s: %s\n", verb, id)
	return exitOK
}

// clip shortens a preview and marks that it was shortened, so a reader never
// mistakes a truncated message for the whole one.
func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func writeSessionJSON(w io.Writer, stderr io.Writer, v any) int {
	enc, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(stderr, "yanshi session: marshal: %v\n", err)
		return exitErr
	}
	fmt.Fprintln(w, string(enc))
	return exitOK
}

// parseSessionArgs splits `yanshi session` arguments into flags and positionals
// in one pass, so flags may appear on either side of the positional arguments.
//
// Recognised flags: -config FILE, -json, -archived, -limit N, -tail N. A value
// may be attached (-config=x) or separate (-config x). Anything else is an
// error rather than being ignored: a silently dropped -json is the difference
// between a script parsing output and the same script burning a turn on
// unparseable text.
func parseSessionArgs(args []string) (sessionFlags, []string, error) {
	sc := newFlagScanner([]string{"json", "archived"}, []string{"config", "limit", "tail", "upto"})
	if err := sc.parse(args); err != nil {
		return sessionFlags{}, nil, err
	}
	sf := sessionFlags{
		config:   sc.value("config", "config.yaml"),
		jsonOut:  sc.has("json"),
		archived: sc.has("archived"),
	}
	var err error
	if sf.limit, err = sc.intValue("limit", 20); err != nil {
		return sessionFlags{}, nil, err
	}
	if sf.tail, err = sc.intValue("tail", 5); err != nil {
		return sessionFlags{}, nil, err
	}
	// -1 keeps the whole session, matching store.ForkSession's convention.
	if sf.upto, err = sc.intValue("upto", -1); err != nil {
		return sessionFlags{}, nil, err
	}
	return sf, sc.positionals(), nil
}
