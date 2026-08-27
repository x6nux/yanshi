// sgr_test.go —— SGR 解析器的测试。
//
// 这些测试断言的是**具体 RGBA 值**而不是"颜色变了"。理由很具体：调色板算错
// （比如色立方的 r/g/b 顺序写反）仍然会产出一组合法颜色，一个只检查
// "FG != 默认值" 的测试对此一路绿灯。既然唯一的消费者是一张给人看的截图，
// 那么"哪个颜色"就是全部的语义。
//
// 断言用的期望值取自算式而非抄自实现：见 TestPalette256Arithmetic 里逐条写出的
// 下标推导。抄实现的输出会让测试与被测物一起错。
//
// 本注释与 package 子句之间**故意**留了空行：它描述的是这个文件，不是这个包。

package main

import (
	"fmt"
	"image/color"
	"strings"
	"testing"
)

// rgb 是构造不透明颜色的简写，让期望值一眼能与 #rrggbb 对上。
func rgb(r, g, b uint8) color.RGBA { return color.RGBA{r, g, b, 0xff} }

// hex 把颜色渲染成 #rrggbb，好让失败信息可以直接与本文件里的十六进制期望值比对。
// 打印 color.RGBA 结构体会得到一串十进制，人眼没法与 #875fff 这种写法对照。
func hex(c color.RGBA) string { return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B) }

// wantCell 断言某一格的字符与颜色。
//
// 一次把三样（字符、前景、背景）一起查：只查前景的测试，会被"颜色写进了背景"
// 这种错误骗过去，而那恰恰是 38/48 分派最容易出的岔子。
func wantCell(t *testing.T, got Cell, r rune, fg, bg color.RGBA, what string) {
	t.Helper()
	if got.R != r {
		t.Errorf("%s: rune = %q, want %q", what, got.R, r)
	}
	if got.FG != fg {
		t.Errorf("%s: FG = %s, want %s", what, hex(got.FG), hex(fg))
	}
	if got.BG != bg {
		t.Errorf("%s: BG = %s, want %s", what, hex(got.BG), hex(bg))
	}
}

// firstRow 解析 s 并返回唯一的一行，若行数不为 1 则中止。
//
// 大多数用例是单行的，而在错误的行数上继续索引会得到一个越界 panic ——
// 那会把"解析器把一行切成了两行"这个真实缺陷伪装成测试自身的崩溃。
func firstRow(t *testing.T, s string) []Cell {
	t.Helper()
	grid := parseANSI(s)
	if len(grid) != 1 {
		t.Fatalf("parseANSI(%q): got %d rows, want 1", s, len(grid))
	}
	return grid[0]
}

// TestPalette256Arithmetic 钉住 256 色表的三段结构。
//
// 每个期望值都从下标算式推出来，而不是从实现的输出里抄：
// 色立方段 idx = 16 + 36r + 6g + b，其中 r/g/b 是 levels[0,95,135,175,215,255] 的下标。
// 这样一来"把 r 和 b 写反"会被抓住 —— 若期望值抄自实现，两边会一起错、测试照绿。
func TestPalette256Arithmetic(t *testing.T) {
	cases := []struct {
		idx  int
		want color.RGBA
		why  string
	}{
		// 0-15：约定俗成的经典色，算不出来，只能钉值。
		{0, rgb(0x00, 0x00, 0x00), "经典色起点 黑"},
		{1, rgb(0xcd, 0x00, 0x00), "经典色 红（暗）"},
		{9, rgb(0xff, 0x00, 0x00), "亮色段 亮红"},
		{15, rgb(0xff, 0xff, 0xff), "经典色终点 白"},

		// 色立方段。16+36r+6g+b。
		{16, rgb(0, 0, 0), "色立方起点：16 → r=g=b=levels[0]=0"},
		{231, rgb(255, 255, 255), "色立方终点：231-16=215=5*36+5*6+5 → levels[5]=255"},
		// 本条是整张表的核心：99 是实测那一屏里最常见的颜色。
		// 99-16 = 83；83 = 2*36 + 11，11 = 1*6 + 5 → (r,g,b) 下标 = (2,1,5)
		// levels[2]=135=0x87, levels[1]=95=0x5f, levels[5]=255=0xff → #875fff。
		{99, rgb(0x87, 0x5f, 0xff), "任务规格点名的值 #875fff；r/g/b 顺序写反会变成 #ff5f87"},
		// 一个 r/g/b 三值互不相同、且反转后落在表内另一个合法项上的点，
		// 专门用来抓顺序错误：#005f87 反转成 #87 5f 00。
		{24, rgb(0x00, 0x5f, 0x87), "24-16=8=0*36+1*6+2 → levels[0,1,2]"},

		// 灰阶段：8 + 10i。
		{232, rgb(0x08, 0x08, 0x08), "灰阶起点 8+10*0=8"},
		{255, rgb(0xee, 0xee, 0xee), "灰阶终点 8+10*23=238=0xee"},
		{245, rgb(0x8a, 0x8a, 0x8a), "实测出现过的 245：8+10*13=138=0x8a"},
	}
	for _, c := range cases {
		if got := palette[c.idx]; got != c.want {
			t.Errorf("palette[%d] = %s, want %s (%s)", c.idx, hex(got), hex(c.want), c.why)
		}
	}
}

