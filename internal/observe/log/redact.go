package log

import (
	"fmt"
	"log/slog"
	"strings"
	"unicode"
)

// Redacted is the only replacement written for sensitive values.
const Redacted = "[REDACTED]"

var sensitiveKeys = map[string]struct{}{
	"apikey": {}, "authorization": {}, "password": {}, "secret": {},
	"token": {}, "accesstoken": {}, "refreshtoken": {},
	"prompt": {}, "messages": {}, "input": {},
	"args": {}, "argsjson": {}, "toolargs": {}, "arguments": {},
	"command": {}, "path": {}, "paths": {}, "host": {}, "url": {},
	"headers": {}, "query": {},
}

func normalizedKey(key string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, key)
}

func sensitiveKey(key string) bool {
	_, ok := sensitiveKeys[normalizedKey(key)]
	return ok
}

func looksSensitiveValue(value string) bool {
	v := strings.TrimSpace(strings.ToLower(value))
	return strings.HasPrefix(v, "sk-") ||
		strings.HasPrefix(v, "bearer ") ||
		strings.HasPrefix(v, "xoxb-") ||
		strings.HasPrefix(v, "xoxp-") ||
		strings.Contains(v, "api_key=") ||
		strings.Contains(v, "authorization:")
}

// redactAttr recursively sanitizes groups and also handles error values stored
// through slog.Any. It is used both at Handle time and before WithAttrs binds
// attributes, so pre-bound secrets cannot bypass the handler.
func redactAttr(attr slog.Attr) slog.Attr {
	attr.Value = attr.Value.Resolve()
	if sensitiveKey(attr.Key) {
		return slog.String(attr.Key, Redacted)
	}
	if attr.Value.Kind() == slog.KindGroup {
		children := attr.Value.Group()
		clean := make([]slog.Attr, 0, len(children))
		for _, child := range children {
			clean = append(clean, redactAttr(child))
		}
		return slog.Attr{Key: attr.Key, Value: slog.GroupValue(clean...)}
	}
	if attr.Value.Kind() == slog.KindString && looksSensitiveValue(attr.Value.String()) {
		return slog.String(attr.Key, Redacted)
	}
	if attr.Value.Kind() == slog.KindAny {
		if _, ok := attr.Value.Any().(error); ok {
			return slog.String(attr.Key, Redacted)
		}
		if looksSensitiveValue(fmt.Sprint(attr.Value.Any())) {
			return slog.String(attr.Key, Redacted)
		}
	}
	return attr
}

func redactAttrs(attrs []slog.Attr) []slog.Attr {
	clean := make([]slog.Attr, len(attrs))
	for i, attr := range attrs {
		clean[i] = redactAttr(attr)
	}
	return clean
}
