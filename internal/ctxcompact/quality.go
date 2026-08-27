// internal/ctxcompact/quality.go
package ctxcompact

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// QualityPolicy is the deterministic gate a candidate summary must clear before
// Assemble is allowed to REPLACE the summarized messages with it.
//
// Why a gate at all. Run's only previous check was "is the summary blank".
// A model that answers "好的，我已经总结完毕。" is not blank, so it passed, and
// Assemble then dropped every summarized message and left that sentence in
// their place. The callers cannot catch it either: their best-effort test is
// TokensAfter < TokensBefore, and a summary that threw the history away is the
// single best score anything can get on that test. The worse the failure, the
// more convincing it looks — which is why the check has to happen here, on the
// text, before the replacement is made.
//
// The checks are deterministic on purpose: a second model call to judge the
// first one has the same failure mode one level up, and costs another round
// trip on the latency path a compaction is already stalling.
//
// A zero QualityPolicy disables every check, which is what makes the gate
// adoptable one caller at a time; DefaultQualityPolicy is the configured shape.
type QualityPolicy struct {
	// MinChars is the CEILING of the length demand, in runes (not bytes — a
	// CJK summary is information-dense and short in runes but long in bytes,
	// and a byte bound would let an English one-liner through while failing a
	// legitimate Chinese paragraph). 0 disables the length check.
	//
	// It is not applied as a flat floor; see requiredRunes for why, and for how
	// it combines with MinCompressionDenominator.
	MinChars int

	// MinCompressionDenominator scales the length demand DOWN for small inputs:
	// the effective floor is inputRunes/N, capped by MinChars. 0 disables the
	// scaling, making MinChars a flat floor.
	//
	// It is a DENOMINATOR rather than a float ratio so the configured value is
	// exact and reads the way the requirement is stated. See requiredRunes.
	MinCompressionDenominator int

	// RequiredSections, when non-empty, requires every listed heading to appear
	// in the summary. This is a SUBSTRING check and stays that way: it is the
	// weak, prompt-agnostic form, usable by a caller whose summarizer is not
	// producing this package's structured document at all.
	//
	// For the structured document, RequireStructure is the check that matters
	// — see there for why "the heading appears somewhere in the text" is a
	// materially weaker claim than "the document parses".
	RequiredSections []string

	// RequireStructure demands that the summary parse as the five-section
	// continuation document (ParseStructured).
	//
	// WHY THIS IS NOT JUST RequiredSections WITH FIVE ENTRIES. A substring
	// scan passes on a summary that MENTIONS "Open Work" in a sentence, on one
	// whose sections arrived out of order, on one that grew a sixth heading
	// whose content will be dropped at the next Render, and on one whose Open
	// Work heading is followed by nothing at all. Each of those is a document
	// the next compaction reads back wrong, and each of them scores perfectly
	// on the callers' only other test (TokensAfter < TokensBefore) precisely
	// because it threw information away.
	//
	// The structured summary is what makes this checkable at all: before C4
	// there was no shape to verify, so "did the model actually answer the
	// question" had no mechanical form. This is C10 collecting on that.
	//
	// It is OFF in the zero policy and OFF in DefaultQualityPolicy, and that
	// is deliberate: RunSummary asks for the structured form, but a caller may
	// have wired a summarizer that does not produce it, and a gate that
	// rejects every summary is indistinguishable from compaction being broken.
	// It is opt-in for a caller who knows both ends are this package's.
	RequireStructure bool
}

// enabled reports whether p asks for any check at all. A zero policy is the
// "gate off" state, and every rule consults this rather than inferring it from
// its own field — otherwise isMetaOnly, whose rule has no numeric knob to zero
// out, would keep firing under a policy the caller believed they had disabled.
func (p QualityPolicy) enabled() bool {
	return p.MinChars > 0 || p.MinCompressionDenominator > 0 || len(p.RequiredSections) > 0 || p.RequireStructure
}

