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

package dashboard

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/store"
)

const (
	transcriptPath = "/sessions/prod/ok-1/transcript"
	fakeCallback   = "http://x/auth/callback"
	nsProd         = "prod"
	alertName      = "KubePodCrashLooping/prod"
	nopePath       = "/nope"
	opsGroup       = "ops"
	agentName      = "hello"
	restartAction  = "restart-app"
	groupID        = "0123456789abcdef"
	operatorNS     = "omp-system"
	basicRoot      = "basic:root"
	agentPath      = "/agents/prod/hello"
	okSessionPath  = "/sessions/prod/ok-1"
	janeEmail      = "jane@example.com"
	deadIdP        = "http://127.0.0.1:1"
)

var testNow = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func hashOf(t *testing.T, pass string) []byte {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func ago(d time.Duration) metav1.Time { return metav1.NewTime(testNow.Add(-d)) }

const (
	hostileTitle   = `<script>alert("t")</script>`
	hostileSummary = `<img src=x onerror=alert("s")> & "quotes"`
	hostileReport  = `<b>report</b></pre><script>alert("r")</script>`
	secretName     = "sekret-store-name"
	secretKey      = "sekret-store-key"
)

func fixtures() []client.Object {
	ready := []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: ago(time.Hour)}}
	prodAgent := &v1alpha1.OmpAgent{
		ObjectMeta: metav1.ObjectMeta{Namespace: nsProd, Name: agentName},
		Spec: v1alpha1.OmpAgentSpec{
			Model: v1alpha1.ModelSpec{
				Default: "anthropic/claude-sonnet-5-5",
				Gateway: &v1alpha1.GatewaySpec{URL: "http://user:gwpass@omp-auth.omp-system.svc:4000"},
			},
			Tools: v1alpha1.ToolsSpec{Allow: []string{"read", "bash"}},
			Skills: []v1alpha1.SkillSource{
				{Git: &v1alpha1.GitSkillSource{URL: "https://x-access-token:ghp_TOPSECRET@github.com/acme/skills.git", Ref: "main", Path: "skills"}},
				{ConfigMap: &v1alpha1.LocalObjectRef{Name: "extra-skills"}},
			},
			Workspace: &v1alpha1.WorkspaceSpec{Git: &v1alpha1.GitWorkspaceSource{URL: "git@github.com:acme/ws.git"}},
			Credentials: []v1alpha1.Credential{
				{Name: "demo", Description: "demo credential", SecretRef: v1alpha1.SecretKeyRef{Name: secretName, Key: secretKey}, Env: "DEMO_TOKEN"},
				{Name: "kube", Description: "a file", SecretRef: v1alpha1.SecretKeyRef{Name: secretName, Key: secretKey}, File: "/etc/creds/kube"},
			},
			MCPServers: []v1alpha1.MCPServer{{Name: "docs", URL: "https://mcp.example.com/?token=QUERYSECRET"}},
			Actions:    []v1alpha1.LocalObjectRef{{Name: restartAction}},
			Limits:     v1alpha1.LimitsSpec{EventSessionsPerDay: 12, NonCriticalSessionsPerDay: 8},
			Notify:     []v1alpha1.NotifySink{{Type: v1alpha1.SinkType("ntfy")}},
		},
		Status: v1alpha1.OmpAgentStatus{Conditions: ready},
	}
	otherAgent := &v1alpha1.OmpAgent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "hidden"},
		Spec:       v1alpha1.OmpAgentSpec{Model: v1alpha1.ModelSpec{Default: "openai/hidden-model"}},
		Status:     v1alpha1.OmpAgentStatus{Conditions: ready},
	}
	on := true
	hook := &v1alpha1.OmpTrigger{
		ObjectMeta: metav1.ObjectMeta{Namespace: nsProd, Name: "am"},
		Spec: v1alpha1.OmpTriggerSpec{
			AgentRef: v1alpha1.LocalObjectRef{Name: agentName}, Enabled: &on, Prompt: "p",
			Session: v1alpha1.TriggerSessionSpec{Resume: v1alpha1.ResumeEpisode},
			Webhook: &v1alpha1.WebhookSpec{
				Adapter: v1alpha1.AdapterAlertmanager,
				Auth:    v1alpha1.WebhookAuth{Bearer: &v1alpha1.BearerAuth{SecretRef: v1alpha1.SecretKeyRef{Name: secretName, Key: secretKey}}},
			},
		},
		Status: v1alpha1.OmpTriggerStatus{
			Type: v1alpha1.TriggerWebhook, HookPath: "/hooks/prod/am", Conditions: ready,
			Groups: []v1alpha1.TriggerGroup{{
				ID: groupID, Name: alertName, Severity: "critical",
				EpisodeStart: ago(time.Hour), LastNotified: ago(time.Minute), TriagedFingerprints: []string{"a", "b"}, LastSession: "am-20261018-110000-abcd",
			}},
		},
	}
	cron := &v1alpha1.OmpTrigger{
		ObjectMeta: metav1.ObjectMeta{Namespace: nsProd, Name: "nightly"},
		Spec: v1alpha1.OmpTriggerSpec{
			AgentRef: v1alpha1.LocalObjectRef{Name: agentName}, Prompt: "p",
			Schedule: &v1alpha1.ScheduleSpec{Cron: "30 7 * * *", TimeZone: "America/Chicago"},
		},
		Status: v1alpha1.OmpTriggerStatus{Type: v1alpha1.TriggerSchedule, Conditions: ready, NextFireAt: &metav1.Time{Time: testNow.Add(3 * time.Hour)}},
	}
	gen := &v1alpha1.OmpTrigger{
		ObjectMeta: metav1.ObjectMeta{Namespace: nsProd, Name: "gen"},
		Spec: v1alpha1.OmpTriggerSpec{
			AgentRef: v1alpha1.LocalObjectRef{Name: agentName}, Prompt: "p",
			Webhook: &v1alpha1.WebhookSpec{
				Adapter: v1alpha1.AdapterGeneric,
				Auth:    v1alpha1.WebhookAuth{HMAC: &v1alpha1.HMACAuth{SecretRef: v1alpha1.SecretKeyRef{Name: secretName, Key: secretKey}}},
				Mapping: &v1alpha1.WebhookMapping{Key: "payload.id"},
			},
		},
	}
	sessions := []client.Object{
		&v1alpha1.OmpSession{
			ObjectMeta: metav1.ObjectMeta{Namespace: nsProd, Name: "ok-1", UID: "uid-ok-1", CreationTimestamp: ago(time.Hour)},
			Spec: v1alpha1.OmpSessionSpec{
				AgentRef: v1alpha1.LocalObjectRef{Name: agentName}, TriggerRef: &v1alpha1.LocalObjectRef{Name: "am"}, Prompt: "SECRET-PROMPT-TEXT",
				Event: &v1alpha1.SessionEvent{Key: "k", GroupID: groupID, Name: alertName, Severity: "critical"},
			},
			Status: v1alpha1.OmpSessionStatus{
				Phase: v1alpha1.SessionPhaseSucceeded, Verdict: v1alpha1.VerdictOK, Outcome: v1alpha1.OutcomeCompleted, Attempts: 1,
				Title: hostileTitle, Summary: hostileSummary, ProposedCommands: []string{"kubectl get pods <ns>"},
				Error: `<i>err</i>`, StartTime: &metav1.Time{Time: testNow.Add(-59 * time.Minute)}, CompletionTime: &metav1.Time{Time: testNow.Add(-50 * time.Minute)},
				Actions:    []v1alpha1.ActionRecord{{Action: restartAction, Target: "<t>", OK: true, At: ago(55 * time.Minute), Result: "restarted"}},
				Usage:      &v1alpha1.SessionUsage{InputTokens: 10, OutputTokens: 20, Cost: "0.0123"},
				Continuity: &v1alpha1.ContinuityStatus{Restored: true, Stored: true},
			},
		},
		&v1alpha1.OmpSession{
			ObjectMeta: metav1.ObjectMeta{Namespace: nsProd, Name: "run-1", UID: "uid-run-1", CreationTimestamp: ago(time.Minute)},
			Spec: v1alpha1.OmpSessionSpec{
				AgentRef: v1alpha1.LocalObjectRef{Name: agentName}, Prompt: "p",
				Event: &v1alpha1.SessionEvent{Severity: "warning"},
			},
			Status: v1alpha1.OmpSessionStatus{Phase: v1alpha1.SessionPhaseRunning, Attempts: 1},
		},
		&v1alpha1.OmpSession{
			ObjectMeta: metav1.ObjectMeta{Namespace: nsProd, Name: "failed-1", UID: "uid-failed-1", CreationTimestamp: ago(2 * time.Hour)},
			Spec:       v1alpha1.OmpSessionSpec{AgentRef: v1alpha1.LocalObjectRef{Name: agentName}, Prompt: "p"},
			Status:     v1alpha1.OmpSessionStatus{Phase: v1alpha1.SessionPhaseFailed, Error: "boom"},
		},
		&v1alpha1.OmpSession{
			ObjectMeta: metav1.ObjectMeta{Namespace: nsProd, Name: "old-1", UID: "uid-old-1", CreationTimestamp: ago(48 * time.Hour)},
			Spec:       v1alpha1.OmpSessionSpec{AgentRef: v1alpha1.LocalObjectRef{Name: agentName}, Prompt: "p", Event: &v1alpha1.SessionEvent{Severity: "info"}},
			Status:     v1alpha1.OmpSessionStatus{Phase: v1alpha1.SessionPhaseSucceeded, Verdict: v1alpha1.VerdictFixed},
		},
		&v1alpha1.OmpSession{
			ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "hidden-session", UID: "uid-hidden", CreationTimestamp: ago(time.Minute)},
			Spec:       v1alpha1.OmpSessionSpec{AgentRef: v1alpha1.LocalObjectRef{Name: "hidden"}, Prompt: "p"},
			Status:     v1alpha1.OmpSessionStatus{Phase: v1alpha1.SessionPhaseSucceeded, Verdict: v1alpha1.VerdictOK, Title: "HIDDEN-TITLE"},
		},
	}
	return append([]client.Object{prodAgent, otherAgent, hook, cron, gen}, sessions...)
}

