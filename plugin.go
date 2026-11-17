package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/bogorad/claude-keep-rolling/internal/provider"
	"github.com/bogorad/claude-keep-rolling/internal/rolling"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// pluginVersion is set at build time with -ldflags "-X main.pluginVersion=<version>".
var pluginVersion = "0.0.0-dev"

const (
	pluginID   = "keep-rolling"
	repository = "https://github.com/bogorad/claude-keep-rolling"

	statePath  = "/v0/management/" + pluginID + "/state"
	pingPath   = "/v0/management/" + pluginID + "/ping"
	statusPath = "/status"
)

//go:embed status.html
var statusPage []byte

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

// registration declares only the management capability. keep-rolling is not
// a scheduler and does not change account selection (ADR-028 §4).
type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  struct {
		ManagementAPI bool `json:"management_api"`
	} `json:"capabilities"`
}

type managementRegistration struct {
	Routes    []managementRoute    `json:"routes"`
	Resources []managementResource `json:"resources"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description"`
}

type managementResource struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}

type managementRequest struct {
	Method string `json:"Method"`
	Path   string `json:"Path"`
	Body   []byte `json:"Body"`
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req lifecycleRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, fmt.Errorf("decode lifecycle request: %w", err)
			}
		}
		keeper.configure(rolling.ParseSettings(req.ConfigYAML))
		keeper.start()
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginQuiesce:
		keeper.stop()
		return okEnvelope(map[string]any{})
	case pluginabi.MethodPluginShutdown:
		keeper.shutdown()
		return okEnvelope(map[string]any{})
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration{
			Routes: []managementRoute{
				{Method: http.MethodGet, Path: statePath, Description: "Accounts, providers, settings and ping schedule."},
				{Method: http.MethodPost, Path: pingPath, Description: "Ping one account now."},
			},
			Resources: []managementResource{
				{Path: statusPath, Menu: "Keep Rolling", Description: "Starts each account's weekly usage window right after its reset."},
			},
		})
	case pluginabi.MethodManagementHandle:
		var req managementRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode management request: %w", err)
		}
		return okEnvelope(handleManagement(req))
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func pluginRegistration() registration {
	reg := registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Keep Rolling",
			Version:          pluginVersion,
			Author:           "bogorad (fork of 265866/claude-keep-rolling)",
			GitHubRepository: repository,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "prompt", Type: pluginapi.ConfigFieldTypeString, Default: rolling.DefaultPrompt, Description: "Message sent with each ping."},
				{Name: "delay_min", Type: pluginapi.ConfigFieldTypeString, Default: rolling.DefaultDelayMin.String(), Description: "Smallest random delay after a weekly reset (Go duration, at least 1m)."},
				{Name: "delay_max", Type: pluginapi.ConfigFieldTypeString, Default: rolling.DefaultDelayMax.String(), Description: "Largest random delay after a weekly reset (Go duration, at most 6h)."},
				{Name: "stagger", Type: pluginapi.ConfigFieldTypeString, Default: rolling.DefaultStagger.String(), Description: "Minimum gap between first pings after a start (Go duration, 0s to 1h)."},
				{Name: "cooldown_reset", Type: pluginapi.ConfigFieldTypeBoolean, Default: true, Description: "Clear an active host cooldown once, right after an account's weekly reset."},
				{Name: "providers", Type: pluginapi.ConfigFieldTypeObject, Description: "Per provider (claude, codex, meta, xai, antigravity): enabled, model, select_all, accounts."},
			},
		},
	}
	reg.Capabilities.ManagementAPI = true
	return reg
}

func handleManagement(req managementRequest) managementResponse {
	switch {
	case req.Method == http.MethodGet && req.Path == "/v0/resource/plugins/"+pluginID+statusPath:
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"}},
			Body:       statusPage,
		}
	case req.Method == http.MethodGet && req.Path == statePath:
		return jsonResponse(http.StatusOK, buildState(time.Now()))
	case req.Method == http.MethodPost && req.Path == pingPath:
		return handlePing(req)
	default:
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

type accountView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Disabled bool   `json:"disabled"`
	Selected bool   `json:"selected"`
	// Rolling reports that the plugin pings this account.
	Rolling    bool       `json:"rolling"`
	Pinging    bool       `json:"pinging"`
	LastPingAt *time.Time `json:"last_ping_at,omitempty"`
	LastOK     bool       `json:"last_ok"`
	LastStatus int        `json:"last_status,omitempty"`
	LastError  string     `json:"last_error,omitempty"`
	// The weekly fields come from the last successful ping.
	WeeklyResetAt     *time.Time `json:"weekly_reset_at,omitempty"`
	WeeklyUtilization *float64   `json:"weekly_utilization,omitempty"`
	WeeklyReached     bool       `json:"weekly_reached,omitempty"`
	LastResetSeen     *time.Time `json:"last_reset_seen,omitempty"`
	NextPingAt        *time.Time `json:"next_ping_at,omitempty"`
	// Host runtime state.
	Unavailable     bool       `json:"unavailable,omitempty"`
	NextRetryAfter  *time.Time `json:"next_retry_after,omitempty"`
	CooldownResetAt *time.Time `json:"cooldown_reset_at,omitempty"`
}

