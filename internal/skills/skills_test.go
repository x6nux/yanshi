package skills

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeSkill(t *testing.T, root, name, body string) string {
	t.Helper()
	d := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(d, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(body), 0o644))
	return d
}

func TestLoad_ValidSkill(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "greet", "---\nname: greet\ndescription: Use when saying hello\n---\n# Greet\nWave warmly.")

	reg, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)
	s, ok := reg.Get("greet")
	require.True(t, ok)
	assert.Equal(t, "greet", s.Name)
	assert.Equal(t, "Use when saying hello", s.Description)
	assert.Equal(t, "builtin", s.Source)
}

func TestLoad_SkipsDirWithoutSkillMd(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "empty"), 0o755))
	reg, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)
	assert.Empty(t, reg.List())
}

func TestLoad_SkipsInvalidName(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "bad", "---\nname: has spaces\ndescription: ok\n---\nbody")
	reg, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)
	_, ok := reg.Get("has spaces")
	assert.False(t, ok)
}

func TestLoad_SkipsMissingDescription(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "nodesc", "---\nname: nodesc\n---\nbody")
	reg, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)
	_, ok := reg.Get("nodesc")
	assert.False(t, ok)
}

func TestLoad_BadYAMLDoesNotFailAll(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "broken", "---\nname: broken\ndescription: [unclosed\n---\nbody")
	writeSkill(t, root, "good", "---\nname: good\ndescription: fine\n---\nbody")
	reg, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)
	_, ok := reg.Get("good")
	assert.True(t, ok, "good skill must still load despite sibling's bad yaml")
}

func TestLoad_FirstSeenWins(t *testing.T) {
	builtin := t.TempDir()
	user := t.TempDir()
	writeSkill(t, builtin, "shared", "---\nname: shared\ndescription: from-builtin\n---\nbody")
	writeSkill(t, user, "shared", "---\nname: shared\ndescription: from-user\n---\nbody")
	reg, err := NewLoader(Builtin(builtin), User(user)).Load()
	require.NoError(t, err)
	s, ok := reg.Get("shared")
	require.True(t, ok)
	assert.Equal(t, "from-builtin", s.Description, "builtin (first root) must win over user")
}

func TestLoad_ToleratesMissingRoot(t *testing.T) {
	reg, err := NewLoader(Builtin(filepath.Join(t.TempDir(), "does-not-exist"))).Load()
	require.NoError(t, err)
	assert.Empty(t, reg.List())
}

func TestLoad_NameTooLong(t *testing.T) {
	root := t.TempDir()
	longName := strings.Repeat("a", 65) // 65 chars, regex-valid but over the 64-char cap
	writeSkill(t, root, "toolong", "---\nname: "+longName+"\ndescription: ok\n---\nbody")
	reg, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)
	_, ok := reg.Get(longName)
	assert.False(t, ok, "65-char name must be skipped")
}

func TestLoad_DescTooLong(t *testing.T) {
	root := t.TempDir()
	longDesc := strings.Repeat("x", 1025) // 1025 chars, over the 1024-char cap
	writeSkill(t, root, "toodesc", "---\nname: toodesc\ndescription: "+longDesc+"\n---\nbody")
	reg, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)
	_, ok := reg.Get("toodesc")
	assert.False(t, ok, "1025-char description must be skipped")
}

func TestLoad_UserAndPluginSourceTags(t *testing.T) {
	userRoot := t.TempDir()
	pluginRoot := t.TempDir()
	writeSkill(t, userRoot, "uskill", "---\nname: uskill\ndescription: user skill\n---\nbody")
	writeSkill(t, pluginRoot, "pskill", "---\nname: pskill\ndescription: plugin skill\n---\nbody")
	reg, err := NewLoader(User(userRoot), Plugin("alpha", pluginRoot)).Load()
	require.NoError(t, err)
	u, ok := reg.Get("uskill")
	require.True(t, ok)
	assert.Equal(t, "user", u.Source)
	p, ok := reg.Get("pskill")
	require.True(t, ok)
	assert.Equal(t, "plugin:alpha", p.Source)
}

func TestRegistry_Body(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "greet", "---\nname: greet\ndescription: hi\n---\n# Greet\nWave.")
	reg, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)
	s, _ := reg.Get("greet")
	body, err := reg.Body(s)
	require.NoError(t, err)
	assert.Contains(t, body, "# Greet")
	assert.NotContains(t, body, "description") // frontmatter stripped
}

