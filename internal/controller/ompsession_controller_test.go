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

//nolint:goconst // table-driven fixtures repeat literals on purpose
package controller

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/metrics"
	"github.com/TechDufus/omp-operator/internal/store"
)

const sessNS = "ns"

var sessT0 = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

type sessClock struct{ t time.Time }

func (c *sessClock) now() time.Time          { return c.t }
func (c *sessClock) advance(d time.Duration) { c.t = c.t.Add(d) }

type sessNotifier struct {
	done    []string // "<session>:<verdict or phase>"
	notices []string // keys
	err     error
}

func (n *sessNotifier) SessionDone(_ context.Context, _ *ompv1alpha1.OmpAgent, s *ompv1alpha1.OmpSession) error {
	n.done = append(n.done, s.Name+":"+string(s.Status.Phase)+":"+string(s.Status.Verdict))
	return n.err
}

func (n *sessNotifier) Notice(_ context.Context, _ *ompv1alpha1.OmpAgent, key string, _ time.Duration, title, _ string) (bool, error) {
	n.notices = append(n.notices, key+"|"+title)
	return true, nil
}

type sessGateway struct{ t time.Time }

func (g *sessGateway) LastClientsWrite() time.Time { return g.t }

type sessHarness struct {
	t     *testing.T
	c     client.Client
	r     *OmpSessionReconciler
	clock *sessClock
	n     *sessNotifier
	store *store.Store
}

func sessScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := ompv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newSessHarness(t *testing.T, objs ...client.Object) *sessHarness {
	t.Helper()
	s := sessScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&ompv1alpha1.OmpSession{}, &ompv1alpha1.OmpAgent{}, &corev1.Pod{}).Build()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clock := &sessClock{t: sessT0}
	n := &sessNotifier{}
	return &sessHarness{t: t, c: c, clock: clock, n: n, store: st, r: &OmpSessionReconciler{
		Client:    c,
		Scheme:    s,
		APIReader: c,
		Options: &config.Options{
			OperatorNamespace: "omp-system", ServiceName: "omp-operator", AgentAPIPort: 8083,
			DefaultRuntimeImage: "example.com/runtime:test",
		},
		Store:    st,
		Notifier: n,
		Now:      clock.now,
	}}
}

func sessAgent(name string) *ompv1alpha1.OmpAgent {
	return &ompv1alpha1.OmpAgent{
		ObjectMeta: metav1.ObjectMeta{Namespace: sessNS, Name: name, UID: types.UID("agent-" + name), Generation: 1},
		Spec: ompv1alpha1.OmpAgentSpec{
			Model:  ompv1alpha1.ModelSpec{Default: "anthropic/claude"},
			Limits: ompv1alpha1.LimitsSpec{MaxAttempts: 3, RetryDelay: metav1.Duration{Duration: 15 * time.Minute}},
		},
		Status: ompv1alpha1.OmpAgentStatus{Conditions: []metav1.Condition{{
			Type: ompv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: metav1.NewTime(sessT0),
		}}},
	}
}

func sessSession(name, agent string, age time.Duration) *ompv1alpha1.OmpSession {
	return &ompv1alpha1.OmpSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: sessNS, Name: name, UID: types.UID("uid-" + name),
			CreationTimestamp: metav1.NewTime(sessT0.Add(-age)),
		},
		Spec: ompv1alpha1.OmpSessionSpec{AgentRef: ompv1alpha1.LocalObjectRef{Name: agent}, Prompt: "go"},
	}
}

// reconcile runs one reconcile, then stamps new pods with the fake clock's time as the API
// server would (the fake client leaves creationTimestamp zero) and a Pending phase.
func (h *sessHarness) reconcile(name string) ctrl.Result {
	h.t.Helper()
	res, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: sessNS, Name: name}})
	if err != nil {
		h.t.Fatalf("reconcile %s: %v", name, err)
	}
	for _, p := range h.pods() {
		if !p.CreationTimestamp.IsZero() {
			continue
		}
		p.CreationTimestamp = metav1.NewTime(h.clock.now())
		if err := h.c.Update(context.Background(), &p); err != nil {
			h.t.Fatal(err)
		}
		p.Status.Phase = corev1.PodPending
		if err := h.c.Status().Update(context.Background(), &p); err != nil {
			h.t.Fatal(err)
		}
	}
	return res
}

func (h *sessHarness) session(name string) *ompv1alpha1.OmpSession {
	h.t.Helper()
	s := &ompv1alpha1.OmpSession{}
	if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: sessNS, Name: name}, s); err != nil {
		h.t.Fatalf("get session %s: %v", name, err)
	}
	return s
}

func (h *sessHarness) pods() []corev1.Pod {
	h.t.Helper()
	var l corev1.PodList
	if err := h.c.List(context.Background(), &l, client.InNamespace(sessNS)); err != nil {
		h.t.Fatal(err)
	}
	return l.Items
}

func (h *sessHarness) setPod(name string, mutate func(*corev1.Pod)) {
	h.t.Helper()
	p := &corev1.Pod{}
	if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: sessNS, Name: name}, p); err != nil {
		h.t.Fatalf("get pod %s: %v", name, err)
	}
	mutate(p)
	if err := h.c.Status().Update(context.Background(), p); err != nil {
		h.t.Fatalf("update pod status: %v", err)
	}
}

