# Batch H1 — 发布工程（版本 / CI 门禁 / 多平台打包 / 升级兼容）设计

> **日期**：2026-07-22
> **归属**：E-H roadmap 的 Tier G，按 hybrid 调整归为 **Batch H1（发布工程）**。即原 roadmap 的 G1（VER1/CIG1）+ G2（PKG1/UPG1）合并（`docs/feature-roadmap-e-h.md` §8–§9）。
> **命题**：把 yanshi 从"能 `go build` 出单二进制"变成"可重复发布、跨平台分发、可升级、发布前可自检"的 v1.0 工程基线。
> **范围**：版本号来源与 CHANGELOG 自动化、CI 门禁矩阵、多平台打包与 checksum、配置 schema 版本化 + release doctor 扩展。**不**改产品功能行为。
> **状态**：设计稿，待用户审阅 → writing-plans。
> **核心依赖链**：[VER1] → [PKG1]（打包要注入版本号）；[GOV1][GOV2][RAC1][FUZ1][PROP1] → [CIG1]（治理/race/fuzz/property 测试必须先于 CI 门禁存在）；[WAL1] → [UPG1] 的 WAL 检查（软依赖）。

---

## 1. 目标与非目标

### 目标

- **版本号有唯一来源**：`internal/version` 是权威；发布构建从 git tag 注入 semver，dev 构建回落到常量；`--version` 与启动 banner 统一显示。
- **CHANGELOG 可自动生成**：从 conventional commit prefix 自动产出，发布时人工校验。
- **CI 门禁存在并阻合并**：跨平台 test/vet/build + `-race` + E3 治理测试 + E2 fuzz/property 种子 + F2 bench（nightly），门禁失败阻合并。
- **多平台产物 + checksum**：windows/amd64、linux/amd64+arm64、darwin/arm64 四目标；默认 `CGO_ENABLED=0` + `-tags nokeyring`；bubbletea fork 行为在产物上保留；release 附 SHA256。
- **配置可升级、发布前可自检**：config schema 版本化 + 迁移框架；doctor 扩展 release 自检（config-version / WAL / keyring 可用性 等，全只读）；升级指南存在。

### 非目标（v1 不做）

- Homebrew formula / Scoop manifest / apt/rpm 包（留后续；仅预留 goreleaser 的扩展位）。
- 自动发布到包管理器、自动签名公证（macOS notarization、Windows code-sign）—— v1 产物附 checksum 但不签名（留 H 后续）。
- 完整 sandbox 自检（`checkSandbox` 的真正实现归 S08/M2，本批只让 release doctor 如实报告现状）。
- 重写历史 commit 以符合规范（CHANGELOG 从本批起前瞻，旧条目按需手补）。
- 引入 golangci-lint 框架（与仓库现状一致；治理用零依赖脚本 + `go vet`）。

---

## 2. 背景（实测落点）

- **版本源已部分统一**：`internal/version/version.go` 已是唯一权威——
  - `Version` 常量 `"0.4.0"`（`version.go:8`），手动 bump。
  - `BuildStamp` 经 ldflags 注入（`version.go:19`，`build.sh:10` 已实现 `-X ...BuildStamp=$(date +%y%m%d%H%M)`）。
  - `GitHash` 由 `debug.ReadBuildInfo` 读 `vcs.revision`/`vcs.modified`（`version.go:29`），`go build` 在 git 仓内自动盖章，无需 ldflags。
  - 消费侧：`--version`（`cmd/yanshi/main.go:80`）与启动 banner（`internal/cli/tui/startup.go:231`）都拼 `v{Version}[.{BuildStamp}][ · {GitHash}]`。
  - **缺口**：版本号来自常量而非 git tag；无 semver 校验；无 CHANGELOG；无 commit 规范文档。
