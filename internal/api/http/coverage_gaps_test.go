// Package http — coverage gap tests for uncounted branches across the WS/SSE
// handler functions. Each test targets a specific uncovered branch identified
// by go tool cover -func, so we can push the package to 99%+.
package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/features"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/shell"
	"github.com/x6nux/yanshi/internal/skills"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
	"github.com/x6nux/yanshi/internal/vcs"
)

// ---------------------------------------------------------------------------
// resolvePermissionMode — uncovered mode-auto-resolution branches
// ---------------------------------------------------------------------------

func TestResolvePermissionMode_AllowEditsDeniesNonEditTool(t *testing.T) {
	cs := &connSession{perm: &permModeState{}}
	cs.perm.set(guard.ModeAllowEdits)
	d, ok := resolvePermissionMode(context.Background(), cs, nil, &tools.PermissionRequest{
		Tool: "shell_run", Args: `{"command":"echo hi"}`,
	})
	assert.False(t, ok, "non-edit tool in allow-edits must not auto-resolve")
	assert.Equal(t, tools.PermissionDeny, d)
}

// TestResolvePermissionMode_AutoHasNoStaticOverride proves the model's answer
// is the WHOLE verdict: nothing in Go second-guesses it, in either direction.
//
// The commands here are the ones a static denylist would have refused. They
// run because the model said ALLOW. That is the design — and it is also the
// thing to check first if auto ever approves something it should not have,
// because the fix then belongs in guard.AutoApprovalPrompt, not here.
//
// `mkfs.ext4 /dev/sda1` USED to be in this list and now lives in
// TestResolvePermissionMode_AutoStillCannotCrossTheStructuralGate. It stopped
// being a policy call: internal/guard/storage.go grades destruction of a raw
// storage device as catastrophic, alongside `rm -rf /`, so it is refused
// before the model is consulted. The move is the point — this list is "things
// Go does not decide", and that command is now one Go does.
//
// `sudo rm -rf /etc` made the SAME move, for the same reason, one layer later.
// It was here because the destructive classifier could not see past the `sudo`
// prefix: `rm -rf /etc` graded catastrophic and `sudo rm -rf /etc` graded
// nothing at all, so the more privileged spelling was the one the model got to
// decide. internal/guard/prefixrunner.go closed that, and the row moved.
//
// Two entries migrating out of this list in two rounds is worth naming as a
// pattern rather than a coincidence: a command sitting here does NOT mean it is
// safe for the model to judge, only that Go currently declines to. Whenever the
// structural tier learns to see a shape, the corresponding row belongs in the
// other test — and the check on this one is whether every remaining row is
// still a genuine policy call.
func TestResolvePermissionMode_AutoHasNoStaticOverride(t *testing.T) {
	for _, shell := range []string{
		"systemctl stop nginx", "git push --force",
		"ssh host uptime",
	} {
		t.Run(shell, func(t *testing.T) {
			yesMan := einollm.NewFakeModel([]string{"ALLOW"}, nil)
			models := map[string]model.BaseChatModel{"default": yesMan}
			cs := &connSession{perm: &permModeState{}, defaultModel: "default"}
			cs.perm.set(guard.ModeAuto)
			d, ok := resolvePermissionMode(context.Background(), cs, models,
				&tools.PermissionRequest{Tool: "shell_run", Shell: shell})
			assert.True(t, ok, "no static layer may override the model's ALLOW")
			assert.Equal(t, tools.PermissionAllow, d)
		})
	}
	// ...and the same commands prompt when the model says so, which is what
	// makes the case above a statement about the design rather than about a
	// broken gate.
	for _, shell := range []string{"systemctl stop nginx", "git push --force"} {
		t.Run(shell+" (model asks)", func(t *testing.T) {
			cautious := einollm.NewFakeModel([]string{"ASK"}, nil)
			models := map[string]model.BaseChatModel{"default": cautious}
			cs := &connSession{perm: &permModeState{}, defaultModel: "default"}
			cs.perm.set(guard.ModeAuto)
			d, ok := resolvePermissionMode(context.Background(), cs, models,
				&tools.PermissionRequest{Tool: "shell_run", Shell: shell})
			assert.False(t, ok)
			assert.Equal(t, tools.PermissionDeny, d)
		})
	}
}

