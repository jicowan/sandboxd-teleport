// sandboxd is the worker agent: a single static binary (scratch image) that runs,
// checkpoints, and restores arbitrary OCI images as nested gVisor sandboxes, and
// teleports their state between workers via S3. It talks to registries and S3 as
// libraries and drives a pinned runsc binary. No shell, no daemon, no host containerd.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type server struct {
	work   string // base workdir (bundles, runsc root, image staging)
	runsc  *runscDriver
	s3     *s3Store
	bucket string
	mu     sync.Mutex
	sb     map[string]*sandbox // sandboxId -> metadata
}

// sandbox is the metadata needed to teleport: which image it is + its config
// digest + latest snapshot URI. Persisted to <work>/meta/<id>.json (see state.go).
type sandbox struct {
	ID        string `json:"id"`
	Image     string `json:"image"`
	Digest    string `json:"digest"`
	Bundle    string `json:"bundle"`
	Snapshot  string `json:"snapshot"` // s3 prefix of the latest checkpoint
	RunscVer  string `json:"runscVersion"`
	CreatedAt string `json:"createdAt"`
}

func main() {
	work := envOr("SANDBOXD_WORK", "/work")
	runscBin := envOr("SANDBOXD_RUNSC", "/usr/local/bin/runsc")
	bucket := os.Getenv("SANDBOXD_BUCKET")
	addr := envOr("SANDBOXD_ADDR", ":8090")
	gcEvery := envDuration("SANDBOXD_GC_INTERVAL", 5*time.Minute)
	os.MkdirAll(work, 0o755)

	s := &server{
		work:   work,
		runsc:  newRunsc(runscBin, filepath.Join(work, "rt")),
		bucket: bucket,
		sb:     map[string]*sandbox{},
	}
	if bucket != "" {
		st, err := newS3(context.Background(), bucket)
		if err != nil {
			log.Printf("WARN: s3 init failed (checkpoint/restore to S3 disabled): %v", err)
		} else {
			s.s3 = st
		}
	}

	// Reconcile in-memory state from persisted metadata + runsc on startup.
	s.reconcile()
	// Background GC of orphaned on-disk artifacts.
	go s.gcLoop(gcEvery)
	// Stream nested gVisor (sentry/gofer) debug logs to stdout so they show up in
	// `kubectl logs` (in addition to the /logs endpoint).
	go s.runsc.tailToStdout()

	mux := http.NewServeMux()
	mux.HandleFunc("/run", s.handleRun)
	mux.HandleFunc("/checkpoint", s.handleCheckpoint)
	mux.HandleFunc("/restore", s.handleRestore)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/sandbox", s.handleDelete)      // DELETE ?sandboxId=
	mux.HandleFunc("/sandboxes", s.handleList)      // GET : all tracked sandboxes
	mux.HandleFunc("/logs", s.handleLogs)           // GET ?sandboxId= : nested gVisor logs
	mux.HandleFunc("/metrics", s.handleMetrics)     // GET : basic counters
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]string{"runsc": s.runsc.version(), "bucket": bucket})
	})

	srv := &http.Server{Addr: addr, Handler: withRequestLog(mux)}

	// Graceful shutdown: on SIGTERM/SIGINT, stop accepting, drain, exit. We do NOT
	// checkpoint-on-shutdown here (that's the control plane's decision); we just
	// stop cleanly so kubelet's termination is orderly.
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Printf("shutdown: draining")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	log.Printf("sandboxd listening on %s (work=%s runsc=%s bucket=%s gc=%s)", addr, work, runscBin, bucket, gcEvery)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	log.Printf("stopped")
}

