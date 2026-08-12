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
	"strconv"
	"sync"
	"syscall"
	"time"
)

type server struct {
	work     string        // base workdir (bundles, runtime root, image staging)
	rt       runtimeDriver // the sandbox runtime engine (gVisor today; see runtime.go)
	s3       *s3Store
	bucket   string
	podIP    string      // worker pod's routable IP (for nftables DNAT target)
	netns    bool        // whether the checkpointable veth/interior-netns path is active
	compress bool        // default: compress checkpoint pages (SANDBOXD_COMPRESS)
	region   string      // AWS region (for injected AWS_REGION)
	cred     *credVendor // per-session AWS credential vendor (nil if disabled)
	credPort int         // interior-gateway port the vendor listens on
	memLimit int64       // per-sandbox memory cgroup limit in bytes (0 = uncapped); worker-side agent OOM-protection
	mu       sync.Mutex
	sb       map[string]*sandbox     // sandboxId -> metadata
	hs       map[string]*healthState // sandboxId -> runtime health (not persisted)
}

// sandbox is the metadata needed to teleport: which image it is + its config
// digest + latest snapshot URI + port mappings. Persisted to <work>/meta/<id>.json.
type sandbox struct {
	ID       string    `json:"id"`
	Image    string    `json:"image"`
	Digest   string    `json:"digest"`
	Bundle   string    `json:"bundle"`
	Snapshot string    `json:"snapshot"` // s3 prefix of the latest checkpoint
	Ports    []portMap `json:"ports"`    // podIP:host -> interior:container
	Health   health    `json:"health"`   // restart policy + readiness probe + idle
	// Runtime + EngineVersion identify the engine that produced this sandbox's
	// snapshots (recorded so a restore can refuse a cross-runtime or incompatible-
	// version image). RunscVer is retained for backward-compat with metadata written
	// by older workers; new writes set EngineVersion (== RunscVer for gVisor).
	Runtime       string `json:"runtime,omitempty"`
	EngineVersion string `json:"engineVersion,omitempty"`
	RunscVer      string `json:"runscVersion"`
	IAMRoleARN    string `json:"iamRoleArn,omitempty"` // session's assumable role (cred vendor)
	CreatedAt     string `json:"createdAt"`
}

