package main

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/config"
)

func TestRenderConfigSkeletonCoversAllTopLevel(t *testing.T) {
	out := RenderConfigSkeleton()
	// Every exported top-level Config field with a yaml tag must produce a
	// "### <key>" group header (### so it nests under a parent section).
	ct := reflect.TypeOf(config.Config{})
	for i := 0; i < ct.NumField(); i++ {
		f := ct.Field(i)
		key := yamlKey(f)
		if key == "" {
			continue
		}
		needle := "### " + key + "\n"
		if !strings.Contains(out, needle) {
			t.Errorf("top-level group %q missing from skeleton (need %q)", key, needle)
		}
	}
}

func TestRenderConfigSkeletonHasFieldRows(t *testing.T) {
	out := RenderConfigSkeleton()
	lines := strings.Split(out, "\n")
	rows := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "| ") && !strings.HasPrefix(l, "| key") && !strings.HasPrefix(l, "|---") {
			rows++
		}
	}
	if rows < 20 {
		t.Errorf("expected many field rows, got %d", rows)
	}
}

func TestRenderConfigSkeletonIdempotent(t *testing.T) {
	first := RenderConfigSkeleton()
	second := RenderConfigSkeleton()
	if first != second {
		t.Errorf("RenderConfigSkeleton not idempotent")
	}
}

// TestRenderConfigSkeletonCoversExampleYAMLKeys reconciles the skeleton with
// config.example.yaml. It was cited for「getting started 可零依赖跑通」and
// shows nothing of the sort — a real check of an unrelated fact.
func TestRenderConfigSkeletonCoversExampleYAMLKeys(t *testing.T) {
	// Every top-level key present in config.example.yaml must have a group in
	// the skeleton (the skeleton is the source of truth for what Config can do).
	example := readExampleConfig(t)
	skel := RenderConfigSkeleton()
	for _, key := range topLevelYAMLKeys(example) {
		if !strings.Contains(skel, "### "+key+"\n") {
			t.Errorf("config.example.yaml top-level key %q has no skeleton group", key)
		}
	}
}

// TestConfigSkeletonFieldsMatchStruct is the Advisory-4 CI invariant: the set
// of dotted-path yaml keys derivable from Config (via independent reflection)
// must equal the set of keys in the generated config-skeleton block. This
// catches both "struct has a field, skeleton is missing a row" and "skeleton
// has a row, struct deleted the field".
func TestConfigSkeletonFieldsMatchStruct(t *testing.T) {
	want := collectStructKeys(reflect.TypeOf(config.Config{}), "")
	got := collectSkeletonKeys(t, RenderConfigSkeleton())
	if len(want) != len(got) {
		t.Errorf("key count mismatch: struct has %d, skeleton has %d\nwant=%v\ngot =%v",
			len(want), len(got), want, got)
	}
	for k := range want {
		if !got[k] {
			t.Errorf("struct key %q missing from skeleton", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("skeleton key %q not in struct", k)
		}
	}
}

// collectStructKeys walks t independently of leafRows to cross-check. Maps and
// slices are leaves (the value type is not expanded); structs recurse.
func collectStructKeys(t reflect.Type, prefix string) map[string]bool {
	out := map[string]bool{}
	dt := t
	for dt.Kind() == reflect.Ptr {
		dt = dt.Elem()
	}
	if dt.Kind() == reflect.Struct && dt != reflect.TypeOf(time.Duration(0)) {
		for i := 0; i < dt.NumField(); i++ {
			f := dt.Field(i)
			tag := f.Tag.Get("yaml")
			if tag == "" || strings.Split(tag, ",")[0] == "-" {
				continue
			}
			key := strings.Split(tag, ",")[0]
			if key == "" {
				key = strings.ToLower(f.Name)
			}
			full := key
			if prefix != "" {
				full = prefix + "." + key
			}
			for k := range collectStructKeys(f.Type, full) {
				out[k] = true
			}
		}
		return out
	}
	if prefix != "" {
		out[prefix] = true
	}
	return out
}

// collectSkeletonKeys parses the first cell of every table row in the rendered
// skeleton.
func collectSkeletonKeys(t *testing.T, skel string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, line := range strings.Split(skel, "\n") {
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "| key") || strings.HasPrefix(line, "|---") {
			continue
		}
		fields := strings.SplitN(strings.TrimPrefix(line, "| "), " | ", 2)
		if len(fields) > 0 {
			out[strings.TrimSpace(fields[0])] = true
		}
	}
	return out
}

func readExampleConfig(t *testing.T) string {
	t.Helper()
	// Locate config.example.yaml relative to this test file (module root).
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "config.example.yaml"))
	if err != nil {
		t.Fatalf("read config.example.yaml: %v", err)
	}
	return string(data)
}

