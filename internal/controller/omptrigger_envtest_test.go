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
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/store"
)

var _ = Describe("OmpTrigger", func() {
	const ns = "et-triggers"

	const goodPrompt = "{{ .Event.Name }} is {{ .Event.Status }}"

	bearer := func(secret string) ompv1alpha1.WebhookAuth {
		return ompv1alpha1.WebhookAuth{Bearer: &ompv1alpha1.BearerAuth{
			SecretRef: ompv1alpha1.SecretKeyRef{Name: secret, Key: "token"},
		}}
	}
	trigger := func(agent string, mutate func(*ompv1alpha1.OmpTriggerSpec)) *ompv1alpha1.OmpTrigger {
		t := &ompv1alpha1.OmpTrigger{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: etName("trigger")},
			Spec:       ompv1alpha1.OmpTriggerSpec{AgentRef: ompv1alpha1.LocalObjectRef{Name: agent}, Prompt: goodPrompt},
		}
		mutate(&t.Spec)
		return t
	}
	webhook := func(secret string, mutate func(*ompv1alpha1.WebhookSpec)) func(*ompv1alpha1.OmpTriggerSpec) {
		return func(s *ompv1alpha1.OmpTriggerSpec) {
			s.Webhook = &ompv1alpha1.WebhookSpec{Adapter: ompv1alpha1.AdapterAlertmanager, Auth: bearer(secret)}
			if mutate != nil {
				mutate(s.Webhook)
			}
		}
	}
	cronSpec := func(expr string) func(*ompv1alpha1.OmpTriggerSpec) {
		return func(s *ompv1alpha1.OmpTriggerSpec) {
			s.Schedule = &ompv1alpha1.ScheduleSpec{Cron: expr}
			s.Prompt = "run {{ .Trigger }} at {{ .Now }}" // schedule prompts have no .Event
		}
	}
	triggerReady := func(key types.NamespacedName, status metav1.ConditionStatus, reason string) *ompv1alpha1.OmpTrigger {
		GinkgoHelper()
		var got ompv1alpha1.OmpTrigger
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
			c := etCond(got.Status.Conditions, ompv1alpha1.ConditionReady)
			g.Expect(c).NotTo(BeNil(), "no Ready condition yet")
			g.Expect(c.Status).To(Equal(status), c.Message)
			g.Expect(c.Reason).To(Equal(reason), c.Message)
		}).Should(Succeed())
		return &got
	}

	var agent *ompv1alpha1.OmpAgent
	var secret *corev1.Secret
	BeforeEach(func() {
		agent = etAgent(ns, etName("agent"), nil)
		etCreate(agent)
		etCleanup(agent)
		secret = etSecret(ns, etName("hook"), map[string]string{
			"token": "hook-token", "username": "u", "password": "p",
		})
		etCreate(secret)
		etCleanup(secret)
	})

	type reasonCase struct {
		reason string
		spec   func(agent, secret string) func(*ompv1alpha1.OmpTriggerSpec)
		typ    ompv1alpha1.TriggerType
	}
	DescribeTable("decides Ready with the documented reasons",
		func(c reasonCase) {
			t := trigger(agent.Name, c.spec(agent.Name, secret.Name))
			etCreate(t)
			etCleanup(t)
			want := metav1.ConditionFalse
			if c.reason == "Ready" {
				want = metav1.ConditionTrue
			}
			got := triggerReady(etKey(t), want, c.reason)
			Expect(got.Status.Type).To(Equal(c.typ))
			if c.typ == ompv1alpha1.TriggerWebhook {
				Expect(got.Status.HookPath).To(Equal("/hooks/" + ns + "/" + t.Name))
			} else {
				Expect(got.Status.HookPath).To(BeEmpty())
			}
		},
		Entry("a webhook with a bearer Secret", reasonCase{
			reason: "Ready", typ: ompv1alpha1.TriggerWebhook,
			spec: func(_, secret string) func(*ompv1alpha1.OmpTriggerSpec) { return webhook(secret, nil) },
		}),
		Entry("a schedule", reasonCase{
			reason: "Ready", typ: ompv1alpha1.TriggerSchedule,
			spec: func(_, _ string) func(*ompv1alpha1.OmpTriggerSpec) { return cronSpec("*/5 * * * *") },
		}),
		Entry("a generic webhook with no mapping", reasonCase{
			reason: "InvalidSpec", typ: ompv1alpha1.TriggerWebhook,
			spec: func(_, secret string) func(*ompv1alpha1.OmpTriggerSpec) {
				return webhook(secret, func(w *ompv1alpha1.WebhookSpec) { w.Adapter = ompv1alpha1.AdapterGeneric })
			},
		}),
		Entry("episode resume on a schedule", reasonCase{
			reason: "InvalidSpec", typ: ompv1alpha1.TriggerSchedule,
			spec: func(_, _ string) func(*ompv1alpha1.OmpTriggerSpec) {
				return func(s *ompv1alpha1.OmpTriggerSpec) {
					cronSpec("0 * * * *")(s)
					s.Session.Resume = ompv1alpha1.ResumeEpisode
				}
			},
		}),
		Entry("a filter that does not parse", reasonCase{
			reason: "InvalidExpression", typ: ompv1alpha1.TriggerWebhook,
			spec: func(_, secret string) func(*ompv1alpha1.OmpTriggerSpec) {
				return webhook(secret, func(w *ompv1alpha1.WebhookSpec) { w.Filter = "event.severity ==" })
			},
		}),
		Entry("an alert filter that is not a bool", reasonCase{
			reason: "InvalidExpression", typ: ompv1alpha1.TriggerWebhook,
			spec: func(_, secret string) func(*ompv1alpha1.OmpTriggerSpec) {
				return webhook(secret, func(w *ompv1alpha1.WebhookSpec) { w.AlertFilter = "1 + 1" })
			},
		}),
		Entry("a prompt template that does not parse", reasonCase{
			reason: "InvalidPrompt", typ: ompv1alpha1.TriggerWebhook,
			spec: func(_, secret string) func(*ompv1alpha1.OmpTriggerSpec) {
				return func(s *ompv1alpha1.OmpTriggerSpec) {
					webhook(secret, nil)(s)
					s.Prompt = "{{ .Event.Name"
				}
			},
		}),
		Entry("an auth Secret that does not exist", reasonCase{
			reason: "MissingSecret", typ: ompv1alpha1.TriggerWebhook,
			spec: func(_, _ string) func(*ompv1alpha1.OmpTriggerSpec) { return webhook("no-such-secret", nil) },
		}),
		Entry("an auth Secret without the referenced key", reasonCase{
			reason: "MissingSecret", typ: ompv1alpha1.TriggerWebhook,
			spec: func(_, secret string) func(*ompv1alpha1.OmpTriggerSpec) {
				return webhook(secret, func(w *ompv1alpha1.WebhookSpec) {
					w.Auth = ompv1alpha1.WebhookAuth{HMAC: &ompv1alpha1.HMACAuth{
						SecretRef: ompv1alpha1.SecretKeyRef{Name: secret, Key: "absent-key"},
					}}
				})
			},
		}),
		Entry("a cron descriptor", reasonCase{
			reason: "InvalidSchedule", typ: ompv1alpha1.TriggerSchedule,
			spec: func(_, _ string) func(*ompv1alpha1.OmpTriggerSpec) { return cronSpec("@every 1m") },
		}),
		Entry("a cron with six fields", reasonCase{
			reason: "InvalidSchedule", typ: ompv1alpha1.TriggerSchedule,
			spec: func(_, _ string) func(*ompv1alpha1.OmpTriggerSpec) { return cronSpec("0 0 * * * *") },
		}),
	)

	It("waits for its agent and becomes Ready when the agent appears", func() {
		name := etName("late-agent")
		t := trigger(name, webhook(secret.Name, nil))
		etCreate(t)
		etCleanup(t)
		got := triggerReady(etKey(t), metav1.ConditionFalse, "AgentNotFound")
		Expect(got.Status.Type).To(Equal(ompv1alpha1.TriggerWebhook))

		late := etAgent(ns, name, nil)
		etCreate(late)
		etCleanup(late)
		triggerReady(etKey(t), metav1.ConditionTrue, "Ready")
	})

	It("fires each due schedule slot once with a deterministic session", func() {
		// A not-Ready agent: sessions are created for it but never started.
		broken := etBrokenAgent(ns)

		const prompt = "run {{ .Trigger }} input={{ .Input }} in {{ .Session }}"
		hourly := func(mutate func(*ompv1alpha1.OmpTriggerSpec)) *ompv1alpha1.OmpTrigger {
			return trigger(broken.Name, func(s *ompv1alpha1.OmpTriggerSpec) {
				cronSpec("0 * * * *")(s)
				s.Schedule.Input = "hello"
				s.Prompt = prompt
				mutate(s)
			})
		}
		plain := hourly(func(*ompv1alpha1.OmpTriggerSpec) {})
		keyed := hourly(func(s *ompv1alpha1.OmpTriggerSpec) {
			s.Session.Resume = ompv1alpha1.ResumeKey
			s.Priority = new(int32(1))
		})
		disabled := hourly(func(s *ompv1alpha1.OmpTriggerSpec) { s.Enabled = new(false) })
		for _, t := range []*ompv1alpha1.OmpTrigger{plain, keyed, disabled} {
			etCreate(t)
			etCleanup(t)
		}
		got := triggerReady(etKey(plain), metav1.ConditionTrue, "Ready")
		Expect(got.Status.NextFireAt).NotTo(BeNil())
		Expect(got.Status.LastFireAt).To(BeNil())
		off := triggerReady(etKey(disabled), metav1.ConditionTrue, "Ready")
		Expect(off.Status.NextFireAt).To(BeNil())

		DeferCleanup(etClock.Reset)
		etClock.Advance(150 * time.Minute)
		for _, t := range []*ompv1alpha1.OmpTrigger{plain, keyed, disabled} {
			etPoke(t)
		}

		sessionsOf := func(g Gomega, trigger string) []ompv1alpha1.OmpSession {
			var l ompv1alpha1.OmpSessionList
			g.Expect(k8sClient.List(ctx, &l, client.InNamespace(ns),
				client.MatchingLabels{ompv1alpha1.LabelTrigger: trigger})).To(Succeed())
			return l.Items
		}
		var slot time.Time
		Eventually(func(g Gomega) {
			var cur ompv1alpha1.OmpTrigger
			g.Expect(k8sClient.Get(ctx, etKey(plain), &cur)).To(Succeed())
			g.Expect(cur.Status.LastFireAt).NotTo(BeNil())
			slot = cur.Status.LastFireAt.UTC()
			g.Expect(sessionsOf(g, plain.Name)).To(HaveLen(1))
		}).Should(Succeed())

		// The slot is the latest full hour before the (fake) now.
		Expect(slot.Minute()).To(BeZero())
		Expect(slot.Second()).To(BeZero())
		Expect(slot.After(etClock.Now())).To(BeFalse())
		Expect(etClock.Now().Sub(slot)).To(BeNumerically("<", time.Hour+time.Minute))

		var agentObj ompv1alpha1.OmpAgent
		Expect(k8sClient.Get(ctx, etKey(broken), &agentObj)).To(Succeed())
		var trigObj ompv1alpha1.OmpTrigger
		Expect(k8sClient.Get(ctx, etKey(plain), &trigObj)).To(Succeed())

		wantName := plain.Name + "-" + slot.Format("20060102-1504")
		Eventually(func(g Gomega) {
			s := sessionsOf(g, plain.Name)
			g.Expect(s).To(HaveLen(1))
			g.Expect(s[0].Name).To(Equal(wantName))
			g.Expect(s[0].Spec.Prompt).To(Equal("run " + plain.Name + " input=hello in " + wantName))
			g.Expect(s[0].Spec.AgentRef.Name).To(Equal(broken.Name))
			g.Expect(s[0].Spec.TriggerRef).NotTo(BeNil())
			g.Expect(s[0].Spec.TriggerRef.Name).To(Equal(plain.Name))
			g.Expect(*s[0].Spec.Priority).To(Equal(DefaultSchedulePriority))
			g.Expect(s[0].Spec.ContinuityKey).To(BeEmpty())
			g.Expect(s[0].Labels).To(HaveKeyWithValue(ompv1alpha1.LabelKind, string(ompv1alpha1.SessionKindSchedule)))
			owners := make([]types.UID, 0, len(s[0].OwnerReferences))
			for _, o := range s[0].OwnerReferences {
				owners = append(owners, o.UID)
			}
			g.Expect(owners).To(ConsistOf(agentObj.UID, trigObj.UID))
		}).Should(Succeed())

		// resume: key shares history under one key, at the trigger's own priority.
		Eventually(func(g Gomega) {
			s := sessionsOf(g, keyed.Name)
			g.Expect(s).To(HaveLen(1))
			g.Expect(s[0].Spec.ContinuityKey).To(Equal("schedule/" + keyed.Name))
			g.Expect(s[0].Labels).To(HaveKeyWithValue(ompv1alpha1.LabelContinuity, store.ContinuityID("schedule/"+keyed.Name)))
			g.Expect(*s[0].Spec.Priority).To(Equal(int32(1)))
		}).Should(Succeed())

		// A disabled trigger never fires.
		Expect(sessionsOf(Default, disabled.Name)).To(BeEmpty())

		// Reconciling again, even with the recorded slot lost, creates nothing new: an existing
		// session at the slot counts as fired.
		for range 3 {
			etPoke(plain)
		}
		stub := &ompv1alpha1.OmpTrigger{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: plain.Name}}
		lost, err := json.Marshal(map[string]any{"status": map[string]any{"lastFireAt": nil}})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Status().Patch(ctx, stub, client.RawPatch(types.MergePatchType, lost))).To(Succeed())
		etPoke(plain)
		Eventually(func(g Gomega) {
			var cur ompv1alpha1.OmpTrigger
			g.Expect(k8sClient.Get(ctx, etKey(plain), &cur)).To(Succeed())
			g.Expect(cur.Status.LastFireAt).NotTo(BeNil())
			g.Expect(cur.Status.LastFireAt.UTC().Equal(slot)).To(BeTrue())
		}).Should(Succeed())
		Consistently(func(g Gomega) []ompv1alpha1.OmpSession { return sessionsOf(g, plain.Name) }).Should(HaveLen(1))
	})
})
