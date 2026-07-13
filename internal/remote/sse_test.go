package remote

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// flushWriter is the minimal interface httptest ResponseWriters satisfy.
type flushWriter interface {
	http.ResponseWriter
	http.Flusher
}

func TestSubscribe_DispatchesOnEventFrame(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fw := w.(flushWriter)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: bundle-updated\ndata: {}\n\n"))
		fw.Flush()
		<-r.Context().Done() // hold the connection open until the client cancels
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "z-01", "tok")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	events := 0
	err := c.Subscribe(ctx, 5*time.Second, func() { events++ })

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected Subscribe to end via ctx deadline, got: %v", err)
	}
	if events != 1 {
		t.Errorf("expected exactly 1 dispatched event, got %d", events)
	}
}

func TestSubscribe_KeepaliveCommentDoesNotDispatchButResetsIdleTimer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fw := w.(flushWriter)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		ticker := time.NewTicker(30 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				_, _ = w.Write([]byte(": keepalive\n\n"))
				fw.Flush()
			}
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "z-01", "tok")
	// idleTimeout (100ms) is longer than the keepalive cadence (30ms), so a
	// stream that only ever sends keepalives must survive the whole test
	// window without tripping the idle timer.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	events := 0
	err := c.Subscribe(ctx, 100*time.Millisecond, func() { events++ })

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected Subscribe to survive on keepalives and end via ctx deadline, got: %v", err)
	}
	if events != 0 {
		t.Errorf("keepalive-only stream must never dispatch onEvent, got %d calls", events)
	}
}

func TestSubscribe_IdleTimeoutForcesReconnect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fw := w.(flushWriter)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fw.Flush()
		// Go silent — no event, no keepalive — for the whole request. This
		// is the "half-open stream" failure mode: connected, but delivering
		// nothing. idle-timeout must fire independent of any transport
		// error/close.
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "z-01", "tok")
	// A generous outer ctx so only the idle timer (short) can plausibly
	// end this call.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	err := c.Subscribe(ctx, 80*time.Millisecond, func() { t.Error("onEvent must not fire on a silent stream") })
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "idle timeout") {
		t.Fatalf("expected an idle-timeout error, got: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Subscribe took %s to return — idle timeout did not force a prompt reconnect", elapsed)
	}
}
