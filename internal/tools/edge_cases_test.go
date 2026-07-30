package tools

import (
	"context"
	"testing"
)

func TestDigitsStartEdge(t *testing.T) {
	tests := []struct {
		s    string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"a1b2", 1},
	}
	for _, tc := range tests {
		got := digitsStart(tc.s)
		if got != tc.want {
			t.Errorf("digitsStart(%q) = %d, want %d", tc.s, got, tc.want)
		}
	}
}

func TestHostOnlyEdge(t *testing.T) {
	tests := []struct {
		rawURL string
		want   string
	}{
		{"://invalid", ""},
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

func TestRelPath(t *testing.T) {
	got := relPath("\\invalid|root", "some/path")
	if got != "some/path" {
		t.Logf("relPath returned: %s", got)
	}
}

func TestRandSuffix(t *testing.T) {
	s1 := randSuffix(4)
	s2 := randSuffix(4)
	if s1 == "" || s2 == "" {
		t.Fatal("randSuffix should return non-empty string")
	}
	if s1 == s2 {
		t.Logf("randSuffix returned same value twice: %s", s1)
	}
}

func TestWithShellManagerEdge(t *testing.T) {
	_ = WithShellManager(context.Background(), nil)
}

func TestWithPermissionCallbackEdge(t *testing.T) {
	cb := func(req PermissionRequest) PermissionDecision {
		return PermissionAllow
	}
	ctx := WithPermissionCallback(context.Background(), cb)
	_, ok := permissionCallback(ctx)
	if !ok {
		t.Fatal("expected permission callback to be available")
	}

	base := context.Background()
	if WithPermissionCallback(base, nil) != base {
		t.Fatal("WithPermissionCallback(nil) should return ctx unchanged")
	}
}

func TestApprovalManagerEdge(t *testing.T) {
	base := context.Background()
	ctx := WithApprovalManager(base, nil, "")
	if ctx != base {
		t.Fatal("WithApprovalManager(nil) should return ctx unchanged")
	}
}
