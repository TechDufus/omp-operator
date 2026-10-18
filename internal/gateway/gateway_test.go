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

package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
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
)

const (
	opNS        = "omp-system"
	gwOwnSecret = "mine"
	gwTokenName = "argus-omp-gateway"
	gwAgent     = "argus"
	gwNS        = "team"
)

func testScheme(t *testing.T) *runtime.Scheme {
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

func testOptions() *config.Options {
	return &config.Options{OperatorNamespace: opNS, GatewayClientsSecret: "omp-auth-clients"}
}

func managedAgent(ns, name, model string) *ompv1alpha1.OmpAgent {
	return &ompv1alpha1.OmpAgent{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(ns + "-" + name)},
		Spec: ompv1alpha1.OmpAgentSpec{Model: ompv1alpha1.ModelSpec{
			Default: model, Gateway: &ompv1alpha1.GatewaySpec{URL: "http://omp-auth:4000"},
		}},
	}
}

func newManager(t *testing.T, objs ...client.Object) (*Manager, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).WithStatusSubresource(&ompv1alpha1.OmpAgent{}).Build()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	return &Manager{Client: c, APIReader: c, Options: testOptions(), Now: func() time.Time { return now }}, c
}

func clientsData(t *testing.T, c client.Client) string {
	t.Helper()
	var s corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: opNS, Name: "omp-auth-clients"}, &s); err != nil {
		t.Fatal(err)
	}
	return string(s.Data[ClientsKey])
}

func TestTokenRef(t *testing.T) {
	managed := testOptions()
	unmanaged := &config.Options{OperatorNamespace: opNS}
	own := managedAgent("a", "x", "anthropic/claude")
	own.Spec.Model.Gateway.TokenSecretRef = &ompv1alpha1.SecretKeyRef{Name: gwOwnSecret, Key: "k"}
	noGW := managedAgent("a", "x", "anthropic/claude")
	noGW.Spec.Model.Gateway = nil

	tests := []struct {
		name     string
		agent    *ompv1alpha1.OmpAgent
		opts     *config.Options
		wantName string
		wantKey  string
		wantOK   bool
		managed  bool
	}{
		{"managed", managedAgent("a", "x", "anthropic/claude"), managed, "x-omp-gateway", "token", true, true},
		{"managed disabled", managedAgent("a", "x", "anthropic/claude"), unmanaged, "", "", false, false},
		{"own secret", own, managed, gwOwnSecret, "k", true, false},
		{"own secret, managed disabled", own, unmanaged, gwOwnSecret, "k", true, false},
		{"no gateway", noGW, managed, "", "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, key, ok := TokenRef(tt.agent, tt.opts)
			if name != tt.wantName || key != tt.wantKey || ok != tt.wantOK {
				t.Errorf("TokenRef = %q %q %v", name, key, ok)
			}
			if got := Managed(tt.agent, tt.opts); got != tt.managed {
				t.Errorf("Managed = %v, want %v", got, tt.managed)
			}
		})
	}
}

func TestAllowedModels(t *testing.T) {
	a := managedAgent("a", "x", "anthropic/claude-sonnet-5-5")
	if got := AllowedModels(a); len(got) != 1 || got[0] != "anthropic/*" {
		t.Errorf("default = %v", got)
	}
	a.Spec.Model.Gateway.AllowedModels = []string{"openai/gpt-5", "anthropic/claude-haiku"}
	if got := AllowedModels(a); len(got) != 2 || got[0] != "openai/gpt-5" {
		t.Errorf("explicit = %v", got)
	}
}

func TestEnsureClientsSecret(t *testing.T) {
	m, c := newManager(t)
	ctx := context.Background()
	if !m.LastClientsWrite().IsZero() {
		t.Fatal("LastClientsWrite must be zero before the first write")
	}
	if err := m.EnsureClientsSecret(ctx); err != nil {
		t.Fatal(err)
	}
	if got := clientsData(t, c); got != "{}" {
		t.Errorf("clients = %s", got)
	}
	first := m.LastClientsWrite()
	if first.IsZero() {
		t.Error("creating the Secret must count as a write")
	}
	// An existing Secret is left alone.
	m.Now = func() time.Time { return first.Add(time.Hour) }
	if err := m.EnsureClientsSecret(ctx); err != nil {
		t.Fatal(err)
	}
	if !m.LastClientsWrite().Equal(first) {
		t.Error("existing Secret must not be rewritten")
	}

	off := &Manager{Client: c, APIReader: c, Options: &config.Options{OperatorNamespace: opNS}}
	if err := off.EnsureClientsSecret(ctx); err != nil || off.Sync(ctx) != nil {
		t.Fatal("disabled managers are no-ops")
	}
}

