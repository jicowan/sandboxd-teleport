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
	"path/filepath"
	"sync"
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
// digest + latest snapshot URI. In production this lives in the control-plane store.
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

	mux := http.NewServeMux()
	mux.HandleFunc("/run", s.handleRun)
	mux.HandleFunc("/checkpoint", s.handleCheckpoint)
	mux.HandleFunc("/restore", s.handleRestore)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/sandbox", s.handleDelete) // DELETE
	mux.HandleFunc("/logs", s.handleLogs)      // GET ?sandboxId= : nested gVisor (sentry/gofer) logs
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]string{"runsc": s.runsc.version(), "bucket": bucket})
	})

	log.Printf("sandboxd listening on %s (work=%s runsc=%s bucket=%s)", addr, work, runscBin, bucket)
	log.Fatal(http.ListenAndServe(addr, mux))
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
		writeJSON(w, 400, map[string]string{"error": "image required"})
		return
	}
	id := req.SandboxID
	if id == "" {
		id = fmt.Sprintf("sb-%d", time.Now().UnixNano())
	}
	bundle := filepath.Join(s.work, "bundles", id)
	rootfs := filepath.Join(bundle, "rootfs")

	log.Printf("[run %s] pulling image %s", id, req.Image)
	t0 := time.Now()
	ic, err := pullAndFlatten(req.Image, rootfs)
	if err != nil {
		log.Printf("[run %s] pull FAILED: %v", id, err)
		writeJSON(w, 500, map[string]string{"error": "pull: " + err.Error()})
		return
	}
	log.Printf("[run %s] pulled+flattened in %s (digest=%s)", id, time.Since(t0), ic.Digest)
	if err := writeOCISpec(bundle, ic, req.Cmd, req.Env); err != nil {
		writeJSON(w, 500, map[string]string{"error": "spec: " + err.Error()})
		return
	}
	log.Printf("[run %s] runsc create+start", id)
	if err := s.runsc.createStart(id, bundle); err != nil {
		log.Printf("[run %s] runsc FAILED: %v", id, err)
		writeJSON(w, 500, map[string]string{"error": "runsc: " + err.Error()})
		return
	}
	st, _ := s.runsc.state(id)
	log.Printf("[run %s] started, status=%s", id, st)
	s.put(&sandbox{ID: id, Image: req.Image, Digest: ic.Digest, Bundle: bundle,
		RunscVer: s.runsc.version(), CreatedAt: time.Now().UTC().Format(time.RFC3339)})
	writeJSON(w, 200, map[string]string{"sandboxId": id, "status": st, "image": req.Image})
}

// POST /checkpoint {sandboxId}
func (s *server) handleCheckpoint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SandboxID    string `json:"sandboxId"`
		LeaveRunning bool   `json:"leaveRunning"`
	}
	if !decode(w, r, &req) {
		return
	}
	sb := s.get(req.SandboxID)
	if sb == nil {
		writeJSON(w, 404, map[string]string{"error": "unknown sandbox"})
		return
	}
	log.Printf("[checkpoint %s] START (leaveRunning=%v) image=%s", req.SandboxID, req.LeaveRunning, sb.Image)
	imgDir := filepath.Join(s.work, "img", req.SandboxID)
	os.MkdirAll(imgDir, 0o755)
	// Persist the EXACT config.json used to run this sandbox alongside the
	// checkpoint. gVisor restore enforces spec match (RestoreSpecValidation=
	// enforce), so the restoring worker must reuse this identical spec, not
	// rebuild one from the image defaults.
	if b, err := os.ReadFile(filepath.Join(sb.Bundle, "config.json")); err == nil {
		os.WriteFile(filepath.Join(imgDir, "config.json"), b, 0o644)
	}
	t0 := time.Now()
	if err := s.runsc.checkpoint(req.SandboxID, imgDir, req.LeaveRunning); err != nil {
		log.Printf("[checkpoint %s] runsc checkpoint FAILED: %v", req.SandboxID, err)
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	sz := dirSize(imgDir)
	log.Printf("[checkpoint %s] runsc checkpoint OK in %s (image %s, %d bytes)", req.SandboxID, time.Since(t0), imgDir, sz)
	snapID := fmt.Sprintf("snap-%d", time.Now().UnixNano())
	prefix := fmt.Sprintf("sandboxes/%s/%s", req.SandboxID, snapID)
	if s.s3 == nil {
		log.Printf("[checkpoint %s] S3 NOT CONFIGURED", req.SandboxID)
		writeJSON(w, 500, map[string]string{"error": "s3 not configured"})
		return
	}
	log.Printf("[checkpoint %s] uploading %d bytes -> s3://%s/%s", req.SandboxID, sz, s.bucket, prefix)
	tu := time.Now()
	if err := s.s3.uploadDir(r.Context(), imgDir, prefix); err != nil {
		log.Printf("[checkpoint %s] S3 upload FAILED: %v", req.SandboxID, err)
		writeJSON(w, 500, map[string]string{"error": "upload: " + err.Error()})
		return
	}
	sb.Snapshot = prefix
	log.Printf("[checkpoint %s] DONE: snapshot=%s uploaded in %s", req.SandboxID, prefix, time.Since(tu))
	writeJSON(w, 200, map[string]any{"sandboxId": req.SandboxID, "snapshot": prefix,
		"sizeBytes": sz, "image": sb.Image, "digest": sb.Digest, "runscVersion": sb.RunscVer})
}