func main() {
	work := envOr("SANDBOXD_WORK", "/work")
	runscBin := envOr("SANDBOXD_RUNSC", "/usr/local/bin/runsc")
	bucket := os.Getenv("SANDBOXD_BUCKET")
	addr := envOr("SANDBOXD_ADDR", ":8090")
	gcEvery := envDuration("SANDBOXD_GC_INTERVAL", 5*time.Minute)
	os.MkdirAll(work, 0o755)

	// Reap orphaned children (gofer/sentry) — sandboxd is PID 1; unreaped zombies
	// pin container cgroups and make `runsc delete` stall/fail (substrate pattern).
	startReaper()

	s := &server{
		work:     work,
		rt:       newRuntimeDriver(runscBin, runtimeStateRoot(work)),
		bucket:   bucket,
		podIP:    os.Getenv("SANDBOXD_POD_IP"),          // set via downward API
		compress: os.Getenv("SANDBOXD_COMPRESS") != "0", // default ON (A/B: ~4x smaller, ~2x faster suspend); opt out with =0
		region:   os.Getenv("AWS_REGION"),
		credPort: int(envInt64("SANDBOXD_CRED_PORT", 8091)),
		memLimit: computeSandboxMemLimit(loadMemReserveConfig()),
		sb:       map[string]*sandbox{},
		hs:       map[string]*healthState{},
	}
	if bucket != "" {
		st, err := newS3(context.Background(), bucket)
		if err != nil {
			log.Printf("WARN: s3 init failed (checkpoint/restore to S3 disabled): %v", err)
		} else {
			s.s3 = st
		}
	}
	// Per-session AWS credential vendor (opt-in): enabled when a fleet token key is
	// configured. Serves temporary role credentials to the sandbox on the interior
	// gateway so the workload can assume its session's IAM role. See
	// docs/sandboxd/PRD/PRD-sandbox-iam-credentials.md.
	if key := os.Getenv("SANDBOXD_CRED_TOKEN_KEY"); key != "" {
		assume, err := stsAssumeFunc(context.Background())
		if err != nil {
			log.Printf("WARN: STS init failed (per-session IAM credentials disabled): %v", err)
		} else {
			s.cred = newCredVendor(assume, []byte(key))
			log.Printf("credential vendor enabled (%s:%d)", credVendorIP, s.credPort)
		}
	}
	// Networking: only the "sandbox" (netstack) mode supports the checkpointable
	// veth/interior-netns data path. With host-net there's no netns to build.
	if s.rt.networkMode() == "sandbox" && s.podIP != "" {
		if err := ensureInteriorNetNS(); err != nil {
			log.Printf("WARN: interior netns setup failed (no sandbox networking): %v", err)
		} else {
			s.netns = true
			log.Printf("networking: interior netns ready (podIP=%s, interiorIP=%s)", s.podIP, interiorIP)
			// Pin the credential-vendor IP on lo (pod netns) so the vendor can bind it
			// at boot, independent of the per-session veth.
			ensureCredVendorAddr()
		}
	} else {
		log.Printf("networking: sandbox data path disabled (network=%s podIP=%q)", s.rt.networkMode(), s.podIP)
	}

	// Reconcile in-memory state from persisted metadata + runsc on startup.
	s.reconcile()
	// Background GC of orphaned on-disk artifacts.
	go s.gcLoop(gcEvery)
	// Supervisor: liveness/readiness/restart + idle detection.
	go s.supervise(envDuration("SANDBOXD_SUPERVISE_INTERVAL", 10*time.Second))
	// Stream nested gVisor (sentry/gofer) debug logs to stdout so they show up in
	// `kubectl logs` (in addition to the /logs endpoint).
	go s.rt.streamLogsToStdout()

	mux := http.NewServeMux()
	mux.HandleFunc("/run", s.handleRun)
	mux.HandleFunc("/checkpoint", s.handleCheckpoint)
	mux.HandleFunc("/restore", s.handleRestore)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/sandbox", s.handleDelete)    // DELETE ?sandboxId=
	mux.HandleFunc("/sandboxes", s.handleList)    // GET : all tracked sandboxes
	mux.HandleFunc("/suspend", s.handleSuspend)   // POST : checkpoint->S3->free worker
	mux.HandleFunc("/reset", s.handleReset)       // POST : free worker WITHOUT checkpoint
	mux.HandleFunc("/capacity", s.handleCapacity) // GET : busy/idle for the scheduler
	mux.HandleFunc("/logs", s.handleLogs)         // GET ?sandboxId= : nested gVisor logs
	mux.HandleFunc("/metrics", s.handleMetrics)   // GET : basic counters
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		// "runsc" retained for back-compat with existing scrapers; "runtime" +
		// "engineVersion" are the runtime-neutral fields.
		writeJSON(w, 200, map[string]string{
			"runsc":         s.rt.version(),
			"runtime":       s.rt.runtimeName(),
			"engineVersion": s.rt.version(),
			"bucket":        bucket,
		})
	})

	// Dedicated PLAIN-HTTP health listener for kubelet probes. It is ALWAYS plain
	// (never mTLS) so liveness/readiness probes work regardless of the control-API
	// TLS mode — kubelet has no SVID. Serves only /healthz; carries no control API.
	// The worker pod's probes target this port, not :8090.
	healthAddr := envOr("SANDBOXD_HEALTH_ADDR", ":8092")
	{
		hmux := http.NewServeMux()
		hmux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
		healthSrv := &http.Server{Addr: healthAddr, Handler: hmux}
		go func() {
			if err := healthSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("WARN: health listener stopped: %v", err)
			}
		}()
	}

	srv := &http.Server{Addr: addr, Handler: withRequestLog(mux)}

	// Credential vendor listens ONLY on the interior gateway (reachable from this
	// worker's sandbox netns, never the pod network or outside). Separate listener
	// from the control API on :8090.
	if s.cred != nil && s.netns {
		credSrv := &http.Server{
			Addr:    fmt.Sprintf("%s:%d", credVendorIP, s.credPort),
			Handler: s.cred,
		}
		go func() {
			if err := credSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("WARN: credential vendor stopped: %v", err)
			}
		}()
	} else if s.cred != nil {
		log.Printf("WARN: credential vendor configured but sandbox networking is off; disabled")
		s.cred = nil
	}

	// Graceful shutdown: on SIGTERM/SIGINT, DRAIN-WAIT so the control plane can
	// checkpoint-on-terminate. We do NOT checkpoint here ourselves (the operator is
	// the sole KV writer and owns the session state machine); instead we keep the
	// HTTP server serving so the operator's /suspend can land, and only shut down
	// once the sandbox is gone (i.e. /suspend deleted it) or a drain deadline
	// elapses. The deadline stays comfortably under the pod's terminationGracePeriod
	// so kubelet never SIGKILLs us mid-checkpoint. If we hold no sandbox, exit fast.
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		deadline := envDuration("SANDBOXD_DRAIN_DEADLINE", 100*time.Second)
		log.Printf("shutdown: draining (deadline=%s, sandboxes=%d)", deadline, len(s.list()))
		dctx, dcancel := context.WithTimeout(context.Background(), deadline)
		defer dcancel()
		t := time.NewTicker(1 * time.Second)
		defer t.Stop()
		for len(s.list()) > 0 {
			select {
			case <-dctx.Done():
				log.Printf("shutdown: drain deadline reached with %d sandbox(es) still running; exiting", len(s.list()))
				goto stop
			case <-t.C:
			}
		}
		log.Printf("shutdown: no sandboxes remain; exiting")
	stop:
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	// P1.5: when SANDBOXD_MTLS=1, the control API requires the operator's client
	// SVID (SPIFFE mTLS). Off by default -> plain HTTP (rollout fallback). The
	// readiness probe must be tcpSocket (not httpGet) under mTLS, since kubelet has
	// no SVID — the operator sets that on the worker Deployment.
	mtls := os.Getenv("SANDBOXD_MTLS") == "1"
	if mtls {
		operatorID := envOr("SANDBOXD_SPIFFE_OPERATOR_ID", "spiffe://sandboxd/operator")
		cfg, closeSrc, err := mtlsServerConfig(os.Getenv("SPIFFE_ENDPOINT_SOCKET"), operatorID, 30*time.Second)
		if err != nil {
			log.Fatalf("mTLS init: %v", err)
		}
		defer closeSrc()
		srv.TLSConfig = cfg
		log.Printf("sandboxd listening on %s with SPIFFE mTLS (authorize caller=%s) (work=%s runtime=%s engine=%s bucket=%s gc=%s)",
			addr, operatorID, work, s.rt.runtimeName(), s.rt.version(), bucket, gcEvery)
		if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
		log.Printf("stopped")
		return
	}

	log.Printf("sandboxd listening on %s (work=%s runtime=%s engine=%s bucket=%s gc=%s)", addr, work, s.rt.runtimeName(), s.rt.version(), bucket, gcEvery)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	log.Printf("stopped")
}

