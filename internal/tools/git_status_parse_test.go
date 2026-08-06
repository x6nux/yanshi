package tools

import (
	"encoding/json"
	"testing"
)

// TestParseGitStatusZHandlesTrackedRecords covers the branch that had no test.
//
// The existing porcelain-v2 test writes files with os.WriteFile and never runs
// `git add`, so every record it produces is untracked ("? <path>") and takes
// the one branch that was already correct. Tracked records -- the "1" and "2"
// forms, which is what `git status` reports for anything a developer is
// actually working on -- were never exercised.
//
// Two things were wrong there. The path was taken as the LAST space-separated
// field, so any tracked path containing a space came back truncated to its
// final word. And a rename record ("2") is followed by a SEPARATE NUL-delimited
// field holding the original path; the loop treated that field as its own
// record, inventing a phantom entry.
//
// Porcelain v2's field layout is fixed, so the path is everything after the
// 8th space for "1" records and after the 9th for "2" records. Counting fields
// is the fix; searching for the path is what caused the bug.
func TestParseGitStatusZHandlesTrackedRecords(t *testing.T) {
	type entry struct {
		Path string `json:"path"`
		XY   string `json:"xy"`
	}
	decode := func(v any) []entry {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Entries []entry `json:"entries"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		return out.Entries
	}

	t.Run("tracked path containing spaces survives", func(t *testing.T) {
		// 1 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <path>
		rec := "1 M. N... 100644 100644 100644 aaaaaaa bbbbbbb my notes/todo list.md\x00"
		got := decode(parseGitStatusZ(rec))
		if len(got) != 1 {
			t.Fatalf("want 1 entry, got %d: %+v", len(got), got)
		}
		if got[0].Path != "my notes/todo list.md" {
			t.Fatalf("path truncated: %q", got[0].Path)
		}
		if got[0].XY != "M." {
			t.Fatalf("xy = %q", got[0].XY)
		}
	})

	t.Run("rename record does not spawn a phantom entry", func(t *testing.T) {
		// 2 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <score> <path>\x00<origPath>\x00
		rec := "2 R. N... 100644 100644 100644 aaaaaaa bbbbbbb R100 new name.md\x00old name.md\x00"
		got := decode(parseGitStatusZ(rec))
		if len(got) != 1 {
			t.Fatalf("the original-path field must not become its own entry; got %d: %+v", len(got), got)
		}
		if got[0].Path != "new name.md" {
			t.Fatalf("rename path = %q, want the destination", got[0].Path)
		}
	})

	t.Run("untracked still works", func(t *testing.T) {
		got := decode(parseGitStatusZ("? some file.txt\x00"))
		if len(got) != 1 || got[0].Path != "some file.txt" || got[0].XY != "??" {
			t.Fatalf("untracked regressed: %+v", got)
		}
	})

	t.Run("mixed record kinds keep their order and count", func(t *testing.T) {
		rec := "1 M. N... 100644 100644 100644 aaaaaaa bbbbbbb a b.go\x00" +
			"2 R. N... 100644 100644 100644 aaaaaaa bbbbbbb R100 c d.go\x00e f.go\x00" +
			"? g h.go\x00"
		got := decode(parseGitStatusZ(rec))
		if len(got) != 3 {
			t.Fatalf("want 3 entries, got %d: %+v", len(got), got)
		}
		want := []string{"a b.go", "c d.go", "g h.go"}
		for i, w := range want {
			if got[i].Path != w {
				t.Fatalf("entry %d path = %q, want %q (all: %+v)", i, got[i].Path, w, got)
			}
		}
	})
}

// TestMergeNumstatByPathDeduplicates covers the staged+unstaged double-report.
//
// collectGitDiffFiles queries three sources for a working-tree diff (unstaged
// numstat, --cached numstat, ls-files --others) and used to append all three.
// A file that is staged AND modified again -- the normal state of a file
// someone is mid-edit on -- came back as two entries, each driving its own
// gitPatchForFile call, so the model saw the same path twice with two
// different partial diffs and nothing saying they were halves of one change.
func TestMergeNumstatByPathDeduplicates(t *testing.T) {
	in := []gitNumstatEntry{
		{Path: "a.go", Additions: 3, Deletions: 1},  // unstaged
		{Path: "b.go", Additions: 5, Deletions: 0},  // staged only
		{Path: "a.go", Additions: 10, Deletions: 2}, // staged half of a.go
		{Path: "c.txt", Untracked: true},            // ls-files --others
	}
	got := mergeNumstatByPath(in)
	if len(got) != 3 {
		t.Fatalf("want 3 merged entries, got %d: %+v", len(got), got)
	}
	if got[0].Path != "a.go" || got[1].Path != "b.go" || got[2].Path != "c.txt" {
		t.Fatalf("first-seen order not preserved: %+v", got)
	}
	if got[0].Additions != 13 || got[0].Deletions != 3 {
		t.Fatalf("line counts must add across staged and unstaged: %+v", got[0])
	}
	if !got[2].Untracked {
		t.Fatalf("untracked flag lost: %+v", got[2])
	}
}
