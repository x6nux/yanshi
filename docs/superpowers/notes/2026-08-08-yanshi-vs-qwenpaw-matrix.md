# yanshi × QwenPaw 能力对比矩阵（2026-08-08）

来源：8 个并行 agent 分域读码（yanshi 70K 行 Go / QwenPaw 272K 行 Python），共产出 173 条能力，
本文档为去重合并后的结果。价值判据统一为「补上它，yanshi 作为**编码 agent + 自驱动目标循环 +
单二进制本地部署**是否变强」。平台化能力一律判 low。

工作量按**移植到 Go 单二进制**估：S < 1 天，M ~ 数天，L ~ 数周，XL ~ 数月。

---

## A. 高价值缺口（19 条）

### A1 安全与沙箱

| ID | 能力 | 量 | 差在哪 |
|----|------|----|--------|
| S1 | OS 级子进程沙箱 | XL | `internal/sandbox/factory.go` 的 `Prepare` 是空实现，四个平台文件全返回 `DegradedHostGuard`；QwenPaw 有 Seatbelt / bubblewrap / Landlock / AppContainer 四套可运行后端（8639 行）。yolo 模式下 guard 词法判断是唯一防线 |
| S2 | 敏感凭据路径默认拒 | S | `checkFS` 只按 profile glob 判定，无内置 denylist；`config.example.yaml` 的 coding profile 宽 `FS.Read` 就让 `~/.ssh/id_rsa` 一路 Allow。QwenPaw `governance/policy.py::DEFAULT_SANDBOX_DENY_PATHS` 约 20 条 |
| S3 | 策略文件防 agent 篡改 | S | `-config` 默认相对路径 `config.yaml`，profile 就在 agent 写作用域里 —— 一次 `fs_write` 即可自我扩权。QwenPaw 把 policy 放 `WORKING_DIR/governance/<sha256>/` 之外 |
| S4 | 子进程环境变量凭据剥离 | S | `netpolicy/proxy.go::PrepareEnv` 只删 4 个 proxy 键，API key 原样继承。`guard/autoapproval.go` 提示词里把 env 外泄列为风险，代码侧一个变量没剥 —— 风险已识别、防线只写在提示词里 |
| S5 | 审批超时倒计时 + 无人值守降级 | S | `ws_perm.go` 的 permTracker 无 wall-clock 过期，pending 可无限挂起。`yanshi goal` 长跑无人值守时一次询问永久卡死整个 turn |
| S6 | 权限决策审计落盘 | M | `permctx.go::auditPermission` 已生成结构化记录，但只进 stderr；`store.go` 建表语句里没有 audit 表。yolo/auto 自动批准后查不到「昨晚这条 rm 是谁批的」。**记录已有，只差 sink** |
| S7 | 技能包内容安全扫描 | M | `skills/validate.go` 只查 frontmatter + symlink，`install.go` 从远端装包不看内容。SKILL.md 正文直接进 system prompt —— 装一个带注入指令的技能目前零阻力。QwenPaw 8 类 YAML 签名可整套搬 |

### A2 上下文（Scroll Context 族）

| ID | 能力 | 量 | 差在哪 |
|----|------|----|--------|
| C1 | 被驱逐历史写穿持久化 | L | `ws_compaction.go::persistMessages` 只按 turn 写扁平 user/assistant 两行，**tool_call/tool_result 从不落库**。压缩掉的测试日志、diff、编译错误永久消失。QwenPaw 先写穿再驱逐（写失败就不驱逐） |
| C2 | 模型可调用的历史召回工具 | M | `tools/memory.go` 只查 memories 表（模型显式写入的），没有任何工具读 `store.Messages`。QwenPaw 让模型把驱逐当分页而非遗忘。**C1 的配套** |
| C3 | 上下文内驱逐地图 | L | `ctxcompact/assemble.go::Assemble` 把被摘要消息整体换成不透明 summary，模型不知道自己丢了什么、也没地址要回来。QwenPaw 分层 odometer 索引常驻 prompt |
| C4 | 结构化续接摘要 | M | `summarize.go::summaryInstruction` 是自由文本提示、产出无结构；压两次后「未完成的事」与「已推翻的决策」混在一起且无法回溯来源。QwenPaw 五节固定结构 + `[seq:lo-hi]` 来源指针 + 增量替换语义 |
| C5 | 压力驱动的工具结果折叠 | M | `spillover.go` 只在单条超 64KiB 时折叠，一百条各 10KiB 的 fs_read 照样撑爆窗口。QwenPaw 三级折叠换成恢复指针，保护最近 5 条。**编码 agent 的窗口压力主要就来自这里** |
| C6 | provider 400 溢出反应式恢复 | M | 只有阈值触发的主动压缩；CLAUDE.md 自陈 `takeChunk` 超窗上界与窗口无关，主动压缩必然有漏网，漏了就是整个 turn 失败。QwenPaw「只重试一次、且只在输入真变小时才重试」 |