- **无任何 CI**：`.github/workflows/` 目录**不存在**（实测）。一切靠本地 `go test`/`vet`。`cmd/testchanged` 是本地增量测试工具，非 CI。
- **git tag 现状**：仅有里程碑标签 `m1-foundation`…`m9-cli-tui`（非 semver）；`git describe` = `m9-cli-tui-429-g56154d2`。**无任何 `v*` semver tag**——VER1 要新立 semver 标签命名空间，且 tag 匹配必须用 `v[0-9]*` glob 跳过 m1–m9。
- **提交已自然遵循 conventional commits**：近期提交 `feat(doctor):`/`fix(config):`/`test(d3):`/`chore(d2):`/`feat(ide-vscode):`——prefix + scope 已成型，CHANGELOG 自动化有干净输入。
- **打包相关**：
  - `build.sh` 只产 `yanshi.exe`（Windows，注入 BuildStamp）。
  - `go.mod:117` `replace github.com/charmbracelet/bubbletea => ./third_party/bubbletea`（fork 区分 Ctrl+Enter/Enter，CLAUDE.md 禁止去掉 replace）。fork 在仓内 → CI/打包直接解析，无需额外 setup。
  - `go.mod:31` `modernc.org/sqlite v1.53.0`（**纯 Go**，跨平台交叉编译不需 CGO）。
  - `go.mod:20` `zalando/go-keyring v0.2.6`；`nokeyring` build tag 在 `internal/secrets/keyring_disabled.go:1`（`//go:build nokeyring`）与 `keyring_enabled.go:1`（`//go:build !nokeyring`）。keyring 后端：Windows 走 wincred（纯 Go）、Linux 走 godbus（纯 Go）、**macOS 走 Security framework（cgo）**——见 PKG1 变体策略。
- **doctor 已有大量只读检查**：`internal/cli/doctor.go:135` `RunDoctor` 串联 config/database/providers/acp/lockfile/port/directories/sandbox/mcp/lsp/permissions/secrets/locale/keymap/high-contrast。其中：
  - `checkPort`（`doctor.go:316`）`net.Listen`+`Close`、`checkDatabase`（`doctor.go:200`）`store.Open`+`Close`、`checkSecretsRefs`（`doctor.go:461`）校验 credential ref——**均为只读/瞬时副作用**，是 release doctor 的安全基线。
  - `checkSandbox`（`doctor.go:382`）是**占位**（"arrives with S08 in M2"）——release doctor 须如实保留，不假装已验证。
  - **缺口**：无 config schema 版本检查、无 WAL 检查（F1 后）、无 keyring 可用性探针、无 release 维度。
- **config 无 schema 版本字段**：`internal/config/config.go:42` `Config` struct 顶层无 `schema_version`（实测 grep 无命中）；A-D 的配置演进全是**加字段**（additive，向前兼容），所以引入 schema_version 是前瞻性地为"第一次破坏性配置变更"铺路。

> **re-verify 提醒**：D3（secrets/auth/i18n/keymap）仍在进行，改 `config.go`/`doctor.go`。本 spec 落地前须 `git log` + 实测确认这两个文件的落点未被改写。

---

## 3. [VER1] 语义化版本 + CHANGELOG 自动化  (P1 | 部分)

- **缺口**：版本源已统一（见 §2），但缺 semver 纪律（无 tag、常量手 bump）、无 CHANGELOG、无 commit 规范文档。
- **落点**：
  - `internal/version/version.go`（改）+ `version_test.go`（改/新）——增 semver 解析/校验助手。
  - `build.sh`（改）——发布构建从 `git describe` 注入 `-X version.Version`。
  - `CHANGELOG.md`（新）+ `cliff.toml`（新，git-cliff 配置）。
  - `docs/commit-convention.md`（新）——commit 规范子集（H2 的 CONTRIBUTING 将引用它）。