func (h *sessHarness) setSessionStatus(name string, mutate func(*ompv1alpha1.OmpSessionStatus)) {
	h.t.Helper()
	s := h.session(name)
	mutate(&s.Status)
	if err := h.c.Status().Update(context.Background(), s); err != nil {
		h.t.Fatalf("update session status: %v", err)
	}
}

func (h *sessHarness) setAgentStatus(name string, mutate func(*ompv1alpha1.OmpAgentStatus)) {
	h.t.Helper()
	a := &ompv1alpha1.OmpAgent{}
	if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: sessNS, Name: name}, a); err != nil {
		h.t.Fatal(err)
	}
	mutate(&a.Status)
	if err := h.c.Status().Update(context.Background(), a); err != nil {
		h.t.Fatal(err)
	}
}

func TestSessionStartCreatesTokenSecretAndPod(t *testing.T) {
	agent := sessAgent("triage")
	h := newSessHarness(t, agent, sessSession("s1", "triage", time.Minute))

	res := h.reconcile("s1")
	if res.RequeueAfter != 10*time.Minute {
		t.Errorf("requeue = %v, want the pod start limit", res.RequeueAfter)
	}
	s := h.session("s1")
	if s.Status.Phase != ompv1alpha1.SessionPhaseRunning || s.Status.Attempts != 1 || s.Status.PodName != "s1-1" ||
		s.Status.StartTime == nil || !s.Status.StartTime.Time.Equal(sessT0) || s.Status.RetryAt != nil {
		t.Fatalf("status = %+v", s.Status)
	}
	if !controllerFinalizer(s) {
		t.Errorf("finalizer missing: %v", s.Finalizers)
	}
	if s.Labels["omp.techdufus.com/agent"] != "triage" || s.Labels["omp.techdufus.com/kind"] != "manual" {
		t.Errorf("labels = %v", s.Labels)
	}
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].UID != agent.UID {
		t.Errorf("owner refs = %+v", s.OwnerReferences)
	}

	sec := &corev1.Secret{}
	if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: sessNS, Name: "s1-token"}, sec); err != nil {
		t.Fatalf("token secret: %v", err)
	}
	if len(sec.Data["token"]) != 64 || !metav1IsOwnedBy(sec.OwnerReferences, "uid-s1") {
		t.Errorf("token secret = data %d bytes, owners %+v", len(sec.Data["token"]), sec.OwnerReferences)
	}

	pods := h.pods()
	if len(pods) != 1 || pods[0].Name != "s1-1" || !metav1.IsControlledBy(&pods[0], s) {
		t.Fatalf("pods = %+v", pods)
	}
	if pods[0].Spec.Containers[0].Image != "example.com/runtime:test" {
		t.Errorf("image = %s", pods[0].Spec.Containers[0].Image)
	}
}

func controllerFinalizer(s *ompv1alpha1.OmpSession) bool {
	return slices.Contains(s.Finalizers, ompv1alpha1.FinalizerCleanup)
}

func metav1IsOwnedBy(refs []metav1.OwnerReference, uid string) bool {
	for _, r := range refs {
		if string(r.UID) == uid {
			return true
		}
	}
	return false
}

func TestSessionTokenSecretReusedAcrossAttempts(t *testing.T) {
	h := newSessHarness(t, sessAgent("triage"), sessSession("s1", "triage", time.Minute))
	h.reconcile("s1")
	first := &corev1.Secret{}
	_ = h.c.Get(context.Background(), client.ObjectKey{Namespace: sessNS, Name: "s1-token"}, first)

	// attempt 1 ends retryable, then attempt 2 starts with the same token
	h.setPod("s1-1", func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed })
	h.reconcile("s1")
	h.clock.advance(16 * time.Minute)
	h.reconcile("s1")

	s := h.session("s1")
	if s.Status.Phase != ompv1alpha1.SessionPhaseRunning || s.Status.Attempts != 2 || s.Status.PodName != "s1-2" {
		t.Fatalf("status = %+v", s.Status)
	}
	second := &corev1.Secret{}
	_ = h.c.Get(context.Background(), client.ObjectKey{Namespace: sessNS, Name: "s1-token"}, second)
	if string(first.Data["token"]) != string(second.Data["token"]) {
		t.Error("token was regenerated for the second attempt")
	}
}

func TestSessionOnePerAgentAndQueueOrder(t *testing.T) {
	prio := func(p int32) *int32 { return &p }
	tests := []struct {
		name     string
		sessions []*ompv1alpha1.OmpSession
		first    string
	}{
		{
			name: "oldest first at equal priority",
			sessions: []*ompv1alpha1.OmpSession{
				sessSession("new", "triage", time.Minute),
				sessSession("old", "triage", time.Hour),
			},
			first: "old",
		},
		{
			name: "lower priority number first",
			sessions: func() []*ompv1alpha1.OmpSession {
				a, b := sessSession("old", "triage", time.Hour), sessSession("urgent", "triage", time.Minute)
				a.Spec.Priority, b.Spec.Priority = prio(2), prio(0)
				return []*ompv1alpha1.OmpSession{a, b}
			}(),
			first: "urgent",
		},
		{
			name: "default priority is 1 (manual)",
			sessions: func() []*ompv1alpha1.OmpSession {
				a, b := sessSession("sched", "triage", time.Hour), sessSession("manual", "triage", time.Minute)
				a.Spec.Priority = prio(3)
				return []*ompv1alpha1.OmpSession{a, b}
			}(),
			first: "manual",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []client.Object{sessAgent("triage")}
			for _, s := range tt.sessions {
				objs = append(objs, s)
			}
			h := newSessHarness(t, objs...)
			// The loser is reconciled first and must wait; then the winner starts; then the loser waits again.
			for _, s := range tt.sessions {
				if s.Name != tt.first {
					if res := h.reconcile(s.Name); res.RequeueAfter != 10*time.Second {
						t.Errorf("%s requeue = %v, want 10s", s.Name, res.RequeueAfter)
					}
				}
			}
			if n := len(h.pods()); n != 0 {
				t.Fatalf("%d pods before the head of the queue ran", n)
			}
			h.reconcile(tt.first)
			for _, s := range tt.sessions {
				h.reconcile(s.Name)
			}
			pods := h.pods()
			if len(pods) != 1 || pods[0].Name != tt.first+"-1" {
				t.Fatalf("pods = %v, want only %s-1", podNames(pods), tt.first)
			}
		})
	}
}

