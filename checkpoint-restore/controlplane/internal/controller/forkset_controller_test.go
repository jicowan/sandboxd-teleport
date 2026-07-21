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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	corev1alpha1 "github.com/jicowan/aio-sandbox/controlplane/api/v1alpha1"
	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

var _ = Describe("ForkSet Controller (image source)", func() {
	const ns = "default"

	reconcile := func(r *ForkSetReconciler, name string) (ctrl.Result, error) {
		return r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: ns, Name: name},
		})
	}

	It("rejects count over the hard cap (CEL) and accepts within it", func() {
		ctx := context.Background()
		// Over the cap (256): the apiserver's CRD schema rejects it at create.
		over := &corev1alpha1.ForkSet{
			ObjectMeta: metav1.ObjectMeta{Name: "batch-cap-over", Namespace: ns},
			Spec:       corev1alpha1.ForkSetSpec{Count: 257, Pool: "aio-pool"},
		}
		err := k8sClient.Create(ctx, over)
		Expect(err).To(HaveOccurred(), "count=257 must be rejected by the CRD cap")
		// At the cap: accepted.
		atCap := &corev1alpha1.ForkSet{
			ObjectMeta: metav1.ObjectMeta{Name: "batch-cap-ok", Namespace: ns},
			Spec:       corev1alpha1.ForkSetSpec{Count: 256, Pool: "aio-pool"},
		}
		Expect(k8sClient.Create(ctx, atCap)).To(Succeed())
	})

	It("fans out N child Sessions owned by the ForkSet, image source", func() {
		ctx := context.Background()
		fs := &corev1alpha1.ForkSet{
			ObjectMeta: metav1.ObjectMeta{Name: "batch-a", Namespace: ns},
			Spec: corev1alpha1.ForkSetSpec{
				Count:      3,
				Pool:       "aio-pool",
				NamePrefix: "roll-a",
				Lifecycle:  corev1alpha1.SessionLifecycle{IdleAction: "reset"},
			},
		}
		Expect(k8sClient.Create(ctx, fs)).To(Succeed())

		r := &ForkSetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := reconcile(r, "batch-a")
		Expect(err).NotTo(HaveOccurred())

		// Three children exist, named deterministically, owned by the ForkSet.
		for _, n := range []string{"sess-fork-roll-a-0", "sess-fork-roll-a-1", "sess-fork-roll-a-2"} {
			var s corev1alpha1.Session
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: n}, &s)).To(Succeed())
			Expect(s.Spec.PoolRef).NotTo(BeNil())
			Expect(s.Spec.PoolRef.Name).To(Equal("aio-pool"))
			Expect(s.Spec.Lifecycle.IdleAction).To(Equal("reset"))
			// image source: fork child with no base.
			Expect(s.Spec.ForkFrom).NotTo(BeNil())
			Expect(s.Spec.ForkFrom.BaseRef).To(BeNil())
			Expect(s.Spec.ForkFrom.SnapshotURI).To(BeEmpty())
			Expect(s.Labels[LabelForkSet]).To(Equal("batch-a"))
			Expect(s.Labels[LabelCreatedBy]).To(Equal(CreatedByOperator))
			Expect(s.OwnerReferences).To(HaveLen(1))
			Expect(s.OwnerReferences[0].Kind).To(Equal("ForkSet"))
		}

		// Status rolls up: desired=3, forks listed, no KV so ready=0/Progressing.
		var got corev1alpha1.ForkSet
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "batch-a"}, &got)).To(Succeed())
		Expect(got.Status.Desired).To(Equal(int32(3)))
		Expect(got.Status.Forks).To(HaveLen(3))
		Expect(got.Status.Phase).To(Equal(corev1alpha1.ForkSetProgressing))
	})

	It("counts a child as ready when its KV entry is Running, and reaches Ready", func() {
		ctx := context.Background()
		mr, err := miniredis.Run()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(mr.Close)
		kv := assign.NewFromRedis(redis.NewClient(&redis.Options{Addr: mr.Addr()}))

		fs := &corev1alpha1.ForkSet{
			ObjectMeta: metav1.ObjectMeta{Name: "batch-b", Namespace: ns},
			Spec:       corev1alpha1.ForkSetSpec{Count: 2, Pool: "aio-pool", NamePrefix: "roll-b"},
		}
		Expect(k8sClient.Create(ctx, fs)).To(Succeed())

		r := &ForkSetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), KV: kv}
		_, err = reconcile(r, "batch-b")
		Expect(err).NotTo(HaveOccurred())

		var got corev1alpha1.ForkSet
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "batch-b"}, &got)).To(Succeed())
		Expect(got.Status.Ready).To(Equal(int32(0)))
		Expect(got.Status.Phase).To(Equal(corev1alpha1.ForkSetProgressing))

		// Mark both children Running in KV.
		for _, sid := range []string{"sess-fork-roll-b-0", "sess-fork-roll-b-1"} {
			Expect(kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
				SID: sid, State: resumeapi.StateRunning, Pool: "aio-pool",
			})).To(Succeed())
		}
		_, err = reconcile(r, "batch-b")
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "batch-b"}, &got)).To(Succeed())
		Expect(got.Status.Ready).To(Equal(int32(2)))
		Expect(got.Status.Phase).To(Equal(corev1alpha1.ForkSetReady))
	})

	It("reaps children when count is lowered (scale-down)", func() {
		ctx := context.Background()
		fs := &corev1alpha1.ForkSet{
			ObjectMeta: metav1.ObjectMeta{Name: "batch-c", Namespace: ns},
			Spec:       corev1alpha1.ForkSetSpec{Count: 3, Pool: "aio-pool", NamePrefix: "roll-c"},
		}
		Expect(k8sClient.Create(ctx, fs)).To(Succeed())
		r := &ForkSetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := reconcile(r, "batch-c")
		Expect(err).NotTo(HaveOccurred())

		// Lower count to 1.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "batch-c"}, fs)).To(Succeed())
		fs.Spec.Count = 1
		Expect(k8sClient.Update(ctx, fs)).To(Succeed())
		_, err = reconcile(r, "batch-c")
		Expect(err).NotTo(HaveOccurred())

		// child -0 kept; -1 and -2 reaped.
		var s corev1alpha1.Session
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sess-fork-roll-c-0"}, &s)).To(Succeed())
		for _, gone := range []string{"sess-fork-roll-c-1", "sess-fork-roll-c-2"} {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: gone}, &s)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected %s reaped", gone)
		}
	})

	It("waits when a snapshot-source baseRef is not Ready", func() {
		ctx := context.Background()
		base := &corev1alpha1.BaseSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "base-pending", Namespace: ns},
			Spec:       corev1alpha1.BaseSnapshotSpec{SourceSessionRef: corev1alpha1.LocalRef{Name: "x"}},
		}
		Expect(k8sClient.Create(ctx, base)).To(Succeed()) // status.Ready defaults false

		fs := &corev1alpha1.ForkSet{
			ObjectMeta: metav1.ObjectMeta{Name: "batch-d", Namespace: ns},
			Spec: corev1alpha1.ForkSetSpec{
				Count: 2, Pool: "aio-pool", BaseRef: &corev1alpha1.LocalRef{Name: "base-pending"},
			},
		}
		Expect(k8sClient.Create(ctx, fs)).To(Succeed())
		r := &ForkSetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := reconcile(r, "batch-d")
		Expect(err).NotTo(HaveOccurred())

		var got corev1alpha1.ForkSet
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "batch-d"}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(corev1alpha1.ForkSetProgressing))
		// No children created while the base isn't ready.
		var s corev1alpha1.Session
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sess-fork-batch-d-0"}, &s)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("seeds Suspended children pointing at a Ready base (snapshot source)", func() {
		ctx := context.Background()
		mr, err := miniredis.Run()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(mr.Close)
		kv := assign.NewFromRedis(redis.NewClient(&redis.Options{Addr: mr.Addr()}))

		base := &corev1alpha1.BaseSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "base-ready", Namespace: ns},
			Spec:       corev1alpha1.BaseSnapshotSpec{SourceSessionRef: corev1alpha1.LocalRef{Name: "x"}, Pinned: true},
		}
		Expect(k8sClient.Create(ctx, base)).To(Succeed())
		base.Status = corev1alpha1.BaseSnapshotStatus{
			Ready: true, Phase: corev1alpha1.BaseSnapshotReady,
			SnapshotURI: "bases/base-ready/snap-100",
			Image:       "ghcr.io/agent-infra/sandbox:latest",
		}
		Expect(k8sClient.Status().Update(ctx, base)).To(Succeed())

		// Snapshot-source forks now require the target pool to resolve to a template.
		Expect(k8sClient.Create(ctx, &corev1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "aio-tmpl-e", Namespace: ns},
			Spec:       corev1alpha1.SandboxTemplateSpec{Image: "ghcr.io/agent-infra/sandbox:latest"},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1alpha1.WarmPool{
			ObjectMeta: metav1.ObjectMeta{Name: "aio-pool", Namespace: ns},
			Spec:       corev1alpha1.WarmPoolSpec{TemplateRef: corev1alpha1.LocalRef{Name: "aio-tmpl-e"}, Replicas: 1},
		})).To(Succeed())

		fs := &corev1alpha1.ForkSet{
			ObjectMeta: metav1.ObjectMeta{Name: "batch-e", Namespace: ns},
			Spec: corev1alpha1.ForkSetSpec{
				Count: 2, Pool: "aio-pool", NamePrefix: "snap",
				BaseRef: &corev1alpha1.LocalRef{Name: "base-ready"},
			},
		}
		Expect(k8sClient.Create(ctx, fs)).To(Succeed())
		r := &ForkSetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), KV: kv}
		_, err = reconcile(r, "batch-e")
		Expect(err).NotTo(HaveOccurred())

		for _, n := range []string{"sess-fork-snap-0", "sess-fork-snap-1"} {
			// Child Session records provenance + fork-base label.
			var s corev1alpha1.Session
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: n}, &s)).To(Succeed())
			Expect(s.Spec.ForkFrom).NotTo(BeNil())
			Expect(s.Spec.ForkFrom.BaseRef).NotTo(BeNil())
			Expect(s.Spec.ForkFrom.BaseRef.Name).To(Equal("base-ready"))
			Expect(s.Spec.ForkFrom.SnapshotURI).To(Equal("bases/base-ready/snap-100"))
			Expect(s.Labels[LabelForkBase]).To(Equal("base-ready"))
			// KV seeded Suspended pointing at the base → resume path will /restore.
			e, gerr := kv.GetSession(ctx, n)
			Expect(gerr).NotTo(HaveOccurred())
			Expect(e.State).To(Equal(resumeapi.StateSuspended))
			Expect(e.SnapshotURI).To(Equal("bases/base-ready/snap-100"))
			Expect(e.Image).To(Equal("ghcr.io/agent-infra/sandbox:latest"))
		}
	})
})
