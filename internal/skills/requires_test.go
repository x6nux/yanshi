package skills

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLookPath returns a LookPath replacement resolving only the named
// programs. It is a fake, not a mock: no expectations, no call recording —
// just a PATH the test fully controls, so probing behaviour is deterministic
// regardless of what the CI runner happens to have installed.
func fakeLookPath(present ...string) func(string) (string, error) {
	set := map[string]bool{}
	for _, p := range present {
		set[p] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "/fake/bin/" + name, nil
		}
		return "", errors.New("exec: \"" + name + "\": executable file not found in $PATH")
	}
}

// withLookPath installs a fake PATH resolver for the duration of the test.
func withLookPath(t *testing.T, present ...string) {
	t.Helper()
	orig := LookPath
	LookPath = fakeLookPath(present...)
	t.Cleanup(func() { LookPath = orig })
}

func TestValidateRequirements(t *testing.T) {
	cases := []struct {
		name    string
		reqs    []Requirement
		wantErr string // substring; "" means it must pass
	}{
		{name: "empty list is valid", reqs: nil},
		{name: "single bin", reqs: []Requirement{{Bin: "gh"}}},
		{name: "several bins", reqs: []Requirement{{Bin: "gh"}, {Bin: "ast-grep"}, {Bin: "rg"}}},
		{name: "dotted name", reqs: []Requirement{{Bin: "python3.12"}}},
		{
			name: "empty entry names no key",
			reqs: []Requirement{{}},
			// A `- foo: bar` entry with an unrecognized key unmarshals to a
			// zero Requirement, which is exactly this case: the pack meant
			// something and the loader understood nothing.
			wantErr: "no recognized requirement key",
		},
		{
			name:    "path separator",
			reqs:    []Requirement{{Bin: "/usr/bin/gh"}},
			wantErr: "not a bare program name",
		},
		{
			name:    "windows path separator",
			reqs:    []Requirement{{Bin: `C:\bin\gh.exe`}},
			wantErr: "not a bare program name",
		},
		{
			name:    "shell punctuation",
			reqs:    []Requirement{{Bin: "rm -rf /"}},
			wantErr: "not a bare program name",
		},
		{
			name:    "command substitution",
			reqs:    []Requirement{{Bin: "$(curl evil)"}},
			wantErr: "not a bare program name",
		},
		{
			name:    "over-long name",
			reqs:    []Requirement{{Bin: string(make([]byte, 0, 200)) + strRepeat("a", 200)}},
			wantErr: "not a bare program name",
		},
		{
			name:    "reports every bad entry, not just the first",
			reqs:    []Requirement{{Bin: "ok"}, {}, {Bin: "/abs/path"}},
			wantErr: "requires[2]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRequirements(tc.reqs)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestValidateRequirements_NamesEveryOffendingIndex proves the error is not
// first-failure-only: a pack author with three typos must learn all three in
// one round trip, or they stop declaring requirements at all.
func TestValidateRequirements_NamesEveryOffendingIndex(t *testing.T) {
	err := ValidateRequirements([]Requirement{{}, {Bin: "/x"}, {Bin: "ok"}, {Bin: "a b"}})
	require.Error(t, err)
	for _, want := range []string{"requires[0]", "requires[1]", "requires[3]"} {
		assert.Contains(t, err.Error(), want)
	}
	assert.NotContains(t, err.Error(), "requires[2]", "the valid entry must not be reported")
}

func TestProbeRequirements(t *testing.T) {
	cases := []struct {
		name    string
		present []string
		reqs    []Requirement
		want    []string
	}{
		{name: "no requirements probes nothing", reqs: nil, want: nil},
		{
			name:    "all present",
			present: []string{"gh", "rg"},
			reqs:    []Requirement{{Bin: "gh"}, {Bin: "rg"}},
			want:    nil,
		},
		{
			name:    "one missing",
			present: []string{"gh"},
			reqs:    []Requirement{{Bin: "gh"}, {Bin: "ast-grep"}},
			want:    []string{"ast-grep"},
		},
		{
			name:    "all missing, sorted",
			present: nil,
			reqs:    []Requirement{{Bin: "rg"}, {Bin: "ast-grep"}, {Bin: "gh"}},
			want:    []string{"ast-grep", "gh", "rg"},
		},
		{
			name:    "duplicates reported once",
			present: nil,
			reqs:    []Requirement{{Bin: "gh"}, {Bin: "gh"}},
			want:    []string{"gh"},
		},
		{
			name:    "empty bin is skipped, not reported as missing",
			present: nil,
			reqs:    []Requirement{{}},
			want:    nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withLookPath(t, tc.present...)
			assert.Equal(t, tc.want, ProbeRequirements(tc.reqs))
		})
	}
}

func TestMissingRequirementHint(t *testing.T) {
	assert.Equal(t, "", MissingRequirementHint("s", nil))
	assert.Equal(t, "", MissingRequirementHint("s", []string{}))
	h := MissingRequirementHint("codesearch", []string{"ast-grep", "rg"})
	assert.Contains(t, h, "codesearch")
	assert.Contains(t, h, "ast-grep, rg")
	assert.Contains(t, h, "Install")
}

func TestRequirementString(t *testing.T) {
	assert.Equal(t, "bin:gh", Requirement{Bin: "gh"}.String())
	assert.Equal(t, "(empty requirement)", Requirement{}.String())
}

// TestNormalizeRequirements_KeepsGoodDropsBad proves Load's contract: a pack
// with one typo'd entry keeps its valid requirements instead of losing the
// whole block (which would make an unusable skill look usable) or being
// dropped entirely (which would make a typo an uninstall).
func TestNormalizeRequirements_KeepsGoodDropsBad(t *testing.T) {
	kept, err := normalizeRequirements([]Requirement{{Bin: "gh"}, {}, {Bin: "rg"}})
	require.Error(t, err, "the caller must still be able to report the problem")
	assert.Equal(t, []Requirement{{Bin: "gh"}, {Bin: "rg"}}, kept)

	kept, err = normalizeRequirements([]Requirement{{Bin: "gh"}})
	require.NoError(t, err)
	assert.Equal(t, []Requirement{{Bin: "gh"}}, kept)

	kept, err = normalizeRequirements([]Requirement{{}})
	require.Error(t, err)
	assert.Nil(t, kept)
}

// --- frontmatter parsing ---

func TestParseSkillFile_ParsesRequires(t *testing.T) {
	fm, body, err := parseSkillFile([]byte(`---
name: codesearch
description: structural search
requires:
  - bin: ast-grep
  - bin: rg
---
# Body here
`))
	require.NoError(t, err)
	assert.Equal(t, "codesearch", fm.Name)
	assert.Equal(t, []Requirement{{Bin: "ast-grep"}, {Bin: "rg"}}, fm.Requires)
	assert.Equal(t, "# Body here", body)
}

// TestParseSkillFile_NoRequiresIsNil proves the overwhelmingly common pack —
// one with no requires: block at all — yields no requirements rather than one
// empty one, which would probe as permanently missing and hide every existing
// skill in the repo the moment this feature shipped.
func TestParseSkillFile_NoRequiresIsNil(t *testing.T) {
	fm, _, err := parseSkillFile([]byte("---\nname: plain\ndescription: d\n---\nbody"))
	require.NoError(t, err)
	assert.Nil(t, fm.Requires)
	assert.Nil(t, ProbeRequirements(fm.Requires))
}

// --- Load / Registry integration ---

// writeSkillFM creates dir/<name>/SKILL.md from frontmatter alone, wrapping it
// in the --- delimiters and appending a body. It complements skills_test.go's
// writeSkill (which takes the whole file) because every case here varies only
// the frontmatter.
func writeSkillFM(t *testing.T, root, name, frontmatter string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"),
		[]byte("---\n"+frontmatter+"\n---\nbody of "+name), 0o644))
	return dir
}

