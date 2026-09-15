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

// ipcUsage is the help for `yanshi ipc`.
const ipcUsage = `usage: yanshi ipc [-root DIR] [-config FILE]
       yanshi ipc <method> [-params JSON] [-root DIR] [-config FILE]

  (no method)   Bridge stdio to the daemon: newline-delimited JSON-RPC in on
                stdin, responses and item/updated notifications out on stdout.
                This is the same protocol "yanshi app" speaks on stdio; the
                only difference is that the server is the project's RUNNING
                daemon instead of a process you just started.
  <method>      Send one request and print its result (or the error), then
                exit. -params takes the JSON params object.

The socket lives in the per-user cache directory next to the daemon's lockfile,
is created 0600, and needs no token: the filesystem is the access control. Start
a daemon with "yanshi -b" if nothing is listening.
`

// runIPC implements `yanshi ipc` — the client half of the local socket IPC.
//
// It exists so a local tool has ONE way to drive a running daemon: connect to a
// filesystem-protected socket, speak the protocol the app-server already
// documents, and never learn an ephemeral port or a bearer token. The HTTP port
// remains the transport for remote and browser clients; this is the local one,
// and the difference is the access control, not the feature set.
func runIPC(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	sc := newFlagScanner(nil, []string{"root", "params", "timeout"})
	if err := sc.parse(args); err != nil {
		if err == errHelpRequested {
			fmt.Fprint(stdout, ipcUsage)
			return exitOK
		}
		fmt.Fprintf(stderr, "yanshi ipc: %v\n", err)
		fmt.Fprint(stderr, ipcUsage)
		return exitUsage
	}
	if len(sc.positionals()) > 1 {
		fmt.Fprint(stderr, ipcUsage)
		return exitUsage
	}
	rootArg := sc.value("root", "")
	paramsArg := sc.value("params", "")
	timeoutArg := sc.value("timeout", "")
	rest := sc.positionals()
	if timeoutArg != "" {
		if _, terr := time.ParseDuration(timeoutArg); terr != nil {
			fmt.Fprintf(stderr, "yanshi ipc: bad -timeout %q: %v\n", timeoutArg, terr)
			return exitUsage
		}
	}
	// Validate -params BEFORE dialing: a malformed parameter is a usage error,
	// and reporting it as "no daemon listening" (or as a dial failure) sends the
	// user looking at the wrong problem.
	if strings.TrimSpace(paramsArg) != "" && !json.Valid([]byte(paramsArg)) {
		fmt.Fprintf(stderr, "yanshi ipc: -params is not valid JSON: %s\n", paramsArg)
		return exitUsage
	}

	if rootArg == "" {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "yanshi ipc: %v\n", err)
			return exitErr
		}
		rootArg = wd
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if timeoutArg != "" {
		d, _ := time.ParseDuration(timeoutArg) // validated above
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	conn, err := ipc.Dial(ctx, rootArg)
	if err != nil {
		fmt.Fprintf(stderr, "yanshi ipc: %v\n", err)
		fmt.Fprintln(stderr, "yanshi ipc: is a daemon running for this project? start one with `yanshi -b`")
		return exitErr
	}
	defer conn.Close()

	if len(rest) == 1 {
		return ipcCall(ctx, conn, rest[0], paramsArg, stdout, stderr)
	}
	return ipcBridge(ctx, conn, stdin, stdout, stderr)
}

