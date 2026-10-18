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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/store"
)

const (
	ns           = "team"
	tokSync      = "tok-sync"
	oldHash      = "oldhash"
	fluxNS       = "flux-system"
	literalValue = "literal-value"
	hash1        = "hash1"
	planPath     = "/v1/sessions/team/a/plan"
	modelID      = "anthropic/claude-sonnet-5-5"
	skillsRepo   = "skills"
	tokA         = "tok-a"
)

type fakeRunner struct {
	defs  []ToolDef
	calls []string
	args  []string
}

func (f *fakeRunner) ToolDefs(context.Context, *v1alpha1.OmpAgent) ([]ToolDef, []v1alpha1.OmpAction, error) {
	return f.defs, nil, nil
}

func (f *fakeRunner) Call(_ context.Context, _ *v1alpha1.OmpAgent, _ *v1alpha1.OmpSession, tool string, args json.RawMessage) (ActionResult, error) {
	if tool == "nope" {
		return ActionResult{}, ErrUnknownTool
	}
	f.calls = append(f.calls, tool)
	f.args = append(f.args, string(args))
	return ActionResult{Text: "did " + tool, IsError: false}, nil
}

type env struct {
	t      *testing.T
	c      client.Client
	srv    *Server
	h      http.Handler
	store  *store.Store
	runner *fakeRunner
}

func newEnv(t *testing.T, objs ...client.Object) *env {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := sourcev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.OmpSession{}, &v1alpha1.OmpAgent{}).
		WithObjects(objs...).Build()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{defs: []ToolDef{
		{Name: "restart_it", Description: "restarts things", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "finish", Description: "ends the session", Parameters: json.RawMessage(`{"type":"object"}`)},
	}}
	srv := &Server{
		Client: c, APIReader: c, Store: st, Runner: runner,
		Options: &config.Options{OperatorNamespace: "omp-system"},
		Now:     func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) },
	}
	return &env{t: t, c: c, srv: srv, h: srv.Handler(), store: st, runner: runner}
}

func secret(name, token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Data:       map[string][]byte{TokenKey: []byte(token)},
	}
}

func agentObj(name string, mutate func(*v1alpha1.OmpAgent)) *v1alpha1.OmpAgent {
	a := &v1alpha1.OmpAgent{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("agent-uid-" + name), Generation: 1},
		Spec:       v1alpha1.OmpAgentSpec{Model: v1alpha1.ModelSpec{Default: modelID}},
	}
	if mutate != nil {
		mutate(a)
	}
	return a
}

func sessionObj(name, agent string, phase v1alpha1.SessionPhase, mutate func(*v1alpha1.OmpSession)) *v1alpha1.OmpSession {
	s := &v1alpha1.OmpSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("uid-" + name)},
		Spec:       v1alpha1.OmpSessionSpec{AgentRef: v1alpha1.LocalObjectRef{Name: agent}, Prompt: "triage it"},
		Status:     v1alpha1.OmpSessionStatus{Phase: phase},
	}
	if mutate != nil {
		mutate(s)
	}
	return s
}

func (e *env) do(method, path, token string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *env) session() v1alpha1.OmpSession {
	e.t.Helper()
	var s v1alpha1.OmpSession
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "a"}, &s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) agent() v1alpha1.OmpAgent {
	e.t.Helper()
	var a v1alpha1.OmpAgent
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "hello"}, &a); err != nil {
		e.t.Fatal(err)
	}
	return a
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func expect(t *testing.T, rec *httptest.ResponseRecorder, code int) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status = %d, want %d; body %q", rec.Code, code, rec.Body.String())
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t,
		agentObj("hello", nil),
		sessionObj("a", "hello", v1alpha1.SessionPhaseRunning, nil),
		sessionObj("b", "hello", v1alpha1.SessionPhaseRunning, nil),
		sessionObj("pending", "hello", v1alpha1.SessionPhasePending, nil),
		secret("a-token", "tok-a"),
		secret("b-token", "tok-b"),
		secret("pending-token", "tok-pending"),
		secret("hello-toolsync-token", tokSync),
	)
	tests := []struct {
		name, method, path, token string
		want                      int
	}{
		{"own token", http.MethodGet, planPath, tokA, 200},
		{"no token", http.MethodGet, planPath, "", 401},
		{"another session's token", http.MethodGet, planPath, "tok-b", 401},
		{"toolsync token on session routes", http.MethodGet, planPath, tokSync, 401},
		{"toolsync token on session config", http.MethodGet, "/v1/sessions/team/a", tokSync, 401},
		{"session token on agent plan", http.MethodGet, "/v1/agents/team/hello/plan", tokA, 401},
		{"session token on agent toolchain", http.MethodGet, "/v1/agents/team/hello/toolchain", tokA, 401},
		{"session token on agent upload", http.MethodPut, "/v1/agents/team/hello/toolchain/abc", tokA, 401},
		{"toolsync token on agent routes", http.MethodGet, "/v1/agents/team/hello/plan", tokSync, 200},
		{"toolsync token for another agent", http.MethodGet, "/v1/agents/team/other/plan", tokSync, 401},
		{"non-running session", http.MethodGet, "/v1/sessions/team/pending/plan", "tok-pending", 409},
		{"non-running session wrong token", http.MethodGet, "/v1/sessions/team/pending/plan", tokA, 401},
		{"unknown session", http.MethodGet, "/v1/sessions/team/ghost/plan", tokA, 401},
		{"non-bearer scheme", http.MethodGet, planPath, "", 401},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.do(tc.method, tc.path, tc.token, nil)
			expect(t, rec, tc.want)
			if tc.want == 401 && strings.Contains(rec.Body.String(), "tok-") {
				t.Fatalf("error body leaks a token: %q", rec.Body.String())
			}
		})
	}
}

