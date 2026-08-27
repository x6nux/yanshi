package cli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/lockfile"
)

// minimalConfig is a config that loads cleanly and carries every required
// top-level block, so a test can remove exactly the block it wants missing.
const minimalConfig = `schema_version: 1
server:
  http_addr: "127.0.0.1:8080"
storage:
  sqlite_path: "yanshi.db"
profiles:
  coding:
    tools:
      allow: ["fs_*"]
`

// minimalExample is the template doctor -fix copies missing blocks from.
const minimalExample = `schema_version: 1
# The HTTP listener. Comments belong to the block they document.
server:
  http_addr: "127.0.0.1:8080"
storage:
  sqlite_path: "yanshi.db"
profiles:
  coding:
    tools:
      allow: ["fs_*"]
llm:
  providers:
    - name: "openai"
      kind: "openai"
      model: "gpt-4o"
      api_key: "${OPENAI_API_KEY}"
`

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func outcomeFor(t *testing.T, r FixReport, action FixAction) FixOutcome {
	t.Helper()
	for _, o := range r.Outcomes {
		if o.Action == action {
			return o
		}
	}
	t.Fatalf("no outcome recorded for %q", action)
	return FixOutcome{}
}

// TestFixAllowlistExcludesCredentialsAndDatabase is the first gate stated as a
// test. The two categories named here are the ones a repair tool must never
// grow into: a wrong api_key authenticates against an account the operator did
// not intend, and a deleted database takes the session history, the VCS commits
// and the task ledger with it.
func TestFixAllowlistExcludesCredentialsAndDatabase(t *testing.T) {
	forbidden := []string{"key", "credential", "secret", "token", "password",
		"database", "db", "sqlite", "delete", "reset", "wipe"}
	for _, action := range FixActions() {
		name := strings.ToLower(string(action))
		for _, word := range forbidden {
			require.NotContains(t, name, word,
				"the fix allowlist must never grow a credential or database repair; %q looks like one", action)
		}
		require.NotEmpty(t, FixActionDescription(action),
			"every allowlisted action needs a description operators can read")
	}
	require.ElementsMatch(t,
		[]FixAction{FixCreateDirs, FixConfigDefaults, FixStaleLockfile, FixFilePermissions},
		FixActions(),
		"adding a repair must be a deliberate change to this assertion, not a silent one")
}

// TestRunDoctorFixRejectsUnknownAction proves a typo fails loudly. Silently
// ignoring an unrecognised name would let `-fix=creat-dirs` report success
// while doing nothing at all.
func TestRunDoctorFixRejectsUnknownAction(t *testing.T) {
	_, err := RunDoctorFix(context.Background(), FixOptions{
		Only: []FixAction{"rewrite-provider-key"},
	})
	require.ErrorIs(t, err, ErrUnknownFixAction)
	require.Contains(t, err.Error(), "rewrite-provider-key")
}

// TestNonInteractiveRefusesFileEditingFixes is the third gate: repairs that
// modify an existing file are refused when nobody is watching, while purely
// additive ones still run. The asymmetry is the point — a rewritten config in
// a container image diverges from the repository copy unseen, whereas a
// created directory cannot surprise anyone.
func TestNonInteractiveRefusesFileEditingFixes(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeFile(t, filepath.Join(dir, "config.yaml"), minimalConfig)
	writeFile(t, filepath.Join(dir, "config.example.yaml"), minimalExample)

	report, err := RunDoctorFix(context.Background(), FixOptions{
		ConfigPath:  cfgPath,
		ExamplePath: filepath.Join(dir, "config.example.yaml"),
		Root:        dir,
		Interactive: false,
	})
	require.NoError(t, err)

	for _, action := range []FixAction{FixConfigDefaults, FixFilePermissions} {
		o := outcomeFor(t, report, action)
		require.Equal(t, FixStatusRefused, o.Status,
			"%s modifies an existing file and must be refused without a terminal", action)
		require.Contains(t, o.Detail, "terminal")
	}
	for _, action := range []FixAction{FixCreateDirs, FixStaleLockfile} {
		o := outcomeFor(t, report, action)
		require.NotEqual(t, FixStatusRefused, o.Status,
			"%s is additive and must still run non-interactively", action)
	}
}

