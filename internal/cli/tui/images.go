package tui

import (
	"github.com/x6nux/yanshi/internal/proto"
)

// buildSendFrame assembles the user_message ClientFrame, attaching images when
// present (Tier G). nil/empty images => byte-identical to a text-only frame
// (additive). This is the single seam through which image attachments enter the
// WS/SSE wire path from the TUI (Ctrl+V paste, @path detection, screenshot).
func buildSendFrame(text string, images []proto.ImageAttach) proto.ClientFrame {
	if len(images) == 0 {
		return proto.NewUserMessage(text)
	}
	return proto.NewUserMessageWithImages(text, images)
}
