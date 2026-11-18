package provider

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var at = time.Date(2026, 11, 17, 18, 21, 0, 0, time.UTC)

func ptr(v float64) *float64 { return &v }

func sameWeekly(t *testing.T, got, want Weekly) {
	t.Helper()
	if got.Seen != want.Seen || !got.Reset.Equal(want.Reset) || got.Window != want.Window || got.Reached != want.Reached || got.Status != want.Status {
		t.Fatalf("weekly = %+v, want %+v", got, want)
	}
	switch {
	case (got.Utilization == nil) != (want.Utilization == nil):
		t.Fatalf("utilization = %v, want %v", got.Utilization, want.Utilization)
	case got.Utilization != nil && *got.Utilization != *want.Utilization:
		t.Fatalf("utilization = %v, want %v", *got.Utilization, *want.Utilization)
	}
}

func TestClaudeParseWeekly(t *testing.T) {
	reset := time.Unix(1795517462, 0).UTC()
	cases := []struct {
		name    string
		headers http.Header
		want    Weekly
	}{
		{"none", http.Header{}, Weekly{Window: Week}},
		{"unix seconds", http.Header{
			"Anthropic-Ratelimit-Unified-7d-Reset":       {"1795517462"},
			"Anthropic-Ratelimit-Unified-7d-Status":      {"allowed"},
			"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.42"},
		}, Weekly{Seen: true, Reset: reset, Window: Week, Utilization: ptr(0.42), Status: "allowed"}},
		{"rfc3339", http.Header{
			"Anthropic-Ratelimit-Unified-7d-Reset": {"2026-11-24T10:00:00Z"},
		}, Weekly{Seen: true, Reset: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC), Window: Week}},
		{"lower-case keys", http.Header{
			"anthropic-ratelimit-unified-7d-reset": {"1795517462"},
		}, Weekly{Seen: true, Reset: reset, Window: Week}},
		{"rejected", http.Header{
			"Anthropic-Ratelimit-Unified-7d-Status": {"rejected"},
		}, Weekly{Seen: true, Window: Week, Reached: true, Status: "rejected"}},
		{"used up", http.Header{
			"Anthropic-Ratelimit-Unified-7d-Utilization": {"1"},
		}, Weekly{Seen: true, Window: Week, Utilization: ptr(1), Reached: true}},
		{"garbled", http.Header{
			"Anthropic-Ratelimit-Unified-7d-Reset":       {"soon"},
			"Anthropic-Ratelimit-Unified-7d-Utilization": {"full"},
		}, Weekly{Seen: true, Window: Week}},
		{"five-hour only is ignored", http.Header{
			"Anthropic-Ratelimit-Unified-5h-Reset":  {"1795000000"},
			"Anthropic-Ratelimit-Unified-5h-Status": {"allowed"},
		}, Weekly{Window: Week}},
		{"model-scoped weekly is ignored", http.Header{
			"Anthropic-Ratelimit-Unified-7d_oi-Reset": {"1795517462"},
		}, Weekly{Window: Week}},
		{"representative seven_day fallback", http.Header{
			"Anthropic-Ratelimit-Unified-Representative-Claim": {"seven_day"},
			"Anthropic-Ratelimit-Unified-Reset":                {"1795517462"},
		}, Weekly{Seen: true, Reset: reset, Window: Week}},
		{"representative five_hour is not weekly", http.Header{
			"Anthropic-Ratelimit-Unified-Representative-Claim": {"five_hour"},
			"Anthropic-Ratelimit-Unified-Reset":                {"1795517462"},
		}, Weekly{Window: Week}},
		{"negative reset", http.Header{
			"Anthropic-Ratelimit-Unified-7d-Reset": {"-5"},
		}, Weekly{Seen: true, Window: Week}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sameWeekly(t, claude{}.ParseWeekly(tc.headers, at), tc.want)
		})
	}
}

