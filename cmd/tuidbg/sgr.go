// sgr.go —— 把 `tmux capture-pane -p -J -e` 抓到的一屏，变成可供光栅化的彩色字符网格。
//
// 为什么只解码 SGR（ESC[...m），其余转义一律丢弃：实测目标 TUI 的真实 100x30 一屏，
// 里面**每一个**转义序列都是 SGR —— 零个光标移动、零个 OSC。tmux 已经跑完了终端状态机、
// 交回来一张定型的网格，在这里再实现一遍光标寻址等于给模拟器做模拟器。
// 那一屏量到的频次表（ESC[39m x43、ESC[38;5;99m x30、ESC[38;5;245m x5、ESC[38;5;255m x3、
// ESC[40m x2、ESC[48;5;236m x2、ESC[7m x1、ESC[0m x1、ESC[49m x1、ESC[37m x1）
// 决定了哪几种形态值得写真实处理。
//
// 有一条规则的优先级高于还原度：绝不 panic，绝不让转义字节流进 cell。
// 漏出去的 0x1b 会光栅成一个可见的豆腐块，并把它后面每一列都推错位 ——
// 一张静默错位的截图比一张样式略欠的截图更糟，因为这个工具存在的全部理由
// 就是那张图可以被信任。
//
// 本注释与 package 子句之间**故意**留了空行：它描述的是这个文件，不是这个包。

package main

import (
	"image/color"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/width"
)

// 默认前景/背景色。终端里没有"页面背景"这个概念 —— SGR 39/49 的语义是
// "还原成终端自己选的那个"，所以默认值只能由渲染方定。选深色不是偏好问题：
// 几乎每个 TUI（包括本仓这个）挑配色时都假定终端是深色的，渲染到白底上会让
// 作者自己的对比度决策全部失真。
var (
	defaultFG = color.RGBA{0xd4, 0xd4, 0xd4, 0xff}
	defaultBG = color.RGBA{0x1e, 0x1e, 0x1e, 0xff}
)

// ansi16 是 xterm 的 16 个经典色。256 色表里**只有**这 16 个算不出来：
// 16-231 和 232-255 都有精确公式（见 palette256），而 0-15 纯属约定俗成。
// 手抄 256 行既臃肿，而且实践上必然在某一行抄错。
//
// 取值用 xterm 的那套，与移植来源的 Python 参考实现一致，这样渲染出的截图
// 能和旧管线逐字节对拍。
var ansi16 = [16]color.RGBA{
	{0x00, 0x00, 0x00, 0xff}, {0xcd, 0x00, 0x00, 0xff},
	{0x00, 0xcd, 0x00, 0xff}, {0xcd, 0xcd, 0x00, 0xff},
	{0x00, 0x00, 0xee, 0xff}, {0xcd, 0x00, 0xcd, 0xff},
	{0x00, 0xcd, 0xcd, 0xff}, {0xe5, 0xe5, 0xe5, 0xff},
	{0x7f, 0x7f, 0x7f, 0xff}, {0xff, 0x00, 0x00, 0xff},
	{0x00, 0xff, 0x00, 0xff}, {0xff, 0xff, 0x00, 0xff},
	{0x5c, 0x5c, 0xff, 0xff}, {0xff, 0x00, 0xff, 0xff},
	{0x00, 0xff, 0xff, 0xff}, {0xff, 0xff, 0xff, 0xff},
}

