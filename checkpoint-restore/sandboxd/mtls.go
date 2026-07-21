package main

// SPIFFE mTLS for the worker's control API (:8090). When enabled, the control
// server requires a client X509-SVID and authorizes the caller's SPIFFE ID ==
// the operator's — so only the operator may drive /run, /restore, /checkpoint,
// /suspend, /reset. Off by default (SANDBOXD_MTLS != "1"): plain HTTP fallback for
// rollout. The credential vendor (:8091, netns-internal HMAC) is unaffected.
//
// This mirrors controlplane/internal/spiffemtls but lives in the sandboxd module
// (separate go.mod, can't import controlplane/internal). Kept behind this seam so
// the identity SOURCE (SPIRE today; possibly k8s pod certs later) can be swapped
// without touching the server wiring.

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

const defaultSpiffeSocket = "unix:///spiffe-workload-api/spire-agent.sock"

// mtlsServerConfig builds a *tls.Config for the control server that requires +
// verifies a client SVID and authorizes peer SPIFFE ID == allowPeer (the
// operator). socketPath empty -> the default CSI mount. Blocks up to timeout for
// the first SVID so startup fails fast if SPIRE isn't reachable. The returned
// closer releases the Workload API stream on shutdown.
func mtlsServerConfig(socketPath, allowPeer string, timeout time.Duration) (*tls.Config, func() error, error) {
	if socketPath == "" {
		socketPath = defaultSpiffeSocket
	}
	id, err := spiffeid.FromString(allowPeer)
	if err != nil {
		return nil, nil, fmt.Errorf("spiffe: bad operator id %q: %w", allowPeer, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	src, err := workloadapi.NewX509Source(ctx,
		workloadapi.WithClientOptions(workloadapi.WithAddr(socketPath)))
	if err != nil {
		return nil, nil, fmt.Errorf("spiffe: X509Source from %s: %w", socketPath, err)
	}
	if _, err := src.GetX509SVID(); err != nil {
		src.Close()
		return nil, nil, fmt.Errorf("spiffe: no SVID yet: %w", err)
	}
	cfg := tlsconfig.MTLSServerConfig(src, src, tlsconfig.AuthorizeID(id))
	return cfg, src.Close, nil
}
