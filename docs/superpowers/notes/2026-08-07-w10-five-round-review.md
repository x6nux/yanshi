# W10 五轮评审记录（2026-08-07）

W10 十个任务完成后按 `docs/superpowers/review-checklist.md` 跑的五轮。
**独立评审 subagent 配额（200/200）在本会话仍然耗尽，五轮全部是主循环自评**
—— 与 W7/W8/W9 同样的限制。

## R1 — 配置缝（一条，未修而是记录）

**覆盖率阈值全部在 darwin/arm64 上实测，而 CI 跑 ubuntu。**
`internal/bootstrap` 有平台相关分支（sandbox 降级路径是 Seatbelt 专属），
它那 3.2pp 余量是三个数字里唯一没在将要执行它的平台上验过的。
写进 `cmd/covercheck::thresholds` 的注释，连同「首次 CI 红了要调阈值而不是
删 job」。**这条不能靠本地验证解决，只能诚实记录。**

顺带核实无缝的几处：`nokeyring` 标签下三个包覆盖率不变；
`examples/*/run.sh` 不污染工作树（`/yanshi` 与 `config.yaml` 都在 gitignore）。

## R2 — 门禁正反探针（一条空壳）

**`TestLicenseShipsWithTheBinary` 删掉 `.goreleaser.yaml` 里的 LICENSE 行
依然绿** —— 它命中的是那一行**上方解释为什么要有它的注释**。

> **这是第三次同款**：W7 的 bench `continue-on-error`（短语在 job 上方注释里）、
> W10-T4 的 `^feat!`（引用在修复说明里）、以及这条。
> 复用已有的 `withoutComments` helper 修掉。

其余探针一次命中：

| 探针 | 结果 |
|---|---|
| README 丢一个入口链接 | 红 |
| CONTRIBUTING 丢一个工具名 | 红 |
| SECURITY.md placeholder 回归 | 红 |
| go.mod 去掉 bubbletea fork replace | 红 |
| ci 的 fuzz 逃生口回归 / bench job 级软门被删 / fuzz-long 软门回归 | 各自红 |
| CHANGELOG 加回 `--latest` / RELEASE_NOTES 丢 `--latest` | 各自红 |
| cliff breaking 规则退回 `^feat!` / 删掉 `^style` | 各自红 |
| covercheck 阈值提到不可达 | 红 |
| SSEEvent 输出加后缀 | 红 |
| bootstrap 的 WorkRoot seam 拆回硬编码 cwd | 红 |

**门禁自身在本包返工两次**：`continue-on-error` 那条第一版用 `Contains`，
而 nightly 的 bench job **两级都有**（job 级软 + 一个软步骤），删掉 job 级
那条它依然绿；改成按缩进只认 job 级键。

## R3 — 文档虚报（三处，同一个不实描述）

`build.sh`、`docs/upgrade-guide.md`、`docs/feature-status-audit.md`（两处）
都写着 `--match 'v[0-9]*'` 是为了「跳过 m1..m9 里程碑 tag」——
**那批 tag 从不存在**，本仓库 `git tag | wc -l` = 0。
match 模式本身仍是对的（它挡未来的非 semver tag），但它挡的不是任何既有对象。

W9-R3 的三处（sdk-ts.md ×2、resources.md、CLAUDE.md）已在上一包修掉。

## R4 — 边界与状态（一条）

**示例覆盖判据太宽。** 它扫全部 `_test.go` 找 `examples/<dir>`，于是任何
**注释里**提到那个路径的测试都会让该示例免检 —— 松的那个方向：示例可以没人跑
却看起来被跑了。改成显式登记表，并补一条断言检查表里引的测试真实存在。

## R5 — 台账证据逐句复核（一条）

**`H2/UDOC1#2「getting started 可零依赖跑通」`引的是 `cmd/gendocs` 的配置
骨架测试** —— 那测的是骨架与 `config.example.yaml` 顶层 key 的对账，
一件真实但**无关**的事实。改成「指南里的零依赖步骤就是 CI 真正跑的那几条」，
两头都钉（指南里必须还有那几步，且必须有 workflow 跑它们）。