func TestCodexParseWeekly(t *testing.T) {
	reset := time.Unix(1795517462, 0).UTC()
	cases := []struct {
		name    string
		headers http.Header
		want    Weekly
	}{
		{"none", http.Header{}, Weekly{Window: Week}},
		// Probe 2026-11-17, Pro account: primary is the weekly window.
		{"pro primary weekly", http.Header{
			"X-Codex-Primary-Used-Percent":          {"22"},
			"X-Codex-Primary-Window-Minutes":        {"10080"},
			"X-Codex-Primary-Reset-At":              {"1795517462"},
			"X-Codex-Primary-Reset-After-Seconds":   {"577787"},
			"X-Codex-Secondary-Used-Percent":        {"0"},
			"X-Codex-Secondary-Window-Minutes":      {"0"},
			"X-Codex-Secondary-Reset-At":            {""},
			"X-Codex-Secondary-Reset-After-Seconds": {"0"},
		}, Weekly{Seen: true, Reset: reset, Window: Week, Utilization: ptr(0.22)}},
		{"plus secondary weekly", http.Header{
			"X-Codex-Primary-Used-Percent":     {"80"},
			"X-Codex-Primary-Window-Minutes":   {"300"},
			"X-Codex-Primary-Reset-At":         {"1795000000"},
			"X-Codex-Secondary-Used-Percent":   {"100"},
			"X-Codex-Secondary-Window-Minutes": {"10080"},
			"X-Codex-Secondary-Reset-At":       {"1795517462"},
		}, Weekly{Seen: true, Reset: reset, Window: Week, Utilization: ptr(1), Reached: true}},
		{"largest window when none is 10080", http.Header{
			"X-Codex-Primary-Window-Minutes":   {"2880"},
			"X-Codex-Primary-Reset-At":         {"1795000000"},
			"X-Codex-Secondary-Window-Minutes": {"10081"},
			"X-Codex-Secondary-Reset-At":       {"1795517462"},
		}, Weekly{Seen: true, Reset: reset, Window: 10081 * time.Minute}},
		{"five-hour only is ignored", http.Header{
			"X-Codex-Primary-Window-Minutes": {"300"},
			"X-Codex-Primary-Reset-At":       {"1795000000"},
		}, Weekly{Window: Week}},
		{"reset after seconds fallback", http.Header{
			"X-Codex-Primary-Window-Minutes":      {"10080"},
			"X-Codex-Primary-Reset-After-Seconds": {"3600"},
		}, Weekly{Seen: true, Reset: at.Add(time.Hour), Window: Week}},
		{"lower-case keys", http.Header{
			"x-codex-primary-window-minutes": {"10080"},
			"x-codex-primary-reset-at":       {"1795517462"},
		}, Weekly{Seen: true, Reset: reset, Window: Week}},
		{"garbled reset", http.Header{
			"X-Codex-Primary-Window-Minutes": {"10080"},
			"X-Codex-Primary-Reset-At":       {"tomorrow"},
		}, Weekly{Seen: true, Window: Week}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sameWeekly(t, codex{}.ParseWeekly(tc.headers, at), tc.want)
		})
	}
}

