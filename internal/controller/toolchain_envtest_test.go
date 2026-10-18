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
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/agentapi"
)

var _ = Describe("OmpAgent toolchain", func() {
	const ns = "et-toolchain"

	It("holds a session for a toolsync pod when a prepared report shows a changed toolchain", func() {
		agent := etReadyAgent(ns, func(s *ompv1alpha1.OmpAgentSpec) { s.Toolchain = &ompv1alpha1.ToolchainSpec{} })
		agentKey := etKey(agent)
		toolsyncToken := func() string { return etToken(ns, agentapi.ToolsyncTokenSecretName(agent.Name)) }
		agentStatus := func() ompv1alpha1.ToolchainStatus {
			var a ompv1alpha1.OmpAgent
			Expect(k8sClient.Get(ctx, agentKey, &a)).To(Succeed())
			return a.Status.Toolchain
		}
		toolsyncPods := func() []corev1.Pod { return etAgentPods(ns, agent.Name, ompv1alpha1.PodRoleToolsync) }
		syncedTo := func(hash string) {
			GinkgoHelper()
			code, body := etAPICall(http.MethodPost, "/v1/agents/"+ns+"/"+agent.Name+"/toolchain/unchanged",
				toolsyncToken(), agentapi.ToolchainHash{Hash: hash})
			Expect(code).To(BeNumerically("<", 300), string(body))
		}

		// A toolchain that was never synced: the first session waits for a toolsync pod.
		first := etCreateSession(ns, agent.Name, nil)
		firstKey := etKey(first)
		Eventually(toolsyncPods).Should(HaveLen(1))
		pod := toolsyncPods()[0]
		Expect(pod.Name).To(MatchRegexp(`^` + agent.Name + `-toolsync-\d+$`))
		Expect(pod.Spec.AutomountServiceAccountToken).To(HaveValue(BeFalse()))
		Expect(pod.Spec.Containers[0].Args).To(Equal([]string{"toolsync"}))
		Expect(etEnv(pod.Spec.Containers[0])[agentapi.EnvAgent].Value).To(Equal(ns + "/" + agent.Name))
		etSessionPhase(firstKey, ompv1alpha1.SessionPhasePending)
		Consistently(func(g Gomega) {
			g.Expect(etGetSession(firstKey).Status.Phase).To(Equal(ompv1alpha1.SessionPhasePending))
			g.Expect(etAgentPods(ns, agent.Name, ompv1alpha1.PodRoleSession)).To(BeEmpty())
		}).Should(Succeed())

		// The toolsync pod reports through the agent API, then exits; the session starts.
		syncedTo("hash-one")
		tc := agentStatus()
		Expect(tc.Hash).To(Equal("hash-one"))
		Expect(tc.SyncedAt).NotTo(BeNil())
		Expect(tc.Stale).To(BeFalse())
		etAgentCondition(agentKey, ompv1alpha1.ConditionToolchainReady, metav1.ConditionTrue, "Synced")
		etSetPod(etKey(&pod), etPodSucceeded)
		firstPod := etAwaitPod(firstKey, 1)
		Eventually(toolsyncPods).Should(BeEmpty(), "finished toolsync pods are cleaned up")

		// The running session reports a different toolchain hash: the agent becomes stale.
		code, body := etAPICall(http.MethodPost, "/v1/sessions/"+ns+"/"+first.Name+"/prepared",
			etToken(ns, agentapi.SessionTokenSecretName(first.Name)),
			agentapi.Prepared{WorkspaceRevision: "main@sha1:abc", ToolchainHash: "hash-two"})
		Expect(code).To(BeNumerically("<", 300), string(body))
		Expect(agentStatus().Stale).To(BeTrue())
		Expect(agentStatus().Hash).To(Equal("hash-one"), "the stored toolchain is kept until a sync replaces it")
		Expect(etGetSession(firstKey).Status.WorkspaceRevision).To(Equal("main@sha1:abc"))
		etFinish(firstKey, firstPod)

		// The agent's next session waits for a new toolsync pod; it is not started on the stale toolchain.
		second := etCreateSession(ns, agent.Name, nil)
		secondKey := etKey(second)
		Eventually(toolsyncPods).Should(HaveLen(1))
		etSessionPhase(secondKey, ompv1alpha1.SessionPhasePending)
		Consistently(func(g Gomega) {
			g.Expect(etGetSession(secondKey).Status.Phase).To(Equal(ompv1alpha1.SessionPhasePending))
			g.Expect(etAgentPods(ns, agent.Name, ompv1alpha1.PodRoleSession)).To(HaveLen(1), "only the first session's pod")
		}).Should(Succeed())

		syncedTo("hash-two")
		Expect(agentStatus().Stale).To(BeFalse())
		Expect(agentStatus().Hash).To(Equal("hash-two"))
		etSetPod(etKey(&toolsyncPods()[0]), etPodSucceeded)
		etAwaitPod(secondKey, 1)
	})

	It("does not mark an agent without a toolchain stale", func() {
		agent := etReadyAgent(ns, nil)
		sess := etCreateSession(ns, agent.Name, nil)
		etAwaitPod(etKey(sess), 1)
		code, body := etAPICall(http.MethodPost, "/v1/sessions/"+ns+"/"+sess.Name+"/prepared",
			etToken(ns, agentapi.SessionTokenSecretName(sess.Name)),
			agentapi.Prepared{ToolchainHash: "whatever"})
		Expect(code).To(BeNumerically("<", 300), string(body))
		var a ompv1alpha1.OmpAgent
		Expect(k8sClient.Get(ctx, etKey(agent), &a)).To(Succeed())
		Expect(a.Status.Toolchain.Stale).To(BeFalse())
	})

	It("refuses a session token on agent routes and a toolsync token on session routes", func() {
		agent := etReadyAgent(ns, func(s *ompv1alpha1.OmpAgentSpec) { s.Toolchain = &ompv1alpha1.ToolchainSpec{} })
		sess := etCreateSession(ns, agent.Name, nil)
		Eventually(func() int { return len(etAgentPods(ns, agent.Name, ompv1alpha1.PodRoleToolsync)) }).Should(Equal(1))
		toolsyncToken := etToken(ns, agentapi.ToolsyncTokenSecretName(agent.Name))
		code, _ := etAPICall(http.MethodPost, "/v1/agents/"+ns+"/"+agent.Name+"/toolchain/unchanged",
			toolsyncToken, agentapi.ToolchainHash{})
		Expect(code).To(Equal(http.StatusNoContent))
		etSetPod(etKey(&etAgentPods(ns, agent.Name, ompv1alpha1.PodRoleToolsync)[0]), etPodSucceeded)
		etAwaitPod(etKey(sess), 1)
		sessionToken := etToken(ns, agentapi.SessionTokenSecretName(sess.Name))

		code, _ = etAPICall(http.MethodGet, "/v1/agents/"+ns+"/"+agent.Name+"/toolchain", sessionToken, nil)
		Expect(code).To(Equal(http.StatusUnauthorized))
		code, _ = etAPICall(http.MethodGet, "/v1/sessions/"+ns+"/"+sess.Name+"/plan", toolsyncToken, nil)
		Expect(code).To(Equal(http.StatusUnauthorized))
		code, _ = etAPICall(http.MethodGet, "/v1/agents/"+ns+"/"+agent.Name+"/toolchain", toolsyncToken, nil)
		Expect(code).To(Equal(http.StatusOK))
	})
})
