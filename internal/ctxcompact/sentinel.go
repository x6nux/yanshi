package ctxcompact

import (
	"github.com/cloudwego/eino/schema"
)

// SummarySentinel prefixes every compaction-produced summary message. It lets
// Plan short-circuit when history already ends in a summary (preventing
// summary-of-summary, bug⑦) and lets the TUI skip rendering the summary as a
// chat bubble (it's model context, not conversational content). The bracketed
// form is deliberately distinctive so normal user text never collides.
//
// It is DERIVED from KindSummary rather than spelled out, so the constant and
// the kind cannot drift apart; the value is byte-identical to the literal it
// replaced, which internal/cli/tui still reads by name. See fragment.go for why
// the two kinds share one form and never one name.
const SummarySentinel = fragmentOpen + string(KindSummary) + fragmentClose

// IsSummaryMessage reports whether m is a compaction-produced summary (a user
// message prefixed with SummarySentinel). nil-safe.
func IsSummaryMessage(m *schema.Message) bool {
	return hasFragmentKind(m, KindSummary)
}

// lastMessageIsSummary reports whether msgs ends in a compaction summary — the
// signal Plan uses to short-circuit (history was just compacted).
func lastMessageIsSummary(msgs []*schema.Message) bool {
	if len(msgs) == 0 {
		return false
	}
	return IsSummaryMessage(msgs[len(msgs)-1])
}
