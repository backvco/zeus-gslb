package bundle

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func noBackoff(int) time.Duration { return 0 }

func TestBootFromFetch_Success(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	calls := 0
	fetch := func(ctx context.Context) ([]byte, error) {
		calls++
		return []byte(validBundleJSON), nil
	}

	s, err := BootFromFetch(context.Background(), fetch, 3, noBackoff, cacheDir)
	if err != nil {
		t.Fatalf("BootFromFetch: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected exactly one fetch call on immediate success, got %d", calls)
	}
	if s.Current().Domain != "z-backv.local" {
		t.Errorf("Current().Domain = %q", s.Current().Domain)
	}
	if _, err := LoadCache(cacheDir); err != nil {
		t.Errorf("expected boot fetch to persist cache: %v", err)
	}
}

func TestBootFromFetch_RetriesThenSucceeds(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	calls := 0
	fetch := func(ctx context.Context) ([]byte, error) {
		calls++
		if calls < 3 {
			return nil, errors.New("transient network error")
		}
		return []byte(validBundleJSON), nil
	}

	s, err := BootFromFetch(context.Background(), fetch, 3, noBackoff, cacheDir)
	if err != nil {
		t.Fatalf("BootFromFetch: %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 fetch attempts, got %d", calls)
	}
	if s.Current().Domain != "z-backv.local" {
		t.Errorf("Current().Domain = %q", s.Current().Domain)
	}
}

func TestBootFromFetch_AllAttemptsFailFallsBackToCache(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	if err := SaveCache(cacheDir, []byte(validBundleJSON)); err != nil {
		t.Fatal(err)
	}

	calls := 0
	fetch := func(ctx context.Context) ([]byte, error) {
		calls++
		return nil, errors.New("zeus unreachable")
	}

	s, err := BootFromFetch(context.Background(), fetch, 3, noBackoff, cacheDir)
	if err != nil {
		t.Fatalf("expected fallback to cache, got error: %v", err)
	}
	if calls != 4 { // retries=3 => 4 total attempts
		t.Errorf("expected 4 fetch attempts (1 + 3 retries), got %d", calls)
	}
	if s.Current().Domain != "z-backv.local" {
		t.Errorf("Current().Domain = %q, want cache-loaded value", s.Current().Domain)
	}
}

func TestBootFromFetch_InvalidBundleFallsBackToCache(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	if err := SaveCache(cacheDir, []byte(validBundleJSON)); err != nil {
		t.Fatal(err)
	}

	fetch := func(ctx context.Context) ([]byte, error) {
		return []byte(`{"protocolVersion": 2, "domain": "bad", "records": []}`), nil
	}

	s, err := BootFromFetch(context.Background(), fetch, 1, noBackoff, cacheDir)
	if err != nil {
		t.Fatalf("expected fallback to cache, got error: %v", err)
	}
	if s.Current().Domain != "z-backv.local" {
		t.Errorf("Current().Domain = %q, want cache-loaded value", s.Current().Domain)
	}
}

func TestBootFromFetch_NeitherFetchNorCacheErrors(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "empty-cache")

	fetch := func(ctx context.Context) ([]byte, error) {
		return nil, errors.New("zeus unreachable")
	}

	_, err := BootFromFetch(context.Background(), fetch, 2, noBackoff, cacheDir)
	if err == nil {
		t.Fatal("expected error when neither fetch nor cache is usable (this is the caller's fatal/exit(1) condition)")
	}
}

func TestApplyIfChanged_HashUnchangedIsNoOp(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	fetch := func(ctx context.Context) ([]byte, error) { return []byte(validBundleJSON), nil }
	s, err := BootFromFetch(context.Background(), fetch, 0, noBackoff, cacheDir)
	if err != nil {
		t.Fatal(err)
	}

	applied, err := s.ApplyIfChanged([]byte(validBundleJSON))
	if err != nil {
		t.Fatalf("ApplyIfChanged: %v", err)
	}
	if applied {
		t.Error("expected applied=false when contentHash is unchanged")
	}
}

func TestApplyIfChanged_HashChangedApplies(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	fetch := func(ctx context.Context) ([]byte, error) { return []byte(validBundleJSON), nil }
	s, err := BootFromFetch(context.Background(), fetch, 0, noBackoff, cacheDir)
	if err != nil {
		t.Fatal(err)
	}

	updated := mustReplaceDomainAndHash(t, validBundleJSON, "z-backv-v2.local", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	applied, err := s.ApplyIfChanged(updated)
	if err != nil {
		t.Fatalf("ApplyIfChanged: %v", err)
	}
	if !applied {
		t.Fatal("expected applied=true when contentHash differs")
	}
	if s.Current().Domain != "z-backv-v2.local" {
		t.Errorf("Current().Domain = %q", s.Current().Domain)
	}
}

func TestApplyIfChanged_BadProtocolVersionKeepsServingOld(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	fetch := func(ctx context.Context) ([]byte, error) { return []byte(validBundleJSON), nil }
	s, err := BootFromFetch(context.Background(), fetch, 0, noBackoff, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	before := s.Current()

	bad := []byte(`{"protocolVersion": 2, "domain": "z-backv-bad.local", "contentHash": "different", "records": []}`)
	applied, err := s.ApplyIfChanged(bad)
	if err == nil {
		t.Fatal("expected error for unsupported protocolVersion")
	}
	if applied {
		t.Fatal("must not report applied=true on a rejected bundle")
	}
	if s.Current().Domain != before.Domain {
		t.Errorf("bad protocolVersion must keep serving the previous bundle, got domain=%q", s.Current().Domain)
	}
}

func mustReplaceDomainAndHash(t *testing.T, raw, newDomain, newHash string) []byte {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	m["domain"] = newDomain
	m["contentHash"] = newHash
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
