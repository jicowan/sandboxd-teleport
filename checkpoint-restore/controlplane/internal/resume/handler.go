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

package resume

import (
	"encoding/json"
	"errors"
	"net/http"

	"golang.org/x/sync/singleflight"

	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

// Handler serves the operator's internal POST /resume (TDD §5.1). The router
// calls it on a KV miss. Concurrent calls for the same sid are coalesced here too
// (defense in depth alongside the router's own singleflight); the KV CAS is the
// cross-replica guard.
type Handler struct {
	wf     *Workflow
	single singleflight.Group
}

// NewHandler wraps a Workflow as an http.Handler.
func NewHandler(wf *Workflow) *Handler { return &Handler{wf: wf} }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req resumeapi.ResumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SID == "" {
		writeErr(w, http.StatusBadRequest, "invalid request: sid required")
		return
	}

	// Coalesce concurrent resumes for the same session within this replica.
	v, err, _ := h.single.Do(req.SID, func() (any, error) {
		return h.wf.Resume(r.Context(), req.SID, req.Subject, req.Pool, req.App)
	})

	if err != nil {
		switch {
		case errors.Is(err, assign.ErrNoCapacity):
			writeErrRetry(w, http.StatusServiceUnavailable, "no capacity", 5)
		case errors.Is(err, assign.ErrVersionConflict):
			writeErr(w, http.StatusConflict, "version conflict")
		default:
			writeErr(w, http.StatusBadGateway, err.Error())
		}
		return
	}

	writeJSON(w, http.StatusOK, resumeapi.ResumeResponse{
		WorkerPodIP: v.(string),
		State:       resumeapi.StateRunning,
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, resumeapi.ResumeError{Error: msg})
}

func writeErrRetry(w http.ResponseWriter, code int, msg string, retryAfter int) {
	writeJSON(w, code, resumeapi.ResumeError{Error: msg, RetryAfterSeconds: retryAfter})
}
