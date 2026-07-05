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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	corev1alpha1 "github.com/jicowan/aio-sandbox/controlplane/api/v1alpha1"
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
			Spec:       corev1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
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
})
