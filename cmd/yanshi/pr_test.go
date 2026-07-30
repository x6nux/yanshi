package main

import (
	"testing"

	"github.com/x6nux/yanshi/internal/tools"
)

func TestFetchGitHubContextParsesComplete(t *testing.T) {
	raw := `{"number":7,"title":"Feat X","body":"","headRefName":"feat-x","baseRefName":"main","author":{"login":"bob"},"files":[{"path":"a.go","additions":5,"deletions":0}],"changedFiles":1}`
	ctx, err := tools.FetchGitHubContext("owner/repo", 7, raw)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Number != 7 || ctx.Title != "Feat X" || ctx.Author != "bob" {
		t.Fatalf("ctx=%+v", ctx)
	}
	if len(ctx.Files) != 1 || ctx.Files[0].Path != "a.go" {
		t.Fatalf("files=%+v", ctx.Files)
	}
}

func TestParsePRInputNumber(t *testing.T) {
	repo, number := parsePRInput("42")
	if number != 42 {
		t.Fatalf("number=%d", number)
	}
	_ = repo // may be empty when git remote is unavailable
}

func TestParsePRInputURL(t *testing.T) {
	repo, number := parsePRInput("https://github.com/owner/repo/pull/42")
	if repo != "owner/repo" || number != 42 {
		t.Fatalf("repo=%q number=%d", repo, number)
	}
}
