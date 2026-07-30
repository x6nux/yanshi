package tui

import (
	"math"
	"testing"
)

func TestFuzzyScore_EmptyQueryIsMax(t *testing.T) {
	got := fuzzyScore("", "/help")
	if got != math.MaxFloat64 {
		t.Fatalf("空 query 应返回 MaxFloat(显示所有项),得到 %v", got)
	}
}

func TestFuzzyScore_CaseInsensitive(t *testing.T) {
	a := fuzzyScore("HELP", "/help")
	b := fuzzyScore("help", "/help")
	if a != b {
		t.Fatalf("大小写不应影响分数: upper=%v lower=%v", a, b)
	}
	if a == 0 {
		t.Fatalf("匹配项不应是 0 分")
	}
}

func TestFuzzyScore_ContiguousBeatsScattered(t *testing.T) {
	// "/model" 里的 "mo" 连续;"mode" 与 "theme" 都跳跃
	cont := fuzzyScore("mo", "/model")
	scat := fuzzyScore("me", "/model") // m...e 跳跃
	if cont <= scat {
		t.Fatalf("连续匹配应得分更高: contiguous=%v scattered=%v", cont, scat)
	}
}

func TestFuzzyScore_NonSubstringIsZero(t *testing.T) {
	if got := fuzzyScore("xyz", "/help"); got != 0 {
		t.Fatalf("非子串应返回 0(过滤),得到 %v", got)
	}
}

func TestFuzzyScore_EarlierMatchHigher(t *testing.T) {
	// "/clear" 里的 "c" 在位置 1;"cmd" 里的 "c" 在位置 0
	early := fuzzyScore("c", "/clear")
	late := fuzzyScore("c", "abclear") // c 在位置 2
	if early <= late {
		t.Fatalf("靠前的匹配应得分更高: early=%v late=%v", early, late)
	}
}

func TestDropLastRune_UTF8(t *testing.T) {
	if got := dropLastRune("ab你"); got != "ab" {
		t.Fatalf("应删除完整 UTF-8 rune,得到 %q", got)
	}
	if got := dropLastRune(""); got != "" {
		t.Fatalf("空串应保持为空,得到 %q", got)
	}
}
