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
- `-tier`：难度分层，决定走哪条实现路径与哪个技能：
  - `auto`（默认）：`RuleTierer` 依据目标文本挑选 tier。
  - `t0`..`t4`：强制指定——t0 快速修复、t1 标准、t2 设计、t3 团队、t4 自治。T0–T2 走轻量路径（单编排器 turn + tier 技能）；T3–T4 走完整 plan→implement→evaluate→judge 循环。

分层开发技能 T0–T4 位于 `skills/` 下（见 [skills.md](skills.md)）。

## 看到的输出

每次迭代打印 `[iter N] <phase>: <detail>`；结束时打印 `decision: complete=<bool>, summary=<...>` 并退出（complete=true → 0，否则 1）。真实路径还会把 run record 持久化进 store。
