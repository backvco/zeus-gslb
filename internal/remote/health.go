// health.go implements the responder → zeus health-report POST (P2
// contract "Health reports (Go → zeus)"):
//
//	POST <zeus-url>/api/global-endpoints/health   Authorization: Bearer <cluster token>
//	{ "cluster": "z-01",
//	  "protocolVersion": 1,
//	  "transitions": [{ "targetKey": "...", "state": "healthy|unhealthy", "at": "ISO" }],
//	  "states": { "targetKey": "healthy|unhealthy" } }
//
// Sent immediately on any transition AND as a 30s heartbeat (possibly empty
// transitions, states always full). 2xx = accepted; non-2xx retries with
// backoff and never crashes or stops the responder from serving.
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// HealthProtocolVersion is the protocolVersion stamped on every health
// report — pinned at 1 alongside the bundle's SupportedProtocolVersion
// (P2 contract: "protocolVersion stays 1 — all additions are additive").
const HealthProtocolVersion = 1

// Transition is one targetKey health-state change in a HealthReport.
type Transition struct {
	TargetKey string `json:"targetKey"`
	State     string `json:"state"` // healthy|unhealthy
	At        string `json:"at"`    // ISO-8601
}

// HealthReport is the exact POST body shape the P2 contract pins.
type HealthReport struct {
	Cluster         string            `json:"cluster"`
	ProtocolVersion int               `json:"protocolVersion"`
	Transitions     []Transition      `json:"transitions"`
	States          map[string]string `json:"states"`
}

func (c *Client) healthURL() string {
	return fmt.Sprintf("%s/api/global-endpoints/health", c.BaseURL)
}

// ReportHealth POSTs one health report. A non-2xx response or transport
// error is returned to the caller (HealthReporter owns retry/backoff — this
// method makes exactly one attempt).
func (c *Client) ReportHealth(ctx context.Context, report HealthReport) error {
	body, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshal health report: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.healthURL(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build health report request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("post health report: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("post health report: unexpected status %d: %s", resp.StatusCode, truncate(respBody, 200))
	}
	return nil
}
