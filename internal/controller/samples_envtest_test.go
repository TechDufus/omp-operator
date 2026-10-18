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
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
)

var _ = Describe("config/samples", func() {
	const ns = "et-samples"

	// load decodes a sample strictly (an unknown field fails) into obj and places it in ns.
	load := func(file string, obj client.Object) {
		GinkgoHelper()
		raw, err := os.ReadFile(filepath.Join("..", "..", "config", "samples", file))
		Expect(err).NotTo(HaveOccurred())
		Expect(yaml.UnmarshalStrict(raw, obj)).To(Succeed(), file)
		obj.SetNamespace(ns)
	}

	It("applies cleanly, with the Secrets and ConfigMap they reference, and the objects become Ready", func() {
		// What the samples refer to.
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "triage-skills"},
			Data: map[string]string{
				"triage.md": "---\nname: triage\ndescription: how to triage an alert\n---\nLook at the logs first.",
			},
		}
		etCreate(cm)
		etCleanup(cm)
		for name, key := range map[string]string{
			"triage-credentials": "demo", "triage-notify": "ntfy-topic", "triage-webhook": "token",
		} {
			sec := etSecret(ns, name, map[string]string{key: "sample-value"})
			etCreate(sec)
			etCleanup(sec)
		}

		action, agent, trigger, session :=
			&ompv1alpha1.OmpAction{}, &ompv1alpha1.OmpAgent{}, &ompv1alpha1.OmpTrigger{}, &ompv1alpha1.OmpSession{}
		load("omp_v1alpha1_ompaction.yaml", action)
		load("omp_v1alpha1_ompagent.yaml", agent)
		// The sample relies on the chart's --default-runtime-image, which this suite leaves unset
		// to exercise NoImage; give it the image the chart would.
		agent.Spec.Image = etRuntimeImage
		load("omp_v1alpha1_omptrigger.yaml", trigger)
		load("omp_v1alpha1_ompsession.yaml", session)
		for _, obj := range []client.Object{action, agent, trigger, session} {
			etCreate(obj)
			etCleanup(obj)
		}

		Eventually(func(g Gomega) {
			var got ompv1alpha1.OmpAction
			g.Expect(k8sClient.Get(ctx, etKey(action), &got)).To(Succeed())
			c := etCond(got.Status.Conditions, ompv1alpha1.ConditionReady)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionTrue), c.Message)
			g.Expect(got.Status.Type).To(Equal(ompv1alpha1.ActionKubernetesRolloutRestart))
		}).Should(Succeed())
		etAgentCondition(etKey(agent), ompv1alpha1.ConditionReady, metav1.ConditionTrue, "Ready")
		Eventually(func(g Gomega) {
			var got ompv1alpha1.OmpTrigger
			g.Expect(k8sClient.Get(ctx, etKey(trigger), &got)).To(Succeed())
			c := etCond(got.Status.Conditions, ompv1alpha1.ConditionReady)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionTrue), c.Message)
			g.Expect(got.Status.Type).To(Equal(ompv1alpha1.TriggerWebhook))
			g.Expect(got.Status.HookPath).To(Equal("/hooks/" + ns + "/alertmanager"))
		}).Should(Succeed())
		Eventually(func(g Gomega) {
			got := etGetSession(etKey(session))
			g.Expect(got.Status.Phase).To(Equal(ompv1alpha1.SessionPhasePending))
			g.Expect(got.Labels).To(HaveKeyWithValue(ompv1alpha1.LabelAgent, "alert-triage"))
		}).Should(Succeed())
	})
})
