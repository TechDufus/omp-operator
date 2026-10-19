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

package cloudapi

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/dashboard"
	"github.com/TechDufus/omp-operator/internal/store"
	"github.com/TechDufus/omp-operator/internal/threads"
)

const (
	nsProd       = "prod"
	nsOther      = "other"
	agentCoder   = "coder"
	opsToken     = "ops-token-0123456789"
	viewerToken  = "viewer-token-0123456789"
	nobodyToken  = "nobody-token-0123456789"
	handoffSHA   = "1111111111111111111111111111111111111111"
	baseSHA      = "2222222222222222222222222222222222222222"
	remoteURL    = "https://github.com/acme/app.git"
	threadsPath  = "/api/v1/namespaces/prod/threads"
	defaultModel = "anthropic/claude-sonnet-5-5"
)

var epoch = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func sha(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func access(t *testing.T) *dashboard.Access {
	t.Helper()
	p := filepath.Join(t.TempDir(), "access.yaml")
	body := `
apiTokens:
  - {name: ops, sha256: ` + sha(opsToken) + `}
  - {name: viewer, sha256: ` + sha(viewerToken) + `}
  - {name: nobody, sha256: ` + sha(nobodyToken) + `}
rules:
  - {users: ["token:ops"], namespaces: [prod], role: operator}
  - {users: ["token:viewer"], namespaces: [prod], role: viewer}
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := dashboard.LoadAccess(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func readyCond(status metav1.ConditionStatus, reason string) []metav1.Condition {
	return []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: status, Reason: reason, LastTransitionTime: metav1.NewTime(epoch)}}
}

func agents() []client.Object {
	cloud := &v1alpha1.CloudSpec{Git: []v1alpha1.CloudGitHost{{
		Host: "github.com", Push: true,
		Auth: v1alpha1.GitAuth{TokenSecretRef: &v1alpha1.SecretKeyRef{Name: "gh", Key: "token"}},
	}}}
	return []client.Object{
		&v1alpha1.OmpAgent{
			ObjectMeta: metav1.ObjectMeta{Namespace: nsProd, Name: agentCoder, UID: "agent-uid"},
			Spec: v1alpha1.OmpAgentSpec{
				Model: v1alpha1.ModelSpec{Default: defaultModel, Gateway: &v1alpha1.GatewaySpec{URL: "http://gw", AllowedModels: []string{"anthropic/*", "openai-codex/gpt-5"}}},
				Cloud: cloud,
			},
			Status: v1alpha1.OmpAgentStatus{Conditions: readyCond(metav1.ConditionTrue, "Ready")},
		},
		&v1alpha1.OmpAgent{
			ObjectMeta: metav1.ObjectMeta{Namespace: nsProd, Name: "nogateway"},
			Spec:       v1alpha1.OmpAgentSpec{Model: v1alpha1.ModelSpec{Default: "openai/a", Small: "openai/b"}, Cloud: &v1alpha1.CloudSpec{}},
			Status:     v1alpha1.OmpAgentStatus{Conditions: readyCond(metav1.ConditionTrue, "Ready")},
		},
		&v1alpha1.OmpAgent{
			ObjectMeta: metav1.ObjectMeta{Namespace: nsProd, Name: "plain"},
			Spec:       v1alpha1.OmpAgentSpec{Model: v1alpha1.ModelSpec{Default: "openai/a"}},
			Status:     v1alpha1.OmpAgentStatus{Conditions: readyCond(metav1.ConditionTrue, "Ready")},
		},
		&v1alpha1.OmpAgent{
			ObjectMeta: metav1.ObjectMeta{Namespace: nsProd, Name: "down"},
			Spec:       v1alpha1.OmpAgentSpec{Model: v1alpha1.ModelSpec{Default: "openai/a"}, Cloud: &v1alpha1.CloudSpec{}},
			Status:     v1alpha1.OmpAgentStatus{Conditions: readyCond(metav1.ConditionFalse, "MissingSecret")},
		},
		&v1alpha1.OmpAgent{
			ObjectMeta: metav1.ObjectMeta{Namespace: nsOther, Name: "hidden"},
			Spec:       v1alpha1.OmpAgentSpec{Model: v1alpha1.ModelSpec{Default: "openai/a"}, Cloud: &v1alpha1.CloudSpec{}},
			Status:     v1alpha1.OmpAgentStatus{Conditions: readyCond(metav1.ConditionTrue, "Ready")},
		},
	}
}

type harness struct {
	srv  *Server
	h    http.Handler
	c    client.Client
	st   *store.Store
	dir  string
	log  *threads.Log
	opts *config.Options
}

func newHarness(t *testing.T, mutate func(*Server), objs ...client.Object) *harness {
	t.Helper()
	return newHarnessWith(t, nil, mutate, objs...)
}

func newHarnessWith(t *testing.T, wrap func(*fake.ClientBuilder), mutate func(*Server), objs ...client.Object) *harness {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(scheme(t)).
		WithStatusSubresource(&v1alpha1.OmpThread{}, &v1alpha1.OmpAgent{}, &v1alpha1.OmpSession{}).
		WithObjects(append(agents(), objs...)...)
	if wrap != nil {
		wrap(b)
	}
	c := b.Build()
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	log := threads.NewLog(st)
	log.Now = func() time.Time { return epoch }
	opts := &config.Options{OperatorNamespace: "omp-system", DashboardURL: "https://omp.example.com/"}
	srv := &Server{
		Client: c, APIReader: c, Store: st, Threads: log, Access: access(t), Options: opts,
		MaxUploadBytes: 1 << 20,
	}
	if mutate != nil {
		mutate(srv)
	}
	return &harness{srv: srv, h: srv.Handler(), c: c, st: st, dir: dir, log: log, opts: opts}
}

func (h *harness) do(method, path, token string, body io.Reader, headers ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, body)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	return w
}

func jsonBody(t *testing.T, v any) io.Reader {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(raw)
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	return v
}

func errorOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	return decode[map[string]string](t, w)["error"]
}

// ---- multipart ----

type part struct {
	name string
	body []byte
}

func multipartBody(t *testing.T, parts ...part) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="`+p.name+`"`)
		w, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(p.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

// baseRequest is a valid full-mode request.
func baseRequest() CreateRequest {
	var r CreateRequest
	r.Agent = agentCoder
	r.Prompt = "fix the flaky test\nthen run everything"
	r.Repo.Name = "app"
	r.Repo.RemoteURL = remoteURL
	r.Repo.HandoffCommit = handoffSHA
	r.Repo.LocalBranch = "main"
	r.Author.Name, r.Author.Email = "Ada", "ada@example.com"
	return r
}

func (h *harness) create(t *testing.T, token string, req CreateRequest, parts ...part) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	body, ctype := multipartBody(t, append([]part{{partRequest, raw}}, parts...)...)
	return h.do(http.MethodPost, threadsPath, token, body, "Content-Type", ctype)
}

func (h *harness) listThreads(t *testing.T) []v1alpha1.OmpThread {
	t.Helper()
	var l v1alpha1.OmpThreadList
	if err := h.c.List(t.Context(), &l); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

// filesUnder counts the regular files below dir.
func filesUnder(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// noLeftovers fails when a thread, a Secret, a thread file or a conversation remains.
func (h *harness) noLeftovers(t *testing.T) {
	t.Helper()
	if ts := h.listThreads(t); len(ts) != 0 {
		t.Errorf("threads left: %d", len(ts))
	}
	var secrets corev1.SecretList
	if err := h.c.List(t.Context(), &secrets); err != nil {
		t.Fatal(err)
	}
	if len(secrets.Items) != 0 {
		t.Errorf("secrets left: %d", len(secrets.Items))
	}
	for _, sub := range []string{"threads", "continuity"} {
		if n := filesUnder(t, filepath.Join(h.dir, sub)); n != 0 {
			t.Errorf("%d files left under %s", n, sub)
		}
	}
}

// ---- auth ----

func TestAuth(t *testing.T) {
	t.Run("503 without tokens", func(t *testing.T) {
		h := newHarness(t, func(s *Server) { s.Access = &dashboard.Access{} })
		for _, tok := range []string{"", opsToken} {
			w := h.do(http.MethodGet, "/api/v1/whoami", tok, nil)
			if w.Code != http.StatusServiceUnavailable || errorOf(t, w) != "cloud API not configured: no apiTokens in the access file" {
				t.Errorf("token %q: %d %s", tok, w.Code, w.Body)
			}
		}
	})

	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/whoami"},
		{http.MethodGet, threadsPath},
		{http.MethodPost, threadsPath},
		{http.MethodGet, threadsPath + "/coder-abc123"},
		{http.MethodDelete, threadsPath + "/coder-abc123"},
		{http.MethodGet, "/api/v1/unknown"},
	}
	t.Run("401 for a missing or bad token", func(t *testing.T) {
		h := newHarness(t, nil)
		for _, rt := range routes {
			for name, headers := range map[string][]string{
				"missing":     nil,
				"bad":         {"Authorization", "Bearer wrong"},
				"basic":       {"Authorization", "Basic b3BzOm9wcw=="},
				"bare":        {"Authorization", opsToken},
				"empty token": {"Authorization", "Bearer "},
			} {
				w := h.do(rt.method, rt.path, "", nil, headers...)
				if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != `Bearer realm="omp-operator"` || errorOf(t, w) == "" {
					t.Errorf("%s %s (%s): %d %v %s", rt.method, rt.path, name, w.Code, w.Header(), w.Body)
				}
			}
		}
	})

	t.Run("404 for a namespace the token has no rule for", func(t *testing.T) {
		h := newHarness(t, nil)
		for _, rt := range routes[1 : len(routes)-1] {
			w := h.do(rt.method, rt.path, nobodyToken, nil)
			if w.Code != http.StatusNotFound || errorOf(t, w) != "not found" {
				t.Errorf("%s %s: %d %s", rt.method, rt.path, w.Code, w.Body)
			}
		}
		if w := h.do(http.MethodGet, "/api/v1/namespaces/other/threads", opsToken, nil); w.Code != http.StatusNotFound {
			t.Errorf("ops in another namespace: %d", w.Code)
		}
	})

	t.Run("404 for a namespace the operator does not serve", func(t *testing.T) {
		h := newHarness(t, nil)
		h.opts.AgentNamespaces = []string{"elsewhere"}
		if w := h.do(http.MethodGet, threadsPath, opsToken, nil); w.Code != http.StatusNotFound {
			t.Errorf("unserved namespace: %d", w.Code)
		}
	})

	t.Run("403 for a viewer on a write route", func(t *testing.T) {
		h := newHarness(t, nil)
		for _, rt := range []struct{ method, path string }{
			{http.MethodPost, threadsPath},
			{http.MethodPost, threadsPath + "/coder-abc123/messages"},
			{http.MethodPost, threadsPath + "/coder-abc123/stop"},
			{http.MethodPost, threadsPath + "/coder-abc123/turns/1/questions/1/answer"},
			{http.MethodDelete, threadsPath + "/coder-abc123"},
		} {
			w := h.do(rt.method, rt.path, viewerToken, nil)
			if w.Code != http.StatusForbidden || errorOf(t, w) != "forbidden: operator role required" {
				t.Errorf("%s %s: %d %s", rt.method, rt.path, w.Code, w.Body)
			}
		}
		// A complete upload is refused before anything is stored.
		w := h.create(t, viewerToken, baseRequest(), part{partSource, []byte("b")})
		if w.Code != http.StatusForbidden {
			t.Errorf("viewer upload: %d %s", w.Code, w.Body)
		}
		h.noLeftovers(t)
	})

	t.Run("headers", func(t *testing.T) {
		h := newHarness(t, nil)
		for _, w := range []*httptest.ResponseRecorder{
			h.do(http.MethodGet, "/api/v1/whoami", opsToken, nil),
			h.do(http.MethodGet, "/api/v1/whoami", "", nil),
			h.do(http.MethodGet, "/api/v1/unknown", opsToken, nil),
		} {
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" ||
				!strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
				t.Errorf("%d headers %v", w.Code, w.Header())
			}
		}
		if w := h.do(http.MethodGet, "/api/v1/unknown", opsToken, nil); w.Code != http.StatusNotFound || errorOf(t, w) != "not found" {
			t.Errorf("unknown route: %d %s", w.Code, w.Body)
		}
	})
}

func TestWhoAmI(t *testing.T) {
	h := newHarness(t, nil)
	w := h.do(http.MethodGet, "/api/v1/whoami", opsToken, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	got := decode[WhoAmI](t, w)
	if got.User != "token:ops" || len(got.Agents) != 3 {
		t.Fatalf("whoami = %+v", got)
	}
	coder, nogw := got.Agents[0], got.Agents[2]
	if coder.Namespace != nsProd || coder.Name != agentCoder || !coder.Ready || coder.DefaultModel != defaultModel ||
		strings.Join(coder.ModelPatterns, ",") != "anthropic/*,openai-codex/gpt-5" ||
		strings.Join(coder.GitHosts, ",") != "github.com" || coder.MaxUploadBytes != 1<<20 {
		t.Errorf("coder = %+v", coder)
	}
	if nogw.Name != "nogateway" || strings.Join(nogw.ModelPatterns, ",") != "openai/a,openai/b" || len(nogw.GitHosts) != 0 {
		t.Errorf("nogateway = %+v", nogw)
	}
	for _, a := range got.Agents {
		if a.Name == "down" && a.Ready {
			t.Error("agent with Ready=False reported ready")
		}
	}
	if !strings.Contains(w.Body.String(), `"gitHosts":[]`) {
		t.Errorf("gitHosts is not an empty list: %s", w.Body)
	}

	// The default pattern of a gateway without allowedModels is every model of the default's provider.
	h2 := newHarness(t, nil, &v1alpha1.OmpAgent{
		ObjectMeta: metav1.ObjectMeta{Namespace: nsProd, Name: "gw"},
		Spec:       v1alpha1.OmpAgentSpec{Model: v1alpha1.ModelSpec{Default: "openai/x", Gateway: &v1alpha1.GatewaySpec{URL: "http://gw"}}, Cloud: &v1alpha1.CloudSpec{}},
	})
	for _, a := range decode[WhoAmI](t, h2.do(http.MethodGet, "/api/v1/whoami", opsToken, nil)).Agents {
		if a.Name == "gw" && strings.Join(a.ModelPatterns, ",") != "openai/*" {
			t.Errorf("gateway default patterns = %v", a.ModelPatterns)
		}
	}

	// A viewer is operator nowhere, so it sees no agent to hand work to.
	if got := decode[WhoAmI](t, h.do(http.MethodGet, "/api/v1/whoami", viewerToken, nil)); len(got.Agents) != 0 || got.User != "token:viewer" {
		t.Errorf("viewer whoami = %+v", got)
	}
}

// ---- create ----

func TestCreateFullHandoff(t *testing.T) {
	h := newHarness(t, nil)
	req := baseRequest()
	req.Model = "anthropic/claude-haiku-5"
	req.Thinking = "high"
	req.Paths.Home, req.Paths.Workspace = "/home/ada", "/home/ada/src/app"
	req.SecretFiles = []SecretFile{{Path: ".env", Content: "U0VDUkVU"}, {Path: "conf/k.json", Content: "e30="}} // SECRET, {}
	req.SecretEnv = map[string]string{"ZED": "z", "API_KEY": "k"}
	req.ConversationFile = "2026-10-18_01.jsonl"
	w := h.create(t, opsToken, req,
		part{partSource, []byte("bundle-bytes")},
		part{partConversation, []byte(`{"type":"session"}` + "\n")},
		part{partSetup, []byte("setup-bytes")},
	)
	if w.Code != http.StatusCreated {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	sum := decode[ThreadSummary](t, w)
	if !strings.HasPrefix(sum.Name, "coder-") || len(sum.Name) != len("coder-")+6 || sum.Namespace != nsProd || sum.Agent != agentCoder ||
		sum.Title != "fix the flaky test" || sum.Owner != "token:ops" || sum.Phase != v1alpha1.ThreadPhaseWorking ||
		sum.Turns != 1 || sum.Queued != 1 || sum.Model != req.Model || len(sum.Warnings) != 0 ||
		sum.URL != "https://omp.example.com/threads/prod/"+sum.Name {
		t.Fatalf("summary = %+v", sum)
	}
	if strings.Contains(w.Body.String(), `"turns":[`) {
		t.Error("summary turns must be a number")
	}

	h.checkThreadObject(t, sum.Name, req)
	h.checkStoredFiles(t, sum.Name)
	h.checkSecret(t, sum.Name)
	turns, err := h.log.Turns(nsProd, sum.Name)
	if err != nil || len(turns) != 1 || turns[0].Text != req.Prompt || turns[0].By != "token:ops" || turns[0].State != v1alpha1.TurnStateQueued {
		t.Errorf("turns = %+v, %v", turns, err)
	}
}

func (h *harness) checkThreadObject(t *testing.T, name string, req CreateRequest) {
	t.Helper()
	var th v1alpha1.OmpThread
	if err := h.c.Get(t.Context(), types.NamespacedName{Namespace: nsProd, Name: name}, &th); err != nil {
		t.Fatal(err)
	}
	if len(th.Finalizers) != 1 || th.Finalizers[0] != v1alpha1.FinalizerCleanup || th.Labels[v1alpha1.LabelAgent] != agentCoder {
		t.Errorf("finalizers %v labels %v", th.Finalizers, th.Labels)
	}
	if len(th.OwnerReferences) != 1 || th.OwnerReferences[0].Kind != "OmpAgent" || th.OwnerReferences[0].Name != agentCoder ||
		(th.OwnerReferences[0].Controller != nil && *th.OwnerReferences[0].Controller) {
		t.Errorf("owner references %+v", th.OwnerReferences)
	}
	sp := th.Spec
	if sp.AgentRef.Name != agentCoder || sp.Owner != "token:ops" || sp.Model != req.Model || sp.Thinking != "high" ||
		sp.Repo.Name != "app" || sp.Repo.RemoteURL != remoteURL || sp.Repo.BaseCommit != "" || sp.Repo.HandoffCommit != handoffSHA || sp.Repo.LocalBranch != "main" ||
		sp.Paths.Home != "/home/ada" || sp.Paths.Workspace != "/home/ada/src/app" ||
		sp.Author.Name != "Ada" || sp.Author.Email != "ada@example.com" ||
		strings.Join(sp.SecretFiles, ",") != ".env,conf/k.json" || strings.Join(sp.SecretEnv, ",") != "API_KEY,ZED" {
		t.Errorf("spec = %+v", sp)
	}
}

func (h *harness) checkStoredFiles(t *testing.T, name string) {
	t.Helper()
	readFile := func(file string) string {
		t.Helper()
		f, err := h.st.OpenThreadFile(nsProd, name, file)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		b, _ := io.ReadAll(f)
		return string(b)
	}
	if readFile("source.bundle") != "bundle-bytes" || readFile("setup.tar.gz") != "setup-bytes" {
		t.Error("source or setup stored wrongly")
	}
	f, err := h.st.OpenContinuity(nsProd, agentCoder, store.ContinuityID("thread/prod/"+name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	entry, content := firstTarEntry(t, f)
	if entry != "2026-10-18_01.jsonl" || content != `{"type":"session"}`+"\n" {
		t.Errorf("conversation entry %q = %q", entry, content)
	}
}

func (h *harness) checkSecret(t *testing.T, name string) {
	t.Helper()
	var sec corev1.Secret
	if err := h.c.Get(t.Context(), types.NamespacedName{Namespace: nsProd, Name: name + "-secrets"}, &sec); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"file-0": "SECRET", "file-1": "{}", "env.API_KEY": "k", "env.ZED": "z"}
	if len(sec.Data) != len(want) {
		t.Errorf("secret keys = %v", sec.Data)
	}
	for k, v := range want {
		if string(sec.Data[k]) != v {
			t.Errorf("secret %s = %q, want %q", k, sec.Data[k], v)
		}
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Name != name || sec.OwnerReferences[0].Controller == nil || !*sec.OwnerReferences[0].Controller {
		t.Errorf("secret owner references %+v", sec.OwnerReferences)
	}
}

// firstTarEntry reads a gzip tarball that holds exactly one file.
func firstTarEntry(t *testing.T, r io.Reader) (name, content string) {
	t.Helper()
	zr, err := gzip.NewReader(r)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("tarball has more than one entry (%v)", err)
	}
	return hdr.Name, string(body)
}

func TestCreateDeltaHandoff(t *testing.T) {
	t.Run("delta with commits beyond the base", func(t *testing.T) {
		h := newHarness(t, nil)
		req := baseRequest()
		req.Repo.RemoteURL = "https://x-access-token:ghp_sekret@github.com/acme/app.git"
		req.Repo.BaseRef, req.Repo.BaseCommit = "main", baseSHA
		w := h.create(t, opsToken, req, part{partSource, []byte("delta")})
		if w.Code != http.StatusCreated {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "ghp_sekret") {
			t.Error("credentials echoed in the response")
		}
		th := h.listThreads(t)[0]
		if th.Spec.Repo.RemoteURL != remoteURL || th.Spec.Repo.BaseCommit != baseSHA || th.Spec.Repo.BaseRef != "main" {
			t.Errorf("repo = %+v", th.Spec.Repo)
		}
		if _, err := h.st.OpenThreadFile(nsProd, th.Name, "source.bundle"); err != nil {
			t.Error(err)
		}
		if _, err := h.st.OpenThreadFile(nsProd, th.Name, "setup.tar.gz"); err == nil {
			t.Error("setup stored without a setup part")
		}
		var sec corev1.Secret
		if err := h.c.Get(t.Context(), types.NamespacedName{Namespace: nsProd, Name: th.Name + "-secrets"}, &sec); err == nil {
			t.Error("a Secret was created without secrets")
		}
	})

	t.Run("handoff equals base needs no source", func(t *testing.T) {
		h := newHarness(t, nil)
		req := baseRequest()
		req.Repo.BaseRef, req.Repo.BaseCommit, req.Repo.HandoffCommit = "main", baseSHA, baseSHA
		if w := h.create(t, opsToken, req); w.Code != http.StatusCreated {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})

	t.Run("scp-style remote loses its user", func(t *testing.T) {
		h := newHarness(t, nil)
		req := baseRequest()
		req.Repo.RemoteURL = "git@github.com:acme/app.git"
		if w := h.create(t, opsToken, req, part{partSource, []byte("b")}); w.Code != http.StatusCreated {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		if got := h.listThreads(t)[0].Spec.Repo.RemoteURL; got != "github.com:acme/app.git" {
			t.Errorf("remoteURL = %q", got)
		}
	})
}

func TestCreateFallbacks(t *testing.T) {
	models := []struct {
		name, model, want string
		warns             []string
	}{
		{"allowed by prefix", "anthropic/claude-haiku-5", "anthropic/claude-haiku-5", nil},
		{"allowed exactly", "openai-codex/gpt-5", "openai-codex/gpt-5", nil},
		{"empty means the agent's", "", "", nil},
		{"not allowed", "openai/gpt-9", defaultModel, []string{"model openai/gpt-9 is not available on coder; using " + defaultModel}},
		{"exact pattern is not a prefix", "openai-codex/gpt-5-mini", defaultModel, []string{"model openai-codex/gpt-5-mini is not available on coder; using " + defaultModel}},
	}
	for _, tc := range models {
		t.Run("model "+tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			req := baseRequest()
			req.Model = tc.model
			w := h.create(t, opsToken, req, part{partSource, []byte("b")})
			if w.Code != http.StatusCreated {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			sum := decode[ThreadSummary](t, w)
			if sum.Model != tc.want || strings.Join(sum.Warnings, "|") != strings.Join(tc.warns, "|") {
				t.Errorf("model %q warnings %v, want %q %v", sum.Model, sum.Warnings, tc.want, tc.warns)
			}
			if got := h.listThreads(t)[0].Spec.Model; got != tc.want {
				t.Errorf("stored model %q", got)
			}
		})
	}

	paths := []struct {
		name, home, workspace string
		keep                  bool
	}{
		{"mirrored", "/home/ada", "/home/ada/src/app", true},
		{"none sent", "", "", true},
		{"workspace equals home", "/home/ada", "/home/ada", false},
		{"workspace parents home", "/home/ada", "/home", false},
		{"system directory", "/home/ada", "/etc/app", false},
		{"relative", "home", "/srv/app", false},
		{"only one", "/home/ada", "", false},
	}
	for _, tc := range paths {
		t.Run("paths "+tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			req := baseRequest()
			req.Paths.Home, req.Paths.Workspace = tc.home, tc.workspace
			w := h.create(t, opsToken, req, part{partSource, []byte("b")})
			if w.Code != http.StatusCreated {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			sum := decode[ThreadSummary](t, w)
			got := h.listThreads(t)[0].Spec.Paths
			if tc.keep {
				if len(sum.Warnings) != 0 || got.Home != tc.home || got.Workspace != tc.workspace {
					t.Errorf("paths %+v warnings %v", got, sum.Warnings)
				}
				return
			}
			if got != (v1alpha1.ThreadPaths{}) || len(sum.Warnings) != 1 ||
				!strings.Contains(sum.Warnings[0], "cannot be mirrored; the cloud agent uses /home/agent and /workspace") {
				t.Errorf("paths %+v warnings %v", got, sum.Warnings)
			}
		})
	}
}

func TestCreateValidation(t *testing.T) {
	src := part{partSource, []byte("b")}
	conv := part{partConversation, []byte("{}\n")}
	delta := func(r *CreateRequest) { r.Repo.BaseRef, r.Repo.BaseCommit = "main", baseSHA }
	files := func(n int) []SecretFile {
		out := make([]SecretFile, n)
		for i := range out {
			out[i] = SecretFile{Path: "f" + strings.Repeat("x", i), Content: "YQ=="}
		}
		return out
	}

	tests := []struct {
		name   string
		mutate func(*CreateRequest)
		parts  []part
		status int
		errHas string
		// created: the thread existed when the failure happened, so it must be gone again.
		created bool
	}{
		{"unknown agent", func(r *CreateRequest) { r.Agent = "nope" }, []part{src}, 404, "agent nope not found", false},
		{"missing agent", func(r *CreateRequest) { r.Agent = "" }, []part{src}, 400, "agent is required", false},
		{"agent without cloud", func(r *CreateRequest) { r.Agent = "plain" }, []part{src}, 404, "agent plain has no cloud configuration", false},
		{"agent not ready", func(r *CreateRequest) { r.Agent = "down" }, []part{src}, 409, "agent down is not ready: MissingSecret", false},
		{"empty prompt", func(r *CreateRequest) { r.Prompt = "" }, []part{src}, 400, "prompt must be 1 to 32768 bytes", false},
		{"huge prompt", func(r *CreateRequest) { r.Prompt = strings.Repeat("x", 32769) }, []part{src}, 400, "prompt must be 1 to 32768 bytes", false},
		{"bad handoff", func(r *CreateRequest) { r.Repo.HandoffCommit = "HEAD" }, []part{src}, 400, "handoffCommit", false},
		{"bad base", func(r *CreateRequest) { delta(r); r.Repo.BaseCommit = "xyz" }, []part{src}, 400, "baseCommit", false},
		{"no repo name", func(r *CreateRequest) { r.Repo.Name = "" }, []part{src}, 400, "repo.name is required", false},
		{"control chars in author", func(r *CreateRequest) { r.Author.Name = "a\nb" }, []part{src}, 400, "author.name", false},
		{"bad remote", func(r *CreateRequest) { r.Repo.RemoteURL = "https://%zz" }, []part{src}, 400, "repo.remoteURL", false},
		{"full mode without source", nil, nil, 400, "needs the source part", true},
		{"delta without remote", func(r *CreateRequest) { delta(r); r.Repo.RemoteURL = "" }, []part{src}, 400, "needs repo.remoteURL", false},
		{"delta without base ref", func(r *CreateRequest) { delta(r); r.Repo.BaseRef = "" }, []part{src}, 400, "needs repo.baseRef", false},
		{"delta on a host the agent does not serve", func(r *CreateRequest) { delta(r); r.Repo.RemoteURL = "ssh://git@gitlab.example.com/a/b.git" }, []part{src}, 400, "not served by agent coder", false},
		{"delta without source", delta, nil, 400, "source part is required", true},
		{"bad thinking", func(r *CreateRequest) { r.Thinking = "extreme" }, []part{src}, 400, "thinking must be one of", false},
		{"too many secret files", func(r *CreateRequest) { r.SecretFiles = files(33) }, []part{src}, 400, "at most 32", false},
		{"absolute secret path", func(r *CreateRequest) { r.SecretFiles = []SecretFile{{Path: "/etc/passwd", Content: "YQ=="}} }, []part{src}, 400, "relative, clean", false},
		{"escaping secret path", func(r *CreateRequest) { r.SecretFiles = []SecretFile{{Path: "../x", Content: "YQ=="}} }, []part{src}, 400, "relative, clean", false},
		{"unclean secret path", func(r *CreateRequest) { r.SecretFiles = []SecretFile{{Path: "a/../b", Content: "YQ=="}} }, []part{src}, 400, "relative, clean", false},
		{"secret path in .git", func(r *CreateRequest) { r.SecretFiles = []SecretFile{{Path: ".git/config", Content: "YQ=="}} }, []part{src}, 400, "outside .git", false},
		{"duplicate secret path", func(r *CreateRequest) {
			r.SecretFiles = []SecretFile{{Path: "a", Content: "YQ=="}, {Path: "a", Content: "YQ=="}}
		}, []part{src}, 400, "listed twice", false},
		{"secret content not base64", func(r *CreateRequest) { r.SecretFiles = []SecretFile{{Path: "a", Content: "***"}} }, []part{src}, 400, "not base64", false},
		{"secrets too large", func(r *CreateRequest) {
			r.SecretFiles = []SecretFile{{Path: "a", Content: strings.Repeat("QUFB", 360000)}}
		}, []part{src}, 400, "at most 1048576", false},
		{"too many env", func(r *CreateRequest) {
			r.SecretEnv = map[string]string{}
			for i := range 65 {
				r.SecretEnv["V"+strings.Repeat("X", i)] = "v"
			}
		}, []part{src}, 400, "at most 64", false},
		{"env invalid name", func(r *CreateRequest) { r.SecretEnv = map[string]string{"1BAD": "v"} }, []part{src}, 400, "secretEnv 1BAD is not a valid variable name", false},
		{"env HOME", func(r *CreateRequest) { r.SecretEnv = map[string]string{"HOME": "v"} }, []part{src}, 400, "secretEnv HOME is reserved", false},
		{"env PATH", func(r *CreateRequest) { r.SecretEnv = map[string]string{"PATH": "v"} }, []part{src}, 400, "secretEnv PATH is reserved", false},
		{"env OMP_ prefix", func(r *CreateRequest) { r.SecretEnv = map[string]string{"OMP_X": "v"} }, []part{src}, 400, "OMP_", false},
		{"env MISE_ prefix", func(r *CreateRequest) { r.SecretEnv = map[string]string{"MISE_X": "v"} }, []part{src}, 400, "MISE_", false},
		{"bad conversation file", func(r *CreateRequest) { r.ConversationFile = "../x.jsonl" }, []part{src, conv}, 400, "conversationFile must match", false},
		{"conversation file without part", func(r *CreateRequest) { r.ConversationFile = "a.jsonl" }, []part{src}, 400, "no conversation part", true},
		{"conversation part without file", nil, []part{src, conv}, 400, "needs conversationFile", true},
		{"empty conversation", func(r *CreateRequest) { r.ConversationFile = "a.jsonl" }, []part{src, {partConversation, nil}}, 400, "conversation part is empty", true},
		{"repeated part", nil, []part{src, src}, 400, `part "source" is repeated`, true},
		{"unknown part", nil, []part{src, {"extra", []byte("x")}}, 400, `unknown part "extra"`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deletes := 0
			h := newHarnessWith(t, func(b *fake.ClientBuilder) { b.WithInterceptorFuncs(threadDeleteCounter(&deletes)) }, nil)
			req := baseRequest()
			if tc.mutate != nil {
				tc.mutate(&req)
			}
			w := h.create(t, opsToken, req, tc.parts...)
			if w.Code != tc.status || !strings.Contains(errorOf(t, w), tc.errHas) {
				t.Fatalf("%d %s; want %d containing %q", w.Code, w.Body, tc.status, tc.errHas)
			}
			h.noLeftovers(t)
			if want := map[bool]int{false: 0, true: 1}[tc.created]; deletes != want {
				t.Errorf("thread deletes = %d, want %d: a thread must be discarded exactly when the failure came after it was created", deletes, want)
			}
		})
	}

	t.Run("source over the limit", func(t *testing.T) {
		h := newHarness(t, func(s *Server) { s.MaxUploadBytes = 100 })
		w := h.create(t, opsToken, baseRequest(), part{partSource, bytes.Repeat([]byte("x"), 101)})
		if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(errorOf(t, w), `"source" is too large`) {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		h.noLeftovers(t)
	})

	t.Run("setup over the limit", func(t *testing.T) {
		h := newHarness(t, func(s *Server) { s.MaxUploadBytes = store.MaxSetupBytes * 2 })
		w := h.create(t, opsToken, baseRequest(), part{partSource, []byte("b")}, part{partSetup, bytes.Repeat([]byte("x"), int(store.MaxSetupBytes)+1)})
		if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(errorOf(t, w), `"setup" is too large`) {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		h.noLeftovers(t)
	})

	t.Run("request is not first", func(t *testing.T) {
		h := newHarness(t, nil)
		raw, _ := json.Marshal(baseRequest())
		body, ctype := multipartBody(t, src, part{partRequest, raw})
		w := h.do(http.MethodPost, threadsPath, opsToken, body, "Content-Type", ctype)
		if w.Code != http.StatusBadRequest || !strings.Contains(errorOf(t, w), `first part must be "request"`) {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})

	t.Run("request is not JSON", func(t *testing.T) {
		h := newHarness(t, nil)
		body, ctype := multipartBody(t, part{partRequest, []byte("{nope")})
		if w := h.do(http.MethodPost, threadsPath, opsToken, body, "Content-Type", ctype); w.Code != http.StatusBadRequest {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})

	t.Run("not multipart", func(t *testing.T) {
		h := newHarness(t, nil)
		w := h.do(http.MethodPost, threadsPath, opsToken, jsonBody(t, baseRequest()), "Content-Type", "application/json")
		if w.Code != http.StatusBadRequest || !strings.Contains(errorOf(t, w), "multipart") {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})

	t.Run("not enough disk", func(t *testing.T) {
		h := newHarness(t, func(s *Server) { s.MaxUploadBytes = 1 << 62 })
		r := httptest.NewRequest(http.MethodPost, threadsPath, strings.NewReader(""))
		r.Header.Set("Authorization", "Bearer "+opsToken)
		r.Header.Set("Content-Type", "multipart/form-data; boundary=x")
		r.ContentLength = 1 << 61
		w := httptest.NewRecorder()
		h.h.ServeHTTP(w, r)
		if w.Code != http.StatusInsufficientStorage || errorOf(t, w) != "insufficient storage on the operator data volume" {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})

	t.Run("content length over the limit", func(t *testing.T) {
		h := newHarness(t, nil)
		r := httptest.NewRequest(http.MethodPost, threadsPath, strings.NewReader(""))
		r.Header.Set("Authorization", "Bearer "+opsToken)
		r.ContentLength = 1 << 40
		w := httptest.NewRecorder()
		h.h.ServeHTTP(w, r)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})
}

func TestCreateFailureAfterCreationDeletesTheThread(t *testing.T) {
	// The Secret is the last object created: when it fails, the thread, its files and its
	// conversation are all removed again, and no turn was queued.
	h := newHarnessWith(t, func(b *fake.ClientBuilder) {
		b.WithInterceptorFuncs(secretCreateFails())
	}, nil)
	req := baseRequest()
	req.SecretEnv = map[string]string{"TOKEN": "x"}
	req.ConversationFile = "c.jsonl"
	w := h.create(t, opsToken, req, part{partSource, []byte("b")}, part{partConversation, []byte("{}\n")}, part{partSetup, []byte("s")})
	if w.Code != http.StatusInternalServerError || errorOf(t, w) != "internal error" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	h.noLeftovers(t)
}

func TestCreateRetriesAThreadNameCollision(t *testing.T) {
	// A Create that answers AlreadyExists is retried with a new name.
	calls := 0
	h := newHarnessWith(t, func(b *fake.ClientBuilder) {
		b.WithInterceptorFuncs(threadCreateCollides(&calls, 2))
	}, nil)
	w := h.create(t, opsToken, baseRequest(), part{partSource, []byte("b")})
	if w.Code != http.StatusCreated || calls != 3 {
		t.Fatalf("%d after %d creates: %s", w.Code, calls, w.Body)
	}

	h = newHarnessWith(t, func(b *fake.ClientBuilder) {
		b.WithInterceptorFuncs(threadCreateCollides(&calls, 100))
	}, nil)
	calls = 0
	if w := h.create(t, opsToken, baseRequest(), part{partSource, []byte("b")}); w.Code != http.StatusInternalServerError || calls != maxCreateAttempts {
		t.Fatalf("%d after %d creates: %s", w.Code, calls, w.Body)
	}
}
