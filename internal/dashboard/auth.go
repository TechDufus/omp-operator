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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/oauth2"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	sessionCookie = "omp_session"
	stateCookie   = "omp_oidc"
	// sessionTTL is how long a signed-in session lasts.
	sessionTTL = 12 * time.Hour
	// stateTTL is how long a login attempt may take.
	stateTTL = 10 * time.Minute

	loginPath    = "/auth/login"
	callbackPath = "/auth/callback"
	logoutPath   = "/auth/logout"

	basicRealm = `Basic realm="omp-operator"`

	// oidcTimeout bounds each request to the identity provider.
	oidcTimeout = 15 * time.Second
)

var log = logf.Log.WithName("dashboard")

// sessionClaims is the payload of the omp_session cookie.
type sessionClaims struct {
	U   string   `json:"u"`
	G   []string `json:"g,omitempty"`
	Exp int64    `json:"exp"`
}

// stateClaims is the payload of the login state cookie.
type stateClaims struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"r,omitempty"`
	Exp      int64  `json:"exp"`
}

// sign returns base64url(JSON(v)) + "." + base64url(HMAC-SHA256). The kind
// separates cookie types so one cannot be replayed as another.
func (s *Server) sign(kind string, v any) (string, error) {
	if len(s.Key) == 0 {
		return "", errors.New("dashboard session key is empty")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + base64.RawURLEncoding.EncodeToString(s.mac(kind, payload)), nil
}

func (s *Server) mac(kind, payload string) []byte {
	m := hmac.New(sha256.New, s.Key)
	m.Write([]byte(kind))
	m.Write([]byte{0})
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// verify checks the signature of value and decodes the payload into out.
func (s *Server) verify(kind, value string, out any) bool {
	if len(s.Key) == 0 {
		return false
	}
	payload, sig, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.mac(kind, payload)) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, out) == nil
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func setCookie(w http.ResponseWriter, name, value, path string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		MaxAge:   int(maxAge / time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: path, MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
}

// authResult is the outcome of authenticating a request.
type authResult struct {
	p        *principal
	badBasic bool
}

// authenticate resolves the request's user from the session cookie or basic credentials.
func (s *Server) authenticate(r *http.Request) authResult {
	if c, err := r.Cookie(sessionCookie); err == nil && s.Access.OIDC != nil {
		var claims sessionClaims
		if s.verify("session", c.Value, &claims) && claims.U != "" && claims.Exp > s.now().Unix() {
			return authResult{p: &principal{Name: claims.U, Groups: claims.G, Session: true}}
		}
	}
	if s.Access.Basic != nil {
		if name, pass, ok := r.BasicAuth(); ok {
			if checkBasic(s.Access.htpasswd, name, pass) {
				return authResult{p: &principal{Name: BasicUserPrefix + name}}
			}
			return authResult{badBasic: true}
		}
	}
	return authResult{}
}

// dummyHash is compared against when the user is unknown, so unknown and wrong-password
// attempts take the same time.
var dummyHash = sync.OnceValue(func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("omp-operator-dummy"), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	return h
})

func checkBasic(users map[string][]byte, name, pass string) bool {
	hash, ok := users[name]
	if !ok {
		_ = bcrypt.CompareHashAndPassword(dummyHash(), []byte(pass))
		return false
	}
	return bcrypt.CompareHashAndPassword(hash, []byte(pass)) == nil
}

// unauthenticated answers a request that carries no valid credentials.
func (s *Server) unauthenticated(w http.ResponseWriter, r *http.Request, res authResult) {
	if s.Access.OIDC != nil && !res.badBasic {
		target := loginPath
		if next := r.URL.RequestURI(); r.Method == http.MethodGet && safeNext(next) != "/" {
			target += "?next=" + url.QueryEscape(next)
		}
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	w.Header().Set("WWW-Authenticate", basicRealm)
	textError(w, http.StatusUnauthorized, "authentication required")
}

func textError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = fmt.Fprintln(w, msg)
}

// safeNext returns next when it is a local path the login may redirect to, else "/".
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") ||
		strings.HasPrefix(next, "/auth/") || strings.ContainsAny(next, "\r\n\t\\") {
		return "/"
	}
	return next
}

// oidcClient holds the lazily-discovered provider.
type oidcClient struct {
	mu       sync.Mutex
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

var oidcHTTP = &http.Client{Timeout: oidcTimeout}

// oidcContext attaches the bounded HTTP client used for every call to the provider.
func oidcContext(ctx context.Context) context.Context { return oidc.ClientContext(ctx, oidcHTTP) }

// oidcSetup returns the OAuth2 config and ID token verifier, running provider
// discovery on first use. A failed discovery is retried by the next request,
// so the server starts while the identity provider is down.
func (s *Server) oidcSetup(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	s.oidc.mu.Lock()
	defer s.oidc.mu.Unlock()
	if s.oidc.oauth != nil {
		return s.oidc.oauth, s.oidc.verifier, nil
	}
	o := s.Access.OIDC
	ctx, cancel := context.WithTimeout(oidcContext(ctx), oidcTimeout)
	defer cancel()
	provider, err := oidc.NewProvider(ctx, o.IssuerURL)
	if err != nil {
		return nil, nil, fmt.Errorf("oidc discovery: %w", err)
	}
	s.oidc.oauth = &oauth2.Config{
		ClientID:     o.ClientID,
		ClientSecret: s.Access.oidcClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  o.RedirectURL,
		Scopes:       o.Scopes,
	}
	s.oidc.verifier = provider.Verifier(&oidc.Config{ClientID: o.ClientID})
	return s.oidc.oauth, s.oidc.verifier, nil
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	cfg, _, err := s.oidcSetup(r.Context())
	if err != nil {
		log.Error(err, "oidc login unavailable")
		textError(w, http.StatusServiceUnavailable, "sign-in provider unavailable")
		return
	}
	state, err1 := randomString(18)
	nonce, err2 := randomString(18)
	if err := errors.Join(err1, err2); err != nil {
		textError(w, http.StatusInternalServerError, "internal error")
		return
	}
	verifier := oauth2.GenerateVerifier()
	cookie, err := s.sign("state", stateClaims{
		State: state, Nonce: nonce, Verifier: verifier,
		Next: safeNext(r.URL.Query().Get("next")),
		Exp:  s.now().Add(stateTTL).Unix(),
	})
	if err != nil {
		log.Error(err, "sign login state")
		textError(w, http.StatusInternalServerError, "internal error")
		return
	}
	setCookie(w, stateCookie, cookie, "/auth", stateTTL)
	http.Redirect(w, r, cfg.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	fail := func(code int, msg string, err error) {
		if err != nil {
			log.Error(err, "oidc callback", "reason", msg)
		}
		clearCookie(w, stateCookie, "/auth")
		textError(w, code, msg)
	}
	c, err := r.Cookie(stateCookie)
	if err != nil {
		fail(http.StatusBadRequest, "sign-in expired; start again at "+loginPath, nil)
		return
	}
	var st stateClaims
	if !s.verify("state", c.Value, &st) || st.Exp <= s.now().Unix() {
		fail(http.StatusBadRequest, "sign-in expired; start again at "+loginPath, nil)
		return
	}
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		fail(http.StatusBadRequest, "sign-in state mismatch", nil)
		return
	}
	if e := q.Get("error"); e != "" {
		fail(http.StatusUnauthorized, "sign-in refused by the identity provider", fmt.Errorf("provider error %q", e))
		return
	}
	cfg, verifier, err := s.oidcSetup(r.Context())
	if err != nil {
		fail(http.StatusServiceUnavailable, "sign-in provider unavailable", err)
		return
	}
	ctx, cancel := context.WithTimeout(oidcContext(r.Context()), oidcTimeout)
	defer cancel()
	tok, err := cfg.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		fail(http.StatusUnauthorized, "sign-in failed", err)
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		fail(http.StatusUnauthorized, "sign-in failed", errors.New("no id_token in token response"))
		return
	}
	idToken, err := verifier.Verify(ctx, rawID)
	if err != nil {
		fail(http.StatusUnauthorized, "sign-in failed", err)
		return
	}
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(st.Nonce)) != 1 {
		fail(http.StatusUnauthorized, "sign-in failed", errors.New("nonce mismatch"))
		return
	}
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		fail(http.StatusUnauthorized, "sign-in failed", err)
		return
	}
	o := s.Access.OIDC
	name, _ := claims[o.UsernameClaim].(string)
	name = strings.TrimSpace(name)
	if name == "" || strings.HasPrefix(name, BasicUserPrefix) {
		fail(http.StatusForbidden, "sign-in failed: no usable username claim", fmt.Errorf("claim %q is empty or reserved", o.UsernameClaim))
		return
	}
	value, err := s.sign("session", sessionClaims{
		U: name, G: s.Access.ruleGroups(claimStrings(claims[o.GroupsClaim])),
		Exp: s.now().Add(sessionTTL).Unix(),
	})
	if err != nil {
		fail(http.StatusInternalServerError, "internal error", err)
		return
	}
	clearCookie(w, stateCookie, "/auth")
	setCookie(w, sessionCookie, value, "/", sessionTTL)
	http.Redirect(w, r, safeNext(st.Next), http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, _ *http.Request) {
	clearCookie(w, sessionCookie, "/")
	s.renderPage(w, http.StatusOK, "signedout", pageData{Title: "Signed out"})
}

// claimStrings reads a string-or-list claim.
func claimStrings(v any) []string {
	switch t := v.(type) {
	case string:
		if t != "" {
			return []string{t}
		}
	case []any:
		var out []string
		for _, e := range t {
			if str, ok := e.(string); ok && str != "" {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}
