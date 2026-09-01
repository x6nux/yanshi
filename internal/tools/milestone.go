// internal/tools/milestone.go
//
// C7 path (a): the tool that lets the model label its own work as it goes.
//
// The eviction map (C3) is a directory of spans, and a directory is only worth
// reading if the entries have names. Two things can supply them. The summary
// harvest (ctxcompact.Milestones) is free and cannot be skipped, and it is the
// primary path — but it only labels what the summary chose to mention, and it
// only runs at compaction time, which is after the work happened and after the
// details the label should have captured are hardest to recover.
//
// milestone_set closes that gap: the model records "what I just finished" at
// the moment it finishes, in its own words, with the seq the note lands at.
//
// WHY THIS IS A DURABLE MESSAGE AND NOT A SIDE TABLE. A milestone written into
// the conversation log is indexed by FTS (history_search finds it), addressed
// by seq (history_read returns it), carried by session fork, and deleted by
// session deletion — all of which a side table would have to reimplement, and
// three of which it would quietly get wrong. It is stored under its own role
// so a reader can tell an agent's self-annotation from its prose.
package tools

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/store"
)

// milestoneSeed randomizes the dedup-key namespace per process lifetime, and
// milestoneNonce serializes calls within it. Together they guarantee key
// uniqueness without relying on wall-clock granularity, which differs per
// platform (see the nonce comment in runSet).
var (
	milestoneSeed  = randomSeed()
	milestoneNonce atomic.Uint64
)

// randomSeed draws the per-process nonce namespace from crypto/rand, falling
// back to a constant (still unique within the process thanks to the counter)
// if the CSPRNG is unavailable — never panics on a path a tool call takes.
func randomSeed() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	return binary.LittleEndian.Uint64(b[:])
}

// MilestoneTools exposes milestone_set as a GuardedTool.
type MilestoneTools struct {
	store *store.Store
	// Set records a labelled milestone against the current conversation.
	Set *GuardedTool
}

// MaxMilestoneText bounds one milestone label, in runes.
//
// It matches ctxcompact.MaxMilestoneHeadline deliberately but is declared
// separately rather than imported: this package must not depend on ctxcompact
// (the dependency runs the other way — ctxcompact is a service-layer package
// this one would create a cycle with through the orchestrator). A label longer
// than the map can carry would be truncated there anyway, so rejecting it here
// tells the model at write time instead of silently shortening it at read time.
const MaxMilestoneText = 160

// NewMilestoneTools builds the milestone annotation tool backed by s.
func NewMilestoneTools(s *store.Store) *MilestoneTools {
	mt := &MilestoneTools{store: s}
	mt.Set = NewGuardedTool(
		"milestone_set", "Milestone",
		"Record a one-line label for the work you just finished, so it stays findable "+
			"after this part of the conversation is dropped from your context window. "+
			"Write it for your future self: what was established, decided, tried and "+
			"ruled out, or broken — not what you are about to do. Good: \"fixed the nil "+
			"deref in lexShellLite; guard tests green\". Bad: \"working on tests\". "+
			"Call it after finishing a step, not on every message.",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"text": {Type: schema.String, Required: true,
				Desc: fmt.Sprintf("the milestone label, one line, at most %d characters", MaxMilestoneText)},
		}),
		SyncStream(mt.runSet),
	)
	return mt
}

// Tools returns all milestone tools as a slice for convenience.
func (m *MilestoneTools) Tools() []*GuardedTool { return []*GuardedTool{m.Set} }

// RoleMilestone is the durable-log role a milestone note is stored under.
//
// A distinct role rather than an assistant message, so a reader can separate
// the agent's index entries from its prose — and so a future consumer can
// select them without a content heuristic, which is how "find the milestones"
// turns into "find the lines that look like milestones".
const RoleMilestone = "milestone"

type milestoneArgs struct {
	Text string `json:"text"`
}

func (m *MilestoneTools) runSet(ctx context.Context, argsJSON string) (string, error) {
	var a milestoneArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	text := strings.Join(strings.Fields(strings.ReplaceAll(a.Text, "\n", " ")), " ")
	if text == "" {
		return "", fmt.Errorf("milestone_set: text is required")
	}
	if n := len([]rune(text)); n > MaxMilestoneText {
		// Rejected rather than truncated: the model can say the same thing
		// shorter, and a silently shortened label is one whose end — usually
		// the outcome — is missing without anyone being told.
		return "", fmt.Errorf("milestone_set: text is %d characters; keep it under %d "+
			"(it is an index label, not a summary — the details stay in the conversation)",
			n, MaxMilestoneText)
	}
	sessionID, err := historySessionID(ctx)
	if err != nil {
		return "", err
	}
	// AN EXPLICIT DEDUP KEY, and it carries a nonce.
	//
	// AppendMessages exists mainly for the WS layer, which re-flushes the whole
	// live window before every eviction and relies on content-derived keys to
	// make that idempotent. A milestone is the opposite shape: it is a single
	// direct append, and recording the same label twice at different points is
	// two real events, not a double flush.
	//
	// Left to the content-derived key, the second call inserts nothing and
	// AppendMessages returns the watermark unchanged — so `nextSeq-1` would
	// name whichever message happens to sit there, which after any intervening
	// message is a DIFFERENT one. The tool would then hand the model a seq
	// pointing at somebody else's text and invite it to cite that. Measured:
	// three identical appends produced one row and a watermark that never
	// moved.
	// The nonce must be unique per call even when two calls land in the same
	// clock tick: time.Now().UnixNano() has platform-dependent granularity
	// (Windows is coarse, milliseconds), and a collided key makes AppendMessages
	// swallow the second insert via ON CONFLICT — the caller then sees
	// "duplicate of an existing entry" for two genuinely distinct events
	// (2026-09-01 windows CI, TestMilestoneSet_RepeatedIdenticalTextGetsDistinctAddresses).
	// A random 64-bit seed makes the counter unique per process lifetime; the
	// counter makes it unique across same-tick calls.
	key := fmt.Sprintf("milestone:%d:%d:%s", milestoneSeed, milestoneNonce.Add(1), text)
	inserted, nextSeq, err := m.store.AppendMessages(sessionID, []store.Message{
		{Role: RoleMilestone, Content: text, DedupKey: key},
	})
	if err != nil {
		return "", err
	}
	if inserted != 1 {
		// Only reachable on a nanosecond-identical collision. Reporting it
		// beats returning a seq that names a row this call did not write.
		return "", fmt.Errorf("milestone_set: the note was not recorded (duplicate of an existing entry); retry")
	}
	// The seq is reported back because it is the ADDRESS: the model can cite it
	// in a summary as [seq:N], and history_read(N, N+1) returns this note.
	// Returning only "ok" would store a pointer the model never learns.
	seq := nextSeq - 1
	return fmt.Sprintf("Milestone recorded at seq %d. Cite it as [seq:%d].", seq, seq), nil
}
