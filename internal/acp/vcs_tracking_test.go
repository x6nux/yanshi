package acp

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
)

// TestSetVCSTracking_StoresValues verifies that SetVCSTracking records the
// worktreeID and recorder callback on the Client.
func TestSetVCSTracking_StoresValues(t *testing.T) {
	c := NewClient(new(bytes.Buffer), new(bytes.Buffer))

	if c.worktreeID != "" || c.recordEdit != nil {
		t.Fatalf("zero Client should have no tracking; got worktreeID=%q recordEdit-set=%v", c.worktreeID, c.recordEdit != nil)
	}

	var called int
	c.SetVCSTracking("wt-1", func(worktreeID, agent, absPath string, content []byte) error {
		called++
		return nil
	})

	if c.worktreeID != "wt-1" {
		t.Errorf("c.worktreeID = %q; want \"wt-1\"", c.worktreeID)
	}
	if c.recordEdit == nil {
		t.Fatal("c.recordEdit = nil; want callback")
	}
	// Sanity: the stored callback is callable.
	if err := c.recordEdit("wt-1", "acp", "/x", []byte("y")); err != nil {
		t.Fatalf("stored recorder returned error: %v", err)
	}
	if called != 1 {
		t.Errorf("recorder called %d times; want 1", called)
	}
}

// TestApplyDiffContent_TracksToRecorder drives Client.handleNotify with a
// crafted session/update carrying a tool_call diff block and asserts the VCS
// recorder callback fires with the worktreeID, resolved absolute path, and
// written content. It also verifies the file is materialized on disk.
func TestApplyDiffContent_TracksToRecorder(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "hello.txt")

	c := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	// Policy must allow writes under dir for applyDiffContent to proceed.
	c.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs.write"}},
		FS:    guard.FSPerm{Write: []string{filepath.Join(dir, "**")}},
	}))

	var (
		mu      sync.Mutex
		gotWtID string
		gotAg   string
		gotPath string
		gotBody []byte
		gotCalls int
	)
	c.SetVCSTracking("wt-1", func(worktreeID, agent, absPath string, content []byte) error {
		mu.Lock()
		defer mu.Unlock()
		gotWtID, gotAg, gotPath, gotBody = worktreeID, agent, absPath, content
		gotCalls++
		return nil
	})

	// Build a session/update notification params with a tool_call diff block.
	// applyDiffContent re-parses the raw params from a probe struct that
	// includes the diff fields (path/newText), so we marshal via a map to
	// include those fields that ContentBlock doesn't carry.
	raw, err := json.Marshal(map[string]any{
		"sessionId": "sess_test",
		"update": map[string]any{
			"sessionUpdate": "tool_call",
			"toolCallId":    "tc_1",
			"kind":          "edit",
			"content": []map[string]any{
				{"type": "diff", "path": target, "newText": "hi"},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal update: %v", err)
	}

	c.handleNotify("session/update", json.RawMessage(raw))

	mu.Lock()
	defer mu.Unlock()

	if gotCalls != 1 {
		t.Fatalf("recorder calls = %d; want 1", gotCalls)
	}
	if gotWtID != "wt-1" {
		t.Errorf("recorder worktreeID = %q; want \"wt-1\"", gotWtID)
	}
	if gotAg != "acp" {
		t.Errorf("recorder agent = %q; want \"acp\"", gotAg)
	}
	if gotPath != target {
		t.Errorf("recorder absPath = %q; want %q", gotPath, target)
	}
	if string(gotBody) != "hi" {
		t.Errorf("recorder content = %q; want \"hi\"", string(gotBody))
	}

	// The file should also have been materialized on disk.
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read materialized file: %v", err)
	}
	if string(body) != "hi" {
		t.Errorf("disk content = %q; want \"hi\"", string(body))
	}
}

// TestApplyDiffContent_NoTrackingIsNoOp verifies that when VCS tracking is not
// configured (zero Client), applyDiffContent still writes the file but does not
// invoke any recorder (and does not panic).
func TestApplyDiffContent_NoTrackingIsNoOp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "silent.txt")

	// No SetVCSTracking call: worktreeID=="", recordEdit==nil.
	c := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	c.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs.write"}},
		FS:    guard.FSPerm{Write: []string{filepath.Join(dir, "**")}},
	}))

	raw, err := json.Marshal(map[string]any{
		"sessionId": "sess_test",
		"update": map[string]any{
			"sessionUpdate": "tool_call",
			"toolCallId":    "tc_2",
			"kind":          "edit",
			"content": []map[string]any{
				{"type": "diff", "path": target, "newText": "ok"},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal update: %v", err)
	}

	// Must not panic even though recordEdit is nil.
	c.handleNotify("session/update", json.RawMessage(raw))

	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read materialized file: %v", err)
	}
	if string(body) != "ok" {
		t.Errorf("disk content = %q; want \"ok\"", string(body))
	}
}