func TestRegistry_ReadFile_RejectsTraversal(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "greet", "---\nname: greet\ndescription: hi\n---\nbody")
	reg, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)
	s, _ := reg.Get("greet")
	_, err = reg.ReadFile(s, "../../etc/passwd")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes skill dir")
}

func TestRegistry_ReadFile_OK(t *testing.T) {
	root := t.TempDir()
	d := writeSkill(t, root, "greet", "---\nname: greet\ndescription: hi\n---\nbody")
	require.NoError(t, os.WriteFile(filepath.Join(d, "ref.md"), []byte("details"), 0o644))
	reg, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)
	s, _ := reg.Get("greet")
	got, err := reg.ReadFile(s, "ref.md")
	require.NoError(t, err)
	assert.Equal(t, "details", got)
}

// Edge cases for ReadFile path sanitization
func TestRegistry_ReadFile_RejectsSubdirTraversal(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "greet", "---\nname: greet\ndescription: hi\n---\nbody")
	reg, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)
	s, _ := reg.Get("greet")
	// sub/../.. resolves to parent of skill dir → must be rejected
	_, err = reg.ReadFile(s, "sub/../..")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes skill dir")
}

func TestRegistry_ReadFile_NestedPath_OK(t *testing.T) {
	root := t.TempDir()
	d := writeSkill(t, root, "greet", "---\nname: greet\ndescription: hi\n---\nbody")
	require.NoError(t, os.MkdirAll(filepath.Join(d, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(d, "sub", "ref.md"), []byte("nested details"), 0o644))
	reg, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)
	s, _ := reg.Get("greet")
	got, err := reg.ReadFile(s, "sub/ref.md")
	require.NoError(t, err)
	assert.Equal(t, "nested details", got)
}

func TestRegistry_ReadFile_RejectsAbsolutePath(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "greet", "---\nname: greet\ndescription: hi\n---\nbody")
	reg, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)
	s, _ := reg.Get("greet")

	// A path that filepath.IsAbs reports as absolute on the CURRENT OS.
	abs := filepath.Join(os.TempDir(), "some-target.txt")
	require.True(t, filepath.IsAbs(abs), "precondition: temp path must be absolute on this OS")

	_, err = reg.ReadFile(s, abs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be relative", "absolute path must be rejected by the IsAbs gate")
}

// ledger: C3/E03#4 模型可 load 匹配技能
func TestRegistry_MetaPrompt(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "greet", "---\nname: greet\ndescription: Use when saying hello\n---\nx")
	writeSkill(t, root, "bye", "---\nname: bye\ndescription: Use when leaving\n---\nx")
	reg, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)
	mp := reg.MetaPrompt()
	assert.Contains(t, mp, "greet")
	assert.Contains(t, mp, "Use when saying hello")
	assert.Contains(t, mp, "bye")
}

func TestRegistry_MetaPrompt_Empty(t *testing.T) {
	reg := &Registry{skills: map[string]*Skill{}}
	assert.Equal(t, "", reg.MetaPrompt())
}

// ---------------------------------------------------------------------------
// Plugin discovery tests
// ---------------------------------------------------------------------------

func TestDiscoverPlugins(t *testing.T) {
	pluginRoot := t.TempDir()
	alpha := filepath.Join(pluginRoot, "alpha")
	require.NoError(t, os.MkdirAll(filepath.Join(alpha, ".yanshi-plugin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(alpha, ".yanshi-plugin", "plugin.json"),
		[]byte(`{"name":"alpha","description":"d","version":"1.0"}`), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(alpha, "skills"), 0o755))
	writeSkill(t, filepath.Join(alpha, "skills"), "greet", "---\nname: greet\ndescription: hi\n---\nx")
	// plugin "beta" without a manifest → skipped
	require.NoError(t, os.MkdirAll(filepath.Join(pluginRoot, "beta", "skills"), 0o755))

	roots, err := DiscoverPlugins(pluginRoot)
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, "plugin:alpha", roots[0].Source)
}