// ipcBridge copies lines in both directions until either side finishes.
//
// Two goroutines with an error channel rather than a select over both: the
// protocol is symmetric (a client may send while notifications arrive), and the
// first direction to end is what ends the session — stdin EOF means the caller
// is done, a socket EOF means the daemon went away, and both should stop the
// other direction instead of leaving it blocked on a read that will never
// return.
func ipcBridge(ctx context.Context, conn net.Conn, stdin io.Reader, stdout, stderr io.Writer) int {
	inDone := make(chan error, 1)
	outDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, stdin)
		// Half-close so the server sees EOF and can finish in-flight work;
		// closing the whole connection here would cut a turn's notifications.
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		inDone <- err
	}()
	go func() {
		_, err := io.Copy(stdout, conn)
		outDone <- err
	}()

	select {
	case err := <-outDone:
		// The daemon closed first: nothing left to send.
		return bridgeExit(ctx, err, stderr)
	case <-inDone:
		// stdin is done. This is NOT the end of the session: the responses to
		// what was just sent are still coming, and returning here is how the
		// first version of this bridge dropped them — `printf '…' | yanshi ipc`
		// exited 0 having printed nothing. Wait for the server to answer and
		// close, bounded so a wedged daemon cannot hold a script forever.
		select {
		case err := <-outDone:
			return bridgeExit(ctx, err, stderr)
		case <-time.After(bridgeDrainTimeout):
			fmt.Fprintf(stderr, "yanshi ipc: still waiting for the daemon after %s; closing\n", bridgeDrainTimeout)
			return exitOK
		case <-ctx.Done():
			return exitOK
		}
	case <-ctx.Done():
		return exitOK
	}
}

// bridgeDrainTimeout bounds how long the bridge keeps reading after stdin ends.
// Generous: a turn started over the bridge can legitimately stream for minutes,
// and the timeout only exists to stop a script from hanging on a daemon that
// accepted the request and then went silent.
const bridgeDrainTimeout = 10 * time.Minute

// bridgeExit maps a copy error to an exit code without reporting the benign
// "the other side hung up" class as a failure.
func bridgeExit(ctx context.Context, err error, stderr io.Writer) int {
	if err != nil && ctx.Err() == nil && !isClosedErr(err) {
		fmt.Fprintf(stderr, "yanshi ipc: %v\n", err)
		return exitErr
	}
	return exitOK
}

// ipcCall sends one request and prints the matching response.
//
// It matches on ID rather than taking the first line: the daemon may be
// streaming item/updated notifications for ANOTHER client's turn (each
// connection has its own server, but a shared agent service can still emit
// anything), and returning a notification as if it were the answer is the kind
// of bug a one-shot caller would never notice until the JSON stopped parsing.
func ipcCall(ctx context.Context, conn net.Conn, method, params string, stdout, stderr io.Writer) int {
	var raw json.RawMessage
	if strings.TrimSpace(params) != "" {
		raw = json.RawMessage(params)
		if !json.Valid(raw) {
			fmt.Fprintf(stderr, "yanshi ipc: -params is not valid JSON: %s\n", params)
			return exitUsage
		}
	}
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if raw != nil {
		req["params"] = raw
	}
	line, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintf(stderr, "yanshi ipc: %v\n", err)
		return exitErr
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		fmt.Fprintf(stderr, "yanshi ipc: write: %v\n", err)
		return exitErr
	}

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var resp struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int             `json:"code"`
				Message string          `json:"message"`
				Data    json.RawMessage `json:"data"`
			} `json:"error"`
		}
		if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
			// Not JSON we understand: pass it through rather than swallow it,
			// so a protocol drift is visible at the client boundary.
			fmt.Fprintln(stdout, string(sc.Bytes()))
			continue
		}
		if len(resp.ID) == 0 {
			// A notification (item/updated). Print it: a one-shot caller that
			// asked for turn/start needs the stream, not just the ack.
			fmt.Fprintln(stdout, string(sc.Bytes()))
			continue
		}
		if resp.Error != nil {
			// The server names the method in its message; repeating it here reads
			// as a nested failure ("jobs/read: jobs/read: …").
			fmt.Fprintf(stderr, "yanshi ipc: %s\n", resp.Error.Message)
			return exitErr
		}
		if len(resp.Result) > 0 {
			fmt.Fprintln(stdout, string(resp.Result))
		}
		return exitOK
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(stderr, "yanshi ipc: read: %v\n", err)
		return exitErr
	}
	fmt.Fprintln(stderr, "yanshi ipc: connection closed before a response arrived")
	return exitErr
}

// isClosedErr reports whether err is the benign "the other side hung up" class
// that ends a bridge without being worth an error message.
func isClosedErr(err error) bool {
	if err == nil || err == io.EOF {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "use of closed network connection") ||
		strings.Contains(s, "connection reset by peer")
}
