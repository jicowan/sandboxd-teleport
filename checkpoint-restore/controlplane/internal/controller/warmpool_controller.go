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
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/jicowan/aio-sandbox/controlplane/api/v1alpha1"
	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
)

// Labels the operator stamps on worker pods so the discovery informer (TDD §4.3)
// and the pool selector can find them.
const (
	LabelApp       = "sandboxd.io/app"  // = "worker"
	LabelPool      = "sandboxd.io/pool" // = <WarmPool name>
	LabelAppWorker = "worker"
)

// WorkerImage is the sandboxd worker image the pool provisions. Workers are
// generic: the sandbox image is supplied per-session at /run time, not here.
// Configurable so we don't hardcode a registry in code.
var WorkerImage = "820537372947.dkr.ecr.us-west-2.amazonaws.com/sandboxd:v40"

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
}

// +kubebuilder:rbac:groups=core.sandboxd.io,resources=warmpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.sandboxd.io,resources=warmpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.sandboxd.io,resources=warmpools/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.sandboxd.io,resources=sandboxtemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=core.sandboxd.io,resources=sessions,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core.sandboxd.io,resources=sessions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

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

	// Desired worker Deployment for this pool.
	dep := r.desiredDeployment(&pool)
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

	// Refresh status.
	if err := r.updateStatus(ctx, &pool); err != nil {
		// status failures are non-fatal to reconcile; requeue implicitly via watch
		log.Error(err, "update status")
	}
	return ctrl.Result{}, nil
}

// desiredDeployment builds the worker Deployment for a pool, modeled on
// sandboxd/worker-deploy.yaml, parameterized by pool name.
func (r *WarmPoolReconciler) desiredDeployment(pool *corev1alpha1.WarmPool) *appsv1.Deployment {
	replicas := pool.Spec.Replicas
	labels := map[string]string{
		LabelApp:  LabelAppWorker,
		LabelPool: pool.Name,
	}
	privileged := true
	propagation := corev1.MountPropagationBidirectional
	hostPathSocket := corev1.HostPathSocket
	hostPathDir := corev1.HostPathDirectory

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
					Labels:      labels,
					Annotations: map[string]string{"karpenter.sh/do-not-disrupt": "true"},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: r.Worker.ServiceAccount,
					NodeSelector:       map[string]string{"sandbox": "gvisor"},
					Tolerations: []corev1.Toleration{{
						Key: "sandbox", Operator: corev1.TolerationOpEqual,
						Value: "gvisor", Effect: corev1.TaintEffectNoSchedule,
					}},
					Containers: []corev1.Container{{
						Name:  "sandboxd",
						Image: WorkerImage,
						Ports: []corev1.ContainerPort{{ContainerPort: 8090, Name: "http"}},
						Env:   r.workerEnv(),
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
// debug off, and — when configured — the S3 bucket/region for checkpoint/restore.
func (r *WarmPoolReconciler) workerEnv() []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "SANDBOXD_DEBUG", Value: "0"},
		{Name: "SANDBOXD_POD_IP", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.WarmPool{}).
		Owns(&appsv1.Deployment{}).
		Named("warmpool").
		Complete(r)
}
