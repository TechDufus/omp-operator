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

// Package gateway manages the operator-held gateway tokens of agents and the
// clients Secret the omp auth-gateway reads them from.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/config"
)

const (
	// TokenKey is the key of the managed token Secret that holds the token.
	TokenKey = "token"
	// ClientsKey is the key of the clients Secret that holds the gateway's clients file.
	ClientsKey = "clients.json"

	tokenBytes = 32
)

// TokenSecretName is the name of the managed token Secret of an agent.
func TokenSecretName(agent string) string { return agent + "-omp-gateway" }

// Managed reports whether the operator holds the agent's gateway token: the agent
// has a gateway without tokenSecretRef and managed tokens are enabled.
func Managed(agent *ompv1alpha1.OmpAgent, opts *config.Options) bool {
	gw := agent.Spec.Model.Gateway
	return gw != nil && gw.TokenSecretRef == nil && opts.GatewayManaged()
}

// TokenRef returns the Secret name and key holding the agent's gateway token: the
// spec's tokenSecretRef, else the managed Secret when managed tokens are enabled.
// ok is false when the agent has no gateway or no token source.
func TokenRef(agent *ompv1alpha1.OmpAgent, opts *config.Options) (name, key string, ok bool) {
	gw := agent.Spec.Model.Gateway
	switch {
	case gw == nil:
		return "", "", false
	case gw.TokenSecretRef != nil:
		return gw.TokenSecretRef.Name, gw.TokenSecretRef.Key, true
	case opts.GatewayManaged():
		return TokenSecretName(agent.Name), TokenKey, true
	}
	return "", "", false
}

// Provider is the provider part of a "provider/id" model name.
func Provider(model string) string {
	p, _, _ := strings.Cut(model, "/")
	return p
}

// AllowedModels is the model patterns the gateway permits the agent: the spec's
// allowedModels, else every model of the default model's provider.
func AllowedModels(agent *ompv1alpha1.OmpAgent) []string {
	if gw := agent.Spec.Model.Gateway; gw != nil && len(gw.AllowedModels) > 0 {
		return slices.Clone(gw.AllowedModels)
	}
	return []string{Provider(agent.Spec.Model.Default) + "/*"}
}

// clientEntry is one client of the gateway's clients file.
type clientEntry struct {
	TokenSha256 string   `json:"tokenSha256"`
	Models      []string `json:"models"`
}

// Manager owns the managed token Secrets and the clients Secret.
type Manager struct {
	// Client performs writes.
	Client client.Client
	// APIReader performs uncached reads.
	APIReader client.Reader
	// Options supplies the operator namespace and the clients Secret name.
	Options *config.Options
	// Now is the clock; defaults to time.Now.
	Now func() time.Time

	mu        sync.Mutex
	lastWrite time.Time
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) clientsKey() types.NamespacedName {
	return types.NamespacedName{Namespace: m.Options.OperatorNamespace, Name: m.Options.GatewayClientsSecret}
}

// LastClientsWrite is when this process last created or changed the clients Secret;
// zero until the first write.
func (m *Manager) LastClientsWrite() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastWrite
}

// EnsureClientsSecret creates the clients Secret containing {} when it is missing.
// It does nothing when managed tokens are disabled.
func (m *Manager) EnsureClientsSecret(ctx context.Context) error {
	if !m.Options.GatewayManaged() {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var s corev1.Secret
	err := m.APIReader.Get(ctx, m.clientsKey(), &s)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	return m.createClients(ctx, []byte("{}"))
}

func (m *Manager) createClients(ctx context.Context, data []byte) error {
	k := m.clientsKey()
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: k.Namespace,
			Name:      k.Name,
			Labels:    map[string]string{ompv1alpha1.LabelManagedBy: ompv1alpha1.ManagedByValue},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{ClientsKey: data},
	}
	if err := m.Client.Create(ctx, s); err != nil {
		return fmt.Errorf("create clients Secret: %w", err)
	}
	m.lastWrite = m.now()
	return nil
}

