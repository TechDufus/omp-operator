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

package agentapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/threads"
)

const (
	threadName   = "coder-k3x9fa"
	sitting      = "coder-k3x9fa-1"
	sittingToken = "tok-sitting"
	sessPrefix   = "/v1/sessions/team/" + sitting
	handoffSHA   = "0123456789012345678901234567890123456789"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func threadObj(mutate func(*v1alpha1.OmpThread)) *v1alpha1.OmpThread {
	th := &v1alpha1.OmpThread{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: threadName, UID: types.UID("uid-thread")},
		Spec: v1alpha1.OmpThreadSpec{
			AgentRef: v1alpha1.LocalObjectRef{Name: "coder"},
			Owner:    "token:dev",
			Title:    "fix the flake",
			Repo: v1alpha1.ThreadRepo{
				Name:          "app",
				RemoteURL:     "git@github.com:acme/app.git",
				BaseRef:       "main",
				BaseCommit:    "1111111111111111111111111111111111111111",
				HandoffCommit: handoffSHA,
				LocalBranch:   "feature/x",
			},
			Paths:  v1alpha1.ThreadPaths{Home: "/home/me", Workspace: "/home/me/app"},
			Author: v1alpha1.GitAuthor{Name: "Me", Email: "me@example.com"},
		},
	}
	if mutate != nil {
		mutate(th)
	}
	return th
}

func cloudAgentObj(mutate func(*v1alpha1.OmpAgent)) *v1alpha1.OmpAgent {
	return agentObj("coder", func(a *v1alpha1.OmpAgent) {
		a.Spec.Tools.Allow = []string{"*"}
		a.Spec.Cloud = &v1alpha1.CloudSpec{Git: []v1alpha1.CloudGitHost{{
			Host: "github.com",
			Auth: v1alpha1.GitAuth{TokenSecretRef: &v1alpha1.SecretKeyRef{Name: "gh", Key: "token"}},
			Push: true,
		}}}
		if mutate != nil {
			mutate(a)
		}
	})
}

// threadEnv is an env with a Running sitting of a thread of a cloud agent.
func threadEnv(t *testing.T, th *v1alpha1.OmpThread, agent *v1alpha1.OmpAgent) *env {
	t.Helper()
	sess := sessionObj(sitting, "coder", v1alpha1.SessionPhaseRunning, func(s *v1alpha1.OmpSession) {
		s.Spec.ThreadRef = &v1alpha1.LocalObjectRef{Name: threadName}
		s.Spec.ContinuityKey = "thread/team/" + threadName
	})
	e := newEnv(t, agent, th, sess, secret(sitting+"-token", sittingToken))
	e.srv.Threads = threads.NewLog(e.store)
	return e
}

func TestThreadRoutesAreForThreadSittingsOnly(t *testing.T) {
	e := newEnv(t, agentObj("hello", nil), sessionObj("a", "hello", v1alpha1.SessionPhaseRunning, nil), secret("a-token", tokA))
	e.srv.Threads = threads.NewLog(e.store)
	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/thread/source"},
		{http.MethodGet, "/thread/result"},
		{http.MethodPut, "/thread/result"},
		{http.MethodGet, "/thread/setup"},
		{http.MethodGet, "/thread/next?wait=1"},
		{http.MethodGet, "/thread/control"},
		{http.MethodPost, "/thread/turns/1/complete"},
		{http.MethodGet, "/toolchains/abc"},
		{http.MethodPut, "/toolchains/abc"},
	} {
		expect(t, e.do(rt.method, "/v1/sessions/team/a"+rt.path, tokA, nil), 404)
	}
}

