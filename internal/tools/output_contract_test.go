package tools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/tools"
)

const fullFiveSectionResult = `SUMMARY:
Refactored the parser into two passes.

CHANGES:
- internal/parse/lexer.go: split scan() from emit()
- internal/parse/parser.go: consume tokens from the new lexer

EVIDENCE:
internal/parse/lexer_test.go:40 covers the split
internal/parse/parser.go:118 is the new entry point

RISKS:
The two-pass form allocates one extra slice per file.

BLOCKERS:
None.
`

// TestParseResultSectionsReturnsAllFive covers the output-contract clause.
//
// knownResultSections had exactly one consumer — matchResultSection, reached
// only from the EVIDENCE extractor — so four of the five sections had no reader
// anywhere. "The five-section output is parseable" was therefore true of the
// vocabulary and of nothing else: a sub-agent could omit four sections and no
// code path would notice. The breakdown named the cheap version:
// Contains(prefix, "BLOCKERS") asserts the PROMPT mentions the word.
//
// ledger: B1/M04b#3 输出 5 段可解析
func TestParseResultSectionsReturnsAllFive(t *testing.T) {
	got := tools.ParseResultSections(fullFiveSectionResult)

	require.True(t, got.Complete(), "sections missing from a complete result: %v", got.Missing)

	assert.Contains(t, got.Summary, "two passes")
	assert.Contains(t, got.Changes, "internal/parse/lexer.go")
	assert.Contains(t, got.Evidence, "lexer_test.go:40")
	assert.Contains(t, got.Risks, "extra slice")
	assert.Contains(t, got.Blockers, "None")

	// Each section stops at the next header. A parser that ran to the end of
	// the text would put every later section inside the first one, and the
	// per-section assertions above would all still pass.
	assert.NotContains(t, got.Summary, "CHANGES")
	assert.NotContains(t, got.Summary, "internal/parse/lexer.go")
	assert.NotContains(t, got.Changes, "EVIDENCE")
	assert.NotContains(t, got.Risks, "BLOCKERS")

	// Get is the same data by name; a divergence here means two sources of
	// truth for one parse.
	for name, want := range map[string]string{
		"SUMMARY": got.Summary, "CHANGES": got.Changes, "EVIDENCE": got.Evidence,
		"RISKS": got.Risks, "BLOCKERS": got.Blockers,
	} {
		assert.Equal(t, want, got.Get(name), "Get(%q) disagrees with the field", name)
	}
	assert.Empty(t, got.Get("NOT_A_SECTION"))
}

// TestParseResultSectionsNamesWhatIsMissing is what makes the parse result
// usable.
//
// A caller reading Evidence cannot otherwise tell an empty section from an
// absent one, and that difference decides whether to ask the sub-agent again.
//
// ledger: B1/M04b#3 输出 5 段可解析
func TestParseResultSectionsNamesWhatIsMissing(t *testing.T) {
	partial := "SUMMARY:\nDid the thing.\n\nEVIDENCE:\nfile.go:1\n"
	got := tools.ParseResultSections(partial)

	assert.False(t, got.Complete())
	assert.ElementsMatch(t, []string{"CHANGES", "RISKS", "BLOCKERS"}, got.Missing)
	assert.Contains(t, got.Summary, "Did the thing")
	assert.Contains(t, got.Evidence, "file.go:1")

	// Out of order is not an error: a model that answers BLOCKERS first has
	// still answered, and rejecting that turns a cosmetic deviation into a
	// failed turn.
	reordered := "BLOCKERS:\nnone\n\nSUMMARY:\ns\n\nCHANGES:\nc\n\nEVIDENCE:\ne\n\nRISKS:\nr\n"
	out := tools.ParseResultSections(reordered)
	assert.True(t, out.Complete(), "a reordered result was rejected: %v", out.Missing)
	assert.Equal(t, "none", out.Blockers)
	assert.Equal(t, "s", out.Summary)
}

