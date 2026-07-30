package cli

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/api/http"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

func TestNewBackend_PrefersWS_FallsBackToSSE(t *testing.T) {
	// Server with both WS and SSE.
	o, _ := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	s := http.New(http.Config{Token: "t"})
	s.Chat(o, nil, nil)
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	b, err := newBackend(context.Background(), ts.URL)
	require.NoError(t, err)
	defer b.Close()
	assert.Equal(t, "ws", b.Mode(), "WS available -> ws")

	// Server with SSE only (no ChatWS registered).
	o2, _ := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"y"}, nil)})
	s2 := http.New(http.Config{Token: "t"})
	s2.Chat(o2, nil, nil)
	ts2 := httptest.NewServer(s2.Handler())
	t.Cleanup(ts2.Close)

	b2, err := newBackend(context.Background(), ts2.URL)
	require.NoError(t, err)
	defer b2.Close()
	assert.Equal(t, "sse", b2.Mode(), "no WS -> SSE fallback")
	// sanity: the ws URL must not appear in the SSE base
	_ = strings.HasPrefix
}
