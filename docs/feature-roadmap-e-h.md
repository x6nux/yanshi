# Yanshi v1.0 生产可用路线图 — Tier E–H（质量 / 性能 / 发布 / 文档）

> **生成日期**：2026-07-22
> **命题**：A-D（对齐 codex/deepseek 功能面）已完成；E-H 转向"把功能面做成可发布、可信赖的 v1.0"。**不再加新功能面**，转而补 `synthesis-final.md` 评的最弱维度（正确性 7 / 测试 7 / 可观测性 6 → 目标 8.5+），并建立可持续的发布与质量机制。
> **方法**：基于 `docs/synthesis-final.md`（07-20，27 项行动 P0-P3 + 风险登记 R1-R16 + 测试缺口 §8）+ `docs/superpowers/specs/2026-07-22-multimodal-vision-design.md`（G spec）+ A-D 执行后实测状态（610 commits，前 13 批已落地），定义 4 tier / 8 batch。
> **前置文档**：`docs/feature-roadmap-codex-deepseek.md`（A-D 源，§0/§1/§2 的约定本文件沿用）、`docs/synthesis-final.md`（行动/风险来源）。
> **上次代码扫描**：2026-07-22 · **Go** 1.26.4 · **模块** `github.com/x6nux/yanshi` · **610 commits**

---

## 0. 阅读说明

### 0.1 字段定义（沿用 A-D §0.1）

- **状态**：`缺失` · `占位` · `部分` · `已完成`
- **优先级**：`P0`（阻塞 v1.0 发布）· `P1`（关键加固）· `P2`（体验/可持续）
- **ID**：E-H 用新域前缀（见 §0.4），与 A-D 的 `S/V/G/M/DT/UX/...` 不冲突。
- **依赖**：引用本表 ID 或 A-D ID；`-` 表示无前置。
- **预估**：`h`/`d`/`w`，含设计+实现+测试。

### 0.2 条目格式（沿用 A-D §0.2）

```
### [ID] 功能名  (优先级 | 状态 | 来源)
- 缺口 / 落点 / 设计 / 依赖 / 风险 / 验收 / 预估
```

### 0.3 架构接法约定（完全沿用 A-D §0.3，不再展开）

装配走唯一组合根 `internal/bootstrap/Build`；鉴权/scope 走 context 注入；安全走 guard 四维 fail-closed；传输新帧同步 `proto/frame.go`+`ws.go`+`ssebackend.go`；测试 Fake 优先；外部依赖缺失走软降级。

### 0.4 E-H 域前缀

| 前缀 | 领域 | 所属 tier |
|---|---|---|
| `COV` | 测试覆盖补齐 | E |
| `FUZ` / `PROP` / `RAC` | fuzz / 属性测试 / race | E |
| `GOV` | 架构治理（分层/行数/文档） | E |
| `WAL` | SQLite 并发 | F |
| `LEAK` / `CCL` / `BENCH` | 资源泄漏 / 压缩冷却 / 基准 | F |
| `VER` / `CIG` | 版本 / CI 门禁 | H |
| `PKG` / `UPG` | 打包分发 / 升级兼容 | H |
| `VISION` / `IMG` / `CLIP` | 多模态理解 / 图片存储 / 剪贴板 | G |
| `UDOC` / `APIREF` / `ADR` | 用户文档 / API 参考 / 决策记录 | H2 |
| `EX` / `CONTRIB` | 示例 / 贡献 | H2 |

---

## 1. 执行摘要

### 1.1 命题转变

| 维度 | A-D（已完成） | E-H（本文件） |
|---|---|---|
| 目标 | 功能对齐 codex/deepseek（55 项缺口 → 0） | 从"功能完备"到"生产可用 v1.0" |
| 手段 | 加新功能面、新工具、新协议 | **不加新功能**；补质量/性能/发布/文档 |
| 杠杆 | 用户体感、能力广度 | 正确性、可发布性、可维护性 |

E-H 不与 codex/deepseek 对照——功能面已对齐。它的"对照源"是 `synthesis-final.md` 评出的最弱维度与未关闭的债。

### 1.2 缺口全景（synthesis 实测 + A-D 后复核）

| 维度 | 现状（synthesis 共识） | v1.0 目标 | 对应 tier |
|---|---|---|---|
| 正确性 | 7/10（死代码已清，但覆盖弱） | 8.5+ | E |
| 测试 | 7/10（store 58% / proto 67% / bootstrap 23%） | 薄弱包 ≥75% | E |
| 可观测性 | 6/10（slog/OTel 已由 C4 落地） | 8（OTel 默认开、采样可配） | F/C4 收尾 |
| 性能 | 7/10（无 WAL、无基准） | 8（并发不退化、有基线） | F |
| 代码规范 | 7/10（治理无自动化） | 8（CI 门禁） | E3/G1 |
| 发布 | 无版本/CI/打包流程 | 可发多平台 v1.0 | G |

> **注**：C4（OBS1 slog / OBS2 OTel / OBS3 features / COST1 pricing / O07 doctor）已落地，可观测性从 6 提升的主要工作在 F2（OTel 采样/基线）与 G2（release doctor），不单列批次。

### 1.3 分批总览（4 tier / 8 batch）