// TestPaletteCubeIsNotSymmetric 保证色立方段里存在 r/g/b 互不相同的项。
//
// 这是 TestPalette256Arithmetic 的前提条件检查：如果所有被断言的点都碰巧是灰色
// （r==g==b），那么"把 r 和 b 写反"根本不会改变任何一个期望值，那批断言就形同虚设。
// 单独立一条，是因为那个前提在阅读期望值表时并不显眼。
func TestPaletteCubeIsNotSymmetric(t *testing.T) {
	for _, idx := range []int{99, 24} {
		c := palette[idx]
		if c.R == c.B {
			t.Errorf("palette[%d] = %s 的 R 与 B 相同 —— 它无法区分 r/g/b 顺序，"+
				"TestPalette256Arithmetic 里这条断言不保护任何东西", idx, hex(c))
		}
	}
}

// TestPaletteIsFullyPopulated 保证 256 项没有一个是零值。
//
// 零值 RGBA 是**透明**黑（A=0），光栅出来是个洞而不是黑色。少填几项在
// "颜色看起来对不对"的检查里发现不了，因为没人会去看下标 200 附近。
func TestPaletteIsFullyPopulated(t *testing.T) {
	for i, c := range palette {
		if c.A != 0xff {
			t.Fatalf("palette[%d] 的 alpha = %d，不是 0xff —— 该项没被填过", i, c.A)
		}
	}
}

// TestColour256 覆盖实测频次表里出现过的每一个 256 色形态，前景与背景各一组。
func TestColour256(t *testing.T) {
	// 前景：ESC[38;5;99m（实测 x30，最常见的颜色形态）
	row := firstRow(t, "\x1b[38;5;99mX")
	wantCell(t, row[0], 'X', rgb(0x87, 0x5f, 0xff), defaultBG, "38;5;99")

	// 前景 245 与 255，实测里也出现过。
	row = firstRow(t, "\x1b[38;5;245mA\x1b[38;5;255mB")
	wantCell(t, row[0], 'A', rgb(0x8a, 0x8a, 0x8a), defaultBG, "38;5;245")
	wantCell(t, row[1], 'B', rgb(0xee, 0xee, 0xee), defaultBG, "38;5;255")

	// 背景：ESC[48;5;236m（实测 x2）。236-232=4 → 8+40=48=0x30。
	// 同时断言前景仍是默认色 —— 抓"背景色写进了前景"这一类分派错误。
	row = firstRow(t, "\x1b[48;5;236mX")
	wantCell(t, row[0], 'X', defaultFG, rgb(0x30, 0x30, 0x30), "48;5;236")
}

// TestColourTruecolour 覆盖 38;2;R;G;B / 48;2;R;G;B。
func TestColourTruecolour(t *testing.T) {
	row := firstRow(t, "\x1b[38;2;16;32;48mX")
	wantCell(t, row[0], 'X', rgb(16, 32, 48), defaultBG, "38;2 前景")

	row = firstRow(t, "\x1b[48;2;200;100;50mX")
	wantCell(t, row[0], 'X', defaultFG, rgb(200, 100, 50), "48;2 背景")

	// 三个通道刻意互不相同：全用同一个值的话，R/G/B 装配顺序写错也测不出来。
	row = firstRow(t, "\x1b[38;2;1;2;3mX")
	if got := row[0].FG; got.R != 1 || got.G != 2 || got.B != 3 {
		t.Errorf("38;2;1;2;3 得到 %s，want #010203 —— R/G/B 顺序错了", hex(got))
	}
}

// TestColourBasic 覆盖 30-37 / 90-97 前景与 40-47 / 100-107 背景全部四段。
//
// 逐段各测两个端点而不是只测一个：段内偏移算错（比如亮色段忘了 +8）在端点上
// 才会露出来 —— 只测中间某一个值时，错误的下标可能仍落在同一段里。
func TestColourBasic(t *testing.T) {
	cases := []struct {
		seq    string
		fg, bg color.RGBA
		why    string
	}{
		{"\x1b[30m", ansi16[0], defaultBG, "30 → ansi16[0]"},
		{"\x1b[31m", rgb(0xcd, 0x00, 0x00), defaultBG, "31 → 暗红"},
		{"\x1b[37m", ansi16[7], defaultBG, "37 → ansi16[7]（实测出现过）"},
		{"\x1b[90m", ansi16[8], defaultBG, "90 → ansi16[8]，亮色段偏移 +8"},
		{"\x1b[91m", rgb(0xff, 0x00, 0x00), defaultBG, "91 → 亮红"},
		{"\x1b[97m", ansi16[15], defaultBG, "97 → ansi16[15]"},
		{"\x1b[40m", defaultFG, ansi16[0], "40 → 背景 ansi16[0]（实测出现过）"},
		{"\x1b[44m", defaultFG, rgb(0x00, 0x00, 0xee), "44 → 背景蓝"},
		{"\x1b[47m", defaultFG, ansi16[7], "47 → 背景 ansi16[7]"},
		{"\x1b[100m", defaultFG, ansi16[8], "100 → 背景 ansi16[8]"},
		{"\x1b[107m", defaultFG, ansi16[15], "107 → 背景 ansi16[15]"},
	}
	for _, c := range cases {
		row := firstRow(t, c.seq+"X")
		wantCell(t, row[0], 'X', c.fg, c.bg, c.why)
	}
}

