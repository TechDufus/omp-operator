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
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/oauth2-proxy/mockoidc"
)

const redirectURL = "http://dashboard.test/auth/callback"

type oidcFixture struct {
	*fixture
	idp *mockoidc.MockOIDC
}

func newOIDCFixture(t *testing.T) *oidcFixture {
	t.Helper()
	m, err := mockoidc.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Shutdown() })
	cfg := m.Config()
	dir := t.TempDir()
	secret := writeFile(t, dir, "client-secret", cfg.ClientSecret+"\n")
	access := writeFile(t, dir, "access.yaml", `
oidc:
  issuerURL: `+cfg.Issuer+`
  clientID: `+cfg.ClientID+`
  clientSecretFile: `+secret+`
  redirectURL: `+redirectURL+`
rules:
  - groups: [ops]
    namespaces: [prod]
  - users: [boss@example.com]
    namespaces: ["*"]
`)
	a, err := LoadAccess(access)
	if err != nil {
		t.Fatal(err)
	}
	return &oidcFixture{fixture: newFixture(t, a), idp: m}
}

func cookieNamed(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// authorize follows the login redirect to the IdP, which (mockoidc) redirects straight back
// with a code, and returns the callback request path and the login response.
func (f *oidcFixture) authorize(t *testing.T, loginTarget string) (callback string, login *httptest.ResponseRecorder) {
	t.Helper()
	login = do(f.h, http.MethodGet, loginTarget)
	if login.Code != http.StatusFound {
		t.Fatalf("login: code %d body %q", login.Code, login.Body.String())
	}
	idpURL, err := url.Parse(login.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(idpURL.String(), f.idp.AuthorizationEndpoint()) {
		t.Fatalf("login redirects to %s, want the IdP authorization endpoint %s", idpURL, f.idp.AuthorizationEndpoint())
	}
	q := idpURL.Query()
	for k, want := range map[string]string{"response_type": "code", "redirect_uri": redirectURL, "client_id": f.idp.ClientID, "code_challenge_method": "S256"} {
		if q.Get(k) != want {
			t.Errorf("authorize param %s = %q, want %q", k, q.Get(k), want)
		}
	}
	if q.Get("state") == "" || q.Get("nonce") == "" || q.Get("code_challenge") == "" || !strings.Contains(q.Get("scope"), "openid") {
		t.Errorf("authorize params incomplete: %v", q)
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Timeout: 10 * time.Second}
	resp, err := noFollow.Get(idpURL.String())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("IdP authorize: %d", resp.StatusCode)
	}
	back, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if back.Scheme+"://"+back.Host+back.Path != redirectURL {
		t.Fatalf("IdP redirects to %s", back)
	}
	return back.Path + "?" + back.RawQuery, login
}

func withCookies(cs ...*http.Cookie) func(*http.Request) {
	return func(r *http.Request) {
		for _, c := range cs {
			r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
		}
	}
}

func TestOIDCLoginRoundTrip(t *testing.T) {
	f := newOIDCFixture(t)
	f.idp.QueueUser(&mockoidc.MockUser{Subject: "1", Email: janeEmail, Groups: []string{opsGroup, "unrelated"}})

	// Unauthenticated: redirect to login.
	if w := do(f.h, http.MethodGet, "/"); w.Code != http.StatusFound || w.Header().Get("Location") != loginPath {
		t.Fatalf("unauthenticated /: %d %q", w.Code, w.Header().Get("Location"))
	}
	callback, login := f.authorize(t, "/auth/login?next=%2Fsessions%2Fprod%2Fok-1")
	state := cookieNamed(login, stateCookie)
	if state == nil || !state.HttpOnly || !state.Secure || state.SameSite != http.SameSiteLaxMode || state.MaxAge != 600 {
		t.Fatalf("state cookie = %+v", state)
	}

	cb := do(f.h, http.MethodGet, callback, withCookies(state))
	if cb.Code != http.StatusFound || cb.Header().Get("Location") != okSessionPath {
		t.Fatalf("callback: %d %q body %q", cb.Code, cb.Header().Get("Location"), cb.Body.String())
	}
	sess := cookieNamed(cb, sessionCookie)
	if sess == nil || !sess.HttpOnly || !sess.Secure || sess.SameSite != http.SameSiteLaxMode || sess.MaxAge != int((12*time.Hour).Seconds()) || sess.Path != "/" {
		t.Fatalf("session cookie = %+v", sess)
	}
	if c := cookieNamed(cb, stateCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("state cookie not cleared: %+v", c)
	}
	checkSessionClaims(t, sess)
	f.checkSessionAccess(t, sess)
}

func checkSessionClaims(t *testing.T, sess *http.Cookie) {
	t.Helper()
	payload, _, _ := strings.Cut(sess.Value, ".")
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	var claims sessionClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.U != janeEmail || len(claims.G) != 1 || claims.G[0] != opsGroup || claims.Exp != testNow.Add(12*time.Hour).Unix() {
		t.Errorf("claims = %+v (only rule-relevant groups are kept)", claims)
	}
}

// checkSessionAccess shows the cookie opens pages in the granted namespace only, then logs out.
func (f *oidcFixture) checkSessionAccess(t *testing.T, sess *http.Cookie) {
	t.Helper()
	page := do(f.h, http.MethodGet, okSessionPath, withCookies(sess))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), janeEmail) || !strings.Contains(page.Body.String(), "/auth/logout") {
		t.Fatalf("page with cookie: %d", page.Code)
	}
	idx := do(f.h, http.MethodGet, "/", withCookies(sess)).Body.String()
	if !strings.Contains(idx, "prod/hello") || strings.Contains(idx, "other/hidden") {
		t.Error("index not limited to the group's namespace")
	}
	if w := do(f.h, http.MethodGet, "/agents/other/hidden", withCookies(sess)); w.Code != http.StatusNotFound {
		t.Errorf("other namespace: %d", w.Code)
	}
	out := do(f.h, http.MethodGet, "/auth/logout", withCookies(sess))
	if c := cookieNamed(out, sessionCookie); out.Code != http.StatusOK || c == nil || c.MaxAge >= 0 || !strings.Contains(out.Body.String(), "signed out") {
		t.Errorf("logout: %d %+v", out.Code, c)
	}
}