// POST /run {image, cmd?, env?, sandboxId?}
func (s *server) handleRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Image     string   `json:"image"`
		Cmd       []string `json:"cmd"`
		Env       []string `json:"env"`
		SandboxID string   `json:"sandboxId"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Image == "" {
		writeErr(w, 400, "image required")
		return
	}
	id, err := ensureID(req.SandboxID)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if s.get(id) != nil {
		writeErr(w, 409, "sandbox already exists: "+id)
		return
	}
	lg := reqLogger(r, "run", id)
	bundle := filepath.Join(s.work, "bundles", id)
	rootfs := filepath.Join(bundle, "rootfs")

	lg("pulling image %s", req.Image)
	t0 := time.Now()
	ic, err := pullAndFlatten(req.Image, rootfs)
	if err != nil {
		lg("pull FAILED: %v", err)
		s.cleanupArtifacts(id)
		writeErr(w, 502, "pull: "+err.Error())
		return
	}
	lg("pulled+flattened in %s (digest=%s)", time.Since(t0), ic.Digest)
	if err := writeOCISpec(bundle, ic, req.Cmd, req.Env); err != nil {
		s.cleanupArtifacts(id)
		writeErr(w, 500, "spec: "+err.Error())
		return
	}
	lg("runsc run -detach")
	if err := s.runsc.createStart(id, bundle); err != nil {
		lg("runsc FAILED: %v", err)
		s.runsc.delete(id) // clear any partial runsc state
		s.cleanupArtifacts(id)
		writeErr(w, 500, "runsc: "+err.Error())
		return
	}
	st, _ := s.runsc.state(id)
	lg("started, status=%s", st)
	s.put(&sandbox{ID: id, Image: req.Image, Digest: ic.Digest, Bundle: bundle,
		RunscVer: s.runsc.version(), CreatedAt: time.Now().UTC().Format(time.RFC3339)})
	writeJSON(w, 200, map[string]string{"sandboxId": id, "status": st, "image": req.Image})
}

// POST /checkpoint {sandboxId, leaveRunning?}
func (s *server) handleCheckpoint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SandboxID    string `json:"sandboxId"`
		LeaveRunning bool   `json:"leaveRunning"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := validateID(req.SandboxID); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	sb := s.get(req.SandboxID)
	if sb == nil {
		writeErr(w, 404, "unknown sandbox")
		return
	}
	lg := reqLogger(r, "checkpoint", req.SandboxID)
	if s.s3 == nil {
		writeErr(w, 503, "s3 not configured")
		return
	}
	lg("START (leaveRunning=%v) image=%s", req.LeaveRunning, sb.Image)
	imgDir := filepath.Join(s.work, "img", req.SandboxID)
	os.MkdirAll(imgDir, 0o755)
	// Persist the EXACT config.json used to run this sandbox alongside the
	// checkpoint (gVisor restore enforces spec match).
	if b, err := os.ReadFile(filepath.Join(sb.Bundle, "config.json")); err == nil {
		os.WriteFile(filepath.Join(imgDir, "config.json"), b, 0o644)
	}
	t0 := time.Now()
	if err := s.runsc.checkpoint(req.SandboxID, imgDir, req.LeaveRunning); err != nil {
		lg("runsc checkpoint FAILED: %v", err)
		writeErr(w, 500, err.Error())
		return
	}
	sz := dirSize(imgDir)
	lg("runsc checkpoint OK in %s (%d bytes)", time.Since(t0), sz)
	snapID := fmt.Sprintf("snap-%d", time.Now().UnixNano())
	prefix := fmt.Sprintf("sandboxes/%s/%s", req.SandboxID, snapID)
	lg("uploading %d bytes -> s3://%s/%s", sz, s.bucket, prefix)
	tu := time.Now()
	if err := s.s3.uploadDir(r.Context(), imgDir, prefix); err != nil {
		lg("S3 upload FAILED: %v", err)
		writeErr(w, 502, "upload: "+err.Error())
		return
	}
	sb.Snapshot = prefix
	s.put(sb) // persist updated snapshot ref
	metrics.inc("checkpoints")
	lg("DONE: snapshot=%s uploaded in %s", prefix, time.Since(tu))
	writeJSON(w, 200, map[string]any{"sandboxId": req.SandboxID, "snapshot": prefix,
		"sizeBytes": sz, "image": sb.Image, "digest": sb.Digest, "runscVersion": sb.RunscVer})
}