// EnsureToken makes sure the agent's managed token Secret exists and holds a token.
// The token is generated once and never rotated. A Secret left behind by an earlier
// incarnation of the agent (a controller owner with a different UID) is replaced.
func (m *Manager) EnsureToken(ctx context.Context, agent *ompv1alpha1.OmpAgent) error {
	key := types.NamespacedName{Namespace: agent.Namespace, Name: TokenSecretName(agent.Name)}
	var s corev1.Secret
	err := m.APIReader.Get(ctx, key, &s)
	switch {
	case apierrors.IsNotFound(err):
		return m.createToken(ctx, agent)
	case err != nil:
		return err
	}
	if owner := metav1.GetControllerOf(&s); owner != nil && owner.UID != agent.UID {
		if err := m.Client.Delete(ctx, &s, client.Preconditions{UID: &s.UID}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return m.createToken(ctx, agent)
	}
	if len(s.Data[TokenKey]) > 0 {
		return nil
	}
	tok, err := newToken()
	if err != nil {
		return err
	}
	if s.Data == nil {
		s.Data = map[string][]byte{}
	}
	s.Data[TokenKey] = tok
	return m.Client.Update(ctx, &s)
}

func (m *Manager) createToken(ctx context.Context, agent *ompv1alpha1.OmpAgent) error {
	tok, err := newToken()
	if err != nil {
		return err
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: agent.Namespace,
			Name:      TokenSecretName(agent.Name),
			Labels: map[string]string{
				ompv1alpha1.LabelManagedBy: ompv1alpha1.ManagedByValue,
				ompv1alpha1.LabelAgent:     agent.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{TokenKey: tok},
	}
	if err := controllerutil.SetControllerReference(agent, s, m.Client.Scheme()); err != nil {
		return err
	}
	if err := m.Client.Create(ctx, s); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create gateway token Secret: %w", err)
	}
	return nil
}

// newToken returns 32 random bytes, hex encoded.
func newToken() ([]byte, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return []byte(hex.EncodeToString(b)), nil
}

// Sync rewrites the clients file from every managed agent that already has a token
// Secret, creating the clients Secret when it is missing and writing only on a
// change. Agents being deleted and agents outside the served namespaces are left out.
func (m *Manager) Sync(ctx context.Context) error {
	if !m.Options.GatewayManaged() {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	var agents ompv1alpha1.OmpAgentList
	if err := m.APIReader.List(ctx, &agents); err != nil {
		return fmt.Errorf("list agents: %w", err)
	}
	clients := map[string]clientEntry{}
	for i := range agents.Items {
		a := &agents.Items[i]
		if !a.DeletionTimestamp.IsZero() || !m.Options.NamespaceAllowed(a.Namespace) || !Managed(a, m.Options) {
			continue
		}
		var tok corev1.Secret
		err := m.APIReader.Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: TokenSecretName(a.Name)}, &tok)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read gateway token of %s/%s: %w", a.Namespace, a.Name, err)
		}
		if len(tok.Data[TokenKey]) == 0 {
			continue
		}
		sum := sha256.Sum256(tok.Data[TokenKey])
		clients[a.Namespace+"."+a.Name] = clientEntry{TokenSha256: hex.EncodeToString(sum[:]), Models: AllowedModels(a)}
	}
	data, err := json.Marshal(clients)
	if err != nil {
		return err
	}

	var s corev1.Secret
	err = m.APIReader.Get(ctx, m.clientsKey(), &s)
	if apierrors.IsNotFound(err) {
		return m.createClients(ctx, data)
	}
	if err != nil {
		return err
	}
	if bytes.Equal(s.Data[ClientsKey], data) {
		return nil
	}
	if s.Data == nil {
		s.Data = map[string][]byte{}
	}
	s.Data[ClientsKey] = data
	if err := m.Client.Update(ctx, &s); err != nil {
		return fmt.Errorf("update clients Secret: %w", err)
	}
	m.lastWrite = m.now()
	return nil
}