- **设计**：
  - **版本来源（最小侵入）**：保留 `Version` 常量作 dev/`go build` 回落值；**发布构建**用 ldflags 覆盖之，值取自 semver tag：
    ```bash
    # build.sh 发布路径（或 goreleaser 的 ldflags，见 PKG1）
    VER=$(git describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null | sed 's/^v//')
    go build -ldflags "-X version.Version=${VER} -X version.BuildStamp=$(date +%y%m%d%H%M)" ...
    ```
    - **`--match 'v[0-9]*'` 是关键**：跳过 m1–m9 里程碑标签命名空间。
    - 消费侧零改动：`--version` 与 banner 都读 `version.Version`，覆盖即生效；dev 构建仍显示常量 + GitHash（带 `*` 标脏树），不破坏现有 startup。
  - **semver 解析**：`version.Parse(v string) (Semver, error)` 解析 `Major.Minor.Patch[-prerelease][+build]`；CI/release 脚本用它校验 tag 合法（非 semver 的 tag 不进入版本注入）。
  - **CHANGELOG 自动化**：用 **git-cliff**（成熟、conventional-commits 原生、Rust 单二进制，CI 里跑）。`cliff.toml` 配置分组：`feat!`/`BREAKING CHANGE` → ⚠ Breaking；`feat` → Features；`fix` → Bug Fixes；`perf` → Performance；`refactor`/`docs`/`test`/`chore`/`ci`/`build` → 折入 Maintenance（或按需展开）。scope 保留。`CHANGELOG.md` 在 release 流程里生成并提交。
  - **commit 规范子集**（`docs/commit-convention.md`）：定义 `feat`/`fix`/`perf`/`refactor`/`docs`/`test`/`chore`/`ci`/`build`/`revert`；破坏性变更用 `feat!:` 或 footer `BREAKING CHANGE:`；scope 用域码（`doctor`/`auth`/`config`/`orchestrator`/`vcs`…，与现有提交一致）。**不**引入 commit-lint 工具强制（与"无 golangci-lint 配置"一致），靠评审 + CHANGELOG 生成时人工校验。
- **依赖**：-（VER1 是 PKG1 的前置）。
- **风险**：
  - 历史 commit 不规范 → 从本批起规范，旧条目手补；提交实测已合规，迁移成本低。
  - 自动分类误判（scope 缺失/breaking 漏标）→ release 时人工校验生成的 CHANGELOG。
  - tag 与常量漂移 → 发布以 tag 为准；CI 校验 tag 是 semver。
  - m1–m9 标签污染 `git describe` → `--match 'v[0-9]*'` 隔离命名空间。
- **验收**：
  1. 发布构建的 `--version` 显示 semver（来自 tag），dev 构建显示常量 + GitHash。
  2. 非 semver 的 tag 不被版本注入采用（CI 校验）。
  3. `CHANGELOG.md` 可从 conventional commits 生成；首个 v1.0 CHANGELOG 存在。
  4. commit 规范文档存在；近期提交符合。
- **预估**：1–2d。

---

## 4. [CIG1] CI 门禁矩阵  (P0 | 缺失 | synthesis A23)

- **缺口**：`.github/workflows/` 不存在（实测）；跨平台/治理/race/fuzz/property/bench 全无自动化；门禁靠人工本地跑。
- **落点**：`.github/workflows/`（全新目录）：
  - `ci.yml`（PR + push main）
  - `nightly.yml`（schedule）
  - `release.yml`（tag 触发，见 PKG1）
