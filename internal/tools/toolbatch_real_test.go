package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/toolreg"
)

// This file is the REAL-TOOL, REAL-SIDE-EFFECT half of the T12 verification.
//
// The existing batch tests use echo tools that append to a slice. Those pin the
// control flow correctly, but a slice entry and a file on disk are not the same
// evidence. A batch that authorized correctly, recorded a denial, and reported
// a clean stop -- while the refused step had already written its file -- would
// satisfy every one of them.
//
// So these drive fs_write and fs_read, the real registered tools, through a
// real path jail, and the assertion is what is on the filesystem afterwards.
// That is the only observation that distinguishes "refused" from "refused, but
// it happened anyway".

// realBatchEnv is a work root plus the batch tool bound over the real fs tools.
type realBatchEnv struct {
	root  string
	batch *ToolBatchTool
	fs    *FSTools
}

// newRealBatchEnv builds fs_write / fs_read / fs_list over a temp work root
// and binds tool_batch across them, exactly as the composition root does.
func newRealBatchEnv(t *testing.T) *realBatchEnv {
	t.Helper()
	root := t.TempDir()
	fs := NewFSTools(root)
	b := NewToolBatchTool()
	b.Bind([]tool.InvokableTool{fs.Write, fs.Read, fs.List, b.Tool})
	if !b.Bound() {
		t.Fatal("Bind did not mark the tool bound")
	}
	return &realBatchEnv{root: root, batch: b, fs: fs}
}

// ctx binds a profile allowing the named tools, with reads and writes confined
// to the work root, and registers the full real tool set with toolreg.
func (e *realBatchEnv) ctx(allowTools []string) context.Context {
	ctx := WithWorkRoot(context.Background(), e.root)
	ctx = WithProfile(ctx, guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: allowTools},
		FS: guard.FSPerm{
			Read:  []string{e.root, e.root + "/**"},
			Write: []string{e.root, e.root + "/**"},
		},
	})
	return toolreg.WithRegistered(ctx, []string{"fs_write", "fs_read", "fs_list", "tool_batch"})
}

// run invokes tool_batch with the given program and decodes the report.
func (e *realBatchEnv) run(t *testing.T, ctx context.Context, program string) BatchReport {
	t.Helper()
	args, err := json.Marshal(map[string]string{"steps": program})
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.batch.Tool.InvokableRun(ctx, string(args))
	if err != nil {
		t.Fatalf("tool_batch returned a Go error, which aborts the turn: %v", err)
	}
	var rep BatchReport
	if uerr := json.Unmarshal([]byte(out), &rep); uerr != nil {
		t.Fatalf("batch report is not JSON (%v): %s", uerr, out)
	}
	return rep
}

// exists reports whether name exists under the work root.
func (e *realBatchEnv) exists(name string) bool {
	_, err := os.Stat(filepath.Join(e.root, name))
	return err == nil
}

// TestBatchReal_ChainedStepsUseTheEarlierResult drives the feature's whole
// reason to exist -- a later step consuming an earlier step's output -- with
// real tools, and checks the result on disk.
//
// Step 0 writes a file, step 1 reads it back, step 2 writes a THIRD file whose
// content is a reference to step 1's result. If substitution silently produced
// the literal text "$1", the final file would contain that instead, so the
// assertion is on the file's bytes rather than on the report.
func TestBatchReal_ChainedStepsUseTheEarlierResult(t *testing.T) {
	e := newRealBatchEnv(t)
	ctx := e.ctx([]string{"fs_*", "tool_batch"})

	rep := e.run(t, ctx, `[
		{"tool":"fs_write","args":{"path":"src.txt","content":"MARKER-9F3A"}},
		{"tool":"fs_read","args":{"path":"src.txt"}},
		{"tool":"fs_write","args":{"path":"copy.txt","content":"$1"}}]`)

	if rep.Completed != 3 {
		t.Fatalf("Completed = %d, want 3: %+v", rep.Completed, rep)
	}
	body, err := os.ReadFile(filepath.Join(e.root, "copy.txt"))
	if err != nil {
		t.Fatalf("the chained step did not produce a file: %v", err)
	}
	if !strings.Contains(string(body), "MARKER-9F3A") {
		t.Errorf("chained file does not carry step 1's real output; substitution did not "+
			"happen. content = %q", string(body))
	}
	if strings.Contains(string(body), "$1") {
		t.Errorf("the literal reference token survived into the file: %q", string(body))
	}
}

