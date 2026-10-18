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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/gateway"
	"github.com/TechDufus/omp-operator/internal/store"
)

var _ = Describe("stored data cleanup", func() {
	const ns = "et-cleanup"

	exists := func(open func() (*os.File, error)) bool {
		f, err := open()
		if err == nil {
			_ = f.Close()
			return true
		}
		Expect(err).To(MatchError(store.ErrNotFound))
		return false
	}

	It("removes a session's files, and its continuity tarball only with the last session holding the label", func() {
		// An agent that is not Ready: its sessions stay Pending, so only deletion is under test.
		agent := etBrokenAgent(ns)

		withKey := func(key string) func(*ompv1alpha1.OmpSession) {
			return func(s *ompv1alpha1.OmpSession) { s.Spec.ContinuityKey = key }
		}
		a1 := etSession(ns, agent.Name, withKey("shared-key"))
		a2 := etSession(ns, agent.Name, withKey("shared-key"))
		other := etSession(ns, agent.Name, withKey("other-key"))
		for _, s := range []*ompv1alpha1.OmpSession{a1, a2, other} {
			etCreate(s)
		}
		sharedID, otherID := store.ContinuityID("shared-key"), store.ContinuityID("other-key")

		// Each session has files; the label and finalizer come from the reconciler.
		for _, s := range []*ompv1alpha1.OmpSession{a1, a2, other} {
			Eventually(func(g Gomega) {
				got := etGetSession(etKey(s))
				g.Expect(got.Finalizers).To(ContainElement(ompv1alpha1.FinalizerCleanup))
				g.Expect(got.Labels).To(HaveKey(ompv1alpha1.LabelContinuity))
				s.UID = got.UID
			}).Should(Succeed())
			Expect(etStore.WriteTranscript(string(s.UID), strings.NewReader("<html>transcript</html>"))).To(Succeed())
		}
		for _, id := range []string{sharedID, otherID} {
			Expect(etStore.PutContinuity(ns, agent.Name, id, strings.NewReader("tarball"))).To(Succeed())
		}
		continuity := func(id string) bool {
			return exists(func() (*os.File, error) { return etStore.OpenContinuity(ns, agent.Name, id) })
		}
		transcript := func(s *ompv1alpha1.OmpSession) bool {
			return exists(func() (*os.File, error) { return etStore.OpenTranscript(string(s.UID)) })
		}
		gone := func(s *ompv1alpha1.OmpSession) {
			GinkgoHelper()
			Expect(k8sClient.Delete(ctx, s)).To(Succeed())
			Eventually(func() bool {
				return etNotFound(k8sClient.Get(ctx, etKey(s), &ompv1alpha1.OmpSession{}))
			}).Should(BeTrue())
		}

		gone(a1)
		Expect(transcript(a1)).To(BeFalse(), "the deleted session's files go")
		Expect(transcript(a2)).To(BeTrue())
		Expect(continuity(sharedID)).To(BeTrue(), "another session still holds the label")

		gone(a2)
		Expect(transcript(a2)).To(BeFalse())
		Expect(continuity(sharedID)).To(BeFalse(), "the last session holding the label takes the tarball")
		Expect(continuity(otherID)).To(BeTrue(), "another label is untouched")

		gone(other)
		Expect(continuity(otherID)).To(BeFalse())
		Expect(transcript(other)).To(BeFalse())
	})

	It("removes an agent's toolchain and continuity directories", func() {
		agent := etAgent(ns, etName("agent"), nil)
		etCreate(agent)
		etCleanup(agent)
		Eventually(func(g Gomega) {
			var got ompv1alpha1.OmpAgent
			g.Expect(k8sClient.Get(ctx, etKey(agent), &got)).To(Succeed())
			g.Expect(got.Finalizers).To(ContainElement(ompv1alpha1.FinalizerCleanup))
		}).Should(Succeed())

		Expect(etStore.PutToolchain(ns, agent.Name, "hash-one", strings.NewReader("mise"))).To(Succeed())
		Expect(etStore.PutContinuity(ns, agent.Name, store.ContinuityID("k"), strings.NewReader("tar"))).To(Succeed())
		toolchains := filepath.Join(etDataDir, "toolchains", ns, agent.Name)
		continuity := filepath.Join(etDataDir, "continuity", ns, agent.Name)
		Expect(toolchains).To(BeADirectory())
		Expect(continuity).To(BeADirectory())
		// Another agent's data stays.
		Expect(etStore.PutToolchain(ns, "bystander", "hash-one", strings.NewReader("mise"))).To(Succeed())
		DeferCleanup(func() { Expect(etStore.RemoveAgent(ns, "bystander")).To(Succeed()) })

		Expect(k8sClient.Delete(ctx, agent)).To(Succeed())
		Eventually(func() bool {
			return etNotFound(k8sClient.Get(ctx, etKey(agent), &ompv1alpha1.OmpAgent{}))
		}).Should(BeTrue())
		Expect(toolchains).NotTo(BeADirectory())
		Expect(continuity).NotTo(BeADirectory())
		Expect(exists(func() (*os.File, error) { return etStore.OpenToolchain(ns, agent.Name, "hash-one") })).To(BeFalse())
		Expect(exists(func() (*os.File, error) { return etStore.OpenToolchain(ns, "bystander", "hash-one") })).To(BeTrue())
	})
})

