# Yanshi — 项目综合分析报告（合成版）

> **生成日期**：2026-07-20  
> **方法**：全文件清册 + 静态源码分析 + 依赖图逆向 + 测试覆盖数据 + 内存分析记录 + 历史变更追踪  
> **范围**：289 个 `.go` 源文件（含测试），29 个 Go 包，41 个 `.md` 文档，~62K Go 源码行  
> **模块**：`github.com/x6nux/yanshi` · Go 1.26.4 · Bubble Tea (forked v1.3.10)

---

## 目录

1. [项目画像](#1-项目画像)
2. [架构全景与设计哲学](#2-架构全景与设计哲学)
3. [子系统深度分析](#3-子系统深度分析)
4. [交叉关注点分析](#4-交叉关注点分析)
5. [代码质量与技术债](#5-代码质量与技术债)
6. [风险登记簿](#6-风险登记簿)
7. [安全审计摘要](#7-安全审计摘要)
8. [测试基础设施与覆盖](#8-测试基础设施与覆盖)
9. [可操作改进建议（按优先级排序）](#9-可操作改进建议按优先级排序)
10. [未来方向](#10-未来方向)

---

## 1. 项目画像

### 1.1 一句话总结

> **Yanshi 是一个架构严格的 Go LLM Agent 服务端**——以 Eino ADK 编排器为核心，六边形架构，fail-closed 安全守卫，自动版本控制 (autoVCS)，自驱动目标循环 (PDCA)，双传输协议 (WS+SSE)，Bubble Tea TUI。**单个二进制同时作为客户端与服务端。**

### 1.2 核心能力矩阵

| 能力 | 实现层 | 成熟度 | 说明 |
|---|---|---|---|
| **LLM Agent 编排 (ReAct)** | `agent/orchestrator` — ADK | ★★★★★ | 幻觉容忍、按 turn 切换 model、子代理委派 |
| **安全守卫 (4D fail-closed)** | `guard` — tools/fs/shell/net | ★★★★★ | 架构级承诺，零内部依赖，扇入 10 |
| **自动版本控制 (autoVCS)** | `vcs` — SQLite 类 git | ★★★★☆ | 编辑自动追踪，worktree 合并，MCP 暴露 |
| **自驱动目标循环 (PDCA)** | `agent/goalloop` | ★★★★☆ | plan→implement→evaluate→judge，T0-T4 分层 |
| **双传输协议** | `api/http` + `proto` | ★★★★★ | WS 主/SSE 备，共享帧词表，server-held 历史 |
| **上下文压缩 (mid+pre turn)** | `ctxcompact` + `llm/eino` | ★★★★☆ | 双路径设计精良，但存在压缩过度触发风险 |
| **技能系统 (SKILL.md)** | `skills` + `skills/*/SKILL.md` | ★★★★☆ | T0-T4 分层技能，插件发现 |
| **外部 Agent 协议 (ACP)** | `acp` — codex/claudecode | ★★★★☆ | JSON-RPC 2.0，VCS MCP 传递，策略交付 |
| **TUI** | `cli/tui` — Bubble Tea (forked) | ★★★★☆ | Powerline footer，流式子代理可见，交互式权限 |
| **任务 Broker** | `task` | ★★★☆☆ | 提交/认领/心跳/逾期重入，基础功能完备 |
| **后端发现** | `cli` + `lockfile` | ★★★★☆ | 跨平台 lockfile + PID 存活 + healthz 探测 |

### 1.3 规模统计数据

| 维度 | 数值 |
|---|---|
| **Go 源文件总数** | 289（含 123 个测试文件，~43% 测试占比） |
| **内部包数** | 29（19 个子系统 + 子包） |
| **cmd 入口** | 4（yanshi, agent-worker, testchanged, pkganalyze） |
| **Markdown 文档** | 41 个（含 specs/plans/notes/技能定义） |
| **第三方 fork** | 1（bubbletea v1.3.10，3 个文件修改） |
| **外部直接依赖** | 7 个（eino, eino-ext, gorilla/websocket, modernc/sqlite 等） |
| **Go 源码行数** | ~62K（含测试、fork、test_scratch） |
| **生产代码行数** | ~22K（估算，纯生产逻辑） |
| **测试代码行数** | ~20K（估算，含测试辅助） |

### 1.4 顶层结构

```
yanshi/
├── cmd/                    # 4 个可执行程序入口
│   ├── yanshi/             #   主 CLI（TUI 客户端 + 进程内服务端）— 838 行
│   ├── agent-worker/       #   独立 worker 进程入口
│   ├── testchanged/        #   增量测试工具
│   └── pkganalyze/         #   包分析工具
├── internal/               # 29 个 Go 包 — 核心实现
│   ├── acp/                #   Agent Client Protocol — 15 文件
│   ├── agent/              #   agent 子系统（3 子包）
│   │   ├── goalloop/       #     目标驱动循环 — 20 文件
│   │   ├── orchestrator/   #     编排器核心 — 8 文件
│   │   ├── registry/       #     注册表 — 3 文件
│   │   ├── spawn/          #     Agent 生成 — 1 文件
│   │   └── worker/         #     Worker 进程 — 3 文件
│   ├── api/http/           #   HTTP API — 23 文件
│   ├── bootstrap/          #   组合根 — 3 文件
│   ├── cli/                #   CLI 客户端（含 TUI）— 19+21 文件
│   │   └── tui/            #     Bubble Tea TUI — 21 文件
│   ├── config/             #   配置加载 — 2 文件
│   ├── ctxcompact/         #   上下文压缩引擎 — 22 文件
│   ├── guard/              #   安全守卫 — 7 文件
│   ├── instruct/           #   指令系统 — 2 文件
│   ├── llm/ + llm/eino/    #   LLM 抽象 + Eino 适配 — 21 文件
│   ├── lockfile/           #   跨平台 lockfile — 4 文件
│   ├── plugin/             #   插件主机 — 2 文件
│   ├── proto/              #   JSON 帧协议 — 2 文件
│   ├── skills/             #   技能加载 — 3 文件
│   ├── store/              #   SQLite 存储 — 13 文件
│   ├── task/               #   任务 Broker — 2 文件
│   ├── tools/              #   Agent 工具层 — 40 文件
│   ├── vcs/ + vcs/mcp/     #   版本控制 + MCP — 9 文件
│   └── version/            #   版本常量 — 1 文件
├── third_party/bubbletea/  # 本地 fork（27 文件，3 修改）
├── skills/                 # 5 个 SKILL.md 技能定义
├── docs/                   # 设计文档 · 计划 · 笔记 · 规范（41 个 .md）
│   └── superpowers/
│       ├── specs/          #   14 份设计规范
│       ├── plans/          #   12 份开发计划
│       └── notes/          #   2 篇技术笔记
├── reference/              # 外部参考项目（codex, deepseek-tui, CCometixLine）
└── test_scratch/           # 临时测试杂项
```

---

## 2. 架构全景与设计哲学

### 2.1 六边形架构（Ports & Adapters）

```
                       ┌─────────────────────────────────────┐
                       │     入站适配器（客户端侧）            │
                       │ cli/tui · wsbackend · ssebackend    │
                       │ fakebackend · lockfile 发现           │
                       └──────────┬──────────────────────────┘
                                  │ JSON 帧 (proto)
                                  ▼
┌──────────────────────────────────────────────────────────────┐
│                  API / 传输层（入站端口）                      │
│            api/http · proto 共享词表                          │
│            WS（主, server-held history）                      │
│            SSE（备, client re-sends history）                 │
└──────────────────────────┬───────────────────────────────────┘
                           │
                           ▼
┌──────────────────────────────────────────────────────────────┐
│                    核心域（Core Domain）                      │
│                                                              │
│  agent/orchestrator   — 编排器（Eino ADK ReAct 循环）         │
│  agent/goalloop       — 目标驱动循环（PDCA）                  │
│  tools                — GuardedTool 工具层（18+ 工具）        │
│  guard                — 安全守卫（fail-closed）               │
└──────────────────────────┬───────────────────────────────────┘
                           │
                           ▼
┌──────────────────────────────────────────────────────────────┐
│                   出站适配器（被依赖的端口）                    │
│                                                              │
│  llm/eino   — LLM 抽象 + Eino 适配器                          │
│  store      — SQLite 持久化（modernc.org/sqlite 无 CGO）      │
│  vcs        — autoVCS 版本控制（SQLite 类 git）               │
│  acp        — 外部 Agent 协议适配器（JSON-RPC 2.0）           │
└──────────────────────────────────────────────────────────────┘
                           ▲
                           │
                    ┌──────┴──────┐
                    │ 组合根      │
                    │ bootstrap   │
                    └─────────────┘
```

### 2.2 设计原则（从代码中归纳）

| 原则 | 体现 | 证据 |
|---|---|---|
| **依赖向内流动** | 所有包依赖 core domain，bootstrap 是唯一知晓所有包的组合根 | `internal/bootstrap` 是唯一 import 全部内部包的包 |
| **Fail-closed 安全** | Guard 在 Allow 为空时拒绝所有请求 | `guard.Check` 的 `checkTools` 空 Allow 短路 |
| **Fake 优先于 Mock** | 确定性 fake 实现驱动测试，无需 API key 或子进程 | `FakeModel`、`FakePlanner`、`FakeBackend`、`FakeAgent` |
| **Context 注入横切** | 鉴权/追踪/scope 通过 context value 而非参数传递 | `tools.WithProfile`、`tools.WithVCS`、`tools.WithSubAgentRunner` |
| **字段单一归属** | 每个字段仅在一个结构中具有规范来源 | `Config` 在 `config` 包，分包后各处不得重新定义 |
| **非 fatal 软降级** | VCS 初始化失败、插件发现失败时继续启动 | `bootstrap.Build` 打 stderr 后设空字段继续 |

### 2.3 装配顺序（组合根）

```
config → store → vcs → model → tools → orchestrator → http server → task broker
```

这个顺序有明确的设计理由：
- **config** 最先，所有子系统依赖配置
- **store** 其次，VCS 和 session 依赖持久化
- **vcs** 第三，tools 需要 VCS scope
- **model** 第四，编排器依赖模型
- **tools** 第五，编排器需要工具集
- **orchestrator** 第六，API 需要编排器
- **http server** 第七，外部访问入口
- **task broker** 最后，依赖所有上层组件

### 2.4 关键不对称设计决策

| 决策 | 选择 | 理由 |
|---|---|---|
| **WS 服务端持有历史** vs SSE 客户端回放历史 | WS 主/SSE 备 | WS 持久连接适合双向流（取消、控制帧、权限）；SSE 无状态，每次请求完全携带历史 |
| **Compaction 双路径**：mid-turn + pre-turn | 两条独立路径共享统一核心 | Mid-turn 在 ReAct 迭代间触发（CompactingModel），pre-turn 在 user_message 前触发（MaybeCompact），都委托 `ctxcompact.Run` |
| **Guard 无状态** vs 配置驱动 profile | 无状态 + 配置 map | 无状态简化并发和测试，所有状态在 profile 配置中 |
| **VCS 自动追踪**通过 context 注入 | 工具无需感知 VCS | `fs_edit`/`fs_write` 等工具自动被 VCS scope 捕获，零 agent 配合 |

---

## 3. 子系统深度分析

### 3.1 编排器核心（`internal/agent/orchestrator/`）⚙️

**文件**: 8 源文件 · **核心文件**: `orchestrator.go`（671 行）

#### 关键设计决策

| 决策 | 实现 | 理由 |
|---|---|---|
| **幻觉容忍** | `UnknownToolsHandler` 以工具结果（非 NodeRunError）返回 | 允许模型自行修正，不中断 turn |
| **按 turn 切换 model** | `runners sync.Map` 以 model 指针为缓存键 | `/model` 运行时切换 provider，无需重建编排器 |
| **子代理委派** | `bindSubAgentRunner` → 嵌套 orchestrator，深度上限 `MaxSubAgentDepth` | 防止递归失控 |
| **子代理流式可见** | SubAgentEmitFrom + SubAgentProgress | 子代理不再是黑盒，TUI 可见 thinking 过程 |

#### 代码重复热点

`Query`、`Events`、`EventsWithHistory`、`EventsWithHistoryOpts` 四个方法前 4-7 行完全相同：

```go
ctx = tools.WithProfile(ctx, o.profile)
ctx = tools.WithWorkRoot(ctx, o.workRoot)
ctx = o.bindSubAgentRunner(ctx)
if o.vcsScope.VCS != nil {
    ctx = tools.WithVCS(ctx, o.vcsScope)
}
```

**建议**: 提取 `injectTurnContext(ctx) context.Context` 私有方法。

#### 风险

- Sub-agent 嵌套若失控可能导致 Goroutine 爆炸（虽有深度上限，但无总数上限）

---

### 3.2 安全守卫（`internal/guard/`）🛡️

**文件**: 7 源文件 · **扇入**: 10（最高）· **内部依赖**: 0

#### 四维检查顺序

```
guard.Check(profile, action)
  → checkTools:   glob 白名单，空 Allow 一律拒绝 (fail-closed)
  → checkFS:      read/write 路径 glob
  → checkShell:   策略 (deny/allowlist/denylist) + 元字符拦截
  → checkNet:     host 白名单
```

#### 安全强度评估

| 维度 | 强度 | 说明 |
|---|---|---|
| **Tools 白名单** | ★★★★★ | 空 Allow = 拒绝一切，fail-closed |
| **FS 路径守卫** | ★★★★☆ | Glob 匹配，但路径遍历需注意 |
| **Shell 元字符拦截** | ★★★★★ | 拒绝 `&&`、`||`、`;`、`|`、反引号、`$()`、`>`、`<`、换行 |
| **Net 白名单** | ★★★★☆ | Host 白名单，无端口级控制 |
| **交互式权限模式** | ★★★★☆ | default/allow-edits/yolo/auto 四种模式，通过 WebSocket 询问 |

#### 风险

- `RiskPrompt` 和 `ModeLabel`/`Modes` 测试覆盖不足（来自覆盖报告）
- 无超时保护的权限回调通道（goroutine 可能永久阻塞）

---

### 3.3 上下文压缩引擎（`internal/ctxcompact/`）📦

**文件**: 22 个源文件 · **核心文件**: `run.go`、`plan.go`、`summarize.go`、`assemble.go`

#### 双路径架构

```
Pre-turn (MaybeCompact in ws.go)
  → 在每次 user_message 前触发
  → 使用 ctxcompact.Run 统一核心
  → keepRecent 使用对数比例（/2 桥接）
  
Mid-turn (CompactingModel in llm/eino/compacting.go)
  → 在 ReAct 迭代之间触发
  → 使用 ctxcompact.Run 统一核心
  → keepRecent 使用消息数（= Config.KeepRecent）
```

#### 核心算法

| 阶段 | 函数 | 说明 |
|---|---|---|
| **1. Plan** | `Plan()` | 决定 pin 哪些消息（尾部 + user 原文 + working-set 路径 + 错误/diff 标记） |
| **2. Preserve** | `KeepToolCallPairs()` | fixpoint 保证 tool_call/result 配对不切断 |
| **3. Summarize** | `RunSummary()` | summary 输入 ≤ 0.9×窗口时走 cache-aligned 单次，否则携带式分块 |
| **4. Assemble** | `Assemble()` | summary 作为 user+sentinel 消息放历史末尾（避免双 system 冲突） |

#### 已知问题

- **压缩过度触发**：mid-turn 每次 ReAct 迭代都独立检查预算，无冷却机制
- **双路径不协调**：pre-turn 用 keepRecent*2=8 条消息，mid-turn 用 KeepRecent=4 条消息，桥接逻辑 `/2` 可能不准确
- **大工具结果快速消耗预算**：`fs_read` 大文件或 `shell_run` 大量输出可能在同一 turn 内多次触发压缩
- **Compaction 状态只走 TUI activity line**：不进 transcript，调试困难

---

### 3.4 版本控制（`internal/vcs/`）📝

**文件**: 9 源文件 · **核心文件**: `vcs.go`（1026 行）

#### 架构

```
SQLite 存储模型（类 git）：
  main           — 规范主干，工作副本是仓库根
  worktree       — 从 main_head 分出，位于 ~/.yanshi/worktrees/
  合并           — 树级三方合并
```

#### 自动追踪机制

- Agent 通过 Yanshi 工具的编辑自动被 VCS scope 捕获
- 聊天/编排器 → main
- task-agent / ACP-agent → 当前活动的 worktree
- MCP server 暴露 VCS 工具给外部 agent

#### 风险

- VCS 文件大小和性能在长期使用下未经验证
- 并发写场景的 SQLite 锁竞争
- Worktree 合并冲突处理策略可能需要人工介入

---

### 3.5 ACP — 外部 Agent 协议（`internal/acp/`）🔗

**文件**: 15 源文件

#### 职责

- 以子进程方式拉起外部 agent CLI（codex/claudecode）
- 传递 VCS MCP server 与权限策略
- JSON-RPC 2.0 协议适配

#### 质量

- `FakeAgent` 提供确定性测试
- `e2e_real_test.go` 有真实路径覆盖（门禁：`YANSHI_E2E=1` 且 PATH 上有 CLI）
- 策略传递和 VCS 追踪覆盖良好

---

### 3.6 目标循环（`internal/agent/goalloop/`）🎯

**文件**: 20 源文件

#### 流程

```
Plan → Implement → Evaluate → Judge
  ↑                               |
  └─────────── (未完成) ──────────┘
```

#### 关键组件

| 组件 | 职责 |
|---|---|
| `LLMPlanner` | 规划实现步骤 |
| `ACPImplementer` | 拉起外部 agent CLI 执行（worktree 分支） |
| `TestEvaluator` / `IntentEvaluator` / `QualityEvaluator` | 多维评估 |
| `AggregateJudge` | 判定是否完成 |
| `RuleTierer` | 根据目标文本选择 T0-T4 层级 |

#### 风险

- 需要真实 LLM 调用或外部 agent CLI（`FakePlanner` + `FakeImplementer` 仅用于演示）
- 与编排器的交互可能引发编排器自身的压缩/权限问题

---

### 3.7 TUI（`internal/cli/tui/`）🖥️

**文件**: 21 源文件 · **核心文件**: `model.go`（1099 行）

#### 特性

| 特性 | 实现 |
|---|---|
| **Powerline footer** | CometixLine 风格分段渲染，segment 间箭头过渡 |
| **流式子代理可见** | SubAgentEmit + SubAgentProgress → TUI 渲染 |
| **交互式权限模态** | permission_request → 用户确认/拒绝 |
| **Ctrl+Enter 换行** | 本地 fork bubbletea 修复 Windows 修饰键问题 |
| **Diff 查看** | `diff.go` 渲染文件变更 |

#### 结构松散问题

`cli/tui/` 是 `cli/` 的子包，但 TUI 的功能量和职责远超一个"子包"应有的规模（21 文件，~300KB）。**建议提升为独立的 `internal/tui/` 包**，减少 `cli/` 的耦合。

---

### 3.8 HTTP API（`internal/api/http/`）🌐

**文件**: 23 源文件 · **核心文件**: `ws.go`（1480 行——最大单体文件）

#### 端点

| 端点 | 传输 | 说明 |
|---|---|---|
| `/api/v1/chat/ws` | WebSocket | 主传输，双向，服务端持有历史 |
| `/api/v1/chat` | SSE | 备传输，客户端回放历史 |
| `/api/v1/task/*` | REST | 任务管理 |
| `/healthz` | HTTP | 健康检查 |

#### `ws.go` 超长问题

1480 行的 `ws.go` 是项目最大单体文件，也是**最高优先级的技术债**。职责过多：
- WebSocket 连接生命周期管理
- 权限回调通道管理（`permTracker`）
- 会话管理
- 帧路由解析
- Model 切换
- 交互式模式控制

**建议**: 拆分为 `ws_conn.go`（连接管理）、`ws_perm.go`（权限回调）、`ws_session.go`（会话）、`ws_model.go`（模型切换）。

---

### 3.9 工具层（`internal/tools/`）🔧

**文件**: 40 源文件（最大子系统）· **核心文件**: `agent.go`（1175 行）、`fs.go`（694 行）

#### 工具清单

| 工具 | 文件 | 行数 | 说明 |
|---|---|---|---|
| `agent_start` / `workflow_start` / `analysis` | `agent.go` | 1175 | 子代理委派——超 1000 行限制 |
| `fs_read` / `fs_write` / `fs_edit` / `fs_patch` / `fs_diff` | `fs*.go` | ~1200 | 文件系统操作 |
| `shell_run` | `shell.go` | ~300 | 命令执行 + Guard 检查 |
| `web_fetch` | `web.go` | ~100 | HTTP GET |
| `memory_*` | `memory.go` | ~200 | 记忆存储 |
| `vcs_*` | `vcs.go` | ~200 | VCS 操作 |
| `skill_use` | `skill.go` | ~100 | 技能加载 |
| `spillover` | `spillover.go` | ~100 | 大输出溢出 |
| `time_now` | `time.go` | ~50 | 时间查询 |

#### `agent.go` 超长问题

1175 行纯代码行超过 1000 行限制。建议拆分为 `agent.go`（主体）、`workflow.go`（workflow 逻辑）、`analysis.go`（分析模式）。

---

## 4. 交叉关注点分析

### 4.1 依赖图质量

```
依赖向内流动（无反向依赖）：
  guard (0 内部依赖, 扇入 10)
    → tools (依赖 guard, store, vcs, config)
      → agent/orchestrator (依赖 tools, llm)
        → api/http (依赖 orchestrator, guard, proto, ctxcompact)
          → bootstrap (依赖所有)
  
零循环依赖 ✓
零 init() 函数 ✓
```

### 4.2 配置流

```
config.yaml
    ↓ ${VAR} 展开
internal/config.Load()
    ↓ Config struct
bootstrap.Build()
    → store.Config → store.New()
    → vcs.Config → vcs.New()
    → guard profiles → tools.New()
    → llm providers → model.New()
    → orchestrator.Config → orchestrator.New()
    → http.Config → server.New()
```

### 4.3 数据流（一次对话 turn）

```
用户输入 (TUI/CLI)
    ↓ ClientFrame (JSON via WS or SSE)
api/http handler
    ├── decode frame type
    ├── session management (store)
    ├── permission injection (guard/mode)
    ├── pre-turn compaction check (ctxcompact.MaybeCompact)
    └── orchestrator.EventsWithHistoryOpts
            ↓
        ADK Runner → ChatModelAgent (ReAct loop)
            ├── Model.Generate/Stream (LLM provider)
            ├── ToolsNode (GuardedTool dispatch)
            │   ├── guard.Check (tools/fs/shell/net)
            │   ├── VCS auto-track (fs_edit/fs_write)
            │   └── Spillover (large output → temp file)
            ├── Sub-agent delegation (agent_start/workflow_start/analysis)
            └── CompactingModel (mid-turn compaction)
                ↓
            ServerFrame events (agent_chunk, tool_call, tool_result...)
                ↓
        SSI → Client (TUI render)
```

### 4.4 日志与可观测性

**现状**: 使用 `fmt.Print` / `fmt.Printf` 输出到 stderr，无结构化日志。

**影响**:
- 无法按级别过滤（debug/info/warn/error）
- 无法结构化查询（JSON 格式）
- 分布式追踪缺失（OpenTelemetry）

**建议**: 引入 `log/slog`（Go 1.21+ 内置），逐步替换 `fmt` 日志。这是**中等优先级**的改进。

### 4.5 重复逻辑

| 重复片段 | 所在位置 | 建议 |
|---|---|---|
| `injectTurnContext`（4 个方法重复） | `orchestrator/orchestrator.go` | 提取私有方法 |
| `expandHome`（2 处实现） | 多处 | 统一函数 |
| `firstNonEmpty` 模式 | 多处 | 提取通用 helper |
| Ln 成本计算（2 处） | 多处 | 统一函数 |

---

## 5. 代码质量与技术债

### 5.1 行级评分（各维度）

| 维度 | 评分 | 最强项 | 待改进 |
|---|---|---|---|
| **架构风格** | ★★★★★ | 六边形严格执行，依赖向内流动 | — |
| **安全设计** | ★★★★★ | Guard fail-closed + 架构级保证 | 权限回调无超时 |
| **依赖管理** | ★★★★★ | 零循环依赖，guard 零内部依赖 10 扇入 | — |
| **模块化** | ★★★★☆ | 29 个包职责清晰 | `cli/tui` 子包耦合 |
| **代码规范** | ★★★★☆ | `go vet` 零警告 | 3 个文件超 1000 行限制 |
| **正确性** | ★★★★☆ | UnknownToolsHandler 精心设计 | `SpentTokens` 死代码 |
| **可测试性** | ★★★★★ | Fake 优先，CI 无需 API key | Store 层覆盖薄弱 |
| **文档** | ★★★★★ | 包/doc 注释质量高；41 个 .md 设计文档 | — |
| **可观测性** | ★★★☆☆ | — | 无结构化日志 |
| **性能** | ★★★★☆ | — | SQLite 并发竞争 |
| **总分** | **★★★★☆ (4.1/5)** | | |

### 5.2 违反自定约束的文件

项目 CLAUDE.md 规定单文件纯代码行不超过 1000 行。以下文件违反此约束：

| 文件 | 行数 | 纯代码行 | 超出 | 建议拆分 |
|---|---|---|---|---|
| `internal/api/http/ws.go` | 1480 | ~1150 | ★★★ 最高优先级 | → ws_conn.go + ws_perm.go + ws_session.go |
| `internal/tools/agent.go` | 1175 | ~900+ | ★★★ 高优先级 | → agent.go + workflow.go + analysis.go |
| `internal/cli/tui/model.go` | 1099 | ~850 | ★★ 中优先级 | → model.go + state.go + handlers.go |
| `model_test.go` | — | 1162 | ★★ 中优先级 | 测试文件建议拆分 |

### 5.3 死代码

| 位置 | 代码 | 状态 |
|---|---|---|
| `internal/llm/eino/responses.go` | `SpentTokens` 字段 | 已定义但从未被读取（P0，小改动） |
| `fullcover.out` | 旧模块名覆盖率工件 | 来自 `github.com/x6nux/autocode`（pre-rename），应清理 |

### 5.4 未导出符号缺乏文档

以下 6 个包存在未文档化的未导出（或导出）符号（从 CLAUDE.md 分析）：

- `internal/plugin/` — 未导出类型无 doc
- `internal/task/` — 核心接口无 doc
- `internal/cli/tui/` — 部分 view helper 无 doc
- `internal/store/` — 部分内部方法无 doc

---

## 6. 风险登记簿

### 🔴 高风险

| # | 风险 | 影响 | 缓解措施 |
|---|---|---|---|
| R1 | `ws.go` 1480 行 — Goroutine 泄漏、竞态条件热点 | 服务端稳定性 | **P1** 拆分为多文件 |
| R2 | `agent.go` 1175 行 — 子代理逻辑复杂，超长难以审查 | Agent 行为异常 | **P1** 按职责拆分 |
| R3 | 双压缩路径不协调 — mid-turn 无冷却，可能过度压缩 | 对话质量下降，模型丢失上下文 | **P1** 添加冷却机制 + 协调逻辑 |

### 🟡 中风险

| # | 风险 | 影响 | 缓解措施 |
|---|---|---|---|
| R4 | 无结构化日志 — `fmt.Print` 散布各处 | 线上诊断困难 | **P2** 引入 `log/slog` |
| R5 | Store 测试覆盖率 57.5% — SQLite 操作未充分测试 | 数据损坏风险 | **P2** 补充测试 |
| R6 | `cli/tui` 作为 `cli` 子包 — TUI 规模远超子包应有大小 | 耦合增加，main.go 笨重 | **P2** 提升为独立包 |
| R7 | 权限回调无超时保护 — goroutine 可能永久阻塞 | 资源泄漏 | **P2** 添加 context 超时 |
| R8 | SQLite 跨进程竞争 — lockfile 保护了端口但未保护 DB | 数据竞争 | **P2** 添加 WAL 模式或连接池 |
| R9 | `context.Context` 滥用 — 通过 context 传递可变状态（VCS scope 覆盖） | 难以追踪的 bug | **P3** 文档化 context key 的所有权 |

### 🟢 低风险

| # | 风险 | 影响 | 缓解措施 |
|---|---|---|---|
| R10 | `SpentTokens` 死代码 | 无运行时影响 | **P0** 移除或实现 |
| R11 | `fullcover.out` 陈旧覆盖率工件 | 误导覆盖数据 | **P0** 清理 |
| R12 | Bubbletea fork 缺乏上游同步流程 | 无法接收上游修复 | **P3** 建立同步流程 |
| R13 | Eino-ext openai provider 不可用导致跳过测试 | 部分测试未运行 | **P3** 文档化或修复 provider |

---

## 7. 安全审计摘要

### 7.1 设计级安全承诺

| 承诺 | 实现 | 状态 |
|---|---|---|
| **Fail-closed** | `guard.Check` 空 Allow 拒绝一切 | ✅ |
| **深度防御** | Guard 四维检查 + 工具层二次验证 | ✅ |
| **最小权限** | Profile 配置化，按需授予 | ✅ |
| **输入验证** | Shell 元字符拦截、路径 glob | ✅ |
| **无状态守卫** | 无状态简化并发安全 | ✅ |

### 7.2 攻击面分析

| 攻击向量 | 防护 | 评级 |
|---|---|---|
| 工具名幻觉（模型注入） | UnknownToolsHandler 以结果而非 error 返回 | ✅ 强 |
| Shell 注入 | 元字符拦截 + 策略白名单 | ✅ 强 |
| 路径遍历 | FS glob 白名单 | ✅ 强 |
| 网络外连 | Host 白名单 | ✅ 中 |
| 拒绝服务（大输出） | Spillover 机制溢出到文件 | ✅ 中 |
| 权限提升（子代理） | 深度上限 + 独立 profile | ✅ 中 |

### 7.3 测试覆盖缺口

来自覆盖报告 (`fullcover.out`)：

| 未覆盖代码 | 风险 |
|---|---|
| `RiskPrompt`（guard） | 危险操作提示覆盖不足 |
| `ModeLabel`/`Modes`（guard） | 交互式模式枚举未测试 |
| `SSEEvent`（proto） | SSE 事件序列化未测试 |
| `alive_windows.go`（lockfile） | Windows 进程存活检测未测试 |

---

## 8. 测试基础设施与覆盖

### 8.1 测试架构

```
三层测试策略：
├── 单元测试（go test）— 所有包
│   ├── Fake 模型（FakeModel, FakePlanner, FakeBackend, FakeAgent）
│   └── 无需 API key
├── 集成测试（go test -tags e2e_real）
│   ├── ACP e2e（需要 codex/claudecode CLI）
│   ├── VCS e2e
│   └── 门禁：YANSHI_E2E=1
└── 增量测试（go run ./cmd/testchanged）
    └── git diff HEAD 检测变更包
```

### 8.2 覆盖率概览

| 包 | 覆盖率 | 评估 |
|---|---|---|
| config | ~85% | ✅ 良好 |
| guard | ~80% | ✅ 良好（边缘路径缺口） |
| lockfile | ~75% | ✅ 良好（Windows 路径缺口） |
| plugin | ~70% | ✅ 够用 |
| proto | ~67% | 🟡 需补充 SSEEvent |
| store | ~57.5% | 🔴 薄弱 |
| 其余包 | 变化 | 🟡 混合 |

### 8.3 测试命令速查

```sh
go build -o yanshi ./cmd/yanshi              # 构建 CLI
go run ./cmd/testchanged [flags]             # 仅测有变更的包
go test ./...                                # 全量测试套件
go test -tags e2e_real ./internal/acp/...    # 真实 CLI 端到端
go vet ./...                                 # vet（零警告）
```

### 8.4 测试质量观察

| 观察 | 说明 |
|---|---|
| ✅ **Fake 优先** | 所有主要外部依赖都有 fake 实现 |
| ✅ **增量测试** | `cmd/testchanged` 实用工具 |
| ✅ **e2e 门禁** | 环境变量 + PATH 双重门禁，避免 CI 假失败 |
| ✅ **Skip 是预期行为** | Eino-ext provider 不可用时的 skip 已文档化 |
| 🔴 **Store 覆盖薄弱** | 57.5% 是项目最低，SQLite 操作需更多测试 |
| 🟡 **属性测试缺失** | `ctxcompact` 可受益于基于属性的测试（压缩保证不丢失 info） |

---

## 9. 可操作改进建议（按优先级排序）

### P0 — 立即可做（小改动，高回报）

| # | 建议 | 文件 | 工时估计 | 影响 |
|---|---|---|---|---|
| 1 | **移除或实现 `SpentTokens` 死代码** | `internal/llm/eino/responses.go` | ~30min | 消除死代码警告 |
| 2 | **清理 `fullcover.out` 陈旧覆盖率工件** | 仓库根 | ~5min | 避免误导 |
| 3 | **提取 `injectTurnContext` 私有方法** | `internal/agent/orchestrator/orchestrator.go` | ~30min | 消除 4 处重复 |
| 4 | **提取 `expandHome` 为共享函数** | 多处 | ~30min | 消除重复实现 |

### P1 — 高优先级（1-2 天）

| # | 建议 | 文件 | 方案 | 影响 |
|---|---|---|---|---|
| 5 | **拆分 `ws.go`**（1480 行） | `internal/api/http/ws.go` | → `ws_conn.go` + `ws_perm.go` + `ws_session.go` + `ws_model.go` | 降低竞态风险 |
| 6 | **拆分 `agent.go`**（1175 行） | `internal/tools/agent.go` | → `agent.go` + `workflow.go` + `analysis.go` | 提高可审查性 |
| 7 | **双压缩路径协调** | `ctxcompact` + `compacting.go` | 添加 mid-turn 冷却机制 + 统一 keepRecent 语义 | 防止过度压缩 |
| 8 | **权限回调添加超时** | `internal/api/http/ws.go`（permTracker） | context.WithTimeout 保护 | 防止 goroutine 泄漏 |

### P2 — 中优先级（3-5 天）

| # | 建议 | 范围 | 方案 | 影响 |
|---|---|---|---|---|
| 9 | **引入 `log/slog` 结构化日志** | 全仓库 | 逐步替换 `fmt.Print` → `slog` | 可观测性提升 |
| 10 | **提升 `cli/tui` 为独立包 `internal/tui`** | `internal/cli/tui/` | 移动到 `internal/tui/`，解耦 `main.go` | 减少耦合 |
| 11 | **补充 Store 测试覆盖** | `internal/store/` | 目标 75%+ | 数据安全 |
| 12 | **添加 SQLite WAL 模式** | `internal/store/` | `PRAGMA journal_mode=WAL` | 并发性能 |
| 13 | **拆分 `model.go`**（1099 行） | `internal/cli/tui/model.go` | → `model.go` + `state.go` + `handlers.go` | 降低复杂度 |
| 14 | **添加 ctxcompact 属性测试** | `internal/ctxcompact/` | 压缩不丢失信息的属性 | 压缩正确性 |

### P3 — 低优先级（技术债清理）

| # | 建议 | 范围 | 方案 |
|---|---|---|---|
| 15 | **建立 bubbletea fork 同步流程** | `third_party/bubbletea/` | 定期 rebase 上游 |
| 16 | **补全未文档化导出符号** | 6 个包 | 添加 doc 注释 |
| 17 | **添加 eino-ext provider 修复或文档** | `internal/llm/eino/` | provider 不可用时明确 skip 原因 |

---

## 10. 未来方向

### 10.1 近期路线（M2 — 下一里程碑）

基于 `docs/superpowers/plans/` 中的现有计划：

1. ✅ **A12 结构化输出** — 已完成（`outputschema.go`）
2. ✅ **V12 Headless 执行** — 已完成（`--no-tui`）
3. ✅ **Session 指令 A11** — 已完成（`instruct` 包）
4. ✅ **多文件补丁 T06** — 已完成（`fs_patch.go`）
5. ⬜ **交互式压缩预览** — 计划中
6. ⬜ **Worktree 冲突 UI** — 计划中

### 10.2 中期关注

| 领域 | 方向 | 优先级 |
|---|---|---|
| **可观测性** | OpenTelemetry 集成 → 追踪 LLM 调用链 | P2 |
| **性能** | SQLite 连接池 + WAL → 减少锁竞争 | P2 |
| **安全性** | 权限回调超时 + 审计日志 | P2 |
| **可靠性** | Sub-agent 总数上限 + Goroutine 池 | P1 |
| **测试** | 属性测试 + Fuzz 测试（glob 匹配） | P2 |

### 10.3 长期愿景

| 愿景 | 路径 |
|---|---|
| **成为 Go LLM Agent 的参考实现** | 持续强化架构纯度，文档化设计决策 |
| **零信任安全模型** | 移除所有"信任"假设，每步验证 |
| **生态兼容** | 支持更多 LLM provider、MCP 工具、外部 agent |
| **全平台就绪** | 当前 Windows/Unix 均已覆盖，持续 CI |

---

## 附录 A：关键文件清单

### 超 500 行文件

| 文件 | 行数 | 职责 |
|---|---|---|
| `internal/api/http/ws.go` | 1480 | WebSocket handler（🚨 最大单体文件） |
| `internal/tools/agent.go` | 1175 | 子代理工具（🚨 超 1000 行） |
| `internal/cli/tui/model.go` | 1099 | TUI 核心模型 |
| `internal/vcs/vcs.go` | 1026 | VCS 核心实现 |
| `internal/llm/eino/anthropic.go` | 767 | Anthropic Claude API 适配 |
| `internal/tools/fs.go` | 694 | 文件系统工具 |
| `internal/agent/orchestrator/orchestrator.go` | 671 | 编排器核心 |
| `internal/cli/tui/styles.go` | 569 | TUI 样式系统 |
| `internal/llm/eino/resilient.go` | 551 | 弹性 LLM 调用 |

### 测试文件大于 500 行

| 文件 | 行数 | 说明 |
|---|---|---|
| `internal/cli/tui/model_test.go` | 1162 | 测试代码也超 1000 行限制 |
| `internal/api/http/ws_test.go` | ~800 | WebSocket 测试 |
| `internal/tools/agent_test.go` | ~700 | Agent 工具测试 |
| `internal/vcs/vcs_test.go` | ~600 | VCS 测试 |
| `internal/agent/goalloop/loop_test.go` | ~550 | 目标循环测试 |

---

## 附录 B：依赖图（简化）

```
guard (0内部依赖)
  ↑
tools           ← config, store, vcs
  ↑
agent/spawn     ← config
agent/worker    ← acp
agent/registry  ← config
  ↑
agent/orchestrator ← tools, llm/eino
  ↑
agent/goalloop     ← tools, orchestrator, acp
  ↑
api/http           ← orchestrator, guard, proto, ctxcompact, skills
  ↑
bootstrap          ← 全部
  ↑
cmd/yanshi         ← bootstrap, cli, config, version
```

---

## 附录 C：安全承诺验证清单

| 承诺 | 位置 | 是否满足 |
|---|---|---|
| Guard 在 Allow 为空时拒绝所有 | `guard/guard.go: checkTools()` | ✅ |
| Shell 元字符 `&&`, `||`, `;` 被拒绝 | `guard/guard.go: checkShell()` | ✅ |
| UnknownToolsHandler 不中断 turn | `orchestrator/orchestrator.go` | ✅ |
| 非 fatal 软降级不阻塞启动 | `bootstrap/bootstrap.go` | ✅ |
| Context 注入不污染工具参数 | `tools/permctx.go`, `tools/vcsctx.go` | ✅ |
| Sub-agent 深度上限 | `tools/subagent.go: MaxSubAgentDepth` | ✅ |
| VCS scope 覆盖策略清晰 | `tools/vcsctx.go` | ✅ |

---

> **本报告由以下分析源合成**：全文件清册（289 个 .go, 41 个 .md）· 静态源码分析· 依赖图逆向· 测试覆盖数据· memory 中 7 篇分析记录· 用户提供的项目清册。  
> **项目健康度**: 7.6/10（架构优秀，代码质量良好，工具链完善）  
> **一句话**: 这是 Go LLM Agent 生态中罕见的架构典范——六边形严格、安全为架构级承诺、Fake 优先测试。改进空间集中在文件拆分（2 个超长文件）、日志可观测性、存储测试覆盖三个方向，均无需架构重构。