// TestDefaultsAndReset 覆盖 39 / 49 / 0。
//
// 每条都先设一个非默认色再复位，然后断言**恰好等于**默认色。
// 若只断言"不等于刚才那个颜色"，把 39 映射成任意第三种颜色也能通过。
func TestDefaultsAndReset(t *testing.T) {
	// 39：只复位前景，背景必须留着。实测频次表里 39 是最常见的序列（x43）。
	row := firstRow(t, "\x1b[38;5;99m\x1b[48;5;236mA\x1b[39mB")
	wantCell(t, row[0], 'A', rgb(0x87, 0x5f, 0xff), rgb(0x30, 0x30, 0x30), "39 之前")
	wantCell(t, row[1], 'B', defaultFG, rgb(0x30, 0x30, 0x30), "39 之后：前景回默认，背景不动")

	// 49：只复位背景，前景必须留着。
	row = firstRow(t, "\x1b[38;5;99m\x1b[48;5;236mA\x1b[49mB")
	wantCell(t, row[1], 'B', rgb(0x87, 0x5f, 0xff), defaultBG, "49 之后：背景回默认，前景不动")

	// 0：全复位，含 bold 与 reverse。
	row = firstRow(t, "\x1b[1;7;38;5;99;48;5;236mA\x1b[0mB")
	wantCell(t, row[1], 'B', defaultFG, defaultBG, "0 之后颜色全复位")
	if row[1].Bold {
		t.Error("0 之后 Bold 仍为 true —— 全复位没有清掉粗体")
	}
	// reverse 若没被清掉，B 会画成 defaultBG 前景 / defaultFG 背景 —— 上面那条
	// wantCell 已经覆盖，此处再单独说明意图。
	if row[1].FG == defaultBG && row[1].BG == defaultFG {
		t.Error("0 之后 reverse 仍然生效 —— 前景背景还是反的")
	}

	// 空参数 ESC[m 按标准等价于 ESC[0m。
	row = firstRow(t, "\x1b[38;5;99mA\x1b[mB")
	wantCell(t, row[1], 'B', defaultFG, defaultBG, "ESC[m 等价于 ESC[0m")
}

// TestDefaultsAreTheSpecifiedTheme 钉死深色主题的两个默认值本身。
//
// 单独立一条：其余测试都用 defaultFG/defaultBG 这两个变量作期望值，
// 于是改掉变量的值不会让它们中任何一条变红。规格把 #d4d4d4 / #1e1e1e 写成了契约，
// 这里就用字面量把契约钉住。
func TestDefaultsAreTheSpecifiedTheme(t *testing.T) {
	if got := hex(defaultFG); got != "#d4d4d4" {
		t.Errorf("defaultFG = %s, want #d4d4d4", got)
	}
	if got := hex(defaultBG); got != "#1e1e1e" {
		t.Errorf("defaultBG = %s, want #1e1e1e", got)
	}
}

// TestBold 覆盖 SGR 1 与它的两个关闭码。
func TestBold(t *testing.T) {
	row := firstRow(t, "A\x1b[1mB\x1b[22mC")
	if row[0].Bold {
		t.Error("A 不该是粗体")
	}
	if !row[1].Bold {
		t.Error("ESC[1m 之后 B 应该是粗体")
	}
	if row[2].Bold {
		t.Error("ESC[22m 之后 C 不该还是粗体")
	}
}

// TestReverseVideo 覆盖 SGR 7，包含它最容易出错的那一种形态。
//
// 裸的 ESC[7m（前后景都还没设过）是实测频次表里真实出现的形态，也正是
// "交换两个未设置值等于没交换"这个 bug 的触发点。这里断言交换后的**具体值**，
// 因为若实现漏掉了交换，颜色仍然是一对合法的默认色。
func TestReverseVideo(t *testing.T) {
	// 裸 ESC[7m：必须变成 前景=默认背景、背景=默认前景。
	row := firstRow(t, "\x1b[7mX")
	wantCell(t, row[0], 'X', defaultBG, defaultFG, "裸 ESC[7m 必须代入默认值后再交换")

	// 有显式颜色时的交换。
	row = firstRow(t, "\x1b[38;5;99m\x1b[48;5;236m\x1b[7mX")
	wantCell(t, row[0], 'X', rgb(0x30, 0x30, 0x30), rgb(0x87, 0x5f, 0xff), "7 交换显式设置的前后景")

	// 只设了前景时：背景侧要代入默认背景。
	row = firstRow(t, "\x1b[31m\x1b[7mX")
	wantCell(t, row[0], 'X', defaultBG, rgb(0xcd, 0x00, 0x00), "只设前景时 7 的半边代入默认")

	// 27 关闭反显。
	row = firstRow(t, "\x1b[7mA\x1b[27mB")
	wantCell(t, row[0], 'A', defaultBG, defaultFG, "27 之前仍反显")
	wantCell(t, row[1], 'B', defaultFG, defaultBG, "27 之后恢复正常")

	// 反显在设置颜色**之前**声明时同样要生效 —— 交换发生在解析时刻，
	// 与两条转义的先后无关。
	row = firstRow(t, "\x1b[7m\x1b[31mX")
	wantCell(t, row[0], 'X', defaultBG, rgb(0xcd, 0x00, 0x00), "先 7 后设色也要交换")
}

