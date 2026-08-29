package execbroker

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ShimExitCode is what the shim exits with when the elevation is refused or
// cannot be adjudicated.
//
// 126 is the POSIX shell convention for "command found but not executable",
// which is the closest existing meaning: the program is right there on PATH and
// deliberately was not run. Reusing it means `sudo …; echo $?` inside a script
// produces a code shells already document, rather than a number only this
// project knows.
const ShimExitCode = 126

// IsShimInvocation reports whether this process was started through one of the
// shim symlinks, by looking at the name it was invoked under.
//
// argv[0]'s basename is the only signal available — the shims are symlinks to
// the same binary, so there is nothing else to distinguish them — and that is
// acceptable precisely because the shim path is fail-closed: a yanshi binary
// someone renamed to "sudo" by accident does not gain a capability, it loses
// one and says so.
func IsShimInvocation(argv0 string) (string, bool) {
	name := filepath.Base(argv0)
	for _, p := range InterceptedPrograms {
		if name == p {
			return name, true
		}
	}
	return "", false
}

// RunShim is the whole child half: ask the broker, and on approval replace this
// process with the real program.
//
// It returns an error on every path that does not exec. There is no branch that
// runs the program without an answer, and adding one would make removing an
// environment variable a way to disable the control.
//
// On success it does not return: syscall.Exec replaces the process image, so
// the pid, the exit status and the signal behaviour the caller observes are the
// real program's. Running it as a child instead would leave a wrapper in the
// tree, break `sudo`'s own terminal handling, and give the script an exit code
// from the wrapper rather than from the command.
func RunShim(name string, argv []string, dir string) error {
	sock := os.Getenv(SocketEnv)
	token := os.Getenv(TokenEnv)
	shimDir := os.Getenv(ShimDirEnv)
	if sock == "" || token == "" || shimDir == "" {
		return fmt.Errorf(
			"%s was invoked through yanshi's elevation shim, but no broker is reachable "+
				"(%s/%s/%s are not all set); refusing to run it",
			name, SocketEnv, TokenEnv, ShimDirEnv)
	}
	resp, err := ask(sock, Request{
		Token:   token,
		Program: name,
		Args:    argv[1:],
		Dir:     dir,
	})
	if err != nil {
		return fmt.Errorf("%s: cannot reach yanshi to approve this elevation: %w", name, err)
	}
	if !resp.Allow {
		reason := resp.Reason
		if reason == "" {
			reason = "denied"
		}
		return fmt.Errorf("%s: refused by yanshi: %s", name, reason)
	}
	real, err := resolveOutsideShimDir(name, os.Getenv("PATH"), shimDir)
	if err != nil {
		return err
	}
	// The environment is passed through UNCHANGED, shim directory still on
	// PATH. That is not an oversight: an approved `sudo make install` runs a
	// make that may itself invoke sudo, and dropping the shims here would make
	// one approval silently cover everything below it.
	return syscall.Exec(real, append([]string{real}, argv[1:]...), os.Environ())
}

// ask performs the one request/response round trip.
//
// The dial has a timeout because a broker whose socket file survived its
// process would otherwise hang the child forever. The READ deliberately does
// not: the parent may be waiting on a human, and a deadline here would turn
// "the operator went to get coffee" into a denial. The connection closing is
// what ends the wait, and it closes when the parent goes away — which is the
// event a timeout was standing in for.
func ask(sock string, req Request) (Response, error) {
	conn, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	raw, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return Response{}, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return Response{}, fmt.Errorf("no verdict from the broker: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Response{}, fmt.Errorf("unreadable verdict from the broker: %w", err)
	}
	return resp, nil
}

// resolveOutsideShimDir finds the real program, skipping the shim directory.
//
// Skipping by directory rather than by "not a symlink to me" is what keeps this
// terminating: the shim is on PATH ahead of the real program precisely so it is
// found first, and any resolution that could pick it again is an exec loop that
// forks until the process table fills.
//
// Every entry is compared after filepath.Clean so that "/tmp/x" and "/tmp/x/"
// are the same directory. An entry that is the shim dir under a different
// symlinked path would slip through — the loop guard is the child's own
// PATH, and there is no defence here against a caller that deliberately aims
// the shim at itself.
func resolveOutsideShimDir(name, path, shimDir string) (string, error) {
	shimDir = filepath.Clean(shimDir)
	for _, entry := range strings.Split(path, string(os.PathListSeparator)) {
		if entry == "" || filepath.Clean(entry) == shimDir {
			continue
		}
		candidate := filepath.Join(entry, name)
		if isExecutableFile(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf(
		"%s: approved, but no real %s was found on PATH outside yanshi's shim directory", name, name)
}

// isExecutableFile reports whether p is a regular file with an execute bit.
func isExecutableFile(p string) bool {
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&fs.FileMode(0o111) != 0
}
