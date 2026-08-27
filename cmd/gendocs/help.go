// Package main: cmd/gendocs CLI help-snapshot generator.
//
// The -help-all mode captures `yanshi <subcmd> -h` output for every dispatched
// subcommand and rewrites the matching <!-- BEGIN GENERATED: help:<id> -->
// blocks in the target doc files. Captured help is fenced in ```text so the
// committed snapshot mirrors what a user actually sees, and CI gates it via
// `git diff --exit-code`.
//
// Subcommand list vs dispatch: yanshiSubcommands is the hand-maintained list of
// every dispatched subcommand. gendocs_test.go asserts it stays in sync with
// cmd/yanshi/main.go's switch cases so a newly added subcommand cannot land
// without a help snapshot entry (and vice versa).
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/x6nux/yanshi/internal/docgen"
)

// yanshiSubcommands lists every dispatched subcommand in cmd/yanshi/main.go,
// including the managed (auth/doctor) and bare (yanshi) entries. Order is the
// display order for snapshots. This MUST stay in sync with main.go's dispatch;
// gendocs_test.go enforces that.
var yanshiSubcommands = []string{
	"yanshi", "serve", "chat", "exec", "app", "goal", "vcs-mcp", "init", "daemon", "schedule", "provider", "acp", "pr", "auth", "doctor",
}

// helpCapturer returns the captured help text (combined stdout+stderr) for one
// subcommand. It is a var so tests can inject a fake without spawning a build.
var helpCapturer = captureHelpLive

// ensureBinary resolves the cached yanshi binary path. It is a var so tests can
// inject a failing resolver or a path to a non-existent binary, exercising the
// error branches of captureHelpLive (build failure, silent spawn) without
// inducing OS-level failures.
var ensureBinary = ensureYanshiBinary

// mkHelpWorkdir creates the isolated temp directory captureHelpLive runs the
// subcommand in. It is a var so tests can inject a failing creator.
var mkHelpWorkdir = func() (string, error) {
	return os.MkdirTemp("", "gendocs-help-*")
}

// RenderHelp wraps captured help output in a ```text fence. Trailing whitespace
// is trimmed so the block is stable regardless of how the subcommand padded its
// output.
func RenderHelp(id, output string) string {
	return "```text\n" + strings.TrimRight(output, "\n") + "\n```"
}

// captureHelpLive builds the yanshi binary once (cached) and runs the right
// invocation for subcmd in an isolated temp directory. auth needs a loadable
// config to reach its usage printer, so a minimal config is materialised for it.
func captureHelpLive(subcmd string) (string, error) {
	bin, err := ensureBinary()
	if err != nil {
		return "", err
	}
	workdir, err := mkHelpWorkdir()
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(workdir)

	args := helpArgs(subcmd, workdir)
	cmd := exec.Command(bin, args...)
	cmd.Dir = workdir
	cmd.Stdin = strings.NewReader("") // close stdin so vcs-mcp's Serve returns on EOF
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		// A non-zero exit is expected for subcommands whose -h path returns a
		// usage exit code (e.g. flag.ErrHelp). We keep whatever was printed; the
		// output is what matters, not the exit code.
		if out.Len() == 0 {
			return "", fmt.Errorf("yanshi %s: %w", subcmd, err)
		}
	}
	return strings.TrimSpace(out.String()), nil
}

// helpArgs returns the yanshi argv tail that produces help/usage for subcmd.
// Most subcommands honour -h directly; auth reaches its usage printer only after
// a successful config load, so a minimal config is synthesised in workdir.
func helpArgs(subcmd, workdir string) []string {
	switch subcmd {
	case "yanshi":
		return []string{"-h"}
	case "auth":
		cfg := filepath.Join(workdir, "config.yaml")
		_ = os.WriteFile(cfg, []byte("server:\n  http_addr: \"127.0.0.1:0\"\nstorage:\n  sqlite_path: \":memory:\"\n"), 0o644)
		return []string{"--config", cfg, "auth"}
	default:
		return []string{subcmd, "-h"}
	}
}

// yanshiBinaryPath caches the built yanshi binary so -help-all does not rebuild
// per subcommand. It is reset per process (the generator is short-lived).
var yanshiBinaryPath string

func ensureYanshiBinary() (string, error) {
	if yanshiBinaryPath != "" {
		return yanshiBinaryPath, nil
	}
	dir, err := os.MkdirTemp("", "gendocs-bin-*")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "yanshi.exe")
	// Use the package import path (not ./cmd/yanshi) so the build resolves
	// regardless of the caller's working directory — the generator is also
	// exercised by tests whose cwd is cmd/gendocs, not the module root.
	cmd := exec.Command("go", "build", "-o", bin, "github.com/x6nux/yanshi/cmd/yanshi")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("build yanshi: %w: %s", err, stderr.String())
	}
	yanshiBinaryPath = bin
	return bin, nil
}

// helpBlockID returns the BEGIN/END GENERATED id for a subcommand's snapshot.
func helpBlockID(subcmd string) string { return "help:" + subcmd }

// writeAllHelpSnapshots captures every subcommand's help and rewrites the
// help:<id> blocks in each of files. A file only receives blocks for the
// subcommands whose markers it already carries, so tui.md (help:yanshi) is not
// polluted with entrypoints.md's subcommand set.
func writeAllHelpSnapshots(files []string) error {
	for _, sub := range yanshiSubcommands {
		output, err := helpCapturer(sub)
		if err != nil {
			return fmt.Errorf("capture help for %s: %w", sub, err)
		}
		content := RenderHelp(helpBlockID(sub), output)
		for _, path := range files {
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			if !strings.Contains(string(data), "<!-- BEGIN GENERATED: "+helpBlockID(sub)+" -->") {
				continue
			}
			if err := docgen.RewriteBlock(path, helpBlockID(sub), content); err != nil {
				return err
			}
		}
	}
	return nil
}
