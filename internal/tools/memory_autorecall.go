// internal/tools/memory_autorecall.go
//
// C12: memory that fires without being asked.
//
// memory_search works and is almost never called. That is not a bug in the
// tool — it is the predictable outcome of a capability whose activation
// condition is "the model remembers it might have written something down about
// this". The model does not know what it stored, so it cannot know when a
// lookup would pay off, so it does not look, so the memories written last week
// are inert. A store nobody reads is the same shape as a store nobody writes.
//
// So the retrieval runs automatically: before each user turn, the user's own
// message is the query, and anything strongly matching is injected as context.
//
// THREE BOUNDS, AND EACH ONE IS THE FEATURE RATHER THAN A SAFETY MARGIN:
//
//  1. RELEVANCE. Below the overlap threshold NOTHING is injected. Injecting a
//     few weak matches every turn is worse than injecting none: it costs
//     tokens on every turn, and it trains the model to skim past the injected
//     block — which also disarms the turn where the match was right. The test
//     is TERM OVERLAP rather than the FTS score, because bm25 is
//     corpus-relative and a score floor suppresses every recall on a small
//     memory table; see AutoRecallCoverageDenominator.
//
//  2. COUNT AND SIZE. At most MaxAutoRecall entries and AutoRecallCharBudget
//     characters. The injection happens on every turn, so an unbounded one is
//     a per-turn tax that grows with the memory table.
//
//  3. FRAMING. The block says these are notes from EARLIER conversations, with
//     their dates. Without that, a stale preference reads as something the
//     user said a moment ago, and the model acts on a year-old instruction as
//     if it were the current request. Retrieval that cannot be dated is
//     retrieval that quietly rewrites the present.
package tools

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/x6nux/yanshi/internal/store"
)

// MaxAutoRecall caps how many memories one automatic retrieval injects.
//
// Three, because the block is competing with the user's actual message for the
// model's attention and a list long enough to skim is a list that gets skimmed.
// A model that wants more can call memory_search, which is what that tool is
// for; this path exists to make the model AWARE there is something to ask
// about, not to replace asking.
const MaxAutoRecall = 3

// AutoRecallCharBudget caps the injected block's memory bodies, in characters.
// A single memory longer than its share is truncated with a marker naming the
// tool that returns it in full, so the bound never silently changes what a
// memory says.
const AutoRecallCharBudget = 900

// autoRecallPerEntry is one entry's share of the budget. Computed rather than
// written as a constant so the two numbers cannot drift apart.
const autoRecallPerEntry = AutoRecallCharBudget / MaxAutoRecall

// AutoRecallHeader introduces the injected block.
//
// Every clause in it is load-bearing. "earlier conversations" dates the
// content; "not part of the current request" stops the model answering a
// memory instead of the user; "may be out of date" is what lets it prefer what
// the user just said when the two conflict; naming memory_search tells it what
// to do when the excerpt is not enough.
const AutoRecallHeader = "[recalled memory] Notes stored during EARLIER conversations, " +
	"retrieved automatically because they match what you were just asked. They are " +
	"background, not part of the current request, and may be out of date — when they " +
	"conflict with what the user just said, the user wins. Use memory_search for more."

// AutoRecall retrieves the memories relevant to text and renders them as a
// context block, or returns "" when nothing qualifies.
//
// The empty return is the common case and the correct one. Every guard below
// resolves toward it: no store, no usable query terms, no hits, no hit
// covering enough of the question. That asymmetry is deliberate — a missed
// injection costs the model one memory_search it might not make, while a
// spurious one is charged to every turn thereafter through the attention it
// trains away.
//
// dims scopes the retrieval. Callers pass the same dimensions the write path
// uses, so a session-scoped recall sees that session's memories plus the
// unscoped ones; a zero filter searches everything, matching memory_search's
// default.
func AutoRecall(ctx context.Context, s *store.Store, text string, dims store.MemoryFilter) string {
	hits := AutoRecallHits(ctx, s, text, dims)
	return RenderRecalledMemories(hits)
}

