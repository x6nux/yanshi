# 安全守卫（Guard）

yanshi 的安全核心是 guard：一个**无状态、fail-closed**的六维权限检查器（`internal/guard::Guard.Check`）。每次工具调用前，guard 按 destructive → mcp → tools → fs → shell → net 依次检查，**第一个不满足的维度即短路拒绝**。

## 六个维度

顺序有意义：拒绝理由来自**第一个**说不的维度，后面的维度根本不跑。

| 顺序 | 维度 | 检查什么 |
|---|---|---|
| 1 | **destructive** | 破坏性删除。**与 profile 无关**，最先跑，再宽松的 profile 也绕不过（见下）。 |
| 2 | **mcp** | 动态发现的 `mcp_<server>_<tool>` 的独立白名单。**空 `allow` 拒绝一切 MCP 工具，哪怕 `tools.allow` 是 `["*"]`**。 |
| 3 | **tools** | 工具名 glob 白名单。**空 `allow` 一律拒绝**（见下）。 |
| 4 | **fs** | 读 / 写路径的 glob 白名单。 |
| 5 | **shell** | 元字符硬拦截 → `rules`（execpolicy）→ `policy` + `patterns`（见下）。 |
| 6 | **net** | 是否允许出站 + host 白名单。 |

> **mcp 排在 tools 前面不是笔误。** 它是独立的一道门，所以 `tools: { allow: ["*"] }` **放不出任何 MCP 工具** —— `config.example.yaml` 的 `orchestrator` profile 里那句 `mcp: { allow: [] }` 就是在显式关掉它们。要用 MCP 工具，必须在 `mcp.allow` 里按 server/tool 逐个（或按 glob，如 `"mcp_github_*"`）放行。

## shell 的三层与四个合法 policy

shell 维度按顺序过三层，任一层给出结论就短路：

1. **元字符硬拦截** —— 见下一节，无条件、不可覆盖。
2. **`rules`（execpolicy 结构化规则表）** —— 非空时**完全接管**，`policy`/`patterns` 根本不会被读到。
3. **`policy` + `patterns`（legacy glob 开关）** —— `rules` 为空时才生效。

`policy` 只有四个合法值，**没有 `allow`**：

| policy | 含义 |
|---|---|
| `""`（省略） | 等同 `allowlist`。 |
| `allowlist` | 命中 `patterns` 之一 → 放行；否则 **Prompt**（交互式可批准）。 |
| `denylist` | 命中 `patterns` 之一 → 拒绝（**可覆盖**）；否则放行。**"不限制 shell" 用空 patterns 的 `denylist` 表达。** |
| `deny` | 一律拒绝（**可覆盖**）。 |

> ⚠️ **写错 policy 会让 shell 永久锁死。** 未知值落进 `checkShell` 的 default 分支，返回的是**结构性 HardDeny** —— `yolo` 和 `auto` 都越不过去（见下方"两档 HardDeny"）。因此配置加载时就会校验：`profiles.<名>.shell.policy` 不在上表里，`config.Load` 直接报错退出，不会让它拖到第一次 `shell_run` 才炸。

## 两档 HardDeny：结构性 vs 可覆盖

拒绝分两档（`Decision.Overridable`）：

- **结构性 HardDeny**（`Overridable=false`）—— **任何模式都越不过**：shell 元字符、execpolicy parse-error、未知 shell policy、灾难性批量删除。
- **可覆盖 HardDeny**（`Overridable=true`）—— profile 能说"不"的一切：空的 tools/fs allowlist、空的 mcp allowlist、`shell.policy: "deny"`、denylist 命中、execpolicy `hard_deny` 规则、`net.allow: false`。`yolo` 直接越过，`auto` 交给 AI 风险评分。

## 破坏性删除门（profile 无关）

