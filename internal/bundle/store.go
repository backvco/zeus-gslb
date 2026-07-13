package bundle

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// Store holds the currently-served bundle and owns the boot + hot-reload
// lifecycle described in plan §8: boot from --bundle (falling back to the
// last-good cache if --bundle is missing/invalid), persist every accepted
// bundle to --cache-dir, and poll the --bundle file's mtime so an operator
// (or a future fetcher) can drop a new bundle in place without a restart.
type Store struct {
	bundlePath string
	cacheDir   string

	mu      sync.RWMutex
	current *Bundle
	modTime time.Time // mtime of bundlePath as of the last successful load
}

// NewStore boots a Store: try --bundle first, fall back to the persisted
// cache, and error only if BOTH are missing/invalid — the responder has
// nothing safe to answer with.
func NewStore(bundlePath, cacheDir string) (*Store, error) {
	s := &Store{bundlePath: bundlePath, cacheDir: cacheDir}

	if b, mt, err := s.tryLoadBundleFile(); err == nil {
		s.setCurrent(b, mt)
		if raw, rerr := os.ReadFile(bundlePath); rerr == nil {
			if cerr := SaveCache(cacheDir, raw); cerr != nil {
				log.Printf("bundle: cache persist failed (continuing on loaded bundle): %v", cerr)
			}
		}
		log.Printf("bundle: loaded %s (%d records, domain=%s)", bundlePath, len(b.Records), b.Domain)
		return s, nil
	} else {
		log.Printf("bundle: --bundle %q not usable at boot (%v) — falling back to cache", bundlePath, err)
	}

	cached, err := LoadCache(cacheDir)
	if err != nil {
		return nil, fmt.Errorf("no usable --bundle and no usable cache in %s: %w", cacheDir, err)
	}
	s.setCurrent(cached, time.Time{})
	log.Printf("bundle: loaded last-good cache from %s (%d records, domain=%s)", CachePath(cacheDir), len(cached.Records), cached.Domain)
	return s, nil
}

func (s *Store) tryLoadBundleFile() (*Bundle, time.Time, error) {
	fi, err := os.Stat(s.bundlePath)
	if err != nil {
		return nil, time.Time{}, err
	}
	b, err := LoadFile(s.bundlePath)
	if err != nil {
		return nil, time.Time{}, err
	}
	return b, fi.ModTime(), nil
}

func (s *Store) setCurrent(b *Bundle, mt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = b
	s.modTime = mt
}

// Current returns the bundle currently being served. Never nil once
// NewStore has succeeded.
func (s *Store) Current() *Bundle {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// pollInterval is the mtime-poll cadence for hot-reload (v0: "1s poll is
// fine", per the task spec — no fsnotify dependency for a scaffold).
const pollInterval = 1 * time.Second

// WatchAndReload polls bundlePath's mtime every pollInterval and hot-reloads
// on change, until ctx is cancelled. A bad reload candidate (parse error,
// wrong protocolVersion) is logged and the previous bundle keeps serving —
// per §8's protocol-skew rule, a bad push must never brick answering.
func (s *Store) WatchAndReload(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reloadIfChanged()
		}
	}
}

func (s *Store) reloadIfChanged() {
	fi, err := os.Stat(s.bundlePath)
	if err != nil {
		// File missing/unreadable on an already-running responder: keep
		// serving what we have. Do not log every poll tick at this level —
		// only state transitions would be worth surfacing, and v0 keeps it
		// simple (a scaffold, not the final telemetry surface).
		return
	}

	s.mu.RLock()
	unchanged := fi.ModTime().Equal(s.modTime)
	s.mu.RUnlock()
	if unchanged {
		return
	}

	raw, err := os.ReadFile(s.bundlePath)
	if err != nil {
		log.Printf("bundle: hot-reload candidate %s unreadable (%v) — keeping previous bundle", s.bundlePath, err)
		return
	}

	if _, err := s.applyBundle(raw, fi.ModTime()); err != nil {
		log.Printf("bundle: hot-reload candidate %s rejected (%v) — keeping previous bundle", s.bundlePath, err)
		// Remember the mtime anyway so we don't retry-parse the same bad
		// file every tick until it changes again.
		s.mu.Lock()
		s.modTime = fi.ModTime()
		s.mu.Unlock()
		return
	}

	log.Printf("bundle: hot-reloaded %s", s.bundlePath)
}

// applyBundle validates raw and, if valid, atomically swaps it in as the
// currently-served bundle (setCurrent) and persists it to the cache dir.
// This is the single swap primitive plan §8 requires be shared between file
// hot-reload and zeus-fetch push/poll ("existing hot-reload path refactored
// so file-watch and push share one applyBundle()"). It always swaps on a
// valid bundle — callers decide *whether* to call it (file-watch: mtime
// changed; push/poll: contentHash differs — see ApplyIfChanged) and never
// second-guess a valid bundle already accepted by the caller's policy.
func (s *Store) applyBundle(raw []byte, mt time.Time) (*Bundle, error) {
	b, err := ParseAndValidate(raw)
	if err != nil {
		return nil, err
	}
	s.setCurrent(b, mt)
	if cerr := SaveCache(s.cacheDir, raw); cerr != nil {
		log.Printf("bundle: cache persist failed (continuing on newly applied bundle): %v", cerr)
	}
	log.Printf("bundle: applied bundle (%d records, domain=%s, generatedAt=%s)", len(b.Records), b.Domain, b.GeneratedAt)
	return b, nil
}

// ApplyIfChanged validates raw and, only if its contentHash differs from the
// bundle currently being served, swaps it in via applyBundle. This is the
// zeus-fetch push/poll trigger policy (plan §8: "apply only if contentHash
// differs" — a no-op refetch must cost one comparison, not a needless
// cache-file rewrite + log line). Returns applied=true iff a swap occurred;
// a parse/validation error (including unsupported protocolVersion) is
// returned without touching the currently-served bundle.
func (s *Store) ApplyIfChanged(raw []byte) (applied bool, err error) {
	b, err := ParseAndValidate(raw)
	if err != nil {
		return false, err
	}

	s.mu.RLock()
	unchanged := s.current != nil && b.ContentHash != "" && s.current.ContentHash == b.ContentHash
	s.mu.RUnlock()
	if unchanged {
		return false, nil
	}

	if _, err := s.applyBundle(raw, time.Now()); err != nil {
		return false, err
	}
	return true, nil
}
