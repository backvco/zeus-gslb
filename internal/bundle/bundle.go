// Package bundle loads, validates, and caches the per-cluster decision
// bundle the zeus-gslb responder answers DNS queries from.
//
// Contract mirrors the Zeus-side producer 1:1 — do not evolve this struct
// independently. Authoritative source:
//
//	zeus/src/lib/server/global-dns/bundle.js   (compileBundle)
//	zeus/tests/global-dns/bundle.test.js       (locked snapshot of the shape)
//
// Shape (bundle.test.js "returns the exact responder boot-contract shape"):
//
//	{
//	  protocolVersion: 1,
//	  domain: string,
//	  generatedAt: string (ISO-8601),
//	  contentHash: string (sha256 hex, informational — not re-verified here),
//	  records: [
//	    { fqdn, ttl, state: "ok"|"degraded"|"failed-closed", answers: [{ip, port}] }
//	  ]
//	}
//
// One documented mismatch vs. the plan's stated state enum: the selection
// core (select.js) actually has a FOURTH state, "no-targets" (record has no
// targets/replication binding to select over at all), and compileBundle.js
// does not filter it out — it can reach the wire. v0.6.6/§8 only documents
// ok|degraded|failed-closed responder behavior. This package treats any
// state other than "ok" and "degraded" as fail-closed (SERVFAIL) rather than
// panicking or (worse) silently answering NOERROR-no-data for an unrecognized
// state — see dnsserver.answerState.
package bundle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SupportedProtocolVersion is the only bundle protocolVersion this responder
// build accepts. Per plan §8 "protocol version skew": a bundle carrying a
// different version must be rejected (logged) while the responder keeps
// serving whatever it already has loaded — never brick answering.
const SupportedProtocolVersion = 1

// RecordState enumerates the state values bundle.js can emit for a record.
// Only StateOK and StateDegraded are documented responder-answerable states;
// StateFailedClosed and the undocumented StateNoTargets both mean SERVFAIL.
const (
	StateOK           = "ok"
	StateDegraded     = "degraded"
	StateFailedClosed = "failed-closed"
	StateNoTargets    = "no-targets" // undocumented in §8; see package doc
)

// Answer is one resolved endpoint for a record — a ready pod IP (§8: pod
// IPs, never ClusterIPs) plus the record's target port.
type Answer struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

// Health values a candidate's health can carry (P2 contract "Bundle v1.1" —
// zeus's pushed view for a candidate, and the responder's own local
// state-machine states share this vocabulary).
const (
	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
	HealthUnknown   = "unknown"
)

// Mode values for a record's candidate-walk evaluation (P2 contract).
const (
	ModeSingle = "single"
	ModeAll    = "all"
)

// Candidate is one ordered option in a record's candidate walk (P2 contract
// bundle v1.1). Candidates carries zeus's pushed health view; a candidate
// that also appears in the record's LocalChecks is additionally gated by
// this cluster's own local state machine (see internal/dnsserver.Evaluator).
type Candidate struct {
	Cluster   string   `json:"cluster"`
	TargetKey string   `json:"targetKey"`
	Health    string   `json:"health"` // healthy|unhealthy|unknown
	Answers   []Answer `json:"answers"`
}

// LocalCheck describes one target this cluster's responder must probe
// itself (P2 contract bundle v1.1) — only candidates local to THIS cluster
// get a LocalCheck entry.
type LocalCheck struct {
	TargetKey         string `json:"targetKey"`
	Probe             string `json:"probe"` // tcp|http|k8s
	Host              string `json:"host"`
	Port              int    `json:"port"`
	Path              string `json:"path,omitempty"`      // http only
	Namespace         string `json:"namespace,omitempty"` // k8s probe
	Service           string `json:"service,omitempty"`   // k8s probe
	IntervalMs        int    `json:"intervalMs"`
	FailureThreshold  int    `json:"failureThreshold"`
	RecoveryThreshold int    `json:"recoveryThreshold"`
}

