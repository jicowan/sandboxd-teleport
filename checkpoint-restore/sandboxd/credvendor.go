package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
)

// credVendor serves per-session AWS credentials to the nested sandbox via the
// AWS SDK's container-credentials provider (AWS_CONTAINER_CREDENTIALS_FULL_URI).
// It binds to the interior gateway (169.254.17.1) so ONLY this worker's sandbox
// netns can reach it — never the pod network or the outside world.
//
// Per session, it calls sts:AssumeRole for the session's authorized role, caches
// the result, and refreshes before expiry. The credentials are never checkpointed
// (they live only here, in the worker); on teleport the endpoint is rebuilt on the
// new worker and the SDK simply re-fetches — so AWS access survives restore with
// no baked-in secrets. See docs/PRD-sandbox-iam-credentials.md.
type credVendor struct {
	assume     assumeFunc    // injectable for tests; real impl calls STS
	tokenKey   []byte        // HMAC key: per-session auth token = HMAC(key, sid)
	skew       time.Duration // refresh this long before Expiration
	mu         sync.Mutex
	roles      map[string]string   // sid -> role ARN (registered at /run|/restore)
	cache      map[string]*credSet // sid -> cached creds
}

// credSet is one session's cached credentials.
type credSet struct {
	AccessKeyID     string
	SecretAccessKey string
	Token           string
	Expiration      time.Time
}

// assumeFunc performs sts:AssumeRole for roleARN, tagging the session with sid
// (a confused-deputy guard: role trust policies can require the session tag).
type assumeFunc func(ctx context.Context, roleARN, sid string) (*credSet, error)

// vendedCreds is the JSON shape the AWS SDK's container-credentials provider
// expects (ISO-8601 Expiration).
type vendedCreds struct {
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
	Expiration      string `json:"Expiration"`
}

func newCredVendor(assume assumeFunc, tokenKey []byte) *credVendor {
	return &credVendor{
		assume:   assume,
		tokenKey: tokenKey,
		skew:     5 * time.Minute,
		roles:    map[string]string{},
		cache:    map[string]*credSet{},
	}
}

// stsAssumeFunc returns a real assumeFunc backed by STS (the vendor's own AWS
// identity — a dedicated worker "vendor" role, distinct from the checkpoint
// identity — must be permitted to assume the target roles). durationSeconds bounds
// each session's credential lifetime.
func stsAssumeFunc(ctx context.Context) (assumeFunc, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	cl := sts.NewFromConfig(cfg)
	return func(ctx context.Context, roleARN, sid string) (*credSet, error) {
		out, err := cl.AssumeRole(ctx, &sts.AssumeRoleInput{
			RoleArn:         &roleARN,
			RoleSessionName: aws.String(roleSessionName(sid)),
			DurationSeconds: aws.Int32(3600),
			Tags:            []ststypes.Tag{{Key: aws.String("sandbox-session"), Value: aws.String(sid)}},
		})
		if err != nil {
			return nil, err
		}
		c := out.Credentials
		return &credSet{
			AccessKeyID:     aws.ToString(c.AccessKeyId),
			SecretAccessKey: aws.ToString(c.SecretAccessKey),
			Token:           aws.ToString(c.SessionToken),
			Expiration:      aws.ToTime(c.Expiration),
		}, nil
	}, nil
}

// register records the role a session may assume (called from /run and /restore).
// deregister on teardown drops the role + any cached creds.
func (v *credVendor) register(sid, roleARN string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.roles[sid] = roleARN
	delete(v.cache, sid) // force a fresh assume for the (possibly new) role
}

func (v *credVendor) deregister(sid string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.roles, sid)
	delete(v.cache, sid)
}

// sessionToken is the per-session bearer token the SDK must present. It is a
// deterministic HMAC of the session id, so BOTH /run and /restore compute the
// same token independently (no handoff) — which is what makes it teleport-safe:
// the sandbox's injected AWS_CONTAINER_AUTHORIZATION_TOKEN keeps matching after a
// restore onto a different worker (all workers share the fleet token key).
func (v *credVendor) sessionToken(sid string) string {
	m := hmac.New(sha256.New, v.tokenKey)
	m.Write([]byte(sid))
	return hex.EncodeToString(m.Sum(nil))
}

// credsFor returns valid credentials for a session, assuming/refreshing as needed.
func (v *credVendor) credsFor(ctx context.Context, sid string) (*credSet, error) {
	v.mu.Lock()
	role, ok := v.roles[sid]
	cached := v.cache[sid]
	v.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no role registered for session %q", sid)
	}
	// Fresh enough? (valid past the refresh skew.)
	if cached != nil && time.Until(cached.Expiration) > v.skew {
		return cached, nil
	}
	cs, err := v.assume(ctx, role, sid)
	if err != nil {
		return nil, err
	}
	v.mu.Lock()
	v.cache[sid] = cs
	v.mu.Unlock()
	return cs, nil
}

// ServeHTTP implements the container-credentials endpoint:
//
//	GET /creds/<sid>   Authorization: <sessionToken(sid)>
//
// The <sid> in the path + the matching HMAC token authorize the fetch; a
// different sandbox (even on the same worker) can't guess another session's token.
func (v *credVendor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = "/creds/"
	if len(r.URL.Path) <= len(prefix) || r.URL.Path[:len(prefix)] != prefix {
		http.NotFound(w, r)
		return
	}
	sid := r.URL.Path[len(prefix):]
	if !hmac.Equal([]byte(r.Header.Get("Authorization")), []byte(v.sessionToken(sid))) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	cs, err := v.credsFor(r.Context(), sid)
	if err != nil {
		log.Printf("cred vendor: assume failed for %s: %v", sid, err)
		http.Error(w, "credentials unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(vendedCreds{
		AccessKeyID:     cs.AccessKeyID,
		SecretAccessKey: cs.SecretAccessKey,
		Token:           cs.Token,
		Expiration:      cs.Expiration.UTC().Format(time.RFC3339),
	})
}

// awsEnvForSession returns the env vars to inject into the sandbox so its AWS SDK
// fetches from the vendor. host is the vendor address the sandbox routes to
// (credVendorIP = 169.254.170.23, an AWS-allow-listed container-creds host);
// port is where the vendor listens.
func (v *credVendor) awsEnvForSession(sid, host string, port int, region string) []string {
	env := []string{
		fmt.Sprintf("AWS_CONTAINER_CREDENTIALS_FULL_URI=http://%s:%d/creds/%s", host, port, sid),
		"AWS_CONTAINER_AUTHORIZATION_TOKEN=" + v.sessionToken(sid),
	}
	if region != "" {
		env = append(env, "AWS_REGION="+region)
	}
	return env
}

// roleSessionName derives a valid RoleSessionName (<=64 chars, limited charset)
// from a session id.
func roleSessionName(sid string) string {
	out := make([]byte, 0, len(sid))
	for i := 0; i < len(sid) && len(out) < 64; i++ {
		c := sid[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '@' {
			out = append(out, c)
		} else {
			out = append(out, '-')
		}
	}
	if len(out) == 0 {
		return "sandbox"
	}
	return string(out)
}