// TestResolvePermissionMode_AutoStillCannotCrossTheStructuralGate proves what
// the model does NOT get to decide. Catastrophic destruction is refused before
// the model is consulted, so an ALLOW cannot buy it.
//
// Both families are listed because they reach the same tier by different
// routes: mass DELETION through the rm-family classifier, and raw-STORAGE
// destruction through internal/guard/storage.go. A verification run measured
// the second one executing under a permissive profile — the tier table it was
// checked against only ever listed deletion programs, so table and gate agreed
// with each other while both were narrower than the threat.
//
// The `sudo`/`timeout` rows are the third route, and the one that mattered
// most: internal/guard/prefixrunner.go. A later verification run measured
// `sudo rm -rf /` reaching Allow outright while the un-prefixed spelling was
// refused — the more privileged form was the one that got through. Every row
// here is a shape a prior round of this same test asserted was the model's to
// judge.
func TestResolvePermissionMode_AutoStillCannotCrossTheStructuralGate(t *testing.T) {
	for _, shell := range []string{
		"rm -rf /",
		"mkfs.ext4 /dev/sda1",
		"dd if=/dev/zero of=/dev/disk0",
		`bash -c "dd if=/dev/zero of=/dev/disk0"`,
		"sudo rm -rf /",
		"sudo rm -rf /etc",
		"timeout 5 rm -rf /",
		"sudo dd if=/dev/zero of=/dev/disk0",
	} {
		t.Run(shell, func(t *testing.T) {
			yesMan := einollm.NewFakeModel([]string{"ALLOW"}, nil)
			models := map[string]model.BaseChatModel{"default": yesMan}
			cs := &connSession{perm: &permModeState{}, defaultModel: "default"}
			cs.perm.set(guard.ModeAuto)
			d, ok := resolvePermissionMode(context.Background(), cs, models,
				&tools.PermissionRequest{Tool: "shell_run", Shell: shell, Workdir: "/proj"})
			assert.True(t, ok, "catastrophic destruction resolves without asking the user")
			assert.Equal(t, tools.PermissionDeny, d, "and it resolves to DENY, whatever the model said")
		})
	}
}

// TestResolvePermissionMode_AutoAsksTheModel covers stage 2: for calls the
// denylist clears, the model's answer is the verdict.
func TestResolvePermissionMode_AutoAsksTheModel(t *testing.T) {
	cases := []struct {
		name      string
		reply     string
		wantAllow bool
	}{
		{"model allows", "ALLOW", true},
		{"model asks", "ASK", false},
		{"model is unreadable", "I am not sure about this one at all", false},
		{"model answers with prose containing allow", "I would not allow this", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fm := einollm.NewFakeModel([]string{c.reply}, nil)
			models := map[string]model.BaseChatModel{"default": fm}
			cs := &connSession{perm: &permModeState{}, defaultModel: "default"}
			cs.perm.set(guard.ModeAuto)
			d, ok := resolvePermissionMode(context.Background(), cs, models,
				&tools.PermissionRequest{Tool: "shell_run", Shell: "go build ./..."})
			if c.wantAllow {
				assert.True(t, ok)
				assert.Equal(t, tools.PermissionAllow, d)
				return
			}
			assert.False(t, ok, "anything but a clean ALLOW must prompt")
			assert.Equal(t, tools.PermissionDeny, d)
		})
	}
}