func TestBuildPing(t *testing.T) {
	req, err := claude{}.BuildPing("claude-haiku-4-5-20251001", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if req.EntryProtocol != "claude" || req.ExitProtocol != "claude" || req.ForcedProvider != "claude" || req.Model != "claude-haiku-4-5-20251001" {
		t.Fatalf("claude request = %+v", req)
	}
	var claudeBody map[string]any
	if err := json.Unmarshal(req.Body, &claudeBody); err != nil {
		t.Fatal(err)
	}
	if claudeBody["max_tokens"] != float64(1) || claudeBody["tools"] != nil {
		t.Fatalf("claude body = %s", req.Body)
	}

	req, err = codex{}.BuildPing("gpt-6-luna", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if req.EntryProtocol != "openai-response" || req.ExitProtocol != "openai-response" || req.ForcedProvider != "codex" {
		t.Fatalf("codex request = %+v", req)
	}
	var codexBody map[string]any
	if err := json.Unmarshal(req.Body, &codexBody); err != nil {
		t.Fatal(err)
	}
	if codexBody["input"] != "hi" || codexBody["max_output_tokens"] != float64(16) || codexBody["store"] != false || codexBody["instructions"] != "" || codexBody["stream"] != nil {
		t.Fatalf("codex body = %s", req.Body)
	}

	for _, adapter := range []Adapter{xai{}, antigravity{}} {
		if _, err := adapter.BuildPing(adapter.DefaultModel(), "hi"); !errors.Is(err, ErrNoPing) {
			t.Fatalf("%s: BuildPing error = %v, want ErrNoPing", adapter.ID(), err)
		}
	}
}

func TestDefaults(t *testing.T) {
	want := map[string]struct{ enabled, firstUse, canPing bool }{
		"claude":      {true, true, true},
		"codex":       {true, true, true},
		"meta":        {false, false, true},
		"xai":         {false, false, false},
		"antigravity": {false, false, false},
	}
	all := All()
	if len(all) != len(want) {
		t.Fatalf("got %d adapters, want %d", len(all), len(want))
	}
	for _, adapter := range all {
		w, ok := want[adapter.ID()]
		if !ok {
			t.Fatalf("unexpected adapter %s", adapter.ID())
		}
		if adapter.PingsOnInterval() != (adapter.ID() == "meta") {
			t.Fatalf("%s: PingsOnInterval = %v", adapter.ID(), adapter.PingsOnInterval())
		}
		if adapter.DefaultEnabled() != w.enabled || adapter.StartsOnFirstUse() != w.firstUse || CanPing(adapter) != w.canPing {
			t.Fatalf("%s: enabled %v first-use %v can-ping %v, want %+v", adapter.ID(), adapter.DefaultEnabled(), adapter.StartsOnFirstUse(), CanPing(adapter), w)
		}
		if adapter.Note() == "" || adapter.Name() == "" || adapter.DefaultModel() == "" {
			t.Fatalf("%s: missing name, note or default model", adapter.ID())
		}
	}
}

func TestForAccount(t *testing.T) {
	cases := []struct {
		name  string
		entry pluginapi.HostAuthFileEntry
		want  string
	}{
		{"claude oauth", pluginapi.HostAuthFileEntry{Provider: "claude", AccountType: "oauth"}, "claude"},
		{"claude by type", pluginapi.HostAuthFileEntry{Type: "Claude"}, "claude"},
		{"claude api key", pluginapi.HostAuthFileEntry{Provider: "claude", AccountType: "api_key"}, ""},
		{"codex oauth", pluginapi.HostAuthFileEntry{Provider: "codex", AccountType: "oauth"}, "codex"},
		{"codex api key", pluginapi.HostAuthFileEntry{Provider: "codex", AccountType: "api_key"}, ""},
		{"meta", pluginapi.HostAuthFileEntry{Provider: "meta"}, "meta"},
		{"xai", pluginapi.HostAuthFileEntry{Provider: "xai"}, "xai"},
		{"antigravity", pluginapi.HostAuthFileEntry{Provider: "antigravity"}, "antigravity"},
		{"unknown", pluginapi.HostAuthFileEntry{Provider: "gemini"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter, ok := ForAccount(tc.entry)
			got := ""
			if ok {
				got = adapter.ID()
			}
			if got != tc.want {
				t.Fatalf("ForAccount = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMetaFixedReset(t *testing.T) {
	monday := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"monday midnight", monday, monday},
		{"monday noon", monday.Add(12 * time.Hour), monday},
		{"sunday late", monday.Add(Week - time.Second), monday},
		{"next monday", monday.Add(Week), monday.Add(Week)},
		{"tuesday in CET", time.Date(2026, 11, 18, 0, 30, 0, 0, time.FixedZone("CET", 3600)), monday},
		{"sunday before", monday.Add(-time.Hour), monday.Add(-Week)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (meta{}).FixedReset(tc.now); !got.Equal(tc.want) {
				t.Fatalf("FixedReset(%s) = %s, want %s", tc.now, got, tc.want)
			}
		})
	}
	for _, adapter := range []Adapter{claude{}, codex{}, xai{}, antigravity{}} {
		if !adapter.FixedReset(monday).IsZero() {
			t.Fatalf("%s should have no fixed reset", adapter.ID())
		}
	}
}

func TestGet(t *testing.T) {
	if adapter, ok := Get("codex"); !ok || adapter.ID() != "codex" {
		t.Fatal("Get(codex) failed")
	}
	if _, ok := Get("nope"); ok {
		t.Fatal("Get(nope) should fail")
	}
}

func TestMetaBuildPing(t *testing.T) {
	adapter := meta{}
	req, err := adapter.BuildPing(adapter.DefaultModel(), "ignored prompt")
	if err != nil {
		t.Fatal(err)
	}
	if req.EntryProtocol != "openai-response" || req.ExitProtocol != "openai-response" || req.ForcedProvider != "meta" || req.Model != "muse-spark-1.3-contributor" {
		t.Fatalf("meta request = %+v", req)
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatal(err)
	}
	reasoning, _ := body["reasoning"].(map[string]any)
	if body["model"] != "muse-spark-1.3-contributor" || body["input"] != "ping" || body["max_output_tokens"] != float64(16) ||
		body["store"] != false || reasoning["effort"] != "minimal" || body["stream"] != nil || len(body) != 5 {
		t.Fatalf("meta body = %s", req.Body)
	}
	if custom, err := adapter.BuildPing("muse-other", "hi"); err != nil || custom.Model != "muse-other" {
		t.Fatalf("custom model: %+v %v", custom, err)
	}
	if !CanPing(adapter) {
		t.Fatal("meta must be pingable")
	}
}

func TestMetaCheckReply(t *testing.T) {
	var checker ReplyChecker = meta{}
	cases := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{"incomplete at the token cap", `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":9,"output_tokens":16}}`, "incomplete (max_output_tokens), 9+16 tokens", false},
		{"incomplete without usage", `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}`, "incomplete (max_output_tokens)", false},
		{"completed", `{"status":"completed","usage":{"input_tokens":9,"output_tokens":3}}`, "completed, 9+3 tokens", false},
		{"incomplete for another reason", `{"status":"incomplete","incomplete_details":{"reason":"content_filter"}}`, "", true},
		{"incomplete without reason", `{"status":"incomplete"}`, "", true},
		{"failed", `{"status":"failed"}`, "", true},
		{"no status", `{}`, "", true},
		{"not json", `event: response.completed`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := checker.CheckReply([]byte(tc.body))
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("CheckReply = %q, %v; want %q, error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
	for _, adapter := range []Adapter{claude{}, codex{}, xai{}, antigravity{}} {
		if _, ok := adapter.(ReplyChecker); ok {
			t.Fatalf("%s must not check replies", adapter.ID())
		}
	}
}