// POST /run {image, cmd?, env?, sandboxId?}
func (s *server) handleRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Image      string    `json:"image"`
		Cmd        []string  `json:"cmd"`
		Env        []string  `json:"env"`
		SandboxID  string    `json:"sandboxId"`
		Ports      []portMap `json:"ports"`
		Health     health    `json:"health"`
		IAMRoleARN string    `json:"iamRoleArn"`
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
	// Per-session AWS credentials: register the role and inject the container-
	// credentials env so the sandbox's AWS SDK fetches from the interior vendor.
	req.Env = s.withCredEnv(id, req.IAMRoleARN, req.Env)
	lg := reqLogger(r, "run", id)
	bundle := filepath.Join(s.work, "bundles", id)
	rootfs := filepath.Join(bundle, "rootfs")

	lg("pulling image %s", req.Image)
	t0 := time.Now()
	ic, err := prepareRootfsContainerd(req.Image, rootfs, snapshotKey(id))
	if err != nil {
		lg("pull FAILED: %v", err)
		s.cleanupArtifacts(id)
		writeErr(w, 502, "pull: "+err.Error())
		return
	}
	lg("pulled+snapshot prepared in %s (digest=%s)", time.Since(t0), ic.Digest)
	// Networking: if ports requested + sandbox netstack path available, build the
	// veth/interior-netns and point the spec at it. Default host-port = container-port.
	netnsPath := ""
	if len(req.Ports) > 0 {
		if !s.netns {
			s.cleanupArtifacts(id)
			writeErr(w, 400, "ports requested but sandbox networking unavailable (need network=sandbox + SANDBOXD_POD_IP)")
			return
		}
		for i := range req.Ports {
			if req.Ports[i].Host == 0 {
				req.Ports[i].Host = req.Ports[i].Container
			}
		}
		// A driver that builds its OWN interior network (microVM) sets up the veth +
		// inbound DNAT + cred-vendor routing itself, inside createStart (from the ports
		// arg). Only build it here (and point the spec at the gVisor netns) for drivers
		// that don't (gVisor). See runtimeDriver.buildsOwnNetwork.
		if !s.rt.buildsOwnNetwork() {
			if err := setupSandboxNet(s.podIP, req.Ports); err != nil {
				lg("network setup FAILED: %v", err)
				s.cleanupArtifacts(id)
				writeErr(w, 500, "network: "+err.Error())
				return
			}
			// DNS (/etc/resolv.conf) was already written into the rootfs by
			// prepareRootfsContainerd on the correct mount-ns thread; nothing to do here.
			netnsPath = interiorNetNSPath
		}
		lg("network up: %s:%v -> %s", s.podIP, req.Ports, interiorIP)
	}
	if err := writeOCISpec(bundle, ic, req.Cmd, req.Env, netnsPath, s.memLimit); err != nil {
		teardownSandboxNet()
		s.cleanupArtifacts(id)
		s.dropCred(id)
		writeErr(w, 500, "spec: "+err.Error())
		return
	}
	lg("%s createStart", s.rt.runtimeName())
	if err := s.rt.createStart(id, bundle, req.Ports); err != nil {
		lg("runtime FAILED: %v", err)
		s.rt.delete(id) // clear any partial runtime state
		teardownSandboxNet()
		s.cleanupArtifacts(id)
		s.dropCred(id)
		writeErr(w, 500, "runtime: "+err.Error())
		return
	}
	st, _ := s.rt.state(id)
	lg("started, status=%s", st)
	s.put(&sandbox{ID: id, Image: req.Image, Digest: ic.Digest, Bundle: bundle,
		Ports: req.Ports, Health: req.Health,
		Runtime: s.rt.runtimeName(), EngineVersion: s.rt.version(), RunscVer: s.rt.version(),
		IAMRoleARN: req.IAMRoleARN,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339)})
	writeJSON(w, 200, map[string]any{"sandboxId": id, "status": st, "image": req.Image, "ports": req.Ports})
}

