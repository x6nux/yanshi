package orchestrator

import (
	"encoding/base64"
	"strings"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/proto"
)

// ApplyImages is the Tier G image fan-out: for the given (current turn) model-id
// and image attachments, it either embeds each image as a native image part
// (multimodal model) or stores it and appends the placeholder text
// [image:img-N|src|WxH fmt] to the trailing user message (non-multimodal model).
// history is the in-progress message slice. With no images it is a pass-through.
func (o *Orchestrator) ApplyImages(history []*schema.Message, modelID string, images []proto.ImageAttach) []*schema.Message {
	if len(images) == 0 {
		return history
	}
	if o.IsMultimodal(modelID) {
		return appendImageParts(history, images)
	}
	return o.appendPlaceholders(history, images)
}

// IsMultimodal reports whether the given model-id is natively multimodal. A nil
// map means no provider declared multimodal: true.
func (o *Orchestrator) IsMultimodal(modelID string) bool {
	return o.multimodalMap[modelID]
}

func appendImageParts(history []*schema.Message, images []proto.ImageAttach) []*schema.Message {
	parts := make([]schema.MessageInputPart, 0, len(images))
	for _, img := range images {
		mime := mimeForImage(img.Fmt)
		url := "data:" + mime + ";base64," + img.DataB64
		parts = append(parts, schema.MessageInputPart{
			Type: schema.ChatMessagePartTypeImageURL,
			Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{MIMEType: mime, URL: &url},
			},
		})
	}
	// Embed into trailing user message; or create a new one
	if n := len(history); n > 0 && history[n-1] != nil && history[n-1].Role == schema.User {
		last := *history[n-1]
		last.UserInputMultiContent = append(append([]schema.MessageInputPart(nil), history[n-1].UserInputMultiContent...), parts...)
		history[n-1] = &last
		return history
	}
	return append(history, &schema.Message{Role: schema.User, UserInputMultiContent: parts})
}

func (o *Orchestrator) appendPlaceholders(history []*schema.Message, images []proto.ImageAttach) []*schema.Message {
	if o.imageStore == nil {
		return history
	}
	var ph strings.Builder
	for _, img := range images {
		data, err := base64.StdEncoding.DecodeString(img.DataB64)
		if err != nil {
			continue
		}
		id, err := o.imageStore.Put(data, firstNonEmptyStr(img.Source, "attach"), img.Fmt)
		if err != nil {
			continue
		}
		ph.WriteString(o.imageStore.Placeholder(id))
		ph.WriteString("\n")
	}
	if ph.Len() == 0 {
		return history
	}
	if n := len(history); n > 0 && history[n-1] != nil && history[n-1].Role == schema.User {
		last := *history[n-1]
		last.Content = strings.TrimRight(last.Content, "\n") + "\n" + ph.String()
		history[n-1] = &last
		return history
	}
	return append(history, schema.UserMessage(strings.TrimRight(ph.String(), "\n")))
}

func mimeForImage(f string) string {
	switch strings.ToLower(strings.TrimSpace(f)) {
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
