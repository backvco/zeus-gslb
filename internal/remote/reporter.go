package remote

import (
	"context"
	"log"
	"sync"
	"time"
)

// maxHealthReportRetries bounds retry attempts for one report round. If a
// round exhausts its retries, transitions accumulated in that round are
// dropped — but this is self-healing, never a silent permanent loss:
// `states` is always the FULL current map on every subsequent report
// (transition-triggered or heartbeat), so the very next successful send
// re-establishes the true picture. Never crash/stop serving on report
// failure (P2 contract) — the caller (Runner.Run) must never be blocked by
// this bound; it just stops retrying this particular round.
const maxHealthReportRetries = 5

// StatesProvider supplies the full current targetKey->health map — the
// `states` field the contract requires on every report, satisfied by
// *checks.Machine.States().
type StatesProvider interface {
	States() map[string]string
}

// HealthReporter drives the P2 contract's report cadence: an immediate send
// on any transition (queued via NotifyTransition), and a heartbeat send
// every HeartbeatInterval regardless (possibly with zero queued
// transitions — `states` is always populated). All sends are serialized
// through one goroutine (Run) so concurrent transitions and the heartbeat
// ticker never race on the network call itself.
type HealthReporter struct {
	Client            *Client
	States            StatesProvider
	HeartbeatInterval time.Duration
	Backoff           func(attempt int) time.Duration // retry backoff on send failure

	wake chan struct{}

	mu          sync.Mutex
	pending     []Transition
	initialized bool
}

// NotifyTransition queues a transition and wakes the sender loop for an
// immediate send (P2 contract: "Sent: immediately on any transition").
// Safe to call before Run starts (the queue is buffered) and from any
// goroutine (e.g. the checks.Runner's OnTransition callback).
func (r *HealthReporter) NotifyTransition(targetKey string, health string, at time.Time) {
	r.ensureInit()

	r.mu.Lock()
	r.pending = append(r.pending, Transition{TargetKey: targetKey, State: health, At: at.UTC().Format(time.RFC3339Nano)})
	r.mu.Unlock()

	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *HealthReporter) ensureInit() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.initialized {
		r.wake = make(chan struct{}, 1)
		r.initialized = true
	}
}

// Run serializes heartbeat ticks and transition-wake sends into one send
// loop until ctx is cancelled.
func (r *HealthReporter) Run(ctx context.Context) {
	r.ensureInit()
	interval := r.HeartbeatInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sendNow(ctx)
		case <-r.wake:
			r.sendNow(ctx)
		}
	}
}

func (r *HealthReporter) sendNow(ctx context.Context) {
	r.mu.Lock()
	transitions := r.pending
	r.pending = nil
	r.mu.Unlock()

	var states map[string]string
	if r.States != nil {
		states = r.States.States()
	}

	report := HealthReport{
		Cluster:         r.Client.Cluster,
		ProtocolVersion: HealthProtocolVersion,
		Transitions:     transitions,
		States:          states,
	}

	attempt := 0
	for {
		err := r.Client.ReportHealth(ctx, report)
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			return // shutting down — never crash, just stop
		}

		attempt++
		if attempt > maxHealthReportRetries {
			log.Printf("remote: health report failed after %d attempts (%v) — giving up this round, next heartbeat resends full states", attempt-1, err)
			return
		}

		var wait time.Duration
		if r.Backoff != nil {
			wait = r.Backoff(attempt)
		}
		log.Printf("remote: health report send failed (attempt %d/%d): %v — retrying in %s", attempt, maxHealthReportRetries, err, wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
