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

package hooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/celx"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/episodes"
	"github.com/TechDufus/omp-operator/internal/metrics"
	"github.com/TechDufus/omp-operator/internal/notify"
)

const (
	testNS         = "apps"
	testTrigger    = "am"
	testHookSecret = "hook"
	testTokenKey   = "token"
	testAgent      = "triage"
	testToken      = "s3cr3t-token"
)

var epoch = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type rig struct {
	t       *testing.T
	clock   *clock
	client  client.Client
	handler *Handler
	gets    atomic.Int64
	ntfy    *httptest.Server
	ntfyHit atomic.Int64
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func readyTrigger(name string) *v1alpha1.OmpTrigger {
	return &v1alpha1.OmpTrigger{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name, UID: "trigger-uid", Generation: 1},
		Spec: v1alpha1.OmpTriggerSpec{
			AgentRef: v1alpha1.LocalObjectRef{Name: testAgent},
			Prompt:   "Triage {{.Event.Name}} ({{.Reason}}, {{.AlertCount}} alerts)\nPrevious: {{.Previous}}\n{{.Alerts}}",
			Webhook: &v1alpha1.WebhookSpec{
				Adapter: v1alpha1.AdapterAlertmanager,
				Auth:    v1alpha1.WebhookAuth{Bearer: &v1alpha1.BearerAuth{SecretRef: v1alpha1.SecretKeyRef{Name: testHookSecret, Key: testTokenKey}}},
				Grouping: v1alpha1.GroupingSpec{
					RetriageAfter: metav1.Duration{Duration: time.Second},
				},
			},
		},
		Status: v1alpha1.OmpTriggerStatus{
			Conditions: []metav1.Condition{{
				Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Ready", ObservedGeneration: 1,
				LastTransitionTime: metav1.NewTime(epoch),
			}},
		},
	}
}

func newRig(t *testing.T, objs ...client.Object) *rig {
	t.Helper()
	r := &rig{t: t, clock: &clock{t: epoch}}
	r.ntfy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		r.ntfyHit.Add(1)
	}))
	t.Cleanup(r.ntfy.Close)

	base := make([]client.Object, 0, 2+len(objs))
	base = append(base,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: testHookSecret},
			Data:       map[string][]byte{testTokenKey: []byte(testToken), "username": []byte("u"), "password": []byte("p")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "ntfy"},
			Data:       map[string][]byte{"topic": []byte("alerts")},
		},
	)
	base = append(base, objs...)
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithStatusSubresource(&v1alpha1.OmpTrigger{}, &v1alpha1.OmpAgent{}, &v1alpha1.OmpSession{}).
		WithObjects(base...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*v1alpha1.OmpTrigger); ok {
					r.gets.Add(1)
				}
				return c.Get(ctx, key, obj, opts...)
			},
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if s, ok := obj.(*v1alpha1.OmpSession); ok && s.CreationTimestamp.IsZero() {
					s.CreationTimestamp = metav1.NewTime(r.clock.Now())
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
	r.client = c
	r.handler = &Handler{
		Client:    c,
		APIReader: c,
		Options:   &config.Options{},
		CEL:       celx.NewCache(),
		Now:       r.clock.Now,
		Notify: &notify.Dispatcher{
			Client: c, APIReader: c, Now: r.clock.Now,
			Sender: &notify.Sender{Client: r.ntfy.Client(), RetryDelay: time.Millisecond},
		},
	}
	return r
}

func (r *rig) agent(mutate func(*v1alpha1.OmpAgent)) *v1alpha1.OmpAgent {
	a := &v1alpha1.OmpAgent{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: testAgent, UID: "agent-uid"},
		Spec: v1alpha1.OmpAgentSpec{
			Notify: []v1alpha1.NotifySink{{
				Type: v1alpha1.SinkNtfy,
				Ntfy: &v1alpha1.NtfySink{URL: r.ntfy.URL, TopicSecretRef: v1alpha1.SecretKeyRef{Name: "ntfy", Key: "topic"}},
			}},
		},
	}
	if mutate != nil {
		mutate(a)
	}
	return a
}

