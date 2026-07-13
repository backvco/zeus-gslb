package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchBundle_SendsExactRequestContract(t *testing.T) {
	var gotMethod, gotPath, gotQuery, gotAuth, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("cluster")
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"protocolVersion":1,"domain":"z-backv.local","records":[]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "z-01", "secrettoken")
	body, err := c.FetchBundle(context.Background())
	if err != nil {
		t.Fatalf("FetchBundle: %v", err)
	}

	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/api/global-endpoints/bundle" {
		t.Errorf("path = %q, want /api/global-endpoints/bundle", gotPath)
	}
	if gotQuery != "z-01" {
		t.Errorf("cluster query = %q, want z-01", gotQuery)
	}
	if gotAuth != "Bearer secrettoken" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer secrettoken")
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q", gotAccept)
	}
	if len(body) == 0 {
		t.Error("expected non-empty body")
	}
}

func TestFetchBundle_NonOKStatusIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "z-01", "tok")
	if _, err := c.FetchBundle(context.Background()); err == nil {
		t.Fatal("expected error on non-200 status")
	}
}

func TestEventsURL_UsesEventsPathAndClusterQuery(t *testing.T) {
	c := NewClient("https://api.example.com/", "z-02", "tok")
	if got, want := c.eventsURL(), "https://api.example.com/api/global-endpoints/events?cluster=z-02"; got != want {
		t.Errorf("eventsURL() = %q, want %q", got, want)
	}
	if got, want := c.bundleURL(), "https://api.example.com/api/global-endpoints/bundle?cluster=z-02"; got != want {
		t.Errorf("bundleURL() = %q, want %q", got, want)
	}
}