// TestResolvePermissionMode_AutoFailsToPrompting pins the error policy: auto
// degrades to manual, never to permissive. No model and a model error are the
// two ways stage 2 can fail to produce a verdict.
func TestResolvePermissionMode_AutoFailsToPrompting(t *testing.T) {
	t.Run("no model registered", func(t *testing.T) {
		cs := &connSession{perm: &permModeState{}}
		cs.perm.set(guard.ModeAuto)
		d, ok := resolvePermissionMode(context.Background(), cs, nil,
			&tools.PermissionRequest{Tool: "shell_run", Shell: "go build ./..."})
		assert.False(t, ok)
		assert.Equal(t, tools.PermissionDeny, d)
	})
	t.Run("model errors", func(t *testing.T) {
		fm := einollm.NewFakeModel(nil, errors.New("upstream 500"))
		models := map[string]model.BaseChatModel{"default": fm}
		cs := &connSession{perm: &permModeState{}, defaultModel: "default"}
		cs.perm.set(guard.ModeAuto)
		d, ok := resolvePermissionMode(context.Background(), cs, models,
			&tools.PermissionRequest{Tool: "shell_run", Shell: "go build ./..."})
		assert.False(t, ok)
		assert.Equal(t, tools.PermissionDeny, d)
	})
}

// TestAutoApprovalPromptCarriesSessionContext proves the session context is
// actually WIRED INTO the prompt, not merely correct in isolation.
//
// This exists because a mutation that hardcoded UserGoal to "" left every
// other test in this file green: latestUserMessage had its own passing test,
// resolvePermissionMode had its own passing tests, and nothing asserted the
// two were connected. Context the model never receives is context that does
// not exist.
func TestAutoApprovalPromptCarriesSessionContext(t *testing.T) {
	cs := &connSession{history: []*schema.Message{
		schema.UserMessage("please refactor the parser package"),
		schema.AssistantMessage("on it", nil),
	}}
	got := autoApprovalPromptFor(cs, tools.PermissionRequest{
		Tool:    "shell_run",
		Shell:   "go build ./...",
		Args:    `{"command":"go build ./..."}`,
		Workdir: "/proj",
		Reason:  "shell command not on allowlist",
	})
	for _, want := range []string{
		"please refactor the parser package", // the goal, via latestUserMessage
		"go build ./...",                     // the command being judged
		"/proj",                              // the in-scope boundary
		"shell command not on allowlist",     // why the static policy declined
	} {
		assert.Contains(t, got, want, "prompt must carry the session context")
	}
}

// TestLatestUserMessage proves the goal handed to the model is the most recent
// user turn, not the first and not an assistant turn. Getting this wrong would
// judge every call in a long session against the opening request.
func TestLatestUserMessage(t *testing.T) {
	cs := &connSession{history: []*schema.Message{
		schema.UserMessage("first request"),
		schema.AssistantMessage("working on it", nil),
		schema.UserMessage("actually, do this instead"),
		schema.AssistantMessage("ok", nil),
	}}
	assert.Equal(t, "actually, do this instead", cs.latestUserMessage())

	assert.Empty(t, (&connSession{}).latestUserMessage(), "no history yields no goal")
	assert.Empty(t, (&connSession{history: []*schema.Message{
		schema.AssistantMessage("hi", nil),
	}}).latestUserMessage(), "assistant-only history yields no goal")
}

func TestResolvePermissionMode_DefaultReturnsNotResolved(t *testing.T) {
	cs := &connSession{perm: &permModeState{}}
	d, ok := resolvePermissionMode(context.Background(), cs, nil, &tools.PermissionRequest{
		Tool: "fs_write", Args: `{}`,
	})
	assert.False(t, ok, "default mode must not auto-resolve")
	assert.Equal(t, tools.PermissionDeny, d)
}

func TestResolvePermissionMode_ForcePromptNotAutoResolved(t *testing.T) {
	for _, mode := range []guard.PermissionMode{guard.ModeYOLO, guard.ModeAuto} {
		t.Run(string(mode), func(t *testing.T) {
			cs := &connSession{perm: &permModeState{}}
			cs.perm.set(mode)
			d, ok := resolvePermissionMode(context.Background(), cs, nil, &tools.PermissionRequest{
				Tool: "dangerous_tool", ForcePrompt: true,
			})
			assert.False(t, ok, "ForcePrompt tool must not auto-resolve in mode %s", mode)
			assert.Equal(t, tools.PermissionDeny, d)
		})
	}
}

