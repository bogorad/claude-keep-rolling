package main

import (
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bogorad/claude-keep-rolling/internal/provider"
	"github.com/bogorad/claude-keep-rolling/internal/rolling"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func testSettings(t *testing.T, raw string) rolling.Settings {
	t.Helper()
	settings, err := rolling.ParseSettings([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

func account(id, providerID string) tracked {
	adapter, ok := provider.Get(providerID)
	if !ok {
		panic("unknown provider " + providerID)
	}
	return tracked{entry: pluginapi.HostAuthFileEntry{ID: id, AuthIndex: "idx-" + id, Provider: providerID}, adapter: adapter}
}

func ids(accounts []tracked) []string {
	out := make([]string, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, a.entry.ID)
	}
	return out
}

func TestClaimDue(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	r := newRoller()
	r.random = func() float64 { return 0 }
	settings := testSettings(t, "stagger: 0s\nproviders:\n  codex:\n    select_all: false\n    accounts: [codex-on]\n  meta:\n    enabled: true\n")

	disabled := account("claude-disabled", "claude")
	disabled.entry.Disabled = true
	cooling := account("claude-cooling", "claude")
	cooling.entry.NextRetryAfter = now.Add(time.Hour)
	accounts := []tracked{
		account("claude-new", "claude"),
		account("claude-due", "claude"),
		account("claude-later", "claude"),
		account("claude-busy", "claude"),
		disabled,
		cooling,
		account("codex-on", "codex"),
		account("codex-off", "codex"),
		account("meta-a", "meta"),
		account("xai-a", "xai"),
	}
	r.states["claude-due"] = &accountState{NextPingAt: now}
	r.states["claude-later"] = &accountState{NextPingAt: now.Add(time.Minute)}
	r.states["claude-busy"] = &accountState{Pinging: true, NextPingAt: now}
	r.states["claude-cooling"] = &accountState{NextPingAt: now}
	r.states["removed"] = &accountState{}

	claimed := ids(r.claimDue(accounts, settings, now))
	if want := []string{"claude-new", "claude-due", "codex-on"}; !slices.Equal(claimed, want) {
		t.Fatalf("claimed %v, want %v", claimed, want)
	}
	for _, id := range claimed {
		if !r.states[id].Pinging {
			t.Fatalf("%s should be marked as pinging", id)
		}
	}
	if got := r.states["claude-cooling"].NextPingAt; !got.Equal(now.Add(time.Hour + rolling.ResetBuffer)) {
		t.Fatalf("cooling account deferred to %s", got)
	}
	if _, ok := r.states["removed"]; ok {
		t.Fatal("state for a removed account should be dropped")
	}
	for _, id := range []string{"meta-a", "xai-a", "codex-off", "claude-disabled"} {
		if state, ok := r.states[id]; ok && !state.NextPingAt.IsZero() {
			t.Fatalf("%s must not be scheduled", id)
		}
	}
	if again := r.claimDue(accounts, settings, now); len(again) != 0 {
		t.Fatalf("accounts already pinging were claimed again: %v", ids(again))
	}
}

func TestClaimDueStaggersFirstPings(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	r := newRoller()
	r.random = func() float64 { return 0 }
	settings := testSettings(t, "stagger: 2m\n")
	accounts := []tracked{account("a", "claude"), account("b", "claude"), account("c", "codex")}

	claimed := ids(r.claimDue(accounts, settings, now))
	if want := []string{"a"}; !slices.Equal(claimed, want) {
		t.Fatalf("first round claimed %v, want %v", claimed, want)
	}
	if got := r.states["b"].NextPingAt; !got.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("b slot %s", got)
	}
	if got := r.states["c"].NextPingAt; !got.Equal(now.Add(4 * time.Minute)) {
		t.Fatalf("c slot %s", got)
	}
	// The ping for a would set its next time; simulate that.
	r.release("a")
	r.states["a"].NextPingAt = now.Add(time.Hour)
	later := ids(r.claimDue(accounts, settings, now.Add(2*time.Minute)))
	if want := []string{"b"}; !slices.Equal(later, want) {
		t.Fatalf("second round claimed %v, want %v", later, want)
	}
}

func TestCooldownsToReset(t *testing.T) {
	reset := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	now := reset.Add(time.Minute)
	r := newRoller()
	settings := testSettings(t, "providers:\n  meta:\n    enabled: true\n")

	cooled := account("claude-cooled", "claude")
	cooled.entry.NextRetryAfter = now.Add(3 * time.Hour)
	free := account("claude-free", "claude")
	future := account("claude-future", "claude")
	future.entry.Unavailable = true
	metaCooled := account("meta-cooled", "meta")
	metaCooled.entry.Unavailable = true
	xaiCooled := account("xai-cooled", "xai")
	xaiCooled.entry.Unavailable = true
	r.states["claude-cooled"] = &accountState{Weekly: provider.Weekly{Reset: reset}}
	r.states["claude-free"] = &accountState{Weekly: provider.Weekly{Reset: reset}}
	r.states["claude-future"] = &accountState{Weekly: provider.Weekly{Reset: now.Add(time.Hour)}}

	// meta resets Monday 00:00 UTC; 2026-11-17 is a Tuesday, so use a Monday.
	monday := time.Date(2026, 11, 16, 0, 0, 30, 0, time.UTC)
	got := ids(r.cooldownsToReset([]tracked{cooled, free, future, xaiCooled}, settings, now))
	if want := []string{"claude-cooled"}; !slices.Equal(got, want) {
		t.Fatalf("reset %v, want %v", got, want)
	}
	if !r.states["claude-free"].CooldownResetFor.Equal(reset) {
		t.Fatal("the decision for a free account must still be recorded")
	}
	if !r.states["claude-cooled"].LastResetSeen.Equal(reset) {
		t.Fatal("a passed reset should be recorded as seen")
	}
	if again := r.cooldownsToReset([]tracked{cooled}, settings, now.Add(time.Minute)); len(again) != 0 {
		t.Fatal("a cooldown must be cleared at most once per reset")
	}
	free.entry.Unavailable = true
	if again := r.cooldownsToReset([]tracked{free}, settings, now.Add(10*time.Minute)); len(again) != 0 {
		t.Fatal("a cooldown that starts after the decision must be left alone")
	}

	if got := ids(r.cooldownsToReset([]tracked{metaCooled}, settings, monday)); !slices.Equal(got, []string{"meta-cooled"}) {
		t.Fatalf("meta at the Monday reset: %v", got)
	}

	off := testSettings(t, "cooldown_reset: false\n")
	r2 := newRoller()
	r2.states["claude-cooled"] = &accountState{Weekly: provider.Weekly{Reset: reset}}
	if got := r2.cooldownsToReset([]tracked{cooled}, off, now); len(got) != 0 {
		t.Fatal("cooldown_reset: false must disable resets")
	}
}

func TestSelectAccounts(t *testing.T) {
	files := []pluginapi.HostAuthFileEntry{
		{ID: "codex-b", Provider: "codex", Email: "b@example.com"},
		{ID: "claude-z", Provider: "claude", Email: "z@example.com"},
		{ID: "", Provider: "claude"},
		{ID: "claude-key", Provider: "claude", AccountType: "api_key"},
		{ID: "gemini-a", Provider: "gemini"},
		{ID: "claude-a", Provider: "claude", Email: "A@example.com"},
		{ID: "meta-a", Provider: "meta"},
	}
	if got, want := ids(selectAccounts(files)), []string{"claude-a", "claude-z", "codex-b", "meta-a"}; !slices.Equal(got, want) {
		t.Fatalf("selectAccounts = %v, want %v", got, want)
	}
}

func TestClaimAllowsOnePingPerAccount(t *testing.T) {
	r := newRoller()
	a, b := account("a", "claude"), account("b", "codex")
	if !r.claim(a) {
		t.Fatal("first claim should succeed")
	}
	if r.claim(a) {
		t.Fatal("second claim should fail while the first ping runs")
	}
	if !r.claim(b) {
		t.Fatal("another account should not be blocked")
	}
	r.release("a")
	if !r.claim(a) {
		t.Fatal("claim should succeed after release")
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		name  string
		value string
		limit int
		want  string
	}{
		{"short", "  rate limited  ", 20, "rate limited"},
		{"exact", "abcde", 5, "abcde"},
		{"ascii", "abcdef", 5, "abcde..."},
		// "é" is two bytes, so a 5-byte cut lands inside the third one.
		{"multibyte", "ééé", 5, "éé..."},
		{"emoji", "ok 👍👍", 5, "ok ..."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.value, tc.limit)
			if got != tc.want {
				t.Fatalf("truncate(%q, %d) = %q, want %q", tc.value, tc.limit, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("truncate(%q, %d) = %q is not valid UTF-8", tc.value, tc.limit, got)
			}
			if len(strings.TrimSuffix(got, "...")) > tc.limit {
				t.Fatalf("truncate(%q, %d) = %q is over the limit", tc.value, tc.limit, got)
			}
		})
	}
}

func TestRegistrationDeclaresOnlyManagement(t *testing.T) {
	reg := pluginRegistration()
	if !reg.Capabilities.ManagementAPI {
		t.Fatal("management capability missing")
	}
	if reg.Metadata.Name != "Keep Rolling" || pluginID != "keep-rolling" {
		t.Fatalf("unexpected identity %q %q", reg.Metadata.Name, pluginID)
	}
}
