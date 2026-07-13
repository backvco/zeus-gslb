// Command zeus-gslb is the Zeus global-DNS responder (plan
// .plans/global-dns-failover.md §8, §18.6). v0 ships responder mode only —
// it answers A/AAAA queries for one global zone straight out of an
// in-memory decision bundle loaded from a local file. It never fetches from
// the Zeus API itself (P1 work) and never forwards/recurses.
//
// Two later modes are planned but not implemented here (see README):
// fleet (external multi-region prober) and authoritative (public failover
// DNS). Both share this binary's name per plan §8's "one agent, two modes"
// rationale; v0 only wires --mode=responder.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/backvco/zeus-gslb/internal/bundle"
	"github.com/backvco/zeus-gslb/internal/checks"
	"github.com/backvco/zeus-gslb/internal/dnsserver"
	"github.com/backvco/zeus-gslb/internal/remote"
)

// needsK8sProbe reports whether any record in the given bundle references a
// localCheck with probe=="k8s" — the trigger for lazily starting the
// in-cluster EndpointSlice lister. Starting that lister unconditionally at
// boot (the previous behavior) meant every responder tried to talk to the
// k8s API even when nothing in its bundle used a k8s probe — pure overhead
// (and a startup failure mode) for the common tcp/http-only case.
func needsK8sProbe(records []bundle.Record) bool {
	for _, rec := range records {
		for _, lc := range rec.LocalChecks {
			if lc.Probe == "k8s" {
				return true
			}
		}
	}
	return false
}

