package store

import (
	"strings"
	"unicode"
)

// maxCJKFallbackRows 限制 LIKE 回退扫描后返回的行数上限。
//
// FTS5 的 MATCH 走倒排索引，LIKE '%…%' 是全表扫描。上限存在不是为了「结果太多
// 看不完」——那是 limit 的职责——而是为了让一次退化查询的代价有界：一个几十万行
// 的 messages 表上，无界的 LIKE 会把一次 history_search 变成一次可感知的停顿。
const maxCJKFallbackRows = 200

// likeEscape 是 LIKE 模式里的转义字符。反斜杠而非默认（无转义），因为查询串
// 来自用户与模型，里面出现 % 和 _ 是常态（路径、SQL 片段、格式化字符串）。
const likeEscape = `\`

// hasCJK 报告 s 是否含中日韩文字。
//
// 判据是 Unicode 脚本表而不是码点区间：区间写法会在扩展区（Ext-B 及以后）
// 和补充平面上漏判，而那正是人名与生僻字所在的地方。
//
// 这个函数决定走 FTS5 还是走 LIKE 回退。判 false 时行为与引入前逐字节一致 ——
// 英文查询永远不进回退路径，这是本次改动零回归的根据。
func hasCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) ||
			unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) ||
			unicode.Is(unicode.Hangul, r) {
			return true
		}
	}
	return false
}

// likePattern 把查询串转成 SQLite LIKE 的模式与转义字符。
//
// 转义顺序是承重的：先转义反斜杠自身，再转义 % 和 _。反过来会把刚插入的
// 转义反斜杠再转义一遍，模式就不再匹配用户输入的字面量了。
func likePattern(q string) (string, string) {
	r := strings.NewReplacer(
		likeEscape, likeEscape+likeEscape,
		"%", likeEscape+"%",
		"_", likeEscape+"_",
	)
	return "%" + r.Replace(q) + "%", likeEscape
}

// cjkSnippetRadius 是片段窗口在命中两侧各保留的 rune 数。
// 与 FTS5 那侧 snippet(..., 24) 的量级对齐，好让两条路径的返回长度接近。
const cjkSnippetRadius = 24

// cjkSnippet 在 content 里围绕 query 的首次出现切出一个有界片段。
//
// 存在的理由：LIKE 回退拿不到 FTS5 的 snippet()，而返回整行会让一条几 KB 的
// 工具输出把检索结果撑爆。命中标记用与 FTS5 侧相同的书名号，因此 UI 不必分辨
// 结果来自哪条路径。
//
// 未命中时返回开头一段而不是空串：调用方拿到的是「这行匹配了」这个事实
// （匹配可能发生在 tool_args 而不是 content 上），空单元格会让它看起来像坏了。
func cjkSnippet(content, query string) string {
	runes := []rune(content)
	idx := strings.Index(content, query)
	if idx < 0 {
		return headRunes(runes, 2*cjkSnippetRadius)
	}
	start := len([]rune(content[:idx]))
	end := start + len([]rune(query))
	lo := max(0, start-cjkSnippetRadius)
	hi := min(len(runes), end+cjkSnippetRadius)

	var b strings.Builder
	if lo > 0 {
		b.WriteString(" … ")
	}
	b.WriteString(string(runes[lo:start]))
	b.WriteString("«")
	b.WriteString(string(runes[start:end]))
	b.WriteString("»")
	b.WriteString(string(runes[end:hi]))
	if hi < len(runes) {
		b.WriteString(" … ")
	}
	return b.String()
}

// headRunes 返回前 n 个 rune，不足则全部返回。
func headRunes(runes []rune, n int) string {
	if len(runes) <= n {
		return string(runes)
	}
	return string(runes[:n]) + " … "
}
