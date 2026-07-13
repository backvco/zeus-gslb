package remote

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Subscribe connects to the events endpoint and calls onEvent once per
// dispatched SSE event (an event is one or more "field: value" lines
// terminated by a blank line — per the SSE wire format — that included at
// least one "event:" or "data:" field; bare comment lines, i.e. lines
// starting with ":", are keepalives and do NOT call onEvent).
//
// Per plan §8, the payload is never trusted for content: onEvent takes no
// arguments, and the caller's job on every call is simply "refetch the
// bundle".
//
// Liveness/idle-timeout (plan §8, explicit requirement — "NOT merely on
// onerror"): every line read (event field OR keepalive comment) resets an
// idle timer. If idleTimeout elapses with no line at all, Subscribe force-
// closes the connection and returns an error — half-open streams that look
// connected but deliver nothing must not wedge the caller.
//
// Subscribe blocks until: ctx is cancelled (returns ctx.Err()), the idle
// timer fires, or the connection errors/closes for any other reason. In
// every case it returns a non-nil error except the ctx-cancelled case,
// which is also returned as an error (ctx.Err()) so the caller can
// distinguish "told to stop" from other reasons uniformly if it wants to,
// though Runner treats ctx.Err() specially to avoid reconnect churn on
// shutdown.
func (c *Client) Subscribe(ctx context.Context, idleTimeout time.Duration, onEvent func()) error {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.eventsURL(), nil)
	if err != nil {
		return fmt.Errorf("build events request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.streamClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("connect events stream: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("events stream: unexpected status %d: %s", resp.StatusCode, truncate(body, 200))
	}

	lines := make(chan string)
	readErr := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-reqCtx.Done():
				return
			}
		}
		if serr := scanner.Err(); serr != nil {
			readErr <- serr
			return
		}
		readErr <- io.EOF
	}()

	timer := time.NewTimer(idleTimeout)
	defer timer.Stop()

	sawEventField := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-timer.C:
			cancel() // force-close the read: the goroutine unblocks via reqCtx
			return fmt.Errorf("events stream: idle timeout after %s with no frames", idleTimeout)

		case line := <-lines:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idleTimeout)

			switch {
			case line == "":
				// Blank line = dispatch boundary. Only a real event
				// (event:/data: field seen since the last boundary) fires
				// onEvent; a keepalive-only block does not.
				if sawEventField {
					onEvent()
					sawEventField = false
				}
			case strings.HasPrefix(line, ":"):
				// Keepalive comment frame: already reset the idle timer
				// above; not an event.
			case strings.HasPrefix(line, "event:"), strings.HasPrefix(line, "data:"):
				sawEventField = true
			default:
				// Other SSE fields (id:, retry:, etc.) — ignored for
				// dispatch purposes but still valid liveness, already
				// accounted for by the timer reset above.
			}

		case err := <-readErr:
			return fmt.Errorf("events stream closed: %w", err)
		}
	}
}