// TestColumnCountIsVisibleCharactersOnly 是任务规格点名的那条回归。
//
// Python 前身在这里错过：它拿原始 ANSI 字符串量列数，转义字节被数成了列，
// 一屏真实的 100 列算出来是 209 列。
func TestColumnCountIsVisibleCharactersOnly(t *testing.T) {
	grid := parseANSI("\x1b[38;5;99m" + strings.Repeat("A", 100) + "\x1b[39m")
	if len(grid) != 1 {
		t.Fatalf("got %d rows, want 1", len(grid))
	}
	if len(grid[0]) != 100 {
		t.Fatalf("列数 = %d, want 100 —— 转义字节被数进列里了（Python 版在这里算出 209）", len(grid[0]))
	}
	for i, c := range grid[0] {
		if c.R != 'A' {
			t.Fatalf("第 %d 列是 %q，want 'A' —— 有非可见字符混进了网格", i, c.R)
		}
	}
}

// TestNoEscapeByteReachesAGrid 是上一条的普适版：任何形态的转义，
// 都不能让 0x1b、'['、或参数数字漏进 cell。
//
// 单独立一条而不是并进列数测试：列数对得上但内容错了（比如吞掉了转义却
// 多留下一个 'm'）是另一种失败，列数断言看不见它。
func TestNoEscapeByteReachesAGrid(t *testing.T) {
	inputs := []string{
		"\x1b[38;5;99mA\x1b[39m",
		"\x1b[0mA",
		"\x1b[mA",
		"\x1b[7mA",
		"\x1b[38;2;1;2;3mA",
		"\x1b[2JA",             // 非 SGR 的 CSI
		"\x1b[10;5HA",          // 光标定位
		"\x1b]0;title\x07A",    // OSC + BEL
		"\x1b]0;title\x1b\\A",  // OSC + ST
		"\x1b[?25lA",           // 带私有标记的 CSI
		"\x1bcA",               // 双字符转义
		"\x1b[38;5;999mA",      // 越界的调色板下标
		"\x1b[4:3mA",           // 带子参数
		"A\x1b[38;5;99m",       // 转义在末尾
		"\x1b[1;2;3;4;5;6;7mA", // 一长串参数
	}
	for _, in := range inputs {
		grid := parseANSI(in)
		for _, row := range grid {
			for i, c := range row {
				switch {
				case c.R == 0x1b:
					t.Errorf("parseANSI(%q) 第 %d 格是裸的 ESC 字节", in, i)
				case c.R == '[' || c.R == ']':
					t.Errorf("parseANSI(%q) 第 %d 格是 %q —— 转义的引导字节漏进了网格", in, i, c.R)
				case c.R == 'm' && !strings.ContainsRune("A", c.R):
					t.Errorf("parseANSI(%q) 第 %d 格是 'm' —— 转义的终结字节漏进了网格", in, i)
				}
			}
		}
		// 每条输入都恰好含一个可见字符 'A'。少了说明被吞掉了，多了说明有残渣。
		var visible int
		for _, row := range grid {
			for _, c := range row {
				if c.Width > 0 {
					visible++
				}
			}
		}
		if visible != 1 {
			t.Errorf("parseANSI(%q): 可见字符数 = %d, want 1（唯一该留下的是 'A'）", in, visible)
		}
	}
}

// TestUnknownSequencesAreSkipped 覆盖"认不出来就静默跳过"，且不能污染其后的样式。
//
// 关键在后半句：一条被误解析的未知转义可能把它的参数当颜色码吃下去，
// 于是后面所有字符的颜色都错。所以每条用例都在未知序列**之后**再断言一次颜色。
func TestUnknownSequencesAreSkipped(t *testing.T) {
	cases := []struct {
		in  string
		why string
	}{
		{"\x1b[2J\x1b[38;5;99mX", "清屏 CSI 之后颜色仍要正常生效"},
		{"\x1b[10;5H\x1b[38;5;99mX", "光标定位之后颜色仍要正常生效"},
		{"\x1b]0;some title\x07\x1b[38;5;99mX", "OSC 之后颜色仍要正常生效"},
		{"\x1b[?25l\x1b[38;5;99mX", "私有模式之后颜色仍要正常生效"},
		{"\x1b[38;5;99m\x1b[5mX", "未知的 5（闪烁）不能改掉已有颜色"},
		{"\x1b[38;5;99m\x1b[3;4;9mX", "一串未知属性码不能改掉已有颜色"},
		{"\x1b[38;5;99m\x1b[38;9;1mX", "未知的色彩空间选择子 9 必须整条跳过，不能改色"},
		// 子参数形态。终端用 ':' 分隔的扩展形式（下划线样式 4:3 是标准里的例子）。
		// 变异测试逼出来的：不按 ':' 切时，"4:3" 会被 atoiSGR 当成畸形串退化成
		// **码 0**，也就是一次全复位 —— 颜色被静默清掉，而这里之前没有任何断言。
		{"\x1b[38;5;99m\x1b[4:3mX", "带子参数的 4:3 必须切开，不能整串退化成码 0 把颜色清掉"},
		{"\x1b[38;5;99m\x1b[58:2:1:2:3mX", "未知的 58（下划线色）连同子参数一起被忽略，不改色"},
	}
	for _, c := range cases {
		row := firstRow(t, c.in)
		if len(row) != 1 {
			t.Fatalf("%s: 得到 %d 格，want 1", c.why, len(row))
		}
		wantCell(t, row[0], 'X', rgb(0x87, 0x5f, 0xff), defaultBG, c.why)
	}

	// 越界的调色板下标：跳过整条、保留原色，且不能把 999 的残渣当成独立码。
	row := firstRow(t, "\x1b[31m\x1b[38;5;999mX")
	wantCell(t, row[0], 'X', rgb(0xcd, 0x00, 0x00), defaultBG, "越界下标必须整条丢弃并保留原前景")

	// 越界的真彩通道同理。
	row = firstRow(t, "\x1b[31m\x1b[38;2;300;0;0mX")
	wantCell(t, row[0], 'X', rgb(0xcd, 0x00, 0x00), defaultBG, "越界真彩通道必须整条丢弃")
}