| 批 | 名称 | 项数 | 优先级 | 预估 | 解锁 |
|---|---|---|---|---|---|
| **E1** | 测试覆盖补齐 | 3 | P0 | ~1w | 持久化/协议/组合根可信 |
| **E2** | fuzz / property / race | 3 | P0/P1 | ~1w | 安全与压缩核心机器保证 |
| **E3** | 架构治理测试（CI） | 3 | P0 | ~0.5-1w | 防止 A-D 引进的债再生 |
| **F1** | SQLite 并发 | 1 | P1 | ~0.5w | 并发写不退化 |
| **F2** | 资源治理与压测基线 | 4 | P1 | ~1w | 长跑稳定、成本可见 |
| **G** | 多模态理解 ⭐ | 5 | P1 | ~1.5-2w | 图像理解能力（A-E 五入口） |
| **H1** | 发布工程 | 4 | P0/P1 | ~1.5w | 版本/CI/打包/升级/doctor |
| **H2** | 文档 / 示例 / 贡献 | 5 | P2 | ~1.5w | 对外可用/生态可参与 |

**总预估**：约 **8-10w 串行**（单人全职）；E1-E3/F1 与 G/H 可部分并行，墙钟约 **6-7w**。G 多模态是 E-H 唯一的新功能面（其余为加固/文档），耗时约 1.5-2w。

### 1.4 推荐执行顺序

```
E3（治理测试）尽早插入 ── 防止 A-D 债再生，且独立无依赖
E1（覆盖）─→ E2（fuzz/property/race）── 质量地基，E2 的 RAC 会暴露 F2 的 LEAK
   │
   └─→ F1（WAL）─→ F2（资源/压测）
                       │
                       ├─→ G（多模态）── 新功能面，路径不冲突，可并行
                       │
                       └─→ H1（发布/CI）─→ H2（文档/示例）── 可发 v1.0
```

- **E3 优先**：架构治理测试一旦入 CI，后续所有改动受保护，越早越好。
- **E2 反哺 F2**：`-race` 固化后大概率暴露 `createdWT`/子代理并发的既有竞态，直接喂给 F2。
- **G（多模态）是唯一新功能**，路径与 E/F 不冲突（分别改 provider/config 与 store/guard），可与 E/F1/F2 并行。
- **H 最后**：发布工程依赖 E2/E3 的 CI 产出去接，文档应在行为稳定后写。

### 1.5 实施流程（沿用 A-D）

每个 batch 走与 A-D 完全相同的管线：

1. **brainstorm**（本文件即各 batch 的总纲；个别 batch 若有设计争议再单独 brainstorm）
2. **spec** → `docs/superpowers/specs/YYYY-MM-DD-<batch>-design.md`
3. **writing-plans** → `docs/superpowers/plans/YYYY-MM-DD-<batch>.md`（任务粒度、RED→GREEN、每 Task 一提交）
4. **评审管线**：3× PASS（reality / TDD / security 三视角复审 + 修复循环）
5. **subagent 执行**：按依赖序逐 Task，每 Task 跑定向测试后提交

> **并行注意**：D3 当前由另一 agent 在跑（secrets/auth/i18n/keymap）。E-H 执行前需 **re-verify 当前代码状态**——D3 改动 `bootstrap`/`store`/`config`/`ws.go`/`chat.go`，可能与 E1(COV store/bootstrap)、F1(WAL)、G(bootstrap 配辅助模型加 image 字段)、H1(doctor) 的工作面重叠。每个 batch 的 spec 阶段先 `git log` + 实测确认落点未被改写。

---

## 2. 前置基线（A-D 交付物 + 已知债）

### 2.1 A-D 已落地（git 实测，610 commits）

- **A1 安全执行**：`execpolicy`（解析+前缀规则）、`sandbox`（Phase 0 能力骨架）、`netpolicy`（loopback proxy+deny-wins）、`shell`（manager+PTY/KillTree+job 持久化）
- **A2 任务/计划**：`task` broker worktree 分发；durable task 模型（`task/work`）
- **A3 MCP**：完整 client（JSON-RPC framing、stdio/HTTP/OAuth、connection manager、reconnect、管理帧）
- **B1 子代理**：registry + 生命周期
- **B2 编辑反馈**：`internal/lsp`（Manager+Client+didOpen/didChange+诊断）、逐轮回滚
- **B3 开发者工具**：skills 安装/信任、git/test/diag 类工具
- **C1 批量**：`rlm` 并发查询、`automation` 调度
- **C2 TUI**：UX1-7（action palette、F1 help、@attach、frecency、stash、history、toast）
- **C3 会话/记忆**：user memory、fork、side、skills install/reload
- **C4 可观测**：slog redacting logger、OTel OTLP、feature registry、pricing/`$`、doctor
- **D1 headless/API/app-server**：版本化 v1 资源、SSE thread/turn/item、JSON-RPC app-server、schema
- **D2 SDK/IDE**：TS+Python SDK、VS Code 扩展
- **D3**（进行中）：secrets S10-L1 已落，auth/i18n/keymap 待完

### 2.2 已知债（E-H 要关的）

来自 `synthesis-final.md` 风险登记 + B0 残余 + D2 已知前瞻缺口：

