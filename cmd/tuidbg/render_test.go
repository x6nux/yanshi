// render_test.go —— 光栅器的测试。
//
// 贯穿本文件的一条判据：**断言必须能区分「画对了」与「画了个空白」。**
// 一张全空白的 PNG 会通过「文件存在」「字节数非零」「能 png.Decode」的全部检查，
// 而这个工具的历史缺陷正是这一类。因此几乎每个测试最后都落在**扫像素**上，
// 而不是落在返回值非 nil 上。
//
// 另一条：颜色取值必须与 defaultFG/defaultBG 都不同。用默认色当测试值，
// 测试会因为「页面底色本来就是这个」而恒绿 —— 那种测试在删掉整个绘制循环之后
// 依然通过。本文件里的测试色都是刻意挑的、不会自然出现的值。

package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"

	"golang.org/x/image/font/sfnt"
)

// testFG、testBG 是刻意挑的、与 defaultFG(0xd4d4d4)/defaultBG(0x1e1e1e) 都不相同的颜色。
// 见文件头注释：拿默认色做测试值会让断言恒真。
var (
	testFG = color.RGBA{0xff, 0x00, 0x00, 0xff} // 纯红
	testBG = color.RGBA{0x00, 0x80, 0x00, 0xff} // 中绿
)

// requireFonts 加载字体链，机器上没有可用字体时整体跳过并说清原因。
func requireFonts(t *testing.T) *fontChain {
	t.Helper()
	fc, err := loadFontChain()
	if err != nil {
		t.Skipf("跳过：本机没有可用字体（%v）。渲染测试需要系统字体，"+
			"在 macOS 上是 Menlo/Songti，在 Linux 上是 DejaVu/Noto。", err)
	}
	t.Cleanup(fc.close)
	// 2026-09-01 Windows: the pixel-geometry assertions were written against
	// Menlo's measured numbers, and the Windows primary (Consolas) plus the
	// CJK fallback (msyh) broke four of them at once. Dump the whole metric
	// sheet once per test so the expectations can be re-derived from data
	// instead of guessed at across an ocean.
	p := fc.primary()
	m := p.face.Metrics()
	row := "字符\t步进ok\t步进px\t墨迹宽"
	_ = row
	for _, r := range []rune{'A', 'i', 'W', '0', ' ', '─', '│', '╭', '中', '国'} {
		adv, advOK := p.face.GlyphAdvance(r)
		t.Logf("METRIC %s primary=%s Height=%v Ascent=%v Descent=%v %q adv=%v(%t)",
			string(r), p.desc, m.Height, m.Ascent, m.Descent, r, adv, advOK)
	}
	t.Logf("METRIC grid: charW=%d lineH=%d ascent=%d pad=%d fontSize=%g fontDPI=%g",
		charW, lineH, ascent, pad, fontSize, fontDPI)
	return fc
}

// cell 造一个普通宽度 1 的 cell。
func cell(r rune, fg, bg color.RGBA) Cell {
	return Cell{R: r, FG: fg, BG: bg, Width: 1}
}

// countColour 数图中恰好等于 c 的像素个数。
func countColour(img *image.RGBA, c color.RGBA) int {
	n := 0
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if img.RGBAAt(x, y) == c {
				n++
			}
		}
	}
	return n
}

// countColourIn 数指定矩形内恰好等于 c 的像素个数。
func countColourIn(img *image.RGBA, r image.Rectangle, c color.RGBA) int {
	n := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if img.RGBAAt(x, y) == c {
				n++
			}
		}
	}
	return n
}

// cellRect 返回第 row 行第 col 列那一格的像素矩形。
// 刻意用与 renderGrid 相同的算式独立写一遍，好让「按格定位」这条约束
// 在测试侧有一个不依赖被测代码的参照。
func cellRect(row, col int) image.Rectangle {
	x := pad + col*charW
	y := pad + row*lineH
	return image.Rect(x, y, x+charW, y+lineH)
}

// inkBleed 是位置类断言容许字形墨迹越出格矩形的像素数。
//
// 它不为任何错位开口 —— 错位按整列计（≥charW）。它量的是**字体墨迹盒**与
// 格矩形的差，这是字体的性质不是渲染器的：Windows 主字体 Consolas 的 '█'
// 在本档位（Size=14/DPI=144/HintingFull）下的 hinted 位图实测有 60/480 个
// 像素落在 15x33 的格矩形之外、却仍然紧贴本格；CJK 回退 msyh 的字形墨迹
// 则触得到格子的第一行像素。这两条在 Menlo/Songti 上都不成立，所以任何
// 「墨迹必须完全在格子里 / 必须碰不到边缘行」的断言都只对一台机器为真。
// 4px 盖住实测的出血量，离「错一格」还差着一个列宽。
const inkBleed = 4

// fgSpanX 返回图上恰好等于 fg 的像素的横向范围。没有时返回 (-1,-1)。
//
// 与 inkSpanX 的差别：只认纯前景色，不含抗锯齿的混色边，也**不含其它前景色**
// 画出的墨迹 —— 当一行里同时有 testFG 与 defaultFG 的字形时，量某一者的
// 跨度必须用它，否则另一者的墨会把范围拉到自己的格子里去。
func fgSpanX(img *image.RGBA, fg color.RGBA) (minX, maxX int) {
	minX, maxX = -1, -1
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if img.RGBAAt(x, y) == fg {
				if minX == -1 || x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
			}
		}
	}
	return minX, maxX
}

// --- 字体链 ---

func TestFontChainLoads(t *testing.T) {
	fc := requireFonts(t)
	if len(fc.fonts) == 0 {
		t.Fatal("字体链为空，但 loadFontChain 没有报错")
	}
	for _, lf := range fc.fonts {
		t.Logf("已加载 %s", lf.desc)
		if lf.font == nil || lf.face == nil {
			t.Errorf("%s: font 或 face 为 nil", lf.desc)
		}
	}
}

