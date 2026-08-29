# 贡献指南（CONTRIBUTING）

本页是给贡献者的**第一步导向 + 承重约定清单**，是 `CLAUDE.md` 的子集与指针，不逐字重复。权威的全景描述与命令见 [`CLAUDE.md`](./CLAUDE.md)；架构决策与不可违反的约束见 [`docs/adr/`](./docs/adr/)。

## 怎么开始

```sh
go build -o yanshi ./cmd/yanshi     # 构建（单二进制 = 客户端 + 服务端）
go test ./...                        # 全量测试
go run ./cmd/testchanged             # 仅测有变更的包（增量）
```

零依赖开发：`--fake-model` 接入确定性 fake model，无需任何 API key（`llm.providers` 为空时自动选）。`cp config.example.yaml config.yaml` 起步（`config.yaml` 已 gitignore）。详见 [`docs/user-guide/getting-started.md`](./docs/user-guide/getting-started.md)。

## 承重架构约定

### 六边形 / 唯一组合根

依赖始终**向内流动**。`internal/bootstrap.Build` 是**唯一**知晓所有 internal 包的组合根——新增组件在此处装配。装配顺序固定且有意义：`config → store → vcs → model → tools → orchestrator → http server → task broker`。非致命启动失败（VCS、插件）打到 stderr 并禁用该子系统继续，而非拒绝整个启动。详见 `CLAUDE.md`「组合根」。

### context 注入是横切模式

鉴权 / 追踪 / scope 状态通过 **context value**（而非工具参数）传递：`tools.WithProfile`、`tools.WithSubAgentRunner`、`tools.WithVCS`。新增需要这些的工具时**从 context 读取**（`internal/tools/permctx.go`、`vcsctx.go`）——不要塞进工具参数。详见 [ADR-0008](./docs/adr/0008-autovcs-context-injection-overrides-scope.md)。

### Guard fail-closed

空 `Tools.Allow` **一律拒绝**（fail-closed）；guard 无状态；shell 命令按 `&&`/`||`/`;`/`|` **拆段逐个判、取最严**，拆不出段的形态（反引号、`$()`、进程替换、子 shell、here-doc、后台 `&`、换行）硬拦截。**新增工具必须显式配权限**，不会因遗忘而静默放行。详见 [ADR-0003](./docs/adr/0003-guard-fail-closed-empty-allow.md) / [ADR-0004](./docs/adr/0004-guard-stateless-and-shell-metachar-hardblock.md)。

### Fake 优先于 mock

优先新增一个 fake（`einollm.FakeModel`、`goalloop.FakePlanner`/`FakeImplementer`、`cli.FakeBackend`、`acp.FakeAgent`）驱动确定性测试，**不**引入 mock 框架。

### 单文件 ≤ 1000 纯代码行

指**纯代码行**（不含注释与空行）。超过先按职责拆分（同包新文件或独立子包），不要在超长文件里继续堆。重复逻辑必须抽成公共函数（禁止复制粘贴）。

### 注释是承重文档

包与导出符号带多段 doc 注释解释**为什么**（尤其在 ADK / guard / VCS 周围）。在这些区域增改时保持同等注释密度。

### 单 binary + 两种传输一套协议

单个 `yanshi` 二进制既是客户端（TUI，本地轻客户端）也是服务端。WebSocket（主）与 SSE（备）**共用同一套 JSON 帧词表**（`internal/proto/frame.go`）；**新增帧类型必须同时更新 `ws.go` 与 SSE handler**。详见 [ADR-0007](./docs/adr/0007-ws-holds-history-sse-replays-shared-proto.md)。

### 本地 fork

`go.mod` 的 `replace` 把 `github.com/charmbracelet/bubbletea` 钉到 `./third_party/bubbletea`（Windows 上区分 Ctrl+Enter 与 Enter）。改 bubbletea 行为改这个 fork——**不要去掉 `replace`**。

## 新架构决策走 ADR

新增或修改承重架构决策时，先写/更新一条 ADR（从 [`docs/adr/0000-template.md`](./docs/adr/0000-template.md) 复制，编号取当前最大 +1），把"不可违反的约束"落进 Consequences。ADR 是单决策演进档案；`CLAUDE.md` 是全景当前态——两者交叉引用而非复制。

## 机器强制的治理门禁（**PR 会红在这里**）

约定不是荣誉制。下面这些由 `go test` 执行，**不看你的意图**：

```sh
go test ./internal/archtest ./internal/bootstrap   # GOV1–GOV9
go run ./cmd/codelines                             # 1000 行上限，即时检查
go run ./cmd/covercheck                            # 每包覆盖率下限
go run ./cmd/featurestatus                         # 功能状态台账（-open 只列未结项）
```

| 门禁 | 拦什么 | 你会怎么撞上 |
|---|---|---|
| GOV1 | 六边形分层与依赖方向；`bootstrap` 是唯一组合根 | 新增一条跨包 import |
| GOV2 | `internal/`、`cmd/` 下非测试文件 ≤ 1000 **纯代码行** | 在长文件里继续堆代码（先拆再写） |
| GOV3 | 导出符号必须有 doc 注释 | 新增导出符号忘了写注释 |
| GOV4 | `bootstrap` 里每个导出的 `Build*` 必须从 `Build` 可达 | 写完零件没接进组合根 |
| GOV5 / GOV7 | profile 允许的、以及 guard 免提示集合里的工具名必须真的注册 | 幻影工具名 |
| GOV6 | 每个 `With<X>(ctx, …)` 注入器必须有生产调用点 | 注入器没人调，整条消费链静默读零值 |
| GOV8 | 台账终态条目逐句对账（子句 → 测试引用 → 测试 doc 注释回写） | 翻牌但没写证据，或改了 acceptance 没改 pin |
| GOV9 | 活文档里的 `路径::符号` 引用必须解析得到 | 改名后没更新文档引用 |

**豁免表只能减不能加。** 每张表都做死条目检测（条目已合规、或主体已消失都判失败），
所以「新增一条违规 + 同时新增一行豁免」虽然测试会绿，但**这是 code review 挡的事**。

**改了这些就必须重跑生成器并提交结果**，否则 `docs.yml` 的 `git diff --exit-code` 会红：

| 你改了 | 重跑 |
|---|---|
| `internal/config.Config` | `go run ./cmd/gendocs -config docs/user-guide/configuration.md` |
| 任何子命令的 `-h` 文本 | `go run ./cmd/gendocs -help-all docs/user-guide/tui.md docs/user-guide/entrypoints.md` |
| `sdk/schema/v1` 或 `internal/api/v1` 的 wire 形状 | `go run ./cmd/api-schema -markdown docs/api/schema.md` 与 `… docs/api/resources.md` |

**对外契约改动要四处同改**：Go struct、`sdk/schema/v1`、`sdk/ts/v1.ts`、`sdk/python` 的
pydantic 模型。后两者是**手工镜像**（不是生成的），漏改一处
`internal/api/v1/parity_test.go::TestContractParityAcrossFourSources` 会红。

## 提交 / PR 约定

使用 **conventional commit** 前缀（`feat:` / `fix:` / `docs:` / `refactor:` / `test:` / `chore:` / `ci:` 等），与 CHANGELOG 自动生成对齐。例如：

```
feat(guard): reject shell metacharacters regardless of glob allowlist
docs(adr): add ADR-0011 for ...
```

每个 batch task 一个小 commit；PR 标题同此约定。

## 被忽略的产物（不要提交）

`config.yaml`、`*.db`（运行时 SQLite，含 `yanshi.db`）、构建二进制（`yanshi`、`yanshi.exe`）都被 gitignore。不要提交它们。