| 债 | 来源 | 归属 |
|---|---|---|
| `store` 覆盖 58% | synthesis §8.2 / R5 | E1 |
| `proto` 覆盖 67% | synthesis §8.2 / A11 | E1 |
| `bootstrap` 覆盖 23% | synthesis §8.2 / R12 | E1 |
| `guard.MatchGlob` 无 fuzz | synthesis R16 / A24 | E2 |
| `ctxcompact` 无属性测试 | synthesis A16 | E2 |
| 无 `-race` CI 固化 | synthesis R2 | E2 |
| 无架构治理自动化 | synthesis S2 / A23 | E3 |
| 超长文件（`tools/agent.go`~900 / `tui/model.go`~850） | synthesis 附录A / A7/A17 | E3 |
| SQLite 无 WAL/连接池 | synthesis R10 / A15 | F1 |
| `createdWT` map 泄漏 | synthesis R13 / A19 | F2 |
| 子代理无并发上限 | synthesis A10 | F2 |
| mid-turn 压缩无 cooldown | synthesis R3 / A8 + B0 残余 | F2 |
| 无基准基线 | synthesis A25 | F2 |
| ACPImplementer usage 不回流 | B0 残余 | F2 |
| 无版本/CI/打包流程 | — | H1 |
| 无用户文档/examples/CONTRIBUTING | — | H2 |
| 无多模态图像理解能力（A-D 功能面完成后的最大缺口） | — | G |
| 同上 | — | E3 |

### 2.3 E-H 之外的债（明确不纳入，归原系列后续）

下列为 D 系列前瞻缺口，**不在 E-H 范围**，避免与 D3/D 后续冲突：

- D1 cursor-based stream replay（SDK recovery 是前瞻）
- D1 WS transport（SDK WS 仅 fake 测）
- IDE `ContextItem`/`FileChange` 仍 provisional

这些归 "D 系列后续"，E-H 完成后再评估。

---

## Tier E — 质量地基（P0）

> 直接打 synthesis 最弱维度。E1 补覆盖，E2 给核心以机器保证，E3 把架构承诺变成 CI 门禁。三者独立，可并行起步。

## 3. Batch E1 — 测试覆盖补齐

### [COV1] `store` 覆盖 58% → 75%+  (P0 | 部分 | synthesis A14/R5)

- **缺口**：持久化核心（会话/任务/VCS/auth_metadata）测试薄弱；`store` 是扇入 7、零内部依赖的关键包，覆盖 58% 不可接受
- **落点**：`internal/store/`（改 `session_test.go` 等扩展）
- **设计**：覆盖会话 CRUD 全路径（create/append/分页/fork/side）、任务生命周期（Pending→Running→Completed/Failed/Cancelled）、VCS 写入与分支、`auth_metadata` upsert/load/delete；用 `t.TempDir()` 临时 SQLite，不依赖外部；表驱动 + 边界（空、超大 content、并发 append）
- **依赖**：-
- **风险**：临时 DB 清理→`t.TempDir`；并发写场景需配合 F1 WAL 才稳定→先单连接测，并发留 F1 后补
- **验收**：覆盖率 ≥75%；关键路径全覆盖；边界场景有测试
- **预估**：3-4d

### [COV2] `proto` 覆盖 67% → 80%+  (P0 | 部分 | synthesis A11)

- **缺口**：`SSEEvent`/帧序列化/golden 缺口；A-D 新增帧（cost/features/memory/fork/side/skills/mcp 等）需回归
- **落点**：`internal/proto/`（改测试）
- **设计**：全帧类型 JSON 往返、`SSEEvent()` 输出 golden 文件（`event:`/`data:` 行）、未知字段兼容、版本字段、SSE 与 WS 词表对称性断言
- **依赖**：-
- **风险**：golden 文件维护→提供 `go test -update` 重新生成
- **验收**：覆盖率 ≥80%；全帧往返；SSE golden 稳定；WS/SSE 词表对称
- **预估**：2d

### [COV3] `bootstrap` 集成测试 23% → 50%+  (P0 | 缺失 | synthesis A18/R12)

- **缺口**：组合根（唯一知晓所有 internal 包的包）几乎无集成测试；装配顺序与软降级无机器验证
- **落点**：`internal/bootstrap/`（改 `bootstrap_test.go`）
- **设计**：最小 `App` 构建（fake model + 内存/临时 store + 禁用 VCS）；验证装配顺序（config→store→vcs→model→tools→orchestrator→http→task）、软降级路径（VCS/插件/MCP/LSP 失败不阻塞，打 warning）、`Options.Cfg` 注入（不经磁盘）；一条端到端 turn（fake model 回一句 + 一个工具调用，断言工具结果回喂）
- **依赖**：-
- **风险**：启动副作用（端口/文件）→内存 store + 临时目录 + 不绑真实端口；与 D3 新增的 `Redactor`/`Auth` 字段同步
- **验收**：覆盖率 ≥50%；最小 App 可构建并跑一轮 turn；软降级被验证
- **预估**：2-3d

---

## 4. Batch E2 — fuzz / property / race

### [FUZ1] `guard.MatchGlob` fuzz  (P1 | 缺失 | synthesis A24/R16)

