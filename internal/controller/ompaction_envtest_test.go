/*
Copyright 2026 TechDufus.

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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
)

var _ = Describe("OmpAction", func() {
	const ns = "et-actions"

	action := func(name string, mutate func(*ompv1alpha1.OmpActionSpec)) *ompv1alpha1.OmpAction {
		a := &ompv1alpha1.OmpAction{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec:       ompv1alpha1.OmpActionSpec{Description: "does a thing"},
		}
		mutate(&a.Spec)
		return a
	}
	restart := func(s *ompv1alpha1.OmpActionSpec) {
		s.KubernetesRolloutRestart = &ompv1alpha1.KubernetesRolloutRestartAction{}
	}
	flux := func(s *ompv1alpha1.OmpActionSpec) {
		s.FluxReconcile = &ompv1alpha1.FluxReconcileAction{}
	}
	silence := func(secret *ompv1alpha1.SecretKeyRef) func(*ompv1alpha1.OmpActionSpec) {
		return func(s *ompv1alpha1.OmpActionSpec) {
			s.AlertmanagerSilence = &ompv1alpha1.AlertmanagerSilenceAction{
				URL: "http://alertmanager.monitoring.svc:9093", AuthSecretRef: secret,
			}
		}
	}
	httpReq := func(prefix, schema string, headers ...ompv1alpha1.ValueOrSecret) func(*ompv1alpha1.OmpActionSpec) {
		return func(s *ompv1alpha1.OmpActionSpec) {
			s.HTTPRequest = &ompv1alpha1.HTTPRequestAction{
				Method:           ompv1alpha1.MethodGet,
				URL:              "https://api.example.com/v1/items/{{ .Args.id }}",
				AllowedURLPrefix: prefix,
				Headers:          headers,
				Parameters:       apiextensionsv1.JSON{Raw: []byte(schema)},
			}
		}
	}
	const goodSchema = `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`

	DescribeTable("reports Ready and status.type",
		func(name string, mutate func(*ompv1alpha1.OmpActionSpec),
			wantStatus metav1.ConditionStatus, wantReason string, wantType ompv1alpha1.ActionType) {
			a := action(name, mutate)
			etCreate(a)
			etCleanup(a)
			Eventually(func(g Gomega) {
				var got ompv1alpha1.OmpAction
				g.Expect(k8sClient.Get(ctx, etKey(a), &got)).To(Succeed())
				c := etCond(got.Status.Conditions, ompv1alpha1.ConditionReady)
				g.Expect(c).NotTo(BeNil())
				g.Expect(c.Status).To(Equal(wantStatus), c.Message)
				g.Expect(c.Reason).To(Equal(wantReason), c.Message)
				g.Expect(got.Status.Type).To(Equal(wantType))
			}).Should(Succeed())
		},
		Entry("rollout restart", "restart-ok", restart,
			metav1.ConditionTrue, "Ready", ompv1alpha1.ActionKubernetesRolloutRestart),
		Entry("flux reconcile", "flux-ok", flux,
			metav1.ConditionTrue, "Ready", ompv1alpha1.ActionFluxReconcile),
		Entry("alertmanager silence", "silence-ok", silence(nil),
			metav1.ConditionTrue, "Ready", ompv1alpha1.ActionAlertmanagerSilence),
		Entry("http request", "http-ok", httpReq("https://api.example.com/v1", goodSchema),
			metav1.ConditionTrue, "Ready", ompv1alpha1.ActionHTTPRequest),
		Entry("a name that does not match the pattern", "1-restart", restart,
			metav1.ConditionFalse, "InvalidName", ompv1alpha1.ActionKubernetesRolloutRestart),
		Entry("a name omp already uses for a tool", "bash", restart,
			metav1.ConditionFalse, "ReservedName", ompv1alpha1.ActionKubernetesRolloutRestart),
		Entry("the built-in finish tool name", "finish", flux,
			metav1.ConditionFalse, "ReservedName", ompv1alpha1.ActionFluxReconcile),
		Entry("an allowedURLPrefix with userinfo", "http-userinfo",
			httpReq("https://user:pw@api.example.com/v1", goodSchema),
			metav1.ConditionFalse, "InvalidSpec", ompv1alpha1.ActionHTTPRequest),
		Entry("a parameters schema that does not compile", "http-schema",
			httpReq("https://api.example.com/v1", `{"type":"nonsense"}`),
			metav1.ConditionFalse, "InvalidSpec", ompv1alpha1.ActionHTTPRequest),
		Entry("a silence url that is not absolute", "silence-url",
			func(s *ompv1alpha1.OmpActionSpec) {
				s.AlertmanagerSilence = &ompv1alpha1.AlertmanagerSilenceAction{URL: "alertmanager:9093"}
			},
			metav1.ConditionFalse, "InvalidSpec", ompv1alpha1.ActionAlertmanagerSilence),
		Entry("a missing auth Secret", "silence-no-secret",
			silence(&ompv1alpha1.SecretKeyRef{Name: "no-such-secret", Key: "token"}),
			metav1.ConditionFalse, "MissingSecret", ompv1alpha1.ActionAlertmanagerSilence),
		Entry("a missing header Secret", "http-no-secret",
			httpReq("https://api.example.com/v1", goodSchema, ompv1alpha1.ValueOrSecret{
				Name: "Authorization", SecretRef: &ompv1alpha1.SecretKeyRef{Name: "no-such-header", Key: "token"},
			}),
			metav1.ConditionFalse, "MissingSecret", ompv1alpha1.ActionHTTPRequest),
	)

	It("becomes Ready once the Secret it needs exists, and not before", func() {
		secretName := etName("silence-auth")
		a := action(etName("silence"), silence(&ompv1alpha1.SecretKeyRef{Name: secretName, Key: "token"}))
		etCreate(a)
		etCleanup(a)
		ready := func(g Gomega) metav1.ConditionStatus {
			var got ompv1alpha1.OmpAction
			g.Expect(k8sClient.Get(ctx, etKey(a), &got)).To(Succeed())
			c := etCond(got.Status.Conditions, ompv1alpha1.ConditionReady)
			g.Expect(c).NotTo(BeNil())
			return c.Status
		}
		Eventually(ready).Should(Equal(metav1.ConditionFalse))

		// A key-less Secret is still missing.
		wrong := etSecret(ns, secretName, map[string]string{"other": "x"})
		etCreate(wrong)
		etCleanup(wrong)
		etPoke(a)
		Consistently(ready).Should(Equal(metav1.ConditionFalse))

		wrong.StringData = map[string]string{"token": "s3cret-token"}
		Expect(k8sClient.Update(ctx, wrong)).To(Succeed())
		etPoke(a)
		Eventually(ready).Should(Equal(metav1.ConditionTrue))
	})

	It("ignores actions in namespaces the operator does not serve", func() {
		a := action(etName("unserved"), restart)
		a.Namespace = etUnserved
		etCreate(a)
		etCleanup(a)
		Consistently(func(g Gomega) {
			var got ompv1alpha1.OmpAction
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: a.Name}, &got)).To(Succeed())
			g.Expect(got.Status.Conditions).To(BeEmpty())
			g.Expect(got.Status.Type).To(BeEmpty())
		}).Should(Succeed())
	})
})