type fixture struct {
	srv *Server
	h   http.Handler
	st  *store.Store
}

func newFixture(t *testing.T, access *Access) *fixture {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(fixtures()...).Build()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteTranscript("uid-ok-1", strings.NewReader("<html>fake</html>")); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteTranscript("uid-hidden", strings.NewReader("<html>hidden</html>")); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteReport("uid-ok-1", hostileReport); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvents("uid-ok-1", []store.Event{
		{At: testNow.Add(-58 * time.Minute), Kind: "tool", Text: `bash <b>ls</b>`},
		{At: testNow.Add(-57 * time.Minute), Kind: "notice", Text: "hello"},
	}); err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Client: c, APIReader: c, Store: st,
		Options: &config.Options{OperatorNamespace: operatorNS, HooksPublicURL: "https://hooks.example.com/"},
		Access:  access, Key: bytes.Repeat([]byte{7}, 32),
		Now: func() time.Time { return testNow },
	}
	return &fixture{srv: srv, h: srv.Handler(), st: st}
}

func basicAccess(t *testing.T, rules ...Rule) *Access {
	t.Helper()
	return &Access{
		Basic: &BasicConfig{HtpasswdFile: "unused"},
		htpasswd: map[string][]byte{
			"alice": hashOf(t, "alice-pass"),
			"root":  hashOf(t, "root-pass"),
		},
		Rules: rules,
	}
}