// TestPrimaryFontHasBoxDrawing 钉住「制表符必须由主字体提供」。
//
// 这是回退顺序那条约束的正面：主字体缺了任何一个制表符，就意味着它会回退到
// CJK 字体，而实测那会画出错位的边框（Songti 的制表符步进是 28.0 而非 17.0）。
// 断言用 GlyphIndex != 0，与 resolve 的判据保持一致 —— 理由见 resolve 的注释
// （简言之：ok 的语义是当前 x/image 版本的实现细节，不是接口约定）。
func TestPrimaryFontHasBoxDrawing(t *testing.T) {
	fc := requireFonts(t)
	primary := fc.primary()
	var buf sfnt.Buffer
	for _, r := range []rune{'─', '│', '╭', '╯', '┃', '╰', '┤', '├'} {
		gi, err := primary.font.GlyphIndex(&buf, r)
		if err != nil {
			t.Errorf("主字体 %s: GlyphIndex(%q) 报错 %v", primary.desc, r, err)
			continue
		}
		if gi == 0 {
			t.Errorf("主字体 %s 缺少制表符 %q（GlyphIndex==0）："+
				"它会回退到 CJK 字体并画出错位的边框", primary.desc, r)
		}
	}
}

// TestChainRendersCJK 断言整条链里有人能提供汉字。
func TestChainRendersCJK(t *testing.T) {
	fc := requireFonts(t)
	lf := fc.resolve('中')
	var buf sfnt.Buffer
	gi, err := lf.font.GlyphIndex(&buf, '中')
	if err != nil {
		t.Fatalf("GlyphIndex(中) 报错：%v", err)
	}
	if gi == 0 {
		t.Fatalf("整条字体链都画不出 '中'（选中的是 %s）", lf.desc)
	}
	t.Logf("'中' 由 %s 提供", lf.desc)
}

// TestBoxDrawingResolvesToPrimaryNotCJK 直接钉住回退**方向**。
//
// 上面那个测试保证主字体有制表符，本测试保证 resolve 真的把它路由到主字体。
// 两个都要：只测「主字体有」的话，把 resolve 改成无条件返回最后一个字体
// 依然全绿。
func TestBoxDrawingResolvesToPrimaryNotCJK(t *testing.T) {
	fc := requireFonts(t)
	if len(fc.fonts) < 2 {
		t.Skip("跳过：只加载到一个字体，无从检验回退方向")
	}
	primary := fc.primary()
	for _, r := range []rune{'─', '│', '╭', '╯', '┃', '╰', '┤', '├'} {
		if got := fc.resolve(r); got != primary {
			t.Errorf("制表符 %q 被解析到 %s，应当留在主字体 %s 上",
				r, got.desc, primary.desc)
		}
	}
}

// TestResolveFallsBackForMissingGlyph 断言回退**会**发生。
//
// 与上一个测试成对：那个防「回退过度」，这个防「根本不回退」。
// 少了这个，把 resolve 改成恒返回 fonts[0] 依然全绿（而汉字会全变豆腐块）。
func TestResolveFallsBackForMissingGlyph(t *testing.T) {
	fc := requireFonts(t)
	if len(fc.fonts) < 2 {
		t.Skip("跳过：只加载到一个字体，无从检验回退")
	}
	var buf sfnt.Buffer
	primary := fc.primary()
	gi, err := primary.font.GlyphIndex(&buf, '中')
	if err == nil && gi != 0 {
		t.Skip("跳过：主字体自带汉字，本机构不出回退场景")
	}
	if got := fc.resolve('中'); got == primary {
		t.Error("主字体没有 '中' 却没有发生回退：整屏汉字都会是豆腐块")
	}
}

// --- 失败路径 ---

// TestNoFontsAvailableIsAnError 是本文件里最重要的一个测试。
//
// 「一个字体都没有」必须返回错误，而**不是**一张空白图。这个工具有四次
// 把失败报成成功的历史，而空白 PNG 正是那个模式最容易复发的形态。
func TestNoFontsAvailableIsAnError(t *testing.T) {
	fc, err := loadFontChainFrom([]fontCandidate{
		{"/nonexistent/definitely-not-a-font.ttf", 0},
		{"/nonexistent/another-missing.ttc", 3},
	})
	if err == nil {
		t.Fatal("没有任何可用字体时 loadFontChainFrom 返回了 nil error")
	}
	if !errorIsNoFonts(err) {
		t.Errorf("期望 errNoFonts，得到 %v", err)
	}
	if fc != nil {
		t.Error("返回错误的同时还返回了非 nil 的字体链")
	}
}

// TestRenderGridPNGWritesNothingOnFontFailure 钉住「失败时不产出字节」。
//
// 只断言 renderGridPNG 返回错误是不够的：一个先编码、后检查的实现会
// 既返回错误、又已经把一张空白图写进了 w，调用方那边就留下一个
// 「大小正常、内容全空」的文件。
func TestRenderGridPNGWritesNothingOnFontFailure(t *testing.T) {
	orig := fontCandidatesForTest
	fontCandidatesForTest = []fontCandidate{{"/nonexistent/nope.ttf", 0}}
	defer func() { fontCandidatesForTest = orig }()

	grid := [][]Cell{{cell('A', testFG, testBG)}}
	var buf bytes.Buffer
	err := renderGridPNG(grid, &buf)
	if err == nil {
		t.Fatal("没有字体时 renderGridPNG 返回了 nil error")
	}
	if buf.Len() != 0 {
		t.Errorf("字体加载失败时仍写出了 %d 字节（应当一个字节都不写，"+
			"否则调用方会留下一张空白 PNG）", buf.Len())
	}
}

// errorIsNoFonts 判断 err 是否就是 errNoFonts。
func errorIsNoFonts(err error) bool { return err == errNoFonts }

// --- 尺寸 ---

// TestImageSizeFollowsGridNotFontMetrics 断言尺寸由网格与常量算出。
//
// 尺寸若取自字体度量，会随「这一屏碰巧有没有汉字」而变（Songti 的 Height
// 是 40.0、Menlo 是 33.0），同一个 TUI 的两张截图尺寸就会不同。
func TestImageSizeFollowsGridNotFontMetrics(t *testing.T) {
	cases := []struct{ rows, cols int }{{1, 1}, {3, 10}, {30, 100}}
	for _, c := range cases {
		grid := make([][]Cell, c.rows)
		for i := range grid {
			row := make([]Cell, c.cols)
			for j := range row {
				row[j] = cell('x', testFG, defaultBG)
			}
			grid[i] = row
		}
		wantW := pad*2 + c.cols*charW
		wantH := pad*2 + c.rows*lineH
		gotW, gotH := imageSize(grid)
		if gotW != wantW || gotH != wantH {
			t.Errorf("%dx%d 网格：得到 %dx%d，期望 %dx%d",
				c.rows, c.cols, gotW, gotH, wantW, wantH)
		}
	}
}