func (r *rig) post(path, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	return rec
}

func (r *rig) hook(body string) *httptest.ResponseRecorder {
	r.t.Helper()
	return r.post("/hooks/"+testNS+"/"+testTrigger, body, nil)
}

func (r *rig) sessions() []v1alpha1.OmpSession {
	r.t.Helper()
	list := &v1alpha1.OmpSessionList{}
	if err := r.client.List(context.Background(), list, client.InNamespace(testNS)); err != nil {
		r.t.Fatal(err)
	}
	return list.Items
}

func (r *rig) trigger() *v1alpha1.OmpTrigger {
	r.t.Helper()
	tr := &v1alpha1.OmpTrigger{}
	if err := r.client.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: testTrigger}, tr); err != nil {
		r.t.Fatal(err)
	}
	return tr
}

// alertPayload is an Alertmanager v4 body with one alert per fingerprint.
func alertPayload(groupKey, status, severity string, fingerprints ...string) string {
	alerts := make([]string, 0, len(fingerprints))
	for _, fp := range fingerprints {
		alerts = append(alerts, fmt.Sprintf(`{"status":%q,"labels":{"alertname":"HighLatency","namespace":"shop","severity":%q},`+
			`"annotations":{"summary":"slow"},"startsAt":"2026-10-18T11:00:00Z","fingerprint":%q}`, status, severity, fp))
	}
	return fmt.Sprintf(`{"version":"4","groupKey":%q,"status":%q,"receiver":"omp",`+
		`"groupLabels":{"alertname":"HighLatency"},"commonLabels":{"alertname":"HighLatency","namespace":"shop","severity":%q},`+
		`"commonAnnotations":{},"externalURL":"http://am","alerts":[%s]}`,
		groupKey, status, severity, strings.Join(alerts, ","))
}

func TestRejectsOversizedBodyBeforeLookup(t *testing.T) {
	r := newRig(t, r0(t), readyTrigger(testTrigger))
	big := strings.Repeat("x", MaxBodyBytes+1)
	rec := r.hook(big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", rec.Code)
	}
	if got := r.gets.Load(); got != 0 {
		t.Fatalf("trigger was read %d times before the size check", got)
	}
	// An oversized body for an unknown trigger is 413 too: no lookup decides it.
	rec = r.post("/hooks/"+testNS+"/nope", big, nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("unknown trigger, oversized body: status %d, want 413", rec.Code)
	}
	// A body of exactly the limit is accepted for reading (and then fails parsing, not size).
	rec = r.hook(strings.Repeat(" ", MaxBodyBytes))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("limit-sized body: status %d, want 400", rec.Code)
	}
}

// r0 returns the default agent for rigs that do not customise it.
func r0(t *testing.T) *v1alpha1.OmpAgent {
	t.Helper()
	return &v1alpha1.OmpAgent{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: testAgent, UID: "agent-uid"},
	}
}