func TestAuthRejectsBasicScheme(t *testing.T) {
	e := newEnv(t, agentObj("hello", nil), sessionObj("a", "hello", v1alpha1.SessionPhaseRunning, nil), secret("a-token", "tok-a"))
	req := httptest.NewRequest(http.MethodGet, planPath, nil)
	req.Header.Set("Authorization", "Basic tok-a")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	expect(t, rec, 401)
}

func TestAuthNamespaceNotAllowed(t *testing.T) {
	e := newEnv(t, agentObj("hello", nil), sessionObj("a", "hello", v1alpha1.SessionPhaseRunning, nil), secret("a-token", "tok-a"))
	e.srv.Options.AgentNamespaces = []string{"elsewhere"}
	expect(t, e.do(http.MethodGet, planPath, "tok-a", nil), 401)
}

func TestTokenCachedAfterFirstRead(t *testing.T) {
	e := newEnv(t, agentObj("hello", nil), sessionObj("a", "hello", v1alpha1.SessionPhaseRunning, nil), secret("a-token", "tok-a"))
	expect(t, e.do(http.MethodGet, planPath, "tok-a", nil), 200)
	if err := e.c.Delete(context.Background(), secret("a-token", "")); err != nil {
		t.Fatal(err)
	}
	expect(t, e.do(http.MethodGet, planPath, "tok-a", nil), 200)
	expect(t, e.do(http.MethodGet, planPath, "bad", nil), 401)
}