// TestImageSizeUsesLongestRow 钉住「列数取各行最长者」。
//
// tmux 会截掉行尾空白，各行长度参差是常态；拿第一行当宽度会裁掉更长的行。
func TestImageSizeUsesLongestRow(t *testing.T) {
	grid := [][]Cell{
		{cell('a', testFG, defaultBG)},
		{cell('a', testFG, defaultBG), cell('b', testFG, defaultBG), cell('c', testFG, defaultBG)},
		{cell('a', testFG, defaultBG), cell('b', testFG, defaultBG)},
	}
	wantW := pad*2 + 3*charW
	if gotW, _ := imageSize(grid); gotW != wantW {
		t.Errorf("宽度 %d，期望 %d（应取最长行的 3 列，而不是第一行的 1 列）", gotW, wantW)
	}
}

func TestRenderGridDimensionsMatchImageSize(t *testing.T) {
	fc := requireFonts(t)
	grid := [][]Cell{
		{cell('h', testFG, defaultBG), cell('i', testFG, defaultBG)},
		{cell('y', testFG, defaultBG)},
	}
	img := renderGrid(grid, fc)
	wantW, wantH := imageSize(grid)
	if b := img.Bounds(); b.Dx() != wantW || b.Dy() != wantH {
		t.Errorf("图片 %dx%d，imageSize 说是 %dx%d", b.Dx(), b.Dy(), wantW, wantH)
	}
}

// TestLayoutConstantsMatchPrimaryFontMetrics 把版面常量钉回它们的**来源**。
//
// 这个测试是变异测试逼出来的。在它之前，把 charW 从 17 改成 20、lineH 从 33
// 改成 40、pad 改成 0，**整个测试套件照样全绿** —— 因为其余每一个测试都拿这些
// 常量自己去算期望值（imageSize、cellRect 全是如此），于是常量取什么值都自洽。
// 那是「测试值本身是惰性的」这一类缺陷：断言写得挺多，却没有一条把常量与
// 它声称的来源联系起来。
//
// 这里补上那条联系：常量既然写着「Menlo 在 Size=14/DPI=144 下实测得来」，
// 就必须真的等于主字体在那组参数下报出来的度量。任何一边漂了，这里立刻红。
//
// 注意这**不违反**「图片尺寸不得取自字体度量」那条约束：那条管的是**渲染路径**
// （尺寸不能随「这一屏碰巧有没有汉字」而变），而这里是**测试**在核对常量的出处。
// 一个是运行时行为，一个是编译期事实。
func TestLayoutConstantsMatchPrimaryFontMetrics(t *testing.T) {
	fc := requireFonts(t)
	primary := fc.primary()
	m := primary.face.Metrics()

	if got := m.Height.Round(); got != lineH {
		t.Errorf("lineH=%d，但主字体 %s 在 Size=%g/DPI=%g 下报出的 Height 是 %d",
			lineH, primary.desc, fontSize, fontDPI, got)
	}
	if got := m.Ascent.Round(); got != ascent {
		t.Errorf("ascent=%d，但主字体 %s 报出的 Ascent 是 %d",
			ascent, primary.desc, got)
	}
	// 主字体必须是等宽的，且步进恰好等于 charW —— 「按格定位」整套算式
	// 就建立在这个等式上。逐个字符查，顺带把「主字体其实不等宽」也拦下来。
	for _, r := range []rune{'A', 'i', 'W', 'l', '0', ' ', '─', '│', '╭', '┃'} {
		adv, ok := primary.face.GlyphAdvance(r)
		if !ok {
			t.Errorf("主字体 %s 取不到 %q 的步进", primary.desc, r)
			continue
		}
		if got := adv.Round(); got != charW {
			t.Errorf("charW=%d，但主字体 %s 的 %q 步进是 %d："+
				"主字体要么不是等宽的，要么 charW 与它对不上",
				charW, primary.desc, r, got)
		}
	}
	t.Logf("主字体 %s：Height=%d Ascent=%d 步进=%d（常量 lineH=%d ascent=%d charW=%d）",
		primary.desc, m.Height.Round(), m.Ascent.Round(),
		func() int { a, _ := primary.face.GlyphAdvance('A'); return a.Round() }(),
		lineH, ascent, charW)
}

// TestPrimaryAdvanceIsExactlyIntegral 钉住 HintingFull 这个选择。
//
// 变异测试发现把 Hinting 从 Full 改成 None 之后整套测试照样全绿。查下去：
// 度量确实变了（步进 17:00 → 16:55，即 16 + 55/64），只是**四舍五入之后
// 仍然是 17**，于是所有按整数比较的断言都看不出差别。
//
// 但这个差别是有意义的：HintingFull 让步进**恰好**等于整数像素，
// 于是「x = pad + col*charW」这套整数算式与字体自己的步进严丝合缝。
// 步进带小数时，字形的实际着墨位置与格子边界之间会有随列号累积的亚像素偏差，
// 表现为整行字忽明忽暗、边框接缝处若隐若现 —— 一种正好逃过结构断言、
// 只能靠人眼在成图上发现的模糊。
//
// 所以这里断言的是「小数部分为 0」而不是某个具体值：
// 它直接表达了那条真正被依赖的性质。
func TestPrimaryAdvanceIsExactlyIntegral(t *testing.T) {
	fc := requireFonts(t)
	primary := fc.primary()
	for _, r := range []rune{'A', '─', '│'} {
		adv, ok := primary.face.GlyphAdvance(r)
		if !ok {
			t.Errorf("取不到 %q 的步进", r)
			continue
		}
		// fixed.Int26_6 的低 6 位是小数部分。
		if frac := adv & 0x3f; frac != 0 {
			t.Errorf("主字体 %q 的步进是 %v（小数部分 %d/64），不是整数像素："+
				"Hinting 不是 HintingFull 会让字形位置产生亚像素偏差",
				r, adv, frac)
		}
	}
}

