# ADR-0023: 受管代理可以解密，但只在被明确要求时，且只记录它判决所需的两个字段

- 状态：Accepted
- 日期：2026-08-29
- 相关：ADR-0014（受管代理是唯一受治出网通道 —— 本 ADR **修订**其 Decision §2 与
  Consequences 的第二条约束）、ADR-0003（guard fail-closed）、W-B-16、W-B-17

## Context

ADR-0014 的 Decision §2 写着：

> CONNECT **只做 host 策略判定 + 盲隧道，禁止 MITM**。……理由是这个进程持有
> provider API key：一旦 MITM，代理就成了整个系统里 secret 浓度最高的一个新泄漏面，
> 而它换来的只是 URL 粒度而非 host 粒度的策略。

这条禁令的**理由是对的，结论过宽**。把它拆成两半看：

1. **「代理会成为新的 secret 泄漏面」** —— 这是关于代理**记录什么**的陈述，不是关于
   它**能否解密**的陈述。一个解密后把 body 流式转发、既不缓冲也不落盘、审计行只写
   host 与 method 的代理，并不比盲隧道多持有一个 secret：它多持有的是一个**瞬间**，
   而盲隧道多持有的是**无知**。
2. **「换来的只是 URL 粒度」** —— 这半在 W-B-17 的验收面前不成立。验收要的是
   「同一域内 GET 与 POST 可分别裁决」，而**盲隧道里 method 根本不存在**。
   `GET https://api.example/read` 与 `POST https://api.example/admin/delete`
   在 CONNECT 行里是同一个字符串 `api.example:443`。这不是粒度粗，是**信息不存在**。

ADR-0014 同时给出了一条替代路径：「需要 URL 粒度策略时，正确做法是在**工具层**判
（`web_fetch` 这类工具本来就看得见完整 URL）」。这条对 `web_fetch` 成立，对本 ADR
要解决的问题不成立 —— 受管代理面对的不是工具，是**子进程**：`npm install` 的
postinstall 脚本、`go mod download`、`gh api`。没有哪个工具层看得见它们发的 method。

## Decision

**代理可以解密 CONNECT，条件有三条，缺一条则退回 ADR-0014 的盲隧道。**

1. **必须被明确要求。** `security.network.inspect_https` 默认 `false`。默认形态
   **逐字节等同于 ADR-0014 决定的形态** —— 没有 CA、没有证书环境变量、CONNECT 仍是
   `io.Copy` 双向盲转。这不是「先做了再说」的开关，是 ADR-0014 的决策仍然是默认决策。
2. **只记录判决用到的字段。** 审计行只有 host 与 method（`Proxy.audit`）。
   `netpolicy.Request` **在类型上就没有** path、header、body 三个字段 —— 想记录也
   没有东西可记录，这比一条「不要记录」的注释可执行。理由：query string 里放
   `?access_token=` 是最普遍的一种凭据传递，而 path 是审批对话框最想显示的东西。
3. **body 不落变量。** 解密后的请求经 `http.Client` 流式转发，响应经
   `resp.Write(conn)` 流式写回。本包没有任何一处 `io.ReadAll` 请求体或响应体。

## 三个信任边界问题

### 证书从哪来

进程自己签的一个 root，`netpolicy.LoadOrCreateCA` 在首次需要时生成：ECDSA P-256、
有效期一年、`MaxPathLen=0`（它只能签叶子，不能签出第二个 CA）。每个被访问的 host
现签一张 48 小时的叶子证书，只缓存在内存里。**过期的 root 直接重新生成**而不是报错
—— 它没有任何外部信任需要保全，报错只会让一年后的操作员对着一条要去查文档的信息。

**上游方向的证书校验没有被削弱**：代理连真实服务器时走 `p.client` → `PolicyDialer`
→ 系统根证书池。子进程失去的是「我自己验了对端证书」这件事，代理替它验，用的是
同一套根。

### 私钥存哪

`~/.yanshi/tls/ca-key.pem`，目录 `0700`，文件 `0600`。**每次加载都重新校验权限**：
`checkKeyPerm` 发现 group/other 可读就拒绝加载并重新生成一对，不会拿一把别人也能读
的 MITM root 继续用。Windows 上跳过这个检查并且**明说理由** —— Go 在 Windows 上报的
文件 mode 是从只读属性合成的，与真正管辖访问的 ACL 无关，断言 `0600` 得到的答案与
「谁能读这个文件」无关。