func TestTokenRefreshedAfterInterval(t *testing.T) {
	e := newEnv(t, agentObj("hello", nil), sessionObj("a", "hello", v1alpha1.SessionPhaseRunning, nil), secret("a-token", "tok-a"))
	now := e.srv.now()
	e.srv.Now = func() time.Time { return now }
	expect(t, e.do(http.MethodGet, planPath, "tok-a", nil), 200)

	sec := secret("a-token", "tok-new")
	if err := e.c.Update(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	// Within the interval a mismatch does not re-read.
	expect(t, e.do(http.MethodGet, planPath, "tok-new", nil), 401)
	now = now.Add(tokenRefreshInterval + time.Second)
	expect(t, e.do(http.MethodGet, planPath, "tok-new", nil), 200)
	expect(t, e.do(http.MethodGet, planPath, "tok-a", nil), 401)
}

func TestPreparedSetsRevisionAndStale(t *testing.T) {
	withToolchain := func(a *v1alpha1.OmpAgent) {
		a.Spec.Toolchain = &v1alpha1.ToolchainSpec{}
		a.Status.Toolchain.Hash = oldHash
	}
	e := newEnv(t,
		agentObj("hello", withToolchain),
		sessionObj("a", "hello", v1alpha1.SessionPhaseRunning, nil),
		secret("a-token", "tok-a"),
	)
	// Same hash: not stale.
	expect(t, e.do("POST", "/v1/sessions/team/a/prepared", "tok-a", Prepared{WorkspaceRevision: "main@sha1:abc", ToolchainHash: oldHash}), 204)
	if a := e.agent(); a.Status.Toolchain.Stale {
		t.Fatal("same hash marked stale")
	}
	if s := e.session(); s.Status.WorkspaceRevision != "main@sha1:abc" {
		t.Fatalf("workspaceRevision = %q", s.Status.WorkspaceRevision)
	}
	// Changed hash: stale.
	expect(t, e.do("POST", "/v1/sessions/team/a/prepared", "tok-a", Prepared{WorkspaceRevision: "main@sha1:def", ToolchainHash: "newhash"}), 204)
	if a := e.agent(); !a.Status.Toolchain.Stale || a.Status.Toolchain.Hash != oldHash {
		t.Fatalf("toolchain status = %+v, want stale with old hash kept", a.Status.Toolchain)
	}
	if s := e.session(); s.Status.WorkspaceRevision != "main@sha1:def" {
		t.Fatalf("workspaceRevision = %q", s.Status.WorkspaceRevision)
	}
}

func TestPreparedWithoutToolchainNeverStale(t *testing.T) {
	e := newEnv(t, agentObj("hello", nil), sessionObj("a", "hello", v1alpha1.SessionPhaseRunning, nil), secret("a-token", "tok-a"))
	expect(t, e.do("POST", "/v1/sessions/team/a/prepared", "tok-a", Prepared{ToolchainHash: "abc"}), 204)
	if a := e.agent(); a.Status.Toolchain.Stale {
		t.Fatal("agent without toolchain marked stale")
	}
}

func TestPreparedValidation(t *testing.T) {
	e := newEnv(t, agentObj("hello", nil), sessionObj("a", "hello", v1alpha1.SessionPhaseRunning, nil), secret("a-token", "tok-a"))
	expect(t, e.do("POST", "/v1/sessions/team/a/prepared", "tok-a", `{"toolchainHash":"../x"}`), 400)
	expect(t, e.do("POST", "/v1/sessions/team/a/prepared", "tok-a", `not json`), 400)
}

func newPlanEnv(t *testing.T) *env {
	t.Helper()
	cm := "skills-cm"
	flux := &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{Namespace: fluxNS, Name: skillsRepo},
		Status: sourcev1.GitRepositoryStatus{Artifact: &fluxmeta.Artifact{
			URL: "http://source-controller.flux-system.svc/gitrepository/flux-system/skills/x.tar.gz", Digest: "sha256:aa", Revision: "main@sha1:1",
		}},
	}
	fluxWS := &sourcev1.OCIRepository{
		ObjectMeta: metav1.ObjectMeta{Namespace: fluxNS, Name: "ws"},
		Status: sourcev1.OCIRepositoryStatus{Artifact: &fluxmeta.Artifact{
			URL: "http://source-controller.flux-system.svc/ocirepository/flux-system/ws/y.tar.gz", Digest: "sha256:bb", Revision: "latest@sha256:2",
		}},
	}
	gitAgent := agentObj("git", func(a *v1alpha1.OmpAgent) {
		a.Spec.ServiceAccountName = "triage"
		a.Spec.Toolchain = &v1alpha1.ToolchainSpec{Disable: []string{"gitleaks"}}
		a.Status.Toolchain.Hash = "stored"
		a.Spec.Workspace = &v1alpha1.WorkspaceSpec{
			Git: &v1alpha1.GitWorkspaceSource{
				URL: "https://example.com/ws.git", Ref: "main", Depth: 5,
				Auth: &v1alpha1.GitAuth{TokenSecretRef: &v1alpha1.SecretKeyRef{Name: "tok", Key: "token"}},
			},
			Links: []v1alpha1.WorkspaceLink{{Path: "state/data", Target: "/data/state"}},
		}
		a.Spec.Skills = []v1alpha1.SkillSource{
			{Git: &v1alpha1.GitSkillSource{
				URL: "git@example.com:skills.git",
				Auth: &v1alpha1.GitAuth{
					SSHKeySecretRef:     &v1alpha1.SecretKeyRef{Name: "k", Key: "id"},
					KnownHostsSecretRef: &v1alpha1.SecretKeyRef{Name: "k", Key: "kh"},
				},
			}},
			{ConfigMap: &v1alpha1.LocalObjectRef{Name: cm}},
			{FluxSource: &v1alpha1.FluxSkillSource{FluxSourceRef: v1alpha1.FluxSourceRef{
				Kind: v1alpha1.FluxSourceGitRepository, Namespace: fluxNS, Name: skillsRepo,
			}}},
			{Git: &v1alpha1.GitSkillSource{URL: "https://example.com/public.git", Ref: "v1", Path: "ai"}},
		}
	})
	fluxAgent := agentObj("fluxws", func(a *v1alpha1.OmpAgent) {
		a.Spec.Workspace = &v1alpha1.WorkspaceSpec{FluxSource: &v1alpha1.FluxSourceRef{
			Kind: v1alpha1.FluxSourceOCIRepository, Namespace: fluxNS, Name: "ws",
		}}
	})
	return newEnv(t, gitAgent, fluxAgent, flux, fluxWS,
		sessionObj("g", "git", v1alpha1.SessionPhaseRunning, func(s *v1alpha1.OmpSession) { s.Spec.ContinuityKey = "trig/abc" }),
		sessionObj("f", "fluxws", v1alpha1.SessionPhaseRunning, nil),
		secret("g-token", "tok-g"), secret("f-token", "tok-f"), secret("git-toolsync-token", tokSync),
	)
}

func checkGitWorkspace(t *testing.T, plan Plan) {
	t.Helper()
	if plan.Workspace == nil || plan.Workspace.Git == nil || plan.Workspace.Artifact != nil {
		t.Fatalf("workspace = %+v", plan.Workspace)
	}
	g := plan.Workspace.Git
	if g.URL != "https://example.com/ws.git" || g.Ref != "main" || g.Depth != 5 ||
		g.AuthDir != AuthDir("workspace") || g.AuthType != "token" || g.Username != "x-access-token" {
		t.Fatalf("workspace git = %+v", g)
	}
}

