# ADR-0027: PreToolUse 跑在 guard 之前，因为 guard 永远对最终入参跑

- 状态：accepted
- 日期：2026-08-31
- 相关：INF4（W-F-02 hook 总线）、ADR-0004（guard fail-closed）、W-B-02（secproc 收敛）

## 背景（Context）

INF4 的承重约束写明：「hook 的 `block` 是**追加**一道拒绝，不能把 guard 的拒绝
翻成允许」，并给出实现机制——「顺序：guard 先判，Allow 之后才跑 PreToolUse」。

实现这条总线时发现，字面顺序在本仓的授权拓扑里没有干净的落点。guard 的判决
不住在编排器里，它住在每个工具自己的执行入口：`GuardedTool.Stream` 的第一句
`Authorize`、`secproc.Launch` 的第一步 Authorize、fs 工具 handler 里的
`checkFS`。编排器的 ADK 中间件能包住整个工具调用（`WrapInvokableToolCall`），
但包不到「工具入口那次 Authorize」与「handler 真正干活」之间。

要让 hook 跑在 guard 的 Allow 之后，中间件必须自己先跑一次 Authorize。那是一
次**裸 Action** 的判决（只有工具名，没有 Shell/FS/路径——那些字段是各工具从
自己的入参里解析出来的，中间件没有这份逐工具的解析知识），它会带来三个后果：

1. 交互模式下，工具不在 profile glob 内的调用会为同一次调用弹**两个**窗——
   一个问裸 Action，一个问真实 Action。这正是 W-B-02 为 shell_run 删掉双
   Authorize 的同一形状。
2. strict 模式下每次被 hook 的调用变成**两次**确认（confirmEveryCall 对任何
   Allow 都改写成 Prompt，裸 Action 那次也不例外）。
3. 安全上什么也不多给：裸 Action 的判决只是 tools 维度的 glob，真正的判决仍
   然在工具入口那次。

## 决策（Decision）

**PreToolUse hook 在 ADK 工具调用包装层运行，先于工具入口的 guard 判决；
guard 永远对「经过全部 hook 之后、最终到达工具的那个入参」做完整判决。**

这条顺序下，约束保护的**不变量**原样成立，且成立得更硬：

- hook 的协议（`hookResponse`）里**不存在 allow 这个判决**。hook 能做的只有
  拦截（追加拒绝）、改写入参（`updated_input`）、附加上下文。判决权自始至终
  只有 guard 一个持有者。
- 「guard 批准 `ls`、hook 改写成 `rm -rf /`」这条 spec 点名的攻击，在这条管
  线里不是「被拦住」而是「**不可表达**」：不存在一份先算好、之后被 hook 绕过
  的判决。guard 对改写后的入参从零跑完整的判决（不是增量），结构性 HardDeny、
  敏感路径 denylist、profile glob 全部照常生效。
- hook 的失败（超时/崩溃/退出非零/输出读不懂）按 fail-closed 拒绝该次调用，
  拒绝作为工具结果回喂，turn 不中断。这保证「模型故意让 hook 崩溃」得到的
  是显式拒绝而不是静默旁路。

已知的行为差异（相对字面顺序）：guard 本会拒绝的调用，其入参会先被 hook 看到
一次。hook 是操作员自己配置的程序，看到的是模型写的入参原文（本来就是模型
可见、会进 transcript 的数据）；这不放宽任何东西。

## 后果（Consequences）

**不可违反的约束：**

1. `updated_input` 之后**必须**重新执行 guard，且判的是**最终**入参。任何
   「缓存判决」「增量跳过」的优化都会重新打开 spec 点名的那条攻击。当前实现
   里这件事是结构性的（endpoint 收到的 argsJSON 就是最终入参，guard 住在
   endpoint 里），钉住它的测试会让「把原始入参交给 endpoint」的变异变红。
2. hook 的 verdict **不得**增加 allow/approve 之类的放行字段并被实现消费。
   判决权只有一个持有者。`hookResponse` 的解析故意忽略未知字段；给协议加放
   行字段等于推翻本 ADR。
3. hook 失败必须 fail-closed（拒绝该次调用），且拒绝必须是工具**结果**而非
   Go error——Go error 会在 ADK 里变成 NodeRunError 拆掉整个 turn，那会把
   「hook 坏了」升级成「会话死了」。
4. hook 子进程只能走 `secproc.Launch`（INF4 承重约束 3），AllowEnv 保持空。
   spec.Tool 填被 hook 工具的名字：hook 子进程的授权与那次工具调用是同一个
   问题，不引入第二个需要配置（或注册）的名字。

**已知代价：**

- 被拒绝的调用也会跑 hook（多一次子进程发射的钱）；被 profile glob 拒绝的
  调用在交互模式下会先收到 hook 子进程那次的弹窗，语义上是「这次调用（连同
  它的 hook）要不要进行」。
- strict 模式下，被 hook 的调用会有两次确认：一次是 hook 子进程的发射，一次
  是工具调用本身。strict 的契约本来就是「每次都问」，这里没有做豁免——豁免
  是授权面的变更，该走工作包。

**为什么要写下来：** spec 的机制句面（guard 先判）与本实现的机制不同，但不变
量相同且更强。不落这条 ADR，下一轮评审会把它当「漏接了承重约束」重报一次，
然后再花一轮重新推导上面的事实。
