# tuidbg 转 Go + 去 Chrome 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task.

**Goal:** 把 `skills/tui-debug/tuidbg.py` 重写为 `cmd/tuidbg` 的 Go 实现，并用纯 Go 字体渲染取代 Chrome 截图。

**Architecture:** 单个 Go 包，逻辑与 Python 版一一对应（起 tmux 会话、发按键、轮询抓屏、退出码契约）。截图路径改为：`capture-pane -e` → 解析 SGR 得到带色的字符网格 → `x/image/font/opentype` 逐格绘制 → `image/png` 编码。tmux 已把屏幕规整成网格，所以**按格定位**而非依赖字体 advance。

**Tech Stack:** Go 1.26，新增依赖 `golang.org/x/image`（go.mod 当前没有）。系统字体，不嵌入。

**替代对象:** `skills/tui-debug/tuidbg.py`（930 行 Python，验收通过后删除）。`skills/tui-debug/SKILL.md` 保留并改写为 Go 版用法。

---

## 实测确定的约束

以下全部在本机（macOS / tmux 3.7c / Go 1.26.5）跑过真实探针。**照抄，不要重新发明**：

### tmux 侧（沿用 Python 版，已被 9 轮评审验证）

| 约束 | 后果 |
|---|---|
| 所有 `-t` 目标带 `=` 前缀 | tmux `-t` 默认前缀匹配，不加则 `stop tuidbg` 误杀 `tuidbg-x` |
| pane 目标写 `=会话名:0.0`，会话目标写 `=会话名` | 只给会话名 `send-keys` 报 can't find pane |
| `capture-pane` 必须加 `-J` | 不加则光标靠右时退出标记折成两行，正则失配 → **崩溃报成 rc=0** |
| `send-keys` 两处都要 `--` | 不加则 `-R` 当 flag 执行，**静默清屏且 rc=0** |
| wrapper 用 `cmd; printf '__TUIDBG_EXIT__=%s\n' $?; exec sleep 100000` | 不用 `remain-on-exit`——它画的 "Pane is dead" 行会挤掉屏幕内容 |
| `new-session -c DIR` 不能省 | 不带时用 tmux server 的默认目录，相对路径 command not found |
| 抓屏失败与空屏必须分开 | 合并（Python 版曾写 `or ""`）→ 失败报成 rc=0 |

### 字体侧（本次新测）

`golang.org/x/image` 的字体栈能力有限，实测：

| 字体 | 结果 |
|---|---|
| `basicfont.Face7x13`（纯标准库） | ASCII 可用，**中文全是方框** |
| `/System/Library/Fonts/Menlo.ttc` (idx 0) | ✅ ASCII + **全部框线字形**（`─│╭` 均 advance=17.0, ok=true） |
| `/System/Library/Fonts/Supplemental/Songti.ttc` (idx 0) | ✅ CJK 可用（`中`=28.0） |
| `/System/Library/Fonts/STHeiti Light.ttc` | ❌ `sfnt: unsupported number of cmap segments` |
| `/System/Library/Fonts/PingFang.ttc` | ❌ 读取失败（受保护） |

Size=14, DPI=144 时的度量：**Menlo advance = 17.0px，height = 33.0，ascent = 26.0**。

**三条由此推出的设计约束：**

1. **框线不回退。** Menlo 自带 `─│╭╯┃` 等字形，回退到 Songti 反而破坏对齐（Songti 的 `╭` 是 `ok=false` 的 fallback 字形）。回退条件应当是「Menlo 没有该字形」，且实现必须用 `sfnt.Font.GlyphIndex(&buf, r) == 0` 判断，不能拿 `Face.GlyphAdvance` 的 ok（它对 fallback 字形也返回 true）。
2. **必须按格定位，不能累加 advance。** Songti 的 ASCII 不等宽（`A`=19.0 / `i`=8.0），中文 28.0 也不是 17.0 的精确 2 倍。每个字符绘制在 `x = PAD + col*CHAR_W`，宽字符占两格但仍从格子左边起画。tmux 输出本身就是网格，列号是现成的。
3. **字体路径要有回退链且缺失时 fail loud。** macOS 用上表两个；Linux 试 `/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf`、`/usr/share/fonts/truetype/noto/NotoSansMono-Regular.ttf`、`/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc`。**一个都找不到时不许画出没有字的图**——那是本工具反复修过的「失败伪装成成功」。

