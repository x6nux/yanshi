package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
)

// statsEntry renders a per-session token-consumption histogram with USD cost.
//
// "with USD cost" was a false statement until W7: render never read CostUSD,
// while the data was carried intact all the way from store through proto to
// here. The whole pricing chain existed and was dropped on the last hop.
//
// "sessions" reply (same NewSessionList fetch as /sessions — SessionInfo already
// carries TokensIn/TokensOut). Bars are scaled to the largest consumer so the
// relative cost of each session reads at a glance.
type statsEntry struct {
	sessions []proto.SessionInfo
}

const statsBarWidth = 20

func (e *statsEntry) render(_ int, _ spinner.Model) string {
	var b strings.Builder
	b.WriteString("  " + toolName.Render("stats") + toolMeta.Render("  · per-session token consumption") + "\n\n")

	// Keep only sessions that burned tokens; rank by total descending so the
	// biggest consumers top the chart.
	type row struct {
		label     string
		total     int
		when      time.Time
		cost      float64
		costKnown bool
	}
	var rows []row
	sum := 0
	costSum := 0.0
	unpriced := 0
	for _, s := range e.sessions {
		total := s.TokensIn + s.TokensOut
		if total <= 0 {
			continue
		}
		label := s.Title
		if label == "" {
			label = "(untitled)"
		}
		rows = append(rows, row{label, total, time.Unix(s.UpdatedAt, 0), s.CostUSD, s.CostKnown})
		sum += total
		if s.CostKnown {
			costSum += s.CostUSD
		} else {
			unpriced++
		}
	}
	if len(rows) == 0 {
		b.WriteString("    " + warnStyle.Render("(no token usage recorded yet — send a message first)") + "\n\n")
		return b.String()
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].total > rows[j].total })
	if len(rows) > 15 {
		rows = rows[:15]
	}
	maxTotal := rows[0].total // sorted desc
	for _, r := range rows {
		barLen := 0
		if maxTotal > 0 {
			barLen = (r.total*statsBarWidth + maxTotal - 1) / maxTotal // ceil division
		}
		bar := okStyle.Render(strings.Repeat("█", barLen)) +
			strings.Repeat("░", statsBarWidth-barLen)
		// Right-align the token count in a 7-cell field so the numbers line up.
		count := fmt.Sprintf("%7s", formatTokens(r.total))
		cost := fmt.Sprintf("%9s", einollm.FormatCost(r.cost, r.costKnown))
		b.WriteString(fmt.Sprintf("    %s %s %s  %s\n", bar, toolMeta.Render(count), toolMeta.Render(cost), r.label))
	}
	avg := 0
	if len(rows) > 0 {
		avg = sum / len(rows)
	}
	// The unpriced count is not decoration. A total that silently omits the
	// sessions with no price table entry reads as the whole bill, and the
	// gap is invisible: the default price table covers claude-* only, so a
	// user on any other provider sees a plausible-looking number that is
	// missing every one of their turns.
	summary := fmt.Sprintf("%d sessions · total %s · avg %s · cost %s",
		len(rows), formatTokens(sum), formatTokens(avg), einollm.FormatCost(costSum, true))
	if unpriced > 0 {
		summary += fmt.Sprintf(" (%d unknown)", unpriced)
	}
	b.WriteString(fmt.Sprintf("\n    %s  %s\n", toolMeta.Render("summary"), summary))
	b.WriteString("\n")
	return b.String()
}