// TestPaddingIsPositive 钉住 pad > 0。
//
// pad 是纯美观值、没有对应的字体度量可对账，但它有一条实打实的职责：
// 让最外圈的字形不至于紧贴图片边缘。pad=0 时套件其余部分依然全绿
// （所有期望值都从 pad 算出来），所以这条得单独写。
func TestPaddingIsPositive(t *testing.T) {
	if pad <= 0 {
		t.Fatalf("pad=%d：内容会紧贴图片边缘，最外圈字形有被裁掉的风险", pad)
	}
	grid := [][]Cell{{cell('x', testFG, defaultBG)}}
	w, h := imageSize(grid)
	if w <= charW || h <= lineH {
		t.Errorf("单格网格算出 %dx%d，没有为 padding 留出空间", w, h)
	}
}

// TestResolveIsStableAcrossRepeatedLookups 钉住 resolve 的缓存。
//
// 变异测试找出来的缺口：把缓存写成 `fc.resolved[r] = 0`（存错下标）之后，
// 首次查询返回正确字体、**第二次开始返回链首** —— 于是一屏里第一个汉字画得
// 好好的，其余每一个都是豆腐块。这是最难从截图上归因的一类损坏，因为它看起来
// 像是「某些字符不支持」而不是「缓存写错了」。
//
// 逐格渲染必然对同一个字符反复调用 resolve，所以这条稳定性是承重的。
func TestResolveIsStableAcrossRepeatedLookups(t *testing.T) {
	fc := requireFonts(t)
	for _, r := range []rune{'中', 'A', '─', '│', '你', '0'} {
		first := fc.resolve(r)
		for i := 0; i < 3; i++ {
			if got := fc.resolve(r); got != first {
				t.Errorf("%q 第 %d 次查询得到 %s，首次是 %s："+
					"缓存存错了下标，一屏里只有第一个该字符是对的",
					r, i+2, got.desc, first.desc)
				break
			}
		}
	}
}

// TestRepeatedCJKAllRenderTheSame 是上一个测试的像素版。
//
// 缓存坏掉时，同一个汉字重复出现，第一个是真字形、后面全是豆腐块。
// 这里直接比较两格的像素：同一个字符在同样的背景上必须画出同样的墨迹量。
func TestRepeatedCJKAllRenderTheSame(t *testing.T) {
	fc := requireFonts(t)
	row := []Cell{
		{R: '中', FG: testFG, BG: defaultBG, Width: 2},
		{R: 0, FG: testFG, BG: defaultBG, Width: 0},
		{R: '中', FG: testFG, BG: defaultBG, Width: 2},
		{R: 0, FG: testFG, BG: defaultBG, Width: 0},
	}
	img := renderGrid([][]Cell{row}, fc)

	first := countColourIn(img, image.Rect(pad, pad, pad+2*charW, pad+lineH), testFG)
	second := countColourIn(img, image.Rect(pad+2*charW, pad, pad+4*charW, pad+lineH), testFG)
	if first == 0 {
		t.Fatal("第一个汉字没画出来")
	}
	if first != second {
		t.Errorf("同一个汉字两次出现的墨迹量不同（%d vs %d）："+
			"多半是 resolve 的缓存坏了，只有第一个用对了字体", first, second)
	}
}

// TestFontIndexOutOfRangeIsRejected 钉住候选下标的边界检查。
//
// .ttc 里的字体数量因系统版本而异，一个越界的下标必须被当作「这个候选不可用」
// 而跳过，不能 panic，也不能悄悄退化成下标 0 —— 后者会让「我要 Menlo Regular」
// 静默变成「随便哪个 Menlo」，而实测 Menlo Bold 缺全部制表符。
func TestFontIndexOutOfRangeIsRejected(t *testing.T) {
	real := fontCandidates()
	if len(real) == 0 {
		t.Skip("跳过：本平台没有候选字体")
	}
	if _, err := loadFont(fontCandidate{real[0].path, 9999}); err == nil {
		t.Error("越界的字体下标被接受了；它应当让这个候选不可用")
	}
	if _, err := loadFont(fontCandidate{real[0].path, -1}); err == nil {
		t.Error("负的字体下标被接受了")
	}
	// 越界候选必须只是被跳过，不能让整条链失败。
	cands := append([]fontCandidate{{real[0].path, 9999}}, real...)
	fc, err := loadFontChainFrom(cands)
	if err != nil {
		t.Fatalf("一个越界候选让整条链都失败了：%v", err)
	}
	defer fc.close()
	if fc.primary().desc == real[0].path+"#9999" {
		t.Error("越界候选混进了字体链")
	}
}

// --- 真的画上去了吗 ---

// TestGlyphIsActuallyDrawn 扫像素确认前景色真的出现在图上。
//
// 这个断言是整个文件的地基：删掉 renderGrid 里的绘制调用之后，
// 「PNG 存在且非空」类的检查全部照样通过，只有这个会红。
func TestGlyphIsActuallyDrawn(t *testing.T) {
	fc := requireFonts(t)
	// 用实心块 █ 而不是字母：它铺满整格，前景像素数量多且稳定，
	// 不会因为字体的字形细节而在某台机器上恰好只有个位数像素。
	grid := [][]Cell{{Cell{R: '█', FG: testFG, BG: testBG, Width: 1}}}
	img := renderGrid(grid, fc)

	if n := countColour(img, testFG); n == 0 {
		t.Fatal("图上没有一个前景色像素：字形根本没画上去" +
			"（这正是「PNG 写出来了、上面什么都没有」那个失败模式）")
	} else {
		t.Logf("前景像素 %d 个", n)
	}
}

