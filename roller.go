package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bogorad/claude-keep-rolling/internal/provider"
	"github.com/bogorad/claude-keep-rolling/internal/rolling"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// startupDelay gives the host time to finish loading auths and executors.
	startupDelay = 20 * time.Second
	tickInterval = 30 * time.Second
	// shutdownWait bounds how long shutdown waits for in-flight pings.
	shutdownWait = time.Minute
)

var keeper = newRoller()

type accountState struct {
	Provider   string
	LastPingAt time.Time
	LastOK     bool
	LastStatus int
	LastError  string
	// LastResult is the adapter's reading of the last successful reply (meta).
	LastResult string
	// Weekly comes from the last successful ping.
	Weekly provider.Weekly
	// LastResetSeen is the most recent weekly reset the plugin saw pass.
	LastResetSeen time.Time
	// CooldownResetFor is the reset for which the plugin cleared a cooldown.
	CooldownResetFor time.Time
	CooldownResetAt  time.Time
	NextPingAt       time.Time
	Pinging          bool
}

type roller struct {
	mu          sync.Mutex
	settings    rolling.Settings
	configError string
	states      map[string]*accountState
	stopCh      chan struct{}
	// lastSlot is the last first-ping slot handed out after a start.
	lastSlot time.Time
	// random returns a number in [0, 1). Tests replace it.
	random func() float64
	// background tracks the loop and its pings so shutdown can wait for host calls to finish.
	background sync.WaitGroup
	// configWarned records that an unreadable host config was already logged.
	configWarned bool
}

func newRoller() *roller {
	settings, _ := rolling.ParseSettings(nil)
	return &roller{settings: settings, states: map[string]*accountState{}, random: rand.Float64}
}

// configure applies new settings. Invalid settings keep the previous ones and are reported on the status page.
func (r *roller) configure(settings rolling.Settings, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.configError = err.Error()
		return
	}
	r.settings = settings
	r.configError = ""
}

func (r *roller) currentSettings() (rolling.Settings, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settings, r.configError
}

func (r *roller) start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopCh != nil {
		return
	}
	stop := make(chan struct{})
	r.stopCh = stop
	r.lastSlot = time.Time{}
	r.background.Add(1)
	go func() {
		defer r.background.Done()
		r.run(stop)
	}()
}

func (r *roller) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopCh != nil {
		close(r.stopCh)
		r.stopCh = nil
	}
}

// shutdown stops the loop and waits, up to shutdownWait, for background host calls to return.
func (r *roller) shutdown() {
	r.stop()
	done := make(chan struct{})
	go func() {
		r.background.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownWait):
	}
}

func (r *roller) run(stop <-chan struct{}) {
	timer := time.NewTimer(startupDelay)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-timer.C:
		}
		r.tick(stop, time.Now())
		timer.Reset(tickInterval)
	}
}

// tracked is a host account with its adapter.
type tracked struct {
	entry   pluginapi.HostAuthFileEntry
	adapter provider.Adapter
}

// tick clears cooldowns that a weekly reset made stale, then starts a ping
// for every due account. Pings run concurrently so one slow account does not
// hold up the others.
func (r *roller) tick(stop <-chan struct{}, now time.Time) {
	if !r.hostEnablesPlugin() {
		return
	}
	accounts, err := listAccounts()
	if err != nil {
		hostLog("warn", "listing accounts failed", map[string]any{"error": err.Error()})
		return
	}
	settings, _ := r.currentSettings()
	for _, account := range r.cooldownsToReset(accounts, settings, now) {
		r.resetCooldown(account, now)
	}
	for _, account := range r.claimDue(accounts, settings, now) {
		select {
		case <-stop:
			r.release(account.entry.ID)
			continue
		default:
		}
		r.background.Add(1)
		go func() {
			defer r.background.Done()
			r.ping(account, settings, false)
		}()
	}
}

