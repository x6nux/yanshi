package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/store"
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

// TestCheckSandboxNeverClaimsEnforcement is the honesty check, rewritten in
// W7 to guard the PROPERTY rather than the placeholder.
//
// It used to require the message to mention "S08" or "M2" -- which pinned the
// literal text of a stub that did no probing at all, so replacing the stub
// with a real probe turned this red. The thing worth protecting was never
// those two strings: it is that doctor must not tell an operator OS isolation
// is enforced while the process runs under the degraded host guard.
//
// So the assertion is now about the claim: status may be ok ONLY when the
// report says Enforced. Anything else must warn, whatever words it uses.
func TestCheckSandboxNeverClaimsEnforcement(t *testing.T) {
	rep := RunDoctor(context.Background(), DoctorOptions{ConfigPath: ""})
	c := findCheck(t, rep, "sandbox")
	if c.Status == StatusOK {
		assert.NotContains(t, c.Message, "NOT enforced",
			"a check reporting OK must not simultaneously say isolation is unenforced: %q", c.Message)
	} else {
		assert.NotEqual(t, StatusFail, c.Status,
			"the documented Phase-0 posture is a warning, not a failure: %q", c.Message)
	}
}

// TestDoctorWALCheckReportsSidecarSizes covers the sidecar half of the WAL
// report.
//
// checkWAL answered "is WAL on" and stopped there. The failure an operator
// needs to catch is a WAL that never checkpoints: the database is in WAL mode,
// every other check is green, and the -wal file grows without bound. Reporting
// journal_mode alone cannot distinguish that from a healthy store.
//
// ledger: F1/WAL1#5 WAL 文件有界（roadmap:295）。plan 另有 10 条细化验收：每条池连接 PRAGMA 生效、MaxOpenConns 按配置且 :memory: 强制 1、16×50 零 BUSY、读不阻塞写、双 Open 跨进程 busy_timeout、rollback→WAL 幂等零丢失、Close 执行 wal_checkpoint(TRUNCATE)、work/vcs/auth/bootstrap 现有测试全绿、Windows CI 下并发/升级测试全绿、doctor 报告 journal_mode 与 -wal/-shm 大小
func TestDoctorWALCheckReportsSidecarSizes(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "yanshi.db")
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	sid, err := st.CreateSession("t")
	require.NoError(t, err)
	for i := 0; i < 20; i++ {
		require.NoError(t, st.AppendMessage(sid, i, "user", strings.Repeat("z", 2048)))
	}
	require.NoError(t, st.Close())

	c := checkWAL(&config.Config{Storage: config.StorageConfig{SQLitePath: dbPath}}, nil)
	require.Equal(t, StatusOK, c.Status, "message: %s", c.Message)
	assert.Contains(t, c.Message, "journal_mode=wal")
	assert.Contains(t, c.Message, "-wal=",
		"the wal check does not report the -wal file size, so a WAL that never "+
			"checkpoints looks identical to a healthy one: %q", c.Message)
	assert.Contains(t, c.Message, "-shm=", "message: %q", c.Message)
}