// TestFixConfigDefaultsBacksUpBeforeEditing is the second gate: the original
// file must exist, byte-for-byte, after the repair.
func TestFixConfigDefaultsBacksUpBeforeEditing(t *testing.T) {
	dir := t.TempDir()
	partial := "schema_version: 1\nserver:\n  http_addr: \"127.0.0.1:9999\"\n"
	cfgPath := writeFile(t, filepath.Join(dir, "config.yaml"), partial)
	examplePath := writeFile(t, filepath.Join(dir, "config.example.yaml"), minimalExample)

	report, err := RunDoctorFix(context.Background(), FixOptions{
		ConfigPath:  cfgPath,
		ExamplePath: examplePath,
		Root:        dir,
		Interactive: true,
		Only:        []FixAction{FixConfigDefaults},
	})
	require.NoError(t, err)

	o := outcomeFor(t, report, FixConfigDefaults)
	require.Equal(t, FixStatusFixed, o.Status, o.Detail)
	require.NotEmpty(t, o.Backup, "a file-modifying repair must take a backup first")

	original, err := os.ReadFile(o.Backup)
	require.NoError(t, err)
	require.Equal(t, partial, string(original),
		"the backup must be the pre-repair file, byte for byte")

	updated, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	require.Contains(t, string(updated), "storage:")
	require.Contains(t, string(updated), "profiles:")
	require.Contains(t, string(updated), "127.0.0.1:9999",
		"an operator's existing value must never be replaced by the template's")
}

// TestFixConfigDefaultsNeverWritesCredentials proves the repair does not
// materialise a provider block. A config that boots and fails at every request
// is worse than one that is visibly incomplete.
func TestFixConfigDefaultsNeverWritesCredentials(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeFile(t, filepath.Join(dir, "config.yaml"),
		"schema_version: 1\nserver:\n  http_addr: \"127.0.0.1:8080\"\n")
	examplePath := writeFile(t, filepath.Join(dir, "config.example.yaml"), minimalExample)

	_, err := RunDoctorFix(context.Background(), FixOptions{
		ConfigPath: cfgPath, ExamplePath: examplePath, Root: dir,
		Interactive: true, Only: []FixAction{FixConfigDefaults},
	})
	require.NoError(t, err)

	updated, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	require.NotContains(t, string(updated), "api_key",
		"doctor -fix must never write a credential field")
	require.NotContains(t, string(updated), "llm:",
		"llm.providers is deliberately not a required block")
}

// TestFixConfigDefaultsSkipsWhenComplete proves the repair is a no-op on a
// healthy config: no backup litter, no rewrite, no false "fixed".
func TestFixConfigDefaultsSkipsWhenComplete(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeFile(t, filepath.Join(dir, "config.yaml"), minimalConfig)
	examplePath := writeFile(t, filepath.Join(dir, "config.example.yaml"), minimalExample)

	report, err := RunDoctorFix(context.Background(), FixOptions{
		ConfigPath: cfgPath, ExamplePath: examplePath, Root: dir,
		Interactive: true, Only: []FixAction{FixConfigDefaults},
	})
	require.NoError(t, err)
	o := outcomeFor(t, report, FixConfigDefaults)
	require.Equal(t, FixStatusSkipped, o.Status)
	require.Empty(t, o.Backup)

	after, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	require.Equal(t, minimalConfig, string(after))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		require.NotContains(t, e.Name(), ".bak-",
			"a skipped repair must not leave a backup behind")
	}
}