- **设计**：
  - **分层控 CI 时长**（roadmap 风险点）：
    - **PR / push main（`ci.yml`）**：快路径，必跑、阻合并。
    - **nightly（`nightly.yml`）**：慢路径，长 fuzz + bench + 全量 race，不阻合并、仅告警。
  - **`ci.yml` 的 job 拆分**（每个 job 独立、可并行、可按需设为 required check）：

    | job | runner | 内容 | 依赖产出 |
    |---|---|---|---|
    | `test` | matrix `ubuntu`/`windows`/`macos` | `go test ./...`（Go cache；`//go:build e2e_real` 的测试默认不编译进） | — |
    | `vet` | `ubuntu` | `go vet ./...` | — |
    | `race` | `ubuntu` | `go test -race ./...` | [RAC1] |
    | `build` | matrix `ubuntu`/`windows`/`macos` × {default, `-tags nokeyring`} | `go build ./cmd/yanshi`；smoke `./yanshi -h`（打印用法 exit 0） | — |
    | `governance` | `ubuntu` | GOV1 架构分层测试 + GOV2 纯代码行数脚本 + GOV3 导出符号文档检查 | [GOV1][GOV2][GOV3] |
    | `fuzz-seed` | `ubuntu`（PR） | 跑 FUZ1 种子语料 + PROP1 属性测试：`go test -run 'Fuzz|TestProperty' -count=1 ./internal/guard/... ./internal/ctxcompact/...` | [FUZ1][PROP1] |

  - **`nightly.yml`**：
    - `fuzz-long`：`go test -fuzz=FuzzMatchGlob -fuzztime=10m`（FUZ1 长跑）。
    - `bench`：`go test -bench=. -benchmem ./...` → `benchstat` 产物（BENCH1，**不做硬门禁**，仅记趋势/大回归告警）。
    - `race-full`：三平台全量 `-race`（PR 只跑 ubuntu 以控时）。
  - **关键工程细节**：
    - **bubbletea fork**：`replace` 指向仓内 `./third_party/bubbletea`（已 tracked），CI 无需特殊处理；`actions/setup-go` 后直接 `go build`/`go test`。
    - **e2e_real 门禁**：`internal/acp/e2e_real_test.go`、`internal/vcs/e2e_acp_test.go` 带 `//go:build e2e_real`，默认 `go test ./...` **不编译它们**；CI 默认路径无需 `codex`/`claudecode` CLI 或 `YANSHI_E2E=1`。eino/bootstrap 的 `t.Skip`（provider 不可用时）在无 API key 的 CI 上**预期跳过**，非失败。
    - **无需 secrets 跑单测**：默认 CI 不需要任何 API key；`--fake-model`/fake 路径覆盖确定性测试。
    - **缓存与并发**：`actions/setup-go@v5` 带 `cache: true`（`~/go/pkg/mod` + build cache）；PR workflow 用 `concurrency: { group: ci-${{ github.ref }}, cancel-in-progress: true }`。
    - **Windows runner 时长**：race/governance/fuzz 只在 ubuntu 跑；Windows 只跑 `test` + `build`（必要的才跨平台，控成本与时延）。
    - **modernc.org/sqlite 纯 Go**：`go test`/`build` 默认 `CGO_ENABLED=0` 可行，无 cgo 依赖。
  - **门禁阻合并**：`test`/`vet`/`race`/`build`/`governance` 设为 GitHub branch protection 的 required checks（需仓库设置，spec 描述、由人配置）。
  - **分层启用**：若 [GOV1]/[RAC1]/[FUZ1] 等产出尚未落地，对应 job 先以 `continue-on-error` 或 `if:` 条件软启用，待产出就绪后转硬门禁（roadmap 附录 B："E3 → CIG1，治理测试必须先于 CI 门禁存在"）。
- **依赖**：[GOV1][GOV2][GOV3]（E3）、[RAC1][FUZ1][PROP1]（E2）、[BENCH1]（F2，nightly）。**衔接**：CIG1 把这些 batch 的测试产物接进 CI；它们落地前，对应 job 软启用。
- **风险**：
  - CI 时长 → 分层（PR 快/nightly 慢）+ Go cache + Windows 最小化。
  - Windows runner 慢/偶发 flakes → 必要项才上 Windows；重试友好（`go test` 失败重跑）。
  - fork replace 在 CI 解析失败 → 仓内已 tracked，实测无风险；仅在 fork 未提交时出问题（CI 会捕获）。
  - e2e_real 误跑 → build tag 隔离，默认不编译。
- **验收**：
  1. `.github/workflows/ci.yml` 存在，PR/push 触发；三平台 test/build 通过。
  2. `vet` + `race`（ubuntu）通过。
  3. `governance` job 跑 GOV1/GOV2/GOV3（产出就绪后转硬门禁）。
  4. `fuzz-seed` 跑 FUZ1/PROP1；`nightly` 跑长 fuzz + bench。
  5. required checks 配置后，门禁失败阻合并。
- **预估**：2–3d。

---

## 5. [PKG1] 多平台打包分发  (P1 | 缺失)

- **缺口**：`build.sh` 只产 Windows `yanshi.exe`；无多平台构建/分发/release 流程；无 checksum；无变体产物策略。
- **落点**：
  - `.goreleaser.yaml`（新）——构建矩阵 + ldflags + checksum + archive 配置。
  - `.github/workflows/release.yml`（新）——tag `v*` 触发 goreleaser-action。
  - `build.sh`（保留作 dev 工具，文档注明 release 走 goreleaser）。
