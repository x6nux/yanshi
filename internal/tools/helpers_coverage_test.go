package tools

import (
	"testing"
)

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1 KiB"},
		{1536, "2 KiB"},
		{1024*1024 - 1, "1024 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{2 * 1024 * 1024, "2.0 MiB"},
		{1540 * 1024, "1.5 MiB"},
		{10 * 1024 * 1024, "10.0 MiB"},
	}
	for _, tc := range tests {
		got := humanBytes(tc.n)
		if got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestHostOnly(t *testing.T) {
	tests := []struct {
		rawURL string
		want   string
	}{
		{"https://example.com/path", "example.com"},
		{"http://example.com:8080/path", "example.com"},
		{"https://sub.example.com:443/path", "sub.example.com"},
		{"", ""},
		{"not-a-url", ""},
	}
	for _, tc := range tests {
		got := hostOnly(tc.rawURL)
		if got != tc.want {
			t.Errorf("hostOnly(%q) = %q, want %q", tc.rawURL, got, tc.want)
		}
	}
}
