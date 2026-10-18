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

// Package hooks serves POST /hooks/{namespace}/{trigger}: it authenticates the
// request against the trigger's Secret, parses the payload with the trigger's
// adapter, applies the episode, dedupe and budget rules and creates OmpSessions.
package hooks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/adapters"
	"github.com/TechDufus/omp-operator/internal/celx"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/hookauth"
	"github.com/TechDufus/omp-operator/internal/metrics"
	"github.com/TechDufus/omp-operator/internal/notify"
)

const (
	// MaxBodyBytes is the largest accepted request body.
	MaxBodyBytes = 1 << 20

	// PathPrefix is the URL prefix of every hook.
	PathPrefix = "/hooks/"

	requestTimeout = 60 * time.Second
)

var log = ctrl.Log.WithName("hooks")

// Handler serves webhook requests. Client may be cached; every read that gates a
// decision (the trigger's state, the agent's sessions, Secrets) goes through
// APIReader. The zero value of the unexported fields is ready to use.
type Handler struct {
	Client    client.Client
	APIReader client.Reader
	Options   *config.Options
	CEL       *celx.Cache
	// Notify delivers cap notices; nil disables them.
	Notify *notify.Dispatcher
	// Now defaults to time.Now.
	Now func() time.Time

	locks   sync.Map // "<namespace>/<agent>" -> *sync.Mutex
	notices sync.Map // cap notices already dispatched by this process
	wg      sync.WaitGroup
}

// Wait blocks until the notices the handler dispatched in the background have
// been delivered. Tests and shutdown use it.
func (h *Handler) Wait() { h.wg.Wait() }

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Handler) namespaceAllowed(ns string) bool {
	return h.Options == nil || h.Options.NamespaceAllowed(ns)
}

func (h *Handler) compiler(t *v1alpha1.OmpTrigger) celx.Compiler {
	if h.CEL == nil {
		return celx.Compile
	}
	return h.CEL.Compiler(t.Namespace, t.Name, t.Generation)
}

// ExpressionsOf returns the raw CEL strings of a webhook trigger.
func ExpressionsOf(t *v1alpha1.OmpTrigger) adapters.Expressions {
	w := t.Spec.Webhook
	if w == nil {
		return adapters.Expressions{}
	}
	x := adapters.Expressions{AlertFilter: w.AlertFilter, Filter: w.Filter}
	if m := w.Mapping; m != nil {
		x.Mapping = &adapters.MappingExpressions{
			Events: m.Events, Key: m.Key, Title: m.Title,
			Severity: m.Severity, Status: m.Status, Fingerprint: m.Fingerprint,
		}
	}
	return x
}

// Enabled reports whether the trigger is switched on (the default).
func Enabled(t *v1alpha1.OmpTrigger) bool {
	return t.Spec.Enabled == nil || *t.Spec.Enabled
}