func defaultRules() []Rule {
	return []Rule{
		{Users: []string{"basic:alice"}, Namespaces: []string{nsProd}, Role: RoleViewer},
		{Users: []string{basicRoot}, Namespaces: []string{"*"}, Role: RoleViewer},
	}
}

func do(h http.Handler, method, target string, mods ...func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for _, m := range mods {
		m(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func asBasic(user, pass string) func(*http.Request) {
	return func(r *http.Request) { r.SetBasicAuth(user, pass) }
}

func asAlice() func(*http.Request) { return asBasic("alice", "alice-pass") }

func TestUnconfiguredAnswers503ExceptHealthz(t *testing.T) {
	f := newFixture(t, &Access{})
	for _, p := range []string{"/", agentPath, okSessionPath, transcriptPath, loginPath, nopePath} {
		w := do(f.h, http.MethodGet, p)
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "dashboard access not configured") {
			t.Errorf("%s: code %d body %q, want 503 dashboard access not configured", p, w.Code, w.Body.String())
		}
	}
	w := do(f.h, http.MethodGet, "/healthz")
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "ok" {
		t.Errorf("/healthz: code %d body %q", w.Code, w.Body.String())
	}
	// A nil Access behaves the same.
	f2 := newFixture(t, nil)
	if w := do(f2.h, http.MethodGet, "/"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("nil access: code %d, want 503", w.Code)
	}
}

func TestBasicAuth(t *testing.T) {
	f := newFixture(t, basicAccess(t, defaultRules()...))
	tests := []struct {
		name string
		mods []func(*http.Request)
		want int
	}{
		{"missing", nil, http.StatusUnauthorized},
		{"wrong password", []func(*http.Request){asBasic("alice", "nope")}, http.StatusUnauthorized},
		{"unknown user", []func(*http.Request){asBasic("mallory", "alice-pass")}, http.StatusUnauthorized},
		{"empty password", []func(*http.Request){asBasic("alice", "")}, http.StatusUnauthorized},
		{"good", []func(*http.Request){asAlice()}, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := do(f.h, http.MethodGet, "/", tc.mods...)
			if w.Code != tc.want {
				t.Fatalf("code %d, want %d", w.Code, tc.want)
			}
			if tc.want == http.StatusUnauthorized && w.Header().Get("WWW-Authenticate") != `Basic realm="omp-operator"` {
				t.Errorf("WWW-Authenticate = %q", w.Header().Get("WWW-Authenticate"))
			}
		})
	}
}

func TestNamespaceRules(t *testing.T) {
	f := newFixture(t, basicAccess(t, defaultRules()...))

	// alice is limited to prod.
	idx := do(f.h, http.MethodGet, "/", asAlice())
	body := idx.Body.String()
	for _, want := range []string{"prod/hello", "claude-sonnet-5-5"} {
		if !strings.Contains(body, want) {
			t.Errorf("index missing %q", want)
		}
	}
	for _, hidden := range []string{"other/hidden", "hidden-model", "HIDDEN-TITLE", "hidden-session"} {
		if strings.Contains(body, hidden) {
			t.Errorf("index leaks %q", hidden)
		}
	}
	for _, p := range []string{
		"/agents/other/hidden", "/sessions/other/hidden-session", "/sessions/other/hidden-session/transcript",
		"/agents/nowhere/x", "/agents/prod/missing", "/triggers/prod/missing", "/sessions/prod/missing", "/sessions/prod/missing/transcript",
		nopePath,
	} {
		if w := do(f.h, http.MethodGet, p, asAlice()); w.Code != http.StatusNotFound {
			t.Errorf("alice %s: code %d, want 404", p, w.Code)
		}
	}
	for _, p := range []string{agentPath, "/triggers/prod/am", okSessionPath, transcriptPath} {
		if w := do(f.h, http.MethodGet, p, asAlice()); w.Code != http.StatusOK {
			t.Errorf("alice %s: code %d, want 200", p, w.Code)
		}
	}

	// root has "*": every namespace.
	rootIdx := do(f.h, http.MethodGet, "/", asBasic("root", "root-pass")).Body.String()
	for _, want := range []string{"prod/hello", "other/hidden", "other/hidden-session"} {
		if !strings.Contains(rootIdx, want) {
			t.Errorf("root index missing %q", want)
		}
	}
	if w := do(f.h, http.MethodGet, "/sessions/other/hidden-session", asBasic("root", "root-pass")); w.Code != http.StatusOK {
		t.Errorf("root hidden session: %d", w.Code)
	}

	// The operator's own namespace filter still applies to "*".
	f.srv.Options.AgentNamespaces = []string{nsProd}
	if w := do(f.h, http.MethodGet, "/agents/other/hidden", asBasic("root", "root-pass")); w.Code != http.StatusNotFound {
		t.Errorf("root outside --agent-namespaces: %d, want 404", w.Code)
	}
	if strings.Contains(do(f.h, http.MethodGet, "/", asBasic("root", "root-pass")).Body.String(), "other/hidden") {
		t.Error("index lists a namespace outside --agent-namespaces")
	}
	f.srv.Options.AgentNamespaces = nil

	// A user without any rule sees nothing.
	norule := basicAccess(t, Rule{Users: []string{basicRoot}, Namespaces: []string{"*"}})
	f2 := newFixture(t, norule)
	w := do(f2.h, http.MethodGet, "/", asAlice())
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "prod/hello") || !strings.Contains(w.Body.String(), "No namespaces are granted") {
		t.Errorf("rule-less user: code %d body leaks or lacks notice", w.Code)
	}
	if w := do(f2.h, http.MethodGet, agentPath, asAlice()); w.Code != http.StatusNotFound {
		t.Errorf("rule-less user agent page: %d", w.Code)
	}
}

