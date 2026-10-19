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
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/store"
	"github.com/TechDufus/omp-operator/internal/threads"
)

const (
	// maxSmallBody bounds JSON request bodies (prepared, events, complete, unchanged, action args).
	maxSmallBody = 1 << 20
	// tokenRefreshInterval is the least time between re-reads of a Secret after a token mismatch,
	// so a recreated Secret is picked up without letting unauthenticated callers drive API reads.
	tokenRefreshInterval = 30 * time.Second
	// maxCachedTokens bounds the in-memory token cache.
	maxCachedTokens = 4096
)

var log = logf.Log.WithName("agentapi")

// Server is the agent API (default :8083) that session and toolsync pods call.
type Server struct {
	// Client writes status (session, agent). It may be a cached client.
	Client client.Client
	// APIReader reads Secrets, sessions, agents and Flux sources uncached.
	APIReader client.Reader
	Store     *store.Store
	Options   *config.Options
	// Runner executes host tool calls and describes the tools; *actions.Runner.
	Runner ActionRunner
	// Threads is the turn log of cloud threads; the thread routes use it.
	Threads *threads.Log
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	tokens tokenCache
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Handler returns the API's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	const sess = "/v1/sessions/{ns}/{name}"
	const agent = "/v1/agents/{ns}/{name}"

	mux.HandleFunc("GET "+sess+"/plan", s.session(s.handleSessionPlan))
	mux.HandleFunc("GET "+sess, s.session(s.handleSessionConfig))
	mux.HandleFunc("POST "+sess+"/prepared", s.session(s.handlePrepared))
	mux.HandleFunc("GET "+sess+"/toolchain", s.session(s.handleSessionToolchain))
	mux.HandleFunc("GET "+sess+"/continuity", s.session(s.handleGetContinuity))
	mux.HandleFunc("PUT "+sess+"/continuity", s.session(s.handlePutContinuity))
	mux.HandleFunc("POST "+sess+"/actions/{tool}", s.session(s.handleAction))
	mux.HandleFunc("POST "+sess+"/events", s.session(s.handleEvents))
	mux.HandleFunc("PUT "+sess+"/transcript", s.session(s.handleTranscript))
	mux.HandleFunc("POST "+sess+"/complete", s.session(s.handleComplete))

	mux.HandleFunc("GET "+sess+"/thread/source", s.threadSession(s.handleThreadSource))
	mux.HandleFunc("GET "+sess+"/thread/result", s.threadSession(s.handleGetThreadResult))
	mux.HandleFunc("PUT "+sess+"/thread/result", s.threadSession(s.handlePutThreadResult))
	mux.HandleFunc("GET "+sess+"/thread/setup", s.threadSession(s.handleThreadSetup))
	mux.HandleFunc("GET "+sess+"/thread/next", s.threadSession(s.handleThreadNext))
	mux.HandleFunc("GET "+sess+"/thread/control", s.threadSession(s.handleThreadControl))
	mux.HandleFunc("POST "+sess+"/thread/turns/{id}/complete", s.threadSession(s.handleThreadTurnComplete))
	mux.HandleFunc("GET "+sess+"/toolchains/{hash}", s.threadSession(s.handleGetCloudToolchain))
	mux.HandleFunc("PUT "+sess+"/toolchains/{hash}", s.threadSession(s.handlePutCloudToolchain))

	mux.HandleFunc("GET "+agent+"/plan", s.agent(s.handleAgentPlan))
	mux.HandleFunc("GET "+agent+"/toolchain", s.agent(s.handleAgentToolchain))
	mux.HandleFunc("POST "+agent+"/toolchain/unchanged", s.agent(s.handleToolchainUnchanged))
	mux.HandleFunc("PUT "+agent+"/toolchain/{hash}", s.agent(s.handleToolchainUpload))
	return mux
}

// sessionReq is an authenticated request for a Running session.
type sessionReq struct {
	ns, name string
	session  *v1alpha1.OmpSession
}

