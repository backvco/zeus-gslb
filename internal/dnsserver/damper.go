package dnsserver

import (
	"strconv"
	"time"

	"github.com/backvco/zeus-gslb/internal/bundle"
)

// Damping constants — plan §7 "Flap damping (precise semantics)": more than
// 3 answer-set changes in a rolling 10-minute window freezes the record at
// its last-served answer set; a freeze auto-clears after 20 minutes with no
// further suppressed change attempt.
const (
	dampWindow          = 10 * time.Minute
	dampChangeThreshold = 3 // >3 changes in window => freeze
	unfreezeQuietPeriod = 20 * time.Minute
)

// recordDampState is one record's damping bookkeeping.
type recordDampState struct {
	served         []bundle.Answer
	changeLog      []time.Time // accepted (non-forced) change timestamps, pruned to dampWindow
	frozen         bool
	lastSuppressed time.Time // last time a non-forced change was suppressed (or the freeze moment itself)
}

// Damper implements the responder-local mirror of the flap-damping policy
// (plan §7). It is deliberately per-fqdn/per-process state — the
// authoritative cross-cluster damping lives in zeus (reconcile.js); this is
// only the "mirrored locally in the responder" half plan §7 calls for, so a
// local flip doesn't oscillate answers between two heartbeat pushes.
type Damper struct {
	now     func() time.Time
	records map[string]*recordDampState
}

// NewDamper builds a Damper. now is injected so tests can drive freeze/
// unfreeze timing without real sleeps.
func NewDamper(now func() time.Time) *Damper {
	if now == nil {
		now = time.Now
	}
	return &Damper{now: now, records: make(map[string]*recordDampState)}
}

// Evaluate decides the answer set actually served for fqdn given the
// freshly-computed candidate answer set. forced must be true iff this
// change is a move away from a candidate that just became unhealthy (plan
// §7: "a move away from a now-unhealthy answer is a forced move and is
// ALWAYS allowed"). Forced moves bypass freeze and are never counted toward
// the damping window themselves.
func (d *Damper) Evaluate(fqdn string, candidate []bundle.Answer, forced bool) []bundle.Answer {
	rs, ok := d.records[fqdn]
	if !ok {
		rs = &recordDampState{served: candidate}
		d.records[fqdn] = rs
		return candidate
	}

	now := d.now()
	d.maybeAutoUnfreeze(rs, now)

	if answerSetEqual(rs.served, candidate) {
		return rs.served
	}

	if forced {
		rs.served = candidate
		return candidate
	}

	if rs.frozen {
		rs.lastSuppressed = now
		return rs.served
	}

	tentative := pruneWindow(append(append([]time.Time{}, rs.changeLog...), now), now)
	if len(tentative) > dampChangeThreshold {
		// This change would be the 4th+ in the window: freeze instead of
		// applying it, and start the quiet-period clock for auto-unfreeze.
		rs.frozen = true
		rs.lastSuppressed = now
		return rs.served
	}

	rs.changeLog = tentative
	rs.served = candidate
	return candidate
}

// Frozen reports whether fqdn is currently damped — exposed for the
// `global-endpoint-flapping` alert surface / diagnostics, not used in the
// answer path itself.
func (d *Damper) Frozen(fqdn string) bool {
	rs, ok := d.records[fqdn]
	return ok && rs.frozen
}

func (d *Damper) maybeAutoUnfreeze(rs *recordDampState, now time.Time) {
	if !rs.frozen {
		return
	}
	if now.Sub(rs.lastSuppressed) >= unfreezeQuietPeriod {
		rs.frozen = false
		rs.changeLog = nil
	}
}

func pruneWindow(ts []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-dampWindow)
	out := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	return out
}

func answerSetEqual(a, b []bundle.Answer) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]int, len(a))
	for _, ans := range a {
		set[answerKey(ans)]++
	}
	for _, ans := range b {
		k := answerKey(ans)
		if set[k] == 0 {
			return false
		}
		set[k]--
	}
	return true
}

func answerKey(a bundle.Answer) string {
	return a.IP + ":" + strconv.Itoa(a.Port)
}
