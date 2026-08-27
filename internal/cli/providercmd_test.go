package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/x6nux/yanshi/internal/secrets"
)

// providerFixture writes a small commented config and returns its path plus a
// file-backed secrets manager.
func providerFixture(t *testing.T, body string) (string, *secrets.Manager) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	mgr, err := secrets.NewManager(secrets.Config{
		Backend:    "file",
		FilePath:   filepath.Join(dir, "secrets.enc"),
		Passphrase: []byte("test-passphrase"),
	})
	if err != nil {
		t.Fatalf("secrets.NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	return path, mgr
}

// baseProviderConfig is a config with one provider and comments around it, so
// every test can check that the comments survive.
const baseProviderConfig = `# yanshi configuration
llm:
  # The provider list. Order matters for the fallback chain.
  providers:
    # The first-party OpenAI adapter.
    - name: "openai"
      kind: "openai"
      model: "gpt-4o"
      api_key: "${OPENAI_API_KEY}"
storage:
  sqlite_path: "./yanshi.db"
`

// TestProviderAddWritesAReferenceNotTheKey is the whole point of the command:
// the key belongs in the credential backend, and config.yaml gets copied into
// dotfile repositories and attached to bug reports.
func TestProviderAddWritesAReferenceNotTheKey(t *testing.T) {
	path, mgr := providerFixture(t, baseProviderConfig)
	const key = "sk-live-abcdef123456"

	res, err := RunProviderAdd(ProviderAddOptions{
		ConfigPath: path, Name: "gateway", Kind: "openai", Model: "gpt-4o-mini",
		BaseURL: "https://gw.example/v1", APIKey: key, Secrets: mgr,
	})
	if err != nil {
		t.Fatalf("RunProviderAdd: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(data), key) {
		t.Fatal("the API key was written into config.yaml, which is exactly what this command exists to prevent")
	}
	if !strings.Contains(string(data), ProviderSecretRef("gateway")) {
		t.Fatalf("config does not carry the secret reference:\n%s", data)
	}
	stored, err := mgr.Store().Get(ProviderSecretService, "gateway")
	if err != nil || stored != key {
		t.Fatalf("stored key = %q, %v; want the key in the secrets backend", stored, err)
	}
	if res.SecretRef != ProviderSecretRef("gateway") || res.Replaced {
		t.Errorf("result = %+v", res)
	}
	if !res.RestartRequired {
		t.Error("llm.providers is on the non-reloadable list; the result must say a restart is needed")
	}
}

// TestProviderAddResultNeverCarriesTheKey: the result is printed, logged and
// returned from a function, and a credential must not be in any of those.
func TestProviderAddResultNeverCarriesTheKey(t *testing.T) {
	path, mgr := providerFixture(t, baseProviderConfig)
	const key = "sk-live-abcdef123456"
	res, err := RunProviderAdd(ProviderAddOptions{
		ConfigPath: path, Name: "gw", Kind: "openai", Model: "m", APIKey: key, Secrets: mgr,
	})
	if err != nil {
		t.Fatalf("RunProviderAdd: %v", err)
	}
	var buf bytes.Buffer
	RenderProviderAddResult(&buf, res)
	if strings.Contains(buf.String(), key) {
		t.Fatalf("the rendered result echoed the key: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "restart") {
		t.Errorf("the rendered result does not tell the operator to restart:\n%s", buf.String())
	}
}

// TestProviderAddPreservesComments. A struct round-trip would erase every
// comment in a file that is roughly half comments, turning a four-line addition
// into a rewrite the operator no longer recognises.
func TestProviderAddPreservesComments(t *testing.T) {
	path, mgr := providerFixture(t, baseProviderConfig)
	if _, err := RunProviderAdd(ProviderAddOptions{
		ConfigPath: path, Name: "gw", Kind: "anthropic", Model: "claude", APIKey: "sk-abcdef",
		Secrets: mgr,
	}); err != nil {
		t.Fatalf("RunProviderAdd: %v", err)
	}
	data, _ := os.ReadFile(path)
	for _, comment := range []string{
		"# yanshi configuration",
		"# The provider list. Order matters for the fallback chain.",
		"# The first-party OpenAI adapter.",
	} {
		if !strings.Contains(string(data), comment) {
			t.Errorf("comment %q was erased:\n%s", comment, data)
		}
	}
	// And the untouched sections survived.
	if !strings.Contains(string(data), "sqlite_path") {
		t.Errorf("an unrelated section was lost:\n%s", data)
	}
}

// TestProviderAddRefusesToClobber: a provider block carries the operator's
// model, base URL and generation parameters, and overwriting because the name
// matched would discard all of it to change one key.
func TestProviderAddRefusesToClobber(t *testing.T) {
	path, mgr := providerFixture(t, baseProviderConfig)
	_, err := RunProviderAdd(ProviderAddOptions{
		ConfigPath: path, Name: "openai", Kind: "openai", Model: "gpt-5", APIKey: "sk-abcdef",
		Secrets: mgr,
	})
	if !errors.Is(err, ErrProviderExists) {
		t.Fatalf("err = %v, want ErrProviderExists", err)
	}
	// Nothing was written: the existing provider still names the old model.
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "gpt-4o") || strings.Contains(string(data), "gpt-5") {
		t.Fatalf("the refused add still modified the config:\n%s", data)
	}
}

// TestProviderAddReplaceOverwritesWholly: a partial patch would leave the old
// model or base_url of a provider the operator explicitly asked to replace,
// producing a hybrid nobody wrote.
func TestProviderAddReplaceOverwritesWholly(t *testing.T) {
	path, mgr := providerFixture(t, `llm:
  providers:
    - name: "openai"
      kind: "openai"
      model: "gpt-4o"
      base_url: "https://old.example/v1"
      api_key: "${OPENAI_API_KEY}"
      temperature: 0.7
`)
	res, err := RunProviderAdd(ProviderAddOptions{
		ConfigPath: path, Name: "openai", Kind: "anthropic", Model: "claude-x",
		APIKey: "sk-abcdef", Replace: true, Secrets: mgr,
	})
	if err != nil {
		t.Fatalf("RunProviderAdd: %v", err)
	}
	if !res.Replaced {
		t.Error("result does not report the replacement")
	}
	if res.Backup == "" {
		t.Error("no backup was taken for a command that rewrites a hand-maintained file")
	}
	data, _ := os.ReadFile(path)
	for _, gone := range []string{"gpt-4o", "old.example", "temperature"} {
		if strings.Contains(string(data), gone) {
			t.Errorf("replace left %q behind, producing a hybrid config:\n%s", gone, data)
		}
	}
	if !strings.Contains(string(data), "claude-x") {
		t.Errorf("replace did not apply:\n%s", data)
	}
	// The backup still holds the original.
	backup, err := os.ReadFile(res.Backup)
	if err != nil || !strings.Contains(string(backup), "gpt-4o") {
		t.Errorf("backup %q does not hold the original: %v", res.Backup, err)
	}
}

// TestProviderAddValidation covers every refusal. Each names the field, because
// the alternative is a provider that boots and then fails on its first call.
func TestProviderAddValidation(t *testing.T) {
	cases := []struct {
		name string
		opts ProviderAddOptions
		want string
	}{
		{"missing model", ProviderAddOptions{Name: "a", Kind: "openai", APIKey: "sk-abcdef"}, "model id"},
		{"unknown kind", ProviderAddOptions{Name: "a", Kind: "gemini", Model: "m", APIKey: "sk-abcdef"}, "unknown adapter kind"},
		{"blank key", ProviderAddOptions{Name: "a", Kind: "openai", Model: "m", APIKey: "   "}, "API key is required"},
		// Below MinSecretLength the Redactor silently drops the value, so the
		// key would show up verbatim in logs and stored messages.
		{"unredactable key", ProviderAddOptions{Name: "a", Kind: "openai", Model: "m", APIKey: "sk1"}, "redact"},
		{"name with a slash", ProviderAddOptions{Name: "a/b", Kind: "openai", Model: "m", APIKey: "sk-abcdef"}, "may not contain"},
		{"negative window", ProviderAddOptions{Name: "a", Kind: "openai", Model: "m", APIKey: "sk-abcdef", ContextWindow: -1}, "negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, mgr := providerFixture(t, baseProviderConfig)
			opts := tc.opts
			opts.ConfigPath, opts.Secrets = path, mgr
			_, err := RunProviderAdd(opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
			// A rejected add must leave nothing behind in either store.
			data, _ := os.ReadFile(path)
			if strings.Contains(string(data), `name: "a"`) {
				t.Errorf("a rejected add still wrote to the config:\n%s", data)
			}
		})
	}
}

// TestProviderAddRefusesWithoutASecretsBackend: with no backend the key has
// nowhere to go but the config, which is the one outcome this command exists
// to prevent. Degrading silently would undo it.
func TestProviderAddRefusesWithoutASecretsBackend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(baseProviderConfig), 0o600)
	none, err := secrets.NewManager(secrets.Config{Backend: "none"})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	_, err = RunProviderAdd(ProviderAddOptions{
		ConfigPath: path, Name: "gw", Kind: "openai", Model: "m", APIKey: "sk-abcdef",
		Secrets: none,
	})
	if err == nil || !strings.Contains(err.Error(), "secrets backend") {
		t.Fatalf("err = %v, want one naming the missing secrets backend", err)
	}
}

