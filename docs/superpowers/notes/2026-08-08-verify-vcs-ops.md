# 实测记录：VCS 与运维域（V1–V8 / O1 O6 O11 / T5 T8 T9 T10 T11 / C12 C13 C14）

日期：2026-08-08。方法：**先跑，再读代码**。每条能力先设计一个「如果它坏了就会失败」的真实运行，
跑它、看输出；发现缺陷就修，修完再跑一次并贴出修前/修后。

结论概览：**3 处真缺陷（已修）**、1 处我自己的修复在首版里犯了同类错误（也已修）、
1 条初判为缺陷经复核**不是**缺陷（记录在案，避免下一轮重复误报）。

---

## 一、发现并修复的缺陷

### 缺陷 1（V2）：GC 对「本会话刚产生的历史」完全无效 —— 年龄下限没有关闭开关

`internal/vcs/gc.go` 的文件头把动机写成「goal loop 跑几百轮后 SQLite 只涨不落」。实测这个动机
场景 GC **一个 commit 都收不掉**。

**修前实测**（20 个 commit，reset 让它们全部脱离 main 可达性，逐个 KeepDays 取值扫）：

```
KeepDays= -1 -> deleted commits=0 blobs=0 kept=22
KeepDays=  0 -> deleted commits=0 blobs=0 kept=22
KeepDays=  1 -> deleted commits=0 blobs=0 kept=22
KeepDays=  2 -> deleted commits=0 blobs=0 kept=22
KeepDays= 14 -> deleted commits=0 blobs=0 kept=22
```

**根因**：`runGCLocked` 把 `KeepDays <= 0` 归一成默认 14 天，于是 API 里**没有任何取值**能表达
「不要年龄下限」。goal loop 几百个 commit 是在**几分钟内**写出来的，全部比任何正数 KeepDays 年轻，
于是全部被年龄保留 —— 想收掉它们只能等十四天。

**为什么单元测试全绿**：`internal/vcs/gc_test.go` 里 **10 处**调用 `backdateAll`，把 created_at
统一改到 365 天前。整套 GC 测试只在「没有任何活进程能产生的历史形状」上测过。

**修复**：新增 `internal/vcs::KeepDaysNone`（值 -1）关闭年龄下限；`retainedByPolicy` 只在
`KeepDays > 0` 时计算 cutoff（负数会把 cutoff 推到**未来**，无条件保留一切）；`0` 仍然表示
「用默认」，所以未设置该字段的调用方行为不变。`internal/tools/vcs.go` 的 `keep_days` 参数描述
同步说明 -1 的语义。

**修后实测**（同一探针）：

```
KeepDays= -1 -> deleted commits=19 blobs=19 kept=3
KeepDays=  0 -> deleted commits=0 blobs=0 kept=22     ← 未设置 = 默认，行为未变
```

真实空间回收（120 turn × 40KiB，vacuum + WAL checkpoint）：

```
gc: deleted 119 commits / 119 blobs, freed 4474400 bytes of payload;
db 9051808 -> 294912 bytes (vacuumed=true)
```

DB 文件从 8.6 MiB 降到 288 KiB，且 GC 之后**只被 seam 引用的 blob 仍在**、对该 seam 的 revert
仍然把原文写回工作副本（`internal/vcs::TestLiveRun_GCReclaimsSpaceWithoutBreakingASeamOnlyRollback`）。

留下的回归测试 `internal/vcs::TestLiveRun_GCCollectsHistoryProducedInThisSession` **刻意不 backdate**
—— 那正是原有 10 个测试看不见问题的原因。

---

### 缺陷 2（运维）：用户缓存目录无界增长（实测 27968 + 1475 个文件）

**修前实测**：一次 `go test ./internal/vcs` 往真实用户缓存目录里新增 **313 个** 0 字节 .lock 文件：

```
BEFORE vcs-locks=   28037 run=    1475
AFTER  vcs-locks=   28350 run=    1475
delta vcs-locks=313 run=0
```

两处泄漏来源不同，因此修法也不同：

