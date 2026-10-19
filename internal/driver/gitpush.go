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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/TechDufus/omp-operator/internal/agentapi"
)

// The environment of the gitpush sidecar. It is set by the operator on the container; the
// agent container cannot change it.
const (
	EnvGitpushURL       = "OMP_GITPUSH_URL"
	EnvGitpushBranch    = "OMP_GITPUSH_BRANCH"
	EnvGitpushWorkspace = "OMP_GITPUSH_WORKSPACE"
	EnvGitpushAuth      = "OMP_GITPUSH_AUTH"
	EnvGitpushAuthDir   = "OMP_GITPUSH_AUTHDIR"
	EnvGitpushUsername  = "OMP_GITPUSH_USERNAME"

	// GitpushListen is where the sidecar listens: loopback of the pod, reached by the agent container.
	GitpushListen = "127.0.0.1:8090"
	// DefaultShadowRepo is the sidecar's own repository.
	DefaultShadowRepo = "/tmp/push.git"

	maxPushBody = 1 << 10
)

var (
	headRE   = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	branchRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
)

// Pusher publishes the thread branch for the agent container, which holds no git credentials.
//
// Everything that decides where and how a push goes (remote URL, branch, credentials) comes
// from the sidecar's own environment; a request only names the commit. The push runs from
// a private bare repository whose only link to the agent's repository is its object store
// (git alternates), so nothing in the agent's .git/config (insteadOf rewrites, hooks, credential
// helpers) or in the process environment of the agent can redirect or intercept it.
type Pusher struct {
	URL       string
	Branch    string
	Workspace string
	AuthType  string // "", "token" or "ssh"
	AuthDir   string
	Username  string
	// ShadowRepo is the path of the private bare repository; DefaultShadowRepo when empty.
	ShadowRepo string
	// GitBin is the git executable; "git" when empty.
	GitBin string

	mu    sync.Mutex
	ready bool
}

// PusherFromEnv builds the Pusher of the sidecar container.
func PusherFromEnv(getenv func(string) string) (*Pusher, error) {
	p := &Pusher{
		URL:       getenv(EnvGitpushURL),
		Branch:    getenv(EnvGitpushBranch),
		Workspace: getenv(EnvGitpushWorkspace),
		AuthType:  getenv(EnvGitpushAuth),
		AuthDir:   getenv(EnvGitpushAuthDir),
		Username:  getenv(EnvGitpushUsername),
	}
	switch {
	case p.URL == "" || strings.HasPrefix(p.URL, "-"):
		return nil, fmt.Errorf("%s is not a usable URL", EnvGitpushURL)
	case !validBranch(p.Branch):
		return nil, fmt.Errorf("%s %q is not a usable branch", EnvGitpushBranch, p.Branch)
	case !filepath.IsAbs(p.Workspace):
		return nil, fmt.Errorf("%s must be an absolute path", EnvGitpushWorkspace)
	case p.AuthType != "" && p.AuthType != authToken && p.AuthType != authSSH:
		return nil, fmt.Errorf("%s %q is neither token nor ssh", EnvGitpushAuth, p.AuthType)
	case p.AuthType != "" && p.AuthDir == "":
		return nil, fmt.Errorf("%s is not set", EnvGitpushAuthDir)
	}
	return p, nil
}

func validBranch(b string) bool {
	return branchRE.MatchString(b) && !strings.Contains(b, "..") && !strings.Contains(b, "//") &&
		!strings.HasSuffix(b, "/") && !strings.HasSuffix(b, ".") && !strings.HasSuffix(b, ".lock")
}

// Gitpush is the push sidecar's main: it serves the Pusher on GitpushListen until ctx ends.
func Gitpush(ctx context.Context, getenv func(string) string, log *slog.Logger) error {
	p, err := PusherFromEnv(getenv)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", GitpushListen)
	if err != nil {
		return err
	}
	return p.Serve(ctx, ln, log)
}

