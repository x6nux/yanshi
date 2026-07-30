# Batch H1 — 发布工程（版本 / CI 门禁 / 多平台打包 / 升级兼容）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` 逐 Task 实施。每完成一个 Task 先跑该 Task 的定向验证再提交；不要把多个 Task 合成一个大提交。Go 代码 Task 走严格 RED→GREEN（先写失败测试、跑一次确认 RED，再补实现转 GREEN）；CI workflow / goreleaser / shell / 纯文档 Task 用对应的 dry-run 验证器（`act --list`、`goreleaser check`、`bash -n`），最终 GREEN 以真实 PR / tag / snapshot 构建为准。

**Goal:** 把 yanshi 从"能 `go build` 出单二进制"变成"可重复发布、跨平台分发、可升级、发布前可自检"的 v1.0 工程基线，覆盖 spec 的四块：VER1（semver + CHANGELOG 自动化）、CIG1（CI 门禁矩阵）、PKG1（多平台打包 + checksum）、UPG1（config schema 版本化 + release doctor）。本批不改任何产品功能行为（agent / orchestrator / guard / VCS 核心不动）。

**Spec:** `docs/superpowers/specs/2026-07-22-h1-release-engineering-design.md`（权威）。本计划的所有设计取舍以此 spec 为准；以下"已锁定决策"覆盖 spec §12 的 Open questions。

## 已锁定决策（团队 lead 直接给定，不再 brainstorm）

| OQ | spec 倾向 | **本计划锁定** |
|---|---|---|
| OQ1 CHANGELOG 工具 | git-cliff | **git-cliff**（CI-only 外部二进制；不进 go.mod、不进本地必需工具链） |
| OQ2 版本来源 | 常量 + tag 覆盖 | **常量回落 + git tag 注入**（dev 回落 `version.go` 的 `0.4.0`；发布从 `git describe --tags --abbrev=0 --match 'v[0-9]*'` 注入） |
| OQ3 keyring 变体 | 待定 | **v1 不进**：默认 `-tags nokeyring` + `CGO_ENABLED=0` 四目标一把交叉编译；darwin keyring 变体延后（go-keyring darwin 端已纯 Go，但 v1 暂不进 keyring 变体） |
| OQ4 CI 托管 | GHA | **GitHub-hosted**（`ubuntu`/`windows`/`macos`-latest） |
| OQ5 release 触发 | tag 自动 | **tag `v*` 自动触发** `release.yml` |
| OQ6 doctor flag | 可选 | **加 `--release` flag**（把 release-blocking 的 warn 提级 fail，如 port-in-use / config-version 异常） |

**关于 OQ3 的简化影响（关键）**：keyring 变体出局后，PKG1 不需要 native macos runner（go-keyring darwin 端已是纯 Go，shell 到 `/usr/bin/security`，无 `import "C"`）、不需要 darwin cgo 路径、不需要 `-keyring` 后缀产物。`nokeyring`（`internal/secrets/keyring_disabled.go`，`//go:build nokeyring`）把 `zalando/go-keyring` 整个摘掉 → 四目标（windows/amd64、linux/amd64、linux/arm64、darwin/arm64）从单一 ubuntu host `CGO_ENABLED=0` 一把交叉编译。`modernc.org/sqlite`（纯 Go）、bubbletea fork（纯 Go，含 Windows 的 `key_windows.go`，走 `golang.org/x/sys/windows` 无 cgo）都不挡交叉编译。

**CIG1 拥有 `.github/workflows/`**（目录现不存在，从零建 `ci.yml` + `nightly.yml`）。`release.yml` 由 PKG1 追加到同目录。CIG1 负责把 E2（race/fuzz/property）、E3（GOV1/GOV2/GOV3）、F2（bench）的测试产物接进 CI；这些 batch 未就绪时对应 job **软启用**（guard 步骤 + `continue-on-error`），就绪后去掉软开关转硬门禁。

---

## 关键技术事实（实测落点，2026-07-22）

执行每个 Task 前先 `git log -1` + grep 实测确认下列落点未被同批（D3/F1）改写。

- **`internal/version/version.go:8`**：`const Version = "0.4.0"`（**const，不是 var**）。`BuildStamp`（`:19`）已是 `var`（ldflags 可注入）；`GitHash`（`:29`）是 `var` 经 `func()` 初始化。**`-ldflags -X` 只能 patch string `var`，不能 patch `const`** —— 所以 VER1 必须把 `const Version` 改成 `var Version`，否则 PKG1 的版本注入不生效。这是 VER1→PKG1 的硬内部依赖。
- **`cmd/yanshi/version.go:7`**：`var Version = version.Version`（package-level 初始化，读 `version.Version`）。ldflags 在 init 之前 patch `version.Version`，故 `main.Version` 拷到的是已注入值。改 const→var 后此文件无需改动。
- **`cmd/yanshi/main.go:80-81`**：`--version` → `fmt.Println("yanshi", Version)`。`usage`（`:38`）拼 `Version`。消费侧零改动。
- **`internal/cli/tui/startup.go:231`**：`ver := "v" + version.Version`，再拼 `BuildStamp` / `GitHash`。消费侧零改动。
- **`build.sh`**：现只注入 `BuildStamp`（`-X ...BuildStamp=$(date +%y%m%d%H%M)`），产 `yanshi.exe`。无 Version 注入、无 release 路径。
- **`internal/config/config.go`**：`Config` struct（`:42`）顶层**无** `schema_version`（grep 无命中）。`LoadBytes`（`:341`）顺序：`os.ExpandEnv` → `yaml.Unmarshal` → `cfg.applyDefaults()` → `validateAPIKeyRefs` → `validate`。A-D 全是 additive，缺字段向前兼容。`Load`（`:332`）读文件后转 `LoadBytes`。
- **`internal/cli/doctor.go`**：
  - `DoctorOptions`（`:51`）字段 `{ConfigPath, Root}`，需加 `Release bool`。
  - `RunDoctor`（`:135`）按固定顺序串联 15 项 check（config/database/providers/acp/lockfile/port/directories/sandbox/mcp/lsp/permissions/secrets/locale/keymap/high-contrast）。
  - `ExitCode()`（`:60`）返回 0/1/2（ok / 有 warn / 有 fail）。
  - `checkPort`（`:322`）`net.Listen`+`Close`（只读、瞬时）；`checkDatabase`（`:206`）`store.Open`+`Close`；`checkSecretsRefs`（`:461`）校验 ref —— 均为只读/瞬时副作用，是新检查的安全基线。
  - `checkSandbox`（`:388`）占位（"arrives with S08 in M2"）—— release doctor **如实保留，不假装已验证**。
- **`internal/secrets/keyring_enabled.go:21`**：`Available() error`（sentinel `Get`，只读）。`keyring_disabled.go:13`：`Available()` 返回 `ErrKeyringUnavailable`。`checkKeyringAvailability` 直接复用 `Available()`。
- **`go-keyring v0.2.6` darwin 端实测**：`keyring_darwin.go` **无** `import "C"`，通过 `os/exec` shell 到 `/usr/bin/security`。darwin keyring 变体**不需要 cgo** 或 native macos runner。本计划默认 `CGO_ENABLED=0` × 四目标正确；此注释仅用于避免未来维护者引入 native macos runner / cgo 工具链。
- **git tags**：仅有 `m1-foundation`…`m9-cli-tui`（里程碑，非 semver）；`git describe` 当前 = `m9-cli-tui-…`。**无任何 `v*` tag**。VER1 新立 semver 命名空间，tag 匹配必须用 `v[0-9]*` glob 跳过 m1–m9。
- **提交已 conventional**：`feat(doctor):` / `fix(config):` / `chore(d2):` / `feat(ide-vscode):` —— prefix + scope 已成型，CHANGELOG 自动化输入干净。
- **`.github/workflows/`**：目录不存在（实测）。
- **`go.mod`**：`zalando/go-keyring v0.2.6`（`:20`）；`modernc.org/sqlite v1.53.0`（纯 Go）；`replace github.com/charmbracelet/bubbletea => ./third_party/bubbletea`（fork 已 tracked，CI/goreleaser 直接解析，CLAUDE.md 禁止去掉 replace）。

---

## Architecture / 落点设计