func TestResolvePermissionMode_ApprovalRequiredNotAutoResolved(t *testing.T) {
	cs := &connSession{perm: &permModeState{}}
	cs.perm.set(guard.ModeYOLO)
	d, ok := resolvePermissionMode(context.Background(), cs, nil, &tools.PermissionRequest{
		Tool: "github_push", ApprovalRequired: true,
	})
	assert.False(t, ok, "ApprovalRequired tool must not auto-resolve in YOLO mode")
	assert.Equal(t, tools.PermissionDeny, d)
}

func TestResolvePermissionMode_AutoNoModelFallsThrough(t *testing.T) {
	cs := &connSession{perm: &permModeState{}}
	cs.perm.set(guard.ModeAuto)
	d, ok := resolvePermissionMode(context.Background(), cs, nil, &tools.PermissionRequest{
		Tool: "fs_write", Args: `{}`,
	})
	assert.False(t, ok, "auto mode with no model must fall through to prompt")
	assert.Equal(t, tools.PermissionDeny, d)
}

// ---------------------------------------------------------------------------
// ConnSession helper unit tests
// ---------------------------------------------------------------------------

func TestConnSession_RecordingSuppressedTrue(t *testing.T) {
	cs := &connSession{perm: &permModeState{}}
	assert.False(t, cs.recordingSuppressed())
	cs.sideStack = append(cs.sideStack, sideSnapshot{})
	assert.True(t, cs.recordingSuppressed())
}

func TestConnSession_DisplayModel(t *testing.T) {
	cs := &connSession{perm: &permModeState{}, defaultModel: "default", model: ""}
	assert.Equal(t, "default", cs.displayModel())
	cs.model = "selected"
	assert.Equal(t, "selected", cs.displayModel())
}

func TestConnSession_SelectModel(t *testing.T) {
	cs := &connSession{perm: &permModeState{}}
	assert.Nil(t, cs.selectModel(nil))
	assert.Nil(t, cs.selectModel(map[string]model.BaseChatModel{}))
	cs.model = "absent"
	assert.Nil(t, cs.selectModel(map[string]model.BaseChatModel{}))
	fm := einollm.NewFakeModel(nil, nil)
	cs.model = "present"
	assert.NotNil(t, cs.selectModel(map[string]model.BaseChatModel{"present": fm}))
}

func TestConnSession_StatusFrame_Defaults(t *testing.T) {
	cs := &connSession{perm: &permModeState{}, startedAt: time.Now()}
	srv := New(Config{})
	st := cs.statusFrame(srv)
	assert.Equal(t, "status", st.Type)
	assert.Zero(t, st.TokensIn)
	assert.Zero(t, st.TokensOut)
}

func TestConnSession_EnterSideMaxDepth(t *testing.T) {
	cs := &connSession{perm: &permModeState{}}
	for i := 0; i < maxSideDepth; i++ {
		assert.NoError(t, cs.enterSide(), "enterSide at depth %d must succeed", i)
	}
	err := cs.enterSide()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "side depth limit")
}

func TestConnSession_ExitSideEmpty(t *testing.T) {
	cs := &connSession{perm: &permModeState{}, startedAt: time.Now()}
	cs.exitSide() // must not panic
}

// ---------------------------------------------------------------------------
// WS end-to-end tests for ChatWS control frames
// ---------------------------------------------------------------------------

func TestChatWS_FeaturesSetError(t *testing.T) {
	reg := features.NewRegistry(true)
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t", FeaturesReg: reg})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewFeaturesSet("", true)))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
}

func TestChatWS_FeaturesSetNilPayload(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.ClientFrame{Type: "features_set"}))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
}

func TestChatWS_JobsListNoManager(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewJobsList()))
	f := readFrame(t, c)
	assert.Equal(t, "jobs", f.Type)
	assert.Empty(t, f.Jobs)
}

