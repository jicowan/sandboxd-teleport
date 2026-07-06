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

package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

// ErrNoCapacity mirrors a 503 from the operator's /resume (pool exhausted).
var ErrNoCapacity = errors.New("router: no capacity")

// ResumeClient calls the operator's internal /resume endpoint (TDD §5.1).
type ResumeClient struct {
	url string
	hc  *http.Client
}

// NewResumeClient targets the operator resume endpoint (e.g.
// http://sandboxd-controlplane-operator:8082/resume). hc may carry the P1.5 mTLS
// transport; nil uses a default client.
func NewResumeClient(url string, hc *http.Client) *ResumeClient {
	if hc == nil {
		hc = &http.Client{}
	}
	return &ResumeClient{url: url, hc: hc}
}

// Resume asks the operator to get sid Running and return the worker IP. Returns
// ErrNoCapacity on 503 so the router can surface Retry-After.
func (c *ResumeClient) Resume(ctx context.Context, sid, subject, poolHint string) (string, error) {
	body, _ := json.Marshal(resumeapi.ResumeRequest{SID: sid, Subject: subject, Pool: poolHint})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch resp.StatusCode {
	case http.StatusOK:
		var out resumeapi.ResumeResponse
		if err := json.Unmarshal(data, &out); err != nil {
			return "", err
		}
		return out.WorkerPodIP, nil
	case http.StatusServiceUnavailable:
		return "", ErrNoCapacity
	default:
		return "", fmt.Errorf("resume %s: %d %s", sid, resp.StatusCode, bytes.TrimSpace(data))
	}
}