// requiredRunes returns the minimum rune count a summary of an inputRunes-long
// transcript must have: `min(MinChars, inputRunes/MinCompressionDenominator)`.
//
// ONE RULE, NOT TWO, and the shape is the whole design. A flat MinChars is the
// wrong test at small input sizes — a six-message exchange legitimately
// summarizes to one line, and a flat floor would reject that correct compaction
// while a bare ratio would demand nothing at all from a huge history that
// collapsed to a sentence. Combining them as a minimum means:
//
//   - LARGE input: the ratio term is the larger, so MinChars binds. A 200K-rune
//     session must produce at least MinChars runes — "好的，我已经总结完毕。"
//     cannot stand in for it.
//   - SMALL input: MinChars is the larger, so the ratio binds and the demand
//     shrinks with the input. Nothing is demanded that the input cannot justify.
//
// The denominator is deliberately loose (see DefaultQualityPolicy). This check
// is a catastrophe detector, not a quality judgement: compaction's entire
// purpose is a large reduction, so any denominator tight enough to express "a
// good summary" would reject good compactions. The semantic failure — a reply
// that is long enough but is still an acknowledgement — is isMetaOnly's job,
// and it fires at every input size.
func (p QualityPolicy) requiredRunes(inputRunes int) int {
	if p.MinChars <= 0 {
		return 0
	}
	if p.MinCompressionDenominator <= 0 || inputRunes <= 0 {
		return p.MinChars
	}
	if scaled := inputRunes / p.MinCompressionDenominator; scaled < p.MinChars {
		return scaled
	}
	return p.MinChars
}

// DefaultQualityPolicy is the policy Run applies when a caller passes none.
//
// MinChars is 80: longer than every acknowledgement in metaOnlyPhrases, and
// shorter than any summary that names a file path, a command and an outcome.
// The denominator is 1000, so the 80-rune demand is only reached once the input
// passes 80,000 runes, and a smaller history is asked for proportionally less.
//
// 1/1000 is loose on purpose. A tighter ratio starts rejecting compactions that
// worked — a 40:1 reduction is a NORMAL result for a tool-heavy ReAct history
// full of repeated file reads — and every such rejection costs a model call and
// leaves the history uncompacted, which is the failure compaction exists to
// prevent. What it still catches is the catastrophic case: a 100,000-rune
// session cannot come back as a 40-rune sentence. The semantic detector
// (isMetaOnly) is what carries the load at ordinary sizes.
var DefaultQualityPolicy = QualityPolicy{
	MinChars:                  80,
	MinCompressionDenominator: 1000,
}

// metaOnlyPhrases are openers that a summary consisting of NOTHING BUT one of
// them is a refusal or an acknowledgement rather than a summary.
//
// The list is bilingual because yanshi's own instruction text is English while
// operators drive it in Chinese, and a model follows the conversation's
// language: an English-only list is blind to the exact failure most likely to
// occur in this repo's primary usage. Compare internal/archtest's D2 removal
// gate, which learned the same lesson.
//
// MATCHING IS PREFIX-ANCHORED AND LENGTH-BOUNDED, not "contains". "Sure, here
// is the summary: <2000 words of real summary>" is a perfectly good summary
// with a chatty preamble, and rejecting it would throw away a real compaction
// for a stylistic tic. isMetaOnly therefore only fires when the phrase is at
// the front AND essentially all of the text — see that function.
var metaOnlyPhrases = []string{
	// English acknowledgements / preambles
	"sure",
	"okay",
	"ok",
	"certainly",
	"of course",
	"understood",
	"got it",
	"here is the summary",
	"here's the summary",
	"here is a summary",
	"here's a summary",
	"i have summarized",
	"i've summarized",
	"the conversation has been summarized",
	"summary complete",
	"done",
	"no summary needed",
	"nothing to summarize",
	// English refusals — a refusal is not a summary, and it is short enough to
	// clear MinChars only in its longer forms, so it is named explicitly.
	"i cannot",
	"i can't",
	"i am unable",
	"i'm unable",
	"as an ai",
	// Chinese acknowledgements / refusals
	"好的",
	"好",
	"当然",
	"明白",
	"收到",
	"没问题",
	"已总结",
	"已经总结",
	"我已总结",
	"我已经总结",
	"总结完毕",
	"总结如下",
	"以下是总结",
	"以下是摘要",
	"摘要如下",
	"抱歉",
	"对不起",
	"无法总结",
	"没有需要总结",
	"我不能",
	"我无法",
}