> 与 W8-R5、W9-R5 同一形状：**证据落在「零件对不对」上，子句问的是「产品
> 做不做得到」**。连续三轮各抓到 1-3 条，GOV8 一条都拦不住 —— 这正是
> ADR-0011 写明的边界。

## 本包最值得记的四件事

1. **发布流水线第一次真跑通。** 本仓库一个 tag 都没有，`release.yml` 从未
   触发过，`goreleaser check` 只验配置能解析。本地跑 snapshot 逐条核验：
   4 归档 / 4 checksum / 归档含 LICENSE / 解包后 nokeyring 二进制 `-h` 退 0。
   最后一条是那个构建配置唯一的真实冒烟。

2. **两个逃生口在 shell 里而不是 YAML 键上。** `soft-pass; exit 0` ——
   删光所有 fuzz/property 目标，两个 job 照样绿。只摘 `continue-on-error`
   关不掉它，而没有任何门禁能看见 shell 里的这种写法。

3. **两条 COV 的真实缺口不是覆盖率**（实测 97.8% / 94.2%，验收要 ≥80% / ≥50%），
   是三条**不可能失败**的断言：golden 没覆盖它自己冻结的词表、
   `TestVocabulary_Symmetry` 比较的是被测函数刚复制的值、
   `TestBuild_VCSSoftDegrade` 从没跑过它命名的那条分支。

4. **计划的论断被探针证伪一次。** 计划说缺 `^style` parser 会让那条提交
   「在 CHANGELOG 里凭空消失，不报错」；实测 git-cliff 2.6.1 会从 type
   自己推导组名，发一个游离的 `### Style` 段。真实缺陷更轻也仍值得修，
   **注释按实测结论写，不抄计划那句**。

## ⚠️ S0 未完成

`go run ./cmd/featurestatus`：**63 条里 33 条终态（52%）**。

| 包 | 终态 |
|---|---|
| W1 | 0/9 |
| W2 | 1/4 |
| W3 | 0/5 |
| W4 | 1/2 |
| W5 | 4/4 |
| W6 | 0/11 |
| W7 | 7/7 |
| W8 | 8/8 |
| W9 | 4/5 |
| W10 | 7/7 |

W1/W3/W6 与 W2/W4 的大部分**实现工作在更早的会话里完成了**，但台账逐句对账
没跟上 —— W6 Task 12 的 11 条翻牌明确记录为「未做，每条阻塞点已逐条标注在
台账里」。**剩余工作是 30 条的逐句证据对账**，那不是形式主义：W8/W9/W10 三轮
R5 各抓到 1-3 条「引对了包、引错了事实」，而 GOV8 一条都拦不住。

`TestS0CompletionGate` 会在终态 == 63 时硬断言三张豁免表全空 —— 现在还到不了
那个条件。

## 未覆盖 / 移交

- **30 条台账翻牌**（W1/W2/W3/W4/W6/W9 各有未结项）→ 下一步工作。
- `D2/V15` 卡在子句 1 的 `stream`：D1 未提供 `/api/v1/threads/{id}/stream`。
  **需要判定是补端点还是改验收**。
- WS/SSE 附件历史分岔（W8-R4 发现，W9 移交）→ 仍未做，需要 wire 契约变更。
- W6-R5 移交的 7 个幻影帧构造器。
- `G/VISION-TOOL` 最后一条子句（没有测试把带图像的一轮跑到成本上）。
- `internal/agent/spawn` 整包 `//go:build ignore`（W9-T9 发现）——**是留是删未定**。
- ubuntu 侧覆盖率阈值复核（首次 CI 运行后）。
- **独立评审仍为零。** 需要提高 `CLAUDE_CODE_MAX_SUBAGENTS_PER_SESSION`
  或换新会话。
