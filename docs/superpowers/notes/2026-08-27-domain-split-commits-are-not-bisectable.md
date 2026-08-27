# 按验证域拆分的那 7 个提交，中间 4 个无法独立编译（2026-08-27）

`a4d18da..54f70a4` 这批把 2026-08-08 实测轮的 439 个文件按**验证域**拆成了 7 个提交
（security / tools+loopguards / model-runtime / vcs-ops / cli+api / tuidbg / governance）。
HEAD 全绿，但**中间提交不是**。实测（每个提交单独 checkout 到干净 worktree）：

| 提交 | `go build` | `go vet`（含测试文件） |
|---|---|---|
| a4d18da tuidbg | ✓ | ✓ |
| 739c6ad security | ✓ | ✗ |
| a55f2d4 tools | ✗ | ✗ |
| 10b64dc llm | ✗ | ✗ |
| 6359a78 vcs | ✗ | ✗ |
| 8492144 cli+api | ✓ | ✓ |
| 54f70a4 governance | ✓ | ✓ |

**`go build` 与 `go vet` 给出不同答案**，两个都要跑：`739c6ad` 的生产代码自洽，是它的
`_test.go` 引用了后一个提交才引入的 `internal/toolreg`。只测 `go build` 会漏掉这一档。

## 根因：域划分与包依赖图正交，且成环

不是"边界划错了、重划就好"。实测重排提交顺序（`cli+api → vcs → tools → security → llm`）
后仍有 4 个不自洽，因为依赖是**双向**的：

- `cli+api` 需要 `ctxcompact.*`（在 llm 域）
- `llm` 需要 `lsp.*` / `acp.SecureSpawned`（在 cli+api 域）、`store.MessageSearchHit`（vcs 域）
- `tools` 需要 `netpolicy.*` / `sandbox.PostStartSandbox`（security 域）
- `security` 需要 `ctxcompact.*`（llm 域）

没有任何拓扑序能解开。唯一能让每个提交自洽的划分是把成环的 5 个域**合并成一个提交**
（已实测通过），代价是安全修复不再能单独 cherry-pick —— 而那正是选择按域拆分的理由。

## 一个真错误，与循环无关

`internal/api/http` 是**一个 Go 包**，拆分时为了贴合 verify-security.md 声明的范围
（`internal/api/http/ws_perm*.go`），把 `ws_perm*.go` / `ws_permtimeout*.go` 摘进了
security 提交，其余留在 cli+api 提交。同一个包被劈到两个提交必然不自洽 ——
`PermissionTimeoutPolicy`、`permDenyNotice`、`newUnattendedState` 全是包内符号。

**教训：提交边界的最小粒度是 Go 包，不是文件。** 文档里的"范围"描述是按主题写的，
照抄成 `git add` 的路径会切开包。

## 决定

保留 7 个域提交。域粒度（安全修复可单独 cherry-pick / revert、review 按域可读）
的价值高于 bisect 便利，且历史已发布，重写要 force push。

**bisect 撞到中间提交时用 `git bisect skip`** —— 编译失败是已知的、有原因的，
不是那个提交引入了坏代码。