// POST /checkpoint {sandboxId, leaveRunning?}
func (s *server) handleCheckpoint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SandboxID    string `json:"sandboxId"`
		LeaveRunning bool   `json:"leaveRunning"`
		Compress     *bool  `json:"compress"` // nil => server default (SANDBOXD_COMPRESS)
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
	compress := s.compress
	if req.Compress != nil {
		compress = *req.Compress
	}
	lg("START (leaveRunning=%v compress=%v) image=%s", req.LeaveRunning, compress, sb.Image)
	imgDir := filepath.Join(s.work, "img", req.SandboxID)
	// clear any prior checkpoint image — runsc refuses to overwrite existing
	// checkpoint.img/pages.img ("file exists"), so a re-checkpoint must start clean.
	os.RemoveAll(imgDir)
	os.MkdirAll(imgDir, 0o755)
	// Persist the EXACT config.json used to run this sandbox alongside the
	// checkpoint (gVisor restore enforces spec match).
	if b, err := os.ReadFile(filepath.Join(sb.Bundle, "config.json")); err == nil {
		os.WriteFile(filepath.Join(imgDir, "config.json"), b, 0o644)
	}
	t0 := time.Now()
	if err := s.rt.checkpoint(req.SandboxID, imgDir, req.LeaveRunning, compress); err != nil {
		lg("checkpoint FAILED: %v", err)
		writeErr(w, 500, err.Error())
		return
	}
	sz := dirSize(imgDir)
	lg("checkpoint OK in %s (%d bytes)", time.Since(t0), sz)
	snapID := fmt.Sprintf("snap-%d", time.Now().UnixNano())
	prefix := fmt.Sprintf("sandboxes/%s/%s", req.SandboxID, snapID)
	lg("uploading %d bytes -> s3://%s/%s", sz, s.bucket, prefix)
	tu := time.Now()
	upCtx, upCancel := opCtx()
	upErr := s.s3.uploadDir(upCtx, imgDir, prefix)
	upCancel()
	if upErr != nil {
		lg("S3 upload FAILED: %v", upErr)
		writeErr(w, 502, "upload: "+upErr.Error())
		return
	}
	sb.Snapshot = prefix
	s.put(sb) // persist updated snapshot ref
	// If not leaving it running, tear down the veth/nftables (like substrate does
	// after checkpoint). If leaveRunning, keep networking so it stays reachable.
	if !req.LeaveRunning && len(sb.Ports) > 0 {
		teardownSandboxNet()
	}
	metrics.inc("checkpoints")
	lg("DONE: snapshot=%s uploaded in %s", prefix, time.Since(tu))
	writeJSON(w, 200, map[string]any{"sandboxId": req.SandboxID, "snapshot": prefix,
		"sizeBytes": sz, "image": sb.Image, "digest": sb.Digest,
		"runtime": s.rt.runtimeName(), "engineVersion": s.rt.version(), "runscVersion": sb.RunscVer})
}

