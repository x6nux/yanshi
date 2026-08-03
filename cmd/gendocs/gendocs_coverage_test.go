package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/testutil"
)

// ---- pure helpers --------------------------------------------------------

// TestParentDir covers the separator scan (both '/' and os.PathSeparator) and
// the no-separator "." fallback.
func TestParentDir(t *testing.T) {
	assert.Equal(t, "a/b", parentDir("a/b/c.md"))
	assert.Equal(t, "a", parentDir(filepath.Join("a", "c.md")))
	assert.Equal(t, ".", parentDir("c.md"))
	assert.Equal(t, "", parentDir("/x"))
}

// TestHelpArgs covers every branch of the helpArgs switch.
func TestHelpArgs(t *testing.T) {
	assert.Equal(t, []string{"-h"}, helpArgs("yanshi", t.TempDir()))

	workdir := t.TempDir()
	got := helpArgs("auth", workdir)
	assert.Contains(t, got[0], "--config")
	// auth synthesises a config file in workdir.
	_, err := os.Stat(filepath.Join(workdir, "config.yaml"))
	require.NoError(t, err)

	assert.Equal(t, []string{"serve", "-h"}, helpArgs("serve", t.TempDir()))
}

// TestHelpBlockIDPrefix proves the id carries the help: prefix.
func TestHelpBlockIDPrefix(t *testing.T) {
	assert.Equal(t, "help:serve", helpBlockID("serve"))
}

// ---- writeConfigSkeleton -------------------------------------------------

// TestWriteConfigSkeletonCreatesFile proves writeConfigSkeleton creates a new
// file (and parent dir) with the config-skeleton block.
func TestWriteConfigSkeletonCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "configuration.md")
	require.NoError(t, writeConfigSkeleton(path))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "<!-- BEGIN GENERATED: config-skeleton -->")
	assert.Contains(t, string(data), "### server")
}

// TestWriteConfigSkeletonRewritesExisting proves writeConfigSkeleton rewrites an
// existing block in place (idempotent, surrounding prose preserved).
func TestWriteConfigSkeletonRewritesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configuration.md")
	seed := "intro\n\n<!-- BEGIN GENERATED: config-skeleton -->\nold\n<!-- END GENERATED: config-skeleton -->\n\ntail\n"
	require.NoError(t, os.WriteFile(path, []byte(seed), 0o644))
	require.NoError(t, writeConfigSkeleton(path))
	data, _ := os.ReadFile(path)
	body := string(data)
	assert.Contains(t, body, "intro")
	assert.Contains(t, body, "tail")
	assert.False(t, strings.Contains(body, "\nold\n"))
}

// ---- runGendocs (main core) ----------------------------------------------

// TestRunGendocsConfig proves the -config path dispatches to
// writeConfigSkeleton and returns 0.
func TestRunGendocsConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configuration.md")
	var errOut bytes.Buffer
	code := runGendocs([]string{"-config", path}, &errOut)
	require.Equal(t, 0, code, "errOut=%s", errOut.String())
	_, err := os.Stat(path)
	require.NoError(t, err)
}

// TestRunGendocsConfigError proves a writeConfigSkeleton failure (path under a
// file, so MkdirAll fails) maps to exit 1.
func TestRunGendocsConfigError(t *testing.T) {
	dir := t.TempDir()
	// Make the parent "dir" a regular file so MkdirAll under it fails.
	parent := filepath.Join(dir, "blocker")
	require.NoError(t, os.WriteFile(parent, []byte("x"), 0o644))
	target := filepath.Join(parent, "nested", "configuration.md")
	var errOut bytes.Buffer
	code := runGendocs([]string{"-config", target}, &errOut)
	assert.Equal(t, 1, code)
}

// TestRunGendocsNoArgs proves the default branch prints usage and returns 2.
func TestRunGendocsNoArgs(t *testing.T) {
	var errOut bytes.Buffer
	code := runGendocs(nil, &errOut)
	assert.Equal(t, 2, code)
}

