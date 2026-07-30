// Package keymap provides the core keybinding primitives shared across TUI
// surfaces. It is a leaf package (depends only on bubbletea) so both
// internal/cli/tui and future headless consumers can reuse it.
//
// Design: Builder collects Add() calls WITHOUT validating; only Build()
// performs conflict + unknown-action checks. This catches conflicts that
// span across registration calls (A adds ctrl+k, B adds ctrl+k — an
// incremental validator would miss the second one).
package keymap

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
)

// Action is the semantic action a keybinding invokes.
type Action string

// Actions a keybinding can invoke.
const (
	ActionNone        Action = ""
	ActionSend        Action = "send"
	ActionNewline     Action = "newline"
	ActionCancel      Action = "cancel"
	ActionScrollUp    Action = "scroll_up"
	ActionScrollDown  Action = "scroll_down"
	ActionClear       Action = "clear"
	ActionHelp        Action = "help"
	ActionQuit        Action = "quit"
	ActionCommandMode Action = "command_mode"
)

var knownActions = map[Action]struct{}{
	ActionSend: {}, ActionNewline: {}, ActionCancel: {},
	ActionScrollUp: {}, ActionScrollDown: {}, ActionClear: {},
	ActionHelp: {}, ActionQuit: {}, ActionCommandMode: {},
}

// Builder collects keybindings and validates the complete candidate set on
// Build. A binding records whether it came from built-in defaults or user
// overrides so a legitimate override replaces a default while two normalized
// override spellings remain a diagnostic.
type Builder struct {
	bindings []binding
}

type binding struct {
	normalized   string
	raw          string
	action       Action
	override     bool
	normalizeErr error
}

// NewBuilder returns an empty Builder.
func NewBuilder() *Builder { return &Builder{} }

// NewDefaultBuilder sorts raw override keys before normalization. Do not
// first copy them into a normalized map: doing so would silently discard
// CTRL+K versus ctrl+k and make duplicate diagnostics nondeterministic.
// overrides may be nil — the common TUI path uses built-in defaults only.
func NewDefaultBuilder(overrides map[string]string) *Builder {
	defaults := map[string]Action{
		"enter": ActionSend, "ctrl+enter": ActionNewline,
		"ctrl+c": ActionCancel, "ctrl+k": ActionScrollUp,
		"ctrl+j": ActionScrollDown, "ctrl+l": ActionClear,
		"f1": ActionHelp, "ctrl+q": ActionQuit,
	}
	defaultKeys := make([]string, 0, len(defaults))
	for key := range defaults {
		defaultKeys = append(defaultKeys, key)
	}
	sort.Strings(defaultKeys)

	b := NewBuilder()
	for _, key := range defaultKeys {
		b.add(key, defaults[key], false)
	}
	overrideKeys := make([]string, 0, len(overrides))
	for raw := range overrides {
		overrideKeys = append(overrideKeys, raw)
	}
	sort.Strings(overrideKeys)
	for _, raw := range overrideKeys {
		b.add(raw, Action(strings.TrimSpace(overrides[raw])), true)
	}
	return b
}

// Add adds a same-priority candidate. It is used by core tests and callers
// that intentionally want duplicate detection, not default replacement
// semantics.
func (b *Builder) Add(rawKey string, action Action) {
	b.add(rawKey, action, false)
}

func (b *Builder) add(rawKey string, action Action, override bool) {
	normalized, err := normalizeConfigKey(rawKey)
	b.bindings = append(b.bindings, binding{
		normalized:   normalized,
		raw:          rawKey,
		action:       action,
		override:     override,
		normalizeErr: err,
	})
}

// Build returns the valid subset and an aggregate validation error. Returning
// the map on error lets the TUI remain operable while rendering typed
// diagnostics.
func (b *Builder) Build() (*Map, error) {
	m := b.buildInternal()
	if len(m.diagnostics) == 0 {
		return m, nil
	}
	return m, fmt.Errorf("keymap: validation failed: %s",
		strings.Join(diagStrings(m.diagnostics), "; "))
}

func (b *Builder) buildInternal() *Map {
	lookup := map[string]Action{}
	sourceIsOverride := map[string]bool{}
	overrideRaw := map[string]string{}
	var diags []Diagnostic

	for _, bd := range b.bindings {
		if bd.normalizeErr != nil {
			diags = append(diags, Diagnostic{
				Kind: "invalid_key", Key: bd.raw,
			})
			continue
		}
		if _, ok := knownActions[bd.action]; !ok {
			diags = append(diags, Diagnostic{
				Kind: "unknown_action", Key: bd.raw,
				Actions: []Action{bd.action},
			})
			continue
		}
		existing, exists := lookup[bd.normalized]
		if !exists {
			lookup[bd.normalized] = bd.action
			sourceIsOverride[bd.normalized] = bd.override
			if bd.override {
				overrideRaw[bd.normalized] = bd.raw
			}
			continue
		}
		if bd.override && !sourceIsOverride[bd.normalized] {
			// One user override replacing a built-in default is valid.
			lookup[bd.normalized] = bd.action
			sourceIsOverride[bd.normalized] = true
			overrideRaw[bd.normalized] = bd.raw
			continue
		}
		kind := "conflict"
		if bd.override && sourceIsOverride[bd.normalized] {
			kind = "normalized_duplicate"
		}
		diags = append(diags, Diagnostic{
			Kind: kind, Key: bd.normalized,
			RawKeys: []string{overrideRaw[bd.normalized], bd.raw},
			Actions: []Action{existing, bd.action},
		})
		// Deterministic first-valid wins for duplicates; do not apply bd.
	}
	sort.Slice(diags, func(i, j int) bool {
		if diags[i].Kind == diags[j].Kind {
			return diags[i].Key < diags[j].Key
		}
		return diags[i].Kind < diags[j].Kind
	})
	return &Map{lookup: lookup, diagnostics: diags}
}

