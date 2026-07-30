package ctxcompact

import (
	"strings"

	"github.com/cloudwego/eino/schema"
)

// SummarySentinel prefixes every compaction-produced summary message. It lets
// Plan short-circuit when history already ends in a summary (preventing
// summary-of-summary, bug⑦) and lets the TUI skip rendering the summary as a
// chat bubble (it's model context, not conversational content). The bracketed
// form is deliberately distinctive so normal user text never collides.
const SummarySentinel = "[yanshi:conversation-summary]\n"

// IsSummaryMessage reports whether m is a compaction-produced summary (a user
// message prefixed with SummarySentinel). nil-safe.
func IsSummaryMessage(m *schema.Message) bool {
	return m != nil && m.Role == schema.User && strings.HasPrefix(m.Content, SummarySentinel)
}

// lastMessageIsSummary reports whether msgs ends in a compaction summary — the
// signal Plan uses to short-circuit (history was just compacted).
func lastMessageIsSummary(msgs []*schema.Message) bool {
	if len(msgs) == 0 {
		return false
	}
	return IsSummaryMessage(msgs[len(msgs)-1])
}
