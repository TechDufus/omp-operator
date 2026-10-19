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
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pushFixture is a pod's two halves: the agent's workspace (which the agent controls) and the
// remote the sidecar pushes to.
type pushFixture struct {
	root   string
	ws     string
	remote string // bare target repository
	decoy  string // bare repository the agent's configuration tries to redirect pushes to
	head   string
	pusher *Pusher
}

func newPushFixture(t *testing.T) *pushFixture {
	t.Helper()
	isolateGit(t)
	root := t.TempDir()
	f := &pushFixture{
		root: root, ws: filepath.Join(root, "ws"),
		remote: filepath.Join(root, "remote.git"), decoy: filepath.Join(root, "decoy.git"),
	}
	runGit(t, root, "init", "--bare", "-q", f.remote)
	runGit(t, root, "init", "--bare", "-q", f.decoy)
	runGit(t, root, "init", "-q", "-b", "main", f.ws)
	writeFile(t, filepath.Join(f.ws, "work.txt"), "the agent's work\n")
	runGit(t, f.ws, "add", "-A")
	runGit(t, f.ws, "commit", "-q", "-m", "work")
	f.head = runGit(t, f.ws, "rev-parse", "HEAD")
	f.pusher = &Pusher{
		URL: f.remote, Branch: threadBranch, Workspace: f.ws, ShadowRepo: filepath.Join(root, "shadow.git"),
	}
	return f
}

func refs(t *testing.T, bare string) string {
	t.Helper()
	return runGit(t, bare, "for-each-ref", "--format=%(objectname) %(refname)")
}

func TestPusherPushesTheNamedCommitToTheConfiguredBranch(t *testing.T) {
	f := newPushFixture(t)

	if err := f.pusher.Push(context.Background(), f.head); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got, want := refs(t, f.remote), f.head+" refs/heads/"+threadBranch; got != want {
		t.Fatalf("remote refs = %q, want %q", got, want)
	}

	// A later turn rewrote the history: the push is forced.
	runGit(t, f.ws, "commit", "-q", "--amend", "-m", "work, reworded")
	amended := runGit(t, f.ws, "rev-parse", "HEAD")
	if err := f.pusher.Push(context.Background(), amended); err != nil {
		t.Fatalf("forced Push: %v", err)
	}
	if got, want := refs(t, f.remote), amended+" refs/heads/"+threadBranch; got != want {
		t.Fatalf("remote refs = %q, want %q", got, want)
	}
}

// The agent owns the workspace's .git/config. Nothing in it may change where, how or what the
// sidecar pushes: not insteadOf rewrites, not hooks, not remote or push defaults.
func TestPusherIgnoresHostileAgentConfiguration(t *testing.T) {
	f := newPushFixture(t)
	sentinel := filepath.Join(f.root, "hook-ran")
	hooks := filepath.Join(f.root, "hooks")
	for _, name := range []string{"pre-push", "reference-transaction", "post-update", "pre-receive"} {
		path := filepath.Join(hooks, name)
		writeFile(t, path, shebang+"touch "+sentinel+"\n")
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, f.ws, "config", "url."+f.decoy+".insteadOf", f.remote)
	runGit(t, f.ws, "config", "url."+f.decoy+".pushInsteadOf", f.remote)
	runGit(t, f.ws, "config", "core.hooksPath", hooks)
	runGit(t, f.ws, "config", "core.sshCommand", "touch "+sentinel+"; false")
	runGit(t, f.ws, "config", "push.default", "nothing")
	runGit(t, f.ws, "config", "remote.pushDefault", "decoy")
	runGit(t, f.ws, "config", "remote.decoy.url", f.decoy)
	runGit(t, f.ws, "config", "credential.helper", "!touch "+sentinel)
	// The configuration is effective in the agent's own repository.
	if got := runGit(t, f.ws, "ls-remote", "--get-url", f.remote); got != f.decoy {
		t.Fatalf("precondition: the agent's repository resolves %s to %q, want the decoy", f.remote, got)
	}

	if err := f.pusher.Push(context.Background(), f.head); err != nil {
		t.Fatalf("Push: %v", err)
	}

	if got, want := refs(t, f.remote), f.head+" refs/heads/"+threadBranch; got != want {
		t.Fatalf("remote refs = %q, want %q", got, want)
	}
	if got := refs(t, f.decoy); got != "" {
		t.Fatalf("the decoy received %q", got)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("a hook or helper from the agent's configuration ran")
	}
}

func TestPusherFailsForACommitItDoesNotHave(t *testing.T) {
	f := newPushFixture(t)
	err := f.pusher.Push(context.Background(), strings.Repeat("c", 40))
	if err == nil {
		t.Fatal("pushing an unknown commit must fail")
	}
	if got := refs(t, f.remote); got != "" {
		t.Fatalf("remote refs = %q", got)
	}
}