// zeus-fetch mode tuning (plan §8/§10 + task spec — exact numbers pinned
// here so the boot/subscribe/idle-timeout/poll behavior is one obvious
// place to read, not scattered constants).
const (
	bootFetchRetries     = 3
	bootFetchBackoffStep = 2 * time.Second
	subscribeIdleTimeout = 45 * time.Second
	pollFallbackInterval = 5 * time.Second
	reconnectBackoffBase = 1 * time.Second
	reconnectBackoffCeil = 30 * time.Second

	// Local health-check subsystem tuning (plan §7, P2 contract).
	checksBaseTick        = 250 * time.Millisecond // scheduling granularity, well under the Fast preset's 1s intervalMs
	healthHeartbeat       = 30 * time.Second       // P2 contract: "heartbeat every 30s"
	healthReportRetryBase = 1 * time.Second
	healthReportRetryCeil = 30 * time.Second
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

// machineAdapter satisfies dnsserver.LocalHealth over a *checks.Machine.
// checks.Machine's State returns the distinct checks.Health type (not a
// bare string), so it doesn't structurally satisfy dnsserver.LocalHealth on
// its own — this is the one-line bridge between the two packages' health
// vocabularies (both use the same underlying string values by convention:
// "healthy"|"unhealthy"|"unknown").
type machineAdapter struct{ m *checks.Machine }

func (a machineAdapter) State(key string) string { return string(a.m.State(key)) }
func (a machineAdapter) Ready(key string) bool   { return a.m.Ready(key) }

func main() {
	log.SetFlags(0)

	mode := flag.String("mode", "responder", "operating mode (v0 supports only: responder)")
	listen := flag.String("listen", ":5355", "UDP+TCP listen address for DNS")
	bundlePath := flag.String("bundle", "", "path to the decision bundle JSON file (file mode; ignored if --zeus-url is set)")
	cacheDir := flag.String("cache-dir", "", "directory to persist the last-good bundle for boot fallback")
	showVersion := flag.Bool("version", false, "print version and exit")

	// zeus-fetch mode flags. All optional — when --zeus-url is absent, the
	// existing --bundle file mode is unchanged (task spec point 1).
	zeusURL := flag.String("zeus-url", "", "zeus API base URL (e.g. https://api.cloud-dev.zeusk8s.com) — enables zeus-fetch mode: boot fetch + SSE subscribe instead of --bundle file mode")
	cluster := flag.String("cluster", "", "cluster name to fetch/subscribe as (required with --zeus-url)")
	token := flag.String("token", "", "bearer token for zeus-fetch mode (prefer the ZEUS_GSLB_TOKEN env var instead so the token isn't visible in argv/ps)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("zeus-gslb %s\n", version)
		return
	}

	if *mode != "responder" {
		log.Fatalf("unsupported --mode %q (v0 supports only: responder)", *mode)
	}
	if *cacheDir == "" {
		log.Fatalf("--cache-dir is required")
	}

	zeusFetchMode := *zeusURL != ""

	bearerToken := *token
	if envToken := os.Getenv("ZEUS_GSLB_TOKEN"); envToken != "" {
		bearerToken = envToken
	}

	if zeusFetchMode {
		if *cluster == "" {
			log.Fatalf("--cluster is required with --zeus-url")
		}
		if bearerToken == "" {
			log.Fatalf("--token (or ZEUS_GSLB_TOKEN env) is required with --zeus-url")
		}
	} else if *bundlePath == "" {
		log.Printf("warning: --bundle not set — will boot from --cache-dir %s only (no fallback if cache is also empty)", *cacheDir)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var store *bundle.Store
	var err error
	var client *remote.Client
	machine := checks.NewMachine()

	if zeusFetchMode {
		client = remote.NewClient(*zeusURL, *cluster, bearerToken)
		store, err = bundle.BootFromFetch(ctx, client.FetchBundle, bootFetchRetries, remote.LinearBackoff(bootFetchBackoffStep), *cacheDir)
	} else {
		store, err = bundle.NewStore(*bundlePath, *cacheDir)
	}
	if err != nil {
		log.Fatalf("boot: %v", err)
	}

	handler := dnsserver.New(store, machineAdapter{machine})
	srv := dnsserver.NewServer(*listen, handler)

	if zeusFetchMode {
		runner := &remote.Runner{
			Client:       client,
			Store:        store,
			IdleTimeout:  subscribeIdleTimeout,
			PollInterval: pollFallbackInterval,
			Backoff:      remote.ExponentialBackoff(reconnectBackoffBase, reconnectBackoffCeil),
		}
		go runner.Run(ctx)

		// Local health checks (plan §7 / P2 contract "Local checks"): probe
		// this cluster's own registered targets, dedup'd by targetKey, and
		// report transitions + 30s heartbeats to zeus. The k8s probe type
		// needs an in-cluster EndpointSlice lister — started LAZILY, only on
		// the first bundle whose localChecks actually reference a k8s probe
		// (needsK8sProbe above), not unconditionally at boot: most bundles
		// are tcp/http-only, and standing up an in-cluster k8s client for
		// nothing is pure overhead (and a needless startup failure mode when
		// running outside a cluster in dev). Degrade (log + skip k8s-probed
		// targets) rather than fail boot if the lister can't be started.
		var (
			k8sMu         sync.Mutex
			k8sLister     checks.EndpointSliceLister
			k8sSkipLogged bool
		)
		ensureK8sLister := func(records []bundle.Record) checks.EndpointSliceLister {
			k8sMu.Lock()
			defer k8sMu.Unlock()
			if k8sLister != nil {
				return k8sLister
			}
			if !needsK8sProbe(records) {
				if !k8sSkipLogged {
					log.Printf("checks: no k8s-probe localChecks in current bundle — skipping EndpointSlice lister start")
					k8sSkipLogged = true
				}
				return nil
			}
			l, kerr := checks.NewK8sLister(ctx)
			if kerr != nil {
				log.Printf("checks: k8s EndpointSlice lister unavailable (k8s-probe targets will be skipped): %v", kerr)
				return nil
			}
			log.Printf("checks: bundle references a k8s-probe localCheck — starting EndpointSlice lister")
			k8sLister = l
			return l
		}

		checksRunner := &checks.Runner{
			Targets: func() map[string]checks.Target {
				records := store.Current().Records
				return checks.BuildTargets(records, ensureK8sLister(records))
			},
			Machine: machine,
		}

		reporter := &remote.HealthReporter{
			Client:            client,
			States:            machine,
			HeartbeatInterval: healthHeartbeat,
			Backoff:           remote.ExponentialBackoff(healthReportRetryBase, healthReportRetryCeil),
		}
		checksRunner.OnTransition = func(targetKey string, health checks.Health, at time.Time) {
			reporter.NotifyTransition(targetKey, string(health), at)
		}

		go checksRunner.Run(ctx, checksBaseTick)
		go reporter.Run(ctx)

		log.Printf("zeus-gslb %s responder listening on %s (udp+tcp), zeus-url=%s cluster=%s cache-dir=%s domain=%s",
			version, *listen, *zeusURL, *cluster, *cacheDir, store.Current().Domain)
	} else {
		go store.WatchAndReload(ctx)
		log.Printf("zeus-gslb %s responder listening on %s (udp+tcp), bundle=%s cache-dir=%s domain=%s",
			version, *listen, *bundlePath, *cacheDir, store.Current().Domain)
	}

	if err := srv.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("server stopped: %v", err)
	}
	os.Exit(0)
}