---

## File Structure

`cmd/tuidbg/` 下按职责分文件（Python 版 930 行挤在一个文件里，Go 版拆开）：

| 文件 | 职责 |
|---|---|
| `main.go` | flag 解析与子命令分发、退出码 |
| `session.go` | tmux 交互：`start`/`stop`/`send`/`key`/`capture`/`sessionExists` |
| `shot.go` | 轮询等待、退出码契约、`dead_exit_code` |
| `sgr.go` | SGR 解析 → 带色字符网格（`[]Cell`），256 色/truecolor 调色板 |
| `render.go` | 网格 → PNG：字体加载、回退链、按格绘制 |
| `*_test.go` | 对应的测试 |

纯逻辑（SGR 解析、调色板、退出码判定）与需要 tmux 的部分分开，前者可纯单测。

---

## Task 1: 骨架 + tmux 原语

**Files:** `cmd/tuidbg/main.go`、`cmd/tuidbg/session.go`、`cmd/tuidbg/session_test.go`

- [ ] **Step 1: 写失败的测试**

`session_test.go` 断言目标字符串推导（纯函数，不碰 tmux）：

```go
func TestTargets(t *testing.T) {
	if got := sessionTarget("foo"); got != "=tuidbg-foo" {
		t.Errorf("sessionTarget = %q, want =tuidbg-foo", got)
	}
	if got := paneTarget("foo"); got != "=tuidbg-foo:0.0" {
		t.Errorf("paneTarget = %q, want =tuidbg-foo:0.0", got)
	}
	// 裸名一律加前缀，不做例外
	if got := sessionTarget("tuidbg"); got != "=tuidbg-tuidbg" {
		t.Errorf("sessionTarget = %q, want =tuidbg-tuidbg", got)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./cmd/tuidbg -run TestTargets`
Expected: 编译失败，`undefined: sessionTarget`

- [ ] **Step 3: 实现 session.go**

要点（doc 注释用中文写清*为什么*，与仓库惯例一致）：
- `sessionTarget`/`paneTarget` 带 `=` 前缀，注释写明 tmux 前缀匹配那条实测
- `tmuxCmd(args ...string) (stdout, stderr string, rc int)`
- `sessionExists`、`cmdStart`（含 wrapper 与撞名拒绝）、`cmdStop`（透传 tmux 原文而非硬编码"不存在"）
- `cmdSend`（`-l --`）、`cmdKey`（`--`，注释写明 `-R` 静默清屏那条）
- `capture(name string, ansi bool) (string, error)`：**返回 error 而非 Go 版的 "" 兜底**，Go 的多返回值天然把「失败」与「空屏」分开，这正是 Python 版踩过的坑

- [ ] **Step 4: 集成测试**

```go
func TestSessionLifecycle(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("需要 tmux")
	}
	name := "gotest"
	cmdStop(name) // 清理残留
	if rc := cmdStart(name, 60, 10, []string{"bash", "-c", "echo READY; sleep 100"}, ""); rc != 0 {
		t.Fatalf("start rc=%d", rc)
	}
	defer cmdStop(name)
	if !sessionExists(name) { t.Fatal("start 后会话应存在") }
	if rc := cmdStart(name, 60, 10, []string{"bash", "-c", "sleep 100"}, ""); rc == 0 {
		t.Error("撞名的 start 必须失败")
	}
}
```

- [ ] **Step 5: 邻居安全测试**（`=` 前缀存在的全部理由）

```go
func TestStopDoesNotKillNeighbours(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil { t.Skip("需要 tmux") }
	exec.Command("tmux", "new-session", "-d", "-s", "tuidbg-neighbour", "sleep 100").Run()
	defer exec.Command("tmux", "kill-session", "-t", "=tuidbg-neighbour").Run()
	cmdStart("n", 60, 10, []string{"bash", "-c", "sleep 100"}, "")
	cmdStop("n")
	if exec.Command("tmux", "has-session", "-t", "=tuidbg-neighbour").Run() != nil {
		t.Fatal("邻居会话被误杀了")
	}
}
```

