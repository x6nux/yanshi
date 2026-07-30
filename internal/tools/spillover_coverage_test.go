package tools

import (
	"testing"
)

func TestDegradedSpill(t *testing.T) {
	t.Run("appends footer for any result", func(t *testing.T) {
		r := "hello"
		got := degradedSpill(r, nil)
		if len(got) <= len(r) {
			t.Fatalf("expected footer appended, got: %s", got)
		}
	})
	t.Run("truncates when result exceeds SpillThreshold", func(t *testing.T) {
		big := make([]byte, SpillThreshold+100)
		for i := range big {
			big[i] = 'x'
		}
		got := degradedSpill(string(big), nil)
		// After truncation + footer, should be <= SpillThreshold + footer
		if len(got) > SpillThreshold+200 {
			t.Fatalf("result should be truncated: %d bytes", len(got))
		}
	})
	t.Run("under threshold no truncation", func(t *testing.T) {
		r := "short result"
		got := degradedSpill(r, nil)
		if !contains(got, r) {
			t.Fatalf("expected original text preserved, got: %s", got)
		}
	})
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