func TestUnauthenticatedRequestsGet401(t *testing.T) {
	scheduleTrigger := readyTrigger("sched")
	scheduleTrigger.Spec.Webhook = nil
	scheduleTrigger.Spec.Schedule = &v1alpha1.ScheduleSpec{Cron: "* * * * *"}
	basic := readyTrigger("basic")
	basic.Spec.Webhook.Auth = v1alpha1.WebhookAuth{Basic: &v1alpha1.BasicAuth{SecretRef: v1alpha1.LocalObjectRef{Name: testHookSecret}}}
	noSecret := readyTrigger("nosecret")
	noSecret.Spec.Webhook.Auth.Bearer.SecretRef.Name = "absent"
	other := readyTrigger("other")
	other.Namespace = "elsewhere"

	r := newRig(t, r0(t), readyTrigger(testTrigger), scheduleTrigger, basic, noSecret, other)
	r.handler.Options = &config.Options{AgentNamespaces: []string{testNS}}

	cases := []struct {
		name string
		path string
		mut  func(*http.Request)
	}{
		{"unknown trigger", "/hooks/apps/missing", nil},
		{"unknown namespace", "/hooks/nowhere/am", nil},
		{"schedule trigger", "/hooks/apps/sched", nil},
		{"disallowed namespace", "/hooks/elsewhere/other", nil},
		{"wrong bearer", "/hooks/apps/am", func(q *http.Request) { q.Header.Set("Authorization", "Bearer nope") }},
		{"missing credential", "/hooks/apps/am", func(q *http.Request) { q.Header.Del("Authorization") }},
		{"bearer on basic trigger", "/hooks/apps/basic", nil},
		{"wrong basic", "/hooks/apps/basic", func(q *http.Request) { q.SetBasicAuth("u", "wrong") }},
		{"missing secret", "/hooks/apps/nosecret", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := r.post(tc.path, alertPayload("g", "firing", "warning", "f1"), tc.mut)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status %d, want 401", rec.Code)
			}
		})
	}
	if n := len(r.sessions()); n != 0 {
		t.Fatalf("%d sessions created by rejected requests", n)
	}

	rec := r.post("/hooks/apps/basic", alertPayload("g", "firing", "warning", "f1"), func(q *http.Request) { q.SetBasicAuth("u", "p") })
	if rec.Code != http.StatusAccepted {
		t.Fatalf("valid basic credentials: status %d, want 202", rec.Code)
	}
}

