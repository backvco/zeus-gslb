package checks

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTCPProber_UpAndDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	up := TCPProber{Addr: ln.Addr().String()}
	if !up.Probe(context.Background()) {
		t.Error("expected up=true against a listening port")
	}

	// Grab a port and immediately close the listener so nothing answers.
	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := deadLn.Addr().String()
	deadLn.Close()

	down := TCPProber{Addr: addr}
	if down.Probe(context.Background()) {
		t.Error("expected up=false against a closed port")
	}
}

func TestHTTPProber_StatusRanges(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(http.StatusOK)
		case "/redirect":
			w.WriteHeader(http.StatusFound)
		case "/error":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cases := []struct {
		path string
		want bool
	}{
		{"/ok", true},
		{"/redirect", true},
		{"/error", false},
		{"/missing", false}, // 404
	}
	for _, c := range cases {
		p := HTTPProber{URL: srv.URL + c.path}
		if got := p.Probe(context.Background()); got != c.want {
			t.Errorf("path %s: Probe() = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestHTTPProber_UnreachableIsDown(t *testing.T) {
	p := HTTPProber{URL: "http://127.0.0.1:1/nope"}
	if p.Probe(context.Background()) {
		t.Error("expected up=false for an unreachable host")
	}
}

// fakeLister is a hand-rolled EndpointSliceLister for K8sProber tests —
// verifies K8sProber's own up/down mapping without any client-go
// dependency (the client-go-backed lister itself is tested in k8s_test.go).
type fakeLister struct {
	counts map[string]int // "ns/svc" -> ready count
}

func (f fakeLister) ReadyCount(namespace, service string) int {
	return f.counts[namespace+"/"+service]
}

func TestK8sProber_ReadyCountGatesUpDown(t *testing.T) {
	lister := fakeLister{counts: map[string]int{"prod/api": 2, "prod/empty": 0}}

	up := K8sProber{Lister: lister, Namespace: "prod", Service: "api"}
	if !up.Probe(context.Background()) {
		t.Error("expected up=true when ReadyCount > 0")
	}

	down := K8sProber{Lister: lister, Namespace: "prod", Service: "empty"}
	if down.Probe(context.Background()) {
		t.Error("expected up=false when ReadyCount == 0")
	}

	unknownSvc := K8sProber{Lister: lister, Namespace: "prod", Service: "nope"}
	if unknownSvc.Probe(context.Background()) {
		t.Error("expected up=false for a service missing from the lister")
	}
}

func TestK8sProber_NilListerIsDown(t *testing.T) {
	p := K8sProber{Namespace: "prod", Service: "api"}
	if p.Probe(context.Background()) {
		t.Error("expected up=false with a nil lister rather than a panic")
	}
}

func TestNewProber_UnrecognizedProbeTypeErrors(t *testing.T) {
	_, err := NewProber(LocalCheckConfig{TargetKey: "x", Probe: "sip-options"}, nil)
	if err == nil {
		t.Fatal("expected an error for an unrecognized probe type")
	}
}

func TestNewProber_BuildsExpectedKinds(t *testing.T) {
	tcp, err := NewProber(LocalCheckConfig{Probe: "tcp", Host: "h", Port: 1}, nil)
	if err != nil || tcp == nil {
		t.Fatalf("tcp: %v", err)
	}
	if _, ok := tcp.(TCPProber); !ok {
		t.Errorf("tcp: got %T, want TCPProber", tcp)
	}

	httpP, err := NewProber(LocalCheckConfig{Probe: "http", Host: "h", Port: 1, Path: "/healthz"}, nil)
	if err != nil || httpP == nil {
		t.Fatalf("http: %v", err)
	}
	if _, ok := httpP.(HTTPProber); !ok {
		t.Errorf("http: got %T, want HTTPProber", httpP)
	}

	k8sP, err := NewProber(LocalCheckConfig{Probe: "k8s", Namespace: "ns", Service: "svc"}, fakeLister{})
	if err != nil || k8sP == nil {
		t.Fatalf("k8s: %v", err)
	}
	if _, ok := k8sP.(K8sProber); !ok {
		t.Errorf("k8s: got %T, want K8sProber", k8sP)
	}
}
