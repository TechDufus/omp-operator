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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/gateway"
	"github.com/TechDufus/omp-operator/internal/store"
)

const (
	agentTestNS         = "team"
	agentTestOpNS       = "omp-system"
	agentTestClients    = "omp-auth-clients"
	agentTestName       = "argus"
	agentTestRestart    = "restart"
	agentTestOther      = "other"
	agentTestTokenKey   = "token"
	agentTestFluxNS     = "flux-system"
	agentTestGatewayURL = "http://omp-auth.omp-system.svc:4000"
	agentTestSkills     = "skills"

	// Ready reasons, spelled out so the test pins the API contract.
	wantReady          = "Ready"
	wantInvalidSpec    = "InvalidSpec"
	wantMissingSecret  = "MissingSecret"
	wantActionNotReady = "ActionNotReady"
)

type agentTestEnv struct {
	r     *OmpAgentReconciler
	c     client.Client
	store *store.Store
	opts  *config.Options
	gw    *gateway.Manager
}

func newAgentTestEnv(t *testing.T, managed bool, objs ...client.Object) *agentTestEnv {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, ompv1alpha1.AddToScheme, sourcev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&ompv1alpha1.OmpAgent{}, &ompv1alpha1.OmpAction{}).Build()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := &config.Options{OperatorNamespace: agentTestOpNS, DefaultRuntimeImage: "runtime:test"}
	if managed {
		opts.GatewayClientsSecret = agentTestClients
	}
	gw := &gateway.Manager{Client: c, APIReader: c, Options: opts, Now: func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) }}
	return &agentTestEnv{
		r: &OmpAgentReconciler{Client: c, Scheme: scheme, APIReader: c, Options: opts, Store: st, Gateway: gw},
		c: c, store: st, opts: opts, gw: gw,
	}
}

func agentTestAgent(mutate func(*ompv1alpha1.OmpAgentSpec)) *ompv1alpha1.OmpAgent {
	a := &ompv1alpha1.OmpAgent{
		ObjectMeta: metav1.ObjectMeta{Namespace: agentTestNS, Name: agentTestName, UID: "uid-argus", Generation: 3},
		Spec:       ompv1alpha1.OmpAgentSpec{Model: ompv1alpha1.ModelSpec{Default: "anthropic/claude-sonnet-5-5"}},
	}
	if mutate != nil {
		mutate(&a.Spec)
	}
	return a
}

func agentTestSecret(name, key string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: agentTestNS, Name: name},
		Data:       map[string][]byte{key: []byte("value-of-" + key)},
	}
}

func agentTestAction(ready *metav1.ConditionStatus) *ompv1alpha1.OmpAction {
	act := &ompv1alpha1.OmpAction{ObjectMeta: metav1.ObjectMeta{Namespace: agentTestNS, Name: agentTestRestart}}
	if ready != nil {
		act.Status.Conditions = []metav1.Condition{{
			Type: ompv1alpha1.ConditionReady, Status: *ready, Reason: wantInvalidSpec, Message: "bad template",
			LastTransitionTime: metav1.Now(),
		}}
	}
	return act
}

func (e *agentTestEnv) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	res, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: agentTestNS, Name: agentTestName}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func (e *agentTestEnv) agent(t *testing.T) *ompv1alpha1.OmpAgent {
	t.Helper()
	var a ompv1alpha1.OmpAgent
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: agentTestNS, Name: agentTestName}, &a); err != nil {
		t.Fatal(err)
	}
	return &a
}

func (e *agentTestEnv) clients(t *testing.T) string {
	t.Helper()
	var s corev1.Secret
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: agentTestOpNS, Name: agentTestClients}, &s); err != nil {
		t.Fatal(err)
	}
	return string(s.Data[gateway.ClientsKey])
}

func (e *agentTestEnv) ready(t *testing.T) *metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(e.agent(t).Status.Conditions, ompv1alpha1.ConditionReady)
	if c == nil {
		t.Fatal("no Ready condition")
	}
	return c
}