func TestPagesContent(t *testing.T) {
	f := newFixture(t, basicAccess(t, defaultRules()...))
	get := func(p string) string {
		t.Helper()
		w := do(f.h, http.MethodGet, p, asAlice())
		if w.Code != http.StatusOK {
			t.Fatalf("%s: code %d", p, w.Code)
		}
		return w.Body.String()
	}

	idx := get("/")
	for _, want := range []string{
		"ok 1 &middot; fixed 0 &middot; needs owner 0 &middot; inconclusive 0 &middot; failed 1 &middot; active 1", // 24 h window
		"events 2/12", // ok-1 (critical) and run-1 (warning); old-1 is outside 24 h
		"non-critical 1/8",
		"prod/am", "prod/nightly", "prod/gen",
	} {
		if !strings.Contains(idx, want) {
			t.Errorf("index missing %q", want)
		}
	}
	if strings.Index(idx, "run-1") > strings.Index(idx, "ok-1") || strings.Index(idx, "ok-1") > strings.Index(idx, "failed-1") {
		t.Error("sessions are not newest first")
	}

	agent := get(agentPath)
	for _, want := range []string{
		"anthropic/claude-sonnet-5-5", "read, bash", "git https://github.com/acme/skills.git @main (skills)", "configMap extra-skills",
		"git github.com:acme/ws.git", "DEMO_TOKEN", "demo credential", "file /etc/creds/kube", "docs", restartAction,
		"omp-auth.omp-system.svc:4000", "(managed token)", "events 2/12",
	} {
		if !strings.Contains(agent, want) {
			t.Errorf("agent page missing %q", want)
		}
	}
	for _, leak := range []string{"ghp_TOPSECRET", "gwpass", "QUERYSECRET", secretName, secretKey, "SECRET-PROMPT-TEXT"} {
		for _, page := range []string{idx, agent, get(okSessionPath), get("/triggers/prod/am")} {
			if strings.Contains(page, leak) {
				t.Errorf("page leaks %q", leak)
			}
		}
	}

	am := get("/triggers/prod/am")
	for _, want := range []string{
		"https://hooks.example.com/hooks/prod/am", "alertmanager", "bearer",
		"!(alert.labels.?alertname.orValue(&#39;&#39;) in [&#39;Watchdog&#39;,&#39;InfoInhibitor&#39;])", // computed alertFilter default
		"<code>true</code>", groupID, alertName, "am-20261018-110000-abcd", "episode gap", "30m0s", "2 (1 when critical)",
	} {
		if !strings.Contains(am, want) {
			t.Errorf("trigger page missing %q", want)
		}
	}
	gen := get("/triggers/prod/gen")
	for _, want := range []string{"payload.id", "[payload]", "sha256(event JSON)", "hmac", "&#39;firing&#39;"} {
		if !strings.Contains(gen, want) {
			t.Errorf("generic trigger page missing %q", want)
		}
	}
	cron := get("/triggers/prod/nightly")
	for _, want := range []string{"30 7 * * *", "America/Chicago", "2026-10-18 15:00:00 UTC", "in 3h00m"} {
		if !strings.Contains(cron, want) {
			t.Errorf("schedule trigger page missing %q", want)
		}
	}
	if strings.Contains(cron, "Hook URL") {
		t.Error("schedule trigger shows a hook URL")
	}

	sess := get(okSessionPath)
	for _, want := range []string{
		"restarted", "0.0123", "restored: true, stored: true", transcriptPath, "kubectl get pods &lt;ns&gt;",
		"KubePodCrashLooping/prod (critical)", "hello", "bash &lt;b&gt;ls&lt;/b&gt;",
	} {
		if !strings.Contains(sess, want) {
			t.Errorf("session page missing %q", want)
		}
	}
	if strings.Contains(sess, `http-equiv="refresh"`) {
		t.Error("finished session refreshes")
	}
	if run := get("/sessions/prod/run-1"); !strings.Contains(run, `<meta http-equiv="refresh" content="10">`) {
		t.Error("running session does not refresh")
	}
	if failed := get("/sessions/prod/failed-1"); !strings.Contains(failed, "boom") || strings.Contains(failed, "transcript") {
		t.Error("failed session page wrong")
	}
}

