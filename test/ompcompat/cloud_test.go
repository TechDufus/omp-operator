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

package ompcompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

	"github.com/TechDufus/omp-operator/test/fakeprovider"
)

const (
	cloudNS     = "ns"
	cloudAgent  = "cloud-agent"
	cloudThread = "cloud-agent-k3x9fa"
	cloudToken  = "compat-cloud-token"
	localPrompt = "remember the codeword xyzzy-compat"
)

// ompClient is a running omp with every server frame recorded.
type ompClient struct {
	*omprpc.Client
	mu     sync.Mutex
	frames []map[string]any
}

// startRecordingOmp starts omp in rpc mode with env and records every frame it emits.
func startRecordingOmp(t *testing.T, dir string, env []string, args ...string) *ompClient {
	t.Helper()
	cmd := exec.Command(ompBin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client, err := omprpc.Start(ctx, cmd)
	if err != nil {
		t.Fatalf("start omp %v: %v\nstderr:\n%s", args, err, stderr.String())
	}
	c := &ompClient{Client: client}
	go func() {
		for f := range client.Frames() {
			raw, err := json.Marshal(f)
			if err != nil {
				continue
			}
			var m map[string]any
			if json.Unmarshal(raw, &m) != nil {
				continue
			}
			c.mu.Lock()
			c.frames = append(c.frames, m)
			c.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = client.Close()
		if t.Failed() {
			t.Logf("omp stderr:\n%s", stderr.String())
		}
	})
	return c
}

// notifications returns the messages of every extension notify frame seen so far.
func (c *ompClient) notifications() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, f := range c.frames {
		if f["type"] == "extension_ui_request" && f["method"] == "notify" {
			msg, _ := f["message"].(string)
			out = append(out, msg)
		}
	}
	return out
}