**(a) `~/Library/Caches/yanshi/vcs-locks`** —— 每个 repo id 一个 flock 文件，`crossproc.go` 原文
明确写着「lock 文件永不 unlink」（理由是正确的：给活着的持有者 unlink 会造出两个 lock domain）。
修法是在**能证明没人持有**时才回收：

- `internal/vcs::VCS.Close` —— 关闭本实例开的描述符，并只删除**它自己刚锁上**且 inode 未变的文件
  （`removeIfSameInode`）。别的进程持有 → 拿不到锁 → 文件留着。
- `internal/vcs::sweepStaleLockFiles` —— 处理 `Close` 永远够不着的那批：**已退出进程**留下的文件
  （包括这次修复之前所有版本留下的 28k 个）。加 24 小时年龄下限，且 `lockRepo` 每次获取都
  `touchLockFile` 刷新 mtime，活跃 lane 因此持续自证存活。
- 新增 `tryLockFileExclusive`（unix `LOCK_NB` / windows `LOCKFILE_FAIL_IMMEDIATELY`）作为回收探针
  —— 写路径必须阻塞，清扫**不能**阻塞。
- 接线：`internal/bootstrap::App.Shutdown` 与 `cmd/yanshi::runVCSMCP`。

**(b) `~/Library/Caches/yanshi/run`** —— 每个项目根一个 lockfile。按根回收永远够不着它们：
一个 lockfile 只会被「再次打开同一项目」的进程重访，而项目目录已经被删掉了。实测这 1475 个里
**952 个**超过 24 小时，其 `root` 字段指向的目录**全部已不存在**。新增
`internal/lockfile::SweepStale`，由 `Acquire` 触发；只删「owner 进程已死 **且** 项目根已消失」的，
解析不了的文件一律留着（那个目录是共享的）。

**修后实测（真实二进制，不是测试）**：

```
# 单项目重复运行
BEFORE=   28719
AFTER 5 real exec runs =    28719, delta=0

# 12 个不同项目各跑一次
BEFORE=   28721
AFTER 12 DISTINCT projects =    28721, delta=0

# 最终复核：3 个全新项目
BEFORE vcs-locks=   29357 run=     602
AFTER  vcs-locks=   29357 run=     602  (delta 0 / 0)
```

清扫在真实存量上的效果：把真实的 28721 个文件目录拷贝出来做旧后跑一次，
`sweep removed 28721 in 1.863182417s; 0 remain`；`run/` 目录直接调用 `SweepStale` 实测
`removed 890 in 248.631792ms`（1476 → 586）。**用真实二进制**验证增量回收：预埋 3 个
「PID 已死 + root 已消失 + 做旧」的 lockfile，跑一次 `yanshi exec` 后 `remaining probe files: 0`。

**反向探针**（先提交再变异，不用 git checkout）：把 `App.Shutdown` 里的 `VCS.Close()` 用环境变量
关掉重编译，同样 5 次 exec → `delta=2`，泄漏立刻回来。

### 缺陷 2b：我自己的修复第一版犯了本仓的招牌错误 —— 后台 goroutine 从不运行

两处清扫我最初都写成 `go SweepStale(...)`（理由看着很充分：ReadDir 一个上万条目的目录不该挡在
启动路径上）。**实测它一次都没跑过**：`yanshi exec` 认领 lockfile、跑完一个 turn 就退出，进程在
goroutine 被调度之前就没了 —— 真实 exec 之后目录**分毫未动**，而直接调用 `SweepStale` 能删掉 890 个。

改成同步执行。代价实测有界：1476 个文件耗时 249ms、28721 个文件耗时 1.86s，每进程一次，且它扫的
正是它刚删掉的那批，第二次几乎为零。回归测试 `internal/lockfile::TestAcquire_SweepsSynchronouslySoAShortLivedProcessActuallyReclaims`
断言「`Acquire` **返回时**回收已完成」—— 这是短命进程唯一能依赖的表述，且不需要 sleep。
变异探针：改回 `go SweepStale(...)`，该测试立刻红（`6 lockfiles remain`），改回来即绿。

