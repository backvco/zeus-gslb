# zeus-gslb

The DNS responder behind [Zeus](https://zeusk8s.com) **global endpoints**: a small,
static Go binary that gives a service or database one stable name across every cluster
in a Zeus mesh, and moves that name — in seconds — when the backing pods, or the
writable database primary, move.

```
mysql-01-rw.prod.app1.z-acme.local   ->  whichever cluster currently holds the primary
api.prod.app1.z-acme.local           ->  the caller's own cluster, failing over if it dies
```

It runs as a sidecar in the `zeus-overlay-dns` pod on each mesh-enrolled cluster. It is
deliberately dumb: **Zeus decides, this answers.** Zeus compiles a per-cluster *decision
bundle* (candidate order, addresses, health) and pushes it; the responder serves it from
memory and runs its own fast local health checks against that cluster's backends.

## Why a separate DNS server, rather than rendering answers into CoreDNS

A ConfigMap-mounted Corefile takes 10–90s to reach the pod (the kubelet sync cycle)
before CoreDNS even reloads it. That makes seconds-class failover impossible by
construction. Answers have to live in memory and arrive by push — so they do.

Stock CoreDNS is left completely alone: it gets one static `forward` for the global zone
and nothing else.

## What it does when things go wrong

This is the part worth reading if you are deciding whether to run it.

- **A candidate with no addresses is never served.** Answering `NOERROR` with an empty
  answer set during an outage is *worse* than failing: resolvers cache "this name has no
  address" and clients stop retrying. Health is not the same as being able to serve.
- **Fail-closed is `SERVFAIL`, never `NXDOMAIN`.** SERVFAIL is a retryable "can't answer
  right now"; NXDOMAIN is "this name does not exist", which resolvers cache hard and
  would delay recovery long after the backends came back.
- **An unrecognised record state is `SERVFAIL`.** If a future Zeus pushes a bundle this
  binary doesn't fully understand, it fails loud rather than answering wrong.
- **`AAAA` for a known name returns a clean `NOERROR` with no records** — musl-based
  images (Alpine) misbehave otherwise.
- **It survives losing the control plane.** The last-good bundle is cached to disk, so a
  restart during a Zeus outage still serves, and local failover keeps working with no
  control plane reachable at all.
- **It only asks for the Kubernetes permissions it actually needs**: the EndpointSlice
  watcher starts only if a record in the bundle uses a `k8s` probe.

It never recurses, never forwards, and answers only inside its own zone (anything else
is `REFUSED`).

## Bundle contract

Zeus serves the bundle at `GET /api/global-endpoints/bundle?cluster=<name>` with a
per-cluster bearer token, and pushes a notification on `…/events` when it changes.

```jsonc
{
  "protocolVersion": 1,
  "domain": "z-acme.local",
  "contentHash": "…",
  "records": [{
    "fqdn": "api.prod.app1.z-acme.local",
    "ttl": 5,
    "state": "ok",            // ok | degraded | failed-closed
    "mode": "single",         // single = first viable candidate; all = union
    "candidates": [{          // in Zeus's policy order for THIS cluster
      "cluster": "z-01",
      "targetKey": "app1/z-01/prod/api:80",
      "health": "healthy",    // Zeus's cross-cluster view
      "answers": [{"ip": "10.244.0.15", "port": 80}]
    }],
    "localChecks": [{         // only for targets in THIS cluster
      "targetKey": "app1/z-01/prod/api:80",
      "probe": "tcp",         // tcp | http | k8s
      "host": "api.prod.svc.cluster.local", "port": 80,
      "intervalMs": 1000, "failureThreshold": 3, "recoveryThreshold": 2
    }]
  }]
}
```

A candidate is viable when its health is not `unhealthy` **and** it has at least one
address. Local checks are authoritative for local targets (nothing else can see them at
1s cadence); remote health arrives from Zeus.

## Running it

```bash
zeus-gslb --mode=responder --listen=:5355 \
          --zeus-url=https://zeus.example.com --cluster=z-01 \
          --cache-dir=/cache            # token via ZEUS_GSLB_TOKEN
```

Or fully offline, from a file:

```bash
zeus-gslb --mode=responder --bundle=./fixtures/bundle.json
```

Zeus deploys and configures this for you; the flags are documented because you should be
able to run it yourself and see what it does.

## Modes

| Mode | Status | What it does |
|------|--------|---------------|
| `responder` | **shipped** | Answers a Zeus mesh's internal global zone (above). |
| `fleet` | planned | Zeus-operated multi-region probers that check *public* endpoints from real internet vantage points and report signed verdicts back. |
| `authoritative` | planned | Serves a delegated public GSLB subzone directly, for latency- and health-aware public DNS. |

One binary, three modes: the health pipeline and bundle plumbing are the same in all of
them.

## Build

```bash
make build        # host binary into dist/
make test         # go test ./...
docker build -t zeus-gslb .
```

Static (`CGO_ENABLED=0`), distroless image, linux/amd64 + linux/arm64.

## License

Apache-2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
