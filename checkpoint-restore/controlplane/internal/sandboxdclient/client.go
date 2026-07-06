/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package sandboxdclient is a typed HTTP client over sandboxd's worker API
// (TDD §5.3). It speaks the shared/sbxapi wire types so the control plane and
// sandboxd agree on the contract at compile time. Transport is injectable so
// P1.5 can swap in a SPIRE-sourced mTLS transport without touching call sites.
package sandboxdclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jicowan/aio-sandbox/shared/sbxapi"
)

// DefaultPort is sandboxd's HTTP listen port (SANDBOXD_ADDR :8090).
const DefaultPort = 8090

// Client talks to a single sandboxd worker at baseURL (e.g. http://10.0.3.95:8090).
type Client struct {
	baseURL string
	hc      *http.Client
}

// New returns a client for a worker reachable at host (pod IP). Transport is the
// http.Client to use; pass nil for a sane default. P1.5 passes an mTLS client.
func New(host string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 0} // no global timeout; per-call ctx governs
	}
	return &Client{
		baseURL: fmt.Sprintf("http://%s:%d", host, DefaultPort),
		hc:      hc,
	}
}

// NewForBaseURL returns a client pointed at an explicit base URL (no port
// assumption). Handy for tests (httptest) and non-default deployments.
func NewForBaseURL(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 0}
	}
	return &Client{baseURL: baseURL, hc: hc}
}

// Run starts a sandbox from an image (POST /run).
func (c *Client) Run(ctx context.Context, req sbxapi.RunRequest) (*sbxapi.RunResponse, error) {
	var out sbxapi.RunResponse
	if err := c.do(ctx, http.MethodPost, "/run", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Restore resumes a sandbox from an S3 snapshot (POST /restore). Used by P3.
func (c *Client) Restore(ctx context.Context, req sbxapi.RestoreRequest) (*sbxapi.RestoreResponse, error) {
	var out sbxapi.RestoreResponse
	if err := c.do(ctx, http.MethodPost, "/restore", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Suspend checkpoints the sandbox to S3 and frees the worker (POST /suspend).
// Returns the snapshot S3 prefix to record in the assignment table.
func (c *Client) Suspend(ctx context.Context, sandboxID string) (*sbxapi.SuspendResponse, error) {
	var out sbxapi.SuspendResponse
	if err := c.do(ctx, http.MethodPost, "/suspend", sbxapi.SuspendRequest{SandboxID: sandboxID}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Status returns the sandbox runtime status (GET /status?sandboxId=).
func (c *Client) Status(ctx context.Context, sandboxID string) (*sbxapi.StatusResponse, error) {
	var out sbxapi.StatusResponse
	if err := c.do(ctx, http.MethodGet, "/status?sandboxId="+sandboxID, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Reset frees the worker without checkpointing (POST /reset).
func (c *Client) Reset(ctx context.Context, sandboxID string) error {
	body := map[string]string{"sandboxId": sandboxID}
	return c.do(ctx, http.MethodPost, "/reset", body, nil)
}

// Capacity reports whether the worker is busy (GET /capacity).
func (c *Client) Capacity(ctx context.Context) (*sbxapi.CapacityResponse, error) {
	var out sbxapi.CapacityResponse
	if err := c.do(ctx, http.MethodGet, "/capacity", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WaitReady polls /status until ready==true, ctx expires, or the sandbox reaches
// a terminal non-running state. It is the TTFB-clock gate for the caller (O8):
// pass a context with the resume deadline.
func (c *Client) WaitReady(ctx context.Context, sandboxID string, interval time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		st, err := c.Status(ctx, sandboxID)
		if err == nil {
			if st.Ready {
				return nil
			}
			if st.Status != "" && st.Status != "running" {
				return fmt.Errorf("sandbox %s entered terminal status %q while waiting for ready", sandboxID, st.Status)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait ready %s: %w", sandboxID, ctx.Err())
		case <-t.C:
		}
	}
}

// do executes a JSON request/response against the worker. A nil body sends no
// payload; a nil out discards the response body.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("sandboxd %s %s: %d %s", method, path, resp.StatusCode, string(bytes.TrimSpace(data)))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode %s %s response: %w", method, path, err)
		}
	}
	return nil
}