### A3 循环护栏 / 工具 / 模型 / VCS

| ID | 能力 | 量 | 差在哪 |
|----|------|----|--------|
| L1 | 重复工具调用 / 死循环检测 | M | 全局 grep 无任何 repetition/doom 检测，只有迭代计数；模型反复调同一个 `fs_read` 会一路烧到 MaxIters。QwenPaw 按 `tool_name+args_hash` 滑窗算相似度，分级 modify_prompt → stop |
| T1 | LSP 代码导航工具 | M | `lsp/client.go` 只发 didOpen/didChange、只收 diagnostics，**没有 definition/references/hover**，模型也没有对应工具。找符号引用只能用正则 fs_search，跨文件改签名会漏改。**连接层已有，补的是请求方法与工具外壳** |
| T2 | 结构化 AST 搜索 | S | `fs.go::runSearch` 只有 Go regexp。「找出所有把 err 直接吞掉的分支」正则写不出来 —— 而这正是重构前置调研的主力查询。外挂 ast-grep 二进制即可 |
| T3 | 长耗时工具转后台 + 结果回灌 | L | 普通工具在 `tools/guard.go` 一律 `context.WithTimeout`，超时即失败无残值，模型只能盲目重跑整个测试。QwenPaw 到点先把「已转后台」还给模型，跑完补一条系统通知（刻意不产生 role=tool 消息以免破坏配对） |
| M1 | Retry-After 解析 + 429 冷却 | M | `resilient.go` 把 rate limit 当普通瞬时错误，200ms 起步 5s 封顶；服务端要 30s 时会连打 10 次全废并加剧限流 |
| M2 | 静态上下文窗口目录 | S | `ContextWindow` 需手写，漏填按 256K 算 —— 128K 模型的压缩门永不触发（与 W4 修的 mid-turn 假窗口同类事故）。QwenPaw 内置 30+ 家族，能穿透 `openrouter/anthropic/...` 与 `us.anthropic.*` 前缀 |
| V1 | 回滚 dry-run 预览 | M | `RevertToSeam` 只有执行态，用户按下去之前看不到会丢什么。QwenPaw 强制 `--dry-run` 再 `--confirm` |
| V2 | 历史 GC / 保留策略 | M | 全包无 prune/gc/vacuum，每 turn 一个 commit 只增不减；goal loop 跑几百轮后 SQLite 只涨不落 |

---

## B. 中价值缺口（按域分组）

### B1 安全
| ID | 能力 | 量 | 一句话 |
|----|------|----|--------|
| S8 | 未注册工具运行期 fail-closed | S | 幻影工具防线只在编译期治理测试，运行期陌生工具名仍能被点一下批准 |
| S9 | 批准后的规则泛化 | M | 每个略有差异的命令都要重新弹窗，长周期 goal loop 的主要人工打断源；QwenPaw 已把「不泛化高危动词 + 失败回落精确」两道闸做对 |
| S10 | shell 引号感知混淆检测 | S | 元字符表有 `$(` 没有裸 `$'`，`bash -c $'…'` 被 lexShellLite 整体当一个参数、破坏性删除门看不见里面 |
| S11 | 灾难性删除路径归一化 | M | QwenPaw 先展开 `~`/`$HOME` 再 normpath，防 `~/..` 塌缩绕过；case 清单可直接抄 |
| S12 | 沙箱违规 → 升级审批 → 重试闭环 | M | 「默认沙箱、按需放行」的可用性关键；**依赖 S1 先落地** |

