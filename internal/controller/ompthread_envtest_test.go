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
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/cloudspec"
	"github.com/TechDufus/omp-operator/internal/store"
	"github.com/TechDufus/omp-operator/internal/threads"
)

const (
	etHandoff   = "0123456789012345678901234567890123456789"
	etThreadsNS = "et-threads"
)

// etCloudAgent creates a Ready cloud agent whose github.com host pushes when push is set.
func etCloudAgent(push bool, mutate func(*ompv1alpha1.OmpAgentSpec)) *ompv1alpha1.OmpAgent {
	GinkgoHelper()
	tok := etName("gh")
	etCreate(etSecret(etThreadsNS, tok, map[string]string{"token": "gh-token-value"}))
	etCleanup(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: etThreadsNS, Name: tok}})
	return etReadyAgent(etThreadsNS, func(s *ompv1alpha1.OmpAgentSpec) {
		s.Tools.Allow = []string{"*"}
		s.Cloud = &ompv1alpha1.CloudSpec{Git: []ompv1alpha1.CloudGitHost{{
			Host: "github.com",
			Auth: ompv1alpha1.GitAuth{TokenSecretRef: &ompv1alpha1.SecretKeyRef{Name: tok, Key: "token"}},
			Push: push,
		}}}
		if mutate != nil {
			mutate(s)
		}
	})
}

// etSittingsOf lists the sessions of a thread.
func etSittingsOf(ns, thread string) []ompv1alpha1.OmpSession {
	GinkgoHelper()
	var l ompv1alpha1.OmpSessionList
	Expect(k8sClient.List(ctx, &l, client.InNamespace(ns), client.MatchingLabels{ompv1alpha1.LabelThread: thread})).To(Succeed())
	return l.Items
}

// etCreateThread creates a thread of the agent and arranges for its removal and that of its
// sittings (nothing garbage-collects them in envtest): the thread goes first so the reconciler
// cannot start a new sitting.
func etCreateThread(agent *ompv1alpha1.OmpAgent, mutate func(*ompv1alpha1.OmpThreadSpec)) *ompv1alpha1.OmpThread {
	GinkgoHelper()
	th := &ompv1alpha1.OmpThread{
		ObjectMeta: metav1.ObjectMeta{Namespace: agent.Namespace, Name: etName("thr")},
		Spec: ompv1alpha1.OmpThreadSpec{
			AgentRef: ompv1alpha1.LocalObjectRef{Name: agent.Name},
			Owner:    "token:dev",
			Title:    "fix the flake",
			Repo: ompv1alpha1.ThreadRepo{
				Name:          "app",
				RemoteURL:     "git@github.com:acme/app.git",
				BaseRef:       "main",
				BaseCommit:    "1111111111111111111111111111111111111111",
				HandoffCommit: etHandoff,
				LocalBranch:   "feature/x",
			},
			Author: ompv1alpha1.GitAuthor{Name: "Me", Email: "me@example.com"},
		},
	}
	if mutate != nil {
		mutate(&th.Spec)
	}
	etCreate(th)
	DeferCleanup(func() {
		for _, s := range etSittingsOf(th.Namespace, th.Name) {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &s))).To(Succeed())
			Eventually(func() bool { return etNotFound(k8sClient.Get(ctx, etKey(&s), &ompv1alpha1.OmpSession{})) }).Should(BeTrue())
		}
		Expect(etStore.RemoveThread(th.Namespace, th.Name)).To(Succeed())
	})
	etCleanup(th)
	return th
}

// etEnqueue queues a message the way the cloud API does: into the turn log, then a signal.
func etEnqueue(th *ompv1alpha1.OmpThread, text string) {
	GinkgoHelper()
	_, err := etThreads.Enqueue(th.Namespace, th.Name, text, "token:dev")
	Expect(err).NotTo(HaveOccurred())
	etThreads.Signal(th.Namespace, th.Name)
}

// etSitting waits for the n-th sitting of the thread and returns its key.
func etSitting(th *ompv1alpha1.OmpThread, n int) types.NamespacedName {
	GinkgoHelper()
	key := types.NamespacedName{Namespace: th.Namespace, Name: th.Name + "-" + string(rune('0'+n))}
	Eventually(func() error { return k8sClient.Get(ctx, key, &ompv1alpha1.OmpSession{}) }).Should(Succeed())
	return key
}