// hostEnablesPlugin reads the host config file. If the file cannot be read, as with
// database-backed configs, it assumes the plugin is enabled and logs that once.
func (r *roller) hostEnablesPlugin() bool {
	workDir, _ := os.Getwd()
	path := rolling.HostConfigPath(os.Args, workDir)
	raw, err := os.ReadFile(path)
	var enabled bool
	if err == nil {
		enabled, err = rolling.PluginEnabled(raw, pluginID)
	}
	if err != nil {
		r.mu.Lock()
		warned := r.configWarned
		r.configWarned = true
		r.mu.Unlock()
		if !warned {
			hostLog("warn", "cannot read the host config to check whether the plugin is disabled; pings continue until the host stops the plugin", map[string]any{"path": path, "error": err.Error()})
		}
		return true
	}
	return enabled
}

// active reports whether the plugin acts on the account at all.
func active(account tracked, settings rolling.Settings) bool {
	p := settings.Provider(account.adapter.ID())
	return p.Enabled && !account.entry.Disabled && p.Selected(account.entry.ID)
}

// knownReset returns the latest weekly reset for the account at or before
// now: the calendar reset for fixed providers, else the reset from the last
// successful ping once it has passed. The caller holds r.mu.
func knownReset(account tracked, state *accountState, now time.Time) time.Time {
	if fixed := account.adapter.FixedReset(now); !fixed.IsZero() {
		return fixed
	}
	if reset := state.Weekly.Reset; !reset.IsZero() && !now.Before(reset) {
		return reset
	}
	return state.LastResetSeen
}

// cooldownsToReset records passed resets and returns the accounts whose
// cooldown should be cleared now. It marks them done before the host call,
// so a reset is attempted at most once.
func (r *roller) cooldownsToReset(accounts []tracked, settings rolling.Settings, now time.Time) []tracked {
	r.mu.Lock()
	defer r.mu.Unlock()
	var due []tracked
	for _, account := range accounts {
		if !active(account, settings) {
			continue
		}
		state := r.stateLocked(account)
		reset := knownReset(account, state, now)
		if !reset.IsZero() && reset.After(state.LastResetSeen) {
			state.LastResetSeen = reset
		}
		// The plugin never clears the cooldown of an interval provider (meta).
		if account.adapter.PingsOnInterval() {
			continue
		}
		decide, clear := rolling.CooldownDecision(rolling.CooldownCheck{
			Now:            now,
			Enabled:        settings.CooldownReset,
			Reset:          reset,
			DoneFor:        state.CooldownResetFor,
			Unavailable:    account.entry.Unavailable,
			NextRetryAfter: account.entry.NextRetryAfter,
		})
		if !decide {
			continue
		}
		state.CooldownResetFor = reset
		if clear {
			due = append(due, account)
		}
	}
	return due
}

func (r *roller) resetCooldown(account tracked, now time.Time) {
	fields := map[string]any{"account": accountName(account.entry), "provider": account.adapter.ID()}
	if strings.TrimSpace(account.entry.AuthIndex) == "" {
		fields["error"] = "account has no auth_index"
		hostLog("warn", "cooldown reset skipped", fields)
		return
	}
	_, err := callHost(pluginabi.MethodHostRoutingResetCooldown, pluginapi.HostRoutingResetCooldownRequest{AuthIndex: account.entry.AuthIndex})
	if err != nil {
		fields["error"] = err.Error()
		hostLog("warn", "cooldown reset after weekly reset failed", fields)
		return
	}
	r.mu.Lock()
	r.stateLocked(account).CooldownResetAt = now
	r.mu.Unlock()
	hostLog("info", "cooldown reset after weekly reset", fields)
}