func TestLoad_BuiltinShadowsUser(t *testing.T) {
	builtin := t.TempDir()
	user := t.TempDir()
	writeSkill(t, builtin, "shared", "---\nname: shared\ndescription: from-builtin\n---\nx")
	writeSkill(t, user, "shared", "---\nname: shared\ndescription: from-user\n---\nx")

	reg, err := NewLoader(Builtin(builtin), User(user)).Load()
	require.NoError(t, err)
	s, _ := reg.Get("shared")
	assert.Equal(t, "from-builtin", s.Description, "builtin (first root) wins")
}

func TestDiscoverPlugins_NonExistentRoot(t *testing.T) {
	roots, err := DiscoverPlugins(filepath.Join(t.TempDir(), "does-not-exist"))
	require.NoError(t, err)
	assert.Nil(t, roots)
}

func TestDiscoverPlugins_SkipsMalformedManifest(t *testing.T) {
	pluginRoot := t.TempDir()
	pDir := filepath.Join(pluginRoot, "p1")
	require.NoError(t, os.MkdirAll(filepath.Join(pDir, ".yanshi-plugin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pDir, ".yanshi-plugin", "plugin.json"),
		[]byte(`{bad json`), 0o644))

	roots, err := DiscoverPlugins(pluginRoot)
	require.NoError(t, err)
	assert.Empty(t, roots)
}

func TestDiscoverPlugins_SkipsNoSkillsDir(t *testing.T) {
	pluginRoot := t.TempDir()
	pDir := filepath.Join(pluginRoot, "p1")
	require.NoError(t, os.MkdirAll(filepath.Join(pDir, ".yanshi-plugin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pDir, ".yanshi-plugin", "plugin.json"),
		[]byte(`{"name":"p1","description":"d","version":"1.0"}`), 0o644))
	// NO skills/ dir created → should be skipped

	roots, err := DiscoverPlugins(pluginRoot)
	require.NoError(t, err)
	assert.Empty(t, roots)
}

func TestDiscoverPlugins_SkipsEmptyName(t *testing.T) {
	pluginRoot := t.TempDir()
	pDir := filepath.Join(pluginRoot, "p1")
	require.NoError(t, os.MkdirAll(filepath.Join(pDir, ".yanshi-plugin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pDir, ".yanshi-plugin", "plugin.json"),
		[]byte(`{"name":"","description":"d","version":"1.0"}`), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(pDir, "skills"), 0o755))
	writeSkill(t, filepath.Join(pDir, "skills"), "skipme", "---\nname: skipme\ndescription: should not appear\n---\nbody")

	roots, err := DiscoverPlugins(pluginRoot)
	require.NoError(t, err)
	assert.Empty(t, roots)
}

func TestDiscoverPlugins_LoadEndToEnd(t *testing.T) {
	pluginRoot := t.TempDir()
	alpha := filepath.Join(pluginRoot, "alpha")
	require.NoError(t, os.MkdirAll(filepath.Join(alpha, ".yanshi-plugin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(alpha, ".yanshi-plugin", "plugin.json"),
		[]byte(`{"name":"alpha","description":"A test plugin","version":"1.0"}`), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(alpha, "skills"), 0o755))
	writeSkill(t, filepath.Join(alpha, "skills"), "pskill", "---\nname: pskill\ndescription: from-plugin\n---\nbody")

	pluginRoots, err := DiscoverPlugins(pluginRoot)
	require.NoError(t, err)
	require.Len(t, pluginRoots, 1)

	reg, err := NewLoader(pluginRoots...).Load()
	require.NoError(t, err)
	s, ok := reg.Get("pskill")
	require.True(t, ok)
	assert.Equal(t, "plugin:alpha", s.Source)
	assert.Equal(t, "from-plugin", s.Description)
}

func TestLoad_BuiltinDevPacks(t *testing.T) {
	// Points at the repo's real skills directory (../../skills relative to the
	// internal/skills package dir). The bootstrap default builtin root is
	// "skills", so these packs must live as immediate children of skills/.
	reg, err := NewLoader(Builtin("../../skills")).Load()
	require.NoError(t, err)
	for _, name := range []string{"dev-quick-fix", "dev-standard-feature", "dev-designed-feature", "dev-team-feature", "dev-autonomous-project"} {
		s, ok := reg.Get(name)
		require.True(t, ok, "missing builtin skill %q", name)
		assert.NotEmpty(t, s.Description, "%q must have a description", name)
	}
}

// TestLoad_DefaultFlagsAndMarkersSurviveReload covers the default-enabled
// contract on Loader output (GB2) and .trusted/.disabled persistence across
// Reload (GB4).
func TestLoad_DefaultFlagsAndMarkersSurviveReload(t *testing.T) {
	root := t.TempDir()
	dir := writeSkill(t, root, "gated", "---\nname: gated\ndescription: gated-desc\n---\n# gated\n")
	loader := NewLoader(Builtin(root))
	reg, err := loader.Load()
	require.NoError(t, err)

	s, ok := reg.Get("gated")
	require.True(t, ok)
	assert.True(t, s.Enabled, "Loader output must default to Enabled=true when no .disabled marker exists")
	assert.False(t, s.Trusted, "Trusted must be false when no .trusted marker exists")

	require.NoError(t, os.WriteFile(filepath.Join(dir, ".trusted"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".disabled"), nil, 0o644))
	require.NoError(t, reg.Reload(loader))

	s, ok = reg.Get("gated")
	require.True(t, ok)
	assert.False(t, s.Enabled, ".disabled must survive Reload (GB4)")
	assert.True(t, s.Trusted, ".trusted must survive Reload")
}

// TestRegistry_MetaPromptOnlyListsEnabled proves MetaPrompt skips Enabled=false.
func TestRegistry_MetaPromptOnlyListsEnabled(t *testing.T) {
	root := t.TempDir()
	onDir := writeSkill(t, root, "on", "---\nname: on\ndescription: on-desc\n---\nbody\n")
	offDir := writeSkill(t, root, "off", "---\nname: off\ndescription: off-desc\n---\nbody\n")
	_ = onDir
	require.NoError(t, os.WriteFile(filepath.Join(offDir, ".disabled"), nil, 0o644))
	r, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)

	mp := r.MetaPrompt()
	assert.Contains(t, mp, "on-desc")
	assert.NotContains(t, mp, "off-desc")
}

// TestRegistry_ReloadPreservesAllLoaderRoots proves Reload uses the same
// original loader to rescan builtin+user+plugin roots (FN1).
func TestRegistry_ReloadPreservesAllLoaderRoots(t *testing.T) {
	builtin := t.TempDir()
	user := t.TempDir()
	plugin := t.TempDir()
	writeSkill(t, builtin, "built", "---\nname: built\ndescription: builtin-desc\n---\nbody\n")
	writeSkill(t, user, "personal", "---\nname: personal\ndescription: user-desc\n---\nbody\n")
	writeSkill(t, plugin, "plug", "---\nname: plug\ndescription: plugin-desc\n---\nbody\n")

	loader := NewLoader(Builtin(builtin), User(user), Plugin("demo", plugin))
	r, err := loader.Load()
	require.NoError(t, err)
	writeSkill(t, user, "second", "---\nname: second\ndescription: second-desc\n---\nbody\n")
	require.NoError(t, r.Reload(loader))

	for _, name := range []string{"built", "personal", "plug", "second"} {
		_, ok := r.Get(name)
		assert.True(t, ok, "Reload must preserve/load %s", name)
	}
}

// TestRegistry_GetAndListReturnCopies proves Get/List return snapshots (FN2).
func TestRegistry_GetAndListReturnCopies(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "safe", "---\nname: safe\ndescription: original\n---\nbody\n")
	r, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)

	got, ok := r.Get("safe")
	require.True(t, ok)
	got.Description = "mutated-via-get"
	list := r.List()
	require.Len(t, list, 1)
	list[0].Description = "mutated-via-list"

	again, ok := r.Get("safe")
	require.True(t, ok)
	assert.Equal(t, "original", again.Description)
}

// TestRegistry_ConcurrentReadReloadAndMutate covers FN2 under -race.
func TestRegistry_ConcurrentReadReloadAndMutate(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "race-safe", "---\nname: race-safe\ndescription: race-safe\n---\nbody\n")
	loader := NewLoader(Builtin(root))
	r, err := loader.Load()
	require.NoError(t, err)

	errCh := make(chan error, 300)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_, _ = r.Get("race-safe")
			_ = r.List()
			_ = r.MetaPrompt()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if err := r.Reload(loader); err != nil {
				errCh <- err
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if err := r.Disable("race-safe"); err != nil {
				errCh <- err
			}
			if err := r.Enable("race-safe"); err != nil {
				errCh <- err
			}
		}
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent registry operation: %v", err)
	}
}
