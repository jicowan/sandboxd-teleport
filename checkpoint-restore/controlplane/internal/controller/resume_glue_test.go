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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/jicowan/aio-sandbox/controlplane/api/v1alpha1"
)

// configTemplateForSession is the shared config-source precedence used by the
// resume, suspend, and checkpoint paths (docs/PRD-arbitrary-image-sessions.md
// §13.3): image > templateRef > pool's own template. These specs pin that
// precedence so a templateRef/generic-pool session resolves config identically to
// a classic pool session — and so the classic and inline-image paths are proven
// UNCHANGED (regression guard for Stage 1).
var _ = Describe("configTemplateForSession precedence", func() {
	const ns = "default"

	// A pool whose own template supplies config (classic mode).
	setupPoolWithTemplate := func(ctx context.Context, pool, tmpl string) {
		Expect(k8sClient.Create(ctx, &corev1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: tmpl, Namespace: ns},
			Spec:       corev1alpha1.SandboxTemplateSpec{Image: "ghcr.io/example/pool-img:1"},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1alpha1.WarmPool{
			ObjectMeta: metav1.ObjectMeta{Name: pool, Namespace: ns},
			Spec:       corev1alpha1.WarmPoolSpec{TemplateRef: corev1alpha1.LocalRef{Name: tmpl}, Replicas: 1},
		})).To(Succeed())
	}

	It("classic poolRef mode resolves to the pool's own template (UNCHANGED)", func() {
		ctx := context.Background()
		setupPoolWithTemplate(ctx, "cfg-pool-classic", "cfg-tmpl-classic")
		s := &corev1alpha1.Session{
			Spec: corev1alpha1.SessionSpec{PoolRef: &corev1alpha1.LocalRef{Name: "cfg-pool-classic"}},
		}
		name, err := configTemplateForSession(ctx, k8sClient, ns, s)
		Expect(err).NotTo(HaveOccurred())
		Expect(name).To(Equal("cfg-tmpl-classic"))
	})

	It("inline image mode resolves to NO template (config is the image)", func() {
		ctx := context.Background()
		s := &corev1alpha1.Session{
			Spec: corev1alpha1.SessionSpec{
				Image:   "ghcr.io/example/inline@sha256:abc",
				PoolRef: &corev1alpha1.LocalRef{Name: "cfg-pool-classic"}, // capacity only
			},
		}
		name, err := configTemplateForSession(ctx, k8sClient, ns, s)
		Expect(err).NotTo(HaveOccurred())
		Expect(name).To(BeEmpty(), "inline image should not resolve a template")
	})

	It("templateRef mode resolves to the named template, DECOUPLED from the pool", func() {
		ctx := context.Background()
		setupPoolWithTemplate(ctx, "cfg-pool-generic", "cfg-tmpl-pooldefault")
		// A separate template the session names directly (not the pool's).
		Expect(k8sClient.Create(ctx, &corev1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "cfg-tmpl-chosen", Namespace: ns},
			Spec:       corev1alpha1.SandboxTemplateSpec{Image: "ghcr.io/example/chosen:2"},
		})).To(Succeed())
		s := &corev1alpha1.Session{
			Spec: corev1alpha1.SessionSpec{
				PoolRef:     &corev1alpha1.LocalRef{Name: "cfg-pool-generic"},   // capacity
				TemplateRef: &corev1alpha1.LocalRef{Name: "cfg-tmpl-chosen"},    // config
			},
		}
		name, err := configTemplateForSession(ctx, k8sClient, ns, s)
		Expect(err).NotTo(HaveOccurred())
		Expect(name).To(Equal("cfg-tmpl-chosen"),
			"templateRef must win over the pool's own template")
	})

	It("image wins over templateRef when both are set (precedence)", func() {
		ctx := context.Background()
		s := &corev1alpha1.Session{
			Spec: corev1alpha1.SessionSpec{
				Image:       "ghcr.io/example/inline@sha256:def",
				TemplateRef: &corev1alpha1.LocalRef{Name: "cfg-tmpl-chosen"},
				PoolRef:     &corev1alpha1.LocalRef{Name: "cfg-pool-generic"},
			},
		}
		name, err := configTemplateForSession(ctx, k8sClient, ns, s)
		Expect(err).NotTo(HaveOccurred())
		Expect(name).To(BeEmpty(), "image is the highest-precedence config source")
	})

	It("no poolRef, no templateRef, no image resolves to empty (no policy), not an error", func() {
		ctx := context.Background()
		s := &corev1alpha1.Session{Spec: corev1alpha1.SessionSpec{}}
		name, err := configTemplateForSession(ctx, k8sClient, ns, s)
		Expect(err).NotTo(HaveOccurred())
		Expect(name).To(BeEmpty())
	})
})