func podNames(ps []corev1.Pod) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

func TestSessionOtherAgentsAreIndependent(t *testing.T) {
	h := newSessHarness(t, sessAgent("a"), sessAgent("b"), sessSession("sa", "a", time.Hour), sessSession("sb", "b", time.Minute))
	h.reconcile("sa")
	h.reconcile("sb")
	if n := len(h.pods()); n != 2 {
		t.Errorf("pods = %d, want one per agent", n)
	}
}

func TestSessionRetryingSessionDoesNotHoldQueue(t *testing.T) {
	h := newSessHarness(t, sessAgent("triage"), sessSession("old", "triage", time.Hour), sessSession("new", "triage", time.Minute))
	h.reconcile("old") // starts
	h.setPod("old-1", func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed })
	h.reconcile("old") // -> Pending with retryAt in the future
	if s := h.session("old"); s.Status.Phase != ompv1alpha1.SessionPhasePending || s.Status.RetryAt == nil {
		t.Fatalf("old status = %+v", s.Status)
	}
	h.reconcile("new")
	if s := h.session("new"); s.Status.Phase != ompv1alpha1.SessionPhaseRunning {
		t.Errorf("new phase = %s; a session waiting for retryAt must not block the queue", s.Status.Phase)
	}
}

func TestSessionAgentGates(t *testing.T) {
	t.Run("agent not found fails", func(t *testing.T) {
		h := newSessHarness(t, sessSession("s1", "ghost", time.Minute))
		h.reconcile("s1")
		s := h.session("s1")
		if s.Status.Phase != ompv1alpha1.SessionPhaseFailed || s.Status.Error != "agent not found" || s.Status.CompletionTime == nil {
			t.Errorf("status = %+v", s.Status)
		}
		if c := meta.FindStatusCondition(s.Status.Conditions, ompv1alpha1.ConditionNotified); c == nil {
			t.Error("Notified condition not set")
		}
	})

	t.Run("agent not ready waits then fails after the limit", func(t *testing.T) {
		agent := sessAgent("triage")
		agent.Status.Conditions = []metav1.Condition{{Type: ompv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "MissingSecret"}}
		h := newSessHarness(t, agent, sessSession("s1", "triage", time.Minute))
		if res := h.reconcile("s1"); res.RequeueAfter != time.Minute {
			t.Errorf("requeue = %v, want 60s", res.RequeueAfter)
		}
		if n := len(h.pods()); n != 0 {
			t.Fatalf("pods = %d", n)
		}
		h.clock.advance(31 * time.Minute)
		h.reconcile("s1")
		s := h.session("s1")
		if s.Status.Phase != ompv1alpha1.SessionPhaseFailed || s.Status.Error != "agent not ready: MissingSecret" {
			t.Errorf("status = phase %s error %q", s.Status.Phase, s.Status.Error)
		}
	})

	t.Run("future retryAt waits", func(t *testing.T) {
		h := newSessHarness(t, sessAgent("triage"), sessSession("s1", "triage", time.Minute))
		h.reconcile("s1")
		h.setPod("s1-1", func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed })
		h.reconcile("s1")
		res := h.reconcile("s1")
		if res.RequeueAfter != 15*time.Minute {
			t.Errorf("requeue = %v, want 15m", res.RequeueAfter)
		}
		if len(h.pods()) != 1 {
			t.Error("second pod started before retryAt")
		}
	})

	t.Run("managed gateway token waits two minutes after a clients write", func(t *testing.T) {
		agent := sessAgent("triage")
		agent.Spec.Model.Gateway = &ompv1alpha1.GatewaySpec{URL: "http://gw"}
		h := newSessHarness(t, agent, sessSession("s1", "triage", time.Minute))
		h.r.Options.GatewayClientsSecret = "clients"
		h.r.Gateway = &sessGateway{t: sessT0.Add(-30 * time.Second)}
		res := h.reconcile("s1")
		if res.RequeueAfter != 90*time.Second || len(h.pods()) != 0 {
			t.Fatalf("requeue = %v pods = %d", res.RequeueAfter, len(h.pods()))
		}
		h.clock.advance(91 * time.Second)
		h.reconcile("s1")
		if len(h.pods()) != 1 {
			t.Error("session did not start after the hold")
		}
	})

	t.Run("a zero clients write time does not hold", func(t *testing.T) {
		agent := sessAgent("triage")
		agent.Spec.Model.Gateway = &ompv1alpha1.GatewaySpec{URL: "http://gw"}
		h := newSessHarness(t, agent, sessSession("s1", "triage", time.Minute))
		h.r.Options.GatewayClientsSecret = "clients"
		h.r.Gateway = &sessGateway{}
		h.reconcile("s1")
		if len(h.pods()) != 1 {
			t.Error("session held by a zero write time")
		}
	})

	t.Run("an unfinished toolsync pod of the agent blocks", func(t *testing.T) {
		h := newSessHarness(t, sessAgent("triage"), sessSession("s1", "triage", time.Minute), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: sessNS, Name: "triage-toolsync-1",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "omp-operator", "omp.techdufus.com/agent": "triage",
				"omp.techdufus.com/role": "toolsync",
			},
		}})
		if res := h.reconcile("s1"); res.RequeueAfter != 10*time.Second || len(h.pods()) != 1 {
			t.Errorf("requeue = %v pods = %d", res.RequeueAfter, len(h.pods()))
		}
	})
}

