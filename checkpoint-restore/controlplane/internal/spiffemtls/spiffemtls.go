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

// Package spiffemtls secures the sandboxd control-plane hops (router->operator,
// operator->worker) with SPIFFE mTLS (P1.5). Each component fetches its X509-SVID
// from the SPIRE Agent's Workload API socket and every hop becomes mutual TLS
// authorized on the PEER's SPIFFE ID — turning "anything that can reach the port"
// into "only the workload with the expected SPIFFE ID."
//
// Disabled (Source==nil) everywhere by a single flag so plain HTTP remains the
// fallback during rollout; when disabled the helpers return nil and callers use
// their existing plain transport/server.
package spiffemtls

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// DefaultSocketPath is where the SPIFFE CSI driver mounts the Workload API socket.
const DefaultSocketPath = "unix:///spiffe-workload-api/spire-agent.sock"

// Source wraps a go-spiffe X509Source (SVID + trust bundle, auto-rotated). It is
// the single object each component builds at startup and threads into the mTLS
// server/client helpers. A nil *Source means "mTLS disabled" and every helper is a
// no-op returning nil — callers then use their plain-HTTP path.
type Source struct {
	x  *workloadapi.X509Source
	td spiffeid.TrustDomain
}

// New builds a Source from the Workload API socket. It blocks (up to timeout) for
// the first SVID so startup fails fast if SPIRE isn't reachable. socketPath empty
// -> DefaultSocketPath.
func New(ctx context.Context, socketPath string, timeout time.Duration) (*Source, error) {
	if socketPath == "" {
		socketPath = DefaultSocketPath
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	x, err := workloadapi.NewX509Source(cctx,
		workloadapi.WithClientOptions(workloadapi.WithAddr(socketPath)))
	if err != nil {
		return nil, fmt.Errorf("spiffe: build X509Source from %s: %w", socketPath, err)
	}
	svid, err := x.GetX509SVID()
	if err != nil {
		x.Close()
		return nil, fmt.Errorf("spiffe: no SVID yet: %w", err)
	}
	return &Source{x: x, td: svid.ID.TrustDomain()}, nil
}

// Close releases the Workload API stream.
func (s *Source) Close() error {
	if s == nil || s.x == nil {
		return nil
	}
	return s.x.Close()
}

// ID returns this workload's own SPIFFE ID (for logging).
func (s *Source) ID() string {
	if s == nil || s.x == nil {
		return ""
	}
	if svid, err := s.x.GetX509SVID(); err == nil {
		return svid.ID.String()
	}
	return ""
}

// implements x509svid.Source + x509bundle.Source by delegating to the wrapped
// X509Source, so we can hand `s` directly to tlsconfig.* below.
func (s *Source) GetX509SVID() (*x509svid.SVID, error) { return s.x.GetX509SVID() }
func (s *Source) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return s.x.GetX509BundleForTrustDomain(td)
}

// ServerTLSConfig returns a *tls.Config for an mTLS server that REQUIRES a client
// SVID and authorizes the peer's SPIFFE ID == allowPeer (e.g. the operator's
// /resume server authorizing spiffe://sandboxd/router). Returns nil if s is nil
// (mTLS disabled).
func (s *Source) ServerTLSConfig(allowPeer string) (*tls.Config, error) {
	if s == nil {
		return nil, nil
	}
	id, err := spiffeid.FromString(allowPeer)
	if err != nil {
		return nil, fmt.Errorf("spiffe: bad allowPeer %q: %w", allowPeer, err)
	}
	return tlsconfig.MTLSServerConfig(s, s, tlsconfig.AuthorizeID(id)), nil
}

// ClientTLSConfig returns a *tls.Config for an mTLS client that presents this
// workload's SVID and authorizes the server's SPIFFE ID == allowPeer (e.g. the
// router dialing the operator authorizes spiffe://sandboxd/operator). Returns nil
// if s is nil.
func (s *Source) ClientTLSConfig(allowPeer string) (*tls.Config, error) {
	if s == nil {
		return nil, nil
	}
	id, err := spiffeid.FromString(allowPeer)
	if err != nil {
		return nil, fmt.Errorf("spiffe: bad allowPeer %q: %w", allowPeer, err)
	}
	return tlsconfig.MTLSClientConfig(s, s, tlsconfig.AuthorizeID(id)), nil
}

// HTTPClient returns an *http.Client whose transport presents this workload's SVID
// and authorizes the server peer == allowPeer. Returns nil if s is nil (caller
// falls back to its plain client). timeout==0 -> no client timeout (per-call ctx
// governs), matching the sandboxd client's convention.
func (s *Source) HTTPClient(allowPeer string, timeout time.Duration) (*http.Client, error) {
	if s == nil {
		return nil, nil
	}
	cfg, err := s.ClientTLSConfig(allowPeer)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{TLSClientConfig: cfg},
	}, nil
}