- **设计**：
  - **构建目标**（roadmap 权威）：`windows/amd64`、`linux/amd64`、`linux/arm64`、`darwin/arm64`。
  - **默认变体（纯 Go，全目标可交叉编译）**：`CGO_ENABLED=0` + `-tags nokeyring`。理由：`modernc.org/sqlite` 纯 Go、bubbletea fork 纯 Go（Windows 的 `key_windows.go` 跨编译进 windows/amd64）、`nokeyring` 关掉 zalando/go-keyring → 四目标都能从任一 host 交叉编译，零 cgo 工具链。
  - **keyring 变体产物**（roadmap："keyring 作变体产物"）：
    - Windows/Linux：keyring 后端纯 Go（wincred/godbus），`CGO_ENABLED=0` 仍可构建 → 产 `-keyring` 后缀变体。
    - **macOS**：go-keyring 走 Security framework（**cgo**），无法从 Linux 交叉编译 → darwin/arm64 的 keyring 变体须在 **native `macos-latest` runner** 上 `CGO_ENABLED=1` 构建。goreleaser 用 `goos: darwin` + 宿主 macos 的 job 实现。
    - 实现 stage 需按锁定的 `go-keyring v0.2.6` 核对 darwin 后端是否确为 cgo（预期是）；若该版本已切纯 Go，则 darwin keyring 变体亦可 `CGO_ENABLED=0`（简化）。
  - **ldflags 注入**（接 VER1）：
    ```yaml
    ldflags:
      - -X github.com/x6nux/yanshi/internal/version.Version={{.Version}}
        -X github.com/x6nux/yanshi/internal/version.BuildStamp={{.Date}} # 或固定 stamp
    ```
    `{{.Version}}` 来自 git tag（goreleaser 内置去 `v` 前缀）；与 VER1 的 `--match 'v[0-9]*'` 一致。
  - **fork 行为保留**：goreleaser 从仓根构建，`replace => ./third_party/bubbletea` 直接解析；Windows 产物的 Ctrl+Enter/Enter 区分行为随 fork 编译进去。
  - **fork 行为验证**：交叉编译的 windows/amd64 二进制上，CI 跑 smoke `./yanshi -h`（exit 0，CLAUDE.md 推荐的自检）。**完整按键行为验证需 TTY，CI 无 TTY**——记为 release 前人工 checklist 项（alt-screen TUI 无法管道驱动，见 CLAUDE.md）。
  - **checksum**：goreleaser `checksum: name_template: 'checksums.txt'`（SHA256），随 release 上传。
  - **release 触发**：`release.yml` `on: push: tags: ['v*']`；用 `goreleaser-action`；产物上传到 GitHub Release。生成 CHANGELOG（接 VER1 的 git-cliff）作为 release notes。
  - **archive 命名**：`yanshi_{version}_{os}_{arch}`，含二进制 + `config.example.yaml` + `README` 片段（可选）。
- **依赖**：[VER1]（版本注入）。
- **风险**：
  - **bubbletea fork 的 Windows 特殊处理** → fork 已 tracked，交叉编译覆盖 windows/amd64；smoke `-h` 验证可启动。
  - **keyring cgo** → 默认 nokeyring 规避；darwin keyring 变体走 native macos runner（见上）；Windows/Linux keyring 变体纯 Go。
  - **交叉编译失败**（遗漏 cgo 依赖） → 默认 `CGO_ENABLED=0` + `-tags nokeyring` 是最稳路径，四目标从 linux 一把出。
  - **checksum 完整性** → goreleaser 自动生成，release 附带。
  - **macOS 仅 arm64** → roadmap 指定 darwin/arm64（Apple Silicon）；Intel/Universal 留后续（out-of-scope）。
- **验收**：
  1. 四目标二进制可构建（默认 nokeyring，从单 host 交叉编译）。
  2. release 产物完整（二进制 + checksums.txt + CHANGELOG notes）。
  3. windows/amd64 产物 smoke `-h` exit 0（fork 编译通过、可启动）。
  4. keyring 变体在能构建的目标上产出（Windows/Linux 纯 Go；darwin native macos）。
  5. tag `v*` 触发自动 release。
- **预估**：2–3d。

---

## 6. [UPG1] 升级兼容 + release doctor  (P1 | 部分 | synthesis O07)

- **缺口**：无 config schema 版本化/迁移；doctor 已覆盖 provider/store/MCP/LSP/port/permissions/secrets 等，但缺 config-version 检查、WAL 检查（F1 后）、keyring 可用性探针、release 维度；无升级指南。
- **落点**：
  - `internal/config/config.go`（改）——`Config` 增 `SchemaVersion`；`Load` 做版本门 + 迁移框架。
  - `internal/config/config_test.go`（改）——版本门/默认值/迁移测试。
  - `internal/cli/doctor.go`（改）+ `doctor_test.go`（改）——新增 `checkConfigVersion`/`checkWAL`/`checkKeyringAvailability`。
  - `docs/upgrade-guide.md`（新）——schema 版本机制 + 升级步骤 + breaking 标注。