// TestExtendedColourDoesNotLeakItsArguments 单独钉住 38/48 的"吃掉后续参数"。
//
// 这是 range 循环写法下最隐蔽的一个 bug：ESC[38;5;99m 会被读成
// 码 38（无效）+ 码 5（闪烁）+ 码 99（亮白背景）。屏幕照样渲染，
// 只是前景没变、背景莫名其妙变白了 —— 一个只查前景的测试对此全绿。
func TestExtendedColourDoesNotLeakItsArguments(t *testing.T) {
	row := firstRow(t, "\x1b[38;5;99mX")
	if row[0].BG != defaultBG {
		t.Errorf("38;5;99 之后背景 = %s，want %s —— 参数 99 被当成了独立的背景色码",
			hex(row[0].BG), hex(defaultBG))
	}

	// 同一条转义里，颜色之后还跟着别的码时，那些码必须**照常生效**（不能被一起吞掉）。
	row = firstRow(t, "\x1b[38;5;99;1mX")
	wantCell(t, row[0], 'X', rgb(0x87, 0x5f, 0xff), defaultBG, "38;5;99;1 的颜色部分")
	if !row[0].Bold {
		t.Error("38;5;99;1 里的 1 没生效 —— 38 多吃了一个参数")
	}

	// 真彩形态同理：38;2;R;G;B 恰好吃 5 个。
	row = firstRow(t, "\x1b[38;2;1;2;3;1mX")
	if !row[0].Bold {
		t.Error("38;2;1;2;3;1 里的 1 没生效 —— 38;2 吃掉的参数个数不对")
	}

	// 少吃一个参数的方向，要靠**被泄漏出去的那个值本身有可观测效果**才测得出来。
	// 这两条是变异测试逼出来的：上面几条用 N=99、B=3，而 99 与 3 恰好都是本实现
	// 静默忽略的码，于是把 consumed 从 3 改成 2、从 5 改成 4，全部测试照绿 ——
	// 那不是实现对，是取值恰好无害。这里改用会留下痕迹的值。
	//
	// 38;5;1：若少吃一个，泄漏出的 1 会被当成"粗体"。
	row = firstRow(t, "\x1b[38;5;1mX")
	wantCell(t, row[0], 'X', rgb(0xcd, 0x00, 0x00), defaultBG, "38;5;1 的颜色部分")
	if row[0].Bold {
		t.Error("38;5;1 之后 Bold 为 true —— 参数 1 泄漏出来被当成了粗体码，38;5 少吃了一个参数")
	}

	// 38;2;1;2;7：若少吃一个，泄漏出的 7 会被当成"反显"，前后景整个对调。
	row = firstRow(t, "\x1b[38;2;1;2;7mX")
	wantCell(t, row[0], 'X', rgb(1, 2, 7), defaultBG,
		"38;2;1;2;7 —— 若 B 通道泄漏成码 7，前后景会被反显对调")

	// 48 侧同样要测：前后景两条分派路径各自独立，只测 38 会漏掉一半。
	row = firstRow(t, "\x1b[48;5;1mX")
	wantCell(t, row[0], 'X', defaultFG, rgb(0xcd, 0x00, 0x00), "48;5;1 的颜色部分")
	if row[0].Bold {
		t.Error("48;5;1 之后 Bold 为 true —— 参数 1 泄漏了，48;5 少吃了一个参数")
	}
	row = firstRow(t, "\x1b[48;2;1;2;7mX")
	wantCell(t, row[0], 'X', defaultFG, rgb(1, 2, 7), "48;2;1;2;7 —— B 通道不得泄漏成码 7")
}

// TestParseParams 直接覆盖参数体的切分。
//
// 与上面走 parseANSI 的用例互补：那条路径只能观察到"颜色对不对"，
// 而子参数切分错误的后果之一（把 "4:3" 整串读成畸形 → 退化成码 0 → 全复位）
// 只有在这里才看得清是**哪一步**错了。
func TestParseParams(t *testing.T) {
	cases := []struct {
		body string
		want []int
	}{
		{"", []int{0}},                        // ESC[m 等价于 ESC[0m
		{"0", []int{0}},                       //
		{"38;5;99", []int{38, 5, 99}},         //
		{";", []int{0, 0}},                    // 空参数各算一个 0
		{"1;;2", []int{1, 0, 2}},              // 中间的空参数不能被吞掉，否则参数会左移
		{"4:3", []int{4, 3}},                  // 子参数按 ':' 切开
		{"38:5:99", []int{38, 5, 99}},         // 冒号形态的 256 色
		{"38;2:1:2:3", []int{38, 2, 1, 2, 3}}, // 分号与冒号混用
	}
	for _, c := range cases {
		got := parseParams(c.body)
		if len(got) != len(c.want) {
			t.Errorf("parseParams(%q) = %v, want %v（长度不同）", c.body, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parseParams(%q) = %v, want %v", c.body, got, c.want)
				break
			}
		}
	}
}