// TestGlyphPixelsLandInTheirOwnCell 钉住「按格定位」。
//
// 只断言「图上有前景色」是不够的 —— 画在哪儿都算过。本测试把同一个字符
// 放在不同列上，要求它的墨迹**以自己那一列为界**。
//
// 判据不能写成「inCell == total」：那是主字体的性质不是渲染器的性质。
// Menlo 的 '█' 位图恰好缩在自己 17x33 的格子里，而 Windows 主字体 Consolas
// 同档位的 '█' hinted 位图会越出格矩形（实测 480 个前景像素里 60 个在外、
// 却紧贴本格）—— 那是字体把字画多大，不是渲染器把字画在哪儿。真正的错位
// （按步进累加、或少算一格）会把整块墨迹推过一个列宽，远超 inkBleed。
func TestGlyphPixelsLandInTheirOwnCell(t *testing.T) {
	fc := requireFonts(t)
	const col = 5
	row := make([]Cell, col+1)
	for i := range row {
		row[i] = Cell{R: ' ', FG: testFG, BG: defaultBG, Width: 1}
	}
	row[col] = Cell{R: '█', FG: testFG, BG: defaultBG, Width: 1}
	img := renderGrid([][]Cell{row}, fc)

	r := cellRect(0, col)
	inCell := countColourIn(img, r, testFG)
	total := countColour(img, testFG)
	if inCell == 0 {
		t.Fatalf("第 %d 列的字形在它自己的格子里一个像素都没有", col)
	}
	minX, maxX := fgSpanX(img, testFG)
	if minX < r.Min.X-inkBleed || maxX > r.Max.X-1+inkBleed {
		t.Errorf("第 %d 列字形的墨迹 x=[%d,%d]，而格子是 x=[%d,%d]（容差 %dpx）："+
			"字形没有落在自己的列里", col, minX, maxX, r.Min.X, r.Max.X-1, inkBleed)
	}
	t.Logf("前景像素共 %d 个，格内 %d 个，墨迹 x=[%d,%d]（格子 x=[%d,%d]，容差 %dpx）",
		total, inCell, minX, maxX, r.Min.X, r.Max.X-1, inkBleed)
}

// TestPositioningIsByColumnNotAccumulatedAdvance 是「按格定位」那条约束的
// 变异杀手。
//
// 构造一行「汉字 + 续格 + ASCII」：按列定位时那个 ASCII 从第 2 列的左边界
// （x = pad + 2*charW）开始；按步进累加时，它被前面那个回退汉字的步进往左推
// —— darwin 上 Songti 步进 28.0 对两列 34，差 6px；Windows 上 msyh 步进 28
// 对两列 30，只差 2px，与字体的墨迹出血同量级，那条方向在这里测不出来，
// 所以承重的是「墨迹不得越过自己那格的右边界」与上一个测试共用的列界判据。
//
// 承重断言是**左界**：inkBleed 容得下字体墨迹盒的出血，容不下整列的推移。
func TestPositioningIsByColumnNotAccumulatedAdvance(t *testing.T) {
	fc := requireFonts(t)
	if len(fc.fonts) < 2 {
		t.Skip("跳过：只有一个字体，构不出「回退字体步进不同」的场景")
	}
	grid := [][]Cell{{
		Cell{R: '中', FG: defaultFG, BG: defaultBG, Width: 2},
		Cell{R: 0, FG: defaultFG, BG: defaultBG, Width: 0},
		Cell{R: '█', FG: testFG, BG: defaultBG, Width: 1},
	}}
	img := renderGrid(grid, fc)

	total := countColour(img, testFG)
	if total == 0 {
		t.Fatal("汉字后面那个字符根本没画出来")
	}
	// 只扫 testFG 的纯色像素：'中' 用 defaultFG 画，混进来会把墨迹左界拉到
	// 第 0 格，左界断言就失去意义。
	minX, maxX := fgSpanX(img, testFG)
	ownX, ownEnd := pad+2*charW, pad+3*charW-1
	if minX < ownX-inkBleed {
		t.Errorf("汉字之后的字符墨迹从 x=%d 开始，低于它自己那格的左边界 x=%d（容差 %dpx）："+
			"定位在按步进累加，而不是按列", minX, ownX, inkBleed)
	}
	if maxX > ownEnd+inkBleed {
		t.Errorf("汉字之后的字符墨迹到 x=%d，越过它自己那格的右边界 x=%d（容差 %dpx）："+
			"它画进了右邻格", maxX, ownEnd, inkBleed)
	}
	t.Logf("汉字之后的字符共 %d 个前景像素，墨迹 x=[%d,%d]，自己那格 x=[%d,%d]",
		total, minX, maxX, ownX, ownEnd)
}

// inkSpanX 返回图上「非 bg 色」像素的横向范围（墨迹跨度）。没有墨迹时返回 (-1,-1)。
func inkSpanX(img *image.RGBA, bg color.RGBA) (minX, maxX int) {
	minX, maxX = -1, -1
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if img.RGBAAt(x, y) != bg {
				if minX == -1 || x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
			}
		}
	}
	return minX, maxX
}

// TestWideGlyphSpansBothColumns 是变异测试挖出来的那个真 bug 的回归测试。
//
// **上面那个续格背景测试单独存在时是骗人的。** 原实现逐格「铺背景 → 画字形」，
// 于是第 0 格画出的汉字横跨到第 1 格之后，紧接着第 1 格的背景填充就把它右半边
// 抹掉了 —— 实测墨迹被齐齐切在 x=pad+charW 处，屏幕上每个汉字都只剩左半边。
// 而续格背景测试**照样全绿**：背景确实铺满了，只不过是靠抹掉半个字形换来的。
//
// 所以这里直接量墨迹跨度：一个双宽字形必须伸过第一列的右边界。
// 判据取「跨度 > charW」而不是某个精确值，这样它不绑定具体字体的字形宽度。
func TestWideGlyphSpansBothColumns(t *testing.T) {
	fc := requireFonts(t)
	grid := [][]Cell{{
		Cell{R: '中', FG: testFG, BG: defaultBG, Width: 2},
		Cell{R: 0, FG: testFG, BG: defaultBG, Width: 0},
	}}
	img := renderGrid(grid, fc)

	minX, maxX := inkSpanX(img, defaultBG)
	if minX < 0 {
		t.Fatal("双宽字符一点墨迹都没有：字形没画出来")
	}
	boundary := pad + charW // 第 0 格的右边界
	if maxX < boundary {
		t.Errorf("双宽字形的墨迹 x=[%d,%d] 完全落在第一列内（右边界 %d）："+
			"字形被续格的背景填充抹掉了右半边，每个汉字都只剩一半",
			minX, maxX, boundary)
	}
	if w := maxX - minX + 1; w <= charW {
		t.Errorf("双宽字形墨迹只有 %d 像素宽，不超过单列的 %d：它没有真的跨两列",
			w, charW)
	}
	t.Logf("双宽字形墨迹 x=[%d,%d]（宽 %d），第 0 格右边界 %d",
		minX, maxX, maxX-minX+1, boundary)
}