- [ ] **Step 6: `go test ./cmd/tuidbg` 全绿，commit**

```
feat(tuidbg): tmux session primitives in Go
```

---

## Task 2: SGR 解析 → 字符网格

**Files:** `cmd/tuidbg/sgr.go`、`cmd/tuidbg/sgr_test.go`

这是纯逻辑，全部可单测，不需要 tmux 也不需要字体。

- [ ] **Step 1: 定义类型并写失败的测试**

```go
type Cell struct {
	R      rune
	FG, BG color.RGBA
	Bold   bool
}
// Grid 是 [][]Cell，按行
```

测试要覆盖（每条都是 Python 版实测见过的形态）：

```go
func TestParseSGR(t *testing.T) {
	// 真实 TUI 里出现过的序列，按频次：38;5;N 最多
	grid := parseANSI("\x1b[38;5;99mX\x1b[39m")
	// 断言 X 的 FG 是 256 色表里的 99 号（#875fff）
	// 断言 39 之后恢复默认前景

	// 256 色立方体：16-231 是 6x6x6，值域 [0,95,135,175,215,255]
	// 灰阶：232-255 是 8+10*i

	// truecolor
	// 基本色 30-37/90-97、背景 40-47/100-107
	// 7m 反显 → 前景背景交换
	// 0m 重置
	// 未知序列静默跳过，不得崩溃、不得把转义字符画进网格
}

func TestGridDimensions(t *testing.T) {
	// 关键：网格的列数必须是"可见字符数"，不含转义字节。
	// Python 版曾把转义字节当列数，100 列的屏幕算成 209 列。
	grid := parseANSI("\x1b[38;5;99m" + strings.Repeat("A", 100) + "\x1b[39m")
	if len(grid[0]) != 100 {
		t.Errorf("列数 = %d, want 100（转义字节不该计入）", len(grid[0]))
	}
}

func TestWideRunes(t *testing.T) {
	// 中文占两格。网格里如何表示由实现决定（占位 Cell 或宽度字段），
	// 但必须能让渲染层知道下一个字符从第几列开始。
}
```

- [ ] **Step 2-4: 红 → 实现 → 绿**

调色板必须**程序化生成**，不许手写 256 项表。

- [ ] **Step 5: commit**

```
feat(tuidbg): SGR parser producing a coloured cell grid
```

---

## Task 3: 纯 Go PNG 渲染

**Files:** `cmd/tuidbg/render.go`、`cmd/tuidbg/render_test.go`；`go.mod`（新增 `golang.org/x/image`）

- [ ] **Step 1: 加依赖**

```sh
go get golang.org/x/image/font/opentype golang.org/x/image/font/sfnt
```

`go.mod` 会同时拉 `golang.org/x/text`（sfnt 的 cmap 需要）。提交 `go.mod`/`go.sum`。

- [ ] **Step 2: 写失败的测试**

```go
func TestFontChainLoads(t *testing.T) {
	fc, err := loadFonts()
	if err != nil { t.Skipf("本机无可用字体: %v", err) }
	// 主字体必须有框线字形 —— 实测 Menlo 有，不该回退
	for _, r := range []rune{'─', '│', '╭', '╯', '┃'} {
		if !fc.primaryHas(r) {
			t.Errorf("主字体缺框线字形 %c，回退会破坏对齐", r)
		}
	}
	// CJK 必须能画（靠回退）
	if !fc.canRender('中') { t.Error("字体链无法渲染 CJK") }
}

func TestRenderProducesRealImage(t *testing.T) {
	fc, err := loadFonts()
	if err != nil { t.Skip("本机无可用字体") }
	grid := parseANSI("\x1b[38;5;196mRED\x1b[39m 中文 ╭─╯")
	img := renderGrid(grid, fc)
	// 尺寸按格子推导，不靠字体 advance 累加
	// 断言图像里确实有那个红色像素 —— 否则就是画了一张空图
	if !containsColour(img, color.RGBA{255, 0, 0, 255}) {
		t.Error("红色字符没画出来")
	}
}

func TestNoFontsIsAnError(t *testing.T) {
	// 字体全部缺失时必须报错，不能返回一张没有字的图。
	// 这是本工具反复修过的「失败伪装成成功」。
}
```