// palette256 生成 xterm 256 色表：16 个经典色 + 6x6x6 色立方 + 24 级灰阶。
//
// 色立方的循环顺序是承重的，而且极易写反：red 是**外**层、blue 是内层，
// 即下标 16+36r+6g+b。把 r 和 b 对调之后仍然会得到一张看起来很像样的 216 色渐变 ——
// 调色板依旧是一组合法颜色，只是每次查表都落在错的那个上 —— 这正是那种能活着
// 通过冒烟测试的 bug。用下标 99（实测那一屏里出现最多的颜色）钉死顺序：
// 99-16 = 83 = 2*36 + 1*6 + 5，于是 (r,g,b) = levels[2,1,5] = (135,95,255) = #875fff。
// 一旦把 r 和 b 对调就会变成 #ff5f87 —— 同样饱和度的另一个颜色。
func palette256() [256]color.RGBA {
	var pal [256]color.RGBA
	copy(pal[:], ansi16[:])

	// 刻意不等距：0 到 95 的跨度远大于其上各档之间的 40。xterm 这么选是为了
	// 不让色立方的暗端浪费在屏幕上根本分辨不出的一堆近黑色上。
	levels := [6]uint8{0, 95, 135, 175, 215, 255}
	i := 16
	for _, r := range levels {
		for _, g := range levels {
			for _, b := range levels {
				pal[i] = color.RGBA{r, g, b, 0xff}
				i++
			}
		}
	}

	// 灰阶：8, 18, … 238。刻意不含纯黑与纯白 —— 下标 0 和 15 已经有了，
	// xterm 不愿意让重复值吃掉 24 档里的两档。
	for g := 0; g < 24; g++ {
		v := uint8(8 + 10*g)
		pal[i] = color.RGBA{v, v, v, 0xff}
		i++
	}
	return pal
}

// palette 是 256 色表，init 期算一次。
var palette = palette256()

// Cell 是屏幕上的一个字符位，颜色**已经**解析成具体 RGBA —— 没有任何"默认"
// 哨兵值能活着进入网格。在这里就解析而不是丢给光栅器，是为了让反显（SGR 7）
// 只需交换两个字段；而那一步只有在两边都是真实值时才正确 —— 交换两个"未设置"
// 标记等于什么都没做，反显正是这样被静默丢掉的。
type Cell struct {
	// R 是要绘制的字符。双宽字符的后半格这里是 0 —— 见 Width。
	R rune

	// FG、BG 已完全解析，绝不会是零值。零值 RGBA 是透明黑，光栅出来是个洞。
	FG, BG color.RGBA

	// Bold 对应 SGR 1。保留成标志位而不是折进 FG，因为光栅器可能选择换一个
	// 粗体字形，而不是提亮颜色。
	Bold bool

	// Width 是这个字符占据的网格列数：东亚宽字符为 2，其余为 1。
	// Width==2 的 cell 后面紧跟一个续格（Width==0、R==0），颜色与前一格相同。
	//
	// 两半都实体化，而不是只存一格、让渲染器自己记住：有了续格，len(row) 才是
	// 真正的列数、row[i] 才真的是第 i 列上的字符。没有它，每个 CJK 字符之后的
	// 所有下标含义都与看起来的不同，而逐格填充背景时每个宽字形下面还会留一列空隙。
	Width int
}

// sgrState 是"笔"：即将写入的下一个字符所处的样式。fg/bg 用指针，是为了把
// "未设置"与"显式设成了默认色"区分开 —— 反显必须在**交换的那一刻**才代入默认值，
// 不能更早。
type sgrState struct {
	fg, bg  *color.RGBA
	bold    bool
	reverse bool
}

// resolve 把笔的状态落成 Cell 携带的具体颜色。
//
// 代入默认值这一步必须发生在**这里**、在 reverse 分支内部，而不是在颜色被设置的时候。
// 单独一个 `ESC[7m` 是常见形态（实测那一屏里就有），而那一刻 fg 和 bg 都还是 nil：
// 交换两个 nil 等于没交换，高亮就此消失。先代入、再交换，才让裸的 ESC[7m
// 如预期那样画成深字浅底。
func (s *sgrState) resolve() (fg, bg color.RGBA) {
	fg, bg = defaultFG, defaultBG
	if s.fg != nil {
		fg = *s.fg
	}
	if s.bg != nil {
		bg = *s.bg
	}
	if s.reverse {
		fg, bg = bg, fg
	}
	return fg, bg
}

