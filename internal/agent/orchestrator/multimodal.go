package orchestrator

import (
	"context"
	"encoding/base64"
	"slices"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/tools"
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

// expandPathRefs resolves "@path" image references in the trailing user message
// (Tier G entry B) and merges the resulting attachments into the turn's image
// list. Called from EventsWithHistoryOpts right after withTurnContext, because
// tools.ResolveImagePathRefs reads the work root and the permission profile from
// ctx — the jail and the FS guard that keep a model-composed "@../../secret.png"
// from becoming an arbitrary file read.
//
// Only the trailing message is scanned: earlier turns' text is already in the
// history, and re-scanning it would re-attach the same file on every subsequent
// turn until the context is compacted away.
//
// The rewrite is copy-on-write for the same reason ApplyImages' clone is: the WS
// turn loop hands its persistent cs.history slice to EventsWithHistoryOpts on
// every retry attempt, so editing the trailing message in place would rewrite
// the user's saved text (and, on the next attempt, re-expand an already-expanded
// message).
//
// Rejections are intentionally not turned into an error: the reference stays
// verbatim in the text, so a denied or out-of-jail "@path" degrades to plain
// prose the model can still reason about instead of failing the whole turn.
func (o *Orchestrator) expandPathRefs(ctx context.Context, messages []*schema.Message, images []proto.ImageAttach) ([]*schema.Message, []proto.ImageAttach) {
	n := len(messages)
	if n == 0 {
		return messages, images
	}
	last := messages[n-1]
	if last == nil || last.Role != schema.User || last.Content == "" {
		return messages, images
	}
	res := tools.ResolveImagePathRefs(ctx, last.Content)
	if len(res.Images) == 0 {
		return messages, images
	}
	out := slices.Clone(messages)
	rewritten := *last
	rewritten.Content = res.Text
	out[n-1] = &rewritten
	merged := make([]proto.ImageAttach, 0, len(images)+len(res.Images))
	merged = append(append(merged, images...), res.Images...)
	return out, merged
}

// imageAttacher is the model-facing half of Tier G entry C. It is the point
// where an image produced BY A TOOL (fs_read on a .png, web_fetch on an image/*
// response) actually reaches the model.
//
// # Why a middleware and not the EventsWithHistoryOpts fan-out
//
// TurnOpts.Images and expandPathRefs both converge on ApplyImages, which runs
// ONCE, before the turn starts — the right place for images the caller already
// held. Tool-produced images do not exist yet at that moment: they appear in the
// middle of the ReAct loop, several model calls later. So they need an injection
// point inside the loop, and BeforeModelRewriteState is the only hook whose
// returned state the ADK PERSISTS. Injecting from a model wrapper instead would
// work for exactly one call and then vanish, because the ADK rebuilds the
// message list from its own state on the next iteration — the model would lose
// the picture the moment it called another tool.
//
// It reuses appendImageParts, so a tool-produced image and a user-pasted one are
// laid out for the provider identically; there is one image-part encoder in this
// package, not two.
//
// Draining is what bounds this: the sink hands over each image once, so the
// growing ADK state carries exactly one copy. The non-multimodal case never
// reaches here at all — the sink refuses to collect, and the tools return their
// text hint instead (see tools.TurnImages).
type imageAttacher struct {
	*adk.BaseChatModelAgentMiddleware
}

// newImageAttacher builds an imageAttacher ready for installation on an
// adk.ChatModelAgentConfig.Handlers slice.
func newImageAttacher() *imageAttacher {
	return &imageAttacher{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{}}
}

// BeforeModelRewriteState appends any images the turn's tools produced since the
// last model call to the ADK state, as native image parts.
//
// The clone mirrors ApplyImages' clone for the same reason: appendImageParts
// rewrites the trailing element of the slice it is handed, and the ADK's state
// slice may be aliased by the recorder middleware's capture.
func (a *imageAttacher) BeforeModelRewriteState(
	ctx context.Context,
	state *adk.ChatModelAgentState,
	_ *adk.ModelContext,
) (context.Context, *adk.ChatModelAgentState, error) {
	if state == nil {
		return ctx, state, nil
	}
	images := tools.TurnImagesFromContext(ctx).Drain()
	if len(images) == 0 {
		return ctx, state, nil
	}
	state.Messages = appendImageParts(slices.Clone(state.Messages), images)
	return ctx, state, nil
}
