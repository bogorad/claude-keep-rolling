package provider

import (
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// meta (Muse) has a calendar-fixed weekly reset at Monday 00:00 UTC (probes
// 2026-11-16 and 2026-11-17: pings did not move it). A ping cannot start a
// weekly window, so the plugin never pings meta. When enabled, the adapter
// only lets the plugin clear an active cooldown right after the Monday reset.
// The weekly quota is in the SSE body (response.subscription_usage), not in
// headers, so ParseWeekly reads nothing.
type meta struct{}

func (meta) ID() string             { return "meta" }
func (meta) Name() string           { return "Meta" }
func (meta) DefaultModel() string   { return "muse-spark-1.3" }
func (meta) StartsOnFirstUse() bool { return false }
func (meta) DefaultEnabled() bool   { return false }
func (meta) Match(account pluginapi.HostAuthFileEntry) bool {
	return isProvider(account, "meta") && isOAuth(account)
}

func (meta) BuildPing(string, string) (Request, error) { return Request{}, ErrNoPing }
func (meta) ParseWeekly(http.Header, time.Time) Weekly { return Weekly{Window: Week} }
func (meta) Note() string {
	return "Weekly reset is calendar-fixed (Monday 00:00 UTC). No pings; cooldown reset at the Monday reset only. Off by default."
}

// FixedReset returns the most recent Monday 00:00 UTC at or before now.
func (meta) FixedReset(now time.Time) time.Time {
	now = now.UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	daysSinceMonday := (int(midnight.Weekday()) + 6) % 7
	return midnight.AddDate(0, 0, -daysSinceMonday)
}

// xai (Grok) has a weekly billing period (probe 2026-11-17: Friday
// 23:14:15.844Z), but it is not known whether the period starts on first use
// or is fixed per account. The reset is only in GET /v1/billing, which a
// plugin can reach only by direct egress; reply headers carry
// X-Ratelimit-* without a reset. Stub: disabled, no ping, until verified
// (see Bead nix-config-cloud.88.2, Unknowns).
type xai struct{}

func (xai) ID() string             { return "xai" }
func (xai) Name() string           { return "xAI" }
func (xai) DefaultModel() string   { return "grok-4.20-0309-non-reasoning" }
func (xai) StartsOnFirstUse() bool { return false }
func (xai) DefaultEnabled() bool   { return false }
func (xai) Match(account pluginapi.HostAuthFileEntry) bool {
	return isProvider(account, "xai") && isOAuth(account)
}
func (xai) BuildPing(string, string) (Request, error) { return Request{}, ErrNoPing }
func (xai) ParseWeekly(http.Header, time.Time) Weekly { return Weekly{Window: Week} }
func (xai) FixedReset(time.Time) time.Time            { return time.Time{} }
func (xai) Note() string {
	return "Stub. Weekly period start rule unverified and no reset in reply headers. No pings."
}

// antigravity exposes only a floating five-hour window through its API
// (probe 2026-11-17). No weekly reset source is known. Stub: disabled, no ping.
type antigravity struct{}

func (antigravity) ID() string             { return "antigravity" }
func (antigravity) Name() string           { return "Antigravity" }
func (antigravity) DefaultModel() string   { return "gemini-3.1-flash-lite" }
func (antigravity) StartsOnFirstUse() bool { return false }
func (antigravity) DefaultEnabled() bool   { return false }
func (antigravity) Match(account pluginapi.HostAuthFileEntry) bool {
	return isProvider(account, "antigravity") && isOAuth(account)
}
func (antigravity) BuildPing(string, string) (Request, error) { return Request{}, ErrNoPing }
func (antigravity) ParseWeekly(http.Header, time.Time) Weekly { return Weekly{Window: Week} }
func (antigravity) FixedReset(time.Time) time.Time            { return time.Time{} }
func (antigravity) Note() string {
	return "Stub. No weekly reset source in the API. No pings."
}
