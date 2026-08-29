# ADR-0020: 一个词里的命令，也是没人读过的 payload

- 状态：accepted
- 日期：2026-08-29

## 背景（Context）

[ADR-0018](0018-an-unread-payload-is-a-refusal-not-a-pass.md) 立了「读不懂 ⇒ 拒绝」，[ADR-0019](0019-the-tier-follows-the-payload-not-the-program-name.md) 把档位从程序名挪到 payload。到本条之前，一条命令携带另一条命令的方式有**三种**有读者：

| # | 入口 | 读者 |
|---|---|---|
| 1 | `<程序> -c <payload>` | `internal/guard::opaquePayload` 的 flag 扫描 → `internal/guard::gradeUnreadPayload` |
| 2 | `<程序> … <尾部 argv>` | `internal/guard::classifyTrailingArgv` |
| 3 | `<程序> <位置操作数>` | `internal/guard::looksLikeStatement` |

**第四种一个读者都没有，而且它比上面三种都更基本 —— 其余三种都要攻击者猜中一个名字（程序名 / flag 拼法），这一种一个都不用猜**，因为赋值前缀是 POSIX shell 的**语法**而不是某个程序的约定。

`internal/guard::lexShellLite` 把赋值前缀当作要**走过去**的东西（`internal/guard::assignmentPrefixLen`），`internal/guard::expandKnownParameters` 只解析**被 `$VAR` 用到**的赋值。于是赋值的**值**从来没有被任何一条路径读过。真 `/bin/sh` + 只含录音器的 PATH 实测，十条形态全部 Allow 且录音器真的收到了 `rm -rf /`：

```
GIT_SSH_COMMAND='rm -rf /' git fetch origin      GIT_PAGER='rm -rf /' git log
PAGER='rm -rf /' git log                          GIT_EDITOR='rm -rf /' git commit
GIT_EXTERNAL_DIFF='rm -rf /' git diff HEAD        VISUAL='rm -rf /' crontab -e
EDITOR='rm -rf /' crontab -e                      LESSOPEN='|rm -rf /' less foo.txt
RSYNC_RSH='rm -rf /' rsync a host:b               MANPAGER='rm -rf /' man git
```

**同一个盲区还有第二半：命令被引号收进了一个 argv 词里。** `internal/guard::classifyTrailingArgv` 读的是 argv 的**后缀**，把每个词当程序词；`nu --commands "rm -rf /"` 的那三个词是**一个**词，`internal/guard::normalizeProgramWord` 取最后一个 `/` 之后的 base，得到空串。`internal/guard::looksLikeStatement` 也救不了它：那条判据要求**空白且结构标点**，而 `rm -rf /` 恰好只有空白。于是 ADR-0019 白纸黑字的「whichever program was going to receive it」在实现里退化成了「**只要那六个 flag 拼法之一宣告过它**」—— `nu --commands` 与 `fish -C` 是已发布的真实语法，都在那六个之外。

### 被否决的替代方案

**A. 已知危险环境变量名的 denylist**（`GIT_SSH_COMMAND` / `LESSOPEN` / `PYTHONSTARTUP` / `BASH_ENV` / …）。**两个维度同时无界**：变量名无界，读它的程序也无界。这正是 `internal/guard/opaque.go` 包头说程序名表永远追不完的那种形状，只是搬到了赋值位。

**B. 把 `codePayloadFlags` 补齐**（`--commands`、`-C`、`--init-command`、`--run`…）。第 1 入口做过六轮的动作；`zzshell --exec 'rm -rf /'` 与裸位置操作数 `zzshell 'rm -rf /'` 还在外面。

**C. 给赋值前缀的值判**全档**（Catastrophic）。** 那会让 `MSG='rm -rf /' echo hi` 变成不可申诉的拒绝，而这条命令什么都不跑。**字符串里没有任何东西能把它和 `GIT_SSH_COMMAND=…` 那一行分开** —— 只有名字能，而名字正是本条拒绝去查的东西。

**D. 不修，记进语料当长尾。** 判据是控制者写死的：「能构造出一整族静默放行（不是单个刁钻拼法）= 结构性的洞」。这一族的两个维度都无界，且过严代价实测为零（八条日常带赋值前缀的命令全部保持 Allow）。

## 决策（Decision）

**一、一个词若能被读成一条 shell 命令、而那个读法是破坏性的，它就是一段没人读过的 payload。判据只问那个词，不问变量叫什么、也不问后面那个程序叫什么。**

落点 `internal/guard::classifyWordAsCommand`，两个调用点：

- `internal/guard::classifyAssignmentPrefix` —— `internal/guard::lexShellLite` 走过去的那些赋值词，由 `internal/guard::classifyDestruction` 作为**又一次读法**折叠进来。
- `internal/guard::classifyTrailingArgv` 的词循环 —— argv 里的每一个词。

一个词有两种读法（`internal/guard::commandReadingsOfWord`）：词本身（它不是 flag 时），以及 `NAME=VALUE` 形态下的**值那一半**。值那一半是必须的：`NAME=` 前缀会让下游每一个读者失效 —— `core.pager=rm -rf /` 的程序词是 `core.pager=rm`（不在任何表里），`GIT_SSH_COMMAND=rm -rf /` 的程序词是 `-rf`（一个 flag）。实测 `docker run -e "CMD=rm -rf /" img` 就是这样：整串 `CMD=rm -rf /` 一路走到了 `gradeUnreadPayload`，然后被赋值走法读成 None。

**二、这一档封顶在 `DestructionOpaque`（弹窗）。** 「接收它的那个程序会不会执行这个字符串」正是不知道的那件事，字符串本身回答不了。与 `classifyTrailingArgv` 给未知程序封顶的理由逐字相同。

