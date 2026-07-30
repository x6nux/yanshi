# 归档文档（docs/archive）

这里存放**历史分析报告**——yanshi 在 v0.4.0 阶段做的多源综合分析、功能差距对比、依赖分析等一次性快照。它们是当时决策的依据，但**不是当前态的权威描述**。

> **权威当前态见根目录 `CLAUDE.md`；架构决策与不可违反的约束见 [`../adr/`](../adr/)。** 本目录的文档保留为历史档案；其中的代码现状、行数、覆盖率等数字反映的是成文时的快照，可能已过时。

## 原路径 → 新路径

| 原路径 | 新路径 |
|---|---|
| `docs/analysis-report.md` | [`analysis-report.md`](analysis-report.md) |
| `docs/feature-comparison-with-codex.md` | [`feature-comparison-with-codex.md`](feature-comparison-with-codex.md) |
| `docs/feature-roadmap-codex-deepseek.md` | [`feature-roadmap-codex-deepseek.md`](feature-roadmap-codex-deepseek.md) |

## 说明

- 这些报告的**决策结论**已被结构化提炼进 [`../adr/`](../adr/)（ADR-0001..0010 从综合报告 §9 提炼）。归档的是"分析快照"，**活的决策在 ADR**。
- 综合报告（`synthesis-final.md` / `synthesis-report.md` / `synthesis-report-v2.md`）与依赖分析（`deps_analysis.md` / `deps_raw.txt`）在本批归档时为未跟踪文件，未进入仓库历史，故不在本目录。
