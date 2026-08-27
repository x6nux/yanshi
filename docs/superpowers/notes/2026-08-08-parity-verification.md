# QwenPaw 对标交付的实测验收（2026-08-08）

这份文档回答一个与「测试是否全绿」不同的问题：**把 yanshi 当成真软件跑起来，这批能力真的在工作吗？**

背景：`2026-08-08-yanshi-vs-qwenpaw-matrix.md` 里的能力在同日实现完毕，`go test ./...` 全绿。
本轮不写新功能，只做实测 —— 每条能力先设计一个「如果它坏了就会失败」的真实运行，跑它、看输出，
发现缺陷当场修，修完再跑一次并保留修前/修后证据。

**结论先行：全绿的测试套件掩盖了 11 处真缺陷，其中 1 处是可被利用的安全绕过。**

分域记录：
- [模型运行时](2026-08-08-verify-model-runtime.md)（M1–M10 / C6 / C8 / C9）
- [工具与循环护栏](2026-08-08-verify-tools-loopguards.md)（C1–C5 / T1–T4 / T12 / L1–L7 / S12）
- [安全与审批](2026-08-08-verify-security.md)（S2–S11 / C11 / O10）
- [VCS 与运维](2026-08-08-verify-vcs-ops.md)（V1–V8 / O1 O6 O11 / T5 T8–T11 / C12–C14）

---

## 一、本轮实测发现的真缺陷（11 处）

这一节是本轮最有价值的产出。**每一条都在单元测试全绿的前提下存在**，且每一条都需要"把它跑起来"
才能看见。按严重度排序。

### 1. `sudo rm -rf /` 判 Allow —— 灾难删除门被 18 种前缀执行器整体绕过（S10）

`rm -rf /` 是结构性 HardDeny，任何模式都拦。`sudo rm -rf /` 的 verdict 是 **0（None）**：
不弹窗、不记录、直接放行。`timeout` / `nohup` / `env` / `xargs` / `chroot` / `su` 等 18 种
「尾部 argv 就是另一条命令」的前缀执行器全部同样绕过。

**档位被反过来了：权限更高的那个形态才是漏过去的。**

它为什么能活到实测：`guard.AutoApprovalPrompt` 的风险类别里明确写了「提权」，读起来像是被覆盖了 ——
但灾难删除门（`checkDestructive`）在**任何模式判定之前**短路，那段提示词对它完全不可达。
**风险被识别了，防线只写在给模型看的文字里。**

修复在 `internal/guard/prefixrunner.go`：识别前缀执行器并对尾部 argv 递归重新分类。
27 条攻击形态全中，18 条反向对照全过 —— 关键的反向对照是 `sudo rm -rf ./build` 仍判 None，
一个把所有 sudo 都拒掉的 guard 通不过验收。

现场复核（本文档写作时实跑）：

```
rm -rf /                       -> 2   (Catastrophic)
sudo rm -rf /                  -> 2
timeout 5 sudo rm -rf /        -> 2
env FOO=1 sudo rm -rf /        -> 2
nohup sudo rm -rf /            -> 2
sudo rm -rf ./build            -> 0   (None — 未误伤)
```

### 2. 审计表 / 崩溃报告 / 摘要只脱敏「注册过的」密钥（S6 / C11 / O10）

`secrets.Redactor` 装的是 **yanshi 自己解析出来的**凭据。审计表记的是 **agent 跑的命令**，
里面的 `sk-proj-…` 从未被注册，于是原样落进 SQLite。

同一个 `Redact` 是三个 sink 的脱敏步骤，缺口一样大，而且三处的后果各不相同：
审计表（**落盘不可逆**）、崩溃报告（**操作员被邀请把它邮寄给维护者**）、压缩摘要
（**会成为 pin 住的消息，此后每一轮都重发给 provider**）。

修复在 `internal/secrets/patternredact.go`：一个 chokepoint 修三处，按厂商前缀 + 最小长度锚定。
**刻意不做熵启发式** —— 误报会写进 SQLite 且改不回来。

### 3. `Retry-After` 在 openai 这条路上一直是死的（M1）

桩 server 返回 `429 + Retry-After: 3`，实测两次请求的**真实间隔**是 5.0s / 10.0s ——
盲目指数退避，服务端要的 3 秒被完全忽略。修后 3.01s / 3.01s。

