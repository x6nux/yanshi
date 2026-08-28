package store

import (
	"regexp"
	"strings"
	"unicode"
)

// maxCJKFallbackRows bounds how many rows the LIKE fallback scans and
// returns.
//
// FTS5's MATCH walks an inverted index; LIKE '%…%' is a full table scan. The
// bound exists not because "too many results are hard to read" — that is
// limit's job — but to keep one degraded query's cost bounded: on a
// messages table with hundreds of thousands of rows, an unbounded LIKE would
// turn a single history_search call into a noticeable stall.
const maxCJKFallbackRows = 200

// likeEscape is the escape character used in LIKE patterns. Backslash rather
// than SQLite's default of "no escape character", because query strings come
// from users and models, and % and _ show up in them routinely (paths, SQL
// fragments, format strings).
const likeEscape = `\`

// hasCJK reports whether s contains Chinese, Japanese, or Korean script.
//
// The test is the Unicode script tables, not a codepoint range: a
// range-based test would miss Extension B and later, and the supplementary
// planes — exactly where uncommon names and rare characters live.
//
// This function decides whether a query goes through FTS5 or the LIKE
// fallback. When it returns false, behaviour is byte-for-byte identical to
// before this change — English queries never enter the fallback path, which
// is the basis for this change causing zero regression there.
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

// likePattern turns a query string into a SQLite LIKE pattern plus the
// escape character to pass alongside it.
//
// strings.NewReplacer performs a single left-to-right pass over the input
// and never rescans text it has just written, so the three replacement
// pairs below do not interact with each other regardless of the order they
// are listed in — swapping the order produces byte-identical output. That
// would NOT be true of a naive sequential series of strings.ReplaceAll
// calls: replacing backslash-escapes first and then escaping literal `%`
// would insert fresh backslashes that a later `_`-escaping pass could
// mistake for user input and re-escape, corrupting the pattern. NewReplacer
// is used precisely so that hazard does not exist; the ordering here is a
// consequence of using it, not a requirement it depends on.
func likePattern(q string) (string, string) {
	r := strings.NewReplacer(
		likeEscape, likeEscape+likeEscape,
		"%", likeEscape+"%",
		"_", likeEscape+"_",
	)
	return "%" + r.Replace(q) + "%", likeEscape
}

// ftsQuotedTermRe matches one double-quoted phrase in FTS5 MATCH syntax.
var ftsQuotedTermRe = regexp.MustCompile(`"([^"]*)"`)

// parseFTSTerms splits an FTS5 MATCH-syntax query into the terms MATCH's OR
// operator would search for, so the CJK LIKE fallback can approximate the
// same "any term matches" semantics without an FTS5 parser.
//
// Every production caller of SearchMessages / SearchMemoryRanked documents
// its query argument as FTS5 syntax: memory_autorecall's ftsQuery renders
// `"term1" OR "term2"`, and history_search's own error message tells the
// model to "use double quotes for phrases, OR / NOT for boolean terms". A
// query built that way is useless as a literal LIKE pattern — a match would
// require the row to contain the quote characters and the literal word OR —
// which is exactly the bug that left memory_autorecall dead in Chinese even
// after the LIKE fallback existed: hasCJK saw the CJK runes inside the
// quoted-OR string and routed it into the fallback, which then searched for
// `"张伟" OR "项目"` as one literal substring and found nothing.
//
// Quoted phrases are extracted whole, so a phrase that happens to contain the
// substring " OR " or a bare double quote is not mistaken for a delimiter: the
// quote pairs are located first, and only the text BETWEEN them is tokenized.
//
// Mixed and unquoted queries are tokenized rather than taken whole. The first
// version handled only the fully-quoted shape and dropped everything else,
// which produced two silent failures on queries the model is actively taught
// to write (history_search's own error text advertises `OR` / `NOT`):
//
//	`"张伟" OR 项目`  → ["张伟"]            — 项目 vanished, no error
//	`张伟 OR 项目`    → ["张伟 OR 项目"]    — LIKE'd whole, including the
//	                                        literal " OR ", so: zero hits
//
// Both now yield ["张伟", "项目"]. FTS5's boolean operators are uppercase-only,
// so dropping the exact tokens OR/AND/NOT/NEAR still lets a search for the
// English word "or" through as an ordinary term.
//
// The OR-of-all-terms approximation is deliberately loose where FTS5 would be
// strict: `a b` is an implicit AND in MATCH, and NOT is an exclusion this
// function has no way to express. Loose costs extra rows on a path that is
// already unranked and bounded (maxCJKFallbackRows); strict-by-accident —
// which is what LIKE'ing the raw string was — costs the whole result set.
//
// The return value is never empty for a non-empty input: a query that
// consists only of quote/OR syntax with no content between the quotes (a
// shape no real caller in this codebase produces) falls back to the raw
// trimmed query as one term. That guarantee matters to the caller —
// likeAnyTermClause turns an empty term list into a clause that matches
// nothing, and building that from a non-empty query would silently discard
// a search a caller expected to run.
func parseFTSTerms(query string) []string {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	var terms []string
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			terms = append(terms, s)
		}
	}
	// Walk the quoted spans in order, tokenizing the unquoted text between
	// them. Locating the quotes first is what keeps ` OR ` inside a phrase
	// from being read as a delimiter.
	last := 0
	for _, loc := range ftsQuotedTermRe.FindAllStringSubmatchIndex(query, -1) {
		for _, tok := range strings.Fields(query[last:loc[0]]) {
			add(trimFTSToken(tok))
		}
		add(query[loc[2]:loc[3]])
		last = loc[1]
	}
	for _, tok := range strings.Fields(query[last:]) {
		add(trimFTSToken(tok))
	}
	if len(terms) == 0 {
		return []string{query}
	}
	return terms
}