// TestProviderAddPromptsForMissingValues drives the interactive path, and pins
// that a blank answer takes the offered default rather than an empty string.
func TestProviderAddPromptsForMissingValues(t *testing.T) {
	path, mgr := providerFixture(t, baseProviderConfig)
	// name, kind (blank -> default openai), model, base URL (blank), api key.
	in := strings.NewReader("gateway\n\ngpt-4o-mini\n\nsk-secret-value\n")
	var out bytes.Buffer

	res, err := RunProviderAdd(ProviderAddOptions{
		ConfigPath: path, In: in, Out: &out, Secrets: mgr,
	})
	if err != nil {
		t.Fatalf("RunProviderAdd: %v", err)
	}
	if res.Name != "gateway" {
		t.Errorf("name = %q", res.Name)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"openai"`) {
		t.Errorf("the blank kind answer did not take the default:\n%s", data)
	}
	if strings.Contains(string(data), "base_url") {
		t.Errorf("a blank base URL was written as an empty override:\n%s", data)
	}
	if !strings.Contains(out.String(), "provider name") {
		t.Errorf("the prompts were not shown: %q", out.String())
	}
	// The prompt warns the key will be visible, since echo cannot be disabled
	// on an injected reader.
	if !strings.Contains(out.String(), "visible") {
		t.Errorf("the API key prompt does not warn about the echo: %q", out.String())
	}
}

// TestProviderAddWithoutATerminalNamesTheFlag: a wizard that just fails is
// unusable from a provisioning script; naming the flag makes it scriptable.
func TestProviderAddWithoutATerminalNamesTheFlag(t *testing.T) {
	path, mgr := providerFixture(t, baseProviderConfig)
	_, err := RunProviderAdd(ProviderAddOptions{ConfigPath: path, Secrets: mgr})
	if err == nil || !strings.Contains(err.Error(), "flag") {
		t.Fatalf("err = %v, want one telling the caller to pass a flag", err)
	}
}

// TestProviderAddCreatesTheProvidersList covers the configs that predate any
// provider: an absent llm block, and a `providers:` key left with no value.
func TestProviderAddCreatesTheProvidersList(t *testing.T) {
	for _, body := range []string{
		"storage:\n  sqlite_path: \"./y.db\"\n",
		"llm:\n  providers:\n",
		"llm: {}\n",
	} {
		t.Run(strings.ReplaceAll(body, "\n", "⏎"), func(t *testing.T) {
			path, mgr := providerFixture(t, body)
			if _, err := RunProviderAdd(ProviderAddOptions{
				ConfigPath: path, Name: "gw", Kind: "openai", Model: "m",
				APIKey: "sk-abcdef", Secrets: mgr,
			}); err != nil {
				t.Fatalf("RunProviderAdd: %v", err)
			}
			names, err := ListProviders(path)
			if err != nil {
				t.Fatalf("ListProviders: %v", err)
			}
			if len(names) != 1 || names[0] != "gw" {
				data, _ := os.ReadFile(path)
				t.Fatalf("providers = %v after add:\n%s", names, data)
			}
		})
	}
}

// TestProviderAddOmitsTheContextWindowWhenUnset: writing 0 would override the
// built-in model catalog with a value nobody chose.
func TestProviderAddOmitsTheContextWindowWhenUnset(t *testing.T) {
	path, mgr := providerFixture(t, baseProviderConfig)
	if _, err := RunProviderAdd(ProviderAddOptions{
		ConfigPath: path, Name: "gw", Kind: "openai", Model: "m", APIKey: "sk-abcdef",
		Secrets: mgr,
	}); err != nil {
		t.Fatalf("RunProviderAdd: %v", err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "context_window: 0") {
		t.Fatalf("an unset context window was written as zero:\n%s", data)
	}

	path2, mgr2 := providerFixture(t, baseProviderConfig)
	if _, err := RunProviderAdd(ProviderAddOptions{
		ConfigPath: path2, Name: "gw", Kind: "openai", Model: "m", APIKey: "sk-abcdef",
		ContextWindow: 200000, Secrets: mgr2,
	}); err != nil {
		t.Fatalf("RunProviderAdd: %v", err)
	}
	data2, _ := os.ReadFile(path2)
	if !strings.Contains(string(data2), "context_window: 200000") {
		t.Fatalf("an explicit context window was dropped:\n%s", data2)
	}
}

// TestProviderAddResultIsLoadableConfig: the whole thing is pointless if the
// file it produces does not parse back as YAML with the expected shape.
func TestProviderAddResultIsLoadableConfig(t *testing.T) {
	path, mgr := providerFixture(t, baseProviderConfig)
	if _, err := RunProviderAdd(ProviderAddOptions{
		ConfigPath: path, Name: "gw", Kind: "openai", Model: "gpt-4o-mini",
		APIKey: "sk-abcdef", Secrets: mgr,
	}); err != nil {
		t.Fatalf("RunProviderAdd: %v", err)
	}
	data, _ := os.ReadFile(path)
	var parsed struct {
		LLM struct {
			Providers []struct {
				Name   string `yaml:"name"`
				Kind   string `yaml:"kind"`
				Model  string `yaml:"model"`
				APIKey string `yaml:"api_key"`
			} `yaml:"providers"`
		} `yaml:"llm"`
	}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("the written config no longer parses: %v\n%s", err, data)
	}
	if len(parsed.LLM.Providers) != 2 {
		t.Fatalf("got %d providers, want 2", len(parsed.LLM.Providers))
	}
	added := parsed.LLM.Providers[1]
	if added.Name != "gw" || added.Kind != "openai" || added.Model != "gpt-4o-mini" {
		t.Errorf("added provider = %+v", added)
	}
	if added.APIKey != ProviderSecretRef("gw") {
		t.Errorf("api_key = %q, want the secret reference", added.APIKey)
	}
}

// TestListProvidersSortsAndTolerates covers the read side, including the
// configs where there is nothing to list.
func TestListProvidersSortsAndTolerates(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	got, err := ListProviders(write("multi.yaml", `llm:
  providers:
    - name: "zeta"
    - name: "alpha"
`))
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Errorf("names = %v, want sorted [alpha zeta]", got)
	}
	for _, body := range []string{"llm: {}\n", "storage: {}\n", "llm:\n  providers:\n"} {
		got, err := ListProviders(write("empty.yaml", body))
		if err != nil || len(got) != 0 {
			t.Errorf("ListProviders(%q) = %v, %v; want empty and no error", body, got, err)
		}
	}
	if _, err := ListProviders(filepath.Join(dir, "nope.yaml")); err == nil {
		t.Error("a missing config must be an error, not an empty list")
	}
}

// TestProviderSecretRefIsPerProvider: one namespace shared by every provider
// would make adding a second key delete the first.
func TestProviderSecretRefIsPerProvider(t *testing.T) {
	a, b := ProviderSecretRef("one"), ProviderSecretRef("two")
	if a == b {
		t.Fatal("two providers share a secret reference")
	}
	if !strings.HasPrefix(a, "secret://") {
		t.Errorf("ref %q is not a secret:// reference", a)
	}
}