- **缺口**：glob 匹配（tools/fs/shell/net 白名单核心）无 fuzz，边缘（嵌套 `**`、转义、超长、`../`）未覆盖
- **落点**：`internal/guard/`（`FuzzMatchGlob`）
- **设计**：`go test -fuzz`；种子用已知绕过样例（IFS、glob 注入、`../`、重复通配）；断言不 panic + 匹配结果与文档语义一致；CI 跑种子语料（`-fuzz=time`），本地长 fuzz
- **依赖**：-
- **风险**：fuzz 暴露语义 bug→建回归 corpus 并修；性能退化→单次匹配超时
- **验收**：fuzz 目标存在；种子语料入仓；已知绕过有回归；CI 跑语料
- **预估**：1-2d

### [PROP1] `ctxcompact` 属性测试  (P1 | 缺失 | synthesis A16)

- **缺口**：压缩核心无属性测试，"压缩不丢失关键信息/不切断工具对"无机器保证
- **落点**：`internal/ctxcompact/`（`testing/quick` 或随机生成器）
- **设计**：不变量：① pin 集 ⊆ 压缩后历史；② tool_call/result 配对不被切断（`EnforceToolCallPairs` fixpoint 成立）；③ summary 输入 ≤ 窗口；④ 携带式分块每次严格 ≤ 窗口；随机生成历史（混合 role/长度/工具对）
- **依赖**：-
- **风险**：属性过强误报→只断言不变量，不断言摘要质量；随机生成器要覆盖工具对
- **验收**：≥3 个属性；随机输入通过；工具对配对不变量成立
- **预估**：2d

### [RAC1] race detector 固化 + 并发热点测试  (P0 | 部分 | synthesis R2)

- **缺口**：无 CI `-race` 固化；`ws_conn`/`runners sync.Map`/`task broker`/`registry` 是竞态热点
- **落点**：CI（`go test -race ./...`）+ 关键包并发测试
- **设计**：CI 跑 `-race`；补 `wsConn.write` 并发写、`runners sync.Map` 按 model 指针缓存、broker claim/requeue、registry 读写锁、`createdWT` map 并发的测试；暴露的既有竞态修在 F2
- **依赖**：-
- **风险**：race 暴露既有 bug→记录后归 F2 LEAK 修；`-race` 慢→CI 分层（PR 跑变更包，merge/nightly 跑全量）
- **验收**：CI `-race` 通过；关键并发点有测试；发现的竞态登记到 F2
- **预估**：2-3d

---

## 5. Batch E3 — 架构治理测试（CI）

> synthesis 多处强调"每 PR 代码审查检查"但无自动化。本批把这些口头承诺变成 CI 门禁。**建议最先做**——它保护后续所有 batch。

### [GOV1] 依赖分层治理测试  (P0 | 缺失 | synthesis S2/A23)

- **缺口**：六边形分层（guard/store/proto/vcs 零内部依赖、依赖向内流动、无循环、bootstrap 唯一全知）靠人工，无门禁
- **落点**：新 `internal/archtest/`（或 `cmd/archtest`）+ CI
- **设计**：解析 `go list -deps`/导入图；断言 `guard`/`store`/`proto`/`vcs` 零内部依赖（或符合白名单）、`bootstrap` 扇出含全部 internal 包、无循环、`config` 不应依赖 `guard`（W2）；失败给可读报告（违反的边）
- **依赖**：-
- **风险**：历史违规误报→白名单 + 增量启用；`go list` 慢→缓存
- **验收**：分层规则可执行；`guard` 零内部依赖被锁定；违规 PR 被 CI 拦
- **预估**：2d

### [GOV2] 文件纯代码行数门禁 + 超长文件拆分  (P1 | 部分 | synthesis S6/W4/W5/A7/A17)

- **缺口**：1000 纯代码行约束靠人工；当前 `ws.go`(1385 纯代码行)、`tools/agent.go`(1134)、`tui/model.go`(1030) **全部超标**（E3 spec agent 实测，2026-07-22 快照；B0 的 857 是 07-21 旧数，A-D 后 ws.go 增长了 ~530 行）
- **落点**：CI 脚本 + 现有三个超长文件拆分
- **设计**：脚本统计 `.go` 纯代码行（去注释/空行，口径与 CLAUDE.md 一致）；CI 门禁 >1000 失败；现状三文件全拆：`ws.go`→按 ws_conn/ws_perm/ws_session/ws_model 拆分（复用 A6 方案）、`agent.go`→agent/workflow/analysis（移顶层声明零风险）、`model.go`→model/state/handlers（Update ~575 行 switch 用 extract-method + golden 对比守回归）
- **依赖**：-
- **风险**：拆分引入回归→拆分前后 `go test ./...` 全绿；model.go 的 extract-method 有回归风险→golden 对比守门
- **验收**：门禁脚本入 CI；现有超长文件已拆且测试全绿；新增超长被拦
- **预估**：拆分 3d + 门禁 0.5d

### [GOV3] exported symbol 文档覆盖  (P2 | 缺失 | synthesis A26)

- **缺口**：约 6 个包有未文档化导出符号；与"注释是承重文档"约定不符
- **落点**：全仓库补 doc 注释 + CI 检查
- **设计**：CI 检查 exported symbol 有 doc 注释（`go vet`+ 自写检查，或引入最小 lint 配置——仓库现无 golangci-lint 配置）；补齐缺失注释
- **依赖**：-
- **风险**：检查工具选型→优先零依赖脚本，避免引入 lint 框架
- **验收**：exported symbol 全文档化；CI 检查存在
- **预估**：1-2d

---

## Tier F — 性能与并发（P1）

## 6. Batch F1 — SQLite 并发

