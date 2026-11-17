package rolling

import (
	"time"

	"github.com/bogorad/claude-keep-rolling/internal/provider"
)

const (
	// ResetBuffer is added to a host cooldown before the next try.
	ResetBuffer = time.Minute
	// RecheckFallback is used when a successful reply carries no usable weekly reset.
	RecheckFallback = 24 * time.Hour
	// RetryAfterFailure is the wait before retrying a failed ping. Failed replies
	// reach plugins without headers, so the real reset time is unknown.
	RetryAfterFailure = 15 * time.Minute
	// SkewTolerance and SkewRetry handle a reset reported at or shortly before
	// the ping time: the local clock runs ahead and the ping landed in the old window.
	SkewTolerance = 10 * time.Minute
	SkewRetry     = 2 * time.Minute
	// MaxAheadSlack: a reset further ahead than its window plus this is not a
	// window reset. CLIProxyAPI caps weekly resets the same way.
	MaxAheadSlack = time.Hour
	// CooldownResetWindow bounds how long after a reset the plugin may still
	// clear a cooldown. A later cooldown is a new one and is left alone.
	CooldownResetWindow = time.Hour
)

// Jitter maps frac in [0, 1) onto [minDelay, maxDelay]. Out-of-range frac is clamped.
func Jitter(minDelay, maxDelay time.Duration, frac float64) time.Duration {
	if maxDelay <= minDelay {
		return minDelay
	}
	switch {
	case frac < 0:
		frac = 0
	case frac > 1:
		frac = 1
	}
	return minDelay + time.Duration(frac*float64(maxDelay-minDelay))
}

// NextSlot returns the first-ping time for an account found after a start.
// Slots are at least step apart, start no earlier than now, and get a random
// offset of up to half a step (frac in [0, 1)), so a restart does not send a
// burst of pings. last is the previous slot handed out, or zero.
// A step of zero or less turns staggering off.
func NextSlot(now, last time.Time, step time.Duration, frac float64) time.Time {
	if step <= 0 {
		return now
	}
	base := now
	if !last.IsZero() {
		if after := last.Add(step); after.After(base) {
			base = after
		}
	}
	return base.Add(Jitter(0, step/2, frac))
}

// Outcome is the result of one ping.
type Outcome struct {
	At time.Time
	OK bool
	// Weekly is read from a successful reply. Failed replies carry no headers.
	Weekly provider.Weekly
	// Delay is the random delay to add after a future reset.
	Delay time.Duration
	// CooldownUntil is the host's retry time for the account after a failure.
	CooldownUntil time.Time
	// Manual marks a ping started from the status page. Scheduled is the next
	// ping that was already planned when it started.
	Manual    bool
	Scheduled time.Time
}

// NextPing returns when the account should be pinged again.
//
//   - Success with a reset ahead (within the window plus slack): reset + Delay.
//   - Success with a reset at or up to SkewTolerance before the ping: the ping
//     landed in the old window; retry after SkewRetry.
//   - Success without a usable reset: recheck after RecheckFallback.
//   - Failure: retry after RetryAfterFailure, or after the host cooldown plus
//     ResetBuffer when that is later. A failed manual ping never moves an
//     already scheduled ping.
func NextPing(o Outcome) time.Time {
	if !o.OK {
		if o.Manual && !o.Scheduled.IsZero() {
			return o.Scheduled
		}
		next := o.At.Add(RetryAfterFailure)
		if !o.CooldownUntil.IsZero() {
			if cooled := o.CooldownUntil.Add(ResetBuffer); cooled.After(next) {
				next = cooled
			}
		}
		return next
	}
	reset := o.Weekly.Reset
	window := o.Weekly.Window
	if window <= 0 {
		window = provider.Week
	}
	switch {
	case reset.IsZero():
		return o.At.Add(RecheckFallback)
	case reset.After(o.At):
		if reset.Sub(o.At) > window+MaxAheadSlack {
			return o.At.Add(RecheckFallback)
		}
		return reset.Add(o.Delay)
	case o.At.Sub(reset) <= SkewTolerance:
		return o.At.Add(SkewRetry)
	default:
		return o.At.Add(RecheckFallback)
	}
}

// DeferForCooldown moves a due ping past an active host cooldown, so the
// plugin does not send a request the host would refuse and count as another
// failure. It returns the new time and true when the ping must wait.
func DeferForCooldown(now, next, retryAfter time.Time) (time.Time, bool) {
	if retryAfter.IsZero() || !retryAfter.After(now) {
		return next, false
	}
	if deferred := retryAfter.Add(ResetBuffer); deferred.After(next) {
		return deferred, true
	}
	return next, true
}

// CooldownCheck is the input to ShouldResetCooldown.
type CooldownCheck struct {
	Now time.Time
	// Enabled is the cooldown_reset setting.
	Enabled bool
	// Reset is the most recent weekly reset known for the account.
	Reset time.Time
	// DoneFor is the reset for which the plugin already cleared a cooldown.
	DoneFor time.Time
	// Unavailable and NextRetryAfter are the host's runtime state for the account.
	Unavailable    bool
	NextRetryAfter time.Time
}

// CooldownDecision is made once per weekly reset, on the first check after
// the reset passes. decide reports that this check is that one: the setting is
// on, a known reset has passed within CooldownResetWindow, and no decision was
// made for it yet (DoneFor differs). The caller then records DoneFor = Reset.
// clear reports that the host shows an active cooldown at that moment, so the
// plugin calls host.routing.reset_cooldown. A cooldown that starts after the
// decision is a new one and is left alone.
func CooldownDecision(c CooldownCheck) (decide, clear bool) {
	switch {
	case !c.Enabled, c.Reset.IsZero(), c.Now.Before(c.Reset):
		return false, false
	case c.Now.Sub(c.Reset) > CooldownResetWindow:
		return false, false
	case !c.DoneFor.IsZero() && c.DoneFor.Equal(c.Reset):
		return false, false
	}
	return true, c.Unavailable || c.NextRetryAfter.After(c.Now)
}