func TestThreadPlan(t *testing.T) {
	th := threadObj(func(th *v1alpha1.OmpThread) { th.Spec.SecretFiles = []string{".env", "cfg/x.yaml"} })
	agent := cloudAgentObj(func(a *v1alpha1.OmpAgent) {
		a.Spec.Toolchain = &v1alpha1.ToolchainSpec{Disable: []string{"gitleaks"}}
		a.Spec.Workspace = &v1alpha1.WorkspaceSpec{Links: []v1alpha1.WorkspaceLink{{Path: "x", Target: "/y"}}}
	})
	e := threadEnv(t, th, agent)
	if err := e.store.PutThreadFile(ns, threadName, "source.bundle", strings.NewReader("bundle"), 0); err != nil {
		t.Fatal(err)
	}
	if err := e.store.PutThreadFile(ns, threadName, "setup.tar.gz", strings.NewReader("tar"), 0); err != nil {
		t.Fatal(err)
	}

	rec := e.do(http.MethodGet, sessPrefix+"/plan", sittingToken, nil)
	expect(t, rec, 200)
	plan := decode[Plan](t, rec)
	if plan.Workspace != nil || len(plan.Links) != 0 || plan.Toolchain != nil {
		t.Errorf("a thread plan has no agent workspace, links or toolchain: %+v", plan)
	}
	if plan.Continuity == nil || plan.Continuity.ID == "" {
		t.Errorf("continuity = %+v", plan.Continuity)
	}
	tp := plan.Thread
	if tp == nil {
		t.Fatal("no thread plan")
	}
	want := ThreadPlan{
		Name: threadName, Branch: "omp-cloud/" + threadName,
		FetchURL: "https://github.com/acme/app.git", RemoteURL: "git@github.com:acme/app.git",
		BaseRef: "main", BaseCommit: th.Spec.Repo.BaseCommit, HandoffCommit: handoffSHA,
		HasSource: true, HasResult: false, HasSetup: true,
		AuthDir: "/var/run/omp-operator/auth/cloud-git", AuthType: "token", Username: "x-access-token",
		SecretFiles: []ThreadSecretFile{{Key: "file-0", Path: ".env"}, {Key: "file-1", Path: "cfg/x.yaml"}},
		Author:      ThreadAuthor{Name: "Me", Email: "me@example.com"},
		Toolchain:   &ToolchainPlan{Disable: []string{"gitleaks"}},
	}
	if got, w := mustJSON(t, tp), mustJSON(t, want); got != w {
		t.Errorf("thread plan:\n got %s\nwant %s", got, w)
	}

	// A result bundle appears once a turn finished.
	if err := e.store.PutThreadFile(ns, threadName, "result.bundle", strings.NewReader("r"), 0); err != nil {
		t.Fatal(err)
	}
	if plan := decode[Plan](t, e.do(http.MethodGet, sessPrefix+"/plan", sittingToken, nil)); !plan.Thread.HasResult {
		t.Error("HasResult = false with a stored result bundle")
	}
}

func TestThreadPlanWithoutMatchingHost(t *testing.T) {
	th := threadObj(func(th *v1alpha1.OmpThread) { th.Spec.Repo.RemoteURL = "git@gitlab.com:acme/app.git" })
	e := threadEnv(t, th, cloudAgentObj(nil))
	tp := decode[Plan](t, e.do(http.MethodGet, sessPrefix+"/plan", sittingToken, nil)).Thread
	if tp.FetchURL != "" || tp.AuthDir != "" || tp.AuthType != "" {
		t.Errorf("an unmatched ssh remote has no fetch URL or auth: %+v", tp)
	}
	th = threadObj(func(th *v1alpha1.OmpThread) { th.Spec.Repo.RemoteURL = "https://gitlab.com/acme/app.git" })
	e = threadEnv(t, th, cloudAgentObj(nil))
	tp = decode[Plan](t, e.do(http.MethodGet, sessPrefix+"/plan", sittingToken, nil)).Thread
	if tp.FetchURL != "https://gitlab.com/acme/app.git" || tp.AuthDir != "" {
		t.Errorf("an unmatched https remote is fetched anonymously: %+v", tp)
	}
}