// waitNotification waits for a notify frame whose message contains every part.
func (c *ompClient) waitNotification(t *testing.T, timeout time.Duration, parts ...string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, msg := range c.notifications() {
			if containsAll(msg, parts) {
				return
			}
		}
		if time.Now().After(deadline) {
			c.dumpFrames(t)
			t.Fatalf("no notification containing %q within %s; saw %q", parts, timeout, c.notifications())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// dumpFrames logs every recorded frame, truncated.
func (c *ompClient) dumpFrames(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range c.frames {
		raw, _ := json.Marshal(f)
		if len(raw) > 300 {
			raw = raw[:300]
		}
		t.Logf("frame: %s", raw)
	}
}

func containsAll(s string, parts []string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

// cloudHome writes a HOME whose omp knows only the fake provider.
func cloudHome(t *testing.T, model *fakeprovider.Server) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	agentDir := filepath.Join(home, ".omp", "agent")
	writeFile(t, filepath.Join(agentDir, "models.yml"), model.ModelsYAML())
	writeFile(t, filepath.Join(agentDir, "config.yml"), "modelRoles:\n  default: "+fakeprovider.Model+"\n")
	return home
}

// gitRepoWithCommit creates a repository with one commit and no remote.
func gitRepoWithCommit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "README.md"), "compat repo\n")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "-A"},
		{"-c", "user.name=compat", "-c", "user.email=compat@example.com", "commit", "-q", "-m", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// fakeCloudAPI is an httptest stand-in for the operator's cloud API: it answers whoami and the
// thread reads, and captures the multipart handoff.
type fakeCloudAPI struct {
	srv *httptest.Server

	mu    sync.Mutex
	order []string          // multipart part names, in arrival order
	parts map[string][]byte // part name -> content
	names map[string]string // part name -> filename
	auth  []string
	// uploads holds every handoff in arrival order; parts, names and order keep only the latest.
	uploads []upload
}

func newFakeCloudAPI(t *testing.T) *fakeCloudAPI {
	t.Helper()
	f := &fakeCloudAPI{parts: map[string][]byte{}, names: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCloudAPI) summary(phase string) map[string]any {
	return map[string]any{
		"namespace": cloudNS, "name": cloudThread, "agent": cloudAgent, "title": "do it",
		"owner": "token:compat", "phase": phase, "turns": 1, "queued": 0, "model": fakeprovider.Model,
		"createdAt": time.Now().UTC().Format(time.RFC3339), "url": "",
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeCloudAPI) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+cloudToken {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	base := "/api/v1/namespaces/" + cloudNS + "/threads"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/whoami":
		writeJSON(w, http.StatusOK, map[string]any{"user": "token:compat", "agents": []any{map[string]any{
			"namespace": cloudNS, "name": cloudAgent, "ready": true, "defaultModel": fakeprovider.Model,
			"modelPatterns": []string{"fake/*"}, "gitHosts": []string{}, "maxUploadBytes": 1 << 30,
		}}})
	case r.Method == http.MethodPost && r.URL.Path == base:
		f.capture(w, r)
	case r.Method == http.MethodGet && r.URL.Path == base:
		writeJSON(w, http.StatusOK, map[string]any{"threads": []any{f.summary("Idle")}})
	case r.Method == http.MethodGet && r.URL.Path == base+"/"+cloudThread:
		detail := f.summary("Idle")
		detail["turns"] = []any{}
		detail["repo"] = map[string]any{"name": "repo"}
		writeJSON(w, http.StatusOK, detail)
	case r.Method == http.MethodGet && r.URL.Path == base+"/"+cloudThread+"/events":
		writeJSON(w, http.StatusOK, map[string]any{"session": "", "events": []any{}})
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// capture reads the handoff multipart in part order.
func (f *fakeCloudAPI) capture(w http.ResponseWriter, r *http.Request) {
	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		data, err := io.ReadAll(part)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		f.order = append(f.order, part.FormName())
		f.parts[part.FormName()] = data
		f.names[part.FormName()] = part.FileName()
	}
	f.uploads = append(f.uploads, upload{
		request: f.parts["request"], conversation: f.parts["conversation"], source: f.parts["source"],
	})
	resp := f.summary("Working")
	resp["warnings"] = []string{}
	writeJSON(w, http.StatusCreated, resp)
}

// upload is one captured handoff.
type upload struct{ request, conversation, source []byte }

// handedOff is what the fake cloud API received from one `/cloud` launch.
type handedOff struct {
	order        []string
	request      map[string]any
	source       []byte
	conversation []byte
}

func (f *fakeCloudAPI) received(t *testing.T) handedOff {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	h := handedOff{
		order: append([]string(nil), f.order...), source: f.parts["source"], conversation: f.parts["conversation"],
	}
	if err := json.Unmarshal(f.parts["request"], &h.request); err != nil {
		t.Fatalf("request part is not JSON: %v (%q)", err, f.parts["request"])
	}
	return h
}

func (f *fakeCloudAPI) hasRequest() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.parts["request"]) > 0
}

// cloudExtensionDir is the extension under test (the repository's clients/omp-cloud).
func cloudExtensionDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "clients", "omp-cloud"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		t.Fatal(err)
	}
	return dir
}

// cloudEnv is the environment of the omp that runs the extension.
func cloudEnv(home string, api *fakeCloudAPI) []string {
	return []string{
		"HOME=" + home,
		"OMP_CLOUD_URL=" + api.srv.URL,
		"OMP_CLOUD_TOKEN=" + cloudToken,
		"OMP_CLOUD_NAMESPACE=" + cloudNS,
		"OMP_CLOUD_AGENT=" + cloudAgent,
	}
}

// handoffResult is one completed `/cloud do it` run.
type handoffResult struct {
	received        handedOff
	originalID      string
	originalFile    string
	requestsAtStart int
	repo            string // the local checkout, which the cloud agent mirrors at the same path
}