### [WAL1] WAL 模式 + 连接池 + busy_timeout  (P1 | 缺失 | synthesis R10/A15)

- **缺口**：SQLite 默认 journal 模式，并发写竞争；无连接池；长会话/多 worktree 下 `database is locked` 风险
- **落点**：`internal/store/`（改 `store.go`/`session.go`）
- **设计**：启动 `PRAGMA journal_mode=WAL` + `synchronous=NORMAL` + `busy_timeout=<ms>`；连接池（`database/sql` `SetMaxOpenConns`，读多写一或写串行化）；并发写测试（多 goroutine append）；旧 DB 自动升级（首次连接 PRAGMA）
- **依赖**：-
- **风险**：WAL 文件膨胀→checkpoint 策略（`PRAGMA wal_checkpoint`）；Windows WAL 行为→跨平台测试；与 D3 的 store 改动（redactor/auth_metadata）落点冲突→先 re-verify
- **验收**：WAL 启用；并发写不报 locked；性能不退化；旧 DB 平滑升级；WAL 文件有界
- **预估**：2-3d

---

## 7. Batch F2 — 资源治理与压测基线

### [LEAK1] `createdWT` map 泄漏清理  (P1 | 缺失 | synthesis R13/A19)

- **缺口**：`task/broker.go` 的 `createdWT` map 在 RequeueStale/取消路径不回收，长跑内存增长
- **落点**：`internal/task/broker.go`
- **设计**：梳理 worktree 生命周期（claim→完成/失败/取消→清理）；终态显式删 map 项；测试：长跑 + 断言 map 不增长
- **依赖**：[RAC1]（并发正确性）
- **风险**：过早清理→状态机明确终态才删；并发→registry/broker 锁 + `-race`
- **验收**：`createdWT` 在任务终态清理；长跑测试 map 有界
- **预估**：1d

### [LEAK2] 子代理并发上限  (P1 | 缺失 | synthesis A10)

- **缺口**：子代理只有深度上限（`MaxSubAgentDepth`），无并发总数上限，可 goroutine 爆炸
- **落点**：`internal/tools/subagent.go` + `internal/agent/registry/`
- **设计**：并发计数（只数 running）；满则 spawn 返回 cap 错误；上限可配（默认 10，硬上限 20）；与 B1 的 M04b 对齐
- **依赖**：-
- **风险**：计数竞态→registry 锁；**执行前先核对 B1 是否已做 M04b 并发上限**，避免重复
- **验收**：并发上限生效；满则拒绝；计数准确；与深度上限交互文档化
- **预估**：1-2d

### [CCL1] mid-turn 压缩 cooldown  (P2 | 缺失 | synthesis R3/A8 + B0 残余)

- **缺口**：B0 说 threshold 天然门控，但 mid-turn（`CompactingModel`）无显式 cooldown，同一 turn 仍可能多次压缩
- **落点**：`internal/llm/eino/compacting.go` + `internal/ctxcompact/`
- **设计**：记录上次压缩的 token/时间；cooldown 内即使超阈值也延后（除非逼近硬上限则强制）；统一 `keepRecent` 语义文档化（`CompactingModel` 消息数 vs `PlanOpts` 对数，`/2` 桥接）
- **依赖**：-
- **风险**：cooldown 漏压→逼近硬窗口时强制压缩兜底；语义统一破坏现有→保留 `/2` 桥接
- **验收**：同 turn 不重复压缩；逼近上限仍触发；`keepRecent` 文档清晰
- **预估**：1-2d

### [BENCH1] 性能基准基线  (P2 | 缺失 | synthesis A25)

- **缺口**：无性能基线，`vcs`/DAG/`fs_edit`/orchestrator 性能不可观测、回归不可发现
- **落点**：各包 `_bench_test.go` + CI 记录
- **设计**：`BenchmarkVCSCommit`/`DAGApply`/`FSEdit`/`OrchestratorTurn`；用 `FakeModel`；CI 跑并用 `benchstat` 记录趋势；不做硬门禁（防噪声），回归 >N% 告警
- **依赖**：-
- **风险**：噪声→多次运行 + benchstat；环境差异→只做相对比较
- **验收**：关键路径有基准；CI 记录趋势；大回归可发现
- **预估**：2d

### [LEAK3] ACPImplementer usage 回流  (P2 | 缺失 | B0 残余)

- **缺口**：外部 CLI（codex/claudecode）的 token 未进 `UsageSink`，goal loop budget 只算 planner/evaluator
- **落点**：`internal/agent/goalloop/implementer.go`
- **设计**：解析 ACP 子进程的 usage 事件/输出；回流到 `UsageSink`；budget 含子进程；解析失败不阻塞
- **依赖**：-
- **风险**：各 CLI 输出格式不一→尽力解析 + 降级（0 不崩）；异步→不阻塞 turn
- **验收**：ACP turn usage 进 sink；budget 含子进程；解析失败安全降级
- **预估**：1-2d

---

## Tier G — 多模态理解（P1）

> 给非原生多模态的主模型兜底图像理解——主模型不换，图像理解按需走"辅助多模态模型 + `image_describe` 工具"代理。详见 `docs/superpowers/specs/2026-07-22-multimodal-vision-design.md`。

## 8. Batch G — 多模态理解

### [VISION] 能力声明 + 辅助模型 + turn 分流  (P1 | 缺失)