- **设计**：
  - **config schema 版本化**：
    ```go
    // 顶层 Config（config.go:42）新增：
    SchemaVersion int `yaml:"schema_version"`
    const SupportedSchemaVersion = 1  // 当前；首次破坏性配置变更时 +1
    ```
    - `Load` 解析后：`SchemaVersion > Supported` → **error**（"config schema_version=%d exceeds supported=%d; upgrade yanshi"）；`SchemaVersion == 0`（字段缺失）→ 视为 `1`（旧配置向前兼容，因 A-D 全是 additive）；`SchemaVersion < Supported`（未来）→ 调 `migrateConfig(cfg, from, to)`。
    - **迁移框架**：v1 无迁移（`migrateConfig` 是 no-op 占位 + 文档化的"加迁移于此"位）。首次破坏性变更时 bump Supported + 加 migration + CHANGELOG major。
    - **不静默改用户文件**：迁移只作用于内存 cfg（或显式 opt-in 写备份）；默认不重写磁盘 config——避免"升级后配置被改"的意外。
  - **doctor 扩展（全只读）**：
    - `checkConfigVersion`：读 `SchemaVersion`，比 `SupportedSchemaVersion`；超高 → fail；等于 → ok；低于（有迁移）→ warn（"will migrate on next boot"）。只读。
    - `checkWAL`（**软依赖 F1**）：`PRAGMA journal_mode` 读回。F1 落地后期望 `wal`；F1 前 → warn（"WAL not enabled yet (F1 pending)"）。只读（不切模式，只 `SELECT` 现状）。
    - `checkKeyringAvailability`：扩展现有 `secrets` 检查——调 keyring `Available()`（sentinel `Get`，只读），报告目标 OS 上 keyring 是否可用；`-tags nokeyring` 构建时直接报 "disabled in this build"。让 release doctor 暴露 keyring 在分发目标上的真实状态。
    - `checkSandbox`：**保留占位但如实**——不改成假装已验证；消息明确"full sandbox verification arrives with S08 (M2)"。release doctor 不对未实现的子系统撒谎。
    - 新增检查遵循既有只读模式（`checkPort` Listen+Close / `checkDatabase` Open+Close / sentinel Get），`DoctorOptions` 不变。
  - **release runbook**（文档，不强制 flag）：发布前 `yanshi doctor` 必须 0 fail（warn 可接受但需人工确认）；`docs/upgrade-guide.md` 记录此步骤。可选 `--release` 轻量模式（把 release-blocking 的 warn 提级为 fail，如 port-in-use/config-version-too-high）——列为可选增强，非本批必需。
  - **升级指南**（`docs/upgrade-guide.md`）：schema_version 机制、A-D→v1.0 的 additive（非破坏）路径、何时 bump schema + 加 migration + CHANGELOG major、配置字段废弃策略（deprecate → warn N 版 → remove）。
- **依赖**：`checkWAL` 软依赖 [WAL1]（F1）；其余 -。
- **风险**：
  - **配置迁移破坏现有** → 版本门（超高拒）+ 前向兼容（缺字段=1）+ 不静默改磁盘文件。
  - **doctor 副作用** → 全检查只读（Listen/Open/sentinel-Get + 立即释放）；与既有约定一致。
  - **sandbox 占位误导** → 保留如实消息，不假装验证。
  - **WAL 检查在 F1 前无意义** → 软依赖 + 占位 warn。
- **验收**：
  1. config schema 版本化：超高 version 拒绝；缺字段兼容；迁移框架存在（no-op）。
  2. doctor 覆盖 config-version / WAL（软依赖）/ keyring-可用性，且既有子系统检查不退化。
  3. 所有 doctor 检查只读（无写副作用、无端口长占）。
  4. `docs/upgrade-guide.md` 存在，含 schema 机制与 breaking 标注。
- **预估**：2d。

---

## 7. 文件结构

