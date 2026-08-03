package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKV_SetGet(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	require.NoError(t, s.KVSet("foo", "bar"))
	got, ok, err := s.KVGet("foo")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "bar", got)
}

func TestKV_Missing(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	_, ok, err := s.KVGet("nope")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestKV_Overwrite(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	require.NoError(t, s.KVSet("k", "1"))
	require.NoError(t, s.KVSet("k", "2"))
	got, _, err := s.KVGet("k")
	require.NoError(t, err)
	assert.Equal(t, "2", got)
}

// TestKV_EmptyKeyAndValue proves the kv table accepts an empty-string key
// and an empty-string value without special-casing (the UPSERT WHERE matches
// on the empty string). This guards against a future "key != ”" guard.
func TestKV_EmptyKeyAndValue(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	require.NoError(t, s.KVSet("", ""))
	got, ok, err := s.KVGet("")
	require.NoError(t, err)
	assert.True(t, ok, "empty key must be retrievable")
	assert.Equal(t, "", got)

	// Overwriting the empty key works the same as any key.
	require.NoError(t, s.KVSet("", "filled"))
	got, _, err = s.KVGet("")
	require.NoError(t, err)
	assert.Equal(t, "filled", got)
}