func TestOmpAgentReadyReasons(t *testing.T) {
	trueStatus, falseStatus := metav1.ConditionTrue, metav1.ConditionFalse
	ref := func(name string) ompv1alpha1.SecretKeyRef { return ompv1alpha1.SecretKeyRef{Name: name, Key: "k"} }
	cred := func(name, env, file, secret string) ompv1alpha1.Credential {
		return ompv1alpha1.Credential{Name: name, Description: "d", SecretRef: ref(secret), Env: env, File: file}
	}
	gwSpec := func(s *ompv1alpha1.OmpAgentSpec) {
		s.Model.Gateway = &ompv1alpha1.GatewaySpec{URL: agentTestGatewayURL}
	}
	flux := func(kind ompv1alpha1.FluxSourceKind) ompv1alpha1.FluxSourceRef {
		return ompv1alpha1.FluxSourceRef{Kind: kind, Namespace: agentTestFluxNS, Name: agentTestSkills}
	}

	tests := []struct {
		name    string
		managed bool
		mutate  func(*ompv1alpha1.OmpAgentSpec)
		opts    func(*config.Options)
		objs    []client.Object
		reason  string
		msg     string
	}{
		{name: "ready", reason: wantReady},
		{name: "namespace not allowed", opts: func(o *config.Options) { o.AgentNamespaces = []string{agentTestOther} }, reason: "NamespaceNotAllowed", msg: `"team"`},
		{name: "no image", opts: func(o *config.Options) { o.DefaultRuntimeImage = "" }, reason: "NoImage"},
		{name: "image on the agent", opts: func(o *config.Options) { o.DefaultRuntimeImage = "" }, mutate: func(s *ompv1alpha1.OmpAgentSpec) { s.Image = "my/runtime:1" }, reason: wantReady},
		{name: "invalid model", mutate: func(s *ompv1alpha1.OmpAgentSpec) { s.Model.Default = "nomodelpath" }, reason: wantInvalidSpec, msg: "provider/id"},
		{name: "skill union", mutate: func(s *ompv1alpha1.OmpAgentSpec) { s.Skills = []ompv1alpha1.SkillSource{{}} }, reason: wantInvalidSpec},
		{name: "mcp consistency", mutate: func(s *ompv1alpha1.OmpAgentSpec) {
			s.MCPServers = []ompv1alpha1.MCPServer{{Name: "m", Type: ompv1alpha1.MCPStdio}}
		}, reason: wantInvalidSpec, msg: "command"},
		{name: "duplicate credential", mutate: func(s *ompv1alpha1.OmpAgentSpec) {
			s.Credentials = []ompv1alpha1.Credential{cred("a", "A", "", "s"), cred("a", "B", "", "s")}
		}, objs: []client.Object{agentTestSecret("s", "k")}, reason: "DuplicateName"},
		{name: "duplicate mcp", mutate: func(s *ompv1alpha1.OmpAgentSpec) {
			s.MCPServers = []ompv1alpha1.MCPServer{{Name: "m", Command: []string{"x"}}, {Name: "m", Command: []string{"y"}}}
		}, reason: "DuplicateName"},
		{name: "reserved env", mutate: func(s *ompv1alpha1.OmpAgentSpec) {
			s.Credentials = []ompv1alpha1.Credential{cred("a", "OMP_X", "", "s")}
		},
			objs: []client.Object{agentTestSecret("s", "k")}, reason: "InvalidCredential"},
		{name: "reserved file", mutate: func(s *ompv1alpha1.OmpAgentSpec) {
			s.Credentials = []ompv1alpha1.Credential{cred("a", "", "/workspace/x", "s")}
		},
			objs: []client.Object{agentTestSecret("s", "k")}, reason: "InvalidCredential"},
		{name: "missing credential secret", mutate: func(s *ompv1alpha1.OmpAgentSpec) {
			s.Credentials = []ompv1alpha1.Credential{cred("a", "TOKEN", "", "absent")}
		},
			reason: wantMissingSecret, msg: `"absent"`},
		{name: "missing secret key", mutate: func(s *ompv1alpha1.OmpAgentSpec) {
			s.Credentials = []ompv1alpha1.Credential{cred("a", "TOKEN", "", "s")}
		},
			objs: []client.Object{agentTestSecret("s", agentTestOther)}, reason: wantMissingSecret, msg: `no key "k"`},
		{name: "credential present", mutate: func(s *ompv1alpha1.OmpAgentSpec) {
			s.Credentials = []ompv1alpha1.Credential{cred("a", "TOKEN", "", "s")}
		},
			objs: []client.Object{agentTestSecret("s", "k")}, reason: wantReady},
		{name: "missing sink secret", mutate: func(s *ompv1alpha1.OmpAgentSpec) {
			s.Notify = []ompv1alpha1.NotifySink{{Type: ompv1alpha1.SinkSlack, Slack: &ompv1alpha1.SlackSink{WebhookURLSecretRef: ref("hook")}}}
		}, reason: wantMissingSecret, msg: "notify[0]"},
		{name: "missing git auth secret", mutate: func(s *ompv1alpha1.OmpAgentSpec) {
			tok := ref("git-token")
			s.Workspace = &ompv1alpha1.WorkspaceSpec{Git: &ompv1alpha1.GitWorkspaceSource{URL: "https://x/y.git", Auth: &ompv1alpha1.GitAuth{TokenSecretRef: &tok}}}
		}, reason: wantMissingSecret, msg: "workspace.git"},
		{name: "missing mcp secret", mutate: func(s *ompv1alpha1.OmpAgentSpec) {
			s.MCPServers = []ompv1alpha1.MCPServer{{Name: "m", URL: "http://mcp", Headers: []ompv1alpha1.ValueOrSecret{{Name: "H", SecretRef: &ompv1alpha1.SecretKeyRef{Name: "mcp-h", Key: "k"}}}}}
		}, reason: wantMissingSecret, msg: "mcpServer"},
		{name: "missing gateway token secret", mutate: func(s *ompv1alpha1.OmpAgentSpec) {
			gwSpec(s)
			s.Model.Gateway.TokenSecretRef = &ompv1alpha1.SecretKeyRef{Name: "gw", Key: agentTestTokenKey}
		}, reason: wantMissingSecret, msg: "gateway"},
		{name: "missing configmap", mutate: func(s *ompv1alpha1.OmpAgentSpec) {
			s.Skills = []ompv1alpha1.SkillSource{{ConfigMap: &ompv1alpha1.LocalObjectRef{Name: agentTestSkills}}}
		}, reason: "MissingConfigMap", msg: `"skills"`},
		{name: "configmap present", mutate: func(s *ompv1alpha1.OmpAgentSpec) {
			s.Skills = []ompv1alpha1.SkillSource{{ConfigMap: &ompv1alpha1.LocalObjectRef{Name: agentTestSkills}}}
		}, objs: []client.Object{&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: agentTestNS, Name: agentTestSkills}}}, reason: wantReady},
		{name: "missing flux git source", mutate: func(s *ompv1alpha1.OmpAgentSpec) {
			s.Skills = []ompv1alpha1.SkillSource{{FluxSource: &ompv1alpha1.FluxSkillSource{FluxSourceRef: flux(ompv1alpha1.FluxSourceGitRepository)}}}
		}, reason: "MissingSource", msg: "flux-system/skills"},
		{name: "missing flux workspace oci source", mutate: func(s *ompv1alpha1.OmpAgentSpec) {
			ref := flux(ompv1alpha1.FluxSourceOCIRepository)
			s.Workspace = &ompv1alpha1.WorkspaceSpec{FluxSource: &ref}
		}, reason: "MissingSource", msg: "OCIRepository"},
		{name: "flux sources present", mutate: func(s *ompv1alpha1.OmpAgentSpec) {
			s.Skills = []ompv1alpha1.SkillSource{{FluxSource: &ompv1alpha1.FluxSkillSource{FluxSourceRef: flux(ompv1alpha1.FluxSourceGitRepository)}}}
			ref := flux(ompv1alpha1.FluxSourceOCIRepository)
			s.Workspace = &ompv1alpha1.WorkspaceSpec{FluxSource: &ref}
		}, objs: []client.Object{
			&sourcev1.GitRepository{ObjectMeta: metav1.ObjectMeta{Namespace: agentTestFluxNS, Name: agentTestSkills}},
			&sourcev1.OCIRepository{ObjectMeta: metav1.ObjectMeta{Namespace: agentTestFluxNS, Name: agentTestSkills}},
		}, reason: wantReady},
		{name: "action missing", mutate: func(s *ompv1alpha1.OmpAgentSpec) { s.Actions = []ompv1alpha1.LocalObjectRef{{Name: agentTestRestart}} },
			reason: wantActionNotReady, msg: "not found"},
		{name: "action without condition", mutate: func(s *ompv1alpha1.OmpAgentSpec) { s.Actions = []ompv1alpha1.LocalObjectRef{{Name: agentTestRestart}} },
			objs: []client.Object{agentTestAction(nil)}, reason: wantActionNotReady, msg: "no Ready condition"},
		{name: "action not ready", mutate: func(s *ompv1alpha1.OmpAgentSpec) { s.Actions = []ompv1alpha1.LocalObjectRef{{Name: agentTestRestart}} },
			objs: []client.Object{agentTestAction(&falseStatus)}, reason: wantActionNotReady, msg: "bad template"},
		{name: "action ready", mutate: func(s *ompv1alpha1.OmpAgentSpec) { s.Actions = []ompv1alpha1.LocalObjectRef{{Name: agentTestRestart}} },
			objs: []client.Object{agentTestAction(&trueStatus)}, reason: wantReady},
		{name: "gateway without token and managed tokens disabled", mutate: gwSpec, reason: "NoGatewayToken", msg: "disabled"},
		{name: "gateway with own token", mutate: func(s *ompv1alpha1.OmpAgentSpec) {
			gwSpec(s)
			s.Model.Gateway.TokenSecretRef = &ompv1alpha1.SecretKeyRef{Name: "gw", Key: agentTestTokenKey}
		}, objs: []client.Object{agentTestSecret("gw", agentTestTokenKey)}, reason: wantReady},
		{name: "gateway with managed token", managed: true, mutate: gwSpec, reason: wantReady},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := append([]client.Object{agentTestAgent(tt.mutate)}, tt.objs...)
			e := newAgentTestEnv(t, tt.managed, objs...)
			if tt.opts != nil {
				tt.opts(e.opts)
			}
			res := e.reconcile(t)
			if res.RequeueAfter != 5*time.Minute {
				t.Errorf("RequeueAfter = %v, want 5m", res.RequeueAfter)
			}
			c := e.ready(t)
			wantStatus := metav1.ConditionFalse
			if tt.reason == wantReady {
				wantStatus = metav1.ConditionTrue
			}
			if c.Reason != tt.reason || c.Status != wantStatus || !strings.Contains(c.Message, tt.msg) {
				t.Errorf("Ready = %s/%s %q, want %s/%s containing %q", c.Status, c.Reason, c.Message, wantStatus, tt.reason, tt.msg)
			}
			if c.ObservedGeneration != 3 || e.agent(t).Status.ObservedGeneration != 3 {
				t.Errorf("observedGeneration: condition %d, status %d, want 3", c.ObservedGeneration, e.agent(t).Status.ObservedGeneration)
			}
		})
	}
}

