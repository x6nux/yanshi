package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- reflection helpers (config.go) --------------------------------------

// yamlKeyProbe exposes every tag shape yamlKey must distinguish.
type yamlKeyProbe struct {
	NoTag    string `json:"x"`           // no yaml tag → ""
	Dashed   string `yaml:"-"`           // explicit skip → ""
	Named    string `yaml:"server"`      // explicit key
	Omitempty string `yaml:",omitempty"` // empty key → ToLower(Name)
}

func TestYAMLKeyBranches(t *testing.T) {
	ct := reflect.TypeOf(yamlKeyProbe{})
	cases := map[string]string{
		"NoTag":    "",
		"Dashed":   "",
		"Named":    "server",
		"Omitempty": "omitempty", // ToLower("Omitempty")
	}
	for i := 0; i < ct.NumField(); i++ {
		f := ct.Field(i)
		assert.Equal(t, cases[f.Name], yamlKey(f), "field %s", f.Name)
	}
}

// TestTypeDisplayNameBranches covers every kind branch via crafted types.
func TestTypeDisplayNameBranches(t *testing.T) {
	named := reflect.TypeOf(namedThing{})
	assert.Equal(t, "namedThing", typeDisplayName(named)) // default → Name()

	assert.Equal(t, "*bool", typeDisplayName(reflect.TypeOf((*bool)(nil))))
	assert.Equal(t, "duration", typeDisplayName(reflect.TypeOf(time.Duration(0))))
	assert.Equal(t, "[]string", typeDisplayName(reflect.TypeOf([]string{})))
	assert.Equal(t, "map[string]int", typeDisplayName(reflect.TypeOf(map[string]int{})))
	assert.Equal(t, "bool", typeDisplayName(reflect.TypeOf(false)))
	assert.Equal(t, "int", typeDisplayName(reflect.TypeOf(int(0))))
	assert.Equal(t, "int", typeDisplayName(reflect.TypeOf(int64(0))))
	assert.Equal(t, "float", typeDisplayName(reflect.TypeOf(float64(0))))
	assert.Equal(t, "string", typeDisplayName(reflect.TypeOf("")))
}

type namedThing struct{ X int }

// TestTypeDisplayNameUnnamedStruct covers the default branch's name=="" fallback
// (t.String()) for an anonymous struct type.
func TestTypeDisplayNameUnnamedStruct(t *testing.T) {
	assert.Equal(t, "struct {}", typeDisplayName(reflect.TypeOf(struct{}{})))
}

// TestLeafRowsEmptyStructFallback covers the "struct with no yaml-tagged
// fields emits one row" branch.
func TestLeafRowsEmptyStructFallback(t *testing.T) {
	type empty struct { //nolint:unused // used via reflection
		Ignored string `json:"x"` // no yaml tag
	}
	rows := leafRows(reflect.TypeOf(empty{}), "grp")
	require.Len(t, rows, 1)
	assert.Equal(t, "grp", rows[0].key)
}

// TestLeafRowsPointerToStruct covers the pointer-deref branch.
func TestLeafRowsPointerToStruct(t *testing.T) {
	type inner struct { //nolint:unused
		Field string `yaml:"field"`
	}
	rows := leafRows(reflect.TypeOf((*inner)(nil)), "ptr")
	require.Len(t, rows, 1)
	assert.Equal(t, "ptr.field", rows[0].key)
	assert.Equal(t, "string", rows[0].typ)
}

// TestRenderConfigSkeletonContainsKnownGroups sanity-checks the skeleton
// content beyond the existing tests (e.g. a known nested key).
func TestRenderConfigSkeletonContainsKnownKey(t *testing.T) {
	out := RenderConfigSkeleton()
	// server.http_addr is a well-known nested key that must surface.
	assert.Contains(t, out, "| server.http_addr")
}

// ---- writeAllHelpSnapshots error branch -----------------------------------

// TestWriteAllHelpSnapshotsMissingFile covers the ReadFile-error branch: a
// positional file that does not exist makes writeAllHelpSnapshots fail fast.
func TestWriteAllHelpSnapshotsMissingFile(t *testing.T) {
	prev := helpCapturer
	helpCapturer = func(subcmd string) (string, error) { return "x", nil }
	t.Cleanup(func() { helpCapturer = prev })

	err := writeAllHelpSnapshots([]string{filepath.Join(t.TempDir(), "nope.md")})
	require.Error(t, err)
}

// TestWriteAllHelpSnapshotsCapturerError covers the capture-error branch.
func TestWriteAllHelpSnapshotsCapturerError(t *testing.T) {
	prev := helpCapturer
	helpCapturer = func(subcmd string) (string, error) {
		return "", &gendocTestErr{"nope"}
	}
	t.Cleanup(func() { helpCapturer = prev })

	path := filepath.Join(t.TempDir(), "entrypoints.md")
	require.NoError(t, os.WriteFile(path, []byte("seed\n"), 0o644))
	err := writeAllHelpSnapshots([]string{path})
	require.Error(t, err)
}