// TestInvalidUTF8IsDroppedNotSubstituted 保证非法字节被丢弃而不是变成 U+FFFD。
//
// 变异测试逼出来的：把这条分支关掉后所有测试照绿。后果是可见的 —— 一个替换字符
// 会在截图上留下一个豆腐块，并额外占掉一列，把该行其余部分整体推错位。
// 而这里的非法字节只是抓屏被截断留下的传输残渣，不是屏幕内容。
func TestInvalidUTF8IsDroppedNotSubstituted(t *testing.T) {
	row := firstRow(t, "A\xffB")
	if len(row) != 2 {
		t.Fatalf("\"A\\xffB\" 占 %d 列, want 2 —— 非法字节占了一列", len(row))
	}
	for i, c := range row {
		if c.R == '�' {
			t.Errorf("第 %d 格是 U+FFFD 替换字符 —— 非法字节应当被丢弃，"+
				"而不是变成一个可见的豆腐块", i)
		}
	}
	if row[0].R != 'A' || row[1].R != 'B' {
		t.Errorf("得到 %q%q, want \"AB\"", row[0].R, row[1].R)
	}

	// 被拦腰截断的多字节字符（中文的前两个字节）同理。
	row = firstRow(t, "A\xe4\xb8")
	if len(row) != 1 || row[0].R != 'A' {
		t.Errorf("截断的多字节序列得到 %d 列, want 1（只剩 'A'）", len(row))
	}
}

// TestWideRunesOccupyTwoColumns 覆盖任务规格的第二条硬要求。
//
// 宽度取值本身是在真实 tmux 3.7c 上量出来的（printf 单字符 + 读 #{cursor_x}），
// 见 runeWidth 的注释。这里断言的是那批实测值。
func TestWideRunesOccupyTwoColumns(t *testing.T) {
	row := firstRow(t, "中A")
	if len(row) != 3 {
		t.Fatalf("\"中A\" 占 %d 列, want 3（宽字符 2 + ASCII 1）", len(row))
	}
	if row[0].R != '中' || row[0].Width != 2 {
		t.Errorf("第 0 格 = %q width=%d, want '中' width=2", row[0].R, row[0].Width)
	}
	if row[1].R != 0 || row[1].Width != 0 {
		t.Errorf("第 1 格 = %q width=%d, want 续格（R=0, width=0）", row[1].R, row[1].Width)
	}
	if row[2].R != 'A' || row[2].Width != 1 {
		t.Errorf("第 2 格 = %q width=%d, want 'A' width=1 —— 'A' 没落在第 2 列，"+
			"含中文的行会整体错位", row[2].R, row[2].Width)
	}

	// 续格必须携带与本体相同的颜色，否则宽字形下面会露出一列默认背景。
	row = firstRow(t, "\x1b[38;5;99m\x1b[48;5;236m中")
	if row[1].BG != row[0].BG || row[1].FG != row[0].FG {
		t.Errorf("续格颜色 fg=%s bg=%s 与本体 fg=%s bg=%s 不同 —— 宽字形下会露出一列空隙",
			hex(row[1].FG), hex(row[1].BG), hex(row[0].FG), hex(row[0].BG))
	}
}

// TestRuneWidthMatchesMeasuredTmux 逐个钉住 runeWidth 的实测值。
//
// 歧义宽那一组是本条存在的理由：本机 tmux 把 ─ │ ★ █ 都排成 **1** 列，
// 而 go-runewidth 在 LANG=zh_CN.UTF-8 下对它们全报 2。被调试的 TUI 用
// lipgloss.RoundedBorder() 画框（整套都是歧义宽制表符），选错宽度函数会让
// 每一条带框的行整体错位。这些期望值来自 tmux 3.7c 的 #{cursor_x}，不是来自任何库。
func TestRuneWidthMatchesMeasuredTmux(t *testing.T) {
	cases := []struct {
		r    rune
		want int
		why  string
	}{
		{'A', 1, "ASCII"},
		{'中', 2, "CJK 表意文字"},
		{'あ', 2, "平假名"},
		{'한', 2, "谚文"},
		{'。', 2, "CJK 标点"},
		{'～', 2, "全角波浪号 U+FF5E（Fullwidth 类）"},
		{'😀', 2, "emoji U+1F600"},
		{'ｱ', 1, "半角片假名 U+FF71（Halfwidth 类）"},
		// 以下全是 EastAsianAmbiguous：tmux 实测 1 列，go-runewidth 在本机报 2。
		{'─', 1, "制表符 U+2500，歧义宽 —— tmux 实测 1 列"},
		{'│', 1, "制表符 U+2502，歧义宽 —— lipgloss 边框用它"},
		{'★', 1, "U+2605，歧义宽"},
		{'→', 1, "U+2192，歧义宽"},
		{'█', 1, "U+2588，歧义宽"},
		{'±', 1, "U+00B1，歧义宽"},
		{'✓', 1, "U+2713，Neutral"},
		// 零宽。
		{'​', 0, "零宽空格 U+200B"},
		{'́', 0, "组合锐音符 U+0301"},
	}
	for _, c := range cases {
		if got := runeWidth(c.r); got != c.want {
			t.Errorf("runeWidth(%q U+%04X) = %d, want %d（%s）", c.r, c.r, got, c.want, c.why)
		}
	}
}