- **版本号唯一来源仍是 `internal/version`**：dev 构建回落常量值 `0.4.0`（改 const→var 但默认值不变）；发布构建由 ldflags 覆盖为 semver tag（goreleaser / `build.sh release` 两路都走 `-X version.Version=<tag>`）。消费侧（`--version`、banner）零改动。
- **CHANGELOG 由 git-cliff 在 CI 生成**：`cliff.toml` 定义 conventional-commit 分组 + `tag_pattern = "v[0-9]*"`（与版本注入的 `--match 'v[0-9]*]'` 同命名空间）；`release.yml` 跑 `git-cliff --latest` 产出 release notes 喂给 goreleaser，并回写 `CHANGELOG.md`。git-cliff **不**进 go.mod、**不**要求本地安装（CI-only）。
- **CI 分两层控时**：`ci.yml`（PR / push main，快路径，必跑阻合并）+ `nightly.yml`（schedule，慢路径，长 fuzz + bench + 全量 race，不阻合并）。race detector 是编译器 flag，`go test -race ./...` 对任意 Go 代码都生效 → race job **硬门禁**；governance（E3 GOV）/ fuzz-seed（E2 FUZ/PROP）/ bench（F2）需对应 batch 的测试文件先存在 → **软启用**。
- **打包只走一条最稳路径**：四目标全 `CGO_ENABLED=0` + `-tags nokeyring`，单 host（ubuntu）交叉编译；goreleaser 产 archive + SHA256 `checksums.txt`；`release.yml` `on: push: tags: ['v*']` 自动 release。
- **config schema 版本化是前瞻性铺路**：v1 无破坏性变更，`migrateConfig` 是 no-op 占位；`Load` 做版本门（超高拒、缺字段=1），**不**静默改磁盘文件。
- **doctor 全检查只读**：新检查复用既有 `Listen+Close` / `Open+Close` / sentinel `Get` 模式；`--release` 把 release-blocking warn 提级 fail。`checkSandbox` 占位如实保留。

**Tech Stack:** Go 1.26.4；GitHub Actions（`actions/checkout@v4` / `actions/setup-go@v5` cache:true）；goreleaser v2（`goreleaser-action`）；git-cliff（CI-only）；`act`（本地 workflow dry-run，可选）；`modernc.org/sqlite`（纯 Go，doctor `PRAGMA` 查询）；标准库 `net`/`os`/`database/sql`（doctor 只读探针）。

## 约束、依赖与非目标

### 跨批依赖门禁

| H1 功能 | 前置批次 | H1 实际依赖 | 软/硬 | 禁止做法 |
|---|---|---|---|---|
| CIG1 race job | — | `go test -race`（编译器 flag，对任意代码生效） | 硬 | 不伪造 RAC1 测试资产；race job 不软启用 |
| CIG1 governance job | **E3** | GOV1/GOV2/GOV3 测试文件存在 | **软**（guard + continue-on-error，E3 落地后转硬） | 不在没有 GOV 测试时把 job 写成必失败；不假装已通过 |
| CIG1 fuzz-seed job | **E2** | FUZ1 种子语料 + PROP1 属性测试 | **软** | 同上 |
| nightly fuzz-long | **E2** | FUZ1 `Fuzz*` target | **软** | — |
| nightly bench | **F2** | `go test -bench` target + benchstat | **软** | 不做硬门禁（仅趋势） |
| UPG1 checkWAL | **F1**（WAL1） | `PRAGMA journal_mode` 读回 `wal` | **软**（F1 前 warn，不 fail） | 不在 F1 前假装 WAL 已开；只读不切模式 |
| VER1 → PKG1 | VER1 | `version.Version` 是 `var`（ldflags 可注入） + `version.Parse` 校验 tag | 硬 | 不在 const 上注入；不注入非 semver tag |

### 非目标（v1 不做，见 spec §1/§11）

- Homebrew / Scoop / apt / rpm 包（留后续）。
- 自动签名 / 公证（macOS notarization、Windows code-sign）—— 产物附 checksum 但不签名。
- darwin keyring 变体（v1 暂不进 keyring 变体；go-keyring darwin 端已是纯 Go，但双变体维护成本超过 v1 收益）—— 延后到 v1 之后。
- 完整 sandbox 自检（归 S08/M2；doctor `checkSandbox` 如实占位）。
- 重写历史 commit；引入 golangci-lint；引入 commit-lint 自动强制。
- config 迁移的真实首例（第一次破坏性变更时才 bump schema + 加 migration）。

## File Structure

| 文件 | 职责 | 新/改 |
|---|---|---|
| `internal/version/version.go` | `const`→`var Version`；`Parse`/`Semver` 助手 | 改 |
| `internal/version/version_test.go` | semver 解析、v 前缀剥离、跳过 m1–m9、可注入性 | 新 |
| `build.sh` | 加 release 路径：`git describe --match 'v[0-9]*'` 注入 `-X version.Version` | 改 |
| `docs/commit-convention.md` | conventional commit 子集 + scope 约定 | 新 |
| `cliff.toml` | git-cliff 配置（分组/scope/breaking/`tag_pattern`） | 新 |
| `CHANGELOG.md` | 发布日志种子（首条 + 指引） | 新 |
| `.github/workflows/ci.yml` | PR/push：test/vet/race/build/governance/fuzz-seed（分层、软启用） | 新 |
| `.github/workflows/nightly.yml` | schedule：长 fuzz + bench + 全量 race（软启用） | 新 |
| `.github/workflows/release.yml` | tag `v*`：git-cliff notes + goreleaser 四目标 + checksum | 新 |
| `.goreleaser.yaml` | 构建矩阵（4 目标 nokeyring）+ ldflags + checksum + archive | 新 |
| `internal/config/config.go` | `Config.SchemaVersion` + `SupportedSchemaVersion` + `Load` 版本门/`migrateConfig` | 改 |
| `internal/config/config_test.go` | schema 版本门/默认值/迁移/不写磁盘 | 改 |
| `config.example.yaml` | `schema_version: 1` 示例 + 说明 | 改 |
| `internal/cli/doctor.go` | `DoctorOptions.Release` + `checkConfigVersion`/`checkWAL`/`checkKeyringAvailability` + `--release` 提级 | 改 |
| `internal/cli/doctor_test.go` | 新检查 ok/warn/fail + 只读断言 + `--release` 提级 | 改 |
| `cmd/yanshi/main.go` | doctor 子命令加 `-release` flag 透传 `DoctorOptions.Release` | 改 |
| `docs/upgrade-guide.md` | schema 机制 + 升级步骤 + breaking 标注 + release runbook | 新 |

---

## 任务依赖图

```
T1 (VER1 version.Parse + const→var)
  ├─→ T2 (VER1 build.sh release + commit-convention)
  │     └─→ T3 (VER1 cliff.toml + CHANGELOG seed)
  │           └─→ T9 (PKG1 goreleaser + release.yml)   ◀── VER1→PKG1 硬依赖
  └────────────────────────────────────────────────────┘ (T1 ldflags 也直供 T9)

T4 (CIG1 ci.yml core: test/vet/race/build)
  ├─→ T5 (CIG1 ci.yml governance[f←E3] + fuzz-seed[f←E2])   软启用
  └─→ T6 (CIG1 nightly.yml fuzz-long[f←E2] + bench[f←F2])   软启用

T7 (UPG1 config schema_version + Load 版本门)
  └─→ T8 (UPG1 doctor --release + checkConfigVersion/checkWAL[f←F1]/checkKeyringAvailability)

跨批软依赖：T5←E3/E2 · T6←E2/F2 · T8.checkWAL←F1
```

**关键路径**：T1→T2→T3→T9（VER1→PKG1）。**独立支线**：CIG1（T4→T5/T6）、UPG1（T7→T8）彼此无依赖，可并行。

---

## Task 1 — VER1：`version.Parse` + `const Version`→`var Version`

**Files:** `internal/version/version.go`（改）、`internal/version/version_test.go`（新）。

**为何 const→var：** `-ldflags -X github.com/x6nux/yanshi/internal/version.Version=...` 只能 patch 字符串 `var`，对 `const` 静默无效。不改这一行，PKG1 的 `{{.Version}}` 注入永远显示常量 `0.4.0`。默认值不变（dev 仍回落 `0.4.0`），只是把存储从编译期常量改成链接期可覆盖变量。

### Step 1.1 — 写失败的测试

```go
// internal/version/version_test.go
package version_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/version"
)

func TestParseSemver(t *testing.T) {
	cases := []struct {
		in        string
		major     int
		minor     int
		patch     int
		pre       string
		build     string
	}{
		{"1.0.0", 1, 0, 0, "", ""},
		{"v1.2.3", 1, 2, 3, "", ""},          // v 前缀剥离
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
```

**Expected failure:** `undefined: version.Parse`、`undefined: version.Semver`；`TestVersionIsOverridable` 在 const 上无法赋值 → 编译错误 `cannot assign to version.Version`（这本身就是 RED 的证据：const 不可赋值）。

### Step 1.2 — 实现

