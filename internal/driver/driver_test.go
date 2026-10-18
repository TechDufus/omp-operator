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

package driver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TechDufus/omp-operator/internal/agentapi"
)

// fakeBinary is test/fakeomp, built once in TestMain.
var fakeBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "driver-test")
	if err != nil {
		panic(err)
	}
	fakeBinary = filepath.Join(dir, "fakeomp")
	out, err := exec.Command("go", "build", "-o", fakeBinary, "github.com/TechDufus/omp-operator/test/fakeomp").CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(dir)
		panic("go build fakeomp: " + err.Error() + "\n" + string(out))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

const (
	testToken = "tok"
	sessionNS = "ns"
	sessionID = "s1"
	agentName = "a1"

	readmeMain     = "main readme"
	readmeArtifact = "from artifact"
	toolGitleaks   = "gitleaks"
	continuityID   = "0123456789abcdef"
	historyText    = "history"
	dataTarget     = "/data"
	fakeModel      = "fake/fake-model"
	demoEnv        = "DEMO_TOKEN"
	demoName       = "demo"
	boomMsg        = "boom"
	shebang        = "#!/bin/sh\n"
)

// testPaths relocates the in-pod layout under a temporary directory.
func testPaths(t *testing.T) Paths {
	t.Helper()
	root := t.TempDir()
	p := DefaultPaths().WithHome(filepath.Join(root, "home"))
	p.Token = filepath.Join(root, "run", "token")
	p.RunDir = filepath.Join(root, "run")
	p.Kubeconfig = filepath.Join(root, "run", "kubeconfig")
	p.Workspace = filepath.Join(root, "workspace")
	p.MiseDir = filepath.Join(root, "mise")
	p.MiseDataDir = filepath.Join(root, "mise", "data")
	p.SystemPrompt = filepath.Join(root, "system-prompt.md")
	p.Transcript = filepath.Join(root, "transcript.html")
	p.SAToken = filepath.Join(root, "sa", "token")
	p.SACA = filepath.Join(root, "sa", "ca.crt")
	p.SANamespace = filepath.Join(root, "sa", "namespace")
	return p
}

// fakeAPI is an httptest stand-in for the operator's agent API. It serves one target
// (session ns/s1 or agent ns/a1) and records what the driver sends.
type fakeAPI struct {
	t    *testing.T
	srv  *httptest.Server
	base string // "/v1/sessions/ns/s1" or "/v1/agents/ns/a1"

	mu sync.Mutex
	// served
	plan       agentapi.Plan
	cfg        agentapi.SessionConfig
	tarballs   map[string][]byte // "/toolchain", "/continuity" -> tar.gz
	artifacts  map[string][]byte // "/artifact/<name>" -> tar.gz
	storedHash string            // agent route GET /toolchain
	action     func(tool string, body []byte) (int, agentapi.ActionResult)
	// recorded
	requests     []string
	prepared     []agentapi.Prepared
	actions      []actionCall
	events       []agentapi.Event
	transcript   []byte
	continuity   []byte
	completes    []agentapi.Complete
	unchanged    []agentapi.ToolchainHash
	toolchainPut map[string][]byte
}

type actionCall struct {
	Tool string
	Body string
}

func newFakeAPI(t *testing.T, kind string) *fakeAPI {
	t.Helper()
	a := &fakeAPI{
		t:            t,
		tarballs:     map[string][]byte{},
		artifacts:    map[string][]byte{},
		toolchainPut: map[string][]byte{},
	}
	if kind == KindAgents {
		a.base = "/v1/agents/" + sessionNS + "/" + agentName
	} else {
		a.base = "/v1/sessions/" + sessionNS + "/" + sessionID
	}
	a.srv = httptest.NewServer(http.HandlerFunc(a.handle))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *fakeAPI) client() *Client {
	c := NewClient(a.srv.URL, "", a.target())
	c.Token = testToken
	c.RetryDelay = time.Millisecond
	return c
}

