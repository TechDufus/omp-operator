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
	"fmt"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/agentapi"
	"github.com/TechDufus/omp-operator/internal/gateway"
	"github.com/TechDufus/omp-operator/internal/metrics"
)

// etEnv indexes a container's environment by name.
func etEnv(c corev1.Container) map[string]corev1.EnvVar {
	out := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		out[e.Name] = e
	}
	return out
}

func etMountPaths(c corev1.Container) []string {
	out := make([]string, 0, len(c.VolumeMounts))
	for _, m := range c.VolumeMounts {
		out = append(out, m.MountPath)
	}
	return out
}

func etSecretKeyRef(name, key string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key,
	}}
}

var _ = Describe("OmpSession pods", func() {
	const ns = "et-pods"

	It("builds the session pod and its token Secret from the agent", func() {
		for name, data := range map[string]map[string]string{
			"creds":      {"api": "api-value", "a": "a-value", "b": "b-value", "id": "key-value"},
			"gw-token":   {"token": "gw-token-value"},
			"git-token":  {"token": "git-token-value"},
			"ssh":        {"key": "ssh-key", "known": "known-hosts"},
			"mcp-secret": {"token": "mcp-token"},
		} {
			sec := etSecret(ns, name, data)
			etCreate(sec)
			etCleanup(sec)
		}
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "skills"},
			Data:       map[string]string{"hello.md": "---\nname: hello\ndescription: says hello\n---\nHello."},
		}
		etCreate(cm)
		etCleanup(cm)

		agent := etReadyAgent(ns, func(s *ompv1alpha1.OmpAgentSpec) {
			s.ServiceAccountName = "runner"
			s.Limits.SessionTimeout = metav1.Duration{Duration: 20 * time.Minute}
			s.Model.Gateway = &ompv1alpha1.GatewaySpec{
				URL:            "http://omp-auth.omp-system.svc:4000",
				TokenSecretRef: &ompv1alpha1.SecretKeyRef{Name: "gw-token", Key: "token"},
			}
			s.Credentials = []ompv1alpha1.Credential{
				{Name: "demo", Description: "demo token", Env: "DEMO_TOKEN", SecretRef: ompv1alpha1.SecretKeyRef{Name: "creds", Key: "api"}},
				{Name: "cert-a", Description: "first file", File: "/etc/creds/a.txt", SecretRef: ompv1alpha1.SecretKeyRef{Name: "creds", Key: "a"}},
				{Name: "cert-b", Description: "second file", File: "/etc/creds/b.txt", SecretRef: ompv1alpha1.SecretKeyRef{Name: "creds", Key: "b"}},
				{Name: "key", Description: "a key", File: "/var/lib/key/id_rsa", SecretRef: ompv1alpha1.SecretKeyRef{Name: "creds", Key: "id"}},
			}
			s.Workspace = &ompv1alpha1.WorkspaceSpec{Git: &ompv1alpha1.GitWorkspaceSource{
				URL: "https://example.com/ws.git",
				Auth: &ompv1alpha1.GitAuth{
					TokenSecretRef: &ompv1alpha1.SecretKeyRef{Name: "git-token", Key: "token"},
				},
			}}
			s.Skills = []ompv1alpha1.SkillSource{
				{Git: &ompv1alpha1.GitSkillSource{
					URL: "git@example.com:org/skills.git",
					Auth: &ompv1alpha1.GitAuth{
						SSHKeySecretRef:     &ompv1alpha1.SecretKeyRef{Name: "ssh", Key: "key"},
						KnownHostsSecretRef: &ompv1alpha1.SecretKeyRef{Name: "ssh", Key: "known"},
					},
				}},
				{ConfigMap: &ompv1alpha1.LocalObjectRef{Name: "skills"}},
			}
			s.MCPServers = []ompv1alpha1.MCPServer{{
				Name: "docs", URL: "https://mcp.example.com/mcp",
				Headers: []ompv1alpha1.ValueOrSecret{{
					Name: "Authorization", SecretRef: &ompv1alpha1.SecretKeyRef{Name: "mcp-secret", Key: "token"},
				}},
			}}
			s.Pod.Labels = map[string]string{"team": "sre"}
			s.Pod.Env = []corev1.EnvVar{{Name: "EXTRA", Value: "yes"}}
		})

		sess := etCreateSession(ns, agent.Name, nil)
		podKey := etAwaitPod(etKey(sess), 1)
		Expect(podKey.Name).To(Equal(sess.Name + "-1"))

		var agentObj ompv1alpha1.OmpAgent
		Expect(k8sClient.Get(ctx, etKey(agent), &agentObj)).To(Succeed())
		session := etGetSession(etKey(sess))
		Expect(session.Status.StartTime).NotTo(BeNil())
		Expect(session.Finalizers).To(ContainElement(ompv1alpha1.FinalizerCleanup))
		Expect(session.Labels).To(HaveKeyWithValue(ompv1alpha1.LabelAgent, agent.Name))
		Expect(session.Labels).To(HaveKeyWithValue(ompv1alpha1.LabelKind, string(ompv1alpha1.SessionKindManual)))
		Expect(session.OwnerReferences).To(HaveLen(1))
		Expect(session.OwnerReferences[0].UID).To(Equal(agentObj.UID))

		// The token Secret: owned by the session, 32 random bytes hex-encoded.
		var token corev1.Secret
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: ns, Name: agentapi.SessionTokenSecretName(sess.Name),
		}, &token)).To(Succeed())
		Expect(string(token.Data[agentapi.TokenKey])).To(MatchRegexp(`^[0-9a-f]{64}$`))
		Expect(token.OwnerReferences).To(HaveLen(1))
		Expect(token.OwnerReferences[0].UID).To(Equal(session.UID))

		var pod corev1.Pod
		Expect(k8sClient.Get(ctx, podKey, &pod)).To(Succeed())
		Expect(metav1.IsControlledBy(&pod, session)).To(BeTrue())
		Expect(pod.Labels).To(And(
			HaveKeyWithValue(ompv1alpha1.LabelManagedBy, ompv1alpha1.ManagedByValue),
			HaveKeyWithValue(ompv1alpha1.LabelAgent, agent.Name),
			HaveKeyWithValue(ompv1alpha1.LabelSession, sess.Name),
			HaveKeyWithValue(ompv1alpha1.LabelRole, string(ompv1alpha1.PodRoleSession)),
			HaveKeyWithValue("team", "sre"),
		))

		spec := pod.Spec
		Expect(spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
		Expect(spec.ServiceAccountName).To(Equal("runner"))
		Expect(spec.AutomountServiceAccountToken).To(HaveValue(BeTrue()))
		Expect(spec.EnableServiceLinks).To(HaveValue(BeFalse()))
		Expect(spec.ActiveDeadlineSeconds).To(HaveValue(Equal(int64((20 + 10) * 60))))
		Expect(spec.SecurityContext.RunAsNonRoot).To(HaveValue(BeTrue()))
		Expect(spec.SecurityContext.RunAsUser).To(HaveValue(Equal(int64(10001))))
		Expect(spec.SecurityContext.RunAsGroup).To(HaveValue(Equal(int64(10001))))
		Expect(spec.SecurityContext.FSGroup).To(HaveValue(Equal(int64(10001))))
		Expect(spec.SecurityContext.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))

		Expect(spec.InitContainers).To(HaveLen(1))
		Expect(spec.Containers).To(HaveLen(1))
		prepare, main := spec.InitContainers[0], spec.Containers[0]
		Expect(prepare.Name).To(Equal("prepare"))
		Expect(prepare.Args).To(Equal([]string{"prepare"}))
		Expect(main.Name).To(Equal("agent"))
		Expect(main.Args).To(Equal([]string{"run"}))
		for _, c := range []corev1.Container{prepare, main} {
			Expect(c.Image).To(Equal(etRuntimeImage))
			Expect(c.SecurityContext.AllowPrivilegeEscalation).To(HaveValue(BeFalse()))
			Expect(c.SecurityContext.ReadOnlyRootFilesystem).To(HaveValue(BeTrue()))
			Expect(c.SecurityContext.Capabilities.Drop).To(ConsistOf(corev1.Capability("ALL")))
			Expect(etMountPaths(c)).To(ContainElements(
				"/workspace", "/opt/mise", "/home/agent", "/tmp", agentapi.RunDir, "/var/run/omp-operator/token"))
			Expect(etEnv(c)).To(And(
				HaveKey("MISE_DATA_DIR"),
			))
			Expect(etEnv(c)[agentapi.EnvSession].Value).To(Equal(ns + "/" + sess.Name))
			Expect(etEnv(c)[agentapi.EnvAPI].Value).To(Equal(etOpts.AgentAPIURL()))
		}

		// Main-only: credentials, MCP secrets, gateway token, kubeconfig, the agent's pod env.
		mainEnv, initEnv := etEnv(main), etEnv(prepare)
		Expect(mainEnv["OMP_GATEWAY_TOKEN"].ValueFrom).To(Equal(etSecretKeyRef("gw-token", "token")))
		Expect(mainEnv["KUBECONFIG"].Value).To(Equal(agentapi.KubeconfigFile))
		Expect(mainEnv["DEMO_TOKEN"].ValueFrom).To(Equal(etSecretKeyRef("creds", "api")))
		Expect(mainEnv[agentapi.MCPEnvName(0, 0)].ValueFrom).To(Equal(etSecretKeyRef("mcp-secret", "token")))
		Expect(mainEnv["EXTRA"].Value).To(Equal("yes"))
		for _, name := range []string{"OMP_GATEWAY_TOKEN", "KUBECONFIG", "DEMO_TOKEN", agentapi.MCPEnvName(0, 0), "EXTRA"} {
			Expect(initEnv).NotTo(HaveKey(name))
		}

		// File credentials: one projected volume per directory, items mapped to base names,
		// mounted in the main container only.
		volume := func(name string) corev1.Volume {
			i := slices.IndexFunc(spec.Volumes, func(v corev1.Volume) bool { return v.Name == name })
			Expect(i).To(BeNumerically(">=", 0), "volume %s", name)
			return spec.Volumes[i]
		}
		mountAt := func(c corev1.Container, path string) []corev1.VolumeMount {
			return slices.DeleteFunc(slices.Clone(c.VolumeMounts), func(m corev1.VolumeMount) bool { return m.MountPath != path })
		}
		for dir, files := range map[string][]string{"/etc/creds": {"a.txt", "b.txt"}, "/var/lib/key": {"id_rsa"}} {
			Expect(etMountPaths(prepare)).NotTo(ContainElement(dir))
			mounts := mountAt(main, dir)
			Expect(mounts).To(HaveLen(1), dir)
			Expect(mounts[0].ReadOnly).To(BeTrue())
			proj := volume(mounts[0].Name).Projected
			Expect(proj).NotTo(BeNil())
			var got []string
			for _, src := range proj.Sources {
				Expect(src.Secret.Name).To(Equal("creds"))
				for _, it := range src.Secret.Items {
					got = append(got, it.Path)
				}
			}
			Expect(got).To(ConsistOf(files))
		}

		// Init-only: git auth and ConfigMap skill mounts.
		for _, path := range []string{
			agentapi.AuthDir("workspace"), agentapi.AuthDir(agentapi.SkillIndexID(0)), agentapi.ConfigMapSkillDir(1),
		} {
			Expect(etMountPaths(prepare)).To(ContainElement(path))
			Expect(etMountPaths(main)).NotTo(ContainElement(path))
		}
		tokenVol := volume(mountAt(main, "/var/run/omp-operator/token")[0].Name)
		Expect(tokenVol.Secret.SecretName).To(Equal(agentapi.SessionTokenSecretName(sess.Name)))
	})

	It("runs a plain agent as the default service account with no token or gateway", func() {
		agent := etReadyAgent(ns, nil)
		sess := etCreateSession(ns, agent.Name, nil)
		podKey := etAwaitPod(etKey(sess), 1)
		var pod corev1.Pod
		Expect(k8sClient.Get(ctx, podKey, &pod)).To(Succeed())
		Expect(pod.Spec.ServiceAccountName).To(Equal("default"))
		Expect(pod.Spec.AutomountServiceAccountToken).To(HaveValue(BeFalse()))
		Expect(pod.Spec.ActiveDeadlineSeconds).To(HaveValue(Equal(int64((30 + 10) * 60))))
		for _, c := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
			Expect(etEnv(c)).NotTo(HaveKey("KUBECONFIG"))
			Expect(etEnv(c)).NotTo(HaveKey("OMP_GATEWAY_TOKEN"))
		}
	})
})

