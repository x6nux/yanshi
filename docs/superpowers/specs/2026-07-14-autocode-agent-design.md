# autocode 智能体系统设计文档

- **日期**：2026-07-14
- **状态**：设计已确认，待评审
- **技术栈**：Go + [Eino](https://github.com/cloudwego/eino)（CloudWeGo LLM 框架）+ [ACP](https://agentclientprotocol.com/)（Agent Client Protocol）+ [go-plugin](https://github.com/hashicorp/go-plugin)（HashiCorp）
- **定位参考**：OpenClaw / Hermes 类智能体产品

---

## 1. 概述

`autocode` 是一个用 Go 实现的**多智能体协作系统**：一个**综合定位的主 agent（Orchestrator）**负责理解用户意图、拆解任务、委派给**功能各异的子 agent** 并聚合结果。子 agent 有三类来源——进程内 Eino agent、本机外部编码 agent（经 ACP）、远程 worker（经自研 Task API）。

系统面向**单用户、本机信任**的场景（类似 Claude Code 个人版的多 agent 化），通过**按 agent 类型分级授权**调和「可信」与「安全」。

### 1.1 核心目标

- 主 agent 综合编排，子 agent 功能各异（编程、联网搜索、知识检索、数据分析、多模态、外部系统对接）。
- 子 agent 可为：本地（进程内）、外部编码 CLI（本机子进程）、远程 worker（网络）。
- **自驱动 Goal Loop**（类 Codex goal loop）：给一个功能目标 → 严格 TDD 规划 → 多 agent 团队实现 → 多角度评判 → 未完成则继续，直至验证完成。
- 接入层分阶段：v1 = CLI + HTTP API；后续 = Web 聊天页 + IM 机器人（内置 + go-plugin 热插拔）。
- 编码能力复用成熟外部 agent（opencode / codex / claudecode），经 ACP 统一接入。

---

## 2. 关键设计决策

| # | 维度 | 决定 |
|---|---|---|
| 1 | 定位 | 多智能体；主 agent 综合编排，子 agent 功能各异 |
| 2 | 接入 | v1：CLI + HTTP API（SSE 流式）；规划：Web 聊天页 + IM 机器人（内置 + go-plugin 连接器） |
| 3 | 子 agent 能力 | 全域（编程、搜索、知识检索、数据分析、多模态、外部系统）；v1 编码走外部 agent |
| 4 | 子 agent 协调 | 自研 Task API，pull-based，支持本地 + 远程 |
| 5 | LLM | 多 provider 可配置（主/子可不同）+ 重试 + 故障转移（`ResilientChatModel`） |
| 6 | 部署/信任 | 单用户本机信任 + 按 agent 类型分级授权（`PermissionGuard`，策略型沙箱，无容器/VM） |
| 7 | 记忆 | **无向量**；记忆存 SQLite + FTS5 全文索引；定期整理；工具查询 |
| 8 | 远程 agent 权限 | 远程 agent 从 Task API 拉取自己的 `PermissionProfile` 配置，**本地自执行**（同一套 Guard 代码） |
| 9 | 外部编码 agent | opencode / codex / claudecode，经 **ACP** 统一接入；自研 Go ACP 客户端 |
| 10 | 持久化 | SQLite（会话 / 任务 / 记忆+FTS5 / kv） |
| 11 | 鉴权 | v1 单用户，简单共享 API Token（配置） |
| 12 | 自驱动 | Goal Loop：TDD 规划 → 多 agent 团队实现（Lead+Workers+Integrator）→ 多角度评判 → 裁决 → 迭代至完成；命令 + 自动判断双入口（进入即提示） |

---

## 3. 威胁模型与权限

### 3.1 信任假设

- **单用户、本机信任**：运行者即所有者，主体可信。
- 不引入容器/VM 沙箱（过重）。「沙箱」= **工具调用时的策略校验**（`PermissionGuard`）。

### 3.2 权限模型（PermissionGuard）

每个子 agent 绑定一个 **`PermissionProfile`**，在**每次工具调用前**逐项校验，越界返回结构化 `Deny` 错误：

```yaml
coding_agent:               # 示例：编程 agent
  fs:
    read:  ["D:\\code\\**", "%TMP%\\**"]
    write: ["D:\\code\\**", "%TMP%\\**"]
  tools:  ["fs.*", "shell.*"]
  shell:
    policy: allowlist        # allowlist | denylist | deny
    patterns: ["go *", "git *", "npm *", "python *"]
  net:
    allow: false

search_agent:               # 示例：搜索 agent
  fs: { read: ["%TMP%\\**"], write: ["%TMP%\\**"] }
  tools: ["web.search", "web.fetch"]
  shell: { policy: deny }
  net: { allow: true, hosts: ["*"] }
```

**字段**：`fs`（读/写路径 globs）、`tools`（允许工具）、`shell`（策略 + 命令模式）、`net`（是否允许 + 域名限制）。可扩展 `env`、资源限额等。

**执行机制**：profile 通过 `context.Context` 在调用链全程透传；`tools/guard` 包装层取出当前 agent 的 profile 并校验。

### 3.3 三类子 agent 的权限执行差异

| 子 agent 类型 | 权限执行 | 说明 |
|---|---|---|
| 本地 Eino（进程内） | **强制** | Guard 实打实拦截每个工具调用 |
| 外部 CLI（ACP） | **可拦截**（结构化可见） | ACP 把工具/文件/终端操作以结构化事件冒出，客户端有拦截点过 Guard；具体可拦到哪一层按 ACP FS/Terminals 语义在实现时核实 |
| 远程 worker | **协同式自约束**（cooperative） | worker 从 Task API 拉取自己的 profile，用同一套 Guard 代码本地自执行。受信二进制（如自有 `agent-worker`）才有效；恶意 worker 可绕过。主 agent 另以「受控任务路由 + 结果校验」兜底 |

---

## 4. 整体架构

五层结构（自底向上）：

```
┌─────────────────────────────────────────────────────────────┐
│  ⑤ 接入层 (Access)                                           │
│   CLI · HTTP API(用户/SSE流式) · 连接器[内置 + go-plugin]      │
│   连接器: Web聊天页 / IM机器人(微信·飞书·钉钉)                 │
├─────────────────────────────────────────────────────────────┤
│  ④ 服务层 (Serving)                                          │
│   HTTP API Server(用户会话/流式) · Task API(子agent拉任务/报结果)│
├─────────────────────────────────────────────────────────────┤
│  ③ 智能体核心 (Agent Core) —— 大脑                            │
│   主 Orchestrator(综合编排/拆解/聚合) · 自驱动 Goal Loop       │
│   子agent: 本地Eino / 外部CLI(ACP) / 远程worker(Task API)     │
│   Task Broker(任务生命周期/分发/聚合/重试) · Registry(三类统一)│
├─────────────────────────────────────────────────────────────┤
│  ② 能力层 (Tools + Guard)                                    │
│   工具: 文件/shell/web/记忆/知识/图表/多模态/外部对接          │
│   PermissionGuard ← 每次调用校验 PermissionProfile           │
├─────────────────────────────────────────────────────────────┤
│  ① 基础层 (Foundation)                                       │
│   ResilientChatModel(provider链·重试·故障转移)                │
│   Storage(SQLite: 任务/会话/记忆/FTS5) · Config               │
│   记忆整理后台 job(consolidation)                             │
└─────────────────────────────────────────────────────────────┘
```

### 4.1 关键数据流

**流① 本地对话**：用户 → CLI → 主 Orchestrator → 拆解 → 委派**本地子 agent**（进程内，DeepAgent 直调，工具经 Guard）→ 聚合 → 流式返回。

**流② 外部编码 agent（ACP）**：Orchestrator → 选定编码 agent（opencode/codex/claudecode）→ 启动 ACP 子进程 → 经 ACP 发 prompt turn → 收结构化事件（工具/文件/终端/diff，过 Guard 拦截）→ 取结果聚合。

**流③ 远程子 agent**：Orchestrator 把任务写入 Task Broker（SQLite）→ Task API 暴露 → 远程 worker **拉任务**（先 `GET /agent/profile` 取权限，本地自执行）→ 在自己环境跑 → **POST 结果** → Broker 更新 → Orchestrator 取回继续推理。

**流④ Web/IM 用户**：Web/IM → 连接器（内置或 go-plugin）→ HTTP API → 主 Orchestrator →（同流①）。

**流⑤ 自驱动 Goal Loop**：见 §12。

### 4.2 三个架构要点

1. **混合执行**：本地子 agent 进程内直调（DeepAgent 委派，快）；外部编码 agent 经 ACP（本机子进程）；远程子 agent 走 Task API（统一、异构）。三者对 Orchestrator 都是「可委派的子 agent」，区别只在传输——由 `Registry` 统一抽象。
2. **Task API 是唯一远程子 agent 接口**：pull-based，配 SSE 通知避免忙轮询。
3. **权限只在工具调用点强制**：Guard 是本地 agent 的唯一关卡；ACP agent 靠协议事件拦截；远程 agent 靠自取配置 + 结果校验。

---

## 5. 子 agent 三类抽象

`Registry` 把三类子 agent 统一为同一接口，Orchestrator 委派时不关心来源：

| 类型 | 实现 | 传输 | 权限 |
|---|---|---|---|
| 本地 Eino | 进程内 Eino agent | 进程内直调 | 强制（Guard） |
| 外部 CLI | opencode/codex/claudecode 适配器 | ACP（JSON-RPC/stdio，本机子进程） | 可拦截（ACP 事件过 Guard） |
| 远程 worker | `agent-worker` 或任意讲 Task API 的进程 | Task API（HTTP，pull） | 协同式自约束 |

---

## 6. 外部编码 agent 与 ACP

### 6.1 为什么用 ACP

经核实（[ACP 官网](https://agentclientprotocol.com/)、[Agents 列表](https://agentclientprotocol.com/overview/agents)）：**三个目标 agent 都支持 ACP**。

| agent | ACP 支持 | 接入方式 |
|---|---|---|
| opencode | ✅ 原生 | 直接作为 ACP 子进程 |
| claudecode | ✅ | 经 Zed 的 SDK adapter |
| codex | ✅ | 经 Zed 的 adapter |

ACP（Agent Client Protocol，Zed 出品）是「编辑器 ↔ 编码 agent」的标准协议（类比 LSP），agent 作为客户端子进程、走 JSON-RPC over stdio，复用 MCP 的 JSON 表示并扩展 diff/工具调用/文件系统/终端等编码场景类型。

**优势**：
1. **统一接口**——一个协议对接三个（及未来 Gemini CLI/Goose/OpenHands 等），取代三套各自拼 CLI + 解析 stdout 的适配器。
2. **结构化交互**——工具调用、文件改动、终端命令、diff、session/turn 都是结构化事件。
3. **权限可拦截**——agent 的文件/终端操作经协议冒出，客户端有拦截点过 `PermissionGuard`，从「盲执行」升级为「可见、可拦」（具体粒度按 ACP FS/Terminals 语义实现时核实）。
4. **未来免扩**——任何新 ACP agent 直接接入。

### 6.2 代价与处理

- **无官方 Go SDK**（ACP 官方库仅 Kotlin/Python/Rust/TS）：自研轻量 Go ACP 客户端（JSON-RPC/stdio + 初始化握手 + session/turn），schema 公开，工作量可控。
- **claudecode / codex 走 adapter**：实现时确认 Zed adapter 能否脱离编辑器独立调用；不行则把 adapter 作为子进程包一层。opencode 原生最干净。
- **ACP 偏编辑器视角**：我们当 ACP 客户端即可，diff 展示类 UX 类型用处不大，核心 prompt turn ↔ 干活 ↔ 结果完全契合。
- v1 **不做** raw-CLI 兜底（YAGNI）；遇适配不稳再加。

---

## 7. LLM 弹性网关（ResilientChatModel）

在 Eino `ChatModel` 接口之上加一层弹性包装：

- 每个 agent 配一条**有序 provider 链**（如 `claude → gpt-4o → 豆包`）。
- 单 provider 内**重试**（指数退避，仅对可重试错误：限流/超时/5xx）。
- 链内**故障转移**：当前 provider 重试耗尽，自动切下一个 provider 继续该轮。
- 主 agent 与子 agent 可配不同链（主用强模型链，子用便宜模型链）。
- 对上层透明。

provider 通过 eino-ext 适配（OpenAI、Claude、Gemini、Ark/豆包、Ollama 等），配置驱动、可切换。

---

## 8. 记忆子系统（无向量）

- **存储**：记忆作为结构化记录存入 SQLite，配 **FTS5 全文索引**（SQLite 自带，零依赖）。
- **定期整理（consolidation）**：后台 job（ticker 触发，按时间或记忆条数阈值）把原始/碎片记忆压缩、去重、提炼成高信号事实，老化无用项——类似「记忆睡眠整理」。
- **工具查询**：agent 持有 `memory.search`（FTS 全文）/`memory.recall`（结构化过滤）/`memory.write` 工具，**自行判断何时查**，不自动注入上下文。

> **知识检索**同理：早期清单中的「知识库 RAG」顺势改为 **SQLite + FTS5 + 工具查询**（无 embedding 管线）。整个系统**完全不引入向量库**。

---

## 9. Task API 契约

子 agent 侧（`/api/v1/...`，Bearer Token；token 编码 worker 身份 → 映射 profile + 允许的 capability 标签）：

| 方法 | 路径 | 作用 |
|---|---|---|
| GET | `/agent/profile` | 拉取本 agent 的 `PermissionProfile`（自执行） |
| POST | `/tasks/claim` | 认领下一个匹配能力的任务，原子置 `running`；空则 204 |
| GET | `/tasks/events` | **SSE**：推送「有新任务」通知，避免忙轮询 |
| POST | `/tasks/:id/heartbeat` | 保活；超时未心跳 → broker 自动重排队 |
| POST | `/tasks/:id/progress` | 上报进度（可选） |
| POST | `/tasks/:id/result` | 上报完成/失败 + 产出（output/artifacts） |
| GET | `/tasks/:id` | 查任务状态 |

**Task 对象**：`{ id, type(能力标签), input(载荷), status, assignedTo, createdAt, updatedAt, deadline, result, parentTaskId }`

**状态机**：`pending → claimed → running → completed | failed | timeout`；失败 → 重试（上限 N）→ 重新入队 `pending`；心跳超时 → 重新入队。

**用户侧 HTTP API**（另设，给 CLI/Web/IM 用）：`POST /chat`（SSE 流式）、`GET /sessions[/:id]`、`POST /sessions/:id/abort`。

> ACP 仅用于「本机外部编码 agent」一类（stdio/本机）；不覆盖远程。远程编码 agent = 远程机器跑 `agent-worker`、内部再用 ACP 调本地编码 CLI、再经 Task API 上报。

---

## 10. Go 模块结构

```
autocode/
├─ cmd/
│  ├─ autocode/              # 主程序: 启动 agent核心 + HTTP/Task API + CLI
│  └─ agent-worker/          # 远程子agent worker(独立二进制, 拉Task API)
├─ internal/
│  ├─ agent/
│  │  ├─ orchestrator/       # 主编排agent(基于Eino DeepAgent)
│  │  ├─ goalloop/           # 自驱动模式(见 §12)
│  │  │  ├─ trigger.go       #   意图分类 + 确认提示(命令/自动双入口)
│  │  │  ├─ planner.go       #   [1] TDD 规划
│  │  │  ├─ implement/       #   [2] 实现团队
│  │  │  │  ├─ lead.go       #     Team Lead(拆活/分派/协调)
│  │  │  │  ├─ worker.go     #     Worker(经 ACP, worktree 隔离)
│  │  │  │  └─ integrator.go #     Integrator(合并/解冲突/跑测试)
│  │  │  ├─ evaluate.go      #   [3] 多角度评判 fan-out
│  │  │  ├─ judge.go         #   [4] 裁决聚合
│  │  │  └─ loop.go          #   闭环控制 + 终止条件
│  │  ├─ subagent/
│  │  │  ├─ coding/ search/ knowledge/ data/ multimodal/ extsys/  # 本地Eino (extsys=外部系统对接)
│  │  │  ├─ external/        # 外部CLI agent(ACP)
│  │  │  │  ├─ acp/          # 自研Go ACP客户端(JSON-RPC/stdio + init/session/turn + types)
│  │  │  │  ├─ guard_intercept.go  # ACP 工具/FS/终端事件过 PermissionGuard
│  │  │  │  └─ launch/       # opencode(原生) / claudecode(adapter) / codex(adapter) 启动描述
│  │  │  └─ registry/        # 三类子agent统一注册表
│  ├─ task/                  # Task Broker: 生命周期/状态机/分发/聚合/重试 + store
│  ├─ llm/                   # ResilientChatModel(provider链+重试+故障转移) + provider适配
│  ├─ tools/                 # 工具实现, 每个经 PermissionGuard 包装
│  │  └─ guard/              # 守卫包装层
│  ├─ guard/                 # PermissionProfile + 策略校验(=沙箱, 本地/远程共用)
│  ├─ store/                 # SQLite封装
│  │  ├─ session/ task/ memory(+FTS5) kv/
│  │  └─ memory/consolidate.go   # 定期整理job
│  ├─ api/
│  │  ├─ http/               # 用户HTTP API(SSE流式)
│  │  └─ taskapi/            # 子agent Task API
│  ├─ access/
│  │  ├─ cli/                # CLI客户端
│  │  ├─ connector/          # 连接器接口 + 内置实现
│  │  └─ web/                # 内置Web聊天页
│  ├─ plugin/                # go-plugin 宿主(connector热插拔)
│  └─ config/                # 配置schema + 加载
├─ web/                      # 内置前端资源
├─ proto/                    # go-plugin gRPC契约(connector)
└─ config.example.yaml
```

**要点**：
- `agent-worker` 是**独立二进制**——远程子 agent 就是跑它（或任何讲 Task API 的进程），主程序不依赖它。
- `guard` 包**本地与远程共用**（远程 worker 自取 profile 后跑同一份守卫代码）。
- 工具与守卫分离：工具只管「做什么」，`guard` 只管「能不能做」，组合在 `tools/guard`。

---

## 11. 接入层（Access）

- **v1**：内置 **CLI**（走 HTTP API 或进程内直调）。
- **go-plugin 宿主骨架**：v1 提供，为 Web/IM 连接器热插拔预留。
- **Phase 2**：内置 **Web 聊天页** + 首个 **IM 连接器**（内置或插件，如飞书/微信）。
- **Phase 3**：更多 IM 平台（go-plugin 连接器）。
- 连接器统一适配外部通道 → HTTP API。

go-plugin（HashiCorp，Terraform/Vault 同款）选型理由：插件为独立进程、本地 socket 走 gRPC/net-rpc、进程隔离 + 版本协商，契合「内置一部分、运行时加载额外插件」。

---

## 12. 自驱动 Goal Loop（类 Codex goal loop）

给一个功能目标，系统**自主**完成：严格 TDD 规划 → 多 agent 团队实现 → 多角度评判 → 未完成则继续，直至验证完成。它是 Orchestrator 的一个**自驱动模式**，复用 Registry / 编码 agent / Task Broker / Guard。

### 12.1 触发（命令 + 自动判断，进入即提示）

两种入口，**都透明**：

- **显式命令**：`/goal <feature>` → 直接进入自驱动模式。
- **自动判断**：消息进 Orchestrator 时先过**意图分类**（轻量/便宜模型），判定 `简单问答 | 功能实现目标 | 其它`；若为「功能实现目标」→ **提示用户确认**（「检测到这是个功能实现目标，是否进入自驱动模式？」）→ 确认才进，拒绝当普通对话。

自动判断**不静默自主**，进入即提示，用户可随时拒绝/中断。

### 12.2 闭环

```
用户目标 ──(命令 or 自动判断+确认)──▶ GoalLoop
   │
   ▼
[1] 规划(TDD,严格) → 验收测试集(= definition of done) + 实现计划
   ▼
[2] 实现团队(多agent工作流, 参考 Claude Code workflow agent team)
      Team Lead 拆活 → Workers(worktree 隔离, 经 ACP) 并行 → Integrator 合并+跑测试
   ▼
[3] 评判(多agent 多角度 fan-out 并行) → 每角度: verdict + 证据 + 缺口
   ▼
[4] 裁决(Judge 聚合) → 完成?
      ├─ 完成 → 报告用户
      └─ 未完成 → 缺口反馈 → 回 [2](或 [1] 若需重规划)
终止: 完成 / 最大迭代数 / 预算耗尽 / 用户中断
```

### 12.3 各阶段

**[1] 严格 TDD 规划**：先把「目标 = 行为」拆成**验收测试**（红）——这些测试就是**客观的 definition of done**；再产出实现计划（写测试 → 实现 → 绿 → 重构）。测试集可机器执行，给评判阶段的「测试验证」角度提供客观判据。

**[2] 实现团队（多 agent 工作流）**——参考 Claude Code 的 workflow agent team：

- **Team Lead**：把实现计划拆成可并行子任务，分派、协调、控冲突（决定哪些并行、哪些须串行）。
- **Workers**：多个编码 agent（opencode/codex/claudecode，经 ACP），各负责一块；**worktree 隔离**（git worktree/分支）并行实现，避免互踩。
- **Integrator**：合并各 worker 产出、解冲突、跑验收测试集，产出「实现成果」交给评判。
- **隔离前提**：多 agent 并发改同一代码库必然冲突 → worktree-per-worker 是干净解法（前提：目标项目是 git 仓库；非 git 则 Team Lead 退化为串行分派）。
- 全程经 Task Broker 跟踪。

**[3] 多角度评判（多 agent fan-out）**——并行起多个评判 agent，各守一个角度，返回 `{ verdict: pass|fail, evidence, gaps[] }`：

| 评判 agent | 角度 | 类型 |
|---|---|---|
| 测试/行为验证 | 验收测试是否全过？覆盖足不足？ | 客观（机器） |
| 意图/需求符合 | 实现是否真正满足用户目标意图？ | 语义 |
| 代码质量 | 是否清晰、地道、可维护？ | 质量 |
| 边界/对抗 *(Phase 2)* | 遗漏的边界？会怎么坏？ | 鲁棒性 |
| 安全 *(Phase 2)* | 有无安全问题？ | 安全 |

**[4] 裁决（Judge）**：聚合所有评判 → 判定「完成/未完成」+ 汇总具体缺口 → 未完成则把缺口作为下一轮实现输入。

### 12.4 终止与安全

- **终止条件**：完成 ✅ / 达到**最大迭代数** / **预算（token/费用）耗尽** / **用户中断**。
- 自驱动多轮、多 agent 并发、费 token → 迭代/预算上限为硬约束；可中断（对齐 Eino interrupt/resume）。
- 评判 agent 多为**只读**权限；编码 Worker 限定工作目录 + 测试目录（各 worker 自己的 worktree）。

### 12.5 与架构的关系

Goal Loop 是**嵌套多 agent**：外层闭环的 [2] 实现与 [3] 评判各自是多 agent 工作流，均通过 Registry 起子 agent、用 Guard 校权限、用 Task Broker 跟踪。它不引入新的基础设施，只新增 `goalloop/` 这一层编排。

---

## 13. 分阶段计划

### Phase 1（v1，本 spec 实现目标）—— 端到端跑通

- **基础**：SQLite（会话/任务/记忆+FTS5/kv）、Config、`ResilientChatModel`（1~2 个 provider + 重试 + 故障转移）
- **核心**：Orchestrator（DeepAgent）+ **外部 agent**（opencode/codex/claudecode 经 ACP，编码主力）+ 本地 **search** 子 agent（+ 可选 knowledge/FTS）+ **1 个远程 worker** demo + registry + Task Broker
- **工具**：`web.*`、`memory.*`（+FTS）；自研 `fs/shell` 工具**降级到 Phase 2**（编码交给外部 agent）
- **守卫**：`PermissionProfile` + `PermissionGuard`（本地 + 远程共用）
- **服务**：用户 HTTP API（chat SSE）+ Task API（claim/result/heartbeat/profile/events）+ **`agent-worker` 独立二进制** + 自研 Go **ACP 客户端**
- **接入**：内置 **CLI** + go-plugin 宿主骨架
- **自驱动 Goal Loop（Phase 1 收尾里程碑，最小可用）**：
  - 双入口触发（命令 + 自动判断+确认）
  - Plan(TDD) → 实现团队（**Lead + 2 Worker + Integrator**，worktree 隔离）→ 评判（**测试验证 + 意图符合 + 代码质量** 3 个）→ Judge → loop/done
- **验收**：
  - 本地多 agent 对话跑通
  - 一个远程 worker 成功拉任务、自取 profile、上报结果
  - 一个编码任务委派给 claudecode（或 opencode/codex）→ 在限定工作目录跑完 → 结果回传聚合
  - **一个功能目标经 Goal Loop 自主完成**（TDD 规划 → 团队实现 → 三角度评判 → 验证通过）

### Phase 2 —— 接入层补全 + Goal Loop 增强

- 内置 **Web 聊天页**
- 补齐子 agent（数据、多模态、外部系统）+ 更多工具（`fs/shell` 等）
- 首个 **IM 连接器**（内置或插件，如飞书/微信）
- 记忆整理 job 调优
- **Goal Loop 增强**：更多 Worker、更多评判角度（边界/对抗、安全）、重规划策略、worktree 高级合并、评判可观测、并发优化

### Phase 3 —— 扩展与打磨

- 更多 IM 平台（go-plugin 连接器）
- 可观测性（Eino callback 的 tracing/metrics）、auth 细化、UI 打磨
- 可选：子 agent 消费外部 MCP 工具

---

## 14. 待实现时核实项（不阻塞设计）

1. **ACP FS/Terminals 语义**：是「上报」还是「中介」？决定 `guard_intercept` 能拦到哪一层（影响外部 agent 权限强制力度）。
2. **Zed adapter 独立可用性**：claudecode/codex 的 ACP adapter 能否脱离 Zed 编辑器独立以子进程调用。
3. **ACP Go 客户端工作量**：对照官方 schema 评估自研客户端的边界。
4. **Goal Loop worktree 隔离前提**：目标项目须为 git 仓库；非 git 项目的退化策略（Team Lead 串行分派）需在实现时明确。
5. **Goal Loop 并发安全**：多 Worker 并发实现的冲突频率与 Integrator 合并策略，需实测调优。

---

## 参考

- Eino：https://github.com/cloudwego/eino
- Agent Client Protocol：https://agentclientprotocol.com/ （[Agents 列表](https://agentclientprotocol.com/overview/agents)）
- go-plugin：https://github.com/hashicorp/go-plugin
- opencode：https://github.com/sst/opencode
- codex：https://github.com/openai/codex
