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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	corev1alpha1 "github.com/jicowan/aio-sandbox/controlplane/api/v1alpha1"
	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

var _ = Describe("WarmPool Controller", func() {
	const ns = "default"

	reconcileOnce := func(name string) {
		r := &WarmPoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: ns, Name: name},
		})
		Expect(err).NotTo(HaveOccurred())
	}

	It("provisions a worker Deployment sized to spec.replicas from a template", func() {
		ctx := context.Background()

		tmpl := &corev1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "tmpl-a", Namespace: ns},
			Spec:       corev1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim", Ports: testPorts(), Health: testHealth()},
		}
		Expect(k8sClient.Create(ctx, tmpl)).To(Succeed())

		pool := &corev1alpha1.WarmPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: ns},
			Spec: corev1alpha1.WarmPoolSpec{
				TemplateRef: corev1alpha1.LocalRef{Name: "tmpl-a"},
				Replicas:    3,
			},
		}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())

		reconcileOnce("pool-a")

		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sandboxd-worker-pool-a"}, &dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(3)))
		Expect(dep.Spec.Template.Labels[LabelApp]).To(Equal(LabelAppWorker))
		Expect(dep.Spec.Template.Labels[LabelPool]).To(Equal("pool-a"))
		Expect(dep.Spec.Template.Spec.Containers[0].Name).To(Equal("sandboxd"))
		// owned by the pool
		Expect(dep.OwnerReferences).To(HaveLen(1))
		Expect(dep.OwnerReferences[0].Name).To(Equal("pool-a"))
	})

	It("shapes a microVM pool's worker pod with /dev/kvm + SANDBOXD_RUNTIME (gVisor has neither)", func() {
		ctx := context.Background()

		// A microVM-runtime template.
		mvTmpl := &corev1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "tmpl-mv", Namespace: ns},
			Spec: corev1alpha1.SandboxTemplateSpec{
				Image: "redis:7-alpine", Runtime: "microvm", Ports: testPorts(), Health: testHealth(),
			},
		}
		Expect(k8sClient.Create(ctx, mvTmpl)).To(Succeed())
		mvPool := &corev1alpha1.WarmPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-mv", Namespace: ns},
			Spec:       corev1alpha1.WarmPoolSpec{TemplateRef: corev1alpha1.LocalRef{Name: "tmpl-mv"}, Replicas: 1},
		}
		Expect(k8sClient.Create(ctx, mvPool)).To(Succeed())
		reconcileOnce("pool-mv")

		var mvDep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sandboxd-worker-pool-mv"}, &mvDep)).To(Succeed())
		podSpec := mvDep.Spec.Template.Spec
		ctr := podSpec.Containers[0]

		// SANDBOXD_RUNTIME=microvm env is injected.
		Expect(ctr.Env).To(ContainElement(corev1.EnvVar{Name: "SANDBOXD_RUNTIME", Value: "microvm"}))
		// /dev/kvm device: a CharDevice hostPath volume + a mount.
		var kvmVol *corev1.Volume
		for i := range podSpec.Volumes {
			if podSpec.Volumes[i].Name == kvmVolume {
				kvmVol = &podSpec.Volumes[i]
			}
		}
		Expect(kvmVol).NotTo(BeNil(), "microVM worker must have a /dev/kvm volume")
		Expect(kvmVol.HostPath).NotTo(BeNil())
		Expect(kvmVol.HostPath.Path).To(Equal("/dev/kvm"))
		Expect(*kvmVol.HostPath.Type).To(Equal(corev1.HostPathCharDev))
		Expect(ctr.VolumeMounts).To(ContainElement(corev1.VolumeMount{Name: kvmVolume, MountPath: "/dev/kvm"}))

		// A default (gVisor) pool gets NEITHER — the pod shape stays unchanged.
		gvTmpl := &corev1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "tmpl-gv", Namespace: ns},
			Spec:       corev1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim", Ports: testPorts(), Health: testHealth()},
		}
		Expect(k8sClient.Create(ctx, gvTmpl)).To(Succeed())
		gvPool := &corev1alpha1.WarmPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-gv", Namespace: ns},
			Spec:       corev1alpha1.WarmPoolSpec{TemplateRef: corev1alpha1.LocalRef{Name: "tmpl-gv"}, Replicas: 1},
		}
		Expect(k8sClient.Create(ctx, gvPool)).To(Succeed())
		reconcileOnce("pool-gv")

		var gvDep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sandboxd-worker-pool-gv"}, &gvDep)).To(Succeed())
		gvCtr := gvDep.Spec.Template.Spec.Containers[0]
		for _, e := range gvCtr.Env {
			Expect(e.Name).NotTo(Equal("SANDBOXD_RUNTIME"), "gVisor worker must not set SANDBOXD_RUNTIME")
		}
		for _, v := range gvDep.Spec.Template.Spec.Volumes {
			Expect(v.Name).NotTo(Equal(kvmVolume), "gVisor worker must not mount /dev/kvm")
		}
	})

	It("surfaces pod resource LIMITS so the microVM guest is sized from them (issue #38), without touching gVisor", func() {
		ctx := context.Background()
		limits := corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		}

		// microVM pool WITH limits: worker gets BOTH POD_MEM_LIMIT and POD_CPU_LIMIT
		// (downward API from limits.memory / limits.cpu) so guestConfig can size the VM.
		mvTmpl := &corev1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "tmpl-mv-sz", Namespace: ns},
			Spec: corev1alpha1.SandboxTemplateSpec{
				Image: "redis:7-alpine", Runtime: "microvm", Resources: &limits,
				Ports: testPorts(), Health: testHealth(),
			},
		}
		Expect(k8sClient.Create(ctx, mvTmpl)).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1alpha1.WarmPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-mv-sz", Namespace: ns},
			Spec:       corev1alpha1.WarmPoolSpec{TemplateRef: corev1alpha1.LocalRef{Name: "tmpl-mv-sz"}, Replicas: 1},
		})).To(Succeed())
		reconcileOnce("pool-mv-sz")

		var mvDep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sandboxd-worker-pool-mv-sz"}, &mvDep)).To(Succeed())
		mvEnv := mvDep.Spec.Template.Spec.Containers[0].Env
		Expect(envNames(mvEnv)).To(ContainElement("SANDBOXD_POD_CPU_LIMIT"))
		Expect(envNames(mvEnv)).To(ContainElement("SANDBOXD_POD_MEM_LIMIT"))
		// The CPU limit is surfaced whole-core via the downward API (divisor "1").
		cpuEnv := envByName(mvEnv, "SANDBOXD_POD_CPU_LIMIT")
		Expect(cpuEnv).NotTo(BeNil())
		Expect(cpuEnv.ValueFrom).NotTo(BeNil())
		Expect(cpuEnv.ValueFrom.ResourceFieldRef).NotTo(BeNil())
		Expect(cpuEnv.ValueFrom.ResourceFieldRef.Resource).To(Equal("limits.cpu"))

		// gVisor pool WITH the SAME limits: it gets POD_MEM_LIMIT (agent OOM-reserve,
		// both runtimes) but MUST NOT get POD_CPU_LIMIT — that's the microVM-only knob,
		// and adding it would needlessly roll every gVisor worker. Regression guard.
		gvTmpl := &corev1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "tmpl-gv-sz", Namespace: ns},
			Spec: corev1alpha1.SandboxTemplateSpec{
				Image: "python:3.12-slim", Resources: &limits, Ports: testPorts(), Health: testHealth(),
			},
		}
		Expect(k8sClient.Create(ctx, gvTmpl)).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1alpha1.WarmPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-gv-sz", Namespace: ns},
			Spec:       corev1alpha1.WarmPoolSpec{TemplateRef: corev1alpha1.LocalRef{Name: "tmpl-gv-sz"}, Replicas: 1},
		})).To(Succeed())
		reconcileOnce("pool-gv-sz")

		var gvDep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sandboxd-worker-pool-gv-sz"}, &gvDep)).To(Succeed())
		gvEnv := gvDep.Spec.Template.Spec.Containers[0].Env
		Expect(envNames(gvEnv)).NotTo(ContainElement("SANDBOXD_POD_CPU_LIMIT"), "gVisor worker must not get the microVM CPU-sizing env")
		Expect(envNames(gvEnv)).To(ContainElement("SANDBOXD_POD_MEM_LIMIT"), "gVisor still gets the mem limit for the agent OOM-reserve")
	})

	It("updates replicas when the pool spec changes", func() {
		ctx := context.Background()
		var pool corev1alpha1.WarmPool
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "pool-a"}, &pool)).To(Succeed())
		pool.Spec.Replicas = 5
		Expect(k8sClient.Update(ctx, &pool)).To(Succeed())

		reconcileOnce("pool-a")

		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sandboxd-worker-pool-a"}, &dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(5)))
	})

	It("does not provision when the referenced template is missing", func() {
		ctx := context.Background()
		pool := &corev1alpha1.WarmPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-missing", Namespace: ns},
			Spec: corev1alpha1.WarmPoolSpec{
				TemplateRef: corev1alpha1.LocalRef{Name: "nope"},
				Replicas:    2,
			},
		}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())

		reconcileOnce("pool-missing")

		var dep appsv1.Deployment
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sandboxd-worker-pool-missing"}, &dep)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("scales up to busy+minIdle to preserve warm headroom", func() {
		ctx := context.Background()
		mr, err := miniredis.Run()
		Expect(err).NotTo(HaveOccurred())
		defer mr.Close()
		kv := assign.NewFromRedis(redis.NewClient(&redis.Options{Addr: mr.Addr()}))

		tmpl := &corev1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "tmpl-mi", Namespace: ns},
			Spec:       corev1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim", Ports: testPorts(), Health: testHealth()},
		}
		Expect(k8sClient.Create(ctx, tmpl)).To(Succeed())
		pool := &corev1alpha1.WarmPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-mi", Namespace: ns},
			Spec: corev1alpha1.WarmPoolSpec{
				TemplateRef: corev1alpha1.LocalRef{Name: "tmpl-mi"},
				Replicas:    2, MinIdle: 2,
			},
		}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())

		// Two busy workers in KV -> effective replicas should become busy(2)+minIdle(2)=4,
		// above the spec baseline of 2.
		for _, p := range []string{"w1", "w2"} {
			Expect(kv.UpsertWorker(ctx, &resumeapi.WorkerEntry{
				Pod: p, Pool: "pool-mi", PodIP: "10.0.0.1", State: resumeapi.WorkerBusy, SID: "s-" + p,
			})).To(Succeed())
		}

		r := &WarmPoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), KV: kv}
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "pool-mi"}})
		Expect(err).NotTo(HaveOccurred())

		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sandboxd-worker-pool-mi"}, &dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(4)), "busy(2)+minIdle(2) should raise replicas above the spec baseline of 2")
	})

	It("stamps pod-deletion-cost: idle workers low, busy workers high (graceful scale-in)", func() {
		ctx := context.Background()
		mr, err := miniredis.Run()
		Expect(err).NotTo(HaveOccurred())
		defer mr.Close()
		kv := assign.NewFromRedis(redis.NewClient(&redis.Options{Addr: mr.Addr()}))

		tmpl := &corev1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "tmpl-dc", Namespace: ns},
			Spec:       corev1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim", Ports: testPorts(), Health: testHealth()},
		}
		Expect(k8sClient.Create(ctx, tmpl)).To(Succeed())
		pool := &corev1alpha1.WarmPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-dc", Namespace: ns},
			Spec: corev1alpha1.WarmPoolSpec{
				TemplateRef: corev1alpha1.LocalRef{Name: "tmpl-dc"}, Replicas: 2,
			},
		}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())

		// Two real worker pods carrying the pool labels; KV marks one busy, one idle.
		mkPod := func(name string) {
			p := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: name, Namespace: ns,
					Labels: map[string]string{LabelApp: LabelAppWorker, LabelPool: "pool-dc"},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sandboxd", Image: "x"}}},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		}
		mkPod("wdc-busy")
		mkPod("wdc-idle")
		Expect(kv.UpsertWorker(ctx, &resumeapi.WorkerEntry{
			Pod: "wdc-busy", Pool: "pool-dc", PodIP: "10.0.0.1", State: resumeapi.WorkerBusy, SID: "s1",
		})).To(Succeed())
		Expect(kv.UpsertWorker(ctx, &resumeapi.WorkerEntry{
			Pod: "wdc-idle", Pool: "pool-dc", PodIP: "10.0.0.2", State: resumeapi.WorkerIdle,
		})).To(Succeed())

		r := &WarmPoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), KV: kv}
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "pool-dc"}})
		Expect(err).NotTo(HaveOccurred())

		cost := func(name string) string {
			var p corev1.Pod
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &p)).To(Succeed())
			return p.Annotations[podDeletionCostAnnotation]
		}
		Expect(cost("wdc-busy")).To(Equal(deletionCostBusy), "busy worker should be expensive to delete")
		Expect(cost("wdc-idle")).To(Equal(deletionCostIdle), "idle worker should be cheap to delete")

		// Flip busy -> idle: cost should update to the idle value.
		Expect(kv.ReleaseWorker(ctx, "wdc-busy", "pool-dc")).To(Succeed())
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "pool-dc"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(cost("wdc-busy")).To(Equal(deletionCostIdle), "released worker should become cheap to delete")
	})
})

// envNames returns just the names of an env list (order-independent membership checks).
func envNames(env []corev1.EnvVar) []string {
	names := make([]string, len(env))
	for i := range env {
		names[i] = env[i].Name
	}
	return names
}

// envByName returns a pointer to the first env var with the given name, or nil.
func envByName(env []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			return &env[i]
		}
	}
	return nil
}
