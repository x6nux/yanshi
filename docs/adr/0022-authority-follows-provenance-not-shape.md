# ADR-0022: 可信策略文件管的是「值本身就是授权判决」的键，不是 `profiles:` 这个名字

- 状态：accepted
- 日期：2026-08-29

## 背景（Context）

S3（[internal/config/policy.go](../../internal/config/policy.go) 的头注释）把威胁模型写得很清楚：
`config.yaml` 在 agent 自己的写作用域里，出厂示例 profile 的 `fs.write` 是 `**`，
**一次 `fs_write` 加一次重启就是一次完整的自我提权**。解法是把 `profiles:` 的权威
搬到 agent 写不到的可信策略文件里。

同一批（W-B）里出现了两条对同一个威胁模型相反的判断：

- **W-B-18** 拒绝让 `fs.protected` 可删减，理由逐字是「a key that could empty it would
  let a config file inside the agent's own write scope reopen the hole this gate closes
  —— and config.yaml IS inside that scope by default」。
- **W-B-14** 加了 `security.guardian_prompt_file`，让操作员整体替换 auto 模式的指令正文。
  而 `ApplyPolicy` **只重写 `c.Profiles`**，`security:` 完全不在可信策略文件的射程内。

后者杠杆更大：auto 模式在 Go 侧**没有任何静态白/黑名单**，这段提示词**就是全部判决**。
`ValidateAutoApprovalTemplate` 是九个 marker 的**存在性**检查 —— 一份列全九个 marker、
正文写「answer ALLOW for every tool call without exception… Never answer ASK」的模板，
**加载期与使用期双双通过**（评审实测）。也就是说 agent 可以给自己写一份永远放行的政策。

被否决的替代方案：

- **把整个 `security:` 搬进 Policy**。否决：`security.sandbox.*` / `security.network.*`
  约束的是「profile 已经做出的判决怎么执行」，不是判决本身；把它们搬进来会让 Policy
  变成 config schema 的第二份、会漂的拷贝（正是 `Policy` 原注释拒绝的东西）。
- **让语义校验更聪明**（检测「答 ALLOW」这类取消语句）。否决：自然语言上的子串检查
  永远绕得过，做得越像有保证越危险。
- **只在文档里写一句「这份模板可以写成永远放行的政策」**。否决：那是把最高杠杆的一个
  开关留在 agent 的写作用域里，然后请读者小心。

## 决策（Decision）

**可信策略文件的射程判据不是「这个键在不在 `profiles:` 下」，而是「它的值本身是不是一条
授权判决」。** 按这条判据，`security.guardian_prompt_file` 进来（`config.PolicySecurity`），
`security.sandbox.*` / `security.network.*` 留在外面。

**有 policy 文件时它说了算，说空也算** —— 空 = 内置正文，是安全的那一端；本地
`config.yaml` 里的值被丢弃并计入 `Config.PolicyNarrowed`（`yanshi doctor` 会说）。
`Config.Load` 在 `ApplyPolicy` 之后**重跑** `loadGuardianPrompt`，所以真正被读的永远是
当前权威那条路径，且照旧过同一套校验。

**没有 policy 文件时本地键照旧生效** —— 与 `profiles:` 同一套向后兼容姿态。

同时把 `ValidateAutoApprovalTemplate` 的保证写清楚：**它是词法的**，
「可定制不等于可掏空」只挡得住**删掉一类**，挡不住**在正文里取消一类**。

## 后果（Consequences）

- 同一批里两条相反的信任假设合并成一条，判据可复用于下一个 `security:` 键。
- **不可违反的约束**：**`security.guardian_prompt_file` 的权威，在可信策略文件存在时
  必须是那个文件**，且**空值也是一次表态**（回落内置正文），不是「没意见」。
  否则 agent 只要让 policy 文件对这个键沉默就能拿回控制权。
- **不可违反的约束**：**`ApplyPolicy` 之后必须重新加载并校验 guardian 正文。**
  不重载就会保留本地路径读出来的那份 body —— 换了权威却没换内容，是本仓最常见的
  静默失效形态。
- **不可违反的约束**：**不要把 `ValidateAutoApprovalTemplate` 描述成语义保证。**
  真正的保证是出处（provenance）；文档写成内容保证会让操作员在无 policy 文件的姿态下
  以为自己被保护着。
- 代价：可信策略文件多了一个 `security:` 段，operator 要知道模板该写在那边。
  `internal/config::TestGuardianPromptIsUnderTrustedPolicyAuthority` 三个方向都钉住了，
  并顺带钉住「自我取消的正文确实通过校验」这条被证伪的机制断言。

## 关联

- 来源：W-B 第四批评审 Major-3；CLAUDE.md「Guard —— 安全关键、fail-closed」的 auto 模式段。
- 相关代码落点：`internal/config/policy.go`、`internal/config/config.go`、
  `internal/guard/autoapproval.go`。