// applySGR 把一条转义的参数序列作用到笔上。
//
// 用下标循环而不是 range：38 和 48 会**吃掉**它们后面的参数（38;5;N 或 38;2;R;G;B）。
// range 循环会接着把那个 N 当成独立的 SGR 码读 —— 对实测的 ESC[38;5;99m 来说，
// 那就是码 5（闪烁）后跟码 99（亮白背景）。屏幕照样渲染得出来，只是颜色错得理直气壮。
//
// 认不出来的参数一律静默跳过。终端会发出本工具没有意见的码（斜体、下划线的各种变体、
// 闪烁），将来还会更多；把它们当错误处理的解析器，会在本可以近乎完美渲染的屏幕上失败。
func applySGR(state *sgrState, params []int) {
	for i := 0; i < len(params); i++ {
		p := params[i]

		if p == 38 || p == 48 {
			consumed, c, ok := parseExtendedColour(params[i:])
			if ok {
				if p == 38 {
					state.fg = &c
				} else {
					state.bg = &c
				}
			}
			// 即便颜色被拒也要按序列自称的长度前进：一条畸形的
			// 38;5;<越界值> 不能把残渣留下来被重新当成独立的码读。
			i += consumed - 1
			continue
		}

		switch {
		case p == 0:
			*state = sgrState{}
		case p == 1:
			state.bold = true
		case p == 7:
			state.reverse = true
		case p == 21 || p == 22:
			state.bold = false
		case p == 27:
			state.reverse = false
		case p == 39:
			state.fg = nil
		case p == 49:
			state.bg = nil
		case p >= 30 && p <= 37:
			c := ansi16[p-30]
			state.fg = &c
		case p >= 90 && p <= 97:
			c := ansi16[p-90+8]
			state.fg = &c
		case p >= 40 && p <= 47:
			c := ansi16[p-40]
			state.bg = &c
		case p >= 100 && p <= 107:
			c := ansi16[p-100+8]
			state.bg = &c
		}
	}
}

// parseExtendedColour 解码从 params[0] 开始的 38/48 各种参数形态，
// 返回这条序列占掉几个参数（含开头的 38/48 本身）以及是否真的产出了颜色。
//
// 被截断的序列同样返回一个消耗数，好让调用方跳过这段残片，
// 而不是把它的尾巴重新解释成一串独立的码。
func parseExtendedColour(params []int) (consumed int, c color.RGBA, ok bool) {
	if len(params) < 2 {
		return 1, c, false
	}
	switch params[1] {
	case 5: // 38;5;N —— 调色板下标
		if len(params) < 3 {
			return len(params), c, false
		}
		// n >= 0 这一半当前**不可达**：atoiSGR 只累加数字并钳制上界，值域是
		// [0, 1<<20]，穷举 scanEscape 允许字符集下所有 ≤3 字节的参数体后，
		// 负值出现 0 次。留着是因为它守的是 palette 的下标，而"参数怎么来的"
		// 将来可能变（换一种解析、或有人直接调用 applySGR）；删掉它换来的
		// 是一个越界 panic，而这个包的第一条规则就是绝不 panic。
		// 变异测试会把它报成存活的等价变异体，这段注释就是那次判决的记录。
		if n := params[2]; n >= 0 && n < len(palette) {
			return 3, palette[n], true
		}
		return 3, c, false
	case 2: // 38;2;R;G;B —— 真彩
		if len(params) < 5 {
			return len(params), c, false
		}
		r, g, b := params[2], params[3], params[4]
		if inByte(r) && inByte(g) && inByte(b) {
			return 5, color.RGBA{uint8(r), uint8(g), uint8(b), 0xff}, true
		}
		return 5, c, false
	}
	// 不认识的色彩空间选择子（标准里还有 0/1/3/4，这里从不出现）。
	// 只跳过 38/48 自身：给一个看不懂的形态猜长度，会吃掉后面可能合法的码。
	return 1, c, false
}

// inByte 判断 v 是否是合法的 0-255 颜色通道值。
func inByte(v int) bool { return v >= 0 && v <= 255 }

