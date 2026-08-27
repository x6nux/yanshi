package http

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/skills"
)

// skillsTestServer starts a WS server backed by a one-root skill registry
// rooted at userRoot, with the supplied Fetcher wired in.
func skillsTestServer(t *testing.T, userRoot string, fetch skills.Fetcher) (*httptest.Server, *skills.Registry) {
	t.Helper()
	loader := skills.NewLoader(skills.User(userRoot))
	reg, err := loader.Load()
	require.NoError(t, err)
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"ok"}, nil)})
	require.NoError(t, err)
	srv := New(Config{
		Token:          "t",
		SkillsRegistry: reg,
		SkillsLoader:   loader,
		SkillsDstRoot:  userRoot,
		SkillsFetcher:  fetch,
	})
	srv.ChatWS(o, nil, reg)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, reg
}

// memFetcher serves canned archive bytes, so the HTTP install path runs with
// no network and no server.
type memFetcher struct {
	body   []byte
	gotURL string
}

// Fetch returns the canned body and records the requested URL.
func (m *memFetcher) Fetch(_ context.Context, rawURL string) ([]byte, string, error) {
	m.gotURL = rawURL
	return m.body, "application/gzip", nil
}

// tgzSkill builds a one-file tar.gz carrying a SKILL.md with the given
// frontmatter.
func tgzSkill(t *testing.T, frontmatter string) []byte {
	t.Helper()
	content := "---\n" + frontmatter + "\n---\n# body\n"
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "SKILL.md", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content)),
	}))
	_, err := tw.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// TestChatWS_InstallSkillFromHTTPURL proves the second registry is reachable
// through the SAME install verb as the git one. Adding it as a separate frame
// would give the TUI, the handler and the docs three chances to disagree about
// what is installable, and the disagreement would be found by a user typing a
// URL into a verb that cannot take one.
func TestChatWS_InstallSkillFromHTTPURL(t *testing.T) {
	userRoot := t.TempDir()
	f := &memFetcher{body: tgzSkill(t, "name: mirrored\ndescription: from an internal mirror")}
	ts, _ := skillsTestServer(t, userRoot, f)

	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewInstallSkill("https://mirror.internal/packs/mirrored.tgz")))
	ack := readFrame(t, c)
	require.Equal(t, "skill_ack", ack.Type)
	assert.Empty(t, ack.Text, "install must succeed")
	assert.Equal(t, "installed", ack.Action)
	require.NotNil(t, ack.Skill)
	assert.Equal(t, "mirrored", ack.Skill.Name)
	assert.Equal(t, "https://mirror.internal/packs/mirrored.tgz", f.gotURL)
	assert.FileExists(t, filepath.Join(userRoot, "mirrored", "SKILL.md"))

	// And it is live in the registry, not merely on disk.
	require.NoError(t, c.WriteJSON(proto.NewListSkills()))
	listed := readFrame(t, c)
	require.Equal(t, "skills_list", listed.Type)
	_, ok := skillFrameMap(listed)["mirrored"]
	assert.True(t, ok, "the installed skill must appear in the list after reload")
}

// TestChatWS_InstallSkillFromPlaintextURLIsRefused proves the transport
// requirement survives the WS layer: the payload is code the model will be
// told to run, so a plaintext channel is refused rather than upgraded.
func TestChatWS_InstallSkillFromPlaintextURLIsRefused(t *testing.T) {
	userRoot := t.TempDir()
	ts, _ := skillsTestServer(t, userRoot, &memFetcher{body: tgzSkill(t, "name: x\ndescription: d")})

	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewInstallSkill("http://mirror.internal/p.tgz")))
	ack := readFrame(t, c)
	require.Equal(t, "skill_ack", ack.Type)
	assert.Contains(t, ack.Text, "refusing scheme",
		"the refusal must name the objection, not fall through to the git path's "+
			"'only github: supported', which describes neither the problem nor the fix")
	entries, err := os.ReadDir(userRoot)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing may be published for a refused source")
}

// TestChatWS_SkillsListCarriesMissingRequirements proves the requirement state
// crosses the wire. It is computed on the BACKEND — in remote mode the TUI
// runs on a different machine, so a client-side PATH probe would answer a
// question nobody asked — and /skills is the only place a user can learn why
// an installed skill is never used.
func TestChatWS_SkillsListCarriesMissingRequirements(t *testing.T) {
	orig := skills.LookPath
	skills.LookPath = func(name string) (string, error) {
		if name == "gh" {
			return "/fake/bin/gh", nil
		}
		return "", os.ErrNotExist
	}
	t.Cleanup(func() { skills.LookPath = orig })

	userRoot := t.TempDir()
	writeWSSkillFM(t, userRoot, "haveit", "name: haveit\ndescription: d\nrequires:\n  - bin: gh")
	writeWSSkillFM(t, userRoot, "lackit", "name: lackit\ndescription: d\nrequires:\n  - bin: ast-grep")
	writeWSSkill(t, userRoot, "plain")

	ts, _ := skillsTestServer(t, userRoot, nil)
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewListSkills()))
	listed := readFrame(t, c)
	require.Equal(t, "skills_list", listed.Type)
	byName := skillFrameMap(listed)

	require.Contains(t, byName, "lackit")
	assert.Equal(t, []string{"ast-grep"}, byName["lackit"].Missing)
	require.Contains(t, byName, "haveit")
	assert.Empty(t, byName["haveit"].Missing)
	require.Contains(t, byName, "plain")
	assert.Empty(t, byName["plain"].Missing,
		"a skill that declares nothing must not acquire a requirement")
}

// writeWSSkillFM writes root/<name>/SKILL.md from frontmatter alone.
func writeWSSkillFM(t *testing.T, root, name, frontmatter string) {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"),
		[]byte("---\n"+frontmatter+"\n---\n# "+name+"\n"), 0o644))
}
