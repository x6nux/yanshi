# autocode 后续功能总体路线图（桶 1 + 桶 2）

> 日期：2026-07-18  
> 依据：`docs/feature-comparison-with-codex.md`（后续功能总表）  
> 范围：桶 1（高杠杆快赢）+ 桶 2（安全基座 / 平台化 / 体验增强），共约 44 项。  
> 排除：桶 3（与「单二进制、本地、单用户」定位冲突或纯锦上添花），除非产品定位转向远程/多人。

## 目标

为 `feature-comparison-with-codex.md` 中桶 1 + 桶 2 的功能提供统一的里程碑路线图，作为后续所有具体 spec / plan 的总纲。**本文件是路线图，不是逐行施工图**；每个里程碑启动时各自走独立的 spec → plan → 实现循环。

## 执行模型

- **形态**：纯路线图（里程碑为单元）。每个里程碑定义范围、并行泳道、验收、风险与重叠窗口。
- **并行**：用 Claude Code 自身的 subagent（`Agent` 工具，配合 `dispatching-parallel-agents`）按独立泳道并行派发；**不使用 autocode 自己的多 agent / goalloop / ACP 来推进开发**。
- **流程**：里程碑 spec → `writing-plans` 拆任务包 → 按泳道派发 subagent → 合并点等前置泳道合并后再派发 → 收尾跑 `requesting-code-review` + `verification-before-completion`。
- **依赖原则**：泳道内部串行，泳道之间并行；里程碑之间允许重叠窗口——长周期项（尤其 `S08`）可提前到上一里程碑进行中启动。

## 依赖原则与关键路径

- 关键路径（最长链）：`A12 → V14 → M04 → M06`（平台线）与 `S08 → T08 → T09`（shell 线）。
- `S08`（OS 级 Sandbox）是全表单点最长、最高风险项，跨平台系统级实现。
- `V14`（公共 Agent API v1）是「基座中的基座」，定错契约会波及半张表，必须先冻结契约再解锁下游。

## 与桶 3 的边界

桶 3 全部排除在计划外，典型类别：

- 假设多人 / 远程 / 账号：`O03`、`V06`、`O13`、`O12`。
- 假设商业插件生态：`E05`、`E07`、`E08`。
- 锦上添花 / 低 ROI：`M07`、`T14`、`C15`、`O06`、`O10`。

**唯一例外**：`T18`（动态工具发现）原依赖 `E05`（桶 3），故 T18 范围**限定为「基于 V16 的 MCP 工具动态发现 + namespace + 延迟加载」**，plugin 部分不做。

若产品定位转向远程 / 多人部署，`O03` / `V06` / `O13` 需重新评估。

---

## M1 — 高杠杆快赢（桶 1，9 项）

**目标**：无安全 / API 重构依赖，立即全面铺开；修复两个 goalloop 缺陷；打通结构化输出 + 无头执行主链。

| 泳道 | 项 | 依赖 |
| --- | --- | --- |
| 1（主链） | A12 结构化输出 → V12 无头 exec | V12 依赖 A12 |
| 2 | T06 多文件 Patch | — |
| 3 | G03 T0-T4 路由 + G02 Token budget | 同在 goalloop 包，相关 |
| 4 | C07 排队模式 | — |
| 5 | V10 会话生命周期 + A11 分层指令 | — |
| 6 | O07 doctor | — |

**验收**：
- A12 可传 JSON Schema、校验失败可重试且 text 模式不受影响。
- `autocode exec` 支持 prompt / stdin、text + JSONL 输出、稳定退出码、取消 / 超时、可恢复 session。
- T06 支持 add / update / delete / move + dry-run + 原子应用 + 成功变更进入 autoVCS tracking。
- goalloop 按 tier 真实执行（不再「只打印后返回」）；token 累计并按预算可靠停止。
- `/queue-mode` 可切三种排队策略；会话可 rename / archive / delete；`AGENTS.md` 父→子分层合并；`autocode doctor` 人 / JSON 双输出。

