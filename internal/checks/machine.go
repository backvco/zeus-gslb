// Package checks implements the responder's local health subsystem (plan
// .plans/global-dns-failover.md §7 + P2 contract "Local checks"): probe
// runners (tcp/http/k8s), a per-targetKey state machine driven by each
// record's localCheck thresholds, and dedupe-by-targetKey scheduling at the
// fastest interval any record sharing that target requests.
package checks

import (
	"sync"
)

// Health mirrors the vocabulary bundle.Candidate.Health and the P2 contract
// health-report "state" field use — kept as its own type here (rather than
// importing bundle's string consts) so this package has no dependency on
// bundle beyond what BuildTargets needs.
type Health string

const (
	Healthy   Health = "healthy"
	Unhealthy Health = "unhealthy"
	Unknown   Health = "unknown"
)

// targetState is one targetKey's state-machine bookkeeping.
type targetState struct {
	health        Health
	consecSuccess int
	consecFail    int
	cycles        int // completed check results ever recorded for this key
}

// Machine is the responder's local per-targetKey health state machine
// (plan §7 "State machine in the responder" + P2 contract "Local checks").
// Safe for concurrent use: RecordResult is called from the probe-runner
// goroutine loop, State/Ready/States are read from the DNS-answering
// goroutines and the health-report sender.
type Machine struct {
	mu      sync.Mutex
	targets map[string]*targetState
}

// NewMachine builds an empty Machine. Every targetKey starts at Unknown
// with zero completed cycles (post-boot grace, P2 contract + plan §7).
func NewMachine() *Machine {
	return &Machine{targets: make(map[string]*targetState)}
}

func (m *Machine) getOrCreate(key string) *targetState {
	t, ok := m.targets[key]
	if !ok {
		t = &targetState{health: Unknown}
		m.targets[key] = t
	}
	return t
}

// RecordResult feeds one probe result (up/down) for targetKey through its
// threshold configuration. Thresholds apply symmetrically from ANY current
// health value (including Unknown) — reaching failureThreshold consecutive
// failures always moves to Unhealthy, reaching recoveryThreshold consecutive
// successes always moves to Healthy — matching the "consecutive-failure/
// success counters against the record's thresholds" mechanic in plan §7.
// Returns whether this result caused a transition and the (possibly
// unchanged) resulting health.
func (m *Machine) RecordResult(key string, up bool, failureThreshold, recoveryThreshold int) (transitioned bool, newHealth Health) {
	if failureThreshold < 1 {
		failureThreshold = 1
	}
	if recoveryThreshold < 1 {
		recoveryThreshold = 1
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	t := m.getOrCreate(key)
	prev := t.health
	t.cycles++

	if up {
		t.consecSuccess++
		t.consecFail = 0
		if t.health != Healthy && t.consecSuccess >= recoveryThreshold {
			t.health = Healthy
		}
	} else {
		t.consecFail++
		t.consecSuccess = 0
		if t.health != Unhealthy && t.consecFail >= failureThreshold {
			t.health = Unhealthy
		}
	}

	return t.health != prev, t.health
}

// State returns targetKey's current health — Unknown for a key this
// Machine has never recorded a result for (never probed, or not a locally-
// checked target at all).
func (m *Machine) State(key string) Health {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.targets[key]
	if !ok {
		return Unknown
	}
	return t.health
}

// Ready reports whether targetKey has completed at least one full check
// cycle. Per the P2 contract's post-boot grace rule, the responder's
// candidate-walk defers a Ready()==false, State()==Unknown target to the
// pushed health value instead of treating "not yet unhealthy" as
// automatically viable.
func (m *Machine) Ready(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.targets[key]
	return ok && t.cycles >= 1
}

// States returns a snapshot of every targetKey's current health, keyed by
// targetKey with string values ("healthy"|"unhealthy"|"unknown") — the
// exact shape the P2 contract's health report "states" field needs on
// every report (heartbeat or transition-triggered).
func (m *Machine) States() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.targets))
	for k, t := range m.targets {
		out[k] = string(t.health)
	}
	return out
}