// etTakeTurn plays the driver taking the sitting's next turn.
func etTakeTurn(sitting types.NamespacedName, thread string) threads.Turn {
	GinkgoHelper()
	turn, err := etThreads.Next(ctx, sitting.Namespace, thread, sitting.Name, 0)
	Expect(err).NotTo(HaveOccurred())
	Expect(turn).NotTo(BeNil(), "no turn to take")
	return *turn
}

// etCompleteTurn plays the driver reporting a finished turn through the agent API's path.
func etCompleteTurn(sitting types.NamespacedName, thread string, id int, r threads.Result) {
	GinkgoHelper()
	Expect(etThreads.Complete(sitting.Namespace, thread, sitting.Name, id, r)).To(Succeed())
	etThreads.Signal(sitting.Namespace, thread)
}

// etEndSitting plays the driver exiting after an idle timeout: outcome completed, pod exit 0.
func etEndSitting(sitting types.NamespacedName) {
	GinkgoHelper()
	pod := etAwaitPod(sitting, 1)
	etReport(sitting, map[string]any{"outcome": "completed"})
	etSetPod(pod, etPodSucceeded)
	etSessionPhase(sitting, ompv1alpha1.SessionPhaseSucceeded)
}

func etThreadOf(th *ompv1alpha1.OmpThread) *ompv1alpha1.OmpThread {
	GinkgoHelper()
	got := &ompv1alpha1.OmpThread{}
	Expect(k8sClient.Get(ctx, etKey(th), got)).To(Succeed())
	return got
}

var _ = Describe("Concurrent sessions", func() {
	const ns = "et-threads"

	It("runs up to maxConcurrentSessions sessions of an agent at once", func() {
		agent := etReadyAgent(ns, func(s *ompv1alpha1.OmpAgentSpec) { s.Limits.MaxConcurrentSessions = 2 })
		a := etSession(ns, agent.Name, nil)
		b := etSession(ns, agent.Name, nil)
		c := etSession(ns, agent.Name, nil)
		etCreate(a)
		etCreate(b)
		etCreate(c)
		etCleanup(a, b, c)

		pods := func() int { return len(etAgentPods(ns, agent.Name, ompv1alpha1.PodRoleSession)) }
		Eventually(pods).Should(Equal(2))
		Consistently(pods).Should(Equal(2))

		running := func() []*ompv1alpha1.OmpSession {
			var out []*ompv1alpha1.OmpSession
			for _, s := range []*ompv1alpha1.OmpSession{a, b, c} {
				if etGetSession(etKey(s)).Status.Phase == ompv1alpha1.SessionPhaseRunning {
					out = append(out, s)
				}
			}
			return out
		}
		Expect(running()).To(HaveLen(2))
		waiting := slices.IndexFunc([]*ompv1alpha1.OmpSession{a, b, c}, func(s *ompv1alpha1.OmpSession) bool {
			return etGetSession(etKey(s)).Status.Phase == ompv1alpha1.SessionPhasePending
		})
		third := []*ompv1alpha1.OmpSession{a, b, c}[waiting]

		// A freed slot goes to the waiting session.
		first := running()[0]
		etFinish(etKey(first), etAwaitPod(etKey(first), 1))
		etAwaitPod(etKey(third), 1)
	})
})

