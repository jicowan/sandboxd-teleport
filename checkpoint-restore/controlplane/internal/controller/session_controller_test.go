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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	corev1alpha1 "github.com/jicowan/aio-sandbox/controlplane/api/v1alpha1"
)

// fakeSuspender records SuspendNow / ReleaseForDelete calls and can be made to fail.
type fakeSuspender struct {
	calls        []string
	releaseCalls []string
	failNext     bool
	failRelease  bool
}

func (f *fakeSuspender) SuspendNow(_ context.Context, sid string) error {
	f.calls = append(f.calls, sid)
	if f.failNext {
		f.failNext = false
		return fmt.Errorf("boom")
	}
	return nil
}

func (f *fakeSuspender) ReleaseForDelete(_ context.Context, sid string) error {
	f.releaseCalls = append(f.releaseCalls, sid)
	if f.failRelease {
		return fmt.Errorf("release boom")
	}
	return nil
}

var _ = Describe("Session Controller (on-demand suspend)", func() {
	const ns = "default"

	reconcile := func(r *SessionReconciler, name string) (ctrl.Result, error) {
		return r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: ns, Name: name},
		})
	}

	It("handles a suspend request and advances the watermark", func() {
		ctx := context.Background()
		s := &corev1alpha1.Session{
			ObjectMeta: metav1.ObjectMeta{Name: "sess-sr-1", Namespace: ns},
			Spec: corev1alpha1.SessionSpec{
				PoolRef:        &corev1alpha1.LocalRef{Name: "aio-pool"},
				SuspendRequest: "tok-1",
			},
		}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())

		fs := &fakeSuspender{}
		r := &SessionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Suspend: fs, Release: fs}
		_, err := reconcile(r, "sess-sr-1")
		Expect(err).NotTo(HaveOccurred())

		Expect(fs.calls).To(Equal([]string{"sess-sr-1"})) // suspended once
		var got corev1alpha1.Session
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sess-sr-1"}, &got)).To(Succeed())
		Expect(got.Status.LastSuspendHandled).To(Equal("tok-1")) // watermark advanced
	})

	It("is idempotent: same token does not re-suspend", func() {
		ctx := context.Background()
		s := &corev1alpha1.Session{
			ObjectMeta: metav1.ObjectMeta{Name: "sess-sr-2", Namespace: ns},
			Spec:       corev1alpha1.SessionSpec{PoolRef: &corev1alpha1.LocalRef{Name: "aio-pool"}, SuspendRequest: "tok-x"},
		}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		fs := &fakeSuspender{}
		r := &SessionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Suspend: fs, Release: fs}

		_, err := reconcile(r, "sess-sr-2")
		Expect(err).NotTo(HaveOccurred())
		_, err = reconcile(r, "sess-sr-2") // same token, watermark already equal
		Expect(err).NotTo(HaveOccurred())
		_, err = reconcile(r, "sess-sr-2")
		Expect(err).NotTo(HaveOccurred())

		Expect(fs.calls).To(HaveLen(1)) // exactly one suspend despite 3 reconciles
	})

	It("does NOT re-suspend after a stale token (level-vs-edge guard)", func() {
		ctx := context.Background()
		// Simulate: request handled, then the session reactively resumed (we don't
		// model KV here — the point is the reconciler must not act while the token is
		// already the watermark, regardless of any resume).
		s := &corev1alpha1.Session{
			ObjectMeta: metav1.ObjectMeta{Name: "sess-sr-3", Namespace: ns},
			Spec:       corev1alpha1.SessionSpec{PoolRef: &corev1alpha1.LocalRef{Name: "aio-pool"}, SuspendRequest: "tok-9"},
		}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		fs := &fakeSuspender{}
		r := &SessionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Suspend: fs, Release: fs}
		_, err := reconcile(r, "sess-sr-3")
		Expect(err).NotTo(HaveOccurred())
		Expect(fs.calls).To(HaveLen(1))

		// Many more reconciles (as would fire on resume-driven status/spec churn):
		for i := 0; i < 5; i++ {
			_, err := reconcile(r, "sess-sr-3")
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(fs.calls).To(HaveLen(1)) // never re-suspended: token == watermark
	})

	It("acts again when a NEW token is set", func() {
		ctx := context.Background()
		s := &corev1alpha1.Session{
			ObjectMeta: metav1.ObjectMeta{Name: "sess-sr-4", Namespace: ns},
			Spec:       corev1alpha1.SessionSpec{PoolRef: &corev1alpha1.LocalRef{Name: "aio-pool"}, SuspendRequest: "a"},
		}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		fs := &fakeSuspender{}
		r := &SessionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Suspend: fs, Release: fs}
		_, err := reconcile(r, "sess-sr-4")
		Expect(err).NotTo(HaveOccurred())

		// New request token.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sess-sr-4"}, s)).To(Succeed())
		s.Spec.SuspendRequest = "b"
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
		_, err = reconcile(r, "sess-sr-4")
		Expect(err).NotTo(HaveOccurred())

		Expect(fs.calls).To(Equal([]string{"sess-sr-4", "sess-sr-4"})) // twice, one per token
		var got corev1alpha1.Session
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sess-sr-4"}, &got)).To(Succeed())
		Expect(got.Status.LastSuspendHandled).To(Equal("b"))
	})

	It("releases the KV footprint on delete via the finalizer", func() {
		ctx := context.Background()
		s := &corev1alpha1.Session{
			ObjectMeta: metav1.ObjectMeta{Name: "sess-del-1", Namespace: ns},
			Spec:       corev1alpha1.SessionSpec{PoolRef: &corev1alpha1.LocalRef{Name: "aio-pool"}},
		}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		fs := &fakeSuspender{}
		r := &SessionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Suspend: fs, Release: fs}

		// First reconcile installs the finalizer.
		_, err := reconcile(r, "sess-del-1")
		Expect(err).NotTo(HaveOccurred())
		var got corev1alpha1.Session
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sess-del-1"}, &got)).To(Succeed())
		Expect(got.Finalizers).To(ContainElement(sessionFinalizer))

		// Delete: the CR lingers (finalizer held) until the reconciler releases KV.
		Expect(k8sClient.Delete(ctx, &got)).To(Succeed())
		_, err = reconcile(r, "sess-del-1")
		Expect(err).NotTo(HaveOccurred())

		Expect(fs.releaseCalls).To(Equal([]string{"sess-del-1"})) // KV released exactly once
		// Finalizer gone -> the object is actually removed.
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sess-del-1"}, &got)
		Expect(err).To(HaveOccurred())
	})

	It("keeps the finalizer (retries) if KV release fails on delete", func() {
		ctx := context.Background()
		s := &corev1alpha1.Session{
			ObjectMeta: metav1.ObjectMeta{Name: "sess-del-2", Namespace: ns},
			Spec:       corev1alpha1.SessionSpec{PoolRef: &corev1alpha1.LocalRef{Name: "aio-pool"}},
		}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		fs := &fakeSuspender{failRelease: true}
		r := &SessionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Suspend: fs, Release: fs}
		_, err := reconcile(r, "sess-del-2")
		Expect(err).NotTo(HaveOccurred())

		var got corev1alpha1.Session
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sess-del-2"}, &got)).To(Succeed())
		Expect(k8sClient.Delete(ctx, &got)).To(Succeed())

		// Release fails -> error surfaced (requeue), finalizer retained, CR still present.
		_, err = reconcile(r, "sess-del-2")
		Expect(err).To(HaveOccurred())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sess-del-2"}, &got)).To(Succeed())
		Expect(got.Finalizers).To(ContainElement(sessionFinalizer))

		// Recover: release succeeds, finalizer drops, object removed.
		fs.failRelease = false
		_, err = reconcile(r, "sess-del-2")
		Expect(err).NotTo(HaveOccurred())
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sess-del-2"}, &got)
		Expect(err).To(HaveOccurred())
	})

	It("no request is a no-op", func() {
		ctx := context.Background()
		s := &corev1alpha1.Session{
			ObjectMeta: metav1.ObjectMeta{Name: "sess-sr-5", Namespace: ns},
			Spec:       corev1alpha1.SessionSpec{PoolRef: &corev1alpha1.LocalRef{Name: "aio-pool"}},
		}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		fs := &fakeSuspender{}
		r := &SessionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Suspend: fs, Release: fs}
		_, err := reconcile(r, "sess-sr-5")
		Expect(err).NotTo(HaveOccurred())
		Expect(fs.calls).To(BeEmpty())
	})

	It("leaves the watermark unchanged on suspend failure (retryable)", func() {
		ctx := context.Background()
		s := &corev1alpha1.Session{
			ObjectMeta: metav1.ObjectMeta{Name: "sess-sr-6", Namespace: ns},
			Spec:       corev1alpha1.SessionSpec{PoolRef: &corev1alpha1.LocalRef{Name: "aio-pool"}, SuspendRequest: "t"},
		}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		fs := &fakeSuspender{failNext: true}
		r := &SessionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Suspend: fs, Release: fs}

		_, err := reconcile(r, "sess-sr-6")
		Expect(err).To(HaveOccurred()) // surfaced -> requeue
		var got corev1alpha1.Session
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sess-sr-6"}, &got)).To(Succeed())
		Expect(got.Status.LastSuspendHandled).To(BeEmpty()) // NOT advanced

		// Retry succeeds and advances.
		_, err = reconcile(r, "sess-sr-6")
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sess-sr-6"}, &got)).To(Succeed())
		Expect(got.Status.LastSuspendHandled).To(Equal("t"))
	})
})
