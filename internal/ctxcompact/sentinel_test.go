package ctxcompact

import (
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
)

func TestIsSummaryMessage_DetectsSentinel(t *testing.T) {
	m := &schema.Message{Role: schema.User, Content: SummarySentinel + "对话摘要..."}
	assert.True(t, IsSummaryMessage(m), "sentinel-prefixed user msg is a summary")
}

func TestIsSummaryMessage_RejectsPlain(t *testing.T) {
	assert.False(t, IsSummaryMessage(&schema.Message{Role: schema.User, Content: "普通消息"}))
	assert.False(t, IsSummaryMessage(nil))
}

func TestIsSummaryMessage_LastMessageIsSummary(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "hi"},
		{Role: schema.User, Content: SummarySentinel + "sum"},
	}
	assert.True(t, lastMessageIsSummary(msgs), "history ending in summary is already compacted")
	assert.False(t, lastMessageIsSummary(msgs[:1]))
	assert.False(t, lastMessageIsSummary(nil))
}