// runHandoff runs one local fakeprovider turn in omp with the extension, then `/cloud do it`, and
// returns what the fake cloud API received.
func runHandoff(t *testing.T) handoffResult {
	t.Helper()
	model := fakeprovider.New(t)
	home := cloudHome(t, model)
	repo := gitRepoWithCommit(t)
	api := newFakeCloudAPI(t)
	sessionDir := filepath.Join(t.TempDir(), "sessions")

	c := startRecordingOmp(t, repo, cloudEnv(home, api),
		"--mode", "rpc", "--approval-mode", "yolo", "--model", fakeprovider.Model,
		"--no-tools", "--no-skills", "--no-rules", "--no-title", "--no-lsp", "--no-pty",
		"--session-dir", sessionDir, "-e", cloudExtensionDir(t))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if _, err := c.PromptAndWait(ctx, omprpc.PromptCommand{Message: localPrompt}, 2*time.Minute); err != nil {
		t.Fatalf("local turn: %v", err)
	}
	if got := model.Count(); got != 1 {
		t.Fatalf("the local turn made %d model requests, want 1", got)
	}
	state, err := c.GetState(ctx)
	if err != nil {
		t.Fatalf("get_state: %v", err)
	}
	if state.SessionFile == nil {
		t.Fatalf("get_state has no sessionFile: %+v", state)
	}
	res := handoffResult{
		originalID: state.SessionID, originalFile: *state.SessionFile, requestsAtStart: model.Count(), repo: repo,
	}

	// The slash command is routed to the extension, not to the model.
	if _, err := c.Prompt(ctx, omprpc.PromptCommand{Message: "/cloud do it"}); err != nil {
		t.Fatalf("prompt /cloud: %v", err)
	}
	c.waitNotification(t, time.Minute, "Handing off to the cloud")
	c.waitNotification(t, time.Minute, "☁ ", cloudThread, " started on "+cloudNS+"/"+cloudAgent)

	if !api.hasRequest() {
		t.Fatalf("the fake cloud API got no handoff although the extension reported success")
	}
	res.received = api.received(t)

	// Round-trip a command so every effect of /cloud do it has been processed, then check that
	// the slash command never reached the model.
	if _, err := c.GetState(ctx); err != nil {
		t.Fatalf("get_state after /cloud: %v", err)
	}
	if got := model.Count(); got != res.requestsAtStart {
		t.Errorf("/cloud do it caused model requests: %d -> %d; the extension must handle the command",
			res.requestsAtStart, got)
	}
	return res
}

// sessionHeader returns the conversation's `"type":"session"` line as a map. Real omp files start
// with a reserved title line before it.
func sessionHeader(t *testing.T, conversation []byte) map[string]any {
	t.Helper()
	for line := range bytes.SplitSeq(conversation, []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(line, &m) == nil && m["type"] == "session" {
			return m
		}
	}
	t.Fatalf("conversation has no session header line:\n%.500s", conversation)
	return nil
}

