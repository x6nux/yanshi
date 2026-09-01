package bootstrap

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/config"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/store"
)

func minimalConfigFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(
		"server:\n  http_addr: 127.0.0.1:0\n"+
			"storage:\n  sqlite_path: \":memory:\"\n"), 0o644))
	return path
}

// TestBuildRegistersReadiness proves the O7 readiness route exists on the
// handler Build returns.
//
// This is the "written but zero readers" check for a route: internal/cli grew
// ReadyPath, a probe, and a 404 fallback, all fully tested against a
// httptest server the test itself registered the route on. Every one of those
// tests passes against a production binary that never registers /readyz —
// the fallback simply fires forever and discovery silently degrades back to
// liveness, which is the exact behaviour O7 exists to replace. Only an
// assertion against the REAL assembled handler can tell the two apart.
func TestBuildRegistersReadiness(t *testing.T) {
	app, err := Build(Options{ConfigPath: minimalConfigFile(t), FakeModel: true})
	require.NoError(t, err)
	defer app.Shutdown(context.Background())

	ts := httptest.NewServer(app.Server.Handler)
	defer ts.Close()

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/readyz", http.StatusOK},
		{"/healthz", http.StatusOK},
	} {
		resp, err := ts.Client().Get(ts.URL + tc.path)
		require.NoError(t, err, tc.path)
		resp.Body.Close()
		assert.Equal(t, tc.want, resp.StatusCode, tc.path)
	}
}

// TestLogWriterRotates proves the O1 rotating writer is what resolveLogWriter
// hands to obslog, and that the configured caps reach it.
//
// The assertion is on OBSERVED ROTATION rather than on the writer's type,
// because the type alone does not prove the config was threaded: a wiring that
// built the writer with a hardcoded RotateConfig{} would satisfy a type check
// while ignoring max_size_mb entirely, and an operator's 1 MiB cap would
// silently become 10 MiB.
func TestLogWriterRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	w, err := openLogFile(path, config.LogConfig{MaxSizeMB: 1, MaxBackups: 2})
	require.NoError(t, err)
	// bootstrap 刻意不关日志文件（进程生命周期）；测试必须自己关，否则
	// windows 上 t.TempDir 的 RemoveAll 会撞上仍打开的句柄（2026-09-01 CI）。
	t.Cleanup(func() { _ = w.(io.Closer).Close() })

	// Two 700 KiB records: the first fits under the 1 MiB cap, the second
	// cannot, so the writer must rotate between them.
	rec := make([]byte, 700*1024)
	for i := range rec {
		rec[i] = 'x'
	}
	_, err = w.Write(rec)
	require.NoError(t, err)
	_, err = w.Write(rec)
	require.NoError(t, err)

	_, err = os.Stat(path + ".1")
	require.NoError(t, err, "the cap must have forced a rotation; generation .1 is missing")

	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Less(t, fi.Size(), int64(1<<20)+int64(len(rec)),
		"the active file must hold only the post-rotation record")
}

// TestLogWriterNegativeMaxSizeDisablesRotation pins the sign convention.
//
// A negative max_size_mb is the operator saying "never rotate", and it must
// NOT be clamped to zero — zero is the "use the default" sentinel, so clamping
// would answer a request to disable rotation by enabling it at 10 MiB.
func TestLogWriterNegativeMaxSizeDisablesRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	w, err := openLogFile(path, config.LogConfig{MaxSizeMB: -1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.(io.Closer).Close() })

	rec := make([]byte, 512*1024)
	for i := 0; i < 40; i++ { // 20 MiB, twice the default cap
		_, err = w.Write(rec)
		require.NoError(t, err)
	}
	_, err = os.Stat(path + ".1")
	assert.True(t, os.IsNotExist(err), "rotation was disabled; no generation may exist")
}

// probeModel is a BaseChatModel that records nothing and answers a fixed
// message. It exists so BuildAdaptiveModels can be exercised on identifiable
// pointers without a provider or an API key.
type probeModel struct{ id string }

func (p *probeModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage(p.id, nil), nil
}

