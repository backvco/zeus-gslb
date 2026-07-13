package checks

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// probeTimeout is the fixed dial/request timeout for tcp and http probes
// (P2 contract: "tcp = dial host:port timeout 500ms. http = GET
// host:port/path timeout 500ms, 2xx-3xx = up").
const probeTimeout = 500 * time.Millisecond

// Prober performs one check attempt and reports up (true) or down (false).
// It never returns an error to the caller — a probe that can't determine
// "up" (dial failure, timeout, non-2xx/3xx, transport error) simply reports
// down; the state machine's thresholds are what turn transient failures
// into an Unhealthy transition, not the prober itself.
type Prober interface {
	Probe(ctx context.Context) bool
}

// TCPProber dials Addr ("host:port") and reports up iff the connect
// succeeds within probeTimeout.
type TCPProber struct {
	Addr string
}

func (p TCPProber) Probe(ctx context.Context) bool {
	dialCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", p.Addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// httpProbeClient is a single shared client for all HTTPProbers — no
// connection reuse benefit worth chasing at this scale (per-tuple probes,
// §7), but avoids constructing a client per probe attempt.
var httpProbeClient = &http.Client{
	Timeout: probeTimeout,
	// Never follow redirects for a health check — a redirect chain landing
	// on 2xx elsewhere is not "this endpoint is up".
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// HTTPProber performs a GET against URL and reports up iff the response
// status is 2xx or 3xx (P2 contract: "2xx-3xx = up").
type HTTPProber struct {
	URL string
}

func (p HTTPProber) Probe(ctx context.Context) bool {
	reqCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, p.URL, nil)
	if err != nil {
		return false
	}
	resp, err := httpProbeClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

// EndpointSliceLister reports the number of ready endpoint addresses for a
// Service's EndpointSlices — the abstraction K8sProber depends on so this
// package's probe logic is testable without a real (or fake) client-go
// informer. The client-go-backed implementation lives in k8s.go.
type EndpointSliceLister interface {
	ReadyCount(namespace, service string) int
}

// K8sProber reports up iff the target Service has at least one ready
// endpoint address (P2 contract: "k8s = EndpointSlice watch ... ready-
// addresses>0 = up"). This is the default/required probe for UDP services
// (plan §7) and a legitimate TCP alternative that generates zero synthetic
// traffic.
type K8sProber struct {
	Lister    EndpointSliceLister
	Namespace string
	Service   string
}

func (p K8sProber) Probe(ctx context.Context) bool {
	if p.Lister == nil {
		return false
	}
	return p.Lister.ReadyCount(p.Namespace, p.Service) > 0
}

// NewProber builds the Prober for one LocalCheckConfig. probe must be one
// of "tcp", "http", "k8s" — an unrecognized probe type is a producer/
// responder contract mismatch, so NewProber returns an error rather than
// silently defaulting to a probe type that wasn't asked for.
func NewProber(cfg LocalCheckConfig, k8sLister EndpointSliceLister) (Prober, error) {
	switch cfg.Probe {
	case "tcp":
		return TCPProber{Addr: fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)}, nil
	case "http":
		return HTTPProber{URL: fmt.Sprintf("http://%s:%d%s", cfg.Host, cfg.Port, cfg.Path)}, nil
	case "k8s":
		return K8sProber{Lister: k8sLister, Namespace: cfg.Namespace, Service: cfg.Service}, nil
	default:
		return nil, fmt.Errorf("checks: unrecognized probe type %q for targetKey %q", cfg.Probe, cfg.TargetKey)
	}
}
