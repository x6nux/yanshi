// internal/llm/eino/streamretry.go
//
// The shared "open a stream, look at the first item, and possibly start over"
// primitive.
//
// Two features need it and neither can be built without it. C6 (reactive
// recovery from a provider context-overflow rejection) and M5 (runtime learning
// of model quirks) both discover the problem in the response to a request they
// have already sent, and both fix it by MUTATING THE REQUEST and sending again.
// For Generate that is a plain function call. For Stream it is not: eino's
// Stream returns a reader immediately and reports the provider's rejection on
// the first Recv, so "did this request fail?" cannot be answered without
// consuming an item — and once an item is consumed the reader cannot be handed
// back to the caller intact.
//
// openAndPeek closes that gap by re-injecting the peeked item into a fresh
// reader, so a caller that decides NOT to retry is left holding a stream
// indistinguishable from the one the provider produced.
//
// SAFETY BOUND. A retry is only ever offered when the first Recv failed, i.e.
// when NOTHING was delivered. A mid-stream failure after content or a tool call
// has already reached the consumer is out of scope here and stays with
// ResilientChatModel, which knows about the tool-call duplication hazard.
package eino

import (
	"context"
	"errors"
	"io"

	"github.com/cloudwego/eino/schema"
)

// openStreamFunc opens one provider stream. It is a function rather than a
// captured reader so a retry can re-read whatever request state the caller
// mutated in between.
type openStreamFunc func(ctx context.Context) (*schema.StreamReader[*schema.Message], error)

// openAndPeek opens a stream and forces the first Recv, returning a reader that
// replays that first item followed by the remainder.
//
// The returned error is the provider's, surfaced BEFORE the caller sees a
// reader — which is the whole point: an error that would otherwise only appear
// on the caller's first Recv becomes a value the caller can inspect and act on.
//
// An immediate EOF (a stream that produced nothing at all) is NOT an error
// here; it yields an empty-but-valid reader, because "the provider returned
// nothing" is a condition ResilientChatModel already handles with its own
// empty-retry budget and duplicating that policy here would give an empty
// response two independent retry ladders.
func openAndPeek(ctx context.Context, open openStreamFunc) (*schema.StreamReader[*schema.Message], error) {
	sr, err := open(ctx)
	if err != nil {
		return nil, err
	}
	first, recvErr := sr.Recv()
	if recvErr != nil && !errors.Is(recvErr, io.EOF) {
		sr.Close()
		return nil, recvErr
	}
	if errors.Is(recvErr, io.EOF) {
		sr.Close()
		empty, ew := schema.Pipe[*schema.Message](1)
		ew.Close()
		return empty, nil
	}
	out, ow := schema.Pipe[*schema.Message](1)
	go replayStream(sr, ow, first)
	return out, nil
}

// replayStream forwards head and then everything still in sr to ow, closing
// both when done. It is the goroutine half of openAndPeek.
func replayStream(sr *schema.StreamReader[*schema.Message], ow *schema.StreamWriter[*schema.Message],
	head *schema.Message) {
	defer func() {
		ow.Close()
		sr.Close()
	}()
	if ow.Send(head, nil) {
		return // consumer closed already
	}
	for {
		msg, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			ow.Send(nil, err)
			return
		}
		if ow.Send(msg, nil) {
			return
		}
	}
}

// isEOF reports whether err is the ordinary end-of-stream signal rather than a
// failure. Stream consumers in this package check it before forwarding an
// error, because eino signals a clean end with io.EOF and forwarding that would
// turn every completed stream into a failed one.
func isEOF(err error) bool { return errors.Is(err, io.EOF) }