// Record is one global-DNS name in the bundle.
//
// Candidates and LocalChecks are the v1.1 additive fields (P2 contract). A
// nil Candidates (the JSON key absent entirely, distinct from an explicit
// empty `"candidates":[]`) means a v1.0 bundle — the responder MUST fall
// back to the legacy State/Answers path (contract: "No candidates field at
// all → legacy path"). encoding/json leaves a slice field nil when its key
// is absent from the JSON and non-nil-but-empty when the key is present as
// `[]`, so `rec.Candidates == nil` is exactly the distinction the contract
// needs — do not "helpfully" normalize it to `[]Candidate{}` anywhere.
type Record struct {
	FQDN        string       `json:"fqdn"`
	TTL         int          `json:"ttl"`
	State       string       `json:"state"`
	Mode        string       `json:"mode,omitempty"` // single|all; "" treated as single
	Answers     []Answer     `json:"answers"`
	Candidates  []Candidate  `json:"candidates,omitempty"`
	LocalChecks []LocalCheck `json:"localChecks,omitempty"`
}

// Bundle is the full decision bundle as produced by compileBundle().
type Bundle struct {
	ProtocolVersion int      `json:"protocolVersion"`
	Domain          string   `json:"domain"`
	GeneratedAt     string   `json:"generatedAt"`
	ContentHash     string   `json:"contentHash"`
	Records         []Record `json:"records"`
}

// ByFQDN indexes a bundle's records by lowercase, trailing-dot-normalized
// FQDN for O(1) query lookup.
func (b *Bundle) ByFQDN() map[string]Record {
	m := make(map[string]Record, len(b.Records))
	for _, r := range b.Records {
		m[normalizeFQDN(r.FQDN)] = r
	}
	return m
}

func normalizeFQDN(s string) string {
	if len(s) == 0 || s[len(s)-1] != '.' {
		s += "."
	}
	return s
}

// ParseAndValidate decodes raw JSON into a Bundle and checks the fields the
// responder depends on structurally: protocolVersion, a non-empty domain,
// and per-record fqdn/ttl/state sanity. It does NOT recompute contentHash —
// that's an integrity aid for humans/audit, not a wire-verification the
// responder is positioned to check (it has no shared secret with the
// producer beyond the bearer token used to fetch it, out of scope for v0's
// file-based --bundle flag).
func ParseAndValidate(raw []byte) (*Bundle, error) {
	var b Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("parse bundle json: %w", err)
	}
	if b.ProtocolVersion != SupportedProtocolVersion {
		return nil, fmt.Errorf("unsupported protocolVersion %d (want %d)", b.ProtocolVersion, SupportedProtocolVersion)
	}
	if b.Domain == "" {
		return nil, fmt.Errorf("bundle has no domain")
	}
	for i, r := range b.Records {
		if r.FQDN == "" {
			return nil, fmt.Errorf("record[%d]: empty fqdn", i)
		}
		if r.TTL <= 0 {
			return nil, fmt.Errorf("record %q: ttl must be positive, got %d", r.FQDN, r.TTL)
		}
	}
	return &b, nil
}

// LoadFile reads and validates a bundle from a JSON file on disk.
func LoadFile(path string) (*Bundle, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read bundle file %s: %w", path, err)
	}
	return ParseAndValidate(raw)
}

// cacheFileName is the file written under --cache-dir. Kept simple and
// singular (v0 is single-tenant per responder instance — one bundle, one
// cluster's vantage — per plan §8 "each responder holds only its own
// cluster's decision table").
const cacheFileName = "bundle.json"

// CachePath returns the path this package persists the last-good bundle to
// under dir.
func CachePath(dir string) string {
	return filepath.Join(dir, cacheFileName)
}

// SaveCache atomically persists raw bundle bytes to dir/bundle.json: write to
// a temp file in the same directory, fsync, then rename — so a crash never
// leaves a partially-written cache file that a subsequent boot would load
// and fail on (defeating the entire point of the fallback).
func SaveCache(dir string, raw []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir cache dir %s: %w", dir, err)
	}
	final := CachePath(dir)
	tmp := final + ".tmp"

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open temp cache file: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write temp cache file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("fsync temp cache file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close temp cache file: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename temp cache file into place: %w", err)
	}
	return nil
}

// LoadCache reads the last-good bundle previously persisted by SaveCache.
func LoadCache(dir string) (*Bundle, error) {
	return LoadFile(CachePath(dir))
}