// TestMissingTopLevelKeysIgnoresCommentedBlocks is the parsing case that
// matters: config.example.yaml ships blocks commented out, and treating those
// as present would leave the operator with the same missing block plus a
// doctor that claims it is there.
func TestMissingTopLevelKeysIgnoresCommentedBlocks(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		keys []string
		want []string
	}{
		{
			name: "commented key is not present",
			yaml: "# storage:\n#   sqlite_path: x\nserver:\n  http_addr: a\n",
			keys: []string{"server", "storage"},
			want: []string{"storage"},
		},
		{
			name: "indented key is not top level",
			yaml: "server:\n  storage: nested\n",
			keys: []string{"storage"},
			want: []string{"storage"},
		},
		{
			name: "list item is not a key",
			yaml: "- storage: item\n",
			keys: []string{"storage"},
			want: []string{"storage"},
		},
		{
			name: "all present yields nothing",
			yaml: minimalConfig,
			keys: []string{"server", "storage", "profiles"},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, missingTopLevelKeys(tc.yaml, tc.keys))
		})
	}
}

// TestExtractTopLevelBlockKeepsCommentsAndIndentedLines pins what a copied
// block looks like. The comments ARE the documentation in this repo, so a
// config assembled without them is materially worse than a hand-copied one.
func TestExtractTopLevelBlockKeepsCommentsAndIndentedLines(t *testing.T) {
	block := extractTopLevelBlock(minimalExample, "server")
	require.Contains(t, block, "# The HTTP listener.")
	require.Contains(t, block, "server:")
	require.Contains(t, block, "  http_addr:")
	require.NotContains(t, block, "storage:",
		"a block must stop at the next top-level key")

	require.Empty(t, extractTopLevelBlock(minimalExample, "no-such-key"))

	// A list-valued block must carry its items.
	llm := extractTopLevelBlock(minimalExample, "llm")
	require.Contains(t, llm, "providers:")
	require.Contains(t, llm, "- name: \"openai\"")
}

// TestFixStaleLockfileRemovesDeadOnly proves the repair distinguishes a dead
// owner from a live one. Removing a live lockfile would not stop the backend;
// it would only make every window unable to find it.
func TestFixStaleLockfileRemovesDeadOnly(t *testing.T) {
	t.Run("dead pid is removed", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "stale-proj")
		require.NoError(t, lockfile.Write(root, lockfile.Lockfile{
			PID: 999999, Addr: "127.0.0.1:1", Root: root,
		}))
		t.Cleanup(func() { _ = lockfile.Remove(root) })

		report, err := RunDoctorFix(context.Background(), FixOptions{
			Root: root, Interactive: true, Only: []FixAction{FixStaleLockfile},
		})
		require.NoError(t, err)
		require.Equal(t, FixStatusFixed, outcomeFor(t, report, FixStaleLockfile).Status)

		_, rerr := lockfile.Read(root)
		require.ErrorIs(t, rerr, lockfile.ErrNotFound)
	})

	t.Run("live pid is left alone", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "live-proj")
		require.NoError(t, lockfile.Write(root, lockfile.Lockfile{
			PID: os.Getpid(), Addr: "127.0.0.1:2", Root: root,
		}))
		t.Cleanup(func() { _ = lockfile.Remove(root) })

		report, err := RunDoctorFix(context.Background(), FixOptions{
			Root: root, Interactive: true, Only: []FixAction{FixStaleLockfile},
		})
		require.NoError(t, err)
		o := outcomeFor(t, report, FixStaleLockfile)
		require.Equal(t, FixStatusSkipped, o.Status)
		require.Contains(t, o.Detail, "running")

		_, rerr := lockfile.Read(root)
		require.NoError(t, rerr, "a live lockfile must survive the repair")
	})

	t.Run("absent lockfile is a no-op", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "empty-proj")
		report, err := RunDoctorFix(context.Background(), FixOptions{
			Root: root, Interactive: true, Only: []FixAction{FixStaleLockfile},
		})
		require.NoError(t, err)
		require.Equal(t, FixStatusSkipped, outcomeFor(t, report, FixStaleLockfile).Status)
	})
}

