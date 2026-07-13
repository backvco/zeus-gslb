package remote

import (
	"context"
	"log"
	"time"

	"github.com/backvco/zeus-gslb/internal/bundle"
)

// Runner drives zeus-fetch mode's steady-state loop (plan §8/§10): hold an
// SSE subscription open; on any event, refetch the bundle and apply it if
// its contentHash changed; if the stream drops (error, close, or its own
// idle-timeout) reconnect with backoff, and — critically — poll the bundle
// endpoint every PollInterval while the stream is down, so answers do not
// go stale for the whole backoff window.
//
// Boot (the initial fetch-with-retries-then-cache-fallback) is a separate,
// one-shot step (bundle.BootFromFetch) that runs before a Runner exists;
// Runner only owns the post-boot subscribe/poll steady state.
type Runner struct {
	Client *Client
	Store  *bundle.Store

	// IdleTimeout is passed through to Client.Subscribe.
	IdleTimeout time.Duration
	// PollInterval is the bundle-poll cadence while the event stream is
	// down.
	PollInterval time.Duration
	// Backoff computes the delay before reconnect attempt N (1-indexed).
	// If nil, reconnects immediately (still bounded by the fact that a
	// connect failure itself takes wall-clock time).
	Backoff func(attempt int) time.Duration
}

// Run blocks until ctx is cancelled, alternating between "subscribed" and
// "down, polling" states.
func (r *Runner) Run(ctx context.Context) {
	attempt := 0
	for ctx.Err() == nil {
		err := r.Client.Subscribe(ctx, r.IdleTimeout, func() {
			r.refetchAndApply(ctx, "event")
		})
		if ctx.Err() != nil {
			return
		}

		attempt++
		var wait time.Duration
		if r.Backoff != nil {
			wait = r.Backoff(attempt)
		}
		log.Printf("remote: events stream down (%v) — polling bundle every %s, reconnecting in %s",
			err, r.PollInterval, wait)
		r.pollUntil(ctx, wait)
	}
}

// pollUntil polls the bundle endpoint every PollInterval until d has
// elapsed or ctx is cancelled — the "poll fallback while stream down"
// behavior. It never invents policy: a poll is just another trigger for
// the same refetchAndApply the event path uses.
func (r *Runner) pollUntil(ctx context.Context, d time.Duration) {
	ticker := time.NewTicker(r.PollInterval)
	defer ticker.Stop()

	var deadline <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		deadline = t.C
	} else {
		// Zero backoff: still take at least one poll pass before looping
		// back to reconnect, via a timer that fires immediately.
		t := time.NewTimer(0)
		defer t.Stop()
		deadline = t.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			return
		case <-ticker.C:
			r.refetchAndApply(ctx, "poll")
		}
	}
}

func (r *Runner) refetchAndApply(ctx context.Context, source string) {
	raw, err := r.Client.FetchBundle(ctx)
	if err != nil {
		log.Printf("remote: %s-triggered bundle refetch failed: %v", source, err)
		return
	}

	applied, err := r.Store.ApplyIfChanged(raw)
	if err != nil {
		// Includes protocolVersion mismatch / structural validation
		// failure — plan §8: "reject, keep serving, log loud". ApplyIfChanged
		// never touches the currently-served bundle on error.
		log.Printf("remote: %s-triggered bundle refetch rejected (keeping previous bundle): %v", source, err)
		return
	}
	if applied {
		log.Printf("remote: %s-triggered bundle refetch applied a new bundle", source)
	}
}

// LinearBackoff returns attempt*step — used for the small, bounded
// boot-fetch retry loop (task spec: "3 retries, backoff").
func LinearBackoff(step time.Duration) func(attempt int) time.Duration {
	return func(attempt int) time.Duration {
		return time.Duration(attempt) * step
	}
}

// ExponentialBackoff doubles from base each reconnect attempt, capped at
// max — used for the SSE reconnect loop, which retries indefinitely (the
// poll fallback keeps answers fresh while the stream stays down, so an
// unbounded retry count is safe).
func ExponentialBackoff(base, max time.Duration) func(attempt int) time.Duration {
	return func(attempt int) time.Duration {
		if attempt < 1 {
			attempt = 1
		}
		d := base
		for i := 1; i < attempt; i++ {
			if d >= max {
				return max
			}
			d *= 2
		}
		if d > max {
			return max
		}
		return d
	}
}