```go
// internal/version/version.go （在现有文件追加；const Version 行改为 var）
package version

import (
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
)

// Version is the current yanshi version. Bump this before each release.
//
// This is a var (not a const) so release builds can override it via ldflags:
//
//	go build -ldflags "-X github.com/x6nux/yanshi/internal/version.Version=1.0.0"
//
// dev builds (`go build` / `go run`) keep the default "0.4.0" below; release
// builds (goreleaser / `build.sh release`) inject the semver git tag instead.
// -X only patches string vars, never consts, hence the var declaration.
var Version = "0.4.0"

// （BuildStamp / GitHash 保持原样，不动。）

// Semver is a parsed semantic version. Prerelease/Build are the raw substrings
// after '-' / '+' (empty when absent). Parse rejects milestone tags like
// m9-cli-tui so the release path's `git describe --match 'v[0-9]*'` semantics
// are testable without git.
type Semver struct {
	Major      int
	Minor      int
	Patch      int
	Prerelease string
	Build      string
}

// Parse parses a semver string, tolerating an optional leading "v". Returns an
// error for milestone tags, missing segments, leading zeros, or non-numeric
// version components. Used by the release path to validate that a git tag is a
// real semver before injecting it via ldflags.
func Parse(v string) (Semver, error) {
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return Semver{}, fmt.Errorf("version: empty")
	}
	// Split build metadata first (+ is not legal inside pre-release).
	var build string
	if i := strings.IndexByte(v, '+'); i >= 0 {
		build = v[i+1:]
		v = v[:i]
		if build == "" {
			return Semver{}, fmt.Errorf("version: empty build metadata")
		}
	}
	var pre string
	if i := strings.IndexByte(v, '-'); i >= 0 {
		pre = v[i+1:]
		v = v[:i]
		if pre == "" {
			return Semver{}, fmt.Errorf("version: empty pre-release")
		}
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return Semver{}, fmt.Errorf("version: want major.minor.patch, got %q", v)
	}
	var s Semver
	var err error
	if s.Major, err = parseNum(parts[0]); err != nil {
		return Semver{}, fmt.Errorf("version: major: %w", err)
	}
	if s.Minor, err = parseNum(parts[1]); err != nil {
		return Semver{}, fmt.Errorf("version: minor: %w", err)
	}
	if s.Patch, err = parseNum(parts[2]); err != nil {
		return Semver{}, fmt.Errorf("version: patch: %w", err)
	}
	s.Prerelease = pre
	s.Build = build
	return s, nil
}

// parseNum rejects non-numeric and leading-zero components ("01" is illegal in
// semver; "0" alone is fine).
func parseNum(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty component")
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, fmt.Errorf("leading zero in %q", s)
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("non-numeric %q", s)
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// String renders the canonical form (no "v" prefix).
func (s Semver) String() string {
	out := fmt.Sprintf("%d.%d.%d", s.Major, s.Minor, s.Patch)
	if s.Prerelease != "" {
		out += "-" + s.Prerelease
	}
	if s.Build != "" {
		out += "+" + s.Build
	}
	return out
}
```

**Run:**

```sh
go test ./internal/version -run 'TestParse|TestVersionIsOverridable' -v
go vet ./internal/version
```

**Expected:** 全部通过；`TestVersionIsOverridable` 赋值成功（证明已 var 化）。回归 `go build ./cmd/yanshi` + `./yanshi.exe -h` exit 0（确认 const→var 不破坏消费侧）。

**Commit:** `feat(version): parse semver and make Version overridable via ldflags`

---

## Task 2 — VER1：`build.sh` release 注入 + commit 规范文档

**Files:** `build.sh`（改）、`docs/commit-convention.md`（新）。

`build.sh` 保留为 dev 工具；新增 release 路径（`./build.sh release` 或 `RELEASE=1 ./build.sh`）：从 `git describe --match 'v[0-9]*'` 取 semver tag，用 `version.Parse` 语义校验（bash 层做正则校验），注入 `-X version.Version` + `-X version.BuildStamp`。无 `v*` tag 时回落到 dev 默认（不注入 Version）——与 OQ2 锁定一致。

### Step 2.1 — 写失败/验证（shell dry-run）

`build.sh` 无单测框架；RED = 语法/行为校验脚本。新增一个内联自检（不改产品）：执行 `bash -n build.sh`（语法）+ 模拟 release 路径在"无 v tag"时回落、在"有 v tag"时注入。

```sh
# 语法 RED（先跑，确认脚本本身可解析）：
bash -n build.sh

# 行为 smoke（dev 路径，不注入 Version）：
./build.sh && ./yanshi.exe --version    # 期望: yanshi 0.4.0（dev 回落常量）

# 行为 smoke（release 路径，临时打 v tag 验证注入；测完删除 tag）：
git tag v0.4.0-test 2>/dev/null || true
./build.sh release && ./yanshi.exe --version  # 期望: yanshi 0.4.0-test
git tag -d v0.4.0-test 2>/dev/null || true
```

**Expected RED:** 改之前 `./build.sh release` 不存在（脚本忽略未知参数）→ Version 仍显示 `0.4.0`。

### Step 2.2 — 实现 `build.sh`

```sh
#!/usr/bin/env bash
# Build yanshi. Two modes:
#   ./build.sh            dev build: inject only BuildStamp; Version stays at the
#                         in-source default (0.4.0). This is what `go build` users get.
#   ./build.sh release    release build: inject Version from the latest semver git
#                         tag (`git describe --tags --abbrev=0 --match 'v[0-9]*'`),
#                         stripping the leading "v". Falls back to the dev default
#                         when no v* tag exists. CI releases use goreleaser, not
#                         this script — but this path keeps a local release build
#                         reproducible without it. Requires bash (Git Bash on Windows).
set -euo pipefail

stamp=$(date +%y%m%d%H%M)
ldflags="-X github.com/x6nux/yanshi/internal/version.BuildStamp=${stamp}"
out=yanshi.exe

if [[ "${1:-}" == "release" || "${RELEASE:-}" == "1" ]]; then
  # --match 'v[0-9]*' skips the m1..m9 milestone tag namespace.
  if ver=$(git describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null); then
    ver=${ver#v}
    ldflags="${ldflags} -X github.com/x6nux/yanshi/internal/version.Version=${ver}"
    echo "release build: version=${ver} stamp=${stamp}"
  else
    echo "release build: no v* semver tag found; falling back to in-source default"
  fi
else
  echo "dev build: stamp=${stamp}"
fi

go build -ldflags "${ldflags}" -o "${out}" ./cmd/yanshi
echo "built ${out}"
```

### Step 2.3 — `docs/commit-convention.md`

```markdown
# Commit Convention

yanshi uses a [Conventional Commits](https://www.conventionalcommits.org/) subset.
The CHANGELOG is generated from these prefixes by git-cliff (`cliff.toml`),
so a clean prefix + scope keeps the release notes accurate. This is enforced by
review and CHANGELOG proofreading, **not** by a commit-lint tool (the repo ships
no golangci-lint config).

## Prefixes

| prefix | meaning | CHANGELOG group |
|---|---|---|
| `feat` | new user-facing capability | Features |
| `feat!` / `fix!` | breaking change (or footer `BREAKING CHANGE:`) | ⚠ Breaking Changes |
| `fix` | bug fix | Bug Fixes |
| `perf` | performance improvement | Performance |
| `refactor` | code restructure, no behavior change | Refactor |
| `docs` | documentation only | Documentation |
| `test` | test-only change | Tests |
| `chore` / `ci` / `build` | tooling, CI, build | Maintenance |
| `revert` | revert a prior commit | Reverted |

`chore(release): ...` is skipped by git-cliff (release commits don't clutter the log).

## Scope

Use a domain code matching the area of the codebase, consistent with existing
history: `doctor`, `config`, `auth`, `secrets`, `orchestrator`, `vcs`, `version`,
`bootstrap`, `ide-vscode`, `tui`, `guard`, etc.

## Examples

    feat(version): parse semver and make Version overridable via ldflags
    fix(doctor): keep sandbox check honest about S08 gap
    feat(config)!: require schema_version on load

    BREAKING CHANGE: configs without schema_version now default to 1 and warn.

## Breaking changes

Prefer the `!` form (`feat(scope)!: ...`). If a multi-line justification is
needed, add a `BREAKING CHANGE:` footer — git-cliff groups either form under
⚠ Breaking Changes. A breaking change must bump `SupportedSchemaVersion` in
`internal/config/config.go` and be called out in `docs/upgrade-guide.md`.
```

**Run / Expected:** `bash -n build.sh` exit 0；dev smoke `yanshi 0.4.0`；release smoke（临时 v tag）`yanshi 0.4.0-test`；文档存在且引用 `cliff.toml` / `SupportedSchemaVersion`。

**Commit:** `feat(build): inject version from semver tag in release path`

---

## Task 3 — VER1：`cliff.toml` + `CHANGELOG.md` 种子