func (a *fakeAPI) target() Target {
	if strings.HasPrefix(a.base, "/v1/agents") {
		return Target{Kind: KindAgents, Namespace: sessionNS, Name: agentName}
	}
	return Target{Kind: KindSessions, Namespace: sessionNS, Name: sessionID}
}

// driver returns a Driver wired to the API with fast timings; omp is fakeomp unless overridden.
func (a *fakeAPI) driver(paths Paths) *Driver {
	d := New(a.client(), paths, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.Stderr = io.Discard
	d.OmpBin = fakeBinary
	d.EventInterval = 20 * time.Millisecond
	d.PromptGrace = 200 * time.Millisecond
	d.ReadyTimeout = 20 * time.Second
	return d
}

func (a *fakeAPI) artifactURL(name string) string { return a.srv.URL + "/artifact/" + name }

func (a *fakeAPI) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (a *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, r.Method+" "+r.URL.EscapedPath())

	if name, ok := strings.CutPrefix(r.URL.Path, "/artifact/"); ok {
		body, found := a.artifacts[name]
		if !found {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, a.base)
	if !ok {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	route := r.Method + " " + rest
	switch {
	case route == "GET ":
		a.writeJSON(w, a.cfg)
	case route == "GET /plan":
		a.writeJSON(w, a.plan)
	case route == "POST /prepared":
		var p agentapi.Prepared
		a.decode(w, body, &p)
		a.prepared = append(a.prepared, p)
	case route == "GET /toolchain" && strings.HasPrefix(a.base, "/v1/agents"):
		a.writeJSON(w, agentapi.ToolchainHash{Hash: a.storedHash})
	case r.Method == http.MethodGet && (rest == "/toolchain" || rest == "/continuity"):
		tarball, found := a.tarballs[rest]
		if !found {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(tarball)
	case route == "PUT /continuity":
		a.continuity = body
	case route == "PUT /transcript":
		a.transcript = body
	case route == "POST /events":
		var ev agentapi.Events
		a.decode(w, body, &ev)
		a.events = append(a.events, ev.Events...)
	case route == "POST /complete":
		var c agentapi.Complete
		a.decode(w, body, &c)
		a.completes = append(a.completes, c)
	case route == "POST /toolchain/unchanged":
		var h agentapi.ToolchainHash
		a.decode(w, body, &h)
		a.unchanged = append(a.unchanged, h)
	case r.Method == http.MethodPut && strings.HasPrefix(rest, "/toolchain/"):
		a.toolchainPut[strings.TrimPrefix(rest, "/toolchain/")] = body
	case r.Method == http.MethodPost && strings.HasPrefix(rest, "/actions/"):
		tool := strings.TrimPrefix(rest, "/actions/")
		a.actions = append(a.actions, actionCall{Tool: tool, Body: string(body)})
		status, res := http.StatusOK, agentapi.ActionResult{Text: "ok " + tool}
		if a.action != nil {
			status, res = a.action(tool, body)
		}
		if status != http.StatusOK {
			http.Error(w, "action failed", status)
			return
		}
		a.writeJSON(w, res)
	default:
		http.NotFound(w, r)
		return
	}
}

func (a *fakeAPI) decode(w http.ResponseWriter, body []byte, v any) {
	if err := json.Unmarshal(body, v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

// snapshot helpers; the handler may still run while a test reads.
func (a *fakeAPI) lock() func() {
	a.mu.Lock()
	return a.mu.Unlock
}

type tarEntry struct {
	Name     string
	Body     string
	Type     byte // tar.TypeReg when zero
	Linkname string
	Mode     int64
}

func buildTarGz(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.Type
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.Mode
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: e.Name, Typeflag: typ, Mode: mode, Linkname: e.Linkname}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.Body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.Body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// tarMembers lists member name -> body of a tar.gz.
func tarMembers(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		out[h.Name] = string(body)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