这条只有量测「服务端观察到的到达时刻」才能发现；读代码会看到解析 `Retry-After` 的函数写得很好，
它只是没有被那条路径调用。

### 4. GC 在它自己声称的动机场景下一个 commit 都收不掉（V2）

`internal/vcs/gc.go` 的文件头把动机写成「goal loop 跑几百轮后 SQLite 只涨不落」。实测该场景
`KeepDays` 取遍 -1/0/1/2/14 全部 `deleted commits=0`。

根因：`KeepDays <= 0` 被归一成默认 14 天，**API 里没有任何取值能表达「不要年龄下限」**。
goal loop 的几百个 commit 是在几分钟内写出来的，比任何正数 KeepDays 都年轻。

为什么单元测试全绿：`gc_test.go` 里 **10 处**调用 `backdateAll` 把 created_at 改到 365 天前。
**整套 GC 测试只在「没有任何活进程能产生的历史形状」上跑过。**

修后同一探针 `KeepDays=-1 -> deleted commits=19`；真实空间回收 9051808 → 294912 字节。

### 5. `yanshi init` 在源码树之外必然失败（O4）

`os.ReadFile("config.example.yaml")` —— 而这条命令的全部受众恰恰是没有那个文件的人。
修复：把模板 embed 进二进制（`internal/cli/embedded`），并用
`internal/archtest::TestEmbeddedExampleConfigMatchesRoot` 钉住两份字节相同。

### 6. `yanshi serve` 从不认领 lockfile（O3 / O6 的前置）

`serve` 的帮助文本写着「其他 yanshi 调用会发现它」，但发现走 lockfile，而只有 TUI 的内嵌后端
写过 lockfile。实测：`serve` 在跑、`/healthz` 与 `/readyz` 都 200，`daemon status` 却报
「no daemon lockfile」，`schedule` 报「no running daemon」。

**连带后果**：同项目的 TUI 发现不了这个 daemon，会**另起一个后端**。

### 7. `App` 不暴露 C1 automation manager（O6）

于是 `yanshi schedule` 永远回「本 daemon 未装配调度器」—— 一句关于接线缺口的真话，
读起来却像一个合法的构建变体。

### 8. `provider add` 在非 TTY 下对可选字段强行交互（O5）

`ProviderAddOptions.In` 的文档写着「nil 关闭提示」，`interactive()` 也确实在读它 ——
但调用点无条件传 `os.Stdin`，那条路径不可达。实测：全参数、非 TTY 的
`provider add` 停在 `base URL (blank for the provider default):` 然后 EOF 死，无法脚本化。

### 9. `ast_search` 把「没找到」当成工具故障（T2）

`ast-grep` 遵循 grep 退出码约定（1 = 没找到），而 `runAstSearch` 把任何非零退出当失败。
于是**每一次零匹配的结构化查询**都返回错误 —— 而「这个代码库里有没有吞掉 error 的分支」
回答「没有」正是这个工具最常见的**成功**结局。模型读到错误会重试或退回 `fs_search`，
**能力恰好在它工作正常的时候静默退化成它本要取代的东西。**

### 10. C6 的溢出报错把消息条数当 token 数报（C6）

恢复逻辑本身正确，但错误文本写着 `26 → 16 tokens`，那两个数其实是消息条数。
一个用来判断「输入真的变小了吗」的数字，量的是错误的量纲。

### 11. 缓存目录单调增长（V8 / lockfile）

用户 cache 目录下 **27968** 个 0 字节 `.lock` 文件、**1475** 个 lockfile，从不回收。
修复：`App.Shutdown` 释放 flock 描述符并回收无人持有的文件；陈旧 lockfile 在下次 Acquire 时清理。

### 附：C12 自动记忆注入写了但没接上

`internal/tools::DistillMemories` 的编排层同病（`grep` 除自身文件外零命中）。C12 已修，
C13 的存储层实测可靠但**扳机未接**，如实记为 NOTRUN 而非通过。

---

## 二、验证矩阵

结论三档：**OK** = 真实运行验证通过；**FIXED** = 实测发现缺陷并已修复；**NOTRUN** = 未能实测。