// etWorkspaceSource is a Flux GitRepository without an artifact yet.
func etWorkspaceSource(ns string) *sourcev1.GitRepository {
	return &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: etName("ws")},
		Spec:       sourcev1.GitRepositorySpec{URL: "https://example.com/ws.git", Interval: metav1.Duration{Duration: time.Minute}},
	}
}

var _ = Describe("OmpSession lifecycle", func() {
	const ns = "et-lifecycle"

	// How a finished attempt looks to the reconciler: what the agent API recorded, then how the
	// pod ended.
	type attempt struct {
		report   map[string]any
		pod      func(*corev1.PodStatus)
		deletion bool // the pod vanishes instead of ending
	}

	endAttempt := func(key, pod types.NamespacedName, a attempt) {
		GinkgoHelper()
		if a.report != nil {
			etReport(key, a.report)
		}
		if a.deletion {
			stub := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: pod.Name}}
			Expect(k8sClient.Delete(ctx, stub)).To(Succeed())
			return
		}
		etSetPod(pod, a.pod)
	}
	failedPod := func(s *corev1.PodStatus) {
		s.Phase = corev1.PodFailed
		s.ContainerStatuses = []corev1.ContainerStatus{{Name: "agent", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"},
		}}}
	}

	type finalCase struct {
		attempt
		phase       ompv1alpha1.SessionPhase
		wantVerdict string
		wantErr     string // substring of status.error ("" = none expected)
	}
	DescribeTable("a session whose pod ends in the step 8 result table",
		func(c finalCase) {
			agent := etReadyAgent(ns, nil)
			sess := etCreateSession(ns, agent.Name, nil)
			key := etKey(sess)
			pod := etAwaitPod(key, 1)
			endAttempt(key, pod, c.attempt)

			got := etSessionPhase(key, c.phase)
			Expect(got.Status.CompletionTime).NotTo(BeNil())
			Expect(got.Status.RetryAt).To(BeNil())
			Expect(string(got.Status.Verdict)).To(Equal(c.wantVerdict))
			if c.wantErr == "" {
				Expect(got.Status.Error).To(BeEmpty())
			} else {
				Expect(got.Status.Error).To(ContainSubstring(c.wantErr))
			}

			// The sinks hear about it exactly once, and the Notified condition records it.
			Eventually(func(g Gomega) {
				fresh := etGetSession(key)
				cond := etCond(fresh.Status.Conditions, ompv1alpha1.ConditionNotified)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			}).Should(Succeed())
			posts := etSink.Posts(etSinkPath(ns, agent.Name))
			Expect(posts).To(HaveLen(1))
			body := posts[0].JSON()
			Expect(body["session"]).To(Equal(ns + "/" + sess.Name))
			wantSink := c.wantVerdict
			if c.phase == ompv1alpha1.SessionPhaseFailed {
				wantSink = "failed"
			}
			Expect(body["verdict"]).To(Equal(wantSink))
			Expect(testutil.ToFloat64(metrics.SessionsTotal.WithLabelValues(ns, agent.Name, "", "manual", wantSink))).
				To(BeEquivalentTo(1))
		},
		Entry("a verdict means Succeeded", finalCase{
			attempt: attempt{
				report: map[string]any{"outcome": "completed", "verdict": "fixed", "title": "restarted it", "summary": "done"},
				pod:    etPodSucceeded,
			},
			phase: ompv1alpha1.SessionPhaseSucceeded, wantVerdict: "fixed",
		}),
		Entry("a verdict wins even when the pod failed afterwards", finalCase{
			attempt: attempt{
				report: map[string]any{"outcome": "retryable", "verdict": "needs_owner", "title": "t", "summary": "s"},
				pod:    failedPod,
			},
			phase: ompv1alpha1.SessionPhaseSucceeded, wantVerdict: "needs_owner",
		}),
		Entry("completed without a verdict means Failed", finalCase{
			attempt: attempt{
				report: map[string]any{"outcome": "completed"},
				pod:    etPodSucceeded,
			},
			phase: ompv1alpha1.SessionPhaseFailed, wantErr: "agent ended without calling finish",
		}),
		Entry("an error outcome means Failed with the agent's own error", finalCase{
			attempt: attempt{
				report: map[string]any{"outcome": "error", "error": "session timed out"},
				pod:    etPodSucceeded,
			},
			phase: ompv1alpha1.SessionPhaseFailed, wantErr: "session timed out",
		}),
		Entry("an exceeded pod deadline means Failed", finalCase{
			attempt: attempt{
				pod: func(s *corev1.PodStatus) { s.Phase, s.Reason = corev1.PodFailed, "DeadlineExceeded" },
			},
			phase: ompv1alpha1.SessionPhaseFailed, wantErr: "session timed out",
		}),
	)

	type retryCase struct {
		attempt
		wantErr string
		auth    bool
	}
	DescribeTable("a session that goes back to Pending with a retry time",
		func(c retryCase) {
			agent := etReadyAgent(ns, nil)
			sess := etCreateSession(ns, agent.Name, nil)
			key := etKey(sess)
			pod := etAwaitPod(key, 1)
			before := etClock.Now()
			endAttempt(key, pod, c.attempt)

			got := etSessionPhase(key, ompv1alpha1.SessionPhasePending)
			Expect(got.Status.Attempts).To(Equal(int32(1)))
			Expect(got.Status.CompletionTime).To(BeNil())
			Expect(got.Status.RetryAt).NotTo(BeNil())
			Expect(got.Status.RetryAt.Time).To(BeTemporally("~", before.Add(30*time.Second), 10*time.Second))
			Expect(got.Status.Error).To(ContainSubstring(c.wantErr))

			if !c.auth {
				return
			}
			// The auth notice reaches the sinks once, and is recorded on the agent.
			Eventually(func(g Gomega) {
				posts := etSink.Posts(etSinkPath(ns, agent.Name))
				g.Expect(posts).To(HaveLen(1))
				g.Expect(posts[0].JSON()["title"]).To(Equal(agent.Name + ": model login missing or expired"))
			}).Should(Succeed())
			var fresh ompv1alpha1.OmpAgent
			Expect(k8sClient.Get(ctx, etKey(agent), &fresh)).To(Succeed())
			Expect(fresh.Status.Notices).To(HaveKey("auth"))
		},
		Entry("a retryable outcome", retryCase{
			attempt: attempt{report: map[string]any{"outcome": "retryable"}, pod: failedPod},
			wantErr: "retryable failure",
		}),
		Entry("an auth outcome", retryCase{
			attempt: attempt{report: map[string]any{"outcome": "auth"}, pod: failedPod},
			wantErr: "authentication was rejected", auth: true,
		}),
		Entry("a pod that ended without calling complete", retryCase{
			attempt: attempt{pod: failedPod},
			wantErr: "pod ended without reporting completion",
		}),
		Entry("a pod that disappeared", retryCase{
			attempt: attempt{deletion: true},
			wantErr: "pod disappeared",
		}),
	)

	It("runs the next attempt after the retry delay and fails when attempts run out", func() {
		agent := etReadyAgent(ns, func(s *ompv1alpha1.OmpAgentSpec) { s.Limits.MaxAttempts = 2 })
		sess := etCreateSession(ns, agent.Name, nil)
		key := etKey(sess)
		DeferCleanup(etClock.Reset)

		first := etAwaitPod(key, 1)
		etSetPod(first, failedPod)
		etSessionPhase(key, ompv1alpha1.SessionPhasePending)
		Consistently(func() int { return len(etAgentPods(ns, agent.Name, ompv1alpha1.PodRoleSession)) }).Should(Equal(1))

		etClock.Advance(31 * time.Second)
		etPoke(sess)
		second := etAwaitPod(key, 2)
		Expect(second.Name).To(Equal(sess.Name + "-2"))
		Expect(first.Name).NotTo(Equal(second.Name))

		etSetPod(second, failedPod)
		got := etSessionPhase(key, ompv1alpha1.SessionPhaseFailed)
		Expect(got.Status.Attempts).To(Equal(int32(2)))
		Expect(got.Status.Error).To(ContainSubstring("attempts exhausted (2)"))
		Expect(got.Status.CompletionTime).NotTo(BeNil())
		Expect(etSink.Posts(etSinkPath(ns, agent.Name))).To(HaveLen(1))
	})

	It("fails a pod that does not start in time and retries it", func() {
		agent := etReadyAgent(ns, nil)
		sess := etCreateSession(ns, agent.Name, nil)
		key := etKey(sess)
		DeferCleanup(etClock.Reset)

		pod := etAwaitPod(key, 1)
		Consistently(func(g Gomega) {
			g.Expect(etGetSession(key).Status.Phase).To(Equal(ompv1alpha1.SessionPhaseRunning))
		}).Should(Succeed())

		etClock.Advance(11 * time.Minute) // the pod start limit is 10 minutes
		etPoke(sess)
		got := etSessionPhase(key, ompv1alpha1.SessionPhasePending)
		Expect(got.Status.RetryAt).NotTo(BeNil())
		Expect(got.Status.Error).To(ContainSubstring("pod not running after 10m0s"))
		Eventually(func() bool {
			return etNotFound(k8sClient.Get(ctx, pod, &corev1.Pod{}))
		}).Should(BeTrue(), "the stuck pod is deleted")
	})

	It("fails a session whose agent stays not Ready", func() {
		agent := etBrokenAgent(ns)
		sess := etCreateSession(ns, agent.Name, nil)
		key := etKey(sess)
		DeferCleanup(etClock.Reset)

		etSessionPhase(key, ompv1alpha1.SessionPhasePending)
		Consistently(func(g Gomega) {
			g.Expect(etGetSession(key).Status.Phase).To(Equal(ompv1alpha1.SessionPhasePending))
		}).Should(Succeed())
		Expect(etAgentPods(ns, agent.Name, "")).To(BeEmpty())

		etClock.Advance(31 * time.Minute) // the not-ready limit is 30 minutes
		etPoke(sess)
		got := etSessionPhase(key, ompv1alpha1.SessionPhaseFailed)
		Expect(got.Status.Error).To(Equal("agent not ready: MissingSecret"))
	})

	It("fails a session whose agent does not exist", func() {
		sess := etCreateSession(ns, etName("ghost"), nil)
		got := etSessionPhase(etKey(sess), ompv1alpha1.SessionPhaseFailed)
		Expect(got.Status.Error).To(Equal("agent not found"))
	})

	It("keeps a Flux workspace session waiting until the source has an artifact", func() {
		src := etWorkspaceSource(ns)
		etCreate(src)
		etCleanup(src)
		agent := etReadyAgent(ns, func(s *ompv1alpha1.OmpAgentSpec) {
			s.Workspace = &ompv1alpha1.WorkspaceSpec{FluxSource: &ompv1alpha1.FluxSourceRef{
				Kind: ompv1alpha1.FluxSourceGitRepository, Namespace: ns, Name: src.Name,
			}}
		})
		sess := etCreateSession(ns, agent.Name, nil)
		key := etKey(sess)

		Eventually(func(g Gomega) {
			cond := etCond(etGetSession(key).Status.Conditions, ompv1alpha1.ConditionWorkspaceReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal("NoArtifact"))
		}).Should(Succeed())
		Expect(etGetSession(key).Status.Phase).To(Equal(ompv1alpha1.SessionPhasePending))
		Expect(etAgentPods(ns, agent.Name, "")).To(BeEmpty())
		Eventually(func(g Gomega) {
			posts := etSink.Posts(etSinkPath(ns, agent.Name))
			g.Expect(posts).To(HaveLen(1))
			g.Expect(posts[0].JSON()["title"]).To(Equal(agent.Name + ": workspace source not ready"))
		}).Should(Succeed())

		artifact := fmt.Sprintf(`{"status":{"artifact":{"path":"p.tar.gz","url":"http://source.example/p.tar.gz",`+
			`"revision":"main@sha1:abc","digest":"sha256:%s","lastUpdateTime":"2026-10-18T12:00:00Z"}}}`, strings.Repeat("a", 64))
		stub := &sourcev1.GitRepository{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: src.Name}}
		Expect(k8sClient.Status().Patch(ctx, stub, client.RawPatch(types.MergePatchType, []byte(artifact)))).To(Succeed())

		etAwaitPod(key, 1)
		cond := etCond(etGetSession(key).Status.Conditions, ompv1alpha1.ConditionWorkspaceReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	})
})

