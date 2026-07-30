# Yanshi 重命名设计

> 状态：命名方向与"全局 rename"范围已于 2026-07-19 与用户确认。本 spec 待 review 批准后，进入 writing-plans 产出逐文件迁移计划。

## 1. 背景与命名决策

当前项目名 `autocode`（module `github.com/x6nux/autocode`，binary `autocode`）被认为过于泛化、缺乏辨识度。经多轮头脑风暴（含撞名调查，见 §3），定为 **Yanshi（偃师）**。

**典故**：偃师出自《列子·汤问》，上古工匠，造出能歌舞的"倡者"（自动人偶）献给周穆王；剖开只见"革、木、胶、漆、白、黑、丹、青"，内别无他物 —— 世界最早的"自动机械 / 机器人"传说之一。

**为何贴合**：autocode 的本质是"造会自己动的编码 agent"（自驱动 goal loop、ReAct 编排、子代理委派、ACP 拉起外部 agent）。偃师造的东西"自己会动"，名字与产品语义近乎直译。同时偃师是少数未被大厂占用的中国好典故（盘古、仓颉、麒麟、鲲鹏、祝融均已被占用）。

## 2. 命名规范

| 用途 | 写法 |
|---|---|
| CLI 命令 / Go module path / 配置目录 | `yanshi`（全小写） |
| 品牌名 / 文档标题 | `Yanshi` |
| 中文 | 偃师 |
| 发音指引 | `yan-shee`（/jænˈʃiː/） |

**排除变体**：
- `Yanshee` —— 撞优必选教育机器人 Yanshee（中文亦称偃师）
- `Yan-shi` —— 连字符难打
- `Yenshi` —— 威妥玛拼音，丢失标准拼音读音

**Tagline**：
> **Yanshi — the self-driven coding agent.**
> 偃师 —— 自驱的编码 agent