// metaOnlyMaxRunes bounds how much INFORMATIVE text a candidate may carry and
// still be judged meta-only. Past this much it is carrying content whatever it
// opened with, and the prefix match stops being evidence.
//
// It is measured in informative runes (letters and digits) rather than raw
// length, which is what makes "好的。" + 400 ideographic full stops fail: that
// reply is 400 runes long and says nothing, and a raw-length bound would hand
// it a pass for being long. See informativeRunes.
//
// It is tied to DefaultQualityPolicy.MinChars rather than being a second free
// constant: the two must not be able to drift into a window where a candidate
// is too long to be meta-only and too short to pass the length rule, since
// nothing would judge it at all.
const metaOnlyMaxRunes = 3 * 80

// metaOnlyResidueMax is the most informative text that may follow a matched
// opener while the candidate still counts as nothing but that opener.
//
// It has to clear the longest phrase-plus-object a refusal or acknowledgement
// naturally carries — "I cannot" + "summarize this conversation" is 33
// informative characters, and "好的" + "我已经总结完毕" is comparable — while
// staying far below any real summary, which names a path, a command and an
// outcome and cannot do so in this space. 48 sits in that gap.
const metaOnlyResidueMax = 48

// QualityIssue names one way a candidate summary failed the gate. It is a
// distinct type rather than a string so callers can log the reasons without
// re-parsing an error message, and so a future policy can add a code without
// changing the shape.
type QualityIssue struct {
	// Rule is the stable identifier of the check that fired
	// ("too_short", "meta_only", "missing_section", "unstructured"). The
	// actual emitted values are the authority; this list is a reader's aid.
	Rule string
	// Detail is the human-readable specifics, including the measured values.
	Detail string
}

// String renders the issue as "rule: detail".
func (q QualityIssue) String() string { return q.Rule + ": " + q.Detail }

// ErrSummaryRejected is the sentinel every quality failure wraps.
//
// It is a sentinel because the failure is ACTIONABLE and distinct from the
// others Run can return: a model error means the provider is unhappy, whereas
// this means the provider answered fine and the answer was not a summary. The
// caller's handling is the same (keep the original history) but an operator
// reading the log needs to tell "compaction is broken" from "the summary model
// is too weak for this job", and errors.Is is how the WS/mid-turn layers will
// classify it without matching on message text.
var ErrSummaryRejected = errSummaryRejected{}

type errSummaryRejected struct{}

func (errSummaryRejected) Error() string { return "compaction: summary rejected by quality gate" }

// SummaryRejectedError reports a candidate summary that failed CheckQuality.
// It carries every issue so the layer that logs it can say which rule fired
// and on what measurements, instead of "compaction failed".
type SummaryRejectedError struct {
	// Issues is the non-empty list of rules that fired.
	Issues []QualityIssue
	// SummaryRunes and InputRunes are the measurements the rules ran against,
	// kept so a log line can show the ratio that was rejected.
	SummaryRunes int
	InputRunes   int
}

// Error renders every issue, plus the two measurements, on one line.
func (e *SummaryRejectedError) Error() string {
	parts := make([]string, 0, len(e.Issues))
	for _, iss := range e.Issues {
		parts = append(parts, iss.String())
	}
	return fmt.Sprintf("%s (%d summary runes over %d input runes): %s",
		ErrSummaryRejected.Error(), e.SummaryRunes, e.InputRunes, strings.Join(parts, "; "))
}

// Is makes errors.Is(err, ErrSummaryRejected) match.
func (e *SummaryRejectedError) Is(target error) bool {
	_, ok := target.(errSummaryRejected)
	return ok
}

