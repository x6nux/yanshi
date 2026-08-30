# ADR-0025: 本地运行时发现是只读缓存，不是目录写入口

- 状态：accepted
- 日期：2026-08-30

## 背景（Context）

W-C-03（Ollama 深度集成）/W-C-05（LM Studio 集成）/W-C-06（发现结果磁盘缓存）/
W-C-15（多模态能力实测探针）是 capability-roadmap §5.2–§5.3 里共用同一条验收前提的
四项：它们都要回答"这台机器上的本地 runtime 现在实际有什么"，而不是"我们已经知道
某个模型 id 该怎么用"——后者正是 ADR-0024 刚刚建好的 `models.yaml` 目录在回答的问题。

四项动工前必须先想清楚一个问题：**发现（discovery）找到的信息，跟 ADR-0024 的目录表
是什么关系——覆盖它、补充它，还是只读？** 这不是风格选择，而是被一个技术事实预先
决定的：

- `models.yaml` 经 `//go:embed` 编译进二进制（`internal/llm/eino/modelcatalog.go`
  的 `mustParseModelCatalog`），**运行期结构性不可变**——没有任何路径能在进程存活期间
  往这张表里写一行，除非重新编译。任何"发现结果覆盖目录"的设计从第一天起就是在描述
  一个做不到的操作。
- 目录表回答的是**能力问题**："这个我们已经认识的模型 id，context window 是多少、
  怎么定价、压缩阈值是多少"——静态、可预先调研、随发布节奏更新。
- 发现要回答的是**库存问题**："这台机器上 Ollama 现在拉了哪些 tag、LM Studio 当前
  加载的是哪个、这个具体端点是否真的接受图片"——这类问题目录表结构上答不了：Ollama
  的模型空间是操作员随时 `ollama pull` 出来的任意字符串，不可能穷举进一个编译期表；
  同一个模型 id 在关闭视觉投影层的量化版本上是否吃图片，也不是"查表"能回答的，而要
  真的发一张图片去看回复。

被否决的替代方案：

- **发现结果自动写回 `models.yaml` 或在内存里覆盖已解析的目录索引。** 否决：
  `models.yaml` 是 embed 数据，运行期不可变，这条路径在技术上不存在；就算改成运行期
  可写文件，也会把"我们调研过认为可信的静态数据"与"这台机器这一次探测到的、可能是
  临时状态（模型正被卸载、daemon 刚重启）的动态数据"混进同一张表，污染其余无关会话
  与无关机器读到的目录。
- **发现结果自动写入 `ProviderConfig`（如把探测到的 `max_context_length` 自动填进
  `ContextWindow`）。** 否决：`ProviderConfig` 是操作员维护的配置文件，ADR-0024 已把
  它钉在覆盖梯子的最高优先级；一个后台探测进程静默改写配置文件，等于让"运行时自动
  发现"获得了本该只属于人工评审的最高优先级写权限——这与 CLAUDE.md 里 `fs.protected`
  一类"agent 不能靠自己重启加自己写的配置提权"的一贯姿态相反。人工看到发现结果后
  自己去改 `config.yaml`，走的是已经存在、已经被评审过的路径。
- **给发现结果单独定义一档比 `ProviderConfig` 更高的覆盖优先级。** 否决：ADR-0024
  的窗口/阈值梯子（显式配置 > 目录命中 > 全局回退）已经是承重约束，塞进第四层只为了
  安放一种从不确定是否仍然准确的运行时探测值，会让本该稳定的解析顺序多一个随时间/
  随进程重启漂移的输入源。

## 决策（Decision）

**发现是一个完全独立、只读的子系统：它从不读取、不写入、不导入 `modelcatalog.go`
派生的任何索引，落盘也落在与 `models.yaml` 无关的另一棵目录里。**

1. 四项共用一个接口：`Fetcher`（`internal/llm/eino/discovery_cache.go`），
   `FetchModels(ctx) (models []DiscoveredModel, etag string, err error)`。
   `OllamaClient`（W-C-03）与 `LMStudioClient`（W-C-05）都实现它；`Cache`（W-C-06）
   只依赖这一个接口，不知道背后是哪个 runtime。W-C-15 的探测结果（`ImageSupport`）
   走同一个 `Cache` 的第二组方法（`GetImageSupport`/`PutImageSupport`），写进同一份
   按 runtime 分文件的磁盘缓存——这就是 spec 里"探测结果回写缓存层"那句话的落点：
   写回的是**发现自己的缓存**，不是 `models.yaml`，也不是 `ProviderConfig`。
