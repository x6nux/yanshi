---
name: computer-control
description: See and operate the macOS GUI — capture the screen, read window contents, click buttons, and type into password dialogs. Requires CuaDriver with Accessibility + Screen Recording permissions granted.
---

# 电脑 GUI 操作（computer use）

当任务只能通过图形界面完成（系统设置授权弹窗、安装器密码框、无 CLI 的应用），用屏幕捕获 + 合成鼠标键盘事件驱动 GUI。

## 前置

- CuaDriver.app 已获 **辅助功能** + **屏幕录制** 权限（系统设置 → 隐私与安全性）
- 权限变更后需重启 CuaDriver 进程才生效

## 能力

- **capture**：截屏 + 无障碍树（每个可交互元素带索引和坐标）
- **click / double_click / right_click**：按元素索引或坐标点击
- **type / key**：输入文本、按键组合（cmd+q、Enter…）
- **scroll / drag**：滚动与拖拽
- **set_value**：直接设置输入框/滑块的值（不偷焦点，优先用）

## 密码框

需要管理员授权的弹窗可以直接输入凭据。纪律：

- 只在密码框存在时输入；输入动作本身不回显明文到任何日志
- 绝不触碰"辅助功能/录屏"列表里 CuaDriver 自己的开关 —— 那是自我断肢
- 触发测试弹窗优先用 `osascript -e 'do shell script "…" with administrator privileges'`

## 流程

1. capture 看当前屏幕，找到目标元素索引
2. 用元素索引点击/输入（比裸坐标稳）
3. 操作后 re-capture 确认效果，再决定下一步
4. 弹窗中的取消/关闭按钮优先于重试