- **缺口**：A-D 功能面已全但缺图像理解；主模型非多模态时无法看图
- **落点**：`internal/config/config.go`（`ProviderConfig.Multimodal bool`）+ `internal/bootstrap/bootstrap.go`（辅助自动选、`providerMultimodal` map）+ `internal/agent/orchestrator/orchestrator.go`（per-turn 图像分流）
- **设计**：`multimodal: bool` 声明；辅助模型自动选第一个 `multimodal: true` 的 provider（无 `vision_model` 字段）；turn 构建时按当前 model-id 查 `MultimodalMap` 分流：主多模态→图直接进 `schema.Message` image part；主非多模态→图进会话级 image store + 占位 `[image:img-N]` → 模型显式调 `image_describe` → 工具路由给辅助模型返回文本；未配辅助→启动 warning + `image_describe` 返回明确配置错误
- **依赖**：[D3]（config/bootstrap 落点 re-verify）
- **风险**：D3 同时改 config/bootstrap→执行前 `git log` 确认；eino schema 图像字段在不同 provider kind 下不一致→实现时按锁定版本核对
- **验收**：主多模态：图直接通过消息内容到达；主非多模态+有辅助：占位+image_describe 走通；无辅助：error 而非静默
- **预估**：2-3d

### [VISION-TOOL] `image_describe` + image store + 五入口  (P1 | 缺失)

- **缺口**：非多模态模型无法理解图片；无统一图存储/引用机制
- **落点**：新 `internal/tools/vision.go`（`image_describe(image_ref, question?)→string` GuardedTool）+ `internal/imagestore/store.go`（会话级 LRU image store）+ `internal/clipimg/`（跨平台剪贴板图像 adapter 子进程为主 cgo 门控）+ `internal/tools/screenshot.go`（截图工具 approval-required）+ `internal/api/v1/types.go` `InputItem` 加 image 字段 additive + SDK 类型重生成
- **设计**：`image_describe` 纯显式不自动调；五入口全做 A-E（剪贴板 Ctrl+V/ `@path` 文件/`fs_read`·`web_fetch` 遇图/截图工具/ headless-SDK-IDE 协议传图）；限制 png/jpeg/webp/gif、单图≤10MB、长边>2048px 降采样、store 20 张/100MB LRU；辅助模型 usage 进 `UsageSink`（标 `vision`）计 budget 与 `/cost`
- **依赖**：[VISION]（config 与辅助模型先行）
- **风险**：剪贴板 cgo 与 `CGO_ENABLED=0` PKG1 打包冲突→子进程路线兜底；多平台截图重→可拆子任务；协议加 image 字段须 additive（不破坏 v1 契约）
- **验收**：五入口各自可产生图像附件；image_describe/id-ref+path-ref 走通；超限/越权被拒；费用纳入 /cost
- **预估**：4-5d

---

## Tier H — 发布工程与文档（P1/P2）

> H1=发布工程，H2=文档/示例/贡献。原 G tier 的 ID 前缀(VER/CIG/PKG/UPG)和 H tier 的 ID 前缀(UDOC/APIREF/ADR/EX/CONTRIB)保持不变，重排后统一归 H。

## 9. Batch H1 — 发布工程

### [VER1] 语义化版本 + CHANGELOG 自动化  (P1 | 缺失)

- **缺口**：`internal/version` 有版本字符串但无发布节奏；无 `CHANGELOG`
- **落点**：`internal/version/` + `CHANGELOG.md` + 构建脚本
- **设计**：semver；版本号来源统一（ldflags 注入 git tag，或 `version.go` 常量）；`CHANGELOG` 从 conventional commit prefix（`feat/fix/chore/breaking`）用 `git-cliff` 自动生成；定 commit 规范子集
- **依赖**：-
- **风险**：历史 commit 不规范→从本批起规范，旧条目手补；自动分类误判→release 时人工校验
- **验收**：版本号来自 git tag；`CHANGELOG` 可生成；发布流程文档化
- **预估**：1-2d

### [CIG1] CI 门禁矩阵  (P0 | 缺失 | synthesis A23)

- **缺口**：无统一 CI（靠本地 `go test`/`vet`）；跨平台/多 Go 版本/治理/fuzz 未自动化
- **落点**：`.github/workflows/`（从零建 `ci.yml`/`nightly.yml`）
- **设计**：矩阵 {Windows, Linux, macOS} × {`go test`, `go vet`, `-race`, `build`}；E3 治理测试（GOV1/GOV2/GOV3）入 CI；E2 的 FUZ1/PROP1 与 F2 的 BENCH1 各自 job（PR 跑种子/属性，nightly 跑长 fuzz/bench）；门禁失败阻合并
- **依赖**：[GOV1][GOV2][RAC1][FUZ1][PROP1]
- **风险**：CI 时长→分层（PR 变更包 / merge 全量 / nightly fuzz+bench）；Windows runner 慢→必要的才跨平台
- **验收**：CI 矩阵存在；跨平台 build/test 通过；治理/竞态/fuzz/属性入 CI
- **预估**：2-3d

### [PKG1] 多平台打包分发  (P1 | 缺失)