func TestHTMLEscaping(t *testing.T) {
	f := newFixture(t, basicAccess(t, defaultRules()...))
	for _, p := range []string{"/", okSessionPath} {
		body := do(f.h, http.MethodGet, p, asAlice()).Body.String()
		for _, raw := range []string{"<script>alert", "<img src=x", "<b>report</b>", "<i>err</i>", "<b>ls</b>", "<t>"} {
			if strings.Contains(body, raw) {
				t.Errorf("%s: unescaped %q in output", p, raw)
			}
		}
	}
	body := do(f.h, http.MethodGet, okSessionPath, asAlice()).Body.String()
	for _, want := range []string{
		"&lt;script&gt;alert(&#34;t&#34;)&lt;/script&gt;",                     // title
		"&lt;img src=x onerror=alert(&#34;s&#34;)&gt; &amp; &#34;quotes&#34;", // summary
		"&lt;b&gt;report&lt;/b&gt;&lt;/pre&gt;&lt;script&gt;",                 // report in <pre>
	} {
		if !strings.Contains(body, want) {
			t.Errorf("session page missing escaped %q", want)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	f := newFixture(t, basicAccess(t, defaultRules()...))
	const csp = "default-src 'none'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	common := func(t *testing.T, name string, w *httptest.ResponseRecorder, wantCSP string) {
		t.Helper()
		h := w.Header()
		if got := h.Get("Content-Security-Policy"); got != wantCSP {
			t.Errorf("%s: CSP = %q, want %q", name, got, wantCSP)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: headers %v", name, h)
		}
	}
	for _, p := range []string{"/", agentPath, "/triggers/prod/am", okSessionPath, nopePath} {
		common(t, p, do(f.h, http.MethodGet, p, asAlice()), csp)
	}
	common(t, "healthz", do(f.h, http.MethodGet, "/healthz"), csp)
	common(t, "401", do(f.h, http.MethodGet, "/"), csp)
	common(t, "503", do(newFixture(t, &Access{}).h, http.MethodGet, "/"), csp)
}

func TestTranscript(t *testing.T) {
	f := newFixture(t, basicAccess(t, defaultRules()...))
	w := do(f.h, http.MethodGet, transcriptPath, asAlice())
	if w.Code != http.StatusOK || w.Body.String() != "<html>fake</html>" {
		t.Fatalf("code %d body %q", w.Code, w.Body.String())
	}
	h := w.Header()
	if h.Get("Content-Security-Policy") != "sandbox allow-scripts allow-popups" {
		t.Errorf("CSP = %q", h.Get("Content-Security-Policy"))
	}
	if !strings.HasPrefix(h.Get("Content-Type"), "text/html") || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("Referrer-Policy") != "no-referrer" || h.Get("Cache-Control") != "no-store" {
		t.Errorf("headers %v", h)
	}
	head := do(f.h, http.MethodHead, transcriptPath, asAlice())
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Security-Policy") != "sandbox allow-scripts allow-popups" {
		t.Errorf("HEAD: code %d body %d", head.Code, head.Body.Len())
	}
	// A session with no stored transcript has none to serve.
	if w := do(f.h, http.MethodGet, "/sessions/prod/run-1/transcript", asAlice()); w.Code != http.StatusNotFound {
		t.Errorf("no transcript: code %d", w.Code)
	}
}