### B2 上下文与记忆
| ID | 能力 | 量 | 一句话 |
|----|------|----|--------|
| C7 | 模型自写检索标题 | M | 索引质量来自模型当时写下的标签，否则驱逐地图只显示「(no milestone)」；C3 好用的前提 |
| C8 | 精确 token 计数 | M | `EstimateTokens` 是字符估算，中日文/JSON 参数误差可达数倍，压缩门要么早触发要么 400 之后才触发 |
| C9 | 输出预留与硬上限拒绝 | S | 压缩后仍超窗时是 best-effort 转发让 provider 报 400，既没给输出留 token 也没有可捕获的本地失败 |
| C10 | 摘要质量校验 | S | 只判空；模型吐一句「好的我已总结」也会顶掉整段中段历史 |
| C11 | 摘要输入密钥脱敏 | S | `ctxcompact` 未 import `internal/secrets`，shell 输出里的 token 会进 summary 并每轮重发。**脱敏器已在仓库里，典型「写了但零读者」** |
| C12 | 自动记忆检索注入 | M | 记忆只有模型主动查才生效，而模型几乎不会主动调 |
| C13 | 后台记忆整合蒸馏 | M | memory 是单调增长流水账，撞上限从尾部硬截 —— 最早的偏好反而先被截掉 |
| C14 | 跨会话/跨 agent 检索维度 | S | memories 表无 session/agent 列，子代理与 goalloop 多轮共用一张表，分不开也追不回 |

### B3 循环护栏
| ID | 能力 | 量 | 一句话 |
|----|------|----|--------|
| L2 | 工具调用预算门 | S | 无法表达「这一轮最多跑 5 次 shell_run」，只能 guard 全禁或全放 |
| L3 | 单 turn wall-clock 超时 | S | turn 只能被用户取消或跑满迭代；QwenPaw 刻意只在迭代边界检查以免切断配对 |
| L4 | turn token 预算硬闸 | S | 交互式 turn 的 token 只写进 status 帧给 TUI 看，没有一处会因花超而停 |
| L5 | 过早停止的续跑注入 | M | yanshi 重跑整个 turn 会重放 `shell_run`/`fs_write`（ws.go 自陈的 SIDE-EFFECT CAVEAT）；QwenPaw 只在尾部追加续跑消息、不重放已执行工具 |
| L6 | 可插拔停止条件框架 | M | 停止逻辑硬编码在 ws.go 的 retry 循环，每加一种条件就要改那个已很长的循环。**L1–L5 的承载物，先移植框架最省** |
| L7 | 文件状态驱动的任务清单循环 | M | `task/work/types.go` 有 Checklist 数据结构却没有消费它的闸门，清单只是给人看的 |

### B4 工具与技能
| ID | 能力 | 量 | 一句话 |
|----|------|----|--------|
| T4 | 历史工具结果分级降级 | M | 64KiB 输出一直占窗口直到整轮摘要；QwenPaw 每轮 acting 后压到 3KB 并保留落盘原文 |
| T5 | 技能声明依赖 | S | 技能依赖 rg/gh/ast-grep 只能等 shell 里失败才发现；声明式 requires 能在 /skills 列表直接标红 |
| T6 | 技能状态决定工具暴露 | M | 所有工具永远在 schema 里，禁用只体现为调用后被拒 —— 白烧 prompt token 且模型反复试注定被拒的工具 |
| T7 | 模型自己创作并落盘技能 | M | goalloop 跑完一轮把「这次怎么修好的」沉淀成可复用技能，是这个循环最缺的闭环，现在只能沉淀成一行 memory |
| T8 | 技能远程安装（多注册中心） | M | 必须有 git 且只能指向 GitHub 仓库；HTTP API 装单个技能对内网镜像更实用 |
| T9 | ACP 委派做成模型可调用工具 | M | ACP 只能由目标循环自上而下调度，聊天中的模型无法说「这段让 codex 去做」。协议层现成 |
| T10 | MCP 凭据管理 | M | 只有 client_credentials，接需要 authorization_code 的企业 MCP 只能手写长期 token 进 config.yaml |
| T11 | 斜杠命令动态下发 | M | `commandTable` 是编译期静态表，技能想暴露 `/命令` 必须改 Go 源码重编译 |
| T12 | 工具批处理 DSL | L | 省 token 而非解锁新能力，且要在 Go 里自己写 AST 安全求值 |