**三、`yolo` 对 `DestructionOpaque` 不再自动放行，改为弹窗。**

落点 `internal/api/http::resolvePermissionMode` 的破坏性 switch。这一档此前**根本不在那个 switch 里**，于是 yolo 自动批准了整档 —— 包括 W-B-B2-3 刚刚闭合的那一族（`pkexec rm -rf /`）。**「我读不出这条命令安不安全」接着「自动批准它」，等于否掉这一档存在的全部理由。**

它走的是 `req.ForcePrompt` 那条路 —— 返回 `(deny, **false**)`，即「不自动放行、交回 callback 显式审批」—— 而**不是**隔壁 `OutOfScope` 在 yolo 下那条 `(deny, true)`（直接拦）。差别是这一档自己的说法：越界删除知道删的东西在项目外面，Opaque 只知道**没人读过**，而不可申诉的拒绝要求理由能被陈述（ADR-0018）。`auto` 刻意不特判：它交给模型，而模型读到的是完整原文。

> **与 ADR-0019 替代方案 C 的关系。** 那条否决的是「**用** `resolvePermissionMode` 的一个 switch **代替**把档位判据挪到 payload」——理由是判据会跑到 guard 包外面，且 `Decision` 与 `ClassifyDestruction` 两个消费点要各改一次。本条不是那个：档位判据已经在 guard 里（ADR-0019 落地了），这里改的是**剩下那一档的模式语义**，而且两个消费点在这件事上是**一致**的（`Decision` 本来就是 Prompt，分类值是 Opaque，两边都说「问」）。

## 后果（Consequences）

- **不可违反的约束：判据不得退化成一张变量名表或程序名表。** 看守是 `internal/guard::TestTheValueOfAnAssignmentPrefixIsRead` 的第一行（`internal/guard::unmodelledVariable` + `internal/guard::unmodelledInterpreter`，后者由 `internal/guard::TestNoTableModelsTheProbeProgram` 证明不在本包任何表里）与 `internal/guard::TestAWholeCommandInsideOneWordIsRead` 的头两行（编造的程序名 + 编造的 flag 拼法）。用名单实现的版本能满足其余每一行，唯独过不了这两行。
- **不可违反的约束：这一档封顶在 `DestructionOpaque`，不得升级为结构性档。** 没有它，`MSG='rm -rf /' echo hi` 会被不可申诉地拒绝。看守是同一条测试里那条 `ClassifyDestruction(...) == DestructionOpaque` 的断言。
- **不可违反的约束：`yolo` 必须对 `DestructionOpaque` 返回「未解决」而不是「已放行」。** 看守是 `internal/api/http::TestYoloAsksAboutAPayloadNobodyRead`，它先断言那几行**仍然是 Opaque 档**再断言模式判决 —— 否则某一行被别处升成 Catastrophic 时，测试会因为与本条无关的理由变绿。反向样本是 `internal/api/http::TestYoloStillAutoApprovesAReadableCommand`：没有它，「yolo 什么都不自动放行」也能满足上一条。
- **不可违反的约束：过严缓解表只 gate「词读法」，不 gate 尾部后缀读法。** `internal/guard::nonInterpreterPrograms` 说的是「这个程序的**操作数**是数据」，那正是词读法在问的问题；把它扩到后缀读法会让 `git rm -r .` 退回静默放行。看守是 `internal/guard::TestAWholeCommandInsideOneWordIsRead` 末尾那条 `git rm -r .` 断言。**赋值前缀读法刻意不查这张表**：环境变量不是任何东西的操作数。
- **过严的代价，实测记下来而不是绕过去。** 一个**开头**就是破坏性命令的词会多一次弹窗：`git commit -m "rm -rf / considered harmful"`。这是 `internal/guard/opaque.go` 包头允许本文件犯的那一类错误，而且比 `gradeUnreadPayload` 对 flag 标记那一半已经在犯的错**更轻**（`zzsend -c "rm -rf / is dangerous"` 是不可申诉的地板）。八条日常带赋值前缀的命令（`CGO_ENABLED=0 go build ./...` 等）实测**全部保持 Allow**。
- **`X='rm -rf /' sh -c "$X"` 从 Allow 变成弹窗，而真 shell 什么也不跑**（前缀赋值只进子进程环境，父 shell 在赋值生效前就把 `"$X"` 展开成空）。这是本条封顶规则的直接后果，不是缺陷：值确实被放进了子进程的环境，而谁会执行它正是不知道的那件事。
- **本条不解决文件型入口。** `BASH_ENV=./payload.sh bash -c :`、`sh ./payload.sh`、`LD_PRELOAD=./evil.so ls` 的 payload 不在这个字符串里，值是一个**路径**而不是一条命令，所以词读法看不见它。把它们变成弹窗要拦下 `sh ./script.sh` 与 `. ./venv/bin/activate` 这类日常操作，代价远大于本条；语料里逐条记着。
- **SSE 那条路更严而不是更松**：没有 callback，Prompt 直接落到 `internal/tools::Authorize` 的 fail-closed 分支。看守是 `internal/api/http::TestOpaqueIsFailClosedWithNoCallback`。

## 关联

- 延续：[ADR-0018](0018-an-unread-payload-is-a-refusal-not-a-pass.md)（读不懂 ⇒ 拒绝）、[ADR-0019](0019-the-tier-follows-the-payload-not-the-program-name.md)（档位从 payload 读）。本条补的是**入口**：0018/0019 说的是拿到 payload 之后怎么办，本条说的是**还有一个入口没人在那儿等**。
- 来源：W-B 第二批终局验证 `verify-b2-final.md` 的 F-1 / F-3 / F-4 / F-5。
- 相关代码落点：`internal/guard/opaque.go`、`internal/guard/destructive.go`、`internal/api/http/ws_perm.go`。