func TestOmpAgentFinalizerAndStatusOwnership(t *testing.T) {
	synced := metav1.NewTime(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	a := agentTestAgent(nil)
	a.Status.Toolchain = ompv1alpha1.ToolchainStatus{Hash: "h", SyncedAt: &synced, Stale: true}
	a.Status.Notices = map[string]metav1.Time{"auth": synced}
	a.Status.Conditions = []metav1.Condition{{
		Type: ompv1alpha1.ConditionToolchainReady, Status: metav1.ConditionTrue, Reason: "Synced", LastTransitionTime: synced,
	}}
	e := newAgentTestEnv(t, false, a)
	e.reconcile(t)

	got := e.agent(t)
	if !agentTestHas(got.Finalizers, ompv1alpha1.FinalizerCleanup) {
		t.Errorf("finalizers = %v", got.Finalizers)
	}
	if got.Status.Toolchain.Hash != "h" || !got.Status.Toolchain.Stale || got.Status.Toolchain.SyncedAt == nil || len(got.Status.Notices) != 1 {
		t.Errorf("reconciler touched toolchain or notices: %+v %+v", got.Status.Toolchain, got.Status.Notices)
	}
	if tc := meta.FindStatusCondition(got.Status.Conditions, ompv1alpha1.ConditionToolchainReady); tc == nil || tc.Reason != "Synced" {
		t.Errorf("ToolchainReady changed: %+v", tc)
	}

	// An unchanged evaluation writes nothing.
	before := got.ResourceVersion
	e.reconcile(t)
	if after := e.agent(t).ResourceVersion; after != before {
		t.Errorf("no-op reconcile wrote the agent: resourceVersion %s -> %s", before, after)
	}

	// Becoming not Ready flips the condition and keeps the rest.
	got.Spec.Model.Default = "bad"
	got.Generation = 4
	if err := e.c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	c := e.ready(t)
	if c.Status != metav1.ConditionFalse || c.Reason != wantInvalidSpec {
		t.Errorf("Ready = %s/%s", c.Status, c.Reason)
	}
}

func agentTestHas(l []string, s string) bool {
	return strings.Contains(strings.Join(l, "\x00"), s)
}

func TestOmpAgentManagedGatewayClients(t *testing.T) {
	ctx := context.Background()
	agent := agentTestAgent(func(s *ompv1alpha1.OmpAgentSpec) {
		s.Model.Gateway = &ompv1alpha1.GatewaySpec{URL: agentTestGatewayURL}
	})
	e := newAgentTestEnv(t, true, agent)
	if err := e.gw.EnsureClientsSecret(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.clients(t); got != "{}" {
		t.Fatalf("clients with no managed agents = %s", got)
	}

	e.reconcile(t)
	var tok corev1.Secret
	if err := e.c.Get(ctx, types.NamespacedName{Namespace: agentTestNS, Name: "argus-omp-gateway"}, &tok); err != nil {
		t.Fatalf("token Secret: %v", err)
	}
	if owner := metav1.GetControllerOf(&tok); owner == nil || owner.Kind != "OmpAgent" || owner.Name != agentTestName {
		t.Errorf("token owner = %+v", owner)
	}
	sum := sha256.Sum256(tok.Data[agentTestTokenKey])
	want := `{"team.argus":{"tokenSha256":"` + hex.EncodeToString(sum[:]) + `","models":["anthropic/*"]}}`
	if got := e.clients(t); got != want {
		t.Fatalf("clients = %s\nwant      %s", got, want)
	}
	if e.ready(t).Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v", e.ready(t))
	}

	// A second reconcile changes neither the token nor the file, and is no write.
	write := e.gw.LastClientsWrite()
	if write.IsZero() {
		t.Fatal("LastClientsWrite not recorded")
	}
	e.gw.Now = func() time.Time { return write.Add(time.Hour) }
	e.reconcile(t)
	if !e.gw.LastClientsWrite().Equal(write) {
		t.Error("no-op reconcile moved LastClientsWrite")
	}
	var again corev1.Secret
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(&tok), &again); err != nil || string(again.Data[agentTestTokenKey]) != string(tok.Data[agentTestTokenKey]) {
		t.Errorf("token rotated: %v", err)
	}
}