// TestRunGendocsBadFlag proves a flag parse error returns 2.
func TestRunGendocsBadFlag(t *testing.T) {
	var errOut bytes.Buffer
	code := runGendocs([]string{"-nope"}, &errOut)
	assert.Equal(t, 2, code)
}

// TestRunGendocsHelpAll proves the -help-all path dispatches to
// writeAllHelpSnapshots (with an injected fake capturer) and returns 0.
func TestRunGendocsHelpAll(t *testing.T) {
	prev := helpCapturer
	helpCapturer = func(subcmd string) (string, error) { return "FAKE " + subcmd, nil }
	t.Cleanup(func() { helpCapturer = prev })

	path := filepath.Join(t.TempDir(), "entrypoints.md")
	seed := "intro\n\n<!-- BEGIN GENERATED: help:serve -->\nold\n<!-- END GENERATED: help:serve -->\n"
	require.NoError(t, os.WriteFile(path, []byte(seed), 0o644))

	var errOut bytes.Buffer
	code := runGendocs([]string{"-help-all", path}, &errOut)
	require.Equal(t, 0, code, "errOut=%s", errOut.String())
	data, _ := os.ReadFile(path)
	assert.Contains(t, string(data), "FAKE serve")
}

// TestRunGendocsHelpAllError proves a capture failure maps to exit 1.
func TestRunGendocsHelpAllError(t *testing.T) {
	prev := helpCapturer
	helpCapturer = func(subcmd string) (string, error) {
		return "", assertNotzeroErr()
	}
	t.Cleanup(func() { helpCapturer = prev })

	var errOut bytes.Buffer
	code := runGendocs([]string{"-help-all", filepath.Join(t.TempDir(), "x.md")}, &errOut)
	assert.Equal(t, 1, code)
}

// assertNotzeroErr returns a non-nil error for the helpAll-error test.
func assertNotzeroErr() error {
	return &gendocTestErr{"capture failed"}
}

type gendocTestErr struct{ msg string }

func (e *gendocTestErr) Error() string { return e.msg }

// ---- live build/spawn paths (dev-time tooling) ---------------------------

// TestEnsureYanshiBinaryBuilds covers ensureYanshiBinary: it builds the yanshi
// binary once, caches the path, and returns it on subsequent calls. This is the
// real `go build` path the -help-all generator relies on.
func TestEnsureYanshiBinaryBuilds(t *testing.T) {
	// Reset the process cache so the test exercises a real build.
	yanshiBinaryPath = ""
	bin, err := ensureYanshiBinary()
	require.NoError(t, err)
	assert.NotEmpty(t, bin)
	assert.FileExists(t, bin)
	// Second call returns the cached path without rebuilding.
	bin2, err := ensureYanshiBinary()
	require.NoError(t, err)
	assert.Equal(t, bin, bin2)
}

// TestCaptureHelpLiveYanshi covers captureHelpLive for the bare `yanshi`
// subcommand (runs `-h`, which prints usage and exits). This is the live spawn
// path; it reuses the binary cached by TestEnsureYanshiBinaryBuilds when run
// after it (and builds it fresh otherwise).
func TestCaptureHelpLiveYanshi(t *testing.T) {
	out, err := captureHelpLive("yanshi")
	require.NoError(t, err)
	assert.Contains(t, out, "yanshi")
}

// TestCaptureHelpLiveEnsureBinaryError covers the ensureBinary-error branch of
// captureHelpLive via the injection seam.
func TestCaptureHelpLiveEnsureBinaryError(t *testing.T) {
	prev := ensureBinary
	ensureBinary = func() (string, error) { return "", &gendocTestErr{"build failed"} }
	t.Cleanup(func() { ensureBinary = prev })
	_, err := captureHelpLive("serve")
	require.Error(t, err)
}