---

### 缺陷 3（C12）：自动记忆注入在生产路径上零调用点

`internal/tools/memory_autorecall.go` 的文件头写得很清楚：memory_search「能用，但几乎没人调」，
所以检索要**自动**发生。实测这个自动从未发生。

**修前实测**（真实 store + 真实 orchestrator + 真实 WebSocket turn，FakeModel 开 Echo，
回显的就是模型真正收到的 prompt）：

```
stored memory cf34...: "The deployment runbook lives in ops/deploy-runbook.md and requires the ACME vpn."
AutoRecall() called directly returns 406 chars      ← 检索本身没问题
model received (641 chars): You are yanshi's orchestrator. ... where is the deployme
                                                     ← prompt 里没有那条记忆
```

`grep -rn "AutoRecall" internal cmd` 除了它自己的文件和一行 doc 注释引用，**零命中**。

**修复**：新增 `internal/api/http/ws_recall.go::withRecalledMemories`，在 `runUserTurn` 里
skill 前缀展开 + attachment 之后、消息进 `cs.history` 之前调用（这样模型看到的、transcript 里的、
持久化的是同一份文本）。作用域刻意留零 filter 而非收窄到当前 session —— 记忆的价值绝大多数是跨会话的，
按 session 收窄会把「记忆」降级成「缓存」；理由写在函数 doc 注释里。`recordingSuppressed()` 的连接不注入。

**修后实测**：

```
model received (1049 chars)     ← 641 → 1049，记忆进来了
--- PASS: TestLiveRun_C12StoredMemoryReachesTheModelWithoutBeingAsked
--- PASS: TestLiveRun_C12IrrelevantMemoriesAreNotInjected
```

第二条同样重要：无关记忆**没有**被塞进来 —— 每轮都注入等于训练模型跳过整个注入块，反而废掉真正
该命中的那一轮。

**反向探针**：删掉调用点重编译 → 测试红；恢复 → 绿。

---

## 二、复核后判定「不是缺陷」的一条

`internal/skills/requires.go` 写着未知 requirement kind「**REJECTED** rather than ignored」，
我的初版测试在 `Loader.Load()` 上断言它被拒，实测没被拒，一度记为缺陷。**复核后撤回**：拒绝发生在
**安装时**（`ValidateSkillDir`，git 与 HTTP 两条安装路径以及 WS validate verb 都会跑它），实测
`ValidateSkillDir -> skills: requires[0]: no recognized requirement key; the only supported form is `- bin: <program>``。
`Load` 故意宽容，因为它的契约是「一个坏 skill 不能让整次加载失败」，为一个 frontmatter 拼写错误
把已装好的 skill 从注册表里丢掉等于一次静默卸载。

测试已改写成断言**这一对**性质（`internal/cli/tui::TestLiveRun_T5UnknownRequirementKindIsRefusedAtInstallNotAtLoad`），
并在 doc 注释里记下这次误判，免得下一轮再报一次。

---

## 三、逐条验证矩阵

标注：**OK** = 实测通过；**FIXED** = 实测发现缺陷已修；**NOTRUN** = 无法实测（附原因与替代手段）。

### VCS