func TestLoad_PopulatesRequiresAndMissing(t *testing.T) {
	withLookPath(t, "gh")
	root := t.TempDir()
	writeSkillFM(t, root, "have-it", "name: have-it\ndescription: d\nrequires:\n  - bin: gh")
	writeSkillFM(t, root, "lack-it", "name: lack-it\ndescription: d\nrequires:\n  - bin: ast-grep")
	writeSkillFM(t, root, "plain", "name: plain\ndescription: d")

	reg, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)

	have, ok := reg.Get("have-it")
	require.True(t, ok)
	assert.Equal(t, []Requirement{{Bin: "gh"}}, have.Requires)
	assert.Empty(t, have.Missing)

	lack, ok := reg.Get("lack-it")
	require.True(t, ok)
	assert.Equal(t, []string{"ast-grep"}, lack.Missing)

	plain, ok := reg.Get("plain")
	require.True(t, ok)
	assert.Empty(t, plain.Requires)
	assert.Empty(t, plain.Missing)
}

// TestLoad_MalformedRequiresDoesNotDropTheSkill holds Load's stated contract
// ("one bad skill never fails the whole load") against the new field, and
// proves the malformed entry is not silently turned into a permanently
// missing requirement — which would hide the skill from the model forever.
func TestLoad_MalformedRequiresDoesNotDropTheSkill(t *testing.T) {
	withLookPath(t)
	root := t.TempDir()
	writeSkillFM(t, root, "typo", "name: typo\ndescription: d\nrequires:\n  - binary: gh")

	reg, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)
	s, ok := reg.Get("typo")
	require.True(t, ok, "a malformed requires: block must not drop the skill")
	assert.Empty(t, s.Missing, "an unparseable entry must not become a permanent missing requirement")
	assert.NotEmpty(t, reg.MetaPrompt(), "and must not hide the skill from the model")
}