var _ = Describe("OmpSession queue", func() {
	const ns = "et-queue"

	It("starts one session per agent: two Pending sessions made together get one pod", func() {
		agent := etReadyAgent(ns, nil)
		first := etSession(ns, agent.Name, nil)
		second := etSession(ns, agent.Name, nil)
		etCreate(first)
		etCreate(second)
		etCleanup(first, second)

		Eventually(func() int { return len(etAgentPods(ns, agent.Name, ompv1alpha1.PodRoleSession)) }).Should(Equal(1))
		Consistently(func() int { return len(etAgentPods(ns, agent.Name, ompv1alpha1.PodRoleSession)) }).Should(Equal(1))

		phases := func() []ompv1alpha1.SessionPhase {
			return []ompv1alpha1.SessionPhase{
				etGetSession(etKey(first)).Status.Phase, etGetSession(etKey(second)).Status.Phase,
			}
		}
		Eventually(phases).Should(ConsistOf(ompv1alpha1.SessionPhaseRunning, ompv1alpha1.SessionPhasePending))

		// When the running one ends, the other gets its pod.
		running, waiting := first, second
		if etGetSession(etKey(first)).Status.Phase != ompv1alpha1.SessionPhaseRunning {
			running, waiting = second, first
		}
		etFinish(etKey(running), etAwaitPod(etKey(running), 1))
		podKey := etAwaitPod(etKey(waiting), 1)
		Expect(podKey.Name).To(Equal(waiting.Name + "-1"))
		Expect(etAgentPods(ns, agent.Name, ompv1alpha1.PodRoleSession)).To(HaveLen(2))
	})

	It("runs pending sessions by priority number, then oldest first", func() {
		agent := etReadyAgent(ns, nil)

		// The blocker holds the agent while the others queue up.
		blocker := etCreateSession(ns, agent.Name, nil)
		blockerPod := etAwaitPod(etKey(blocker), 1)

		withPriority := func(name string, p int32) func(*ompv1alpha1.OmpSession) {
			return func(s *ompv1alpha1.OmpSession) {
				s.Name = name
				s.Spec.Priority = new(p)
			}
		}
		// Creation timestamps have 1 s resolution. hi-b is older than hi-a but sorts after it by
		// name, so only a real age comparison starts it first.
		low := etCreateSession(ns, agent.Name, withPriority(etName("low"), 3))
		time.Sleep(1100 * time.Millisecond)
		hiB := etCreateSession(ns, agent.Name, withPriority(etName("zz-hi-b"), 0))
		time.Sleep(1100 * time.Millisecond)
		hiA := etCreateSession(ns, agent.Name, withPriority(etName("aa-hi-a"), 0))

		Consistently(func() int { return len(etAgentPods(ns, agent.Name, ompv1alpha1.PodRoleSession)) }).Should(Equal(1))

		etFinish(etKey(blocker), blockerPod)
		order := []*ompv1alpha1.OmpSession{hiB, hiA, low}
		for i, next := range order {
			pod := etAwaitPod(etKey(next), 1)
			for _, later := range order[i+1:] {
				got := etGetSession(etKey(later))
				Expect(got.Status.Phase).To(Equal(ompv1alpha1.SessionPhasePending))
				Expect(got.Status.Attempts).To(BeZero())
			}
			etFinish(etKey(next), pod)
		}
	})
})