**Files:** `cliff.toml`（新）、`CHANGELOG.md`（新）。

git-cliff 是 CI-only 外部二进制（不进 go.mod、本地可选安装）。本 Task 只产出配置 + 种子文件；真正的 `git-cliff` 执行在 Task 9 的 `release.yml` 里。本地验证（装了 git-cliff 才能跑）：`git-cliff --config cliff.toml --unreleased --dry-run` 应按分组输出、且只匹配 `v[0-9]*` tag（跳过 m1–m9）。

### Step 3.1 — `cliff.toml`

```toml
# git-cliff configuration for yanshi. Run by .github/workflows/release.yml to
# generate release notes and maintain CHANGELOG.md. Not a build dependency:
# git-cliff is a CI-only external binary and is NOT required for local builds.
# tag_pattern "v[0-9]*" matches the same namespace as the version-injection
# path (git describe --match 'v[0-9]*'), skipping the m1..m9 milestone tags.

[changelog]
header = """
# Changelog\n
All notable changes to yanshi are documented here. The file is generated by
[git-cliff](https://github.com/orhun/git-cliff) from Conventional Commits; see
`docs/commit-convention.md` for the prefix subset.\n
"""
body = """
{% if version %}\
    ## [{{ version | trim_start_matches(pat="v") }}] - {{ timestamp | date(format="%Y-%m-%d") }}
{% else %}\
    ## [unreleased]
{% endif %}\
{% for group, commits in commits | group_by(attribute="group") %}
    ### {{ group | upper_first }}
    {% for commit in commits %}
        - {% if commit.scope %}(`{{ commit.scope }}`) {% endif %}{{ commit.message | upper_first }}\
    {% endfor %}
{% endfor %}\n
"""
trim = true
footer = ""

[git]
conventional_commits = true
filter_unconventional = false
split_commits = false
require_conventional = false
filter_commits = false
tag_pattern = "v[0-9]*"
commit_preprocessors = []
commit_parsers = [
    { message = "^feat!", group = "⚠ Breaking Changes" },
    { body = ".*BREAKING CHANGE.*", group = "⚠ Breaking Changes" },
    { message = "^feat", group = "Features" },
    { message = "^fix", group = "Bug Fixes" },
    { message = "^perf", group = "Performance" },
    { message = "^refactor", group = "Refactor" },
    { message = "^docs", group = "Documentation" },
    { message = "^test", group = "Tests" },
    { message = "^chore\\(release\\)", skip = true },
    { message = "^chore|^ci|^build", group = "Maintenance" },
    { message = "^revert", group = "Reverted" },
]
```

### Step 3.2 — `CHANGELOG.md` 种子

```markdown
# Changelog

All notable changes to yanshi are documented here. The file is generated by
[git-cliff](https://github.com/orhun/git-cliff) from Conventional Commits; see
`docs/commit-convention.md` for the prefix subset.

## [unreleased]

### Features
- (version) parse semver and make Version overridable via ldflags
- (build) inject version from semver tag in release path
- (release) ship cross-platform binaries with SHA256 checksums
- (ci) add pull-request gate matrix (test/vet/race/build) and nightly jobs
- (config) version config schema and gate loads on schema_version
- (doctor) add --release self-check (config-version / WAL / keyring)

> The first tagged release (`v1.0.0`) will replace this seed with the
> git-cliff-generated body. Hand-edit only to back-fill pre-conventional history.
```

**Run / Expected（本地可选，装 git-cliff 才跑）:**

```sh
git-cliff --config cliff.toml --unreleased --dry-run   # 按分组输出，无 m1–m9
```

`tag_pattern = "v[0-9]*"` 确保只认 semver tag；`cliff.toml` 与 `CHANGELOG.md` 存在；`release.yml`（Task 9）引用 `cliff.toml`。

**Commit:** `docs(release): add git-cliff config and seed changelog`

---

## Task 4 — CIG1：`ci.yml` 核心门禁（test / vet / race / build）

**Files:** `.github/workflows/ci.yml`（新）。

建立 `.github/workflows/` 目录 + PR/push 快路径。race 是编译器 flag（对任意 Go 代码生效），故 **硬门禁**；本 Task 只放不需要 E2/E3 产物的 job。governance/fuzz-seed 在 Task 5 追加（软启用）。

### Step 4.1 — RED（`act --list` / 文件不存在）

```sh
# 改之前：
test -f .github/workflows/ci.yml && echo EXISTS || echo MISSING    # MISSING（RED）
# 若本地装了 act：
act --list 2>&1 | head   # 无 workflow / 报错
```

### Step 4.2 — 实现 `ci.yml`

```yaml
# PR / push-to-main gate. Fast path: must pass and blocks merge (set the four
# hard jobs — test, vet, race, build — as required in GitHub branch protection).
# Windows runs only test + build (race/vet are ubuntu-only to control cost).
name: ci

on:
  push:
    branches: [main]
  pull_request:

concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true

jobs:
  test:
    name: test (${{ matrix.os }})
    runs-on: ${{ matrix.os }}-latest
    strategy:
      fail-fast: false
      matrix:
        os: [ubuntu, windows, macos]
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      # e2e_real build tag is off by default -> codex/claudecode CLIs are not
      # needed. eino/bootstrap t.Skip on missing providers is expected, not a fail.
      - run: go test ./...

  vet:
    name: vet
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - run: go vet ./...

  # race is a compiler flag, not a test asset — it works on any Go code, so it
  # is a HARD gate (no soft-enable on E2/RAC1).
  race:
    name: race
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - run: go test -race ./...

  build:
    name: build (${{ matrix.os }}, ${{ matrix.tags }})
    runs-on: ${{ matrix.os }}-latest
    strategy:
      fail-fast: false
      matrix:
        os: [ubuntu, windows, macos]
        tags: [default, nokeyring]
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - name: build
        env:
          CGO_ENABLED: "0"
        run: |
          tag_flag=""
          if [ "${{ matrix.tags }}" = "nokeyring" ]; then tag_flag="-tags=nokeyring"; fi
          go build $tag_flag ./cmd/yanshi
      - name: smoke -h
        # fork compiles in (replace => ./third_party/bubbletea is tracked); the
        # binary must at least print usage and exit 0.
        run: ${{ matrix.os == 'windows' && '.\yanshi.exe -h' || './yanshi -h' }}
```

**Run / Expected:**

```sh
# 本地 dry-run（装了 act）：
act --list                                      # 列出 test/vet/race/build 四 job
act -j vet pull_request                         # 本地真跑 vet job（需 docker）
```

最终 GREEN 以 PR 触发的真实运行为准（三平台 test/build 通过、vet 通过、ubuntu race 通过）。`e2e_real` 默认不编译；eino/bootstrap 的 `t.Skip` 在无 API key 的 CI 上预期跳过。

**Commit:** `ci: add pull-request gate matrix (test/vet/race/build)`

---

## Task 5 — CIG1：`ci.yml` 追加 governance + fuzz-seed（软启用 E3/E2）

**Files:** `.github/workflows/ci.yml`（改，追加两个 job）。

衔接 E3（GOV1 架构分层 / GOV2 行数脚本 / GOV3 导出文档）与 E2（FUZ1 种子 / PROP1 属性）。**软启用**：guard 步骤检查依赖 batch 的测试产物是否存在，不存在则 `exit 0`（soft-pass）；`continue-on-error: true` 兜底。E3/E2 落地后，把 guard 收紧、去掉 `continue-on-error`、在 branch protection 设为 required check。

> **衔接说明（给 E2/E3 作者）**：governance job 当前 guard 于 `internal/**/gov*.go`（GOV1/GOV2/GOV3 测试约定的文件名）；fuzz-seed guard 于 `go test -list 'Fuzz.*'` 能列出 FUZ1 target。E2/E3 落地后更新这两个 guard 的匹配条件并删 `continue-on-error`。

### Step 5.1 — RED

```sh
act --list 2>&1 | grep -E 'governance|fuzz-seed' && echo OK || echo MISSING   # MISSING（RED）
```

### Step 5.2 — 追加 job（追加到 `ci.yml` 的 `jobs:` 下）

