// Package remote implements zeus-fetch mode: the responder talking to the
// zeus API's global-endpoints surface (plan .plans/global-dns-failover.md
// §8 boot/subscribe/idle-timeout/cache bullets + §10 push channel).
//
// Wire contract (coded against exactly, per the parallel-build task spec —
// the zeus side must match this):
//
//	GET <zeus-url>/api/global-endpoints/bundle?cluster=<name>
//	  Authorization: Bearer <token>
//	  -> 200 application/json, body = the decision bundle (bundle.Bundle shape)
//
//	GET <zeus-url>/api/global-endpoints/events?cluster=<name>
//	  Authorization: Bearer <token>
//	  -> 200 text/event-stream (SSE). An event frame means "a new bundle may
//	     be available" — the payload is not trusted for content, the client
//	     always refetches the bundle endpoint on ANY event. The stream also
//	     emits keepalive comment frames (lines starting with ":") roughly
//	     every push interval; both event frames and keepalive frames count
//	     as liveness for idle-timeout purposes.
package remote

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxBundleBytes bounds how much of a bundle response body this client will
// read into memory. Plan §8 notes hundreds of records is trivial in
// memory; this is a generous safety ceiling against a misbehaving server,
// not a real-world sizing limit.
const maxBundleBytes = 16 << 20 // 16MiB

// Client is a bearer-token HTTP client bound to one zeus base URL and
// cluster name — the two path parameters every request needs.
type Client struct {
	BaseURL string
	Cluster string
	Token   string

	// httpClient is used for the (short, one-shot) bundle GET. It carries
	// an overall request timeout — safe because that request is not
	// long-lived.
	httpClient *http.Client

	// streamClient is used for the SSE GET. It intentionally carries NO
	// overall http.Client.Timeout: that field would cancel the request
	// (including in-flight body reads) after a fixed wall-clock duration
	// regardless of activity, which is wrong for a long-lived stream.
	// Liveness is instead enforced by Subscribe's idle timer via a
	// cancellable per-attempt context.
	streamClient *http.Client
}

// NewClient builds a Client. baseURL is trimmed of a trailing slash so
// callers can pass either form.
func NewClient(baseURL, cluster, token string) *Client {
	return &Client{
		BaseURL:      strings.TrimRight(baseURL, "/"),
		Cluster:      cluster,
		Token:        token,
		httpClient:   &http.Client{Timeout: 15 * time.Second},
		streamClient: &http.Client{},
	}
}

func (c *Client) bundleURL() string {
	return fmt.Sprintf("%s/api/global-endpoints/bundle?cluster=%s", c.BaseURL, url.QueryEscape(c.Cluster))
}

func (c *Client) eventsURL() string {
	return fmt.Sprintf("%s/api/global-endpoints/events?cluster=%s", c.BaseURL, url.QueryEscape(c.Cluster))
}

// FetchBundle performs one GET of the bundle endpoint and returns the raw
// response body. Validation of the body's shape happens in the bundle
// package (ParseAndValidate) — this layer only owns the transport contract.
func (c *Client) FetchBundle(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.bundleURL(), nil)
	if err != nil {
		return nil, fmt.Errorf("build bundle request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch bundle: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBundleBytes))
	if err != nil {
		return nil, fmt.Errorf("read bundle response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch bundle: unexpected status %d: %s", resp.StatusCode, truncate(body, 200))
	}
	return body, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
