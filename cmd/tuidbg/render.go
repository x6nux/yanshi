// render.go —— 把 sgr.go 解出的彩色字符网格光栅成 PNG，全程纯 Go。
//
// 不用 Chrome、不用 headless 浏览器、不调任何外部二进制 —— 这是本文件存在的
// 全部理由。前身管线把网格转成 HTML 再让浏览器截图：一条依赖几百 MB 运行时、
// 在没装浏览器的机器上直接不可用、且失败信息出现在别人的进程里。
//
// 这里的取舍与 sgr.go 一脉相承：宁可样式略欠，也绝不产出一张**看起来正常、
// 实际错位**的图。这个工具存在的意义就是那张图可以被信任，所以本文件里每一处
// 「差一点也行」的选择都倒向「宁可显式失败」。最典型的一条：字体一个都加载不出来时
// 返回 error，而**不是**返回一张空白图 —— 见 loadFontChain 的注释。
//
// 本注释与 package 子句之间**故意**留了空行：它描述的是这个文件，不是这个包。

package main

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"os"
	"runtime"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

// 版面常量。全部在本机（macOS 15.7、Go 1.26.4、golang.org/x/image v0.44.0）
// 对 Menlo Regular 以 fontSize=14 / fontDPI=144 / HintingFull **实测**得来，
// 不是估的：face.Metrics() 报 Height=33.0、Ascent=26.0，而 'A'、'i'、'─'、'│'、
// '╭'、'╯'、'┃' 的 GlyphAdvance 一律是 17.0（等宽字体的定义）。
//
// 这几个数与 fontSize/fontDPI 是**绑死的**：改动那两个之中的任何一个，
// 下面三个都必须重新量，否则字形会溢出自己的格子或在格子里缩成一团。
// charW/lineH/ascent 与字体运行时报出的度量之间没有任何自动对账 —— 这是有意的，
// 见 renderGrid 关于「按格定位而非按步进累加」的注释。
// charW 是一个网格列的像素宽。darwin（及其余平台）量的是 Menlo；Windows
// 主字体是 Consolas（同档位实测：步进一律 15、Ascent 21，Height 恰好同为 33）。
// 平台分支是编译期的，同一平台内的值永远不变 —— 「图片尺寸只取决于网格，
// 不取决于这一屏的字符」的保证仍然成立。
const (
	// fontSize、fontDPI 是量出上面那组数字时用的档位。
	fontSize = 14.0
	fontDPI  = 144.0
)

var (
	charW = 17
	// lineH 是一个网格行的像素高（Menlo 的 Height；Consolas 同档位也是 33）。
	lineH = 33
	// ascent 是从行顶到基线的像素距离（Menlo 的 Ascent）。
	ascent = 26
	// pad 是页面四周的留白。纯美观值，不是量出来的；它同时让最左一列的
	// 字形不至于贴着图片边缘被裁掉半个像素。
	pad = 8
)

func init() {
	if runtime.GOOS == "windows" {
		charW, ascent = 15, 21
	}
}

// fontCandidate 是一个待尝试的字体文件，以及它在文件内的字体下标。
//
// 带下标是因为 macOS 的系统字体几乎全是 .ttc（collection），一个文件里装着
// 好几个字重，而它们的字形覆盖**并不相同** —— 见 loadFontChain 的实测记录。
type fontCandidate struct {
	path  string
	index int
}

// loadedFont 是一个已经解析并可用于绘制的字体。
//
// sfnt.Font 与 font.Face 两个都留着，因为它们回答的是**两个不同的问题**：
// Face 负责画和量步进，而「这个字体到底有没有这个字形」只有 Font.GlyphIndex
// 答得准 —— 见 fontChain.resolve。
type loadedFont struct {
	font *sfnt.Font
	face font.Face
	desc string // "路径#下标"，只用于错误信息与测试诊断
}

// fontChain 是按优先级排好的一串字体：第一个能提供某字形的胜出。
//
// 顺序是承重的，理由见 resolve。
type fontChain struct {
	fonts []*loadedFont

	// buf 供 GlyphIndex 复用。sfnt.Buffer 明确**不是**并发安全的，
	// 因此一个 fontChain 不能被多个 goroutine 同时使用。本工具是单次调用的
	// 命令行程序，这个约束天然成立。
	buf sfnt.Buffer
}

// primary 返回链首字体，即负责 ASCII 与制表符的那个。
func (fc *fontChain) primary() *loadedFont { return fc.fonts[0] }

