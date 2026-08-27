package http

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/tools"
)

// ws_perm_timeout_verify_test.go measures the approval countdown from the
// CLIENT side of a real WebSocket, by connecting and then deliberately saying
// nothing.
//
// A prompt that hangs forever and a prompt that expires are indistinguishable
// from inside the server: both are a goroutine blocked on a channel. The only
// way to tell them apart is to be the client that never answers and observe
// whether the turn finishes. Everything here is therefore driven over the wire,
// with a short configured timeout so the whole file runs in seconds.
//
// The property under test is narrow and absolute: EXPIRY IS NEVER APPROVAL.
// Every other detail (how many expiries latch the connection, how fast the
// refusal comes afterwards) is latency. This one is the security boundary,
// because a timeout is the absence of an authorization gesture and the entire
// guard stack is fail-closed.

// newTimeoutWSServer builds a WS server whose scripted model makes n fs_write
// calls the profile does not permit, so each one raises a prompt. The
// permission timeout is set short enough to observe.
func newTimeoutWSServer(t *testing.T, n int, policy PermissionTimeoutPolicy) (url, workdir string) {
	t.Helper()
	workdir = t.TempDir()

	msgs := make([]*schema.Message, 0, n+1)
	for i := 0; i < n; i++ {
		msgs = append(msgs, schema.AssistantMessage("", []schema.ToolCall{
			{ID: "c" + string(rune('1'+i)), Type: "function", Function: schema.FunctionCall{
				Name:      "fs_write",
				Arguments: `{"path":"out` + string(rune('1'+i)) + `.txt","content":"x"}`,
			}},
		}))
	}
	msgs = append(msgs, schema.AssistantMessage("finished", nil))
	mdl := einollm.NewFakeModelWithMessages(msgs, nil)

	fs := tools.NewFSTools(workdir)
	o, err := orchestrator.New(orchestrator.Config{
		Model: mdl,
		Tools: []orchestrator.BaseTool{fs.Write},
		// Write allowlist names a subdirectory the writes miss, so the FS
		// dimension returns Prompt and the interactive callback is consulted.
		Profile: guard.PermissionProfile{
			Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
			FS:    guard.FSPerm{Write: []string{filepath.Join(workdir, "safe/**")}},
		},
	})
	require.NoError(t, err)

	s := New(Config{Token: "t", PermissionTimeout: policy})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/chat/ws", workdir
}

// TestS5_UnansweredPromptExpiresAndDenies is the core measurement. The client
// receives the prompt, answers nothing, and the turn must still finish — with
// the side effect NOT applied.
func TestS5_UnansweredPromptExpiresAndDenies(t *testing.T) {
	url, workdir := newTimeoutWSServer(t, 1, PermissionTimeoutPolicy{
		Timeout: 1500 * time.Millisecond, UnattendedAfter: 3,
	})
	c := dial(t, url)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("write the file")))
	req := drainUntil(t, c, "permission_request")
	t.Logf("permission_request id=%s reason=%q timeout_secs=%d",
		req.ID, req.Reason, req.PermTimeoutSecs)

	// Deliberately answer nothing.
	start := time.Now()
	for {
		f := readFrame(t, c)
		if f.Type == "done" || f.Type == "error" {
			t.Logf("turn ended with %q after %s of silence", f.Type, time.Since(start).Round(10*time.Millisecond))
			break
		}
	}

	assert.NoFileExists(t, filepath.Join(workdir, "out1.txt"),
		"AN EXPIRED PROMPT MUST NOT BE AN APPROVAL: the write happened anyway")
	if time.Since(start) > 30*time.Second {
		t.Errorf("the turn took %s to give up; the prompt is effectively unbounded", time.Since(start))
	}
}

// TestS5_CountdownIsOnTheWire checks the client is TOLD how long it has. The
// deadline is useless to a human who cannot see it, and the TUI renders these
// two fields.
func TestS5_CountdownIsOnTheWire(t *testing.T) {
	url, _ := newTimeoutWSServer(t, 1, PermissionTimeoutPolicy{
		Timeout: 2 * time.Second, UnattendedAfter: 3,
	})
	c := dial(t, url)
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("write the file")))
	req := drainUntil(t, c, "permission_request")

	t.Logf("timeout_secs=%d deadline_unix=%d", req.PermTimeoutSecs, req.PermDeadlineUnix)
	assert.Positive(t, req.PermTimeoutSecs, "the prompt must carry its budget")
	assert.Positive(t, req.PermDeadlineUnix, "the prompt must carry its deadline")
	assert.Greater(t, req.PermDeadlineUnix, time.Now().Unix()-5,
		"the deadline must be in the future when it is sent")
}