// POST /restore {sandboxId, image, snapshot, digest?, runscVersion?}
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
		writeJSON(w, 400, map[string]string{"error": "image and snapshot required"})
		return
	}
	// refuse a runsc version mismatch (restore would hard-fail anyway)
	if req.RunscVersion != "" && req.RunscVersion != s.runsc.version() {
		writeJSON(w, 409, map[string]string{"error": "runsc version mismatch",
			"want": req.RunscVersion, "have": s.runsc.version()})
		return
	}
	id := req.SandboxID
	if id == "" {
		id = fmt.Sprintf("sb-%d", time.Now().UnixNano())
	}
	log.Printf("[restore %s] START from snapshot=%s image=%s", id, req.Snapshot, req.Image)
	// 1) rebuild the SAME base rootfs from the image (base never travels via S3)
	bundle := filepath.Join(s.work, "bundles", id)
	rootfs := filepath.Join(bundle, "rootfs")
	tp := time.Now()
	ic, err := pullAndFlatten(req.Image, rootfs)
	if err != nil {
		log.Printf("[restore %s] pull FAILED: %v", id, err)
		writeJSON(w, 500, map[string]string{"error": "pull: " + err.Error()})
		return
	}
	log.Printf("[restore %s] base rootfs from %s in %s", id, req.Image, time.Since(tp))
	// 2) download the checkpoint from S3 (includes the original config.json)
	imgDir := filepath.Join(s.work, "img", id)
	if s.s3 == nil {
		log.Printf("[restore %s] S3 NOT CONFIGURED", id)
		writeJSON(w, 500, map[string]string{"error": "s3 not configured"})
		return
	}
	td := time.Now()
	if err := s.s3.downloadPrefix(r.Context(), req.Snapshot, imgDir); err != nil {
		log.Printf("[restore %s] S3 download FAILED: %v", id, err)
		writeJSON(w, 500, map[string]string{"error": "download: " + err.Error()})
		return
	}
	log.Printf("[restore %s] downloaded checkpoint (%d bytes) in %s", id, dirSize(imgDir), time.Since(td))
	// Use the EXACT config.json the checkpoint was made with (restore enforces
	// spec match). It was uploaded alongside the images; move it into the bundle
	// and out of imgDir (runsc would otherwise treat it as a stray image file).
	cfgSrc := filepath.Join(imgDir, "config.json")
	if b, err := os.ReadFile(cfgSrc); err == nil {
		os.WriteFile(filepath.Join(bundle, "config.json"), b, 0o644)
		os.Remove(cfgSrc)
		log.Printf("[restore %s] using original spec from snapshot", id)
	} else {
		// fallback: rebuild from image defaults (may fail spec validation)
		log.Printf("[restore %s] WARN: no config.json in snapshot, rebuilding spec: %v", id, err)
		if err := writeOCISpec(bundle, ic, nil, nil); err != nil {
			writeJSON(w, 500, map[string]string{"error": "spec: " + err.Error()})
			return
		}
	}
	// 3) create + restore
	tr := time.Now()
	if err := s.runsc.restore(id, bundle, imgDir); err != nil {
		log.Printf("[restore %s] runsc restore FAILED: %v", id, err)
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	st, _ := s.runsc.state(id)
	log.Printf("[restore %s] DONE in %s, status=%s", id, time.Since(tr), st)
	s.put(&sandbox{ID: id, Image: req.Image, Digest: ic.Digest, Bundle: bundle,
		Snapshot: req.Snapshot, RunscVer: s.runsc.version(),
		CreatedAt: time.Now().UTC().Format(time.RFC3339)})
	writeJSON(w, 200, map[string]string{"sandboxId": id, "status": st, "restoredFrom": req.Snapshot})
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("sandboxId")
	st, err := s.runsc.state(id)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"sandboxId": id, "status": st})
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("sandboxId")
	if err := s.runsc.delete(id); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.mu.Lock()
	delete(s.sb, id)
	s.mu.Unlock()
	writeJSON(w, 200, map[string]string{"sandboxId": id, "deleted": "true"})
}

// GET /logs?sandboxId= — surface the nested gVisor container's own sentry/gofer
// debug logs (and the run/restore launch log) in the parent, as plain text.
func (s *server) handleLogs(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("sandboxId")
	w.Header().Set("Content-Type", "text/plain")
	// per-sandbox launch log (run/restore) if present
	if sb := s.get(id); sb != nil {
		for _, name := range []string{id + ".run.log", id + ".restore.log"} {
			if b, err := os.ReadFile(filepath.Join(sb.Bundle, name)); err == nil {
				fmt.Fprintf(w, "==== %s ====\n%s\n", name, string(b))
			}
		}
	}
	// shared sentry/gofer debug logs for this worker's runsc root
	fmt.Fprint(w, s.runsc.tailGvisorLogs(64*1024))
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

func (s *server) put(sb *sandbox) { s.mu.Lock(); s.sb[sb.ID] = sb; s.mu.Unlock() }
func (s *server) get(id string) *sandbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sb[id]
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json: " + err.Error()})
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
