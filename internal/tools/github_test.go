package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/secproc"
)

func TestGitHubPRContextParsesGHJSON(t *testing.T) {
	raw := `{"number":42,"title":"Fix bug","body":"The fix","headRefName":"fix-branch","baseRefName":"main","author":{"login":"alice"},"files":[{"path":"main.go","additions":10,"deletions":2}],"changedFiles":1}`
	ghCtx, err := FetchGitHubContext("owner/repo", 42, raw)
	if err != nil {
		t.Fatal(err)
	}
	if ghCtx.Number != 42 || ghCtx.Title != "Fix bug" || ghCtx.Author != "alice" {
		t.Fatalf("ghCtx=%+v", ghCtx)
	}
	if len(ghCtx.Files) != 1 || ghCtx.Files[0].Path != "main.go" {
		t.Fatalf("files=%+v", ghCtx.Files)
	}
}

func TestGitHubPRContextRejectsInvalidJSON(t *testing.T) {
	_, err := FetchGitHubContext("owner/repo", 1, `not json`)
	if err == nil || !strings.Contains(err.Error(), "parse GitHub") {
		t.Fatalf("err=%v", err)
	}
}

func TestGitHubCommentRequiresApproval(t *testing.T) {
	// SSE path: no callback → denied
	tool := NewGitHubTools(nil).Comment
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		Net:   guard.NetPerm{Allow: true},
	})
	out, err := tool.InvokableRun(ctx, `{"repo":"owner/repo","number":1,"body":"LGTM"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "permission denied") {
		t.Fatalf("out=%s", out)
	}
}

func TestGitHubCommentApprovalAllow(t *testing.T) {
	var last secproc.SecureProcessSpec
	factory := newScriptedFactory(t, func(s secproc.SecureProcessSpec) cannedResult {
		last = s
		// Real `gh pr comment` returns a URL string (not JSON); mock matches.
		return cannedResult{Stdout: "https://github.com/owner/repo/issues/1#issuecomment-123"}
	})
	ctx := WithSecureProcessFactory(WithPermissionCallback(
		WithProfile(context.Background(), guard.PermissionProfile{
			Tools: guard.ToolsPerm{Allow: []string{"*"}},
			Net:   guard.NetPerm{Allow: true},
		}),
		func(req PermissionRequest) PermissionDecision {
			if req.Tool != "github_comment" || req.ApprovalRequired != true {
				t.Fatalf("bad req=%+v", req)
			}
			return PermissionAllow
		}), factory)
	tool := NewGitHubTools(nil).Comment
	out, err := tool.InvokableRun(ctx, `{"repo":"owner/repo","number":1,"body":"LGTM"}`)
	if err != nil {
		t.Fatal(err)
	}
	// runGitHubComment wraps gh's trimmed stdout as a string ID; check it round-trips.
	if !strings.Contains(out, "123") {
		t.Fatalf("out=%s", out)
	}
	if last.Program != "gh" || !strings.Contains(strings.Join(last.Args, " "), "pr comment") {
		t.Fatalf("args=%+v", last.Args)
	}
}

func TestGitHubMergeRejectsUnknownMethod(t *testing.T) {
	// ApprovalGuardedTool with callback allow so the merge logic runs and rejects "xyz"
	ctx := WithPermissionCallback(
		WithProfile(context.Background(), allowAllProfile()),
		func(req PermissionRequest) PermissionDecision { return PermissionAllow })
	tool := NewGitHubTools(nil).Merge
	out, err := tool.InvokableRun(ctx, `{"repo":"owner/repo","number":1,"method":"xyz"}`)
	if err != nil {
		t.Fatal(err)
	}
	// The result should mention "xyz" as unknown
	var res struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(out), &res)
	if !strings.Contains(out, "xyz") {
		t.Fatalf("out=%s", out)
	}
}
