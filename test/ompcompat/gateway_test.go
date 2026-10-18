//go:build ompcompat

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

package ompcompat

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/agentapi"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/gateway"
)

// syncBuffer is a bytes.Buffer safe for a process's output goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startService runs omp with args (HOME relocated) until the test ends and waits for url to answer 200.
func startService(t *testing.T, home, healthURL string, extraEnv []string, args ...string) {
	t.Helper()
	cmd := exec.Command(ompBin, args...)
	cmd.Dir = home
	cmd.Env = append(append(os.Environ(), "HOME="+home), extraEnv...)
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			t.Fatalf("omp %v exited early (%v):\n%s", args, cmd.ProcessState, out.String())
		default:
		}
		if code, err := httpStatus(healthURL); err == nil && code == http.StatusOK {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("omp %v never answered %s:\n%s", args, healthURL, out.String())
}

// clientsFile renders the gateway's clients file with the operator's own code: the clients
// Secret that gateway.Manager.Sync writes for the given managed agents. It returns the file's
// content and each agent's token.
func clientsFile(t *testing.T, agents ...*ompv1alpha1.OmpAgent) (clients []byte, tokens map[string]string) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := ompv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&ompv1alpha1.OmpAgent{})
	for _, a := range agents {
		builder = builder.WithObjects(a)
	}
	c := builder.Build()
	opts := &config.Options{OperatorNamespace: "omp-system", GatewayClientsSecret: "omp-auth-clients"}
	mgr := &gateway.Manager{Client: c, APIReader: c, Options: opts}
	ctx := context.Background()
	for _, a := range agents {
		if err := mgr.EnsureToken(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if err := mgr.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "omp-system", Name: "omp-auth-clients"}, &s); err != nil {
		t.Fatal(err)
	}
	tokens = map[string]string{}
	for _, a := range agents {
		var tok corev1.Secret
		key := types.NamespacedName{Namespace: a.Namespace, Name: gateway.TokenSecretName(a.Name)}
		if err := c.Get(ctx, key, &tok); err != nil {
			t.Fatal(err)
		}
		tokens[a.Name] = string(tok.Data[gateway.TokenKey])
	}
	return s.Data[gateway.ClientsKey], tokens
}

func managedAgent(name, model string) *ompv1alpha1.OmpAgent {
	return &ompv1alpha1.OmpAgent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: name, UID: types.UID("uid-" + name)},
		Spec: ompv1alpha1.OmpAgentSpec{Model: ompv1alpha1.ModelSpec{
			Default: model, Gateway: &ompv1alpha1.GatewaySpec{URL: "http://omp-auth:4000"},
		}},
	}
}

func getStatus(t *testing.T, url, bearer string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestAuthGateway is plan test 4: omp's auth-gateway starts against a throwaway broker with the
// clients file the operator writes, with no agents (`{}`) and with a managed agent, answers
// /healthz, enforces the client token and its limits, and a model routed through it with a wrong
// token ends a driver run as an auth failure.
func TestAuthGateway(t *testing.T) {
	home := t.TempDir()
	brokerAddr := freeAddr(t)
	startService(t, home, "http://"+brokerAddr+"/v1/healthz", nil, "auth-broker", "serve", "--bind="+brokerAddr)
	if info, err := os.Stat(filepath.Join(home, ".omp", "auth-broker.token")); err != nil || info.Size() == 0 {
		t.Fatalf("broker did not create its token file: %v", err)
	}
	brokerEnv := []string{"OMP_AUTH_BROKER_URL=http://" + brokerAddr}

	serve := func(t *testing.T, clients []byte) string {
		t.Helper()
		file := filepath.Join(t.TempDir(), "clients.json")
		writeFile(t, file, string(clients))
		addr := freeAddr(t)
		startService(t, home, "http://"+addr+"/healthz", brokerEnv,
			"auth-gateway", "serve", "--bind="+addr, "--clients="+file)
		return "http://" + addr
	}

	t.Run("no managed agents", func(t *testing.T) {
		clients, _ := clientsFile(t)
		if string(clients) != "{}" {
			t.Fatalf("clients file = %q, want {}", clients)
		}
		url := serve(t, clients)
		if got := getStatus(t, url+"/healthz", ""); got != http.StatusOK {
			t.Errorf("/healthz = %d", got)
		}
		if got := getStatus(t, url+"/v1/models", "no-such-token"); got != http.StatusUnauthorized {
			t.Errorf("/v1/models with an unknown token = %d, want 401", got)
		}
	})

	t.Run("managed agent", func(t *testing.T) {
		clients, tokens := clientsFile(t, managedAgent("argus", flagsModel))
		t.Logf("clients file: %s", clients)
		url := serve(t, clients)
		token := tokens["argus"]
		if got := getStatus(t, url+"/healthz", ""); got != http.StatusOK {
			t.Errorf("/healthz = %d", got)
		}
		if got := getStatus(t, url+"/v1/models", token); got != http.StatusOK {
			t.Errorf("/v1/models with the managed token = %d, want 200", got)
		}
		if got := getStatus(t, url+"/v1/models", token+"x"); got != http.StatusUnauthorized {
			t.Errorf("/v1/models with a wrong token = %d, want 401", got)
		}
		if got := getStatus(t, url+"/v1/usage", token); got != http.StatusForbidden {
			t.Errorf("/v1/usage with a client token = %d, want 403 (client tokens are not gateway-wide)", got)
		}

		// A model routed through the real gateway with a wrong token is an auth failure.
		done := runGatewayModel(t, url, token+"x")
		if done.Outcome != agentapi.OutcomeAuth {
			t.Errorf("outcome with a wrong gateway token = %q (%s), want %q", done.Outcome, done.Error, agentapi.OutcomeAuth)
		}
		// With the right token the gateway has no credentials to serve the model.
		done = runGatewayModel(t, url, token)
		if done.Outcome == agentapi.OutcomeCompleted {
			t.Errorf("a gateway without credentials completed a run: %+v", done)
		}
	})
}