// Map is the validated runtime lookup plus immutable startup diagnostics.
type Map struct {
	lookup      map[string]Action
	diagnostics []Diagnostic
}

// Lookup returns ActionNone for paste, multi-rune input, invalid keys, or an
// unbound normalized key.
func (m *Map) Lookup(msg tea.KeyMsg) Action {
	if m == nil {
		return ActionNone
	}
	normalized, ok := NormalizeKey(msg)
	if !ok {
		return ActionNone
	}
	return m.lookup[normalized]
}

// Diagnostics returns a defensive copy of the startup diagnostics so callers
// cannot mutate the immutable Map state.
func (m *Map) Diagnostics() []Diagnostic {
	if m == nil {
		return nil
	}
	out := make([]Diagnostic, len(m.diagnostics))
	copy(out, m.diagnostics)
	for i := range out {
		out[i].RawKeys = append([]string(nil), out[i].RawKeys...)
		out[i].Actions = append([]Action(nil), out[i].Actions...)
	}
	return out
}

// Diagnostic.Kind is one of conflict, unknown_action, invalid_key, or
// normalized_duplicate. RawKeys preserves both user spellings where relevant.
type Diagnostic struct {
	Kind    string
	Key     string
	RawKeys []string
	Actions []Action
}

func diagStrings(ds []Diagnostic) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		acts := make([]string, len(d.Actions))
		for j, a := range d.Actions {
			acts[j] = string(a)
		}
		out[i] = fmt.Sprintf("%s:%s -> [%s]", d.Kind, d.Key,
			strings.Join(acts, ", "))
	}
	sort.Strings(out)
	return out
}

// NormalizeKey uses the checked-in Bubble Tea fork as the canonical runtime
// spelling. Paste and multi-rune KeyRunes are text payloads, never shortcuts.
func NormalizeKey(msg tea.KeyMsg) (string, bool) {
	if msg.Paste || (msg.Type == tea.KeyRunes && len(msg.Runes) != 1) {
		return "", false
	}
	normalized, err := normalizeConfigKey(msg.String())
	return normalized, err == nil
}

// supportedConfigKeys is the closed set of special key names accepted in
// addition to ctrl+letter, single rune, and alt+rune. Keep this in sync with
// the local Bubble Tea fork's KeyType String() output — adding a key here
// without that sync would let NormalizeKey accept a string the fork never
// emits.
var supportedConfigKeys = map[string]struct{}{
	"enter": {}, "tab": {}, "shift+tab": {}, "backspace": {}, "esc": {},
	"up": {}, "down": {}, "left": {}, "right": {}, "home": {}, "end": {},
	"pgup": {}, "pgdown": {}, "delete": {}, "insert": {}, "ctrl+enter": {},
	"ctrl+up": {}, "ctrl+down": {}, "ctrl+left": {}, "ctrl+right": {},
	"ctrl+home": {}, "ctrl+end": {}, "ctrl+pgup": {}, "ctrl+pgdown": {},
	"ctrl+\"": {}, "ctrl+]": {}, "ctrl+_": {}, "ctrl+^": {},
	"f1": {}, "f2": {}, "f3": {}, "f4": {}, "f5": {}, "f6": {},
	"f7": {}, "f8": {}, "f9": {}, "f10": {}, "f11": {}, "f12": {},
}

// normalizeConfigKey validates the documented configuration grammar. It
// accepts exact special-key names, ctrl+a..ctrl+z, one printable rune, or
// alt+<rune>. The error message echoes the raw input — caller wraps it.
func normalizeConfigKey(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "escape":
		s = "esc"
	case "return":
		s = "enter"
	}
	if _, ok := supportedConfigKeys[s]; ok {
		return s, nil
	}
	if len(s) == len("ctrl+a") && strings.HasPrefix(s, "ctrl+") &&
		s[len(s)-1] >= 'a' && s[len(s)-1] <= 'z' {
		return s, nil
	}
	if utf8.RuneCountInString(s) == 1 {
		r, _ := utf8.DecodeRuneInString(s)
		if unicode.IsPrint(r) {
			return s, nil
		}
	}
	if strings.HasPrefix(s, "alt+") {
		tail := strings.TrimPrefix(s, "alt+")
		if utf8.RuneCountInString(tail) == 1 {
			r, _ := utf8.DecodeRuneInString(tail)
			if unicode.IsPrint(r) {
				return s, nil
			}
		}
	}
	return "", fmt.Errorf("unsupported key %q", raw)
}
