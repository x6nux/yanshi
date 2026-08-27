// internal/tools/memory_distill.go
//
// C13: the model-driven half of memory consolidation.
//
// store.ApplyDistillation is the transaction — it merges rows and cannot lose
// one. This file is what decides WHAT to merge: it takes the oldest current
// memories, asks a model to consolidate them, parses the answer, and applies
// the groups it can verify.
//
// EVERY FAILURE HERE ENDS WITH THE ORIGINALS INTACT AND CURRENT. No model, a
// timeout, an API error, an unparseable answer, an id the model invented, a
// group of one, an empty merged body — each returns without writing anything.
// That is the same rule C1 applies to eviction (do not evict what did not
// persist) and for the same reason: a distillation is the only operation in
// the system that can make a memory unreachable, so the cost of skipping one
// has to stay strictly below the cost of botching one. Skipping costs a model
// call. Botching costs the memories.
//
// THE MODEL IS THE COMPACTION MODEL. `compaction.model` already names a small
// cheap model for exactly this kind of work — read a pile of text, write a
// shorter faithful version — and adding a second knob would give operators two
// places to configure the same thing and one of them to get wrong.
package tools

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/store"
)

// DistillModel is the narrow contract the distiller needs: one Generate call.
//
// Declared here rather than taking model.BaseChatModel wholesale so the
// dependency is the one method actually used, and so a test can supply a
// scripted model without a provider. Any model.BaseChatModel satisfies it,
// including the compaction model this is meant to run on.
type DistillModel interface {
	Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error)
}

// MinDistillBatch is the smallest number of candidate memories worth a
// distillation pass. Below it there is nothing to consolidate that would not
// be better left alone, and a model call per new memory would cost more than
// the storage it saves.
const MinDistillBatch = 6

// DistillResult reports one pass.
type DistillResult struct {
	// Considered is how many current memories were shown to the model.
	Considered int
	// Merged is how many memories were superseded across all applied groups.
	Merged int
	// Groups is how many merged rows were written.
	Groups int
	// Skipped explains, one line per group, why a group the model proposed was
	// NOT applied. It is populated on the success path too: a pass that merged
	// two groups and rejected a third is the normal shape, and losing the
	// reason for the third would make a systematically bad prompt look like a
	// systematically quiet one.
	Skipped []string
}

// DistillPrompt is the instruction the consolidation model receives.
//
// The rules are in the prompt rather than in Go because none of them is
// mechanically checkable: whether two notes are about the same subject, and
// whether a later one overrode an earlier one, are judgements. What Go checks
// is the part it can — that the ids exist, that a group has at least two
// members, that no memory appears in two groups, that the body is non-empty —
// and everything ApplyDistillation refuses is left untouched.
//
// The output format is line-oriented rather than JSON because a small model
// producing JSON fails in a way that loses the whole batch (one unescaped
// quote), while a line format fails per line — and a per-line failure is a
// skipped group, which this file is built to survive.
const DistillPrompt = `You are consolidating an agent's long-term memory. Below are stored notes, oldest first, each with an id.

Merge notes that are about the SAME subject into one statement of the CURRENT position. Rules:
- Where a later note contradicts or updates an earlier one, keep only the later state. Do not record the history of the change.
- Preserve verbatim: file paths, commands, names, versions, numbers, and any other identifier. Paraphrasing one makes it useless.
- Do NOT merge notes about different subjects just because they are short.
- Do NOT invent anything that is not in the notes you are merging.
- Leave a note alone if it has nothing to merge with. Most notes should be left alone.

Answer with one merge per line, in exactly this form:

MERGE id1,id2,id3 :: the consolidated statement

Use only ids from the list. Each id may appear in at most one MERGE line. If nothing should be merged, answer exactly:

NOTHING

Notes:
`

