package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/store"
)

// sessionTestEnv creates a project config + database with one session.
func sessionTestEnv(t *testing.T, title string) (cfgPath, sessionID string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.ToSlash(filepath.Join(dir, "test.db"))
	cfgPath = filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf("server:\n  http_addr: \"127.0.0.1:0\"\nstorage:\n  sqlite_path: %q\n", dbPath)
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	id, err := st.CreateSession(title)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := st.AppendMessage(id, 0, "user", "hello from the test"); err != nil {
		t.Fatalf("append: %v", err)
	}
	return cfgPath, id
}

func runSessionCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := runSession(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// TestSessionListJSONIsMachineReadable pins the contract a script depends on:
// one JSON object on stdout, exit 0, and the fields the TUI shows.
func TestSessionListJSONIsMachineReadable(t *testing.T) {
	cfg, id := sessionTestEnv(t, "listed by the test")
	code, out, errs := runSessionCLI(t, "list", "-config", cfg, "-json")
	if code != exitOK {
		t.Fatalf("exit=%d stderr=%q", code, errs)
	}
	var payload struct {
		Sessions []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, out)
	}
	if len(payload.Sessions) != 1 || payload.Sessions[0].ID != id {
		t.Fatalf("sessions = %+v, want the one just created (%s)", payload.Sessions, id)
	}
	if payload.Sessions[0].Title != "listed by the test" {
		t.Fatalf("title = %q", payload.Sessions[0].Title)
	}
}

// TestSessionShowAcceptsFlagsAfterTheID pins the flag placement that a user
// types first: `show <id> -json`. flag.FlagSet stops at the first positional
// argument and this form used to print usage instead.
func TestSessionShowAcceptsFlagsAfterTheID(t *testing.T) {
	cfg, id := sessionTestEnv(t, "shown by the test")
	code, out, errs := runSessionCLI(t, "show", id, "-config", cfg, "-json", "-tail", "1")
	if code != exitOK {
		t.Fatalf("exit=%d stderr=%q", code, errs)
	}
	var payload struct {
		Session struct {
			ID       string `json:"id"`
			Messages int    `json:"messages"`
		} `json:"session"`
		Tail []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"tail"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, out)
	}
	if payload.Session.ID != id || payload.Session.Messages != 1 {
		t.Fatalf("session = %+v", payload.Session)
	}
	if len(payload.Tail) != 1 || payload.Tail[0].Role != "user" {
		t.Fatalf("tail = %+v, want the single user message", payload.Tail)
	}
}

// TestSessionRenameArchiveDelete covers the mutating verbs against the store,
// including the guard on delete: a bare id is a usage error, not a deletion.
func TestSessionRenameArchiveDelete(t *testing.T) {
	cfg, id := sessionTestEnv(t, "before rename")

	if code, _, errs := runSessionCLI(t, "rename", id, "after", "rename", "-config", cfg); code != exitOK {
		t.Fatalf("rename exit=%d stderr=%q", code, errs)
	}
	if code, out, _ := runSessionCLI(t, "show", id, "-config", cfg, "-json"); code != exitOK ||
		!strings.Contains(out, "after rename") {
		t.Fatalf("rename did not land: %q", out)
	}

	if code, _, errs := runSessionCLI(t, "archive", id, "-config", cfg); code != exitOK {
		t.Fatalf("archive exit=%d stderr=%q", code, errs)
	}
	if code, out, _ := runSessionCLI(t, "list", "-config", cfg, "-json"); code != exitOK ||
		!strings.Contains(out, "\"sessions\":[]") && strings.Contains(out, id) {
		t.Fatalf("archived session still listed as active: %q", out)
	}
	if code, out, _ := runSessionCLI(t, "list", "-archived", "-config", cfg); code != exitOK || !strings.Contains(out, "after rename") {
		t.Fatalf("archived list missing the session: %q", out)
	}
	if code, _, errs := runSessionCLI(t, "unarchive", id, "-config", cfg); code != exitOK {
		t.Fatalf("unarchive exit=%d stderr=%q", code, errs)
	}

	// A bare id must NOT delete: the TUI requires the literal "yes" and so does
	// this, so the same mistake is survivable in both places.
	if code, _, _ := runSessionCLI(t, "delete", id, "-config", cfg); code != exitUsage {
		t.Fatalf("delete without yes: exit=%d, want exitUsage", code)
	}
	if code, _, errs := runSessionCLI(t, "delete", id, "yes", "-config", cfg); code != exitOK {
		t.Fatalf("delete exit=%d stderr=%q", code, errs)
	}
	if code, _, _ := runSessionCLI(t, "show", id, "-config", cfg); code != exitErr {
		t.Fatalf("show of a deleted session: exit=%d, want exitErr", code)
	}
}

// TestParseSessionArgs pins the one-pass flag scan, including that an unknown
// flag is an error rather than something silently ignored.
func TestParseSessionArgs(t *testing.T) {
	sf, pos, err := parseSessionArgs([]string{"abc", "-config", "c.yaml", "-json", "-tail=3"})
	if err != nil {
		t.Fatalf("parseSessionArgs: %v", err)
	}
	if sf.config != "c.yaml" || !sf.jsonOut || sf.tail != 3 {
		t.Fatalf("sf = %+v", sf)
	}
	if strings.Join(pos, ",") != "abc" {
		t.Fatalf("positionals = %v", pos)
	}
	if _, _, err := parseSessionArgs([]string{"-nope"}); err == nil {
		t.Fatal("unknown flag must be an error")
	}
	if _, _, err := parseSessionArgs([]string{"-limit", "many"}); err == nil {
		t.Fatal("non-integer -limit must be an error")
	}
	if _, _, err := parseSessionArgs([]string{"-config"}); err == nil {
		t.Fatal("a value flag with no value must be an error")
	}
}

// TestRunSessionUnknownVerbIsUsage keeps a typo from looking like a successful
// no-op in a script.
func TestRunSessionUnknownVerbIsUsage(t *testing.T) {
	if code, _, _ := runSessionCLI(t, "lsit"); code != exitUsage {
		t.Fatalf("exit=%d, want exitUsage", code)
	}
	if code, out, _ := runSessionCLI(t, "help"); code != exitOK || !strings.Contains(out, "usage: yanshi session") {
		t.Fatalf("help exit=%d out=%q", code, out)
	}
	if code, _, _ := runSessionCLI(t); code != exitUsage {
		t.Fatalf("no verb: exit=%d, want exitUsage", code)
	}
}
