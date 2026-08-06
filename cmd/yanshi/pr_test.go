package main

import (
	"context"
	"io"
	"os"
	"strings"
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

// TestRunPRBindsASubAgentRunner asserts the thing runPR silently lacked.
//
// The review tool reads its sub-agent runner from a context value that only a
// turn binds. runPR passed the bare process context, so streamReview answered
// "review requires a bound sub-agent runner" on every invocation and runPR
// printed that string and returned exitOK -- the subcommand had never worked,
// and its test documented the failure as the expected path.
//
// Asserting on the OUTPUT rather than on the context is deliberate: any future
// refactor that drops the App, changes the binding, or reorders the calls
// reproduces the original symptom, and that symptom is a specific string the
// user sees.
func TestRunPRBindsASubAgentRunner(t *testing.T) {
	withPRConfig(t)
	withFakeGH(t, func(ctx context.Context, args ...string) (string, string, error) {
		if len(args) > 1 && args[1] == "view" {
			return `{"number":1,"title":"T","body":"b","headRefName":"h","baseRefName":"main","author":{"login":"a"},"files":[{"path":"f.go","additions":1,"deletions":0}],"changedFiles":1}`, "", nil
		}
		return "+diff content", "", nil
	})

	out := captureStdout(t, func() {
		if code := runPR(context.Background(), "https://github.com/o/r/pull/1"); code != exitOK {
			t.Fatalf("runPR = %d", code)
		}
	})
	if strings.Contains(out, "requires a bound sub-agent runner") {
		t.Fatalf("the review tool still has no runner bound:\n%s", out)
	}
}

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = prev
	return <-done
}