func TestMethods(t *testing.T) {
	f := newFixture(t, basicAccess(t, defaultRules()...))
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		w := do(f.h, m, "/", asAlice())
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: code %d allow %q", m, w.Code, w.Header().Get("Allow"))
		}
	}
	if w := do(f.h, http.MethodHead, "/", asAlice()); w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Errorf("HEAD /: code %d, content type %q", w.Code, w.Header().Get("Content-Type"))
	}
}

func TestEnsureSessionKey(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	ctx := context.Background()
	key, err := EnsureSessionKey(ctx, c, c, operatorNS)
	if err != nil || len(key) != 32 {
		t.Fatalf("key %d bytes, err %v", len(key), err)
	}
	var sec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: operatorNS, Name: "omp-operator-dashboard-session"}, &sec); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sec.Data["key"], key) {
		t.Error("stored key differs from returned key")
	}
	again, err := EnsureSessionKey(ctx, c, c, operatorNS)
	if err != nil || !bytes.Equal(again, key) {
		t.Errorf("second call: err %v, same key %v", err, bytes.Equal(again, key))
	}
	// An existing Secret without a key is an error, not silently replaced.
	bad := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: operatorNS, Name: "omp-operator-dashboard-session"},
	}).Build()
	if _, err := EnsureSessionKey(ctx, bad, bad, operatorNS); err == nil {
		t.Error("empty existing secret: want error")
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAccess(t *testing.T) {
	dir := t.TempDir()
	secret := writeFile(t, dir, "secret", "s3cret\n")
	good := hashOf(t, "pw")
	htp := writeFile(t, dir, "htpasswd", "# comment\n\nalice:"+string(good)+"\n")
	plain := writeFile(t, dir, "plain", "alice:plaintext\n")
	md5 := writeFile(t, dir, "md5", "alice:$apr1$abcdefgh$0123456789012345678901\n")
	dup := writeFile(t, dir, "dup", "alice:"+string(good)+"\nalice:"+string(good)+"\n")

	t.Run("full", func(t *testing.T) {
		p := writeFile(t, dir, "full.yaml", `
oidc:
  issuerURL: https://idp.example.com
  clientID: omp
  clientSecretFile: `+secret+`
  redirectURL: https://omp.example.com/auth/callback
basic:
  htpasswdFile: `+htp+`
rules:
  - users: [alice@example.com, "basic:alice"]
    groups: [ops]
    namespaces: ["*"]
`)
		a, err := LoadAccess(p)
		if err != nil {
			t.Fatal(err)
		}
		if a.oidcClientSecret != "s3cret" || a.OIDC.UsernameClaim != defaultUsernameClaim || a.OIDC.GroupsClaim != defaultGroupsClaim ||
			strings.Join(a.OIDC.Scopes, " ") != "openid email profile groups" || a.Rules[0].Role != RoleViewer {
			t.Errorf("defaults not applied: %+v %+v", a.OIDC, a.Rules)
		}
		if !checkBasic(a.htpasswd, "alice", "pw") || checkBasic(a.htpasswd, "alice", "bad") || checkBasic(a.htpasswd, "bob", "pw") {
			t.Error("htpasswd check wrong")
		}
		if !a.configured() {
			t.Error("not configured")
		}
	})
	t.Run("missing file is unconfigured", func(t *testing.T) {
		a, err := LoadAccess(filepath.Join(dir, "absent.yaml"))
		if err != nil || a.configured() {
			t.Errorf("err %v configured %v", err, a.configured())
		}
	})
	t.Run("empty file is unconfigured", func(t *testing.T) {
		a, err := LoadAccess(writeFile(t, dir, "empty.yaml", ""))
		if err != nil || a.configured() {
			t.Errorf("err %v", err)
		}
	})
	bad := map[string]string{
		"unknown field":        "bogus: 1\n",
		"oidc without issuer":  "oidc:\n  clientID: a\n  clientSecretFile: " + secret + "\n  redirectURL: https://x/auth/callback\n",
		"oidc missing secret":  "oidc:\n  issuerURL: https://i\n  clientID: a\n  clientSecretFile: " + filepath.Join(dir, "nope") + "\n  redirectURL: https://x\n",
		"plaintext htpasswd":   "basic:\n  htpasswdFile: " + plain + "\n",
		"apr1 htpasswd":        "basic:\n  htpasswdFile: " + md5 + "\n",
		"duplicate user":       "basic:\n  htpasswdFile: " + dup + "\n",
		"rule without subject": "rules:\n  - namespaces: [a]\n",
		"rule without ns":      "rules:\n  - users: [a]\n",
		"unknown role":         "rules:\n  - users: [a]\n    namespaces: [a]\n    role: admin\n",
	}
	for name, content := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadAccess(writeFile(t, dir, strings.ReplaceAll(name, " ", "-")+".yaml", content)); err == nil {
				t.Error("want error")
			}
		})
	}
}

