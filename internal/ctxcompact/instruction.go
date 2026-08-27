// internal/ctxcompact/instruction.go
package ctxcompact

import (
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// defaultSummaryWordLimit is the length asked for when the caller configures
// none.
const defaultSummaryWordLimit = 500

// instructionBudgetDenominator bounds what fraction of the model window the
// instruction itself may occupy before it is emitted in its TERSE form.
//
// WHY THIS EXISTS AT ALL. The five-section instruction with its full rule list
// costs ~760 tokens to produce a summary and ~1025 to update one — measured,
// not guessed; TestInstructionSizesAreBounded prints the current numbers. That
// is a rounding error against a 128K window and a catastrophe against the 8K
// window of a small fast summary model, which is exactly the configuration
// `compaction.model` exists to enable: at 8K the update form is an eighth of
// the window, charged again on EVERY chunk of the carry loop, and at 2K it
// exceeds the whole budget and chunkBudgetFor returns negative — RunSummary
// then refuses with ErrNoWindowRoom and no compaction happens at all.
//
// An instruction that prevents the compaction it is asking for is worse than a
// shorter instruction that gets a slightly worse summary. So past this
// fraction the elaborated rules are dropped and the structural core is kept;
// see terseInstruction for what survives the cut and why those clauses and not
// others.
//
// 8 is chosen so the verbose form (about 1000 tokens at its largest) stays
// available down to a 8K window and switches below it, which is the point
// where budget.go's proportional output reserve is also getting tight.
const instructionBudgetDenominator = 8

// summaryInstruction is the final user turn appended to ask for the summary.
//
// It asks for the five fixed sections of SummarySections with [seq:lo-hi]
// source pointers, and — when hasPrior is set — for an INCREMENTAL UPDATE of
// the previous summary already present in the request rather than a fresh one.
//
// WHY THE SHAPE IS SPELLED OUT AT THIS LENGTH. The instruction this replaced
// was two sentences asking for "concise but comprehensive" prose, and its
// output degraded in a way that only appears on the SECOND compaction: prose
// has nowhere to put the difference between work that is still open and a
// decision that has been reversed, so on the re-summarize pass they merge and
// a withdrawn approach comes back out as the current plan. Every clause below
// is aimed at one failure of that kind, and none of them can be enforced from
// Go — a parser can check that five headings exist, it cannot check that a
// superseded decision was replaced rather than appended. Hence the split: the
// RULES live in the prompt, the STRUCTURE lives in the parser, and each
// enforces what it is able to.
//
// THE PRIOR SUMMARY IS NOT EMBEDDED HERE. Whenever there is one it is already
// in the request as a message — the carry loop puts it there as a
// sentinel-prefixed user turn, and across compactions Plan leaves the earlier
// summary in the summarize set, where it arrives as ordinary history. Copying
// it into the instruction as well would pay for the same text twice out of a
// budget this package spends real effort computing, and would put two copies
// in front of the model with nothing to say which is authoritative. hasPrior
// therefore switches the VERB (update vs produce) and nothing else.
//
// window is the model window this instruction has to fit inside; 0 means
// unbudgeted and always selects the full form. See instructionBudgetDenominator.
func summaryInstruction(wordLimit int, hasPrior bool, covered SeqRef, window int) string {
	if wordLimit <= 0 {
		wordLimit = defaultSummaryWordLimit
	}
	full := fullInstruction(wordLimit, hasPrior, covered)
	if window <= 0 {
		return full
	}
	if estimateTextTokens(full) <= window/instructionBudgetDenominator {
		return full
	}
	return terseInstruction(wordLimit, hasPrior)
}

// fullInstruction is the elaborated form: the five headings with per-section
// guidance, the source-pointer contract, and the rule list.
func fullInstruction(wordLimit int, hasPrior bool, covered SeqRef) string {
	var b strings.Builder

	if hasPrior {
		b.WriteString("A previous continuation summary appears above, marked " +
			strings.TrimSpace(SummarySentinel) + ". " +
			"UPDATE IT using the rest of the conversation and return ONE COMPLETE REPLACEMENT. " +
			"Treat it as the baseline:\n" +
			"- KEEP items that are still relevant and not contradicted.\n" +
			"- MERGE new facts into the section they belong to.\n" +
			"- REPLACE a superseded item with its current state, in the same section. Do not keep both.\n" +
			"- REMOVE an item only once it is explicitly superseded, completed, withdrawn, or obsolete.\n" +
			"- MOVE finished work out of " + SectionOpenWork + " and into " + SectionCurrentState + ".\n" +
			"Where the conversation contradicts the previous summary, the conversation wins. " +
			"Where a fact changed over time, the newer state wins. " +
			"Do NOT append a change log, and do not leave the old and new versions side by side.\n\n")
	} else {
		b.WriteString("Produce a continuation summary of the conversation above. " +
			"Extract the EFFECTIVE STATE of the task; do not narrate the conversation.\n\n")
	}

	b.WriteString("Return ordinary Markdown. No JSON, no tool call, no code fence. " +
		"Use exactly these headings, in this order, and add no others:\n\n")
	b.WriteString("## " + SectionActiveTask + "\none sentence naming the task currently in flight\n\n")
	b.WriteString("## " + SectionCurrentState + "\n- verified facts and progress so far\n\n")
	b.WriteString("## " + SectionConstraints + "\n- requirements and preferences still in force\n\n")
	b.WriteString("## " + SectionDecisions + "\n- decisions that are currently effective (replace superseded ones in place)\n\n")
	b.WriteString("## " + SectionOpenWork + "\n- outstanding work, blockers, and next actions\n\n")

	b.WriteString("Rules:\n")
	b.WriteString("- End every bullet with a source pointer such as " + SeqRef{Lo: 120, Hi: 134}.String() +
		" or " + SeqRef{Lo: 87}.String() + ", naming the message sequence numbers it came from. " +
		"These are real handles, not decoration: history_read(from_seq, to_seq) returns those messages verbatim, " +
		"so a later reader can recover the exact wording instead of trusting this summary's compression of it.\n")
	if covered.citable() {
		b.WriteString(fmt.Sprintf("- The messages being summarized cover sequence range %d-%d. "+
			"Cite ranges inside it; when the exact bounds are unclear, cite the whole range.\n", covered.Lo, covered.Hi))
	}
	b.WriteString("- PRESERVE VERBATIM: file paths, shell commands, error messages, function and symbol names, " +
		"version numbers, ports, and every other opaque identifier. Paraphrasing one makes it unusable.\n")
	b.WriteString("- Keep only what the conversation supports. Do not infer that something completed, " +
		"succeeded, or was decided. Distinguish verified from attempted from planned.\n")
	b.WriteString("- Each task or entity gets ONE current state. Before answering, reconcile " +
		SectionCurrentState + " against " + SectionOpenWork +
		": finished work must not also be listed as pending.\n")
	b.WriteString("- Tool output may be abbreviated when repetitive. Never copy a whole tool result; " +
		"keep the fact it established.\n")
	b.WriteString("- Never copy credentials, tokens, API keys, or connection strings. Describe them instead.\n")
	b.WriteString("- Write " + emptyMarker + " under a heading with nothing to say. Never leave one blank.\n")
	b.WriteString(fmt.Sprintf("- Keep the whole summary under %d words.\n", wordLimit))
	return b.String()
}

// terseInstruction is the form used when the full one would eat too much of
// the window. It keeps exactly what the rest of the system DEPENDS on and
// drops what merely improves the result:
//
//   - KEPT: the five headings in order, the empty marker, the source-pointer
//     form, and the update-vs-produce verb. The first two are what
//     ParseStructured requires — drop them and every summary fails to parse,
//     which fails compaction entirely. The pointer form is what makes the
//     summary an index into history rather than a replacement for it. The verb
//     is the whole of C4's incremental semantics.
//   - DROPPED: preserve-verbatim, no-inference, reconcile-the-sections,
//     no-secrets, and the per-section guidance. Each of these makes the
//     summary better and none of them makes it PARSE, so on a window too small
//     to hold both they are what a compaction can afford to lose. Dropping
//     no-secrets is safe in a way the others are not: the Redactor has already
//     stripped registered secrets from the input by the time this is composed,
//     so this clause was belt-and-braces to begin with (see redactForSummary).
func terseInstruction(wordLimit int, hasPrior bool) string {
	var b strings.Builder
	if hasPrior {
		b.WriteString("Update the summary above from this conversation; return one replacement, superseded items replaced in place.\n")
	} else {
		b.WriteString("Summarize the conversation above as task state, not narration.\n")
	}
	b.WriteString("Markdown, exactly these headings in order:\n")
	for _, name := range SummarySections {
		b.WriteString("## " + name + "\n")
	}
	b.WriteString(fmt.Sprintf("%s: one sentence. Others: \"- \" bullets, each ending %s citing its source messages. %s if empty. Under %d words.\n",
		SectionActiveTask, SeqRef{Lo: 12, Hi: 30}.String(), emptyMarker, wordLimit))
	return b.String()
}

// instructionMessage builds the final user turn that asks for the summary.
// Centralized so the single path, the carry path, and the token estimate all
// construct it identically (no drift between the budgeted size and the actual
// request).
func instructionMessage(wordLimit int, hasPrior bool, covered SeqRef, window int) *schema.Message {
	return &schema.Message{Role: schema.User, Content: summaryInstruction(wordLimit, hasPrior, covered, window)}
}

// containsPriorSummary reports whether any message in msgs is a compaction
// summary from an earlier pass.
//
// This is what makes the incremental-update instruction fire ACROSS
// compactions and not merely within one carry loop. Plan short-circuits only
// while a summary is the LAST message; a few turns later the same summary is
// ordinary history and lands in the summarize set, and that is exactly the
// second-compaction case where prose used to collapse open work and reversed
// decisions into each other. Detecting it here is what lets the model be told
// to update that document rather than write a fresh one over the top of it.
func containsPriorSummary(msgs []*schema.Message) bool {
	for _, m := range msgs {
		if IsSummaryMessage(m) {
			return true
		}
	}
	return false
}
