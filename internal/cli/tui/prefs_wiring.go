package tui

import (
	"github.com/x6nux/yanshi/internal/keymap"
)

// themeForPrefs resolves the effective theme name, with high contrast winning
// over an explicit theme choice.
//
// The precedence is deliberate and asymmetric: high_contrast is an
// accessibility switch, and a project config that also names a low-contrast
// theme must not override an accessibility need. An unknown theme name falls
// back to the default rather than failing startup — the TUI is what you would
// use to fix a bad theme name.
func themeForPrefs(eff EffectivePreferences) ThemeName {
	if eff.HighContrast {
		return ThemeHighContrast
	}
	name := ThemeName(eff.ThemeName)
	if _, ok := themeByName(name); !ok {
		return ThemeDefault
	}
	return name
}

// buildKeymap resolves the effective key map from the cascade.
//
// project.KeymapReset (the tombstone in Preferences) is honoured here: a user
// who ran /keymap reset gets built-in defaults even while project config still
// carries tui.bindings. That field existed with no writer and no reader, which
// made it a specification with no implementation.
//
// User-level bindings (eff.KeymapBindings, written by /keymap bind) are merged
// over project bindings so the user's personal shortcuts survive a project
// config change.
//
// A build error is NOT fatal. Map is still returned populated by
// buildInternal, and Diagnostics() carries the reason — which is exactly what
// /keymap diagnostics prints and what doctor's failure message points users
// at. Refusing to start would leave the operator with a broken keymap and no
// tool that reports why.
func buildKeymap(eff EffectivePreferences, project Preferences) *keymap.Map {
	var overrides map[string]string
	if !eff.KeymapReset {
		// Merge project bindings then user bindings (user wins on conflict).
		if len(projectBindings)+len(eff.KeymapBindings) > 0 {
			overrides = make(map[string]string, len(projectBindings)+len(eff.KeymapBindings))
			for k, v := range projectBindings {
				overrides[k] = v
			}
			for k, v := range eff.KeymapBindings {
				overrides[k] = v
			}
		}
	}
	_ = project
	m, _ := keymap.NewDefaultBuilder(overrides).Build()
	return m
}

// projectBindings holds tui.bindings from config.yaml. It is package state
// rather than a Preferences field because Preferences is the JSON shape
// persisted to prefs.json, and a map of key overrides read from the project's
// config has no business being written back into the user's file.
var projectBindings map[string]string

// SetProjectBindings installs the tui.bindings map from config.yaml. Called
// once by the TUI entry point before the program starts.
func SetProjectBindings(b map[string]string) { projectBindings = b }
