// Package i18n provides locale-aware message lookup. Catalogs are embedded
// JSON (en, zh-Hans). The Bundle is constructed at startup with the
// PERSISTENT locale string ("auto" / "en" / "zh-Hans") and recomputes the
// EFFECTIVE locale on every NewBundle call — so "auto" tracks LC_ALL/LANG
// across restarts instead of being resolved once and frozen.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

//go:embed catalog/*.json
var catalogFS embed.FS

// Supported lists the locales with embedded catalogs. "auto" is a valid
// PERSISTENT value but not a member of Supported (it resolves to one).
var Supported = []string{"en", "zh-Hans"}

// requiredCatalogKeys is the canonical UI contract. Tests compare each
// embedded catalog independently against this exact set, so deleting the same
// key from both translations cannot make a pairwise-parity test pass.
// Adding a new UI string means adding its key here AND to both catalogs —
// the manifest test will fail otherwise.
var requiredCatalogKeys = []string{
	"tui.input.placeholder",
	"tui.command.help.title",
	"tui.command.help.row",
	"tui.command.help.help",
	"tui.command.help.model",
	"tui.command.help.think",
	"tui.command.help.mode",
	"tui.command.help.queue_mode",
	"tui.command.help.clear",
	"tui.command.help.config",
	"tui.command.help.cost",
	"tui.command.help.stats",
	"tui.command.help.compact",
	"tui.command.help.mcp",
	"tui.command.help.sessions",
	"tui.command.help.restore",
	"tui.command.help.rename",
	"tui.command.help.archive",
	"tui.command.help.unarchive",
	"tui.command.help.archived",
	"tui.command.help.delete",
	"tui.command.help.theme",
	"tui.command.preference.persist_failed",
	"tui.status.model",
	"tui.status.thinking",
	"tui.status.tokens_in",
	"tui.status.tokens_out",
	"tui.error.connect_failed",
	"tui.error.auth_failed",
	"tui.error.send_failed",
	"tui.error.unknown_command",
	"tui.workflow.running",
	"tui.workflow.completed",
	"tui.workflow.failed",
	"auth.cli.prompt",
	"auth.cli.stored",
	"auth.cli.deleted",
	"auth.cli.status.authenticated",
	"auth.cli.status.unauthenticated",
	"keymap.diagnostics.conflict",
	"keymap.diagnostics.normalized_duplicate",
	"keymap.diagnostics.unknown_action",
	"keymap.diagnostics.invalid_key",
	"keymap.diagnostics.unsupported_keymap",
	"keymap.diagnostics.reset",
	"vim.mode.insert",
	"vim.mode.normal",
	"vim.mode.visual",
	"contrast.enabled",
	"contrast.disabled",
}

// Bundle is a locale catalog. Persistent is the user's configured value
// (may be "auto"); Effective is the resolved locale actually used for lookups.
type Bundle struct {
	persistent string
	effective  string
	messages   map[string]string
}

// NewBundle constructs a Bundle from a persistent locale string. An empty or
// "auto" persistent value triggers LC_ALL/LANG detection on every call.
// Unsupported locales return an error.
func NewBundle(persistent string) (*Bundle, error) {
	if persistent == "" {
		persistent = "auto"
	}
	effective := persistent
	if persistent == "auto" {
		effective = detectLocale()
	}
	if !isSupported(effective) {
		// Unknown explicit locale: return a usable English bundle plus an error.
		messages, loadErr := loadCatalog("en")
		if loadErr != nil {
			return nil, loadErr
		}
		b := &Bundle{persistent: persistent, effective: "en", messages: messages}
		return b, fmt.Errorf("i18n: locale %q not supported; using en", persistent)
	}
	messages, err := loadCatalog(effective)
	if err != nil {
		return nil, err
	}
	return &Bundle{
		persistent: persistent,
		effective:  effective,
		messages:   messages,
	}, nil
}

// Persistent returns the original configured locale (may be "auto").
// Callers persisting preferences MUST use this, not Effective, so that
// changes to LC_ALL/LANG take effect on the next startup.
func (b *Bundle) Persistent() string { return b.persistent }

// Effective returns the resolved locale used for lookups.
func (b *Bundle) Effective() string { return b.effective }

// Get looks up key in the catalog. Missing keys fall back to the key itself
// (not an error) so the UI stays usable if a catalog lags behind code.
func (b *Bundle) Get(key string) string {
	if v, ok := b.messages[key]; ok {
		return v
	}
	return key
}

// GetF looks up key and substitutes {name} placeholders from vars.
func (b *Bundle) GetF(key string, vars map[string]string) string {
	s := b.Get(key)
	for k, v := range vars {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}

// detectLocale returns the locale inferred from LC_ALL (with C/POSIX honored
// as English) or LANG, falling back to "en" when neither is set. It is the
// single source of truth for "auto" resolution and is invoked on every
// NewBundle call so the persistent "auto" string stays honest across
// restarts.
func detectLocale() string {
	// LC_ALL truly overrides LANG. In particular C/POSIX means English and
	// must not fall through to a zh LANG value.
	if v := os.Getenv("LC_ALL"); v != "" {
		return normalizeLocale(v)
	}
	if v := os.Getenv("LANG"); v != "" {
		return normalizeLocale(v)
	}
	return "en"
}

// normalizeLocale strips @modifier and .codeset, canonicalizes separators,
// and maps the result to one of the supported locales. C/POSIX is always
// English; the zh-Hans family covers Simplified Chinese (zh-CN, zh-SG);
// the zh-Hant family and any other language fall back to English (the UI's
// original language) rather than silently rendering half-translated.
func normalizeLocale(raw string) string {
	s := raw
	if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	s = strings.ReplaceAll(s, "_", "-")
	lower := strings.ToLower(s)
	switch {
	case lower == "c" || lower == "posix":
		return "en"
	case strings.HasPrefix(lower, "zh-hans"),
		lower == "zh-cn", lower == "zh-sg":
		return "zh-Hans"
	case strings.HasPrefix(lower, "zh-hant"),
		lower == "zh-tw", lower == "zh-hk":
		return "en"
	case strings.HasPrefix(lower, "en"):
		return "en"
	default:
		return "en"
	}
}

func isSupported(loc string) bool {
	for _, s := range Supported {
		if s == loc {
			return true
		}
	}
	return false
}

func loadCatalog(loc string) (map[string]string, error) {
	data, err := catalogFS.ReadFile("catalog/" + loc + ".json")
	if err != nil {
		return nil, fmt.Errorf("i18n: read embedded catalog %s: %w", loc, err)
	}
	var messages map[string]string
	if err := json.Unmarshal(data, &messages); err != nil {
		return nil, fmt.Errorf("i18n: decode embedded catalog %s: %w", loc, err)
	}
	return messages, nil
}

// Preferences persistence intentionally does not live in this package. The
// sole owner is internal/cli/tui/preferences.go (Task 11).