func TestThreadConfig(t *testing.T) {
	th := threadObj(func(th *v1alpha1.OmpThread) {
		th.Spec.Model = "openai-codex/gpt-5.5"
		th.Spec.Thinking = "high"
		th.Spec.SecretFiles = []string{".env"}
		th.Spec.SecretEnv = []string{"API_KEY"}
	})
	agent := cloudAgentObj(func(a *v1alpha1.OmpAgent) {
		a.Spec.Model.Gateway = &v1alpha1.GatewaySpec{URL: "http://gw", AllowedModels: []string{"anthropic/*", "openai-codex/*"}}
		a.Spec.Instructions = "be careful"
		a.Spec.Limits.SessionTimeout = metav1.Duration{Duration: 45 * time.Minute}
		a.Spec.Credentials = []v1alpha1.Credential{{
			Name: "tok", Description: "d", SecretRef: v1alpha1.SecretKeyRef{Name: "s", Key: "k"}, Env: "TOK",
		}}
	})
	e := threadEnv(t, th, agent)

	rec := e.do(http.MethodGet, sessPrefix, sittingToken, nil)
	expect(t, rec, 200)
	cfg := decode[SessionConfig](t, rec)

	if cfg.Model.Default != "openai-codex/gpt-5.5" {
		t.Errorf("model = %q, want the thread's", cfg.Model.Default)
	}
	if gw := cfg.Model.Gateway; gw == nil || strings.Join(gw.Providers, ",") != "openai-codex,anthropic" {
		t.Errorf("gateway = %+v, want the thread model's provider and every allowedModels provider", gw)
	}
	if cfg.Tools != nil {
		t.Errorf("tools = %v, want nil for allow [*]", cfg.Tools)
	}
	for _, a := range cfg.Actions {
		if a.Name == FinishToolName {
			t.Error("a thread sitting must not get finish")
		}
	}
	if len(cfg.Actions) != 1 || cfg.Actions[0].Name != "restart_it" {
		t.Errorf("actions = %+v", cfg.Actions)
	}
	if cfg.TimeoutSeconds != 45*60 || !cfg.Continuity {
		t.Errorf("timeout = %d continuity = %v", cfg.TimeoutSeconds, cfg.Continuity)
	}
	wantThread := ThreadConfig{
		Name: threadName, Branch: "omp-cloud/" + threadName, BaseCommit: th.Spec.Repo.BaseCommit,
		RepoName: "app", LocalBranch: "feature/x", Thinking: "high",
		TurnTimeoutSeconds: 45 * 60, IdleTimeoutSeconds: 600, MaxSittingSeconds: 4 * 3600, Push: true,
	}
	if cfg.Thread == nil || *cfg.Thread != wantThread {
		t.Errorf("thread config = %+v, want %+v", cfg.Thread, wantThread)
	}

	sp := cfg.SystemPrompt
	for _, want := range []string{
		"be careful", "## Available credentials", "## Actions you may call", "## Running in the cloud",
		"The repository app is checked out at /home/me/app on branch omp-cloud/" + threadName + ", based on feature/x.",
		"the branch is pushed for you after each turn",
		"never be printed or committed: .env.",
	} {
		if !strings.Contains(sp, want) {
			t.Errorf("system prompt lacks %q:\n%s", want, sp)
		}
	}
	if strings.Contains(sp, "finish") {
		t.Errorf("system prompt mentions finish:\n%s", sp)
	}

	byName := map[string]RedactSource{}
	for _, r := range cfg.Redact {
		byName[r.Name] = r
	}
	if r := byName["thread-env-API_KEY"]; r.Env != "API_KEY" || r.Lines {
		t.Errorf("secret env redaction = %+v", r)
	}
	if r := byName["thread-file-0"]; r.File != "/home/me/app/.env" || !r.Lines {
		t.Errorf("secret file redaction = %+v", r)
	}
	if _, ok := byName["tok"]; !ok {
		t.Error("the agent's own credentials are still redacted")
	}
}

func TestThreadConfigWithoutPushOrPaths(t *testing.T) {
	th := threadObj(func(th *v1alpha1.OmpThread) {
		th.Spec.Paths = v1alpha1.ThreadPaths{}
		th.Spec.Repo.LocalBranch = ""
	})
	agent := cloudAgentObj(func(a *v1alpha1.OmpAgent) { a.Spec.Cloud.Git[0].Push = false })
	e := threadEnv(t, th, agent)
	cfg := decode[SessionConfig](t, e.do(http.MethodGet, sessPrefix, sittingToken, nil))
	if cfg.Thread.Push {
		t.Error("push = true for a host without push")
	}
	if !strings.Contains(cfg.SystemPrompt, "checked out at /workspace on branch") ||
		!strings.Contains(cfg.SystemPrompt, "the user pulls the branch from the operator") {
		t.Errorf("system prompt:\n%s", cfg.SystemPrompt)
	}
}

func TestToolAllowEveryTool(t *testing.T) {
	tests := []struct {
		allow []string
		want  []string
	}{
		{nil, DefaultTools},
		{[]string{"*"}, nil},
		{[]string{"read", "bash"}, []string{"read", "bash"}},
	}
	for _, tt := range tests {
		spec := &v1alpha1.OmpAgentSpec{Tools: v1alpha1.ToolsSpec{Allow: tt.allow}}
		got := toolAllow(spec)
		if strings.Join(got, ",") != strings.Join(tt.want, ",") || (got == nil) != (tt.want == nil) {
			t.Errorf("toolAllow(%v) = %v, want %v", tt.allow, got, tt.want)
		}
	}
}

