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

package notify

import (
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/threads"
)

const (
	dispatchNS        = "prod"
	dispatchAgent     = "argus"
	dispatchTrigger   = "alerts"
	dispatchAction    = "restart"
	dispatchFixed     = "fixed"
	dispatchRestarted = "restarted"
)

func dispatchSecret(name, key, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: dispatchNS, Name: name},
		Data:       map[string][]byte{key: []byte(value)},
	}
}

func dispatchRef(name string) ompv1alpha1.SecretKeyRef {
	return ompv1alpha1.SecretKeyRef{Name: name, Key: "v"}
}

// dispatchEnv has a slack sink for failed only, a discord sink with the default
// `on`, and a webhook sink for ok and fixed that signs with an HMAC.
type dispatchEnv struct {
	d                       *Dispatcher
	c                       client.Client
	slack, discord, webhook *captured
	agent                   *ompv1alpha1.OmpAgent
	now                     *time.Time
}

func newDispatchEnv(t *testing.T) *dispatchEnv {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := ompv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	slackSrv, slack := server(t, 200)
	discordSrv, discord := server(t, 200)
	hookSrv, webhook := server(t, 200)
	agent := &ompv1alpha1.OmpAgent{
		ObjectMeta: metav1.ObjectMeta{Namespace: dispatchNS, Name: dispatchAgent},
		Spec: ompv1alpha1.OmpAgentSpec{Notify: []ompv1alpha1.NotifySink{
			{Type: ompv1alpha1.SinkSlack, On: []ompv1alpha1.NotifyOn{ompv1alpha1.NotifyOnFailed},
				Slack: &ompv1alpha1.SlackSink{WebhookURLSecretRef: dispatchRef("slack-url")}},
			{Type: ompv1alpha1.SinkDiscord,
				Discord: &ompv1alpha1.DiscordSink{WebhookURLSecretRef: dispatchRef("discord-url")}},
			{Type: ompv1alpha1.SinkWebhook, On: []ompv1alpha1.NotifyOn{ompv1alpha1.NotifyOnOK, ompv1alpha1.NotifyOnFixed},
				Webhook: &ompv1alpha1.WebhookSink{URLSecretRef: dispatchRef("hook-url"), HMACSecretRef: new(dispatchRef("hook-key"))}},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&ompv1alpha1.OmpAgent{}).WithObjects(
		agent,
		dispatchSecret("slack-url", "v", slackSrv.URL+"\n"),
		dispatchSecret("discord-url", "v", discordSrv.URL),
		dispatchSecret("hook-url", "v", hookSrv.URL),
		dispatchSecret("hook-key", "v", "s3cret"),
	).Build()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	env := &dispatchEnv{c: c, slack: slack, discord: discord, webhook: webhook, agent: agent, now: &now}
	env.d = &Dispatcher{
		Client: c, APIReader: c, Sender: fastSender(),
		Options: &config.Options{DashboardURL: "https://omp.example.com/"},
		Now:     func() time.Time { return *env.now },
	}
	return env
}

func (e *dispatchEnv) counts() (slack, discord, webhook int) {
	return len(e.slack.bodies), len(e.discord.bodies), len(e.webhook.bodies)
}

func dispatchSession(phase ompv1alpha1.SessionPhase, verdict ompv1alpha1.Verdict) *ompv1alpha1.OmpSession {
	return &ompv1alpha1.OmpSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: dispatchNS, Name: "alerts-1"},
		Spec: ompv1alpha1.OmpSessionSpec{
			AgentRef:   ompv1alpha1.LocalObjectRef{Name: dispatchAgent},
			TriggerRef: &ompv1alpha1.LocalObjectRef{Name: dispatchTrigger},
		},
		Status: ompv1alpha1.OmpSessionStatus{
			Phase: phase, Verdict: verdict, Title: "disk full", Summary: "freed 4G",
			ProposedCommands: []string{"kubectl get pods"},
			Actions:          []ompv1alpha1.ActionRecord{{Action: dispatchAction, Target: "Deployment prod/web", OK: true, At: metav1.NewTime(time.Date(2026, 10, 18, 11, 0, 0, 0, time.UTC)), Result: dispatchRestarted}},
		},
	}
}