**风险**：
- A12 的校验-重试循环不能打断 text 模式。
- V12 退出码 / 取消 / 超时要和 WS / SSE 传输对齐。

**重叠窗口**：无前置，可立即 6 泳道全开；A12 + T06 就绪即可提前启动 M3 的 V13。

---

## M2 — 安全基座（桶 2a，8 项）

**目标**：全表最难、最耦合的里程碑。先定 sandbox abstraction 接口，再分平台实现 adapter。

| 泳道 | 项 | 依赖 |
| --- | --- | --- |
| 1 | S06 结构化 Shell 策略 → S07 持久审批规则 | S07 依赖 S06 |
| 2 | S08 OS 级 Sandbox → S09 子进程网络隔离 | S09 依赖 S08 |
| 3（合并点） | T07 Shell runtime v2 | 需 S06 + S08 |
| 4（合并点） | T08 持久 Shell 与 stdin | 需 S06 + S08 |
| 5 | V16 通用 MCP client | 需 S07 |
| 6 | S10 凭据与 Secret 保护 | 相对独立 |

**验收**：
- guard 能解析程序 / 参数 / 管道 / 重定向并执行细粒度规则，有绕过样例回归测试。
- 审批规则含来源 / scope / 过期 / 审计，可查看和撤销。
- sandbox（Windows job object + restricted token / Unix landlock + bwrap）使越界文件与进程操作被系统拒绝。
- 子进程网络默认拒绝、host / port 规则生效、DNS 和重定向不能绕过。
- shell runtime v2 能跑真实构建，超时和取消能终止整个进程树。
- MCP client 支持 stdio / HTTP、OAuth、tools / list / call 与 resources、断线重连，启动超时有测试。
- secret 不写入日志 / 事件 / DB 明文，keyring 可读写，无 keyring 时安全降级。

**风险**：
- `S08` 跨平台系统级，是全表最高风险——**必须先单独 spec 定统一 sandbox abstraction 接口，再实现各平台 adapter**。
- T07 / T08 是合并点，阻塞在 S06 + S08。
- V16 的 OAuth + 重连测试复杂。

**重叠窗口**：`S06` 与 `S08` 互不依赖可同时启动；`S08` 周期最长，建议 M1 进行中就启动其接口设计。

---

## M3 — 平台化 / API（桶 2b，9 项）

**目标**：在现有 server 上稳定 thread / turn / item 契约，建立多 Agent 与 Goal 的平台化能力。`V14` 契约先单独冻结，再解锁下游。

| 泳道 | 项 | 依赖 |
| --- | --- | --- |
| 1（基座） | V14 公共 Agent API v1 | 契约冻结后解锁泳道 3 / 4 / 5 / 6 |
| 2 | V13 结构化代码 Review | 需 M1 的 A12 + T06 + V12 |
| 3 | M04 多 Agent 生命周期 → M05 角色 / 模型覆盖 | 需 V14；M05 需 M04 |
| 4 | V09 会话 Fork → V15 TS SDK | 需 V14 |
| 5 | V17 通用 Agent MCP server | 需 V14 + M2 的 V16 |
| 6 | G04 Goal 暂停 / 恢复 + G05 Plan mode | 需 V14 |

**验收**：
- 版本化 thread / turn / item JSON-RPC 契约 + JSON Schema；start / resume / interrupt 与流式 item 可用。
- 多 Agent 全生命周期（list / message / wait / interrupt / resume / close）+ 线程树 / 深度 / 并发 / usage 可查询；取消不泄漏任务。
- fork 生成新 ID 且原历史不可变；Agent 角色 / 模型 override 受策略限制。
- Goal 跨进程恢复（plan / iteration / verdict / budget 持久化），取消和失败状态可查询。
- plan mode 禁用编辑类工具 + 计划可流式更新 + 用户确认后切换执行且历史连续。
- TS SDK 支持 start / resume / run / stream / cancel，类型由协议生成。

