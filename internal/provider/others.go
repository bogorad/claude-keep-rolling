package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// meta (Muse) has a calendar-fixed weekly reset at Monday 00:00 UTC (probes
// 2026-11-16 and 2026-11-17: pings did not move it), so a ping cannot start a
// weekly window. A ping after a five-hour window ended starts a new window at
// ping time (probe 2026-11-17). So the plugin pings meta on a fixed interval
// (meta_interval, default 60m), first at plugin start. It never clears a meta
// cooldown. The quota is in the SSE body (response.subscription_usage), which
// the host strips, so ParseWeekly reads nothing.
type meta struct{}

const (
	// metaPingInput and metaMaxOutputTokens give the measured cheapest ping
	// (2026-11-18): 9 input + 16 output tokens, status incomplete.
	metaPingInput       = "ping"
	metaMaxOutputTokens = 16
	metaReasoningEffort = "minimal"
)

func (meta) ID() string             { return "meta" }
func (meta) Name() string           { return "Meta" }
func (meta) DefaultModel() string   { return "muse-spark-1.3-contributor" }
func (meta) StartsOnFirstUse() bool { return false }
func (meta) PingsOnInterval() bool  { return true }
func (meta) DefaultEnabled() bool   { return false }
func (meta) Match(account pluginapi.HostAuthFileEntry) bool {
	return isProvider(account, "meta") && isOAuth(account)
}

type metaReasoning struct {
	Effort string `json:"effort"`
}

type metaRequest struct {
	Model           string        `json:"model"`
	Input           string        `json:"input"`
	Reasoning       metaReasoning `json:"reasoning"`
	MaxOutputTokens int           `json:"max_output_tokens"`
	Store           bool          `json:"store"`
}

// BuildPing sends the measured minimal OpenAI Responses request. The input is
// always "ping"; the prompt setting does not apply, so the cost stays at the
// measured value. The host sends it upstream as a stream and aggregates the
// reply, so stream=false is valid for host.model.execute.
func (meta) BuildPing(model, _ string) (Request, error) {
	body, err := json.Marshal(metaRequest{
		Model:           model,
		Input:           metaPingInput,
		Reasoning:       metaReasoning{Effort: metaReasoningEffort},
		MaxOutputTokens: metaMaxOutputTokens,
		Store:           false,
	})
	if err != nil {
		return Request{}, err
	}
	return Request{EntryProtocol: "openai-response", ExitProtocol: "openai-response", ForcedProvider: "meta", Model: model, Body: body}, nil
}

func (meta) ParseWeekly(http.Header, time.Time) Weekly { return Weekly{Window: Week} }
func (meta) Note() string {
	return "Pinged every meta_interval (default 60m), first at plugin start, to keep a Muse window open. Weekly reset is calendar-fixed (Monday 00:00 UTC). The plugin never clears a Meta cooldown. Off by default."
}

type metaReply struct {
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// CheckReply reads the OpenAI Responses object that the host returns. Status
// completed is a success. Status incomplete is a success only for reason
// max_output_tokens, which the 16-token cap causes on purpose.
func (meta) CheckReply(body []byte) (string, error) {
	var reply metaReply
	if err := json.Unmarshal(body, &reply); err != nil {
		return "", fmt.Errorf("decode reply: %w", err)
	}
	var result string
	switch reply.Status {
	case "completed":
		result = "completed"
	case "incomplete":
		reason := ""
		if reply.IncompleteDetails != nil {
			reason = reply.IncompleteDetails.Reason
		}
		if reason != "max_output_tokens" {
			if reason == "" {
				reason = "no reason"
			}
			return "", fmt.Errorf("reply incomplete: %s", reason)
		}
		result = "incomplete (max_output_tokens)"
	case "":
		return "", fmt.Errorf("reply has no status")
	default:
		return "", fmt.Errorf("reply status %s", reply.Status)
	}
	if reply.Usage != nil {
		result += fmt.Sprintf(", %d+%d tokens", reply.Usage.InputTokens, reply.Usage.OutputTokens)
	}
	return result, nil
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
func (xai) PingsOnInterval() bool  { return false }
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
func (antigravity) PingsOnInterval() bool  { return false }
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
