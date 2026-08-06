package appserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestSidecarPathsDoNotCollide keeps two different -config values from sharing
// one runtime store.
//
// filepath.Ext(".hidden") returns the WHOLE name — there is no stem — so
// trimming it left an empty base and the fallback turned every extension-less
// dotfile into "config.appstate.json". `yanshi app -config .hidden` and
// `yanshi app -config config.yaml` in the same directory then read and wrote
// the same file, each silently overwriting the other's runtime configuration.
// The empty path keeps the fallback: it has no name to preserve.
func TestSidecarPathsDoNotCollide(t *testing.T) {
	inputs := []string{
		"config.yaml", "config.yml", ".hidden", ".yaml",
		"other.yaml", "noext", filepath.Join("d", "config.yaml"),
	}
	seen := map[string]string{}
	for _, in := range inputs {
		got := SidecarPath(in)
		if prev, dup := seen[got]; dup {
			t.Errorf("-config %q and -config %q both use %q; one process silently "+
				"overwrites the other's runtime config", prev, in, got)
		}
		seen[got] = in
	}
	if got := SidecarPath(""); got != "config.appstate.json" {
		t.Errorf("SidecarPath(\"\") = %q, want the config.appstate.json fallback", got)
	}
}

// TestFileConfigRefusesACorruptStore covers the load path.
//
// A missing file is an empty store (the first run has nothing to load), but a
// file that exists and does not parse is an error: starting empty there would
// present a corrupted store as an unconfigured one, and the next write would
// overwrite whatever the operator still had.
func TestFileConfigRefusesACorruptStore(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")

	if _, err := NewFileConfig(cfg); err != nil {
		t.Fatalf("a missing sidecar must be an empty store, got %v", err)
	}
	if err := os.WriteFile(SidecarPath(cfg), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileConfig(cfg); err == nil {
		t.Error("a corrupt sidecar was accepted as an empty store; the next write " +
			"would overwrite whatever the operator still had")
	}
}

// TestFileConfigWriteIsVisibleToAFreshReader is the persistence claim at the
// backend level, independent of the JSON-RPC transport.
func TestFileConfigWriteIsVisibleToAFreshReader(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	first, err := NewFileConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Write("ui.theme", json.RawMessage(`"dark"`)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	second, err := NewFileConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := second.Read("ui.theme")
	if err != nil {
		t.Fatalf("Read after reopen: %v", err)
	}
	if got != "dark" {
		t.Errorf("Read = %v, want dark", got)
	}
}

// TestFileConfigDoesNotLoseAnotherWritersKeys covers the multi-process seam.
//
// Two `yanshi app` processes may share one -config; nothing prevents it. When
// each held its own in-memory snapshot and flushed the WHOLE document, the
// second writer's flush erased every key the first had written — last-writer-
// wins over the entire store rather than per key. A supervisor that wrote
// ui.theme from one process and log.level from another kept whichever flushed
// last and lost the other, silently.
//
// Two FileConfig values over one path is exactly that shape: they are as
// independent as two processes are.
func TestFileConfigDoesNotLoseAnotherWritersKeys(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")

	a, err := NewFileConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewFileConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Write("ui.theme", json.RawMessage(`"dark"`)); err != nil {
		t.Fatal(err)
	}
	if err := b.Write("log.level", json.RawMessage(`"debug"`)); err != nil {
		t.Fatal(err)
	}

	fresh, err := NewFileConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"ui.theme": "dark", "log.level": "debug"} {
		got, err := fresh.Read(key)
		if err != nil {
			t.Errorf("%s was lost: %v", key, err)
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
}

// TestFileConfigReadSeesAnotherWritersValue is the read half of the same seam.
//
// A long-running process must not serve a snapshot frozen at startup: a
// supervisor that writes from one process and reads from another would get
// "config key is not set" for a key that is plainly in the file.
func TestFileConfigReadSeesAnotherWritersValue(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")

	reader, err := NewFileConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := NewFileConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Write("ui.theme", json.RawMessage(`"dark"`)); err != nil {
		t.Fatal(err)
	}
	got, err := reader.Read("ui.theme")
	if err != nil {
		t.Fatalf("a reader opened before the write cannot see it: %v", err)
	}
	if got != "dark" {
		t.Errorf("Read = %v, want dark", got)
	}
}
