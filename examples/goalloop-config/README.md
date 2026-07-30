# goalloop-config

`yanshi goal --fake-model` 的两轮演示 + goalloop 相关配置片段。

## 跑（两轮 fake-model 演示）

```sh
bash run.sh
```

或手动：

```sh
go build -o yanshi ./cmd/yanshi
./yanshi goal --fake-model --max-iters 2 -goal "add a smoke test"
```

`--fake-model` 接入零依赖的 FakePlanner + FakeImplementer（失败一次）+ counterEvaluator（失败一次后通过）+ AggregateJudge，跑两轮（第一次失败、第二次通过）即终止。详见 [../../docs/user-guide/goalloop.md](../../docs/user-guide/goalloop.md)。

## 配置片段

goalloop 的预算与 tier 由命令行 flag 控制（不进 config.yaml），但真实路径依赖 `config.yaml` 里的 provider / VCS 配置。关键 flag：

| flag | 默认 | 说明 |
|---|---|---|
| `-max-iters` | 5 | `MaxIterations` 预算，耗尽即停 |
| `-tier` | `auto` | `auto`（RuleTierer 挑）或 `t0`..`t4`（强制） |
| `-agent` | `claudecode` | 真实路径实现用的外部 agent CLI |
| `-workdir` | `.` | 实现的工作目录 |

真实路径（不带 `--fake-model`）会 bootstrap 出 LLM model + 编排器 + store；当 `vcs` 已配置时，实现跑在一条会合并回 main 的新 worktree 分支上（见 [../../docs/user-guide/autovcs.md](../../docs/user-guide/autovcs.md)）。