| 域 | OK | FIXED | NOTRUN | 小计 |
|----|----|-------|--------|------|
| 模型运行时 | 11 | 2 | 0 | 13 |
| 工具与循环护栏 | 13 | 1 | 1 | 15 |
| 安全与审批 | 8 | 3 | 0 | 11 |
| VCS 与运维 | 15 | 2 | 2 | 19 |
| 主控直接实测（运维 CLI） | 5 | 4 | 0 | 9 |
| **合计** | **52** | **12** | **3** | **67** |

（主控那 9 条与 VCS/运维域不重叠：O2 O3 O4 O5 O7 O9 与 daemon 全链路，其中 4 条是上面的缺陷 5–8。
FIXED 合计 12 而缺陷清单是 11 条，差的一条是 T5 的「未知 kind」，属同一条目下的第二个方向。）

逐条结论见四份分域记录，此处不复制。

---

## 三、无法实测的条目与原因（诚实清单）

| 条目 | 状态 | 原因与退而求其次做了什么 |
|------|------|--------------------------|
| S1 Linux **Landlock** 系统调用路径 | 编译 + vet 过，从未执行 | Docker Desktop 的 linuxkit 内核没有 `CONFIG_SECURITY_LANDLOCK`，`landlock_create_ruleset` 返回 ENOSYS。**bwrap→landlock→degraded 降级链的中间一档从未跑过。** bwrap 那一档已在真实内核 6.12.54 上实测 4 条拦截 |
| S1 **Windows** Job Object 全部 Win32 调用 | 编译 + vet 过，从未执行 | 开发机是 darwin。`GOOS=windows go vet` 会类型检查 `_test.go`，所以 API 用法对着 `x/sys/windows` 校验过了，仅此而已。首次真跑将是 Windows CI leg。**其中「assign 失败时 fail-closed 终止」这条只被 windows-only 测试覆盖，探针确认 darwin 侧无法捕获** |
| S1 **AppContainer**（Windows 文件系统隔离） | 明确不做 | `os/exec` 不暴露 `STARTUPINFOEX`，需要绕过整个 `internal/shell` 的管道/控制台装配自建 spawn 后端。已在 `sandbox_windows.go` 头部写清，并**如实报 `DegradedHostGuard`** 而非谎称 OSIsolated |
| C5 压力折叠 | NOTRUN | 落在 `internal/ctxcompact`，不在该 agent 可写范围；T4（即时降级）那一半已实测 |
| T7 `skill_write` | NOTRUN | 落在 `internal/tools`，与并行 agent 相邻。退而求其次：确认它在 `w3wiring.go` 有装配点（不是孤儿），并实测了它产出必须通过的 `ValidateSkillDir` 门禁 |
| C13 蒸馏的**编排层** | NOTRUN（不是通过） | 存储层已实测可靠；缺的是扳机（每 N 轮？turn 后台？）。这是一个独立工作包，不该顺手塞进本轮 |
| 真实付费 provider 调用 | 不做 | 环境里的 `ANTHROPIC_API_KEY` 对 Anthropic 无效（实测 401 —— 顺带真实验证了 M8：401 被正确短路不重试）。全部 provider 行为改用 loopback 桩 server 验证，**比真调用更可控**：429 + `Retry-After`、上下文超长 400、挂起 60s 这些真调用造不出来 |

---

## 四、方法学：为什么这一轮能找到这些

四个域各自独立总结出了同一个形状，值得记下来：

- **测试表和被测门禁通常是同一个心智模型写的，于是一起比威胁窄。** `sudo` 绕过、审计表脱敏缺口、
  技能扫描的同形字绕过，三条都是这个形状。
- **夹具会把被测物挪出它的真实运行域。** GC 的 10 处 `backdateAll` 让整套测试只在「活进程产生不出来的
  历史形状」上跑；`bigMessage` 用 NUL 字节冒充 token 让整个文件的夹具悄悄值双倍。
- **量纲错误在单元测试里不可见。** C6 把消息条数当 token 报，两个都是 int，断言照样过。
- **「有这个能力」与「这条路径调用它」是两件事。** `Retry-After` 解析器、`interactive()` 判据、
  `RuleSet` 泛化、`DistillMemories` —— 四处都是写好了但调用侧没接。
- **诚实的降级比乐观的声称更有价值。** Windows 后端拒绝报 `OSIsolated`，理由具体：
  `tools.SandboxEnforcing` 会据此把每个 "Access is denied" 读成沙箱拒绝，
  从而对每个普通权限错误弹一次修不了问题的提权审批，训练操作员反射性批准。