func TestSessionResultTable(t *testing.T) {
	tests := []struct {
		name        string
		pod         func(*corev1.Pod) // nil: the pod is gone
		deletePod   bool
		status      func(*ompv1alpha1.OmpSessionStatus)
		wantPhase   ompv1alpha1.SessionPhase
		wantError   string
		wantRetry   bool
		wantVerdict string
		wantNotice  string
	}{
		{
			name: "verdict set succeeds",
			pod:  func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded },
			status: func(s *ompv1alpha1.OmpSessionStatus) {
				s.Verdict = ompv1alpha1.VerdictOK
				s.Outcome = ompv1alpha1.OutcomeCompleted
			},
			wantPhase:   ompv1alpha1.SessionPhaseSucceeded,
			wantVerdict: "ok",
		},
		{
			name: "verdict set wins over a retryable outcome",
			pod:  func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed },
			status: func(s *ompv1alpha1.OmpSessionStatus) {
				s.Verdict = ompv1alpha1.VerdictFixed
				s.Outcome = ompv1alpha1.OutcomeRetryable
			},
			wantPhase:   ompv1alpha1.SessionPhaseSucceeded,
			wantVerdict: "fixed",
		},
		{
			name:      "completed without verdict fails",
			pod:       func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded },
			status:    func(s *ompv1alpha1.OmpSessionStatus) { s.Outcome = ompv1alpha1.OutcomeCompleted },
			wantPhase: ompv1alpha1.SessionPhaseFailed,
			wantError: "agent ended without calling finish",
		},
		{
			name:      "retryable goes back to pending",
			pod:       func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded },
			status:    func(s *ompv1alpha1.OmpSessionStatus) { s.Outcome = ompv1alpha1.OutcomeRetryable; s.Error = "HTTP 529" },
			wantPhase: ompv1alpha1.SessionPhasePending,
			wantRetry: true,
			wantError: "HTTP 529",
		},
		{
			name:       "auth goes back to pending and sends the auth notice",
			pod:        func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded },
			status:     func(s *ompv1alpha1.OmpSessionStatus) { s.Outcome = ompv1alpha1.OutcomeAuth },
			wantPhase:  ompv1alpha1.SessionPhasePending,
			wantRetry:  true,
			wantNotice: "auth|triage: model login missing or expired",
		},
		{
			name:      "pod failed without complete retries",
			pod:       func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed },
			wantPhase: ompv1alpha1.SessionPhasePending,
			wantRetry: true,
		},
		{
			name:      "pod succeeded without complete retries",
			pod:       func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded },
			wantPhase: ompv1alpha1.SessionPhasePending,
			wantRetry: true,
		},
		{
			name:      "pod disappeared retries",
			deletePod: true,
			wantPhase: ompv1alpha1.SessionPhasePending,
			wantRetry: true,
		},
		{
			name: "error outcome fails and keeps the agent's error",
			pod:  func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded },
			status: func(s *ompv1alpha1.OmpSessionStatus) {
				s.Outcome = ompv1alpha1.OutcomeError
				s.Error = "session timed out"
			},
			wantPhase: ompv1alpha1.SessionPhaseFailed,
			wantError: "session timed out",
		},
		{
			name:      "error outcome without text gets a generic one",
			pod:       func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded },
			status:    func(s *ompv1alpha1.OmpSessionStatus) { s.Outcome = ompv1alpha1.OutcomeError },
			wantPhase: ompv1alpha1.SessionPhaseFailed,
			wantError: "agent reported an error",
		},
		{
			name: "pod deadline exceeded fails",
			pod: func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodFailed
				p.Status.Reason = "DeadlineExceeded"
			},
			wantPhase: ompv1alpha1.SessionPhaseFailed,
			wantError: "session timed out: pod deadline exceeded",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSessHarness(t, sessAgent("triage"), sessSession("s1", "triage", time.Minute))
			h.reconcile("s1") // starts attempt 1
			if tt.deletePod {
				if err := h.c.Delete(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: sessNS, Name: "s1-1"}}); err != nil {
					t.Fatal(err)
				}
			} else {
				h.setPod("s1-1", tt.pod)
			}
			if tt.status != nil {
				h.setSessionStatus("s1", tt.status)
			}
			h.clock.advance(5 * time.Minute)
			h.reconcile("s1")

			s := h.session("s1")
			if s.Status.Phase != tt.wantPhase {
				t.Fatalf("phase = %s, want %s (status %+v)", s.Status.Phase, tt.wantPhase, s.Status)
			}
			if tt.wantError != "" && !strings.Contains(s.Status.Error, tt.wantError) {
				t.Errorf("error = %q, want it to contain %q", s.Status.Error, tt.wantError)
			}
			if tt.wantRetry != (s.Status.RetryAt != nil) {
				t.Errorf("retryAt = %v, want set %v", s.Status.RetryAt, tt.wantRetry)
			}
			if tt.wantRetry && !s.Status.RetryAt.Time.Equal(h.clock.now().Add(15*time.Minute)) {
				t.Errorf("retryAt = %v, want now+15m", s.Status.RetryAt)
			}
			terminal := tt.wantPhase == ompv1alpha1.SessionPhaseSucceeded || tt.wantPhase == ompv1alpha1.SessionPhaseFailed
			if terminal {
				if s.Status.CompletionTime == nil || !s.Status.CompletionTime.Time.Equal(h.clock.now()) {
					t.Errorf("completionTime = %v", s.Status.CompletionTime)
				}
				if c := meta.FindStatusCondition(s.Status.Conditions, ompv1alpha1.ConditionNotified); c == nil || c.Status != metav1.ConditionTrue {
					t.Errorf("Notified = %+v", c)
				}
				want := "s1:" + string(tt.wantPhase) + ":" + tt.wantVerdict
				if len(h.n.done) != 1 || h.n.done[0] != want {
					t.Errorf("SessionDone calls = %v, want [%s]", h.n.done, want)
				}
			} else if s.Status.CompletionTime != nil || len(h.n.done) != 0 {
				t.Errorf("non-terminal session finished: %+v %v", s.Status.CompletionTime, h.n.done)
			}
			if tt.wantNotice != "" && (len(h.n.notices) != 1 || h.n.notices[0] != tt.wantNotice) {
				t.Errorf("notices = %v, want [%s]", h.n.notices, tt.wantNotice)
			}
			if tt.wantNotice == "" && len(h.n.notices) != 0 {
				t.Errorf("unexpected notices %v", h.n.notices)
			}
		})
	}
}

