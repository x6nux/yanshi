# custom-skill

一个自定义技能的目录结构样例：`reverse-echo/SKILL.md`。无代码编译。

## 结构

```
custom-skill/
└── reverse-echo/
    └── SKILL.md      # frontmatter (name/description) + instructions
```

SKILL.md 的 frontmatter：

- `name`：技能的唯一标识（kebab-case）。
- `description`：**渐进披露**的关键——模型先读它决定要不要用这个技能，所以写清"什么情况下用"。

正文是 instructions（模型决定用之后才读）。

## 让 yanshi 发现它

把技能目录放到一个发现路径下（见 [../../docs/user-guide/skills.md](../../docs/user-guide/skills.md)）：

- 用户技能：`~/.yanshi/skills/reverse-echo/SKILL.md`（`skills.user_dir`，默认）。
- 或在 `config.yaml` 里把 `skills.user_dir` 指到本目录：
  ```yaml
  skills:
    user_dir: ./examples/custom-skill
  ```

然后启动 yanshi，`/skill` 即可调用 `reverse-echo`。

> 编写 SKILL.md 的权威细节见 [../../docs/skills-authoring.md](../../docs/skills-authoring.md)；本示例只给最小骨架。