// AutoRecallHits is AutoRecall's retrieval half, exposed so a caller can log
// or count what was injected without re-running the query.
func AutoRecallHits(ctx context.Context, s *store.Store, text string, dims store.MemoryFilter) []store.MemoryHit {
	if s == nil {
		return nil
	}
	terms := AutoRecallTerms(text)
	if len(terms) == 0 {
		return nil
	}
	// Over-fetch: the relevance filter runs in Go, so asking for exactly
	// MaxAutoRecall would let a weak hit occupy a slot a good one could have
	// used. The multiplier is small — this is a per-turn query.
	hits, err := s.SearchMemoryRanked(ftsQuery(terms), MaxAutoRecall*3, dims)
	if err != nil {
		// A malformed FTS query or a closed database must not fail the TURN.
		// This is an unrequested enrichment; the honest degradation is to
		// carry on without it, which is exactly the pre-C12 behaviour.
		return nil
	}
	out := make([]store.MemoryHit, 0, MaxAutoRecall)
	for _, h := range hits {
		if !Relevant(terms, h.Content) {
			continue
		}
		out = append(out, h)
		if len(out) == MaxAutoRecall {
			break
		}
	}
	if ctx != nil && ctx.Err() != nil {
		return nil
	}
	return out
}

// AutoRecallCoverageDenominator sets how much of the user's question a memory
// must cover before it is injected: at least len(terms)/N of the distinctive
// terms, rounded up, and never fewer than one.
//
// RELEVANCE IS A RATIO, NOT A COUNT, and it is not the FTS score either.
//
// The score was the obvious choice and is unusable, measured rather than
// assumed: bm25's IDF term is corpus-relative, so on a two-row memory table an
// obviously-correct single-term match scores -1e-06 while the same quality of
// match on a large table scores in the single digits. Any absolute floor
// across that is a fit to one table size, and it fails in the worst direction
// — a new install has few memories, every score sits near zero, and the floor
// suppresses every correct recall on exactly the tables where each memory
// carries the most weight.
//
// A fixed COUNT ("share at least two words") fails the other way: a user who
// asks "format this with gofmt" offers three distinctive terms and a correct
// note shares one of them, so a floor of two silences the best match in the
// store. That case is what turned this constant from a count into a ratio.
//
// A ratio says something corpus-independent and shape-independent: the memory
// covers a meaningful share of WHAT WAS ASKED. Twelve distinctive words
// against a note sharing one is coincidence; three words against a note
// sharing one is a third of the question. The denominator is written as an
// integer rather than a float for the same reason
// ctxcompact.QualityPolicy.MinCompressionDenominator is — the configured value
// is then exact and reads the way the requirement is stated.
const AutoRecallCoverageDenominator = 3

// Relevant reports whether content covers enough of the query's distinctive
// terms to be worth injecting unbidden.
func Relevant(terms []string, content string) bool {
	need := RequiredOverlap(len(terms))
	if need == 0 {
		return false
	}
	low := strings.ToLower(content)
	hit := 0
	for _, t := range terms {
		if strings.Contains(low, t) {
			hit++
			if hit >= need {
				return true
			}
		}
	}
	return false
}

// RequiredOverlap is how many of n query terms a memory must contain:
// ceil(n / AutoRecallCoverageDenominator), floored at 1 for any non-empty
// query. It returns 0 for an empty query, where nothing can qualify.
func RequiredOverlap(n int) int {
	if n <= 0 {
		return 0
	}
	need := (n + AutoRecallCoverageDenominator - 1) / AutoRecallCoverageDenominator
	if need < 1 {
		need = 1
	}
	return need
}

// autoRecallTermRe extracts the tokens worth searching for: runs of letters and
// digits, plus CJK, which FTS5's default tokenizer indexes per character.
var autoRecallTermRe = regexp.MustCompile(`[\p{L}\p{N}_]+`)

// autoRecallStopWords are terms too common to carry a signal. The list is
// short and English-only on purpose: it exists to stop a query degenerating
// into "the OR a OR is", not to be a linguistics project. A term that slips
// through still has to clear the relevance ceiling, so the stop list is an
// optimisation on top of a real filter rather than the filter itself.
var autoRecallStopWords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "but": true,
	"is": true, "are": true, "was": true, "were": true, "be": true, "been": true,
	"to": true, "of": true, "in": true, "on": true, "at": true, "for": true,
	"with": true, "by": true, "from": true, "as": true, "it": true, "this": true,
	"that": true, "these": true, "those": true, "i": true, "you": true, "we": true,
	"do": true, "does": true, "did": true, "can": true, "could": true, "would": true,
	"should": true, "will": true, "please": true, "help": true, "me": true,
}