// POST /restore {sandboxId, image, snapshot, runscVersion?}
func (s *server) handleRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SandboxID     string    `json:"sandboxId"`
		Image         string    `json:"image"`
		Snapshot      string    `json:"snapshot"`
		Runtime       string    `json:"runtime"`       // engine that produced the snapshot ("gvisor"|"microvm")
		EngineVersion string    `json:"engineVersion"` // engine version of the snapshot
		RunscVersion  string    `json:"runscVersion"`  // back-compat alias for EngineVersion (gVisor)
		Ports         []portMap `json:"ports"`
		Health        health    `json:"health"`
		IAMRoleARN    string    `json:"iamRoleArn"`
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
	// Restore guard, generalized to {runtime, version} (see checkRestoreCompat):
	// refuse a cross-runtime restore unconditionally, and an engine-version mismatch
	// within the same runtime. EngineVersion is the new field; RunscVersion is its
	// back-compat alias so older callers/snapshots keep working.
	if v := checkRestoreCompat(req.Runtime, req.EngineVersion, req.RunscVersion, s.rt.runtimeName(), s.rt.version()); !v.OK {
		writeJSON(w, 409, map[string]string{"error": v.Kind,
			"runtime": s.rt.runtimeName(), "want": v.Want, "have": v.Have})
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
	ic, err := prepareRootfsContainerd(req.Image, rootfs, snapshotKey(id))
	if err != nil {
		lg("pull FAILED: %v", err)
		s.cleanupArtifacts(id)
		writeErr(w, 502, "pull: "+err.Error())
		return
	}
	lg("base rootfs from %s in %s", req.Image, time.Since(tp))
	imgDir := filepath.Join(s.work, "img", id)
	td := time.Now()
	dlCtx, dlCancel := opCtx()
	dlErr := s.s3.downloadPrefix(dlCtx, req.Snapshot, imgDir)
	dlCancel()
	if dlErr != nil {
		lg("S3 download FAILED: %v", dlErr)
		s.cleanupArtifacts(id)
		writeErr(w, 502, "download: "+dlErr.Error())
		return
	}
	lg("downloaded checkpoint (%d bytes) in %s", dirSize(imgDir), time.Since(td))
	// Re-establish networking with the SAME interior IP so the restored sandbox is
	// reachable at the same podIP:hostPort (only the session reconnects).
	if len(req.Ports) > 0 {
		if !s.netns {
			s.cleanupArtifacts(id)
			writeErr(w, 400, "ports requested but sandbox networking unavailable")
			return
		}
		for i := range req.Ports {
			if req.Ports[i].Host == 0 {
				req.Ports[i].Host = req.Ports[i].Container
			}
		}
		// microVM rebuilds its own veth + DNAT + cred routing inside restore (from the
		// ports arg); gVisor's is built here. See runtimeDriver.buildsOwnNetwork.
		if !s.rt.buildsOwnNetwork() {
			if err := setupSandboxNet(s.podIP, req.Ports); err != nil {
				lg("network setup FAILED: %v", err)
				s.cleanupArtifacts(id)
				writeErr(w, 500, "network: "+err.Error())
				return
			}
		}
		// write resolv.conf into the freshly-rebuilt rootfs (the saved config.json
		// no longer bind-mounts it; DNS is a direct file in the rootfs).
		if err := writeResolvIntoRootfs(rootfs); err != nil {
			lg("WARN: resolv.conf: %v", err)
		}
		lg("network re-established: %s:%v -> %s", s.podIP, req.Ports, interiorIP)
	}
	// Use the EXACT config.json the checkpoint was made with; move it out of imgDir
	// (runsc would treat a stray file there as an image file).
	if moveConfigJSON(imgDir, bundle) {
		lg("using original spec from snapshot")
	} else {
		lg("WARN: no config.json in snapshot, rebuilding spec")
		netnsPath := ""
		if len(req.Ports) > 0 {
			netnsPath = interiorNetNSPath
		}
		if err := writeOCISpec(bundle, ic, nil, nil, netnsPath, s.memLimit); err != nil {
			s.cleanupArtifacts(id)
			writeErr(w, 500, "spec: "+err.Error())
			return
		}
	}
	// Re-register the session's IAM role with THIS worker's credential vendor before
	// the sandbox resumes. The AWS env is already baked into the restored config.json
	// (it travels in the checkpoint) and the per-session token is deterministic
	// (HMAC of the sid), so the same URI+token keep working — we only need to make
	// the new worker's vendor able to assume the role again. No env re-injection.
	if s.cred != nil && req.IAMRoleARN != "" {
		s.cred.register(id, req.IAMRoleARN)
	}
	tr := time.Now()
	if err := s.rt.restore(id, bundle, imgDir, req.Ports); err != nil {
		lg("restore FAILED: %v", err)
		s.rt.delete(id)
		teardownSandboxNet()
		s.cleanupArtifacts(id)
		s.dropCred(id)
		writeErr(w, 500, err.Error())
		return
	}
	st, _ := s.rt.state(id)
	metrics.inc("restores")
	lg("DONE in %s, status=%s", time.Since(tr), st)
	s.put(&sandbox{ID: id, Image: req.Image, Digest: ic.Digest, Bundle: bundle,
		Snapshot: req.Snapshot, Ports: req.Ports, Health: req.Health,
		Runtime: s.rt.runtimeName(), EngineVersion: s.rt.version(), RunscVer: s.rt.version(),
		IAMRoleARN: req.IAMRoleARN,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339)})
	writeJSON(w, 200, map[string]any{"sandboxId": id, "status": st, "restoredFrom": req.Snapshot, "ports": req.Ports})
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("sandboxId")
	if err := validateID(id); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	st, err := s.rt.state(id)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	hs := s.healthState(id)
	writeJSON(w, 200, map[string]any{"sandboxId": id, "status": st,
		"ready": hs.ready, "idle": hs.idle, "restarts": hs.restarts})
}