// TestS5_ConsecutiveExpiriesLatchUnattended measures the degradation: after the
// threshold, later prompts are refused IMMEDIATELY rather than waited on.
//
// The assertion is on TOTAL ELAPSED TIME against the naive cost. With four
// prompts at a 1.2s timeout, waiting on every one costs ~4.8s; latching after
// three costs ~3.6s. The test allows generous slack and only fails when no
// latch happened at all, so it measures the behaviour without being a
// stopwatch-precision test that flakes on a loaded CI box.
func TestS5_ConsecutiveExpiriesLatchUnattended(t *testing.T) {
	const perPrompt = 1200 * time.Millisecond
	const prompts = 5
	url, workdir := newTimeoutWSServer(t, prompts, PermissionTimeoutPolicy{
		Timeout: perPrompt, UnattendedAfter: 2,
	})
	c := dial(t, url)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("write them all")))
	start := time.Now()
	var seenPrompts int
	for {
		f := readFrame(t, c)
		if f.Type == "permission_request" {
			seenPrompts++
		}
		if f.Type == "done" || f.Type == "error" {
			break
		}
	}
	elapsed := time.Since(start)
	naive := time.Duration(prompts) * perPrompt
	t.Logf("%d prompts, %d seen; elapsed %s vs %s if every prompt were waited on",
		prompts, seenPrompts, elapsed.Round(10*time.Millisecond), naive)

	if elapsed >= naive {
		t.Errorf("no unattended latch: the run cost the full %s, so every prompt burned its whole budget", naive)
	}
	// The verdict must be unchanged by the latch — it changes WHEN, not WHETHER.
	for i := 1; i <= prompts; i++ {
		assert.NoFileExists(t, filepath.Join(workdir, "out"+string(rune('0'+i))+".txt"),
			"the latch must not turn a refusal into an approval")
	}
}

// TestS5_InteractionUnlatches is the recovery direction, and it is what keeps
// the latch from being a one-way trip. An operator who walks away, comes back
// and types something must get interactive prompts again — otherwise the
// heuristic silently converts their session into a read-only one.
//
// It needs its own server rather than newTimeoutWSServer's because it drives
// TWO turns on one connection, and a scripted FakeModel is a single queue: a
// script sized for one turn is exhausted by it, and turn two then ends
// immediately with no tool call at all — which looks exactly like "the prompt
// was refused unheard", i.e. the bug this test is supposed to detect.
func TestS5_InteractionUnlatches(t *testing.T) {
	const perPrompt = 800 * time.Millisecond
	workdir := t.TempDir()

	// Two turns' worth: each is one fs_write followed by a closing message.
	write := func(name string) *schema.Message {
		return schema.AssistantMessage("", []schema.ToolCall{
			{ID: name, Type: "function", Function: schema.FunctionCall{
				Name:      "fs_write",
				Arguments: `{"path":"` + name + `.txt","content":"x"}`,
			}},
		})
	}
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{
		write("first"), schema.AssistantMessage("turn one done", nil),
		write("second"), schema.AssistantMessage("turn two done", nil),
	}, nil)

	fs := tools.NewFSTools(workdir)
	o, err := orchestrator.New(orchestrator.Config{
		Model: mdl,
		Tools: []orchestrator.BaseTool{fs.Write},
		Profile: guard.PermissionProfile{
			Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
			FS:    guard.FSPerm{Write: []string{filepath.Join(workdir, "safe/**")}},
		},
	})
	require.NoError(t, err)
	s := New(Config{Token: "t", PermissionTimeout: PermissionTimeoutPolicy{
		Timeout: perPrompt, UnattendedAfter: 1, // latch on the very first expiry
	}})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()

	// Turn one: say nothing, get latched.
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("write the first")))
	waitForType(t, c, "permission_request")
	for {
		f := readFrame(t, c)
		if f.Type == "done" || f.Type == "error" {
			t.Logf("turn one ended with %q (latched)", f.Type)
			break
		}
	}
	require.NoFileExists(t, filepath.Join(workdir, "first.txt"),
		"turn one's write must have been denied by expiry")

	// The operator comes back and interacts. A set_mode frame is an
	// interaction like any other.
	//
	// drainUntil is not usable here: it treats "error" as fatal, and turn one
	// legitimately ended in one (its tool call was denied). Waiting for the
	// status echo directly is the correct read of a connection that has
	// already reported a failed turn.
	require.NoError(t, c.WriteJSON(proto.NewSetMode("default")))
	waitForType(t, c, "status")

	// Turn two: a prompt must be RAISED again rather than refused unheard.
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("write the second")))
	req := waitForType(t, c, "permission_request")
	t.Logf("after interaction, a prompt was raised again: id=%s", req.ID)
	require.NoError(t, c.WriteJSON(proto.NewPermissionResponse(req.ID, "allow")))
	for {
		f := readFrame(t, c)
		if f.Type == "done" || f.Type == "error" {
			break
		}
	}
	assert.FileExists(t, filepath.Join(workdir, "second.txt"),
		"after unlatching, an answered prompt must actually take effect")
}

// waitForType reads frames until one of the wanted type arrives, tolerating
// "error" frames along the way.
//
// It is deliberately NOT drainUntil: that helper fails on "error", which is the
// right strictness for a turn expected to succeed and the wrong one for a
// connection whose previous turn was supposed to fail. Reusing it here would
// make a correct denial look like a broken test.
func waitForType(t *testing.T, c *websocket.Conn, want string) proto.ServerFrame {
	t.Helper()
	for i := 0; i < 50; i++ {
		f := readFrame(t, c)
		if f.Type == want {
			return f
		}
	}
	t.Fatalf("never saw a %q frame", want)
	return proto.ServerFrame{}
}
