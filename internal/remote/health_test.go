package remote

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReportHealth_SendsExactContractShape(t *testing.T) {
	var gotBody []byte
	var gotAuth, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "z-01", "tok-123")
	report := HealthReport{
		Cluster:         "z-01",
		ProtocolVersion: 1,
		Transitions: []Transition{
			{TargetKey: "app1/z-01/prod/api:443", State: "unhealthy", At: "2026-07-12T00:00:00Z"},
		},
		States: map[string]string{"app1/z-01/prod/api:443": "unhealthy"},
	}
	if err := c.ReportHealth(context.Background(), report); err != nil {
		t.Fatalf("ReportHealth: %v", err)
	}

	if gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q", gotContentType)
	}

	var decoded map[string]any
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("decode posted body: %v", err)
	}
	if decoded["cluster"] != "z-01" {
		t.Errorf("cluster = %v", decoded["cluster"])
	}
	if decoded["protocolVersion"].(float64) != 1 {
		t.Errorf("protocolVersion = %v", decoded["protocolVersion"])
	}
	transitions := decoded["transitions"].([]any)
	if len(transitions) != 1 {
		t.Fatalf("transitions len = %d, want 1", len(transitions))
	}
	states := decoded["states"].(map[string]any)
	if states["app1/z-01/prod/api:443"] != "unhealthy" {
		t.Errorf("states = %v", states)
	}
}

func TestReportHealth_NonTwoXXIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "z-01", "tok")
	err := c.ReportHealth(context.Background(), HealthReport{Cluster: "z-01", States: map[string]string{}})
	if err == nil {
		t.Fatal("expected an error for a 500 response")
	}
}