**风险**：
- `V14` 重构现有 frame 词表为版本化资源模型，风险最高——**先冻结契约再动其余泳道**。
- G05 plan mode 要和现有 collaboration mode 体系对齐，不能另起炉灶。

**重叠窗口**：V13 只等 M1 的 A12 + T06，可最早启动；V15 / M04 / V17 全阻塞在 V14 契约。

---

## M4 — 体验增强（桶 2c，18 项，分批）

**目标**：最松耦合、最可并行的里程碑；大量子项在 M2 / M3 进行中即可启动。

| 主题组 | 项 | 依赖 |
| --- | --- | --- |
| 多模态 | A13 多模态输入 → T13 图片查看 | T13 需 A13 |
| 工具增强 | T11 Web Search、T16 结构化询问、T09 后台任务、T18（范围限定为 MCP） | T09 需 M2 T08；T18 需 M2 V16 |
| 管理界面 | C13 MCP 界面、E03 Skill 管理、C14 会话选择器、M06 线程 UI | C13 需 M2 V16；M06 需 M3 M04 |
| 可观测 | O05 遥测、O08 Debug 命令 | — |
| 会话体验 | V11 side conversation、C16 IDE mention | V11 需 M3 V09；C16 需 A13 + V14 |
| VCS / Git | W07 Git 体验 | — |
| 扩展点 | E06 Hooks v1 | — |
| 平台杂项 | O02 Feature flags、O09 Shell completion | — |

**验收**：
- TUI / exec / API 均可传图片且校验大小 / 类型；不支持图片的 provider 返回可理解错误。
- MCP / Skill / 会话 / 线程管理界面可用；后台任务可 list / wait / stop。
- Web Search 受网络策略约束，返回标题 / 摘要 / URL。
- 遥测默认脱敏 + 支持 OTel export；Debug 命令输出模型目录 / 有效配置来源 / 可见 prompt / trace 摘要且不打印 secret。
- Git adapter 保留 autoVCS 状态边界，不修改用户 Git 配置。
- hooks 有超时 / 失败策略 / 输入输出脱敏，hook 不能绕过权限。

**风险**：
- A13 跨 provider / TUI / exec / API 四层改动。
- C16 跨 M3 / M4 依赖。
- E06 安全敏感（不能绕过 guard）。

**重叠窗口**：A13 / T11 / T16 / E03 / O05 / O08 / W07 / E06 / O02 / O09 不阻塞任何主线，可在 M2 / M3 进行中随时启动。

---

## 跨里程碑关键接口

- `M1.A12 + M1.T06 + M1.V12` → **M3.V13**
- `M2.S06 → S07 → V16` → **M4.C13、M4.T18**
- `M2.S08 → T08` → **M4.T09**
- `M3.V14` → **M3.M04 / M05 / V09 / V15 / V17 + M4.M06 / C16**
- `M3.V09` → **M4.V11**

## 风险登记

| 风险 | 影响 | 缓解 |
| --- | --- | --- |
| `S08` 跨平台系统级，周期最长 | 阻塞 T07 / T08 / T09 / S09 | M1 期间即启动接口设计；先定 abstraction 再实现 adapter |
| `V14` 契约定错 | 波及 M04 / V15 / V17 / M06 / C16 | 先冻结契约并写契约测试，再解锁下游泳道 |
| 安全基座（S06 / S08）绕过 | agent 越权 | 每项必须有绕过样例回归测试 |
| 并行泳道在合并点冲突 | T07 / T08 / V13 合并困难 | 合并点显式标注，前置泳道合并后再派发 |
| A12 / A13 跨层改动打断现有主流程 | text 模式 / 现有 turn 受影响 | 保留 text 模式不变；多层改动逐层加测试 |

## 后续步骤

本路线图是总纲。每个里程碑启动时，基于本文档写**独立的里程碑 spec**（M1 优先），再进入 `writing-plans`。第一个具体 spec 将是 **M1 — 高杠杆快赢**。