2. **"服务没启动"与"服务启动了但没有模型"必须在返回值里可区分**，不能只靠日志。
   `Fetcher.FetchModels`／`OllamaClient.ListModels`／`LMStudioClient.ListModels` 的
   契约是：非 nil `error` = 不可达/协议不可解析（"我们不知道它有什么"）；nil `error`
   + 可能为空的切片 = 可达，空切片是"还没拉过模型"这一合法状态，从不与不可达合并。
   这是对 `discover.go`（M9 preflight）现有缺陷的刻意背离：`FetchModelCatalog` 在
   `len(out)==0` 时返回 error，即使端点明明应答成功——"没有监听端点"与"监听端点
   返回零个模型"被压进了同一条错误路径。新代码不重复这个形状。
3. **发现数据结构上不可能覆盖目录，因为它连目录索引长什么样都不知道**——
   `internal/llm/eino/discovery*.go` 四个文件不引用 `modelcatalog.go`/
   `contextwindow.go`/`pricing.go` 定义的任何标识符，`DiscoveredModel` 与目录的
   `ModelEntry`（`modelcatalog.go`）是两个互不相关的类型，字段集合也不同
   （`DiscoveredModel` 记的是"这台机器现在有什么"，`ModelEntry` 记的是"这个 id
   该怎么用"）。**这句话原先写"不 import 任何符号"——七个文件全部同属
   `package eino`，同包引用一个包级标识符根本不需要、也不可能有 import 语句，"没有
   import 关系"因此从写下的第一天起就是真的、且永远为真，不是靠什么机制守住的边界，
   是在描述一件结构上不存在的事情。** 真正维持这条边界的是
   `internal/llm/eino/discoverycatalog_boundary_test.go`
   （`TestGOV_DiscoveryDoesNotReferenceCatalog`，C3 remediation 补的 L-5）：解析三个
   目录文件声明的每一个包级标识符（类型/变量/常量/非方法的顶层函数），解析四个发现
   文件里出现的每一个标识符，交集必须为空。方法名（如 `Ledger.Add`）刻意排除在
   目录标识符集合之外——方法从不能被裸标识符引用，只能通过某个值的 selector
   （`ledger.Add(...)`）调用，纳入方法名只会让发现侧一个同名但无关的顶层函数（两者
   分属不同命名空间，编译不会冲突）被误判成"引用了目录"。该测试用一次真实的
   正向探针验证过会变红：临时在 `discovery.go` 里加一行引用 `contextwindow.go` 的
   `DefaultContextWindow`，测试立刻报出确切的引用位置与目录声明位置，随后用 `cp`
   备份/还原撤销（未使用 `git checkout`/`git stash`）。
4. **人工桥接是唯一允许的桥接**：一个操作员看到 `yanshi` 探测出 LM Studio 某模型
   `max_context_length=32768`，自己去编辑 `config.yaml` 把它填进
   `ProviderConfig.ContextWindow`（ADR-0024 覆盖梯子里已经存在、优先级最高的那一层）
   ——发现子系统不做这一步，也不提供"自动应用"的开关。这一批（C3）不新增任何
   bootstrap 接线或自动执行这个搬运动作的 CLI 子命令，理由见后果段落。**C3
   remediation（I-1）之后新增的唯一消费点是 `internal/cli/doctorlocalruntimes.go`
   的 `checkLocalRuntimes`——一个只读诊断，随 `yanshi doctor` 这条已存在的、操作员
   显式敲的命令一起跑，报告发现结果并（对 LM Studio 已加载的模型）回写探测出的
   `ImageSupport` 到发现自己的缓存层；它不读也不写 `ProviderConfig`/`config.yaml`，
   不是本决策点说的"人工桥接"，那一步仍然要操作员自己动手。这与下面"不在
   `internal/bootstrap` 接线自动探测"是两件事：`yanshi doctor` 是操作员主动调用的
   诊断路径，不是进程启动时静默跑起来的 bootstrap 接线。

## 后果（Consequences）

- 发现的磁盘缓存落在 `os.UserCacheDir()/yanshi/discovery/`——与 `internal/lockfile`
  的 `os.UserCacheDir()/yanshi/run` 同级、同语义（可随时清空重建的缓存数据，不是
  用户需要保留的状态），与 `internal/cli/tui` 存偏好设置用的 `os.UserConfigDir()`
  是两个不同的根，因为发现结果的语义就是缓存而不是用户配置。
- **不可违反的约束**：**`internal/llm/eino/discovery*.go` 不得引用
  `modelcatalog.go`、`contextwindow.go`、`pricing.go` 里声明的任何标识符**（不限于
  导出的——七个文件同属一个包，同包访问跨越的正是包级可见性这一层，"导出"在这条
  边界上不是分界线），反之亦然——发现子系统与目录解析子系统之间不该有任何依赖，
  任何一侧的重构都不该波及另一侧。这条约束由
  `internal/llm/eino/discoverycatalog_boundary_test.go` 机器强制（见决策点 3），
  不是靠没有 import 语句这件事自然成立——同包本来就不需要 import 语句，那从来不是
  一道防线。谁要打通两者（例如让发现结果自动建议一条目录覆盖），必须先写一条
  新 ADR 论证为什么"人工桥接是唯一允许的桥接"这条决策被推翻。