// TestWideGlyphIsCentredInItsTwoColumns 钉住居中偏移。
//
// 回退字体的汉字步进是 28.0，而两列是 34px。不居中的话字形整体左靠，
// 与右邻格之间留一道不对称的缝 —— 结构上不算错，视觉上是歪的，
// 而这个工具的产出就是给人看的。
//
// 判据是左右留白之差 ≤ 2px（整数除法本身就会带来 1px 的不对称），
// 而不是某个精确的偏移值，这样它不绑定具体字体的度量。
func TestWideGlyphIsCentredInItsTwoColumns(t *testing.T) {
	fc := requireFonts(t)
	if len(fc.fonts) < 2 {
		t.Skip("跳过：只有一个字体，构不出「步进小于格宽」的场景")
	}
	grid := [][]Cell{{
		Cell{R: '中', FG: testFG, BG: defaultBG, Width: 2},
		Cell{R: 0, FG: testFG, BG: defaultBG, Width: 0},
	}}
	img := renderGrid(grid, fc)

	minX, maxX := inkSpanX(img, defaultBG)
	if minX < 0 {
		t.Fatal("双宽字符一点墨迹都没有")
	}
	spanLeft, spanRight := pad, pad+2*charW-1
	left, right := minX-spanLeft, spanRight-maxX
	if d := left - right; d > 2 || d < -2 {
		t.Errorf("双宽字形在两列内没有居中：左留白 %d、右留白 %d（差 %d）。"+
			"不居中会让每个汉字与右邻格之间出现一道不对称的缝", left, right, d)
	}
	t.Logf("双宽字形留白：左 %d、右 %d", left, right)
}

// TestBoxDrawingIsContinuousAcrossCells 断言相邻制表符之间**没有缝**。
//
// 这条是「边框看起来对不对」里唯一能机器判的部分，也正是人眼在缩略图上
// 最容易漏掉的部分：一条由 4 个 ─ 拼成的横线，若每格右边缘差一个像素，
// 在图上表现为极淡的虚线，缩小看完全正常。
//
// 它同时是「charW 必须等于主字体步进」那条约束的视觉后果：两者一旦不等，
// 这里立刻出现规律性的缝。
func TestBoxDrawingIsContinuousAcrossCells(t *testing.T) {
	fc := requireFonts(t)
	const n = 4
	row := make([]Cell, n)
	for i := range row {
		row[i] = Cell{R: '─', FG: testFG, BG: defaultBG, Width: 1}
	}
	img := renderGrid([][]Cell{row}, fc)

	// 找到墨迹最多的那一行（横线所在的扫描线）。
	bestY, bestN := -1, 0
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		c := 0
		for x := b.Min.X; x < b.Max.X; x++ {
			if img.RGBAAt(x, y) != defaultBG {
				c++
			}
		}
		if c > bestN {
			bestN, bestY = c, y
		}
	}
	if bestY < 0 {
		t.Fatal("整张图没有墨迹：横线没画出来")
	}
	// 那条扫描线上，跨越 n 格的区间内必须每个像素都有墨。
	for x := pad; x < pad+n*charW; x++ {
		if img.RGBAAt(x, bestY) == defaultBG {
			t.Fatalf("x=%d（第 %d 格内）处横线断开：相邻制表符之间有缝，"+
				"边框会显示成虚线", x, (x-pad)/charW)
		}
	}
	t.Logf("横线在 y=%d 处连续覆盖 %d 像素（%d 格 x %d）", bestY, n*charW, n, charW)
}

// --- 背景 ---

// TestBackgroundFillsWholeCell 断言一格的背景铺满整格。
func TestBackgroundFillsWholeCell(t *testing.T) {
	fc := requireFonts(t)
	// 用空格：没有字形像素来干扰背景计数，整格应当是纯背景色。
	grid := [][]Cell{{Cell{R: ' ', FG: testFG, BG: testBG, Width: 1}}}
	img := renderGrid(grid, fc)

	r := cellRect(0, 0)
	want := charW * lineH
	if got := countColourIn(img, r, testBG); got != want {
		t.Errorf("格子里背景色像素 %d 个，期望铺满 %d 个（%dx%d）",
			got, want, charW, lineH)
	}
}

// TestWideRuneContinuationCellGetsBackground 钉住续格的背景。
//
// 这正是 sgr.go 让续格携带同样颜色的理由：漏掉续格的背景填充，
// 每个汉字后面都会露出一列页面底色，状态栏之类的整行底色会变成断续的。
// 断言直接看**第 1 列**（续格那一列）而不是看总数 —— 看总数的话，
// 把续格的宽度当成 1 来铺也能凑够数。
func TestWideRuneContinuationCellGetsBackground(t *testing.T) {
	fc := requireFonts(t)
	grid := [][]Cell{{
		Cell{R: '中', FG: testFG, BG: testBG, Width: 2},
		Cell{R: 0, FG: testFG, BG: testBG, Width: 0},
	}}
	img := renderGrid(grid, fc)

	cont := cellRect(0, 1)
	got := countColourIn(img, cont, testBG)
	want := charW * lineH
	if got == 0 {
		t.Fatal("续格那一列完全没有背景：每个汉字后面都会露出一道页面底色的缝")
	}
	// 汉字字形会跨进续格并盖掉一部分背景像素，所以不能要求恰好铺满；
	// 但缺口必须只来自字形。用一个宽松但仍能杀死「整列没铺」的下限。
	if got < want/2 {
		t.Errorf("续格背景像素只有 %d 个（整格 %d 个），疑似没有铺满整格", got, want)
	}
	// 上下边缘各取一行。这里查的不是「必须是纯背景」—— 那是主字体的性质
	// 不是渲染器的：Windows 的 CJK 回退 msyh 字形墨迹触得到格子的第一行像素
	// （实测顶行 15 个像素里有 2 个是字形墨），Menlo/Songti 碰不到而已。
	// 这两行真正要抓的失败模式是「整列没铺背景」——那露出来的是**页面底色**；
	// 被字形及其抗锯齿边盖掉的像素不是 defaultBG。
	for _, y := range []int{cont.Min.Y, cont.Max.Y - 1} {
		strip := image.Rect(cont.Min.X, y, cont.Max.X, y+1)
		if n := countColourIn(img, strip, defaultBG); n != 0 {
			t.Errorf("续格第 y=%d 行有 %d 个页面底色像素：背景没有横向铺满这一列",
				y, n)
		}
	}
}