// runeWidth 返回 r 占据几个网格列。
//
// 在本机 tmux 3.7c 上**实测**得来（printf 单个字符，再读 #{cursor_x}），不是推断的：
//
//	中 U+4E2D -> 2    ─ U+2500 -> 1    ★ U+2605 -> 1
//	😀 U+1F600 -> 2   │ U+2502 -> 1    → U+2192 -> 1
//	한 U+D55C -> 2    █ U+2588 -> 1    ± U+00B1 -> 1
//	～ U+FF5E -> 2    ✓ U+2713 -> 1    ｱ U+FF71 -> 1
//	U+200B -> 0       组合符 U+0301 -> 0
//
// 即：只有 Wide 与 Fullwidth 两类前进两列，东亚**歧义**宽（Ambiguous）前进一列。
//
// 这正是这里用 golang.org/x/text/width 而不用 github.com/mattn/go-runewidth 的理由，
// 尽管后者已经是本仓的间接依赖。go-runewidth 会读 locale：在本机的
// LANG=zh_CN.UTF-8 下，它对上面每一个歧义宽字符都报 2，与产出这份输入的 tmux 相左。
// 这不是理论问题而是具体问题 —— 被调试的那个 TUI 用 lipgloss.RoundedBorder() 画边框，
// 那一整套都是歧义宽的制表符，于是一个看 locale 的宽度函数会把每个边框字符都数成两列，
// 让每一条带框的行按它自身的宽度整体错位。x/text/width 是纯 Unicode 属性查表、
// 不吃 locale，而这正是此处 tmux 表现出来的行为。
//
// 组合符与格式字符给 0 宽。它们是附着在前一个字符上到达的、不占自己的格子；
// 数成 1 会把整行余下部分推错位。
func runeWidth(r rune) int {
	if r == 0 {
		return 0
	}
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r) {
		return 0
	}
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	return 1
}

// parseANSI 把抓到的一屏变成一行行 cell。
//
// 列数是**可见**列数。Python 前身在这里是错的：它拿原始字符串量宽度，
// 转义字节被算成了列，于是一屏真实的 100 列算出来是 209 列，
// PNG 宽出一倍、所有内容挤在左半边。这里只为真正会打印的字符 append cell，
// 这才是 len(row) 有意义的原因。
//
// 按 \n 切行；\r 由 C0 控制字符那一条统一丢弃（它是 0x0D，落在同一个范围里），
// 所以 CRLF 形态的抓屏不会在每行末尾留一个游离的控制字符。
func parseANSI(s string) [][]Cell {
	state := &sgrState{}
	var grid [][]Cell
	row := []Cell{}

	for i := 0; i < len(s); {
		// 转义序列在任何解码之前就被整条吞掉，所以它的任何一个字节都到不了 cell。
		if s[i] == 0x1b {
			n, params, isSGR := scanEscape(s[i:])
			if isSGR {
				applySGR(state, params)
			}
			i += n
			continue
		}

		r, size := utf8.DecodeRuneInString(s[i:])
		i += size

		switch {
		case r == '\n':
			grid = append(grid, row)
			row = []Cell{}
			continue
		case r == utf8.RuneError && size == 1:
			// 非法 UTF-8。tmux 吐的是合法 UTF-8，但一次被截断的抓屏可能把多字节
			// 字符拦腰切断。丢掉这个字节：替换成 U+FFFD 会在屏幕上放一个可见的
			// 豆腐块，而这只是传输残留、不是屏幕内容。
			continue
		case r < 0x20 || r == 0x7f:
			// 其余 C0 控制字符与 DEL（**含 \r**）。tmux 已经处理过它们；
			// 还留在这里的不是屏幕内容，也没有宽度。
			//
			// 这里曾经在上面单列过一条 `case r == '\r'`。变异测试证明它是**死代码**：
			// 删掉它之后，4000 条含 \r/\r\n 的随机语料解析出的网格指纹逐字节相同 ——
			// 因为 \r 是 0x0D、本来就 < 0x20，恒被本分支接住。留着它会让读者以为
			// CRLF 需要一条专门的规则，而真正承担这件事的是下面这个范围判断。
			continue
		}

		w := runeWidth(r)
		if w == 0 {
			// 零宽（组合符、ZWSP）。它属于前一个字符、不占自己的列。
			// 选择丢弃而不是合并：光栅器一格画一个 rune，要携带它就得引入一套
			// 渲染端根本用不上的字素簇模型。
			continue
		}

		fg, bg := state.resolve()
		row = append(row, Cell{R: r, FG: fg, BG: bg, Bold: state.bold, Width: w})
		if w == 2 {
			// 续格。颜色相同，好让逐格填背景能盖满整个字形；
			// R==0 且 Width==0 标明它"不是一个独立的字符"。
			row = append(row, Cell{R: 0, FG: fg, BG: bg, Bold: state.bold, Width: 0})
		}
	}

	// 末尾没有换行的那一行仍然是一行。tmux 的 capture-pane 不给最后一行收尾，
	// 丢掉它等于每张截图都静默少掉最下面一行。反过来，以 \n 结尾时那一行早已
	// 被冲出，绝不能再造出一个幽灵空行。
	if len(row) > 0 {
		grid = append(grid, row)
	}
	return grid
}