func TestChatWS_JobsListWithManager(t *testing.T) {
	sm := shell.NewManager(shell.Config{})
	o, err := orchestrator.New(orchestrator.Config{
		Model:   einollm.NewFakeModel([]string{"x"}, nil),
		Profile: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}},
	})
	require.NoError(t, err)
	srv := New(Config{Token: "t", ShellManager: sm})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewJobsList()))
	f := readFrame(t, c)
	assert.Equal(t, "jobs", f.Type)
	assert.Empty(t, f.Jobs)
}

func TestChatWS_JobReadNoManager(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.ClientFrame{Type: "job_read", ID: "j1"}))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
}

func TestChatWS_JobWriteNoManager(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.ClientFrame{Type: "job_write", ID: "j1", Text: "input"}))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
}

func TestChatWS_JobCancelNoManager(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.ClientFrame{Type: "job_cancel", ID: "j1"}))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
}

func TestChatWS_EnterSideMaxDepth(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	for i := 0; i < maxSideDepth+1; i++ {
		require.NoError(t, c.WriteJSON(proto.NewEnterSide()))
		f := readFrame(t, c)
		if i < maxSideDepth {
			assert.Equal(t, "side_state", f.Type)
			assert.Equal(t, i+1, f.SideDepth)
		} else {
			assert.Equal(t, "error", f.Type)
		}
	}
}

func TestChatWS_ExitSideEmptyStack(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewExitSide()))
	f := readFrame(t, c)
	assert.Equal(t, "side_state", f.Type)
	assert.Equal(t, 0, f.SideDepth)
}

func TestChatWS_MCPActionEnableDisable(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.ClientFrame{Type: "mcp_action", MCPServer: "test", MCPAction: "enable"}))
	f := readFrame(t, c)
	assert.Equal(t, "mcp_status", f.Type)
}

func TestChatWS_SessionListWithStore(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	defer st.Close()
	sid, err := st.CreateSession("test-session")
	require.NoError(t, err)

	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t", Store: st})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewSessionList()))
	f := readFrame(t, c)
	assert.Equal(t, "sessions", f.Type)
	assert.NotEmpty(t, f.Sessions)
	assert.Equal(t, sid, f.Sessions[0].ID)
}

func TestChatWS_SessionListStoreError(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewSessionList()))
	f := readFrame(t, c)
	assert.Equal(t, "sessions", f.Type)
	assert.Empty(t, f.Sessions)
}

// ---------------------------------------------------------------------------
// ensureSession / persistMessages — uncovered error branches
// ---------------------------------------------------------------------------

func TestEnsureSession_NilStore(t *testing.T) {
	srv := &Server{}
	cs := &connSession{perm: &permModeState{}}
	cs.ensureSession(srv, "test")
}

func TestEnsureSession_AlreadyExists(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	defer st.Close()
	cs := &connSession{perm: &permModeState{}, sessionID: "existing"}
	srv := &Server{store: st}
	cs.ensureSession(srv, "should-not-create")
	assert.Equal(t, "existing", cs.sessionID)
}

func TestEnsureSession_CreateSessionError(t *testing.T) {
	cs := &connSession{perm: &permModeState{}}
	cs.ensureSession(&Server{}, "test-title")
	assert.Empty(t, cs.sessionID)
}

func TestPersistMessages_NilStore(t *testing.T) {
	cs := &connSession{perm: &permModeState{}}
	cs.history = []*schema.Message{schema.UserMessage("user msg")}
	cs.persistMessages(&Server{})
}

func TestPersistMessages_RecordingSuppressed(t *testing.T) {
	cs := &connSession{perm: &permModeState{}, sessionID: "s1"}
	cs.sideStack = []sideSnapshot{{}}
	cs.history = []*schema.Message{schema.UserMessage("user")}
	cs.persistMessages(&Server{})
}

