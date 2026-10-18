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

//nolint:goconst // test fixtures repeat literals on purpose
package actions

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
)

const itemSchema = `{"type":"object","required":["id"],"additionalProperties":false,
 "properties":{"id":{"type":"integer","minimum":1},"name":{"type":"string","maxLength":20}}}`

func TestCheckURL(t *testing.T) {
	const prefix = "https://api.example.com/v1"
	good := []struct{ in, want string }{
		{"https://api.example.com/v1", "https://api.example.com/v1"},
		{"https://api.example.com/v1/", "https://api.example.com/v1/"},
		{"https://api.example.com/v1/items?q=1", "https://api.example.com/v1/items?q=1"},
		{"https://API.example.com/v1/x", "https://API.example.com/v1/x"},
		{"https://api.example.com:443/v1/x", "https://api.example.com:443/v1/x"},
		{"https://api.example.com/v1/a/../b", "https://api.example.com/v1/b"},
		{"https://api.example.com/v1//x", "https://api.example.com/v1/x"},
	}
	for _, tt := range good {
		t.Run("ok "+tt.in, func(t *testing.T) {
			u, err := CheckURL(prefix, tt.in)
			if err != nil || u.String() != tt.want {
				t.Fatalf("CheckURL(%q) = %v, %v; want %q", tt.in, u, err, tt.want)
			}
		})
	}
	bad := []string{
		"https://api.example.com.evil.net/x",
		"https://api.example.com@evil.net/",
		"https://api.example.com:pw@evil.net/v1",
		"https://user@api.example.com/v1",
		"http://api.example.com/v1",
		"https://api.example.com:8443/v1",
		"https://evil.net/v1",
		"//api.example.com/v1",
		"/v1/x",
		"https://api.example.com/v10",
		"https://api.example.com/v1/../admin",
		"https://api.example.com/v1/..",
		"https://api.example.com/v1/%2e%2e/admin",
		"https://api.example.com/v1/..%2fadmin",
		"https://api.example.com/v1%2Fx",
		"https://api.example.com/v1%2fx",
		"https://api.example.com/v1/a%5cb",
		"https://api.example.com/other",
		"https://api.example.com/",
		"https://api.example.com",
		"https://api.example.com/v1#frag",
		`https://api.example.com/v1\..\x`,
		"https://api.example.com/v1/x y",
		"https://api.example.com/v1/x\r\nHost: evil",
	}
	for _, in := range bad {
		t.Run("refuse "+in, func(t *testing.T) {
			if u, err := CheckURL(prefix, in); err == nil {
				t.Fatalf("CheckURL(%q) = %v, want an error", in, u)
			}
		})
	}
}

func TestCheckURLRootPrefix(t *testing.T) {
	for _, in := range []string{"https://h.example", "https://h.example/", "https://h.example/a/b"} {
		if _, err := CheckURL("https://h.example", in); err != nil {
			t.Errorf("CheckURL(root, %q): %v", in, err)
		}
	}
	if _, err := CheckURL("https://h.example", "https://h.example/a/../../x"); err != nil {
		t.Errorf("cleaning above the root stays in the root: %v", err)
	}
}

func httpAction(srvURL string, mutate func(*v1alpha1.HTTPRequestAction)) *v1alpha1.OmpAction {
	return newAction("http-call", func(s *v1alpha1.OmpActionSpec) {
		s.HTTPRequest = &v1alpha1.HTTPRequestAction{
			Method:           v1alpha1.MethodGet,
			URL:              srvURL + "/api/items/{{.Args.id}}",
			AllowedURLPrefix: srvURL + "/api",
			Parameters:       jsonParams(itemSchema),
		}
		if mutate != nil {
			mutate(s.HTTPRequest)
		}
	})
}

