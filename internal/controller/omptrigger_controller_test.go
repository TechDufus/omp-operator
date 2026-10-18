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
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/celx"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/metrics"
	"github.com/TechDufus/omp-operator/internal/store"
)

const (
	trigNS    = "apps"
	trigAgent = "triage"

	trigHookSecret = "hook"
)

var trigEpoch = time.Date(2026, 10, 18, 12, 0, 30, 0, time.UTC)

type trigRig struct {
	t   *testing.T
	now time.Time
	c   client.Client
	r   *OmpTriggerReconciler
}

func newTrigRig(t *testing.T, objs ...client.Object) *trigRig {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := ompv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	base := make([]client.Object, 0, 2+len(objs))
	base = append(base,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: trigNS, Name: trigHookSecret},
			Data:       map[string][]byte{"token": []byte("t"), "username": []byte("u"), "password": []byte("p")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: trigNS, Name: "empty"},
			Data:       map[string][]byte{"other": []byte("x")},
		},
	)
	base = append(base, objs...)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&ompv1alpha1.OmpTrigger{}).
		WithObjects(base...).
		Build()
	rig := &trigRig{t: t, now: trigEpoch, c: c}
	rig.r = &OmpTriggerReconciler{
		Client: c, Scheme: scheme, APIReader: c,
		Options: &config.Options{}, CEL: celx.NewCache(),
		Now: func() time.Time { return rig.now },
	}
	return rig
}

func trigAgentObj() *ompv1alpha1.OmpAgent {
	return &ompv1alpha1.OmpAgent{ObjectMeta: metav1.ObjectMeta{Namespace: trigNS, Name: trigAgent, UID: "agent-uid"}}
}

func webhookTrigger() *ompv1alpha1.OmpTrigger {
	return &ompv1alpha1.OmpTrigger{
		ObjectMeta: metav1.ObjectMeta{Namespace: trigNS, Name: "t", UID: "trigger-uid", Generation: 3},
		Spec: ompv1alpha1.OmpTriggerSpec{
			AgentRef: ompv1alpha1.LocalObjectRef{Name: trigAgent},
			Prompt:   "Triage {{.Event.Name}} {{.Alerts}}",
			Webhook: &ompv1alpha1.WebhookSpec{
				Adapter: ompv1alpha1.AdapterAlertmanager,
				Auth:    ompv1alpha1.WebhookAuth{Bearer: &ompv1alpha1.BearerAuth{SecretRef: ompv1alpha1.SecretKeyRef{Name: trigHookSecret, Key: "token"}}},
			},
		},
	}
}

func scheduleTrigger(name, cron string) *ompv1alpha1.OmpTrigger {
	return &ompv1alpha1.OmpTrigger{
		ObjectMeta: metav1.ObjectMeta{Namespace: trigNS, Name: name, UID: "sched-uid", Generation: 1, CreationTimestamp: metav1.NewTime(trigEpoch)},
		Spec: ompv1alpha1.OmpTriggerSpec{
			AgentRef: ompv1alpha1.LocalObjectRef{Name: trigAgent},
			Prompt:   "Run {{.Trigger}} at {{.Now}} with {{.Input}} as {{.Session}}",
			Schedule: &ompv1alpha1.ScheduleSpec{Cron: cron, Input: "daily report"},
		},
	}
}

func (g *trigRig) reconcile(name string) ctrl.Result {
	g.t.Helper()
	res, err := g.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: trigNS, Name: name}})
	if err != nil {
		g.t.Fatalf("reconcile %s: %v", name, err)
	}
	return res
}

func (g *trigRig) get(name string) *ompv1alpha1.OmpTrigger {
	g.t.Helper()
	t := &ompv1alpha1.OmpTrigger{}
	if err := g.c.Get(context.Background(), client.ObjectKey{Namespace: trigNS, Name: name}, t); err != nil {
		g.t.Fatal(err)
	}
	return t
}

func (g *trigRig) sessions() []ompv1alpha1.OmpSession {
	g.t.Helper()
	list := &ompv1alpha1.OmpSessionList{}
	if err := g.c.List(context.Background(), list, client.InNamespace(trigNS)); err != nil {
		g.t.Fatal(err)
	}
	return list.Items
}

