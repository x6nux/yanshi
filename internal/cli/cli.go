package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/x6nux/yanshi/internal/proto"
)

// This package no longer exposes a TUI-launching Run: package cli cannot import
// package tui (the tui package depends on cli.StreamEvent), so the composition
// root — cli.NewSession → Session.Resolve → tui.NewProgram → prog.Run — lives in
// cmd/yanshi (package main, which may import both). The in-package test seam
// runHeadless stays for integration tests.

// runHeadless resolves a backend and pumps the given queries through it,
// collecting every event — a test seam that exercises Resolve + Send without a
// terminal. Used by integration tests.
func runHeadless(ctx context.Context, opts Options, queries []string) ([]StreamEvent, error) {
	if opts.Root == "" {
		wd, _ := os.Getwd()
		opts.Root = wd
	}
	sess := newSession(opts.Root, opts.ConfigPath, opts.FakeModel)
	sess.setForced(opts.Server, opts.InProcess)
	if err := sess.Resolve(ctx); err != nil {
		return nil, err
	}
	defer sess.Close()

	var out []StreamEvent
	for _, q := range queries {
		ch, err := sess.Backend().Send(ctx, q)
		if err != nil {
			return out, fmt.Errorf("send %q: %w", q, err)
		}
		for ev := range ch {
			out = append(out, ev)
		}
	}
	return out, nil
}

// runHeadlessWithFrames is the frame-aware sibling of runHeadless: it drives an
// already-resolved backend through a sequence of client frames — user messages
// AND Phase-10 control frames (list_models / set_model / set_thinking / clear /
// get_status / compact / list_mcp) — collecting every StreamEvent each step
// produces.
//
// Each frame is dispatched by Type: a "user_message" is a full turn (drained to
// done/error, like runHeadless); any other Type is a control request sent via
// SendFrame and drained to its single-frame reply (models / status / mcp_list).
// Steps run sequentially; the backend serializes writes (sendMu), and each step
// blocks until its reply/turn-terminator closes the channel before the next
// begins.
//
// It does NOT handle mid-turn permission_response: answering a permission_request
// requires writing the response while the turn's channel is still open (the turn
// is blocked inside the ask callback). The integration test drives that turn
// inline so it can read permission_request from the live channel and then
// SendFrame the response. Every other frame — including the user turns that do
// not prompt — is covered here.
func runHeadlessWithFrames(ctx context.Context, b ChatBackend, frames []proto.ClientFrame) ([]StreamEvent, error) {
	var out []StreamEvent
	for _, f := range frames {
		var ch <-chan StreamEvent
		var err error
		if f.Type == "user_message" {
			ch, err = b.Send(ctx, f.Text)
		} else {
			ch, err = b.SendFrame(ctx, f)
		}
		if err != nil {
			return out, fmt.Errorf("frame %q: %w", f.Type, err)
		}
		if ch == nil {
			continue // permission_response: no reply expected (not used here)
		}
		for ev := range ch {
			out = append(out, ev)
			if ev.Kind == "error" && ev.Err != nil {
				return out, fmt.Errorf("frame %q errored: %w", f.Type, ev.Err)
			}
		}
	}
	return out, nil
}