func TestSessionDoneFiltersSinksByOn(t *testing.T) {
	tests := []struct {
		name                   string
		phase                  ompv1alpha1.SessionPhase
		verdict                ompv1alpha1.Verdict
		slack, discord, webhok int
	}{
		{"ok", ompv1alpha1.SessionPhaseSucceeded, ompv1alpha1.VerdictOK, 0, 1, 1},
		{dispatchFixed, ompv1alpha1.SessionPhaseSucceeded, ompv1alpha1.VerdictFixed, 0, 1, 1},
		{"needs_owner", ompv1alpha1.SessionPhaseSucceeded, ompv1alpha1.VerdictNeedsOwner, 0, 1, 0},
		{"inconclusive", ompv1alpha1.SessionPhaseSucceeded, ompv1alpha1.VerdictInconclusive, 0, 1, 0},
		{"failed", ompv1alpha1.SessionPhaseFailed, "", 1, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newDispatchEnv(t)
			if err := e.d.SessionDone(t.Context(), e.agent, dispatchSession(tt.phase, tt.verdict)); err != nil {
				t.Fatal(err)
			}
			s, d, w := e.counts()
			if s != tt.slack || d != tt.discord || w != tt.webhok {
				t.Errorf("slack/discord/webhook = %d/%d/%d, want %d/%d/%d", s, d, w, tt.slack, tt.discord, tt.webhok)
			}
		})
	}
}

func TestSessionDoneMessage(t *testing.T) {
	e := newDispatchEnv(t)
	if err := e.d.SessionDone(t.Context(), e.agent, dispatchSession(ompv1alpha1.SessionPhaseSucceeded, ompv1alpha1.VerdictFixed)); err != nil {
		t.Fatal(err)
	}
	body := decode(t, e.webhook.bodies[0])
	if body["session"] != "prod/alerts-1" || body["agent"] != dispatchAgent || body["trigger"] != dispatchTrigger ||
		body["verdict"] != dispatchFixed || body["title"] != "disk full" || body["summary"] != "freed 4G" ||
		body["url"] != "https://omp.example.com/sessions/prod/alerts-1" {
		t.Errorf("payload = %v", body)
	}
	if cmds := body["proposedCommands"].([]any); len(cmds) != 1 || cmds[0] != "kubectl get pods" {
		t.Errorf("proposedCommands = %v", cmds)
	}
	acts := body["actions"].([]any)
	if len(acts) != 1 || acts[0].(map[string]any)["action"] != dispatchAction || acts[0].(map[string]any)["ok"] != true {
		t.Errorf("actions = %v", acts)
	}
	if e.webhook.header[0].Get(SignatureHeader) == "" {
		t.Error("webhook is not signed although hmacSecretRef is set")
	}
	if content := decode(t, e.discord.bodies[0])["content"].(string); !strings.Contains(content, "https://omp.example.com/sessions/prod/alerts-1") {
		t.Errorf("discord content lacks the dashboard link: %q", content)
	}
}

func TestSessionDoneFailedFallsBackToError(t *testing.T) {
	e := newDispatchEnv(t)
	s := dispatchSession(ompv1alpha1.SessionPhaseFailed, "")
	s.Status.Title, s.Status.Summary, s.Status.Error = "", "", "agent not found"
	e.d.Options.DashboardURL = ""
	if err := e.d.SessionDone(t.Context(), e.agent, s); err != nil {
		t.Fatal(err)
	}
	content := decode(t, e.slack.bodies[0])["text"].(string)
	if !strings.Contains(content, "agent not found") || !strings.Contains(content, "failed") || strings.Contains(content, "http") {
		t.Errorf("slack text = %q", content)
	}
}