// resolve 挑出该由哪个字体来画 r。
//
// 判据是 GlyphIndex(r) != 0。「没有这个字形」的准确信号是 GlyphIndex 返回 0
// （.notdef 的固定下标）。
//
// **关于 Face.GlyphAdvance 的 ok 返回值：在当前锁定的 x/image v0.44.0 上，
// 用它做判据是等价的，但仍然不用。** 变异测试把「改用 GlyphAdvance 的 ok」
// 报成了存活变异体，查下去发现它是**等价变异体**而非测试缺口：
// opentype.Face.GlyphAdvance 的实现就是
//
//	x, _ := f.f.GlyphIndex(&f.buf, r)
//	advance, err := f.f.GlyphAdvance(&f.buf, x, f.scale, f.hinting)
//	return advance, (err == nil) && (x != 0)
//
// 它内部已经查了 GlyphIndex 并把 x != 0 揉进了 ok。实测在 Menlo 与 Songti 上
// 横扫约 2.5 万个码位（ASCII、Latin-1、制表符、CJK、假名、全角、emoji），
// 两个判据的分歧数是 **0**。
//
// 那为什么不换：ok 的这层语义是**那个版本的实现细节**，不是 font.Face 接口的
// 约定 —— 接口文档只说 ok 表示「能否取到步进」。换一个 Face 实现（或这个实现
// 改一版），ok 就可能对 .notdef 返回 true，而那时回退会**静默停止工作**、
// 整屏汉字变豆腐块，且没有任何测试会红（两个判据一致时测不出差别）。
// GlyphIndex 直接问的就是本函数真正关心的那件事，不依赖任何人的实现细节。
// 这段注释是那次变异判决的记录。
//
// 顺序为什么重要：制表符（─│╭╯┃╰┤├）**必须**落在 Menlo 上而不是中文字体上。
// 实测 Songti SC Black 的 '╭' GlyphIndex 就是 0，而它的 '─' 虽然有、步进却是
// 28.0（全宽），画进 17px 的格子里会横向压扁并与邻格重叠。把中文字体排在前面
// 或者对制表符提前回退，得到的都是一个**看起来只是有点丑、实际每条边框都错位**的边框。
//
// 全链都没有时返回链首。此时画出来是一个可见的 .notdef 豆腐块 —— 这是**刻意**的：
// 一个缺字形的位置应该看得见，而不是留一片空白让读者以为屏幕上本来就没东西。
//
// **这里刻意不加 rune → 字体的缓存。** 初版有一个，理由是「一屏里同一个字符
// 会重复成百上千次」—— 听起来显然，实测是错的：在 30 行 x 84 列、汉字与制表符
// 混排的整屏上，带缓存 5.098ms、去掉缓存 5.106ms，差 0.15%（噪声级别）。
// 真正的开销全在光栅化，cmap 查表可以忽略。而那个缓存已经贡献过一个真 bug 的
// 形状：变异测试把「存错下标」的版本放进来时，首个汉字正常、其余全变豆腐块 ——
// 一种极难从截图归因的损坏。零收益换一个 bug 面，删掉。
func (fc *fontChain) resolve(r rune) *loadedFont {
	for _, lf := range fc.fonts {
		gi, err := lf.font.GlyphIndex(&fc.buf, r)
		if err == nil && gi != 0 {
			return lf
		}
	}
	return fc.fonts[0]
}

// close 释放链上所有 face。
func (fc *fontChain) close() {
	for _, lf := range fc.fonts {
		_ = lf.face.Close()
	}
}