// Serve answers pushes on ln until ctx ends.
func (p *Pusher) Serve(ctx context.Context, ln net.Listener, log *slog.Logger) error {
	srv := &http.Server{Handler: p.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	if log != nil {
		log.Info("gitpush listening", "addr", ln.Addr().String(), "branch", p.Branch)
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Handler serves POST /push {"head": "<commit>"} and nothing else.
func (p *Pusher) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /push", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Head string `json:"head"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPushBody))
		if err := dec.Decode(&req); err != nil || !headRE.MatchString(req.Head) {
			writePushJSON(w, http.StatusBadRequest, map[string]any{"error": "head must be a full commit id"})
			return
		}
		if err := p.Push(r.Context(), req.Head); err != nil {
			writePushJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writePushJSON(w, http.StatusOK, map[string]any{"pushed": true})
	})
	return mux
}

func writePushJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Push force-pushes head to the thread branch of the remote. The error text is safe to show:
// credentials are removed and it is at most 300 characters.
func (p *Pusher) Push(ctx context.Context, head string) error {
	if !headRE.MatchString(head) {
		return errors.New("head must be a full commit id")
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	auth, cleanup, err := gitEnv(agentapi.GitSource{AuthDir: p.AuthDir, AuthType: p.AuthType, Username: p.Username})
	if err != nil {
		return p.scrub(err.Error())
	}
	defer cleanup()
	env := p.env(auth)
	shadow := p.shadow()
	if err := p.ensureShadow(ctx, env); err != nil {
		return p.scrub(err.Error())
	}
	ref := "refs/heads/" + p.Branch
	if err := p.git(ctx, env, "--git-dir="+shadow, "update-ref", ref, head); err != nil {
		return p.scrub(err.Error())
	}
	err = p.git(ctx, env, "--git-dir="+shadow, "-c", "core.hooksPath=/dev/null",
		"push", "--force", "--no-verify", p.URL, ref+":"+ref)
	if err != nil {
		return p.scrub(err.Error())
	}
	return nil
}

func (p *Pusher) shadow() string {
	if p.ShadowRepo != "" {
		return p.ShadowRepo
	}
	return DefaultShadowRepo
}

// env is the whole environment of a push: nothing is inherited, so no variable of the pod can
// change which configuration git reads.
func (p *Pusher) env(auth []string) []string {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/tmp"
	}
	env := make([]string, 0, 6+len(auth))
	env = append(env,
		"PATH="+os.Getenv("PATH"),
		"HOME="+home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=http:https:ssh:file",
	)
	return append(env, auth...)
}

// ensureShadow creates the private repository once and links it to the workspace's objects.
func (p *Pusher) ensureShadow(ctx context.Context, env []string) error {
	if p.ready {
		return nil
	}
	shadow := p.shadow()
	if err := p.git(ctx, env, "init", "--bare", "-q", shadow); err != nil {
		return err
	}
	objects, err := filepath.Abs(filepath.Join(p.Workspace, ".git", "objects"))
	if err != nil {
		return err
	}
	info := filepath.Join(shadow, "objects", "info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(info, "alternates"), []byte(objects+"\n"), 0o644); err != nil {
		return err
	}
	p.ready = true
	return nil
}

func (p *Pusher) git(ctx context.Context, env []string, args ...string) error {
	gctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	bin := p.GitBin
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(gctx, bin, args...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", gitVerb(args), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// gitVerb is the subcommand among git's leading global options.
func gitVerb(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c":
			i++
		case strings.HasPrefix(args[i], "-"):
		default:
			return args[i]
		}
	}
	return ""
}

// scrub removes the credentials from git's error output and cuts it to 300 characters.
func (p *Pusher) scrub(msg string) error {
	if p.AuthType == authToken {
		if raw, err := os.ReadFile(filepath.Join(p.AuthDir, agentapi.AuthTokenFile)); err == nil {
			token := strings.TrimSpace(string(raw))
			user := p.Username
			if user == "" {
				user = defaultGitUser
			}
			if token != "" {
				basic := base64.StdEncoding.EncodeToString([]byte(user + ":" + token))
				msg = strings.NewReplacer(basic, "[redacted]", token, "[redacted]").Replace(msg)
			}
		}
	}
	return errors.New(truncate(msg, maxEventError))
}
