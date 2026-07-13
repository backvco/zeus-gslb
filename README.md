# zeus-gslb

The Zeus global-DNS responder/prober — plan `.plans/global-dns-failover.md`
(§8 internal plane, §9 external plane, §18.6 decision). A single static Go
binary, built on [`miekg/dns`](https://github.com/miekg/dns), that answers
health-aware DNS for a Zeus instance's global zone (`z-<instanceName>.local`)
and — in later phases — probes external endpoints for public failover DNS.

This is a standalone repo, mirroring
[`zeus-agent-go`](https://github.com/backvco/zeus-agent-go)'s repo shape and
release conventions (see plan §18.6: "New repo `zeus-gslb-go` (mirror the
agent's release flow)").

## Three planned modes, one binary

Per plan §8's "one agent, two modes" rationale (extended to three across the
full plan):

| Mode | Status | What it does |
|------|--------|---------------|
| `responder` | **shipped (v0, this repo)** | Runs as a second container in the `zeus-overlay-dns` pod. Answers the global zone's A/AAAA queries straight out of an in-memory decision bundle (§8). No recursion, no upstream forwarding, no zeus-API fetching in v0 — the bundle is loaded from a local file (`--bundle`). |
| `fleet` | planned (§9, external plane) | The zeus-operated multi-region prober fleet (§18.6 decision 8) that closes the public-path blind spot: checks public entry points from multiple vantage regions, reports signed verdicts back to Zeus. |
| `authoritative` | planned (§9) | Serves the public failover DNS zone directly with control-plane-view health, ahead of the fleet's external-vantage data landing (§18.6 decision 5: "P3→P5 gap accepted"). |

v0 wires only `--mode=responder`; the flag exists so a future binary version
can dispatch to `fleet`/`authoritative` without a rename.

## Bundle contract (MUST match the Zeus-side producer exactly)

The bundle is the sole input to `responder` mode — a JSON document Zeus
compiles per-cluster and (in P1) will push over the network; v0 reads it
from `--bundle <path>` only. The authoritative shape is defined by:

- `zeus/src/lib/server/global-dns/bundle.js` (`compileBundle()`)
- `zeus/tests/global-dns/bundle.test.js` (locked snapshot of the wire shape)

```json
{
  "protocolVersion": 1,
  "domain": "z-backv.local",
  "generatedAt": "2026-07-12T00:00:00.000Z",
  "contentHash": "sha256 hex of the records array (informational)",
  "records": [
    {
      "fqdn": "mysql-01-rw.prod.app1.z-backv.local",
      "ttl": 7,
      "state": "ok | degraded | failed-closed",
      "answers": [
        { "ip": "10.0.5.5", "port": 443 }
      ]
    }
  ]
}
```

- `answers` holds **up to 3** ready pod IPs (`MAX_ANSWERS_PER_TARGET` in
  bundle.js) — pod IPs, never ClusterIPs (§8 P0 spike: cross-cluster
  ClusterIP routing is asymmetrically blackholed; pod CIDRs are the
  always-routable invariant).
- `ttl` is per-record (`record.health.ttl`, 1–60s, falls back to a 5s
  default) — never a global TTL.
- `contentHash` is a sha256 hex digest for human/audit diffing; the
  responder does not recompute or verify it (no shared secret with the
  producer beyond the fetch-time bearer token, out of scope while v0 is
  file-based).

### Contract mismatch found vs. the task description

The task description states record `state` is one of `'ok'|'degraded'|
'failed-closed'`. Reading the actual selection core
(`zeus/src/lib/server/global-dns/select.js`), there is a **fourth** state,
`'no-targets'` ("record has no targets/replication binding to select over at
all"), and `compileBundle()` does **not** filter it out before it reaches the
bundle — `bundle.test.js` just never happens to exercise a record that hits
it. Plan §8 only documents responder behavior for `ok | degraded |
failed-closed`. This responder treats `no-targets` (and any other
unrecognized state) as fail-closed → SERVFAIL, the same as `failed-closed`,
rather than crashing or silently answering NOERROR-no-data for a state it
doesn't understand — see `internal/bundle/bundle.go`'s doc comment and
`internal/dnsserver/handler.go`'s `answerA`.

## DNS behavior (v0, responder mode)

| Query | Behavior |
|-------|----------|
| `A` for a known fqdn, state `ok`/`degraded` | NOERROR, all answer IPs as `A` records, TTL = the record's `ttl` |
| `A` for a known fqdn, state `failed-closed` (or `no-targets`/unrecognized) | SERVFAIL |
| `A`/`AAAA`/other for an unknown name under the zone | NXDOMAIN, synthesized SOA in the authority section, negative TTL 5 |
| `AAAA` for **any** known name (regardless of state) | clean NOERROR, empty answer section — required for musl/Go resolvers that misbehave on anything else (§8) |
| any query type outside the zone | REFUSED |
| — | recursion is never offered (`RA` unset in every response) |

Bad bundles (wrong `protocolVersion`, malformed JSON, missing required
fields) are rejected and logged; the responder keeps serving whatever it had
loaded before — a bad bundle must never brick answering (§8 "protocol
version skew").

## Boot + hot-reload

1. Try to load `--bundle`. If present and valid, serve it and persist a copy
   to `--cache-dir` (atomic write: temp file + `fsync` + `rename`).
2. If `--bundle` is missing or invalid, fall back to the last-good bundle
   persisted in `--cache-dir`. Only if **both** are unusable does the process
   fail to start — there is nothing safe to answer with.
3. While running, `--bundle`'s mtime is polled every 1s (v0: no fsnotify
   dependency for a scaffold). A changed, valid bundle is hot-reloaded and
   re-persisted to the cache; a changed-but-invalid bundle is logged and
   ignored, keeping the previous bundle live.

P1 replaces step 3's file poll with the real Zeus push (SSE with idle-timeout
+ reconnect, per §8) — the `bundle.Store` boundary is deliberately narrow so
that swap doesn't touch `internal/dnsserver` at all.

## Flags

```
zeus-gslb --mode=responder --listen=:5355 --bundle=/path/bundle.json --cache-dir=/var/lib/zeus-gslb
```

| Flag | Default | Meaning |
|------|---------|---------|
| `--mode` | `responder` | Operating mode. v0 accepts only `responder`; other values fail fast. |
| `--listen` | `:5355` | UDP **and** TCP bind address for DNS. |
| `--bundle` | *(none)* | Path to the decision bundle JSON file. If unset, boots from `--cache-dir` only (no fallback if the cache is also empty). |
| `--cache-dir` | *(required)* | Directory to persist/read the last-good bundle (`<dir>/bundle.json`). |
| `--version` | — | Print version and exit. |

## Build

```sh
make build            # host platform -> dist/zeus-gslb
make release          # linux amd64 + arm64 -> dist/
make docker           # multi-stage image, CGO_ENABLED=0, distroless/static base
make test             # go test ./...
```

## Layout

```
cmd/zeus-gslb/          entrypoint: flag parsing, boot, signal handling
internal/bundle/        bundle struct, parse/validate, atomic cache, hot-reload Store
internal/dnsserver/     miekg/dns handler + UDP/TCP server
fixtures/bundle.json    sample bundle for manual smoke-testing (`dig @127.0.0.1 -p 5355 ...`)
```

## Manual smoke test

```sh
make build
./dist/zeus-gslb --mode=responder --listen=:5355 \
  --bundle=fixtures/bundle.json --cache-dir=/tmp/zeus-gslb-cache &

dig @127.0.0.1 -p 5355 mysql-01-rw.prod.app1.z-backv.local A +short
dig @127.0.0.1 -p 5355 shared-cache.app1.z-backv.local A +short
dig @127.0.0.1 -p 5355 down-service.app1.z-backv.local A        # expect SERVFAIL
dig @127.0.0.1 -p 5355 mysql-01-rw.prod.app1.z-backv.local AAAA # expect NOERROR, no answers
dig @127.0.0.1 -p 5355 nope.app1.z-backv.local A                # expect NXDOMAIN
dig @127.0.0.1 -p 5355 example.com A                            # expect REFUSED
```
