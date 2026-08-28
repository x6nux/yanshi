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
| 5 | **shell** | 拆段（拆不出就拒）→ 每段：重定向目标进 fs 判定 + `rules`（execpolicy）或 `policy` + `patterns`（见下）→ 取最严的一段。 |
| 6 | **net** | 是否允许出站 + host 白名单。 |

> **mcp 排在 tools 前面不是笔误。** 它是独立的一道门，所以 `tools: { allow: ["*"] }` **放不出任何 MCP 工具** —— `config.example.yaml` 的 `orchestrator` profile 里那句 `mcp: { allow: [] }` 就是在显式关掉它们。要用 MCP 工具，必须在 `mcp.allow` 里按 server/tool 逐个（或按 glob，如 `"mcp_github_*"`）放行。

## shell 的三层与四个合法 policy

shell 维度先拆段（见下一节），再对**每一段**过下面两层，任一层给出结论就短路；整条取最严的一段：

1. **拆段** —— 拆不出来就无条件、不可覆盖地拒绝。见下一节。
2. **`rules`（execpolicy 结构化规则表）** —— 非空时**完全接管**，`policy`/`patterns` 根本不会被读到。
3. **`policy` + `patterns`（legacy glob 开关）** —— `rules` 为空时才生效。

`policy` 只有四个合法值，**没有 `allow`**：

| policy | 含义 |
|---|---|
| `""`（省略） | 等同 `allowlist`。 |
| `allowlist` | 命中 `patterns` 之一 → 放行；否则 **Prompt**（交互式可批准）。 |
| `denylist` | 命中 `patterns` 之一 → 拒绝（**可覆盖**）；否则放行。**"不限制 shell" 用空 patterns 的 `denylist` 表达。** |
| `deny` | 一律拒绝（**可覆盖**）。 |

> ⚠️ **`rules` 为空时写错 policy 会让 shell 永久锁死。** 未知值落进 `checkShellPolicy` 的 default 分支，返回的是**结构性 HardDeny** —— `yolo` 和 `auto` 都越不过去（见下方"两档 HardDeny"）。因此配置加载时就会校验：`profiles.<名>.shell.policy` 不在上表里，`config.Load` 直接报错退出，不会让它拖到第一次 `shell_run` 才炸。
>
> **`rules` 非空时这道校验不跑**，和上面第 2 层说的一致：`rules` 完全接管，`policy` 根本不会被读到，写错的值是**惰性**的，不影响这个 profile 今天的任何行为。校验只在那个值真的能拒绝东西时才拦，不会因为一个 guard 从不读的字段拒绝一次启动。清空 `rules` 后该值变成活的，下一次加载就会被拒。范围与理由见 `internal/config::Config.validateProfiles` 的 doc 注释。

## 两档 HardDeny：结构性 vs 可覆盖

拒绝分两档（`Decision.Overridable`）：

- **结构性 HardDeny**（`Overridable=false`）—— **任何模式都越不过**：shell 结构读不出来、execpolicy parse-error、未知 shell policy、灾难性批量删除。
- **可覆盖 HardDeny**（`Overridable=true`）—— profile 能说"不"的一切：空的 tools/fs allowlist、空的 mcp allowlist、`shell.policy: "deny"`、denylist 命中、execpolicy `hard_deny` 规则、`net.allow: false`。`yolo` 直接越过，`auto` 交给 AI 判断。

> **本页这份枚举比 `CLAUDE.md` 的同名枚举短一项，两边都对**（两边的当前条数都别从这里读，`CLAUDE.md` 那份自带现场清点命令）。少的那一项是源码里 `checkShellPolicy` 的另一个结构性分支（`switch result.Verdict` 的 `default`），它是**防御性的、从任何配置都到不了**：`execpolicy.Evaluate` 的出口集合是 `allow` / `prompt` / `hard_deny`，三个都被前面的 `case` 接住了。规则里把 `decision` 写错（比如 `decision: warn`）不会走到那里 —— `Evaluate` 自己先把它转成 `hard_deny`，落进 `case "hard_deny", "deny":`，那是**可覆盖**的一档，`yolo` 能越过。本页不列它，`CLAUDE.md` 那份枚举面向改 guard 源码的人，把源码分支也数进去。这个"出口集合到不了 default"由 `internal/guard::TestExecPolicyVerdictsAreHandledByCheckShell` 钉住。
>
> ⚠️ **别把这个差别读成「不可达就不数」——那个判据不成立。** 上面第 3 条「未知 shell policy」同样从 yaml 走不到：`rules` 为空时 `internal/config::Config.validateProfiles` 让 `config.Load` 当场拒绝加载，`rules` 非空时 `checkShellPolicy` 在 execpolicy 分支里就 return 了、policy switch 根本不可达（那个函数的 doc 注释写了为什么两条都不留活口）。按「不可达就不数」这条也该删，本页就只剩 3 条。**在两个都不可达的分支之间**，判据是**有没有对应的配置面**：`shell.policy` 是操作者会亲手写进 yaml 的键，所以即使当前不可达也留在本页；execpolicy 的 verdict 词表不是任何配置字段，写规则的人碰不到它，所以只留在 `CLAUDE.md`。