// scanEscape 吞掉 s 开头的一条转义序列，返回它的字节长度、若是 SGR 则返回其参数、
// 以及它到底是不是 SGR。
//
// 它返回的长度**恒 ≥ 1**，所以调用方不可能在畸形输入上死循环。
// 在字符串末尾被截断的序列（现实中的形态是末尾一个裸的 "\x1b["，来自趁 tmux
// 正在写入时读到的抓屏）会吞掉剩余全部：把它的字节当 cell 吐出去就是在屏幕上
// 放可见垃圾，而那正是这个函数存在所要防的事。
func scanEscape(s string) (n int, params []int, isSGR bool) {
	if len(s) < 2 {
		return len(s), nil, false
	}

	switch s[1] {
	case '[': // CSI —— 参数字节，然后是一个位于 @-~ 的终结字节
		i := 2
		for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == ';' || s[i] == ':' ||
			s[i] == '?' || s[i] == '<' || s[i] == '=' || s[i] == '>') {
			i++
		}
		if i >= len(s) {
			return len(s), nil, false // 被截断
		}
		final := s[i]
		body := s[2:i]
		i++
		if final != 'm' {
			return i, nil, false // 非 SGR 的 CSI：吞掉，忽略
		}
		return i, parseParams(body), true

	case ']': // OSC —— 一直到 BEL 或 ST（ESC \）
		i := 2
		for i < len(s) {
			if s[i] == 0x07 {
				return i + 1, nil, false
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2, nil, false
			}
			i++
		}
		return len(s), nil, false // 未终结

	default:
		// 双字符转义（ESC c、ESC (B …）。只吞 2 字节可能会把 "ESC ( B" 里的 "B"
		// 这类字符集指示符留下来变成可见字符，但对真正会出现的那几种形态而言 2 是对的，
		// 而多吞会改为吃掉真实的屏幕内容。
		return 2, nil, false
	}
}

// parseParams 把一条 SGR 的参数体切成整数。
//
// 空参数当 0：按 ECMA-48，ESC[m 与 ESC[;m 等价于 ESC[0m。这里写错会把一次 reset
// 变成空操作，于是它之前的样式会一路渗透到屏幕余下部分。
//
// 子参数（下划线样式那种 "4:3" 形态）也按 ':' 切开，好让它的尾部值被当成独立的码读、
// 落进 applySGR 的忽略分支，而不是被解析成一个假颜色。
func parseParams(body string) []int {
	if body == "" {
		return []int{0}
	}
	params := make([]int, 0, strings.Count(body, ";")+1)
	for _, f := range strings.Split(body, ";") {
		for _, sub := range strings.Split(f, ":") {
			params = append(params, atoiSGR(sub))
		}
	}
	return params
}

// atoiSGR 解析一个 SGR 参数，空串或畸形输入返回 0。
//
// 刻意不用 strconv.Atoi，这样就没有一条可以忘记处理的错误路径：SGR 的参数体
// 已经被 scanEscape 约束成数字了，而万一漏进来一个私有标记字节，它必须退化成 0
// （一次无害的 reset），而不是让整条转义中止。数值做钳制而不是任其溢出 ——
// 一个 20 位的参数不能回绕成某个恰好意为"粗体"的小数。
func atoiSGR(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0
		}
		n = n*10 + int(s[i]-'0')
		if n > 1<<20 {
			return 1 << 20
		}
	}
	return n
}

// stripSGR 去掉全部转义序列，还原成纯文本。
//
// Python 原版用真实屏幕对拍，保证了 stripSGR(capture(ansi=true)) 与
// capture(ansi=false) **逐字节相同**。正是这条等式让调用方敢于总是带颜色抓屏：
// --wait 的正则与退出标记都是在抓屏文本上匹配的，两种形态一旦不同，
// "要个颜色"就会静默改掉那些匹配所看到的东西。
func stripSGR(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			n, _, _ := scanEscape(s[i:])
			i += n
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