`rm -rf` 打到 `/`、`~`、`$HOME`、`*`、`/etc`、`/usr`、`/home`、`C:\`、工作目录自身或其祖先 = **Catastrophic**，结构性 HardDeny，**所有模式都拦**（`yolo` 也拦）。删除工作目录之外的路径 = **OutOfScope**，升级为 Prompt。工作目录**内部**的 `rm -rf build/` 不受此门限制，仍由 shell 维度决定。

## fail-closed：空 Allow 拒绝一切

> 这是架构级安全承诺，不可妥协。详见 [../adr/0003-guard-fail-closed-empty-allow.md](../adr/0003-guard-fail-closed-empty-allow.md)。

空的工具白名单不是"无约束"，而是"什么都不允许"。新增任何工具都必须在 profile 里**显式**配权限才能被调用——不会因开发者忘了配权限而静默放行。

## shell 元字符硬拦截

`shell_run` 在 `rules` / glob 白名单**之前**就硬拦截元字符：`&&`、单个 `&`（后台执行）、`||`、`;`、`|`、反引号、`$(`、`>`、`<`、换行与回车。无论 glob 或 execpolicy 怎么配，含这些字符的命令一律拒绝，且这是**结构性** HardDeny —— `yolo` / `auto` 都越不过（见 [../adr/0004-guard-stateless-and-shell-metachar-hardblock.md](../adr/0004-guard-stateless-and-shell-metachar-hardblock.md)）。

> 需要顺序执行多条命令时，**改为多次顺序调用**，不要试图放开元字符。

## profile

权限来自 `profiles:` 配置 map（见 [configuration.md](configuration.md)）。每个 profile 是一条具名的**五维**策略（`tools` / `mcp` / `fs` / `shell` / `net` —— destructive 是 profile 之外的结构性维度，不可配置）。示例里的 `coding` profile 给了一个"全工具 + 指定 MCP server + 仓库读写 + allowlist shell + 出站网络"的例子。

### profile 名怎么被选中

profile **不是**按 `agents[].profile` 字段选的 —— 那个字段今天**没有任何生产读点**。当前只有两条真实的选取路径，都以 **profile 的 map 键名**为准：

| 谁 | 怎么选 |
|---|---|
| 编排器（TUI / WS / SSE 聊天） | 固定读键名 `orchestrator`（`internal/bootstrap::Build`）。没有这个键就退回内置的 `DefaultOrchestratorProfile`。 |
| task-API 的远程 worker | 用 **worker 名**当键名查（`internal/api/http::Server.TaskAPI`）。`cmd/agent-worker -name coding` 才会拿到 `coding` profile；查不到时 fail-closed 退回 deny-all。 |

所以示例里的 `coding` profile 想生效，要么把它改名成 `orchestrator`（让聊天用上），要么起一个 `-name coding` 的 worker。`agents:` 块里写 `profile: "coding"` 不会有任何效果，也不会有任何告警。

## 交互式权限模式

在 profile 之上叠加交互式模式（仅 WS 路径可用，见 [tui.md](tui.md)）：

| 模式 | 行为 |
|---|---|
| `default` | 普通拒绝弹窗询问；profile 策略拒绝（`policy: "deny"` 等）**静默拒绝**，不问。 |
| `allow-edits` | 编辑类工具（`internal/guard::EditToolNames`）免提示放行，其余同 `default`。 |
| `yolo` | 越过全部 profile 策略（含 MCP allowlist）。**仍然拦**：灾难性删除、工作目录之外的删除、强制批准工具。 |
| `auto` | 灾难性删除直接拦；其余一切（含 profile 拒绝、越界删除）交给 AI 风险评分，低分放行、高分弹窗。评分失败一律回落弹窗。 |
| `plan` | 只读，写操作一律拒绝。 |

> **`yolo` 不是"放行所有"。** 结构性 HardDeny（元字符、未知 policy、execpolicy parse-error）与灾难性删除在任何模式下都拦得住；强制批准工具（下一节）也一样。`yolo` 越过的是 **profile 说的"不"**。

> SSE 备用路径用静态 profile，**不支持**交互式弹窗（见 [../adr/0010-sse-static-profile-no-interactive-perm.md](../adr/0010-sse-static-profile-no-interactive-perm.md)）。

## 强制批准工具（approval-required）：只在交互式传输上可用

有一类工具的破坏力或成本高到不适合被任何静态策略预先放行，它们被标记为**强制批准**：每次调用都必须由用户当场点"允许"，**profile 的 `tools.allow`（哪怕是 `"*"`）、历史授权记录、`yolo` / `auto` 模式一律绕不过**，"始终允许"这个选项对它们也无效。

当前的强制批准工具：

| 工具 | 为什么 |
|---|---|
| `automation_create` / `list` / `read` / `update` / `pause` / `resume` / `delete` / `run` | 持久化定时任务：一次批准会让 agent 在未来无人值守地反复运行 |
| `agent_batch` | 一次调用扇出 N 个子 agent，成本远高于普通工具调用 |
| `github_comment` / `github_approve` / `github_merge` | 对外部仓库的不可撤销写操作 |
| `screenshot` | 抓取屏幕内容 |

**必然后果：这些工具只在 WebSocket / TUI 上可用。** 强制批准需要一条能把弹窗送达用户的双向通道，而只有 WS 传输具备。在 SSE、v1 REST、task-agent 这三条非交互式路径上调用它们，会**恒定**收到：

```
✗ permission denied: tool requires explicit approval
```

这不是缺陷，也不是配置错误 —— 没有人在场，就没有人能批准。若需要在非交互式路径上使用这类能力，正确做法是引入 profile 级的**预授权策略**，而不是把工具降级成普通门禁（那会让它的描述对用户撒谎）。
