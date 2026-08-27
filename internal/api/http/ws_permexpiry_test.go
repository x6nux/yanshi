package http

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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

// newExpiringPermWSServer is newPermWSServer with an explicit (short) approval
// countdown, and with a scripted model that issues n fs_write calls the profile
// will not admit. Each call becomes one interactive prompt the test can leave
// unanswered.
//
// Two properties of the script are load-bearing, and getting either wrong
// produces a test that passes with the feature removed:
//
//   - The n calls are SEQUENTIAL — n assistant messages of one tool call each,
//     i.e. n ReAct iterations — not one message with n parallel calls. Parallel
//     calls have their prompts outstanding at the same time and therefore all
//     expire at the same instant, so total elapsed time is one budget whether
//     or not anything latches. The first draft of this helper did that and the
//     latency assertion below was vacuous.
//   - They all live in ONE turn. Every client frame resets the latch, and
//     user_message is a client frame, so n separate turns would clear the
//     counter between each. Within a turn is also where the wedge this
//     prevents actually occurs.
//
// The pair is scripted twice over so a test can run a SECOND turn on the same
// connection (the reset test needs one); a fake whose responses are exhausted
// returns empty assistant messages and issues no tool calls at all.
//
// The timeout is a Config value rather than a package variable a test could
// poke, so these tests exercise the same plumbing production uses: Config →
// New → per-connection unattendedState → the callback's wait.
func newExpiringPermWSServer(t *testing.T, policy PermissionTimeoutPolicy, n int) (url, workdir string) {
	t.Helper()
	workdir = t.TempDir()

	var msgs []*schema.Message
	for turn := 0; turn < 2; turn++ {
		for i := 0; i < n; i++ {
			msgs = append(msgs, schema.AssistantMessage("", []schema.ToolCall{{
				ID: "c" + strconv.Itoa(turn) + "-" + strconv.Itoa(i), Type: "function",
				Function: schema.FunctionCall{
					Name:      "fs_write",
					Arguments: `{"path":"out` + strconv.Itoa(i) + `.txt","content":"hello"}`,
				},
			}}))
		}
		msgs = append(msgs, schema.AssistantMessage("done", nil))
	}
	mdl := einollm.NewFakeModelWithMessages(msgs, nil)

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

	s := New(Config{Token: "t", PermissionTimeout: policy})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/chat/ws", workdir
}

// TestChatWS_PermissionExpiryDeniesAndUnblocksTheTurn is the end-to-end shape of
// S5's first half.
//
// Before this, an unanswered prompt blocked the turn for as long as the
// hardcoded wait lasted and there was no way for a client to know a clock was
// running. The three assertions correspond to the three things that were
// broken: the turn ENDS (an unattended goal loop is not wedged), the tool did
// NOT run (a timeout is not consent), and the frame carried a countdown (a UI
// can show the user what is about to happen).
func TestChatWS_PermissionExpiryDeniesAndUnblocksTheTurn(t *testing.T) {
	url, workdir := newExpiringPermWSServer(t,
		PermissionTimeoutPolicy{Timeout: 300 * time.Millisecond, UnattendedAfter: 99}, 1)
	c := dial(t, url)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("write the file")))

	req := drainUntil(t, c, "permission_request")
	assert.Equal(t, "fs_write", req.ToolName)
	// The countdown must be on the wire. Without it the TUI has nothing to
	// render and the prompt appears to die for no reason.
	assert.Positive(t, req.PermTimeoutSecs,
		"permission_request must advertise the answer budget")
	assert.Positive(t, req.PermDeadlineUnix,
		"permission_request must advertise the absolute deadline")

	// Deliberately answer nothing. The turn must still reach done.
	var sawDenyNotice bool
	for {
		f := readFrame(t, c)
		if f.Type == "error" && strings.Contains(f.Text, "expired") {
			sawDenyNotice = true
		}
		if f.Type == "done" {
			break
		}
	}
	assert.True(t, sawDenyNotice,
		"an expired prompt must say so, or the transcript shows a denial with no stated reason")

	// The load-bearing assertion: silence did not authorize the write.
	_, err := os.ReadFile(filepath.Join(workdir, "out0.txt"))
	assert.Error(t, err, "an expired permission prompt must never let the tool run")
}