// minAutoRecallTerm is the shortest term kept from the user's text. One- and
// two-character latin fragments match almost everything; CJK is exempt because
// a single han character is a whole word.
const minAutoRecallTerm = 3

// maxAutoRecallTerms bounds the generated query. A pasted stack trace would
// otherwise produce a thousand-term OR that matches every memory weakly — the
// exact shape the relevance ceiling then has to reject one by one.
const maxAutoRecallTerms = 12

// AutoRecallTerms extracts the distinctive lower-cased terms from free user
// text — the words worth searching for and worth scoring an overlap against.
//
// Stop words and short latin fragments are dropped: a query of common words
// matches everything weakly, which is precisely the noise the overlap test
// then has to reject one row at a time. CJK singles are kept, since a han
// character is a whole word rather than a fragment.
func AutoRecallTerms(text string) []string {
	raw := autoRecallTermRe.FindAllString(strings.ToLower(text), -1)
	seen := make(map[string]bool, len(raw))
	kept := make([]string, 0, maxAutoRecallTerms)
	for _, t := range raw {
		if seen[t] || autoRecallStopWords[t] {
			continue
		}
		if len([]rune(t)) < minAutoRecallTerm && !hasWideChar(t) {
			continue
		}
		seen[t] = true
		kept = append(kept, t)
		if len(kept) == maxAutoRecallTerms {
			break
		}
	}
	return kept
}

// AutoRecallQuery turns free user text into an FTS5 query, or "" when the text
// has nothing worth searching for.
//
// The terms are ORed rather than ANDed. An AND query over a sentence finds a
// memory only if it contains every content word of that sentence, which for a
// store of short notes means it finds nothing — and "nothing" here is
// indistinguishable from "you have no relevant memories", so the feature would
// look like it worked while never firing. OR plus the overlap test in
// Relevant is the combination that can both fire and stay quiet.
//
// Every term is double-quoted, which is what keeps a user message from being
// interpreted as FTS5 syntax: a bare `NOT` or `*` in the text would otherwise
// be an operator, and an unbalanced quote a syntax error.
func AutoRecallQuery(text string) string { return ftsQuery(AutoRecallTerms(text)) }

// ftsQuery renders terms as a quoted OR query, or "" for no terms.
func ftsQuery(terms []string) string {
	if len(terms) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(terms))
	for _, t := range terms {
		quoted = append(quoted, `"`+t+`"`)
	}
	return strings.Join(quoted, " OR ")
}

// hasWideChar reports whether s contains a character outside the ASCII range,
// which is the cheap test for "this short token is CJK, not a latin fragment".
func hasWideChar(s string) bool {
	for _, r := range s {
		if r > 0x7F {
			return true
		}
	}
	return false
}

// RenderRecalledMemories formats hits as the injected context block, or ""
// when there are none.
//
// Each entry carries its DATE, which is the single most important thing in the
// block: a preference recorded in March and a preference stated in this
// conversation are different kinds of fact, and a renderer that omits the date
// makes them look identical.
func RenderRecalledMemories(hits []store.MemoryHit) string {
	if len(hits) == 0 {
		return ""
	}
	// Oldest first: the block reads as a timeline, and the newest note — the
	// one most likely to supersede the others — sits closest to the request.
	ordered := append([]store.MemoryHit(nil), hits...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].CreatedAt < ordered[j].CreatedAt })

	var b strings.Builder
	b.WriteString(AutoRecallHeader)
	for _, h := range ordered {
		b.WriteString("\n\n")
		fmt.Fprintf(&b, "- [%s] %s", h.Kind, time.Unix(h.CreatedAt, 0).Format("2006-01-02"))
		if len(h.DistilledFrom) > 0 {
			// Say so: a consolidated note is a summary of several originals,
			// and a reader that treats it as a single verbatim statement will
			// over-trust its wording.
			fmt.Fprintf(&b, " (consolidated from %d earlier notes)", len(h.DistilledFrom))
		}
		body := strings.TrimSpace(h.Content)
		if len(body) > autoRecallPerEntry {
			body = body[:autoRecallPerEntry] + " … [truncated; memory_search returns it in full]"
		}
		if body != "" {
			b.WriteString("\n  " + strings.ReplaceAll(body, "\n", "\n  "))
		}
	}
	return b.String()
}
