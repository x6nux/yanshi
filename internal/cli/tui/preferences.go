package tui

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Preferences is one sparse preference layer. It is also the JSON shape for
// user-level TUI preferences persisted to disk. Pointer booleans distinguish
// unset (nil) from explicitly false, so a higher layer can disable a true
// value from project config without losing intent.
type Preferences struct {
	UILocale     string `json:"ui_locale,omitempty"`
	ThemeName    string `json:"theme,omitempty"`
	KeymapName   string `json:"keymap,omitempty"`
	HighContrast *bool  `json:"high_contrast,omitempty"`
	Vim          *bool  `json:"vim,omitempty"`
	// Frecency gates the file-usage ranking behind @path completion (UX4).
	// A *bool because "unset" has to stay distinguishable from an explicit
	// false: the default is ON, so a nil here means "use the default" and a
	// false means "the user turned it off".
	Frecency *bool `json:"frecency,omitempty"`
	// KeymapReset is a sparse user-level tombstone for project tui.bindings:
	// a stored true would let defaults survive the next startup, nil means
	// project bindings may still apply. Nothing writes it outside tests —
	// see the package note on preferencesPath.
	KeymapReset *bool `json:"keymap_reset,omitempty"`
}

// EffectivePreferences is the fully merged, non-sparse result of the cascade.
// It never contains tri-state values. newModel populates one from hardcoded
// defaults but never reads it back, so nothing in the TUI is driven by these
// values yet — see the wiring note on preferencesPath.
type EffectivePreferences struct {
	UILocale     string
	ThemeName    string
	KeymapName   string
	HighContrast bool
	Vim          bool
	Frecency     bool
	KeymapReset  bool
}

// preferencesPersistDisabled mirrors persistPermMode: tests flip it to skip
// actual disk writes. See TestPreferences_CleanupRestoresState.
var preferencesPersistDisabled = false

// replacePreferencesFile is a test seam around the OS-specific atomic
// replace. Tests restore it with t.Cleanup; production points at the
// build-tagged helper.
var replacePreferencesFile = replacePreferencesFileOS

// loadPreferences reads one sparse user layer. A missing file returns an
// empty layer rather than forcing "auto", because an absent user value
// must not mask project config during the four-level merge.
func loadPreferences(path string) (Preferences, error) {
	var p Preferences
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil
		}
		return p, err
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return Preferences{}, fmt.Errorf("decode TUI preferences: %w", err)
	}
	return p, nil
}

// persistPreferences writes prefs atomically: .tmp.<rand> then rename. On
// rename failure the tmp file is removed and the existing prefs file is
// left untouched.
func persistPreferences(path string, p Preferences) error {
	if preferencesPersistDisabled {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	tmp := path + ".tmp." + suffix
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := replacePreferencesFile(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func randomSuffix() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate preferences temp suffix: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// mergeTUIPrefs applies sparse layers from lowest to highest priority:
// defaults < project config < user prefs < env < flags. All five fields are
// merged uniformly; pointer booleans make explicit false override lower
// true. KeymapReset participates in the same cascade so a stored keymap-reset
// tombstone would survive a project config carrying tui.bindings.
func mergeTUIPrefs(flags, env, user, project Preferences) EffectivePreferences {
	out := EffectivePreferences{
		UILocale:   "auto",
		ThemeName:  "default",
		KeymapName: "default",
		// Frecency defaults ON: it is the ranking behind @path completion, and
		// an unranked completion list is the pre-UX4 behaviour nobody asked to
		// keep. The switch exists for users who do not want a usage profile
		// stored at all.
		Frecency: true,
	}
	apply := func(layer Preferences) {
		if layer.UILocale != "" {
			out.UILocale = layer.UILocale
		}
		if layer.ThemeName != "" {
			out.ThemeName = layer.ThemeName
		}
		if layer.KeymapName != "" {
			out.KeymapName = layer.KeymapName
		}
		if layer.HighContrast != nil {
			out.HighContrast = *layer.HighContrast
		}
		if layer.Vim != nil {
			out.Vim = *layer.Vim
		}
		if layer.Frecency != nil {
			out.Frecency = *layer.Frecency
		}
		if layer.KeymapReset != nil {
			out.KeymapReset = *layer.KeymapReset
		}
	}
	apply(project)
	apply(user)
	apply(env)
	apply(flags)
	return out
}

// preferencesPathFn is the seam tests point at a temp dir. Production uses
// preferencesPath; a test that swapped os.UserConfigDir instead would race
// every other test in the package that reads the same directory.
var preferencesPathFn = preferencesPath

// preferencesPath mirrors permModeFile: os.UserConfigDir()/yanshi/prefs.json.
//
// WIRED as of W8: newModelWithPrefs reads the user layer through
// preferencesPathFn, merges it with the env and project layers via
// mergeTUIPrefs, and the /keymap, /vim, /contrast and /locale commands write
// it back. Before that the whole cascade was implemented, tested, and had no
// production caller — typing /vim produced "unknown command".
func preferencesPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "yanshi", "prefs.json")
}

// PreferencesFromEnv constructs the sparse environment layer. Empty
// variables remain unset; invalid booleans fail startup instead of
// silently changing mode. The getenv function is parameterized so tests
// can inject a stub without mutating process env.
func PreferencesFromEnv(getenv func(string) string) (Preferences, error) {
	p := Preferences{
		UILocale:   strings.TrimSpace(getenv("YANSHI_UI_LOCALE")),
		ThemeName:  strings.TrimSpace(getenv("YANSHI_THEME")),
		KeymapName: strings.TrimSpace(getenv("YANSHI_KEYMAP")),
	}
	var err error
	if p.HighContrast, err = optionalBool(getenv("YANSHI_HIGH_CONTRAST")); err != nil {
		return Preferences{}, fmt.Errorf("YANSHI_HIGH_CONTRAST: %w", err)
	}
	if p.Vim, err = optionalBool(getenv("YANSHI_VIM")); err != nil {
		return Preferences{}, fmt.Errorf("YANSHI_VIM: %w", err)
	}
	return p, nil
}

func optionalBool(raw string) (*bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return nil, fmt.Errorf("expected boolean, got %q", raw)
	}
	return &v, nil
}