func TestOmpAgentFinalizerCleanup(t *testing.T) {
	ctx := context.Background()
	agent := agentTestAgent(func(s *ompv1alpha1.OmpAgentSpec) {
		s.Model.Gateway = &ompv1alpha1.GatewaySpec{URL: agentTestGatewayURL}
	})
	other := agentTestAgent(func(s *ompv1alpha1.OmpAgentSpec) {
		s.Model.Gateway = &ompv1alpha1.GatewaySpec{URL: agentTestGatewayURL}
	})
	other.Name, other.UID = agentTestOther, "uid-other"
	e := newAgentTestEnv(t, true, agent, other)

	e.reconcile(t)
	if _, err := e.r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: agentTestNS, Name: agentTestOther}}); err != nil {
		t.Fatal(err)
	}
	if got := e.clients(t); !strings.Contains(got, "team.argus") || !strings.Contains(got, "team.other") {
		t.Fatalf("clients = %s", got)
	}
	if err := e.store.PutToolchain(agentTestNS, agentTestName, "hash1", strings.NewReader("tc")); err != nil {
		t.Fatal(err)
	}
	if err := e.store.PutContinuity(agentTestNS, agentTestName, "0123456789abcdef", strings.NewReader("cont")); err != nil {
		t.Fatal(err)
	}
	if err := e.store.PutToolchain(agentTestNS, agentTestOther, "hash2", strings.NewReader("tc")); err != nil {
		t.Fatal(err)
	}

	if err := e.c.Delete(ctx, e.agent(t)); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)

	var gone ompv1alpha1.OmpAgent
	if err := e.c.Get(ctx, types.NamespacedName{Namespace: agentTestNS, Name: agentTestName}, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("agent still there after finalizer pass: %v %v", err, gone.Finalizers)
	}
	if got := e.clients(t); strings.Contains(got, "team.argus") || !strings.Contains(got, "team.other") {
		t.Errorf("clients entry not removed alone: %s", got)
	}
	if _, err := e.store.OpenToolchain(agentTestNS, agentTestName, "hash1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("toolchain not removed: %v", err)
	}
	if _, err := e.store.OpenContinuity(agentTestNS, agentTestName, "0123456789abcdef"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("continuity not removed: %v", err)
	}
	if f, err := e.store.OpenToolchain(agentTestNS, agentTestOther, "hash2"); err != nil {
		t.Errorf("other agent's toolchain removed: %v", err)
	} else {
		_ = f.Close()
	}
}