// TestStatusBarRunKeepsBackgroundContiguous 是上一个测试的整行版：
// 一条混着汉字与 ASCII 的状态栏，背景必须**连续无缝**。
func TestStatusBarRunKeepsBackgroundContiguous(t *testing.T) {
	fc := requireFonts(t)
	var row []Cell
	for _, r := range []rune{'状', '态', 'O', 'K'} {
		w := runeWidth(r)
		row = append(row, Cell{R: r, FG: testFG, BG: testBG, Width: w})
		if w == 2 {
			row = append(row, Cell{R: 0, FG: testFG, BG: testBG, Width: 0})
		}
	}
	img := renderGrid([][]Cell{row}, fc)

	// 取整行最顶那一行像素。这里要抓的失败模式是「续格没铺背景」——那会在
	// 汉字后面露出一列**页面底色**。字形墨迹本身可以碰到这一行（Windows 的
	// CJK 回退 msyh 实测就碰得到：90 个像素里 14 个是字形墨），所以判据不是
	// 「纯背景色」，而是「不得出现页面底色」：每个像素要么是状态栏底色，
	// 要么被字形及其抗锯齿边盖住。
	y := pad
	strip := image.Rect(pad, y, pad+len(row)*charW, y+1)
	if got := countColourIn(img, strip, defaultBG); got != 0 {
		t.Errorf("状态栏顶边有 %d 个页面底色像素：背景在某处断开了（多半是宽字符的续格）",
			got)
	}
}

// TestPageBackgroundIsFilled 断言 padding 区域也有底色，
// 不是一片透明黑（RGBA 零值光栅出来是个洞）。
func TestPageBackgroundIsFilled(t *testing.T) {
	fc := requireFonts(t)
	grid := [][]Cell{{cell('x', testFG, defaultBG)}}
	img := renderGrid(grid, fc)
	corners := []image.Point{
		{0, 0},
		{img.Bounds().Dx() - 1, 0},
		{0, img.Bounds().Dy() - 1},
		{img.Bounds().Dx() - 1, img.Bounds().Dy() - 1},
	}
	for _, p := range corners {
		if got := img.RGBAAt(p.X, p.Y); got != defaultBG {
			t.Errorf("角落 (%d,%d) 是 %v，期望页面底色 %v", p.X, p.Y, got, defaultBG)
		}
	}
}

// --- 粗体 ---

// TestBoldDiffersFromRegular 断言粗体确实画得不一样。
//
// 实现用的是合成加重（同字形错开一像素画两遍），所以粗体的前景像素
// 必然**严格多于**非粗体。用同一个字符、同样的位置对拍。
func TestBoldDiffersFromRegular(t *testing.T) {
	fc := requireFonts(t)
	mk := func(bold bool) *image.RGBA {
		return renderGrid([][]Cell{{
			Cell{R: 'M', FG: testFG, BG: defaultBG, Bold: bold, Width: 1},
		}}, fc)
	}
	plain, bold := mk(false), mk(true)

	np, nb := countColour(plain, testFG), countColour(bold, testFG)
	if np == 0 {
		t.Fatal("非粗体的字形没画出来，无从对拍")
	}
	if nb <= np {
		t.Errorf("粗体前景像素 %d 个，非粗体 %d 个：粗体没有比非粗体更重", nb, np)
	}
	if bytes.Equal(plain.Pix, bold.Pix) {
		t.Error("粗体与非粗体渲染出的像素完全相同：Bold 标志被忽略了")
	}
	t.Logf("非粗体 %d 像素，粗体 %d 像素", np, nb)
}

// TestBoldKeepsBoxDrawingGlyphs 钉住「合成加重」这个选择本身。
//
// 本机实测 Menlo Bold（Menlo.ttc 下标 1）**缺失全部制表符字形**。
// 若把粗体改成换用真正的粗体字面，加粗的边框会整片变成豆腐块。
// 合成加重与非粗体走同一个字形，所以粗体的制表符必须照样画得出来。
func TestBoldKeepsBoxDrawingGlyphs(t *testing.T) {
	fc := requireFonts(t)
	for _, r := range []rune{'─', '│', '╭', '╯'} {
		img := renderGrid([][]Cell{{
			Cell{R: r, FG: testFG, BG: defaultBG, Bold: true, Width: 1},
		}}, fc)
		if n := countColour(img, testFG); n == 0 {
			t.Errorf("粗体的制表符 %q 一个前景像素都没有：它变成豆腐块或空白了", r)
		}
	}
}

// --- 反显 ---

// TestReverseVideoCellRenders 走一遍 sgr.go 的反显路径并确认它真的成像：
// 反显后 FG/BG 互换，格子应当是浅底深字。
func TestReverseVideoCellRenders(t *testing.T) {
	fc := requireFonts(t)
	grid := parseANSI("\x1b[7mR\x1b[0m")
	if len(grid) != 1 || len(grid[0]) != 1 {
		t.Fatalf("期望 1x1 网格，得到 %v", grid)
	}
	c := grid[0][0]
	if c.FG != defaultBG || c.BG != defaultFG {
		t.Fatalf("反显没有交换颜色：FG=%v BG=%v", c.FG, c.BG)
	}
	img := renderGrid(grid, fc)
	if n := countColourIn(img, cellRect(0, 0), defaultFG); n == 0 {
		t.Error("反显格子里没有浅色背景像素")
	}
}

// --- PNG 编码 ---

// TestEncodePNGRoundTrips 确认编出来的字节真的是一张能解回来的 PNG，
// 且尺寸与像素都对得上 —— 不只是「非空」。
func TestEncodePNGRoundTrips(t *testing.T) {
	fc := requireFonts(t)
	grid := [][]Cell{{Cell{R: '█', FG: testFG, BG: testBG, Width: 1}}}
	img := renderGrid(grid, fc)

	var buf bytes.Buffer
	if err := encodePNG(img, &buf); err != nil {
		t.Fatalf("encodePNG 报错：%v", err)
	}
	decoded, err := png.Decode(&buf)
	if err != nil {
		t.Fatalf("解码自己编出来的 PNG 失败：%v", err)
	}
	if decoded.Bounds() != img.Bounds() {
		t.Errorf("解码后 %v，原图 %v", decoded.Bounds(), img.Bounds())
	}
	// 解回来的图里必须还有那个前景色 —— 编码没把内容丢掉。
	found := false
	b := decoded.Bounds()
	for y := b.Min.Y; y < b.Max.Y && !found; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bb, a := decoded.At(x, y).RGBA()
			if uint8(r>>8) == testFG.R && uint8(g>>8) == testFG.G &&
				uint8(bb>>8) == testFG.B && uint8(a>>8) == testFG.A {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("解回来的 PNG 里找不到前景色：编码过程把内容丢了")
	}
}