func TestThreadTurnLifecycle(t *testing.T) {
	e := threadEnv(t, threadObj(nil), cloudAgentObj(nil))
	log := e.srv.Threads
	if _, err := log.Enqueue(ns, threadName, "do the thing", "token:dev"); err != nil {
		t.Fatal(err)
	}

	// No turn is delivered twice, and nothing waiting is a 204.
	rec := e.do(http.MethodGet, sessPrefix+"/thread/next?wait=1", sittingToken, nil)
	expect(t, rec, 200)
	if got := decode[TurnAssignment](t, rec); got.ID != 1 || got.Text != "do the thing" {
		t.Errorf("assignment = %+v", got)
	}
	// The running turn is redelivered to the same sitting (a retried request), and the
	// sitting's control says go on.
	expect(t, e.do(http.MethodGet, sessPrefix+"/thread/next", sittingToken, nil), 200)
	if got := decode[map[string]bool](t, e.do(http.MethodGet, sessPrefix+"/thread/control", sittingToken, nil)); got["stop"] {
		t.Error("stop = true before a stop was requested")
	}
	if _, _, err := log.Stop(ns, threadName, "token:dev"); err != nil {
		t.Fatal(err)
	}
	if got := decode[map[string]bool](t, e.do(http.MethodGet, sessPrefix+"/thread/control", sittingToken, nil)); !got["stop"] {
		t.Error("stop = false after a stop was requested")
	}

	// Reports are validated.
	for _, bad := range []TurnComplete{
		{Outcome: "retryable"},
		{Outcome: OutcomeCompleted, Head: "not-a-commit"},
		{Outcome: OutcomeCompleted, FilesChanged: -1},
		{Outcome: OutcomeCompleted, Usage: Usage{Cost: strings.Repeat("9", 40)}},
	} {
		expect(t, e.do(http.MethodPost, sessPrefix+"/thread/turns/1/complete", sittingToken, bad), 400)
	}
	expect(t, e.do(http.MethodPost, sessPrefix+"/thread/turns/zero/complete", sittingToken, TurnComplete{Outcome: OutcomeCompleted}), 400)

	rec = e.do(http.MethodPost, sessPrefix+"/thread/turns/1/complete", sittingToken, TurnComplete{
		Outcome: OutcomeCompleted, Reply: "# Done\nall good", Head: handoffSHA, FilesChanged: 2, Insertions: 5, Deletions: 1,
		Pushed: true, Usage: Usage{InputTokens: 10, OutputTokens: 3, Cost: "0.01"},
	})
	expect(t, rec, 204)
	ts, err := log.Turns(ns, threadName)
	if err != nil || len(ts) != 1 {
		t.Fatalf("turns = %v, %v", ts, err)
	}
	got := ts[0]
	// A stop requested during the turn fails it even though the agent reported completion.
	if got.State != v1alpha1.TurnStateFailed || got.Error != "stopped by token:dev" {
		t.Errorf("turn = %s %q, want failed: stopped by token:dev", got.State, got.Error)
	}
	if got.Head != handoffSHA || got.FilesChanged != 2 || got.Insertions != 5 || got.Deletions != 1 || !got.Pushed ||
		got.Usage == nil || got.Usage.InputTokens != 10 || got.Title != "Done" {
		t.Errorf("turn result = %+v", got)
	}

	// A repeated report of a finished turn: 409.
	expect(t, e.do(http.MethodPost, sessPrefix+"/thread/turns/1/complete", sittingToken, TurnComplete{Outcome: OutcomeCompleted}), 409)
	// Nothing left: 204.
	expect(t, e.do(http.MethodGet, sessPrefix+"/thread/next", sittingToken, nil), 204)
	expect(t, e.do(http.MethodGet, sessPrefix+"/thread/next?wait=abc", sittingToken, nil), 400)
}

func TestThreadTurnCompleteTruncatesAndSignals(t *testing.T) {
	e := threadEnv(t, threadObj(nil), cloudAgentObj(nil))
	log := e.srv.Threads
	if _, err := log.Enqueue(ns, threadName, "go", "token:dev"); err != nil {
		t.Fatal(err)
	}
	expect(t, e.do(http.MethodGet, sessPrefix+"/thread/next", sittingToken, nil), 200)
	for len(log.Events()) > 0 {
		<-log.Events()
	}
	expect(t, e.do(http.MethodPost, sessPrefix+"/thread/turns/1/complete", sittingToken, TurnComplete{
		Outcome: OutcomeError, Error: strings.Repeat("e", 5000), Reply: strings.Repeat("r", 30000), PushError: strings.Repeat("p", 900),
	}), 204)
	ts, _ := log.Turns(ns, threadName)
	if ts[0].State != v1alpha1.TurnStateFailed || len(ts[0].Reply) != maxReplyChars ||
		len(ts[0].Error) != maxErrorChars || len(ts[0].PushError) != maxPushErrorChars {
		t.Errorf("turn = state %s reply %d error %d pushError %d", ts[0].State, len(ts[0].Reply), len(ts[0].Error), len(ts[0].PushError))
	}
	select {
	case ev := <-log.Events():
		if ev.Object.GetName() != threadName || ev.Object.GetNamespace() != ns {
			t.Errorf("event for %s/%s", ev.Object.GetNamespace(), ev.Object.GetName())
		}
	default:
		t.Error("completing a turn must wake the thread reconciler")
	}
}

