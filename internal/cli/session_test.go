package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	apihttp "github.com/x6nux/yanshi/internal/api/http"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/lockfile"
)

// toYAMLPath converts an OS-native path to a YAML-safe string (forward slashes).
func toYAMLPath(p string) string { return strings.ReplaceAll(p, "\\", "/") }

// writeTestConfig writes a minimal config (SQLite under dir) and returns its
// path. Bootstrapping an in-process backend needs a real config so config.Load
// and store.Open succeed; FakeModel=true means no providers/keys are required.
func writeTestConfig(t *testing.T, dir string) string {
	t.Helper()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	cfg := "storage:\n  sqlite_path: \"" + dbPath + "\"\n"
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o644))
	return cfgPath
}

// TestResolve_ConnectsToLiveRemote proves: when a live backend is recorded in
// the lockfile, Resolve connects to it (Mode ws) and does NOT bootstrap.
func TestResolve_ConnectsToLiveRemote(t *testing.T) {
	o, _ := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"r"}, nil)})
	s := apihttp.New(apihttp.Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	s.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	root := t.TempDir()
	require.NoError(t, lockfile.Write(root, lockfile.Lockfile{
		PID: os.Getpid(), Addr: ts.Listener.Addr().String(), Auth: "none", Root: root,
	}))

	sess := newSession(root, "", true) // fakeModel=true (unused: we connect remote)
	require.NoError(t, sess.Resolve(context.Background()))
	defer sess.Close()

	assert.False(t, sess.IsOwner(), "live remote must NOT make us the owner")
	assert.Equal(t, "ws", sess.Backend().Mode())
}

// TestReconnect_OwnerDied_NewClientBecomesOwner simulates owner death: a stale
// lockfile (dead PID, addr not listening) with no live healthz -> the
// reconnecting session bootstraps a new in-process backend and becomes the
// owner. A subsequent Reconnect is idempotent because the session already owns
// a live backend.
func TestReconnect_OwnerDied_NewClientBecomesOwner(t *testing.T) {
	root := t.TempDir()
	// Stale lockfile: dead PID, no server actually listening on :9.
	require.NoError(t, lockfile.Write(root, lockfile.Lockfile{
		PID: 1<<31 - 1, Addr: "127.0.0.1:9", Auth: "none", Root: root,
	}))

	sess := newSession(root, writeTestConfig(t, t.TempDir()), true)
	require.NoError(t, sess.Resolve(context.Background()))
	defer sess.Close()

	assert.True(t, sess.IsOwner(), "stale owner -> we bootstrap and become owner")
	assert.Equal(t, "ws", sess.Backend().Mode())

	// Re-resolve is idempotent when already owner+live.
	require.NoError(t, sess.Reconnect(context.Background()))
}

// TestBootstrapOwner_ForwardsSelfHealToBootstrap is the plumbing guard between
// cli.Options.SelfHeal and bootstrap.Build.
//
// The AST assertion in internal/bootstrap can see that runTUI writes
// `opts.SelfHeal = true`, but not whether that value survives the trip through
// NewSession and bootstrapOwner. Delete the one assignment in NewSession and
// the AST test still passes, the TUI still compiles, and healing is silently
// dead on the exact entry point it exists for — measured, that mutation left
// every other package green.
//
// Both directions are asserted, because the default carries the safety property
// (exec and headless share this code path and must not quarantine anything).
func TestBootstrapOwner_ForwardsSelfHealToBootstrap(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selfHeal bool
		wantBoot bool
	}{
		{"TUI opts in and boots", true, true},
		{"exec and headless keep the default and fail loudly", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			cfgPath := writeTestConfig(t, root)
			dbPath := filepath.Join(root, "test.db")

			// SQLITE_NOTADB: a header that is not SQLite's.
			junk := make([]byte, 8192)
			for i := range junk {
				junk[i] = byte(i*31 + 7)
			}
			require.NoError(t, os.WriteFile(dbPath, junk, 0o600))

			sess := NewSession(Options{
				Root: root, ConfigPath: cfgPath, FakeModel: true,
				InProcess: true, SelfHeal: tc.selfHeal,
			})
			err := sess.Resolve(context.Background())
			if sess != nil {
				t.Cleanup(func() { _ = sess.Close() })
			}

			entries, rerr := os.ReadDir(root)
			require.NoError(t, rerr)
			quarantined := false
			for _, e := range entries {
				if strings.Contains(e.Name(), ".corrupt-") {
					quarantined = true
				}
			}

			if tc.wantBoot {
				require.NoError(t, err, "with SelfHeal the TUI must come up on a corrupt database")
				assert.True(t, quarantined, "the corrupt database must have been moved aside")
				return
			}
			require.Error(t, err, "without SelfHeal a corrupt database must stop the boot")
			assert.False(t, quarantined, "a non-owning entry point must not quarantine anything")
			after, aerr := os.ReadFile(dbPath)
			require.NoError(t, aerr)
			assert.Equal(t, junk, after, "the database must be left exactly as found")
		})
	}
}
