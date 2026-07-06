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
	"errors"
	"net/http"
	"strings"
)

// Identity is the resolved caller: a stable session id and the subject it came
// from (for authz / logging). Derived from the request per O4.
type Identity struct {
	SID     string
	Subject string
}

// ErrUnauthenticated is returned when a request carries no usable identity.
var ErrUnauthenticated = errors.New("router: unauthenticated")

// Resolver extracts an Identity from a request. O4 (decided) reuses the existing
// Keycloak/agentgateway JWT: the production resolver validates it locally (JWKS)
// and derives the sid from subject+session claims. That validator lands with the
// broker integration; this interface keeps the router decoupled from it.
type Resolver interface {
	Resolve(r *http.Request) (Identity, error)
}

// HeaderResolver is the P1 resolver for trusted internal callers (the broker,
// behind agentgateway which already authenticated). It reads X-Session-ID (and
// optional X-Session-Subject). This is the agent-sandbox X-Sandbox-ID style; it
// is replaced/augmented by the JWT resolver at broker-cutover (O4).
type HeaderResolver struct {
	SessionHeader string // default "X-Session-ID"
	SubjectHeader string // default "X-Session-Subject"
}

// NewHeaderResolver returns a HeaderResolver with default header names.
func NewHeaderResolver() *HeaderResolver {
	return &HeaderResolver{SessionHeader: "X-Session-ID", SubjectHeader: "X-Session-Subject"}
}

func (h *HeaderResolver) Resolve(r *http.Request) (Identity, error) {
	sid := strings.TrimSpace(r.Header.Get(h.SessionHeader))
	if sid == "" {
		return Identity{}, ErrUnauthenticated
	}
	subj := strings.TrimSpace(r.Header.Get(h.SubjectHeader))
	if subj == "" {
		subj = sid
	}
	return Identity{SID: sid, Subject: subj}, nil
}