| 文件 | 职责 | 新/改 |
|---|---|---|
| `internal/version/version.go` | `Parse`/semver 助手；版本来源文档（tag 注入 vs 常量回落） | 改 |
| `internal/version/version_test.go` | semver 解析/校验、tag 匹配（跳过 m1–m9） | 改/新 |
| `build.sh` | 发布路径从 `git describe --match 'v[0-9]*'` 注入 `-X version.Version` | 改 |
| `CHANGELOG.md` | 自动生成的发布日志（git-cliff 产出） | 新 |
| `cliff.toml` | git-cliff 配置（分组/scope/breaking） | 新 |
| `docs/commit-convention.md` | conventional commit 子集 + scope 约定 | 新 |
| `.github/workflows/ci.yml` | PR/push：test/vet/race/build/governance/fuzz-seed（分层） | 新 |
| `.github/workflows/nightly.yml` | 长 fuzz + bench + 全量 race | 新 |
| `.github/workflows/release.yml` | tag `v*` 触发 goreleaser | 新 |
| `.goreleaser.yaml` | 构建矩阵（4 目标 + 变体）+ ldflags + checksum + archive | 新 |
| `internal/config/config.go` | `Config.SchemaVersion` + `SupportedSchemaVersion` + `Load` 版本门/迁移框架 | 改 |
| `internal/config/config_test.go` | schema 版本门/默认值/迁移测试 | 改 |
| `config.example.yaml` | `schema_version: 1` 示例 + 说明 | 改 |
| `internal/cli/doctor.go` | `checkConfigVersion`/`checkWAL`/`checkKeyringAvailability` | 改 |
| `internal/cli/doctor_test.go` | 新检查的测试（只读断言） | 改 |
| `docs/upgrade-guide.md` | schema 机制 + 升级步骤 + breaking 标注 | 新 |

> 不改任何 `.go` 产品功能代码（除 version/config/doctor 的发布工程扩展）；本批是工程基线，不动 agent/orchestrator/guard 等核心。

---

## 8. 测试策略（Fake 优先 / 只读优先）

- **VER1**：
  - `version.Parse`：合法 semver（含 prerelease/build）、非法拒绝、`v` 前缀剥离。
  - tag 匹配：`v[0-9]*` glob 选中 semver、跳过 `m9-cli-tui` 里程碑标签（用 fake `git describe` 输出或纯字符串单测）。
  - CHANGELOG 生成：用 fixture commit 历史（fake `feat!:`/`feat:`/`fix:`/`chore:`）断言分组与 breaking 提取（git-cliff dry-run）。
