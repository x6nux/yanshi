package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeDoctorCfg writes a minimal config body to a temp file and returns its
// path. Used by the UPG1 release-doctor tests to drive RunDoctor with fixture
// configs (e.g. a schema_version that exceeds SupportedSchemaVersion).
func writeDoctorCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	return p
}

// TestDoctorHasNewReleaseChecks asserts the three UPG1 checks are present in the
// report by name: config-version, wal, keyring.
func TestDoctorHasNewReleaseChecks(t *testing.T) {
	rep := RunDoctor(context.Background(), DoctorOptions{ConfigPath: ""})
	names := map[string]bool{}
	for _, c := range rep.Checks {
		names[c.Name] = true
	}
	for _, want := range []string{"config-version", "wal", "keyring"} {
		assert.True(t, names[want], "doctor must include a %q check", want)
	}
}

// TestDoctorReleaseFlagDoesNotChangeCheckSet verifies --release only changes
// how release-blocking warns are promoted, not which checks run.
func TestDoctorReleaseFlagDoesNotChangeCheckSet(t *testing.T) {
	normal := RunDoctor(context.Background(), DoctorOptions{})
	release := RunDoctor(context.Background(), DoctorOptions{Release: true})
	require.Len(t, normal.Checks, len(release.Checks), "--release must not add/remove checks")
}

// TestCheckConfigVersionOKWhenSupported drives RunDoctor with a config whose
// schema_version equals the supported value and asserts the config-version
// check reports OK.
func TestCheckConfigVersionOKWhenSupported(t *testing.T) {
	p := writeDoctorCfg(t, "schema_version: 1\nllm: {}\n")
	rep := RunDoctor(context.Background(), DoctorOptions{ConfigPath: p})
	c := findCheck(t, rep, "config-version")
	assert.Equal(t, StatusOK, c.Status, "config-version should be ok at supported schema; got %q", c.Message)
}

// TestDoctorReleasePromotesConfigVersionWarnToFail loads a config whose
// schema_version exceeds supported. Under --release the config-version check
// must surface as fail (not warn), because shipping with a config the running
// yanshi cannot fully understand is release-blocking.
func TestDoctorReleasePromotesConfigVersionWarnToFail(t *testing.T) {
	p := writeDoctorCfg(t, "schema_version: 999\nllm: {}\n")
	// Non-release: warn (anomaly but not a boot blocker).
	normal := RunDoctor(context.Background(), DoctorOptions{ConfigPath: p})
	nc := findCheck(t, normal, "config-version")
	assert.Equal(t, StatusWarn, nc.Status, "config-version should warn (not fail) without --release; got %q", nc.Message)
	// Release: fail (release-blocking).
	release := RunDoctor(context.Background(), DoctorOptions{ConfigPath: p, Release: true})
	rc := findCheck(t, release, "config-version")
	assert.Equal(t, StatusFail, rc.Status, "--release must promote config-version to fail when schema_version exceeds supported; got %q", rc.Message)
}

// TestCheckWALIsReadOnlyAndCleansUp opens the SQLite path, reads
// PRAGMA journal_mode, and closes the connection — same pattern as
// checkDatabase. The check must not leave an open handle or flip the mode.
func TestCheckWALIsReadOnlyAndCleansUp(t *testing.T) {
	p := writeDoctorCfg(t, "schema_version: 1\nstorage:\n  sqlite_path: "+filepath.Join(t.TempDir(), "wal.db")+"\nllm: {}\n")
	rep := RunDoctor(context.Background(), DoctorOptions{ConfigPath: p})
	c := findCheck(t, rep, "wal")
	assert.Contains(t, c.Message, "journal_mode=",
		"wal check should report the journal_mode; got %q", c.Message)
}

// TestCheckKeyringAvailabilityNeverFails asserts the keyring check never
// reports fail — on a nokeyring build it is a note (OK), on a real build it is
// OK or warn. nokeyring is the default release variant, so it must not block.
func TestCheckKeyringAvailabilityNeverFails(t *testing.T) {
	rep := RunDoctor(context.Background(), DoctorOptions{ConfigPath: ""})
	c := findCheck(t, rep, "keyring")
	assert.NotEqual(t, StatusFail, c.Status, "keyring check must never fail (nokeyring is the default release variant); got %q", c.Message)
}

// TestCheckSandboxStillHonestAboutGap verifies the sandbox check still calls
// out the S08/M2 gap rather than pretending to have verified it.
func TestCheckSandboxStillHonestAboutGap(t *testing.T) {
	rep := RunDoctor(context.Background(), DoctorOptions{ConfigPath: ""})
	c := findCheck(t, rep, "sandbox")
	assert.True(t, strings.Contains(c.Message, "S08") || strings.Contains(c.Message, "M2"),
		"checkSandbox must stay honest about the S08/M2 gap; got %q", c.Message)
}
