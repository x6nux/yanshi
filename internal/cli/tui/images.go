package tui

import (
	"github.com/x6nux/yanshi/internal/proto"
)

// buildSendFrame assembles the user_message ClientFrame, attaching images
// (Tier G) and @path references (UX3) when present. Empty slices produce a
// frame byte-identical to a text-only turn, because omitempty drops both.
//
// This is the single seam through which attachments enter the WS/SSE wire path
// from the TUI — Ctrl+V paste, @path detection, screenshots. Keeping it one
// function is what stops the two attachment kinds from growing two different
// send paths that then disagree about which one clears state.
func buildSendFrame(text string, images []proto.ImageAttach, attachments []proto.AttachRef) proto.ClientFrame {
	f := proto.NewUserMessage(text)
	f.Images = images
	f.Attachments = attachments
	return f
}
