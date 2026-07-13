package bundle

import (
	"context"
	"fmt"
	"log"
	"time"
)

// BootFromFetch boots a Store from a remote fetch function instead of a
// local --bundle file (zeus-fetch mode, plan §8 "Boot order"): try fetch up
// to retries+1 total attempts with backoff between them; on the first
// attempt that returns a structurally valid bundle, swap it in and persist
// to cacheDir. If every attempt fails (transport error or a bundle that
// fails ParseAndValidate — including unsupported protocolVersion), fall
// back to the last-good cache. Only if neither a successful fetch nor a
// usable cache exists does this return an error — the caller (main) treats
// that as fatal ("never exit while a cache exists").
func BootFromFetch(
	ctx context.Context,
	fetch func(context.Context) ([]byte, error),
	retries int,
	backoff func(attempt int) time.Duration,
	cacheDir string,
) (*Store, error) {
	s := &Store{cacheDir: cacheDir}
	var lastErr error

attempts:
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			if backoff == nil {
				break attempts
			}
			select {
			case <-ctx.Done():
				lastErr = ctx.Err()
				break attempts
			case <-time.After(backoff(attempt)):
			}
		}

		raw, err := fetch(ctx)
		if err != nil {
			lastErr = err
			log.Printf("bundle: boot fetch attempt %d/%d failed: %v", attempt+1, retries+1, err)
			continue
		}

		b, err := s.applyBundle(raw, time.Now())
		if err != nil {
			lastErr = err
			log.Printf("bundle: boot fetch attempt %d/%d returned invalid bundle: %v", attempt+1, retries+1, err)
			continue
		}

		log.Printf("bundle: boot fetch succeeded on attempt %d/%d (%d records, domain=%s)",
			attempt+1, retries+1, len(b.Records), b.Domain)
		return s, nil
	}

	log.Printf("bundle: all boot fetch attempts exhausted (%v) — falling back to cache", lastErr)
	cached, err := LoadCache(cacheDir)
	if err != nil {
		return nil, fmt.Errorf("boot fetch failed (%v) and no usable cache in %s: %w", lastErr, cacheDir, err)
	}
	s.setCurrent(cached, time.Time{})
	log.Printf("bundle: loaded last-good cache from %s after boot fetch failure (%d records, domain=%s)",
		CachePath(cacheDir), len(cached.Records), cached.Domain)
	return s, nil
}