func TestCookieForgery(t *testing.T) {
	a := &Access{OIDC: &OIDCConfig{IssuerURL: deadIdP, ClientID: "c", RedirectURL: fakeCallback, UsernameClaim: defaultUsernameClaim, GroupsClaim: defaultGroupsClaim},
		Rules: []Rule{{Users: []string{janeEmail}, Namespaces: []string{nsProd}}}}
	f := newFixture(t, a)
	valid, err := f.srv.sign("session", sessionClaims{U: janeEmail, Exp: testNow.Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	expired, _ := f.srv.sign("session", sessionClaims{U: janeEmail, Exp: testNow.Add(-time.Second).Unix()})
	asState, _ := f.srv.sign("state", sessionClaims{U: janeEmail, Exp: testNow.Add(time.Hour).Unix()})
	other := &Server{Key: bytes.Repeat([]byte{9}, 32)}
	wrongKey, _ := other.sign("session", sessionClaims{U: janeEmail, Exp: testNow.Add(time.Hour).Unix()})
	tamper := valid[:len(valid)-2] + "AA"

	withCookie := func(v string) func(*http.Request) {
		return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: sessionCookie, Value: v}) }
	}
	if w := do(f.h, http.MethodGet, "/", withCookie(valid)); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), janeEmail) {
		t.Fatalf("valid cookie: %d", w.Code)
	}
	for name, v := range map[string]string{"expired": expired, "state cookie replayed": asState, "wrong key": wrongKey, "tampered": tamper, "garbage": "x.y", "empty": ""} {
		w := do(f.h, http.MethodGet, "/", withCookie(v))
		if w.Code != http.StatusFound || w.Header().Get("Location") != loginPath {
			t.Errorf("%s: code %d location %q, want redirect to /auth/login", name, w.Code, w.Header().Get("Location"))
		}
	}
	// A deep link survives the login redirect; open redirects do not.
	w := do(f.h, http.MethodGet, okSessionPath)
	if loc := w.Header().Get("Location"); loc != "/auth/login?next=%2Fsessions%2Fprod%2Fok-1" {
		t.Errorf("deep link location = %q", loc)
	}
	for _, n := range []string{"//evil.example.com", "/\\evil", "https://evil.example.com", "/auth/logout", "relative"} {
		if got := safeNext(n); got != "/" {
			t.Errorf("safeNext(%q) = %q", n, got)
		}
	}
	// Empty signing key never validates anything.
	f.srv.Key = nil
	if w := do(f.h, http.MethodGet, "/", withCookie(valid)); w.Code != http.StatusFound {
		t.Errorf("empty key accepted a cookie: %d", w.Code)
	}
}