// fontCandidates 给出本平台按优先级排列的候选字体。
//
// 等宽字体排在前、CJK 字体排在后，这个顺序就是 resolve 依赖的那个优先级。
func fontCandidates() []fontCandidate {
	switch runtime.GOOS {
	case "darwin":
		return []fontCandidate{
			// Menlo Regular。实测 ASCII 与全部制表符齐备，步进一律 17.0。
			// 只取下标 0：同一个 .ttc 里的**下标 1（Menlo Bold）实测缺失全部
			// 制表符字形**（─│╭╯┃╰┤├ 的 GlyphIndex 全是 0），这也是本文件
			// 用合成粗体而不是真粗体字面的原因，见 drawGlyph。
			{"/System/Library/Fonts/Menlo.ttc", 0},
			// Songti SC。实测 '中' 可用（GlyphIndex 非 0、步进 28.0）。
			// 它的 ASCII **不等宽**（'A'=19.0、'i'=8.0），所以它只能当回退，
			// 绝不能排到前面去 —— 这也是绝不按步进累加定位的原因之一。
			{"/System/Library/Fonts/Supplemental/Songti.ttc", 0},
			// STHeiti 实测无法被 x/image 解析（"sfnt: unsupported number of
			// cmap segments"），PingFang.ttc 受 SIP 保护读不出来。两者都留在
			// 名单里是因为跳过解析失败的候选本来就是正常流程、不是错误；
			// 换一台 x/image 版本更新的机器它们可能就能用了。
			{"/System/Library/Fonts/STHeiti Light.ttc", 0},
		}
	case "windows":
		// Windows 此前落进 default 分支拿到的全是 Linux 路径，每个候选都
		// read 失败，tuidbg 在 Windows 上从来起不来（CI 的 race-full
		// windows 与 test (windows) 都被它绊倒过）。Consolas 自 Vista 起是
		// 系统等宽字体；微软雅黑（msyh）提供 CJK 回退，Win10 起随系统装
		// .ttc、更老的安装是 .ttf，两个都列上 —— 解析失败的候选被跳过
		// 本来就是正常流程。
		return []fontCandidate{
			{`C:\Windows\Fonts\consola.ttf`, 0},
			{`C:\Windows\Fonts\msyh.ttc`, 0},
			{`C:\Windows\Fonts\msyh.ttf`, 0},
		}
	default:
		// Linux 及其余平台。发行版之间路径差异很大，全部尝试、能用几个算几个。
		return []fontCandidate{
			{"/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf", 0},
			{"/usr/share/fonts/truetype/noto/NotoSansMono-Regular.ttf", 0},
			{"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc", 0},
		}
	}
}

// errNoFonts 表示一个候选字体都没能加载。
//
// 这是本文件里最重要的一条错误路径，所以它是一个具名哨兵而不是临时 fmt.Errorf：
// 调用方（以及测试）需要能精确地断言「就是这一种失败」。
var errNoFonts = errors.New("tuidbg: no usable font found")

// loadFontChainFrom 依次尝试 cands，返回可用的字体链。
//
// **解析失败的候选被跳过，不算错误。** 这不是宽容，是实测：本机的
// Songti.ttc 有 8 个字体，其中下标 2/5/7 会让 x/image 报
// "sfnt: unsupported number of cmap segments"；STHeiti 两个下标全报同一个错。
// 把这些当致命错误，等于在一台字体齐全的机器上因为**没被选中**的那几个字重
// 而拒绝出图。
//
// 但一个都加载不出来时**必须**返回 error，而不是返回一张空白图。这条是承重的：
// 本工具有一段成文的历史 —— 四个各自独立的 bug 都是把失败报成成功 ——
// 而「PNG 写出来了，只是上面一个字都没有」正是那个模式的下一次复发。
// 一张空白 PNG 会一路通过「文件存在」「字节数非零」「能解码成图片」全部检查。
func loadFontChainFrom(cands []fontCandidate) (*fontChain, error) {
	fc := &fontChain{}
	for _, c := range cands {
		lf, err := loadFont(c)
		if err != nil {
			continue
		}
		fc.fonts = append(fc.fonts, lf)
	}
	if len(fc.fonts) == 0 {
		return nil, errNoFonts
	}
	return fc, nil
}

// fontCandidatesForTest 覆盖候选名单，仅供测试使用；为 nil 时走 fontCandidates()。
//
// 存在的理由是「一个字体都加载不出来」这条路径必须被测到，而它在一台字体齐全的
// 机器上无法自然发生。这条路径是本文件承重的失败行为（绝不返回空白图），
// 一条测不到的失败路径等于没有。
var fontCandidatesForTest []fontCandidate

// loadFontChain 用本平台的候选名单加载字体链。
func loadFontChain() (*fontChain, error) {
	if fontCandidatesForTest != nil {
		return loadFontChainFrom(fontCandidatesForTest)
	}
	return loadFontChainFrom(fontCandidates())
}