**README 顶部声明**（建议）：
> Named after 偃师 (Yǎnshī), the legendary artisan who built an autonomous automaton in 《列子·汤问》. Not affiliated with [chaitin/yanshi](https://github.com/chaitin/yanshi).

## 3. 撞名评估

### 3.1 最终选定：Yanshi

- **chaitin/yanshi**（长亭科技，2018 开源）：100 stars、C++、类 Ragel 的有限状态自动机生成器，不活跃。**与本项目同典故**（其 README 亦引"3rd century BC automata"），但领域（安全 / 状态机）与本项目（LLM coding agent）完全不重叠，英文社区基本不认知。**评估为可控撞名**，用 README 声明 + tagline 锁定定位即可缓解。
- 经一轮针对 `Hetu` 的深度调查（见 3.2），反证 Yanshi 的相对干净 —— 偃师是这批中国典故里语义最准且撞名最可控的选项。

### 3.2 已排除的备选

| 候选 | 排除原因 |
|---|---|
| **Hetu（河图）** | 严重撞名：北大 PKU-DAIR/Hetu（分布式深度学习，SCIS 2022 论文，被引 28）、华为 openLooKeng `hetu-core`、链家 LianjiaTech/hetu（低代码，占 GitHub org `hetu`）、Apache Hetu；外加芬兰语 `hetu` = henkilötunnus（个人身份码）造成西方语义污染 |
| **Baize（白泽）** | 撞开源 LLM chatbot 框架 Baize |
| **Cangjie（仓颉）** | 撞华为 2024"仓颉编程语言" |
| **Nuwa（女娲）** | 撞微软 NÜWA 文生视频模型 |
| **Modi（墨翟）** | 撞印度总理 Modi（同名） |
| **Pangu / Kunpeng / Qilin** | 撞华为大模型 / 芯片 / 银河麒麟 |

## 4. 范围：全局 rename

### 4.1 决策

**全局 rename**，非"代号 only"。理由：
- 项目 v0.4.0，用户极少，现在是改名成本最低的窗口；
- binary 命令必须叫 `yanshi`，名字才能真正落地（用户每天打的就是这个命令）；
- 只改代号、保留 `autocode` 命令与 `~/.autocode/` 路径会造成认知分裂，名字白改。

### 4.2 包含的维度

基于真实盘点：`Grep "autocode"` = **699 处 / 161 文件**；`Grep "ACODE"` = **46 处 / 11 文件**（env 前缀 `ACODE_*` 主要分布在 `cmd/autocode/main.go`、`internal/acp/*`、`internal/agent/goalloop/*`，另含 `cov2.html` 等未跟踪无关项）。

1. **Go module**：`go.mod` 的 `module github.com/x6nux/autocode` → `github.com/x6nux/yanshi`，触发全量 `import` 替换（占 occurrences 大头，机械替换）。`go mod tidy` 重算 `go.sum`。
2. **主 binary**：`cmd/autocode/` → `cmd/yanshi/`（构建命令变为 `go build -o yanshi ./cmd/yanshi`）。
3. **数据 / 配置目录**：`~/.autocode/`（含 lockfile、`worktrees/`、`*.db`）→ `~/.yanshi/`。
4. **环境变量前缀**：`ACODE_*`（如 `ACODE_E2E`）→ `YANSHI_*`。注意前缀是 `ACODE`（autocode 缩写），**不是** `AUTOCODE`。
5. **配置文件**：`config.example.yaml`（跟踪）与 `config.yaml`（gitignored）中的 "autocode" 字面量。
6. **文档（活）**：`README.md`、`CLAUDE.md`、`docs/vcs.md`、`docs/skills-authoring.md`、`docs/feature-comparison-with-codex.md`、`docs/analysis-report.md`、`skills/dev-team-feature/SKILL.md` 等。
7. **UI / 字符串字面量**：所有人机可见文本中的 "autocode"。

### 4.3 不在本次范围（明确排除）

- **git 历史不重写**：历史 commit message、旧版文件内容保持原样；仅保证后续 commit 使用新名。
- **历史 spec / plan 文档不改**：`docs/superpowers/specs/` 与 `docs/superpowers/plans/` 下日期早于本 spec（2026-07-19）的文档是历史设计记录，记录的是当时状态，**不回溯改名**。本 design doc 及之后新建的文档用 Yanshi。
- **未跟踪的临时 / 分析文件不碰**：工作树中的 `_analyze_cov.go`、`cov2.html`、`pkglist.tmp`（`git status` 中 `??`）不在 rename 范围内。
- **`agent-worker` binary 名保留**：`cmd/agent-worker/` 本就不叫 autocode，仅其内部 import / 字符串随 module 改名而更新。

### 4.4 用户本地数据迁移（决策点）

改 `~/.autocode/` → `~/.yanshi/` 后，已存在的本地数据（worktrees、db、lockfile）将"失联"。

**推荐：本次不做迁移。** 理由：v0.4.0 早期、用户极少、worktrees / db 可重建。旧 `~/.autocode/` 留在原地（不删），用户可手动迁移或弃用。若后续需要，可加一个启动时检测旧目录并提示的 helper —— 列为 future nice-to-have，**不进本次 scope**（YAGNI）。

## 5. 迁移策略（设计层）

逐文件执行清单由 writing-plans 产出，此处只定策略与门禁。

### 5.1 替换分类与注意点

- **import / module path**：改 `go.mod` 后全量文本替换 `github.com/x6nux/autocode` → `github.com/x6nux/yanshi`，再 `go mod tidy`。
- **分类替换**：import path、字符串字面量（`"autocode"`、`".autocode"` 等）、路径常量、env 前缀（`ACODE_`→`YANSHI_`）、UI 文本、文档 —— 按类别分批处理，避免误伤。
- **大小写敏感**：`autocode`（小写：命令 / 路径 / module）与 `Autocode`（标题）需分别处理；env 前缀 `ACODE` 是大写缩写、单独处理为 `YANSHI`。
- **`replace` 指令不受影响**：`replace github.com/charmbracelet/bubbletea => ./third_party/bubbletea` 与 module 名无关，保持不动。

### 5.2 验证门禁

每批替换后：
1. `go build -o yanshi ./cmd/yanshi` —— 编译通过
2. `go vet ./...` —— vet 通过
3. `go test ./...` —— 全量测试通过（带 `e2e_real` tag 的 skip 为预期，见 CLAUDE.md 测试门禁说明）
4. 启动自检：`./yanshi -h` 退出 0；`timeout 5 ./yanshi --fake-model -inprocess` 跑通
5. subcommand 可用：`./yanshi vcs-mcp`、`./yanshi goal` 等

### 5.3 风险点

- **外部引用**：若有其他仓库 `import github.com/x6nux/autocode`，改名后会断。v0.4.0 早期，预期无外部依赖；若担心可保留临时重定向 module，**本次不做**。
- **文档内自指链接**：README / docs 中指向自身 repo 路径的链接需同步。
- **`go.sum`**：module path 变更后由 `go mod tidy` 自动重算。

## 6. 后续

- 本 spec 经用户 review 批准后，调用 **writing-plans** skill 产出逐文件、可执行的迁移计划（按 §4.2 维度分批，附每批验证步骤）。
- 迁移执行完成后，落地 README 顶部声明、"Why Yanshi?" 小节与 tagline。