// ServeHTTP handles POST /hooks/{namespace}/{trigger}.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ns, name, ok := parsePath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		if _, tooBig := errors.AsType[*http.MaxBytesError](err); tooBig {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "cannot read request body", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), requestTimeout)
	defer cancel()

	t := &v1alpha1.OmpTrigger{}
	err = h.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, t)
	switch {
	case apierrors.IsNotFound(err):
		unauthorized(w)
		return
	case err != nil:
		log.Error(err, "reading trigger", "namespace", ns, "trigger", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if t.Spec.Webhook == nil || !h.namespaceAllowed(ns) {
		unauthorized(w)
		return
	}
	authn, err := h.authenticator(ctx, t)
	if err != nil || !authn.Authenticate(r, body, h.now()) {
		unauthorized(w)
		return
	}
	if !Enabled(t) {
		http.Error(w, "trigger is disabled", http.StatusConflict)
		return
	}
	if !meta.IsStatusConditionTrue(t.Status.Conditions, v1alpha1.ConditionReady) {
		http.Error(w, "trigger is not ready", http.StatusConflict)
		return
	}
	cfg, err := adapters.Resolve(string(t.Spec.Webhook.Adapter), ExpressionsOf(t), h.compiler(t))
	if err != nil {
		http.Error(w, "trigger is not ready", http.StatusConflict)
		return
	}
	events, stats, err := adapters.Parse(string(t.Spec.Webhook.Adapter), body, cfg)
	if err != nil {
		if errors.Is(err, adapters.ErrConfig) {
			http.Error(w, "trigger is not ready", http.StatusConflict)
			return
		}
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	for range stats.Errors() {
		metrics.WebhookEventsTotal.WithLabelValues(ns, name, resultError).Inc()
	}
	if stats.LastError != nil {
		log.V(1).Info("webhook expression error", "namespace", ns, "trigger", name, "error", stats.LastError.Error())
	}
	agent := &v1alpha1.OmpAgent{}
	if err := h.APIReader.Get(ctx, client.ObjectKey{Namespace: ns, Name: t.Spec.AgentRef.Name}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, "agent not found", http.StatusConflict)
			return
		}
		log.Error(err, "reading agent", "namespace", ns, "agent", t.Spec.AgentRef.Name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	created, err := h.process(ctx, t, agent, cfg, events)
	switch {
	case err != nil:
		http.Error(w, "internal error", http.StatusInternalServerError)
	case created > 0:
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func unauthorized(w http.ResponseWriter) {
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// parsePath splits /hooks/<namespace>/<trigger>.
func parsePath(p string) (ns, name string, ok bool) {
	rest, found := strings.CutPrefix(p, PathPrefix)
	if !found {
		return "", "", false
	}
	ns, name, found = strings.Cut(rest, "/")
	if !found || ns == "" || name == "" || strings.Contains(name, "/") {
		return "", "", false
	}
	return ns, name, true
}

// authenticator builds the trigger's authenticator from its Secret, read
// through the API reader.
func (h *Handler) authenticator(ctx context.Context, t *v1alpha1.OmpTrigger) (hookauth.Authenticator, error) {
	a := t.Spec.Webhook.Auth
	set := 0
	for _, b := range []bool{a.Bearer != nil, a.Basic != nil, a.HMAC != nil} {
		if b {
			set++
		}
	}
	if set != 1 {
		return nil, errors.New("exactly one of bearer, basic or hmac must be set")
	}
	switch {
	case a.Bearer != nil:
		token, err := h.secretValue(ctx, t.Namespace, a.Bearer.SecretRef)
		if err != nil {
			return nil, err
		}
		return hookauth.Bearer{Token: token}, nil
	case a.Basic != nil:
		s, err := h.secret(ctx, t.Namespace, a.Basic.SecretRef.Name)
		if err != nil {
			return nil, err
		}
		return hookauth.Basic{Username: string(s.Data["username"]), Password: string(s.Data["password"])}, nil
	default:
		return NewHMAC(ctx, h.APIReader, t.Namespace, a.HMAC)
	}
}

// NewHMAC builds the HMAC authenticator for spec, applying the field defaults.
// An explicit empty prefix is honoured (hookauth.NewHMAC alone would turn it
// into "sha256=" for the default header).
func NewHMAC(ctx context.Context, reader client.Reader, namespace string, spec *v1alpha1.HMACAuth) (hookauth.Authenticator, error) {
	s := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: spec.SecretRef.Name}, s); err != nil {
		return nil, err
	}
	key := s.Data[spec.SecretRef.Key]
	if len(key) == 0 {
		return nil, fmt.Errorf("secret key %q is empty or missing", spec.SecretRef.Key)
	}
	skew := spec.MaxSkew.Duration
	if spec.Prefix == nil {
		return hookauth.NewHMAC(key, spec.Header, "", spec.TimestampHeader, skew), nil
	}
	h := hookauth.NewHMAC(key, spec.Header, "x", spec.TimestampHeader, skew)
	h.Prefix = *spec.Prefix
	return h, nil
}

func (h *Handler) secret(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	if err := h.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, s); err != nil {
		return nil, err
	}
	return s, nil
}

func (h *Handler) secretValue(ctx context.Context, namespace string, ref v1alpha1.SecretKeyRef) (string, error) {
	s, err := h.secret(ctx, namespace, ref.Name)
	if err != nil {
		return "", err
	}
	v := s.Data[ref.Key]
	if len(v) == 0 {
		return "", fmt.Errorf("secret key %q is empty or missing", ref.Key)
	}
	return string(v), nil
}