- **CIG1**：CI 本身的"测试"是 workflow 在 PR 上的运行结果；本地可用 [`act`](https://github.com/nektos/act) 干跑验证语法，或最小 PR 触发实测。`cmd/testchanged` 不进 CI（CI 跑全量 `go test ./...`，靠 Go cache 控时）。
- **PKG1**：
  - 本地 `goreleaser release --snapshot --clean` 干跑（不发布）验证四目标 + checksum 生成。
  - windows/amd64 产物 smoke：`./yanshi.exe -h` exit 0（fork 编译/启动验证）。
  - darwin keyring 变体：在 macos runner 上验证构建成功（native cgo）。
- **UPG1**：
  - config：超高 version 拒绝、缺字段=1、`migrateConfig` no-op、不写磁盘。
  - doctor：新检查的 ok/warn/fail 路径；**只读断言**（port 检查后端口可再绑、database 检查后连接已 Close、keyring sentinel-Get 无写入）；`-tags nokeyring` 构建下 `checkKeyringAvailability` 报 disabled。
  - doctor JSON/text 输出契约（沿用 `doctor_test.go` 既有 golden 风格）。
- **跨平台**：`go vet ./...` + `go build` 在三平台 CI 上即为跨平台回归。

---

## 9. 风险与缓解

| 风险 | 缓解 |
|---|---|
| CI 时长膨胀（三平台 + race + fuzz + bench） | 分层：PR 快路径（test/vet/race-ubuntu/build/governance/fuzz-seed）/ nightly 慢路径（长 fuzz/bench/全量 race）；Go cache；Windows 最小化（只 test+build） |
| Windows runner 慢/偶发 flake | 必要项才上 Windows；race/governance/fuzz 只在 ubuntu；重试友好 |
| bubbletea fork replace 在 CI/打包解析失败 | fork 在仓内 tracked，CI/goreleaser 直接解析；仅 fork 未提交时失败（CI 会捕获） |
| keyring cgo 打破纯 Go 交叉编译 | 默认 `-tags nokeyring` + `CGO_ENABLED=0`（四目标纯 Go）；darwin keyring 变体走 native macos runner |
| modernc.org/sqlite 跨编译问题 | 纯 Go 驱动，`CGO_ENABLED=0` 可行；CI/build 验证 |
| tag 与常量漂移 / m1–m9 标签污染 | 发布以 semver tag 为准；`--match 'v[0-9]*'` 隔离里程碑命名空间；CI 校验 tag 是 semver |
| CHANGELOG 自动分类误判 | release 时人工校验生成的 CHANGELOG；commit 规范文档约束 |
| 配置迁移破坏现有用户配置 | 版本门（超高拒）+ 前向兼容（缺字段=1）+ 不静默改磁盘文件 |
| doctor 副作用 | 全检查只读（Listen/Open/sentinel-Get + 立即释放），沿用既有约定 |
| sandbox 占位误导发布判断 | release doctor 如实保留占位消息，不假装已验证；完整自检归 S08/M2 |
| D3 同时改 config/doctor 落点冲突 | 执行前 re-verify；spec 标注依赖 D3 的 config/doctor 最终态 |
| 治理/race/fuzz 产出未就绪 | 对应 CI job 软启用（`continue-on-error`/`if:`），产出就绪后转硬门禁 |

---

## 10. 验收标准（batch 级）

1. `--version` 在发布构建显示 semver（tag 注入），dev 构建显示常量 + GitHash。
2. `CHANGELOG.md` 可从 conventional commits 生成；commit 规范文档存在。
3. `.github/workflows/` 存在；PR 上三平台 test/build + vet + race(ubuntu) + governance + fuzz-seed 通过；门禁阻合并。
4. nightly 跑长 fuzz + bench（趋势记录，无硬门禁）。
5. 四目标二进制可构建（默认 nokeyring/CGO_ENABLED=0）；windows 产物 smoke `-h` exit 0。
6. release 产物含 checksums.txt；tag `v*` 触发自动 release。
7. config schema 版本化（超高拒、缺字段兼容、迁移框架在位）。
8. doctor 新增 config-version/WAL/keyring-可用性 检查，全只读；既有检查不退化。
9. `docs/upgrade-guide.md` 存在。

---

## 11. 后续（非本批）

- macOS Intel/Universal 二进制（本批仅 darwin/arm64）。
- 包管理器分发（Homebrew/Scoop/apt/rpm）。
- 自动签名与公证（macOS notarization、Windows code-sign）。
- release artifacts 的 SBOM / 供应链证明（SLSA）。
- 完整 sandbox 自检（S08/M2 落地后，doctor `checkSandbox` 升级为真验证）。
- commit-lint 自动强制（当前靠评审 + CHANGELOG 人工校验）。
- 配置迁移的真实首个用例（第一次破坏性 config 变更时）。

---

## 12. Open questions（需人决策）

1. **CHANGELOG 工具选型**：git-cliff（外部二进制进 CI，成熟/conventional-native）vs 零依赖 Go 小工具（与仓库"无 lint 框架"极简一致）？倾向 git-cliff，待确认对外部二进制的接受度。
2. **版本来源策略**：保留常量作 dev 回落 + tag 覆盖（最小侵入，本 spec 方案）vs 完全 `git describe` 派生（去掉常量，dev 也从 describe 出 `0.0.0-dev-N-gHASH`）？本 spec 取前者以不破坏 dev startup。
3. **keyring 变体范围**：默认 nokeyring 足够 v1，keyring 变体是否纳入 v1 release（需 native macos runner + 额外 CI 分钟）？还是 defer 到 v1 后？
4. **CI 托管**：GitHub Actions（本 spec 假设）确认；GitHub-hosted vs self-hosted Windows runner（成本/速度）？
5. **release 触发**：git tag `v*` 自动 release（本 spec 方案）vs 手动 `workflow_dispatch`？自动更快但需信任 tag 推送者。
6. **`doctor --release` flag**：是否引入（把 release-blocking warn 提级 fail）？本 spec 列为可选，倾向先用 runbook 文档、不加 flag。
