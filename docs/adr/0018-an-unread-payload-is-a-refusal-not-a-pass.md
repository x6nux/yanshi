# ADR-0018: 读不懂的 payload 是拒绝，不是放行

- 状态：accepted
- 日期：2026-08-29

## 背景（Context）

W-B 把 guard 的 shell 元字符防线从「整条 HardDeny」改成「逐段判定」（[ADR-0004](0004-guard-stateless-and-shell-metachar-hardblock.md) 的 INF1 补充）。逐段判定要求 guard **正确解析 shell**，于是每一轮评审都能找到新的漏读形态：五轮累计 39 条真实绕过，每一条都在真 `/bin/sh` 下确认执行了 `rm -rf /` 或写了 `~/.ssh/authorized_keys`。

**每一轮修完，下一轮还能找到新的。** 这不是评审太严，是问题的形状：「正确解析 shell」是无界的，而用「发现一条补一条」去追它，等于把**未知拼法的默认判决设成放行**。攻击者只需要找到一种还没人写下来的构造。

本批自己已经造好了修这件事的零件 —— W-B-03 引入 `DestructionUnreadable`（穿透层数耗尽 = 拒绝而非放行），并在它的 doc 里写下了正确的原则：**「没读到 payload」不等于「读到了一个安全的 payload」**。问题是这个零件只用在**预算耗尽**这一种情况上。同一时刻，另外三种形态在实测里全部 Allow：

```
python3 -c "import shutil;shutil.rmtree('/')"       Allow
perl -e "unlink '/etc/passwd'"                      Allow
powershell -EncodedCommand <base64 的 rm -rf />     Allow
```

第三条与 W-B-03 的原则**正好相反**：读不懂的预算 = 拒绝，读不懂的 base64 = 放行。

### 被否决的替代方案

**A. 继续加表项。** 给 `nestedCommandUnwrappers` 加 `python -c` / `perl -e` / `-EncodedCommand`。这正是前五轮做过五次的事，而且这次连「加哪些」都答不上来：`deno eval`、`osascript -e`、`bun -e` 明天就会有新的。默认仍然是放行。

**B. 加一张「已知危险构造」的黑名单。** 白名单游戏的镜像，一样无界，而且更糟：黑名单同时承担「识别」与「定性」两件事，一条没命中就既不识别也不定性。

**C. 把「读不懂」判成结构性 HardDeny（yolo 也越不过）。** 这会让 `python3 -c` 在所有模式下永久不可用。不可申诉的拒绝只有在**理由能被陈述**时才站得住，而「这里有段我读不懂的东西」是要求问一次的理由，不是永久封禁的理由。ADR-0017 的《后果》记着同一条教训的另一面。

**D. 真的去读 Python / Perl / base64。** 读 Python 不在本包职责内。base64 是可以解的（15 行），但那是**另一件事**：它把这个形态从「读不懂」搬到「读得懂」，需要它自己的改动和测试，而且只解决 base64 一种。

## 决策（Decision）

**读法遇到自己不建模的构造时，必须能说出「我读不懂」，判决是拒绝而不是放行。**

新增 `DestructionOpaque` 档，位于 `DestructionOutOfScope` 与 `DestructionCatastrophic` 之间，映射到 **Prompt**：

- **不是静默放行** —— 这是这条决策的全部要点。
- **不是结构性地板** —— profile 之外，yolo 仍然放行，`auto` 交给模型判，default/allow-edits 弹窗。**代价是过严会变多，一些合法命令（`python3 -c`、`psql -c`）会开始弹窗**，这个代价是被明确接受的：判错的方向是可观测的（用户看见弹窗会抱怨），反方向不可观测（静默绕过不留痕迹）。

判据由 `internal/guard/opaque.go` 的 `opaquePayload` 给出，且**只在没有任何 unwrapper / prefix stripper 认领这条命令时**才咨询它（`classifyLexed` 的 `read` 标志）。这一条让同一个机制同时承担两件事：

1. **解释器规则** —— 一个本包不认识的程序，带着一个 code flag（`-c` / `-e` / `--command` / `--eval` / `--execute`）和一个「长得像程序而不像选项值」的操作数（`looksLikeCode`）。
2. **wrapper 表的兜底** —— `bash +o posix -c "rm -rf /"` 用了一种 `unwrapShellCommand` 不认的 shell 选项拼法，于是没有任何读法拿到 payload；这个形态从「静默 Allow」变成「弹窗」。**这一半才是「未来任何没人想到的新拼法默认 fail-closed」那句话的兑现处。**

