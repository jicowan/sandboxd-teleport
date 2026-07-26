// ECR pull authentication for the worker's containerd image pulls.
//
// The worker pulls the WORKLOAD (sandbox) image directly via the node containerd
// API (see containerd.go) — this is a DIFFERENT path than the kubelet's pull of
// the worker POD image. containerd's default resolver is anonymous, so PRIVATE
// registries (notably ECR) return 401. Public images (ghcr/docker hub) work
// anonymously; private ECR images need credentials.
//
// This file adds an ECR authorizer: for an image ref whose registry host is an
// ECR endpoint, fetch a short-lived registry token via ecr:GetAuthorizationToken
// using the worker's AWS identity (EKS Pod Identity — the same default cred chain
// the credential vendor + S3 client already use), and hand containerd a
// RegistryHosts that authenticates with it. Non-ECR hosts fall through to the
// default (anonymous) path, so public images are unaffected.
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	dockerconfig "github.com/containerd/containerd/v2/core/remotes/docker/config"
)

// ecrHostRE matches an ECR (or ECR-FIPS) registry host:
//   <account>.dkr.ecr.<region>.amazonaws.com  /  ...ecr-fips.<region>...
var ecrHostRE = regexp.MustCompile(`^[0-9]{12}\.dkr\.ecr(-fips)?\.[a-z0-9-]+\.amazonaws\.com$`)

func isECRHost(host string) bool { return ecrHostRE.MatchString(host) }

// registryHost returns the registry host portion of an image ref
// (e.g. "820537372947.dkr.ecr.us-west-2.amazonaws.com" from that repo's ref).
func registryHost(ref string) string {
	s := ref
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	return strings.SplitN(s, "/", 2)[0]
}

// ecrToken caches the decoded ECR authorization (user, pass) per registry host.
// ECR tokens are valid ~12h; refresh a few minutes early. Guards against a token
// fetch on every pull (which would also rate-limit GetAuthorizationToken).
type ecrCred struct {
	user, pass string
	expires    time.Time
}

var (
	ecrMu    sync.Mutex
	ecrCache = map[string]ecrCred{} // host -> creds
)

// ecrCredsFor returns (username, password) for an ECR host, fetching+caching an
// authorization token via the worker's AWS identity. Username is always "AWS";
// password is the decoded token.
func ecrCredsFor(ctx context.Context, host string) (string, string, error) {
	ecrMu.Lock()
	if c, ok := ecrCache[host]; ok && time.Now().Before(c.expires.Add(-5*time.Minute)) {
		ecrMu.Unlock()
		return c.user, c.pass, nil
	}
	ecrMu.Unlock()

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return "", "", fmt.Errorf("aws config for ECR: %w", err)
	}
	out, err := ecr.NewFromConfig(cfg).GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return "", "", fmt.Errorf("ecr GetAuthorizationToken: %w", err)
	}
	if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil {
		return "", "", fmt.Errorf("ecr GetAuthorizationToken: empty authorization data")
	}
	ad := out.AuthorizationData[0]
	dec, err := base64.StdEncoding.DecodeString(*ad.AuthorizationToken)
	if err != nil {
		return "", "", fmt.Errorf("decode ECR token: %w", err)
	}
	user, pass, ok := strings.Cut(string(dec), ":")
	if !ok {
		return "", "", fmt.Errorf("malformed ECR token (no ':' separator)")
	}
	exp := time.Now().Add(12 * time.Hour)
	if ad.ExpiresAt != nil {
		exp = *ad.ExpiresAt
	}
	ecrMu.Lock()
	ecrCache[host] = ecrCred{user: user, pass: pass, expires: exp}
	ecrMu.Unlock()
	return user, pass, nil
}

// pullDialContext forces registry pulls onto IPv4 ("tcp4").
//
// WHY: the worker pulls the workload image from INSIDE its (IPv4-only) pod network
// namespace via the node containerd's resolver. Dual-stack registries — docker.io
// (its CloudFront CDN), quay.io, registry.k8s.io — advertise AAAA (IPv6) records.
// Go's dialer attempts an IPv6 address, which fails "connect: network is
// unreachable" on the v4-only pod netns, and containerd surfaces that instead of
// falling back to the A record → the pull fails. (kubelet is unaffected: it pulls
// in the HOST netns. ECR/ghcr happened to return reachable addresses.) Forcing
// "tcp4" makes the dialer resolve+dial only A records, so these registries pull.
// Opt out with SANDBOXD_PULL_FORCE_IPV4=0 on a genuinely IPv6-capable pod.
func pullDialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	forceV4 := os.Getenv("SANDBOXD_PULL_FORCE_IPV4") != "0" // default ON
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if forceV4 && (network == "tcp" || network == "tcp6") {
			network = "tcp4"
		}
		return d.DialContext(ctx, network, addr)
	}
}

// pullTransport is an http.RoundTripper for registry pulls that clones the default
// transport and swaps in the IPv4-forcing dialer.
func pullTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = pullDialContext()
	return t
}

// ecrRegistryHosts returns a containerd RegistryHosts used for ALL worker pulls: it
// authenticates ECR hosts with a fetched token (anonymous for everything else) AND
// installs the IPv4-forcing transport on every host (see pullDialContext). Used as
// ResolverOptions.Hosts for the worker's Pull, regardless of registry.
func ecrRegistryHosts(ctx context.Context) docker.RegistryHosts {
	return dockerconfig.ConfigureHosts(ctx, dockerconfig.HostOptions{
		DefaultScheme: "https",
		Credentials: func(host string) (string, string, error) {
			if !isECRHost(host) {
				return "", "", nil // anonymous for non-ECR hosts
			}
			return ecrCredsFor(ctx, host)
		},
		UpdateClient: func(client *http.Client) error {
			client.Transport = pullTransport()
			return nil
		},
	})
}
