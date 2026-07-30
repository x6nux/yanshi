# 技能（Skills）

技能（skill）是 yanshi 的可复用能力单元：一段带元数据的指令（SKILL.md），yanshi 在合适的时候发现并调用它。技能系统是分层的（T0–T4），支持内置 / 用户 / 插件三级发现。

## 技能放哪

技能从三个目录发现（由 [configuration.md](configuration.md) 的 `skills` 块配置）：

- `skills.builtin_dir`（默认 `./skills`）：仓库内置技能（如分层开发技能 T0–T4）。
- `skills.user_dir`（默认 `~/.yanshi/skills`）：你的个人技能。
- `skills.plugin_dir`（默认 `~/.yanshi/plugins`）：插件技能。

## 怎么写 SKILL.md

一个技能就是一个目录，内含一个 `SKILL.md`。SKILL.md 的 frontmatter 声明 `name`、`description`、何时触发；正文是指令。

> **权威细节看 [../skills-authoring.md](../skills-authoring.md)**，本页只给用户面概述，不复制编写规范。

## 渐进披露

技能采用渐进披露（progressive disclosure）：模型先看到 `description` 决定要不要用，决定用才读完整 instructions。所以 `description` 要写清"什么情况下用这个技能"，正文才写"怎么用"。

## 在 TUI 里用

`/skill` 命令调用一个技能。goalloop 的分层开发（见 [goalloop.md](goalloop.md)）按 tier 选用对应的 T0–T4 技能。