func readyCondition(t *ompv1alpha1.OmpTrigger) *metav1.Condition {
	for i := range t.Status.Conditions {
		if t.Status.Conditions[i].Type == ompv1alpha1.ConditionReady {
			return &t.Status.Conditions[i]
		}
	}
	return nil
}

func TestTriggerReadyReasons(t *testing.T) {
	mutate := func(f func(*ompv1alpha1.OmpTrigger)) *ompv1alpha1.OmpTrigger {
		tr := webhookTrigger()
		f(tr)
		return tr
	}
	cases := []struct {
		name   string
		agent  bool
		trig   *ompv1alpha1.OmpTrigger
		reason string
		typ    ompv1alpha1.TriggerType
	}{
		{"ready webhook", true, webhookTrigger(), triggerReasonReady, ompv1alpha1.TriggerWebhook},
		{"ready schedule", true, scheduleTrigger("t", "0 7 * * *"), triggerReasonReady, ompv1alpha1.TriggerSchedule},
		{"agent missing", false, webhookTrigger(), triggerReasonAgentNotFound, ompv1alpha1.TriggerWebhook},
		{"neither union member", true, mutate(func(tr *ompv1alpha1.OmpTrigger) { tr.Spec.Webhook = nil }), triggerReasonInvalidSpec, ""},
		{"both union members", true, mutate(func(tr *ompv1alpha1.OmpTrigger) {
			tr.Spec.Schedule = &ompv1alpha1.ScheduleSpec{Cron: "* * * * *"}
		}), triggerReasonInvalidSpec, ""},
		{"no auth", true, mutate(func(tr *ompv1alpha1.OmpTrigger) { tr.Spec.Webhook.Auth = ompv1alpha1.WebhookAuth{} }), triggerReasonInvalidSpec, ompv1alpha1.TriggerWebhook},
		{"two auth methods", true, mutate(func(tr *ompv1alpha1.OmpTrigger) {
			tr.Spec.Webhook.Auth.Basic = &ompv1alpha1.BasicAuth{SecretRef: ompv1alpha1.LocalObjectRef{Name: trigHookSecret}}
		}), triggerReasonInvalidSpec, ompv1alpha1.TriggerWebhook},
		{"generic without mapping", true, mutate(func(tr *ompv1alpha1.OmpTrigger) { tr.Spec.Webhook.Adapter = ompv1alpha1.AdapterGeneric }), triggerReasonInvalidSpec, ompv1alpha1.TriggerWebhook},
		{"bad filter", true, mutate(func(tr *ompv1alpha1.OmpTrigger) { tr.Spec.Webhook.Filter = "event.severity ==" }), triggerReasonInvalidExpression, ompv1alpha1.TriggerWebhook},
		{"filter of the wrong type", true, mutate(func(tr *ompv1alpha1.OmpTrigger) { tr.Spec.Webhook.Filter = "'critical'" }), triggerReasonInvalidExpression, ompv1alpha1.TriggerWebhook},
		{"bad alert filter", true, mutate(func(tr *ompv1alpha1.OmpTrigger) { tr.Spec.Webhook.AlertFilter = "nope.labels" }), triggerReasonInvalidExpression, ompv1alpha1.TriggerWebhook},
		{"bad mapping", true, mutate(func(tr *ompv1alpha1.OmpTrigger) {
			tr.Spec.Webhook.Adapter = ompv1alpha1.AdapterGeneric
			tr.Spec.Webhook.Mapping = &ompv1alpha1.WebhookMapping{Key: "payload.id +"}
		}), triggerReasonInvalidExpression, ompv1alpha1.TriggerWebhook},
		{"good generic", true, mutate(func(tr *ompv1alpha1.OmpTrigger) {
			tr.Spec.Webhook.Adapter = ompv1alpha1.AdapterGeneric
			tr.Spec.Webhook.Mapping = &ompv1alpha1.WebhookMapping{Key: "payload.id"}
		}), triggerReasonReady, ompv1alpha1.TriggerWebhook},
		{"prompt syntax error", true, mutate(func(tr *ompv1alpha1.OmpTrigger) { tr.Spec.Prompt = "{{.Event.Name" }), triggerReasonInvalidPrompt, ompv1alpha1.TriggerWebhook},
		{"prompt unknown field", true, mutate(func(tr *ompv1alpha1.OmpTrigger) { tr.Spec.Prompt = "{{.Event.Nope}}" }), triggerReasonInvalidPrompt, ompv1alpha1.TriggerWebhook},
		{"prompt field of the other type", true, mutate(func(tr *ompv1alpha1.OmpTrigger) { tr.Spec.Prompt = "{{.Input}}" }), triggerReasonInvalidPrompt, ompv1alpha1.TriggerWebhook},
		{"prompt label depends on the event", true, mutate(func(tr *ompv1alpha1.OmpTrigger) { tr.Spec.Prompt = "{{.Event.Labels.pod}}" }), triggerReasonReady, ompv1alpha1.TriggerWebhook},
		{"auth secret missing", true, mutate(func(tr *ompv1alpha1.OmpTrigger) { tr.Spec.Webhook.Auth.Bearer.SecretRef.Name = "absent" }), triggerReasonMissingSecret, ompv1alpha1.TriggerWebhook},
		{"auth secret key missing", true, mutate(func(tr *ompv1alpha1.OmpTrigger) { tr.Spec.Webhook.Auth.Bearer.SecretRef.Key = "nokey" }), triggerReasonMissingSecret, ompv1alpha1.TriggerWebhook},
		{"basic secret without username", true, mutate(func(tr *ompv1alpha1.OmpTrigger) {
			tr.Spec.Webhook.Auth = ompv1alpha1.WebhookAuth{Basic: &ompv1alpha1.BasicAuth{SecretRef: ompv1alpha1.LocalObjectRef{Name: "empty"}}}
		}), triggerReasonMissingSecret, ompv1alpha1.TriggerWebhook},
		{"bad cron", true, scheduleTrigger("t", "@every 1m"), triggerReasonInvalidSchedule, ompv1alpha1.TriggerSchedule},
		{"bad time zone", true, func() *ompv1alpha1.OmpTrigger {
			tr := scheduleTrigger("t", "0 7 * * *")
			tr.Spec.Schedule.TimeZone = "Mars/Olympus"
			return tr
		}(), triggerReasonInvalidSchedule, ompv1alpha1.TriggerSchedule},
		{"episode resume on a schedule", true, func() *ompv1alpha1.OmpTrigger {
			tr := scheduleTrigger("t", "0 7 * * *")
			tr.Spec.Session.Resume = ompv1alpha1.ResumeEpisode
			return tr
		}(), triggerReasonInvalidSpec, ompv1alpha1.TriggerSchedule},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{tc.trig}
			if tc.agent {
				objs = append(objs, trigAgentObj())
			}
			g := newTrigRig(t, objs...)
			g.reconcile("t")
			got := g.get("t")
			cond := readyCondition(got)
			if cond == nil {
				t.Fatal("no Ready condition")
			}
			if cond.Reason != tc.reason {
				t.Fatalf("reason %q (%s), want %q", cond.Reason, cond.Message, tc.reason)
			}
			if wantReady := tc.reason == triggerReasonReady; (cond.Status == metav1.ConditionTrue) != wantReady {
				t.Fatalf("status %s for reason %q", cond.Status, cond.Reason)
			}
			if cond.ObservedGeneration != got.Generation {
				t.Errorf("observedGeneration %d, want %d", cond.ObservedGeneration, got.Generation)
			}
			if got.Status.Type != tc.typ {
				t.Errorf("status.type %q, want %q", got.Status.Type, tc.typ)
			}
			wantPath := ""
			if tc.typ == ompv1alpha1.TriggerWebhook {
				wantPath = "/hooks/apps/t"
			}
			if got.Status.HookPath != wantPath {
				t.Errorf("status.hookPath %q, want %q", got.Status.HookPath, wantPath)
			}
		})
	}
}