### B5 模型运行时
| ID | 能力 | 量 | 一句话 |
|----|------|----|--------|
| M3 | 本地 provider 不套云端窗口 | S | 给本地 30b 按 262K 算窗口会让压缩彻底不触发、服务端静默截断 prompt 头。**M2 的必要配套** |
| M4 | 生成参数可配置 | S | 无法调高 max_tokens（长补丁被 4096 截断），也无法给判定型调用调低 temperature。加三个 yaml 字段 |
| M5 | 模型怪癖运行期学习 | S | 切到要求 reasoning_content 的模型后，历史里混有非思考模型消息就每轮 400，用户只看到失败 |
| M6 | 工具 JSON Schema 净化 | M | 工具多且嵌套深，接国产网关或 vLLM 时 `$ref`/nullable 常被拒 |
| M7 | 每模型 QPM 限流 | M | 子代理 fan-out 同时打同一 provider，没有前置节流只能靠事后重试 |
| M8 | 错误分类（可重试 vs 短路） | M | 字符串匹配误伤：错误消息里出现 "404" 的任何文本都判不可重试 |
| M9 | 模型清单发现与预检 | M | model 名写错要到第一次真实调用才暴露，且被判成不可重试直接失败 |
| M10 | token 用量落盘与时序聚合 | M | 只在会话内累计；goalloop 跑一夜后答不出「哪天哪个模型烧了多少」 |

### B6 存储与 VCS
| ID | 能力 | 量 | 一句话 |
|----|------|----|--------|
| V3 | 快照时间线可读呈现 | M | 只给一串 id，用户认不出要回到哪一刻；QwenPaw 用「那一轮你问的是什么」定位 |
| V4 | 选择性文件恢复 | M | 要么单文件手取、要么整树回退，缺中间粒度；goal loop 常见「只想撤销改坏的那两个文件」 |
| V5 | 回滚期间暂停并发写者 | M | repo 锁挡不住不走 VCS 的写者（shell_run 起的编译进程、后台 worker），可在展开树的中途改盘 |
| V6 | 恢复写入符号链接防逃逸 | S | 词法校验挡不住「树里路径合法、但工作副本那层目录已被换成指向 /etc 的软链」—— agent 自己就能造 |
| V7 | 子 agent 分支完成度注册表 | L | 没有「这条分支是否已安全收尾」的账本，子进程崩溃后残留 worktree 只能靠人 |
| V8 | 跨进程写者互斥 | S | lockfile 是为多窗口设计的，但 VCS 的 lockRepo 只在单进程内有效 |

### B7 运维与上手
| ID | 能力 | 量 | 一句话 |
|----|------|----|--------|
| O1 | 日志落盘与轮转 | S | alt-screen TUI 期间 stderr 基本被吞，事后排障零记录 |
| O2 | doctor 自动修复 | M | 检查面已很全，差「一键修」；QwenPaw 的白名单 + 备份 + 非交互拒危险项可直接抄 |
| O3 | 守护进程运维子命令 | S | 改 config 只能杀掉 serve 重启且丢 lockfile 归属；热重载是每天会碰的摩擦点 |
| O4 | 初始化向导 | M | 新用户只能手抄 config.example.yaml |
| O5 | 交互式 provider 配置 | M | 加 provider 的唯一路径是编辑 YAML 并重启后端 |
| O6 | 定时任务操作入口 | S | **零件造好了、总装线没接上**：调度器与持久化都有，操作员看不到有哪些任务、下次何时触发、怎么停 |
| O7 | 就绪探针语义 | S | 多窗口后端发现靠 healthz 认领 owner，一个还在装配 store/VCS 的进程就被判为活的 |
| O8 | govulncheck | S | 会在用户机器上执行 shell 与拉子进程的工具，依赖链 CVE 无人盯 |
| O9 | 发布产物 e2e 验证 | S | `-h` 打印用法不能证明二进制能真的引导后端 |
| O10 | 异常现场落盘 | M | turn 崩掉只留一行 error_type 且刻意不打错误体，既不泄密也无法复盘 |
| O11 | stdio ACP server | M | 只做 ACP 客户端，反向没有 server —— 无法被 Zed 等宿主直接接入 |

