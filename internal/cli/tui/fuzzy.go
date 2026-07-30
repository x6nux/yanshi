package tui

import (
	"math"
	"strings"
	"unicode/utf8"
)

// dropLastRune removes the last UTF-8 rune from s, returning "" for an empty
// string. Used by history/input backspace paths that must respect multi-byte
// runes (Chinese, emoji) rather than dropping a single byte.
func dropLastRune(s string) string {
	if s == "" {
		return ""
	}
	_, size := utf8.DecodeLastRuneInString(s)
	return s[:len(s)-size]
}

// fuzzyScore 给 popup 补全项打分。返回 [0, +∞):0 表示不匹配(应过滤掉);
// 空 query 返回 MaxFloat64(所有项都应显示,排序无意义)。
//
// 规则(从强到弱):
//   - 连续匹配 > 跳跃匹配(每断开一次扣分)。
//   - 靠前的匹配 > 靠后的(前缀加分)。
//   - query 占 target 比例越高越好(短 target 上的完整匹配 > 长 target 上的同样匹配)。
//
// 故意简单(无 edit-distance、无分词),~30 行覆盖 90% TUI 用例;deepseek-tui 的
// palette 用的也是类似简单评分。后续若需更智能的模糊(fuzzy-match 如 fzf),再单独
// 抽包,现在避免引入第三方依赖。
func fuzzyScore(query, target string) float64 {
	if query == "" {
		return math.MaxFloat64
	}
	q := strings.ToLower(query)
	t := strings.ToLower(target)
	if !strings.Contains(t, q) {
		// 不是子串 → 退化到"逐字符包含"检查(允许跳跃)
		return scatteredScore(q, t)
	}
	// 完整子串:基础分 = 长度比(短 target 得分高)+ 前缀奖励(越靠前越高)
	idx := strings.Index(t, q)
	base := float64(len(q)) / float64(max(len(t), 1))
	prefix := 1.0 / float64(idx+1) // idx=0 → 1.0;idx 越大奖励越小
	return 1.0 + base + prefix
}

// scatteredScore 在 query 不是 target 连续子串时,逐字符在 target 中寻找(允许
// 跳跃)。任一字符在 target 中找不到 = 0(完全无法匹配)。否则按"用到的 target
// 跨度"打分:跨度越短越好(跳跃越少)。
func scatteredScore(query, target string) float64 {
	from := 0
	for _, r := range query {
		i := strings.IndexRune(target[from:], r)
		if i < 0 {
			return 0
		}
		from += i + 1
	}
	// from 现在是"匹配完所有字符后的 target 游标"——值越小,匹配越紧凑。
	// 用 (1 - from/len) 作为分数,最低 0(target 刚好装下)。
	if from >= len(target) {
		return 0.01 // 极端情况:匹配消耗了整个 target
	}
	return float64(from) / float64(len(target)) * 0.5 // 散匹最高 0.5,低于完整子串的最低 1.0
}
