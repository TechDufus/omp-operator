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
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/store"
	"github.com/TechDufus/omp-operator/internal/threads"
)

const (
	// SessionKeySecret is the Secret holding the key that signs dashboard cookies.
	SessionKeySecret = "omp-operator-dashboard-session"
	// SessionKeyKey is the Secret key of the signing key.
	SessionKeyKey = "key"
	sessionKeyLen = 32

	// defaultCSP applies to every page except the transcript.
	defaultCSP = "default-src 'none'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	// transcriptCSP lets the exported transcript run its own scripts inside an opaque origin.
	transcriptCSP = "sandbox allow-scripts allow-popups"
)

// Server is the read-only dashboard.
type Server struct {
	// Client is the cached reader used for lists.
	Client client.Reader
	// APIReader reads single objects without the cache.
	APIReader client.Reader
	Store     *store.Store
	// Threads is the turn log the thread page reads; nil means one over Store.
	Threads *threads.Log
	Options *config.Options
	Access  *Access
	// Key signs the session and login-state cookies.
	Key []byte
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	oidc oidcClient
}

// EnsureSessionKey returns the dashboard's cookie-signing key, creating the Secret
// omp-operator-dashboard-session (key "key", 32 random bytes) in namespace when it is missing.
func EnsureSessionKey(ctx context.Context, c client.Client, apiReader client.Reader, namespace string) ([]byte, error) {
	name := types.NamespacedName{Namespace: namespace, Name: SessionKeySecret}
	read := func() ([]byte, error) {
		var sec corev1.Secret
		if err := apiReader.Get(ctx, name, &sec); err != nil {
			return nil, err
		}
		key := sec.Data[SessionKeyKey]
		if len(key) == 0 {
			return nil, fmt.Errorf("secret %s has no %q key", name, SessionKeyKey)
		}
		return key, nil
	}
	key, err := read()
	if err == nil {
		return key, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	key = make([]byte, sessionKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      SessionKeySecret,
			Labels:    map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue},
		},
		Data: map[string][]byte{SessionKeyKey: key},
	}
	if err := c.Create(ctx, sec); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return read()
		}
		return nil, err
	}
	return key, nil
}

type principalKey struct{}

// Handler returns the dashboard's HTTP handler.
func (s *Server) Handler() http.Handler {
	if s.Access == nil {
		s.Access = &Access{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", s.handleIndex)
	mux.HandleFunc("/agents/{ns}/{name}", s.handleAgent)
	mux.HandleFunc("/triggers/{ns}/{name}", s.handleTrigger)
	mux.HandleFunc("/sessions/{ns}/{name}", s.handleSession)
	mux.HandleFunc("/threads/{ns}/{name}", s.handleThread)
	mux.HandleFunc("/sessions/{ns}/{name}/transcript", s.handleTranscript)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { notFound(w) })

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w.Header())
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			textError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = fmt.Fprintln(w, "ok")
			return
		}
		if !s.Access.configured() {
			textError(w, http.StatusServiceUnavailable, "dashboard access not configured")
			return
		}
		if s.Access.OIDC != nil {
			switch r.URL.Path {
			case loginPath:
				s.handleLogin(w, r)
				return
			case callbackPath:
				s.handleCallback(w, r)
				return
			case logoutPath:
				s.handleLogout(w, r)
				return
			}
		}
		res := s.authenticate(r)
		if res.p == nil {
			s.unauthenticated(w, r, res)
			return
		}
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, res.p)))
	})
}

func setSecurityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", defaultCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
}

func notFound(w http.ResponseWriter) { textError(w, http.StatusNotFound, "404 page not found") }

// who returns the authenticated user of a routed request.
func who(r *http.Request) *principal {
	p, _ := r.Context().Value(principalKey{}).(*principal)
	if p == nil {
		return &principal{}
	}
	return p
}

// canSee reports whether p may see anything in ns.
func (s *Server) canSee(p *principal, ns string) bool {
	if s.Options != nil && !s.Options.NamespaceAllowed(ns) {
		return false
	}
	return s.Access.allows(p, ns)
}

// scopeOptions turns p's grants into list scopes: one cluster-wide list for a
// "*" grant, else one list per granted namespace. Results must still pass canSee.
func (s *Server) scopeOptions(p *principal) [][]client.ListOption {
	all, nss := s.Access.scope(p)
	if all {
		return [][]client.ListOption{nil}
	}
	out := make([][]client.ListOption, 0, len(nss))
	for _, ns := range nss {
		if s.Options == nil || s.Options.NamespaceAllowed(ns) {
			out = append(out, []client.ListOption{client.InNamespace(ns)})
		}
	}
	return out
}

// reader is the cached reader, or the API reader when none is set.
func (s *Server) reader() client.Reader {
	if s.Client != nil {
		return s.Client
	}
	return s.APIReader
}

// fresh is the API reader, or the cached reader when none is set.
func (s *Server) fresh() client.Reader {
	if s.APIReader != nil {
		return s.APIReader
	}
	return s.Client
}
