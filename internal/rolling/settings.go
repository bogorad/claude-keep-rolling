// Package rolling holds the host-independent rules for keeping weekly usage
// windows rolling: settings, account selection, ping timing, start-up
// staggering, and the cooldown-reset decision.
package rolling

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bogorad/claude-keep-rolling/internal/provider"
	"gopkg.in/yaml.v3"
)

const (
	DefaultPrompt   = "hi"
	DefaultDelayMin = time.Minute
	DefaultDelayMax = 10 * time.Minute
	DefaultStagger  = 2 * time.Minute

	// MinDelay is the smallest allowed delay after a reset. A ping exactly at
	// the reported reset can land in the old window.
	MinDelay = time.Minute
	// MaxDelay bounds the random delay after a reset.
	MaxDelay = 6 * time.Hour
	// MaxStagger bounds the gap between first pings after a start.
	MaxStagger = time.Hour
)

// Duration is a time.Duration that reads Go duration strings ("90s", "5m")
// or integer seconds, and writes a duration string.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	parsed, err := parseDuration(node.Value)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

func (d *Duration) UnmarshalJSON(raw []byte) error {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	var parsed time.Duration
	var err error
	switch v := value.(type) {
	case string:
		parsed, err = parseDuration(v)
	case float64:
		parsed = time.Duration(v * float64(time.Second))
	default:
		err = fmt.Errorf("invalid duration %s", string(raw))
	}
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func parseDuration(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Duration(seconds) * time.Second, nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", raw)
	}
	return parsed, nil
}

// ProviderSettings is one entry under providers.
type ProviderSettings struct {
	Enabled   bool     `json:"enabled"`
	Model     string   `json:"model"`
	SelectAll bool     `json:"select_all"`
	Accounts  []string `json:"accounts"`
}

// Selected reports whether the account should be kept rolling.
func (p ProviderSettings) Selected(accountID string) bool {
	return p.SelectAll || slices.Contains(p.Accounts, accountID)
}

// Settings is the plugin's block under plugins.configs.keep-rolling.
type Settings struct {
	Prompt string `json:"prompt"`
	// DelayMin and DelayMax bound the random delay after a weekly reset.
	DelayMin Duration `json:"delay_min"`
	DelayMax Duration `json:"delay_max"`
	// Stagger is the minimum gap between first pings after a start.
	Stagger Duration `json:"stagger"`
	// CooldownReset clears an active host cooldown once, right after a weekly reset.
	CooldownReset bool                        `json:"cooldown_reset"`
	Providers     map[string]ProviderSettings `json:"providers"`
}

// Provider returns the resolved settings for a provider key. Unknown keys are disabled.
func (s Settings) Provider(id string) ProviderSettings {
	if p, ok := s.Providers[id]; ok {
		return p
	}
	return ProviderSettings{}
}

type rawProvider struct {
	Enabled   *bool    `yaml:"enabled"`
	Model     string   `yaml:"model"`
	SelectAll *bool    `yaml:"select_all"`
	Accounts  []string `yaml:"accounts"`
}

type rawSettings struct {
	Prompt        string                 `yaml:"prompt"`
	DelayMin      *Duration              `yaml:"delay_min"`
	DelayMax      *Duration              `yaml:"delay_max"`
	Stagger       *Duration              `yaml:"stagger"`
	CooldownReset *bool                  `yaml:"cooldown_reset"`
	Providers     map[string]rawProvider `yaml:"providers"`
}

// ParseSettings reads the plugin config YAML. Missing fields take defaults.
// Each known provider gets an entry; its enabled flag defaults to the
// adapter's default and select_all to true. Unknown providers are an error.
func ParseSettings(raw []byte) (Settings, error) {
	var in rawSettings
	if err := yaml.Unmarshal(raw, &in); err != nil {
		return Settings{}, err
	}
	out := Settings{
		Prompt:        in.Prompt,
		DelayMin:      Duration(DefaultDelayMin),
		DelayMax:      Duration(DefaultDelayMax),
		Stagger:       Duration(DefaultStagger),
		CooldownReset: true,
		Providers:     map[string]ProviderSettings{},
	}
	if strings.TrimSpace(out.Prompt) == "" {
		out.Prompt = DefaultPrompt
	}
	if in.DelayMin != nil {
		out.DelayMin = *in.DelayMin
	}
	if in.DelayMax != nil {
		out.DelayMax = *in.DelayMax
	}
	if in.Stagger != nil {
		out.Stagger = *in.Stagger
	}
	if in.CooldownReset != nil {
		out.CooldownReset = *in.CooldownReset
	}
	if err := out.validate(); err != nil {
		return Settings{}, err
	}
	for id := range in.Providers {
		if _, ok := provider.Get(id); !ok {
			return Settings{}, fmt.Errorf("unknown provider %q", id)
		}
	}
	for _, adapter := range provider.All() {
		entry := in.Providers[adapter.ID()]
		resolved := ProviderSettings{
			Enabled:   adapter.DefaultEnabled(),
			Model:     strings.TrimSpace(entry.Model),
			SelectAll: true,
			Accounts:  entry.Accounts,
		}
		if entry.Enabled != nil {
			resolved.Enabled = *entry.Enabled
		}
		if entry.SelectAll != nil {
			resolved.SelectAll = *entry.SelectAll
		}
		if resolved.Model == "" {
			resolved.Model = adapter.DefaultModel()
		}
		if resolved.Accounts == nil {
			resolved.Accounts = []string{}
		}
		out.Providers[adapter.ID()] = resolved
	}
	return out, nil
}

func (s Settings) validate() error {
	minDelay, maxDelay, stagger := time.Duration(s.DelayMin), time.Duration(s.DelayMax), time.Duration(s.Stagger)
	switch {
	case minDelay < MinDelay:
		return fmt.Errorf("delay_min %s is below %s", minDelay, MinDelay)
	case maxDelay < minDelay:
		return fmt.Errorf("delay_max %s is below delay_min %s", maxDelay, minDelay)
	case maxDelay > MaxDelay:
		return fmt.Errorf("delay_max %s is above %s", maxDelay, MaxDelay)
	case stagger < 0 || stagger > MaxStagger:
		return fmt.Errorf("stagger %s is outside 0s to %s", stagger, MaxStagger)
	}
	return nil
}
