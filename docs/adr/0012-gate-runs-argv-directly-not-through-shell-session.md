# ADR-0012: task_gate_run 直接执行 argv，不经 shell session runtime

- 状态：Accepted
- 日期：2026-08-05
- 相关：ADR-0002（guard fail-closed）、A2/DT2（台账）

## Context

路线图对 A2/DT2 的措辞暗示 gate 应当复用 shell v2 的 session runtime。实际实现用
`exec.CommandContext` 直接跑一条 argv，两者不一致。2026-08-03 的审计据此判它
divergent。

三个事实决定了这次不是「实现偷懒」：

1. **shell v2 的 runtime 至今全仓零注册。** 那正是 W1 里九条豁免的由来。把 gate
   挂到一个尚未装配的子系统上，等于把一个能用的东西改成不能用的。
2. **gate 命令是一次性 argv，不是会话。** session runtime 的价值在于跨命令的状态
   （cwd、环境、后台进程、stdin 交互）。gate 一条命令跑完即终，这些一样都用不上，
   接上去只会让它继承一套需要显式关闭的生命周期。
3. **安全面不因绕开 session 而变。** gate 与 shell v2 过的是同一个
   `guard.Authorize`。metachar 的结构性 HardDeny、profile 的 shell 策略、破坏性
   删除维度，对两者一视同仁——`53f6719` 补上 `Workdir` 后，连破坏性删除的
   workdir 相关规则也已对齐。

## Decision

**接受这条偏离，并把 DT2 的验收标准改写为与执行载体无关。**

DT2 的四条正式验收——证据结构、大输出落 artifact、挂对 task、退出码与 duration
——**没有一条依赖命令跑在哪个载体上**。它们描述的是 gate 产出什么，不是它怎么起
进程。用「必须走 shell session」当验收，是为满足一句路线图措辞而引入耦合。

## Consequences

**不可违反的约束：**

- gate **必须**继续经 `tools.Authorize` 授权。绕开 guard 直接 exec 是本 ADR 明确
  不授权的事，任何这样的改动都要另立 ADR 推翻本条。
- gate 的 `guard.Action` **必须**带 `Workdir`。缺了它，破坏性删除维度会跳过
  「删工作目录自身/祖先」两条规则——这正是 `53f6719` 修的缺陷，
  `TestGateActionCarriesWorkdir` 守着它。
- gate **不得**获得 shell v2 才有的能力（后台进程、stdin 交互、跨命令状态）。
  真需要这些，说明需求已经变了，届时重新评估载体而不是悄悄加。

**已知代价：**

- gate 与 shell v2 是两条执行路径，各自带鉴权。**新增 gate 类工具时必须自己带
  `Authorize`**，那里没有 `secproc` 兜底（与 CLAUDE.md 对 shell v2 的同一条告诫
  同源）。
- 两条路径的超时、输出截断、退出码处理各写一份，存在漂移风险。目前两边行为一致
  是人工核对的结果，没有门禁对账。若日后漂移造成事故，正确的收敛方向是抽公共的
  「跑一条 argv 并采集证据」helper，**而不是**把 gate 塞进 session runtime。

**为什么要写下来：** 不落这条 ADR，下一轮审计会再判一次 divergent，然后再花一轮
重新推导出上面三个事实。