func TestOmpAgentNamespaceNotAllowedGetsNoFinalizer(t *testing.T) {
	e := newAgentTestEnv(t, true, agentTestAgent(nil))
	e.opts.AgentNamespaces = []string{agentTestOther}
	e.reconcile(t)
	if got := e.agent(t); len(got.Finalizers) != 0 {
		t.Errorf("finalizers = %v", got.Finalizers)
	}
	if c := e.ready(t); c.Reason != "NamespaceNotAllowed" {
		t.Errorf("Ready = %+v", c)
	}
}

func TestOmpAgentActionWatchMapping(t *testing.T) {
	withAction := func(name string, actions ...string) *ompv1alpha1.OmpAgent {
		a := agentTestAgent(func(s *ompv1alpha1.OmpAgentSpec) {
			for _, n := range actions {
				s.Actions = append(s.Actions, ompv1alpha1.LocalObjectRef{Name: n})
			}
		})
		a.Name = name
		return a
	}
	elsewhere := withAction(agentTestName, agentTestRestart)
	elsewhere.Namespace = "elsewhere"
	e := newAgentTestEnv(t, false, withAction("a", agentTestRestart), withAction("b", agentTestRestart, "silence"), withAction("c", "silence"), withAction("d"), elsewhere)
	reqs := e.r.agentsForAction(context.Background(), agentTestAction(nil))
	got := make([]string, 0, len(reqs))
	for _, r := range reqs {
		got = append(got, r.Namespace+"/"+r.Name)
	}
	if len(got) != 2 || !agentTestHas(got, "team/a") || !agentTestHas(got, "team/b") {
		t.Errorf("requests = %v, want team/a and team/b", got)
	}
}