// POST /suspend {sandboxId} — checkpoint to S3, then free the worker (delete +
// cleanup). This is substrate's suspend-on-idle: state persists in S3, worker
// becomes reusable. Compose of checkpoint + delete.
func (s *server) handleSuspend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SandboxID string `json:"sandboxId"`
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
	if s.s3 == nil {
		writeErr(w, 503, "s3 not configured")
		return
	}
	lg := reqLogger(r, "suspend", req.SandboxID)
	imgDir := s.imgDir(req.SandboxID)
	os.RemoveAll(imgDir)
	os.MkdirAll(imgDir, 0o755)
	if b, err := os.ReadFile(filepath.Join(sb.Bundle, "config.json")); err == nil {
		os.WriteFile(filepath.Join(imgDir, "config.json"), b, 0o644)
	}
	if err := s.rt.checkpoint(req.SandboxID, imgDir, false, s.compress); err != nil {
		lg("checkpoint FAILED: %v", err)
		writeErr(w, 500, "checkpoint: "+err.Error())
		return
	}
	snapID := fmt.Sprintf("snap-%d", time.Now().UnixNano())
	prefix := fmt.Sprintf("sandboxes/%s/%s", req.SandboxID, snapID)
	suCtx, suCancel := opCtx()
	suErr := s.s3.uploadDir(suCtx, imgDir, prefix)
	suCancel()
	if suErr != nil {
		lg("upload FAILED: %v", suErr)
		writeErr(w, 502, "upload: "+suErr.Error())
		return
	}
	// free the worker
	s.rt.delete(req.SandboxID)
	if len(sb.Ports) > 0 {
		teardownSandboxNet()
	}
	s.cleanupArtifacts(req.SandboxID)
	s.forget(req.SandboxID)
	s.dropCred(req.SandboxID)
	metrics.inc("suspends")
	lg("SUSPENDED: snapshot=%s, worker freed", prefix)
	writeJSON(w, 200, map[string]any{"sandboxId": req.SandboxID, "snapshot": prefix,
		"image": sb.Image, "suspended": true,
		"runtime": s.rt.runtimeName(), "engineVersion": s.rt.version()})
}

