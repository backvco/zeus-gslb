package checks

import (
	"context"
	"log"
	"time"

	"github.com/backvco/zeus-gslb/internal/bundle"
)

// LocalCheckConfig is the probe-runner's own copy of bundle.LocalCheck — a
// distinct type (rather than importing bundle.LocalCheck directly into
// Prober/Runner signatures) so this package's public surface doesn't leak
// the wire/JSON shape, only the fields the runner actually needs.
type LocalCheckConfig struct {
	TargetKey         string
	Probe             string
	Host              string
	Port              int
	Path              string
	Namespace         string
	Service           string
	IntervalMs        int
	FailureThreshold  int
	RecoveryThreshold int
}

// Target is one deduped, schedulable probe target.
type Target struct {
	Config LocalCheckConfig
	Prober Prober
}

// BuildTargets dedupes every record's localChecks by targetKey (plan §7 /
// P2 contract: "Dedupe by targetKey across records; run at the fastest
// interval any record requests"). Thresholds and probe definition are taken
// from the first occurrence seen for a targetKey — they are properties of
// the target, not the record, so producers are expected to repeat them
// identically across records sharing a target; only IntervalMs is actively
// reconciled to the minimum across duplicates.
//
// A localCheck whose probe type NewProber rejects, or whose k8s probe has
// no lister available, is logged and skipped rather than aborting the
// whole build — one bad/unsupported target must never stop every other
// target's probing (mirrors the "per-record try/catch" resilience
// invariant used elsewhere in this system).
func BuildTargets(records []bundle.Record, k8sLister EndpointSliceLister) map[string]Target {
	configs := make(map[string]LocalCheckConfig)
	for _, rec := range records {
		for _, lc := range rec.LocalChecks {
			cfg := LocalCheckConfig{
				TargetKey:         lc.TargetKey,
				Probe:             lc.Probe,
				Host:              lc.Host,
				Port:              lc.Port,
				Path:              lc.Path,
				Namespace:         lc.Namespace,
				Service:           lc.Service,
				IntervalMs:        lc.IntervalMs,
				FailureThreshold:  lc.FailureThreshold,
				RecoveryThreshold: lc.RecoveryThreshold,
			}
			existing, ok := configs[lc.TargetKey]
			if !ok {
				configs[lc.TargetKey] = cfg
				continue
			}
			if cfg.IntervalMs > 0 && (existing.IntervalMs <= 0 || cfg.IntervalMs < existing.IntervalMs) {
				existing.IntervalMs = cfg.IntervalMs
				configs[lc.TargetKey] = existing
			}
		}
	}

	out := make(map[string]Target, len(configs))
	for key, cfg := range configs {
		prober, err := NewProber(cfg, k8sLister)
		if err != nil {
			log.Printf("checks: skipping targetKey %q: %v", key, err)
			continue
		}
		out[key] = Target{Config: cfg, Prober: prober}
	}
	return out
}

// Runner drives the periodic probe loop: each tick, run any target whose
// own IntervalMs has elapsed since it last ran, feed the result through
// Machine, and invoke OnTransition on any resulting health transition.
//
// Targets is a function (not a static map) so the live target set can
// change as the bundle changes — RunOnce always uses whatever Targets()
// returns for that tick, dropped/added targets take effect on the next
// tick with no separate reconciliation step. lastRun state persists on the
// Runner across ticks so an added target starts "due immediately" (no
// lastRun entry yet) and a removed target's schedule state is simply
// forgotten (bounded memory: the live target set is bounded by the ≤200
// soft record cap, plan §7/§8).
type Runner struct {
	Targets      func() map[string]Target
	Machine      *Machine
	OnTransition func(targetKey string, health Health, at time.Time)
	Now          func() time.Time // defaults to time.Now if nil

	lastRun map[string]time.Time
}

// RunOnce executes one scheduling pass at the given "now" — the whole
// runner is expressed around an explicit "now" parameter (rather than
// reading time.Now() internally) precisely so tests can drive many
// scheduling decisions deterministically without any real sleep (task
// requirement: "no sleeps>100ms in tests").
func (r *Runner) RunOnce(ctx context.Context, now time.Time) {
	if r.lastRun == nil {
		r.lastRun = make(map[string]time.Time)
	}
	for key, t := range r.Targets() {
		interval := time.Duration(t.Config.IntervalMs) * time.Millisecond
		if interval <= 0 {
			interval = time.Second
		}
		last, seen := r.lastRun[key]
		if seen && now.Sub(last) < interval {
			continue
		}
		r.lastRun[key] = now

		up := t.Prober.Probe(ctx)
		transitioned, newHealth := r.Machine.RecordResult(key, up, t.Config.FailureThreshold, t.Config.RecoveryThreshold)
		if transitioned && r.OnTransition != nil {
			r.OnTransition(key, newHealth, now)
		}
	}
}

// Run drives RunOnce on a real ticker until ctx is cancelled — the
// production steady-state loop. baseTick is the scheduling granularity
// (how often the runner re-checks which targets are due); it should be
// comfortably smaller than the fastest supported intervalMs preset (Fast
// preset = 1s, plan §7) — 250ms keeps detection latency error well under a
// second without busy-looping.
func (r *Runner) Run(ctx context.Context, baseTick time.Duration) {
	now := r.Now
	if now == nil {
		now = time.Now
	}
	ticker := time.NewTicker(baseTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			_ = t
			r.RunOnce(ctx, now())
		}
	}
}