func TestSessionDoneSinkFailuresAreIndependent(t *testing.T) {
	e := newDispatchEnv(t)
	// The discord sink's Secret disappears; the webhook must still be sent.
	if err := e.c.Delete(t.Context(), dispatchSecret("discord-url", "v", "")); err != nil {
		t.Fatal(err)
	}
	err := e.d.SessionDone(t.Context(), e.agent, dispatchSession(ompv1alpha1.SessionPhaseSucceeded, ompv1alpha1.VerdictOK))
	if err == nil || !strings.Contains(err.Error(), "discord-url") {
		t.Fatalf("err = %v, want the discord failure", err)
	}
	if _, d, w := e.counts(); d != 0 || w != 1 {
		t.Errorf("discord/webhook = %d/%d, want 0/1", d, w)
	}
	// A key missing from the Secret fails the same way, without leaking any value.
	if err := e.c.Update(t.Context(), dispatchSecret("hook-url", "other", "https://secret.example")); err != nil {
		t.Fatal(err)
	}
	err = e.d.SessionDone(t.Context(), e.agent, dispatchSession(ompv1alpha1.SessionPhaseSucceeded, ompv1alpha1.VerdictOK))
	if err == nil || strings.Contains(err.Error(), "secret.example") || !strings.Contains(err.Error(), `key "v"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestNoticeOncePerInterval(t *testing.T) {
	e := newDispatchEnv(t)
	every := 6 * time.Hour
	send := func() bool {
		t.Helper()
		sent, err := e.d.Notice(t.Context(), e.agent, "auth", every, "argus: model login missing", "log in again")
		if err != nil {
			t.Fatal(err)
		}
		return sent
	}
	if !send() {
		t.Fatal("first notice suppressed")
	}
	// Notices go to every sink regardless of `on`.
	if s, d, w := e.counts(); s != 1 || d != 1 || w != 1 {
		t.Fatalf("slack/discord/webhook = %d/%d/%d, want 1/1/1", s, d, w)
	}
	if got := decode(t, e.webhook.bodies[0]); got["agent"] != dispatchAgent || got["title"] != "argus: model login missing" ||
		got["summary"] != "log in again" || got["verdict"] != "" || got["url"] != "https://omp.example.com/agents/prod/argus" {
		t.Errorf("notice payload = %v", got)
	}

	// The claim is stored on the agent, and the caller's copy sees it too.
	var cur ompv1alpha1.OmpAgent
	if err := e.c.Get(t.Context(), types.NamespacedName{Namespace: dispatchNS, Name: dispatchAgent}, &cur); err != nil {
		t.Fatal(err)
	}
	if at, ok := cur.Status.Notices["auth"]; !ok || !at.Time.Equal(*e.now) {
		t.Fatalf("status.notices = %v", cur.Status.Notices)
	}
	if _, ok := e.agent.Status.Notices["auth"]; !ok {
		t.Error("caller's agent copy does not carry the notice time")
	}

	*e.now = e.now.Add(every - time.Minute)
	if send() {
		t.Error("notice repeated within the interval")
	}
	// A different key is independent.
	if sent, err := e.d.Notice(t.Context(), e.agent, "cap:alerts:2026-10-18", 24*time.Hour, "cap", "limit reached"); err != nil || !sent {
		t.Fatalf("other key: sent=%v err=%v", sent, err)
	}
	*e.now = e.now.Add(2 * time.Minute)
	if !send() {
		t.Error("notice not sent after the interval")
	}
	if s, _, _ := e.counts(); s != 3 {
		t.Errorf("slack received %d notices, want 3", s)
	}
}

func TestNoticeConcurrentCallersSendOnce(t *testing.T) {
	e := newDispatchEnv(t)
	var wg sync.WaitGroup
	results := make(chan bool, 8)
	for range 8 {
		wg.Go(func() {
			// Each caller holds its own stale copy of the agent.
			a := e.agent.DeepCopy()
			sent, err := e.d.Notice(t.Context(), a, "toolchain", time.Hour, "t", "x")
			if err != nil {
				t.Error(err)
			}
			results <- sent
		})
	}
	wg.Wait()
	close(results)
	n := 0
	for sent := range results {
		if sent {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d callers sent the notice, want 1", n)
	}
	if s, _, _ := e.counts(); s != 1 {
		t.Errorf("slack received %d notices, want 1", s)
	}
}

func TestNoticeWithoutSinksClaimsNothing(t *testing.T) {
	e := newDispatchEnv(t)
	var cur ompv1alpha1.OmpAgent
	if err := e.c.Get(t.Context(), types.NamespacedName{Namespace: dispatchNS, Name: dispatchAgent}, &cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Notify = nil
	if err := e.c.Update(t.Context(), &cur); err != nil {
		t.Fatal(err)
	}
	sent, err := e.d.Notice(t.Context(), e.agent, "auth", time.Hour, "t", "x")
	if sent || err != nil {
		t.Fatalf("sent=%v err=%v", sent, err)
	}
	if err := e.c.Get(t.Context(), types.NamespacedName{Namespace: dispatchNS, Name: dispatchAgent}, &cur); err != nil || len(cur.Status.Notices) != 0 {
		t.Fatalf("notices = %v err=%v", cur.Status.Notices, err)
	}
}

func dispatchThread() *ompv1alpha1.OmpThread {
	return &ompv1alpha1.OmpThread{
		ObjectMeta: metav1.ObjectMeta{Namespace: dispatchNS, Name: "argus-k3x9fa"},
		Spec: ompv1alpha1.OmpThreadSpec{
			AgentRef: ompv1alpha1.LocalObjectRef{Name: dispatchAgent},
			Title:    "fix the flake",
		},
	}
}

func TestTurnDoneFiltersSinksByResult(t *testing.T) {
	tests := []struct {
		name                   string
		turn                   threads.Turn
		slack, discord, webhok int
	}{
		{"done without changes is ok", threads.Turn{ID: 1, State: ompv1alpha1.TurnStateDone}, 0, 1, 1},
		{"done with changes is fixed", threads.Turn{ID: 1, State: ompv1alpha1.TurnStateDone, FilesChanged: 3}, 0, 1, 1},
		{"failed", threads.Turn{ID: 1, State: ompv1alpha1.TurnStateFailed, Error: "boom"}, 1, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newDispatchEnv(t)
			if err := e.d.TurnDone(t.Context(), e.agent, dispatchThread(), tt.turn); err != nil {
				t.Fatal(err)
			}
			s, d, w := e.counts()
			if s != tt.slack || d != tt.discord || w != tt.webhok {
				t.Errorf("slack/discord/webhook = %d/%d/%d, want %d/%d/%d", s, d, w, tt.slack, tt.discord, tt.webhok)
			}
		})
	}
}

func TestTurnDoneMessage(t *testing.T) {
	e := newDispatchEnv(t)
	reply := strings.Repeat("r", 900)
	turn := threads.Turn{ID: 2, State: ompv1alpha1.TurnStateDone, FilesChanged: 1, Title: "Added Greet", Reply: reply}
	if err := e.d.TurnDone(t.Context(), e.agent, dispatchThread(), turn); err != nil {
		t.Fatal(err)
	}
	body := decode(t, e.webhook.bodies[0])
	if body["session"] != "prod/argus-k3x9fa" || body["agent"] != dispatchAgent || body["verdict"] != dispatchFixed ||
		body["title"] != "fix the flake: Added Greet" || body["summary"] != reply[:800] ||
		body["url"] != "https://omp.example.com/threads/prod/argus-k3x9fa" {
		t.Errorf("payload = %v", body)
	}

	// Without a title the turn number names it; a failed turn reports its error.
	e = newDispatchEnv(t)
	turn = threads.Turn{ID: 3, State: ompv1alpha1.TurnStateFailed, Reply: "partial", Error: "stopped by token:dev"}
	if err := e.d.TurnDone(t.Context(), e.agent, dispatchThread(), turn); err != nil {
		t.Fatal(err)
	}
	raw := string(e.slack.bodies[0])
	if !strings.Contains(raw, "fix the flake: turn 3") || !strings.Contains(raw, "stopped by token:dev") {
		t.Errorf("slack payload = %s", raw)
	}
}