func TestTriggerSkipsDisallowedNamespace(t *testing.T) {
	g := newTrigRig(t, webhookTrigger(), trigAgentObj())
	g.r.Options = &config.Options{AgentNamespaces: []string{"other"}}
	g.reconcile("t")
	if got := g.get("t"); got.Status.Type != "" || len(got.Status.Conditions) != 0 {
		t.Fatalf("status written for a disallowed namespace: %+v", got.Status)
	}
}

func TestTriggerReadyIsRepublishedWhenTheAgentAppears(t *testing.T) {
	g := newTrigRig(t, webhookTrigger())
	g.reconcile("t")
	if c := readyCondition(g.get("t")); c == nil || c.Reason != triggerReasonAgentNotFound {
		t.Fatalf("condition %+v", c)
	}
	if err := g.c.Create(context.Background(), trigAgentObj()); err != nil {
		t.Fatal(err)
	}
	reqs := g.r.triggersOfAgent(context.Background(), trigAgentObj())
	if len(reqs) != 1 || reqs[0].Name != "t" {
		t.Fatalf("agent event maps to %v", reqs)
	}
	g.reconcile("t")
	if c := readyCondition(g.get("t")); c.Reason != triggerReasonReady {
		t.Fatalf("condition %+v", c)
	}
}