```yaml
  # --- soft-gated jobs: E3 (governance) and E2 (fuzz/property) -----------------
  # Each job soft-passes (exit 0) until its batch's test assets exist, and has
  # continue-on-error as a safety net. Flip to a hard required check when the
  # batch lands: tighten the guard, drop continue-on-error.

  governance:
    name: governance (E3 GOV1/GOV2/GOV3)
    runs-on: ubuntu-latest
    continue-on-error: true   # soft until E3 governance tests land
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - name: run governance tests if present
        run: |
          set -e
          # Guard: E3 is expected to ship GOV1/GOV2/GOV3 tests. Until then, soft-pass.
          # Update this match when E3's test layout is finalized, then drop continue-on-error.
          if ! find internal cmd -name 'gov*.go' 2>/dev/null | grep -q .; then
            echo "no governance tests yet (E3 pending); soft-pass"
            exit 0
          fi
          go test -run 'TestGOV|TestGovernance|TestArchitecture|TestLineCount|TestExports' ./...

  fuzz-seed:
    name: fuzz-seed (E2 FUZ1/PROP1)
    runs-on: ubuntu-latest
    continue-on-error: true   # soft until E2 fuzz/property tests land
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - name: run fuzz seeds + property tests if present
        run: |
          set -e
          # Guard: E2 ships FUZ1 seed corpora + PROP1 property tests in guard/ctxcompact.
          if ! go test -list 'Fuzz.*|TestProperty.*' ./internal/guard/... ./internal/ctxcompact/... 2>/dev/null | grep -Eq 'Fuzz|TestProperty'; then
            echo "no fuzz/property targets yet (E2 pending); soft-pass"
            exit 0
          fi
          go test -run 'Fuzz|TestProperty' -count=1 ./internal/guard/... ./internal/ctxcompact/...
```

**Run / Expected:**

```sh
act --list 2>&1 | grep -E 'governance|fuzz-seed'   # 两个 job 出现
act -j governance pull_request                      # E3 未就绪 → soft-pass exit 0
```

GREEN：PR 真实运行时 governance/fuzz-seed 在 E3/E2 未就绪情况下 exit 0（不阻合并），就绪后实跑。

**Commit:** `ci: soft-enable governance and fuzz-seed jobs for E3/E2`

---

## Task 6 — CIG1：`nightly.yml`（长 fuzz + bench + 全量 race，软启用 E2/F2）

**Files:** `.github/workflows/nightly.yml`（新）。

慢路径，`schedule`（每日）+ `workflow_dispatch` 手动。不阻合并；bench 仅记趋势，**不做硬门禁**。fuzz-long（E2）/ bench（F2）软启用。

### Step 6.1 — 实现 `nightly.yml`

```yaml
# Nightly slow path: long fuzz, benchmarks, full multi-platform race. Does NOT
# block merge (no required checks here). Bench is trend-only (no hard gate).
name: nightly

on:
  schedule:
    - cron: "17 3 * * *"   # 03:17 UTC daily (off-the-hour to ease runner load)
  workflow_dispatch:

concurrency:
  group: nightly
  cancel-in-progress: false

jobs:
  fuzz-long:
    name: fuzz-long (E2 FUZ1)
    runs-on: ubuntu-latest
    continue-on-error: true   # soft until E2 fuzz targets land
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - name: long fuzz if target present
        run: |
          set -e
          if ! go test -list 'Fuzz.*' ./internal/guard/... 2>/dev/null | grep -q '^Fuzz'; then
            echo "no Fuzz target yet (E2 pending); soft-pass"
            exit 0
          fi
          go test -fuzz=FuzzMatchGlob -fuzztime=10m ./internal/guard/...

  bench:
    name: bench (F2)
    runs-on: ubuntu-latest
    continue-on-error: true   # soft until F2 bench targets land; trend-only even then
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - name: bench if targets present
        run: |
          set -e
          if ! go test -list 'Benchmark.*' ./... 2>/dev/null | grep -q '^Benchmark'; then
            echo "no Benchmark target yet (F2 pending); soft-pass"
            exit 0
          fi
          go test -bench=. -benchmem ./... | tee bench-results.txt
      - name: upload bench results
        if: always()
        uses: actions/upload-artifact@v4
        with:
          name: bench-results
          path: bench-results.txt
          if-no-files-found: ignore

  race-full:
    name: race-full (${{ matrix.os }})
    runs-on: ${{ matrix.os }}-latest
    strategy:
      fail-fast: false
      matrix:
        os: [ubuntu, windows, macos]
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - run: go test -race ./...
```

**Run / Expected:**

```sh
act --list 2>&1 | grep -E 'fuzz-long|bench|race-full'   # 三个 job 出现
act -W .github/workflows/nightly.yml                    # schedule/workflow_dispatch event
```

GREEN：`workflow_dispatch` 手动触发，fuzz-long/bench 在 E2/F2 未就绪时 soft-pass、race-full 三平台通过。bench 结果作为 artifact 上传（趋势用）。

**Commit:** `ci: add nightly fuzz/bench/race-full jobs (soft on E2/F2)`

---

## Task 7 — UPG1：config `schema_version` + `Load` 版本门 + 迁移框架

**Files:** `internal/config/config.go`（改）、`internal/config/config_test.go`（改）、`config.example.yaml`（改）。

前瞻性铺路：v1 无破坏性变更。`SchemaVersion` 缺字段（旧配置）视为 `1`；超高拒绝；`migrateConfig` no-op 占位。**不**静默改磁盘文件（迁移只作用于内存 cfg）。

### Step 7.1 — 写失败的测试

```go
// internal/config/config_test.go （追加；不要替换整个文件）
package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/config"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	return p
}

func TestSchemaVersionMissingDefaultsToOne(t *testing.T) {
	cfg, err := config.Load(writeCfg(t, `llm: {}`))
	require.NoError(t, err)
	assert.Equal(t, config.SupportedSchemaVersion, cfg.SchemaVersion,
		"omitted schema_version must read as the current supported version")
}

func TestSchemaVersionExplicitOneLoads(t *testing.T) {
	cfg, err := config.Load(writeCfg(t, "schema_version: 1\nllm: {}\n"))
	require.NoError(t, err)
	assert.Equal(t, 1, cfg.SchemaVersion)
}

func TestSchemaVersionTooHighIsRejected(t *testing.T) {
	future := config.SupportedSchemaVersion + 1
	_, err := config.Load(writeCfg(t,
		"schema_version: "+itoa(future)+"\nllm: {}\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "schema_version")
}

func TestMigrateConfigIsNoOpAtV1(t *testing.T) {
	// v1 has no destructive migration. migrateConfig must not mutate the cfg
	// passed in beyond setting the canonical version, and must not error.
	cfg := &config.Config{SchemaVersion: 1}
	err := config.MigrateConfig(cfg, 1, config.SupportedSchemaVersion)
	require.NoError(t, err)
	assert.Equal(t, config.SupportedSchemaVersion, cfg.SchemaVersion)
}

func TestLoadDoesNotWriteDisk(t *testing.T) {
	p := writeCfg(t, "schema_version: 1\nllm: {}\n")
	before, err := os.ReadFile(p)
	require.NoError(t, err)
	_, err = config.Load(p)
	require.NoError(t, err)
	after, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Equal(t, before, after, "Load must never rewrite the user's config file")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
```

**Expected failure:** `undefined: config.SupportedSchemaVersion`、`undefined: config.MigrateConfig`、`cfg.SchemaVersion` undefined；`TestSchemaVersionMissingDefaultsToOne` 会失败（字段不存在，零值 `0` ≠ `SupportedSchemaVersion`）。

### Step 7.2 — 实现（`config.go`）

在 `Config` struct（`:42` 起）顶层字段中追加（与 `Batch` 等并列，保持在 struct 顶部易于发现）：

```go
type Config struct {
	// SchemaVersion is the config schema generation. Omitted (zero) on older
	// configs is normalized to SupportedSchemaVersion by Load — A–D config
	// evolution was purely additive, so a missing field is forward-compatible.
	// A value above SupportedSchemaVersion is rejected (the user must upgrade
	// yanshi). Bump SupportedSchemaVersion on the first destructive change and
	// add a case to MigrateConfig.
	SchemaVersion int `yaml:"schema_version"`
	Server        ServerConfig                       `yaml:"server"`
	// ...（其余字段原样不动）
}
```

在 `config.go` 合适位置（紧邻 `Load`/`LoadBytes`）追加常量与迁移函数：

```go
// SupportedSchemaVersion is the current config schema generation. Load rejects
// configs whose schema_version is higher; MigrateConfig upgrades lower ones.
// Bump this only on a destructive change (and add a migration + CHANGELOG
// major entry). Today v1 == 1.
const SupportedSchemaVersion = 1

// MigrateConfig upgrades an in-memory cfg from `from` to `to`. v1 has no
// destructive migration, so this is a no-op beyond asserting the target. It is
// the single insertion point for future schema migrations — Load never rewrites
// the user's disk file; migration is in-memory only (callers may opt in to
// writing a backup explicitly).
func MigrateConfig(cfg *Config, from, to int) error {
	if cfg == nil {
		return fmt.Errorf("config: nil cfg")
	}
	if from > to {
		return fmt.Errorf("config: cannot migrate schema %d down to %d", from, to)
	}
	// No-op at v1: A–D evolution was additive. Future migrations chain here:
	//   for v := from; v < to; v++ { switch v { case 1: /* ... */ } }
	cfg.SchemaVersion = to
	return nil
}
```