func TestSessionRetryResetsOutcome(t *testing.T) {
	h := newSessHarness(t, sessAgent("triage"), sessSession("s1", "triage", time.Minute))
	h.reconcile("s1")
	h.setPod("s1-1", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded })
	h.setSessionStatus("s1", func(s *ompv1alpha1.OmpSessionStatus) { s.Outcome = ompv1alpha1.OutcomeRetryable })
	h.reconcile("s1")
	h.clock.advance(16 * time.Minute)
	h.reconcile("s1")
	s := h.session("s1")
	if s.Status.Phase != ompv1alpha1.SessionPhaseRunning || s.Status.Outcome != "" {
		t.Errorf("phase = %s outcome = %q; a new attempt must start without the old outcome", s.Status.Phase, s.Status.Outcome)
	}
}

func TestSessionMaxAttemptsExhausted(t *testing.T) {
	h := newSessHarness(t, sessAgent("triage"), sessSession("s1", "triage", time.Minute)) // maxAttempts 3
	for attempt := 1; attempt <= 3; attempt++ {
		h.reconcile("s1")
		s := h.session("s1")
		if s.Status.Phase != ompv1alpha1.SessionPhaseRunning || s.Status.Attempts != int32(attempt) {
			t.Fatalf("attempt %d: status = %+v", attempt, s.Status)
		}
		h.setPod(s.Status.PodName, func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed })
		h.reconcile("s1")
		h.clock.advance(16 * time.Minute)
	}
	s := h.session("s1")
	if s.Status.Phase != ompv1alpha1.SessionPhaseFailed || !strings.HasPrefix(s.Status.Error, "attempts exhausted (3)") {
		t.Errorf("status = phase %s error %q", s.Status.Phase, s.Status.Error)
	}
	if len(h.pods()) != 3 {
		t.Errorf("pods = %v", podNames(h.pods()))
	}
}

func TestSessionPodStartTimeout(t *testing.T) {
	h := newSessHarness(t, sessAgent("triage"), sessSession("s1", "triage", time.Minute))
	h.reconcile("s1")
	// The fake client does not stamp creation times; give the pod one.
	h.setPod("s1-1", func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending })
	p := &corev1.Pod{}
	_ = h.c.Get(context.Background(), client.ObjectKey{Namespace: sessNS, Name: "s1-1"}, p)
	p.CreationTimestamp = metav1.NewTime(h.clock.now())
	if err := h.c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}

	h.clock.advance(4 * time.Minute)
	if res := h.reconcile("s1"); res.RequeueAfter != 6*time.Minute {
		t.Errorf("requeue = %v, want the remaining 6m", res.RequeueAfter)
	}
	if h.session("s1").Status.Phase != ompv1alpha1.SessionPhaseRunning {
		t.Fatal("session left Running before the limit")
	}

	h.clock.advance(7 * time.Minute)
	h.reconcile("s1")
	s := h.session("s1")
	if s.Status.Phase != ompv1alpha1.SessionPhasePending || s.Status.RetryAt == nil {
		t.Errorf("status = %+v", s.Status)
	}
	if len(h.pods()) != 0 {
		t.Errorf("stuck pod not deleted: %v", podNames(h.pods()))
	}
}

func TestSessionTunableTimings(t *testing.T) {
	agent := sessAgent("triage")
	agent.Status.Conditions = nil
	h := newSessHarness(t, agent, sessSession("s1", "triage", time.Second))
	h.r.NotReadyLimit = time.Millisecond
	h.r.NotReadyRequeue = time.Second
	h.clock.advance(time.Minute)
	h.reconcile("s1")
	if s := h.session("s1"); s.Status.Phase != ompv1alpha1.SessionPhaseFailed || s.Status.Error != "agent not ready: no Ready condition yet" {
		t.Errorf("status = %+v", s.Status)
	}
}