// loadFont 读取并解析单个候选。
//
// 一律走 sfnt.ParseCollection，.ttf 与 .ttc 都能吃 —— 实测把一个普通 .ttf
// 交给 ParseCollection 会得到一个 NumFonts()==1 的集合。因此这里不需要按扩展名
// 分支，也就不存在「扩展名与真实格式不符」这一整类 bug。
func loadFont(c fontCandidate) (*loadedFont, error) {
	desc := fmt.Sprintf("%s#%d", c.path, c.index)

	b, err := os.ReadFile(c.path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", desc, err)
	}
	coll, err := sfnt.ParseCollection(b)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", desc, err)
	}
	if c.index < 0 || c.index >= coll.NumFonts() {
		return nil, fmt.Errorf("parse %s: index out of range (have %d)", desc, coll.NumFonts())
	}
	f, err := coll.Font(c.index)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", desc, err)
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{
		Size:    fontSize,
		DPI:     fontDPI,
		Hinting: font.HintingFull,
	})
	if err != nil {
		return nil, fmt.Errorf("face %s: %w", desc, err)
	}
	return &loadedFont{font: f, face: face, desc: desc}, nil
}

// gridCols 返回网格的列数，取各行最长者。
//
// 逐行取最大而不是信任第 0 行：tmux 的 capture-pane 会把行尾空白截掉，
// 于是各行长度参差不齐是常态。拿第一行当宽度，会让比它长的行被裁掉右半边。
func gridCols(grid [][]Cell) int {
	n := 0
	for _, row := range grid {
		if len(row) > n {
			n = len(row)
		}
	}
	return n
}

// imageSize 由网格尺寸与版面常量算出图片像素尺寸。
//
// **只依赖列数、行数和常量，绝不问字体要度量。** 这条是刻意的：字体度量会随
// 回退链上哪个字体被选中而变（Songti 的 Height 是 40.0，Menlo 是 33.0），
// 让图片尺寸取决于「这一屏碰巧有没有汉字」，同一个 TUI 的两张截图就会尺寸不同。
func imageSize(grid [][]Cell) (w, h int) {
	return pad*2 + gridCols(grid)*charW, pad*2 + len(grid)*lineH
}

// renderGrid 把网格画成一张 RGBA 图。fc 必须非 nil（由调用方保证已成功加载）。
//
// **每一格都按自己的列号定位：x = pad + col*charW，绝不累加步进。**
// 这条是本函数最容易写错、也最容易「看起来能用」的地方。按步进累加在纯 ASCII、
// 纯 Menlo 的一屏上完全正确 —— 因为那时每一步都恰好是 17 —— 然后在第一个汉字
// 处开始崩：回退到 Songti 的汉字步进是 28.0，而两列是 34；Songti 的 ASCII 更是
// 根本不等宽（'A'=19.0、'i'=8.0）。于是错位从该行第一个非 Menlo 字符开始，
// 一路把这一行余下的每一格都推偏，而**前面的行全是对的**，让人以为只是某几行有问题。
// 网格自己已经知道列号了，用它。
//
// 双宽字符从它自己的列开始、视觉上跨两列，但下一格仍然落在它自己的列边界上 ——
// 这正是 sgr.go 把续格也实体化的价值：这里不需要记住任何跨列状态。
//
// **背景与字形分两趟画，这一点是承重的、且是实测逼出来的。**
// 单趟（逐格「铺背景 → 画字形」）时，双宽字符在第 N 格画出的字形会横跨到第 N+1 格，
// 然后**紧接着**第 N+1 格（续格）的背景填充就把它右半边抹掉了。实测：单趟渲染
// '中' 的墨迹 x 范围是 [14,24]，而它本该是 [14,32] —— 恰好在 pad+charW=25 处被
// 齐齐切断，屏幕上每一个汉字都只剩左半边。
//
// 这个 bug 尤其阴险的地方在于，它会让一个**写得对的**续格背景测试变绿：
// 续格的背景确实铺满了，只不过是靠抹掉半个字形换来的。所以除了分两趟以外，
// 还必须有一条直接量墨迹跨度的断言（TestWideGlyphSpansBothColumns）盯着它。
func renderGrid(grid [][]Cell, fc *fontChain) *image.RGBA {
	w, h := imageSize(grid)
	img := image.NewRGBA(image.Rect(0, 0, w, h))

	// 页面底色。用 sgr.go 的默认背景而不是黑，这样 padding 区域与内容区的
	// 未着色格子是同一个颜色，边缘看不出一圈框。
	draw.Draw(img, img.Bounds(), &image.Uniform{C: defaultBG}, image.Point{}, draw.Src)

	// 第一趟：所有格子的背景。**每一格都铺**，包括双宽字符的续格。
	// 续格携带着与前一格相同的颜色（sgr.go 保证），所以一个汉字底下
	// 是完整的两列色块而不是一列色块加一列空隙。漏掉这一步，
	// 状态栏之类的整行底色会在每个汉字后面出现一道缝。
	bg := &image.Uniform{}
	for rowIdx, row := range grid {
		top := pad + rowIdx*lineH
		for colIdx, cell := range row {
			x := pad + colIdx*charW
			bg.C = cell.BG
			draw.Draw(img, image.Rect(x, top, x+charW, top+lineH),
				bg, image.Point{}, draw.Src)
		}
	}

	// 第二趟：所有字形。此时背景已经全部就位，任何跨列的字形都不会再被
	// 后续的背景填充抹掉。
	//
	// 一个 Uniform 反复改 C 复用，省掉每格一次分配。Drawer 在 DrawString
	// 期间同步读取它，没有跨调用的残留。
	src := &image.Uniform{}
	drawer := &font.Drawer{Dst: img, Src: src}
	for rowIdx, row := range grid {
		baseline := pad + rowIdx*lineH + ascent
		for colIdx, cell := range row {
			if cell.R == 0 {
				// 续格没有自己的字形（前一格的字形已经跨过来了）。
				continue
			}
			src.C = cell.FG
			drawGlyph(drawer, fc, cell, pad+colIdx*charW, baseline)
		}
	}
	return img
}