func TestThreadFilesAndToolchains(t *testing.T) {
	e := threadEnv(t, threadObj(nil), cloudAgentObj(nil))
	if err := e.store.PutThreadFile(ns, threadName, "source.bundle", strings.NewReader("SRC"), 0); err != nil {
		t.Fatal(err)
	}
	if err := e.store.PutThreadFile(ns, threadName, "setup.tar.gz", strings.NewReader("SETUP"), 0); err != nil {
		t.Fatal(err)
	}
	if rec := e.do(http.MethodGet, sessPrefix+"/thread/source", sittingToken, nil); rec.Code != 200 || rec.Body.String() != "SRC" {
		t.Errorf("source = %d %q", rec.Code, rec.Body.String())
	}
	if rec := e.do(http.MethodGet, sessPrefix+"/thread/setup", sittingToken, nil); rec.Code != 200 || rec.Body.String() != "SETUP" {
		t.Errorf("setup = %d %q", rec.Code, rec.Body.String())
	}

	expect(t, e.do(http.MethodGet, sessPrefix+"/thread/result", sittingToken, nil), 404)
	expect(t, e.do(http.MethodPut, sessPrefix+"/thread/result", sittingToken, "BUNDLE"), 204)
	if rec := e.do(http.MethodGet, sessPrefix+"/thread/result", sittingToken, nil); rec.Code != 200 || rec.Body.String() != "BUNDLE" {
		t.Errorf("result = %d %q", rec.Code, rec.Body.String())
	}

	expect(t, e.do(http.MethodGet, sessPrefix+"/toolchains/abc123", sittingToken, nil), 404)
	expect(t, e.do(http.MethodPut, sessPrefix+"/toolchains/abc123", sittingToken, "TOOLS"), 204)
	if rec := e.do(http.MethodGet, sessPrefix+"/toolchains/abc123", sittingToken, nil); rec.Code != 200 || rec.Body.String() != "TOOLS" {
		t.Errorf("toolchain = %d %q", rec.Code, rec.Body.String())
	}
	// Another agent's cache is separate.
	if _, err := e.store.OpenCloudToolchain(ns, "other", "abc123"); err == nil {
		t.Error("the cache is shared across agents")
	}
	expect(t, e.do(http.MethodGet, sessPrefix+"/toolchains/..%2Fx", sittingToken, nil), 400)
}

func TestThreadSittingMissingThread(t *testing.T) {
	sess := sessionObj(sitting, "coder", v1alpha1.SessionPhaseRunning, func(s *v1alpha1.OmpSession) {
		s.Spec.ThreadRef = &v1alpha1.LocalObjectRef{Name: threadName}
	})
	e := newEnv(t, cloudAgentObj(nil), sess, secret(sitting+"-token", sittingToken))
	e.srv.Threads = threads.NewLog(e.store)
	expect(t, e.do(http.MethodGet, sessPrefix+"/plan", sittingToken, nil), 404)
	expect(t, e.do(http.MethodGet, sessPrefix, sittingToken, nil), 404)
	expect(t, e.do(http.MethodGet, sessPrefix+"/thread/next", sittingToken, nil), 404)
}

func TestPreparedOfThreadSittingKeepsAgentToolchain(t *testing.T) {
	agent := cloudAgentObj(func(a *v1alpha1.OmpAgent) { a.Spec.Toolchain = &v1alpha1.ToolchainSpec{} })
	e := threadEnv(t, threadObj(nil), agent)
	expect(t, e.do(http.MethodPost, sessPrefix+"/prepared", sittingToken, Prepared{WorkspaceRevision: handoffSHA, ToolchainHash: "somehash"}), 204)
	var a v1alpha1.OmpAgent
	if err := e.c.Get(t.Context(), types.NamespacedName{Namespace: ns, Name: "coder"}, &a); err != nil {
		t.Fatal(err)
	}
	if a.Status.Toolchain.Stale {
		t.Error("a thread sitting's repository toolchain marked the agent toolchain stale")
	}
	var s v1alpha1.OmpSession
	if err := e.c.Get(t.Context(), types.NamespacedName{Namespace: ns, Name: sitting}, &s); err != nil {
		t.Fatal(err)
	}
	if s.Status.WorkspaceRevision != handoffSHA {
		t.Errorf("workspaceRevision = %q", s.Status.WorkspaceRevision)
	}
}