---

## C. 明确不建议做（与定位冲突）

浏览器端管理控制台、多渠道 IM 接入（钉钉/飞书/微信/Discord/Telegram/QQ）、Tauri 桌面外壳与
Computer Use、mini-app 平台、第三方插件系统（注册 HTTP 路由/channel）、技能市场浏览、
本地推理运行时（下载安装起服务）、视觉压缩（长文本渲染成 PNG）、容器化部署与进程管家、
向量检索长期记忆后端、二进制自更新。

全部 L/XL，且都是把 yanshi 从「单二进制编码 agent」推向「个人助手平台」的方向。

---

## C'. 交付记录（2026-08-08，同日实现）

本矩阵产出当天即按 A/B 两段逐条实现，分四波并行推进（每波若干 agent 分包实现 + 一个集成校验
agent 做组合根接线与全量修红）。**新增 3 个叶子包**：`internal/loopguard`（停止条件框架）、
`internal/toolreg`（运行期已注册工具名集合）、`internal/acpserver`（stdio ACP server）。

**A 段 19 条与 B 段全部实现，无跳过项。**（首轮曾跳过 S1 / S12 / T12，随后按「全部做」的要求补齐。）

**S1 OS 级子进程沙箱**（原判 XL，QwenPaw 侧四后端约 8.6K 行）现在四个平台都有真实实现或诚实降级：

| 平台 | 后端 | 强制了什么 | 实测状态 |
|------|------|-----------|---------|
| macOS | Seatbelt（`sandbox-exec` + 生成的 SBPL） | 分级文件写、网络（NetworkDeny）、信号作用域 | **真跑过**：ReadOnly 拒写、WorkspaceWrite 边界、出网被拒，各带无沙箱对照组 |
| Linux | bubblewrap，回落 Landlock | bwrap：文件+pid+net+ipc+uts；Landlock：**仅文件系统** | bwrap 在真实内核 6.12.54（Docker）实测 4 条拦截；**Landlock 系统调用路径只编译过** |
| Windows | Job Object | **进程树 kill**（`CanKillTree=true`），文件系统**未强制** | 仅 `GOOS=windows` 编译 + vet；Win32 调用从未执行 |
| 其他 | — | — | 诚实报 `DegradedHostGuard` |

Windows 一档**刻意报 `DegradedHostGuard` 而非 `OSIsolated`** —— Job Object 限制的是生命周期不是访问，
谎报会让 `tools.SandboxEnforcing` 把每个 "Access is denied" 读成沙箱拒绝，对每个普通权限错误弹一次
修不了问题的提权审批。AppContainer 明确不做（`os/exec` 不暴露 `STARTUPINFOEX`，要自建 spawn 后端）。

**S12 沙箱违规 → 升级审批 → 重试**：四条路径（批准后重试 / 拒绝 / 超时 / 无 callback）实测走对，
超时**绝不等于放行**。

**T12 工具批处理 DSL**：按「不写通用表达式求值器」的原则实现 —— 它是一个数据结构（JSON 数组 +
步骤引用），不是一门语言；每一步各自走完整 `Authorize`，批内被拒即整批停下（实测被拒步骤的文件
不存在）。原矩阵担心的自研 AST 求值面因此不存在。

**单文件行数上限**同期由 1000 放宽到 5000（GOV2）：1000 那一档已经不再筛选内聚性而是在筛选碎片化
—— 组合根卡在 984，每加一行接线都被逼进新文件。详见 CLAUDE.md 对应条目。

**全部能力已做实测验收**，结果见 [实测验收矩阵](2026-08-08-parity-verification.md)：
67 条中 52 条实测通过、12 条实测发现缺陷并修复、3 条如实记为无法实测。
**全绿的测试套件掩盖了 11 处真缺陷，其中 `sudo rm -rf /` 曾判 Allow 是可被利用的安全绕过。**

### 几处与原矩阵判断不同的落地结论