func TestPusherHandler(t *testing.T) {
	f := newPushFixture(t)
	srv := httptest.NewServer(f.pusher.Handler())
	defer srv.Close()
	post := func(path, body string) (int, string) {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	tests := []struct {
		name, path, body string
		wantStatus       int
		wantBody         string
	}{
		{"pushes", "/push", `{"head":"` + f.head + `"}`, http.StatusOK, `"pushed":true`},
		{"unknown commit", "/push", `{"head":"` + strings.Repeat("d", 40) + `"}`, http.StatusBadGateway, `"error"`},
		{"not a commit id", "/push", `{"head":"main"}`, http.StatusBadRequest, "head must be a full commit id"},
		{"option injection", "/push", `{"head":"--upload-pack=x"}`, http.StatusBadRequest, "head must be a full commit id"},
		{"malformed body", "/push", `{`, http.StatusBadRequest, "head must be a full commit id"},
		{"another path", "/other", `{}`, http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := post(tt.path, tt.body)
			if status != tt.wantStatus || !strings.Contains(body, tt.wantBody) {
				t.Fatalf("status = %d, body = %q; want %d containing %q", status, body, tt.wantStatus, tt.wantBody)
			}
		})
	}

	// Branch and URL are never taken from a request: extra fields change nothing.
	body := `{"head":"` + f.head + `","branch":"main","url":"` + f.decoy + `"}`
	if status, _ := post("/push", body); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if got, want := refs(t, f.remote), f.head+" refs/heads/"+threadBranch; got != want {
		t.Fatalf("remote refs = %q, want %q", got, want)
	}
	if got := refs(t, f.decoy); got != "" {
		t.Fatalf("the decoy received %q", got)
	}

	// Only POST is served.
	resp, err := http.Get(srv.URL + "/push")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /push = %d", resp.StatusCode)
	}
}

func TestPusherScrubsTheToken(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "token"), "ghp_supersecret-token-value\n")
	p := &Pusher{AuthType: authToken, AuthDir: dir, Username: "bot"}

	basic := base64.StdEncoding.EncodeToString([]byte("bot:ghp_supersecret-token-value"))
	msg := "fatal: unable to access: ghp_supersecret-token-value was rejected; header " +
		"Authorization: Basic " + basic + " " + strings.Repeat("x", 400)
	got := p.scrub(msg).Error()
	if strings.Contains(got, "ghp_supersecret-token-value") || strings.Contains(got, basic) {
		t.Fatalf("scrubbed message still holds the credential: %q", got)
	}
	if !strings.Contains(got, "[redacted]") || len([]rune(got)) > maxEventError {
		t.Fatalf("scrubbed message = %q (%d runes)", got, len([]rune(got)))
	}
}

func TestPusherFromEnv(t *testing.T) {
	good := map[string]string{
		EnvGitpushURL: "https://github.com/o/r.git", EnvGitpushBranch: threadBranch,
		EnvGitpushWorkspace: "/workspace", EnvGitpushAuth: "token", EnvGitpushAuthDir: "/auth", EnvGitpushUsername: "x-access-token",
	}
	with := func(k, v string) func(string) string {
		return func(key string) string {
			if key == k {
				return v
			}
			return good[key]
		}
	}
	p, err := PusherFromEnv(with("", ""))
	if err != nil || p.URL != good[EnvGitpushURL] || p.Branch != threadBranch || p.AuthType != "token" || p.Username != "x-access-token" {
		t.Fatalf("PusherFromEnv = %+v, %v", p, err)
	}
	for name, bad := range map[string][2]string{
		"no url":             {EnvGitpushURL, ""},
		"option as url":      {EnvGitpushURL, "--upload-pack=x"},
		"no branch":          {EnvGitpushBranch, ""},
		"option as branch":   {EnvGitpushBranch, "-f"},
		"dotdot branch":      {EnvGitpushBranch, "omp-cloud/../main"},
		"lock branch":        {EnvGitpushBranch, "omp-cloud/x.lock"},
		"relative workdir":   {EnvGitpushWorkspace, "workspace"},
		"unknown auth":       {EnvGitpushAuth, "kerberos"},
		"auth without a dir": {EnvGitpushAuthDir, ""},
	} {
		if _, err := PusherFromEnv(with(bad[0], bad[1])); err == nil {
			t.Errorf("%s: PusherFromEnv must fail", name)
		}
	}
}

func TestPusherServeStopsWithItsContext(t *testing.T) {
	f := newPushFixture(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.pusher.Serve(ctx, ln, nil) }()

	resp, err := http.Post("http://"+ln.Addr().String()+"/push", "application/json", strings.NewReader(`{"head":"`+f.head+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not stop")
	}
}
