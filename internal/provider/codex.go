package provider

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// codex reads the ChatGPT Codex usage headers. The backend reports up to two
// windows, X-Codex-Primary-* and X-Codex-Secondary-*. Which one is weekly
// depends on the plan (Pro: primary is 10080 minutes, secondary is 0; Plus:
// primary is 300 minutes, secondary is 10080), so the window is chosen by its
// length.
type codex struct{}

const (
	codexWeekMinutes = 7 * 24 * 60
	// codexMinWindowMinutes keeps five-hour windows out: a window shorter than
	// one day is never treated as the weekly window.
	codexMinWindowMinutes = 24 * 60
	// codexMaxOutputTokens is the smallest max_output_tokens the Codex backend
	// accepts (CLIProxyAPI raises smaller values to it).
	codexMaxOutputTokens = 16
)

func (codex) ID() string             { return "codex" }
func (codex) Name() string           { return "Codex" }
func (codex) DefaultModel() string   { return "gpt-6-luna" }
func (codex) StartsOnFirstUse() bool { return true }
func (codex) PingsOnInterval() bool  { return false }
func (codex) DefaultEnabled() bool   { return true }
func (codex) FixedReset(time.Time) time.Time {
	return time.Time{}
}

func (codex) Note() string {
	return "Weekly window starts on first use. Reset from X-Codex-Primary/Secondary-Reset-At, window of 10080 minutes."
}

func (codex) Match(account pluginapi.HostAuthFileEntry) bool {
	return isProvider(account, "codex") && isOAuth(account)
}

type codexRequest struct {
	Model           string `json:"model"`
	Instructions    string `json:"instructions"`
	Input           string `json:"input"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	Store           bool   `json:"store"`
}

// BuildPing sends an OpenAI Responses request. The host translates it to the
// Codex backend shape (stream=true upstream, store=false, input as a message)
// and aggregates the reply, so stream=false is valid for host.model.execute.
func (codex) BuildPing(model, prompt string) (Request, error) {
	body, err := json.Marshal(codexRequest{
		Model:           model,
		Instructions:    "",
		Input:           prompt,
		MaxOutputTokens: codexMaxOutputTokens,
		Store:           false,
	})
	if err != nil {
		return Request{}, err
	}
	return Request{EntryProtocol: "openai-response", ExitProtocol: "openai-response", ForcedProvider: "codex", Model: model, Body: body}, nil
}

type codexWindow struct {
	minutes    int
	reset      time.Time
	used       float64
	hasUsed    bool
	hasAnyData bool
}

func readCodexWindow(headers http.Header, prefix string, at time.Time) codexWindow {
	var window codexWindow
	if raw := header(headers, prefix+"-Window-Minutes"); raw != "" {
		window.hasAnyData = true
		if minutes, err := strconv.Atoi(raw); err == nil && minutes > 0 {
			window.minutes = minutes
		}
	}
	if raw := header(headers, prefix+"-Reset-At"); raw != "" {
		window.hasAnyData = true
		window.reset = parseReset(raw)
	}
	if window.reset.IsZero() {
		if raw := header(headers, prefix+"-Reset-After-Seconds"); raw != "" {
			window.hasAnyData = true
			if seconds, ok := parseNumber(raw); ok && seconds > 0 {
				window.reset = at.Add(time.Duration(seconds * float64(time.Second))).UTC().Truncate(time.Second)
			}
		}
	}
	if raw := header(headers, prefix+"-Used-Percent"); raw != "" {
		window.hasAnyData = true
		if used, ok := parseNumber(raw); ok {
			window.used = used
			window.hasUsed = true
		}
	}
	return window
}

func (codex) ParseWeekly(headers http.Header, at time.Time) Weekly {
	candidates := []codexWindow{
		readCodexWindow(headers, "X-Codex-Primary", at),
		readCodexWindow(headers, "X-Codex-Secondary", at),
	}
	var chosen *codexWindow
	for i := range candidates {
		if candidates[i].minutes == codexWeekMinutes {
			chosen = &candidates[i]
			break
		}
	}
	if chosen == nil {
		for i := range candidates {
			c := &candidates[i]
			if c.minutes < codexMinWindowMinutes {
				continue
			}
			if chosen == nil || c.minutes > chosen.minutes {
				chosen = c
			}
		}
	}
	if chosen == nil {
		return Weekly{Window: Week}
	}
	weekly := Weekly{Seen: true, Reset: chosen.reset, Window: time.Duration(chosen.minutes) * time.Minute}
	if chosen.hasUsed {
		share := chosen.used / 100
		weekly.Utilization = &share
		weekly.Reached = share >= 1
	}
	return weekly
}