// TestParentHintReportsAPartialContract is the production consumer of the
// parse.
//
// A five-section parser with no caller is the same defect it was written to
// fix, one level up: knownResultSections had exactly one consumer, and adding
// ParseResultSections without wiring it would have left the new four sections
// just as unread as the old ones. ParentWorkingSetHint is the call site — it
// runs on both terminal paths (agent_start and analysis), so a sub-agent that
// answered in the contract format but skipped sections now says so to the
// parent rather than passing off a partial answer as a whole one.
//
// The pass-through case is the boundary: free-form results have no sections at
// all, and appending a complaint to those would corrupt ordinary tool output.
//
// ledger: B1/M04b#3 输出 5 段可解析
func TestParentHintReportsAPartialContract(t *testing.T) {
	partial := "SUMMARY:\nDid the thing.\n\nEVIDENCE:\nfile.go:1\n"
	got := tools.ParentWorkingSetHint(partial)

	assert.Contains(t, got, "file.go:1", "the EVIDENCE hint is gone")
	assert.Contains(t, got, "CHANGES", "the parent is not told which sections are missing")
	assert.Contains(t, got, "RISKS")
	assert.Contains(t, got, "BLOCKERS")
	assert.NotContains(t, got, "omitted: SUMMARY",
		"a section that WAS supplied is reported as missing")

	// Complete results carry no complaint: a note on every result is a note
	// the parent learns to ignore.
	full := tools.ParentWorkingSetHint(fullFiveSectionResult)
	assert.NotContains(t, full, "contract note",
		"a complete five-section result was reported as incomplete")
	assert.Contains(t, full, "lexer_test.go:40")

	// No sections at all — free-form output — is passed through untouched.
	freeform := "just some prose about what happened, no headers anywhere"
	assert.Equal(t, freeform, tools.ParentWorkingSetHint(freeform),
		"free-form tool output was rewritten")
}

// TestAgentStartAsksForTheOutputContract closes the seam.
//
// The five-section contract lives in RoleDef.PromptPrefix, and the prefix was
// applied only in managedTurnRunner — the agent_spawn path, which
// DefaultOrchestratorProfile does not permit. On the entry point that ships, no
// sub-agent was ever ASKED for the sections, so the parser above would have had
// nothing to parse and ParentWorkingSetHint was a no-op in production for want
// of an input.
//
// The runner records the instruction it was handed, which is what the sub-agent
// turn actually receives.
//
// ledger: B1/M04b#3 输出 5 段可解析
func TestAgentStartAsksForTheOutputContract(t *testing.T) {
	at := tools.NewAgentTools(&einollm.FakeModel{Echo: true})
	ctx := tools.WithProfile(context.Background(), allowAll("*"))

	var handed string
	ctx = tools.WithSubAgentRunner(ctx, tools.SubAgentRunner(
		func(ic context.Context, prompt string, allowed []string, instr string) (string, error) {
			handed = instr
			return "done", nil
		}))

	_, err := at.StartAgent.InvokableRun(ctx, `{"prompt":"look around","role":"explore"}`)
	require.NoError(t, err)

	require.NotEmpty(t, handed,
		"agent_start handed the sub-agent no instruction, so the output contract never "+
			"reached it and the five sections were never requested")
	for _, section := range []string{"SUMMARY", "CHANGES", "EVIDENCE", "RISKS", "BLOCKERS"} {
		assert.Contains(t, handed, section,
			"the instruction does not ask for the %s section", section)
	}

	// A caller-supplied instruction must be kept, not replaced: overriding the
	// system prompt is what that parameter is for.
	handed = ""
	_, err = at.StartAgent.InvokableRun(ctx,
		`{"prompt":"x","role":"explore","instruction":"CALLER_INSTRUCTION_MARKER"}`)
	require.NoError(t, err)
	assert.Contains(t, handed, "CALLER_INSTRUCTION_MARKER",
		"the caller's instruction was dropped when the contract prefix was added")
	assert.Contains(t, handed, "EVIDENCE",
		"the contract was dropped when the caller supplied an instruction")
	assert.True(t, strings.Index(handed, "EVIDENCE") < strings.Index(handed, "CALLER_INSTRUCTION_MARKER"),
		"the contract must come first so the caller's instruction can refine it")
}
