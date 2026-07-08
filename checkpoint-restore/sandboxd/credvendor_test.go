//go:build linux

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func testVendor(assume assumeFunc) *credVendor {
	return newCredVendor(assume, []byte("fleet-secret-key"))
}

// staticAssume returns creds valid for the given duration and counts calls.
func staticAssume(valid time.Duration, calls *int32) assumeFunc {
	return func(_ context.Context, roleARN, sid string) (*credSet, error) {
		atomic.AddInt32(calls, 1)
		return &credSet{
			AccessKeyID:     "AKIA" + sid,
			SecretAccessKey: "secret",
			Token:           "tok-" + roleARN,
			Expiration:      time.Now().Add(valid),
		}, nil
	}
}

func TestCredVendorServesRegisteredSession(t *testing.T) {
	var calls int32
	v := testVendor(staticAssume(time.Hour, &calls))
	v.register("sess-a", "arn:aws:iam::111:role/foo")

	srv := httptest.NewServer(v)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/creds/sess-a", nil)
	req.Header.Set("Authorization", v.sessionToken("sess-a"))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var vc vendedCreds
	json.NewDecoder(resp.Body).Decode(&vc)
	if vc.AccessKeyID != "AKIAsess-a" || vc.Token != "tok-arn:aws:iam::111:role/foo" {
		t.Fatalf("wrong creds: %+v", vc)
	}
	if vc.Expiration == "" {
		t.Fatal("missing expiration")
	}
}

func TestCredVendorRejectsBadToken(t *testing.T) {
	v := testVendor(staticAssume(time.Hour, new(int32)))
	v.register("sess-a", "arn:role/foo")
	srv := httptest.NewServer(v)
	defer srv.Close()

	// no token
	resp, _ := srv.Client().Get(srv.URL + "/creds/sess-a")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403 without token, got %d", resp.StatusCode)
	}
	// wrong token (another session's)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/creds/sess-a", nil)
	req.Header.Set("Authorization", v.sessionToken("sess-b"))
	resp2, _ := srv.Client().Do(req)
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403 with wrong token, got %d", resp2.StatusCode)
	}
}

func TestCredVendorUnknownSession(t *testing.T) {
	v := testVendor(staticAssume(time.Hour, new(int32)))
	srv := httptest.NewServer(v)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/creds/ghost", nil)
	req.Header.Set("Authorization", v.sessionToken("ghost"))
	resp, _ := srv.Client().Do(req)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("want 500 for unregistered session, got %d", resp.StatusCode)
	}
}

func TestCredVendorCachesUntilSkew(t *testing.T) {
	var calls int32
	// valid 6m; skew is 5m, so first fetch caches and a second within the window reuses.
	v := testVendor(staticAssume(6*time.Minute, &calls))
	v.register("s", "arn:role/x")
	ctx := context.Background()
	if _, err := v.credsFor(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.credsFor(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 assume call (cached), got %d", got)
	}
}

func TestCredVendorRefreshesNearExpiry(t *testing.T) {
	var calls int32
	// valid only 1m; skew 5m => always considered stale => re-assume each call.
	v := testVendor(staticAssume(1*time.Minute, &calls))
	v.register("s", "arn:role/x")
	ctx := context.Background()
	v.credsFor(ctx, "s")
	v.credsFor(ctx, "s")
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected 2 assume calls (refresh), got %d", got)
	}
}

func TestCredVendorRegisterChangesRole(t *testing.T) {
	var calls int32
	seen := map[string]bool{}
	assume := func(_ context.Context, roleARN, sid string) (*credSet, error) {
		atomic.AddInt32(&calls, 1)
		seen[roleARN] = true
		return &credSet{Expiration: time.Now().Add(time.Hour)}, nil
	}
	v := testVendor(assume)
	ctx := context.Background()
	v.register("s", "arn:role/one")
	v.credsFor(ctx, "s")
	v.register("s", "arn:role/two") // re-register with a new role clears the cache
	v.credsFor(ctx, "s")
	if !seen["arn:role/one"] || !seen["arn:role/two"] {
		t.Fatalf("expected both roles assumed after re-register: %v", seen)
	}
}

func TestCredVendorDeregisterStopsServing(t *testing.T) {
	v := testVendor(staticAssume(time.Hour, new(int32)))
	v.register("s", "arn:role/x")
	v.deregister("s")
	if _, err := v.credsFor(context.Background(), "s"); err == nil {
		t.Fatal("expected error after deregister")
	}
}

func TestSessionTokenDeterministicPerKey(t *testing.T) {
	// Same key + sid => same token on any worker (teleport-safe). Different sid => different.
	v1 := newCredVendor(nil, []byte("k"))
	v2 := newCredVendor(nil, []byte("k"))
	if v1.sessionToken("sid1") != v2.sessionToken("sid1") {
		t.Fatal("token must be identical across workers with the same key")
	}
	if v1.sessionToken("sid1") == v1.sessionToken("sid2") {
		t.Fatal("different sessions must get different tokens")
	}
}

func TestAWSEnvForSession(t *testing.T) {
	v := testVendor(nil)
	env := v.awsEnvForSession("sid1", "169.254.17.1", 8091, "us-west-2")
	want := map[string]bool{
		"AWS_CONTAINER_CREDENTIALS_FULL_URI=http://169.254.17.1:8091/creds/sid1": false,
		"AWS_CONTAINER_AUTHORIZATION_TOKEN=" + v.sessionToken("sid1"):            false,
		"AWS_REGION=us-west-2": false,
	}
	for _, e := range env {
		if _, ok := want[e]; ok {
			want[e] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Fatalf("missing env: %s (got %v)", k, env)
		}
	}
}