| ID | 结论 | 实测内容 |
|----|------|---------|
| V1 回滚 dry-run | **OK** | 建 repo、两轮 turn、外部改一个文件，预览后真执行，**逐路径**比对预览声明的 op 与前后行数 vs 磁盘遍历结果；反向也查（预览没提到却变了 = 失败）。5 处变更全部吻合，dirty 恰为那个外部改动的文件，终态等于 turn1 快照 |
| V2 GC | **FIXED** | 见缺陷 1。DB 9051808 → 294912 字节；只被 seam 引用的 blob 存活且 revert 成功 |
| V3 时间线 | **OK** | 整机 bootstrap + 真实 WS 跑 3 轮，时间线逐轮显示**真实提问**："rename the config loader" / "add a retry to the http client" / "delete the unused parser"，序号一一对应，无占位符 |
| V4 选择性恢复 | **OK** | 5 个文件全部改过，只恢复 `broken/*.go` 两个；**全树快照比对**证明其余 3 个一字未动，且 head 没有回退 |
| V5 冻结检测 | **OK** | 预览之后、apply 之前用 `os.WriteFile` 从外部改文件（VCS 完全不知情）→ `ErrExternalMutation` 且错误里**点名** `src/app.go`；并复核外部内容**未被覆盖**（拒绝发生在写之前） |
| V6 symlink 逃逸 | **OK** | 真造 `docs -> /外部目录` 软链，外部放一个已知内容的文件；恢复被拒，且**读回外部文件**验证字节完全未变 |
| V7 孤儿 worktree | **OK** | 真起 `/bin/sh -c "sleep 60"` 子进程持有 worktree：活着时扫描 0 个孤儿；`Kill` + `Wait` 之后扫描恰好认出那一个；cleanup 真删目录且 `LogWorktree` 历史仍在 |
| V8 跨进程锁 | **OK** | 已有 `TestV8_RealSubprocessesDoNotLoseCommits` 起**真实子进程**并发写同一 DB（3 进程 × 15 次），`TestV8_LockIsReleasedWhenHolderProcessDies` 杀掉持锁进程验证锁被回收。全部实跑通过 |
| 缓存目录泄漏 | **FIXED** | 见缺陷 2 / 2b |

### 运维

| ID | 结论 | 实测内容 |
|----|------|---------|
| O1 日志轮转 | **OK** | **真写 4 MiB**（18642 行真实结构化日志）过 1 MiB 阈值：`.1`/`.2` 真的出现、`.3` 不存在、总占用 2097450 字节在界内、每代都以完整行结尾。并发：12 goroutine × 400 行跨 5 代**一行不丢**（4800/4800）、0 处截断。降 max_backups 5→1 后多余代真的被清掉（`[yanshi.log yanshi.log.1]`） |
| O6 schedule 写路径 | **OK** | 整机 daemon + 真 lockfile + 真 `cli.RunSchedule`。**每个动作之后都回 manager 重读状态**：pause → `active=false`；resume → `active=true` 且 next 重算；**run-now 真的产生了一次执行**（history 0→1，`run-290cd384... status="queued" task="wt-c7m7rprapa"`）且**不扰动** next fire time；delete 后 list 为空且 `Read` 报错。另测 4 个变更动作缺 id 一律被拒、目标 automation 未受影响 |
| O11 stdio ACP | **OK** | 测试内 `go build` 出**真二进制**，真喂协议帧到 stdin：initialize / session/new / session/prompt 三个请求各得回复，`session/update` 通知真的产生，**stdout 每一行都是合法 JSON-RPC 2.0**（boot 诊断全在 stderr，且断言 stderr 非空——否则「stdout 干净」可能是「什么都没打」）。畸形输入用例：喂一行非 JSON + 一个未知方法，其后的请求**仍被正常应答**（ids `[1 2 3]`） |

### 技能 / ACP / MCP