func TestSync(t *testing.T) {
	ctx := context.Background()
	agent := managedAgent(gwNS, gwAgent, "anthropic/claude-sonnet-5-5")
	m, c := newManager(t, agent)

	// No managed agents with a token yet: the Secret is created holding {}.
	if err := m.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := clientsData(t, c); got != "{}" {
		t.Fatalf("clients with no token = %s", got)
	}
	created := m.LastClientsWrite()
	if created.IsZero() {
		t.Fatal("expected a write")
	}

	// An unchanged Sync is not a write.
	m.Now = func() time.Time { return created.Add(time.Minute) }
	if err := m.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if !m.LastClientsWrite().Equal(created) {
		t.Fatal("no-op Sync moved LastClientsWrite")
	}

	// Token creation, then the exact JSON.
	if err := m.EnsureToken(ctx, agent); err != nil {
		t.Fatal(err)
	}
	var tok corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: gwNS, Name: gwTokenName}, &tok); err != nil {
		t.Fatal(err)
	}
	token := string(tok.Data[TokenKey])
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(token) {
		t.Fatalf("token %q is not 32 hex-encoded bytes", token)
	}
	if ref := metav1.GetControllerOf(&tok); ref == nil || ref.Name != gwAgent || ref.Kind != "OmpAgent" {
		t.Fatalf("token Secret owner = %+v", ref)
	}
	if err := m.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(token))
	want := `{"team.argus":{"tokenSha256":"` + hex.EncodeToString(sum[:]) + `","models":["anthropic/*"]}}`
	if got := clientsData(t, c); got != want {
		t.Fatalf("clients = %s\nwant      %s", got, want)
	}
	written := m.LastClientsWrite()
	if !written.Equal(created.Add(time.Minute)) {
		t.Fatalf("LastClientsWrite = %v", written)
	}

	// Never rotated; idempotent.
	if err := m.EnsureToken(ctx, agent); err != nil {
		t.Fatal(err)
	}
	var again corev1.Secret
	if err := c.Get(ctx, client.ObjectKeyFromObject(&tok), &again); err != nil {
		t.Fatal(err)
	}
	if string(again.Data[TokenKey]) != token {
		t.Fatal("token rotated")
	}
	m.Now = func() time.Time { return written.Add(time.Hour) }
	if err := m.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if !m.LastClientsWrite().Equal(written) {
		t.Fatal("no-op Sync moved LastClientsWrite")
	}

	// Other keys of the clients Secret survive; JSON is valid and parseable.
	var parsed map[string]struct {
		TokenSha256 string   `json:"tokenSha256"`
		Models      []string `json:"models"`
	}
	if err := json.Unmarshal([]byte(clientsData(t, c)), &parsed); err != nil || len(parsed) != 1 {
		t.Fatalf("clients not parseable: %v %v", err, parsed)
	}

	// An agent with its own token Secret, one outside the served namespaces and a
	// terminating one are not clients; changing allowedModels rewrites the entry.
	own := managedAgent(gwNS, "own", "openai/gpt-5")
	own.Spec.Model.Gateway.TokenSecretRef = &ompv1alpha1.SecretKeyRef{Name: "s", Key: "k"}
	if err := c.Create(ctx, own); err != nil {
		t.Fatal(err)
	}
	var fresh ompv1alpha1.OmpAgent
	if err := c.Get(ctx, types.NamespacedName{Namespace: gwNS, Name: gwAgent}, &fresh); err != nil {
		t.Fatal(err)
	}
	fresh.Spec.Model.Gateway.AllowedModels = []string{"anthropic/claude-haiku"}
	if err := c.Update(ctx, &fresh); err != nil {
		t.Fatal(err)
	}
	if err := m.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	want = `{"team.argus":{"tokenSha256":"` + hex.EncodeToString(sum[:]) + `","models":["anthropic/claude-haiku"]}}`
	if got := clientsData(t, c); got != want {
		t.Fatalf("clients = %s\nwant      %s", got, want)
	}

	m.Options.AgentNamespaces = []string{"elsewhere"}
	if err := m.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := clientsData(t, c); got != "{}" {
		t.Fatalf("namespace filter: clients = %s", got)
	}
}

func TestSyncSkipsDeletingAgents(t *testing.T) {
	ctx := context.Background()
	agent := managedAgent(gwNS, gwAgent, "anthropic/claude")
	agent.Finalizers = []string{ompv1alpha1.FinalizerCleanup}
	m, c := newManager(t, agent)
	if err := m.EnsureToken(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := m.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := clientsData(t, c); got == "{}" {
		t.Fatal("agent missing from clients")
	}
	if err := c.Delete(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := m.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := clientsData(t, c); got != "{}" {
		t.Fatalf("terminating agent still in clients: %s", got)
	}
}

func TestEnsureTokenRepairsAndReplacesStale(t *testing.T) {
	ctx := context.Background()
	agent := managedAgent(gwNS, gwAgent, "anthropic/claude")
	empty := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: gwNS, Name: gwTokenName},
	}
	m, c := newManager(t, agent, empty)
	if err := m.EnsureToken(ctx, agent); err != nil {
		t.Fatal(err)
	}
	var s corev1.Secret
	if err := c.Get(ctx, client.ObjectKeyFromObject(empty), &s); err != nil || len(s.Data[TokenKey]) != 64 {
		t.Fatalf("token not filled in: %v %d", err, len(s.Data[TokenKey]))
	}

	// A Secret controlled by a previous incarnation of the agent is replaced.
	stale := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: gwNS, Name: gwTokenName,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: ompv1alpha1.GroupVersion.String(), Kind: "OmpAgent", Name: gwAgent, UID: "old", Controller: new(true),
			}},
		},
		Data: map[string][]byte{TokenKey: []byte("old-token")},
	}
	m2, c2 := newManager(t, agent, stale)
	if err := m2.EnsureToken(ctx, agent); err != nil {
		t.Fatal(err)
	}
	var got corev1.Secret
	if err := c2.Get(ctx, client.ObjectKeyFromObject(stale), &got); err != nil {
		t.Fatal(err)
	}
	if string(got.Data[TokenKey]) == "old-token" || metav1.GetControllerOf(&got).UID != agent.UID {
		t.Fatalf("stale Secret kept: %+v", got.OwnerReferences)
	}
}