func TestOIDCLazyDiscovery(t *testing.T) {
	a := &Access{OIDC: &OIDCConfig{IssuerURL: deadIdP, ClientID: "c", RedirectURL: fakeCallback, Scopes: []string{"openid"}, UsernameClaim: defaultUsernameClaim, GroupsClaim: defaultGroupsClaim}}
	f := newFixture(t, a) // the IdP is down; the server still serves
	if w := do(f.h, http.MethodGet, "/healthz"); w.Code != http.StatusOK {
		t.Errorf("healthz: %d", w.Code)
	}
	if w := do(f.h, http.MethodGet, loginPath); w.Code != http.StatusServiceUnavailable {
		t.Errorf("login with IdP down: %d, want 503", w.Code)
	}
	if w := do(f.h, http.MethodGet, "/"); w.Code != http.StatusFound {
		t.Errorf("unauthenticated: %d, want 302", w.Code)
	}
}

func TestBadBasicWithOIDCIsChallenged(t *testing.T) {
	a := basicAccess(t, defaultRules()...)
	a.OIDC = &OIDCConfig{IssuerURL: deadIdP, ClientID: "c", RedirectURL: fakeCallback, UsernameClaim: defaultUsernameClaim, GroupsClaim: defaultGroupsClaim}
	f := newFixture(t, a)
	if w := do(f.h, http.MethodGet, "/"); w.Code != http.StatusFound {
		t.Errorf("no credentials with OIDC: %d, want redirect", w.Code)
	}
	if w := do(f.h, http.MethodGet, "/", asBasic("alice", "bad")); w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("bad basic: %d", w.Code)
	}
	if w := do(f.h, http.MethodGet, "/", asAlice()); w.Code != http.StatusOK {
		t.Errorf("good basic with OIDC enabled: %d", w.Code)
	}
}