// POST /reset {sandboxId} — free the worker WITHOUT checkpointing (discard state).
func (s *server) handleReset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SandboxID string `json:"sandboxId"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := validateID(req.SandboxID); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	sb := s.get(req.SandboxID)
	s.rt.delete(req.SandboxID)
	if sb != nil && len(sb.Ports) > 0 {
		teardownSandboxNet()
	}
	s.cleanupArtifacts(req.SandboxID)
	s.forget(req.SandboxID)
	s.dropCred(req.SandboxID)
	metrics.inc("resets")
	writeJSON(w, 200, map[string]any{"sandboxId": req.SandboxID, "reset": true})
}

// GET /capacity — is this worker busy? (one sandbox per worker). The scheduler
// uses this to find idle workers.
func (s *server) handleCapacity(w http.ResponseWriter, r *http.Request) {
	list := s.list()
	busy := len(list) > 0
	resp := map[string]any{"busy": busy, "count": len(list)}
	if busy {
		resp["sandboxId"] = list[0].ID
		hs := s.healthState(list[0].ID)
		resp["idle"] = hs.idle
	}
	writeJSON(w, 200, resp)
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("sandboxId")
	if err := validateID(id); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// best-effort delete of runsc state, then network, artifacts + metadata
	if err := s.rt.delete(id); err != nil {
		reqLogger(r, "delete", id)("runsc delete: %v (continuing cleanup)", err)
	}
	if sb := s.get(id); sb != nil && len(sb.Ports) > 0 {
		teardownSandboxNet()
	}
	s.cleanupArtifacts(id)
	s.forget(id)
	s.dropCred(id)
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
	fmt.Fprint(w, s.rt.recentLogs(64*1024))
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

// withCredEnv registers a session's assumable IAM role with the credential vendor
// (if enabled and a role is requested) and appends the AWS container-credentials
// env so the sandbox's SDK fetches temporary creds from the interior vendor. A
// no-op (returns env unchanged) when no vendor or no role. Used by /run and
// /restore so the credential path re-establishes identically after teleport.
func (s *server) withCredEnv(id, roleARN string, env []string) []string {
	if s.cred == nil || roleARN == "" {
		return env
	}
	s.cred.register(id, roleARN)
	return append(env, s.cred.awsEnvForSession(id, credVendorIP, s.credPort, s.region)...)
}

// dropCred deregisters a session from the credential vendor (on teardown).
func (s *server) dropCred(id string) {
	if s.cred != nil {
		s.cred.deregister(id)
	}
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
func envInt64(k string, d int64) int64 {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
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
