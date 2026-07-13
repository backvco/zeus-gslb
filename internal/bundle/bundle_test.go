package bundle

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// validBundleJSON mirrors the exact shape locked by bundle.test.js's
// "returns the exact responder boot-contract shape" case.
const validBundleJSON = `{
  "protocolVersion": 1,
  "domain": "z-backv.local",
  "generatedAt": "2026-07-10T00:00:00.000Z",
  "contentHash": "` + `0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd` + `",
  "records": [
    {
      "fqdn": "mysql-01-rw.prod.app1.z-backv.local",
      "ttl": 7,
      "state": "degraded",
      "answers": [
        {"ip": "10.0.5.5", "port": 443},
        {"ip": "10.0.5.6", "port": 443},
        {"ip": "10.0.5.7", "port": 443}
      ]
    }
  ]
}`

func TestParseAndValidate_Valid(t *testing.T) {
	b, err := ParseAndValidate([]byte(validBundleJSON))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.ProtocolVersion != 1 || b.Domain != "z-backv.local" || len(b.Records) != 1 {
		t.Fatalf("unexpected parse result: %+v", b)
	}
	if b.Records[0].TTL != 7 || len(b.Records[0].Answers) != 3 {
		t.Fatalf("unexpected record: %+v", b.Records[0])
	}
}

func TestParseAndValidate_RejectsWrongProtocolVersion(t *testing.T) {
	raw := `{"protocolVersion": 2, "domain": "z-backv.local", "records": []}`
	if _, err := ParseAndValidate([]byte(raw)); err == nil {
		t.Fatal("expected error for protocolVersion != 1, got nil")
	}
}

func TestParseAndValidate_RejectsMissingDomain(t *testing.T) {
	raw := `{"protocolVersion": 1, "domain": "", "records": []}`
	if _, err := ParseAndValidate([]byte(raw)); err == nil {
		t.Fatal("expected error for empty domain, got nil")
	}
}

func TestParseAndValidate_RejectsBadJSON(t *testing.T) {
	if _, err := ParseAndValidate([]byte("not json")); err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestSaveAndLoadCache_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := SaveCache(dir, []byte(validBundleJSON)); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	b, err := LoadCache(dir)
	if err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if b.Domain != "z-backv.local" {
		t.Errorf("domain = %q", b.Domain)
	}
	// Confirm the write was atomic — no leftover .tmp file.
	if _, err := os.Stat(CachePath(dir) + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("expected no leftover .tmp file, stat err = %v", err)
	}
}

func TestByFQDN_NormalizesTrailingDot(t *testing.T) {
	b, err := ParseAndValidate([]byte(validBundleJSON))
	if err != nil {
		t.Fatal(err)
	}
	idx := b.ByFQDN()
	if _, ok := idx["mysql-01-rw.prod.app1.z-backv.local."]; !ok {
		t.Fatalf("expected fqdn with trailing dot in index, got keys: %v", keys(idx))
	}
}

func keys[K comparable, V any](m map[K]V) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestNewStore_LoadsBundleFileAndPersistsCache(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundle.json")
	if err := os.WriteFile(bundlePath, []byte(validBundleJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(dir, "cache")

	s, err := NewStore(bundlePath, cacheDir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if s.Current().Domain != "z-backv.local" {
		t.Errorf("Current().Domain = %q", s.Current().Domain)
	}
	if _, err := os.Stat(CachePath(cacheDir)); err != nil {
		t.Errorf("expected cache to be persisted at boot: %v", err)
	}
}

func TestNewStore_FallsBackToCacheWhenBundleMissing(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	if err := SaveCache(cacheDir, []byte(validBundleJSON)); err != nil {
		t.Fatal(err)
	}

	missingBundlePath := filepath.Join(dir, "does-not-exist.json")
	s, err := NewStore(missingBundlePath, cacheDir)
	if err != nil {
		t.Fatalf("NewStore should fall back to cache, got error: %v", err)
	}
	if s.Current().Domain != "z-backv.local" {
		t.Errorf("Current().Domain = %q, want cache-loaded value", s.Current().Domain)
	}
}

func TestNewStore_FallsBackToCacheWhenBundleInvalid(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	if err := SaveCache(cacheDir, []byte(validBundleJSON)); err != nil {
		t.Fatal(err)
	}

	badBundlePath := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badBundlePath, []byte(`{"protocolVersion": 2, "domain": "x", "records": []}`), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := NewStore(badBundlePath, cacheDir)
	if err != nil {
		t.Fatalf("NewStore should fall back to cache on invalid bundle, got error: %v", err)
	}
	if s.Current().Domain != "z-backv.local" {
		t.Errorf("Current().Domain = %q, want cache-loaded value", s.Current().Domain)
	}
}

func TestNewStore_ErrorsWhenNeitherBundleNorCacheUsable(t *testing.T) {
	dir := t.TempDir()
	_, err := NewStore(filepath.Join(dir, "nope.json"), filepath.Join(dir, "empty-cache"))
	if err == nil {
		t.Fatal("expected error when neither --bundle nor cache is usable")
	}
}

func TestStore_HotReloadOnMtimeChange(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundle.json")
	if err := os.WriteFile(bundlePath, []byte(validBundleJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(dir, "cache")

	s, err := NewStore(bundlePath, cacheDir)
	if err != nil {
		t.Fatal(err)
	}

	updated := mustReplaceDomain(t, validBundleJSON, "z-backv-v2.local")
	writeWithBumpedMtime(t, bundlePath, updated)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go s.WatchAndReload(ctx)

	waitUntil(t, 2*time.Second, func() bool {
		return s.Current().Domain == "z-backv-v2.local"
	})
}

func TestStore_HotReloadRejectsBadProtocolVersionKeepsPrevious(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundle.json")
	if err := os.WriteFile(bundlePath, []byte(validBundleJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(dir, "cache")

	s, err := NewStore(bundlePath, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	before := s.Current()

	bad := []byte(`{"protocolVersion": 2, "domain": "z-backv-bad.local", "records": []}`)
	writeWithBumpedMtime(t, bundlePath, bad)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.WatchAndReload(ctx) // runs until ctx timeout; poll interval is 1s so this covers at least one tick

	if s.Current().Domain != before.Domain {
		t.Errorf("bad protocolVersion reload should keep previous bundle, got domain=%q", s.Current().Domain)
	}
	if s.Current().Domain == "z-backv-bad.local" {
		t.Fatal("must never adopt a bundle with an unsupported protocolVersion")
	}
}

// --- test helpers ---

func mustReplaceDomain(t *testing.T, raw, newDomain string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	m["domain"] = newDomain
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// writeWithBumpedMtime writes content and forces the mtime forward so the
// 1s poll loop reliably observes a change even on fast filesystems where
// two writes within the same tick could otherwise land on an identical
// truncated mtime.
func writeWithBumpedMtime(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}