// CheckQuality applies p to a candidate summary produced from inputRunes runes
// of transcript. It returns nil when the candidate is acceptable, and a
// *SummaryRejectedError listing EVERY rule that fired otherwise.
//
// All rules are evaluated rather than short-circuiting at the first failure:
// the caller logs this, and "too short AND meta-only" is a different diagnosis
// from either alone (the first says the model is weak, the pair says it
// refused).
//
// A zero policy accepts everything except the blank-summary case its callers
// already enforce, which is what lets the gate be adopted incrementally.
func CheckQuality(summary string, inputRunes int, p QualityPolicy) error {
	trimmed := strings.TrimSpace(summary)
	runes := utf8.RuneCountInString(trimmed)
	var issues []QualityIssue

	if required := p.requiredRunes(inputRunes); runes < required {
		issues = append(issues, QualityIssue{
			Rule: "too_short",
			Detail: fmt.Sprintf("summary is %d runes; %d runes of input require at least %d (min(%d, input/%d))",
				runes, inputRunes, required, p.MinChars, p.MinCompressionDenominator),
		})
	}
	if p.enabled() && isMetaOnly(trimmed) {
		issues = append(issues, QualityIssue{
			Rule:   "meta_only",
			Detail: fmt.Sprintf("summary is an acknowledgement or refusal, not a summary: %q", firstRunes(trimmed, 60)),
		})
	}
	for _, section := range p.RequiredSections {
		if section == "" {
			continue
		}
		if !strings.Contains(strings.ToLower(trimmed), strings.ToLower(section)) {
			issues = append(issues, QualityIssue{
				Rule:   "missing_section",
				Detail: fmt.Sprintf("required section %q is absent", section),
			})
		}
	}
	if p.RequireStructure {
		// The covered range is irrelevant to whether the document PARSES —
		// it only supplies the fallback attribution for unpointed items — so
		// the gate passes a zero range rather than plumbing one it would not
		// use. See ParseStructured.
		if _, err := ParseStructured(trimmed, SeqRef{}); err != nil {
			issues = append(issues, QualityIssue{
				Rule:   "unstructured",
				Detail: err.Error(),
			})
		}
	}
	if len(issues) == 0 {
		return nil
	}
	return &SummaryRejectedError{Issues: issues, SummaryRunes: runes, InputRunes: inputRunes}
}

// isMetaOnly reports whether s is nothing but an acknowledgement, a refusal, or
// a preamble with no summary behind it.
//
// Two guards keep it from eating real summaries, and BOTH measure informative
// characters — letters and digits — rather than raw length:
//
//  1. SIZE. Past metaOnlyMaxRunes of informative text the candidate is carrying
//     content no matter how it opened, so no phrase match is consulted at all.
//     Measuring informative characters rather than raw runes is what makes
//     "好的。" followed by four hundred full stops fail: it is long and says
//     nothing, and a raw bound would pass it for being long.
//  2. PREFIX ANCHORING plus a RESIDUE test. The phrase must START the text, and
//     what remains after it must exceed metaOnlyResidueMax to count as content.
//     "Sure, here is the summary: <a real summary>" leaves a large residue and
//     is accepted; "Sure, here is the summary." leaves none and is rejected.
//
// The anchoring is why this is not a `contains` scan. A real summary that
// merely mentions "the user said okay" opens with neither phrase and is never
// tested against them.
func isMetaOnly(s string) bool {
	if s == "" {
		return true
	}
	if informativeRunes(s) > metaOnlyMaxRunes {
		return false
	}
	lower := strings.ToLower(s)
	for _, phrase := range metaOnlyPhrases {
		if !strings.HasPrefix(lower, phrase) {
			continue
		}
		if informativeRunes(lower[len(phrase):]) <= metaOnlyResidueMax {
			return true
		}
	}
	return false
}

// informativeRunes counts letters and digits, ignoring whitespace, punctuation
// and symbols. See isMetaOnly for why the distinction matters.
func informativeRunes(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			n++
		}
	}
	return n
}

// firstRunes returns at most n runes of s, rune-safe, with an ellipsis when it
// truncated. Used to quote a rejected candidate in an error without dumping it
// whole into a log line.
func firstRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
