---
name: browser-automation
description: Drive a real browser via the agent-browser CLI — navigate pages, click by element refs, fill forms, screenshot, and extract data. Requires `agent-browser` on PATH (installed via `npm i -g agent-browser && agent-browser install`).
---

# 浏览器自动化（agent-browser）

用无障碍树快照 + `@eN` 元素引用驱动真实 Chrome/Chromium，专为 agent 设计。

## 前置

- `agent-browser` 在 PATH（本机已装：`npm i -g agent-browser && agent-browser install`）
- daemon 卡死/连接失败时：`pkill -f agent-browser` 后重试

## 核心循环

```bash
agent-browser open <url>        # 1. 打开页面
agent-browser snapshot -i       # 2. 只看可交互元素（拿 @eN 引用）
agent-browser click @e3         # 3. 按引用操作
agent-browser snapshot -i       # 4. 页面变化后引用会过期，必须重抓
```

## 常用命令

```bash
agent-browser open <url>              # 导航（goto/navigate 别名）
agent-browser read [url]              # 直接读页面正文（agent 可读文本）
agent-browser snapshot -i -u          # 快照带链接 href
agent-browser fill @e5 "text"         # 清空后填入
agent-browser type @e5 "text"         # 追加输入
agent-browser press Enter             # 按键（Tab / Control+a / …）
agent-browser get title|url|text @eN  # 取属性
agent-browser eval "js"               # 跑任意 JS
agent-browser screenshot out.png      # 截图（--full 全页）
agent-browser wait @e3 | --text T | --load networkidle
agent-browser tab list|new <url>|<n>  # 多标签
agent-browser close                   # 收尾（daemon 常驻，必须显式关）
```

## 经验

- 引用 `@eN` 在页面变化后立即失效 —— 每次操作前重新 snapshot
- 反爬强的站（Google 搜索、X、Cloudflare 站）此工具会被拦，需升级到 stealth 浏览器
- Bash 里 grep 日志时注意 `--` 分隔（`grep -e "--- FAIL"`）