// TestFixCreateDirsIsAdditiveOnly proves the repair creates what is missing and
// never replaces what is there. Deleting a file to make room for a directory is
// exactly the "helpful" step that loses data.
func TestFixCreateDirsIsAdditiveOnly(t *testing.T) {
	dir := t.TempDir()
	userDir := filepath.Join(dir, "skills-user")
	worktrees := filepath.Join(dir, "worktrees")
	cfg := "schema_version: 1\nserver:\n  http_addr: \"127.0.0.1:8080\"\n" +
		"storage:\n  sqlite_path: \"yanshi.db\"\n" +
		"skills:\n  user_dir: \"" + filepath.ToSlash(userDir) + "\"\n" +
		"vcs:\n  worktree_dir: \"" + filepath.ToSlash(worktrees) + "\"\n"
	cfgPath := writeFile(t, filepath.Join(dir, "config.yaml"), cfg)

	report, err := RunDoctorFix(context.Background(), FixOptions{
		ConfigPath: cfgPath, Root: dir, Interactive: true,
		Only: []FixAction{FixCreateDirs},
	})
	require.NoError(t, err)
	o := outcomeFor(t, report, FixCreateDirs)
	require.Equal(t, FixStatusFixed, o.Status, o.Detail)
	for _, p := range []string{userDir, worktrees} {
		fi, serr := os.Stat(p)
		require.NoError(t, serr)
		require.True(t, fi.IsDir())
	}

	// A second run has nothing to do.
	report2, err := RunDoctorFix(context.Background(), FixOptions{
		ConfigPath: cfgPath, Root: dir, Interactive: true,
		Only: []FixAction{FixCreateDirs},
	})
	require.NoError(t, err)
	require.Equal(t, FixStatusSkipped, outcomeFor(t, report2, FixCreateDirs).Status)
}

// TestFixCreateDirsRefusesToReplaceAFile pins the destructive case it must not
// take: a configured directory path that already exists as a file is reported,
// never unlinked.
func TestFixCreateDirsRefusesToReplaceAFile(t *testing.T) {
	dir := t.TempDir()
	collision := filepath.Join(dir, "not-a-dir")
	writeFile(t, collision, "important data")
	cfg := "schema_version: 1\nserver:\n  http_addr: \"127.0.0.1:8080\"\n" +
		"storage:\n  sqlite_path: \"yanshi.db\"\n" +
		"skills:\n  user_dir: \"" + filepath.ToSlash(collision) + "\"\n"
	cfgPath := writeFile(t, filepath.Join(dir, "config.yaml"), cfg)

	report, err := RunDoctorFix(context.Background(), FixOptions{
		ConfigPath: cfgPath, Root: dir, Interactive: true,
		Only: []FixAction{FixCreateDirs},
	})
	require.NoError(t, err)
	o := outcomeFor(t, report, FixCreateDirs)
	require.Equal(t, FixStatusFailed, o.Status)
	require.Contains(t, o.Detail, "not replaced")

	data, rerr := os.ReadFile(collision)
	require.NoError(t, rerr)
	require.Equal(t, "important data", string(data))
	require.Equal(t, 2, report.ExitCode(), "an attempted repair that failed must be visible in the exit code")
}

// TestFixFilePermissionsOnlyNarrows proves the mode repair removes bits and
// never adds them. A repair that could widen a mode would be privilege
// escalation with a friendly name.
func TestFixFilePermissionsOnlyNarrows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	cfgPath := writeFile(t, filepath.Join(dir, "config.yaml"), minimalConfig)
	require.NoError(t, os.Chmod(cfgPath, 0o644))

	report, err := RunDoctorFix(context.Background(), FixOptions{
		ConfigPath: cfgPath, Root: dir, Interactive: true,
		Only: []FixAction{FixFilePermissions},
	})
	require.NoError(t, err)
	require.Equal(t, FixStatusFixed, outcomeFor(t, report, FixFilePermissions).Status)

	fi, err := os.Stat(cfgPath)
	require.NoError(t, err)
	require.Equal(t, credentialFileMode, fi.Mode().Perm())

	// Already-tight modes are left exactly as they are, not widened to 0600
	// from something stricter.
	require.NoError(t, os.Chmod(cfgPath, 0o400))
	report2, err := RunDoctorFix(context.Background(), FixOptions{
		ConfigPath: cfgPath, Root: dir, Interactive: true,
		Only: []FixAction{FixFilePermissions},
	})
	require.NoError(t, err)
	require.Equal(t, FixStatusSkipped, outcomeFor(t, report2, FixFilePermissions).Status)
	fi2, err := os.Stat(cfgPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o400), fi2.Mode().Perm(),
		"a stricter mode must not be loosened to the target")
}