- **不可违反的约束**：**`Fetcher.FetchModels` 与两个具体客户端的 `ListModels` 用
  `error != nil` 表达"不可达"、用"`error == nil` 但切片为空"表达"可达但为空"，两者
  永远不能合并成同一个返回值形状。** 这是 W-C-03/W-C-05 验收"服务不可用时如实报告"
  的机制落点；合并回单一布尔或单一错误会让调用方（未来的模型选择 UI、`yanshi doctor`
  一类诊断命令）无法区分"该装个 Ollama"和"Ollama 装了但还没 `pull` 过东西"。
- 本批不新增 `internal/config.Config` 字段、不新增 CLI 子命令、不在
  `internal/bootstrap` 接线自动探测——capability-roadmap 里明确要求 bootstrap 接线的
  是别的条目（如 W-C-11），这四条验收本身都不要求。对没有安装 Ollama/LM Studio 的
  用户，每次启动都去探测两个本地端口是纯粹的浪费；把发现做成一个库、由后续批次
  （若立项）决定何时调用，是这批交付的边界。**C3 remediation 是这样一个后续批次**：
  I-1 的裁决要求发现库必须有一个真实的生产消费点（否则整个子系统是没人读的死
  代码），选定的消费点是 `yanshi doctor`（`internal/cli`，见 `checkLocalRuntimes`）
  ——它复用已存在的诊断命令入口，不新增子命令，也不碰 `internal/bootstrap`：doctor
  只在操作员显式运行 `yanshi doctor` 时才探测，进程正常启动/对话循环仍然从不触碰
  这两个本地端口。上一段"每次启动都去探测两个本地端口是纯粹的浪费"这句话描述的
  正是 bootstrap 接线，doctor 路径不落入这句话的射程。
- 代价：发现结果目前只能被人工搬进 `ProviderConfig`，不存在"探测到就自动生效"的
  体验；这是刻意的（见决策点 4 的被否决替代方案），不是遗漏。
- **发现的出网请求完全在 `netpolicy` 之外（C3 remediation L-3(a)，结构性、不打算
  代码修复）**：`internal/netpolicy` 的代理/审计路径是按子进程环境变量生效的
  （`netpolicy.ManagedEnvWithPolicy`/`ScrubbedEnviron` 装配的是**子进程**看到的
  `HTTP_PROXY`/`NO_PROXY` 等变量），而发现子系统的 HTTP 调用（`OllamaClient`/
  `LMStudioClient`/`ProbeImageSupport`）全部是**进程内**的 `net/http` 调用，从不经过
  `exec.Command`，因此结构上没有环境变量可供 `netpolicy` 装配——它审计的是"yanshi
  起的子进程看见了什么环境"，不是"yanshi 自己的 goroutine 发了什么请求"。这不是
  疏漏而是两套机制管的是两件事：本文件已有的 `noProxyTransport`/`localHTTPClient`
  （见 `discovery.go`）已经保证发现请求**不**走操作员的 `HTTP_PROXY`（目标是
  `127.0.0.1` 一类本机 loopback 地址，代理反而是绕路甚至泄漏），这条边界与
  `netpolicy` 的出网策略正交，不是它的子集也不是替代品。若未来发现子系统需要接受
  `netpolicy` 式的 host allowlist（例如允许探测远程 Ollama），需要一条新 ADR 引入
  进程内 HTTP 客户端的策略钩子，而不是往这四个文件里塞子进程环境变量。

## 关联

- 来源：`docs/superpowers/specs/2026-08-27-capability-roadmap-design.md` §5.2
  （W-C-03/W-C-05/W-C-06 验收表）与 §5.3（W-C-15 验收表）；CLAUDE.md「本地 fork
  说明」之前「架构」章节暂未收录发现子系统，本 ADR 是它的落点。
- 相关代码落点：`internal/llm/eino/discovery.go`（共享类型、无代理 HTTP 客户端、
  多模态探针）、`internal/llm/eino/discovery_ollama.go`、
  `internal/llm/eino/discovery_lmstudio.go`、`internal/llm/eino/discovery_cache.go`。
- 相关 ADR：[ADR-0024](0024-model-catalog-is-data-the-ladder-is-code.md)（模型能力
  目录是数据，覆盖梯子是代码）——本 ADR 划的是发现子系统与那份决策之间的边界：目录
  回答能力问题、发现回答库存问题，两者结构上不相通，只能靠人工搬运。