- **C8 精确 token 计数**未引入 tokenizer 依赖。tiktoken-go 首次使用要联网取 BPE 表，恰好让
  离线部署的**第一次**压缩失败。改为改进启发式并**实测**：1169 份真实文档 × 2 种编码共 2338 次
  测量，旧的 chars/4 有 1983 个样本低估（最低 0.25×），新估算器 **0 个样本低估**、中位 1.52×。
  压缩阈值是窗口的分数，一个有界且始终偏高的估算在运行上等价于精确计数且更安全。
- **C5 折叠落在 `internal/ctxcompact` 而非 `internal/tools/spillover.go`**。spillover 是**输出时**
  针对单条结果的决策，折叠是**入窗时**针对整个历史的决策；且 GOV1 不允许 ctxcompact 反向依赖
  tools。T4（每轮即时降级）才是 spillover 侧那一半，两者叠加。
- **S2 敏感路径判为 `Prompt` 而非 HardDeny**。HardDeny 在 default 模式不可申诉，会打断
  「读我的 `~/.gitconfig`」这类正当请求，从而教会操作员**全局放宽 profile** —— 严格更糟。
  逃生门是 profile 里**字面**写出该路径（不含通配符）。因此本次**没有**新增结构性 HardDeny，
  CLAUDE.md 里「结构性 HardDeny 当前是 5 类」那段闭合枚举仍然成立、未被改动。
- **S9 的泛化产物是 execpolicy 规则，但 `execpolicy` 表达不了「精确匹配」**：`Rule.Prefix` 匹配
  argv 前缀，于是列全参数的规则仍会放行任意超集。QwenPaw 的「失败回落精确」因此在本仓落成
  「失败回落**无规则**」（命令继续每次弹窗）。要真正对齐需要给 `execpolicy.Rule` 加一个 exact
  语义，是独立工作包。**另注意 S9 只对使用 `shell.rules` 的 profile 生效** —— 出厂的 `coding`
  profile 用的是 policy/patterns，对它是 no-op。
- **T11 斜杠命令动态下发走 `/skill run <name>` 前缀方案**，而不是让技能注册顶级 `/foo`。
  `commandTable` 因此仍是编译期可解析的真相源，`internal/archtest::TestPhantomSlashCommandsNotAdvertised`
  的解析假设不受影响（已用正反探针复核该门禁未被削弱）。
- **O8 govulncheck 落成 CI 硬门禁**，并把它当场报出的 8 个可达漏洞一并修掉（grpc / x/text /
  goldmark / otel 全家 → 各自 fixed 版本，Go toolchain → 1.26.5 覆盖 crypto/tls 与 os 两条
  stdlib 项）。现在 `govulncheck ./...` 报 0 个可达漏洞。
- **O9 的发布产物验证不是 `-h`**。`-h` 对每一种真实故障（store schema 打不开、组合根 panic、
  provider 注册表拒绝示例配置、nokeyring 构建缺后端）都返回 0。改成在临时目录里用
  `config.example.yaml` 跑一次 `exec --fake-model` 的完整 turn，并断言输出帧。

### 未纳入 S0 台账的理由

`docs/feature-status.yaml` 的作用域是审计出的那 63 条 S0 条目，且分母被 `acceptancePins` 钉死。
本轮交付的是 QwenPaw 对标项，与那 63 条不是同一个集合；混进去会让台账的分母失去含义。
本节即这批工作的记录处。

---

## D. yanshi 的护城河（QwenPaw 没有或更弱）

- **治理门禁**：架构分层/规模/装配可达性的机器强制（GOV1–9）、功能台账逐句验收证据握手、
  生成文档 diff 门禁、覆盖率门禁 —— QwenPaw 侧没有对应物
- **autoVCS**：agent 每次编辑的自动逐笔追踪与作者归属、worktree 隔离与三方合并
- **guard 细节**：argv 级结构化 execpolicy（program + prefix + deny_flags）、root-jail 路径逃逸
  校验（symlink/卷/大小写）、批准规则绑定脚本内容哈希、受管出站代理、统一密钥脱敏注册表
- **AI 判决式自动批准**（auto 模式，本周刚合）
- **goalloop**：plan→implement→evaluate→judge 四阶段 + 可执行验收测试评估（无测试即失败）
- 幻觉工具名作为工具结果回喂重试、多 provider failover、OTLP 标准遥测、
  会话分叉、工具输出溢出落盘、连续工具失败熔断