func TestHMACAuth(t *testing.T) {
	sign := func(key, body string) string {
		m := hmac.New(sha256.New, []byte(key))
		m.Write([]byte(body))
		return hex.EncodeToString(m.Sum(nil))
	}
	tr := readyTrigger(testTrigger)
	tr.Spec.Webhook.Auth = v1alpha1.WebhookAuth{HMAC: &v1alpha1.HMACAuth{SecretRef: v1alpha1.SecretKeyRef{Name: testHookSecret, Key: testTokenKey}}}
	r := newRig(t, r0(t), tr)
	body := alertPayload("g", "firing", "warning", "f1")

	rec := r.post("/hooks/apps/am", body, func(q *http.Request) {
		q.Header.Del("Authorization")
		q.Header.Set("X-Hub-Signature-256", "sha256="+sign("wrong", body))
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: status %d, want 401", rec.Code)
	}
	rec = r.post("/hooks/apps/am", body, func(q *http.Request) {
		q.Header.Del("Authorization")
		q.Header.Set("X-Hub-Signature-256", "sha256="+sign(testToken, body))
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("good signature: status %d, want 202", rec.Code)
	}
}

func TestStateChecks(t *testing.T) {
	off := false
	disabled := readyTrigger("disabled")
	disabled.Spec.Enabled = &off
	notReady := readyTrigger("notready")
	notReady.Status.Conditions[0].Status = metav1.ConditionFalse
	noCondition := readyTrigger("nocondition")
	noCondition.Status.Conditions = nil
	noAgent := readyTrigger("noagent")
	noAgent.Spec.AgentRef.Name = "ghost"

	r := newRig(t, r0(t), disabled, notReady, noCondition, noAgent)
	for _, name := range []string{"disabled", "notready", "nocondition", "noagent"} {
		t.Run(name, func(t *testing.T) {
			rec := r.post("/hooks/apps/"+name, alertPayload("g", "firing", "warning", "f1"), nil)
			if rec.Code != http.StatusConflict {
				t.Fatalf("status %d, want 409", rec.Code)
			}
		})
	}
	if n := len(r.sessions()); n != 0 {
		t.Fatalf("%d sessions created", n)
	}
}

func TestUnparsablePayloadIs400(t *testing.T) {
	r := newRig(t, r0(t), readyTrigger(testTrigger))
	for _, body := range []string{"not json", `{"alerts": 3}`, ""} {
		if rec := r.hook(body); rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status %d, want 400", body, rec.Code)
		}
	}
}

func TestSessionCreatedThenDeduped(t *testing.T) {
	tr := readyTrigger(testTrigger)
	tr.Spec.Session.Resume = v1alpha1.ResumeEpisode
	r := newRig(t, r0(t), tr)
	body := alertPayload("group-1", "firing", "warning", "f1", "f2")

	before := testutil.ToFloat64(metrics.WebhookEventsTotal.WithLabelValues(testNS, testTrigger, "session"))
	rec := r.hook(body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first request: status %d, want 202 (%s)", rec.Code, rec.Body)
	}
	sessions := r.sessions()
	if len(sessions) != 1 {
		t.Fatalf("%d sessions, want 1", len(sessions))
	}
	s := sessions[0]
	gid := episodes.GroupID("group-1")
	checkWebhookSession(t, &s, gid)

	groups := r.trigger().Status.Groups
	if len(groups) != 1 {
		t.Fatalf("%d groups, want 1", len(groups))
	}
	g := groups[0]
	if g.ID != gid || g.LastSession != s.Name || len(g.TriagedFingerprints) != 2 || g.LastSessionAt == nil || g.ResolvedAt != nil {
		t.Errorf("group %+v", g)
	}

	// The same payload again has nothing new to triage.
	r.clock.Advance(10 * time.Second)
	rec = r.hook(body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("second request: status %d, want 204", rec.Code)
	}
	if n := len(r.sessions()); n != 1 {
		t.Fatalf("%d sessions after dedupe, want 1", n)
	}
	if got := testutil.ToFloat64(metrics.WebhookEventsTotal.WithLabelValues(testNS, testTrigger, "session")) - before; got != 1 {
		t.Errorf("session counter moved by %v, want 1", got)
	}
	if testutil.ToFloat64(metrics.WebhookEventsTotal.WithLabelValues(testNS, testTrigger, "deduped")) < 1 {
		t.Error("deduped counter did not move")
	}
	if got := r.trigger().Status.Groups[0].LastNotified.Time; !got.Equal(epoch.Add(10 * time.Second)) {
		t.Errorf("lastNotified %v, want it advanced to the second request", got)
	}
}

// checkWebhookSession asserts the fields of the session TestSessionCreatedThenDeduped expects.
func checkWebhookSession(t *testing.T, s *v1alpha1.OmpSession, gid string) {
	t.Helper()
	wantName := fmt.Sprintf("%s-20261018-120000-", testTrigger)
	if !strings.HasPrefix(s.Name, wantName) || len(s.Name) != len(wantName)+4 {
		t.Errorf("session name %q, want %s<4 hex>", s.Name, wantName)
	}
	wantLabels := map[string]string{
		v1alpha1.LabelAgent: testAgent, v1alpha1.LabelTrigger: testTrigger, v1alpha1.LabelKind: "webhook",
		v1alpha1.LabelGroup: gid, v1alpha1.LabelSeverity: "warning",
	}
	for k, v := range wantLabels {
		if s.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, s.Labels[k], v)
		}
	}
	wantKey := fmt.Sprintf("%s/%s/%d", testTrigger, gid, epoch.Unix())
	if s.Spec.ContinuityKey != wantKey {
		t.Errorf("continuityKey %q, want %q", s.Spec.ContinuityKey, wantKey)
	}
	if s.Labels[v1alpha1.LabelContinuity] == "" {
		t.Error("continuity label missing")
	}
	if s.Spec.Priority == nil || *s.Spec.Priority != 2 {
		t.Errorf("priority %v, want 2", s.Spec.Priority)
	}
	if s.Spec.Event == nil || s.Spec.Event.Key != "group-1" || s.Spec.Event.GroupID != gid || s.Spec.Event.Name != "HighLatency/shop" {
		t.Errorf("event %+v", s.Spec.Event)
	}
	if s.Spec.TriggerRef == nil || s.Spec.TriggerRef.Name != testTrigger || s.Spec.AgentRef.Name != testAgent {
		t.Errorf("refs: agent %v trigger %v", s.Spec.AgentRef, s.Spec.TriggerRef)
	}
	if len(s.OwnerReferences) != 2 || s.OwnerReferences[0].Kind != "OmpAgent" || s.OwnerReferences[1].Kind != "OmpTrigger" {
		t.Errorf("owner references %+v", s.OwnerReferences)
	}
	for _, want := range []string{"Triage HighLatency/shop (new episode, 2 alerts)", "Previous: none", "Untrusted data (alerts)"} {
		if !strings.Contains(s.Spec.Prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, s.Spec.Prompt)
		}
	}
}

