package remote

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/backvco/zeus-gslb/internal/bundle"
)

func bundleJSON(domain, hash string) string {
	return fmt.Sprintf(`{"protocolVersion":1,"domain":%q,"contentHash":%q,"records":[]}`, domain, hash)
}

// testServer serves both endpoints so Runner can be driven end-to-end. The
// events handler is swappable per test so each test can script its own
// stream behavior.
type testServer struct {
	mu         sync.Mutex
	bundleBody string
	bundleHits int32

	eventsHandler http.HandlerFunc

	srv *httptest.Server
}

func newTestServer(initialBundle string) *testServer {
	ts := &testServer{bundleBody: initialBundle}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/global-endpoints/bundle", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ts.bundleHits, 1)
		ts.mu.Lock()
		body := ts.bundleBody
		ts.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/api/global-endpoints/events", func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		h := ts.eventsHandler
		ts.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	ts.srv = httptest.NewServer(mux)
	return ts
}

func (ts *testServer) setBundle(body string) {
	ts.mu.Lock()
	ts.bundleBody = body
	ts.mu.Unlock()
}

func (ts *testServer) setEventsHandler(h http.HandlerFunc) {
	ts.mu.Lock()
	ts.eventsHandler = h
	ts.mu.Unlock()
}

func (ts *testServer) close() { ts.srv.Close() }

func TestRunner_EventTriggersRefetchAndSwap(t *testing.T) {
	v1 := bundleJSON("z-backv-v1.local", "hash-v1")
	v2 := bundleJSON("z-backv-v2.local", "hash-v2")

	ts := newTestServer(v1)
	defer ts.close()

	fired := make(chan struct{}, 1)
	ts.setEventsHandler(func(w http.ResponseWriter, r *http.Request) {
		fw := w.(flushWriter)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: bundle-updated\ndata: {}\n\n"))
		fw.Flush()
		select {
		case fired <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	})

	store := bootStoreForTest(t, ts.srv.URL, v1)
	client := NewClient(ts.srv.URL, "z-01", "tok")
	runner := &Runner{Client: client, Store: store, IdleTimeout: 2 * time.Second, PollInterval: 50 * time.Millisecond, Backoff: LinearBackoff(20 * time.Millisecond)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runner.Run(ctx)

	<-fired
	// Once the event fires, the server's next bundle GET should serve v2 —
	// flip it now so the refetch triggered by the event picks up the change.
	ts.setBundle(v2)

	waitFor(t, 2*time.Second, func() bool {
		return store.Current().Domain == "z-backv-v2.local"
	})
}

func TestRunner_PollFallbackWhileStreamDown(t *testing.T) {
	v1 := bundleJSON("z-backv-v1.local", "hash-v1")
	v2 := bundleJSON("z-backv-v2.local", "hash-v2")

	ts := newTestServer(v1)
	defer ts.close()
	// No events handler set => 503 on every connect attempt, so Subscribe
	// fails immediately every time and the Runner must fall back to polling.

	store := bootStoreForTest(t, ts.srv.URL, v1)
	client := NewClient(ts.srv.URL, "z-01", "tok")
	runner := &Runner{Client: client, Store: store, IdleTimeout: 2 * time.Second, PollInterval: 30 * time.Millisecond, Backoff: LinearBackoff(20 * time.Millisecond)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runner.Run(ctx)

	ts.setBundle(v2)

	waitFor(t, 2*time.Second, func() bool {
		return store.Current().Domain == "z-backv-v2.local"
	})
}

func TestRunner_HashUnchangedRefetchIsNoOp(t *testing.T) {
	v1 := bundleJSON("z-backv-v1.local", "hash-v1")

	ts := newTestServer(v1)
	defer ts.close()

	fired := make(chan struct{}, 1)
	ts.setEventsHandler(func(w http.ResponseWriter, r *http.Request) {
		fw := w.(flushWriter)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: bundle-updated\ndata: {}\n\n"))
		fw.Flush()
		select {
		case fired <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	})

	store := bootStoreForTest(t, ts.srv.URL, v1)
	client := NewClient(ts.srv.URL, "z-01", "tok")
	runner := &Runner{Client: client, Store: store, IdleTimeout: 2 * time.Second, PollInterval: 200 * time.Millisecond, Backoff: LinearBackoff(20 * time.Millisecond)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runner.Run(ctx)

	<-fired
	time.Sleep(100 * time.Millisecond) // let the event-triggered refetch land

	before := store.Current()
	if before.Domain != "z-backv-v1.local" {
		t.Fatalf("expected unchanged domain after hash-identical refetch, got %q", before.Domain)
	}
	// Same content-hash bundle body never changed on the server: the
	// applied bundle pointer must be the exact one boot loaded (no swap
	// occurred), which we verify indirectly via ApplyIfChanged directly.
	applied, err := store.ApplyIfChanged([]byte(v1))
	if err != nil {
		t.Fatalf("ApplyIfChanged: %v", err)
	}
	if applied {
		t.Error("expected applied=false for a hash-identical bundle")
	}
}

func TestRunner_BadProtocolVersionKeepsServingOld(t *testing.T) {
	v1 := bundleJSON("z-backv-v1.local", "hash-v1")
	bad := `{"protocolVersion":2,"domain":"z-backv-bad.local","contentHash":"hash-bad","records":[]}`

	ts := newTestServer(v1)
	defer ts.close()

	fired := make(chan struct{}, 1)
	ts.setEventsHandler(func(w http.ResponseWriter, r *http.Request) {
		fw := w.(flushWriter)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: bundle-updated\ndata: {}\n\n"))
		fw.Flush()
		select {
		case fired <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	})

	store := bootStoreForTest(t, ts.srv.URL, v1)
	client := NewClient(ts.srv.URL, "z-01", "tok")
	runner := &Runner{Client: client, Store: store, IdleTimeout: 2 * time.Second, PollInterval: 200 * time.Millisecond, Backoff: LinearBackoff(20 * time.Millisecond)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runner.Run(ctx)

	<-fired
	ts.setBundle(bad)
	// give the event-triggered refetch time to see the bad bundle and reject it
	time.Sleep(150 * time.Millisecond)

	if store.Current().Domain != "z-backv-v1.local" {
		t.Errorf("bad protocolVersion push must keep serving the previous bundle, got domain=%q", store.Current().Domain)
	}
}

// bootStoreForTest boots a Store via BootFromFetch against the given
// server, exactly like main.go's zeus-fetch boot path.
func bootStoreForTest(t *testing.T, baseURL, initialBundle string) *bundle.Store {
	t.Helper()
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	client := NewClient(baseURL, "z-01", "tok")
	s, err := bundle.BootFromFetch(context.Background(), client.FetchBundle, 1, LinearBackoff(10*time.Millisecond), cacheDir)
	if err != nil {
		t.Fatalf("BootFromFetch: %v", err)
	}
	return s
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}
