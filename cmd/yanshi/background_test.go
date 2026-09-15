package main

import (
	"bytes"
	"testing"
)

// TestStripBackgroundFlagAcceptsBothPlacements pins that -b is found wherever
// it appears, because `yanshi -b` (no subcommand) and `yanshi serve -b` parse
// through different flag sets and a definition in either one alone would reject
// the other spelling.
func TestStripBackgroundFlagAcceptsBothPlacements(t *testing.T) {
	cases := []struct {
		in    []string
		rest  string
		found bool
	}{
		{[]string{"-b"}, "", true},
		{[]string{"--background"}, "", true},
		{[]string{"serve", "-b", "-config", "c.yaml"}, "serve -config c.yaml", true},
		{[]string{"-b", "serve", "-config", "c.yaml"}, "serve -config c.yaml", true},
		{[]string{"serve", "-config", "c.yaml"}, "serve -config c.yaml", false},
	}
	for _, c := range cases {
		rest, found := stripBackgroundFlag(c.in)
		if found != c.found {
			t.Errorf("stripBackgroundFlag(%v) found=%v, want %v", c.in, found, c.found)
		}
		if got := joinArgs(rest); got != c.rest {
			t.Errorf("stripBackgroundFlag(%v) rest=%q, want %q", c.in, got, c.rest)
		}
	}
}

// TestRunBackgroundRejectsUnknownFlag proves a typo is a usage error (exit 2)
// rather than a silently ignored flag on a command whose whole job is to
// detach.
func TestRunBackgroundRejectsUnknownFlag(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := runBackground([]string{"-no-such-flag"}, &out, &errBuf); code != exitUsage {
		t.Fatalf("exit=%d, want exitUsage=%d (%s)", code, exitUsage, errBuf.String())
	}
}

// TestRunBackgroundRejectsStrayPositional is the case a `serve` word slipping
// through would produce: it must be a usage error, not a daemon start.
func TestRunBackgroundRejectsStrayPositional(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := runBackground([]string{"bogus"}, &out, &errBuf); code != exitUsage {
		t.Fatalf("exit=%d, want exitUsage (%s)", code, errBuf.String())
	}
}

func joinArgs(a []string) string {
	out := ""
	for i, s := range a {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}
