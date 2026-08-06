package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFileInputHonoursTheInputMode pins the fix for --file bypassing --input.
//
// The --file branch read the whole file and made it ONE prompt, never calling
// cli.ReadHeadlessInputs, so `--input jsonl --file <3-line file>` ran a single
// turn whose prompt was three lines of raw JSON — the modes it advertises
// (text/lines/jsonl, with a 1MiB line cap and per-line error reporting for
// jsonl) applied to stdin only.
//
// docs.yml's CI smoke ran exactly this command with output redirected to
// /dev/null and no assertion, so the one thing that exercised the path could
// not observe it.
func TestFileInputHonoursTheInputMode(t *testing.T) {
	dir := t.TempDir()

	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	jsonl := write("in.jsonl", `{"prompt":"one"}`+"\n"+`{"prompt":"two"}`+"\n"+`{"prompt":"three"}`+"\n")
	lines := write("in.txt", "alpha\nbeta\n")
	text := write("in.md", "a single\nmulti-line prompt\n")

	t.Run("jsonl", func(t *testing.T) {
		got, err := headlessInputs(headlessConfig{Input: "jsonl", File: jsonl}, nil)
		if err != nil {
			t.Fatalf("headlessInputs: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("--input jsonl --file <3 objects> produced %d turn(s), want 3", len(got))
		}
		for i, want := range []string{"one", "two", "three"} {
			if got[i].Prompt != want {
				t.Errorf("turn %d prompt = %q, want %q", i, got[i].Prompt, want)
			}
		}
	})

	t.Run("lines", func(t *testing.T) {
		got, err := headlessInputs(headlessConfig{Input: "lines", File: lines}, nil)
		if err != nil {
			t.Fatalf("headlessInputs: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("--input lines --file <2 lines> produced %d turn(s), want 2", len(got))
		}
	})

	t.Run("text keeps the whole file as one prompt", func(t *testing.T) {
		// The regression guard for the fix itself: text mode must still behave
		// the way --file always did, or fixing jsonl silently breaks every
		// existing caller that passes a prompt file.
		got, err := headlessInputs(headlessConfig{Input: "text", File: text}, nil)
		if err != nil {
			t.Fatalf("headlessInputs: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("--input text --file produced %d turn(s), want 1", len(got))
		}
		if !strings.Contains(got[0].Prompt, "multi-line") {
			t.Errorf("text mode lost part of the file: %q", got[0].Prompt)
		}
	})

	t.Run("a malformed jsonl line names its line number", func(t *testing.T) {
		bad := write("bad.jsonl", `{"prompt":"ok"}`+"\n"+`{not json`+"\n")
		_, err := headlessInputs(headlessConfig{Input: "jsonl", File: bad}, nil)
		if err == nil {
			t.Fatal("a malformed jsonl line was accepted")
		}
		if !strings.Contains(err.Error(), "2") {
			t.Errorf("error does not name the offending line: %v", err)
		}
	})

	t.Run("stdin still works", func(t *testing.T) {
		got, err := headlessInputs(headlessConfig{Input: "lines"}, strings.NewReader("x\ny\n"))
		if err != nil {
			t.Fatalf("headlessInputs: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("stdin lines produced %d turn(s), want 2", len(got))
		}
	})
}
