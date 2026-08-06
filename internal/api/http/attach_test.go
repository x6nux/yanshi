package http

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
)

// TestResolveAttachmentsIsFailClosed is written before the resolver, and the
// negative cases are written before the positive one, because this path is
// arbitrary file reading triggered by user TEXT and executed outside any tool
// context — there is no permission callback to escalate to, so every doubt has
// to resolve to "no".
func TestResolveAttachmentsIsFailClosed(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "notes.md")
	if err := os.WriteFile(inside, []byte("hello from inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	allowAll := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_read"}},
		FS:    guard.FSPerm{Read: []string{filepath.Join(root, "**")}},
	}

	t.Run("a path outside the work root is refused", func(t *testing.T) {
		out, refused := resolveAttachments(root, allowAll,
			[]proto.AttachRef{{Path: outside}})
		if len(refused) == 0 {
			t.Fatal("an out-of-root path was accepted")
		}
		if strings.Contains(out, "SECRET") {
			t.Fatalf("the file's CONTENT leaked into the prompt: %q", out)
		}
	})

	t.Run("traversal out of the root is refused", func(t *testing.T) {
		rel := filepath.Join(root, "..", filepath.Base(outsideDir), "secret.txt")
		out, refused := resolveAttachments(root, allowAll, []proto.AttachRef{{Path: rel}})
		if len(refused) == 0 {
			t.Fatal("a ../ traversal was accepted")
		}
		if strings.Contains(out, "SECRET") {
			t.Fatalf("content leaked through traversal: %q", out)
		}
	})

	t.Run("guard Prompt counts as refusal, not as an escalation", func(t *testing.T) {
		// An empty Read allowlist makes checkFS answer Prompt rather than
		// HardDeny. There is no callback here, so treating Prompt as anything
		// but "no" would silently read a file the profile did not allow.
		promptProfile := guard.PermissionProfile{
			Tools: guard.ToolsPerm{Allow: []string{"fs_read"}},
			FS:    guard.FSPerm{Read: []string{filepath.Join(root, "nothing", "**")}},
		}
		out, refused := resolveAttachments(root, promptProfile, []proto.AttachRef{{Path: inside}})
		if len(refused) == 0 {
			t.Fatal("a Prompt verdict was treated as permission")
		}
		if strings.Contains(out, "hello from inside") {
			t.Fatalf("content read despite a non-Allow verdict: %q", out)
		}
	})

	t.Run("a file over the per-file cap is described, not read", func(t *testing.T) {
		big := filepath.Join(root, "big.txt")
		if err := os.WriteFile(big, make([]byte, maxAttachBytes+1), 0o644); err != nil {
			t.Fatal(err)
		}
		out, refused := resolveAttachments(root, allowAll, []proto.AttachRef{{Path: big}})
		if len(refused) != 1 {
			t.Fatalf("an oversized file was read wholesale: refused=%v", refused)
		}
		if !strings.Contains(strings.ToLower(refused[0]), "fs_read") {
			t.Errorf("the refusal should point at fs_read for chunked access: %q", refused[0])
		}
		if len(out) > maxAttachBytes {
			t.Errorf("output is %d bytes: the cap did not hold", len(out))
		}
	})

	t.Run("the per-turn total is capped across several files", func(t *testing.T) {
		var refs []proto.AttachRef
		each := maxAttachBytes / 2
		for i := 0; i < (maxTurnAttachBytes/each)+2; i++ {
			p := filepath.Join(root, "chunk", filepath.Base(t.Name())+string(rune('a'+i))+".txt")
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, make([]byte, each), 0o644); err != nil {
				t.Fatal(err)
			}
			refs = append(refs, proto.AttachRef{Path: p})
		}
		out, refused := resolveAttachments(root, allowAll, refs)
		if len(out) > maxTurnAttachBytes+maxAttachBytes {
			t.Errorf("turn total is %d bytes, cap is %d: one message can eat the "+
				"whole context budget", len(out), maxTurnAttachBytes)
		}
		if len(refused) == 0 {
			t.Error("nothing was refused even though the turn total was exceeded")
		}
	})

	t.Run("the jail holds even when the profile allows everything", func(t *testing.T) {
		// The realistic misconfiguration, and the case that isolates the jail.
		// With a narrow Read glob the guard refuses an out-of-root path on its
		// own, so removing pathjail entirely leaves the earlier subtests green
		// — a mutation probe confirmed exactly that. A profile with Read: **
		// is one line in config.yaml, and past it the jail is the only thing
		// standing between "@" in a chat message and the whole filesystem.
		wideOpen := guard.PermissionProfile{
			Tools: guard.ToolsPerm{Allow: []string{"fs_read"}},
			FS:    guard.FSPerm{Read: []string{"**"}},
		}
		out, refused := resolveAttachments(root, wideOpen, []proto.AttachRef{{Path: outside}})
		if len(refused) == 0 {
			t.Fatal("an out-of-root path was read under a permissive profile: the root " +
				"jail is not being consulted")
		}
		if strings.Contains(out, "SECRET") {
			t.Fatalf("content escaped the work root: %q", out)
		}
		// Positive control on the same profile, or "refuses everything" would pass.
		out, refused = resolveAttachments(root, wideOpen, []proto.AttachRef{{Path: inside}})
		if len(refused) != 0 || !strings.Contains(out, "hello from inside") {
			t.Fatalf("the permissive profile also blocked an in-root file: %v / %q", refused, out)
		}
	})

	t.Run("an in-root allowed file IS read", func(t *testing.T) {
		// The positive control. Without it every assertion above is satisfied
		// by a resolver that refuses everything.
		out, refused := resolveAttachments(root, allowAll, []proto.AttachRef{{Path: inside}})
		if len(refused) != 0 {
			t.Fatalf("a legitimate in-root read was refused: %v", refused)
		}
		if !strings.Contains(out, "hello from inside") {
			t.Fatalf("the file was not read: %q", out)
		}
		if !strings.Contains(out, "notes.md") {
			t.Errorf("the attachment is not labelled with its path: %q", out)
		}
	})
}

// TestWSTurnPrependsAttachmentContent is the wiring half.
//
// The resolver's unit tests prove it refuses and reads correctly; they cannot
// prove anything calls it. This drives a real WS turn with an attachment and
// reads back what the model was handed — which is the only place the feature
// is either present or absent.
func TestWSTurnPrependsAttachmentContent(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("MARKER-IN-FILE"), 0o644); err != nil {
		t.Fatal(err)
	}

	fm := einollm.NewFakeModel([]string{"ok"}, nil)
	fm.RecordMessages = true
	o, err := orchestrator.New(orchestrator.Config{
		Model:    fm,
		WorkRoot: root,
		Profile: guard.PermissionProfile{
			Tools: guard.ToolsPerm{Allow: []string{"fs_read"}},
			FS:    guard.FSPerm{Read: []string{filepath.Join(root, "**")}},
		},
	})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessageWithAttachments(
		"summarize @notes.md",
		[]proto.AttachRef{{Path: filepath.Join(root, "notes.md")}})))
	for {
		if recvFrame(t, c).Type == "done" {
			break
		}
	}

	var seen string
	for _, msg := range fm.ReceivedMessages {
		seen += msg.Content
	}
	if !strings.Contains(seen, "MARKER-IN-FILE") {
		t.Fatalf("the attachment never reached the model:\n%s", seen)
	}
	if !strings.Contains(seen, "summarize @notes.md") {
		t.Error("the user's own text was lost")
	}
}
