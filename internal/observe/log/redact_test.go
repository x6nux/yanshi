package log

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestEveryRedactionRuleActuallyFires walks both tables through the real
// handler.
//
// A rule that is present but unreachable is worse than a missing one: the
// table reads as covering a vendor it does not. Two ways that happens here --
// a key spelling that normalizedKey never produces, and a value prefix
// written with any uppercase character, which can never match because the
// value is lowercased first. Both are invisible by inspection.
//
// ledger: C4/OBS1#2 secret 不入日志
func TestEveryRedactionRuleActuallyFires(t *testing.T) {
	t.Run("every sensitive key redacts", func(t *testing.T) {
		for key := range sensitiveKeys {
			var buf bytes.Buffer
			l := slog.New(&redactHandler{inner: slog.NewTextHandler(&buf, nil)})
			l.Info("m", slog.String(key, "CANARY-VALUE"))
			if strings.Contains(buf.String(), "CANARY-VALUE") {
				t.Errorf("key %q is in the table but its value survived: %s", key, buf.String())
			}
		}
	})

	t.Run("key matching is spelling-insensitive", func(t *testing.T) {
		// The header spelling every HTTP client sends, which was the gap:
		// "apikey" was in the table and this was not.
		for _, spelling := range []string{"X-API-Key", "x_api_key", "xApiKey"} {
			var buf bytes.Buffer
			l := slog.New(&redactHandler{inner: slog.NewTextHandler(&buf, nil)})
			l.Info("m", slog.String(spelling, "CANARY-VALUE"))
			if strings.Contains(buf.String(), "CANARY-VALUE") {
				t.Errorf("%q leaked: %s", spelling, buf.String())
			}
		}
	})

	t.Run("every value prefix is lowercase and fires", func(t *testing.T) {
		for _, prefix := range sensitiveValuePrefixes {
			if prefix != strings.ToLower(prefix) {
				t.Errorf("prefix %q has uppercase: looksSensitiveValue lowercases first, "+
					"so this rule can never match", prefix)
				continue
			}
			var buf bytes.Buffer
			l := slog.New(&redactHandler{inner: slog.NewTextHandler(&buf, nil)})
			// An innocent key name: the value shape alone must trigger it.
			l.Info("m", slog.String("value", prefix+"CANARYTAIL"))
			if strings.Contains(buf.String(), "CANARYTAIL") {
				t.Errorf("prefix %q did not redact: %s", prefix, buf.String())
			}
		}
	})

	t.Run("ordinary values survive", func(t *testing.T) {
		var buf bytes.Buffer
		l := slog.New(&redactHandler{inner: slog.NewTextHandler(&buf, nil)})
		l.Info("m", slog.String("count", "42"), slog.String("state", "running"))
		if !strings.Contains(buf.String(), "42") || !strings.Contains(buf.String(), "running") {
			t.Fatalf("over-redaction: %s", buf.String())
		}
	})
}
