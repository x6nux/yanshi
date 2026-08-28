# 目标循环（`yanshi goal`）

`yanshi goal` 是 yanshi 的自驱动模式：给一个目标，它按 **plan → implement → evaluate → judge** 循环迭代，直到完成或耗尽预算。

## 一个零依赖两轮演示

`--fake-model` 接入零依赖的 FakePlanner + FakeImplementer（失败一次）+ counterEvaluator（失败一次后通过）+ AggregateJudge，跑两轮就终止：

```sh
./yanshi goal --fake-model --max-iters 2 -goal "add a smoke test"
```

这不需要任何 API key 或外部 CLI，适合先验证循环本身。

## 真实路径

不带 `--fake-model` 时走真实路径：bootstrap 出 LLM model + 编排器 + store；`LLMPlanner` 规划，`ACPImplementer` 拉起外部 agent CLI（`codex`/`claudecode`）实现，评估器（Test/Intent/Quality）+ `AggregateJudge` 判定。当 VCS 可用时，实现跑在一条会合并回 main 的新 worktree 分支上（见 [autovcs.md](autovcs.md)）。

## 预算与 tier

- `-max-iters`（默认 5）：`MaxIterations` 预算，耗尽即停。
- `-max-tokens`（默认 0 = 不限）：整轮目标的 token 预算，跨所有 LLM 调用累计。
- `-tier`：难度分层，决定走哪条实现路径与哪个技能：
  - `auto`（默认）：`RuleTierer` 依据目标文本挑选 tier。
  - `t0`..`t4`：强制指定——t0 快速修复、t1 标准、t2 设计、t3 团队、t4 自治。T0–T2 走轻量路径（单编排器 turn + tier 技能）；T3–T4 走完整 plan→implement→evaluate→judge 循环。

分层开发技能 T0–T4 位于 `skills/` 下（见 [skills.md](skills.md)）。

## 中断之后：从原处续跑

真实路径（T3–T4）**每跑完一轮就把进度写进 store**：目标文本、两个预算、已完成轮数、已花费 token。所以进程被 Ctrl-C、被 kill、机器断电之后，**再跑同一条目标就从下一轮继续，预算接着上次扣**，而不是从第 1 轮带着满预算重来。

判定「是不是同一条目标」用的是**工作目录 + 目标文本**：

- 同目录 + 同目标文本 → 续跑。
- 同目录 + **换了**目标文本 → 视为新目标，旧进度被覆盖。
- 目标**跑完过**（complete）→ 不续跑，下次从头开始。

落盘发生在每轮结束，所以最多丢掉「正在跑的那一轮」；已经判定完的轮次不会重跑。

### 续跑时预算怎么算

| 情况 | 生效的预算 |
| --- | --- |
| 命令行**敲了** `-max-tokens` / `-max-iters` | 敲的值赢，并成为新的存档值 |
| **没敲**（走配置或默认值） | 存档里的值赢 |

两条规则各有各的道理：没敲的时候让存档赢，是因为崩溃后没人会重敲一遍预算，让 config 默认值回来就等于预算被悄悄清零；敲了的时候让命令行赢，是因为否则你改了数字却看不到任何效果。**逐字段生效**——只敲 `-max-tokens` 时，`-max-iters` 仍然用存档值。任一侧被覆盖时都会打印一行 `State:` 说明，不会静默。

`-max-tokens 0` 是「本次不限 token」的显式指令，续跑时同样生效（不会被存档值顶回去）。

把新上限调到**低于已消耗量**时不报错：这一轮直接按超支处理，立刻结束。

### 放弃存档、从头再来

```sh
./yanshi goal -reset -workdir /path/to/repo
```

清掉该工作目录的续跑点后退出。下次跑同一条目标就是全新的一轮，预算也是满的。

> 注意：同一个工作目录同时跑两个 `yanshi goal` 进程会互相覆盖续跑点（后写的赢）。这里没有加锁——`goal` 不是设计成并发跑的。

## 看到的输出

每次迭代打印 `[iter N] <phase>: <detail>`；结束时打印 `decision: complete=<bool>, summary=<...>` 并退出（complete=true → 0，否则 1）。真实路径还会把 run record 持久化进 store，用 `-history N` 可以回看最近 N 条。

续跑与预算覆盖走 `State:` 这个 phase，例如：

```
[iter 3] State: resuming at iteration 4/6 with 300 tokens already spent
```
