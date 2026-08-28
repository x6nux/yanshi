# ADR-0016: guard 保留两个 shell reader，但它们共用同一个词法层

- 状态：accepted
- 日期：2026-08-29

## 背景（Context）

guard 对一条 shell 命令有**两个**独立的读法：

- `internal/guard/destructive.go::lexShellLite` —— 破坏性删除门用。它必须**宽容对待词的内容**：
  `*`、`$HOME`、`C:\` 正是它要抓的灾难形式，拒绝它们等于对灾难视而不见。
- `internal/execpolicy/commandlist.go::ParseCommandList` —— execpolicy / fs 维度用。它必须
  **严格对待结构**：命令替换、进程替换、子 shell、here-doc、后台 `&`、未闭合引号一律 fail-closed，
  因为「这条命令从哪里到哪里」答错一次就是一次静默放行。

INF1 之后两者都在授权路径上，而 W-B 第一批的两轮评审各抓到一条**同一形状**的绕过，
分别走两者盲点中的一个：

| 绕过 | 走谁的盲点 | 后果（实测） |
|---|---|---|
| `r\m -rf /` | `lexShellLite` 不解反斜杠转义 | 程序词读成 `m`，verdict Allow，真 `/bin/sh` 执行了 `rm -rf /` |
| `echo … > ~/$'\x2e\x73\x73\x68'/authorized_keys` | `ParseCommandList` 不解 ANSI-C | 重定向目标读成字面量，凭据清单按 `~/.ssh` 前缀匹配不上，密钥无提示落盘 |

**「两套读法」本身不是缺陷 —— 它们回答的是不同的问题。** 缺陷是两者对**同一个词**
解出不同的字节：一个懂 ANSI-C 不懂反斜杠，另一个懂反斜杠不懂 ANSI-C。

被否决的替代方案：

- **合并成一个 parser。** 严格的那半会把 `ls *.go` 变成结构性 HardDeny（`ParseCommandList`
  自己的包头解释过为什么它不能走 `Lex`）；宽容的那半会让「这条命令到哪里结束」变成猜测。
  一个 parser 同时满足两个相反的严格性要求，只能靠一堆模式开关，那是两个 parser 穿一件外套。
- **给每个盲点各打一个补丁。** 前两轮就是这么做的，于是同一形状连出两条 Blocking。
  补丁不产生「下一个盲点在哪」这个问题的答案。

## 决策（Decision）

**保留两个 reader，把它们的词法层（word layer）统一到一份实现上。**

「词法层」的定义是：**一个 token 在引号解析完成之后是哪些字节**。当前这一层包含 ANSI-C
`$'…'` 解码（`internal/execpolicy/ansic.go::DecodeANSIC` / `DecodeANSICSpan`，从 guard 迁出）
与反斜杠转义。结构层（哪里是命令边界、哪里是重定向）**不统一** —— 那正是两者必须不同的地方。

配套一条可被检验的不变量，而不是一句约定：**拼法不改变判决**。同一条命令的任意混淆拼法与它的
朴素拼法必须得到同一个 `Guard.Check` verdict。守在
`internal/guard::TestSpellingDoesNotChangeTheVerdict` 上。

## 后果（Consequences）

- ANSI-C 解码器现在住在 `internal/execpolicy`，两个 reader 共用一份，
  「一个懂另一个不懂」这个形状在这一层上被消除。
- **不可违反的约束**：**新增任何词法层能力（新的引号形式、新的转义、新的展开）时，
  两个 reader 必须同时获得它。** 单边实现会立刻重建本 ADR 记录的那个缺陷形状。
  判据不是 code review 而是 `TestSpellingDoesNotChangeTheVerdict`：往它的等价类表里加一行
  新拼法，两个 reader 中任何一个没跟上都会变红。
- **不可违反的约束**：**词法层的解码只能写进「当前 token」，绝不能在扫描之前对原始字符串做。**
  `$'\x26\x26'` 在 token 内是两个字节的数据，在原始串上解码则会**凭空造出一条 `&&` 链**。
  两个 reader 的实现都依赖这条，`internal/execpolicy::TestParseCommandListDecodesANSICWords`
  两个方向都钉住了。
- **不可违反的约束**：**结构层不统一，且这不是待办。** 严格与宽容是两个 reader 各自存在的
  理由；把 `ParseCommandList` 的结构性拒绝放宽到 `lexShellLite` 的宽容度，会把
  「读不懂就拒绝」换成「读不懂就猜」。
- 代价：`internal/guard` 现在从 `internal/guard/destructive.go` 也依赖 `internal/execpolicy`
  （此前只有 `guard.go` / `generalize.go` 依赖）。方向仍然向内，GOV1 的 port allowlist 不变。

## 关联

- 来源：W-B 第一批再评审 R-1 / R-2（`.superpowers/sdd/2026-08-28-w-b-exec-safety/rereview-b1.md`）。
- 相关代码落点：`internal/execpolicy/ansic.go`、`internal/execpolicy/commandlist.go`、
  `internal/guard/destructive.go`、`internal/guard/prefixrunner.go`。
- 前置：[ADR-0004](0004-guard-stateless-and-shell-metachar-hardblock.md) 及其 INF1 增补
  —— 逐段判定把这两个 reader 同时放上了授权路径，本 ADR 是那条决策的直接后果。