// TestDryRunTouchesNothing proves the plan mode is a plan: it reports what it
// would do and leaves every byte on disk alone.
func TestDryRunTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	partial := "schema_version: 1\nserver:\n  http_addr: \"127.0.0.1:8080\"\n"
	cfgPath := writeFile(t, filepath.Join(dir, "config.yaml"), partial)
	examplePath := writeFile(t, filepath.Join(dir, "config.example.yaml"), minimalExample)
	root := filepath.Join(dir, "proj")
	require.NoError(t, lockfile.Write(root, lockfile.Lockfile{PID: 999999, Root: root}))
	t.Cleanup(func() { _ = lockfile.Remove(root) })

	report, err := RunDoctorFix(context.Background(), FixOptions{
		ConfigPath: cfgPath, ExamplePath: examplePath, Root: root,
		Interactive: true, DryRun: true,
	})
	require.NoError(t, err)
	require.True(t, report.DryRun)
	require.True(t, report.Changed(), "a dry run still reports what it would change")

	after, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	require.Equal(t, partial, string(after), "a dry run must not edit the config")
	_, rerr := lockfile.Read(root)
	require.NoError(t, rerr, "a dry run must not remove the lockfile")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		require.NotContains(t, e.Name(), ".bak-", "a dry run must not write backups")
	}
}

// TestFixReportRendering covers the operator-facing surface: the exit code
// contract and the text shape.
func TestFixReportRendering(t *testing.T) {
	r := FixReport{DryRun: true, Outcomes: []FixOutcome{
		{Action: FixCreateDirs, Status: FixStatusFixed, Detail: "made a dir"},
		{Action: FixConfigDefaults, Status: FixStatusRefused, Detail: "no terminal"},
		{Action: FixStaleLockfile, Status: FixStatusSkipped, Detail: "nothing to do"},
	}}
	var sb strings.Builder
	r.RenderText(&sb)
	out := sb.String()
	require.Contains(t, out, "dry run")
	require.Contains(t, out, "[FIXED]")
	require.Contains(t, out, "[REFUSED]")
	require.True(t, r.Changed())
	require.Zero(t, r.ExitCode(), "a refusal is the gate working, not a failure")

	withBackup := FixReport{Outcomes: []FixOutcome{
		{Action: FixConfigDefaults, Status: FixStatusFixed, Detail: "added storage", Backup: "/tmp/c.bak"},
		{Action: FixFilePermissions, Status: FixStatusFailed, Detail: "chmod refused"},
	}}
	sb.Reset()
	withBackup.RenderText(&sb)
	require.Contains(t, sb.String(), "backup: /tmp/c.bak")
	require.Equal(t, 2, withBackup.ExitCode())

	require.False(t, FixReport{}.Changed())
	require.Zero(t, FixReport{}.ExitCode())
}

// TestStdinIsTerminal covers the TTY probe used to set Interactive in
// production. A pipe, a regular file, /dev/null and a nil handle must all read
// as "no human is watching".
//
// The /dev/null case is the one that matters and the one that caught a real
// bug. An earlier version of this probe checked os.ModeCharDevice, and
// /dev/null IS a character device -- so `yanshi doctor -fix < /dev/null`, the
// exact shape of a CI job, a container entrypoint and a systemd unit, was
// classified as interactive. The gate passed in every environment it exists to
// refuse.
func TestStdinIsTerminal(t *testing.T) {
	require.False(t, StdinIsTerminal(nil))

	f := writeFile(t, filepath.Join(t.TempDir(), "plain"), "x")
	fh, err := os.Open(f)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fh.Close() })
	require.False(t, StdinIsTerminal(fh), "a regular file is not a terminal")

	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	require.False(t, StdinIsTerminal(r), "a pipe is not a terminal")

	devNull, err := os.Open(os.DevNull)
	require.NoError(t, err)
	t.Cleanup(func() { _ = devNull.Close() })
	require.False(t, StdinIsTerminal(devNull),
		"/dev/null is a CHARACTER DEVICE but not a terminal; classifying it as "+
			"interactive lets every CI job and container entrypoint through the gate")

	require.NoError(t, fh.Close())
	require.False(t, StdinIsTerminal(fh), "a closed handle must not panic")
}