// POST /restore {sandboxId, image, snapshot, runscVersion?}
func (s *server) handleRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SandboxID    string `json:"sandboxId"`
		Image        string `json:"image"`
		Snapshot     string `json:"snapshot"`
		RunscVersion string `json:"runscVersion"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Image == "" || req.Snapshot == "" {
		writeErr(w, 400, "image and snapshot required")
		return
	}
	id, err := ensureID(req.SandboxID)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if s.get(id) != nil {
		writeErr(w, 409, "sandbox already exists: "+id)
		return
	}
	if req.RunscVersion != "" && req.RunscVersion != s.runsc.version() {
		writeJSON(w, 409, map[string]string{"error": "runsc version mismatch",
			"want": req.RunscVersion, "have": s.runsc.version()})
		return
	}
	if s.s3 == nil {
		writeErr(w, 503, "s3 not configured")
		return
	}
	lg := reqLogger(r, "restore", id)
	lg("START from snapshot=%s image=%s", req.Snapshot, req.Image)
	bundle := filepath.Join(s.work, "bundles", id)
	rootfs := filepath.Join(bundle, "rootfs")
	tp := time.Now()
	ic, err := pullAndFlatten(req.Image, rootfs)
	if err != nil {
		lg("pull FAILED: %v", err)
		s.cleanupArtifacts(id)
		writeErr(w, 502, "pull: "+err.Error())
		return
	}
	lg("base rootfs from %s in %s", req.Image, time.Since(tp))
	imgDir := filepath.Join(s.work, "img", id)
	td := time.Now()
	if err := s.s3.downloadPrefix(r.Context(), req.Snapshot, imgDir); err != nil {
		lg("S3 download FAILED: %v", err)
		s.cleanupArtifacts(id)
		writeErr(w, 502, "download: "+err.Error())
		return
	}
	lg("downloaded checkpoint (%d bytes) in %s", dirSize(imgDir), time.Since(td))
	// Use the EXACT config.json the checkpoint was made with; move it out of imgDir
	// (runsc would treat a stray file there as an image file).
	cfgSrc := filepath.Join(imgDir, "config.json")
	if b, err := os.ReadFile(cfgSrc); err == nil {
		os.WriteFile(filepath.Join(bundle, "config.json"), b, 0o644)
		os.Remove(cfgSrc)
		lg("using original spec from snapshot")
	} else {
		lg("WARN: no config.json in snapshot, rebuilding spec: %v", err)
		if err := writeOCISpec(bundle, ic, nil, nil); err != nil {
			s.cleanupArtifacts(id)
			writeErr(w, 500, "spec: "+err.Error())
			return
		}
	}
	tr := time.Now()
	if err := s.runsc.restore(id, bundle, imgDir); err != nil {
		lg("runsc restore FAILED: %v", err)
		s.runsc.delete(id)
		s.cleanupArtifacts(id)
		writeErr(w, 500, err.Error())
		return
	}
	st, _ := s.runsc.state(id)
	metrics.inc("restores")
	lg("DONE in %s, status=%s", time.Since(tr), st)
	s.put(&sandbox{ID: id, Image: req.Image, Digest: ic.Digest, Bundle: bundle,
		Snapshot: req.Snapshot, RunscVer: s.runsc.version(),
		CreatedAt: time.Now().UTC().Format(time.RFC3339)})
	writeJSON(w, 200, map[string]string{"sandboxId": id, "status": st, "restoredFrom": req.Snapshot})
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("sandboxId")
	if err := validateID(id); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	st, err := s.runsc.state(id)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"sandboxId": id, "status": st})
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("sandboxId")
	if err := validateID(id); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// best-effort delete of runsc state, then artifacts + metadata
	if err := s.runsc.delete(id); err != nil {
		reqLogger(r, "delete", id)("runsc delete: %v (continuing cleanup)", err)
	}
	s.cleanupArtifacts(id)
	s.forget(id)
	writeJSON(w, 200, map[string]string{"sandboxId": id, "deleted": "true"})
}

func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"sandboxes": s.list()})
}

// GET /logs?sandboxId= — the nested gVisor sentry/gofer debug logs + launch log.
func (s *server) handleLogs(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("sandboxId")
	if err := validateID(id); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	if sb := s.get(id); sb != nil {
		for _, name := range []string{id + ".run.log", id + ".restore.log"} {
			if b, err := os.ReadFile(filepath.Join(sb.Bundle, name)); err == nil {
				fmt.Fprintf(w, "==== %s ====\n%s\n", name, string(b))
			}
		}
	}
	fmt.Fprint(w, s.runsc.tailGvisorLogs(64*1024))
}

func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	m := metrics.snapshot()
	m["tracked_sandboxes"] = int64(len(s.list()))
	writeJSON(w, 200, m)
}

// dirSize sums file sizes in a directory (checkpoint image size).
func dirSize(dir string) int64 {
	var n int64
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if fi, err := e.Info(); err == nil {
			n += fi.Size()
		}
	}
	return n
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func envDuration(k string, d time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if p, err := time.ParseDuration(v); err == nil {
			return p
		}
	}
	return d
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, 400, "bad json: "+err.Error())
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