在 `LoadBytes`（`:341`）中，`applyDefaults()` 之后、`validate()` 之前，插入版本门：

```go
func LoadBytes(data []byte) (*Config, error) {
	expanded := os.ExpandEnv(string(data))
	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, err
	}
	cfg.applyDefaults()

	// Schema version gate (UPG1). Missing field (0) -> treat as current
	// (A–D was additive). Higher than supported -> reject (user must upgrade
	// yanshi). Lower -> in-memory migration (no disk rewrite).
	switch {
	case cfg.SchemaVersion == 0:
		cfg.SchemaVersion = SupportedSchemaVersion
	case cfg.SchemaVersion > SupportedSchemaVersion:
		return nil, fmt.Errorf(
			"config: schema_version=%d exceeds supported=%d; upgrade yanshi",
			cfg.SchemaVersion, SupportedSchemaVersion)
	case cfg.SchemaVersion < SupportedSchemaVersion:
		if err := MigrateConfig(&cfg, cfg.SchemaVersion, SupportedSchemaVersion); err != nil {
			return nil, err
		}
	}

	if err := cfg.validateAPIKeyRefs(cfg.Auth.LegacyInsecure); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}
```

> **注意 D3 并发**：D3 仍可能改 `config.go`。执行本 Task 前 `git log -- internal/config/config.go` 确认 `LoadBytes` 顺序未变；若 D3 已改 `LoadBytes`，把版本门插在 `applyDefaults` 之后、其它校验之前即可，不改 D3 逻辑。`SchemaVersion` 字段加在 struct 顶部不与 D3 字段冲突。

### Step 7.3 — `config.example.yaml`

在文件顶部追加：

```yaml
# schema_version is the config schema generation. Omit on existing configs to
# keep them forward-compatible (treated as the current version). yanshi rejects
# a value higher than it supports — upgrade yanshi first. See
# docs/upgrade-guide.md.
schema_version: 1
```

**Run:**

```sh
go test ./internal/config -run 'TestSchemaVersion|TestMigrateConfig|TestLoadDoesNotWriteDisk' -v
go vet ./internal/config
```

**Expected:** 五个子测试通过；缺字段=1、超高拒、迁移 no-op、磁盘不被改写。

**Commit:** `feat(config): version config schema and gate loads on schema_version`

---

## Task 8 — UPG1：doctor `--release` + `checkConfigVersion`/`checkWAL`/`checkKeyringAvailability`

**Files:** `internal/cli/doctor.go`（改）、`internal/cli/doctor_test.go`（改）、`cmd/yanshi/main.go`（改，透传 `-release`）。

新增三项只读检查；`DoctorOptions.Release` 把 release-blocking warn 提级 fail（port-in-use、config-version 异常）。`checkWAL` 软依赖 F1（F1 前只 warn）。`checkSandbox` 占位**如实保留**，不假装已验证。

### Step 8.1 — 写失败的测试

```go
// internal/cli/doctor_test.go （追加；沿用既有 testify/table-driven 风格）
package cli_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/cli"
)

func TestDoctorHasNewReleaseChecks(t *testing.T) {
	rep := cli.RunDoctor(context.Background(), cli.DoctorOptions{ConfigPath: ""})
	names := map[string]bool{}
	for _, c := range rep.Checks {
		names[c.Name] = true
	}
	for _, want := range []string{"config-version", "wal", "keyring"} {
		assert.True(t, names[want], "doctor must include a %q check", want)
	}
}

func TestDoctorReleaseFlagIsHonored(t *testing.T) {
	// --release is plumbed through DoctorOptions.Release; its presence must not
	// change the set of checks, only how release-blocking warns are promoted.
	normal := cli.RunDoctor(context.Background(), cli.DoctorOptions{})
	release := cli.RunDoctor(context.Background(), cli.DoctorOptions{Release: true})
	require.Len(t, normal.Checks, len(release.Checks), "--release must not add/remove checks")
}

	func TestCheckConfigVersionOKWhenSupported(t *testing.T) {
		// checkConfigVersion is package-internal (lowercase); drive through RunDoctor.
		// If written as package cli (internal test), the unexported helper is callable directly.
		rep := cli.RunDoctor(context.Background(), cli.DoctorOptions{ConfigPath: ""})
		for _, c := range rep.Checks {
			if c.Name == "config-version" {
				assert.NotEmpty(t, c.Message)
				return
			}
		}
	}

	func TestDoctorReleasePromotesConfigVersionWarnToFail(t *testing.T) {
		// When Release=true, a config-version mismatch that would normally warn
		// must surface as fail. Drive through RunDoctor with a fixture config
		// whose schema_version exceeds SupportedSchemaVersion.
		p := writeCfg(t, fmt.Sprintf("schema_version: %d\nllm: {}\n", config.SupportedSchemaVersion+1))
		rep := cli.RunDoctor(context.Background(), cli.DoctorOptions{ConfigPath: p, Release: true})
		for _, c := range rep.Checks {
			if c.Name == "config-version" {
				require.Equal(t, cli.StatusFail, c.Status,
					"--release must promote config-version to fail when schema_version exceeds supported")
				return
			}
		}
		t.Error("config-version check not found in doctor report")
	}
```

> 实施说明：若 `CheckConfigVersion` 等 helper 保持小写（包内），把上述对单 check 的断言改走 `RunDoctor` 聚合（用 fixture config 驱动），或在 `doctor_test.go`（`package cli` 内部测试）里直接调小写 helper。**关键断言**（必留）：
> 1. `config-version` / `wal` / `keyring` 三个 name 出现在 report。
> 2. `checkWAL` 只读：`PRAGMA journal_mode` 查询后数据库连接已 Close、无残留（沿用 `checkDatabase` 的 `store.Open`+`Close` 模式）。
> 3. `checkKeyringAvailability` 在 `-tags nokeyring` 构建下报 "disabled in this build"，**不** fail（nokeyring 是默认 release 变体）。
> 4. `--release` 把 `checkPort` 的 "address in use" warn 提级 fail。
> 5. `checkSandbox` 消息仍含 "S08"/"M2"，未假装已验证。

### Step 8.2 — 实现（`doctor.go`）

`DoctorOptions` 加字段：

```go
type DoctorOptions struct {
	ConfigPath string
	Root       string
	// Release promotes release-blocking warns to fails (e.g. port-in-use,
	// config-version anomalies). Used by the release runbook: `yanshi doctor
	// --release` must exit 0 before cutting a release. Docs: upgrade-guide.md.
	Release bool
}
```

在 `RunDoctor`（`:135`）的 checks 序列中插入三项（位置紧随 `checkConfig` 之后，因为它们都依赖 cfg）：

```go
checks = append(checks, checkConfig(opts.ConfigPath, cfg, cfgErr))
checks = append(checks, checkConfigVersion(cfg, cfgErr, opts.Release)) // 新
checks = append(checks, checkDatabase(cfg, cfgErr))
// ...（既有序列原样）
checks = append(checks, checkWAL(cfg, cfgErr))                  // 新（软依赖 F1）
checks = append(checks, checkKeyringAvailability(cfg, cfgErr))  // 新
```

三个新检查（全只读；`release` 仅用于提级）：

```go
// checkConfigVersion reports the loaded config's schema_version against
// SupportedSchemaVersion. Load already rejects a too-high version, so this is
// mostly a clarity check; under --release an anomaly is fail, not warn.
func checkConfigVersion(cfg *config.Config, cfgErr error, release bool) CheckResult {
	if cfgErr != nil {
		return skipped("config-version", cfgErr)
	}
	switch {
	case cfg.SchemaVersion == config.SupportedSchemaVersion:
		return CheckResult{Name: "config-version", Status: StatusOK,
			Message: fmt.Sprintf("schema_version=%d (supported)", cfg.SchemaVersion)}
	case cfg.SchemaVersion > config.SupportedSchemaVersion:
		st := StatusWarn
		if release {
			st = StatusFail
		}
		return CheckResult{Name: "config-version", Status: st,
			Message: fmt.Sprintf("schema_version=%d exceeds supported=%d; upgrade yanshi",
				cfg.SchemaVersion, config.SupportedSchemaVersion)}
	default:
		st := StatusWarn
		if release {
			st = StatusFail
		}
		return CheckResult{Name: "config-version", Status: st,
			Message: fmt.Sprintf("schema_version=%d < supported=%d; will migrate on next boot",
				cfg.SchemaVersion, config.SupportedSchemaVersion)}
	}
}

