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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	corev1alpha1 "github.com/jicowan/aio-sandbox/controlplane/api/v1alpha1"
	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/controlplane/internal/snapshot"
	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

// fakeSnapS3 supports List/Copy/Delete over an in-memory keyspace for snapshot tests.
type fakeSnapS3 struct{ objects map[string]bool }

func (f *fakeSnapS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	out := &s3.ListObjectsV2Output{}
	prefix := aws.ToString(in.Prefix)
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			kk := k
			out.Contents = append(out.Contents, s3types.Object{Key: &kk})
		}
	}
	return out, nil
}

func (f *fakeSnapS3) CopyObject(_ context.Context, in *s3.CopyObjectInput, _ ...func(*s3.Options)) (*s3.CopyObjectOutput, error) {
	f.objects[aws.ToString(in.Key)] = true
	return &s3.CopyObjectOutput{}, nil
}

func (f *fakeSnapS3) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	delete(f.objects, aws.ToString(in.Key))
	return &s3.DeleteObjectOutput{}, nil
}

var _ = Describe("BaseSnapshot Controller (copy-on-promote)", func() {
	const ns = "default"

	It("promotes a suspended session's snapshot to a fork-stable bases/ prefix", func() {
		ctx := context.Background()
		mr, err := miniredis.Run()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(mr.Close)
		kv := assign.NewFromRedis(redis.NewClient(&redis.Options{Addr: mr.Addr()}))

		// Source session: Suspended with a snapshot under sandboxes/.
		Expect(kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
			SID: "sess-origin", State: resumeapi.StateSuspended, Pool: "aio-pool",
			Image: "ghcr.io/agent-infra/sandbox:latest", SnapshotURI: "sandboxes/sess-origin/snap-100",
		})).To(Succeed())

		s3f := &fakeSnapS3{objects: map[string]bool{
			"sandboxes/sess-origin/snap-100/checkpoint.img": true,
			"sandboxes/sess-origin/snap-100/config.json":    true,
		}}
		store := snapshot.New(s3f, "bucket")

		base := &corev1alpha1.BaseSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "golden-1", Namespace: ns},
			Spec: corev1alpha1.BaseSnapshotSpec{
				SourceSessionRef: corev1alpha1.LocalRef{Name: "sess-origin"}, Pinned: true,
			},
		}
		Expect(k8sClient.Create(ctx, base)).To(Succeed())

		r := &BaseSnapshotReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), KV: kv, Store: store}
		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "golden-1"}}
		// First reconcile adds the finalizer + requeues; second promotes.
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		var got corev1alpha1.BaseSnapshot
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "golden-1"}, &got)).To(Succeed())
		Expect(got.Status.Ready).To(BeTrue())
		Expect(got.Status.Phase).To(Equal(corev1alpha1.BaseSnapshotReady))
		Expect(got.Status.SnapshotURI).To(Equal("bases/golden-1/snap-100"))
		Expect(got.Status.Image).To(Equal("ghcr.io/agent-infra/sandbox:latest"))
		// Objects copied into bases/, source left intact.
		Expect(s3f.objects["bases/golden-1/snap-100/checkpoint.img"]).To(BeTrue())
		Expect(s3f.objects["bases/golden-1/snap-100/config.json"]).To(BeTrue())
		Expect(s3f.objects["sandboxes/sess-origin/snap-100/checkpoint.img"]).To(BeTrue())
	})

	It("fails a base whose source is not Suspended-with-snapshot", func() {
		ctx := context.Background()
		mr, err := miniredis.Run()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(mr.Close)
		kv := assign.NewFromRedis(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
		Expect(kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
			SID: "sess-running", State: resumeapi.StateRunning, Pool: "aio-pool",
		})).To(Succeed())

		base := &corev1alpha1.BaseSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "golden-2", Namespace: ns},
			Spec:       corev1alpha1.BaseSnapshotSpec{SourceSessionRef: corev1alpha1.LocalRef{Name: "sess-running"}},
		}
		Expect(k8sClient.Create(ctx, base)).To(Succeed())

		store := snapshot.New(&fakeSnapS3{objects: map[string]bool{}}, "bucket")
		r := &BaseSnapshotReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), KV: kv, Store: store}
		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "golden-2"}}
		_, _ = r.Reconcile(ctx, req) // finalizer + requeue
		_, _ = r.Reconcile(ctx, req) // resolve source -> fail

		var got corev1alpha1.BaseSnapshot
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "golden-2"}, &got)).To(Succeed())
		Expect(got.Status.Ready).To(BeFalse())
		Expect(got.Status.Phase).To(Equal(corev1alpha1.BaseSnapshotFailed))
	})

	It("reclaims bases/ S3 via finalizer when a (pinned) base CR is deleted", func() {
		ctx := context.Background()
		mr, err := miniredis.Run()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(mr.Close)
		kv := assign.NewFromRedis(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
		Expect(kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
			SID: "sess-fin", State: resumeapi.StateSuspended, Pool: "aio-pool",
			Image: "img", SnapshotURI: "sandboxes/sess-fin/snap-9",
		})).To(Succeed())

		s3f := &fakeSnapS3{objects: map[string]bool{
			"sandboxes/sess-fin/snap-9/checkpoint.img": true,
		}}
		store := snapshot.New(s3f, "bucket")
		r := &BaseSnapshotReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), KV: kv, Store: store}

		base := &corev1alpha1.BaseSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "golden-fin", Namespace: ns},
			Spec:       corev1alpha1.BaseSnapshotSpec{SourceSessionRef: corev1alpha1.LocalRef{Name: "sess-fin"}, Pinned: true},
		}
		Expect(k8sClient.Create(ctx, base)).To(Succeed())

		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "golden-fin"}}
		// First reconcile adds finalizer + requeues; second promotes.
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		var got corev1alpha1.BaseSnapshot
		Expect(k8sClient.Get(ctx, req.NamespacedName, &got)).To(Succeed())
		Expect(got.Finalizers).To(ContainElement(baseSnapshotFinalizer))
		Expect(got.Status.Ready).To(BeTrue())
		Expect(s3f.objects["bases/golden-fin/snap-9/checkpoint.img"]).To(BeTrue())

		// Delete the (pinned) CR: it lingers with a deletion timestamp until the
		// finalizer reconcile reclaims S3 and removes the finalizer.
		Expect(k8sClient.Delete(ctx, &got)).To(Succeed())
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		// S3 base objects reclaimed; source untouched; CR gone.
		Expect(s3f.objects["bases/golden-fin/snap-9/checkpoint.img"]).To(BeFalse())
		Expect(s3f.objects["sandboxes/sess-fin/snap-9/checkpoint.img"]).To(BeTrue())
		err = k8sClient.Get(ctx, req.NamespacedName, &got)
		Expect(err).To(HaveOccurred()) // NotFound: finalizer removed → object deleted
	})
})