func TestBusyThenNewAlertsAfterRetriage(t *testing.T) {
	r := newRig(t, r0(t), readyTrigger(testTrigger))
	if rec := r.hook(alertPayload("g", "firing", "warning", "f1")); rec.Code != http.StatusAccepted {
		t.Fatalf("status %d", rec.Code)
	}
	// The first session is still Pending: a new alert in the group is busy, not a new session.
	r.clock.Advance(5 * time.Second)
	if rec := r.hook(alertPayload("g", "firing", "warning", "f1", "f2")); rec.Code != http.StatusNoContent {
		t.Fatalf("busy: status %d, want 204", rec.Code)
	}
	if n := len(r.sessions()); n != 1 {
		t.Fatalf("%d sessions, want 1", n)
	}
	// Once it has finished the new alert gets its own session, with the previous summary.
	first := r.sessions()[0]
	first.Status.Phase = v1alpha1.SessionPhaseSucceeded
	first.Status.Summary = "restarted the pod"
	if err := r.client.Status().Update(context.Background(), &first); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(5 * time.Second)
	if rec := r.hook(alertPayload("g", "firing", "warning", "f1", "f2")); rec.Code != http.StatusAccepted {
		t.Fatalf("after finish: status %d, want 202", rec.Code)
	}
	var second *v1alpha1.OmpSession
	for _, s := range r.sessions() {
		if s.Name != first.Name {
			second = &s
		}
	}
	if second == nil || !strings.Contains(second.Spec.Prompt, "Previous: restarted the pod") ||
		!strings.Contains(second.Spec.Prompt, "new alerts in group") {
		t.Fatalf("second session %+v", second)
	}
}

func TestCriticalPriorityAndResolved(t *testing.T) {
	tr := readyTrigger(testTrigger)
	p := int32(0)
	tr.Spec.Priority = &p
	tr.Spec.Session.Resume = v1alpha1.ResumeKey
	r := newRig(t, r0(t), tr)

	if rec := r.hook(alertPayload("crit", "firing", "critical", "f1")); rec.Code != http.StatusAccepted {
		t.Fatalf("status %d", rec.Code)
	}
	s := r.sessions()[0]
	if s.Spec.Priority == nil || *s.Spec.Priority != 1 {
		t.Errorf("critical priority %v, want 1", s.Spec.Priority)
	}
	if want := testTrigger + "/" + episodes.GroupID("crit"); s.Spec.ContinuityKey != want {
		t.Errorf("continuityKey %q, want %q", s.Spec.ContinuityKey, want)
	}

	r.clock.Advance(time.Minute)
	if rec := r.hook(alertPayload("crit", "resolved", "critical", "f1")); rec.Code != http.StatusNoContent {
		t.Fatalf("resolved: status %d, want 204", rec.Code)
	}
	if g := r.trigger().Status.Groups[0]; g.ResolvedAt == nil || !g.ResolvedAt.Time.Equal(r.clock.Now()) {
		t.Errorf("resolvedAt %v, want %v", g.ResolvedAt, r.clock.Now())
	}
}

func TestFilteredEventsCreateNothing(t *testing.T) {
	tr := readyTrigger(testTrigger)
	tr.Spec.Webhook.Filter = `event.severity == 'critical'`
	r := newRig(t, r0(t), tr)
	if rec := r.hook(alertPayload("g", "firing", "warning", "f1")); rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204", rec.Code)
	}
	if n := len(r.sessions()); n != 0 {
		t.Fatalf("%d sessions, want 0", n)
	}
	if g := r.trigger().Status.Groups; len(g) != 0 {
		t.Fatalf("filtered event stored groups: %+v", g)
	}
}

