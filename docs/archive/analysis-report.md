# Yanshi v0.3.0 — 综合分析报告

> **生成日期**：2026-07-18
> **范围**：项目结构 · 架构模式 · 依赖关系 · 代码质量
> **方法**：静态源码分析 + 测试覆盖分析 + 依赖图逆向工程

---

## 目录

1. [执行摘要](#1-执行摘要)
2. [项目概览](#2-项目概览)
3. [项目结构](#3-项目结构)
4. [架构分析](#4-架构分析)
5. [依赖关系分析](#5-依赖关系分析)
6. [代码质量分析](#6-代码质量分析)
7. [交叉关联分析](#7-交叉关联分析)
8. [改进建议（按优先级排序）](#8-改进建议按优先级排序)
9. [结论](#9-结论)

---

## 1. 执行摘要

| 维度 | 评分 | 关键亮点 |
|---|---|---|
| **架构风格** | ★★★★★ | 六边形架构严格执行，依赖向内流动 |
| **设计模式** | ★★★★☆ | 组合根 + 上下文注入 + Guard + ReAct + PDCA 模式运用恰当 |
| **模块化** | ★★★★☆ | 18 个内部包，职责边界清晰；`cli/tui` 子包划分可优化 |
| **依赖管理** | ★★★★★ | 纯 DAG，零循环依赖，`guard` 是理想的基础包（最高扇入、零内部依赖） |
| **安全设计** | ★★★★★ | Guard fail-closed + 四维权限 + 元字符拦截 + 多层防御 |
| **可测试性** | ★★★★★ | Fake 优先策略，零外部依赖可运行；平均覆盖率 ~80% |
| **代码风格** | ★★★★☆ | go vet 零警告；少量文件超长、重复辅助函数 |
| **正确性** | ★★★★☆ | 1 处死代码（`SpentTokens` 永不更新），2 个低风险问题 |
| **可维护性** | ★★★★☆ | 架构为可维护性打下坚实基础；工具包 20+ 文件需适度拆分 |

**总体评级：★★★★☆（优秀）**

这是一个经过精心架构设计的项目。六边形架构严格执行，设计模式使用恰当，关注点分离清晰。少数改进点集中在工具包子包划分、去除死代码、以及抽取公共辅助函数。

---

## 2. 项目概览

### 2.1 基本信息

| 属性 | 值 |
|---|---|
| **项目** | `yanshi`（`github.com/x6nux/yanshi`） |
| **版本** | v0.3.0 |
| **语言** | Go 1.26.4 |
| **架构风格** | 六边形架构（Ports & Adapters） |
| **核心运行时** | Eino ADK 编排器（`cloudwego/eino` v0.9.12） |
| **CLI 框架** | Bubble Tea TUI（`charmbracelet/bubbletea` v1.3.10，本地 fork） |
| **存储** | 嵌入式 SQLite（`modernc.org/sqlite` v1.53.0，无 CGO） |
| **构建产物** | 单个二进制 `yanshi`，同时作为客户端（TUI）与服务端 |

### 2.2 核心能力

- **LLM Agent 编排**：基于 Eino ADK 的 ReAct 循环，支持工具调用、子代理委派、回合中压缩
- **安全守卫**：四维权限检查（tools / fs / shell / net），fail-closed，交互式权限弹窗
- **自动版本控制**：基于 SQLite 的类 git VCS，自动追踪 agent 编辑（`fs_edit`/`fs_write`）
- **自驱动目标循环**：Plan → Implement → Evaluate → Judge 四阶段闭环（PDCA）
- **技能系统**：基于 SKILL.md 的标准技能包（T0-T4 分层）
- **双传输协议**：WebSocket（主）+ SSE（备），共享 JSON 帧词表
- **后端发现**：项目级 lockfile + PID 存活检测，支持多窗口自愈

### 2.3 子命令

| 命令 | 功能 |
|---|---|
| `yanshi`（无参数） | 自包含 TUI：为当前项目发现后端或嵌入启动一个 |
| `yanshi chat` | 同上；`--no-tui` 退化为 SSE 行式 REPL |
| `yanshi serve` | 启动共享守护进程（HTTP） |
| `yanshi goal` | 自驱动目标循环 |
| `yanshi vcs-mcp` | autoVCS MCP 服务器（被 ACP 适配器拉起） |
| `yanshi -h` | 帮助（退出 0） |

---

## 3. 项目结构

### 3.1 顶层目录

```
yanshi/
├── cmd/                    # 二进制入口（2 个 main 包）
│   ├── yanshi/main.go    # 主 CLI — TUI / chat / serve / goal / vcs-mcp
│   └── agent-worker/main.go # 远程 Worker — SSE 连接 Task API
├── internal/               # 核心源码 — 18 个内部包
├── skills/                 # 5 个标准 SKILL.md 开发技能
│   ├── dev-autonomous-project/
│   ├── dev-designed-feature/
│   ├── dev-quick-fix/
│   ├── dev-standard-feature/
│   └── dev-team-feature/
├── docs/                   # 文档（含超能力里程碑规划 M1-M8）
├── third_party/            # 本地 fork — bubbletea（Windows Ctrl+Enter 修复）
├── config.example.yaml     # 配置模板
├── CLAUDE.md               # 项目级 Agent 指令（本文件）
└── go.mod / go.sum         # 模块定义 + 依赖锁
```

### 3.2 内部包拓扑（18 个包，含子包）

```
internal/
├── bootstrap/          # 组合根 — 唯一知晓所有包的包（装配 9+ 内部包）
├── config/             # 配置加载 — YAML + ${VAR} 环境变量替换
├── proto/              # 通信协议 — ClientFrame/ServerFrame 共享词表
├── guard/              # 安全守卫 — 四维 fail-closed 权限检查（被 10 个包依赖）
│
├── agent/              # Agent 系统
│   ├── orchestrator/   #   Eino ADK 编排器（ReAct 循环）
│   ├── goalloop/       #   自驱动目标循环（Plan→Implement→Evaluate→Judge）
│   ├── registry/       #   子代理注册表
│   ├── spawn/          #   并发子代理创建
│   └── worker/         #   远程 Worker 客户端
│
├── tools/              # 工具层 — GuardedTool 框架 + 20+ 工具实现
├── llm/                # LLM 抽象层
│   └── eino/           #   Eino 适配器（FakeModel / OpenAI / Anthropic）
│
├── api/http/           # HTTP API — WebSocket（主）+ SSE（备）
├── cli/                # CLI 客户端
│   └── tui/            #   Bubble Tea TUI
│
├── store/              # SQLite 持久化（KV / 记忆 / 会话 / 任务）
├── vcs/                # 自动版本控制（autoVCS，基于 SQLite）
│   └── mcp/            #   VCS MCP 服务器
├── task/               # 任务 Broker（声明周期：提交→认领→心跳→执行→记录）
├── acp/                # Agent Client Protocol 适配器（JSON-RPC 2.0）
├── ctxcompact/         # 上下文压缩（对话历史 Token 预算管理）
├── skills/             # 技能系统（SKILL.md 加载器）
├── plugin/             # 插件主机（Connector 接口）
├── lockfile/           # 进程锁 + 后端发现（跨平台 PID 存活检测）
└── version/            # 版本号单一数据源（AutoCode = "0.3.0"）
```

### 3.3 结构亮点与问题

**亮点：**
- 严格的分层结构，18 个包各自承担单一职责
- `version` 是版本号的**唯一数据源**，无重复定义
- `guard` 同时是"零内部依赖"和"被最多包依赖"的理想基础包
- `proto` 零内部依赖，所有传输层共享同一套帧词表

**结构问题：**
1. `cli/tui` 作为 `cli` 的子包，导致 `main.go` 必须处理 `cli → tui` 的手动接线（因循环导入无法在包内完成）。将 `tui` 提升为 `internal/tui` 可消除此问题。
2. `vcs/mcp/server.go` 在 VCS 包内引入了 MCP 协议实现，增加了 VCS 包的职责范围。若 MCP 协议演化，建议提取为独立包。
3. `agent/worker` 作为远程 worker（SSE 客户端）的职责与"Agent"核心含义不同，或许应独立为 `internal/worker`。

---

## 4. 架构分析

### 4.1 架构风格：六边形架构（Hexagonal Architecture）

Yanshi 明确采用六边形架构，并得到了**非常严谨的执行**。

```
                       ┌─────────────────────────────────────┐
                       │     入站适配器（客户端侧）            │
                       │ cli/tui · cli/backend               │
                       │ wsbackend · ssebackend              │
                       └──────────┬──────────────────────────┘
                                  │ JSON 帧 (proto)
                                  ▼
┌──────────────────────────────────────────────────────────────┐
│                 API / 传输层（入站端口）                       │
│           api/http · proto 共享词表                           │
└──────────────────────────┬───────────────────────────────────┘
                           │
                           ▼
┌──────────────────────────────────────────────────────────────┐
│                   核心域（Core Domain）                       │
│                                                              │
│  agent/orchestrator   — 编排器（Eino ADK ReAct 循环）         │
│  agent/goalloop       — 目标驱动循环（PDCA）                  │
│  tools                — GuardedTool 工具层                   │
│  guard                — 安全守卫（fail-closed）               │
└──────────────────────────┬───────────────────────────────────┘
                           │
                           ▼
┌──────────────────────────────────────────────────────────────┐
│                  出站适配器（被依赖的端口）                    │
│                                                              │
│  llm/eino   — LLM 抽象 + Eino 适配器                          │
│  store      — SQLite 持久化                                   │
│  vcs        — autoVCS 版本控制                                │
│  acp        — 外部 Agent 协议适配器                           │
└──────────────────────────────────────────────────────────────┘
                           ▲
                           │
                    ┌──────┴──────┐
                    │ 组合根      │
                    │ bootstrap   │
                    └─────────────┘
```

**强度：**
- 依赖方向严格**向内流动**，无循环依赖或反向依赖
- `bootstrap` 是**唯一**知晓所有包的地方 —— 符合六边形架构经典实践
- 每个内部包只通过接口（`model.BaseChatModel`、`tool.InvokableTool`）依赖外部组件
- 入站和出站适配器可以独立替换（例如添加 gRPC 传输层只需新增入站适配器）

### 4.2 核心设计模式分析

#### 4.2.1 组合根模式（Composition Root）

**位置**：`internal/bootstrap/bootstrap.go` — `Build()`

```
装配顺序: config → store → vcs → model → tools → orchestrator → http server → task broker
```

**评估**：✅ 极好。`App` 结构体持有所有组件引用，仅暴露必要字段。非致命失败以"软禁用"方式继续（VCS 初始化失败不阻塞启动），体现**容错启动**实践。

#### 4.2.2 上下文注入模式（Context Injection）

```go
tools.WithProfile(ctx, profile)            // 权限 profile
tools.WithVCS(ctx, vcsScope)               // VCS 追踪范围
tools.WithSubAgentRunner(ctx, runner)      // 子代理委派
tools.WithPermissionCallback(ctx, cb)      // 交互式权限回调
```

**评估**：✅ 深思熟虑的设计。工具函数**不应该**把鉴权/追踪作为参数暴露给 LLM——那是横切关注点。单一权限入口 `tools.Authorize()` 是唯一的授权咨询点，五步解析顺序（profile→allowlist→guard→callback→deny）只实现一次。

**关键风险**：上下文值在 Go 中是隐式的，容易被忽略。当前通过详尽的 doc 注释缓解此问题。

#### 4.2.3 守卫模式（Guard / Fail-Closed）

```go
Guard.Check(profile, action) → Decision
// 四维检查：tools（glob 白名单）→ fs（读/写路径）→ shell（策略+元字符拦截）→ net（host 白名单）
// 空的 Tools.Allow 一律拒绝（fail-closed）
// shell_run 额外拒绝元字符（&&、||、;、`、$()、<>、|）
```

**评估**：✅ 安全关键组件做到了 fail-closed。交互式权限的回退机制（SSE 无回调→静态 deny）确保安全策略不会因传输方式改变而被绕过。`shell_run` 对元字符的独立检查是深思熟虑的防御深度（defense-in-depth）。

#### 4.2.4 ReAct 编排器 + 幻觉容忍

```go
// UnknownToolsHandler：把幻觉工具名作为工具结果（而非错误）回喂给 LLM
// Mid-turn Compaction：通过 CompactingModel 包装器在迭代之间压缩历史
// 按 turn 切换 model：基于 BaseChatModel 指针缓存在 runners sync.Map 中
```

**评估**：✅ 
- `UnknownToolsHandler` 是处理 LLM 幻觉的**正确做法** —— `NodeRunError` 会终止整个 turn，而把幻觉转化为正常结果则给模型自我修正的机会
- Mid-turn compaction 解决了长 Agent 会话中的上下文窗口溢出问题，而不需要等到用户发起新的 turn
- `sync.Map` 缓存 runner 支持 /model 运行时切换，无需重建编排器

#### 4.2.5 自驱动目标循环（PDCA / Plan-Do-Check-Act）

```
Plan ──→ Implement ──→ Evaluate ──→ Judge
  ↑                                      │
  └──────────────────────────────────────┘
        (未完成 → 重新规划)
```

**评估**：PDCA 循环在 LLM Agent 系统中的自然映射。分层技能（T0-T4）细化了规划策略的选择。`--fake-model` 接入 `FakePlanner` + `FakeImplementer` + `counterEvaluator`，使测试可在零外部依赖下验证循环逻辑。

#### 4.2.6 通信协议帧模式（Protocol Framing）

```
ClientFrame / ServerFrame — 共享词表，两种传输（WS + SSE）
帧类型：user_message · cancel · set_model · set_thinking · set_mode · permission_response · ...
```

**评估**：✅ 精心设计。WS（主）和 SSE（备）传输**相同**的帧词表。传输差异封装在 `wsbackend.go` / `ssebackend.go` 中，上层代码（TUI）无需感知。

#### 4.2.7 自动版本控制（Event Sourcing 风格）

```
每次 fs_edit/fs_write → 自动记录到 VCS（通过注入的 context scope）
main（主干）←→ worktree（分支，从 main_head 分出）
树级三方合并 → MergeToMain
```

**评估**：隐式的 event sourcing——Agent 的每次编辑操作都被自动记录为 VCS 提交，无需 Agent 配合。通过 context 注入 VCS scope 实现，工具函数无感。

### 4.3 架构反模式检查

| 模式 | 状态 | 说明 |
|---|---|---|
| 循环依赖 | ✅ 无 | DAG 结构完整 |
| 上帝包 | ✅ 无 | `bootstrap` 知晓一切但仅装配不含业务逻辑 |
| 过度抽象 | ✅ 无 | 每个接口有明确用途和有限实现 |
| 内部泄露 | ✅ 无 | 包 API 边界清晰 |
| 配置硬编码 | ✅ 无 | 所有运行时参数来自 `config.yaml` |

---

## 5. 依赖关系分析

### 5.1 内部依赖 DAG

```
Application Layer (bootstrap, api/http)
    ↓
Coordination Layer (agent/orchestrator, agent/goalloop, cli)
    ↓
Tool/Infra Layer (tools, task, vcs, acp)
    ↓
Foundation Layer (guard, store, proto, skills, config, llm/eino)
```

### 5.2 扇入统计（被依赖次数）

| 包 | 扇入 | 被谁依赖 |
|---|---|---|
| **guard** | **10** | 几乎所有需要权限检查的包 — 最核心的基础设施 |
| **store** | **7** | vcs, tools, task, api/http, bootstrap, agent/worker, cmd |
| **proto** | **5** | orchestrator, api/http, cli, cli/tui, ctxcompact |
| **vcs** | **5** | goalloop, bootstrap, task, tools, vcs/mcp |
| **tools** | **3** | orchestrator, api/http, bootstrap |

**关键发现**：`guard` 同时是"零内部依赖"（扇出=0）和"最高扇入"（10）的包。这是**理想的架构状态**——被所有人依赖的基础设施应当不依赖任何人。

### 5.3 扇出统计（依赖多少个内部包）

| 包 | 扇出 | 内部依赖 |
|---|---|---|
| **bootstrap** | **10** | 组合根，装配所有组件 — 设计使然 |
| **api/http** | **9** | HTTP 传输层，需协调所有核心域组件 |
| **agent/orchestrator** | **4** | guard, llm/eino, tools, proto |
| **tools** | **4** | guard, store, skills, vcs |
| **agent/goalloop** | **3** | acp, guard, vcs |

`bootstrap` 和 `api/http` 的高扇出是其架构角色的自然结果。`orchestrator` 和 `tools` 的中等扇出反映了它们作为核心域的协调职责。

### 5.4 循环依赖分析

**结论：不存在循环依赖。** 项目采用三种模式成功避免循环引用：

| 模式 | 示例 |
|---|---|
| **Context 注入** | `tools/agent.go` 通过 context 读取 `SubAgentRunner`，而非导入 `orchestrator` |
| **接口定义在下游** | `tui` 导入 `cli`，`cli` 不导入 `tui`；组装在 `main.go` 中完成 |
| **纯数据包** | `version`、`proto`、`guard`、`plugin`、`lockfile` — 零内部依赖 |

### 5.5 耦合健康度

| 包 | 耦合评估 | 理由 |
|---|---|---|
| `guard` | 🟢 理想 | 零内部依赖，最高扇入 |
| `store` | 🟢 理想 | 零内部依赖，次高扇入 |
| `tools` | 🟡 适度关注 | 20+ 文件，4 个内部依赖，建议拆分子包 |
| `agent/goalloop/implementer.go` | 🟡 可接受 | 导入 acp/guard/vcs，但这是拉起外部 agent 的业务本质 |
| `config` | 🟡 值得关注 | 依赖 `guard` 只因内嵌 `PermissionProfile`——可抽到独立 types 包 |
| `api/http` | 🟢 合理 | 9 个内部依赖是其作为传输层的自然职责 |
| `bootstrap` | 🟢 合理 | 10 个内部依赖是组合根的设计意图 |

### 5.6 外部依赖清单

| 依赖 | 用途 | 评价 |
|---|---|---|
| `cloudwego/eino` v0.9.12 | ADK 编排器 | 核心依赖，版本锁定 |
| `charmbracelet/bubbletea` v1.3.10 | TUI 框架 | 本地 fork（Windows Ctrl+Enter 修复），需跟踪上游 |
| `modernc.org/sqlite` v1.53.0 | 嵌入式 SQLite | 无 CGO，跨平台好 |
| `gorilla/websocket` v1.5.3 | WebSocket | 成熟稳定 |
| `gopkg.in/yaml.v3` | YAML 解析 | 标准选择 |
| `wk8/go-ordered-map/v2` | 有序 map | 测试辅助 |
| `eino-contrib/jsonschema` | JSON Schema | 工具参数校验 |

外部依赖少且集中（7 个直接依赖），管理良好。

---

## 6. 代码质量分析

### 6.1 总览

| 维度 | 评分 (1-5) | 关键发现 |
|---|---|---|
| 编码规范 | 4 | go vet 零警告；少量文件超长、缩进不一致 |
| 正确性 | 4 | 1 处死代码（`SpentTokens` 永不更新）、2 个低风险问题 |
| 安全性 | 5 | Guard fail-closed + 多层验证 + 元字符拦截 |
| 可维护性 | 4 | 架构佳，但重复辅助函数和超长文件需关注 |
| 测试覆盖 | 4 | 整体 ~80%，`store`/`proto` 需补充 |
| 文档 | 5 | 包注释、导出符号注释质量高 |
| **总分** | **4.3** | 高质量项目，v0.4.0 可改进局部问题 |

### 6.2 编码规范

#### ✅ 好的实践
- 统一的 `go vet` 零警告
- 良好的 doc 注释密度（包注释、导出函数注释）
- 一致的文件命名和包组织
- `context` 优先而非全局状态

#### ⚠️ 需要改进

**问题 1：`internal/api/http/ws.go` 文件超长（~1090 行）**
违反了 CLAUDE.md 中"单文件不超过 1000 行（纯代码行）"的约定。包含 WebSocket 主逻辑 + 权限追踪器 + 会话管理 + 上下文压缩 + 重试逻辑。建议将 `permTracker`、`connSession` 等抽离到单独文件。

**问题 2：重复的辅助函数**

| 函数 | 位置 1 | 位置 2 |
|---|---|---|
| `firstNonEmpty` | `bootstrap.go:324` | `goalloop/implementer.go:27` |
| `contains` | `orchestrator.go:411` | 可在其他包复用 |

建议提取到公共包（如 `internal/util` 或 `internal/helpers`）。

**问题 3：上下文注入重复代码（`orchestrator.go`）**
`Query`、`Events`、`EventsWithHistory`、`EventsWithHistoryOpts` 四个方法的前 4-7 行完全相同（profile + subagent runner + VCS 注入）。建议提取私有方法 `injectTurnContext()`。

### 6.3 潜在 Bug

#### ⚠️ 1. `SpentTokens` 永不更新（死代码）

**位置**：`internal/agent/goalloop/loop.go:64-66`、`types.go:44`

```go
if l.cfg.Budget.MaxTokens > 0 && l.cfg.Budget.SpentTokens > l.cfg.Budget.MaxTokens {
    return Decision{Complete: false, Summary: "budget exceeded"}, nil
}
```

**问题**：`Budget.SpentTokens` 在整个循环中从未被递增。Token 预算检查要么**永远不会触发**（当 `SpentTokens` 保持 0），要么**总是触发**（如果 `MaxTokens == 0`）。测试覆盖率 78.3% 也没有暴露它，说明测试未模拟 budget 耗尽路径。

**影响**：中。用户若依赖 token 预算控制，此功能不生效。

**建议**：要么（a）在每次 LLM 调用后更新 `SpentTokens`，要么（b）移除此死代码并注明未来实现计划。

#### ⚠️ 2. `compactNow` 强制压缩的 hack 用法

**位置**：`internal/api/http/ws.go:1020-1024`

```go
const forceThreshold = 1.0
const forceWindow = 1
```

**问题**：手动 `/compact` 通过 `threshold=1.0`、`window=1` 绕过阈值检查。依赖于 `MaybeCompact` 的内部实现细节，若未来 `estimateTokens` 逻辑改变，此 hack 可能静默失效。

### 6.4 安全分析

#### ✅ 最佳实践
- **Guard fail-closed**：无 profile 时一律拒绝
- **Shell 元字符拦截**：guard + shell 工具双重保护
- **路径越狱防护**：`filepath.Clean` + `withinRoot` 双重校验
- **Web 重定向安全**：每次重定向重新检查权限
- **交互式权限回退**：SSE 无回调时静态 deny，不被绕过

#### ⚠️ 低风险

**SQL 注入（准注入——当前安全）**

`internal/store/store.go:86-88` 使用 `fmt.Sprintf` 拼接 ALTER TABLE。但 `table`/`col`/`decl` 均来自硬编码常量，非用户输入，安全。建议添加注释说明此函数的安全前提条件。

### 6.5 测试覆盖分析

| 包 | 覆盖率 | 评价 |
|---|---|---|
| `plugin` | **100%** | 🟢 完美 |
| `ctxcompact` | **97.0%** | 🟢 优秀 |
| `llm` | **95.1%** | 🟢 优秀 |
| `config` | **93.3%** | 🟢 优秀 |
| `skills` | **89.4%** | 🟢 优秀 |
| `guard` | **88.8%** | 🟢 优秀 |
| `agent/orchestrator` | **85.9%** | 🟢 良好 |
| `tools` | **84.0%** | 🟢 良好 |
| `vcs` | **84.0%** | 🟢 良好 |
| `bootstrap` | **82.1%** | 🟢 良好 |
| `task` | **81.8%** | 🟢 良好 |
| `agent/goalloop` | **78.3%** | 🟡 含死代码未被测试捕获 |
| `cli/tui` | **73.4%** | 🟡 良好 |
| `api/http` | **73.4%** | 🟡 良好 |
| `acp` | **73.0%** | 🟡 良好 |
| `proto` | **66.7%** | 🟡 需补充帧序列化/反序列化测试 |
| `store` | **57.5%** | 🔴 需补充（存储层是持久化核心）|

**测试薄弱环节**：
1. **`store` (57.5%)** — SQLite 存储层是持久化核心，但覆盖率最低。建议补充会话 CRUD、任务生命周期、VCS 写入测试。
2. **`proto` (66.7%)** — 帧序列化/反序列化。`SSEEvent()` 等新 API 缺少直接测试。
3. **缺少基准测试** — `vcs` 的回溯重建、DAG 拓扑排序、`fs_edit` 模糊匹配在大型输入下的性能未覆盖。

---

## 7. 交叉关联分析

### 7.1 架构模式 → 代码质量

| 架构模式 | 对代码质量的影响 |
|---|---|
| **六边形架构 + 组合根** | enable 了高可测试性——`bootstrap` 统一装配，各组件可独立替换为 fake |
| **上下文注入** | 降低了工具函数的参数复杂度，但也带来了隐式依赖的认知负担（Go context 的经典 tradeoff） |
| **Guard fail-closed** | 安全质量得到了架构级保证，而非依赖开发者的纪律性 |
| **Fake 优先** | 直接贡献了高测试覆盖率（平均 80%），使 CI 无需 API key |
| **协议帧共享词表** | 减少了双传输（WS+SSE）的重复代码，但要求新增帧类型时同步更新两种 handler |

### 7.2 依赖结构 → 可维护性

- **`guard` 零内部依赖 + 最高扇入**：这是架构健康度的最强信号。修改 `guard` 不会级联影响其他包，但需要谨慎因为 10 个包依赖它。
- **`bootstrap` 和 `api/http` 的高扇出**：这是六边形架构的自然结果，但意味着修改这两个包时需要关注 9-10 个下游包的配合。当前通过明确的装配顺序和接口契约来管控。
- **`config` 对 `guard` 的依赖**：因为 `PermissionProfile` 嵌入在 `Config` 结构体中。如果未来抽离为公共 types 包，`config` 可降为纯底层包，进一步提升依赖图整洁度。

### 7.3 设计决策 → 潜在 Bug

- **`UnknownToolsHandler` 幻觉容忍**：这个精心设计的模式正确处理了 LLM 幻觉，但如果未来修改为返回 `NodeRunError`（更直观但错误的方式），会破坏整个 turn 级别的重试能力。需要在代码审查中特别关注。
- **`sync.Map` 以接口值为 key**：如果同一 provider 创建了多个 model 实例，接口值比较语义（指针 + 动态类型）可能导致 runner 缓存未命中。建议在 `bootstrap` 中确保 model 实例的复用。
- **`SpentTokens` 死代码**：这是"预留设计"与"实际实现"之间的隙缝——架构上预留了 budget 控制，但实现未完成。这种 gap 如果不符合文档化策略，会误导用户和维护者。

### 7.4 安全 = 架构级承诺

Yanshi 最值得称道的质量属性是**安全性**。这不是偶然的——它是多个架构模式叠加的结果：

```
Guard fail-closed（核心策略）
  + 四维权限检查（覆盖面）
  + 交互式权限弹窗（用户参与）
  + SSE 回退机制（传输无关）
  + 路径双重校验（防御深度）
  + 元字符拦截（shell 特化防御）
  = 安全攻不破的第一道防线
```

没有这些架构级别的设计，安全将完全依赖每个工具实现者的个人纪律。

### 7.5 TUI 的绑定耦合

`cli/tui` 作为 `cli` 的子包是一个特殊的耦合问题：

```
cmd/yanshi/main.go
  ├── cli.NewSession()           // 需要 bootstarp、lockfile、proto
  └── tui.NewProgram()           // 需要 cli.StreamEvent, cli.Session
  
  问题：tui 导入 cli，cli 不能导入 tui（否则循环）
  所以：main.go 必须手动串联两者
  解法：tui → internal/tui（消除子包限制）
```

这是当前架构中最明显的"代码异味"——不是 bug，但每次修改 `cli` 或 `tui` 的接口时都需要确保 `main.go` 的接线正确。

---

## 8. 改进建议（按优先级排序）

### P0 — 应立即修复

| # | 建议 | 关联分析 | 工作量 |
|---|---|---|---|
| 1 | **移除或修复 `SpentTokens` 死代码** | 正确性 — budget 控制功能不生效 | 小（移除）或中（实现 token 计数） |
| 2 | **抽取重复辅助函数**（`firstNonEmpty`、`contains`）到公共包 | 可维护性 — 消除重复 | 小 |

### P1 — 应尽快处理

| # | 建议 | 关联分析 | 工作量 |
|---|---|---|---|
| 3 | **拆分 `internal/api/http/ws.go`**（~1090 行） | 编码规范 — 超文件边界 | 中（抽 2-3 个文件） |
| 4 | **拆分 `internal/tools/agent.go`**（~850 行） | 编码规范 — 职责混合（Agent 工具 + DAG 引擎 + 模板插值 + 拓扑排序） | 中（4-5 个文件） |
| 5 | **提取 `injectTurnContext()` 私有方法**（`orchestrator.go` 4 处重复） | 编码规范 — DRY | 小 |
| 6 | **将 `cli/tui` 提升为 `internal/tui`** | 架构 — 消除 `main.go` 的手动接线 | 中（移动 + 更新导入路径） |

### P2 — 中优先级

| # | 建议 | 关联分析 | 工作量 |
|---|---|---|---|
| 7 | **为 `compactNow` hack 添加正式 API** | 正确性 — 消除对内部实现的依赖 | 小 |
| 8 | **将 `PermissionProfile` 抽到独立 types 包** | 依赖管理 — 消除 `config` 对 `guard` 的依赖 | 中 |
| 9 | **补充 `store` 测试（57.5%）** | 质量 — 存储层是持久化核心 | 中 |
| 10 | **补充 `proto` 测试（66.7%）** | 质量 — 帧序列化准确性 | 小 |
| 11 | **在 `RequeueStale` 路径中清理 `createdWT` map 泄漏** | 正确性 — 防止任务 broker 中的 map 泄漏 | 小 |

### P3 — 长期考虑

| # | 建议 | 关联分析 | 工作量 |
|---|---|---|---|
| 12 | **MCP 适配器提取为独立包** | 架构 — 保持 VCS 包单一职责 | 大 |
| 13 | **`agent/worker` 提取为独立 `internal/worker`** | 架构 — 分离远程 worker 职责 | 大 |
| 14 | **添加 vcs / DAG / fs_edit 基准测试** | 质量 — 性能基线 | 中 |
| 15 | **配置热加载支持**（`fsnotify` + 原子替换） | 功能 — 运行时配置变更 | 大 |

---

## 9. 结论

### 9.1 项目画像

```
Yanshi v0.3.0
├── 架构严格的六边形 Agent 系统
├── 18 个内部包，依赖 DAG 零循环
├── 安全：fail-closed 守卫 + 四维权限 + 多层防御
├── 测试：Fake 优先策略，平均 ~80% 覆盖率
├── 主要结构问题：cli/tui 子包导致 main.go 手动接线
├── 主要正确性问题：SpentTokens 死代码
└── 主要可维护性问题：ws.go 超长、重复辅助函数、tools 包 20+ 文件
```

### 9.2 综合评级

| 维度 | 项目结构 | 架构模式 | 依赖管理 | 代码质量 |
|---|---|---|---|---|
| **评分** | ★★★★☆ | ★★★★★ | ★★★★★ | ★★★★☆ |
| **最强项** | 职责边界清晰 | 六边形架构严格执行 | 零循环依赖 | 安全设计 + Fake 测试 |
| **待改进** | cli/tui 子包划分 | — | config→guard 耦合 | 死代码 + 超长文件 |

### 9.3 一句话总结

> **Yanshi v0.3.0 是一个架构设计优秀的 LLM Agent 系统——六边形架构严格执行、安全设计为架构级承诺、依赖管理零循环。** 代码质量整体优秀（4.3/5），主要改进空间集中在代码规范层面的局部优化（超长文件拆分、重复辅助函数抽取、死代码移除）和少数包的测试覆盖补充。改进点多为增量优化，不涉及架构重构。