var _ = Describe("OmpThread", func() {
	const ns = "et-threads"

	It("starts one sitting for queued turns and ends it without a verdict", func() {
		agent := etCloudAgent(false, nil)
		th := etCreateThread(agent, nil)
		etEnqueue(th, "first message")

		sittingKey := etSitting(th, 1)
		pod := etAwaitPod(sittingKey, 1)
		got := etGetSession(sittingKey)
		Expect(got.Spec.ThreadRef).To(Equal(&ompv1alpha1.LocalObjectRef{Name: th.Name}))
		Expect(got.Spec.AgentRef.Name).To(Equal(agent.Name))
		Expect(got.Spec.Prompt).To(Equal("first message"))
		Expect(got.Spec.ContinuityKey).To(Equal(cloudspec.ContinuityKey(ns, th.Name)))
		Expect(got.Labels).To(HaveKeyWithValue(ompv1alpha1.LabelKind, "cloud"))
		Expect(got.Labels).To(HaveKeyWithValue(ompv1alpha1.LabelThread, th.Name))
		Expect(got.Labels).To(HaveKeyWithValue(ompv1alpha1.LabelContinuity, store.ContinuityID(cloudspec.ContinuityKey(ns, th.Name))))
		ctrlRef := metav1.GetControllerOf(got)
		Expect(ctrlRef).NotTo(BeNil())
		Expect(ctrlRef.Kind).To(Equal("OmpThread"))
		Expect(ctrlRef.Name).To(Equal(th.Name))
		Expect(pod.Name).To(Equal(got.Name + "-1"))

		// Further messages join the running sitting; no second one starts.
		etEnqueue(th, "second message")
		Consistently(func() int { return len(etSittingsOf(ns, th.Name)) }).Should(Equal(1))
		Eventually(func(g Gomega) {
			cur := etThreadOf(th)
			g.Expect(cur.Status.Phase).To(Equal(ompv1alpha1.ThreadPhaseWorking))
			g.Expect(cur.Status.Turns).To(BeEquivalentTo(2))
			g.Expect(cur.Status.ActiveSession).To(Equal(got.Name))
		}).Should(Succeed())

		// The sitting takes both turns.
		for i := 1; i <= 2; i++ {
			turn := etTakeTurn(sittingKey, th.Name)
			Expect(turn.ID).To(Equal(i))
			etCompleteTurn(sittingKey, th.Name, turn.ID, threads.Result{
				Outcome: threads.OutcomeCompleted, Reply: "# Did " + turn.Text + "\nbody",
				Head: etHandoff, FilesChanged: 1, Insertions: 3, Deletions: 1,
			})
		}
		Eventually(func(g Gomega) {
			cur := etThreadOf(th)
			g.Expect(cur.Status.Phase).To(Equal(ompv1alpha1.ThreadPhaseIdle))
			g.Expect(cur.Status.Latest).NotTo(BeNil())
			g.Expect(cur.Status.Latest.ID).To(BeEquivalentTo(2))
			g.Expect(cur.Status.Latest.State).To(Equal(ompv1alpha1.TurnStateDone))
			g.Expect(cur.Status.Latest.Title).To(Equal("Did second message"))
			g.Expect(cur.Status.Result).NotTo(BeNil())
			g.Expect(cur.Status.Result.Branch).To(Equal("omp-cloud/" + th.Name))
			g.Expect(cur.Status.Result.Head).To(Equal(etHandoff))
			g.Expect(cur.Status.Result.Base).To(Equal(th.Spec.Repo.BaseCommit))
			g.Expect(cur.Status.Result.FilesChanged).To(BeEquivalentTo(1))
			g.Expect(cur.Status.LastActivityAt).NotTo(BeNil())
		}).Should(Succeed())

		// The sitting idles out: Succeeded, no verdict, and its notification is the thread's job.
		etEndSitting(sittingKey)
		ended := etGetSession(sittingKey)
		Expect(ended.Status.Verdict).To(BeEmpty())
		Eventually(func(g Gomega) {
			c := etCond(etGetSession(sittingKey).Status.Conditions, ompv1alpha1.ConditionNotified)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(c.Reason).To(Equal("ThreadTurns"))
		}).Should(Succeed())
		Consistently(func() int { return len(etSittingsOf(ns, th.Name)) }).Should(Equal(1))
	})

	It("is Waiting with status.question while the running turn has an unanswered question", func() {
		agent := etCloudAgent(false, nil)
		th := etCreateThread(agent, nil)
		etEnqueue(th, "migrate the database")
		sittingKey := etSitting(th, 1)
		turn := etTakeTurn(sittingKey, th.Name)
		Eventually(func() ompv1alpha1.ThreadPhase { return etThreadOf(th).Status.Phase }).Should(Equal(ompv1alpha1.ThreadPhaseWorking))
		Expect(etThreadOf(th).Status.Question).To(BeNil())

		// The driver's ask path: record the ask, then signal.
		_, _, err := etThreads.Ask(th.Namespace, th.Name, sittingKey.Name, turn.ID, []threads.QuestionInput{
			{Key: "db", Text: "Which database?", Options: []threads.QuestionOption{{Label: "sqlite"}, {Label: "postgres"}}},
			{Key: "why", Text: "Why that one?"},
		})
		Expect(err).NotTo(HaveOccurred())
		etThreads.Signal(th.Namespace, th.Name)
		Eventually(func(g Gomega) {
			cur := etThreadOf(th)
			g.Expect(cur.Status.Phase).To(Equal(ompv1alpha1.ThreadPhaseWaiting))
			g.Expect(cur.Status.Question).NotTo(BeNil())
			g.Expect(cur.Status.Question.Turn).To(BeEquivalentTo(turn.ID))
			g.Expect(cur.Status.Question.ID).To(BeEquivalentTo(1))
			g.Expect(cur.Status.Question.Text).To(Equal("Which database?"))
			g.Expect(cur.Status.Question.AskedAt.IsZero()).To(BeFalse())
		}).Should(Succeed())

		// Answering the first moves status.question to the next one of the ask.
		_, err = etThreads.Answer(th.Namespace, th.Name, turn.ID, 1, []string{"sqlite"}, "", "token:dev")
		Expect(err).NotTo(HaveOccurred())
		etThreads.Signal(th.Namespace, th.Name)
		Eventually(func(g Gomega) {
			cur := etThreadOf(th)
			g.Expect(cur.Status.Phase).To(Equal(ompv1alpha1.ThreadPhaseWaiting))
			g.Expect(cur.Status.Question).NotTo(BeNil())
			g.Expect(cur.Status.Question.ID).To(BeEquivalentTo(2))
		}).Should(Succeed())

		// The last answer ends the wait: Working again, no question.
		_, err = etThreads.Answer(th.Namespace, th.Name, turn.ID, 2, nil, "it is embedded", "token:dev")
		Expect(err).NotTo(HaveOccurred())
		etThreads.Signal(th.Namespace, th.Name)
		Eventually(func(g Gomega) {
			cur := etThreadOf(th)
			g.Expect(cur.Status.Phase).To(Equal(ompv1alpha1.ThreadPhaseWorking))
			g.Expect(cur.Status.Question).To(BeNil())
		}).Should(Succeed())

		// A stop expires an open question, so the thread is not Waiting any more.
		_, _, err = etThreads.Ask(th.Namespace, th.Name, sittingKey.Name, turn.ID, []threads.QuestionInput{{Key: "more", Text: "And now?"}})
		Expect(err).NotTo(HaveOccurred())
		etThreads.Signal(th.Namespace, th.Name)
		Eventually(func() ompv1alpha1.ThreadPhase { return etThreadOf(th).Status.Phase }).Should(Equal(ompv1alpha1.ThreadPhaseWaiting))
		_, _, err = etThreads.Stop(th.Namespace, th.Name, "token:dev")
		Expect(err).NotTo(HaveOccurred())
		etThreads.Signal(th.Namespace, th.Name)
		Eventually(func(g Gomega) {
			cur := etThreadOf(th)
			g.Expect(cur.Status.Phase).To(Equal(ompv1alpha1.ThreadPhaseWorking))
			g.Expect(cur.Status.Question).To(BeNil())
		}).Should(Succeed())
	})

	It("notifies each finished turn once", func() {
		agent := etCloudAgent(false, nil)
		th := etCreateThread(agent, nil)
		path := etSinkPath(ns, agent.Name)
		etEnqueue(th, "do it")
		sittingKey := etSitting(th, 1)
		etAwaitPod(sittingKey, 1)

		turn := etTakeTurn(sittingKey, th.Name)
		etCompleteTurn(sittingKey, th.Name, turn.ID, threads.Result{
			Outcome: threads.OutcomeCompleted, Reply: "Added Greet\nmore", Head: etHandoff, FilesChanged: 2,
		})
		Eventually(func() []etPost { return etSink.Posts(path) }).Should(HaveLen(1))
		post := etSink.Posts(path)[0].JSON()
		Expect(post["verdict"]).To(Equal("fixed"))
		Expect(post["title"]).To(Equal("fix the flake: Added Greet"))
		Expect(post["session"]).To(Equal(ns + "/" + th.Name))
		Expect(post["url"]).To(Equal("https://dash.example.com/threads/" + ns + "/" + th.Name))

		// Reconciling again does not tell the sinks twice, and the sitting's end adds nothing.
		etPoke(th)
		etEndSitting(sittingKey)
		Consistently(func() []etPost { return etSink.Posts(path) }).Should(HaveLen(1))
		Eventually(func() bool {
			ts, err := etThreads.Turns(ns, th.Name)
			return err == nil && len(ts) == 1 && ts[0].NotifiedAt != nil
		}).Should(BeTrue())
	})

	It("puts the running turn back and starts the next sitting when a sitting ends", func() {
		agent := etCloudAgent(false, nil)
		th := etCreateThread(agent, nil)
		etEnqueue(th, "long job")
		first := etSitting(th, 1)
		etAwaitPod(first, 1)
		turn := etTakeTurn(first, th.Name)
		Expect(turn.Session).To(Equal(first.Name))

		// The sitting ends cleanly while its turn is still running (the driver was told to stop
		// at its sitting limit): the turn is redone by the next sitting.
		etEndSitting(first)
		second := etSitting(th, 2)
		Expect(second.Name).To(Equal(th.Name + "-2"))
		Expect(etGetSession(second).Spec.Prompt).To(Equal("long job"))
		etAwaitPod(second, 1)
		redone := etTakeTurn(second, th.Name)
		Expect(redone.ID).To(Equal(turn.ID))
		Expect(redone.Session).To(Equal(second.Name))
	})

	It("fails the open turns of a Failed sitting and starts no new sitting for them", func() {
		agent := etCloudAgent(false, func(s *ompv1alpha1.OmpAgentSpec) { s.Limits.MaxAttempts = 1 })
		th := etCreateThread(agent, nil)
		path := etSinkPath(ns, agent.Name)
		etEnqueue(th, "one")
		sittingKey := etSitting(th, 1)
		pod := etAwaitPod(sittingKey, 1)
		etTakeTurn(sittingKey, th.Name) // running
		etEnqueue(th, "two")            // queued

		etSetPod(pod, func(s *corev1.PodStatus) { s.Phase = corev1.PodFailed })
		failed := etSessionPhase(sittingKey, ompv1alpha1.SessionPhaseFailed)

		Eventually(func(g Gomega) {
			ts, err := etThreads.Turns(ns, th.Name)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(ts).To(HaveLen(2))
			for _, tn := range ts {
				g.Expect(tn.State).To(Equal(ompv1alpha1.TurnStateFailed))
				g.Expect(tn.Error).To(HavePrefix("cloud session failed: "))
				g.Expect(tn.Error).To(ContainSubstring(failed.Status.Error))
			}
			g.Expect(etGetSession(sittingKey).Annotations).To(HaveKeyWithValue(AnnotationTurnsFailed, "true"))
		}).Should(Succeed())
		Consistently(func() int { return len(etSittingsOf(ns, th.Name)) }).Should(Equal(1))
		Eventually(func() []etPost { return etSink.Posts(path) }).Should(HaveLen(2)) // one per failed turn
		Expect(etSink.Posts(path)[0].JSON()["verdict"]).To(Equal("failed"))
		Eventually(func() ompv1alpha1.ThreadPhase { return etThreadOf(th).Status.Phase }).Should(Equal(ompv1alpha1.ThreadPhaseIdle))

		// A message sent afterwards gets a fresh sitting.
		etEnqueue(th, "three")
		next := etSitting(th, 2)
		Expect(etGetSession(next).Spec.Prompt).To(Equal("three"))
	})

	It("deletes an Idle thread after the agent's retention", func() {
		DeferCleanup(etClock.Reset)
		agent := etCloudAgent(false, func(s *ompv1alpha1.OmpAgentSpec) {
			s.Cloud.ThreadRetention = metav1.Duration{Duration: time.Hour}
		})
		th := etCreateThread(agent, nil)
		Eventually(func() ompv1alpha1.ThreadPhase { return etThreadOf(th).Status.Phase }).Should(Equal(ompv1alpha1.ThreadPhaseIdle))
		Consistently(func() error { return k8sClient.Get(ctx, etKey(th), &ompv1alpha1.OmpThread{}) }).Should(Succeed())

		etClock.Advance(2 * time.Hour)
		etPoke(th)
		Eventually(func() bool { return etNotFound(k8sClient.Get(ctx, etKey(th), &ompv1alpha1.OmpThread{})) }).Should(BeTrue())
	})

	It("keeps a Working thread however old it is", func() {
		DeferCleanup(etClock.Reset)
		agent := etCloudAgent(false, func(s *ompv1alpha1.OmpAgentSpec) {
			s.Cloud.ThreadRetention = metav1.Duration{Duration: time.Hour}
		})
		th := etCreateThread(agent, nil)
		etEnqueue(th, "slow")
		etSitting(th, 1)
		etClock.Advance(2 * time.Hour)
		etPoke(th)
		Consistently(func() error { return k8sClient.Get(ctx, etKey(th), &ompv1alpha1.OmpThread{}) }).Should(Succeed())
	})

	It("removes the thread's files and conversation with it, but a sitting keeps them", func() {
		agent := etCloudAgent(false, nil)
		th := etCreateThread(agent, nil)
		id := store.ContinuityID(cloudspec.ContinuityKey(ns, th.Name))
		Expect(etStore.PutThreadFile(ns, th.Name, "source.bundle", strings.NewReader("bundle"), 0)).To(Succeed())
		Expect(etStore.PutContinuity(ns, agent.Name, id, strings.NewReader("conversation"))).To(Succeed())

		// Deleting a sitting leaves the shared conversation alone.
		etEnqueue(th, "go")
		sittingKey := etSitting(th, 1)
		turn := etTakeTurn(sittingKey, th.Name)
		etCompleteTurn(sittingKey, th.Name, turn.ID, threads.Result{Outcome: threads.OutcomeCompleted, Reply: "ok"})
		etEndSitting(sittingKey)
		sitting := etGetSession(sittingKey)
		Expect(k8sClient.Delete(ctx, sitting)).To(Succeed())
		Eventually(func() bool { return etNotFound(k8sClient.Get(ctx, sittingKey, &ompv1alpha1.OmpSession{})) }).Should(BeTrue())
		f, err := etStore.OpenContinuity(ns, agent.Name, id)
		Expect(err).NotTo(HaveOccurred())
		Expect(f.Close()).To(Succeed())

		// Deleting the thread removes both.
		Expect(k8sClient.Delete(ctx, th)).To(Succeed())
		Eventually(func() bool { return etNotFound(k8sClient.Get(ctx, etKey(th), &ompv1alpha1.OmpThread{})) }).Should(BeTrue())
		_, err = etStore.OpenThreadFile(ns, th.Name, "source.bundle")
		Expect(err).To(MatchError(store.ErrNotFound))
		_, err = etStore.OpenContinuity(ns, agent.Name, id)
		Expect(err).To(MatchError(store.ErrNotFound))
	})

	It("reports a missing agent", func() {
		ghost := &ompv1alpha1.OmpAgent{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: etName("ghost")}}
		th := etCreateThread(ghost, nil)
		Eventually(func(g Gomega) {
			c := etCond(etThreadOf(th).Status.Conditions, ompv1alpha1.ConditionReady)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(c.Reason).To(Equal("AgentNotFound"))
		}).Should(Succeed())
	})
})