| ID | 结论 | 实测内容 |
|----|------|---------|
| T5 依赖声明 | **OK** | 真写带 `requires: - bin: <不存在的程序>` 的 SKILL.md → 真 loader → 真 proto → 真 TUI palette：条目 `disabled=true`、help 里**点名**缺失程序、`SkillInfo.Missing` 非空（远程 TUI 也看得到）。对照组（无依赖的技能）未被误判 |
| T5 未知 kind | **OK** | 见第二节。安装门禁拒绝、加载路径宽容，两个方向都钉住 |
| T8 HTTP 装包 | **OK** | 真 TLS server + **生产 Fetcher**（传 nil）。正向：真装成功且文件落地。**6 种恶意包全部被拒**：tar.gz `../` / tar.gz 绝对路径 / tar.gz symlink 条目 / zip `../` / zip symlink 条目 / 超单条 16 MiB 上限。每例都**读回目标目录外的 canary 文件**验证字节未变、且外部目录没多出任何条目。另测明文 http 被拒且目标目录一个条目都没留下 |
| T9 ACP 委派 | **OK** | 环境里 `codex` 与 `claudecode` **都在 PATH 上**，直接跑了带 build tag 的真实 e2e：两个真 CLI 各自被 spawn、真的写出 hello.txt、`stopReason=end_turn`。`TestE2E_RealClaudeCode` 18.96s / `TestE2E_RealCodex` 79.44s 全部 PASS |
| T10 MCP OAuth | **OK** | 本地起**严格的** OAuth 桩 server（自己校验 PKCE、单次码、轮换 refresh），跑完 authorization_code + PKCE + refresh：S256 challenge 正确、有效期内走缓存（0 次刷新）、过期后刷新、**轮换后的 refresh token 真的被持久化**（否则下下次调用静默登出）、第二次刷新仍成功。负向对照：错误 verifier 被拒（证明桩真的在校验）、重放授权码被拒。并发：8 个 goroutine 同时取 token 只花掉 **1 次** refresh，之后存储的 token 仍然可用 |
| T11 斜杠命令动态下发 | **OK** | 真写 SKILL.md 到磁盘 → 真 loader → 真 palette：技能出现在 `/skill run` 列表、携带自己的 description、`kind=cmdKindSkillRun`（补全会插入完整命令行）、前缀过滤有效 |
| T7 模型自创技能 | **NOTRUN** | 落在 `internal/tools`（`skill_write`），不在我的可写范围，且与其他 agent 正在改的文件相邻。**退而求其次**：确认 `BuildSkillWriteTool` 在 `internal/bootstrap/w3wiring.go` 有装配点（不是孤儿），并在 T8 里实测了它写出的技能包必须通过的那道 `ValidateSkillDir` 门禁 |

### 记忆

| ID | 结论 | 实测内容 |
|----|------|---------|
| C12 自动注入 | **FIXED** | 见缺陷 3。修后 prompt 641 → 1049 字符，无关记忆不进来 |
| C13 蒸馏（存储层） | **OK** | 真库真写：3 条重复记忆合并后默认检索只见合并行；**审计视图 4 行原文一字不少**；lineage 双向可解析（合并行 cites 三个 id，每个原件 `superseded_by` 指回）。失败保全：空内容 / 单源 / 源不存在 三种拒绝之后，**逐条复核**原件仍 `superseded_by=""`（仅「还在」不够——被标记 superseded 就等于对所有默认读不可见）；对已合并行二次蒸馏被拒 |
| C13 蒸馏（编排层） | **NOTRUN**（并非通过） | `internal/tools::DistillMemories` 与 C12 同病：`grep -rn "DistillMemories" .` 除自身文件外**零命中**，没有工具注册、没有调用点。它落在 `internal/tools`（不在我的可写范围），且修法需要决定触发时机（每 N 轮？turn 结束后台？）—— 是一个独立工作包，不该顺手塞进本轮。**存储层已实测可靠**，缺的是扳机 |
| C14 检索维度 | **OK** | 三种 scope 各写记忆，**四个方向全测**：无 scope 搜索看得到全部 4 条、session A 只看到自己那条、session B 只看到自己那条、agent 维度只看到子代理那条、不存在的 session 返回空。scoped recall 与 scoped search 结论一致。并验证蒸馏**保留 scope**（合并行仍带 sess-alpha，否则它对自己合并的那个 session 就不可见了），且 alpha 的蒸馏没有动 beta |

---

## 四、留下的可重跑测试

判据是「CI 上无外部依赖能重跑吗」。以下全部满足（T9 除外，它本来就在 build tag 后面）。