// TestBatchReal_DeniedStepLeavesNoFileAndStopsTheBatch is the attack test with
// a real side effect.
//
// The batch is: write allowed.txt, write denied.txt (refused by the profile),
// write after.txt. The report is checked, but the load-bearing assertions are
// the two files that must NOT be on disk. A batch that recorded the denial
// after the write had already landed would pass every report-level check and
// fail here -- and it is exactly the bug a batch tool invites, because the
// wrapper was authorized once and could plausibly be treated as covering its
// contents.
func TestBatchReal_DeniedStepLeavesNoFileAndStopsTheBatch(t *testing.T) {
	e := newRealBatchEnv(t)
	// fs_read is allowed; fs_write is NOT. Both are registered, so toolreg's
	// structural check passes and the refusal comes from the profile, which is
	// the layer under test.
	ctx := WithWorkRoot(context.Background(), e.root)
	ctx = WithProfile(ctx, guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_read", "fs_list", "tool_batch"}},
		FS: guard.FSPerm{
			Read:  []string{e.root, e.root + "/**"},
			Write: []string{e.root, e.root + "/**"},
		},
	})
	ctx = toolreg.WithRegistered(ctx, []string{"fs_write", "fs_read", "fs_list", "tool_batch"})

	rep := e.run(t, ctx, `[
		{"tool":"fs_list","args":{"path":"."}},
		{"tool":"fs_write","args":{"path":"denied.txt","content":"should never exist"}},
		{"tool":"fs_write","args":{"path":"after.txt","content":"should never exist"}}]`)

	if e.exists("denied.txt") {
		t.Fatal("a profile-denied fs_write CREATED ITS FILE inside a batch — " +
			"the batch is a guard bypass")
	}
	if e.exists("after.txt") {
		t.Fatal("a step AFTER the denial wrote its file — the batch did not stop")
	}
	if len(rep.Steps) != 2 {
		t.Fatalf("report has %d steps, want 2 (stop at the denial): %+v", len(rep.Steps), rep.Steps)
	}
	if !rep.Steps[1].Denied {
		t.Errorf("the refused step must be marked Denied, got %+v", rep.Steps[1])
	}
	if rep.Completed != 1 {
		t.Errorf("Completed = %d, want 1", rep.Completed)
	}
}

// TestBatchReal_BatchGrantsNothingByItself checks the strongest form of the
// bypass claim: under a profile that allows tool_batch and NOTHING else, no
// step may have any effect on disk.
func TestBatchReal_BatchGrantsNothingByItself(t *testing.T) {
	e := newRealBatchEnv(t)
	ctx := WithWorkRoot(context.Background(), e.root)
	ctx = WithProfile(ctx, guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"tool_batch"}},
		FS: guard.FSPerm{
			Read:  []string{e.root, e.root + "/**"},
			Write: []string{e.root, e.root + "/**"},
		},
	})
	ctx = toolreg.WithRegistered(ctx, []string{"fs_write", "fs_read", "fs_list", "tool_batch"})

	rep := e.run(t, ctx, `[
		{"tool":"fs_write","args":{"path":"a.txt","content":"x"}},
		{"tool":"fs_write","args":{"path":"b.txt","content":"x"}}]`)

	for _, name := range []string{"a.txt", "b.txt"} {
		if e.exists(name) {
			t.Fatalf("%s was written under a profile that allows only tool_batch", name)
		}
	}
	if rep.Completed != 0 {
		t.Errorf("Completed = %d, want 0", rep.Completed)
	}
}

// TestBatchReal_PathJailAppliesInsideABatch checks that the fs jail is not
// bypassed by batching: a step escaping the work root must fail, and must not
// create anything outside it.
//
// The batch dispatches through each tool's own Stream, so the jail should
// apply -- but "should" is the word that precedes every bypass, and a path
// escape is the one failure whose blast radius is outside the directory the
// test controls.
func TestBatchReal_PathJailAppliesInsideABatch(t *testing.T) {
	e := newRealBatchEnv(t)
	outside := filepath.Join(t.TempDir(), "escaped.txt")
	ctx := e.ctx([]string{"fs_*", "tool_batch"})

	// An absolute path outside the root, and a traversal out of it.
	program, err := json.Marshal([]map[string]any{
		{"tool": "fs_write", "args": map[string]string{"path": outside, "content": "escaped"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rep := e.run(t, ctx, string(program))

	if _, statErr := os.Stat(outside); statErr == nil {
		t.Fatal("a batch step wrote OUTSIDE the work root — the path jail does not " +
			"apply inside a batch")
	}
	if rep.Completed != 0 {
		t.Errorf("Completed = %d, want 0 (the escaping step must not count as done): %+v",
			rep.Completed, rep)
	}
}