私钥**不进** store、不进 KV、不进任何会被 redactor 扫描的通道 —— 它从来不经过那些
路径，所以也不需要它们保护。

### 子进程怎么信任它

**只靠环境变量，而且只有 yanshi 亲手拉起的子进程能拿到。** `netpolicy.CAEnv` 发布
五个变量（`SSL_CERT_FILE` / `CURL_CA_BUNDLE` / `REQUESTS_CA_BUNDLE` /
`NODE_EXTRA_CA_CERTS` / `GIT_SSL_CAINFO`），由 `PrepareEnvFor` 写进
`childLaunchPosture.env` 产出的环境。

**这个 root 不进任何系统信任库、不进浏览器、不进 keychain。** 这是本条最重要的一句：
装进系统信任库会把「伪造证书」这个能力从「yanshi 的子进程」扩大到**这台机器上的
所有程序**，换来的只是 yanshi 自己子进程内部的请求可见性。代价与收益完全不成比例。

因此**信任边界恰好是「读这些环境变量的、由 factory 拉起的子进程」**，比 ADR-0014
描述的受管代理边界还窄一层。

## Consequences

### 哪些平台/客户端可被绕过 —— 必须如实说，不得写成无条件防线

| 位置 | 是否受本机制约束 | 理由 |
|---|---|---|
| Linux + landlock 后端 | **有内核兜底** | W-B-09 的 seccomp 只放行 `AF_UNIX`，裸 socket 出不去 |
| Linux + bubblewrap 后端 | **有内核兜底** | network namespace（比 seccomp 更强），但 `io_uring` 不在拦截面内 |
| darwin | **可被绕过** | Seatbelt 不做 socket 族过滤；无视环境变量、直接 `connect()` 的子进程完全不受约束 |
| Windows | **可被绕过** | 同上，且 Go 的 `crypto/tls` 在 Windows 上不读 `SSL_CERT_FILE`，所以 Go 写的子进程**连信任都建立不起来** |
| ACP / MCP / LSP 子进程 | **不经过** | 它们从 `os.Environ()` 建环境，不走 `childLaunchPosture` |
| 证书固定（pinning）的客户端 | **握手失败** | 这是**可见**的失败，不是静默回落 |

第三、四行是本 ADR 与 ADR-0014 补记里那段 seccomp 说明的直接推论：**代理在 linux
上有底，在 darwin/windows 上没有**。任何文档、UI 或台账都不得把 HTTPS 检查描述成
无平台限定的防线。

### 不可违反的约束

- **`netpolicy.Request` 不得新增 path / header / body / query 字段。** 那三个字段
  的缺席是 Decision §2 的机器可执行形式；加回来就等于删掉本 ADR 的核心让步。
- **`inspect_https` 的默认值不得从 `false` 改为 `true`。** 改默认值等于把
  ADR-0014 的决策从「默认形态」降级为「一个选项」，那需要另一条 ADR。
- **不得把生成的 root 装进任何系统信任库**，也不得提供做这件事的命令或文档指引。
- **审批对话框不得显示 URL 或 path。** `egressArgs` 只序列化 protocol/host/method，
  它是 `netpolicy.Request` 的忠实投影而不是另一个字段集。

### 已知代价

- 打开检查后，一个 pin 证书的子进程会**失去出网能力**（握手失败）。没有为它做旁路
  名单：一份 host 旁路名单会立刻变成「怎么让 X 绕过检查」的操作手册，而检查是操作员
  自己打开的，关掉它是一次配置编辑。
- 检查关闭时 `security.network.methods` 对 https 完全无效。这是「写了但没有读者」的
  形态，所以 bootstrap 在启动时**主动在 stderr 上说出来**，而不是让操作员从流量里
  发现。
- 逐域审批（W-B-16）在没有任何 WS 客户端连接时一律拒绝。headless 运行下这意味着
  只有 `security.network.allow` 里写明的 host 出得去 —— 这是 fail-closed 方向，
  与 ADR-0003 一致。