// TestAmbiguousWidthRunesDoNotDoubleCount 用一条真实形状的边框行，
// 把歧义宽的判决落到"列数"这个可观测量上。
//
// 单独立一条，是因为 TestRuneWidthMatchesMeasuredTmux 测的是内部函数：
// 即使 runeWidth 正确，parseANSI 里若另有一处宽度判断也可能错。
// 这里量的是最终网格。
func TestAmbiguousWidthRunesDoNotDoubleCount(t *testing.T) {
	line := "╭" + strings.Repeat("─", 20) + "╮"
	row := firstRow(t, line)
	if len(row) != 22 {
		t.Errorf("边框行 %q 占 %d 列, want 22 —— 歧义宽字符被数成了两列，"+
			"每条带框的行都会按自身宽度整体错位", line, len(row))
	}
}

// TestZeroWidthRunesDoNotConsumeAColumn 保证组合符与零宽字符不占列。
func TestZeroWidthRunesDoNotConsumeAColumn(t *testing.T) {
	// "e" + 组合锐音符：实测 tmux 前进 1 列。
	row := firstRow(t, "éX")
	if len(row) != 2 {
		t.Errorf("\"e\"+U+0301+\"X\" 占 %d 列, want 2 —— 组合符占了自己的列", len(row))
	}
	if len(row) == 2 && row[1].R != 'X' {
		t.Errorf("第 1 格 = %q, want 'X'", row[1].R)
	}
}

// TestEmptyAndPlainInput 覆盖两个退化输入。
func TestEmptyAndPlainInput(t *testing.T) {
	if grid := parseANSI(""); len(grid) != 0 {
		t.Errorf("parseANSI(\"\") 得到 %d 行, want 0", len(grid))
	}

	// 完全没有转义的纯文本。
	grid := parseANSI("hello\nworld")
	if len(grid) != 2 {
		t.Fatalf("两行纯文本得到 %d 行", len(grid))
	}
	if len(grid[0]) != 5 || len(grid[1]) != 5 {
		t.Fatalf("行长 = %d/%d, want 5/5", len(grid[0]), len(grid[1]))
	}
	for _, c := range grid[0] {
		if c.FG != defaultFG || c.BG != defaultBG {
			t.Errorf("无转义时字符应带默认色，得到 fg=%s bg=%s", hex(c.FG), hex(c.BG))
		}
		if c.Bold {
			t.Error("无转义时不该是粗体")
		}
	}

	// 只有转义、没有可见字符 —— 不该造出一行空行。
	if grid := parseANSI("\x1b[38;5;99m\x1b[39m"); len(grid) != 0 {
		t.Errorf("只含转义的输入得到 %d 行, want 0", len(grid))
	}
}

// TestRowSplitting 覆盖换行的三种边界形态。
//
// 末尾那一行尤其重要：tmux 的 capture-pane 不给最后一行收尾，
// 丢掉它等于每张截图都静默少掉最下面一行。
func TestRowSplitting(t *testing.T) {
	if got := len(parseANSI("a\nb")); got != 2 {
		t.Errorf("\"a\\nb\" 得到 %d 行, want 2 —— 末尾无换行的那行被丢了", got)
	}
	if got := len(parseANSI("a\nb\n")); got != 2 {
		t.Errorf("\"a\\nb\\n\" 得到 %d 行, want 2 —— 末尾换行造出了幽灵空行", got)
	}
	// 中间的空行必须保留：TUI 靠它对齐。
	grid := parseANSI("a\n\nb")
	if len(grid) != 3 {
		t.Fatalf("\"a\\n\\nb\" 得到 %d 行, want 3", len(grid))
	}
	if len(grid[1]) != 0 {
		t.Errorf("中间那行长 = %d, want 0", len(grid[1]))
	}
	// CRLF 不能在行尾留下控制字符。
	grid = parseANSI("a\r\nb")
	if len(grid) != 2 || len(grid[0]) != 1 {
		t.Fatalf("CRLF: 得到 %d 行、首行 %d 格, want 2 行、首行 1 格", len(grid), len(grid[0]))
	}
	// 样式必须跨行保持 —— tmux 每行开头不重发一次 SGR。
	grid = parseANSI("\x1b[38;5;99ma\nb")
	if grid[1][0].FG != rgb(0x87, 0x5f, 0xff) {
		t.Errorf("第二行的前景 = %s，want 与第一行相同 —— 样式没跨行保持", hex(grid[1][0].FG))
	}
}