// TestDefaultExamplePath covers the template lookup: next to the config when
// one is there, otherwise the working-directory default.
func TestDefaultExamplePath(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.Equal(t, "config.example.yaml", DefaultExamplePath(cfgPath),
		"with no template beside the config, fall back to the working directory")

	example := writeFile(t, filepath.Join(dir, "config.example.yaml"), minimalExample)
	require.Equal(t, example, DefaultExamplePath(cfgPath))

	require.Equal(t, "config.example.yaml", DefaultExamplePath("config.yaml"))
}

// TestFixConfigDefaultsSkipsWithoutTemplate proves a missing template is a
// skip, not a failure: there is nothing wrong with the system, the repair just
// has no source of defaults.
func TestFixConfigDefaultsSkipsWithoutTemplate(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeFile(t, filepath.Join(dir, "config.yaml"),
		"schema_version: 1\nserver:\n  http_addr: \"127.0.0.1:8080\"\n")

	report, err := RunDoctorFix(context.Background(), FixOptions{
		ConfigPath: cfgPath, ExamplePath: filepath.Join(dir, "absent.yaml"),
		Root: dir, Interactive: true, Only: []FixAction{FixConfigDefaults},
	})
	require.NoError(t, err)
	o := outcomeFor(t, report, FixConfigDefaults)
	require.Equal(t, FixStatusSkipped, o.Status)
	// The detail must name the path the operator asked for. Asserting the
	// specific words of the OS error ("not found") pinned a phrasing rather
	// than a behaviour, and broke when template resolution moved to
	// LoadExampleTemplate — while the behaviour under test (an explicit
	// -template that cannot be read is a skip, never a silent fall back to the
	// compiled-in copy) never changed.
	require.Contains(t, o.Detail, "absent.yaml")
	require.Contains(t, o.Detail, "nothing to copy defaults from")
	// The decisive half: it did NOT quietly use the embedded template.
	require.NotContains(t, o.Detail, "built-in")

	// An absent CONFIG is likewise a skip pointing at `yanshi init`.
	report2, err := RunDoctorFix(context.Background(), FixOptions{
		ConfigPath:  filepath.Join(dir, "nope.yaml"),
		ExamplePath: writeFile(t, filepath.Join(dir, "config.example.yaml"), minimalExample),
		Root:        dir, Interactive: true, Only: []FixAction{FixConfigDefaults},
	})
	require.NoError(t, err)
	require.Contains(t, outcomeFor(t, report2, FixConfigDefaults).Detail, "yanshi init")
}

// TestBackupFilePreservesModeAndNeverOverwrites pins the backup helper: a
// second repair must not overwrite the one copy the operator wants back, and
// the copy must not be widened by the process umask.
func TestBackupFilePreservesModeAndNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "config.yaml"), "one")
	require.NoError(t, os.Chmod(path, 0o600))

	first, err := backupFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("two"), 0o600))
	second, err := backupFile(path)
	require.NoError(t, err)
	require.NotEqual(t, first, second, "a second backup must not clobber the first")

	firstData, err := os.ReadFile(first)
	require.NoError(t, err)
	require.Equal(t, "one", string(firstData))

	if runtime.GOOS != "windows" {
		fi, serr := os.Stat(first)
		require.NoError(t, serr)
		require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	}

	_, err = backupFile(filepath.Join(dir, "absent"))
	require.Error(t, err)
}
