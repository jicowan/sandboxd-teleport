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

package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	corev1alpha1 "github.com/jicowan/aio-sandbox/controlplane/api/v1alpha1"
	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/controlplane/internal/metrics"
	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

// Labels the operator stamps on worker pods so the discovery informer (TDD §4.3)
// and the pool selector can find them.
const (
	LabelApp       = "sandboxd.io/app"  // = "worker"
	LabelPool      = "sandboxd.io/pool" // = <WarmPool name>
	LabelAppWorker = "worker"
)

// podDeletionCostAnnotation and the idle/busy cost values drive graceful
// scale-in: the built-in ReplicaSet controller deletes the LOWEST-cost pods
// first when it scales a Deployment down. By stamping idle workers with a low
// cost and busy workers (holding a live session) with a high cost, a scale-in
// preferentially removes idle workers and spares busy ones. This is a
// best-effort preference, not a guarantee — if a scale-in removes more pods than
// there are idle workers, busy ones can still be deleted (that lossless case is
// covered separately by checkpoint-on-terminate).
const (
	podDeletionCostAnnotation = "controller.kubernetes.io/pod-deletion-cost"
	deletionCostIdle          = "0"   // delete idle workers first
	deletionCostBusy          = "100" // keep busy workers
)

// WorkerImage is the sandboxd worker image the pool provisions. Workers are
// generic: the sandbox image is supplied per-session at /run time, not here.
// Configurable so we don't hardcode a registry in code.
var WorkerImage = "820537372947.dkr.ecr.us-west-2.amazonaws.com/sandboxd:v40"

// WorkerTerminationGracePeriodSeconds is the pod termination grace period for
// worker pods. It must exceed the worst-case checkpoint-on-terminate time
// (checkpoint + S3 upload) so kubelet doesn't SIGKILL a worker mid-checkpoint
// when its pod is deleted (scale-in, drain, rollout). The worker's own drain
// deadline (SANDBOXD_DRAIN_DEADLINE, default 100s) stays under this. 0 uses the
// k8s default (30s) — too short for large sessions.
var WorkerTerminationGracePeriodSeconds int64 = 120

// WorkerConfig carries the deployment-time settings for provisioned worker pods
// that aren't part of the WarmPool API (cluster-specific: S3 identity + bucket).
// Defaults match the proven static worker-deploy.yaml.
type WorkerConfig struct {
	ServiceAccount string // Pod Identity SA for S3 (checkpoint/restore)
	Bucket         string // SANDBOXD_BUCKET
	Region         string // AWS_REGION
}

// WarmPoolReconciler reconciles a WarmPool object into a Deployment of sandboxd
// worker pods (TDD §3.2, P0). It reconciles to spec.replicas; minIdle-driven
// autoscaling is deferred to a later phase.
type WarmPoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// KV is the assignment table, used only to compute idle/busy status counts.
	// Optional: nil disables status counts (useful in envtest without Valkey).
	KV *assign.Client
	// Worker configures provisioned worker pods (SA, bucket, region).
	Worker WorkerConfig
	// PoolEvents is the channel the resume/suspend paths write pool names to when
	// they change worker busy/idle state; the controller watches it to reconcile
	// the affected pool immediately (near-real-time status without polling).
	PoolEvents <-chan event.GenericEvent
}

// +kubebuilder:rbac:groups=core.sandboxd.io,resources=warmpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.sandboxd.io,resources=warmpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.sandboxd.io,resources=warmpools/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.sandboxd.io,resources=sandboxtemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=core.sandboxd.io,resources=sessions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.sandboxd.io,resources=sessions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch

// Reconcile drives the worker Deployment toward the pool's desired replicas and
// refreshes status counts.
func (r *WarmPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var pool corev1alpha1.WarmPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Resolve the template (P0: validate it exists; per-image wiring lands with
	// the resume path). A missing template is a surfaced condition, not a crash.
	var tmpl corev1alpha1.SandboxTemplate
	if err := r.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: pool.Spec.TemplateRef.Name}, &tmpl); err != nil {
		if apierrors.IsNotFound(err) {
			meta := "SandboxTemplate " + pool.Spec.TemplateRef.Name + " not found"
			r.setReady(ctx, &pool, metav1.ConditionFalse, "TemplateMissing", meta)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// minIdle autoscaling: keep at least minIdle idle workers by raising the
	// effective replica count to max(spec.Replicas, busy + minIdle). spec.Replicas
	// stays the baseline (HPA-driven); minIdle only ever scales UP to guarantee
	// warm headroom, so it doesn't fight an HPA. Busy is read from KV (the
	// assignment table is the source of truth).
	effReplicas := pool.Spec.Replicas
	if r.KV != nil && pool.Spec.MinIdle > 0 {
		if idle, total, cerr := r.KV.CountWorkers(ctx, pool.Name); cerr == nil {
			busy := int32(total - idle)
			if want := busy + pool.Spec.MinIdle; want > effReplicas {
				effReplicas = want
			}
		}
	}

	// Desired worker Deployment for this pool (scheduling comes from the template).
	dep := r.desiredDeployment(&pool, &tmpl, effReplicas)
	if err := controllerutil.SetControllerReference(&pool, dep, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	// Create-or-update.
	var existing appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Namespace: dep.Namespace, Name: dep.Name}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, dep); err != nil {
			return ctrl.Result{}, fmt.Errorf("create worker deployment: %w", err)
		}
		log.Info("created worker deployment", "deployment", dep.Name, "replicas", pool.Spec.Replicas)
	case err != nil:
		return ctrl.Result{}, err
	default:
		// Reconcile replicas + pod template if drifted.
		if !deploymentMatches(&existing, dep) {
			existing.Spec.Replicas = dep.Spec.Replicas
			existing.Spec.Template = dep.Spec.Template
			if err := r.Update(ctx, &existing); err != nil {
				return ctrl.Result{}, fmt.Errorf("update worker deployment: %w", err)
			}
			log.Info("updated worker deployment", "deployment", dep.Name, "replicas", pool.Spec.Replicas)
		}
	}

	// Refresh status (idle/busy are derived from the KV assignment table). The
	// resume/suspend paths mutate KV out-of-band and enqueue a reconcile of the
	// affected pool via a channel event source (see PoolEvents / SetupWithManager),
	// so this projection tracks reality in near-real-time without polling — O(1)
	// per state change rather than O(pools) on a timer.
	if err := r.updateStatus(ctx, &pool); err != nil {
		log.Error(err, "update status")
	}

	// Graceful scale-in: stamp pod-deletion-cost so the ReplicaSet controller
	// deletes idle workers before busy ones. This reconcile is nudged on every
	// claim/release (PoolNotifier), so costs track busy/idle transitions in
	// near-real-time. Best-effort: a failure here never blocks the pool.
	if err := r.syncDeletionCosts(ctx, &pool); err != nil {
		log.Error(err, "sync pod deletion costs")
	}
	return ctrl.Result{}, nil
}

// syncDeletionCosts sets the controller.kubernetes.io/pod-deletion-cost annotation
// on each worker pod in the pool: a low cost for idle workers and a high cost for
// busy ones, so a scale-in deletes idle workers first. It patches only pods whose
// current cost differs (idempotent), using a merge patch of just the annotation so
// it never clobbers other fields. KV is the source of truth for busy/idle.
func (r *WarmPoolReconciler) syncDeletionCosts(ctx context.Context, pool *corev1alpha1.WarmPool) error {
	if r.KV == nil {
		return nil
	}
	workers, err := r.KV.PoolWorkers(ctx, pool.Name)
	if err != nil {
		return err
	}
	for _, w := range workers {
		want := deletionCostIdle
		if w.State == resumeapi.WorkerBusy {
			want = deletionCostBusy
		}
		var pod corev1.Pod
		if err := r.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: w.Pod}, &pod); err != nil {
			continue // pod gone/not visible yet; discovery + prune reconcile it
		}
		if pod.Annotations[podDeletionCostAnnotation] == want {
			continue // already correct — no patch
		}
		patch := client.RawPatch(types.MergePatchType,
			[]byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, podDeletionCostAnnotation, want)))
		if err := r.Patch(ctx, &pod, patch); err != nil {
			return fmt.Errorf("patch deletion cost on %s: %w", w.Pod, err)
		}
	}
	return nil
}

// gracePeriod returns the worker pod termination grace period as a pointer
// (nil when 0, letting k8s apply its default).
func gracePeriod() *int64 {
	if WorkerTerminationGracePeriodSeconds <= 0 {
		return nil
	}
	g := WorkerTerminationGracePeriodSeconds
	return &g
}