func TestScheduleFiresEachSlotOnce(t *testing.T) {
	tr := scheduleTrigger("daily", "*/5 * * * *")
	tr.Spec.Priority = nil
	tr.Spec.Session.Resume = ompv1alpha1.ResumeKey
	tr.Spec.Schedule.TimeZone = "America/Chicago"
	g := newTrigRig(t, tr, trigAgentObj())

	// Created at 12:00:30; at 12:03 the first slot (12:05) is still ahead.
	g.now = time.Date(2026, 10, 18, 12, 3, 0, 0, time.UTC)
	res := g.reconcile("daily")
	if n := len(g.sessions()); n != 0 {
		t.Fatalf("%d sessions before the first slot", n)
	}
	got := g.get("daily")
	if got.Status.NextFireAt == nil || !got.Status.NextFireAt.Time.Equal(time.Date(2026, 10, 18, 12, 5, 0, 0, time.UTC)) {
		t.Fatalf("nextFireAt %v, want 12:05", got.Status.NextFireAt)
	}
	if res.RequeueAfter != 2*time.Minute {
		t.Fatalf("requeue %v, want 2m", res.RequeueAfter)
	}
	if got.Status.LastFireAt != nil {
		t.Fatalf("lastFireAt %v before any fire", got.Status.LastFireAt)
	}

	// At 12:05:02 the slot fires.
	g.now = time.Date(2026, 10, 18, 12, 5, 2, 0, time.UTC)
	g.reconcile("daily")
	sessions := g.sessions()
	if len(sessions) != 1 {
		t.Fatalf("%d sessions at the first slot, want 1", len(sessions))
	}
	s := sessions[0]
	if s.Name != "daily-20261018-1205" {
		t.Errorf("session name %q", s.Name)
	}
	for k, v := range map[string]string{
		ompv1alpha1.LabelAgent: trigAgent, ompv1alpha1.LabelTrigger: "daily", ompv1alpha1.LabelKind: "schedule",
		ompv1alpha1.LabelContinuity: store.ContinuityID("schedule/daily"),
	} {
		if s.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, s.Labels[k], v)
		}
	}
	if s.Spec.ContinuityKey != "schedule/daily" {
		t.Errorf("continuityKey %q", s.Spec.ContinuityKey)
	}
	if s.Spec.Priority == nil || *s.Spec.Priority != 3 {
		t.Errorf("priority %v, want 3", s.Spec.Priority)
	}
	if s.Spec.Event != nil {
		t.Errorf("schedule session has an event: %+v", s.Spec.Event)
	}
	if want := "Run daily at 2026-10-18T07:05:02-05:00 with daily report as daily-20261018-1205"; s.Spec.Prompt != want {
		t.Errorf("prompt %q, want %q", s.Spec.Prompt, want)
	}
	if len(s.OwnerReferences) != 2 || s.OwnerReferences[0].Kind != "OmpAgent" || s.OwnerReferences[1].Kind != "OmpTrigger" {
		t.Errorf("owner references %+v", s.OwnerReferences)
	}
	got = g.get("daily")
	if got.Status.LastFireAt == nil || !got.Status.LastFireAt.Time.Equal(time.Date(2026, 10, 18, 12, 5, 0, 0, time.UTC)) {
		t.Errorf("lastFireAt %v, want the 12:05 slot", got.Status.LastFireAt)
	}
	if got.Status.NextFireAt == nil || !got.Status.NextFireAt.Time.Equal(time.Date(2026, 10, 18, 12, 10, 0, 0, time.UTC)) {
		t.Errorf("nextFireAt %v, want 12:10", got.Status.NextFireAt)
	}

	// Reconciling again inside the same slot creates nothing.
	g.now = g.now.Add(time.Minute)
	g.reconcile("daily")
	if n := len(g.sessions()); n != 1 {
		t.Fatalf("%d sessions after a repeat reconcile, want 1", n)
	}

	// Missed slots collapse into the latest one.
	g.now = time.Date(2026, 10, 18, 12, 33, 0, 0, time.UTC)
	g.reconcile("daily")
	names := map[string]bool{}
	for _, s := range g.sessions() {
		names[s.Name] = true
	}
	if len(names) != 2 || !names["daily-20261018-1230"] {
		t.Fatalf("sessions %v, want the 12:05 and 12:30 slots only", names)
	}
}