// claimDue drops state for accounts that no longer exist, gives new accounts a
// staggered first ping, and marks due accounts as pinging.
func (r *roller) claimDue(accounts []tracked, settings rolling.Settings, now time.Time) []tracked {
	r.mu.Lock()
	defer r.mu.Unlock()
	present := make(map[string]bool, len(accounts))
	var due []tracked
	for _, account := range accounts {
		present[account.entry.ID] = true
		if !active(account, settings) || !provider.CanPing(account.adapter) {
			continue
		}
		state := r.stateLocked(account)
		if state.Pinging {
			continue
		}
		switch {
		case !state.NextPingAt.IsZero():
		case account.adapter.PingsOnInterval():
			// An interval provider pings at once after a start, without stagger.
			state.NextPingAt = now
		default:
			slot := rolling.NextSlot(now, r.lastSlot, time.Duration(settings.Stagger), r.random())
			r.lastSlot = slot
			state.NextPingAt = slot
		}
		if now.Before(state.NextPingAt) {
			continue
		}
		if next, wait := rolling.DeferForCooldown(now, state.NextPingAt, account.entry.NextRetryAfter); wait {
			state.NextPingAt = next
			continue
		}
		state.Pinging = true
		due = append(due, account)
	}
	for id := range r.states {
		if !present[id] {
			delete(r.states, id)
		}
	}
	return due
}

// claim marks one account as pinging for a manual ping. It fails if a ping is already running.
func (r *roller) claim(account tracked) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.stateLocked(account)
	if state.Pinging {
		return false
	}
	state.Pinging = true
	return true
}

func (r *roller) release(accountID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if state, ok := r.states[accountID]; ok {
		state.Pinging = false
	}
}

func (r *roller) snapshot(accountID string) (accountState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state, ok := r.states[accountID]
	if !ok {
		return accountState{}, false
	}
	return *state, true
}

func (r *roller) stateLocked(account tracked) *accountState {
	state, ok := r.states[account.entry.ID]
	if !ok {
		state = &accountState{Provider: account.adapter.ID()}
		r.states[account.entry.ID] = state
	}
	return state
}

// ping sends one minimal request through the exact account and records when
// to ping it next. The caller must have claimed the account.
func (r *roller) ping(account tracked, settings rolling.Settings, manual bool) {
	at := time.Now()
	p := settings.Provider(account.adapter.ID())
	weekly, result, status, err := sendPing(account, p.Model, settings.Prompt, at)
	delay := rolling.Jitter(time.Duration(settings.DelayMin), time.Duration(settings.DelayMax), r.randomLocked())
	outcome := rolling.Outcome{At: at, OK: err == nil, Weekly: weekly, Delay: delay, Manual: manual, Interval: pingInterval(account.adapter, settings)}
	if err != nil {
		outcome.CooldownUntil = cooldownUntil(account.entry.ID)
	}

	r.mu.Lock()
	state := r.stateLocked(account)
	outcome.Scheduled = state.NextPingAt
	next := rolling.NextPing(outcome)
	state.Pinging = false
	state.LastPingAt = at
	state.LastOK = err == nil
	state.LastStatus = status
	state.LastError = ""
	state.LastResult = result
	if err != nil {
		state.LastError = err.Error()
	} else {
		if old := state.Weekly.Reset; !old.IsZero() && !at.Before(old) && old.After(state.LastResetSeen) {
			state.LastResetSeen = old
		}
		state.Weekly = weekly
	}
	state.NextPingAt = next
	r.mu.Unlock()

	fields := map[string]any{"account": accountName(account.entry), "provider": account.adapter.ID(), "next_ping": next.Format(time.RFC3339)}
	if err != nil {
		fields["error"] = err.Error()
		hostLog("warn", "keep-rolling ping failed", fields)
		return
	}
	if !weekly.Reset.IsZero() {
		fields["weekly_reset"] = weekly.Reset.Format(time.RFC3339)
	}
	if result != "" {
		fields["result"] = result
	}
	hostLog("info", "keep-rolling ping sent", fields)
}

// pingInterval returns the fixed ping interval for an interval provider (meta),
// or zero for providers pinged at the weekly reset.
func pingInterval(adapter provider.Adapter, settings rolling.Settings) time.Duration {
	if !adapter.PingsOnInterval() {
		return 0
	}
	return time.Duration(settings.MetaInterval)
}

// randomLocked draws a random number under the lock, because tests swap r.random.
func (r *roller) randomLocked() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.random()
}