func TestSessionAdoptsPodFromHalfFinishedStart(t *testing.T) {
	s := sessSession("s1", "triage", time.Minute)
	h := newSessHarness(t, sessAgent("triage"), s)
	h.reconcile("s1")
	// Simulate a lost status write: pod exists, session is Pending with zero attempts.
	h.setSessionStatus("s1", func(st *ompv1alpha1.OmpSessionStatus) {
		*st = ompv1alpha1.OmpSessionStatus{Phase: ompv1alpha1.SessionPhasePending}
	})
	h.reconcile("s1")
	got := h.session("s1")
	if got.Status.Phase != ompv1alpha1.SessionPhaseRunning || got.Status.Attempts != 1 || got.Status.PodName != "s1-1" {
		t.Errorf("status = %+v", got.Status)
	}
	if len(h.pods()) != 1 {
		t.Errorf("pods = %v", podNames(h.pods()))
	}
}

func TestSessionToolchainGate(t *testing.T) { //nolint:gocyclo // one subtest per gate outcome
	toolAgent := func() *ompv1alpha1.OmpAgent {
		a := sessAgent("triage")
		a.Spec.Toolchain = &ompv1alpha1.ToolchainSpec{}
		return a
	}
	getAgent := func(h *sessHarness) *ompv1alpha1.OmpAgent {
		a := &ompv1alpha1.OmpAgent{}
		if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: sessNS, Name: "triage"}, a); err != nil {
			t.Fatal(err)
		}
		return a
	}

	t.Run("unsynced toolchain runs a toolsync pod first", func(t *testing.T) {
		h := newSessHarness(t, toolAgent(), sessSession("s1", "triage", time.Minute))
		res := h.reconcile("s1")
		if res.RequeueAfter != 10*time.Second {
			t.Errorf("requeue = %v", res.RequeueAfter)
		}
		pods := h.pods()
		if len(pods) != 1 || !strings.HasPrefix(pods[0].Name, "triage-toolsync-") || pods[0].Labels["omp.techdufus.com/role"] != "toolsync" {
			t.Fatalf("pods = %v", podNames(pods))
		}
		if !metav1IsOwnedBy(pods[0].OwnerReferences, "agent-triage") {
			t.Errorf("toolsync pod owners = %+v", pods[0].OwnerReferences)
		}
		sec := &corev1.Secret{}
		if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: sessNS, Name: "triage-toolsync-token"}, sec); err != nil || !metav1IsOwnedBy(sec.OwnerReferences, "agent-triage") {
			t.Errorf("toolsync token: %v %+v", err, sec.OwnerReferences)
		}
		if h.session("s1").Status.Phase != ompv1alpha1.SessionPhasePending {
			t.Error("session left Pending while toolsync runs")
		}
		// still running: the session waits
		h.setPod(pods[0].Name, func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning })
		h.reconcile("s1")
		if len(h.pods()) != 1 {
			t.Errorf("pods = %v", podNames(h.pods()))
		}
	})

	t.Run("a synced toolchain goes straight to the session", func(t *testing.T) {
		a := toolAgent()
		now := metav1.NewTime(sessT0)
		a.Status.Toolchain = ompv1alpha1.ToolchainStatus{Hash: "h", SyncedAt: &now}
		h := newSessHarness(t, a, sessSession("s1", "triage", time.Minute))
		h.reconcile("s1")
		if pods := h.pods(); len(pods) != 1 || pods[0].Name != "s1-1" {
			t.Errorf("pods = %v", podNames(pods))
		}
	})

	t.Run("a stale toolchain runs toolsync again", func(t *testing.T) {
		a := toolAgent()
		now := metav1.NewTime(sessT0)
		a.Status.Toolchain = ompv1alpha1.ToolchainStatus{Hash: "h", SyncedAt: &now, Stale: true}
		h := newSessHarness(t, a, sessSession("s1", "triage", time.Minute))
		h.reconcile("s1")
		if pods := h.pods(); len(pods) != 1 || !strings.Contains(pods[0].Name, "toolsync") {
			t.Errorf("pods = %v", podNames(pods))
		}
	})

	t.Run("success cleans up the toolsync pod and the session starts", func(t *testing.T) {
		h := newSessHarness(t, toolAgent(), sessSession("s1", "triage", time.Minute))
		h.reconcile("s1")
		name := h.pods()[0].Name
		h.setPod(name, func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded })
		h.setAgentStatus("triage", func(s *ompv1alpha1.OmpAgentStatus) {
			now := metav1.NewTime(sessT0)
			s.Toolchain = ompv1alpha1.ToolchainStatus{Hash: "h", SyncedAt: &now}
		})
		h.reconcile("s1")
		if pods := h.pods(); len(pods) != 1 || pods[0].Name != "s1-1" {
			t.Errorf("pods = %v", podNames(pods))
		}
		if len(h.n.notices) != 0 {
			t.Errorf("notices = %v", h.n.notices)
		}
	})

	t.Run("a failed toolsync is recorded once and the session proceeds", func(t *testing.T) {
		h := newSessHarness(t, toolAgent(), sessSession("s1", "triage", time.Minute))
		h.reconcile("s1")
		name := h.pods()[0].Name
		h.setPod(name, func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodFailed
			p.Status.Reason = "DeadlineExceeded"
		})
		h.clock.advance(time.Minute)
		h.reconcile("s1")

		a := getAgent(h)
		tc := a.Status.Toolchain
		if tc.LastAttemptAt == nil || !tc.LastAttemptAt.Time.Equal(h.clock.now()) || !strings.Contains(tc.LastError, "DeadlineExceeded") {
			t.Errorf("toolchain status = %+v", tc)
		}
		c := meta.FindStatusCondition(a.Status.Conditions, ompv1alpha1.ConditionToolchainReady)
		if c == nil || c.Status != metav1.ConditionFalse {
			t.Errorf("ToolchainReady = %+v", c)
		}
		if len(h.n.notices) != 1 || !strings.HasPrefix(h.n.notices[0], "toolchain|") {
			t.Errorf("notices = %v", h.n.notices)
		}
		if pods := h.pods(); len(pods) != 1 || pods[0].Name != "s1-1" {
			t.Errorf("pods = %v; the failed toolsync pod should be gone and the session started", podNames(pods))
		}
	})

	t.Run("no new toolsync within six hours of a failed attempt", func(t *testing.T) {
		a := toolAgent()
		at := metav1.NewTime(sessT0.Add(-time.Hour))
		a.Status.Toolchain.LastAttemptAt = &at
		h := newSessHarness(t, a, sessSession("s1", "triage", time.Minute))
		h.reconcile("s1")
		if pods := h.pods(); len(pods) != 1 || pods[0].Name != "s1-1" {
			t.Fatalf("pods = %v", podNames(pods))
		}

		// after six hours it tries again
		h2 := newSessHarness(t, func() *ompv1alpha1.OmpAgent {
			b := toolAgent()
			old := metav1.NewTime(sessT0.Add(-7 * time.Hour))
			b.Status.Toolchain.LastAttemptAt = &old
			return b
		}(), sessSession("s1", "triage", time.Minute))
		h2.reconcile("s1")
		if pods := h2.pods(); len(pods) != 1 || !strings.Contains(pods[0].Name, "toolsync") {
			t.Errorf("pods = %v", podNames(pods))
		}
	})
}