// DistillMemories runs one consolidation pass and returns what it did.
//
// It is a NO-OP (nil error, zero-ish result) when there is no store, no model,
// or fewer than MinDistillBatch candidates. Those are ordinary states, not
// failures: a fresh session has few memories and a pass over them would merge
// things that merely happen to be adjacent.
//
// dims scopes both the candidate query and the merged row's tags, so a
// session-scoped pass consolidates that session's notes and writes the result
// back into the same scope.
func DistillMemories(ctx context.Context, s *store.Store, m DistillModel, dims store.MemoryFilter) (DistillResult, error) {
	var res DistillResult
	if s == nil || m == nil {
		return res, nil
	}
	candidates, err := s.DistillCandidates(store.MaxDistillInputs, dims)
	if err != nil {
		return res, fmt.Errorf("memory distillation: read candidates: %w", err)
	}
	if len(candidates) < MinDistillBatch {
		return res, nil
	}
	res.Considered = len(candidates)

	reply, err := m.Generate(ctx, []*schema.Message{{
		Role:    schema.User,
		Content: DistillPrompt + renderDistillCandidates(candidates),
	}})
	if err != nil {
		return res, fmt.Errorf("memory distillation: model call: %w", err)
	}
	if reply == nil {
		return res, fmt.Errorf("memory distillation: model returned no message")
	}

	byID := make(map[string]store.Memory, len(candidates))
	for _, c := range candidates {
		byID[c.ID] = c
	}
	groups, skipped := ParseDistillPlan(reply.Content, byID)
	res.Skipped = skipped

	for _, g := range groups {
		if _, err := s.ApplyDistillation(store.MemoryDistillation{
			SourceIDs: g.SourceIDs,
			Content:   g.Content,
			Dims:      dims,
		}); err != nil {
			// One group failing does not abort the pass: ApplyDistillation is
			// all-or-nothing per group, so the sources of a failed group are
			// exactly as they were. Reporting and continuing merges what can
			// be merged rather than discarding a good group because a later
			// one raced.
			res.Skipped = append(res.Skipped, err.Error())
			continue
		}
		res.Groups++
		res.Merged += len(g.SourceIDs)
	}
	return res, nil
}

// DistillGroup is one verified merge proposal.
type DistillGroup struct {
	SourceIDs []string
	Content   string
}

// distillLineRe matches a MERGE line: ids, then "::", then the merged text.
//
// The text group is `(.*)` rather than `(.+)` so a line that proposes a merge
// and supplies NO body still matches. The difference is not cosmetic: with
// `(.+)` such a line falls through to the "prose the model wrapped around its
// answer" branch and is dropped in silence, so a model that systematically
// emits empty bodies produces a pass that merges nothing and explains nothing.
// Matching it here routes it to the empty-text rejection, which is reported.
var distillLineRe = regexp.MustCompile(`(?i)^\s*MERGE\s+([^:]+?)\s*::\s*(.*)$`)

// ParseDistillPlan turns the model's answer into verified groups, plus a
// human-readable reason for every proposal it refused.
//
// known is the candidate set the model was shown. A group is REJECTED, not
// repaired, when it names an id that is not in that set — a hallucinated id
// means the line as a whole is not describing the memories it claims to
// describe, and merging the subset that happens to exist would apply a
// judgement the model did not make. Same for an id claimed twice: the second
// claim contradicts the first, and there is no way to tell which was intended.
func ParseDistillPlan(answer string, known map[string]store.Memory) ([]DistillGroup, []string) {
	var groups []DistillGroup
	var skipped []string
	claimed := map[string]bool{}

	for _, line := range strings.Split(answer, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.EqualFold(trimmed, "NOTHING") {
			continue
		}
		mt := distillLineRe.FindStringSubmatch(trimmed)
		if mt == nil {
			continue // prose the model wrapped around its answer
		}
		content := strings.TrimSpace(mt[2])
		var ids []string
		bad := ""
		for _, raw := range strings.Split(mt[1], ",") {
			id := strings.TrimSpace(raw)
			if id == "" {
				continue
			}
			if _, ok := known[id]; !ok {
				bad = fmt.Sprintf("unknown memory id %q", id)
				break
			}
			if claimed[id] {
				bad = fmt.Sprintf("memory %q claimed by an earlier merge", id)
				break
			}
			ids = append(ids, id)
		}
		switch {
		case bad != "":
			skipped = append(skipped, bad+" in "+firstChars(trimmed, 80))
			continue
		case len(ids) < 2:
			skipped = append(skipped, "merge of "+fmt.Sprint(len(ids))+" memory in "+firstChars(trimmed, 80))
			continue
		case content == "":
			skipped = append(skipped, "empty merged text in "+firstChars(trimmed, 80))
			continue
		}
		for _, id := range ids {
			claimed[id] = true
		}
		groups = append(groups, DistillGroup{SourceIDs: ids, Content: content})
	}
	return groups, skipped
}

// distillCandidateChars bounds one candidate's body in the prompt. A single
// enormous memory would otherwise crowd out every other candidate in the
// batch, which is how a consolidation pass ends up merging nothing while
// costing a full-window model call.
const distillCandidateChars = 600

// renderDistillCandidates lists the batch for the model, id first so the
// answer format is a copy rather than a construction.
func renderDistillCandidates(ms []store.Memory) string {
	var b strings.Builder
	for _, m := range ms {
		fmt.Fprintf(&b, "\n[%s] (%s) %s\n", m.ID, m.Kind,
			firstChars(strings.ReplaceAll(strings.TrimSpace(m.Content), "\n", " "), distillCandidateChars))
	}
	return b.String()
}

// firstChars caps s at n characters, marking a cut so a truncated line cannot
// be mistaken for a complete one.
func firstChars(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