func checkSkills(t *testing.T, plan Plan) {
	t.Helper()
	if len(plan.Skills) != 4 {
		t.Fatalf("skills = %+v", plan.Skills)
	}
	s0 := plan.Skills[0]
	if s0.Git == nil || s0.Git.AuthDir != AuthDir(SkillIndexID(0)) || s0.Git.AuthType != "ssh" || s0.Path != "skills" || s0.Git.Depth != 1 {
		t.Fatalf("skill 0 = %+v / %+v", s0, s0.Git)
	}
	if s1 := plan.Skills[1]; s1.ConfigMapDir != ConfigMapSkillDir(1) || s1.Git != nil || s1.Artifact != nil {
		t.Fatalf("skill 1 = %+v", s1)
	}
	s2 := plan.Skills[2]
	if s2.Artifact == nil || s2.Artifact.Digest != "sha256:aa" || s2.Artifact.Revision != "main@sha1:1" ||
		!strings.HasSuffix(s2.Artifact.URL, "x.tar.gz") || s2.Path != "skills" {
		t.Fatalf("skill 2 = %+v", s2)
	}
	if s3 := plan.Skills[3]; s3.Git == nil || s3.Git.AuthDir != "" || s3.Git.Ref != "v1" || s3.Path != "ai" {
		t.Fatalf("skill 3 = %+v", s3)
	}
}

func TestSessionPlanGitWorkspaceAndSkills(t *testing.T) {
	e := newPlanEnv(t)
	rec := e.do(http.MethodGet, "/v1/sessions/team/g/plan", "tok-g", nil)
	expect(t, rec, 200)
	plan := decode[Plan](t, rec)

	checkGitWorkspace(t, plan)
	checkSkills(t, plan)
	if len(plan.Links) != 1 || plan.Links[0] != (Link{Path: "state/data", Target: "/data/state"}) {
		t.Fatalf("links = %+v", plan.Links)
	}
	if plan.Toolchain == nil || plan.Toolchain.Hash != "stored" || len(plan.Toolchain.Disable) != 1 || plan.Toolchain.Disable[0] != "gitleaks" {
		t.Fatalf("toolchain = %+v", plan.Toolchain)
	}
	if plan.Continuity == nil || plan.Continuity.ID != store.ContinuityID("trig/abc") {
		t.Fatalf("continuity = %+v", plan.Continuity)
	}
	if !plan.Kubeconfig {
		t.Fatal("kubeconfig not wanted despite serviceAccountName")
	}
}

func TestSessionPlanFluxWorkspace(t *testing.T) {
	e := newPlanEnv(t)
	rec := e.do(http.MethodGet, "/v1/sessions/team/f/plan", "tok-f", nil)
	expect(t, rec, 200)
	plan := decode[Plan](t, rec)
	if plan.Workspace == nil || plan.Workspace.Artifact == nil || plan.Workspace.Git != nil {
		t.Fatalf("workspace = %+v", plan.Workspace)
	}
	if a := plan.Workspace.Artifact; a.Digest != "sha256:bb" || a.Revision != "latest@sha256:2" || !strings.HasSuffix(a.URL, "y.tar.gz") {
		t.Fatalf("artifact = %+v", a)
	}
	if plan.Toolchain != nil || plan.Continuity != nil || plan.Kubeconfig || len(plan.Skills) != 0 {
		t.Fatalf("unexpected plan sections: %+v", plan)
	}
}

func TestAgentPlanHasOnlyWorkspaceAndToolchain(t *testing.T) {
	e := newPlanEnv(t)
	rec := e.do(http.MethodGet, "/v1/agents/team/git/plan", tokSync, nil)
	expect(t, rec, 200)
	plan := decode[Plan](t, rec)
	if plan.Workspace == nil || plan.Workspace.Git == nil || plan.Toolchain == nil {
		t.Fatalf("plan = %+v", plan)
	}
	if len(plan.Skills) != 0 || len(plan.Links) != 0 || plan.Continuity != nil || plan.Kubeconfig {
		t.Fatalf("agent plan carries session sections: %+v", plan)
	}
}

func TestPlanFluxWithoutArtifactIs503(t *testing.T) {
	repo := &sourcev1.GitRepository{ObjectMeta: metav1.ObjectMeta{Namespace: fluxNS, Name: "ws"}}
	agent := agentObj("hello", func(a *v1alpha1.OmpAgent) {
		a.Spec.Workspace = &v1alpha1.WorkspaceSpec{FluxSource: &v1alpha1.FluxSourceRef{
			Kind: v1alpha1.FluxSourceGitRepository, Namespace: fluxNS, Name: "ws",
		}}
	})
	e := newEnv(t, agent, repo, sessionObj("a", "hello", v1alpha1.SessionPhaseRunning, nil), secret("a-token", "tok-a"))
	expect(t, e.do(http.MethodGet, planPath, "tok-a", nil), 503)

	missing := newEnv(t, agent, sessionObj("a", "hello", v1alpha1.SessionPhaseRunning, nil), secret("a-token", "tok-a"))
	expect(t, missing.do(http.MethodGet, planPath, "tok-a", nil), 503)
}