func fluxSource(kind string, artifact bool) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(fluxSourceGV.WithKind(kind))
	u.SetNamespace("flux-system")
	u.SetName("src")
	if artifact {
		_ = unstructured.SetNestedField(u.Object, "http://source-controller/a.tar.gz", "status", "artifact", "url")
	}
	return u
}

func TestSessionWorkspaceGate(t *testing.T) {
	fluxAgent := func() *ompv1alpha1.OmpAgent {
		a := sessAgent("triage")
		a.Spec.Workspace = &ompv1alpha1.WorkspaceSpec{FluxSource: &ompv1alpha1.FluxSourceRef{
			Kind: ompv1alpha1.FluxSourceGitRepository, Namespace: "flux-system", Name: "src",
		}}
		return a
	}

	t.Run("missing artifact waits, notifies and then fails", func(t *testing.T) {
		h := newSessHarness(t, fluxAgent(), sessSession("s1", "triage", time.Minute), fluxSource("GitRepository", false))
		res := h.reconcile("s1")
		if res.RequeueAfter != time.Minute || len(h.pods()) != 0 {
			t.Fatalf("requeue = %v pods = %d", res.RequeueAfter, len(h.pods()))
		}
		s := h.session("s1")
		c := meta.FindStatusCondition(s.Status.Conditions, ompv1alpha1.ConditionWorkspaceReady)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "NoArtifact" {
			t.Errorf("WorkspaceReady = %+v", c)
		}
		if len(h.n.notices) != 1 || !strings.HasPrefix(h.n.notices[0], "workspace|") {
			t.Errorf("notices = %v", h.n.notices)
		}
		h.clock.advance(31 * time.Minute)
		h.reconcile("s1")
		if s := h.session("s1"); s.Status.Phase != ompv1alpha1.SessionPhaseFailed || !strings.HasPrefix(s.Status.Error, "workspace not ready") {
			t.Errorf("status = phase %s error %q", s.Status.Phase, s.Status.Error)
		}
	})

	t.Run("missing source reports SourceNotFound", func(t *testing.T) {
		h := newSessHarness(t, fluxAgent(), sessSession("s1", "triage", time.Minute))
		h.reconcile("s1")
		c := meta.FindStatusCondition(h.session("s1").Status.Conditions, ompv1alpha1.ConditionWorkspaceReady)
		if c == nil || c.Reason != "SourceNotFound" {
			t.Errorf("WorkspaceReady = %+v", c)
		}
	})

	t.Run("an artifact lets the session start and flips the condition", func(t *testing.T) {
		src := fluxSource("GitRepository", false)
		h := newSessHarness(t, fluxAgent(), sessSession("s1", "triage", time.Minute), src)
		h.reconcile("s1")
		cur := fluxSource("GitRepository", false)
		if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: "flux-system", Name: "src"}, cur); err != nil {
			t.Fatal(err)
		}
		_ = unstructured.SetNestedField(cur.Object, "http://source-controller/a.tar.gz", "status", "artifact", "url")
		if err := h.c.Update(context.Background(), cur); err != nil {
			t.Fatal(err)
		}
		h.reconcile("s1")
		s := h.session("s1")
		if s.Status.Phase != ompv1alpha1.SessionPhaseRunning || len(h.pods()) != 1 {
			t.Fatalf("status = %+v pods = %d", s.Status, len(h.pods()))
		}
		if c := meta.FindStatusCondition(s.Status.Conditions, ompv1alpha1.ConditionWorkspaceReady); c == nil || c.Status != metav1.ConditionTrue {
			t.Errorf("WorkspaceReady = %+v", c)
		}
	})
}

