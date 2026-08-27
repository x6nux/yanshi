package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHasCJKDetectsEachScript(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"截止日期", true},
		{"张伟", true},
		{"プロジェクト", true}, // 片假名
		{"ひらがな", true},   // 平假名
		{"프로젝트", true},   // 한글
		{"deadline", false},
		{"", false},
		{"api_key v2", false},
		{"deadline 截止", true}, // 混合也算
	} {
		require.Equalf(t, tc.want, hasCJK(tc.in), "hasCJK(%q)", tc.in)
	}
}

func TestLikePatternEscapesWildcards(t *testing.T) {
	pat, esc := likePattern("100%_done")
	require.Equal(t, `%100\%\_done%`, pat)
	require.Equal(t, `\`, esc)
}

func TestLikePatternEscapesTheEscapeChar(t *testing.T) {
	pat, _ := likePattern(`a\b`)
	require.Equal(t, `%a\\b%`, pat,
		"an unescaped backslash would make the next character literal and change the match")
}

func TestParseFTSTermsStripsQuotesAndSplitsOnOR(t *testing.T) {
	require.Equal(t, []string{"张伟"}, parseFTSTerms(`"张伟"`))
	require.Equal(t, []string{"张伟", "项目"}, parseFTSTerms(`"张伟" OR "项目"`))
	require.Equal(t, []string{"张伟"}, parseFTSTerms("张伟"),
		"an unquoted query is one term, matching what MATCH does with a bare word")
	require.Equal(t, []string{"go OR die"}, parseFTSTerms(`"go OR die"`),
		"OR inside a quoted phrase is data, not a delimiter")
	require.Nil(t, parseFTSTerms(""))
}

// TestParseFTSTermsHandlesMixedAndBareBooleanQueries covers the two shapes the
// first version got silently wrong. Both are shapes the MODEL is instructed to
// produce: history_search's own error message tells it to "use double quotes
// for phrases, OR / NOT for boolean terms", and nothing requires it to quote
// every term.
func TestParseFTSTermsHandlesMixedAndBareBooleanQueries(t *testing.T) {
	require.Equal(t, []string{"张伟", "项目"}, parseFTSTerms(`"张伟" OR 项目`),
		"the unquoted half of a mixed query was dropped, silently, with no error")
	require.Equal(t, []string{"张伟", "项目"}, parseFTSTerms(`张伟 OR 项目`),
		"a bare boolean query was LIKE'd whole, literal \" OR \" included, so it matched nothing")
	require.Equal(t, []string{"项目", "张伟", "截止"}, parseFTSTerms(`项目 NOT "张伟" OR (截止)`),
		"grouping parens and NOT are syntax, not content; terms keep source order")
	require.Equal(t, []string{"gone or missing"}, parseFTSTerms(`"gone or missing"`),
		"lowercase or is not an FTS5 operator and must stay searchable")
	require.Equal(t, []string{"deadline", "or", "due"}, parseFTSTerms(`deadline or due`),
		"a bare lowercase or is an ordinary term, not a dropped delimiter")
}

func TestLikeAnyTermClauseEmptyTermsMatchesNothing(t *testing.T) {
	clause, args := likeAnyTermClause([]string{"m.content"}, nil)
	require.Equal(t, "0", clause)
	require.Empty(t, args)
}

func TestCJKSnippetBoundsTheWindow(t *testing.T) {
	// Padding on each side must exceed cjkSnippetRadius, or the "window" covers
	// the whole string and the snippet is not actually a window.
	pad := strings.Repeat("填充", 30)
	content := "前" + pad + "截止日期" + pad + "后"
	s := cjkSnippet(content, "截止日期")
	require.Contains(t, s, "截止日期")
	require.Less(t, len([]rune(s)), len([]rune(content)),
		"a snippet that returns the whole row is not a snippet")
}

func TestCJKSnippetOnMissReturnsHead(t *testing.T) {
	s := cjkSnippet("完全不相关的内容", "截止日期")
	require.NotEmpty(t, s, "a miss must still yield something renderable, not an empty cell")
}

// seedCJK writes one message and one memory carrying the same Chinese sentence.
func seedCJK(t *testing.T, s *Store) string {
	t.Helper()
	const sentence = "项目的截止日期是周二，负责人是张伟"
	sid, err := s.CreateSession("cjk")
	require.NoError(t, err)
	require.NoError(t, s.AppendMessage(sid, 1, RoleUser, sentence))
	_, err = s.WriteMemory("note", sentence)
	require.NoError(t, err)
	return sid
}

// ledger: A2/W-A-03#1 中文词查询在 history_search 上返回非零命中
func TestSearchMessagesFindsChineseWords(t *testing.T) {
	s := openTestStore(t)
	sid := seedCJK(t, s)

	for _, word := range []string{"截止日期", "项目", "周二", "张伟"} {
		hits, err := s.SearchMessages(sid, word, 10)
		require.NoError(t, err)
		require.NotEmptyf(t, hits, "query %q returned zero hits", word)
		require.NotEmptyf(t, hits[0].Snippet, "query %q returned an empty snippet", word)
	}
}

// ledger: A2/W-A-03#2 SearchMemory 与 memory_autorecall 走同一检索路径因而同时生效
func TestSearchMemoryFindsChineseWords(t *testing.T) {
	s := openTestStore(t)
	seedCJK(t, s)

	for _, word := range []string{"截止日期", "项目", "周二", "张伟"} {
		hits, err := s.SearchMemoryRanked(word, 10, MemoryFilter{})
		require.NoError(t, err)
		require.NotEmptyf(t, hits, "memory query %q returned zero hits", word)
	}
}

// ledger: A2/W-A-03#3 英文查询的命中集合与本改动前逐条一致
func TestSearchMessagesEnglishPathIsUnchanged(t *testing.T) {
	s := openTestStore(t)
	sid, err := s.CreateSession("en")
	require.NoError(t, err)
	require.NoError(t, s.AppendMessage(sid, 1, RoleUser,
		"the deadline for the project is Tuesday and the owner is Wei"))
	require.NoError(t, s.AppendMessage(sid, 2, RoleAssistant,
		"unrelated text about compilers"))

	hits, err := s.SearchMessages(sid, "deadline", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1, "the English path must still go through FTS5 MATCH")
	require.Equal(t, 1, hits[0].Seq)
	require.Contains(t, hits[0].Snippet, "«",
		"an English hit must still carry the FTS5 snippet markers")

	// Stemming is a porter-tokenizer property; losing it would mean the English
	// path silently switched to LIKE.
	stemmed, err := s.SearchMessages(sid, "deadlines", 10)
	require.NoError(t, err)
	require.Len(t, stemmed, 1, "porter stemming must survive this change")
}

// ledger: A2/W-A-03#4 CJK 回退路径有结果数上限
func TestSearchMessagesCJKFallbackIsBounded(t *testing.T) {
	s := openTestStore(t)
	sid, err := s.CreateSession("bulk")
	require.NoError(t, err)
	for i := 1; i <= maxCJKFallbackRows+50; i++ {
		require.NoError(t, s.AppendMessage(sid, i, RoleUser, "截止日期"))
	}

	hits, err := s.SearchMessages(sid, "截止日期", 100000)
	require.NoError(t, err)
	require.LessOrEqual(t, len(hits), maxCJKFallbackRows,
		"an unbounded LIKE scan turns one search into a visible stall")
}
