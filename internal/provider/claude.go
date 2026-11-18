package provider

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// claude reads Anthropic's unified rate-limit headers. Only the all-models
// seven-day limit counts. Model-scoped limits, such as 7d_oi, are ignored
// because pings use another model. Five-hour headers are ignored.
type claude struct{}

const claudePrefix = "Anthropic-Ratelimit-Unified-"

func (claude) ID() string           { return "claude" }
func (claude) Name() string         { return "Claude" }
func (claude) DefaultModel() string { return "claude-haiku-4-5-20251001" }
func (claude) StartsOnFirstUse() bool {
	return true
}
func (claude) PingsOnInterval() bool          { return false }
func (claude) DefaultEnabled() bool           { return true }
func (claude) FixedReset(time.Time) time.Time { return time.Time{} }
func (claude) Note() string {
	return "Weekly window starts on first use. Reset from Anthropic-Ratelimit-Unified-7d-Reset."
}

func (claude) Match(account pluginapi.HostAuthFileEntry) bool {
	return isProvider(account, "claude") && isOAuth(account)
}

type claudeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type claudeRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	Messages  []claudeMessage `json:"messages"`
}

// BuildPing sends one user message with max_tokens 1 and no tools, so the
// model can only answer with a token of text.
func (claude) BuildPing(model, prompt string) (Request, error) {
	body, err := json.Marshal(claudeRequest{
		Model:     model,
		MaxTokens: 1,
		Messages:  []claudeMessage{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return Request{}, err
	}
	return Request{EntryProtocol: "claude", ExitProtocol: "claude", ForcedProvider: "claude", Model: model, Body: body}, nil
}

func (claude) ParseWeekly(headers http.Header, _ time.Time) Weekly {
	weekly := Weekly{Window: Week}
	if raw := header(headers, claudePrefix+"7d-Reset"); raw != "" {
		weekly.Seen = true
		weekly.Reset = parseReset(raw)
	}
	if raw := header(headers, claudePrefix+"7d-Status"); raw != "" {
		weekly.Seen = true
		weekly.Status = raw
		weekly.Reached = strings.EqualFold(raw, "rejected")
	}
	if raw := header(headers, claudePrefix+"7d-Utilization"); raw != "" {
		weekly.Seen = true
		if value, ok := parseNumber(raw); ok {
			weekly.Utilization = &value
			if value >= 1 {
				weekly.Reached = true
			}
		}
	}
	// Fallback: the unified reset applies to the weekly limit when that limit
	// is the representative claim.
	if weekly.Reset.IsZero() && strings.EqualFold(header(headers, claudePrefix+"Representative-Claim"), "seven_day") {
		if reset := parseReset(header(headers, claudePrefix+"Reset")); !reset.IsZero() {
			weekly.Seen = true
			weekly.Reset = reset
		}
	}
	return weekly
}