var _ = Describe("gateway clients Secret", func() {
	const ns = "et-clients"

	clients := func() []byte {
		GinkgoHelper()
		var s corev1.Secret
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: etOperatorNS, Name: etClientsSecret}, &s)).To(Succeed())
		return s.Data[gateway.ClientsKey]
	}
	sha := func(token string) string {
		sum := sha256.Sum256([]byte(token))
		return hex.EncodeToString(sum[:])
	}
	managed := func(models ...string) func(*ompv1alpha1.OmpAgentSpec) {
		return func(s *ompv1alpha1.OmpAgentSpec) {
			s.Model.Gateway = &ompv1alpha1.GatewaySpec{URL: "http://omp-auth.omp-system.svc:4000", AllowedModels: models}
		}
	}
	tokenOf := func(agent *ompv1alpha1.OmpAgent) string {
		return etToken(ns, gateway.TokenSecretName(agent.Name))
	}
	waitGone := func(a *ompv1alpha1.OmpAgent) {
		GinkgoHelper()
		Expect(k8sClient.Delete(ctx, a)).To(Succeed())
		Eventually(func() bool {
			return etNotFound(k8sClient.Get(ctx, etKey(a), &ompv1alpha1.OmpAgent{}))
		}).Should(BeTrue())
	}

	It("is {} with no managed agent, then lists each by namespace.name with the sha256 of its token", func() {
		// Start from no agents at all: an earlier spec's leftovers would be in the file.
		var all ompv1alpha1.OmpAgentList
		Expect(k8sClient.List(ctx, &all)).To(Succeed())
		for i := range all.Items {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &all.Items[i]))).To(Succeed())
		}
		Eventually(func(g Gomega) {
			var l ompv1alpha1.OmpAgentList
			g.Expect(k8sClient.List(ctx, &l)).To(Succeed())
			g.Expect(l.Items).To(BeEmpty())
		}).Should(Succeed())

		// Deleting the last agents rewrites the file (the clients Secret already exists).
		Eventually(clients).Should(MatchJSON(`{}`))

		// An agent with its own token Secret is not managed: it never appears.
		own := etSecret(ns, "own-gateway-token", map[string]string{"token": "mine"})
		etCreate(own)
		etCleanup(own)
		ownAgent := etAgent(ns, etName("own"), func(s *ompv1alpha1.OmpAgentSpec) {
			s.Model.Gateway = &ompv1alpha1.GatewaySpec{
				URL: "http://omp-auth.omp-system.svc:4000", TokenSecretRef: &ompv1alpha1.SecretKeyRef{Name: own.Name, Key: "token"},
			}
		})
		etCreate(ownAgent)
		etCleanup(ownAgent)
		etAgentCondition(etKey(ownAgent), ompv1alpha1.ConditionReady, metav1.ConditionTrue, "Ready")
		Expect(clients()).To(MatchJSON(`{}`))

		// A managed agent with explicit allowed models.
		first := etAgent(ns, etName("first"), managed("anthropic/claude-sonnet-5-5", "openai/*"))
		etCreate(first)
		etCleanup(first)
		firstToken := func() string { return tokenOf(first) }
		etAgentCondition(etKey(first), ompv1alpha1.ConditionReady, metav1.ConditionTrue, "Ready")
		Expect(firstToken()).To(MatchRegexp(`^[0-9a-f]{64}$`))
		want := func(entries map[string]map[string]any) {
			GinkgoHelper()
			b, err := json.Marshal(entries)
			Expect(err).NotTo(HaveOccurred())
			Eventually(clients).Should(MatchJSON(b))
		}
		want(map[string]map[string]any{
			ns + "." + first.Name: {"tokenSha256": sha(firstToken()), "models": []string{"anthropic/claude-sonnet-5-5", "openai/*"}},
		})

		// A second one defaults its models to its default model's provider.
		second := etAgent(ns, etName("second"), managed())
		etCreate(second)
		etCleanup(second)
		etAgentCondition(etKey(second), ompv1alpha1.ConditionReady, metav1.ConditionTrue, "Ready")
		want(map[string]map[string]any{
			ns + "." + first.Name:  {"tokenSha256": sha(firstToken()), "models": []string{"anthropic/claude-sonnet-5-5", "openai/*"}},
			ns + "." + second.Name: {"tokenSha256": sha(tokenOf(second)), "models": []string{"anthropic/*"}},
		})

		// Deleting an agent removes its entry; the last one leaves {}.
		secondToken := tokenOf(second)
		waitGone(first)
		want(map[string]map[string]any{
			ns + "." + second.Name: {"tokenSha256": sha(secondToken), "models": []string{"anthropic/*"}},
		})
		waitGone(second)
		want(map[string]map[string]any{})
		Expect(string(clients())).To(Equal("{}"))
	})
})
