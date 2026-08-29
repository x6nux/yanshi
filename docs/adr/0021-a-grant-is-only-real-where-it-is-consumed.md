# ADR-0021: 授权只在被消费的那条路径上是真的

- 状态：accepted
- 日期：2026-08-29

## 背景（Context）

W-B-12 给模型加了 `request_permission`：撞墙之前先申请一条 (tool, path/host)，
人点头后写进 `approval.Manager`，后续同 scope 的调用不再弹窗。

评审复核的结论出乎意料：**五处放宽没有一处超出声称，风险全在反方向** ——
这条功能宣布发放了权限，实际什么都没发放，而模型和操作员都被告知发放了。
两个维度各有一种形态：

- **fs**：授权记录的是模型给的**原始字符串**，而每个真实 fs 工具先过 `FSTools.abs`
  的路径牢笼、再拿**解析后的绝对路径**调 `Authorize`。`approval.Manager` 用
  `reflect.DeepEqual` 匹配，于是四种拼法里三种失配。更糟的是其中两种（`../x`、
  `/etc/hosts`）**结构上不可能**被消费：牢笼在 `Authorize` **之前**，那条调用根本
  到不了 guard。工具的 description 卖给模型的正是「项目外的路径」。
- **net**：`guard.Action.NetHost` 全仓**唯一的生产者就是 `request_permission` 自己**。
  `web_fetch` 自 Task 11 起由 `netpolicy.Policy` 判 host，**从不查 approval manager**。
  用户批准了、模型收到 `granted=true`、下一次 `web_fetch` 的判决与批准前逐字节相同。

出厂守护测试没抓到，因为它**手搓** `guard.Action` 直接喂 `Authorize` —— 两端各自都对，
中间没接上。这是本仓 MEMORY 记的「写了但零读者」形态。

被否决的替代方案：

- **把牢笼挪到 `Authorize` 之后**，让项目外路径成为一个可批准的策略问题。否决：
  那会把一条结构性边界降格成策略问题，一次对话框就能换来整台机器的读权限。
- **下架 net 维度**。否决：spec 验收明写「net 与 fs 分维」。
- **在 `runFetch` 里只跳过自己那次 `CheckHost`**。否决：`netpolicy.NewTransport` 的
  dialer 每次连接都重跑 `CheckHost`，同一个静默失效只是下沉了一层。

## 决策（Decision）

**一条授权只有在「将要判决这次调用的那段代码」真的会读它时才可以被发放。**
落到三条具体规则：

1. **预测未来那次调用的 Action，必须与真实工具走同一段代码。** `FSTools.abs` 的函数体
   拆成包级 `resolveWithinRoot(root, path)`，`permissionActionFor` 调同一个函数。
2. **消费不了的申请当场拒绝，并把原因告诉模型，而不是发一条永不匹配的规则。**
   项目外路径、没有消费者的工具名、被 `security.network.deny` 点名的 host，三种都
   **不弹窗**直接拒。
3. **消费端返回的是「这次调用该用的策略」，不是一个布尔。** `netpolicy.Policy.GrantHost`
   返回放宽了一个 host 的**副本**，`web_fetch`/`web_search` 把它一路交给 transport，
   于是 dialer 看到的和工具看到的是同一份。

申请侧问的是**将要判决它的那个权威**：fs 问 permission profile，net 问
`netpolicy.Policy`。问 `guard.checkNet` 会得到一个没人执行的判决。

## 后果（Consequences）

- 四种 fs 拼法里能成的从 1 种变成 2 种（根内相对、根内绝对），另外两种从「假批准」
  变成「带理由的当场拒绝」。授权面**没有变宽**：能拿到的路径集合仍然是牢笼之内。
- net 维度第一次真的能用，且只放宽 host 规则一层。
- **不可违反的约束**：**路径牢笼（`resolveWithinRoot`）必须留在 `Authorize` 之前。**
  它是结构性边界不是策略；挪到后面会让 approval 能批准项目外的读写。
- **不可违反的约束**：**`security.network.deny` 里被点名的 host 不可由任何运行期对话框
  放行**（`GrantHost` 第二返回值为 false）。allowlist 与 default 表达的是「还没允许」，
  deny 条目是操作员点名说了不。
- **不可违反的约束**：**net 授权不得绕过 IP 层。** `GrantHost` 只往 `Allow` 里加一个
  名字，`CheckResolvedIPs`（SSRF 防线）照跑 —— 被批准的 host 解析到 169.254.169.254
  仍然拒。
- **不可违反的约束**：**守护测试必须驱动真实工具。** 手搓 `guard.Action` 喂 `Authorize`
  证明的是「两端互相同意」，不是「中间那段会产出这个 Action」——
  `internal/tools::TestGrantedPermissionAdmitsTheLaterCall` 与
  `internal/tools::TestNetGrantAdmitsTheLaterFetch` 都跑真实工具，删掉任一消费点即红。
- 代价：`request_permission` 现在要认识两个权威、两套「已经允许」的判据，
  `preflightRequest` 是那段分叉。合成一个「统一权限查询」会把两个真实不同的
  enforcement 层伪装成一个。

## 关联

- 来源：W-B 第四批评审 Blocking-1 / Blocking-2；CLAUDE.md「Guard —— 安全关键、fail-closed」。
- 相关代码落点：`internal/tools/requestpermission.go`、`internal/tools/fs.go`、
  `internal/tools/web.go`、`internal/netpolicy/policy.go`。
- 相关 ADR：[ADR-0003](0003-guard-fail-closed-empty-allow.md)（fail-closed 的档位语义）。