var _ = Describe("OmpSession gateway gate", func() {
	const ns = "et-gate"

	It("waits until two minutes after the clients Secret was written", func() {
		DeferCleanup(etClock.Reset)
		// No tokenSecretRef: the operator issues the token and writes the clients Secret.
		agent := etReadyAgent(ns, func(s *ompv1alpha1.OmpAgentSpec) {
			s.Model.Gateway = &ompv1alpha1.GatewaySpec{URL: "http://omp-auth.omp-system.svc:4000"}
		})
		Expect(etClock.Now().Sub(etGateway.LastClientsWrite())).To(BeNumerically("<", time.Minute))

		sess := etCreateSession(ns, agent.Name, nil)
		key := etKey(sess)
		etSessionPhase(key, ompv1alpha1.SessionPhasePending)
		Consistently(func(g Gomega) {
			g.Expect(etGetSession(key).Status.Phase).To(Equal(ompv1alpha1.SessionPhasePending))
			g.Expect(etAgentPods(ns, agent.Name, "")).To(BeEmpty())
		}).Should(Succeed())

		etClock.Advance(2*time.Minute + time.Second)
		etPoke(sess)
		podKey := etAwaitPod(key, 1)

		// The managed token reaches the main container only.
		var pod corev1.Pod
		Expect(k8sClient.Get(ctx, podKey, &pod)).To(Succeed())
		Expect(etEnv(pod.Spec.Containers[0])["OMP_GATEWAY_TOKEN"].ValueFrom).
			To(Equal(etSecretKeyRef(gateway.TokenSecretName(agent.Name), gateway.TokenKey)))
		Expect(etEnv(pod.Spec.InitContainers[0])).NotTo(HaveKey("OMP_GATEWAY_TOKEN"))
	})
})