// TestRenderGridPNGEndToEnd 走完整路径并检查产物可解码、且有内容。
func TestRenderGridPNGEndToEnd(t *testing.T) {
	requireFonts(t) // 没字体就跳过，而不是让下面报错
	grid := parseANSI("\x1b[38;5;99mhello\x1b[0m 中文")
	var buf bytes.Buffer
	if err := renderGridPNG(grid, &buf); err != nil {
		t.Fatalf("renderGridPNG 报错：%v", err)
	}
	img, err := png.Decode(&buf)
	if err != nil {
		t.Fatalf("产物不是合法 PNG：%v", err)
	}
	wantW, wantH := imageSize(grid)
	if b := img.Bounds(); b.Dx() != wantW || b.Dy() != wantH {
		t.Errorf("尺寸 %dx%d，期望 %dx%d", b.Dx(), b.Dy(), wantW, wantH)
	}
	// 那个 38;5;99 的紫色（#875fff）必须真的出现在图上。
	want := palette[99]
	rgba, ok := img.(*image.RGBA)
	if !ok {
		t.Skip("解码结果不是 *image.RGBA，跳过像素扫描")
	}
	if n := countColour(rgba, want); n == 0 {
		t.Errorf("图上找不到 256 色下标 99 的颜色 %v：前景色没有被应用", want)
	}
}

// --- 空网格 ---

// TestEmptyGridDoesNotPanic 空输入不能 panic —— 与 sgr.go 的第一条规则一致。
func TestEmptyGridDoesNotPanic(t *testing.T) {
	fc := requireFonts(t)
	img := renderGrid(nil, fc)
	if img == nil {
		t.Fatal("空网格返回了 nil 图片")
	}
	if b := img.Bounds(); b.Dx() != pad*2 || b.Dy() != pad*2 {
		t.Errorf("空网格得到 %dx%d，期望只有 padding %dx%d", b.Dx(), b.Dy(), pad*2, pad*2)
	}
}

// --- 视觉产物 ---

// TestWriteVisualArtifact 渲染一屏覆盖面较广的内容到 /tmp，供人眼复核。
//
// 结构化断言看不见「边框错位」「字形重叠」这类问题，所以这张图是那一半的检查。
// 它不是断言，失败也只是写不出文件；真正的断言在上面那些扫像素的测试里。
func TestWriteVisualArtifact(t *testing.T) {
	requireFonts(t)
	const out = "/tmp/tuidbg-render-check.png"

	screen := visualCheckScreen()
	grid := parseANSI(screen)

	f, err := os.Create(out)
	if err != nil {
		t.Skipf("跳过：无法创建 %s（%v）", out, err)
	}
	defer f.Close()
	if err := renderGridPNG(grid, f); err != nil {
		t.Fatalf("渲染视觉产物失败：%v", err)
	}
	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("stat 失败：%v", err)
	}
	w, h := imageSize(grid)
	t.Logf("视觉产物：%s  %dx%d  %d 字节  （%d 行 x %d 列）",
		out, w, h, fi.Size(), len(grid), gridCols(grid))
}

// visualCheckScreen 拼出一屏刻意覆盖各种形态的内容：
// ASCII、汉字、由制表符围成的**真实边框**、若干 256 色前景、
// 一条整行底色的状态栏、以及一个反显格子。
//
// 每行的右边框由 runeWidth 现算补白后对齐，**不是手数空格**。
// 初版是手数的，结果汉字按 1 列数、实际占 2 列，右边框参差不齐 ——
// 而那看起来**恰好像是光栅器在错位**，正是这张图本该用来排除的那种嫌疑。
// 让排版逻辑和被测的宽度逻辑用同一个函数，图上剩下的任何歪斜就都是真的了。
func visualCheckScreen() string {
	const (
		reset  = "\x1b[0m"
		rev    = "\x1b[7m"
		bold   = "\x1b[1m"
		barBG  = "\x1b[48;5;236m"
		purple = "\x1b[38;5;99m"
		grey   = "\x1b[38;5;245m"
		white  = "\x1b[38;5;255m"
		green  = "\x1b[38;5;41m"
		orange = "\x1b[38;5;208m"
		cyan   = "\x1b[38;5;51m"
	)
	// inner 是边框内部的可见列数。
	const inner = 44

	// visW 用 runeWidth 数一段**已去掉转义**的文本占多少列。
	visW := func(s string) int {
		n := 0
		for _, r := range stripSGR(s) {
			n += runeWidth(r)
		}
		return n
	}
	// framed 把一段内容包进左右边框，右侧按真实显示宽度补白。
	framed := func(content string) string {
		padN := inner - visW(content)
		if padN < 0 {
			padN = 0
		}
		return purple + "│" + reset + content + strings.Repeat(" ", padN) + purple + "│" + reset
	}
	rule := func(l, m, r string) string {
		return purple + l + strings.Repeat("─", inner) + r + reset + m
	}

	lines := []string{
		rule("╭", "", "╮"),
		framed(" " + bold + white + "tuidbg 渲染自检" + reset),
		rule("├", "", "┤"),
		framed(" ASCII    the quick brown fox 0123456789"),
		framed(" " + green + "中文" + reset + "     你好，世界！宽字符对齐检查"),
		framed(" " + orange + "colours" + reset + "  " + purple + "紫" + grey + "灰" + white + "白" +
			green + "绿" + orange + "橙" + cyan + "青" + reset),
		framed(" " + cyan + "box" + reset + "      ┌─┬─┐ ┏━┳━┓ ╔═╦═╗ ├─┤ ╰─╯"),
		framed(" " + grey + "reverse" + reset + "  " + rev + " 反显 REVERSE " + reset),
		rule("╰", "", "╯"),
		"",
		barBG + white + " NORMAL " + grey + " main.go " + white + " 12:34 " + grey +
			" 中文状态栏 " + white + " UTF-8 " + reset,
	}
	return strings.Join(lines, "\n") + "\n"
}