func TestPersistMessages_AppendError(t *testing.T) {
	cs := &connSession{perm: &permModeState{}, sessionID: "nonexistent"}
	cs.history = []*schema.Message{schema.UserMessage("user")}
	cs.persistMessages(&Server{})
}

// ---------------------------------------------------------------------------
// loadSession — uncovered error branches
// ---------------------------------------------------------------------------

func TestLoadSession_NilStore(t *testing.T) {
	cs := &connSession{perm: &permModeState{}}
	err := cs.loadSession(&Server{}, "some-id")
	assert.NoError(t, err)
}

func TestLoadSession_GetSessionError(t *testing.T) {
	cs := &connSession{perm: &permModeState{}}
	err := cs.loadSession(&Server{}, "some-id")
	assert.NoError(t, err) // nil store is no-op
}

// ---------------------------------------------------------------------------
// isLoopback — SplitHostPort error path
// ---------------------------------------------------------------------------

func TestIsLoopback_BadAddr(t *testing.T) {
	assert.False(t, isLoopback("not-a-valid-addr"))
	assert.False(t, isLoopback(""))
}

// ---------------------------------------------------------------------------
// authorizeControlAction — approval manager path
// ---------------------------------------------------------------------------

func TestAuthorizeControlAction_WithApprovals(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{
		Model:   einollm.NewFakeModel([]string{"x"}, nil),
		Profile: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}},
	})
	require.NoError(t, err)
	am, err := approval.New(nil, "test-conn", nil)
	require.NoError(t, err)
	srv := New(Config{Token: "t", Approvals: am})
	srv.ChatWS(o, nil, nil)

	err = srv.authorizeControlAction(context.Background(), "test-session", nil, "task_shell_stdin")
	assert.NoError(t, err)
}

// ---------------------------------------------------------------------------
// scopeJSON
// ---------------------------------------------------------------------------

func TestScopeJSON_Normal(t *testing.T) {
	s := scopeJSON(approval.Scope{Tool: "fs_write"})
	assert.Contains(t, s, "fs_write")
}

// ---------------------------------------------------------------------------
// compileSchema
// ---------------------------------------------------------------------------

func TestCompileSchema_InvalidDocument(t *testing.T) {
	_, err := compileSchema(json.RawMessage(`not valid json`))
	assert.Error(t, err)
}