// checkWAL reads the SQLite journal_mode (read-only: a SELECT, never a PRAGMA
// that changes mode). Soft-depends on F1 (WAL1): before F1 lands the mode is
// not "wal", so this warns — it does NOT fail and does NOT flip the mode.
func checkWAL(cfg *config.Config, cfgErr error) CheckResult {
	if cfgErr != nil {
		return skipped("wal", cfgErr)
	}
	path := cfg.Storage.SQLitePath
	if path == "" {
		path = "<unset>"
	}
	st, err := store.Open(path)
	if err != nil {
		return CheckResult{Name: "wal", Status: StatusWarn,
			Message: fmt.Sprintf("open %q to read journal_mode: %v", path, err)}
	}
	defer st.Close()
	// Read-only probe: SELECT the current mode, do not write/PRAGMA-set.
	mode := ""
	if row := st.DB.QueryRow("PRAGMA journal_mode"); row != nil {
		_ = row.Scan(&mode) // best-effort; PRAGMA returns the current mode
	}
	if mode == "wal" {
		return CheckResult{Name: "wal", Status: StatusOK, Message: "journal_mode=wal"}
	}
	return CheckResult{Name: "wal", Status: StatusWarn,
		Message: fmt.Sprintf("journal_mode=%q (WAL not enabled yet; F1 pending)", mode)}
}

// checkKeyringAvailability probes the OS keyring via Available() (a sentinel
// Get, read-only). On a -tags nokeyring build Available() returns
// ErrKeyringUnavailable; since nokeyring is the default release variant, that
// is reported as a note, not a failure.
func checkKeyringAvailability(cfg *config.Config, cfgErr error) CheckResult {
	if cfgErr != nil {
		return skipped("keyring", cfgErr)
	}
	kr := secrets.NewOSKeyringStore()
	if err := kr.Available(); err != nil {
		if errors.Is(err, secrets.ErrKeyringUnavailable) {
			return CheckResult{Name: "keyring", Status: StatusOK,
				Message: "OS keyring disabled in this build (nokeyring); secrets fall back to file"}
		}
		return CheckResult{Name: "keyring", Status: StatusWarn,
			Message: fmt.Sprintf("OS keyring unavailable: %v", err)}
	}
	return CheckResult{Name: "keyring", Status: StatusOK, Message: "OS keyring available"}
}
```

> **`checkWAL` 实现注记**：`store.Store` 是否已导出 `DB()` 决定怎么读 `PRAGMA`。执行前 grep `func (s \*Store) DB()` 确认；若无，改为 `store.Open` 后用 `database/sql` 的导出句柄，或在 `internal/store` 加一个只读 `JournalMode() (string, error)` 方法（本计划倾向于后者：加一个明确只读的方法，避免暴露原始 `*sql.DB`）。`checkKeyringAvailability` 的 `secrets.NewOSKeyringStore()` 入口在 `internal/secrets/keyring.go`（D3 已建）；执行前确认其签名，若不同则调整调用、不改 secrets 包语义。`checkSandbox`（`:388`）**不改** —— 保留 "arrives with S08 in M2" 消息。

`--release` 提级 port：修改 `checkPort`（`:322`），接收 `release bool`：

```go
func checkPort(cfg *config.Config, cfgErr error, release bool) CheckResult {
	if cfgErr != nil {
		return skipped("port", cfgErr)
	}
	addr := cfg.Server.HTTPAddr
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		st := StatusWarn
		if release {
			st = StatusFail // release-blocking: configured port must be free
		}
		return CheckResult{Name: "port", Status: st,
			Message: fmt.Sprintf("%s not bindable: %v (a backend may already be running)", addr, err)}
	}
	_ = ln.Close()
	return CheckResult{Name: "port", Status: StatusOK, Message: fmt.Sprintf("%s is free", addr)}
}
```

`RunDoctor` 中 `checkPort(cfg, cfgErr)` 调用改为 `checkPort(cfg, cfgErr, opts.Release)`。

> **注意**：`checkPort` 签名从 `(cfg, cfgErr)` 变为 `(cfg, cfgErr, release)` 会破坏现有 `TestCheckPort_FreeAndInUse` 测试 —— 记得同步更新测试中的 `checkPort` 调用点（改签名为三参数或经 `RunDoctor` 间接测试）。

### Step 8.3 — `cmd/yanshi/main.go` 透传 `-release`

`runDoctor`（`:917` 起，`flag.NewFlagSet("doctor", ...)`）注册 `-release` 并写入 `DoctorOptions.Release`：

```go
fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
// ...既有 -config / -json...
release := fs.Bool("release", false, "promote release-blocking warns to fails (release runbook)")
// 解析后：
opts := cli.DoctorOptions{ConfigPath: cfgPath, Root: root, Release: *release}
```

并在 `usage` 的 doctor 行追加 `[-release]`，说明"发布前自检；详见 docs/upgrade-guide.md"。

**Run:**

```sh
go test ./internal/cli -run 'TestDoctor|TestCheckConfigVersion|TestCheckWAL|TestCheckKeyring' -v
go test -tags nokeyring ./internal/cli -run TestCheckKeyring    # nokeyring 构建下报 disabled，不 fail
go vet ./internal/cli ./cmd/yanshi
```

**Expected:** 新检查 name 齐全；`--release` 提级生效；nokeyring 下 keyring 报 OK-note；既有 15 项检查不退化；`checkSandbox` 仍如实占位。`yanshi doctor -json` 与 `yanshi doctor -release -json` 输出契约沿用既有 golden 风格。

**Commit:** `feat(doctor): add release self-check (config-version/wal/keyring) and --release flag`

---

## Task 9 — PKG1：`.goreleaser.yaml` + `release.yml`（tag 触发 / git-cliff notes / 四目标）

**Files:** `.goreleaser.yaml`（新）、`.github/workflows/release.yml`（新）。

四目标全 `CGO_ENABLED=0` + `-tags nokeyring`（OQ3 锁定：keyring 变体 v1 不进，无 native macos runner / 无 cgo）。ldflags 注入 `version.Version`（依赖 Task 1 的 var 化）+ `BuildStamp`。`release.yml` `on: push: tags: ['v*']` 自动 release，先用 git-cliff 产 notes 喂 goreleaser，再产 archive + SHA256。

### Step 9.1 — RED

```sh
test -f .goreleaser.yaml && echo EXISTS || echo MISSING          # MISSING（RED）
test -f .github/workflows/release.yml && echo EXISTS || echo MISSING
# 装了 goreleaser：
goreleaser check 2>&1 | head    # 报 config 不存在
```

### Step 9.2 — `.goreleaser.yaml`

```yaml
# goreleaser v2 config for yanshi. Four targets, all CGO_ENABLED=0 + nokeyring
# (keyring variant deferred past v1; see docs/superpowers/specs/...h1...md §5).
# Version is injected via ldflags (version.Version is a var since VER1).
# bubbletea fork resolves via go.mod replace => ./third_party/bubbletea (tracked).
version: 2

project_name: yanshi

before:
  hooks:
    - go mod tidy

builds:
  - id: yanshi
    main: ./cmd/yanshi
    binary: yanshi
    env:
      - CGO_ENABLED=0
    flags:
      - -tags=nokeyring
      - -trimpath
    ldflags:
      - -s -w
      - -X github.com/x6nux/yanshi/internal/version.Version={{.Version}}
      - -X github.com/x6nux/yanshi/internal/version.BuildStamp={{.Date}}
    goos: [linux, windows, darwin]
    goarch: [amd64, arm64]
    # Ship exactly four targets: windows/amd64, linux/amd64, linux/arm64, darwin/arm64.
    ignore:
      - goos: windows
        goarch: arm64
      - goos: darwin
        goarch: amd64

archives:
  - id: default
    name_template: >-
      {{ .ProjectName }}_{{ .Version }}_
      {{- if eq .Os "darwin" }}macos{{ else }}{{ .Os }}{{ end }}_{{ .Arch }}
    format_overrides:
      - goos: windows
        formats: [zip]
    files:
      - config.example.yaml
      - src: README.md
        dst: README.md

checksum:
  name_template: "checksums.txt"
  algorithm: sha256

snapshot:
  version_template: "{{ incpatch .Version }}-dev"

# git-cliff generates the release notes (release.yml runs it and feeds the file
# via --release-notes). Disable goreleaser's built-in changelog to avoid a dupe.
changelog:
  skip: true

release:
  draft: false
  prerelease: auto
  name_template: "v{{.Version}}"
```

### Step 9.3 — `release.yml`

```yaml
# Release on tag push. git-cliff generates notes from Conventional Commits
# (cliff.toml, tag_pattern v[0-9]*), then goreleaser builds four nokeyring
# targets, attaches checksums.txt, and publishes the GitHub Release.
name: release