// TestCaptureHelpLiveSilentSpawn covers the empty-output error branch: a binary
// path that does not exist makes cmd.Run error with no captured output.
func TestCaptureHelpLiveSilentSpawn(t *testing.T) {
	prev := ensureBinary
	ensureBinary = func() (string, error) { return filepath.Join(t.TempDir(), "no-such-binary.exe"), nil }
	t.Cleanup(func() { ensureBinary = prev })
	_, err := captureHelpLive("serve")
	require.Error(t, err)
}

// TestCaptureHelpLiveWorkdirError covers the workdir-creation error branch via
// the injection seam.
func TestCaptureHelpLiveWorkdirError(t *testing.T) {
	prevBin := ensureBinary
	prevWd := mkHelpWorkdir
	ensureBinary = func() (string, error) { return "bin", nil }
	mkHelpWorkdir = func() (string, error) { return "", &gendocTestErr{"no workdir"} }
	t.Cleanup(func() { ensureBinary = prevBin; mkHelpWorkdir = prevWd })
	_, err := captureHelpLive("serve")
	require.Error(t, err)
}

// TestWriteAllHelpSnapshotsRewriteBlockError covers the docgen.RewriteBlock
// error branch inside writeAllHelpSnapshots: the file is read-only after being
// seeded with the BEGIN/END markers, so ReadFile succeeds but WriteFile fails.
func TestWriteAllHelpSnapshotsRewriteBlockError(t *testing.T) {
	prev := helpCapturer
	helpCapturer = func(subcmd string) (string, error) { return "x", nil }
	t.Cleanup(func() { helpCapturer = prev })

	path := filepath.Join(t.TempDir(), "entrypoints.md")
	seed := "<!-- BEGIN GENERATED: help:yanshi -->\nold\n<!-- END GENERATED: help:yanshi -->\n"
	require.NoError(t, os.WriteFile(path, []byte(seed), 0o644))
	// Make the file read-only so docgen.RewriteBlock's os.WriteFile fails. The
	// cleanup restores write permission so t.TempDir's removal can delete it.
	testutil.SkipIfRoot(t) // root bypasses the 0444 guard below
	require.NoError(t, os.Chmod(path, 0o444))
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	err := writeAllHelpSnapshots([]string{path})
	require.Error(t, err)
}

// TestEnsureYanshiBinaryMkdirError covers the os.MkdirTemp error branch of
// ensureYanshiBinary. TMPDIR is pointed at a path that already exists as a
// regular file, which makes MkdirTemp fail. The original TMPDIR is restored on
// cleanup.
func TestEnsureYanshiBinaryMkdirError(t *testing.T) {
	// Skip on plan9/other platforms where TMPDIR isn't honoured the same way.
	if testing.Short() {
		t.Skip("skipping env-mutation test in -short mode")
	}
	// Save and restore both TMP and TMPDIR (Windows uses TMP; POSIX uses
	// TMPDIR).
	prevTMP, hadTMP := os.LookupEnv("TMP")
	prevTMPDIR, hadTMPDIR := os.LookupEnv("TMPDIR")
	t.Cleanup(func() {
		if hadTMP {
			_ = os.Setenv("TMP", prevTMP)
		} else {
			_ = os.Unsetenv("TMP")
		}
		if hadTMPDIR {
			_ = os.Setenv("TMPDIR", prevTMPDIR)
		} else {
			_ = os.Unsetenv("TMPDIR")
		}
	})
	// TMPDIR/TMP as a regular file makes MkdirTemp fail.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o644))
	_ = os.Setenv("TMP", blocker)
	_ = os.Setenv("TMPDIR", blocker)

	// Reset the binary cache so ensureYanshiBinary reaches the MkdirTemp call.
	yanshiBinaryPath = ""
	t.Cleanup(func() { yanshiBinaryPath = "" })
	_, err := ensureYanshiBinary()
	require.Error(t, err)
}
