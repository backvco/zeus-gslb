package checks

import (
	"context"
	"testing"
	"time"

	"github.com/backvco/zeus-gslb/internal/bundle"
)

func TestBuildTargets_DedupesByTargetKeyAtFastestInterval(t *testing.T) {
	records := []bundle.Record{
		{
			FQDN: "a.z-backv.local",
			LocalChecks: []bundle.LocalCheck{
				{TargetKey: "app1/z-01/prod/api:443", Probe: "tcp", Host: "api", Port: 443, IntervalMs: 5000, FailureThreshold: 3, RecoveryThreshold: 2},
			},
		},
		{
			// Second record shares the same target at a faster interval —
			// the dedupe must run at the fastest requested interval.
			FQDN: "b.z-backv.local",
			LocalChecks: []bundle.LocalCheck{
				{TargetKey: "app1/z-01/prod/api:443", Probe: "tcp", Host: "api", Port: 443, IntervalMs: 1000, FailureThreshold: 3, RecoveryThreshold: 2},
			},
		},
	}

	targets := BuildTargets(records, nil)
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want 1 (deduped)", len(targets))
	}
	tgt := targets["app1/z-01/prod/api:443"]
	if tgt.Config.IntervalMs != 1000 {
		t.Errorf("IntervalMs = %d, want 1000 (fastest of 5000/1000)", tgt.Config.IntervalMs)
	}
}

func TestBuildTargets_SkipsUnrecognizedProbeButKeepsOthers(t *testing.T) {
	records := []bundle.Record{
		{FQDN: "a", LocalChecks: []bundle.LocalCheck{{TargetKey: "bad", Probe: "carrier-pigeon"}}},
		{FQDN: "b", LocalChecks: []bundle.LocalCheck{{TargetKey: "good", Probe: "tcp", Host: "h", Port: 1}}},
	}
	targets := BuildTargets(records, nil)
	if _, ok := targets["bad"]; ok {
		t.Error("unrecognized probe type should be skipped, not included")
	}
	if _, ok := targets["good"]; !ok {
		t.Error("valid target should still be built despite a sibling bad one")
	}
}

// countingProber counts Probe() calls and always reports up=upValue.
type countingProber struct {
	calls  *int
	upFunc func() bool
}

func (p countingProber) Probe(ctx context.Context) bool {
	*p.calls++
	return p.upFunc()
}

func TestRunner_RunOnce_RespectsPerTargetInterval(t *testing.T) {
	calls := 0
	target := Target{
		Config: LocalCheckConfig{IntervalMs: 1000, FailureThreshold: 1, RecoveryThreshold: 1},
		Prober: countingProber{calls: &calls, upFunc: func() bool { return true }},
	}
	r := &Runner{
		Targets: func() map[string]Target { return map[string]Target{"t1": target} },
		Machine: NewMachine(),
	}

	base := time.Unix(0, 0)
	r.RunOnce(context.Background(), base) // due immediately (never run before)
	r.RunOnce(context.Background(), base.Add(200*time.Millisecond))
	r.RunOnce(context.Background(), base.Add(999*time.Millisecond))
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (interval not yet elapsed)", calls)
	}
	r.RunOnce(context.Background(), base.Add(1001*time.Millisecond))
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (interval elapsed)", calls)
	}
}

func TestRunner_RunOnce_FiresOnTransition(t *testing.T) {
	calls := 0
	up := true
	target := Target{
		Config: LocalCheckConfig{IntervalMs: 100, FailureThreshold: 1, RecoveryThreshold: 1},
		Prober: countingProber{calls: &calls, upFunc: func() bool { return up }},
	}
	var gotKey string
	var gotHealth Health
	transitions := 0
	r := &Runner{
		Targets: func() map[string]Target { return map[string]Target{"t1": target} },
		Machine: NewMachine(),
		OnTransition: func(key string, h Health, at time.Time) {
			transitions++
			gotKey, gotHealth = key, h
		},
	}

	base := time.Unix(0, 0)
	r.RunOnce(context.Background(), base) // Unknown -> Healthy (threshold 1)
	if transitions != 1 || gotKey != "t1" || gotHealth != Healthy {
		t.Fatalf("after 1st run: transitions=%d key=%q health=%v", transitions, gotKey, gotHealth)
	}

	up = false
	r.RunOnce(context.Background(), base.Add(200*time.Millisecond)) // Healthy -> Unhealthy
	if transitions != 2 || gotHealth != Unhealthy {
		t.Fatalf("after 2nd run: transitions=%d health=%v", transitions, gotHealth)
	}

	// Same result again: no further transition.
	r.RunOnce(context.Background(), base.Add(400*time.Millisecond))
	if transitions != 2 {
		t.Fatalf("transitions = %d, want 2 (no-op repeat must not re-fire)", transitions)
	}
}

func TestRunner_RunOnce_TargetSetChangesLive(t *testing.T) {
	calls1, calls2 := 0, 0
	live := map[string]Target{
		"t1": {Config: LocalCheckConfig{IntervalMs: 100, FailureThreshold: 1, RecoveryThreshold: 1}, Prober: countingProber{calls: &calls1, upFunc: func() bool { return true }}},
	}
	r := &Runner{
		Targets: func() map[string]Target { return live },
		Machine: NewMachine(),
	}
	base := time.Unix(0, 0)
	r.RunOnce(context.Background(), base)
	if calls1 != 1 {
		t.Fatalf("calls1 = %d, want 1", calls1)
	}

	// Simulate a bundle update adding a second target — must be picked up
	// without any special reconciliation step.
	live["t2"] = Target{Config: LocalCheckConfig{IntervalMs: 100, FailureThreshold: 1, RecoveryThreshold: 1}, Prober: countingProber{calls: &calls2, upFunc: func() bool { return true }}}
	r.RunOnce(context.Background(), base.Add(150*time.Millisecond))
	if calls2 != 1 {
		t.Fatalf("calls2 = %d, want 1 (new target should run on first tick it appears)", calls2)
	}
}
