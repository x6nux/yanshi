package secrets

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

const maxSecretInputBytes = 64 * 1024

// isTerminal and readPasswordFromTerminal are seams over golang.org/x/term so
// the interactive (echo-off) password branch of Run is exercisable without a
// real console. The defaults delegate to term.IsTerminal / term.ReadPassword
// exactly, preserving production behavior; tests swap them to drive the TTY
// path. This mirrors the replaceEncryptedFile / newKeyringStore seams.
var (
	isTerminal               = term.IsTerminal
	readPasswordFromTerminal = func(fd int) ([]byte, error) { return term.ReadPassword(fd) }
)

// AuthCommand implements the `yanshi auth` subcommand. It supports three
// modes: interactive (prompt for key with echo off), --api-key-stdin (read
// raw bytes from stdin), and --delete (remove the stored entry). All modes
// target exactly ONE (provider, account) pair — the CLI forbids bulk
// operations to prevent accidental leak.
type AuthCommand struct {
	Provider      string
	Account       string
	Backend       string
	FilePath      string
	PassphraseEnv string
	Passphrase    []byte // runtime-only; takes precedence over PassphraseEnv
	APIKeyStdin   bool
	Delete        bool

	// Manager is optional. Production runAuthSub injects its already-open
	// process Manager so set/delete share the same FileStore snapshot and
	// Redactor. nil preserves standalone package tests.
	Manager *Manager
	Stdin   io.Reader
	Stdout  io.Writer
}

// Run executes the command. Returns an error on any failure; the caller is
// expected to print it and exit non-zero. Cleanup branches (--delete on a
// missing entry) still execute fully and return the underlying store error
// (per structural fix #13: cleanup failure path must be reachable & correct).
func (c *AuthCommand) Run() error {
	if c.Provider == "" || c.Account == "" {
		return fmt.Errorf("auth: --provider and --account are required")
	}
	if c.Stdin == nil {
		c.Stdin = os.Stdin
	}
	if c.Stdout == nil {
		c.Stdout = os.Stdout
	}

	mgr := c.Manager
	ownsManager := false
	if mgr == nil {
		var err error
		mgr, err = NewManager(Config{
			Backend:       c.Backend,
			FilePath:      c.FilePath,
			PassphraseEnv: c.PassphraseEnv,
			Passphrase:    c.Passphrase,
		})
		if err != nil {
			return err
		}
		ownsManager = true
	}
	if ownsManager {
		defer mgr.Close()
	}

	if c.Delete {
		if err := mgr.Delete(c.Provider, c.Account); err != nil {
			return fmt.Errorf("auth: delete %s/%s: %w", c.Provider, c.Account, err)
		}
		fmt.Fprintf(c.Stdout, "deleted %s/%s\n", c.Provider, c.Account)
		return nil
	}

	var key string
	if c.APIKeyStdin {
		raw, err := readLimitedSecret(c.Stdin)
		if err != nil {
			return fmt.Errorf("auth: read stdin: %w", err)
		}
		key = strings.TrimRight(string(raw), "\r\n")
	} else {
		fmt.Fprintf(c.Stdout, "Enter API key for %s/%s: ", c.Provider, c.Account)
		// Only a real injected *os.File may use term.ReadPassword. Consulting
		// os.Stdin here would ignore AuthCommand.Stdin and make buffer/pipe
		// tests read from the test runner's terminal.
		inputFile, isFile := c.Stdin.(*os.File)
		if !isFile || !isTerminal(int(inputFile.Fd())) {
			line, err := readLine(c.Stdin)
			if err != nil {
				return fmt.Errorf("auth: read stdin line: %w", err)
			}
			key = strings.TrimRight(line, "\r\n")
		} else {
			b, err := readPasswordFromTerminal(int(inputFile.Fd()))
			if err != nil {
				return fmt.Errorf("auth: read password: %w", err)
			}
			fmt.Fprintln(c.Stdout)
			if len(b) > maxSecretInputBytes {
				return fmt.Errorf("auth: secret input exceeds %d bytes", maxSecretInputBytes)
			}
			key = string(b)
		}
	}

	if key == "" {
		return fmt.Errorf("auth: empty API key refused")
	}
	if err := mgr.Set(c.Provider, c.Account, key); err != nil {
		return fmt.Errorf("auth: set %s/%s: %w", c.Provider, c.Account, err)
	}
	fmt.Fprintf(c.Stdout, "stored %s/%s\n", c.Provider, c.Account)
	return nil
}

func readLimitedSecret(r io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxSecretInputBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxSecretInputBytes {
		return nil, fmt.Errorf("secret input exceeds %d bytes", maxSecretInputBytes)
	}
	return raw, nil
}

func readLine(r io.Reader) (string, error) {
	var buf strings.Builder
	b := make([]byte, 1)
	for {
		if _, err := r.Read(b); err != nil {
			if err == io.EOF && buf.Len() > 0 {
				return buf.String(), nil
			}
			return "", err
		}
		if b[0] == '\n' {
			return buf.String(), nil
		}
		if buf.Len() >= maxSecretInputBytes {
			return "", fmt.Errorf("secret input exceeds %d bytes", maxSecretInputBytes)
		}
		buf.WriteByte(b[0])
	}
}
