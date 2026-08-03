package plugin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeConnector is a minimal Connector for testing.
type fakeConnector struct{ name string }

func (f *fakeConnector) Name() string                  { return f.name }
func (f *fakeConnector) Start(_ context.Context) error { return nil }
func (f *fakeConnector) Stop(_ context.Context) error  { return nil }

func TestHost_RegisterGet(t *testing.T) {
	h := NewHost()
	c := &fakeConnector{name: "irc"}
	require.NoError(t, h.Register(c))

	// Found.
	got, ok := h.Get("irc")
	require.True(t, ok)
	assert.Equal(t, "irc", got.Name())

	// Not found.
	_, ok = h.Get("nope")
	assert.False(t, ok)

	// All returns the registered connector.
	all := h.All()
	require.Len(t, all, 1)
	assert.Equal(t, "irc", all[0].Name())
}

func TestHost_RegisterInvalid(t *testing.T) {
	h := NewHost()

	// nil connector.
	err := h.Register(nil)
	require.Error(t, err)

	// Empty-name connector.
	err = h.Register(&fakeConnector{name: ""})
	require.Error(t, err)
}