type providerView struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Enabled          bool       `json:"enabled"`
	DefaultEnabled   bool       `json:"default_enabled"`
	StartsOnFirstUse bool       `json:"starts_on_first_use"`
	CanPing          bool       `json:"can_ping"`
	DefaultModel     string     `json:"default_model"`
	FixedResetAt     *time.Time `json:"fixed_reset_at,omitempty"`
	Note             string     `json:"note"`
}

type stateView struct {
	Version     string           `json:"version"`
	Now         time.Time        `json:"now"`
	Settings    rolling.Settings `json:"settings"`
	Defaults    rolling.Settings `json:"defaults"`
	ConfigError string           `json:"config_error,omitempty"`
	Providers   []providerView   `json:"providers"`
	Accounts    []accountView    `json:"accounts"`
	ListError   string           `json:"list_error,omitempty"`
}

func buildState(now time.Time) stateView {
	settings, configError := keeper.currentSettings()
	defaults, _ := rolling.ParseSettings(nil)
	view := stateView{
		Version: pluginVersion, Now: now.UTC(), Settings: settings, Defaults: defaults,
		ConfigError: configError, Providers: providerViews(settings, now), Accounts: []accountView{},
	}
	accounts, err := listAccounts()
	if err != nil {
		view.ListError = err.Error()
		return view
	}
	for _, account := range accounts {
		view.Accounts = append(view.Accounts, buildAccountView(account, settings))
	}
	return view
}

func providerViews(settings rolling.Settings, now time.Time) []providerView {
	var views []providerView
	for _, adapter := range provider.All() {
		item := providerView{
			ID:               adapter.ID(),
			Name:             adapter.Name(),
			Enabled:          settings.Provider(adapter.ID()).Enabled,
			DefaultEnabled:   adapter.DefaultEnabled(),
			StartsOnFirstUse: adapter.StartsOnFirstUse(),
			CanPing:          provider.CanPing(adapter),
			DefaultModel:     adapter.DefaultModel(),
			Note:             adapter.Note(),
		}
		if fixed := adapter.FixedReset(now); !fixed.IsZero() {
			item.FixedResetAt = timeOrNil(fixed.Add(provider.Week))
		}
		views = append(views, item)
	}
	return views
}

func buildAccountView(account tracked, settings rolling.Settings) accountView {
	p := settings.Provider(account.adapter.ID())
	item := accountView{
		ID:             account.entry.ID,
		Name:           accountName(account.entry),
		Provider:       account.adapter.ID(),
		Disabled:       account.entry.Disabled,
		Selected:       p.Selected(account.entry.ID),
		Rolling:        active(account, settings) && provider.CanPing(account.adapter),
		Unavailable:    account.entry.Unavailable,
		NextRetryAfter: timeOrNil(account.entry.NextRetryAfter),
	}
	if state, ok := keeper.snapshot(account.entry.ID); ok {
		item.Pinging = state.Pinging
		item.LastPingAt = timeOrNil(state.LastPingAt)
		item.LastOK = state.LastOK
		item.LastStatus = state.LastStatus
		item.LastError = state.LastError
		item.WeeklyResetAt = timeOrNil(state.Weekly.Reset)
		item.WeeklyUtilization = state.Weekly.Utilization
		item.WeeklyReached = state.Weekly.Reached
		item.LastResetSeen = timeOrNil(state.LastResetSeen)
		item.NextPingAt = timeOrNil(state.NextPingAt)
		item.CooldownResetAt = timeOrNil(state.CooldownResetAt)
	}
	return item
}

func handlePing(req managementRequest) managementResponse {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil || body.ID == "" {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "body must be {\"id\": \"<account id>\"}"})
	}
	accounts, err := listAccounts()
	if err != nil {
		return jsonResponse(http.StatusBadGateway, map[string]string{"error": err.Error()})
	}
	for _, account := range accounts {
		if account.entry.ID != body.ID {
			continue
		}
		if account.entry.Disabled {
			return jsonResponse(http.StatusConflict, map[string]string{"error": "account is disabled"})
		}
		if !provider.CanPing(account.adapter) {
			return jsonResponse(http.StatusConflict, map[string]string{"error": account.adapter.Name() + " has no ping: " + account.adapter.Note()})
		}
		if !keeper.claim(account) {
			return jsonResponse(http.StatusConflict, map[string]string{"error": "a ping is already running for this account"})
		}
		settings, _ := keeper.currentSettings()
		keeper.ping(account, settings, true)
		return jsonResponse(http.StatusOK, buildState(time.Now()))
	}
	return jsonResponse(http.StatusNotFound, map[string]string{"error": "account not found"})
}

func jsonResponse(status int, v any) managementResponse {
	body, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte(`{"error":"encode response"}`)
	}
	return managementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}},
		Body:       body,
	}
}

func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}
