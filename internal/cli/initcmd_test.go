package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// initTemplate mirrors the shape of config.example.yaml that matters here:
// credential fields behind ${VAR}, a commented-out block, and a non-credential
// field that also uses ${VAR}.
const initTemplate = `schema_version: 1
server:
  http_addr: "${YANSHI_ADDR}"
storage:
  sqlite_path: "yanshi.db"
llm:
  providers:
    - name: "openai"
      kind: "openai"
      model: "gpt-4o"
      api_key: "${INIT_TEST_OPENAI_KEY}"
    - name: "claude"
      kind: "anthropic"
      model: "claude-opus-4-8"
      api_key: "${INIT_TEST_ANTHROPIC_KEY}"
# - name: "commented-out"
#   api_key: "${INIT_TEST_NEVER_REPORTED}"
`

// envFrom builds a lookup over a fixed map, so tests never mutate the process
// environment.
func envFrom(set map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := set[name]
		return v, ok
	}
}

// TestRunInitRefusesToOverwrite is the safety contract. A config file holds
// provider choices, profile policy and storage paths, none of which is
// recoverable from the template, so an init that overwrote silently would be a
// data-loss command wearing a setup command's name.
func TestRunInitRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeFile(t, filepath.Join(dir, "config.yaml"), "my: careful config\n")
	examplePath := writeFile(t, filepath.Join(dir, "config.example.yaml"), initTemplate)

	_, err := RunInit(InitOptions{
		ConfigPath: cfgPath, ExamplePath: examplePath, Env: envFrom(nil),
	})
	require.ErrorIs(t, err, ErrConfigExists)
	require.Contains(t, err.Error(), "-force")

	after, rerr := os.ReadFile(cfgPath)
	require.NoError(t, rerr)
	require.Equal(t, "my: careful config\n", string(after),
		"a refused init must not have touched a byte")
}

// TestRunInitForceBacksUpFirst proves the explicit override still cannot lose
// the original.
func TestRunInitForceBacksUpFirst(t *testing.T) {
	dir := t.TempDir()
	original := "my: careful config\n"
	cfgPath := writeFile(t, filepath.Join(dir, "config.yaml"), original)
	examplePath := writeFile(t, filepath.Join(dir, "config.example.yaml"), initTemplate)

	res, err := RunInit(InitOptions{
		ConfigPath: cfgPath, ExamplePath: examplePath, Force: true, Env: envFrom(nil),
	})
	require.NoError(t, err)
	require.NotEmpty(t, res.Backup)

	preserved, rerr := os.ReadFile(res.Backup)
	require.NoError(t, rerr)
	require.Equal(t, original, string(preserved))

	written, rerr := os.ReadFile(cfgPath)
	require.NoError(t, rerr)
	require.Equal(t, initTemplate, string(written))
}

// TestRunInitCopiesTemplateVerbatim pins the "leave ${VAR} as ${VAR}" rule:
// expanding a reference would bake a credential into a file on disk, which is
// exactly what the template's indirection exists to avoid.
func TestRunInitCopiesTemplateVerbatim(t *testing.T) {
	dir := t.TempDir()
	examplePath := writeFile(t, filepath.Join(dir, "config.example.yaml"), initTemplate)
	cfgPath := filepath.Join(dir, "config.yaml")

	_, err := RunInit(InitOptions{
		ConfigPath: cfgPath, ExamplePath: examplePath,
		Env: envFrom(map[string]string{"INIT_TEST_OPENAI_KEY": "sk-live-SECRET"}),
	})
	require.NoError(t, err)

	written, rerr := os.ReadFile(cfgPath)
	require.NoError(t, rerr)
	require.Equal(t, initTemplate, string(written))
	require.Contains(t, string(written), "${INIT_TEST_OPENAI_KEY}")
	require.NotContains(t, string(written), "sk-live-SECRET",
		"a resolved credential must stay in the environment, never in the file")
}

