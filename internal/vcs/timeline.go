// internal/vcs/timeline.go
//
// V3: make a snapshot list something a human can navigate.
//
// # The problem with ids
//
// ListSeams returns rows keyed by a 24-hex-character id. Nobody recognises the
// moment they want to go back to from `9f2c1a04e7b3...`. The operator's memory
// of a session is not indexed by commit — it is indexed by what they ASKED.
// "Before I told it to rename the config loader" is a real memory; "commit
// 9f2c1a04" is not.
//
// QwenPaw's checkpoint timeline is built on exactly that observation: its
// render_timeline table has a Query column carrying the user's prompt for each
// checkpoint, and it is the column that makes the table usable. This file is
// the same idea over this repo's data model.
//
// # How a seam is joined to a question
//
// The join is ORDINAL, not temporal: the Nth turn of a session corresponds to
// the Nth user message in that session's durable log.
//
// Time would be the obvious key and is the wrong one. A pre-turn seam is sealed
// the instant the user message enters the live window, but that message does
// not become DURABLE until the turn ends (persistMessages runs after the model
// does). "The newest user message at or before this seam's timestamp" therefore
// resolves a pre-turn seam to the PREVIOUS question — the one failure mode that
// makes the whole feature worse than useless, because it is wrong in a way that
// reads as right.
//
// The ordinal join survives the things that break naive joins:
//
//   - COMPACTION. The live window shrinks; the durable log does not.
//     store.AppendMessages is append-only and deduplicated, and the WS layer
//     flushes the entire window before every eviction, so message ordinals are
//     stable even though HistoryLen (a live-window length) is not. This is why
//     the join does not use HistoryLen despite it being right there on the row.
//   - REVERTS. TruncateSessionForRevert deletes messages at and after the
//     restored boundary, so ordinals renumber from there — which is the
//     intended semantics, since the turns after that boundary no longer exist.
//
// Where TurnSeq is measured differs by seam kind (pre-turn is sealed before the
// turn counter increments, post-turn after), so turnOrdinal normalises it.
//
// # Honest degradation
//
// A seam with no session (an agent-initiated revert stores session_id="") and
// a seam whose ordinal falls outside the durable log both yield an entry with
// an empty Question. They are still listed — a snapshot the operator cannot
// label is still a snapshot they may need — and the caller can render the fall
// back it prefers. Nothing here invents a question.

package vcs

import (
	"fmt"
	"strings"

	"github.com/x6nux/yanshi/internal/store"
)

// TimelineEntry is one navigable moment in a repository's history.
type TimelineEntry struct {
	SeamID   string
	Kind     SeamKind
	CommitID string
	// Question is the first line of the user message that opened this turn, or
	// "" when it cannot be resolved (no session, or the turn predates the
	// durable log). It is never synthesised.
	Question string
	// QuestionTruncated reports that Question was cut to fit the preview width,
	// so a renderer can append an ellipsis without guessing.
	QuestionTruncated bool
	// TurnSeq is the 1-based turn number this seam belongs to, normalised
	// across seam kinds. 0 means the seam carries no usable turn number.
	TurnSeq int
	// FilesChanged is how many paths the seam's commit changed versus its
	// parent — the "how big was this step" column.
	FilesChanged int
	CreatedAt    int64
	// IsHead marks the seam whose commit is the repository's current main_head,
	// i.e. where the working copy stands right now.
	IsHead bool
	// SessionID scopes the entry; empty for VCS-only (agent) seams.
	SessionID string
}

// TimelineOptions bounds a Timeline query.
type TimelineOptions struct {
	// SessionID restricts the timeline to one chat session. Empty lists every
	// seam of the repo, which is what a cross-session operator view wants.
	SessionID string
	// Limit caps the number of entries, newest first. <= 0 uses
	// DefaultTimelineLimit.
	Limit int
	// QuestionPreviewChars caps Question's length. <= 0 uses
	// DefaultQuestionPreviewChars.
	QuestionPreviewChars int
	// IncludeRevertSeams adds pre-revert / post-revert audit seams. They are
	// excluded by default: they describe a ROLLBACK rather than a question, so
	// mixing them into the "what did I ask" list is noise for the common case,
	// while an operator auditing a rollback specifically wants them.
	IncludeRevertSeams bool
}

// DefaultTimelineLimit bounds an unspecified TimelineOptions.Limit.
const DefaultTimelineLimit = 30

// DefaultQuestionPreviewChars bounds an unspecified preview width. It is a rune
// count, not a byte count, so a CJK prompt is not cut mid-character.
const DefaultQuestionPreviewChars = 100

// maxQuestionPages bounds how far Timeline pages back through the durable log
// looking for user messages. A turn produces one user message and arbitrarily
// many tool rows, so the number of pages needed is data-dependent; without a
// bound a session with enormous tool output would make the timeline walk the
// entire log. At store.MaxMessagePageSize per page this covers sessions with
// tens of thousands of rows, which is far past what any timeline shows.
const maxQuestionPages = 24

