package remote

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeStates struct {
	mu sync.Mutex
	m  map[string]string
}

func (f *fakeStates) States() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.m))
	for k, v := range f.m {
		out[k] = v
	}
	return out
}
func (f *fakeStates) set(k, v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = map[string]string{}
	}
	f.m[k] = v
}

type reportRecorder struct {
	mu      sync.Mutex
	reports []HealthReport
	handler func(w http.ResponseWriter, r *http.Request) // override for failure-injection tests
	srv     *httptest.Server
}

func newReportRecorder() *reportRecorder {
	rr := &reportRecorder{}
	rr.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rr.handler != nil {
			rr.handler(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var report HealthReport
		_ = json.Unmarshal(body, &report)
		rr.mu.Lock()
		rr.reports = append(rr.reports, report)
		rr.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	return rr
}

func (rr *reportRecorder) count() int {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	return len(rr.reports)
}

func (rr *reportRecorder) last() HealthReport {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	return rr.reports[len(rr.reports)-1]
}

func (rr *reportRecorder) close() { rr.srv.Close() }

func waitForCount(t *testing.T, rr *reportRecorder, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rr.count() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d reports, got %d", n, rr.count())
}

func TestHealthReporter_TransitionSendsImmediately(t *testing.T) {
	rr := newReportRecorder()
	defer rr.close()

	client := NewClient(rr.srv.URL, "z-01", "tok")
	states := &fakeStates{m: map[string]string{"t1": "healthy"}}
	reporter := &HealthReporter{Client: client, States: states, HeartbeatInterval: time.Hour} // heartbeat effectively disabled for this test

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reporter.Run(ctx)

	reporter.NotifyTransition("t1", "unhealthy", time.Now())

	waitForCount(t, rr, 1, 2*time.Second)
	last := rr.last()
	if len(last.Transitions) != 1 || last.Transitions[0].TargetKey != "t1" || last.Transitions[0].State != "unhealthy" {
		t.Fatalf("unexpected transitions in report: %+v", last.Transitions)
	}
	if last.States["t1"] != "healthy" {
		t.Fatalf("states in report = %v, want full current states map", last.States)
	}
}

func TestHealthReporter_HeartbeatCadenceSendsFullStatesEvenWithNoTransitions(t *testing.T) {
	rr := newReportRecorder()
	defer rr.close()

	client := NewClient(rr.srv.URL, "z-01", "tok")
	states := &fakeStates{m: map[string]string{"t1": "healthy", "t2": "unhealthy"}}
	reporter := &HealthReporter{Client: client, States: states, HeartbeatInterval: 30 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reporter.Run(ctx)

	waitForCount(t, rr, 2, 2*time.Second)
	last := rr.last()
	if len(last.Transitions) != 0 {
		t.Errorf("heartbeat report should have empty transitions, got %+v", last.Transitions)
	}
	if last.States["t1"] != "healthy" || last.States["t2"] != "unhealthy" {
		t.Errorf("heartbeat states = %v, want full map", last.States)
	}
}

func TestHealthReporter_RetriesOnFailureThenSucceeds(t *testing.T) {
	rr := newReportRecorder()
	defer rr.close()

	var attempts int32
	rr.handler = func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var report HealthReport
		_ = json.Unmarshal(body, &report)
		rr.mu.Lock()
		rr.reports = append(rr.reports, report)
		rr.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}

	client := NewClient(rr.srv.URL, "z-01", "tok")
	states := &fakeStates{m: map[string]string{"t1": "healthy"}}
	reporter := &HealthReporter{
		Client:            client,
		States:            states,
		HeartbeatInterval: time.Hour,
		Backoff:           LinearBackoff(5 * time.Millisecond),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reporter.Run(ctx)

	reporter.NotifyTransition("t1", "unhealthy", time.Now())

	waitForCount(t, rr, 1, 2*time.Second)
	if atomic.LoadInt32(&attempts) < 3 {
		t.Fatalf("attempts = %d, want >= 3 (2 failures then success)", attempts)
	}
}

func TestHealthReporter_NeverBlocksOnPermanentFailure(t *testing.T) {
	rr := newReportRecorder()
	defer rr.close()
	rr.handler = func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.WriteHeader(http.StatusInternalServerError) // always fails
	}

	client := NewClient(rr.srv.URL, "z-01", "tok")
	states := &fakeStates{m: map[string]string{"t1": "healthy"}}
	reporter := &HealthReporter{
		Client:            client,
		States:            states,
		HeartbeatInterval: 20 * time.Millisecond,
		Backoff:           LinearBackoff(1 * time.Millisecond),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		reporter.Run(ctx)
		close(done)
	}()

	// Give it enough heartbeat cycles that, if sendNow ever blocked
	// forever, this test would hang and eventually time out — instead we
	// just assert the loop is still alive and processing ticks well past
	// several failed-report rounds.
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation — report failures may be blocking the loop")
	}
}
