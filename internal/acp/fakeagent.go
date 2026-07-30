package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// FakeAgent is an in-process ACP agent for testing. It owns the agent-side
// of two io.Pipe pairs, cross-connected to the client's transport. The
// FakeAgent's ReadLoop is started automatically in a goroutine by NewFakeAgent.
//
// Scriptable fields (set before calling Prompt on the client):
//
//   - Updates: text chunks emitted as agent_message_chunk session/update
//     notifications. Defaults to ["hello ", "world"] when nil.
//   - HoldPrompt: when true, the FakeAgent emits updates but does NOT
//     auto-resolve the prompt — the prompt id stays pending until Cancel
//     is received (used by the Task 6 cancel test).
//   - InboundRequests: scripted server->client requests to send during
//     session/prompt (before resolving). Each entry specifies the method
//     and params; the FakeAgent records the response (result or error).
type FakeAgent struct {
	tr     *Transport
	agentR *io.PipeReader
	agentW *io.PipeWriter
	cancel context.CancelFunc
	done   chan struct{}

	// Scriptable behaviour.
	Updates         []string // scripted agent_message_chunk texts (default ["hello ","world"])
	HoldPrompt      bool     // if true, don't auto-resolve the prompt (for cancel tests)
	InboundRequests []InboundSpec // server->client requests to send during prompt
	// UsageReports, when non-empty, are emitted as session/update usage_report
	// notifications right before the prompt resolves (after text chunks).
	UsageReports []Usage

	// Recorded responses for inbound requests. Guarded by inboundMu.
	inboundMu    sync.Mutex
	inboundResps []InboundResponse

	// capturedNewSession is the raw session/new params received, if any.
	// Guarded by sessionMu.
	sessionMu         sync.Mutex
	capturedNewSession json.RawMessage

	// Pending prompt state (hold mode). Guarded by promptMu.
	promptMu    sync.Mutex
	promptID    int64   // request id of the held session/prompt (> 0 means held)
	promptCanc  chan struct{} // closed when session/cancel arrives for the held prompt
}

// InboundSpec describes a scripted server->client request.
type InboundSpec struct {
	Method string
	Params any
}

// InboundResponse records what the client answered to an inbound request.
type InboundResponse struct {
	// Result is the raw JSON result (nil if the client returned an error).
	Result json.RawMessage
	// Err is the RPC error (nil if the client returned a result).
	Err *RPCError
}

// InboundResponses returns a snapshot of recorded client responses to
// inbound requests sent during the last prompt.
func (fa *FakeAgent) InboundResponses() []InboundResponse {
	fa.inboundMu.Lock()
	defer fa.inboundMu.Unlock()
	out := make([]InboundResponse, len(fa.inboundResps))
	copy(out, fa.inboundResps)
	return out
}

// CapturedNewSession returns the raw session/new params received by the agent
// (nil if no session/new was handled). Used to assert on the mcpServers field
// actually sent over the wire.
func (fa *FakeAgent) CapturedNewSession() json.RawMessage {
	fa.sessionMu.Lock()
	defer fa.sessionMu.Unlock()
	return fa.capturedNewSession
}

// NewFakeAgent creates a FakeAgent and returns the client-side pipe ends.
//
// Two io.Pipe pairs are cross-connected:
//
//	agentR, clientOut := io.Pipe()   // client writes to clientOut -> agent reads
//	clientIn, agentW  := io.Pipe()   // agent writes to agentW -> client reads
//
// The client's NewTransport(clientIn, clientOut) and the agent's transport
// are thus fully connected. The FakeAgent starts its ReadLoop immediately.
func NewFakeAgent() (fa *FakeAgent, clientIn io.Reader, clientOut io.Writer) {
	agentR, clientOut := io.Pipe() // client writes -> agent reads
	clientIn, agentW := io.Pipe()  // agent writes -> client reads

	fa = &FakeAgent{
		tr:     NewTransport(agentR, agentW),
		agentR: agentR,
		agentW: agentW,
		done:   make(chan struct{}),
	}

	fa.tr.SetHandlers(fa.handleNotify, fa.handleRequest)

	ctx, cancel := context.WithCancel(context.Background())
	fa.cancel = cancel
	go func() {
		fa.tr.ReadLoop(ctx)
		close(fa.done)
	}()

	return fa, clientIn, clientOut
}

