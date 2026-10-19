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
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/store"
	"github.com/TechDufus/omp-operator/internal/threads"
)

const (
	hostileThreadTitle = `<script>alert("thread")</script>`
	hostileReply       = `done </pre><img src=x onerror=alert("r")>`
	threadPath         = "/threads/prod/hello-abc123"
	workingThreadPath  = "/threads/prod/hello-work01"
	secretToken        = "0123456789abcdef0123456789abcdef-secret"
)

func tokenSum(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func threadObjects() []client.Object {
	spec := func(title string) v1alpha1.OmpThreadSpec {
		return v1alpha1.OmpThreadSpec{
			AgentRef: v1alpha1.LocalObjectRef{Name: agentName}, Owner: "token:dev", Title: title, Model: "anthropic/claude-sonnet-5-5",
			Repo: v1alpha1.ThreadRepo{
				Name: "app", RemoteURL: "https://x-access-token:ghp_TOPSECRET@github.com/acme/app.git", BaseCommit: strings.Repeat("a", 40),
				HandoffCommit: strings.Repeat("b", 40), LocalBranch: "main",
			},
			SecretFiles: []string{".env"},
		}
	}
	return []client.Object{
		&v1alpha1.OmpThread{
			ObjectMeta: metav1.ObjectMeta{Namespace: nsProd, Name: "hello-abc123", CreationTimestamp: ago(time.Hour)},
			Spec:       spec(hostileThreadTitle),
			Status: v1alpha1.OmpThreadStatus{
				Phase: v1alpha1.ThreadPhaseIdle, Turns: 1,
				Latest: &v1alpha1.ThreadTurnSummary{ID: 1, State: v1alpha1.TurnStateDone},
				Result: &v1alpha1.ThreadResult{
					Branch: "omp-cloud/hello-abc123", Head: strings.Repeat("c", 40), FilesChanged: 3, Insertions: 10, Deletions: 2,
					Pushed: true, UpdatedAt: ago(time.Minute),
				},
			},
		},
		&v1alpha1.OmpThread{
			ObjectMeta: metav1.ObjectMeta{Namespace: nsProd, Name: "hello-work01", CreationTimestamp: ago(time.Minute)},
			Spec:       spec("busy"),
			Status:     v1alpha1.OmpThreadStatus{Phase: v1alpha1.ThreadPhaseWorking, Turns: 1, Queued: 1},
		},
		&v1alpha1.OmpThread{
			ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "hidden-t", CreationTimestamp: ago(time.Minute)},
			Spec:       spec("HIDDEN-THREAD"),
		},
		&v1alpha1.OmpSession{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: nsProd, Name: "hello-abc123-1", UID: "uid-sitting-1", CreationTimestamp: ago(50 * time.Minute),
				Labels: map[string]string{v1alpha1.LabelThread: "hello-abc123"},
			},
			Spec: v1alpha1.OmpSessionSpec{
				AgentRef: v1alpha1.LocalObjectRef{Name: agentName}, Prompt: "p", ThreadRef: &v1alpha1.LocalObjectRef{Name: "hello-abc123"},
			},
			Status: v1alpha1.OmpSessionStatus{Phase: v1alpha1.SessionPhaseSucceeded},
		},
	}
}

func threadFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, basicAccess(t, defaultRules()...))
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(append(fixtures(), threadObjects()...)...).Build()
	f.srv.Client, f.srv.APIReader = c, c
	log := threads.NewLog(f.st)
	log.Now = func() time.Time { return testNow }
	f.srv.Threads = log

	turn, err := log.Enqueue(nsProd, "hello-abc123", "fix <b>it</b>", "token:dev")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Next(t.Context(), nsProd, "hello-abc123", "hello-abc123-1", 0); err != nil {
		t.Fatal(err)
	}
	if err := log.Complete(nsProd, "hello-abc123", "hello-abc123-1", turn.ID, threads.Result{
		Outcome: threads.OutcomeCompleted, Reply: hostileReply, Usage: threads.Usage{InputTokens: 7, OutputTokens: 9}, Head: strings.Repeat("c", 40),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Enqueue(nsProd, "hello-work01", "work", "token:dev"); err != nil {
		t.Fatal(err)
	}
	if err := f.st.AppendEvents("uid-sitting-1", []store.Event{{At: testNow.Add(-49 * time.Minute), Kind: "assistant", Text: `<i>hi</i>`}}); err != nil {
		t.Fatal(err)
	}
	f.h = f.srv.Handler()
	return f
}

func TestThreadPage(t *testing.T) {
	f := threadFixture(t)
	get := func(p string, mods ...func(*http.Request)) *http.Response {
		t.Helper()
		if len(mods) == 0 {
			mods = []func(*http.Request){asAlice()}
		}
		return do(f.h, http.MethodGet, p, mods...).Result()
	}
	body := func(p string) string {
		t.Helper()
		w := do(f.h, http.MethodGet, p, asAlice())
		if w.Code != http.StatusOK {
			t.Fatalf("%s: code %d", p, w.Code)
		}
		return w.Body.String()
	}

	idle := body(threadPath)
	for _, want := range []string{
		"prod/hello-abc123", "token:dev", "app (https://github.com/acme/app.git)", "omp-cloud/hello-abc123", "/cloud pull hello-abc123",
		"&#43;10 \u22122 in 3 files", "pushed", "hello-abc123-1", "input 7, output 9",
		"&lt;script&gt;alert(&#34;thread&#34;)&lt;/script&gt;" /* title in the heading */, "assistant", ".env",
	} {
		if !strings.Contains(idle, want) {
			t.Errorf("thread page missing %q", want)
		}
	}
	for _, raw := range []string{"<script>alert", "<img src=x", "<b>it</b>", "<i>hi</i>", "ghp_TOPSECRET"} {
		if strings.Contains(idle, raw) {
			t.Errorf("thread page contains %q unescaped or leaked", raw)
		}
	}
	for _, want := range []string{"fix &lt;b&gt;it&lt;/b&gt;", "&lt;/pre&gt;&lt;img src=x onerror=alert(&#34;r&#34;)&gt;", "&lt;i&gt;hi&lt;/i&gt;"} {
		if !strings.Contains(idle, want) {
			t.Errorf("thread page missing escaped %q", want)
		}
	}
	if strings.Contains(idle, `http-equiv="refresh"`) {
		t.Error("idle thread refreshes")
	}
	if !strings.Contains(body(workingThreadPath), `<meta http-equiv="refresh" content="10">`) {
		t.Error("working thread does not refresh")
	}

	if code := get("/threads/other/hidden-t").StatusCode; code != http.StatusNotFound {
		t.Errorf("thread outside the user's namespaces: %d, want 404", code)
	}
	if code := get("/threads/prod/missing").StatusCode; code != http.StatusNotFound {
		t.Errorf("missing thread: %d, want 404", code)
	}
	if code := get(threadPath, asBasic("root", "root-pass")).StatusCode; code != http.StatusOK {
		t.Errorf("root: %d", code)
	}
}

func TestThreadsOnIndexAndSessionPage(t *testing.T) {
	f := threadFixture(t)
	idx := do(f.h, http.MethodGet, "/", asAlice()).Body.String()
	for _, want := range []string{"<h2>Threads</h2>", threadPath, workingThreadPath, "Working"} {
		if !strings.Contains(idx, want) {
			t.Errorf("index missing %q", want)
		}
	}
	if strings.Contains(idx, "HIDDEN-THREAD") || strings.Contains(idx, "hidden-t") {
		t.Error("index lists a thread of a namespace the user cannot see")
	}
	if strings.Index(idx, "hello-work01") > strings.Index(idx, "hello-abc123\"") {
		t.Error("threads are not newest first")
	}
	root := do(f.h, http.MethodGet, "/", asBasic("root", "root-pass")).Body.String()
	if !strings.Contains(root, "other/hidden-t") {
		t.Error("root index misses the thread of another namespace")
	}

	sess := do(f.h, http.MethodGet, "/sessions/prod/hello-abc123-1", asAlice()).Body.String()
	if !strings.Contains(sess, `href="`+threadPath+`"`) {
		t.Error("sitting page does not link to its thread")
	}
	if strings.Contains(do(f.h, http.MethodGet, okSessionPath, asAlice()).Body.String(), "<dt>Thread</dt>") {
		t.Error("a session without threadRef links to a thread")
	}
}

func TestAPITokensLoad(t *testing.T) {
	dir := t.TempDir()
	tokFile := writeFile(t, dir, "tok", secretToken+"\n")
	short := writeFile(t, dir, "short", "abc\n")
	write := func(name, content string) string { return writeFile(t, dir, name, content) }

	a, err := LoadAccess(write("ok.yaml", `
apiTokens:
  - name: file-token
    tokenFile: `+tokFile+`
  - name: hashed
    sha256: `+tokenSum("hashed-token")+`
rules:
  - users: ["token:hashed"]
    namespaces: [prod]
    role: operator
`))
	if err != nil {
		t.Fatal(err)
	}
	if !a.TokensConfigured() || a.configured() {
		t.Errorf("TokensConfigured %v, dashboard configured %v; want true, false", a.TokensConfigured(), a.configured())
	}
	if (&Access{}).TokensConfigured() {
		t.Error("an empty access file has tokens")
	}

	tests := []struct {
		name, token, want string
		ok                bool
	}{
		{"file token", secretToken, "token:file-token", true},
		{"hashed token", "hashed-token", "token:hashed", true},
		{"unknown", "nope", "", false},
		{"empty", "", "", false},
		{"hash itself is not a token", tokenSum("hashed-token"), "", false},
		{"prefix", secretToken[:20], "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := a.TokenPrincipal(tc.token)
			if got != tc.want || ok != tc.ok {
				t.Errorf("TokenPrincipal = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
	if _, ok := (*Access)(nil).TokenPrincipal(secretToken); ok {
		t.Error("nil Access authenticates a token")
	}

	bad := map[string]string{
		"bad name":             "apiTokens:\n  - name: Bad_Name\n    sha256: " + tokenSum("x") + "\n",
		"duplicate name":       "apiTokens:\n  - {name: a, sha256: " + tokenSum("x") + "}\n  - {name: a, sha256: " + tokenSum("y") + "}\n",
		"neither":              "apiTokens:\n  - name: a\n",
		"both":                 "apiTokens:\n  - {name: a, sha256: " + tokenSum("x") + ", tokenFile: " + tokFile + "}\n",
		"short hash":           "apiTokens:\n  - {name: a, sha256: abcd}\n",
		"non-hex hash":         "apiTokens:\n  - {name: a, sha256: " + strings.Repeat("z", 64) + "}\n",
		"short token file":     "apiTokens:\n  - {name: a, tokenFile: " + short + "}\n",
		"missing token file":   "apiTokens:\n  - {name: a, tokenFile: " + filepath.Join(dir, "absent") + "}\n",
		"role neither of both": "rules:\n  - {users: [a], namespaces: [a], role: owner}\n",
	}
	for name, content := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadAccess(write(strings.ReplaceAll(name, " ", "-")+".yaml", content)); err == nil {
				t.Error("want error")
			}
		})
	}
}

func TestAllows(t *testing.T) {
	a := &Access{Rules: []Rule{
		{Users: []string{"token:ops"}, Namespaces: []string{"*"}, Role: RoleOperator},
		{Users: []string{"token:viewer"}, Namespaces: []string{nsProd}, Role: RoleViewer},
		{Groups: []string{opsGroup}, Namespaces: []string{nsProd}, Role: RoleOperator},
		{Users: []string{"token:both"}, Namespaces: []string{nsProd}, Role: RoleViewer},
		{Users: []string{"token:both"}, Namespaces: []string{nsProd}, Role: RoleOperator},
	}}
	tests := []struct {
		user   string
		groups []string
		ns     string
		role   string
		want   bool
	}{
		{"token:ops", nil, "anywhere", RoleOperator, true},
		{"token:ops", nil, "anywhere", RoleViewer, true},
		{"token:viewer", nil, nsProd, RoleViewer, true},
		{"token:viewer", nil, nsProd, RoleOperator, false},
		{"token:viewer", nil, "other", RoleViewer, false},
		{"token:nobody", nil, nsProd, RoleViewer, false},
		{"token:nobody", []string{opsGroup}, nsProd, RoleOperator, true},
		{"token:nobody", []string{"dev"}, nsProd, RoleViewer, false},
		{"token:both", nil, nsProd, RoleOperator, true},
	}
	for _, tc := range tests {
		if got := a.Allows(tc.user, tc.groups, tc.ns, tc.role); got != tc.want {
			t.Errorf("Allows(%s, %v, %s, %s) = %v, want %v", tc.user, tc.groups, tc.ns, tc.role, got, tc.want)
		}
	}
}

func TestTokenPrincipalCannotSignInToTheDashboard(t *testing.T) {
	// A token rule grants nothing on the dashboard: pages authenticate basic and OIDC users only.
	a := basicAccess(t, Rule{Users: []string{"token:dev"}, Namespaces: []string{"*"}, Role: RoleOperator})
	a.tokens = []tokenHash{{name: "dev", sum: sha256.Sum256([]byte(secretToken))}}
	f := newFixture(t, a)
	w := do(f.h, http.MethodGet, "/", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secretToken) })
	if w.Code != http.StatusUnauthorized {
		t.Errorf("bearer token on a dashboard page: %d, want 401", w.Code)
	}
}