on:
  push:
    tags: ["v*"]

permissions:
  contents: write

jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0   # git-cliff needs full history to group by tag

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true

      # git-cliff is a CI-only external binary (not in go.mod). Pin a version.
      - name: install git-cliff
        run: |
          curl -sSL https://github.com/orhun/git-cliff/releases/download/v2.6.1/git-cliff-2.6.1-x86_64-unknown-linux-gnu.tar.gz \
            | tar -xz -C /tmp
          echo "/tmp/git-cliff-2.6.1" >> "$GITHUB_PATH"

      - name: generate release notes
        run: git-cliff --config cliff.toml --latest --output RELEASE_NOTES.md

      - name: update CHANGELOG.md
        run: git-cliff --config cliff.toml --latest --output CHANGELOG.md

      - name: commit changelog
        run: |
          git config user.name "github-actions[bot]"
          git config user.email "github-actions[bot]@users.noreply.github.com"
          git add CHANGELOG.md
          git commit -m "chore(release): update changelog for v${GITHUB_REF_NAME#v}" || echo "no changelog changes"
          git push origin HEAD:main

      - uses: goreleaser/goreleaser-action@v6
        with:
          distribution: goreleaser
          version: "~> v2"
          args: release --clean --release-notes RELEASE_NOTES.md
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

### Step 9.4 — 本地 snapshot 验证

```sh
goreleaser check                                  # config 合法
goreleaser build --snapshot --clean               # 四目标交叉编译（CGO_ENABLED=0）
# windows 产物 smoke（fork 编译通过、可启动；完整按键验证需 TTY，记为人工 checklist）：
file dist/yanshi_windows_amd64_v1*/yanshi.exe
# (在 Windows 上) ./dist/.../yanshi.exe -h    # exit 0
```

**Expected:** `goreleaser check` 通过；`--snapshot` 产四个目标（windows/amd64、linux/amd64、linux/arm64、darwin/arm64）+ `checksums.txt`；windows 产物 `-h` exit 0。最终 GREEN 以真实 `git tag v1.0.0 && git push --tags` 触发 `release.yml`、Release 含四 archive + checksums.txt + git-cliff notes 为准。

> **fork 行为验证边界**：交叉编译的 windows/amd64 二进制 `-h` exit 0 证明 fork 编译通过、可启动。Ctrl+Enter/Enter 区分需 TTY，CI 无 TTY —— 记为 release 前人工 checklist 项（alt-screen TUI 无法管道驱动，见 CLAUDE.md）。

**Commit:** `feat(release): ship four nokeyring targets with checksums and git-cliff notes`

---

## 测试策略汇总（Fake 优先 / 只读优先 / dry-run 分层）

| Task | 验证器 | RED 形态 | GREEN 证据 |
|---|---|---|---|
| T1 version | `go test` | `undefined: Parse` + const 不可赋值 | Parse 表驱动通过；Version 可赋值 |
| T2 build.sh | `bash -n` + smoke | `release` 路径不存在 | 临时 v tag 注入成功、无 tag 回落 |
| T3 cliff/changelog | git-cliff dry-run（CI-only，本地可选） | 文件不存在 | cliff.toml 合法 + CHANGELOG 种子在 |
| T4 ci.yml core | `act --list` + PR 真跑 | workflow 不存在 | 四 job 列出、PR 通过 |
| T5 ci gov/fuzz | `act --list` + PR | job 不存在 | soft-pass exit 0 |
| T6 nightly | `act --list` + workflow_dispatch | workflow 不存在 | 三 job 列出、手动跑 race-full 通过 |
| T7 config schema | `go test` | `undefined: SupportedSchemaVersion` | 五路径通过、磁盘不改写 |
| T8 doctor | `go test`（含 `-tags nokeyring`） | `undefined: Release`/新检查 name | 新检查齐、`--release` 提级、nokeyring 报 note |
| T9 goreleaser/release | `goreleaser check` + snapshot + tag | config 不存在 | 四目标 + checksums + notes |

**跨平台回归**：`go vet ./...` + `go build`（default 与 `-tags nokeyring`）在三平台 CI（T4）即为跨平台回归。`cmd/testchanged` 不进 CI（CI 跑全量 `go test ./...`，靠 Go cache 控时）。

## 风险与缓解

| 风险 | 缓解 |
|---|---|
| **`const Version` 未改 var → ldflags 静默无效** | T1 强制 const→var + `TestVersionIsOverridable` 断言可赋值；T9 snapshot 验证 `--version` 显示 tag |
| m1–m9 标签污染 `git describe` / git-cliff | `--match 'v[0-9]*'`（build.sh）+ `tag_pattern = "v[0-9]*"`（cliff.toml）同命名空间；`TestParseRejectsMilestoneTags` 守住 |
| CI 时长膨胀（三平台 + race + fuzz + bench） | 分层 PR 快 / nightly 慢；Go cache；Windows 只 test+build；race/governance/fuzz 只 ubuntu |
| Windows runner flake | 必要项才上 Windows；race/vet/governance/fuzz 只 ubuntu |
| bubbletea fork replace 在 CI/打包解析失败 | fork 仓内 tracked，setup-go 后直接 build/test；goreleaser 从仓根构建，replace 直接解析；仅 fork 未提交时失败（CI 捕获） |
| E2/E3/F2 产出未就绪 | governance/fuzz-seed/nightly-fuzz/bench 全 guard + `continue-on-error` 软启用；就绪后去软开关转硬门禁 |
| keyring 与纯 Go 交叉编译 | v1 默认 `-tags nokeyring` + `CGO_ENABLED=0` 安全覆盖四目标（go-keyring darwin 端纯 Go，无 cgo）；darwin keyring 变体延后（OQ3） |
| 配置迁移破坏现有用户配置 | 版本门（超高拒）+ 前向兼容（缺字段=1）+ 不静默改磁盘（`TestLoadDoesNotWriteDisk`） |
| doctor 副作用 | 全检查只读（Listen+Close / Open+Close / sentinel Get / PRAGMA SELECT）；沿用既有约定 |
| sandbox 占位误导发布判断 | `checkSandbox` 如实保留 "S08/M2" 消息，不假装已验证 |
| D3/F1 同时改 config/doctor/store 落点冲突 | 执行前 `git log` re-verify；T7/T8 标注 D3/F1 最终态依赖 |
| tag 与常量漂移 | 发布以 semver tag 为准；CI 校验 tag 是 semver（`version.Parse`） |

## 验收标准（batch 级，对齐 spec §10）

1. `--version` 发布构建显示 semver（tag 注入），dev 显示 `0.4.0` + GitHash。
2. `CHANGELOG.md` 可从 conventional commits 生成；commit 规范文档存在；近期提交符合。
3. `.github/workflows/` 存在；PR 上三平台 test/build + vet + race(ubuntu) 通过；governance + fuzz-seed 软启用（E3/E2 就绪后转硬）；门禁阻合并（branch protection 配置）。
4. nightly 跑长 fuzz + bench（趋势记录，无硬门禁）+ 全量 race。
5. 四目标二进制可构建（默认 nokeyring/CGO_ENABLED=0）；windows 产物 smoke `-h` exit 0。
6. release 产物含 `checksums.txt`；tag `v*` 触发自动 release；notes 由 git-cliff 生成。
7. config schema 版本化（超高拒、缺字段=1、迁移框架在位、不写磁盘）。
8. doctor 新增 config-version / WAL（软依赖 F1）/ keyring-可用性 检查，全只读；`--release` 提级；既有检查不退化；`checkSandbox` 如实占位。
9. `docs/upgrade-guide.md` 存在，含 schema 机制、breaking 标注、release runbook。

## 升级指南（`docs/upgrade-guide.md`）内容大纲（Task 8 产出）

- **schema_version 机制**：`SupportedSchemaVersion` 当前值；缺字段=当前（向前兼容）；超高拒（升级 yanshi）；低于走 `MigrateConfig`（内存迁移，不写磁盘）。
- **A–D → v1.0 路径**：全 additive，无破坏；首次安装只需复制 `config.example.yaml` → `config.yaml`。
- **何时 bump schema**：第一次破坏性 config 变更时 bump `SupportedSchemaVersion` + 加 `MigrateConfig` case + CHANGELOG major + 更新本指南。
- **字段废弃策略**：deprecate → warn N 版 → remove。
- **release runbook**：发布前 `yanshi doctor --release` 必须 exit 0（fail 不可发，warn 需人工确认）；打 `v*` semver tag 触发 `release.yml`；校验 Release 含四 archive + `checksums.txt`；windows 产物人工 TUI 按键 checklist。
- **checksum 校验**：用户下载后 `sha256sum -c checksums.txt` 验证（v1 不签名，见非目标）。