var _ = Describe("Thread sitting pods", func() {
	threadWithSecrets := func(s *ompv1alpha1.OmpThreadSpec) {
		s.Paths = ompv1alpha1.ThreadPaths{Home: "/home/techdufus", Workspace: "/home/techdufus/dev/app"}
		s.SecretFiles = []string{".env"}
		s.SecretEnv = []string{"API_KEY"}
	}
	podOf := func(th *ompv1alpha1.OmpThread) corev1.Pod {
		GinkgoHelper()
		etEnqueue(th, "go")
		key := etSitting(th, 1)
		podKey := etAwaitPod(key, 1)
		var pod corev1.Pod
		Expect(k8sClient.Get(ctx, podKey, &pod)).To(Succeed())
		return pod
	}
	mountNames := func(c corev1.Container) []string {
		out := make([]string, 0, len(c.VolumeMounts))
		for _, m := range c.VolumeMounts {
			out = append(out, m.Name)
		}
		return out
	}

	It("mirrors the user's paths, keeps the git credential to prepare and the sidecar, and pushes", func() {
		agent := etCloudAgent(true, nil)
		th := etCreateThread(agent, threadWithSecrets)
		pod := podOf(th)

		Expect(pod.Labels).To(HaveKeyWithValue(ompv1alpha1.LabelThread, th.Name))
		Expect(pod.Spec.TerminationGracePeriodSeconds).To(HaveValue(BeEquivalentTo(60)))

		Expect(pod.Spec.InitContainers).To(HaveLen(2))
		prepare, push := pod.Spec.InitContainers[0], pod.Spec.InitContainers[1]
		agentC := pod.Spec.Containers[0]
		Expect(prepare.Name).To(Equal("prepare"))
		Expect(push.Name).To(Equal("gitpush"))
		Expect(push.RestartPolicy).To(HaveValue(Equal(corev1.ContainerRestartPolicyAlways)))

		for _, c := range []corev1.Container{prepare, agentC} {
			mounts := map[string]string{}
			for _, m := range c.VolumeMounts {
				mounts[m.MountPath] = m.Name
			}
			Expect(mounts).To(HaveKeyWithValue("/home/techdufus/dev/app", "workspace"), c.Name)
			Expect(mounts).To(HaveKeyWithValue("/home/techdufus", "home"), c.Name)
			Expect(c.WorkingDir).To(Equal("/home/techdufus/dev/app"))
			env := map[string]string{}
			for _, e := range c.Env {
				env[e.Name] = e.Value
			}
			Expect(env).To(HaveKeyWithValue("HOME", "/home/techdufus"), c.Name)
			Expect(env).To(HaveKeyWithValue("OMP_OPERATOR_WORKSPACE", "/home/techdufus/dev/app"), c.Name)
			Expect(env["PATH"]).To(HavePrefix("/opt/mise/data/shims:"), c.Name)
		}

		// The credential is mounted in prepare and the sidecar only.
		Expect(mountNames(prepare)).To(ContainElement("auth-cloud-git"))
		Expect(mountNames(push)).To(ContainElement("auth-cloud-git"))
		Expect(mountNames(agentC)).NotTo(ContainElement("auth-cloud-git"))
		// The shipped files come in through prepare; the variables are the agent container's.
		Expect(mountNames(prepare)).To(ContainElement("thread-secrets"))
		Expect(mountNames(agentC)).NotTo(ContainElement("thread-secrets"))
		Expect(mountNames(push)).NotTo(ContainElement("thread-secrets"))
		hasEnv := func(c corev1.Container, name string) bool {
			return slices.ContainsFunc(c.Env, func(e corev1.EnvVar) bool { return e.Name == name })
		}
		Expect(hasEnv(agentC, "API_KEY")).To(BeTrue())
		Expect(hasEnv(prepare, "API_KEY")).To(BeFalse())
		Expect(hasEnv(push, "API_KEY")).To(BeFalse())
		Expect(hasEnv(agentC, "OMP_GITPUSH_ADDR")).To(BeTrue())
		Expect(hasEnv(push, "OMP_GITPUSH_URL")).To(BeTrue())
	})

	It("has no sidecar and no push address without push, and defaults the paths without any", func() {
		agent := etCloudAgent(false, nil)
		th := etCreateThread(agent, nil)
		pod := podOf(th)

		Expect(pod.Spec.InitContainers).To(HaveLen(1))
		agentC := pod.Spec.Containers[0]
		Expect(slices.ContainsFunc(agentC.Env, func(e corev1.EnvVar) bool { return e.Name == "OMP_GITPUSH_ADDR" })).To(BeFalse())
		Expect(agentC.WorkingDir).To(Equal("/workspace"))
		for _, e := range agentC.Env {
			if e.Name == "HOME" {
				Expect(e.Value).To(Equal("/home/agent"))
			}
		}
	})
})