`nonInterpreterPrograms` 是配套的**过严缓解表**（`grep -e` 是 pattern、`git -c` 是配置项、`docker -e` 是环境变量）。它的**失败方向是安全的**：漏一个条目 = 多一次弹窗；错放一个进去 = 静默放行 —— 所以成员判据是「这个 flag 的操作数按文档就不是程序」，不是「这个程序可信」。

## 后果（Consequences）

> 含**不可违反的约束**（加粗）。

- 未知构造的默认判决从 Allow 变成 Prompt。这是**本决策唯一想要的行为改变**，其余都是它的推论。
- `DestructionOpaque` 的排序位置是承重的：它必须**高于 OutOfScope、低于 Catastrophic**。放到 Catastrophic 之上会让 `python3 -c "…" && rm -rf /` 从结构性地板悄悄降级成弹窗。看守是 `internal/guard::TestOpaqueRanksBetweenOutOfScopeAndCatastrophic`。
- **不可违反的约束：`DestructionOpaque` 必须是 Prompt，不得升级为结构性 HardDeny。** 升级会让每一次 `python3 -c` 在 default 模式下永久不可用、在 yolo 下不可申诉，这正是替代方案 C 被否决的理由。看守是 `internal/guard::TestOpaqueIsNotTheStructuralFloor` —— 它断言的是 `Verdict == Prompt && Promptable`，而不是「不是 Allow」，因为后者两种档位都满足。

  ⚠️ **[ADR-0019](0019-the-tier-follows-the-payload-not-the-program-name.md) 收窄了这条约束的作用域。** 它继续成立于本条举证时用的那个形态 —— **payload 读不出灾难性读法**（`python3 -c "print(1)"`）。当 payload 能被读成 shell 命令且判为 Catastrophic 时，档位由那个读法决定而不是由这一档决定：本条写这句话时只想着 `python3 -c`，没想到 `fish -c "rm -rf /"` 会因为 `fish` 不在 `posixShellPrograms` 里而落到同一档，于是「换一个 guard 没听说过的 shell」成了通用的 yolo 绕过。
- **不可违反的约束：`opaquePayload` 只在没有读法认领该命令时生效。** 去掉这个条件会让 `bash -c "npm test"` 这类已经被正确读出来的命令再挨一次 opaque 判定，把一个兜底变成一道普遍的过严。
- **不可违反的约束：这个机制不得退化成「已知危险构造的黑名单」。** 表里的条目描述的是**形状**（「这个 flag 的操作数是另一种语言的程序」），不是危险性；判决是「没人读过」，不是「这很危险」。
- `nonInterpreterPrograms` 的失败方向必须保持「漏一条 = 多一次弹窗」。语料里 `grep -e "foo bar"` 与 `git -c user.name=x …` 两行是它活着的证明；`tail -c 100` / `cut -c 1-5` / `gcc -c` 三行是 `looksLikeCode` 活着的证明（它们不靠缓解表就通过）。
- 本决策**不解决**「读得懂 payload」这件事。`powershell -EncodedCommand` 现在是弹窗而不是灾难档；把它解出来是替代方案 D，是另一个工作包。
- **`ssh <host> CMD` 的判决在本批一并决定：判。** 理由记在 `internal/guard::remoteShellRunners` 的 doc 上 —— catastrophic 档的定义（系统根 / home / 整个盘）不含任何「本地」语义，`rm -rf /` 在哪台机器上都是灾难；而 out-of-scope 档确实是 workdir 相对的，拿它去判远端路径会多问一次，方向安全。读法取**拼接后的 argv**（ssh 自己就是这么把剩余操作数交给远端 shell 的），否则 `ssh h rm -rf /` 被拦而 `ssh h "rm -rf /"` 放行 —— 又一次「包得更严实的那个反而过」。

## 关联

- 来源：W-B 第二批复评 `rereview-b2.md` 的三条 Blocking/Major（解释器 payload、`-EncodedCommand`、三层 `bash -c`）与其上的裁定。
- 前置：W-B-03（`DestructionUnreadable`，`internal/guard/ansic.go::maxUnwrapDepth` 的 doc）—— 本条是把它那条原则从「预算耗尽」推广到「读不懂」。
- 相关代码落点：`internal/guard/opaque.go`、`internal/guard/destructive.go`（`Destruction` 档位与 `classifyLexed` 的 `read` 标志）、`internal/guard/guard.go`（`checkDestructive`）。
- 与 [ADR-0016](0016-two-shell-readers-one-word-layer.md) 的分工：0016 说「哪个 reader 读这条命令」，本条说「reader 读不动的时候怎么办」。
- 与 [ADR-0017](0017-expansion-is-a-second-reading-not-a-rewrite.md) 的分工：0017 是**多加一种读法**，本条是**承认读法有尽头**。两者的失败方向判断一致：多问一次可观测，静默放行不可观测。