func TestHTTPRequestSendsHeadersAndBody(t *testing.T) {
	var (
		mu                     sync.Mutex
		gotPath, gotQuery      string
		gotMethod, gotBody     string
		gotToken, gotLiteralXY string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		b, _ := io.ReadAll(req.Body)
		gotPath, gotQuery, gotMethod, gotBody = req.URL.Path, req.URL.RawQuery, req.Method, string(b)
		gotToken, gotLiteralXY = req.Header.Get("X-Token"), req.Header.Get("X-Literal")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	lit := "yes"
	act := httpAction(srv.URL, func(h *v1alpha1.HTTPRequestAction) {
		h.Method = v1alpha1.MethodPost
		h.URL = srv.URL + "/api/items/{{.Args.id}}?name={{urlquery .Args.name}}"
		h.Body = `{"id": {{.Args.id}}, "name": {{json .Args.name}}}`
		h.Headers = []v1alpha1.ValueOrSecret{
			{Name: "X-Token", SecretRef: &v1alpha1.SecretKeyRef{Name: "api", Key: "token"}},
			{Name: "X-Literal", Value: &lit},
		}
	})
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "api"}, Data: map[string][]byte{"token": []byte("tok-123")}}
	r, c := newRunner(t, nil, newSession(testSession), act, sec)

	res := call(t, r, newAgent("http-call"), "http_call", map[string]any{"id": 42, "name": `a "b" & c`})
	if res.IsError || res.Text != "HTTP 201\n{\"ok\":true}" {
		t.Fatalf("result = %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotMethod != "POST" || gotPath != "/api/items/42" || gotQuery != "name=a+%22b%22+%26+c" {
		t.Fatalf("request = %s %s?%s", gotMethod, gotPath, gotQuery)
	}
	if gotBody != `{"id": 42, "name": "a \"b\" \u0026 c"}` {
		t.Fatalf("body = %s", gotBody)
	}
	if gotToken != "tok-123" || gotLiteralXY != "yes" {
		t.Fatalf("headers token=%q literal=%q", gotToken, gotLiteralXY)
	}
	recs := getSession(t, c).Status.Actions
	if len(recs) != 1 || strings.Contains(recs[0].Target, "?") || recs[0].Target != "POST "+srv.URL+"/api/items/42" {
		t.Fatalf("status.actions = %+v (the query must not be recorded)", recs)
	}
}

func TestHTTPRequestSchemaRejection(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer srv.Close()
	for name, args := range map[string]any{
		"wrong type":      map[string]any{"id": "abc"},
		"missing":         map[string]any{},
		"extra property":  map[string]any{"id": 1, "x": 1},
		"below minimum":   map[string]any{"id": 0},
		"too long string": map[string]any{"id": 1, "name": strings.Repeat("n", 21)},
	} {
		t.Run(name, func(t *testing.T) {
			// Each subtest uses its own session budget.
			rr, _ := newRunner(t, nil, newSession(testSession), httpAction(srv.URL, nil))
			res := call(t, rr, newAgent("http-call"), "http_call", args)
			if !res.IsError || !strings.HasPrefix(res.Text, "refused: arguments rejected by the schema") {
				t.Fatalf("result = %+v", res)
			}
		})
	}
	if hits != 0 {
		t.Fatalf("server hit %d times for rejected arguments", hits)
	}
}

func TestHTTPRequestRefusesEscapes(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	urls := []string{
		"http://" + host + ".evil.net/api/x",
		"http://" + strings.Split(host, ":")[0] + "@evil.net/api",
		"http://" + host + "@evil.net/api",
		srv.URL + "/api/../secret",
		srv.URL + "/api/%2e%2e/secret",
		srv.URL + "/api%2fx",
		srv.URL + "/api/..%2fsecret",
		srv.URL + "/apix",
		srv.URL + "/",
		"https://" + host + "/api",
	}
	act := newAction("http-call", func(s *v1alpha1.OmpActionSpec) {
		s.MaxPerSession = 100
		s.HTTPRequest = &v1alpha1.HTTPRequestAction{
			Method: v1alpha1.MethodGet, URL: "{{.Args.url}}", AllowedURLPrefix: srv.URL + "/api",
			Parameters: jsonParams(`{"type":"object","required":["url"],"properties":{"url":{"type":"string"}}}`),
		}
	})
	r, _ := newRunner(t, nil, newSession(testSession), act)
	for _, u := range urls {
		res := call(t, r, newAgent("http-call"), "http_call", map[string]any{"url": u})
		if !res.IsError || !strings.HasPrefix(res.Text, "refused: url is not allowed") {
			t.Errorf("url %q: result = %+v", u, res)
		}
	}
	if hits != 0 {
		t.Fatalf("server hit %d times for refused URLs", hits)
	}
}

func TestHTTPRequestDoesNotFollowRedirects(t *testing.T) {
	var mu sync.Mutex
	followed := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/items/1", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/api/other", http.StatusFound)
	})
	mux.HandleFunc("/api/other", func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		followed = true
		mu.Unlock()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	r, _ := newRunner(t, nil, newSession(testSession), httpAction(srv.URL, nil))
	res := call(t, r, newAgent("http-call"), "http_call", map[string]any{"id": 1})
	if res.IsError || !strings.HasPrefix(res.Text, "HTTP 302\n") {
		t.Fatalf("result = %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if followed {
		t.Fatal("the redirect was followed")
	}
}

func TestHTTPRequestTruncatesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("0123456789", 10)))
	}))
	defer srv.Close()
	r, _ := newRunner(t, nil, newSession(testSession), httpAction(srv.URL, func(h *v1alpha1.HTTPRequestAction) { h.ResponseMaxBytes = 25 }))
	res := call(t, r, newAgent("http-call"), "http_call", map[string]any{"id": 1})
	if res.IsError || res.Text != "HTTP 200\n0123456789012345678901234" {
		t.Fatalf("result = %q", res.Text)
	}
}