- **缺口**：单二进制设计但无多平台构建/分发流程
- **落点**：`build.sh`（已存在）/release workflow +（可选）包管理器 formula
- **设计**：goreleaser 或自写脚本；目标 `windows/amd64`、`linux/amd64+arm64`、`darwin/arm64`；构建矩阵含 bubbletea fork（`replace` 指令）与 `nokeyring` build tag；release 附 checksum
- **依赖**：[VER1]
- **风险**：bubbletea fork 的 Windows 特殊处理→构建矩阵覆盖；keyring CGO 依赖→默认 `CGO_ENABLED=0` + `-tags nokeyring`，keyring 作变体产物；fork 行为（Ctrl+Enter）需在产物上验证
- **验收**：多平台二进制可构建；release 产物完整；checksum；fork 行为保留
- **预估**：2-3d

### [UPG1] 升级兼容 + release doctor  (P1 | 部分 | synthesis O07)

- **缺口**：无升级/配置兼容策略；`doctor` 基础已有（C4 扩展了 MCP/LSP/permissions），未含 release 前自检
- **落点**：`internal/cli/doctor.go` + 升级文档
- **设计**：配置 schema 版本化 + 迁移；`doctor` 扩展 release 自检（provider/store/WAL/sandbox/MCP/LSP/端口/权限/keyring，加 `--release` flag）；升级指南（breaking change 标注）
- **依赖**：[F1]（WAL 检查）
- **风险**：配置迁移破坏→版本门 + 回滚；doctor 副作用→全只读检查
- **验收**：配置版本化；`doctor` 覆盖各子系统；升级指南存在
- **预估**：2d

---

## 10. Batch H2 — 文档 / 示例 / 贡献

### [UDOC1] 用户指南  (P2 | 部分)

- **缺口**：无面向用户的"怎么用 yanshi"指南（现有 `docs/` 是架构/分析文档）
- **落点**：`docs/user-guide/`（新建）
- **设计**：配置（`config.example.yaml` 详解）、TUI 命令/键位、技能系统、autoVCS、goalloop、guard 权限模型、headless/SDK/IDE 入口；getting started（`--fake-model` 零依赖起步）；从 CLAUDE.md 提炼对外可见部分；命令表/配置骨架自动生成片段（新 `cmd/gendocs`，保 `cmd/api-schema` 职责单一）；CI `git diff --exit-code` 守门防漂移
- **依赖**：[G]（API 文档含 image 字段）
- **风险**：漂移→自动生成 + 守门
- **验收**：覆盖主要用法；getting started 可零依赖跑通；与实际不漂移
- **预估**：2-3d

### [APIREF1] v1 API/协议参考  (P2 | 部分 | D1/D2 产出)

- **缺口**：D1 落地 v1 资源模型 + JSON Schema，D2 落地 SDK，但无统一对外 API 参考
- **落点**：`docs/api/`（新建）
- **设计**：v1 thread/turn/item 资源、SSE 事件（`turn`/`item`）、JSON Schema、TS/Python SDK 用法、app-server JSON-RPC；从 `sdk/schema/` 与 `internal/api/v1/types.go` 生成片段
- **依赖**：[G]（API image 字段）
- **风险**：协议演进→生成驱动，不手写
- **验收**：v1 API 有参考；SDK 用法有示例；与 schema 一致
- **预估**：2d

### [ADR1] 架构决策记录  (P2 | 部分)

- **缺口**：关键决策散落 CLAUDE.md / synthesis §9，无独立 ADR 可检索
- **落点**：`docs/adr/`（新建）
- **设计**：从 synthesis §9（编排器 `UnknownToolsHandler`/guard fail-closed/压缩 User 角色/WS vs SSE/autoVCS）提炼为 ADR；模板（status/context/decision/consequences）；新决策走 ADR
- **依赖**：-
- **风险**：与 CLAUDE.md 重复→ADR 引用不复制
- **验收**：关键决策有 ADR；模板存在；可检索
- **预估**：1-2d

### [EX1] examples 目录  (P2 | 缺失)

- **缺口**：无可运行示例（headless/SDK/IDE/自定义工具/技能）
- **落点**：`examples/`（新建）
- **设计**：headless 脚本、TS/Python SDK 调用、自定义 `GuardedTool`、自定义技能、goalloop 配置；每个最小可跑（`--fake-model` 友好）；CI 跑 examples 验证可编译（Go build + TS tsc + Python 解析 + headless 冒烟）
- **依赖**：-
- **风险**：漂移→CI 跑 examples 验证可编译
- **验收**：≥5 个示例；可跑；覆盖主要集成点
- **预估**：2d

### [CONTRIB1] 贡献指南 + docs 归档  (P2 | 缺失)

- **缺口**：无 `CONTRIBUTING`；多份 synthesis/analysis 报告未归档
- **落点**：`CONTRIBUTING.md` + `docs/archive/`
- **设计**：贡献指南（六边形布局、context 注入、guard fail-closed、Fake 优先、1000 行限制、注释密度、单 binary 客户端+服务端）；现有 synthesis/analysis 报告移 `docs/archive/`；保留 CLAUDE.md 为权威
- **依赖**：[VER1]（提交规范对齐 CHANGELOG）
- **风险**：指南与实际漂移→引用 CLAUDE.md
- **验收**：`CONTRIBUTING` 存在；约定可执行；docs 结构清晰
- **预估**：1d

---

---

## 附录

### 附录 A：与 synthesis 行动/风险映射

