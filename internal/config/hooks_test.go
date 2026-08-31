package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestLoad_HooksFromYAML pins that every hooks key reaches its struct field.
// Pure plumbing between YAML and the orchestrator: yaml.v3 silently drops
// unknown or mistyped keys, so a missing yaml tag would leave the zero value,
// which for this block means "no hooks run" — an operator who configured a
// PreToolUse gate would get no gate and no error. Values are pairwise
// distinct so a copy-paste swap between entries cannot pass.
func TestLoad_HooksFromYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	require.NoError(t, os.WriteFile(p, []byte(
		"hooks:\n"+
			"  pre_tool_use:\n"+
			"    - program: /usr/local/bin/check-policy\n"+
			"      args: [\"--strict\", \"--project\", myproj]\n"+
			"      timeout: 1500ms\n"+
			"    - program: /usr/local/bin/audit-hook\n"+
			"      args: []\n"), 0o644))

	cfg, err := Load(p)
	require.NoError(t, err)

	require.Len(t, cfg.Hooks.PreToolUse, 2)
	first := cfg.Hooks.PreToolUse[0]
	require.Equal(t, "/usr/local/bin/check-policy", first.Program)
	require.Equal(t, []string{"--strict", "--project", "myproj"}, first.Args)
	require.Equal(t, 1500*time.Millisecond, first.Timeout)
	second := cfg.Hooks.PreToolUse[1]
	require.Equal(t, "/usr/local/bin/audit-hook", second.Program)
	require.Empty(t, second.Args)
	require.Equal(t, time.Duration(0), second.Timeout)
}

// TestLoad_HooksAbsentIsZero is the compatibility half, same decision as the
// loop_guard block: Load applies no defaults here, so an installation with no
// hooks block runs no hook processes at all and turns behave byte-identically
// to before the bus existed.
func TestLoad_HooksAbsentIsZero(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(p, []byte("server:\n  http_addr: 127.0.0.1:0\n"), 0o644))

	cfg, err := Load(p)
	require.NoError(t, err)
	require.Equal(t, HooksConfig{}, cfg.Hooks)
}

// TestLoad_HooksRejectsEmptyProgram pins the load-time half of the fail-closed
// ruling: a hook with no program would otherwise fail closed on EVERY tool
// call at runtime ("hook  failed: ..."), in every turn, forever — and the
// operator would have to read a turn transcript to learn why. The mistake is
// easy to make because yaml.v3 silently drops mistyped keys, so "program:"
// with the value at the wrong indentation decodes to an empty string. Load is
// the one moment the operator is guaranteed to be looking.
func TestLoad_HooksRejectsEmptyProgram(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	require.NoError(t, os.WriteFile(p, []byte(
		"hooks:\n"+
			"  pre_tool_use:\n"+
			"    - program: \"\"\n"), 0o644))

	_, err := Load(p)
	require.ErrorContains(t, err, "hooks.pre_tool_use[0]")
	require.ErrorContains(t, err, "program is required")
}

// TestLoad_HooksTimeoutRejectsBareNumbers documents the same time.Duration
// edge as loop_guard's turn_timeout: "1500ms" decodes, a bare "1500" does NOT
// and fails at load. A silent 1500ns hook budget would refuse every tool call
// with a timeout — fail-closed applied to a typo.
func TestLoad_HooksTimeoutRejectsBareNumbers(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	require.NoError(t, os.WriteFile(p, []byte(
		"hooks:\n  pre_tool_use:\n    - program: /bin/true\n      timeout: 1500\n"), 0o644))

	_, err := Load(p)
	require.Error(t, err, "a unitless hook timeout must fail loudly, not become 1500ns")
}

// TestLoad_CompactionHooksFromYAML 是压缩段（W-F-08）的 plumbing 对账，与
// TestLoad_HooksFromYAML 同一把尺子：yaml.v3 静默吞掉拼错的键，零值意味着
// 「没有 hook 运行」——操作员配置的压缩观察者无声消失。两段各自逐字段断言，
// 值两两不同，段间 copy-paste 互换过不了；timeout 只在 pre_compact 出现、
// args 只在 post_compact 出现，错位的字段映射会当场现形。
//
// 会变红的变异：把 pre_compact 的 yaml tag 拼错 → 本测试红（该段零值）；
// 把映射循环删掉 → 不影响本测试（这层只管 YAML），装配半边由 bootstrap 的
// hookswiring 测试钉。
func TestLoad_CompactionHooksFromYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	require.NoError(t, os.WriteFile(p, []byte(
		"hooks:\n"+
			"  pre_compact:\n"+
			"    - program: /opt/hooks/pre-compact\n"+
			"      timeout: 3s\n"+
			"  post_compact:\n"+
			"    - program: /opt/hooks/post-compact\n"+
			"      args: [\"--json\"]\n"+
			"    - program: /opt/hooks/post-compact-2\n"), 0o644))

	cfg, err := Load(p)
	require.NoError(t, err)

	require.Len(t, cfg.Hooks.PreCompact, 1)
	pre := cfg.Hooks.PreCompact[0]
	require.Equal(t, "/opt/hooks/pre-compact", pre.Program)
	require.Equal(t, 3*time.Second, pre.Timeout)

	require.Len(t, cfg.Hooks.PostCompact, 2)
	require.Equal(t, "/opt/hooks/post-compact", cfg.Hooks.PostCompact[0].Program)
	require.Equal(t, []string{"--json"}, cfg.Hooks.PostCompact[0].Args)
	require.Equal(t, "/opt/hooks/post-compact-2", cfg.Hooks.PostCompact[1].Program)
}

// TestLoad_CompactionHooksRejectEmptyProgram 把压缩段的空 program 加载期拒绝
// 钉住。与 pre_tool_use 的空 program 后果不同（那边 fail-closed 拒绝每次工具
// 调用，这边每次压缩各记一条日志），但诊断时机是同一条：加载期是操作员保证
// 在看的唯一时刻，缩错进的空值拖到运行期才显形就是一条无人看的日志。
// 会变红的变异：删掉 validateHooks 里的压缩段循环 → 本测试红。
func TestLoad_CompactionHooksRejectEmptyProgram(t *testing.T) {
	for _, section := range []string{"pre_compact", "post_compact"} {
		t.Run(section, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "c.yaml")
			require.NoError(t, os.WriteFile(p, []byte(
				"hooks:\n  "+section+":\n    - program: \"\"\n"), 0o644))

			_, err := Load(p)
			require.ErrorContains(t, err, "hooks."+section+"[0]")
			require.ErrorContains(t, err, "program is required")
		})
	}
}