func TestScheduleSlotThatAlreadyHasASessionCountsAsDone(t *testing.T) {
	tr := scheduleTrigger("daily", "*/5 * * * *")
	existing := &ompv1alpha1.OmpSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: trigNS, Name: "daily-20261018-1205"},
		Spec:       ompv1alpha1.OmpSessionSpec{AgentRef: ompv1alpha1.LocalObjectRef{Name: trigAgent}, Prompt: "earlier"},
	}
	g := newTrigRig(t, tr, trigAgentObj(), existing)
	g.now = time.Date(2026, 10, 18, 12, 5, 2, 0, time.UTC)
	g.reconcile("daily")
	sessions := g.sessions()
	if len(sessions) != 1 || sessions[0].Spec.Prompt != "earlier" {
		t.Fatalf("sessions %+v: the existing slot session must be left alone", sessions)
	}
	if got := g.get("daily"); got.Status.LastFireAt == nil || got.Status.LastFireAt.Minute() != 5 {
		t.Fatalf("lastFireAt %v, want the 12:05 slot recorded", got.Status.LastFireAt)
	}
}

func TestScheduleDoesNotFireWhenDisabledOrNotReady(t *testing.T) {
	off := false
	disabled := scheduleTrigger("off", "*/5 * * * *")
	disabled.Spec.Enabled = &off
	noAgent := scheduleTrigger("noagent", "*/5 * * * *")
	noAgent.Spec.AgentRef.Name = "ghost"
	g := newTrigRig(t, disabled, noAgent, trigAgentObj())
	g.now = time.Date(2026, 10, 18, 13, 0, 0, 0, time.UTC)
	for _, name := range []string{"off", "noagent"} {
		res := g.reconcile(name)
		if got := g.get(name); got.Status.NextFireAt != nil || got.Status.LastFireAt != nil {
			t.Errorf("%s: fire times set: %+v", name, got.Status)
		}
		if res.RequeueAfter != TriggerRecheckInterval {
			t.Errorf("%s: requeue %v, want %v", name, res.RequeueAfter, TriggerRecheckInterval)
		}
	}
	if n := len(g.sessions()); n != 0 {
		t.Fatalf("%d sessions created", n)
	}
	if c := readyCondition(g.get("off")); c.Status != metav1.ConditionTrue {
		t.Errorf("a disabled trigger is still Ready: %+v", c)
	}
}

func TestTriggerGroupsGauge(t *testing.T) {
	tr := webhookTrigger()
	res := metav1.NewTime(trigEpoch)
	tr.Status.Groups = []ompv1alpha1.TriggerGroup{{ID: "a"}, {ID: "b", ResolvedAt: &res}, {ID: "c"}}
	g := newTrigRig(t, tr, trigAgentObj())
	g.reconcile("t")
	if got := g.get("t").Status.Groups; len(got) != 3 {
		t.Fatalf("reconcile changed groups: %+v", got)
	}
	if got := testutil.ToFloat64(metrics.TriggerGroupsActive.WithLabelValues(trigNS, "t")); got != 2 {
		t.Fatalf("groups_active %v, want 2", got)
	}
}
