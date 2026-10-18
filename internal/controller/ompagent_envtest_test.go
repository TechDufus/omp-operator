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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/gateway"
)

var _ = Describe("OmpAgent", func() {
	const ns = "et-agents"

	// credential is a credential whose Secret key the spec refers to.
	credential := func(secret, env string) ompv1alpha1.Credential {
		return ompv1alpha1.Credential{
			Name: "demo", Description: "credential demo", Env: env,
			SecretRef: ompv1alpha1.SecretKeyRef{Name: secret, Key: "value"},
		}
	}

	// reasonCase builds the agent a Ready reason is expected for; setup creates what the spec
	// needs, and returns the agent's namespace (the unserved one for NamespaceNotAllowed).
	type reasonCase struct {
		reason string
		build  func(spec *ompv1alpha1.OmpAgentSpec)
		// name and namespace override the defaults.
		name, namespace string
	}
	DescribeTable("decides Ready with the documented reasons",
		func(c reasonCase) {
			namespace := ns
			if c.namespace != "" {
				namespace = c.namespace
			}
			name := c.name
			if name == "" {
				name = etName("agent")
			}
			a := etAgent(namespace, name, c.build)
			etCreate(a)
			etCleanup(a)
			status := metav1.ConditionFalse
			if c.reason == "Ready" {
				status = metav1.ConditionTrue
			}
			etAgentCondition(etKey(a), ompv1alpha1.ConditionReady, status, c.reason)
			var got ompv1alpha1.OmpAgent
			Expect(k8sClient.Get(ctx, etKey(a), &got)).To(Succeed())
			Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
		},
		Entry("a complete agent", reasonCase{reason: "Ready", build: func(*ompv1alpha1.OmpAgentSpec) {}}),
		Entry("a namespace the operator does not serve", reasonCase{
			reason: "NamespaceNotAllowed", namespace: etUnserved, build: func(*ompv1alpha1.OmpAgentSpec) {},
		}),
		Entry("no image and no default image", reasonCase{
			reason: "NoImage", build: func(s *ompv1alpha1.OmpAgentSpec) { s.Image = "" },
		}),
		Entry("a model that is not provider/id", reasonCase{
			reason: "InvalidSpec", build: func(s *ompv1alpha1.OmpAgentSpec) { s.Model.Default = "claude" },
		}),
		Entry("a credential Secret that does not exist", reasonCase{
			reason: "MissingSecret", build: func(s *ompv1alpha1.OmpAgentSpec) {
				s.Credentials = []ompv1alpha1.Credential{credential("no-such-secret", "DEMO")}
			},
		}),
		Entry("a ConfigMap skill that does not exist", reasonCase{
			reason: "MissingConfigMap", build: func(s *ompv1alpha1.OmpAgentSpec) {
				s.Skills = []ompv1alpha1.SkillSource{{ConfigMap: &ompv1alpha1.LocalObjectRef{Name: "no-such-skills"}}}
			},
		}),
		Entry("a Flux workspace source that does not exist", reasonCase{
			reason: "MissingSource", build: func(s *ompv1alpha1.OmpAgentSpec) {
				s.Workspace = &ompv1alpha1.WorkspaceSpec{FluxSource: &ompv1alpha1.FluxSourceRef{
					Kind: ompv1alpha1.FluxSourceGitRepository, Namespace: "et-flux", Name: "no-such-repo",
				}}
			},
		}),
		Entry("an action that does not exist", reasonCase{
			reason: "ActionNotReady", build: func(s *ompv1alpha1.OmpAgentSpec) {
				s.Actions = []ompv1alpha1.LocalObjectRef{{Name: "no-such-action"}}
			},
		}),
		// The managed token Secret is named <agent>-omp-gateway: an agent name that is valid but 245
		// characters long makes the Secret's name (and its agent label) invalid, so creating the token
		// fails and the agent reports the reason.
		Entry("a managed gateway token that cannot be created", reasonCase{
			reason: "NoGatewayToken", name: strings.Repeat("a", 245),
			build: func(s *ompv1alpha1.OmpAgentSpec) {
				s.Model.Gateway = &ompv1alpha1.GatewaySpec{URL: "http://omp-auth.omp-system.svc:4000"}
			},
		}),
		Entry("two credentials with one name", reasonCase{
			reason: "DuplicateName", build: func(s *ompv1alpha1.OmpAgentSpec) {
				s.Credentials = []ompv1alpha1.Credential{credential("x", "ONE"), credential("x", "TWO")}
			},
		}),
		Entry("a credential that sets a reserved environment variable", reasonCase{
			reason: "InvalidCredential", build: func(s *ompv1alpha1.OmpAgentSpec) {
				s.Credentials = []ompv1alpha1.Credential{credential("x", "HOME")}
			},
		}),
	)

	It("needs an action to be Ready, not just to exist, and follows it", func() {
		actionName := etName("restart")
		a := etAgent(ns, etName("agent"), func(s *ompv1alpha1.OmpAgentSpec) {
			s.Actions = []ompv1alpha1.LocalObjectRef{{Name: actionName}}
		})
		etCreate(a)
		etCleanup(a)
		etAgentCondition(etKey(a), ompv1alpha1.ConditionReady, metav1.ConditionFalse, "ActionNotReady")

		// An action with a reserved name exists but is never Ready.
		bad := &ompv1alpha1.OmpAction{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "grep"},
			Spec: ompv1alpha1.OmpActionSpec{
				Description:              "reserved",
				KubernetesRolloutRestart: &ompv1alpha1.KubernetesRolloutRestartAction{},
			},
		}
		etCreate(bad)
		etCleanup(bad)
		withBad := etAgent(ns, etName("agent"), func(s *ompv1alpha1.OmpAgentSpec) {
			s.Actions = []ompv1alpha1.LocalObjectRef{{Name: "grep"}}
		})
		etCreate(withBad)
		etCleanup(withBad)
		etAgentCondition(etKey(withBad), ompv1alpha1.ConditionReady, metav1.ConditionFalse, "ActionNotReady")

		// The action appears: the agent's Ready follows without being touched.
		act := &ompv1alpha1.OmpAction{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: actionName},
			Spec: ompv1alpha1.OmpActionSpec{
				Description:              "restart a workload",
				KubernetesRolloutRestart: &ompv1alpha1.KubernetesRolloutRestartAction{},
			},
		}
		etCreate(act)
		etCleanup(act)
		etAgentCondition(etKey(a), ompv1alpha1.ConditionReady, metav1.ConditionTrue, "Ready")
	})

	It("becomes Ready when a Secret or Flux source it was missing appears", func() {
		secretName, repoName := etName("creds"), etName("repo")
		a := etAgent(ns, etName("agent"), func(s *ompv1alpha1.OmpAgentSpec) {
			s.Credentials = []ompv1alpha1.Credential{credential(secretName, "DEMO")}
			s.Workspace = &ompv1alpha1.WorkspaceSpec{FluxSource: &ompv1alpha1.FluxSourceRef{
				Kind: ompv1alpha1.FluxSourceGitRepository, Namespace: ns, Name: repoName,
			}}
		})
		etCreate(a)
		etCleanup(a)
		etAgentCondition(etKey(a), ompv1alpha1.ConditionReady, metav1.ConditionFalse, "MissingSecret")

		sec := etSecret(ns, secretName, map[string]string{"value": "v"})
		etCreate(sec)
		etCleanup(sec)
		etPoke(a)
		etAgentCondition(etKey(a), ompv1alpha1.ConditionReady, metav1.ConditionFalse, "MissingSource")

		repo := &sourcev1.GitRepository{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: repoName},
			Spec: sourcev1.GitRepositorySpec{
				URL: "https://example.com/repo.git", Interval: metav1.Duration{Duration: 60e9},
			},
		}
		etCreate(repo)
		etCleanup(repo)
		etPoke(a)
		etAgentCondition(etKey(a), ompv1alpha1.ConditionReady, metav1.ConditionTrue, "Ready")
	})

	It("keeps its cleanup finalizer only in namespaces the operator serves", func() {
		served := etAgent(ns, etName("agent"), nil)
		unserved := etAgent(etUnserved, etName("agent"), nil)
		etCreate(served)
		etCleanup(served)
		etCreate(unserved)
		etCleanup(unserved)
		Eventually(func(g Gomega) {
			var got ompv1alpha1.OmpAgent
			g.Expect(k8sClient.Get(ctx, etKey(served), &got)).To(Succeed())
			g.Expect(got.Finalizers).To(ContainElement(ompv1alpha1.FinalizerCleanup))
		}).Should(Succeed())
		etAgentCondition(etKey(unserved), ompv1alpha1.ConditionReady, metav1.ConditionFalse, "NamespaceNotAllowed")
		var got ompv1alpha1.OmpAgent
		Expect(k8sClient.Get(ctx, etKey(unserved), &got)).To(Succeed())
		Expect(got.Finalizers).To(BeEmpty())
	})

	It("holds one managed gateway token per agent and never rotates it", func() {
		a := etAgent(ns, etName("agent"), func(s *ompv1alpha1.OmpAgentSpec) {
			s.Model.Gateway = &ompv1alpha1.GatewaySpec{URL: "http://omp-auth.omp-system.svc:4000"}
		})
		etCreate(a)
		etCleanup(a)
		etAgentCondition(etKey(a), ompv1alpha1.ConditionReady, metav1.ConditionTrue, "Ready")

		secretKey := types.NamespacedName{Namespace: ns, Name: gateway.TokenSecretName(a.Name)}
		var sec corev1.Secret
		Expect(k8sClient.Get(ctx, secretKey, &sec)).To(Succeed())
		Expect(string(sec.Data["token"])).To(MatchRegexp(`^[0-9a-f]{64}$`))
		var owner ompv1alpha1.OmpAgent
		Expect(k8sClient.Get(ctx, etKey(a), &owner)).To(Succeed())
		Expect(sec.OwnerReferences).To(HaveLen(1))
		Expect(sec.OwnerReferences[0].UID).To(Equal(owner.UID))
		first := string(sec.Data["token"])

		etPoke(a)
		etAgentCondition(etKey(a), ompv1alpha1.ConditionReady, metav1.ConditionTrue, "Ready")
		Consistently(func(g Gomega) string {
			var again corev1.Secret
			g.Expect(k8sClient.Get(ctx, secretKey, &again)).To(Succeed())
			return string(again.Data["token"])
		}).Should(Equal(first))
	})
})