func TestCompileSchema_ValidDocument(t *testing.T) {
	doc := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`)
	sch1, err := compileSchema(doc)
	require.NoError(t, err)
	require.NotNil(t, sch1)

	sch2, err := compileSchema(doc)
	require.NoError(t, err)
	assert.Same(t, sch1, sch2)
}

// ---------------------------------------------------------------------------
// Sessions — uncovered error branches
// ---------------------------------------------------------------------------

func TestSessions_GetNotFound(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	defer st.Close()

	s := New(Config{Token: "tok"})
	s.Sessions(st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := ts.Client().Do(doAuthed(t, "GET", ts.URL+"/api/v1/sessions/nonexistent", "tok"))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// Job read error path via WS
// ---------------------------------------------------------------------------

func TestJobRead_NotFound(t *testing.T) {
	sm := shell.NewManager(shell.Config{})
	o, err := orchestrator.New(orchestrator.Config{
		Model:   einollm.NewFakeModel([]string{"x"}, nil),
		Profile: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}},
	})
	require.NoError(t, err)
	srv := New(Config{Token: "t", ShellManager: sm})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.ClientFrame{Type: "job_read", ID: "nonexistent", Text: "100"}))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
}

// ---------------------------------------------------------------------------
// HandleRestoreSession with a real store but missing session
// ---------------------------------------------------------------------------

func TestHandleRestoreSession_MissingSession(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	defer st.Close()

	srv := New(Config{Store: st})
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewRestoreSession("nonexistent")))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
}

// ---------------------------------------------------------------------------
// Skill mutation test via WS
// ---------------------------------------------------------------------------

func TestHandleSkillMutation_NilRegistryWS(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Token: "t"})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewEnableSkill("test")))
	f := readFrame(t, c)
	assert.Equal(t, "skill_ack", f.Type)
	assert.NotEmpty(t, f.Text)
}

// ---------------------------------------------------------------------------
// Uninstall non-user skill via WS
// ---------------------------------------------------------------------------

func TestUninstallNonUserSkillWS(t *testing.T) {
	builtinRoot := t.TempDir()
	_ = os.MkdirAll(filepath.Join(builtinRoot, "built"), 0o755)
	_ = os.WriteFile(filepath.Join(builtinRoot, "built", "SKILL.md"),
		[]byte("---\nname: built\nsource: builtin\n---\n# x"), 0o644)

	loader := skills.NewLoader(skills.Builtin(builtinRoot))
	reg, err := loader.Load()
	require.NoError(t, err)

	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"ok"}, nil)})
	require.NoError(t, err)

	srv := New(Config{
		Token:          "t",
		SkillsRegistry: reg,
		SkillsLoader:   loader,
		SkillsDstRoot:  t.TempDir(),
	})
	srv.ChatWS(o, nil, reg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewUninstallSkill("built")))
	f := readFrame(t, c)
	assert.Equal(t, "skill_ack", f.Type)
	assert.Equal(t, "uninstalled", f.Action)
	assert.NotEmpty(t, f.Text)
}

// ---------------------------------------------------------------------------
// Enable non-existent skill via WS
// ---------------------------------------------------------------------------

func TestEnableNonexistentSkillWS(t *testing.T) {
	builtinRoot := t.TempDir()
	_ = os.MkdirAll(filepath.Join(builtinRoot, "known"), 0o755)
	_ = os.WriteFile(filepath.Join(builtinRoot, "known", "SKILL.md"),
		[]byte("---\nname: known\n---\n# x"), 0o644)

	loader := skills.NewLoader(skills.Builtin(builtinRoot))
	reg, err := loader.Load()
	require.NoError(t, err)

	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"ok"}, nil)})
	require.NoError(t, err)

	srv := New(Config{Token: "t", SkillsRegistry: reg})
	srv.ChatWS(o, nil, reg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewEnableSkill("nonexistent")))
	f := readFrame(t, c)
	assert.Equal(t, "skill_ack", f.Type)
	assert.NotEmpty(t, f.Text)
}

// ---------------------------------------------------------------------------
// ListSeams with empty session (VCS configured, no messages yet)
// ---------------------------------------------------------------------------

func TestListSeamsEmptySessionWS(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(root, 0o755))
	st, err := store.Open(filepath.Join(base, "test.db"))
	require.NoError(t, err)
	defer st.Close()
	v := vcs.New(st, filepath.Join(base, "worktrees"))
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"ok"}, nil)})
	require.NoError(t, err)
	srv := New(Config{Store: st, VCS: v, RepoID: repoID})
	srv.ChatWS(o, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewListSeams()))
	f := readFrame(t, c)
	assert.Equal(t, "seams", f.Type)
}

// ---------------------------------------------------------------------------
// CompactNow via WS end-to-end
// ---------------------------------------------------------------------------

func TestCompactNow_WithCompaction(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"COMPACT_SUMMARY"}, nil)
	models := map[string]model.BaseChatModel{"fm": fm}
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)
	srv := New(Config{Token: "t", Compaction: CompactionConfig{Model: "fm", KeepRecent: 1}})
	srv.ChatWS(o, models, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	long := strings.Repeat("x", 100)
	require.NoError(t, c.WriteJSON(proto.NewUserMessage(long)))
	recvTurn(t, c)
	require.NoError(t, c.WriteJSON(proto.NewUserMessage(long)))
	recvTurn(t, c)

	require.NoError(t, c.WriteJSON(proto.NewCompact()))
	var sawStatus bool
	for {
		f := readFrame(t, c)
		if f.Type == "status" {
			sawStatus = true
			break
		}
		if f.Type == "compact_chunk" {
			continue
		}
	}
	assert.True(t, sawStatus)
}
