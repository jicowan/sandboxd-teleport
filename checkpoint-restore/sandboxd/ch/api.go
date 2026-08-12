// Package ch drives a single cloud-hypervisor VMM over its REST api-socket.
//
// PROVENANCE: ported from Agent Substrate's cmd/ateom-microvm/internal/ch
// (github.com/agent-substrate/substrate, Apache-2.0, Copyright 2026 Google LLC).
// Adapted for sandboxd's microVM RuntimeDriver (see
// docs/sandboxd/PRD/PRD-microvm-runtime-cloud-hypervisor.md §5.4). The REST wire
// format is the one cloud-hypervisor documents for vm.create/boot/pause/snapshot/
// restore/resume.
//
// Licensed under the Apache License, Version 2.0.
package ch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
)

// apiBase is a placeholder host; the transport always dials the unix api-socket,
// so the host portion of the URL is ignored.
const apiBase = "http://localhost"

// apiClient speaks the cloud-hypervisor REST API over its unix api-socket.
type apiClient struct {
	http *http.Client
}

func newAPIClient(socketPath string) *apiClient {
	return &apiClient{
		http: &http.Client{
			Transport: &http.Transport{
				// CH's API server closes idle connections (and can be heavily
				// swapped during reclaim). Reusing a kept-alive connection then
				// blocks forever on the next request (observed in substrate:
				// vm.resume hangs on a reused connection while a fresh one works
				// instantly). Force a fresh connection per request.
				DisableKeepAlives: true,
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// get issues a GET and checks for a 2xx status.
func (c *apiClient) get(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}
	return nil
}

// getJSON issues a GET and decodes the 2xx JSON response into out.
func (c *apiClient) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: status %d: %s", path, resp.StatusCode, bytes.TrimSpace(b))
	}
	return json.Unmarshal(b, out)
}

// put issues a PUT with an optional JSON body and checks for a 2xx status.
func (c *apiClient) put(ctx context.Context, path string, body any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, apiBase+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("PUT %s: status %d: %s", path, resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}

// snapshotConfig is the body of /api/v1/vm.snapshot.
type snapshotConfig struct {
	DestinationURL string `json:"destination_url"`
}
