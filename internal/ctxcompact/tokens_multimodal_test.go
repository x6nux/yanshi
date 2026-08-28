package ctxcompact

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"
)

// imagePart builds a user input image part carrying a base64 data URL of the
// requested payload size, mirroring what
// orchestrator.appendImageParts writes in production.
func imagePart(detail schema.ImageURLDetail, payloadBytes int) schema.MessageInputPart {
	url := "data:image/png;base64," + strings.Repeat("A", payloadBytes)
	return schema.MessageInputPart{
		Type: schema.ChatMessagePartTypeImageURL,
		Image: &schema.MessageInputImage{
			MessagePartCommon: schema.MessagePartCommon{MIMEType: "image/png", URL: &url},
			Detail:            detail,
		},
	}
}

// ledger: A2/W-A-01#1 带图片的消息其 token 估算随图片数量单调增长
func TestEstimateTokensGrowsWithImageCount(t *testing.T) {
	base := EstimateTokens([]*schema.Message{{Role: schema.User}})
	one := EstimateTokens([]*schema.Message{{
		Role:                  schema.User,
		UserInputMultiContent: []schema.MessageInputPart{imagePart(schema.ImageURLDetailHigh, 1024)},
	}})
	two := EstimateTokens([]*schema.Message{{
		Role: schema.User,
		UserInputMultiContent: []schema.MessageInputPart{
			imagePart(schema.ImageURLDetailHigh, 1024),
			imagePart(schema.ImageURLDetailHigh, 1024),
		},
	}})

	require.Greater(t, one, base, "one image must cost more than none")
	require.Equal(t, two-one, one-base, "each additional image costs the same")
}

// ledger: A2/W-A-01#2 估算不随 base64 载荷字节数线性增长
func TestEstimateTokensImageCostIsIndependentOfPayloadSize(t *testing.T) {
	small := EstimateTokens([]*schema.Message{{
		Role:                  schema.User,
		UserInputMultiContent: []schema.MessageInputPart{imagePart(schema.ImageURLDetailHigh, 4<<10)},
	}})
	large := EstimateTokens([]*schema.Message{{
		Role:                  schema.User,
		UserInputMultiContent: []schema.MessageInputPart{imagePart(schema.ImageURLDetailHigh, 1<<20)},
	}})

	require.Equal(t, small, large,
		"image cost is per-tile in provider accounting; len(data)/4 would differ by 3 orders of magnitude")
}

// ledger: A2/W-A-01#3 Message 上三个多模态字段都被计入
func TestEstimateTokensCountsAllThreeMultimodalFields(t *testing.T) {
	base := EstimateTokens([]*schema.Message{{Role: schema.User}})
	url := "data:image/png;base64,AAAA"

	deprecated := EstimateTokens([]*schema.Message{{
		Role: schema.User,
		MultiContent: []schema.ChatMessagePart{{
			Type:     schema.ChatMessagePartTypeImageURL,
			ImageURL: &schema.ChatMessageImageURL{URL: url, Detail: schema.ImageURLDetailHigh},
		}},
	}})
	userInput := EstimateTokens([]*schema.Message{{
		Role:                  schema.User,
		UserInputMultiContent: []schema.MessageInputPart{imagePart(schema.ImageURLDetailHigh, 4)},
	}})
	assistantGen := EstimateTokens([]*schema.Message{{
		Role: schema.Assistant,
		AssistantGenMultiContent: []schema.MessageOutputPart{{
			Type: schema.ChatMessagePartTypeImageURL,
			Image: &schema.MessageOutputImage{
				MessagePartCommon: schema.MessagePartCommon{MIMEType: "image/png", URL: &url},
			},
		}},
	}})

	require.Greater(t, deprecated, base, "MultiContent must be counted")
	require.Greater(t, userInput, base, "UserInputMultiContent must be counted")
	require.Greater(t, assistantGen, base, "AssistantGenMultiContent must be counted")
}

// ledger: A2/W-A-01#4 低 detail 档位的估算低于高 detail 档位
func TestEstimateTokensLowDetailCostsLessThanHigh(t *testing.T) {
	low := EstimateTokens([]*schema.Message{{
		Role:                  schema.User,
		UserInputMultiContent: []schema.MessageInputPart{imagePart(schema.ImageURLDetailLow, 4<<10)},
	}})
	high := EstimateTokens([]*schema.Message{{
		Role:                  schema.User,
		UserInputMultiContent: []schema.MessageInputPart{imagePart(schema.ImageURLDetailHigh, 4<<10)},
	}})

	require.Less(t, low, high)
}

// TestEstimateTokensCountsTextParts pins that a text part inside multimodal
// content is estimated as text, not skipped alongside the media branch.
func TestEstimateTokensCountsTextParts(t *testing.T) {
	base := EstimateTokens([]*schema.Message{{Role: schema.User}})
	withText := EstimateTokens([]*schema.Message{{
		Role: schema.User,
		UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeText, Text: strings.Repeat("hello world ", 100)},
		},
	}})

	require.Greater(t, withText, base)
}
