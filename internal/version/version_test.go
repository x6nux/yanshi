package version_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/version"
)

func TestParseSemver(t *testing.T) {
	cases := []struct {
		in    string
		major int
		minor int
		patch int
		pre   string
		build string
	}{
		{"1.0.0", 1, 0, 0, "", ""},
		{"v1.2.3", 1, 2, 3, "", ""}, // v 前缀剥离
		{"0.4.0", 0, 4, 0, "", ""},
		{"2.0.0-rc.1", 2, 0, 0, "rc.1", ""},
		{"1.0.0+build.7", 1, 0, 0, "", "build.7"},
		{"v3.1.4-beta+x86", 3, 1, 4, "beta", "x86"},
	}
	for _, c := range cases {
		got, err := version.Parse(c.in)
		require.NoError(t, err, "Parse(%q)", c.in)
		assert.Equal(t, c.major, got.Major, "Parse(%q).Major", c.in)
		assert.Equal(t, c.minor, got.Minor, "Parse(%q).Minor", c.in)
		assert.Equal(t, c.patch, got.Patch, "Parse(%q).Patch", c.in)
		assert.Equal(t, c.pre, got.Prerelease, "Parse(%q).Prerelease", c.in)
		assert.Equal(t, c.build, got.Build, "Parse(%q).Build", c.in)
	}
}

func TestParseRejectsNonSemver(t *testing.T) {
	for _, in := range []string{
		"", "1", "1.2", "1.2.3.4", "v", "01.2.3", "1.2.3-",
		"m9-cli-tui", "m1-foundation", "vX.Y.Z",
	} {
		_, err := version.Parse(in)
		require.Errorf(t, err, "Parse(%q) must be rejected", in)
	}
}

// TestParseRejectsMilestoneTags 证明 --match 'v[0-9]*' 的语义在纯字符串层可测：
// 里程碑标签 m1–m9 绝不是 semver，版本注入必须跳过它们。
func TestParseRejectsMilestoneTags(t *testing.T) {
	for _, tag := range []string{"m9-cli-tui", "m1-foundation"} {
		_, err := version.Parse(tag)
		require.Errorf(t, err, "milestone tag %q must not parse as semver", tag)
	}
}

// TestVersionIsOverridable 证明 Version 是 var 而非 const：release 构建经 ldflags
// 覆盖后消费侧能读到注入值。dev 默认值仍是 "0.4.0"。
func TestVersionIsOverridable(t *testing.T) {
	require.Equal(t, "0.4.0", version.Version, "dev default must stay 0.4.0")
	saved := version.Version
	defer func() { version.Version = saved }()
	version.Version = "1.0.0"
	assert.Equal(t, "1.0.0", version.Version, "Version must be a var so ldflags can patch it")
}

func TestParseStringRoundTrip(t *testing.T) {
	got, err := version.Parse("v1.2.3-rc.1+b2")
	require.NoError(t, err)
	assert.Equal(t, "1.2.3-rc.1+b2", got.String())
}
