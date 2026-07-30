// internal/mcp/types_test.go
package mcp

import (
	"testing"
)

func TestQualifyToolName_Basic(t *testing.T) {
	got := QualifyToolName("my-server", "get-file")
	want := "mcp_my_server_get_file"
	if got != want {
		t.Fatalf("QualifyToolName(%q,%q) = %q, want %q", "my-server", "get-file", got, want)
	}
}

func TestQualifyToolName_SanitizesSpecials(t *testing.T) {
	got := QualifyToolName("Server 1!", "read:data")
	want := "mcp_server_1_read_data"
	if got != want {
		t.Fatalf("QualifyToolName = %q, want %q", got, want)
	}
}

func TestQualifyToolName_TruncatesLong(t *testing.T) {
	longServer := "this-is-a-very-long-server-name-that-exceeds-sixty-four-characters"
	got := QualifyToolName(longServer, "a-tool-name")
	if len(got) > 64 {
		t.Fatalf("QualifyToolName length %d > 64: %q", len(got), got)
	}
	if len(got) != 64 {
		t.Fatalf("truncated name should be exactly 64 chars, got %d (%q)", len(got), got)
	}
	if got[:4] != "mcp_" {
		t.Fatalf("prefix lost: %q", got)
	}
}

func TestQualifyToolName_StableForSameInput(t *testing.T) {
	a := QualifyToolName("x-y", "z-w")
	b := QualifyToolName("x-y", "z-w")
	if a != b {
		t.Fatalf("QualifyToolName not stable: %q vs %q", a, b)
	}
}

func TestQualifyToolName_DistinctForDifferentServers(t *testing.T) {
	a := QualifyToolName("srv-a", "tool")
	b := QualifyToolName("srv-b", "tool")
	if a == b {
		t.Fatalf("different servers should produce different names: %q", a)
	}
}
