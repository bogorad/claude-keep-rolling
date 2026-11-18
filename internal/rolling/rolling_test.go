package rolling

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/bogorad/claude-keep-rolling/internal/provider"
)

func TestParseSettingsDefaults(t *testing.T) {
	settings, err := ParseSettings([]byte("enabled: true\npriority: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if settings.Prompt != DefaultPrompt || time.Duration(settings.DelayMin) != time.Minute || time.Duration(settings.DelayMax) != 10*time.Minute ||
		time.Duration(settings.Stagger) != 2*time.Minute || !settings.CooldownReset || time.Duration(settings.MetaInterval) != 60*time.Minute {
		t.Fatalf("unexpected defaults: %+v", settings)
	}
	want := map[string]bool{"claude": true, "codex": true, "meta": false, "xai": false, "antigravity": false}
	if len(settings.Providers) != len(want) {
		t.Fatalf("providers = %v", settings.Providers)
	}
	for id, enabled := range want {
		p := settings.Provider(id)
		adapter, _ := provider.Get(id)
		if p.Enabled != enabled || !p.SelectAll || p.Model != adapter.DefaultModel() || p.Accounts == nil {
			t.Fatalf("%s defaults: %+v", id, p)
		}
	}
	if settings.Provider("unknown").Enabled {
		t.Fatal("an unknown provider must be disabled")
	}
}

func TestParseSettingsExplicit(t *testing.T) {
	raw := []byte(`
enabled: true
prompt: "  hello there "
delay_min: 2m
delay_max: 900
stagger: 30s
cooldown_reset: false
meta_interval: 30m
providers:
  claude:
    enabled: false
    model: claude-sonnet-4-6
  codex:
    select_all: false
    accounts: [codex-a.json, codex-b.json]
  meta:
    enabled: true
`)
	settings, err := ParseSettings(raw)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Prompt != "  hello there " {
		t.Fatalf("prompt should be sent as typed, got %q", settings.Prompt)
	}
	if time.Duration(settings.DelayMin) != 2*time.Minute || time.Duration(settings.DelayMax) != 15*time.Minute || time.Duration(settings.Stagger) != 30*time.Second ||
		time.Duration(settings.MetaInterval) != 30*time.Minute {
		t.Fatalf("durations: %+v", settings)
	}
	if settings.CooldownReset {
		t.Fatal("cooldown_reset should be false")
	}
	claude := settings.Provider("claude")
	if claude.Enabled || claude.Model != "claude-sonnet-4-6" || !claude.SelectAll {
		t.Fatalf("claude: %+v", claude)
	}
	codex := settings.Provider("codex")
	if !codex.Enabled || codex.SelectAll || !codex.Selected("codex-b.json") || codex.Selected("codex-c.json") || codex.Model != "gpt-6-luna" {
		t.Fatalf("codex: %+v", codex)
	}
	if !settings.Provider("meta").Enabled {
		t.Fatal("meta should be enabled")
	}
}

func TestParseSettingsErrors(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"invalid yaml", "providers: [unclosed"},
		{"unknown provider", "providers:\n  gemini:\n    enabled: true\n"},
		{"bad duration", "delay_min: soon\n"},
		{"delay below minimum", "delay_min: 30s\n"},
		{"max below min", "delay_min: 5m\ndelay_max: 2m\n"},
		{"max above bound", "delay_max: 7h\n"},
		{"negative stagger", "stagger: -1s\n"},
		{"stagger above bound", "stagger: 2h\n"},
		{"meta interval below bound", "meta_interval: 4m\n"},
		{"meta interval above bound", "meta_interval: 25h\n"},
		{"meta interval zero", "meta_interval: 0s\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseSettings([]byte(tc.raw)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestParseSettingsBlankPromptFallsBack(t *testing.T) {
	settings, err := ParseSettings([]byte("prompt: \"  \"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if settings.Prompt != DefaultPrompt {
		t.Fatalf("prompt = %q", settings.Prompt)
	}
}

func TestDurationJSON(t *testing.T) {
	var d struct {
		A Duration `json:"a"`
		B Duration `json:"b"`
	}
	if err := json.Unmarshal([]byte(`{"a":"90s","b":120}`), &d); err != nil {
		t.Fatal(err)
	}
	if time.Duration(d.A) != 90*time.Second || time.Duration(d.B) != 2*time.Minute {
		t.Fatalf("decoded %v %v", time.Duration(d.A), time.Duration(d.B))
	}
	out, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"a":"1m30s","b":"2m0s"}` {
		t.Fatalf("encoded %s", out)
	}
	if err := json.Unmarshal([]byte(`{"a":true}`), &d); err == nil {
		t.Fatal("expected an error for a boolean duration")
	}
}

func TestSelectAllIncludesUnknownAccounts(t *testing.T) {
	if !(ProviderSettings{SelectAll: true}).Selected("added-later.json") {
		t.Fatal("select all must include accounts added later")
	}
	if (ProviderSettings{}).Selected("x.json") {
		t.Fatal("an empty selection must include nothing")
	}
}

func TestJitterBounds(t *testing.T) {
	minDelay, maxDelay := time.Minute, 10*time.Minute
	for _, frac := range []float64{-1, 0, 0.001, 0.25, 0.5, 0.999999, 1, 2} {
		got := Jitter(minDelay, maxDelay, frac)
		if got < minDelay || got > maxDelay {
			t.Fatalf("Jitter(%v) = %s, outside [%s, %s]", frac, got, minDelay, maxDelay)
		}
	}
	if got := Jitter(minDelay, maxDelay, 0); got != minDelay {
		t.Fatalf("Jitter(0) = %s", got)
	}
	if got := Jitter(minDelay, maxDelay, 0.5); got != 5*time.Minute+30*time.Second {
		t.Fatalf("Jitter(0.5) = %s", got)
	}
	if got := Jitter(5*time.Minute, 5*time.Minute, 0.7); got != 5*time.Minute {
		t.Fatalf("equal bounds: %s", got)
	}
	if got := Jitter(5*time.Minute, time.Minute, 0.7); got != 5*time.Minute {
		t.Fatalf("inverted bounds: %s", got)
	}
}

func TestNextSlotStaggers(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	step := 2 * time.Minute
	cases := []struct {
		name string
		now  time.Time
		last time.Time
		frac float64
		want time.Time
	}{
		{"first slot", now, time.Time{}, 0, now},
		{"first slot with offset", now, time.Time{}, 0.5, now.Add(30 * time.Second)},
		{"after previous slot", now, now.Add(time.Minute), 0, now.Add(3 * time.Minute)},
		{"previous slot long past", now, now.Add(-time.Hour), 0, now},
		{"no stagger", now, now.Add(time.Minute), 0.9, now},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := step
			if tc.name == "no stagger" {
				s = 0
			}
			if got := NextSlot(tc.now, tc.last, s, tc.frac); !got.Equal(tc.want) {
				t.Fatalf("NextSlot = %s, want %s", got, tc.want)
			}
		})
	}

	// Ten accounts found in one round: slots are at least one step apart and
	// never before now.
	var last time.Time
	for i := range 10 {
		slot := NextSlot(now, last, step, float64(i%3)/3)
		if slot.Before(now) {
			t.Fatalf("slot %d before now", i)
		}
		if !last.IsZero() && slot.Sub(last) < step {
			t.Fatalf("slot %d only %s after the previous one", i, slot.Sub(last))
		}
		last = slot
	}
}

func TestNextPing(t *testing.T) {
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	delay := 4 * time.Minute
	reset := at.Add(3 * 24 * time.Hour)
	weekly := func(r time.Time) provider.Weekly { return provider.Weekly{Seen: true, Reset: r, Window: provider.Week} }
	cases := []struct {
		name    string
		outcome Outcome
		want    time.Time
	}{
		{"reset ahead", Outcome{At: at, OK: true, Weekly: weekly(reset), Delay: delay}, reset.Add(delay)},
		{"new window just started", Outcome{At: at, OK: true, Weekly: weekly(at.Add(provider.Week)), Delay: delay}, at.Add(provider.Week + delay)},
		{"weekly limit used up waits for reset", Outcome{At: at, OK: true, Weekly: provider.Weekly{Seen: true, Reset: reset, Window: provider.Week, Reached: true}, Delay: delay}, reset.Add(delay)},
		{"zero window defaults to a week", Outcome{At: at, OK: true, Weekly: provider.Weekly{Reset: at.Add(provider.Week)}, Delay: delay}, at.Add(provider.Week + delay)},
		{"reset too far ahead", Outcome{At: at, OK: true, Weekly: weekly(at.Add(provider.Week + 2*time.Hour)), Delay: delay}, at.Add(RecheckFallback)},
		{"longer reported window allows a later reset", Outcome{At: at, OK: true, Weekly: provider.Weekly{Reset: at.Add(8 * 24 * time.Hour), Window: 8 * 24 * time.Hour}, Delay: delay}, at.Add(8*24*time.Hour + delay)},
		{"reset at ping time is clock skew", Outcome{At: at, OK: true, Weekly: weekly(at), Delay: delay}, at.Add(SkewRetry)},
		{"reset just before ping is clock skew", Outcome{At: at, OK: true, Weekly: weekly(at.Add(-SkewTolerance)), Delay: delay}, at.Add(SkewRetry)},
		{"stale reset", Outcome{At: at, OK: true, Weekly: weekly(at.Add(-time.Hour)), Delay: delay}, at.Add(RecheckFallback)},
		{"no reset", Outcome{At: at, OK: true, Delay: delay}, at.Add(RecheckFallback)},
		{"failure", Outcome{At: at}, at.Add(RetryAfterFailure)},
		{"failure with later cooldown", Outcome{At: at, CooldownUntil: at.Add(time.Hour)}, at.Add(time.Hour + ResetBuffer)},
		{"failure with short cooldown", Outcome{At: at, CooldownUntil: at.Add(time.Minute)}, at.Add(RetryAfterFailure)},
		{"failed manual ping keeps the schedule", Outcome{At: at, Manual: true, Scheduled: reset.Add(delay)}, reset.Add(delay)},
		{"failed manual ping keeps an earlier schedule", Outcome{At: at, Manual: true, Scheduled: at.Add(time.Minute)}, at.Add(time.Minute)},
		{"failed manual ping without schedule", Outcome{At: at, Manual: true}, at.Add(RetryAfterFailure)},
		{"successful manual ping reschedules", Outcome{At: at, OK: true, Manual: true, Scheduled: at.Add(time.Minute), Weekly: weekly(reset), Delay: delay}, reset.Add(delay)},
		{"interval success ignores reset and delay", Outcome{At: at, OK: true, Weekly: weekly(reset), Delay: delay, Interval: time.Hour}, at.Add(time.Hour)},
		{"interval success without reset", Outcome{At: at, OK: true, Interval: 30 * time.Minute}, at.Add(30 * time.Minute)},
		{"interval manual success reschedules", Outcome{At: at, OK: true, Manual: true, Scheduled: at.Add(10 * time.Minute), Interval: time.Hour}, at.Add(time.Hour)},
		{"interval failure retries", Outcome{At: at, Interval: time.Hour}, at.Add(RetryAfterFailure)},
		{"interval failure waits for cooldown", Outcome{At: at, Interval: time.Hour, CooldownUntil: at.Add(2 * time.Hour)}, at.Add(2*time.Hour + ResetBuffer)},
		{"interval failed manual ping keeps the schedule", Outcome{At: at, Manual: true, Scheduled: at.Add(10 * time.Minute), Interval: time.Hour}, at.Add(10 * time.Minute)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NextPing(tc.outcome); !got.Equal(tc.want) {
				t.Fatalf("NextPing = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestDeferForCooldown(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		next       time.Time
		retryAfter time.Time
		want       time.Time
		wait       bool
	}{
		{"no cooldown", now, time.Time{}, now, false},
		{"cooldown over", now, now.Add(-time.Second), now, false},
		{"cooldown at now is over", now, now, now, false},
		{"active cooldown", now, now.Add(time.Hour), now.Add(time.Hour + ResetBuffer), true},
		{"next already later", now.Add(2 * time.Hour), now.Add(time.Hour), now.Add(2 * time.Hour), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, wait := DeferForCooldown(now, tc.next, tc.retryAfter)
			if !got.Equal(tc.want) || wait != tc.wait {
				t.Fatalf("DeferForCooldown = %s, %v; want %s, %v", got, wait, tc.want, tc.wait)
			}
		})
	}
}

func TestCooldownDecision(t *testing.T) {
	reset := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	base := CooldownCheck{Now: reset.Add(30 * time.Second), Enabled: true, Reset: reset, NextRetryAfter: reset.Add(time.Hour)}
	with := func(change func(*CooldownCheck)) CooldownCheck {
		c := base
		change(&c)
		return c
	}
	cases := []struct {
		name          string
		check         CooldownCheck
		decide, clear bool
	}{
		{"active cooldown after reset", base, true, true},
		{"unavailable flag only", with(func(c *CooldownCheck) { c.NextRetryAfter = time.Time{}; c.Unavailable = true }), true, true},
		{"no cooldown: decide, do not clear", with(func(c *CooldownCheck) { c.NextRetryAfter = time.Time{} }), true, false},
		{"cooldown already over", with(func(c *CooldownCheck) { c.NextRetryAfter = c.Now.Add(-time.Second) }), true, false},
		{"disabled setting", with(func(c *CooldownCheck) { c.Enabled = false }), false, false},
		{"reset unknown", with(func(c *CooldownCheck) { c.Reset = time.Time{} }), false, false},
		{"reset not passed yet", with(func(c *CooldownCheck) { c.Now = reset.Add(-time.Second) }), false, false},
		{"reset exactly now", with(func(c *CooldownCheck) { c.Now = reset }), true, true},
		{"reset too long ago", with(func(c *CooldownCheck) { c.Now = reset.Add(CooldownResetWindow + time.Second) }), false, false},
		{"already decided for this reset", with(func(c *CooldownCheck) { c.DoneFor = reset }), false, false},
		{"decided for an older reset", with(func(c *CooldownCheck) { c.DoneFor = reset.Add(-provider.Week) }), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decide, clear := CooldownDecision(tc.check)
			if decide != tc.decide || clear != tc.clear {
				t.Fatalf("CooldownDecision = %v, %v; want %v, %v", decide, clear, tc.decide, tc.clear)
			}
		})
	}
}

func TestHostConfigPath(t *testing.T) {
	wd := filepath.Join("srv", "cpa")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"separate value", []string{"/CLIProxyAPI/CLIProxyAPI", "-config", "/data/config.yaml", "-local-model"}, "/data/config.yaml"},
		{"equals form", []string{"cpa", "--config=/etc/cpa.yaml"}, "/etc/cpa.yaml"},
		{"double dash separate", []string{"cpa", "--config", "c.yaml"}, "c.yaml"},
		{"no flag", []string{"cpa", "-local-model"}, filepath.Join(wd, "config.yaml")},
		{"flag without value", []string{"cpa", "-config"}, filepath.Join(wd, "config.yaml")},
		{"empty equals", []string{"cpa", "-config="}, filepath.Join(wd, "config.yaml")},
		{"similar flag", []string{"cpa", "-config-dir", "x"}, filepath.Join(wd, "config.yaml")},
		{"value named config", []string{"cpa", "config"}, filepath.Join(wd, "config.yaml")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HostConfigPath(tc.args, wd); got != tc.want {
				t.Fatalf("HostConfigPath = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPluginEnabled(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"enabled", "plugins:\n  enabled: true\n  configs:\n    keep-rolling:\n      enabled: true\n", true},
		{"plugin disabled", "plugins:\n  enabled: true\n  configs:\n    keep-rolling:\n      enabled: false\n", false},
		{"plugins disabled", "plugins:\n  enabled: false\n  configs:\n    keep-rolling:\n      enabled: true\n", false},
		{"only the old plugin configured", "plugins:\n  enabled: true\n  configs:\n    claude-keep-rolling:\n      enabled: true\n", false},
		{"no plugins section", "port: 8317\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PluginEnabled([]byte(tc.raw), "keep-rolling")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("PluginEnabled = %v, want %v", got, tc.want)
			}
		})
	}
	if _, err := PluginEnabled([]byte("plugins: [unclosed"), "keep-rolling"); err == nil {
		t.Fatal("expected a parse error")
	}
}
