package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/store"
)

// enqueueUsage is printed for -h and for a malformed invocation.
const enqueueUsage = `Usage: yanshi enqueue [-config FILE] <session-id> <message...>
       yanshi enqueue [-config FILE] -list <session-id>

Queue a user message for a session, whether or not anything is connected to it.
The message is stored in the project database and delivered the next time that
session is resumed (yanshi exec/chat -resume <session-id>).

  -list   show what is waiting for a session without consuming it

The message may be given as several arguments; they are joined with spaces.
`

// runEnqueue implements `yanshi enqueue`.
//
// IT OPENS THE STORE DIRECTLY rather than going through bootstrap.Build, the
// same way `yanshi goal -history` does: queueing a message touches one row and
// has no reason to need a model provider, a VCS or an HTTP listener. It also
// has to work while a backend for this project is already running, which is the
// whole point — the database is the rendezvous, and SQLite's WAL plus
// busy_timeout is what makes a second process safe.
//
// store.Open (not OpenWith) so SelfHeal stays off: this is an incidental
// reader, and an incidental reader has no business quarantining somebody's
// history.
func runEnqueue(args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("enqueue", flag.ContinueOnError)
	fs.SetOutput(stdout)
	configPath := fs.String("config", "config.yaml", "path to configuration file")
	list := fs.Bool("list", false, "show queued messages without consuming them")
	fs.Usage = func() { fmt.Fprint(stdout, enqueueUsage) }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	if len(rest) == 0 || (!*list && len(rest) < 2) {
		fmt.Fprint(os.Stderr, enqueueUsage)
		return exitUsage
	}
	sessionID := rest[0]

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yanshi enqueue: config: %v\n", err)
		return exitErr
	}
	st, err := store.Open(cfg.Storage.SQLitePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yanshi enqueue: open store: %v\n", err)
		return exitErr
	}
	defer st.Close()

	if *list {
		pending, err := st.PendingQueuedMessages(sessionID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "yanshi enqueue: %v\n", err)
			return exitErr
		}
		for _, m := range pending {
			fmt.Fprintln(stdout, m)
		}
		fmt.Fprintf(stdout, "%d message(s) queued for %s\n", len(pending), sessionID)
		return exitOK
	}

	n, err := st.EnqueueMessage(sessionID, strings.Join(rest[1:], " "))
	if err != nil {
		fmt.Fprintf(os.Stderr, "yanshi enqueue: %v\n", err)
		return exitErr
	}
	fmt.Fprintf(stdout, "queued; %d message(s) waiting for %s\n", n, sessionID)
	return exitOK
}