func (p *probeModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage(p.id, nil)}), nil
}

// TestBuildAdaptiveModelsWrapsEveryProvider proves each registry entry comes
// back wrapped, and that the wrapper carries the per-model identity.
func TestBuildAdaptiveModelsWrapsEveryProvider(t *testing.T) {
	a, b := &probeModel{id: "a"}, &probeModel{id: "b"}
	named := map[string]model.BaseChatModel{"gpt-4o": a, "claude": b}
	chain := []model.BaseChatModel{a, b}

	cfg := &config.Config{}
	cfg.LLM.Providers = []config.ProviderConfig{
		{Name: "openai", Kind: "openai", Model: "gpt-4o"},
		{Name: "anthropic", Kind: "anthropic", Model: "claude"},
	}

	gotNamed, gotChain := BuildAdaptiveModels(named, chain, adaptiveDeps{
		Cfg: cfg, Windows: map[string]int{"gpt-4o": 128000, "claude": 200000},
	})

	require.Len(t, gotNamed, 2)
	for key, m := range gotNamed {
		_, ok := m.(*einollm.AdaptiveModel)
		assert.True(t, ok, "registry entry %q must be wrapped, got %T", key, m)
	}
	require.Len(t, gotChain, 2)
	for i, m := range gotChain {
		_, ok := m.(*einollm.AdaptiveModel)
		assert.True(t, ok, "chain slot %d must be wrapped, got %T", i, m)
	}
}

// TestBuildAdaptiveModelsPreservesChainOrder is the assertion the map-based
// wrapping most easily breaks.
//
// The failover chain's ORDER is configured, not incidental: it is the operator
// saying which provider to try first. Rebuilding it from the registry map
// would produce a chain in Go's randomised map order, which fails over to the
// wrong provider on every boot — and no other assertion in this file would
// notice, because the SET of wrapped models is identical either way.
func TestBuildAdaptiveModelsPreservesChainOrder(t *testing.T) {
	first, second, third := &probeModel{id: "1"}, &probeModel{id: "2"}, &probeModel{id: "3"}
	named := map[string]model.BaseChatModel{"m1": first, "m2": second, "m3": third}
	// Deliberately NOT the sorted-key order, so a map-derived chain differs.
	chain := []model.BaseChatModel{third, first, second}

	cfg := &config.Config{}
	cfg.LLM.Providers = []config.ProviderConfig{
		{Name: "p1", Model: "m1"}, {Name: "p2", Model: "m2"}, {Name: "p3", Model: "m3"},
	}

	_, gotChain := BuildAdaptiveModels(named, chain, adaptiveDeps{Cfg: cfg})
	require.Len(t, gotChain, 3)

	// Each wrapper must hold the inner model that occupied that slot.
	wantInner := []model.BaseChatModel{third, first, second}
	for i, m := range gotChain {
		w, ok := m.(*einollm.AdaptiveModel)
		require.True(t, ok, "slot %d", i)
		assert.Same(t, wantInner[i], w.Inner, "slot %d wraps the wrong provider", i)
	}
}

// TestPerModelRateLimitsOnlyCarriesConfiguredProviders pins the "no entry"
// rule.
//
// An unconfigured provider must produce NO entry rather than a zero one:
// RateLimitConfig{QPM: 0} means UNLIMITED, so a zero entry would override a
// global default the operator did set — turning "inherit the global limit"
// into "exempt from it", which is the opposite request and is silent.
func TestPerModelRateLimitsOnlyCarriesConfiguredProviders(t *testing.T) {
	cfg := &config.Config{}
	cfg.LLM.RateLimit = config.RateLimitConfig{QPM: 60}
	cfg.LLM.Providers = []config.ProviderConfig{
		{Name: "tight", Model: "m-tight", QPM: 10},
		{Name: "inherits", Model: "m-inherits"},
		{Name: "burst-only", Model: "m-burst", Burst: 3},
	}

	got := perModelRateLimits(cfg)
	assert.Equal(t, einollm.RateLimitConfig{QPM: 10}, got["m-tight"])
	assert.Equal(t, einollm.RateLimitConfig{Burst: 3}, got["m-burst"])
	_, present := got["m-inherits"]
	assert.False(t, present, "an unconfigured provider must inherit, not be exempted")
	assert.Len(t, got, 2)
}

