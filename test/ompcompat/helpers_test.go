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

// Package ompcompat checks omp-operator's assumptions about omp against the real, pinned omp
// binary ($OMP_BIN). It runs only under the ompcompat build tag (mise run ompcompat).
package ompcompat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	omprpc "github.com/can1357/oh-my-pi/sdk/go/omp-rpc"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/actions"
	"github.com/TechDufus/omp-operator/internal/agentapi"
	"github.com/TechDufus/omp-operator/internal/driver"
)

const (
	apiToken  = "compat-token"
	sessionNS = "ns"
	sessionID = "s1"
	agentName = "a1"

	finishTool = "finish"
	toolBash   = "bash"
	// finishArgs satisfies the finish tool's schema.
	finishArgs = `{"verdict":"ok","title":"compat","summary":"compat run","report":"all good"}`
)

// ompBin is the omp executable under test. TestMain resolves it from $OMP_BIN.
var ompBin string

func TestMain(m *testing.M) {
	ompBin = os.Getenv("OMP_BIN")
	if ompBin == "" {
		panic("OMP_BIN must name the pinned omp executable (run: mise run ompcompat)")
	}
	abs, err := filepath.Abs(ompBin)
	if err != nil {
		panic(err)
	}
	ompBin = abs
	// The developer's own omp session (and its environment) must not leak into the omp under test.
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "PI_") || strings.HasPrefix(k, "__PI_") || strings.HasPrefix(k, "OMP_") {
			_ = os.Unsetenv(k)
		}
	}
	_ = os.Setenv("OMP_BIN", ompBin)
	os.Exit(m.Run())
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// testPaths relocates the in-pod layout under a temporary directory.
func testPaths(t *testing.T) driver.Paths {
	t.Helper()
	root := t.TempDir()
	p := driver.DefaultPaths().WithHome(filepath.Join(root, "home"))
	p.Token = filepath.Join(root, "run", "token")
	p.RunDir = filepath.Join(root, "run")
	p.Kubeconfig = filepath.Join(root, "run", "kubeconfig")
	p.Workspace = filepath.Join(root, "workspace")
	p.MiseDir = filepath.Join(root, "mise")
	p.MiseDataDir = filepath.Join(root, "mise", "data")
	p.SystemPrompt = filepath.Join(root, "system-prompt.md")
	p.Transcript = filepath.Join(root, "transcript.html")
	for _, dir := range []string{p.Home, p.Workspace, p.RunDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, p.Token, apiToken)
	return p
}

// finishDef is the finish tool exactly as the operator hands it to the driver.
func finishDef(t *testing.T) agentapi.ToolDef {
	t.Helper()
	defs, _, err := (&actions.Runner{}).ToolDefs(context.Background(), &ompv1alpha1.OmpAgent{})
	if err != nil || len(defs) != 1 || defs[0].Name != finishTool {
		t.Fatalf("ToolDefs = %+v, %v", defs, err)
	}
	return defs[0]
}

// startOmp starts omp the way the driver does (same command line, HOME relocated) and drains its
// frames. The client is closed when the test ends.
func startOmp(t *testing.T, paths driver.Paths, args []string, tools ...omprpc.HostTool) *omprpc.Client {
	t.Helper()
	cmd := exec.Command(ompBin, args...)
	cmd.Dir = paths.Workspace
	cmd.Env = append(os.Environ(), "HOME="+paths.Home)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client, err := omprpc.Start(ctx, cmd, omprpc.WithHostTools(tools...))
	if err != nil {
		t.Fatalf("start omp %v: %v\nstderr:\n%s", args, err, stderr.String())
	}
	go func() {
		for range client.Frames() { //nolint:revive // drained so omp never blocks on stdout
		}
	}()
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func httpStatus(url string) (int, error) {
	resp, err := http.Get(url) //nolint:gosec,noctx // local test endpoint
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// agentAPI is an httptest stand-in for the operator's agent API serving one target.
type agentAPI struct {
	srv  *httptest.Server
	base string

	mu           sync.Mutex
	cfg          agentapi.SessionConfig
	storedHash   string
	actions      []actionCall
	events       []agentapi.Event
	transcript   []byte
	completes    []agentapi.Complete
	unchanged    []agentapi.ToolchainHash
	toolchainPut map[string][]byte
}

type actionCall struct {
	Tool string
	Body string
}

func newAgentAPI(t *testing.T, kind string) *agentAPI {
	t.Helper()
	a := &agentAPI{toolchainPut: map[string][]byte{}}
	if kind == driver.KindAgents {
		a.base = "/v1/agents/" + sessionNS + "/" + agentName
	} else {
		a.base = "/v1/sessions/" + sessionNS + "/" + sessionID
	}
	a.srv = httptest.NewServer(http.HandlerFunc(a.handle))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *agentAPI) driver(t *testing.T, kind string, paths driver.Paths) *driver.Driver {
	t.Helper()
	target := driver.Target{Kind: kind, Namespace: sessionNS, Name: sessionID}
	if kind == driver.KindAgents {
		target.Name = agentName
	}
	client := driver.NewClient(a.srv.URL, paths.Token, target)
	client.RetryDelay = time.Millisecond
	d := driver.New(client, paths, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.Stderr = io.Discard
	d.OmpBin = ompBin
	d.EventInterval = 50 * time.Millisecond
	d.PromptGrace = 5 * time.Second
	d.ReadyTimeout = 2 * time.Minute
	return d
}

func (a *agentAPI) handle(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+apiToken {
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
	writeJSON := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case route == "GET ":
		writeJSON(a.cfg)
	case route == "GET /plan":
		writeJSON(agentapi.Plan{Toolchain: &agentapi.ToolchainPlan{}})
	case route == "GET /toolchain":
		writeJSON(agentapi.ToolchainHash{Hash: a.storedHash})
	case route == "PUT /transcript":
		a.transcript = body
	case route == "PUT /continuity":
	case route == "POST /events":
		var ev agentapi.Events
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		a.events = append(a.events, ev.Events...)
	case route == "POST /complete":
		var c agentapi.Complete
		if err := json.Unmarshal(body, &c); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		a.completes = append(a.completes, c)
	case route == "POST /toolchain/unchanged":
		var h agentapi.ToolchainHash
		if err := json.Unmarshal(body, &h); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		a.unchanged = append(a.unchanged, h)
	case r.Method == http.MethodPut && strings.HasPrefix(rest, "/toolchain/"):
		a.toolchainPut[strings.TrimPrefix(rest, "/toolchain/")] = body
	case r.Method == http.MethodPost && strings.HasPrefix(rest, "/actions/"):
		tool := strings.TrimPrefix(rest, "/actions/")
		a.actions = append(a.actions, actionCall{Tool: tool, Body: string(body)})
		text := "ok " + tool
		if tool == finishTool {
			text = "finish recorded"
		}
		writeJSON(agentapi.ActionResult{Text: text})
	default:
		http.NotFound(w, r)
	}
}

// snapshot runs f with the recorded state locked.
func (a *agentAPI) snapshot(f func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f()
}

// gatewayModel is a model routed through the auth-gateway at url.
func gatewayModel(model, url, thinking string) agentapi.ModelConfig {
	provider, _, _ := strings.Cut(model, "/")
	return agentapi.ModelConfig{
		Default: model, Thinking: thinking, Gateway: &agentapi.GatewayConfig{URL: url, Providers: []string{provider}},
	}
}