func TestPromptRenderErrorLeavesFingerprintsUntriaged(t *testing.T) {
	tr := readyTrigger(testTrigger)
	tr.Spec.Prompt = "{{.Event.Nope}}"
	r := newRig(t, r0(t), tr)
	before := testutil.ToFloat64(metrics.WebhookEventsTotal.WithLabelValues(testNS, testTrigger, "error"))
	if rec := r.hook(alertPayload("g", "firing", "warning", "f1")); rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204", rec.Code)
	}
	if n := len(r.sessions()); n != 0 {
		t.Fatalf("%d sessions, want 0", n)
	}
	if got := testutil.ToFloat64(metrics.WebhookEventsTotal.WithLabelValues(testNS, testTrigger, "error")) - before; got != 1 {
		t.Errorf("error counter moved by %v, want 1", got)
	}
	g := r.trigger().Status.Groups
	if len(g) != 1 || len(g[0].TriagedFingerprints) != 0 {
		t.Fatalf("groups %+v: the fingerprint must stay untriaged", g)
	}
}

func TestCapSendsOneNoticePerDay(t *testing.T) {
	r := newRig(t, readyTrigger(testTrigger))
	agent := r.agent(func(a *v1alpha1.OmpAgent) { a.Spec.Limits.EventSessionsPerDay = 1 })
	if err := r.client.Create(context.Background(), agent); err != nil {
		t.Fatal(err)
	}

	if rec := r.hook(alertPayload("g1", "firing", "warning", "f1")); rec.Code != http.StatusAccepted {
		t.Fatalf("first: status %d, want 202", rec.Code)
	}
	for i, key := range []string{"g2", "g3", "g4"} {
		r.clock.Advance(time.Minute)
		if rec := r.hook(alertPayload(key, "firing", "warning", "f"+key)); rec.Code != http.StatusNoContent {
			t.Fatalf("capped request %d: status %d, want 204", i, rec.Code)
		}
		r.handler.Wait()
	}
	if n := len(r.sessions()); n != 1 {
		t.Fatalf("%d sessions, want 1", n)
	}
	if got := r.ntfyHit.Load(); got != 1 {
		t.Fatalf("%d notices sent, want 1", got)
	}
	got := &v1alpha1.OmpAgent{}
	if err := r.client.Get(context.Background(), client.ObjectKeyFromObject(agent), got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Status.Notices["cap:"+testTrigger+":2026-10-18"]; !ok {
		t.Errorf("notices %v lack the cap key", got.Status.Notices)
	}

	// A restarted operator has no in-process guard; the agent's status.notices still suppresses the notice.
	restarted := &Handler{
		Client: r.client, APIReader: r.client, Options: r.handler.Options, CEL: celx.NewCache(), Now: r.clock.Now,
		Notify: r.handler.Notify,
	}
	req := httptest.NewRequest(http.MethodPost, "/hooks/"+testNS+"/"+testTrigger, strings.NewReader(alertPayload("g9", "firing", "warning", "f9")))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	restarted.ServeHTTP(rec, req)
	restarted.Wait()
	if rec.Code != http.StatusNoContent || r.ntfyHit.Load() != 1 {
		t.Fatalf("restarted handler: status %d, %d notices; want 204 and still 1", rec.Code, r.ntfyHit.Load())
	}

	// The next day the cap is reported again (the process guard and the agent record are both per day).
	r.clock.Advance(24 * time.Hour)
	r.clock.Advance(time.Minute)
	// The first session has aged out of the rolling window, so make the cap bite again.
	for _, s := range r.sessions() {
		s.CreationTimestamp = metav1.NewTime(r.clock.Now())
		if err := r.client.Update(context.Background(), &s); err != nil {
			t.Fatal(err)
		}
	}
	if rec := r.hook(alertPayload("g5", "firing", "warning", "f5")); rec.Code != http.StatusNoContent {
		t.Fatalf("next day: status %d, want 204", rec.Code)
	}
	r.handler.Wait()
	if got := r.ntfyHit.Load(); got != 2 {
		t.Fatalf("%d notices after the next day, want 2", got)
	}
}

func TestGroupPruningAtLimit(t *testing.T) {
	tr := readyTrigger(testTrigger)
	// 100 groups: the oldest of them is resolved, the rest are firing.
	for i := range episodes.MaxGroups {
		g := v1alpha1.TriggerGroup{
			ID:           fmt.Sprintf("old%02d", i),
			EpisodeStart: metav1.NewTime(epoch.Add(-3 * time.Hour)),
			LastNotified: metav1.NewTime(epoch.Add(-time.Duration(i+1) * time.Minute)),
		}
		switch i {
		case 40:
			res := metav1.NewTime(epoch.Add(-5 * time.Hour))
			g.ResolvedAt = &res
		case 41:
			res := metav1.NewTime(epoch.Add(-2 * time.Hour))
			g.ResolvedAt = &res
		}
		tr.Status.Groups = append(tr.Status.Groups, g)
	}
	r := newRig(t, r0(t), tr)
	if rec := r.hook(alertPayload("brand-new", "firing", "warning", "fx")); rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202", rec.Code)
	}
	groups := r.trigger().Status.Groups
	if len(groups) != episodes.MaxGroups {
		t.Fatalf("%d groups, want %d", len(groups), episodes.MaxGroups)
	}
	ids := map[string]bool{}
	for _, g := range groups {
		ids[g.ID] = true
	}
	if !ids[episodes.GroupID("brand-new")] {
		t.Error("the new group was pruned")
	}
	if ids["old40"] {
		t.Error("the oldest resolved group (old40) was kept")
	}
	if !ids["old41"] {
		t.Error("a newer resolved group (old41) was dropped before the oldest")
	}
	if got := testutil.ToFloat64(metrics.TriggerGroupsActive.WithLabelValues(testNS, testTrigger)); got != float64(episodes.MaxGroups-1) {
		t.Errorf("groups_active %v, want %d", got, episodes.MaxGroups-1)
	}
}