| synthesis 项 | 类型 | E-H 归属 | 状态 |
|---|---|---|---|
| A1 SpentTokens 死代码 | P0 行动 | — | ✅ B0 已完成 |
| A6 ws.go 拆分 | P1 行动 | [GOV2] | ✅ 已转为三文件拆分（E3 spec agent 重新实测 1385 纯代码行） |
| A7 tools/agent.go 拆分 | P1 行动 | [GOV2] | 待（与 A6/A17 同批三文件拆分） |
| A8 双压缩协调 | P1 行动 | [CCL1] | 待 |
| A10 子代理并发上限 | P1 行动 | [LEAK2] | 待 |
| A11 proto 测试 | P1 行动 | [COV2] | 待 |
| A12 slog 结构化日志 | P2 行动 | — | ✅ C4 已完成 |
| A14 store 测试 | P2 行动 | [COV1] | 待 |
| A15 SQLite WAL | P2 行动 | [WAL1] | 待 |
| A16 ctxcompact 属性测试 | P2 行动 | [PROP1] | 待 |
| A17 model.go 拆分 | P2 行动 | [GOV2] | 待（与 A6/A17 同批三文件拆分，Update ~575 行 switch extract-method + golden） |
| A18 bootstrap 集成测试 | P2 行动 | [COV3] | 待 |
| A19 createdWT 泄漏 | P2 行动 | [LEAK1] | 待 |
| A23 架构治理测试 | P3 行动 | [GOV1][CIG1] | 待 |
| A24 guard fuzz | P3 行动 | [FUZ1] | 待 |
| A25 性能基准 | P3 行动 | [BENCH1] | 待 |
| A26 未文档化导出 | P3 行动 | [GOV3] | 待 |
| R2 ws.go 竞态 | 高风险 | [RAC1] | 待 |
| R3 双压缩不协调 | 高风险 | [CCL1] | 待 |
| R5 store 覆盖弱 | 高风险 | [COV1] | 待 |
| R10 SQLite 并发 | 中风险 | [WAL1] | 待 |
| R13 createdWT 泄漏 | 低风险 | [LEAK1] | 待 |
| R16 guard 无 fuzz | 低风险 | [FUZ1] | 待 |

> synthesis 27 项行动中，约 5 项已由 B0/C4 关闭，剩余多数被 E-H 覆盖；少量纯重构项（A20 config 抽离、A21 fork 同步、A22 MCP 独立包、A27 worker 抽离）为 P3 长期，不阻塞 v1.0，暂不纳入 E-H。

### 附录 B：依赖图

```
E1(COV1/2/3) ── 独立，可并行
E2(FUZ1/PROP1/RAC1) ── 独立；RAC1 会暴露 → 喂给 F2(LEAK)
E3(GOV1/GOV2/GOV3) ── 独立，建议最先（保护后续）
F1(WAL1) ── 独立（store 并发基础）
F2(LEAK1 ← RAC1, LEAK2, CCL1, BENCH1, LEAK3) ── 依赖 E2 的 race 发现
G(VISION, VISION-TOOL) ── 独立，可并行
H1(VER1, CIG1 ← GOV1/GOV2/RAC1/FUZ1/PROP1, PKG1 ← VER1, UPG1) ── 依赖 E2/E3 产出入 CI
H2(UDOC1 ← G, APIREF1 ← G/D1/D2, ADR1, EX1, CONTRIB1 ← VER1) ── 依赖 G 与 D1/D2
```

关键链：**E3 → CIG1**（治理测试必须先于 CI 门禁存在）；**E2-RAC1 → F2-LEAK**（race 发现驱动泄漏修复）；**VER1 → PKG1**（打包依赖版本号）。

### 附录 C：每批工作量汇总

| 批 | 工作量 | 关键产出 |
|---|---|---|
| E1 | ~1w | store/proto/bootstrap 覆盖达标 |
| E2 | ~1w | guard fuzz + ctxcompact 属性 + race CI |
| E3 | ~0.5-1w | 架构治理门禁（分层/行数/文档+三文件拆分） |
| F1 | ~0.5w | SQLite WAL + 连接池 |
| F2 | ~1w | 泄漏清理 + 上限核对 + 压缩冷却 + 基准 |
| G | ~1.5-2w | 多模态理解（VISION + VISION-TOOL） |
| H1 | ~1.5w | 版本策略 + CI 门禁矩阵 + 多平台打包 + 升级/doctor |
| H2 | ~1.5w | 用户指南 + API 参考 + ADR + examples + CONTRIBUTING |
| **合计** | **~8-10w 串行 / ~6-7w 并行** | **可发布 v1.0** |

---

> **核心结论**：A-D 让 yanshi 功能对齐了 codex/deepseek；E-H 在加固（测试/正确性/性能/架构治理）基础上新增多模态图像理解（Tier G），并建立 CI 治理与发布流程，补对外文档。8 个 batch、~8-10 周串行，可直接按 A-D 管线（brainstorm → spec → writing-plans → 3×PASS 评审 → subagent 执行）逐批施工。建议从 **E3（治理测试）** 起步，因为它保护后续所有改动；G（多模态）与 E/F 路径不冲突，可并行。
>
> 本计划为每项提供了设计骨架（落点/设计/依赖/风险/验收/预估），任一批次可直接进入 `writing-plans` 产出可施工任务。
