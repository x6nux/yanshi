package log

import (
	"fmt"
	"log/slog"
	"strings"
	"unicode"
)

// Redacted is the only replacement written for sensitive values.
const Redacted = "[REDACTED]"

// sensitiveKeys are attribute names whose VALUE is replaced wholesale.
//
// Keys are matched after normalizedKey strips punctuation and lowercases, so
// "X-API-Key", "x_api_key" and "xApiKey" all arrive as "xapikey" -- which is
// why each spelling variant does NOT need its own entry, but each distinct
// WORD does. That distinction is easy to get backwards: "apikey" was present
// and "xapikey" was not, so the header spelling every HTTP client actually
// sends went through unredacted.
//
// The content and path groups are here because a log line is not a debugger:
// file contents, tool output and absolute paths are the three things most
// likely to carry someone's data or reveal their directory layout, and none
// of them is worth the risk in a line nobody reads until an incident.
var sensitiveKeys = map[string]struct{}{
	// Credentials.
	"apikey": {}, "xapikey": {}, "authorization": {}, "password": {}, "secret": {},
	"token": {}, "accesstoken": {}, "refreshtoken": {},
	"credential": {}, "credentials": {}, "passphrase": {}, "cookie": {},
	"clientsecret": {}, "privatekey": {},
	// Model I/O.
	"prompt": {}, "messages": {}, "input": {},
	"args": {}, "argsjson": {}, "toolargs": {}, "arguments": {},
	// Content: whatever a tool read or wrote.
	"content": {}, "text": {}, "body": {}, "stdout": {}, "stderr": {},
	// Locations: paths reveal directory layout and often usernames.
	"command": {}, "path": {}, "paths": {}, "host": {}, "url": {},
	"file": {}, "filename": {}, "dir": {}, "cwd": {},
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
	// Every prefix here MUST be written lowercase: v was lowercased above, so
	// an uppercase literal below can never match and would be a silently dead
	// rule -- the worst kind, because the table would look like it covers a
	// vendor it does not.
	for _, prefix := range sensitiveValuePrefixes {
		if strings.HasPrefix(v, prefix) {
			return true
		}
	}
	return strings.Contains(v, "api_key=") ||
		strings.Contains(v, "authorization:")
}

// sensitiveValuePrefixes are the token shapes recognised regardless of which
// attribute they arrive under, for the case where a credential is logged with
// an innocent key name ("value", "arg", "line").
//
// Prefix rather than regex: these vendor prefixes are stable and unambiguous,
// and a value-scanning regex over every logged string is a cost paid on every
// line for a rarer catch.
var sensitiveValuePrefixes = []string{
	"sk-",            // OpenAI, Anthropic
	"bearer ",        // any Authorization header value
	"xoxb-", "xoxp-", // Slack bot / user
	"ghp_", "gho_", "github_pat_", // GitHub classic / OAuth / fine-grained
	"glpat-",       // GitLab
	"akia", "asia", // AWS access key / temporary
	"aiza",       // Google API
	"-----begin", // PEM block of any kind
	"eyj",        // JWT (base64 of `{"`)
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
