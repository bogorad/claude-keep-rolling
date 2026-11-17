// Package provider holds one adapter for each upstream provider whose weekly
// usage window keep-rolling can track. An adapter knows which host accounts
// belong to it, how to build a minimal ping for host.model.execute, and how to
// read the weekly reset from the reply headers. Adapters are host-independent
// and safe for concurrent use.
package provider

import (
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Week is the length of a weekly usage window.
const Week = 7 * 24 * time.Hour

// ErrNoPing is returned by adapters that cannot or must not send a ping.
var ErrNoPing = errors.New("this provider has no ping")

// Request is the host.model.execute request for one ping. The roller adds the
// account ID and stream=false.
type Request struct {
	EntryProtocol  string
	ExitProtocol   string
	ForcedProvider string
	Model          string
	Body           []byte
}

// Weekly is what one reply says about the account's weekly window.
type Weekly struct {
	// Seen reports that the reply carried any weekly signal.
	Seen bool
	// Reset is the end of the current weekly window. It is zero when unknown.
	Reset time.Time
	// Window is the window length. It is Week unless the provider reports another.
	Window time.Duration
	// Utilization is the used share of the weekly limit, 1 when used up.
	// It is nil when unknown.
	Utilization *float64
	// Reached reports that the weekly limit is used up or the provider rejects requests.
	Reached bool
	// Status is the provider's raw weekly status, when it reports one.
	Status string
}

// Adapter is one provider.
type Adapter interface {
	// ID is the provider key used in settings, for example "claude".
	ID() string
	// Name is the display name.
	Name() string
	// Match reports whether a host account belongs to this provider and has a
	// weekly usage window (OAuth subscription, not a metered API key).
	Match(account pluginapi.HostAuthFileEntry) bool
	// DefaultModel is the model used for pings when settings name none.
	DefaultModel() string
	// BuildPing returns the minimal request that starts a window. It returns
	// ErrNoPing when the provider has no verified ping.
	BuildPing(model, prompt string) (Request, error)
	// ParseWeekly reads the weekly window from reply headers. at is the time
	// the reply arrived, used for relative reset values.
	ParseWeekly(headers http.Header, at time.Time) Weekly
	// StartsOnFirstUse reports that the weekly window starts with the first
	// request after a reset, so a ping right after the reset helps. When
	// false, the plugin never pings the provider.
	StartsOnFirstUse() bool
	// DefaultEnabled is the provider's enabled flag when settings omit it.
	DefaultEnabled() bool
	// FixedReset returns the most recent calendar-fixed weekly reset at or
	// before now, or zero when the reset is not calendar-fixed.
	FixedReset(now time.Time) time.Time
	// Note explains the adapter's state for the status page.
	Note() string
}

var adapters = []Adapter{claude{}, codex{}, meta{}, xai{}, antigravity{}}

// All returns every adapter in display order.
func All() []Adapter {
	return slices.Clone(adapters)
}

// Get returns the adapter for a provider key.
func Get(id string) (Adapter, bool) {
	for _, adapter := range adapters {
		if adapter.ID() == id {
			return adapter, true
		}
	}
	return nil, false
}

// ForAccount returns the adapter that owns the account.
func ForAccount(account pluginapi.HostAuthFileEntry) (Adapter, bool) {
	for _, adapter := range adapters {
		if adapter.Match(account) {
			return adapter, true
		}
	}
	return nil, false
}

// CanPing reports whether the plugin may ping the provider at all.
func CanPing(adapter Adapter) bool {
	if !adapter.StartsOnFirstUse() {
		return false
	}
	_, err := adapter.BuildPing(adapter.DefaultModel(), "hi")
	return err == nil
}

// isProvider reports whether the account's provider or type is id.
func isProvider(account pluginapi.HostAuthFileEntry, id string) bool {
	return strings.EqualFold(strings.TrimSpace(account.Provider), id) || strings.EqualFold(strings.TrimSpace(account.Type), id)
}

// isOAuth excludes API-key credentials, which bill per request and have no usage window.
func isOAuth(account pluginapi.HostAuthFileEntry) bool {
	accountType := strings.TrimSpace(account.AccountType)
	return accountType == "" || strings.EqualFold(accountType, "oauth")
}

// header returns the first value of a header in any letter case, trimmed.
func header(headers http.Header, name string) string {
	if values := headers.Values(name); len(values) > 0 {
		return strings.TrimSpace(values[0])
	}
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}

// parseReset reads Unix seconds, or RFC 3339, and returns zero for anything else.
func parseReset(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if seconds <= 0 {
			return time.Time{}
		}
		return time.Unix(seconds, 0).UTC()
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed.UTC()
	}
	return time.Time{}
}

// parseNumber reads a finite, non-negative number.
func parseNumber(raw string) (float64, bool) {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, false
	}
	return value, true
}