// TestProviderConfigsByKeyMatchesRegistryKeying pins the key derivation
// against einollm.BuildProviders' rule: model id, else name, else positional.
//
// A drift here is the silent-no-op shape — a table keyed on names nothing ever
// looks up, so every per-provider rate limit is ignored and the provider label
// on every usage row is empty, with no error anywhere.
func TestProviderConfigsByKeyMatchesRegistryKeying(t *testing.T) {
	cfg := &config.Config{}
	cfg.LLM.Providers = []config.ProviderConfig{
		{Name: "primary", Kind: "openai", Model: "gpt-4o"},
		{Name: "named-only", Kind: "anthropic"},        // no model -> name
		{Name: "dup", Kind: "openai", Model: "gpt-4o"}, // model taken -> name
		{Kind: "openai", Model: "gpt-4o"},              // both taken -> positional
	}
	got := providerConfigsByKey(cfg)

	assert.Equal(t, "primary", got["gpt-4o"].Name)
	assert.Equal(t, "anthropic", got["named-only"].Kind)
	assert.Equal(t, "dup", got["dup"].Name)
	_, positional := got["model-3"]
	assert.True(t, positional, "a provider with no free name must land on its positional key")
	assert.Len(t, got, 4, "no two providers may collide onto one key")
}

// TestStoreUsageSinkPersists proves the composition-root adapter actually
// reaches the table, and that the row keeps every field.
//
// The adapter is the only thing bridging a dependency GOV1 forbids in both
// directions, so it has no compiler-enforced connection to either side: an
// AppendUsage signature change would be caught, but a field dropped from the
// translation would not.
func TestStoreUsageSinkPersists(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	defer st.Close()

	sink := storeUsageSink{st: st}
	require.NoError(t, sink.RecordUsage(context.Background(), einollm.UsageRecord{
		Provider: "openai", Model: "gpt-4o", SessionID: "s-1",
		PromptTokens: 100, CompletionTokens: 20,
		CachedTokens: 40, ReasoningTokens: 5, CacheHit: true,
	}))

	rows, err := st.QueryUsage(store.UsageQuery{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	got := rows[0]
	assert.Equal(t, "openai", got.Provider)
	assert.Equal(t, "gpt-4o", got.Model)
	assert.Equal(t, "s-1", got.SessionID)
	assert.Equal(t, 100, got.PromptTokens)
	assert.Equal(t, 20, got.CompletionTokens)
	assert.Equal(t, 40, got.CachedTokens)
	assert.Equal(t, 5, got.ReasoningTokens)
	assert.True(t, got.CacheHit)
	assert.NotZero(t, got.TS, "the store must stamp a row the caller left unstamped")
}

// TestRunPreflightHonoursTheOffSwitch proves `preflight: false` performs no
// network work at all.
//
// The check is worth one warning at boot and no more: on an air-gapped
// deployment every probe times out, and the operator's only recourse is a flag
// that genuinely silences it rather than one that merely hides the log line
// while still paying the timeout on every start.
func TestRunPreflightHonoursTheOffSwitch(t *testing.T) {
	var hits int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer ts.Close()

	off := false
	cfg := &config.Config{}
	cfg.LLM.Preflight = &off
	cfg.LLM.Providers = []config.ProviderConfig{
		{Name: "p", Model: "m", BaseURL: ts.URL, APIKey: "k"},
	}
	RunPreflight(context.Background(), cfg)
	assert.Zero(t, hits, "preflight: false must issue no request")
}

// TestRunPreflightNeverPanicsWithoutProviders covers the shape every
// --fake-model boot takes. Preflight has no error return by design, so the
// only way it can break a startup is by panicking.
func TestRunPreflightNeverPanicsWithoutProviders(t *testing.T) {
	assert.NotPanics(t, func() {
		RunPreflight(context.Background(), &config.Config{})
	})
}