// cooldownUntil returns the retry time the host recorded for the account after a failure.
func cooldownUntil(accountID string) time.Time {
	accounts, err := listAccounts()
	if err != nil {
		return time.Time{}
	}
	for _, account := range accounts {
		if account.entry.ID == accountID {
			return account.entry.NextRetryAfter
		}
	}
	return time.Time{}
}

// sendPing returns the weekly window and the adapter's result from a
// successful reply, and the HTTP status. The host returns a failed reply as an
// error without its headers. No host callback ID is passed, so a manual ping
// is not cancelled when its browser request goes away.
func sendPing(account tracked, model, prompt string, at time.Time) (provider.Weekly, string, int, error) {
	req, err := account.adapter.BuildPing(model, prompt)
	if err != nil {
		return provider.Weekly{}, "", 0, err
	}
	raw, err := callHost(pluginabi.MethodHostModelExecute, pluginapi.HostModelExecutionRequest{
		EntryProtocol:  req.EntryProtocol,
		ExitProtocol:   req.ExitProtocol,
		Model:          req.Model,
		Stream:         false,
		Body:           req.Body,
		ForcedProvider: req.ForcedProvider,
		AuthID:         account.entry.ID,
	})
	if err != nil {
		var hostErr *hostError
		if errors.As(err, &hostErr) {
			return provider.Weekly{}, "", hostErr.Status, err
		}
		return provider.Weekly{}, "", 0, err
	}
	var resp pluginapi.HostModelExecutionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return provider.Weekly{}, "", 0, fmt.Errorf("decode model response: %w", err)
	}
	return readReply(account.adapter, resp, at)
}

// readReply turns a host.model.execute response into the ping result. A
// status of 400 or more is a failure. An adapter that implements
// provider.ReplyChecker also judges the body of a successful reply.
func readReply(adapter provider.Adapter, resp pluginapi.HostModelExecutionResponse, at time.Time) (provider.Weekly, string, int, error) {
	if resp.StatusCode >= 400 {
		return provider.Weekly{}, "", resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(resp.Body), 300))
	}
	var result string
	if checker, ok := adapter.(provider.ReplyChecker); ok {
		var err error
		result, err = checker.CheckReply(resp.Body)
		if err != nil {
			return provider.Weekly{}, "", resp.StatusCode, err
		}
	}
	return adapter.ParseWeekly(resp.Headers, at), result, resp.StatusCode, nil
}

type authListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

// listAccounts returns host accounts that some adapter owns, sorted by
// provider and display name.
func listAccounts() ([]tracked, error) {
	raw, err := callHost(pluginabi.MethodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var list authListResponse
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode auth list: %w", err)
	}
	return selectAccounts(list.Files), nil
}

func selectAccounts(files []pluginapi.HostAuthFileEntry) []tracked {
	order := map[string]int{}
	for i, adapter := range provider.All() {
		order[adapter.ID()] = i
	}
	accounts := make([]tracked, 0, len(files))
	for _, file := range files {
		if strings.TrimSpace(file.ID) == "" {
			continue
		}
		adapter, ok := provider.ForAccount(file)
		if !ok {
			continue
		}
		accounts = append(accounts, tracked{entry: file, adapter: adapter})
	}
	sort.SliceStable(accounts, func(i, j int) bool {
		a, b := accounts[i], accounts[j]
		if a.adapter.ID() != b.adapter.ID() {
			return order[a.adapter.ID()] < order[b.adapter.ID()]
		}
		return strings.ToLower(accountName(a.entry)) < strings.ToLower(accountName(b.entry))
	})
	return accounts
}

func accountName(account pluginapi.HostAuthFileEntry) string {
	for _, name := range []string{account.Email, account.Label, account.Name, account.ID} {
		if strings.TrimSpace(name) != "" {
			return name
		}
	}
	return ""
}

func hostLog(level, message string, fields map[string]any) {
	fields["plugin"] = pluginID
	_, _ = callHost(pluginabi.MethodHostLog, map[string]any{"level": level, "message": message, "fields": fields})
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit] + "..."
}