// TestMetaPrompt_HidesSkillsWithMissingRequirements is the T6 assertion: the
// model's listing is what the requirement state gates. Without this the model
// picks a skill whose first instruction cannot run, spends a turn learning so,
// and has no way to have known.
func TestMetaPrompt_HidesSkillsWithMissingRequirements(t *testing.T) {
	withLookPath(t, "gh")
	root := t.TempDir()
	writeSkillFM(t, root, "usable", "name: usable\ndescription: works here\nrequires:\n  - bin: gh")
	writeSkillFM(t, root, "unusable", "name: unusable\ndescription: needs a missing tool\nrequires:\n  - bin: ast-grep")

	reg, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)
	mp := reg.MetaPrompt()
	assert.Contains(t, mp, "usable")
	assert.NotContains(t, mp, "unusable")
}

// TestMetaPrompt_EmptyWhenEveryOneIsUnavailable proves the filter degrades to
// the same "" the no-skills case produces, rather than emitting a header with
// nothing under it.
func TestMetaPrompt_EmptyWhenEveryOneIsUnavailable(t *testing.T) {
	withLookPath(t)
	root := t.TempDir()
	writeSkillFM(t, root, "nope", "name: nope\ndescription: d\nrequires:\n  - bin: ast-grep")
	reg, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)
	assert.Equal(t, "", reg.MetaPrompt())
}

// TestCloneSkill_DeepCopiesSlices proves Get/List hand out snapshots that
// cannot be used to mutate the registry. A shallow copy would alias the
// backing arrays, so appending to a returned skill's Missing would write into
// the live entry through exactly the pointer cloneSkill exists to protect.
func TestCloneSkill_DeepCopiesSlices(t *testing.T) {
	withLookPath(t)
	root := t.TempDir()
	writeSkillFM(t, root, "s", "name: s\ndescription: d\nrequires:\n  - bin: ast-grep\n  - bin: rg")
	reg, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)

	a, ok := reg.Get("s")
	require.True(t, ok)
	require.Len(t, a.Missing, 2)
	a.Missing[0] = "CLOBBERED"
	a.Requires[0] = Requirement{Bin: "CLOBBERED"}

	b, ok := reg.Get("s")
	require.True(t, ok)
	assert.Equal(t, []string{"ast-grep", "rg"}, b.Missing)
	assert.Equal(t, []Requirement{{Bin: "ast-grep"}, {Bin: "rg"}}, b.Requires)
}

// --- ValidateSkillDir / Install ---

func TestValidateSkillDir_RejectsMalformedRequires(t *testing.T) {
	root := t.TempDir()
	dir := writeSkillFM(t, root, "bad", "name: bad\ndescription: d\nrequires:\n  - bin: /usr/bin/gh")
	err := ValidateSkillDir(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a bare program name")
}

func TestValidateSkillDir_AcceptsGoodRequires(t *testing.T) {
	root := t.TempDir()
	dir := writeSkillFM(t, root, "good", "name: good\ndescription: d\nrequires:\n  - bin: gh")
	require.NoError(t, ValidateSkillDir(dir))
}

// TestInstall_RejectsMalformedRequires proves the git install path refuses a
// pack whose requirement block means nothing, rather than installing a skill
// the user can see in /skills and the model can never reach.
func TestInstall_RejectsMalformedRequires(t *testing.T) {
	// CloneStub copies <AsRemote>/<repo>, so the pack lives one level in.
	remote := t.TempDir()
	writeSkillFM(t, remote, "reqbad", "name: reqbad\ndescription: d\nrequires:\n  - bin: a b c")
	_, err := Install("github:fake/reqbad", filepath.Join(t.TempDir(), "dst"),
		&CloneStub{AsRemote: remote})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a bare program name")
}

// strRepeat is strings.Repeat without importing strings for one call site in
// a table entry.
func strRepeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
