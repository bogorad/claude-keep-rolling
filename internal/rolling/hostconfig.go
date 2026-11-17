package rolling

import (
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// HostConfigPath returns the CLIProxyAPI config file path the way the host resolves it:
// the -config flag, otherwise config.yaml in the working directory.
func HostConfigPath(args []string, workDir string) string {
	for i, arg := range args {
		name, value, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if !strings.HasPrefix(arg, "-") || name != "config" {
			continue
		}
		if hasValue {
			value = strings.TrimSpace(value)
		} else if i+1 < len(args) {
			value = strings.TrimSpace(args[i+1])
		}
		if value != "" {
			return value
		}
	}
	return filepath.Join(workDir, "config.yaml")
}

// PluginEnabled reports whether the host config enables plugins and this plugin.
// The host keeps a disabled plugin's library loaded without notifying it, so the
// plugin reads its own enabled flag before acting. Only the two flags are decoded.
func PluginEnabled(raw []byte, pluginID string) (bool, error) {
	var cfg struct {
		Plugins struct {
			Enabled bool `yaml:"enabled"`
			Configs map[string]struct {
				Enabled bool `yaml:"enabled"`
			} `yaml:"configs"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return false, err
	}
	return cfg.Plugins.Enabled && cfg.Plugins.Configs[pluginID].Enabled, nil
}