// Timeline returns the seams of repoID as human-navigable entries, newest
// first.
func (v *VCS) Timeline(repoID string, opts TimelineOptions) ([]TimelineEntry, error) {
	if opts.Limit <= 0 {
		opts.Limit = DefaultTimelineLimit
	}
	if opts.QuestionPreviewChars <= 0 {
		opts.QuestionPreviewChars = DefaultQuestionPreviewChars
	}
	// Over-fetch: the revert seams filtered out below would otherwise shorten
	// the result silently, so a session with many rollbacks would show fewer
	// entries than asked for with no way to tell why.
	fetch := opts.Limit
	if !opts.IncludeRevertSeams {
		fetch *= 3
	}
	seams, err := v.ListSeams(repoID, opts.SessionID, fetch)
	if err != nil {
		return nil, fmt.Errorf("vcs: timeline: list seams: %w", err)
	}
	head, err := v.RepoMainHead(repoID)
	if err != nil {
		return nil, fmt.Errorf("vcs: timeline: read head: %w", err)
	}

	questions := map[string][]string{}
	out := make([]TimelineEntry, 0, opts.Limit)
	for _, s := range seams {
		if len(out) >= opts.Limit {
			break
		}
		if !opts.IncludeRevertSeams && isRevertSeam(s.Kind) {
			continue
		}
		e := TimelineEntry{
			SeamID:       s.ID,
			Kind:         s.Kind,
			CommitID:     s.CommitID,
			TurnSeq:      turnOrdinal(s),
			FilesChanged: v.changedCount(s.CommitID, ""),
			CreatedAt:    s.CreatedAt,
			IsHead:       s.CommitID != "" && s.CommitID == head,
			SessionID:    s.SessionID,
		}
		if s.SessionID != "" && e.TurnSeq > 0 {
			list, ok := questions[s.SessionID]
			if !ok {
				list, err = v.userQuestions(s.SessionID)
				if err != nil {
					return nil, err
				}
				questions[s.SessionID] = list
			}
			if e.TurnSeq <= len(list) {
				e.Question, e.QuestionTruncated =
					previewLine(list[e.TurnSeq-1], opts.QuestionPreviewChars)
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// isRevertSeam reports whether a kind describes a rollback rather than a turn.
func isRevertSeam(k SeamKind) bool {
	return k == SeamPreRevert || k == SeamPostRevert
}

// turnOrdinal normalises a seam's TurnSeq into a 1-based turn number.
//
// The two turn kinds measure the same counter at different instants:
// SealMainTurnSeam is called with cs.turns BEFORE the increment for pre-turn
// and AFTER it for post-turn, so a pre-turn seam of the first turn carries 0
// and its post-turn twin carries 1. Both describe turn 1.
//
// Revert seams copy the TARGET seam's turn_seq (see RevertToSeam), which is
// already a post-turn-shaped value, so they take the same branch.
func turnOrdinal(s Seam) int {
	if s.Kind == SeamPreTurn {
		return s.TurnSeq + 1
	}
	return s.TurnSeq
}

// userQuestions returns the session's user messages in conversation order.
//
// It pages through store.MessagesPage rather than reading the whole log,
// because the durable log is deliberately allowed to be larger than any context
// window (see internal/store/message_log.go) and a timeline must not be the
// thing that loads all of it into memory.
func (v *VCS) userQuestions(sessionID string) ([]string, error) {
	var out []string
	from := 0
	for page := 0; page < maxQuestionPages; page++ {
		msgs, err := v.store.MessagesPage(store.MessageRange{
			SessionID: sessionID,
			FromSeq:   from,
			Limit:     store.MaxMessagePageSize,
		})
		if err != nil {
			return nil, fmt.Errorf("vcs: timeline: read session %s: %w", sessionID, err)
		}
		if len(msgs) == 0 {
			break
		}
		for _, m := range msgs {
			if m.Role == store.RoleUser {
				out = append(out, m.Content)
			}
		}
		next := msgs[len(msgs)-1].Seq + 1
		if next <= from {
			// Defensive: a non-advancing watermark would spin. Seq is assigned
			// monotonically inside store's write transaction, so this cannot
			// happen with a healthy log — but a timeline is a read-only view
			// and must not be able to hang on damaged data.
			break
		}
		from = next
		if len(msgs) < store.MaxMessagePageSize {
			break
		}
	}
	return out, nil
}

// previewLine reduces a whole user message to its first line, collapsed to
// single spaces and cut to at most maxRunes runes.
//
// First line rather than first sentence: a prompt's opening line is what the
// author wrote as the headline, whereas sentence splitting on arbitrary user
// text (paths, code, CJK with no spaces) has no correct implementation. The cut
// counts RUNES because a byte cut would split a multi-byte character and emit
// a replacement glyph in every UI that renders it.
func previewLine(s string, maxRunes int) (string, bool) {
	line := s
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(strings.Join(strings.Fields(line), " "))
	runes := []rune(line)
	if len(runes) <= maxRunes {
		return line, false
	}
	return strings.TrimRight(string(runes[:maxRunes]), " "), true
}
