package http

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/features"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/secrets"
)

// --- formatTokenCount ---

func TestFormatTokenCount(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{42, "42"},
		{999, "999"},
		{1000, "1.0k"},
		{1500, "1.5k"},
		{77700, "77.7k"},
		{123456, "123.5k"},
	}
	for _, tc := range tests {
		got := formatTokenCount(tc.n)
		if got != tc.want {
			t.Errorf("formatTokenCount(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// --- formatElapsed ---

func TestFormatElapsed(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{1 * time.Second, "1s"},
		{42 * time.Second, "42s"},
		{1 * time.Minute, "1m 0s"},
		{5*time.Minute + 3*time.Second, "5m 3s"},
		{1*time.Hour + 59*time.Minute + 59*time.Second, "1h 59m 59s"},
		{24*time.Hour + 30*time.Minute, "24h 30m 0s"},
	}
	for _, tc := range tests {
		got := formatElapsed(tc.d)
		if got != tc.want {
			t.Errorf("formatElapsed(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// --- scopeJSON ---

func TestScopeJSON(t *testing.T) {
	scope := approval.Scope{
		Tool:    "fs_write",
		FSOp:    "write",
		Paths:   []string{"/tmp/test.txt"},
		Program: "cat",
		Host:    "localhost",
	}
	got := scopeJSON(scope)
	assert.Contains(t, got, `"tool":"fs_write"`)
	assert.Contains(t, got, `"paths":`)
	assert.Contains(t, got, `"host":"localhost"`)

	// Empty scope.
	empty := scopeJSON(approval.Scope{})
	assert.Contains(t, empty, `"tool":""`)

	// Verify it's valid JSON.
	var parsed map[string]any
	assert.NoError(t, json.Unmarshal([]byte(got), &parsed))
}

// --- setFeature ---

func TestSetFeature(t *testing.T) {
	t.Run("nil registry returns error", func(t *testing.T) {
		err := setFeature(nil, proto.FeaturesSetPayload{Key: "test", Enabled: boolPtr(true)})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unavailable")
	})

	t.Run("missing key returns error", func(t *testing.T) {
		reg := features.NewRegistry(false)
		err := setFeature(reg, proto.FeaturesSetPayload{Key: "", Enabled: boolPtr(true)})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "key")
	})

	t.Run("missing enabled returns error", func(t *testing.T) {
		reg := features.NewRegistry(false)
		err := setFeature(reg, proto.FeaturesSetPayload{Key: "test", Enabled: nil})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "enabled")
	})

	t.Run("set valid feature", func(t *testing.T) {
		reg := features.NewRegistry(false)
		reg.Register(features.Spec{Key: "test-flag", Stage: features.Beta, Default: false, Owner: "test"})
		err := setFeature(reg, proto.FeaturesSetPayload{Key: "test-flag", Enabled: boolPtr(true)})
		require.NoError(t, err)
		assert.True(t, reg.Enabled("test-flag"))
	})

	t.Run("set unknown feature returns error", func(t *testing.T) {
		reg := features.NewRegistry(false)
		err := setFeature(reg, proto.FeaturesSetPayload{Key: "unknown", Enabled: boolPtr(true)})
		require.Error(t, err)
	})
}

// --- featureRows ---

func TestFeatureRows(t *testing.T) {
	t.Run("nil registry returns nil", func(t *testing.T) {
		rows := featureRows(nil)
		assert.Nil(t, rows)
	})

	t.Run("empty registry returns empty", func(t *testing.T) {
		reg := features.NewRegistry(false)
		rows := featureRows(reg)
		assert.Empty(t, rows)
	})

	t.Run("registered flags appear", func(t *testing.T) {
		reg := features.NewRegistry(false)
		reg.Register(features.Spec{Key: "flag-a", Stage: features.Beta, Default: true, Owner: "alice"})
		reg.Register(features.Spec{Key: "flag-b", Stage: features.Experimental, Default: false, Owner: "bob"})

		rows := featureRows(reg)
		require.Len(t, rows, 2)

		m := map[string]proto.FeatureRow{}
		for _, r := range rows {
			m[r.Key] = r
		}
		assert.Equal(t, "beta", m["flag-a"].Stage)
		assert.True(t, m["flag-a"].Enabled)
		assert.Equal(t, "alice", m["flag-a"].Owner)
		assert.Equal(t, "experimental", m["flag-b"].Stage)
		assert.False(t, m["flag-b"].Enabled)
	})
}

// --- sseLifecycleRelay ---

func TestSSELifecycleRelayClose(t *testing.T) {
	r := newSSELifecycleRelay()
	assert.NotNil(t, r)
	r.Close()
	// After Close, Emit should not block or panic.
	f := proto.ServerFrame{Type: "agent_chunk", Text: "dropped"}
	r.Emit(f)
}

func TestSSELifecycleRelayEmitTerminal(t *testing.T) {
	r := newSSELifecycleRelay()
	// Terminal events go to the terminal channel.
	terminal := proto.ServerFrame{Type: "done", Event: "completed"}
	r.Emit(terminal)

	// Terminal is buffered 8, so immediate read should work.
	select {
	case f := <-r.terminal:
		assert.Equal(t, "done", f.Type)
	default:
		t.Fatal("terminal event was not delivered")
	}
}

func TestSSELifecycleRelayEmitProgress(t *testing.T) {
	r := newSSELifecycleRelay()
	// Progress events go to the progress channel (cap 64).
	for i := 0; i < 70; i++ {
		r.Emit(proto.ServerFrame{Type: "agent_chunk", Text: "progress", Event: "progress"})
	}
	// At cap 64, the last 6 were dropped. Verify we can read at most 64.
	count := 0
	for {
		select {
		case <-r.progress:
			count++
		default:
			goto done
		}
	}
done:
	assert.Equal(t, 64, count, "progress channel capped at 64")
}

// --- drainLifecycleFrames ---

func TestDrainLifecycleFrames(t *testing.T) {
	r := newSSELifecycleRelay()
	r.Emit(proto.ServerFrame{Type: "agent_chunk", Text: "hello", Event: "progress"})
	r.Emit(proto.ServerFrame{Type: "done", Event: "completed"})

	w := httptest.NewRecorder()
	drainLifecycleFrames(w, nil, r, nil)

	body := w.Body.String()
	assert.Contains(t, body, "hello")
}

func TestDrainLifecycleFramesRedacts(t *testing.T) {
	redactor := secrets.NewRedactor()
	redactor.Register("secret-key")
	r := newSSELifecycleRelay()
	r.Emit(proto.ServerFrame{Type: "agent_chunk", Text: "my secret-key is here", Event: "progress"})

	w := httptest.NewRecorder()
	drainLifecycleFrames(w, nil, r, redactor)

	body := w.Body.String()
	assert.NotContains(t, body, "secret-key")
	assert.Contains(t, body, "[REDACTED]")
}