// desiredDeployment builds the worker Deployment for a pool, modeled on
// sandboxd/worker-deploy.yaml, parameterized by pool name.
func (r *WarmPoolReconciler) desiredDeployment(pool *corev1alpha1.WarmPool, tmpl *corev1alpha1.SandboxTemplate, replicas int32) *appsv1.Deployment {
	labels := map[string]string{
		LabelApp:  LabelAppWorker,
		LabelPool: pool.Name,
	}
	privileged := true
	propagation := corev1.MountPropagationBidirectional
	hostPathSocket := corev1.HostPathSocket
	hostPathDir := corev1.HostPathDirectory

	// Placement and resources are pure pass-through from the template — the
	// operator injects NO defaults (no nodeSelector, toleration, spread, affinity,
	// or resource requests). Whatever the template sets is applied verbatim;
	// whatever it omits is left unset, and the scheduler places workers at will.
	// This makes placement/sizing an explicit operator decision.
	//
	// NOTE: the topology-spread label selector, if the user wants per-pool spread,
	// must target this pool's workers — document that pool workers carry the
	// labels {sandboxd.io/app: worker, sandboxd.io/pool: <name>}.
	sched := tmpl.Spec.Scheduling
	var workerResources corev1.ResourceRequirements
	if tmpl.Spec.Resources != nil {
		workerResources = *tmpl.Spec.Resources
	}

	// Worker image: global default, optionally overridden per-pool by the template
	// (canarying a new worker build). See SandboxTemplateSpec.WorkerImage for the
	// restore-compatibility caveat.
	workerImage := WorkerImage
	if tmpl.Spec.WorkerImage != "" {
		workerImage = tmpl.Spec.WorkerImage
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandboxd-worker-" + pool.Name,
			Namespace: pool.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					// NOTE: intentionally NO karpenter.sh/do-not-disrupt annotation.
					// Pool workers are ephemeral/replaceable; pinning them blocks node
					// drain/consolidation (and wedged a node-replace during testing).
				},
				Spec: corev1.PodSpec{
					ServiceAccountName:            r.Worker.ServiceAccount,
					TerminationGracePeriodSeconds: gracePeriod(),
					NodeSelector:                  sched.NodeSelector,
					Tolerations:                   sched.Tolerations,
					Affinity:                      sched.Affinity,
					TopologySpreadConstraints:     sched.TopologySpreadConstraints,
					Containers: []corev1.Container{{
						Name:            "sandboxd",
						Image:           workerImage,
						Ports:           []corev1.ContainerPort{{ContainerPort: 8090, Name: "http"}},
						Env:             r.workerEnv(tmpl),
						Resources:       workerResources,
						SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
								Path: "/healthz", Port: intstr8090()}},
							InitialDelaySeconds: 2, PeriodSeconds: 5,
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "containerd-sock", MountPath: "/run/containerd/containerd.sock"},
							{Name: "containerd-data", MountPath: "/var/lib/containerd", MountPropagation: &propagation},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "containerd-sock", VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{Path: "/run/containerd/containerd.sock", Type: &hostPathSocket}}},
						{Name: "containerd-data", VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/containerd", Type: &hostPathDir}}},
					},
				},
			},
		},
	}
}

// workerEnv builds the env for a worker container: pod IP (downward API),
// debug off, per-pool console streaming, and — when configured — the S3
// bucket/region for checkpoint/restore.
func (r *WarmPoolReconciler) workerEnv(tmpl *corev1alpha1.SandboxTemplate) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "SANDBOXD_DEBUG", Value: "0"},
		{Name: "SANDBOXD_POD_IP", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
	}
	// Per-pool opt-in: surface the nested workload console to kubectl logs.
	if tmpl != nil && tmpl.Spec.StreamConsole {
		env = append(env, corev1.EnvVar{Name: "SANDBOXD_STREAM_CONSOLE", Value: "1"})
	}
	if r.Worker.Bucket != "" {
		env = append(env, corev1.EnvVar{Name: "SANDBOXD_BUCKET", Value: r.Worker.Bucket})
	}
	if r.Worker.Region != "" {
		env = append(env, corev1.EnvVar{Name: "AWS_REGION", Value: r.Worker.Region})
	}
	return env
}

// updateStatus refreshes replicas/idle/busy/selector on the pool.
func (r *WarmPoolReconciler) updateStatus(ctx context.Context, pool *corev1alpha1.WarmPool) error {
	var dep appsv1.Deployment
	depName := types.NamespacedName{Namespace: pool.Namespace, Name: "sandboxd-worker-" + pool.Name}
	if err := r.Get(ctx, depName, &dep); err == nil {
		pool.Status.Replicas = dep.Status.Replicas
	}
	pool.Status.Selector = fmt.Sprintf("%s=%s,%s=%s", LabelApp, LabelAppWorker, LabelPool, pool.Name)

	if r.KV != nil {
		idle, total, err := r.KV.CountWorkers(ctx, pool.Name)
		if err == nil {
			pool.Status.Idle = int32(idle)
			pool.Status.Busy = int32(total - idle)
			metrics.SetPoolWorkers(pool.Name, idle, total-idle) // real-time gauge
		}
	}
	setReadyCond(&pool.Status.Conditions, metav1.ConditionTrue, "Provisioned", "worker deployment reconciled")
	return r.Status().Update(ctx, pool)
}

func (r *WarmPoolReconciler) setReady(ctx context.Context, pool *corev1alpha1.WarmPool, s metav1.ConditionStatus, reason, msg string) {
	setReadyCond(&pool.Status.Conditions, s, reason, msg)
	_ = r.Status().Update(ctx, pool)
}

// SetupWithManager sets up the controller with the Manager.
func (r *WarmPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.WarmPool{}).
		Owns(&appsv1.Deployment{}).
		Named("warmpool")
	// Watch the resume/suspend nudge channel: each event names a WarmPool whose
	// worker busy/idle state just changed, so we reconcile it immediately to
	// refresh status (near-real-time, event-driven — no polling).
	if r.PoolEvents != nil {
		b = b.WatchesRawSource(source.Channel(r.PoolEvents, &handler.EnqueueRequestForObject{}))
	}
	return b.Complete(r)
}