// TestRunInitReportsMissingAndResolvedKeys covers the one thing init tells the
// operator that copying the template by hand does not: which environment
// variables are still unset.
func TestRunInitReportsMissingAndResolvedKeys(t *testing.T) {
	dir := t.TempDir()
	examplePath := writeFile(t, filepath.Join(dir, "config.example.yaml"), initTemplate)

	res, err := RunInit(InitOptions{
		ConfigPath:  filepath.Join(dir, "config.yaml"),
		ExamplePath: examplePath,
		Env:         envFrom(map[string]string{"INIT_TEST_OPENAI_KEY": "sk-x"}),
	})
	require.NoError(t, err)
	require.Equal(t, []string{"INIT_TEST_OPENAI_KEY"}, res.ResolvedKeys)
	require.Equal(t, []string{"INIT_TEST_ANTHROPIC_KEY"}, res.MissingKeys)
	require.NotContains(t, res.MissingKeys, "INIT_TEST_NEVER_REPORTED",
		"a commented-out block is not a reference the config makes")
	require.NotContains(t, res.MissingKeys, "YANSHI_ADDR",
		"only credential-ish keys are reported; a listen address is not one")
}

// TestClassifyEnvRefs is the table for the scanner.
func TestClassifyEnvRefs(t *testing.T) {
	cases := []struct {
		name         string
		template     string
		env          map[string]string
		wantResolved []string
		wantMissing  []string
	}{
		{
			name:        "plain api_key reference",
			template:    "api_key: \"${A}\"\n",
			wantMissing: []string{"A"},
		},
		{
			name:         "set variable is resolved",
			template:     "api_key: \"${A}\"\n",
			env:          map[string]string{"A": "v"},
			wantResolved: []string{"A"},
		},
		{
			name:        "list-item key is recognised",
			template:    "- api_key: \"${A}\"\n",
			wantMissing: []string{"A"},
		},
		{
			name:        "token and client_secret are credentials too",
			template:    "token: \"${T}\"\nclient_secret: \"${C}\"\n",
			wantMissing: []string{"C", "T"},
		},
		{
			name:     "non-credential key is ignored",
			template: "base_url: \"${U}\"\n",
		},
		{
			name:     "commented line is ignored",
			template: "#   api_key: \"${A}\"\n",
		},
		{
			name:     "literal value has no reference",
			template: "api_key: \"sk-literal\"\n",
		},
		{
			name:        "duplicate references collapse",
			template:    "api_key: \"${A}\"\napi_key: \"${A}\"\n",
			wantMissing: []string{"A"},
		},
		{
			name:        "two references on one line",
			template:    "api_key: \"${A}-${B}\"\n",
			wantMissing: []string{"A", "B"},
		},
		{
			name:     "unterminated reference is not a name",
			template: "api_key: \"${A\"\n",
		},
		{
			name:     "empty reference is not a name",
			template: "api_key: \"${}\"\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolved, missing := classifyEnvRefs(tc.template, envFrom(tc.env))
			require.Equal(t, tc.wantResolved, resolved)
			require.Equal(t, tc.wantMissing, missing)
		})
	}
}

// TestRunInitCreatesParentDirectory proves init works against a path in a
// directory that does not exist yet, which is the normal `yanshi init
// ~/.config/yanshi/config.yaml` case.
func TestRunInitCreatesParentDirectory(t *testing.T) {
	dir := t.TempDir()
	examplePath := writeFile(t, filepath.Join(dir, "config.example.yaml"), initTemplate)
	cfgPath := filepath.Join(dir, "nested", "deeper", "config.yaml")

	_, err := RunInit(InitOptions{
		ConfigPath: cfgPath, ExamplePath: examplePath, Env: envFrom(nil),
	})
	require.NoError(t, err)
	_, serr := os.Stat(cfgPath)
	require.NoError(t, serr)
}