// TestCloudExtensionLoads loads clients/omp-cloud into the real omp and runs `/cloud do it`: the
// command reaches the extension (no model request), which uploads a full-mode handoff whose
// conversation carries a new session id and the original as its parent.
func TestCloudExtensionLoads(t *testing.T) {
	res := runHandoff(t)
	h := res.received

	wantOrder := []string{"request", "source", "conversation", "setup"}
	if fmt.Sprint(h.order) != fmt.Sprint(wantOrder) {
		t.Errorf("multipart part order = %v, want %v", h.order, wantOrder)
	}
	if h.request["prompt"] != "do it" {
		t.Errorf("request.prompt = %v, want %q", h.request["prompt"], "do it")
	}
	if h.request["agent"] != cloudAgent {
		t.Errorf("request.agent = %v, want %q", h.request["agent"], cloudAgent)
	}
	if h.request["model"] != fakeprovider.Model {
		t.Errorf("request.model = %v, want %q", h.request["model"], fakeprovider.Model)
	}
	repo, _ := h.request["repo"].(map[string]any)
	if repo["baseCommit"] != nil && repo["baseCommit"] != "" {
		t.Errorf("repo.baseCommit = %v, want empty (full mode: the repository has no remote)", repo["baseCommit"])
	}
	if hc, _ := repo["handoffCommit"].(string); len(hc) != 40 {
		t.Errorf("repo.handoffCommit = %q, want a commit id", hc)
	}
	if len(h.source) == 0 {
		t.Fatalf("no source bundle was uploaded")
	}
	if !bytes.HasPrefix(h.source, []byte("# v")) || !bytes.Contains(h.source[:128], []byte("git bundle")) {
		t.Errorf("source is not a git bundle: %.60q", h.source)
	}

	header := sessionHeader(t, h.conversation)
	newID, _ := header["id"].(string)
	if newID == "" || newID == res.originalID {
		t.Errorf("conversation session id = %q, want a new id different from %q", newID, res.originalID)
	}
	if header["parentSession"] != res.originalFile {
		t.Errorf("conversation parentSession = %v, want the original file %q", header["parentSession"], res.originalFile)
	}
	if !strings.Contains(string(h.conversation), localPrompt) {
		t.Errorf("conversation does not contain the local turn %q", localPrompt)
	}
	file, _ := h.request["conversationFile"].(string)
	if !strings.HasSuffix(file, ".jsonl") || !strings.Contains(file, newID) {
		t.Errorf("request.conversationFile = %q, want a .jsonl named after the new id %q", file, newID)
	}
}

// TestHandedOffSessionResumes opens the conversation the extension uploaded in a fresh omp, as
// the cloud agent's driver does: the session resumes with the local turn in its history and the
// next prompt reaches the model with that history.
func TestHandedOffSessionResumes(t *testing.T) {
	res := runHandoff(t)
	h := res.received
	file, _ := h.request["conversationFile"].(string)
	if file == "" {
		t.Fatal("handoff carried no conversationFile")
	}
	newID, _ := sessionHeader(t, h.conversation)["id"].(string)

	model := fakeprovider.New(t, fakeprovider.WithReply("cloud reply"))
	home := cloudHome(t, model)
	sessionDir := filepath.Join(t.TempDir(), "sessions")
	writeFile(t, filepath.Join(sessionDir, filepath.Base(file)), string(h.conversation))

	c := startRecordingOmp(t, res.repo, []string{"HOME=" + home},
		"--mode", "rpc", "--no-ui", "--approval-mode", "yolo", "--no-tools", "--no-skills", "--no-rules",
		"--no-title", "--no-lsp", "--no-pty", "--session-dir", sessionDir)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	provider, modelID := fakeprovider.Provider, fakeprovider.ModelID
	open, err := c.OpenSession(ctx, omprpc.OpenSessionCommand{
		SessionDir: sessionDir, Provider: &provider, ModelID: &modelID,
	})
	if err != nil {
		t.Fatalf("open_session: %v", err)
	}
	if !open.Resumed || open.Cancelled {
		t.Fatalf("open_session = %+v, want resumed", open)
	}
	if open.SessionID != newID {
		t.Errorf("resumed session id = %q, want the rewritten id %q", open.SessionID, newID)
	}

	msgs, err := c.GetMessages(ctx)
	if err != nil {
		t.Fatalf("get_messages: %v", err)
	}
	raw, _ := json.Marshal(msgs)
	if !strings.Contains(string(raw), localPrompt) {
		t.Fatalf("resumed messages do not include the local turn %q:\n%s", localPrompt, raw)
	}

	if _, err := c.PromptAndWait(ctx, omprpc.PromptCommand{Message: "continue in the cloud"}, 2*time.Minute); err != nil {
		t.Fatalf("prompt after resume: %v", err)
	}
	reqs := model.RawRequests()
	if len(reqs) != 1 {
		t.Fatalf("model saw %d requests, want 1", len(reqs))
	}
	for _, want := range []string{localPrompt, "continue in the cloud"} {
		if !strings.Contains(reqs[0], want) {
			t.Errorf("model request lacks %q; the resumed history did not reach it:\n%.2000s", want, reqs[0])
		}
	}
}
