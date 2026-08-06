# 归档文档（docs/archive）

这里存放**历史分析报告**——yanshi 在 v0.4.0 阶段做的多源综合分析、功能差距对比、依赖分析等一次性快照。它们是当时决策的依据，但**不是当前态的权威描述**。

> **权威当前态见根目录 `CLAUDE.md`；架构决策与不可违反的约束见 [`../adr/`](../adr/)。** 本目录的文档保留为历史档案；其中的代码现状、行数、覆盖率等数字反映的是成文时的快照，可能已过时。

## 原路径 → 新路径

| 原路径 | 新路径 |
|---|---|
| `docs/analysis-report.md` | [`analysis-report.md`](analysis-report.md) |
| `docs/feature-comparison-with-codex.md` | [`feature-comparison-with-codex.md`](feature-comparison-with-codex.md) |
| `docs/feature-roadmap-codex-deepseek.md` | [`feature-roadmap-codex-deepseek.md`](feature-roadmap-codex-deepseek.md) |
| `docs/feature-roadmap-e-h.md` | [`feature-roadmap-e-h.md`](feature-roadmap-e-h.md) |
| `docs/synthesis-final.md` | [`synthesis-final.md`](synthesis-final.md) |
| `docs/synthesis-report.md` | [`synthesis-report.md`](synthesis-report.md) |
| `docs/synthesis-report-v2.md` | [`synthesis-report-v2.md`](synthesis-report-v2.md) |
| 仓库根 `deps_analysis.md` | [`deps_analysis.md`](deps_analysis.md) |
| 仓库根 `deps_analyze.py`（其实是数据快照，不是脚本） | [`deps_raw-2026-07-20.txt`](deps_raw-2026-07-20.txt) |

## 说明

- 这些报告的**决策结论**已被结构化提炼进 [`../adr/`](../adr/)（ADR-0001..0010 从综合报告 §9 提炼）。归档的是"分析快照"，**活的决策在 ADR**。
- **上一版这里写着**「综合报告与依赖分析在本批归档时为未跟踪文件，未进入仓库历史，故不在本目录」——**两句都假**：三份 synthesis 报告当时就在本目录且已被 git 跟踪（`git ls-files` 可复现），`deps_analysis.md` 也一直被跟踪，只是躺在仓库根没归档。W10 把它连同 `deps_analyze.py`（那其实是一份 `key=value` 数据快照，不是可执行脚本）一并移了进来，映射表也补齐到目录实际内容。
- 依赖数字请用 `go run ./cmd/depsanalyze` 现算。`deps_raw-2026-07-20.txt` 是 2026-07-20 那一刻的冻结记录，**不可重生成**：它自称由 `cmd/pkganalyze` 产出，而那个命令从未存在过。
