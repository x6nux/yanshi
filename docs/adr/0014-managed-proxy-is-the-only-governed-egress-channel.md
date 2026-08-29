# ADR-0014: 受管代理是子进程出网的唯一受治通道，且只对协作型子进程成立

- 状态：Accepted
- 日期：2026-08-06
- 相关：ADR-0003（guard fail-closed）、A1/S09（台账）、W5

## Context

在 W5 之前，`shell.DefaultSecureFactory` 把 `http://127.0.0.1:0` 作为
`HTTP_PROXY`/`HTTPS_PROXY` 发给被拉起的子进程。这个 URL 指向一个**永远不会存在**
的端口 —— 端口 0 是「让内核挑一个」的占位，不是一个可连的地址。

它的实际效果是：子进程试图连代理 → 连接被拒 → 多数客户端**回落到直连**，或者
干脆报错。两种结局都不是「按策略出网」。而代码与文档读起来像是有一道网络闸门。

这是**比没有闸门更糟的状态**：没有闸门时，`CapabilityReport` 会诚实地说没有；
有一个假闸门时，读代码的人、写文档的人、以及台账都会以为有。

W5 采用 Option B —— 只做不依赖 OS 沙箱的那一半：真的起一个 loopback 代理，
按 `netpolicy.Policy` 判 host，把真实 URL 发给子进程。

## Decision

**子进程的出站流量，唯一受治的通道是 bootstrap 启动的 loopback 受管代理。**

1. `shell.DefaultSecureFactory` **不得再发布伪装成强制的占位 URL**。要么是一个
   真在监听的代理地址，要么是空字符串（= 显式声明「本进程没有网络策略」）。
   一个连不上的 `HTTP_PROXY` 不是策略，是噪音。
2. CONNECT **只做 host 策略判定 + 盲隧道，禁止 MITM**。代理拿到 CONNECT 后
   只解析出 host、过 `CheckHost`、然后 `io.Copy` 双向盲转。不解 TLS、不换证书。
   理由是这个进程持有 provider API key：一旦 MITM，代理就成了整个系统里
   secret 浓度最高的一个新泄漏面，而它换来的只是 URL 粒度而非 host 粒度的策略。

   > **⚠️ 本条已被 [ADR-0023](0023-inspecting-proxy-trust-boundary.md) 修订（W-B-17）。**
   > 修订**不是推翻**：上面这段描述的仍然是**默认形态**，`security.network.inspect_https`
   > 默认 `false`，此时行为逐字节等同于本条。ADR-0023 收窄的是禁令的范围 ——
   > 它接受了「secret 浓度」这个理由，并把它落成「代理不得**记录**」（`netpolicy.Request`
   > 在类型上就没有 path/header/body 字段）而不是「代理不得**解密**」。
   > 同时它指出本条第二个理由在 W-B-17 的验收面前不成立：盲隧道里 method
   > **不存在**，所以「换来的只是 URL 粒度」低估了差距。改这一段之前先读 ADR-0023。
3. 每一次 host 判定 —— 放行与拒绝**都要**进审计（`Proxy.audit`）。只审计拒绝
   会让「代理到底有没有在工作」无法从日志上回答。

## Consequences

**不可违反的约束：**

- 任何新增的子进程发射路径，要么经 `secproc`/`shell.Factory` 拿到代理 URL，
  要么在代码里显式写明它不受网络策略约束。**不允许第三种状态**（发一个
  连不上的地址）。
- ~~代理不得获得解密能力。~~ **由 ADR-0023 修订为：代理不得在未被明确要求时解密，
  且任何情况下不得记录 path / header / body。** 「需要 URL 粒度策略时在**工具层**判」
  这条对 `web_fetch` 仍然成立并且仍是首选 —— 它对**子进程**不成立，因为
  `npm install` 的 postinstall 脚本没有哪个工具层看得见它发的 method。

**明示残留风险 —— 这条是本 ADR 存在的主要理由：**

Option B 只约束**协作型子进程**。一个无视 `HTTP_PROXY` 环境变量、直接
`connect()` 或开裸 socket 的子进程**完全不受此约束**。要挡住它需要内核层强制
（seccomp / Seatbelt / AppContainer），那属于 **S1**。

因此：**在 `CapabilityReport.Effective` 脱离 `DegradedHostGuard` 之前，任何
文档、台账或 UI 都不得声称「未授权连接失败」是全覆盖的。** 台账 `A1/S09` 的
acceptance 相应带一条作用域限定（「经受管代理通道的连接」），它不是措辞润色，
是这条残留风险在验收标准上的投影 —— 去掉限定词，验收标准就开始撒谎。

**残留风险的一半已被消化（W-B-09，补记）：** 上面点名的内核层强制，在 Linux 的
landlock 后端上已经落地 —— 再执行助手在 `execve` 之前给自己装一道 seccomp-BPF
过滤器，`socket(2)`/`socketpair(2)` 只放行 `AF_UNIX`，`ptrace`/`process_vm_readv`/
`io_uring` 恒拦。**这不改变本 ADR 的决策，只把「哪里仍然不覆盖」讲精确了**，
边界现在是三条而不是一条：

1. **只有 landlock 后端**。bubblewrap 是首选后端，它靠 network namespace 达到
   同一效果（更强），但 `io_uring` 不在它的拦截面内；darwin 的 Seatbelt 与
   Windows 后端不受本条影响。
2. **粒度是地址族，不是主机**。seccomp 读不到 `connect(2)` 的 sockaddr（指针），
   所以判定点在套接字创建。「允许出网但只准连某台主机」仍然只有代理能做。
3. **`NetworkDeny` 为 false 时不装网络那半**。操作员没要求禁网时，过滤器只保留
   那三条恒拦项。

判断当前处于哪一档，读 `CapabilityReport.Backend`（`landlock` 还是
`landlock+seccomp`）与 `Reason`，**不要**从平台推断。

**已知代价：**

- 一次代理启动失败会让子进程完全无策略出网。bootstrap 为此在 stderr 上打
  `subprocess egress is UNFILTERED`，而**不是**拒绝启动。理由与 VCS 初始化失败
  一致：非致命子系统失败应降级并说清楚，而不是把整个进程拖死。这条与
  ADR-0003 的 fail-closed 不冲突 —— fail-closed 管的是 guard 对**动作**的判定，
  这里失败的是一个可选的传输层设施。