// TestChatWS_UnattendedLatchStopsWaitingAfterConsecutiveExpiries is S5's second
// half, and the reason the first half alone is not enough.
//
// With expiry but no latch, an unattended run pays the full timeout for EVERY
// prompt. One turn that fans out four denied tool calls stalls for 4×budget,
// and a goal loop doing that repeatedly burns its wall-clock budget on waiting.
// The measurable consequence is elapsed time, so that is what this asserts:
// four prompts at a 400ms budget cost ~1.6s unlatched, but the second latches
// and the rest return immediately.
//
// The bound is 3×budget: comfortably under the unlatched 4×, comfortably over
// the latched ~2×, so the test tells the two behaviours apart without being a
// timing race. It is a one-sided bound on purpose — asserting a lower bound too
// would make it fail on a fast machine for no defect.
func TestChatWS_UnattendedLatchStopsWaitingAfterConsecutiveExpiries(t *testing.T) {
	const budget = 400 * time.Millisecond
	const prompts = 4
	url, workdir := newExpiringPermWSServer(t,
		PermissionTimeoutPolicy{Timeout: budget, UnattendedAfter: 2}, prompts)
	c := dial(t, url)
	defer c.Close()

	start := time.Now()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("write the files")))
	drainToDone(t, c)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, prompts*budget,
		"the unattended latch must stop paying a full timeout per prompt")

	// Latency is the ONLY thing the latch changes. The verdict is deny either
	// way, and this is where that would break if a future edit made the fast
	// path permissive to "save time".
	entries, err := os.ReadDir(workdir)
	require.NoError(t, err)
	assert.Empty(t, entries, "the unattended fast path must still deny, not allow")
}

// TestChatWS_UnattendedLatchResetsOnUserInteraction proves the latch is not a
// one-way door.
//
// A user who walks away must get a real prompt on their next request, not a
// silent auto-deny. The reset lives in the reader goroutine and fires on ANY
// frame, so this exercises it with a plain get_status — not a permission answer
// — because that is the case a reset scoped to permission traffic would miss.
func TestChatWS_UnattendedLatchResetsOnUserInteraction(t *testing.T) {
	const budget = 250 * time.Millisecond
	// Two prompts in the first turn: with a threshold of 1 the first expiry
	// latches and the second is refused outright, so the connection is
	// definitely latched by the time the turn ends.
	url, workdir := newExpiringPermWSServer(t,
		PermissionTimeoutPolicy{Timeout: budget, UnattendedAfter: 1}, 2)
	c := dial(t, url)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("write the files")))
	drainToDone(t, c)

	// Unrelated interaction. This must unlatch.
	require.NoError(t, c.WriteJSON(proto.NewGetStatus()))
	drainUntil(t, c, "status")

	// The next turn must PROMPT again — reaching the client and waiting for an
	// answer — rather than being refused on arrival. Honouring the allow is the
	// observable proof: a latched connection could not produce this file.
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("write the files")))
	req := drainUntil(t, c, "permission_request")
	require.NotEmpty(t, req.ID)
	require.NoError(t, c.WriteJSON(proto.NewPermissionResponse(req.ID, "allow")))
	drainToDone(t, c)

	assert.FileExists(t, filepath.Join(workdir, "out0.txt"),
		"after an interaction the connection must prompt (and honour the answer) again")
}

// drainToDone reads frames until the turn's done frame.
func drainToDone(t *testing.T, c *websocket.Conn) {
	t.Helper()
	for {
		if readFrame(t, c).Type == "done" {
			return
		}
	}
}