- [ ] **Step 3: 实现**

- `loadFonts()`：按平台候选路径依次尝试，`sfnt.ParseCollection` 失败就试下一个（STHeiti 会失败，属正常），全失败返回 error
- 回退判定用 `sfnt.Font.GlyphIndex(&buf, r) == 0`，**不要**用 `Face.GlyphAdvance` 的 ok
- 常量：`CharW = 17`、`LineH = 33`、`Ascent = 26`、`Pad = 8`（Size=14/DPI=144 实测值）
- 逐格绘制：`x = Pad + col*CharW`，`y = Pad + row*LineH + Ascent`
- 先铺背景色矩形再画字（背景色块是 TUI 状态栏的主要视觉元素）

- [ ] **Step 4: 绿，然后眼验**

跑测试生成一张真实 TUI 的 PNG，**人工看一眼**（或让协调者看）：框线要连续、中文要对齐、颜色要对。这一步不能只看测试绿——Python 版的教训是结构性断言看不出排版错乱。

- [ ] **Step 5: commit**

```
feat(tuidbg): render screenshots with pure-Go font rasterisation
```

---

## Task 4: shot 的退出码契约

**Files:** `cmd/tuidbg/shot.go`、`cmd/tuidbg/shot_test.go`

**这是整个工具唯一非平凡的逻辑，三条机制的顺序是承重的。**

- [ ] **Step 1: 写失败的测试**（六个场景，对应 Python 版的 a–f）

```go
func TestShot(t *testing.T) {
	// (a) --wait 命中 → 0
	// (b) --wait 超时 → 124，且最后一屏必须写到 stdout（捕获输出断言）
	// (c) 程序退出 42 → 返回 42
	// (d) 对已死进程 --wait → 立即短路（<3s），返回真实退出码
	// (e) 标记被 pane 边缘折断时仍须识别 —— 少了 -J 会 rc=0（最坏形态）
	//     用 printf "%50s" X; exit 3 在 60 列 pane 上构造
	// (f) 死亡检查必须排在 deadline 之前：sleep 1.0; exit 3 配 timeout 1.05
	//     顺序反了返回 124（把崩溃说成超时）
	// (g) 程序以 0 退出且 --wait 从未命中 → rc 也是 0，与"命中"撞码。
	//     断言 stdout 里没有锚点但有屏幕内容。
}
```

- [ ] **Step 2-4: 红 → 实现 → 绿**

三条检查的顺序：**dead-exit-code → pattern → deadline**。doc 注释写明为什么。

`--png` 的契约照搬 Python 版：渲染在退出码分支决定**之后**（所以超时屏和崩溃屏也能截到），渲染失败返回 `PNGFailRC = 3` 覆盖正常退出码，stderr 必有一行说明成败。

- [ ] **Step 5: 变异验证**

对每条机制做一次变异，确认对应测试变红。**特别验证 (e) 和 (f)** —— 它们守的是 Python 版真实踩过的坑。把结果写进报告。

- [ ] **Step 6: commit**

```
feat(tuidbg): shot with the wait/timeout/exit-code contract
```

---

## Task 5: CLI 装配 + 真机验收

**Files:** `cmd/tuidbg/main.go`

- [ ] **Step 1: 子命令与 flag**

与 Python 版保持一致的用户界面（SKILL.md 的用法示例才不用大改）：

```
tuidbg start [-session NAME] [-cols N] [-rows N] [-cwd DIR] -- 命令...
tuidbg send  [-session NAME] 文本
tuidbg key   [-session NAME] 键名...
tuidbg shot  [-session NAME] [-wait 正则] [-timeout 秒] [-ansi] [-png 路径]
tuidbg stop  [-session NAME]
```