func TestSessionRetentionAndFinalizer(t *testing.T) {
	setup := func(t *testing.T) *sessHarness {
		agent := sessAgent("triage")
		agent.Spec.SessionRetention = metav1.Duration{Duration: time.Hour}
		h := newSessHarness(t, agent, sessSession("s1", "triage", time.Minute))
		h.reconcile("s1")
		h.setPod("s1-1", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded })
		h.setSessionStatus("s1", func(s *ompv1alpha1.OmpSessionStatus) { s.Verdict = ompv1alpha1.VerdictOK })
		h.reconcile("s1")
		if h.session("s1").Status.Phase != ompv1alpha1.SessionPhaseSucceeded {
			t.Fatal("not Succeeded")
		}
		return h
	}

	t.Run("kept until completionTime + retention, then deleted with cleanup", func(t *testing.T) {
		h := setup(t)
		uid := "uid-s1"
		if err := h.store.WriteReport(uid, "report"); err != nil {
			t.Fatal(err)
		}
		res := h.reconcile("s1")
		if res.RequeueAfter != time.Hour {
			t.Errorf("requeue = %v, want 1h", res.RequeueAfter)
		}
		h.clock.advance(30 * time.Minute)
		if res := h.reconcile("s1"); res.RequeueAfter != 30*time.Minute {
			t.Errorf("requeue = %v, want 30m", res.RequeueAfter)
		}
		h.clock.advance(31 * time.Minute)
		h.reconcile("s1") // deletes; the finalizer keeps the object with a deletionTimestamp
		if s := h.session("s1"); s.DeletionTimestamp.IsZero() {
			t.Fatal("expired session not deleted")
		}
		h.reconcile("s1") // finalizer runs
		s := &ompv1alpha1.OmpSession{}
		if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: sessNS, Name: "s1"}, s); !apierrors.IsNotFound(err) {
			t.Errorf("session still present: %v", err)
		}
		if _, err := h.store.ReadReport(uid); err == nil {
			t.Error("session data not removed")
		}
	})

	t.Run("continuity tarball is removed only with the last session of its key", func(t *testing.T) {
		agent := sessAgent("triage")
		a, b := sessSession("a", "triage", time.Minute), sessSession("b", "triage", time.Minute)
		a.Spec.ContinuityKey, b.Spec.ContinuityKey = "trig/g1", "trig/g1"
		h := newSessHarness(t, agent, a, b)
		id := store.ContinuityID("trig/g1")
		if err := h.store.PutContinuity(sessNS, "triage", id, strings.NewReader("tar")); err != nil {
			t.Fatal(err)
		}
		// set up labels + finalizers without starting pods
		h.r.NotReadyLimit = time.Hour
		for _, n := range []string{"a", "b"} {
			if err := h.r.ensureMetadata(context.Background(), h.session(n)); err != nil {
				t.Fatal(err)
			}
		}
		if h.session("a").Labels["omp.techdufus.com/continuity"] != id {
			t.Fatalf("continuity label = %v", h.session("a").Labels)
		}

		if err := h.c.Delete(context.Background(), h.session("a")); err != nil {
			t.Fatal(err)
		}
		h.reconcile("a")
		if f, err := h.store.OpenContinuity(sessNS, "triage", id); err != nil {
			t.Fatalf("tarball removed while session b still holds the key: %v", err)
		} else {
			_ = f.Close()
		}

		if err := h.c.Delete(context.Background(), h.session("b")); err != nil {
			t.Fatal(err)
		}
		h.reconcile("b")
		if _, err := h.store.OpenContinuity(sessNS, "triage", id); err == nil {
			t.Error("tarball kept after the last session was deleted")
		}
	})
}

func TestSessionMetricsAndActiveGauge(t *testing.T) {
	h := newSessHarness(t, sessAgent("metrics-agent"), sessSession("m1", "metrics-agent", time.Minute))
	h.reconcile("m1")
	active := func(phase string) float64 {
		return testutil.ToFloat64(metrics.SessionsActive.WithLabelValues(sessNS, "metrics-agent", phase))
	}
	if active("Running") != 1 || active("Pending") != 0 {
		t.Errorf("active gauge = running %v pending %v", active("Running"), active("Pending"))
	}
	h.setPod("m1-1", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded })
	h.setSessionStatus("m1", func(s *ompv1alpha1.OmpSessionStatus) { s.Verdict = ompv1alpha1.VerdictNeedsOwner })
	h.clock.advance(90 * time.Second)
	h.reconcile("m1")
	if got := testutil.ToFloat64(metrics.SessionsTotal.WithLabelValues(sessNS, "metrics-agent", "", "manual", "needs_owner")); got != 1 {
		t.Errorf("sessions_total = %v", got)
	}
	if active("Running") != 0 {
		t.Errorf("running gauge = %v", active("Running"))
	}
}

func TestSessionNamespaceNotAllowedIsIgnored(t *testing.T) {
	h := newSessHarness(t, sessAgent("triage"), sessSession("s1", "triage", time.Minute))
	h.r.Options.AgentNamespaces = []string{"other"}
	h.reconcile("s1")
	if s := h.session("s1"); s.Status.Phase != "" || len(s.Finalizers) != 0 || len(h.pods()) != 0 {
		t.Errorf("session in a disallowed namespace was handled: %+v", s.Status)
	}
}