> 这个判据**只在裁决不可达分支时用，不要提升成全页判据**：另外三条（shell 结构读不出来、execpolicy parse-error、灾难性批量删除）都不对应任何 yaml 键，它们由**命令内容**触发而不是由配置打开，照配置面去筛会把本页从 4 条砍成 1 条。本页的入选标准始终是「操作者需要知道它拦得住什么」，配置面只是不可达分支的补充裁决。

## 破坏性删除门（profile 无关）

`rm -rf` 打到 `/`、`~`、`$HOME`、`*`、`/etc`、`/usr`、`/home`、`C:\`、工作目录自身或其祖先 = **Catastrophic**，结构性 HardDeny，**所有模式都拦**（`yolo` 也拦）。删除工作目录之外的路径 = **OutOfScope**，升级为 Prompt。工作目录**内部**的 `rm -rf build/` 不受此门限制，仍由 shell 维度决定。

## fail-closed：空 Allow 拒绝一切

> 这是架构级安全承诺，不可妥协。详见 [../adr/0003-guard-fail-closed-empty-allow.md](../adr/0003-guard-fail-closed-empty-allow.md)。

空的工具白名单不是"无约束"，而是"什么都不允许"。新增任何工具都必须在 profile 里**显式**配权限才能被调用——不会因开发者忘了配权限而静默放行。

## shell 命令的结构：逐段判定，读不出来就拒

`shell_run` 先把命令拆成**段**（以 `&&`、`||`、`;`、`|` 为界），然后对**每一段**跑一遍完整的 shell 判定（rules 或 glob 白名单），整条命令的判决取**最严的那一段**。任何一段被拒，整条被拒。

这意味着：

- `git status && go test` 在 `patterns: ["git *", "go test"]` 下**可以跑**了（两段都命中白名单）。以前含 `&&` 一律拒绝，需要拆成两次调用。
- `git status && curl http://x` 在同一份 patterns 下**整条被拒**：`curl` 那段不命中，最严的那段决定整条。
- **重定向的目标路径会进 fs 维度判定**。`echo x > ~/.ssh/authorized_keys` 的程序是 `echo`，只看程序等于没看；去掉前导 fd 数字后以 `<` 开头的按读、其余按写，送去和 `fs.write` / `fs.read` 白名单以及内建凭据 denylist 对账。
  - **`>&文件` 算写**，不是描述符复制。bash / sh / zsh 三者都把 `>&` 后面的非数字词当成文件写进去；只有 `>&数字`（如 `2>&1`）和 `>&-` 不指向文件。
  - **重定向可以写在命令词之前**，`>/dev/null rm -rf /` 仍然是一条 `rm -rf /`，破坏性删除门照样拦。

**拆不出段的形态仍然一律拒绝，且是结构性 HardDeny**（`yolo` / `auto` 都越不过）：命令替换 `$(…)` 与反引号、进程替换 `<(…)` / `>(…)`、子 shell 括号 `( )`、here-document `<<`、后台执行的单个 `&`、裸换行与回车、未闭合的引号、结尾的反斜杠。这些形态里「真正要跑的文本」不在被判定的这个字符串里，所以判它等于判了别的东西。

理由、代价与不变量见 [../adr/0004-guard-stateless-and-shell-metachar-hardblock.md](../adr/0004-guard-stateless-and-shell-metachar-hardblock.md) 的补充后果一节。

> **注意一个额外的收口**：链式命令**没法交互批准**。落到 Prompt 的链会在审批作用域构造时被拒（一条批准规则不能覆盖多个可执行段），所以链要么每一段都被静态放行、要么被拒 —— 弹窗那条路对它不开。

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
| `auto` | 灾难性删除直接拦、越界删除弹窗；**其余一切交给 AI 判断**（`guard.AutoApprovalPrompt`），Go 侧没有静态白/黑名单。模型拿到完整命令原文 + 会话上下文（用户最近的请求、workdir、策略拒绝理由）答 ALLOW/ASK。风险类别写在提示词里，四组：伸出项目之外（提权/关机/磁盘/系统账户/防火墙/系统包管理器/定时任务/远程执行）、不可逆（force-push、删除 VCS 未记录的东西、容器逃逸）、**执行没人读过的代码**（下载即执行、从 `/tmp` `~/Downloads` 跑脚本 —— 远程脚本必须先落盘审计）、数据外泄（外传项目内容/凭据、`env` 把 API key 打进 transcript）。无模型、超时、出错、回复读不懂 → 一律弹窗；auto 退化成 manual，不退化成放行。无阈值可调。 |
| `plan` | 只读，写操作一律拒绝。 |

> **`yolo` 不是"放行所有"。** 结构性 HardDeny（shell 结构读不出来、未知 policy、execpolicy parse-error）与灾难性删除在任何模式下都拦得住；强制批准工具（下一节）也一样。`yolo` 越过的是 **profile 说的"不"**。

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