func TestHTTPRequestTruncationKeepsRunes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("aéééé")) // é is two bytes
	}))
	defer srv.Close()
	r, _ := newRunner(t, nil, newSession(testSession), httpAction(srv.URL, func(h *v1alpha1.HTTPRequestAction) { h.ResponseMaxBytes = 4 }))
	res := call(t, r, newAgent("http-call"), "http_call", map[string]any{"id": 1})
	if res.Text != "HTTP 200\naé" {
		t.Fatalf("result = %q", res.Text)
	}
}

func TestHTTPRequestErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	r, _ := newRunner(t, nil, newSession(testSession), httpAction(srv.URL, nil))
	res := call(t, r, newAgent("http-call"), "http_call", map[string]any{"id": 1})
	if !res.IsError || !strings.HasPrefix(res.Text, "HTTP 403\n") {
		t.Fatalf("result = %+v", res)
	}
}

func TestHTTPRequestMissingSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	act := httpAction(srv.URL, func(h *v1alpha1.HTTPRequestAction) {
		h.Headers = []v1alpha1.ValueOrSecret{{Name: "X-Token", SecretRef: &v1alpha1.SecretKeyRef{Name: "gone", Key: "token"}}}
	})
	r, _ := newRunner(t, nil, newSession(testSession), act)
	res := call(t, r, newAgent("http-call"), "http_call", map[string]any{"id": 1})
	if !res.IsError || !strings.Contains(res.Text, "reading header X-Token") {
		t.Fatalf("result = %+v", res)
	}
}

func TestValidateSpecHTTPRequest(t *testing.T) {
	good := func() *v1alpha1.OmpActionSpec {
		return &v1alpha1.OmpActionSpec{
			Description: "d",
			HTTPRequest: &v1alpha1.HTTPRequestAction{
				Method: "GET", URL: "https://a.example/x/{{.Args.id}}", AllowedURLPrefix: "https://a.example/x",
				Parameters: jsonParams(itemSchema),
			},
		}
	}
	if err := ValidateSpec(good()); err != nil {
		t.Fatalf("good spec: %v", err)
	}
	tests := map[string]func(*v1alpha1.HTTPRequestAction){
		"prefix with userinfo": func(h *v1alpha1.HTTPRequestAction) { h.AllowedURLPrefix = "https://u:p@a.example/x" },
		"prefix with query":    func(h *v1alpha1.HTTPRequestAction) { h.AllowedURLPrefix = "https://a.example/x?q=1" },
		"prefix with fragment": func(h *v1alpha1.HTTPRequestAction) { h.AllowedURLPrefix = "https://a.example/x#f" },
		"relative prefix":      func(h *v1alpha1.HTTPRequestAction) { h.AllowedURLPrefix = "/x" },
		"non-http prefix":      func(h *v1alpha1.HTTPRequestAction) { h.AllowedURLPrefix = "ftp://a.example/x" },
		"url template":         func(h *v1alpha1.HTTPRequestAction) { h.URL = "{{.Args.id" },
		"body template":        func(h *v1alpha1.HTTPRequestAction) { h.Body = "{{nosuchfunc}}" },
		"schema not an object": func(h *v1alpha1.HTTPRequestAction) { h.Parameters = jsonParams(`{"type":"string"}`) },
		"schema without type":  func(h *v1alpha1.HTTPRequestAction) { h.Parameters = jsonParams(`{"properties":{}}`) },
		"schema does not parse": func(h *v1alpha1.HTTPRequestAction) {
			h.Parameters = jsonParams(`{"type":"object","properties":{"a":{"type":"nope"}}}`)
		},
		"external $ref": func(h *v1alpha1.HTTPRequestAction) {
			h.Parameters = jsonParams(`{"type":"object","$ref":"file:///etc/passwd"}`)
		},
		"no schema": func(h *v1alpha1.HTTPRequestAction) { h.Parameters = jsonParams(``) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			s := good()
			mutate(s.HTTPRequest)
			if err := ValidateSpec(s); err == nil {
				t.Fatal("ValidateSpec accepted the spec")
			}
		})
	}
}