func TestResolvedGroupsOlderThanADayAreDropped(t *testing.T) {
	tr := readyTrigger(testTrigger)
	res := metav1.NewTime(epoch.Add(-25 * time.Hour))
	tr.Status.Groups = []v1alpha1.TriggerGroup{{ID: "stale", ResolvedAt: &res}}
	r := newRig(t, r0(t), tr)
	if rec := r.hook(alertPayload("fresh", "firing", "warning", "f1")); rec.Code != http.StatusAccepted {
		t.Fatalf("status %d", rec.Code)
	}
	groups := r.trigger().Status.Groups
	if len(groups) != 1 || groups[0].ID != episodes.GroupID("fresh") {
		t.Fatalf("groups %+v", groups)
	}
}

func TestMethodAndPath(t *testing.T) {
	r := newRig(t, r0(t), readyTrigger(testTrigger))
	req := httptest.NewRequest(http.MethodGet, "/hooks/apps/am", nil)
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status %d, want 405", rec.Code)
	}
	for _, p := range []string{"/hooks/apps", "/hooks/apps/", "/hooks//am", "/hooks/apps/am/extra", "/elsewhere/apps/am"} {
		req := httptest.NewRequest(http.MethodPost, p, bytes.NewReader(nil))
		rec := httptest.NewRecorder()
		r.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("POST %s: status %d, want 404", p, rec.Code)
		}
	}
}

func TestLabelValue(t *testing.T) {
	for in, want := range map[string]string{
		"critical":              "critical",
		"":                      "",
		"very bad!":             "very-bad",
		"-x-":                   "x",
		strings.Repeat("a", 80): strings.Repeat("a", 63),
		"Ünïcode":               "n-code",
	} {
		if got := LabelValue(in); got != want {
			t.Errorf("LabelValue(%q) = %q, want %q", in, got, want)
		}
	}
}