// TestRunInitWritesOwnerOnlyMode asserts the generated file is 0600: it is
// where an api key lands the moment the operator replaces a ${VAR} with a
// literal, and doctor's permission repair would otherwise flag a file yanshi
// itself just created.
func TestRunInitWritesOwnerOnlyMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	examplePath := writeFile(t, filepath.Join(dir, "config.example.yaml"), initTemplate)
	cfgPath := filepath.Join(dir, "config.yaml")

	_, err := RunInit(InitOptions{
		ConfigPath: cfgPath, ExamplePath: examplePath, Env: envFrom(nil),
	})
	require.NoError(t, err)

	fi, serr := os.Stat(cfgPath)
	require.NoError(t, serr)
	require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
}

// TestRunInitFailsWithoutTemplate asserts a missing template is a loud error,
// not an empty config file. Writing a zero-byte config would produce a yanshi
// that boots with no providers and no profiles and blames the operator.
func TestRunInitFailsWithoutTemplate(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	_, err := RunInit(InitOptions{
		ConfigPath: cfgPath, ExamplePath: filepath.Join(dir, "absent.yaml"),
		Env: envFrom(nil),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "template")

	_, serr := os.Stat(cfgPath)
	require.True(t, os.IsNotExist(serr), "no config must be created when the template is missing")
}

// TestRunInitDefaultsToConfigYaml covers the zero-option path used by a bare
// `yanshi init`.
func TestRunInitDefaultsToConfigYaml(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.example.yaml"), initTemplate)

	cwd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	res, err := RunInit(InitOptions{Env: envFrom(nil)})
	require.NoError(t, err)
	require.Equal(t, "config.yaml", res.Path)
	_, serr := os.Stat(filepath.Join(dir, "config.yaml"))
	require.NoError(t, serr)
}

// TestRenderInitResultNamesMissingVarsNeverValues pins the console surface: the
// operator must learn WHICH variable is unset, and the report must not become
// another place a credential is printed.
func TestRenderInitResultNamesMissingVarsNeverValues(t *testing.T) {
	var sb strings.Builder
	RenderInitResult(&sb, InitResult{
		Path:         "config.yaml",
		Backup:       "config.yaml.bak-1",
		ResolvedKeys: []string{"OPENAI_API_KEY"},
		MissingKeys:  []string{"ANTHROPIC_API_KEY"},
	})
	out := sb.String()
	require.Contains(t, out, "wrote config.yaml")
	require.Contains(t, out, "config.yaml.bak-1")
	require.Contains(t, out, "OPENAI_API_KEY")
	require.Contains(t, out, "ANTHROPIC_API_KEY")
	require.Contains(t, out, "yanshi doctor")

	// A clean run says nothing about keys it has nothing to say about.
	sb.Reset()
	RenderInitResult(&sb, InitResult{Path: "config.yaml"})
	require.NotContains(t, sb.String(), "still to set")
	require.NotContains(t, sb.String(), "preserved at")
}

// TestRunInitAgainstTheRealTemplate proves the shipped config.example.yaml is
// actually usable as an init source, rather than the tests only ever exercising
// a hand-written fixture that happens to match the parser.
func TestRunInitAgainstTheRealTemplate(t *testing.T) {
	real := filepath.Join("..", "..", "config.example.yaml")
	if _, err := os.Stat(real); err != nil {
		t.Skipf("repo template not reachable from here: %v", err)
	}
	dir := t.TempDir()
	res, err := RunInit(InitOptions{
		ConfigPath: filepath.Join(dir, "config.yaml"), ExamplePath: real,
		Env: envFrom(nil),
	})
	require.NoError(t, err)
	require.NotEmpty(t, res.MissingKeys,
		"the shipped template references provider env vars; init must name them")
	for _, name := range res.MissingKeys {
		require.NotContains(t, name, "$", "a reported name must be a bare variable name")
		require.NotContains(t, name, "{", "a reported name must be a bare variable name")
	}
}