// topLevelYAMLKeys returns the zero-indented `key:` lines from a YAML body.
func topLevelYAMLKeys(yaml string) []string {
	seen := map[string]bool{}
	var keys []string
	for _, line := range strings.Split(yaml, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") {
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue // indented → not top-level
		}
		if i := strings.Index(trimmed, ":"); i > 0 {
			k := strings.TrimSpace(trimmed[:i])
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	return keys
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	// file = .../cmd/gendocs/gendocs_test.go → up two to module root.
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// --- help snapshot tests ---

func TestRenderHelpWrapsAndIdempotent(t *testing.T) {
	out := RenderHelp("serve", "Usage of serve:\n  -addr string\n")
	if !strings.Contains(out, "```text") {
		t.Errorf("missing text fence")
	}
	if !strings.Contains(out, "Usage of serve:") {
		t.Errorf("help content lost")
	}
	if !strings.HasPrefix(out, "```text\n") || !strings.HasSuffix(out, "\n```") {
		t.Errorf("fence not well-formed: %q", out)
	}
	again := RenderHelp("serve", "Usage of serve:\n  -addr string\n")
	if out != again {
		t.Errorf("RenderHelp not idempotent")
	}
}

func TestWriteAllHelpSnapshotsUsesFake(t *testing.T) {
	// Inject a fake capturer so the test does not build/spawn yanshi.
	prev := helpCapturer
	helpCapturer = func(subcmd string) (string, error) {
		return "FAKE HELP " + subcmd, nil
	}
	t.Cleanup(func() { helpCapturer = prev })

	dir := t.TempDir()
	path := filepath.Join(dir, "entrypoints.md")
	// Seed with markers for two subcommands and surrounding prose.
	seed := "intro\n\n<!-- BEGIN GENERATED: help:serve -->\nold\n<!-- END GENERATED: help:serve -->\n\ntail\n"
	seed += "\n<!-- BEGIN GENERATED: help:doctor -->\nold\n<!-- END GENERATED: help:doctor -->\n"
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeAllHelpSnapshots([]string{path}); err != nil {
		t.Fatalf("writeAllHelpSnapshots: %v", err)
	}
	got, _ := os.ReadFile(path)
	g := string(got)
	if !strings.Contains(g, "FAKE HELP serve") || strings.Contains(g, "\nold\n") {
		t.Errorf("serve block not replaced: %q", g)
	}
	if !strings.Contains(g, "FAKE HELP doctor") {
		t.Errorf("doctor block not replaced: %q", g)
	}
	if !strings.Contains(g, "intro") || !strings.Contains(g, "tail") {
		t.Errorf("surrounding prose lost: %q", g)
	}
	// Idempotent: run again, no change.
	if err := writeAllHelpSnapshots([]string{path}); err != nil {
		t.Fatalf("second writeAllHelpSnapshots: %v", err)
	}
	got2, _ := os.ReadFile(path)
	if string(got) != string(got2) {
		t.Errorf("writeAllHelpSnapshots not idempotent")
	}
}

// TestSubcommandListMatchesDispatch asserts yanshiSubcommands stays in sync
// with cmd/yanshi/main.go's top-level dispatch. A new top-level `case "foo":`
// in main.go must add "foo" here; the canonical set is pinned so drift on
// either side fails the test. auth's own sub-subcommands (set/status/logout/
// device) live inside runAuthSub, not the top-level dispatch, so they are
// excluded.
func TestSubcommandListMatchesDispatch(t *testing.T) {
	src := readMainGo(t)
	caseRe := regexp.MustCompile(`case "([a-z][a-z0-9-]*)":`)
	matches := caseRe.FindAllStringSubmatch(src, -1)
	// auth sub-subcommands are nested inside runAuthSub, not top-level dispatch.
	nonTopLevel := map[string]bool{"set": true, "status": true, "logout": true, "device": true}
	listed := map[string]bool{}
	for _, s := range yanshiSubcommands {
		listed[s] = true
	}
	for _, m := range matches {
		sub := m[1]
		if nonTopLevel[sub] {
			continue
		}
		if !listed[sub] {
			t.Errorf("main.go top-level dispatch case %q is not in yanshiSubcommands", sub)
		}
	}
	// Canonical set pin: yanshiSubcommands must be exactly this. A change here
	// is intentional and should update both the list and this assertion.
	want := []string{"yanshi", "serve", "chat", "exec", "app", "goal", "vcs-mcp", "mcp", "init", "daemon", "schedule", "provider", "models", "acp", "pr", "enqueue", "auth", "doctor"}
	if !reflect.DeepEqual(yanshiSubcommands, want) {
		t.Errorf("yanshiSubcommands drifted from canonical set:\n got=%v\nwant=%v", yanshiSubcommands, want)
	}
	// Every dispatched top-level subcommand must actually appear in main.go.
	for _, required := range []string{"serve", "chat", "exec", "app", "goal", "vcs-mcp", "mcp", "init", "daemon", "schedule", "provider", "models", "acp", "pr", "enqueue", "auth", "doctor"} {
		if !strings.Contains(src, `case "`+required+`":`) && !strings.Contains(src, `"`+required+`"`) {
			t.Errorf("required subcommand %q not found in main.go", required)
		}
	}
}

func readMainGo(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "cmd", "yanshi", "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	return string(data)
}
