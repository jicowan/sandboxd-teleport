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

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"os"
	"strconv"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	corev1alpha1 "github.com/jicowan/aio-sandbox/controlplane/api/v1alpha1"
	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/controlplane/internal/controller"
	"github.com/jicowan/aio-sandbox/controlplane/internal/resume"
	// +kubebuilder:scaffold:imports
)

// envOr returns the value of env var key, or def if unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envInt returns the int value of env var key, or def if unset/unparseable.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(corev1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var kvAddr string
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(&kvAddr, "kv-addr", envOr("SANDBOXD_KV_ADDR", "valkey:6379"),
		"Valkey/Redis address (host:port) for the assignment table.")
	var resumeAddr, resumeNamespace string
	flag.StringVar(&resumeAddr, "resume-addr", envOr("SANDBOXD_RESUME_ADDR", ":8082"),
		"Address for the internal /resume endpoint the router calls.")
	flag.StringVar(&resumeNamespace, "resume-namespace", envOr("SANDBOXD_NAMESPACE", "sandboxd-controlplane-system"),
		"Namespace where SandboxTemplate/WarmPool/Session objects live.")
	var workerSA, workerBucket, workerRegion, workerImage, credTokenSecret string
	flag.StringVar(&workerSA, "worker-sa", envOr("SANDBOXD_WORKER_SA", ""),
		"ServiceAccount for provisioned worker pods (S3 Pod Identity).")
	flag.StringVar(&workerBucket, "worker-bucket", envOr("SANDBOXD_BUCKET", ""),
		"S3 bucket for worker checkpoints.")
	flag.StringVar(&workerRegion, "worker-region", envOr("AWS_REGION", ""),
		"AWS region for worker pods.")
	flag.StringVar(&workerImage, "worker-image", envOr("SANDBOXD_WORKER_IMAGE", ""),
		"sandboxd worker image (overrides the built-in default).")
	flag.StringVar(&credTokenSecret, "cred-token-secret", envOr("SANDBOXD_CRED_TOKEN_SECRET", ""),
		"Secret name (key 'token') holding the fleet HMAC key for the per-session IAM credential vendor; empty disables it.")
	var maxConcurrentResumes int
	flag.IntVar(&maxConcurrentResumes, "max-concurrent-resumes", envInt("SANDBOXD_MAX_CONCURRENT_RESUMES", 0),
		"Cap on in-flight resumes (backpressure); 0 = unlimited (default).")
	var resumeDeadlineSec int
	flag.IntVar(&resumeDeadlineSec, "resume-deadline-seconds", envInt("SANDBOXD_RESUME_DEADLINE_SECONDS", 90),
		"Max time for a resume (cold start/restore) to reach ready. Large images (AIO) need >default.")
	var enableGC bool
	var gcIntervalSec int
	flag.BoolVar(&enableGC, "enable-checkpoint-gc", envOr("SANDBOXD_ENABLE_GC", "") == "1",
		"Enable S3 checkpoint GC (TTL + orphans). Off by default (deletes data; needs scoped S3 role).")
	flag.IntVar(&gcIntervalSec, "checkpoint-gc-interval-seconds", envInt("SANDBOXD_GC_INTERVAL_SECONDS", 300),
		"Checkpoint GC sweep interval in seconds.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		// Scope the Pod informer to sandboxd worker pods at the WATCH level so the
		// API server only streams worker pods to this operator — cluster-wide pod
		// churn of other pods never reaches our cache. (A reconcile predicate would
		// filter events only, not the ListWatch; the cache selector filters both.)
		// NOTE: this means the operator's cached client can only read worker pods;
		// it does not need any other pod.
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Pod{}: {
					Label: labels.SelectorFromSet(labels.Set{
						controller.LabelApp: controller.LabelAppWorker,
					}),
				},
			},
		},
		LeaderElection:   enableLeaderElection,
		LeaderElectionID: "4b8d1bd5.sandboxd.io",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// Assignment table (Valkey). The operator is the sole KV writer (TDD §4).
	kv := assign.New(kvAddr)
	defer kv.Close()
	if err := kv.Ping(context.Background()); err != nil {
		setupLog.Error(err, "Failed to reach Valkey assignment table", "addr", kvAddr)
		os.Exit(1)
	}
	setupLog.Info("connected to assignment table", "addr", kvAddr)

	if workerImage != "" {
		controller.WorkerImage = workerImage
	}
	// Pool notifier: resume/suspend push the changed pool name here; the WarmPool
	// controller watches it to refresh idle/busy status in near-real-time (O(1)
	// per change, event-driven — no polling).
	poolNotifier := controller.NewPoolNotifier(resumeNamespace, 256)
	if err := (&controller.WarmPoolReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		KV:     kv,
		Worker: controller.WorkerConfig{
			ServiceAccount:  workerSA,
			Bucket:          workerBucket,
			Region:          workerRegion,
			CredTokenSecret: credTokenSecret,
		},
		PoolEvents: poolNotifier.Events(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "warmpool")
		os.Exit(1)
	}
	discovery := &controller.WorkerDiscoveryReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		KV:        kv,
		Namespace: resumeNamespace,
	}
	if err := discovery.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "worker-discovery")
		os.Exit(1)
	}
	// Periodic reconciliation: prune KV worker entries whose pods no longer exist
	// (catches deletes missed while the operator was down). TDD §4.2.
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		return discovery.StartPruneLoop(ctx, 0)
	})); err != nil {
		setupLog.Error(err, "Failed to add worker prune loop")
		os.Exit(1)
	}

	// Internal /resume endpoint (control-plane half of the request path, TDD §5.1).
	// P1: plain HTTP; P1.5 wraps with SPIRE mTLS. Uses the manager's cached client.
	resumeWF := controller.BuildResumeWorkflow(mgr.GetClient(), kv, resumeNamespace, nil,
		resume.Options{
			MaxConcurrentResumes: maxConcurrentResumes,
			ResumeDeadline:       time.Duration(resumeDeadlineSec) * time.Second,
		}).WithNotifier(poolNotifier)
	if err := controller.AddResumeServer(mgr, resumeAddr, resume.NewHandler(resumeWF)); err != nil {
		setupLog.Error(err, "Failed to add resume server")
		os.Exit(1)
	}

	// Idle-suspend sweeper (P2): periodically checkpoint idle Running sessions to
	// S3 and free their workers. The teleport completes when a later request hits
	// the router and the resume path restores from the recorded snapshot (P3).
	suspender := controller.BuildSuspender(mgr.GetClient(), kv, resumeNamespace, nil).WithNotifier(poolNotifier)
	if err := controller.AddSuspendSweeper(mgr, suspender, 0); err != nil {
		setupLog.Error(err, "Failed to add suspend sweeper")
		os.Exit(1)
	}
	// Checkpoint-on-terminate: when a busy worker's pod enters Terminating, the
	// discovery reconciler drives this suspender to checkpoint the session to S3
	// before the pod dies (safe scale-in / drain). Wired here since the suspender is
	// built after discovery; SetupWithManager only registers, so the field is read
	// once the manager starts.
	discovery.TerminateSuspender = suspender

	// Periodic background checkpoints (P5, opt-in per template): checkpoint
	// long-lived Running sessions in place so a worker crash loses at most the
	// interval, not everything since the last idle-suspend.
	checkpointer := controller.BuildCheckpointer(mgr.GetClient(), kv, resumeNamespace, nil)
	if err := controller.AddCheckpointSweeper(mgr, checkpointer, 0); err != nil {
		setupLog.Error(err, "Failed to add checkpoint sweeper")
		os.Exit(1)
	}

	// Checkpoint GC (P4): reap TTL-expired + orphaned S3 checkpoints. Opt-in
	// (deletes data) and needs the operator's scoped S3 role (list+delete on
	// sandboxes/* only; worker keeps read/write, no delete).
	if enableGC {
		if workerBucket == "" {
			setupLog.Error(nil, "checkpoint GC enabled but no --worker-bucket set")
			os.Exit(1)
		}
		col, err := controller.BuildCollector(context.Background(), mgr.GetClient(), kv, resumeNamespace, workerBucket)
		if err != nil {
			setupLog.Error(err, "Failed to build checkpoint collector")
			os.Exit(1)
		}
		if err := controller.AddGCSweeper(mgr, col, time.Duration(gcIntervalSec)*time.Second); err != nil {
			setupLog.Error(err, "Failed to add GC sweeper")
			os.Exit(1)
		}
		setupLog.Info("checkpoint GC enabled", "bucket", workerBucket, "intervalSec", gcIntervalSec)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