// TestMalformedInputDoesNotPanic 覆盖畸形与截断输入。
//
// 全部只要求"不 panic 且不漏出转义字节"：畸形输入没有唯一正确的渲染，
// 但有一个唯一正确的失败方式 —— 别崩、别把垃圾画到屏幕上。
func TestMalformedInputDoesNotPanic(t *testing.T) {
	inputs := []string{
		"\x1b[",                      // 规格点名的形态：末尾一个裸的 CSI 引导
		"\x1b",                       // 孤零零一个 ESC
		"\x1b[38;5;",                 // 截断在参数中间
		"\x1b[38;5",                  // 同上，无尾分号
		"\x1b[38;2;1;2",              // 真彩截断
		"\x1b[38",                    // 只有 38
		"\x1b[48",                    // 只有 48
		"\x1b]0;unterminated",        // 未终结的 OSC
		"\x1b]",                      // 空 OSC
		"\x1b[;;;m",                  // 全空参数
		"\x1b[99999999999999999999m", // 超长数字（不能溢出成别的码）
		"\x1b[38;5;99mA\x1b[",        // 正常内容后跟截断
		"\xff\xfe",                   // 非法 UTF-8
		"A\xffB",                     // 正文中间的非法字节
		"\x1b[38;5;-1mA",             // 负号（不合法的参数字节）
		"\x00\x01\x07A",              // C0 控制字符
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("parseANSI(%q) panic: %v", in, r)
				}
			}()
			grid := parseANSI(in)
			for _, row := range grid {
				for i, c := range row {
					if c.R == 0x1b {
						t.Errorf("parseANSI(%q) 第 %d 格漏出了 ESC 字节", in, i)
					}
					if c.R != 0 && c.R < 0x20 {
						t.Errorf("parseANSI(%q) 第 %d 格漏出了控制字符 U+%04X", in, i, c.R)
					}
				}
			}
		}()
	}
}

// TestScanEscapeAlwaysAdvances 保证 scanEscape 绝不返回 0。
//
// 这是 parseANSI 不死循环的**唯一**依据。返回 0 会让主循环原地打转、
// 测试挂死而不是失败 —— 挂死比失败更难诊断，所以单独钉住这个不变量。
func TestScanEscapeAlwaysAdvances(t *testing.T) {
	inputs := []string{
		"\x1b", "\x1b[", "\x1b]", "\x1b[m", "\x1b[38;5;99m",
		"\x1b[2J", "\x1bc", "\x1b\x1b", "\x1b[?", "\x1b]0;x\x07",
	}
	for _, in := range inputs {
		n, _, _ := scanEscape(in)
		if n < 1 {
			t.Errorf("scanEscape(%q) = %d —— 必须 ≥1，否则 parseANSI 会死循环", in, n)
		}
		if n > len(in) {
			t.Errorf("scanEscape(%q) = %d，超过了输入长度 %d", in, n, len(in))
		}
	}
}

// TestAtoiSGRClamps 覆盖参数解析的边界。
func TestAtoiSGRClamps(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0}, {"0", 0}, {"7", 7}, {"99", 99}, {"255", 255},
		{"-1", 0},                         // 负号不是合法参数字节 → 退化成 0
		{"1a", 0},                         // 混入字母 → 退化成 0
		{"99999999999999999999", 1 << 20}, // 钳制而不是溢出回绕
	}
	for _, c := range cases {
		if got := atoiSGR(c.in); got != c.want {
			t.Errorf("atoiSGR(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestStripSGR 覆盖纯文本还原。
//
// 这条等式（stripSGR(带色抓屏) == 不带色抓屏）是调用方敢于总是带颜色抓屏的前提：
// --wait 的正则与退出标记都在抓屏文本上匹配。
func TestStripSGR(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\x1b[31mred\x1b[0m", "red"},
		{"plain", "plain"},
		{"", ""},
		{"\x1b[38;5;99mA\x1b[39mB", "AB"},
		{"\x1b[2Jx", "x"},
		{"\x1b]0;title\x07x", "x"},
		{"a\nb", "a\nb"},
		{"\x1b[", ""}, // 截断：吞掉，不留残渣
	}
	for _, c := range cases {
		if got := stripSGR(c.in); got != c.want {
			t.Errorf("stripSGR(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRealisticCapture 把上述各条放进一段形状接近真实抓屏的输入里跑一遍。
//
// 单条特性各自正确、组合起来仍可能错（状态在序列之间泄漏是最常见的形态），
// 所以这里按实测频次表里的形态混排一段，逐格核对。
func TestRealisticCapture(t *testing.T) {
	// 形态取自实测：256 色前景 + 默认前景 + 反显 + 256 色背景 + 全复位。
	in := "\x1b[38;5;99mAB\x1b[39mC\x1b[7mD\x1b[27m\x1b[48;5;236mE\x1b[0mF"
	row := firstRow(t, in)
	if len(row) != 6 {
		t.Fatalf("得到 %d 列, want 6", len(row))
	}
	purple := rgb(0x87, 0x5f, 0xff)
	wantCell(t, row[0], 'A', purple, defaultBG, "A：256 色前景")
	wantCell(t, row[1], 'B', purple, defaultBG, "B：颜色跨字符保持")
	wantCell(t, row[2], 'C', defaultFG, defaultBG, "C：39 复位前景")
	wantCell(t, row[3], 'D', defaultBG, defaultFG, "D：反显")
	wantCell(t, row[4], 'E', defaultFG, rgb(0x30, 0x30, 0x30), "E：27 关反显后设背景")
	wantCell(t, row[5], 'F', defaultFG, defaultBG, "F：0 全复位")

	// 同一段文本，含中文，检验宽度与颜色不互相干扰。
	row = firstRow(t, "\x1b[38;5;99m中文\x1b[39mAB")
	if len(row) != 6 {
		t.Fatalf("\"中文AB\" 得到 %d 列, want 6", len(row))
	}
	if row[4].R != 'A' {
		t.Errorf("第 4 列 = %q, want 'A' —— 宽字符的列账算错了", row[4].R)
	}
	if row[0].FG != purple || row[2].FG != purple {
		t.Error("中文字符没拿到 256 色前景")
	}
	if row[4].FG != defaultFG {
		t.Error("39 之后的 ASCII 没复位成默认前景")
	}
}