func TestSessionConfig(t *testing.T) {
	val := literalValue
	agent := agentObj("hello", func(a *v1alpha1.OmpAgent) {
		a.Spec.Instructions = "Be careful."
		a.Spec.Model = v1alpha1.ModelSpec{
			Default: modelID, Small: "openai/gpt-5-mini",
			Gateway: &v1alpha1.GatewaySpec{URL: "http://omp-auth.omp-system.svc:4000"},
		}
		a.Spec.Credentials = []v1alpha1.Credential{
			{Name: "demo", Description: "demo credential", Env: "DEMO_TOKEN", SecretRef: v1alpha1.SecretKeyRef{Name: "s", Key: "k"}},
			{Name: "kube", Description: "kube config", File: "/etc/creds/kubeconfig", SecretRef: v1alpha1.SecretKeyRef{Name: "s", Key: "k2"}},
		}
		a.Spec.MCPServers = []v1alpha1.MCPServer{
			{
				Name: "local", Command: []string{"mcp-server", "--flag", "x"},
				Env: []v1alpha1.ValueOrSecret{
					{Name: "PLAIN", Value: &val},
					{Name: "API_KEY", SecretRef: &v1alpha1.SecretKeyRef{Name: "m", Key: "key"}},
				},
			},
			{
				Name: "remote", URL: "https://mcp.example.com/mcp",
				Headers: []v1alpha1.ValueOrSecret{
					{Name: "X-Plain", Value: &val},
					{Name: "Authorization", SecretRef: &v1alpha1.SecretKeyRef{Name: "m", Key: "auth"}},
				},
			},
			{Name: "legacy", Type: v1alpha1.MCPSSE, URL: "https://mcp.example.com/sse"},
		}
	})
	e := newEnv(t, agent,
		sessionObj("a", "hello", v1alpha1.SessionPhaseRunning, func(s *v1alpha1.OmpSession) { s.Spec.ContinuityKey = "k" }),
		secret("a-token", "tok-a"))

	rec := e.do(http.MethodGet, "/v1/sessions/team/a", "tok-a", nil)
	expect(t, rec, 200)
	cfg := decode[SessionConfig](t, rec)

	if cfg.Prompt != "triage it" || !cfg.Continuity || cfg.TimeoutSeconds != 1800 {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.Model.Default != modelID || cfg.Model.Small != "openai/gpt-5-mini" || cfg.Model.Thinking != "medium" {
		t.Fatalf("model = %+v", cfg.Model)
	}
	if g := cfg.Model.Gateway; g == nil || g.URL != "http://omp-auth.omp-system.svc:4000" || strings.Join(g.Providers, ",") != "anthropic,openai" {
		t.Fatalf("gateway = %+v", cfg.Model.Gateway)
	}
	if strings.Join(cfg.Tools, ",") != "read,grep,glob,bash,todo" {
		t.Fatalf("tools = %v", cfg.Tools)
	}
	for _, want := range []string{"Be careful.", "Available credentials", "demo: demo credential", "$DEMO_TOKEN", "(file /etc/creds/kubeconfig)", "Actions you may call", "restart_it: restarts things", "finish: ends the session"} {
		if !strings.Contains(cfg.SystemPrompt, want) {
			t.Errorf("system prompt lacks %q:\n%s", want, cfg.SystemPrompt)
		}
	}
	if len(cfg.Actions) != 2 || cfg.Actions[1].Name != "finish" {
		t.Fatalf("actions = %+v", cfg.Actions)
	}

	checkMCP(t, cfg.MCP)
	checkRedact(t, cfg.Redact)
}

// checkMCP verifies mcp.json: placeholders for secrets, literals as is, transport types computed.
func checkMCP(t *testing.T, raw json.RawMessage) {
	t.Helper()
	var mcp struct {
		MCPServers map[string]mcpServerEntry `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &mcp); err != nil {
		t.Fatal(err)
	}
	local := mcp.MCPServers["local"]
	if local.Type != "stdio" || local.Command != "mcp-server" || strings.Join(local.Args, " ") != "--flag x" ||
		local.Env["PLAIN"] != literalValue || local.Env["API_KEY"] != "${OMP_MCP_0_1}" {
		t.Fatalf("local = %+v", local)
	}
	remote := mcp.MCPServers["remote"]
	if remote.Type != "http" || remote.URL != "https://mcp.example.com/mcp" ||
		remote.Headers["X-Plain"] != literalValue || remote.Headers["Authorization"] != "${OMP_MCP_1_1}" {
		t.Fatalf("remote = %+v", remote)
	}
	if legacy := mcp.MCPServers["legacy"]; legacy.Type != "sse" || legacy.URL != "https://mcp.example.com/sse" {
		t.Fatalf("legacy = %+v", legacy)
	}
	if !strings.Contains(string(raw), literalValue) {
		t.Fatal("literal value missing from mcp.json")
	}
}

func checkRedact(t *testing.T, got []RedactSource) {
	t.Helper()
	want := []RedactSource{
		{Name: "demo", Env: "DEMO_TOKEN"},
		{Name: "kube", File: "/etc/creds/kubeconfig"},
		{Name: "mcp-local-API_KEY", Env: "OMP_MCP_0_1"},
		{Name: "mcp-remote-Authorization", Env: "OMP_MCP_1_1"},
		{Name: "gateway-token", Env: "OMP_GATEWAY_TOKEN"},
	}
	if len(got) != len(want) {
		t.Fatalf("redact = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("redact[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSessionConfigDefaultsAndNoGateway(t *testing.T) {
	agent := agentObj("hello", func(a *v1alpha1.OmpAgent) {
		a.Spec.Limits.SessionTimeout = metav1.Duration{Duration: 5 * time.Minute}
		a.Spec.Tools.Allow = []string{"read"}
		a.Spec.Model.Thinking = v1alpha1.ThinkingHigh
	})
	e := newEnv(t, agent, sessionObj("a", "hello", v1alpha1.SessionPhaseRunning, nil), secret("a-token", "tok-a"))
	cfg := decode[SessionConfig](t, e.do(http.MethodGet, "/v1/sessions/team/a", "tok-a", nil))
	if cfg.TimeoutSeconds != 300 || cfg.Model.Thinking != "high" || cfg.Model.Gateway != nil || cfg.Continuity {
		t.Fatalf("config = %+v", cfg)
	}
	if len(cfg.Tools) != 1 || cfg.Tools[0] != "read" || cfg.MCP != nil || len(cfg.Redact) != 0 {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestStdioMCPWithoutCommandIsError(t *testing.T) {
	agent := agentObj("hello", func(a *v1alpha1.OmpAgent) {
		a.Spec.MCPServers = []v1alpha1.MCPServer{{Name: "broken", Type: v1alpha1.MCPStdio}}
	})
	e := newEnv(t, agent, sessionObj("a", "hello", v1alpha1.SessionPhaseRunning, nil), secret("a-token", "tok-a"))
	expect(t, e.do(http.MethodGet, "/v1/sessions/team/a", "tok-a", nil), 500)
}

func TestActions(t *testing.T) {
	e := newEnv(t, agentObj("hello", nil), sessionObj("a", "hello", v1alpha1.SessionPhaseRunning, nil), secret("a-token", "tok-a"))
	rec := e.do("POST", "/v1/sessions/team/a/actions/restart_it", "tok-a", `{"name":"x"}`)
	expect(t, rec, 200)
	if res := decode[ActionResult](t, rec); res.Text != "did restart_it" || res.IsError {
		t.Fatalf("result = %+v", res)
	}
	if len(e.runner.args) != 1 || e.runner.args[0] != `{"name":"x"}` {
		t.Fatalf("args = %v", e.runner.args)
	}
	expect(t, e.do("POST", "/v1/sessions/team/a/actions/nope", "tok-a", `{}`), 404)
	expect(t, e.do("POST", "/v1/sessions/team/a/actions/restart_it", "tok-a", `{bad`), 400)
	expect(t, e.do("POST", "/v1/sessions/team/a/actions/restart_it", "tok-a", nil), 200)
	if e.runner.args[len(e.runner.args)-1] != "{}" {
		t.Fatalf("empty body args = %q", e.runner.args[len(e.runner.args)-1])
	}
	expect(t, e.do("POST", "/v1/sessions/team/a/actions/restart_it", "bad", `{}`), 401)
}

func TestEventsAndTranscript(t *testing.T) {
	e := newEnv(t, agentObj("hello", nil), sessionObj("a", "hello", v1alpha1.SessionPhaseRunning, nil), secret("a-token", "tok-a"))
	long := strings.Repeat("é", 700)
	expect(t, e.do("POST", "/v1/sessions/team/a/events", "tok-a", Events{Events: []Event{
		{At: time.Date(2026, 10, 18, 11, 0, 0, 0, time.UTC), Kind: "tool", Text: "bash ls"},
		{Kind: "", Text: long},
	}}), 204)
	got, err := e.store.ReadEvents("uid-a", 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("events = %+v, %v", got, err)
	}
	if got[0].Kind != "tool" || got[0].Text != "bash ls" {
		t.Fatalf("event 0 = %+v", got[0])
	}
	if got[1].Kind != "info" || len([]rune(got[1].Text)) != 500 || !got[1].At.Equal(e.srv.now()) {
		t.Fatalf("event 1 = kind %q, %d chars, at %v", got[1].Kind, len([]rune(got[1].Text)), got[1].At)
	}
	tooMany := Events{Events: make([]Event, maxEventsPerPost+1)}
	expect(t, e.do("POST", "/v1/sessions/team/a/events", "tok-a", tooMany), 400)

	expect(t, e.do("PUT", "/v1/sessions/team/a/transcript", "tok-a", "<html>fake</html>"), 204)
	f, err := e.store.OpenTranscript("uid-a")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if b, _ := io.ReadAll(f); string(b) != "<html>fake</html>" {
		t.Fatalf("transcript = %q", b)
	}
}

func TestContinuity(t *testing.T) {
	with := func(s *v1alpha1.OmpSession) { s.Spec.ContinuityKey = "trig/g" }
	e := newEnv(t, agentObj("hello", nil),
		sessionObj("a", "hello", v1alpha1.SessionPhaseRunning, with),
		sessionObj("plain", "hello", v1alpha1.SessionPhaseRunning, nil),
		secret("a-token", "tok-a"), secret("plain-token", "tok-plain"))

	expect(t, e.do(http.MethodGet, "/v1/sessions/team/a/continuity", "tok-a", nil), 404)
	expect(t, e.do("PUT", "/v1/sessions/team/a/continuity", "tok-a", "tarball-bytes"), 204)
	if s := e.session(); s.Status.Continuity == nil || !s.Status.Continuity.Stored {
		t.Fatalf("continuity status = %+v", s.Status.Continuity)
	}
	rec := e.do(http.MethodGet, "/v1/sessions/team/a/continuity", "tok-a", nil)
	expect(t, rec, 200)
	if rec.Body.String() != "tarball-bytes" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	f, err := e.store.OpenContinuity(ns, "hello", store.ContinuityID("trig/g"))
	if err != nil {
		t.Fatalf("tarball not under the agent's continuity id: %v", err)
	}
	_ = f.Close()

	expect(t, e.do(http.MethodGet, "/v1/sessions/team/plain/continuity", "tok-plain", nil), 404)
	expect(t, e.do("PUT", "/v1/sessions/team/plain/continuity", "tok-plain", "x"), 400)
}

func TestSessionToolchainDownload(t *testing.T) {
	agent := agentObj("hello", func(a *v1alpha1.OmpAgent) { a.Spec.Toolchain = &v1alpha1.ToolchainSpec{} })
	e := newEnv(t, agent, sessionObj("a", "hello", v1alpha1.SessionPhaseRunning, nil), secret("a-token", "tok-a"))
	expect(t, e.do(http.MethodGet, "/v1/sessions/team/a/toolchain", "tok-a", nil), 404)

	if err := e.store.PutToolchain(ns, "hello", "h1", strings.NewReader("chain-bytes")); err != nil {
		t.Fatal(err)
	}
	// Stored but not yet recorded in status: still 404.
	expect(t, e.do(http.MethodGet, "/v1/sessions/team/a/toolchain", "tok-a", nil), 404)

	a := e.agent()
	a.Status.Toolchain.Hash = "h1"
	if err := e.c.Status().Update(context.Background(), &a); err != nil {
		t.Fatal(err)
	}
	rec := e.do(http.MethodGet, "/v1/sessions/team/a/toolchain", "tok-a", nil)
	expect(t, rec, 200)
	if rec.Body.String() != "chain-bytes" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	a.Status.Toolchain.Hash = "missing"
	if err := e.c.Status().Update(context.Background(), &a); err != nil {
		t.Fatal(err)
	}
	expect(t, e.do(http.MethodGet, "/v1/sessions/team/a/toolchain", "tok-a", nil), 404)
}

func TestComplete(t *testing.T) {
	e := newEnv(t, agentObj("hello", nil), sessionObj("a", "hello", v1alpha1.SessionPhaseRunning, nil), secret("a-token", "tok-a"))
	expect(t, e.do("POST", "/v1/sessions/team/a/complete", "tok-a", `{"outcome":"nonsense"}`), 400)

	body := Complete{
		Outcome: OutcomeError, Error: strings.Repeat("x", 1500), FinishCalled: false, ContinuityRestored: true,
		Usage: Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 3, CacheWriteTokens: 4, Cost: "0.0123"},
	}
	expect(t, e.do("POST", "/v1/sessions/team/a/complete", "tok-a", body), 204)
	s := e.session()
	if s.Status.Outcome != v1alpha1.OutcomeError || len(s.Status.Error) != 1000 {
		t.Fatalf("outcome/error = %q/%d", s.Status.Outcome, len(s.Status.Error))
	}
	if u := s.Status.Usage; u == nil || *u != (v1alpha1.SessionUsage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 3, CacheWriteTokens: 4, Cost: "0.0123"}) {
		t.Fatalf("usage = %+v", u)
	}
	if c := s.Status.Continuity; c == nil || !c.Restored {
		t.Fatalf("continuity = %+v", c)
	}
	if s.Status.Phase != v1alpha1.SessionPhaseRunning {
		t.Fatalf("phase changed to %q: the reconciler owns it", s.Status.Phase)
	}
}

func TestCompleteKeepsStoredContinuity(t *testing.T) {
	e := newEnv(t, agentObj("hello", nil),
		sessionObj("a", "hello", v1alpha1.SessionPhaseRunning, func(s *v1alpha1.OmpSession) {
			s.Status.Continuity = &v1alpha1.ContinuityStatus{Stored: true}
		}), secret("a-token", "tok-a"))
	expect(t, e.do("POST", "/v1/sessions/team/a/complete", "tok-a", Complete{Outcome: OutcomeCompleted, FinishCalled: true}), 204)
	if c := e.session().Status.Continuity; c == nil || !c.Stored || c.Restored {
		t.Fatalf("continuity = %+v", c)
	}
}

func TestToolsyncRoutes(t *testing.T) {
	agent := agentObj("hello", func(a *v1alpha1.OmpAgent) { a.Spec.Toolchain = &v1alpha1.ToolchainSpec{} })
	e := newEnv(t, agent, secret("hello-toolsync-token", tokSync))

	rec := e.do(http.MethodGet, "/v1/agents/team/hello/toolchain", tokSync, nil)
	expect(t, rec, 200)
	if h := decode[ToolchainHash](t, rec); h.Hash != "" {
		t.Fatalf("hash = %q", h.Hash)
	}

	// Upload.
	expect(t, e.do("PUT", "/v1/agents/team/hello/toolchain/"+hash1, tokSync, "tar-1"), 204)
	a := e.agent()
	tc := a.Status.Toolchain
	if tc.Hash != hash1 || tc.Stale || tc.SyncedAt == nil || !tc.SyncedAt.Time.Equal(e.srv.now()) {
		t.Fatalf("toolchain status = %+v", tc)
	}
	cond := findCondition(a.Status.Conditions, v1alpha1.ConditionToolchainReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("ToolchainReady = %+v", cond)
	}
	if f, err := e.store.OpenToolchain(ns, "hello", hash1); err != nil {
		t.Fatal(err)
	} else {
		_ = f.Close()
	}
	rec = e.do(http.MethodGet, "/v1/agents/team/hello/toolchain", tokSync, nil)
	if h := decode[ToolchainHash](t, rec); h.Hash != hash1 {
		t.Fatalf("hash = %q", h.Hash)
	}

	// A session saw another hash: stale; the unchanged report clears it.
	a.Status.Toolchain.Stale = true
	a.Status.Toolchain.LastError = "boom"
	if err := e.c.Status().Update(context.Background(), &a); err != nil {
		t.Fatal(err)
	}
	expect(t, e.do("POST", "/v1/agents/team/hello/toolchain/unchanged", tokSync, ToolchainHash{Hash: hash1}), 204)
	tc = e.agent().Status.Toolchain
	if tc.Stale || tc.Hash != hash1 || tc.LastError != "" {
		t.Fatalf("toolchain status = %+v", tc)
	}

	// Empty hash is a valid "unchanged" (no mise.toml).
	expect(t, e.do("POST", "/v1/agents/team/hello/toolchain/unchanged", tokSync, ToolchainHash{}), 204)
	if h := e.agent().Status.Toolchain.Hash; h != "" {
		t.Fatalf("hash = %q", h)
	}

	// Bad hashes.
	expect(t, e.do("PUT", "/v1/agents/team/hello/toolchain/a%2Fb", tokSync, "x"), 400)
	expect(t, e.do("POST", "/v1/agents/team/hello/toolchain/unchanged", tokSync, `{"hash":"a/b"}`), 400)
}

func TestToolchainKeepsNewAndPrevious(t *testing.T) {
	e := newEnv(t, agentObj("hello", func(a *v1alpha1.OmpAgent) { a.Spec.Toolchain = &v1alpha1.ToolchainSpec{} }),
		secret("hello-toolsync-token", tokSync))
	for _, h := range []string{"h1", "h2", "h3"} {
		expect(t, e.do("PUT", "/v1/agents/team/hello/toolchain/"+h, tokSync, "tar-"+h), 204)
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := e.store.OpenToolchain(ns, "hello", "h1"); err == nil {
		t.Fatal("oldest toolchain kept")
	}
	for _, h := range []string{"h2", "h3"} {
		f, err := e.store.OpenToolchain(ns, "hello", h)
		if err != nil {
			t.Fatalf("%s: %v", h, err)
		}
		_ = f.Close()
	}
}

func TestUnknownRoutes(t *testing.T) {
	e := newEnv(t, agentObj("hello", nil))
	if rec := e.do(http.MethodGet, "/v1/other", "x", nil); rec.Code != 404 {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec := e.do("DELETE", "/v1/sessions/team/a", "x", nil); rec.Code != 405 {
		t.Fatalf("status = %d", rec.Code)
	}
}

func findCondition(conds []metav1.Condition, typ string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == typ {
			return &conds[i]
		}
	}
	return nil
}
