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

// Command router is the session-aware data-plane proxy (PRD §5). It is a thin
// binary: resolve identity, read the KV assignment table, and stream-proxy to the
// worker (resuming via the operator on a miss). Deliberately separate from the
// operator (O2) — different scaling and blast radius.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/controlplane/internal/router"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	var listenAddr, kvAddr, resumeURL string
	var workerPort int
	flag.StringVar(&listenAddr, "listen", envOr("ROUTER_LISTEN", ":8080"), "Router data-plane listen address.")
	flag.StringVar(&kvAddr, "kv-addr", envOr("SANDBOXD_KV_ADDR", "valkey:6379"), "Valkey address (host:port).")
	flag.StringVar(&resumeURL, "resume-url", envOr("SANDBOXD_RESUME_URL",
		"http://sandboxd-controlplane-operator:8082/resume"), "Operator /resume endpoint.")
	flag.IntVar(&workerPort, "worker-port", 8090, "sandboxd port on worker pods.")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	kv := assign.New(kvAddr)
	defer kv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := kv.Ping(ctx); err != nil {
		cancel()
		log.Error("cannot reach assignment table", "addr", kvAddr, "err", err)
		os.Exit(1)
	}
	cancel()

	rt := router.New(
		kv,
		router.NewResumeClient(resumeURL, nil), // P1: plain HTTP; P1.5 mTLS transport
		router.NewHeaderResolver(),             // P1: header identity; JWT at broker cutover (O4)
		http.DefaultTransport,                  // P1.5: SPIRE mTLS transport
		router.Options{WorkerPort: workerPort},
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("/", rt)

	srv := &http.Server{Addr: listenAddr, Handler: mux}

	// graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scancel()
		_ = srv.Shutdown(sctx)
	}()

	log.Info("router listening", "addr", listenAddr, "resume", resumeURL, "kv", kvAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("router server error", "err", err)
		os.Exit(1)
	}
}