func TestOIDCUserRule(t *testing.T) {
	f := newOIDCFixture(t)
	f.idp.QueueUser(&mockoidc.MockUser{Subject: "2", Email: "boss@example.com"})
	callback, login := f.authorize(t, loginPath)
	cb := do(f.h, http.MethodGet, callback, withCookies(cookieNamed(login, stateCookie)))
	if cb.Code != http.StatusFound || cb.Header().Get("Location") != "/" {
		t.Fatalf("callback: %d %q", cb.Code, cb.Header().Get("Location"))
	}
	idx := do(f.h, http.MethodGet, "/", withCookies(cookieNamed(cb, sessionCookie))).Body.String()
	if !strings.Contains(idx, "prod/hello") || !strings.Contains(idx, "other/hidden") {
		t.Error("user rule with * does not show every namespace")
	}
}

func TestOIDCCallbackFailures(t *testing.T) {
	f := newOIDCFixture(t)

	t.Run("no state cookie", func(t *testing.T) {
		if w := do(f.h, http.MethodGet, "/auth/callback?code=x&state=y"); w.Code != http.StatusBadRequest {
			t.Errorf("code %d", w.Code)
		}
	})
	t.Run("forged state cookie", func(t *testing.T) {
		w := do(f.h, http.MethodGet, "/auth/callback?code=x&state=y", withCookies(&http.Cookie{Name: stateCookie, Value: "e30.AAAA"}))
		if w.Code != http.StatusBadRequest {
			t.Errorf("code %d", w.Code)
		}
	})
	t.Run("state mismatch", func(t *testing.T) {
		f.idp.QueueUser(&mockoidc.MockUser{Subject: "3", Email: janeEmail, Groups: []string{opsGroup}})
		callback, login := f.authorize(t, loginPath)
		bad := strings.Replace(callback, "state=", "state=x", 1)
		w := do(f.h, http.MethodGet, bad, withCookies(cookieNamed(login, stateCookie)))
		if w.Code != http.StatusBadRequest || cookieNamed(w, sessionCookie) != nil {
			t.Errorf("code %d", w.Code)
		}
	})
	t.Run("expired login attempt", func(t *testing.T) {
		f.idp.QueueUser(&mockoidc.MockUser{Subject: "3", Email: janeEmail, Groups: []string{opsGroup}})
		callback, login := f.authorize(t, loginPath)
		f.srv.Now = func() time.Time { return testNow.Add(11 * time.Minute) }
		defer func() { f.srv.Now = func() time.Time { return testNow } }()
		w := do(f.h, http.MethodGet, callback, withCookies(cookieNamed(login, stateCookie)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("code %d", w.Code)
		}
	})
	t.Run("nonce mismatch", func(t *testing.T) {
		f.idp.QueueUser(&mockoidc.MockUser{Subject: "3", Email: janeEmail, Groups: []string{opsGroup}})
		callback, login := f.authorize(t, loginPath)
		var st stateClaims
		if !f.srv.verify("state", cookieNamed(login, stateCookie).Value, &st) {
			t.Fatal("cannot read state cookie")
		}
		st.Nonce = "not-the-nonce"
		forged, err := f.srv.sign("state", st)
		if err != nil {
			t.Fatal(err)
		}
		w := do(f.h, http.MethodGet, callback, withCookies(&http.Cookie{Name: stateCookie, Value: forged}))
		if w.Code != http.StatusUnauthorized || cookieNamed(w, sessionCookie) != nil {
			t.Errorf("code %d", w.Code)
		}
	})
	t.Run("wrong audience", func(t *testing.T) {
		f.idp.QueueUser(&mockoidc.MockUser{Subject: "3", Email: janeEmail, Groups: []string{opsGroup}})
		callback, login := f.authorize(t, loginPath)
		f.srv.oidc.mu.Lock()
		f.srv.oidc.oauth, f.srv.oidc.verifier = nil, nil
		f.srv.oidc.mu.Unlock()
		f.srv.Access.OIDC.ClientID = "someone-else"
		defer func() { f.srv.Access.OIDC.ClientID = f.idp.ClientID }()
		w := do(f.h, http.MethodGet, callback, withCookies(cookieNamed(login, stateCookie)))
		if w.Code != http.StatusUnauthorized || cookieNamed(w, sessionCookie) != nil {
			t.Errorf("code %d", w.Code)
		}
	})
	t.Run("no username claim", func(t *testing.T) {
		f.srv.oidc.mu.Lock()
		f.srv.oidc.oauth, f.srv.oidc.verifier = nil, nil
		f.srv.oidc.mu.Unlock()
		f.idp.QueueUser(&mockoidc.MockUser{Subject: "4"})
		callback, login := f.authorize(t, loginPath)
		w := do(f.h, http.MethodGet, callback, withCookies(cookieNamed(login, stateCookie)))
		if w.Code != http.StatusForbidden || cookieNamed(w, sessionCookie) != nil {
			t.Errorf("code %d", w.Code)
		}
	})
	t.Run("reserved basic: username", func(t *testing.T) {
		f.idp.QueueUser(&mockoidc.MockUser{Subject: "5", Email: basicRoot})
		callback, login := f.authorize(t, loginPath)
		w := do(f.h, http.MethodGet, callback, withCookies(cookieNamed(login, stateCookie)))
		if w.Code != http.StatusForbidden {
			t.Errorf("code %d", w.Code)
		}
	})
	t.Run("provider error", func(t *testing.T) {
		f.idp.QueueUser(&mockoidc.MockUser{Subject: "6", Email: janeEmail})
		_, login := f.authorize(t, loginPath)
		var st stateClaims
		f.srv.verify("state", cookieNamed(login, stateCookie).Value, &st)
		w := do(f.h, http.MethodGet, "/auth/callback?error=access_denied&state="+url.QueryEscape(st.State), withCookies(cookieNamed(login, stateCookie)))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("code %d", w.Code)
		}
	})
}

func TestAuthRoutesAbsentWithoutOIDC(t *testing.T) {
	f := newFixture(t, basicAccess(t, defaultRules()...))
	for _, p := range []string{loginPath, "/auth/callback", "/auth/logout"} {
		if w := do(f.h, http.MethodGet, p); w.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401 (no OIDC routes)", p, w.Code)
		}
		if w := do(f.h, http.MethodGet, p, asAlice()); w.Code != http.StatusNotFound {
			t.Errorf("%s authenticated: %d, want 404", p, w.Code)
		}
	}
}