// ftsOperators are the FTS5 MATCH keywords that are syntax, not content. FTS5
// recognises them only in uppercase, so a lowercase "or" stays searchable as
// an ordinary word.
var ftsOperators = map[string]bool{"OR": true, "AND": true, "NOT": true, "NEAR": true}

// trimFTSToken strips the grouping punctuation FTS5 allows around a bare term
// and returns "" for a token that is pure syntax. Returning "" rather than the
// token itself matters: a literal "OR" in the LIKE pattern is what made
// `张伟 OR 项目` match nothing at all.
func trimFTSToken(tok string) string {
	tok = strings.Trim(tok, "()")
	if ftsOperators[tok] {
		return ""
	}
	return tok
}

// likeAnyTermClause builds a SQL fragment (plus its bound arguments) that
// matches a row if ANY of terms is found, via LIKE, in ANY of cols.
//
// This is the LIKE-side equivalent of what FTS5's MATCH does for an OR'd set
// of terms across the columns an FTS table indexes — the CJK fallback has no
// index and no query planner, but it can still honour the same "any term,
// any column" contract its callers rely on.
//
// An empty terms list returns the SQL literal "0" (always false) rather than
// an empty string. Concatenating an empty fragment into `WHERE x = ? AND
// ()` is invalid SQL, and — more importantly — an empty OR-chain is exactly
// the shape that would silently match every row instead of none. Callers
// only reach this function with a non-empty query (hasCJK requires at least
// one CJK rune), and parseFTSTerms never returns an empty slice for a
// non-empty query, so this path is a defensive backstop, not a path any
// current caller exercises.
func likeAnyTermClause(cols []string, terms []string) (string, []any) {
	if len(terms) == 0 {
		return "0", nil
	}
	var sb strings.Builder
	var args []any
	for i, term := range terms {
		if i > 0 {
			sb.WriteString(" OR ")
		}
		pattern, esc := likePattern(term)
		sb.WriteString("(")
		for j, col := range cols {
			if j > 0 {
				sb.WriteString(" OR ")
			}
			sb.WriteString(col + " LIKE ? ESCAPE ?")
			args = append(args, pattern, esc)
		}
		sb.WriteString(")")
	}
	return sb.String(), args
}

// cjkSnippetRadius is how many runes of context the snippet window keeps on
// each side of a hit. Matched to the order of magnitude of the FTS5 side's
// snippet(..., 24) call, so the two paths return excerpts of comparable
// length.
const cjkSnippetRadius = 24

// cjkSnippet cuts a bounded excerpt out of content, centred on the first
// occurrence of query.
//
// Why it exists: the LIKE fallback has no access to FTS5's snippet(), and
// returning the whole row would let a multi-kilobyte tool output blow up a
// search result. The hit markers match the ones FTS5's snippet() uses, so
// the UI does not need to know which path produced a given result.
//
// A miss returns a leading excerpt instead of an empty string: the caller
// still has the fact that this row matched (the match may have been on
// tool_args rather than content), and an empty cell would read as broken
// rather than as "matched elsewhere".
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

// snippetForTerms builds a cjkSnippet windowed around whichever of terms
// first appears in content.
//
// A multi-term OR match (see parseFTSTerms) does not know in advance which
// term is the one that matched a given row, and centring the snippet on a
// term absent from this particular row would produce a "miss" excerpt for a
// row that did, in fact, match. Falling through to terms[0] keeps the
// function total when none of the terms is literally present (which can
// happen when the match came from a different column than content).
func snippetForTerms(content string, terms []string) string {
	for _, t := range terms {
		if strings.Contains(content, t) {
			return cjkSnippet(content, t)
		}
	}
	if len(terms) > 0 {
		return cjkSnippet(content, terms[0])
	}
	return headRunes([]rune(content), 2*cjkSnippetRadius)
}

// headRunes returns the first n runes of runes, or all of them if there are
// fewer than n.
func headRunes(runes []rune, n int) string {
	if len(runes) <= n {
		return string(runes)
	}
	return string(runes[:n]) + " … "
}
