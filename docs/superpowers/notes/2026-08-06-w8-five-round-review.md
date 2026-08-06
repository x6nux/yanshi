# W8 五轮评审记录（2026-08-06）

W8 八个任务完成后按 `docs/superpowers/review-checklist.md` 跑的五轮。
**独立评审 subagent 配额（200/200）在本会话仍然耗尽，五轮全部是主循环自评**
—— 与 W7 同样的限制，同样记在最前面。

七条修复，全部是本工作包自己当天提交引入的。

## R1 — 配置缝（两条阻塞）

1. **`tui.frecency` 关掉后 frecency 文件照写。** 禁用只挡住了排序，
   `recordAtPath` 仍在每次补全后落盘 —— 用户关掉的是「记录我打开过哪些文件」，
   得到的是「照样记录，只是不用」。

2. **project 层 keymap 绑定被 `/keymap reset` 清掉后，重启即回来。**
   墓碑（`KeymapReset`）当时只清 user 层的 name，不阻断 project 层，
   所谓 reset 的寿命是到下次启动为止。

## R2 — 门禁正反探针（一条）

`internal/archtest::TestPhantomSlashCommandsNotAdvertised` 的四个幻影名
本轮全部毕业（`/keymap` `/vim` `/contrast` `/locale` 真被实现），
`phantomSlashCommands` 清空。**清空后这道门禁不再有活样本**，所以补跑了
反探针：往表里塞一个假名、在 `config.example.yaml` 里提一次 —— 变红。
表清空是目标态，不是门禁失效。

其余探针（帮助面板裁剪、@ 补全项目隔离、attachment fail-closed）都能被
对应变异打红。其中帮助面板的头两次探针是**假绿**：第一次只删了锚点计算，
裁剪由收缩循环兜住；第二次删收缩循环，锚点又在两处各算了一遍。
合并成单一 `helpStart` 之后才是真探针。

## R3 — 文档虚报（两条）

1. **计划书声称 i18n 目录已收齐所有 TUI 字符串。** 实测 14 个里有 1 个。
   照着计划接线会得到一个「已本地化」的界面，13 条硬编码英文。
   补齐 `en.json` / `zh-Hans.json` 与 `requiredCatalogKeys`。

2. **`internal/keymap` 的默认绑定表是编造的。** `ctrl+k` 写成 `scroll_up`，
   而 TUI 用 `ctrl+k` 开动作面板。按原样接线会静默搬走三个键 ——
   一个从没被消费的包，它的「默认值」谁都没验证过。改正默认表，
   并让分发只路由**被重绑过**的键。

## R4 — 边界与状态（两条）

1. **WS 与 SSE 的附件历史分岔。** 服务端把 `@path` 展开进交给模型的那条
   消息；WS 的历史在服务端，展开后的文本留在里面，SSE 的历史在客户端、
   进去的是裸文本，于是文件内容只存在一轮，第二轮模型看不见自己刚读过的
   文件。收口要服务端把展开后的文本回 publish 给客户端（SSE 已有的
   `history_replaced` 帧是现成载体），属 wire 契约变更，**移交 W9**。
   行为已由 `internal/api/http::TestSSEAttachmentIsNotKeptInClientHistory` 钉住。

2. **`command.helpKey` 只有一个消费者。** struct 的 doc 注释声称 `/help`
   与 palette 都渲染本地化文案 —— **两个都没有**：F1 面板与 `/` 菜单读的
   都是静态英文字段，只有 `/help` 记录条走了 helpKey。`/locale zh-Hans`
   得到三分之二英文的界面，而 D3/I18N1 就是在这个表面上翻的 done。

## R5 — 台账证据逐句复核（三条）

按「这条测试证明的是不是这条子句」重读 W8 的 7 条新翻牌，三条引错：

1. **`C2/UX4#2 衰减合理`** 引的测试一次都没推进时钟。计数排序在任何
   衰减函数下都成立，包括完全不衰减。
2. **`C3/E03#4 模型可 load 匹配技能`** 引的是「TUI 发出 list_skills 帧」
   —— 那是用户列表，不是模型加载。真实路径（`MetaPrompt` 进 system prompt
   → 模型调 `skill_use` 取正文）两端各有测试，只是没被引。
3. **`D3/C15#1 核心按键可重映射`** 只引了 keymap 包内的查表测试。
   必要而不充分：`internal/keymap` 曾经是零生产消费者的完整叶子包，
   **一张没人查的正确表不重映射任何东西**。补端到端并带无绑定负对照。

三条的共同形状：**证据落在「零件对不对」上，子句问的是「产品做不做得到」**。
GOV8 拦得住「引用解析不开」，拦不住「引对了包、引错了事实」—— 这正是
ADR-0011 写明的边界，也是这一轮必须由人读的理由。

## 反复出现的形状（本轮）

- **「造好了没装上」**：本轮的主线。`keymap.VimMachine`、`Frecency.TopN`、
  `mergeTUIPrefs`、`buildSendFrame` 全是完整、有测试、零生产调用点。
  R3 第 2 条是它的加强版 —— 没被消费的包，连它的默认值都是错的，
  因为从来没有任何东西验证过。
- **「测试把缺陷钉成预期行为」**：`TestCov_HelpModal` 把「帮助面板忽略方向键」
  写成断言。
- **「没有正对照的否定断言」**（W7 的教训）本轮以 R5 第 1、3 条的形式复发：
  断言在一个恒真的维度上成立。

## 未覆盖 / 移交

- WS/SSE 附件历史分岔 → **W9**（wire 契约变更）。
- W6-R5 移交的 7 个幻影帧构造器仍在 W9 待办。
- `G/VISION-TOOL` 最后一条子句（没有测试把带图像的一轮跑到成本上）→ **W10**。
- W6 Task 12 的 11 条台账翻牌仍未做，每条阻塞点已逐条标注在台账里。
- **独立评审仍为零。** 需要提高 `CLAUDE_CODE_MAX_SUBAGENTS_PER_SESSION`
  或换新会话。
