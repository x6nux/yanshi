package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// PluginManifest is the minimal plugin.json we read (extra fields ignored).
type PluginManifest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
}

// DiscoverPlugins scans pluginRoot/*/.yanshi-plugin/plugin.json and returns
// a Root for each plugin that also has a skills/ directory.
func DiscoverPlugins(pluginRoot string) ([]Root, error) {
	entries, err := os.ReadDir(pluginRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var roots []Root
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		manifest := filepath.Join(pluginRoot, e.Name(), ".yanshi-plugin", "plugin.json")
		if _, err := os.Stat(manifest); err != nil {
			continue
		}
		data, err := os.ReadFile(manifest)
		if err != nil {
			continue
		}
		var pm PluginManifest
		if err := json.Unmarshal(data, &pm); err != nil || pm.Name == "" {
			continue
		}
		skillsDir := filepath.Join(pluginRoot, e.Name(), "skills")
		if _, err := os.Stat(skillsDir); err != nil {
			continue
		}
		roots = append(roots, Plugin(pm.Name, skillsDir))
	}
	return roots, nil
}