Go 的 `flag` 包不支持子命令后置 flag 的 argparse 风格，用 `flag.NewFlagSet` 每个子命令一个。`-wait` 的正则要在跑之前 `regexp.Compile` 校验，非法时 rc=1 带清楚消息（不是 panic）。

- [ ] **Step 2: 真机端到端验收**

```sh
go build -o /tmp/tuidbg ./cmd/tuidbg
/tmp/tuidbg start -cwd "$PWD" -- ./yanshi --fake-model -inprocess
/tmp/tuidbg shot -wait 'Ctrl\+Enter' -timeout 20 -png /tmp/a.png | tail -5
/tmp/tuidbg send '你好'
/tmp/tuidbg key Enter
/tmp/tuidbg shot -wait 'assistant:' -timeout 30 -png /tmp/b.png | tail -12
/tmp/tuidbg stop
```

**验收判据是内容不是退出码：** 第一屏要有输入框行 + 状态栏行（约 20–30 行），第二屏要有 `you:`/`assistant:`。两张 PNG 都要**人工看过**，确认框线连续、中文对齐、颜色正确。

锚点用 `Ctrl\+Enter`：它 locale 无关且只在 TUI 接管后出现（`yanshi` 满足前者不满足后者——它也在启动 stderr 里，实测 0.1–0.3s 窗口内命中而屏幕只有 5 行）。

- [ ] **Step 3: commit**

```
feat(tuidbg): CLI dispatch and end-to-end acceptance
```

---

## Task 6: 文档与收尾

**Files:** `skills/tui-debug/SKILL.md`、删除 `skills/tui-debug/tuidbg.py`、`CLAUDE.md`

- [ ] **Step 1: 改写 SKILL.md**

- 用法从 `python3 skills/tui-debug/tuidbg.py` 改为 `go run ./cmd/tuidbg`（或先 `go build`）
- 依赖从 `agent-browser` 改为「无（字体用系统自带）」
- 坑那节保留全部条目（锚点两条件、`$?` 为 0 不等于命中、`-cwd`、TUI 退出后屏幕为空、正则匹配渲染后的屏幕、窗口尺寸、会话名前缀、`tmux attach` 接管）
- 新增一条：**本机无可用等宽字体时 `-png` 会报错而不是画空图**，并列出候选路径

- [ ] **Step 2: 删除 Python 版**

`git rm skills/tui-debug/tuidbg.py`。SKILL.md 留在原地。

- [ ] **Step 3: CLAUDE.md 的 dev 工具清单加一行**

那段列举 `cmd/depsanalyze`/`cmd/agent-worker`/`cmd/featurestatus`/`cmd/covercheck` 的话里补上 `cmd/tuidbg`。

- [ ] **Step 4: 治理门禁**

```sh
go test ./internal/archtest ./internal/bootstrap
go vet ./cmd/tuidbg
gofmt -l cmd/tuidbg
```

注意 GOV2（单文件 ≤5000 纯代码行）、GOV3（导出符号要 doc 注释）。`cmd/tuidbg` 里能不导出的就不导出，可以躲开 GOV3 的大部分要求，但导出的必须写注释。

- [ ] **Step 5: commit**

```
docs(tui-debug): switch the skill to the Go implementation
```

---

## 最终验收

- [ ] `go test ./cmd/tuidbg` 全绿（含 tmux 集成测试）
- [ ] `go test ./internal/archtest ./internal/bootstrap` 全绿
- [ ] `go vet ./...`、`gofmt -l cmd/tuidbg` 干净
- [ ] 真机 e2e：两屏内容正确，两张 PNG **人工看过**且框线/中文/颜色都对
- [ ] `-png` 在无字体时报错而非画空图
- [ ] `skills/tui-debug/tuidbg.py` 已删除，SKILL.md 描述的是 Go 版
- [ ] 零残留 tmux 会话，`agbt-*` 未受影响

## 明确不做

- **不嵌入字体** —— 用系统字体（已定），跨机器差异靠回退链与 fail-loud 覆盖
- **不做 Windows 支持** —— tmux 本身就没有
- **不进 CI** —— 交互式调试工具，不是门禁
- **不保留 Python 版** —— 一个工具两份实现必定漂移