| 文件 | 覆盖 |
|------|------|
| `internal/vcs/liverun_test.go` | V1 预览准确性（逐路径 op + 行数，双向）、V4 全树比对、V2 GC 真实空间回收 + seam-only blob 存活、V2 同会话 GC 回归、V5 外部写检测、V6 symlink 逃逸（读回外部文件）、V7 真子进程孤儿扫描、锁目录有界性 + 活持有者不被回收 + 死进程遗留回收 |
| `internal/lockfile/sweep_test.go` | 清扫的四类判定（含「解析不了就别删」）、`Acquire` **同步**回收回归。附带修掉本包测试污染真实缓存目录的问题（`isolateCacheDir` 按平台重定向 `os.UserCacheDir`） |
| `internal/cli/liverun_v3_test.go` | V3 整机三轮真实对话 → 时间线标注真实提问 |
| `internal/cli/liverun_o1_test.go` | O1 真写 4 MiB 触发轮转、12 并发写者跨代不丢行、降 MaxBackups 真清理 |
| `internal/cli/liverun_c12_test.go` | C12 记忆真的进 prompt（Echo 模型看到什么就断言什么）、无关记忆不进 |
| `internal/cli/liverun_t8_test.go` | T8 真 TLS + 生产 Fetcher，正向安装 + 6 种恶意包 + canary 复核 + 明文被拒 |
| `internal/cli/tui/liverun_t5t11_test.go` | T5 缺依赖标红（含 wire 层 Missing）、T11 技能进 palette、未知 kind 的安装/加载两分 |
| `internal/store/liverun_c1314_test.go` | C13 合并/审计/lineage、失败保全（4 种）、C14 四方向维度 + 蒸馏保 scope |
| `internal/mcp/liverun_t10_test.go` | T10 严格 OAuth 桩：PKCE + 轮换 + 并发单飞 + 两个负向对照 |
| `cmd/yanshi/liverun_o6_test.go` | O6 整机 daemon 六个动作逐一回读状态、缺 id 被拒 |
| `cmd/yanshi/liverun_o11_test.go` | O11 测试内 `go build` 真二进制、真协议帧、stdout 纯净性、畸形输入不中断会话 |

## 五、改动的生产代码

- `internal/vcs/gc.go` —— `KeepDaysNone`；`retainedByPolicy` 仅在 `KeepDays > 0` 时算 cutoff；
  `runGCLocked` 改 `== 0` 才取默认
- `internal/vcs/crossproc.go` —— `VCS.Close`、`removeIfSameInode`、`sweepStaleLockFiles`、
  `maybeSweep`（同步）、`touchLockFile`、`staleLockMaxAge`
- `internal/vcs/flock_unix.go` / `flock_windows.go` —— `tryLockFileExclusive`（非阻塞探针）
- `internal/vcs/vcs.go` —— `sweepOnce sync.Once` 字段
- `internal/lockfile/lockfile.go` —— `SweepStale`、`StaleMaxAge`、`Acquire` 里的 `sweepOnce()`（同步）
- `internal/api/http/ws_recall.go`（新文件）+ `ws.go` 一行调用 —— C12 注入点
- `internal/bootstrap/bootstrap.go` —— `Shutdown` 里 `VCS.Close()`
- `cmd/yanshi/main.go` —— `runVCSMCP` 里 `defer v.Close()`
- `internal/tools/vcs.go` —— `keep_days` 参数描述补充 -1 语义（一行文案）

## 六、门禁与全量

```
go build ./...                                    通过
go vet（本域全部包）                                通过
go test ./internal/archtest ./internal/bootstrap   ok / ok
go test（vcs / cli / cli/tui / lockfile / mcp / store / cmd/yanshi / acp / acpserver / skills）  全 ok
go test -count=1 ./internal/api/http               ok（40.6s，因 C12 改动复跑）
```

## 七、留给下一轮的两件事（本轮**没有**做，不要当成已完成）

1. **C13 编排层缺扳机**：`internal/tools::DistillMemories` 零调用点。存储层已实测可靠，需要的是
   决定触发时机并注册工具/接线。落在 `internal/tools`，是独立工作包。
2. **T7 未实测**：`skill_write` 在我的可写范围之外，只确认了装配点存在与安装门禁会拦它的产物。