// drawGlyph 把单个 cell 的字形画到基线上。
//
// 字形在自己的格子内**横向居中**。这不违反「按格定位」：格子的左边界依旧是
// pad+col*charW，只是字形在这个格子里居中而已。对主字体这一步恒等于 0
// （步进 17 == charW），它救的是回退字体：Songti 的汉字步进 28.0 塞进两列 34px，
// 不居中就会整体左靠、与右邻格之间留一道不对称的缝。
//
// 粗体用**合成加重**（同一字形画两遍、横向错开一个像素），而不是换一个真正的
// 粗体字面。这是实测逼出来的：本机 Menlo.ttc 下标 1（Menlo Bold）**缺失全部
// 制表符字形**（─│╭╯┃╰┤├ 的 GlyphIndex 全是 0），而下标 0（Regular）全都有。
// 用真粗体字面意味着「加粗的边框」会变成一整屏豆腐块，且只在粗体那些格子上发生 ——
// 又一个只在部分屏幕上显形的静默损坏。合成加重让粗体与非粗体走**完全相同的
// 字形覆盖**，于是不存在「只有粗体才缺字」这种可能。
func drawGlyph(drawer *font.Drawer, fc *fontChain, cell Cell, x, baseline int) {
	lf := fc.resolve(cell.R)
	drawer.Face = lf.face

	span := cell.Width * charW
	offset := 0
	if adv, ok := lf.face.GlyphAdvance(cell.R); ok {
		if d := span - adv.Round(); d > 0 {
			offset = d / 2
		}
	}

	s := string(cell.R)
	drawer.Dot = fixed.P(x+offset, baseline)
	drawer.DrawString(s)
	if cell.Bold {
		drawer.Dot = fixed.P(x+offset+1, baseline)
		drawer.DrawString(s)
	}
}

// encodePNG 把图片写成 PNG。
func encodePNG(img image.Image, w io.Writer) error {
	return png.Encode(w, img)
}

// renderGridPNG 是完整的一趟：加载字体 → 光栅 → 编码 PNG。
//
// **字体加载失败时，w 上一个字节都不会被写。** 顺序是承重的：先 load、
// 后 render、最后 encode。反过来（先建图、发现没字体再返回错误）会在
// 调用方已经创建了输出文件的情况下留下一个 0 字节或半张图的文件，
// 而那正是「失败被报成成功」的又一种形态。
func renderGridPNG(grid [][]Cell, w io.Writer) error {
	fc, err := loadFontChain()
	if err != nil {
		return err
	}
	defer fc.close()
	return encodePNG(renderGrid(grid, fc), w)
}

// 断言 Cell.FG 的类型与 image/color 的 Color 接口相容，好让 image.Uniform
// 直接吃它而不必每格转换一次。放在这里是为了让 sgr.go 若把 FG 改成别的类型时
// 在编译期就断掉，而不是在运行期悄悄走进一条转换路径。
var _ color.Color = Cell{}.FG