// agentReq is an authenticated toolsync request.
type agentReq struct {
	ns, name string
}

// session authenticates with the session's token and requires the session to be Running.
func (s *Server) session(h func(http.ResponseWriter, *http.Request, *sessionReq)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ns, name := r.PathValue("ns"), r.PathValue("name")
		if !s.authorize(w, r, ns, SessionTokenSecretName(name)) {
			return
		}
		var sess v1alpha1.OmpSession
		if err := s.APIReader.Get(r.Context(), types.NamespacedName{Namespace: ns, Name: name}, &sess); err != nil {
			if apierrors.IsNotFound(err) {
				// A token for a session that no longer exists proves nothing.
				s.tokens.forget(ns, SessionTokenSecretName(name))
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			s.internalError(w, "get session", err)
			return
		}
		if sess.Status.Phase != v1alpha1.SessionPhaseRunning {
			writeError(w, http.StatusConflict, "session is not running")
			return
		}
		h(w, r, &sessionReq{ns: ns, name: name, session: &sess})
	}
}

// agent authenticates with the agent's toolsync token.
func (s *Server) agent(h func(http.ResponseWriter, *http.Request, *agentReq)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ns, name := r.PathValue("ns"), r.PathValue("name")
		if !s.authorize(w, r, ns, ToolsyncTokenSecretName(name)) {
			return
		}
		h(w, r, &agentReq{ns: ns, name: name})
	}
}

// authorize checks the bearer token against Secret secretName in ns; it writes the
// error response and returns false on failure.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, ns, secretName string) bool {
	if !s.Options.NamespaceAllowed(ns) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	presented, ok := bearer(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	good, err := s.tokens.verify(r.Context(), s.APIReader, s.now(), ns, secretName, presented)
	if err != nil {
		s.internalError(w, "read token secret", err)
		return false
	}
	if !good {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

func bearer(r *http.Request) (string, bool) {
	const prefix = "bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	return tok, tok != ""
}

// tokenCache keeps tokens in memory after their first read.
type tokenCache struct {
	mu sync.Mutex
	m  map[string]cachedToken
}

type cachedToken struct {
	token  string
	readAt time.Time
}

func tokenKey(ns, secret string) string { return ns + "/" + secret }

func (c *tokenCache) get(key string) (cachedToken, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.m[key]
	return t, ok
}

func (c *tokenCache) put(key string, t cachedToken) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= maxCachedTokens {
		c.m = make(map[string]cachedToken)
	}
	c.m[key] = t
}

func (c *tokenCache) forget(ns, secret string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, tokenKey(ns, secret))
}

func tokensEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// verify compares presented with the token in Secret ns/secret. The Secret is read once and the
// token cached; after a mismatch the Secret is re-read at most every tokenRefreshInterval.
func (c *tokenCache) verify(ctx context.Context, reader client.Reader, now time.Time, ns, secret, presented string) (bool, error) {
	key := tokenKey(ns, secret)
	cached, ok := c.get(key)
	if ok {
		if tokensEqual(cached.token, presented) {
			return true, nil
		}
		if now.Sub(cached.readAt) < tokenRefreshInterval {
			return false, nil
		}
	}
	var sec corev1.Secret
	if err := reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: secret}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			c.forget(ns, secret)
			return false, nil
		}
		return false, err
	}
	token := strings.TrimSpace(string(sec.Data[TokenKey]))
	if token == "" {
		c.forget(ns, secret)
		return false, nil
	}
	c.put(key, cachedToken{token: token, readAt: now})
	return tokensEqual(token, presented), nil
}

// ---- response helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// internalError logs err and answers 500 without detail.
func (s *Server) internalError(w http.ResponseWriter, what string, err error) {
	log.Error(err, "agent API request failed", "step", what)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// decodeBody reads a JSON body of at most maxSmallBody bytes into v; it writes the error response on failure.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxSmallBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		if tooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
		}
		return false
	}
	return true
}

func tooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe) || errors.Is(err, store.ErrTooLarge)
}

// truncate cuts s to at most n characters.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