// handleRequest processes inbound JSON-RPC requests from the client.
// Supported methods: initialize, session/new, session/prompt.
// Unknown methods return a JSON-RPC method-not-found error (-32601).
//
// For session/prompt the FakeAgent emits each scripted Update text as an
// agent_message_chunk session/update notification, then (unless HoldPrompt)
// resolves the prompt's id with PromptResult{StopReason:"end_turn"}.
// In hold mode the handler registers the pending prompt and returns
// errNoResponse so the transport does not write an immediate response; a
// background goroutine resolves the prompt when session/cancel arrives.
func (fa *FakeAgent) handleRequest(req inboundRequest) (json.RawMessage, error) {
	switch req.Method {
	case "initialize":
		result := InitResult{
			ProtocolVersion: 1,
			AgentInfo:       AgentInfo{Name: "fake"},
		}
		data, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		return data, nil

	case "session/new":
		fa.sessionMu.Lock()
		fa.capturedNewSession = append(fa.capturedNewSession[:0], req.Params...)
		fa.sessionMu.Unlock()
		result := NewSessionResult{SessionID: "sess_fake"}
		data, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		return data, nil

	case "session/prompt":
		var params PromptParams
		_ = json.Unmarshal(req.Params, &params)

		chunks := fa.Updates
		if chunks == nil {
			chunks = []string{"hello ", "world"}
		}

		// Emit each chunk as an agent_message_chunk session/update notification.
		for _, text := range chunks {
			updParams := UpdateParams{
				SessionID: params.SessionID,
				Update: Update{
					SessionUpdate: "agent_message_chunk",
					Content:       []ContentBlock{{Type: "text", Text: text}},
				},
			}
			if err := fa.tr.Notify("session/update", updParams); err != nil {
				return nil, err
			}
		}

		// Emit scripted usage_report notifications. The Update struct has no
		// Usage field, so we write raw JSON matching the {update:{usage:...}}
		// shape that parseUsageReport expects.
		for _, u := range fa.UsageReports {
			raw := fmt.Sprintf(`{"sessionId":%q,"update":{"sessionUpdate":"usage_report","usage":{"inputTokens":%d,"outputTokens":%d,"totalTokens":%d}}}`,
				params.SessionID, u.InputTokens, u.OutputTokens, u.TotalTokens)
			if err := fa.tr.Notify("session/update", json.RawMessage(raw)); err != nil {
				return nil, err
			}
		}

		// Send any scripted inbound server->client requests and record
		// the client's responses. This must happen in a separate goroutine
		// because tr.Call blocks waiting for a response that arrives through
		// the same ReadLoop that invoked this handler — calling Call inline
		// would deadlock. The prompt response is also deferred (via
		// errNoResponse) so the ReadLoop stays free to deliver the inbound
		// responses.

		go func() {
			ctx := context.Background()
			for _, spec := range fa.InboundRequests {
				result, err := fa.tr.Call(ctx, spec.Method, spec.Params)
				fa.inboundMu.Lock()
				if err != nil {
					if rpcErr, ok := err.(*RPCError); ok {
						fa.inboundResps = append(fa.inboundResps, InboundResponse{Err: rpcErr})
					} else {
						fa.inboundResps = append(fa.inboundResps, InboundResponse{
							Err: &RPCError{Code: -32603, Message: err.Error()},
						})
					}
				} else {
					fa.inboundResps = append(fa.inboundResps, InboundResponse{Result: result})
				}
				fa.inboundMu.Unlock()
			}

			// Hold mode: register the pending prompt and wait for cancel.
			if fa.HoldPrompt {
				canc := make(chan struct{})
				fa.promptMu.Lock()
				fa.promptID = req.ID
				fa.promptCanc = canc
				fa.promptMu.Unlock()

				<-canc
				fa.tr.Respond(req.ID, PromptResult{StopReason: "cancelled"}, nil)
				fa.promptMu.Lock()
				fa.promptID = 0
				fa.promptCanc = nil
				fa.promptMu.Unlock()
				return
			}

			// Normal mode: resolve the prompt with end_turn.
			fa.tr.Respond(req.ID, PromptResult{StopReason: "end_turn"}, nil)
		}()

		return nil, errNoResponse

	default:
		return nil, &RPCError{Code: -32601, Message: "method not found: " + req.Method}
	}
}

// handleNotify processes inbound JSON-RPC notifications from the client.
// Currently handles "session/cancel" by finalising a held prompt.
func (fa *FakeAgent) handleNotify(method string, params json.RawMessage) {
	if method != "session/cancel" {
		return
	}
	fa.promptMu.Lock()
	canc := fa.promptCanc
	fa.promptMu.Unlock()
	if canc != nil {
		close(canc)
	}
}

// CancelPrompt manually finalises a held prompt with the given stop reason.
// This is an alternative to sending a session/cancel notification — useful
// for tests that want to control the exact stop reason or response timing.
func (fa *FakeAgent) CancelPrompt(id int64, stopReason string) {
	fa.tr.Respond(id, PromptResult{StopReason: stopReason}, nil)
}

// Close shuts down the FakeAgent: cancels its ReadLoop context and closes
// both pipe ends. This also causes the client's ReadLoop to exit (its
// reader returns EOF). Blocks until the agent's ReadLoop has stopped.
func (fa *FakeAgent) Close() {
	fa.cancel()
	fa.agentR.Close()
	fa.agentW.Close()
	<-fa.done
}
