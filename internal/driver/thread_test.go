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
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TechDufus/omp-operator/internal/agentapi"
)

const (
	threadName   = "t-abc123"
	threadBranch = "omp-cloud/" + threadName
	threadRemote = "https://user:secret-token@example.com/app.git"
)

// isolateGit keeps the developer's git configuration (signing, hooks, templates) out of the
// driver's git processes. The identity must come from the repository, as prepare sets it.
func isolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// resultCount is how many result bundles the operator has received.
func (a *threadAPI) resultCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.results)
}

// threadFixture is a user's repository at the moment of a handoff: a remote that has the base
// commit, a local clone with one unpushed commit and uncommitted work, and the bundles the CLI
// would upload.
type threadFixture struct {
	root      string
	remote    string // bare repository
	local     string
	base      string // the remote's tip
	unpushed  string
	handoff   string // the "WIP" commit on top of unpushed
	deltaPath string // handoff without the base's history
	fullPath  string // handoff with all history
}

func newThreadFixture(t *testing.T) *threadFixture {
	t.Helper()
	isolateGit(t)
	root := t.TempDir()
	f := &threadFixture{root: root, remote: filepath.Join(root, "remote.git"), local: filepath.Join(root, "local")}
	runGit(t, root, "init", "--bare", "-q", "-b", "main", f.remote)
	runGit(t, root, "init", "-q", "-b", "main", f.local)
	writeFile(t, filepath.Join(f.local, "README.md"), "base\n")
	runGit(t, f.local, "add", "-A")
	runGit(t, f.local, "commit", "-q", "-m", "base")
	runGit(t, f.local, "remote", "add", "origin", f.remote)
	runGit(t, f.local, "push", "-q", "origin", "main")
	f.base = runGit(t, f.local, "rev-parse", "HEAD")

	writeFile(t, filepath.Join(f.local, "feature.txt"), "unpushed\n")
	runGit(t, f.local, "add", "-A")
	runGit(t, f.local, "commit", "-q", "-m", "unpushed")
	f.unpushed = runGit(t, f.local, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(f.local, "wip.txt"), "uncommitted\n")
	runGit(t, f.local, "add", "-A")
	runGit(t, f.local, "commit", "-q", "-m", "omp-cloud: local changes at handoff")
	f.handoff = runGit(t, f.local, "rev-parse", "HEAD")

	runGit(t, f.local, "update-ref", refHandoff, f.handoff)
	f.deltaPath = filepath.Join(root, "delta.bundle")
	runGit(t, f.local, "bundle", "create", f.deltaPath, refHandoff, "^"+f.base)
	f.fullPath = filepath.Join(root, "full.bundle")
	runGit(t, f.local, "bundle", "create", f.fullPath, refHandoff)
	return f
}

// plan is the thread plan for the fixture: delta mode, with the remote reachable as a local path.
func (f *threadFixture) plan() *agentapi.ThreadPlan {
	return &agentapi.ThreadPlan{
		Name:          threadName,
		Branch:        threadBranch,
		FetchURL:      f.remote,
		RemoteURL:     threadRemote,
		BaseRef:       "main",
		BaseCommit:    f.base,
		HandoffCommit: f.handoff,
		HasSource:     true,
		Author:        agentapi.ThreadAuthor{Name: "Ada", Email: "ada@example.com"},
	}
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// threadAPI is an httptest stand-in for the operator's agent API of one thread sitting. Its
// long poll is capped so that idle sittings end quickly.
type threadAPI struct {
	t       *testing.T
	srv     *httptest.Server
	base    string
	pollCap time.Duration

	mu       sync.Mutex
	plan     agentapi.Plan
	cfg      agentapi.SessionConfig
	files    map[string][]byte // "source", "result", "setup"
	tarballs map[string][]byte // "/continuity", "/toolchains/<hash>"
	turns    []*apiTurn
	// recorded
	prepared     []agentapi.Prepared
	events       []agentapi.Event
	completes    []agentapi.Complete
	turnDone     map[int]agentapi.TurnComplete
	results      [][]byte
	continuity   []byte
	transcript   []byte
	toolchainPut map[string][]byte
}

type apiTurn struct {
	ID    int
	Text  string
	state string // queued, running, done
	// StopAfter makes thread/control answer stop once the turn has been running that long.
	StopAfter time.Duration
	started   time.Time
}

func newThreadAPI(t *testing.T) *threadAPI {
	t.Helper()
	a := &threadAPI{
		t:            t,
		base:         "/v1/sessions/" + sessionNS + "/" + sessionID,
		pollCap:      100 * time.Millisecond,
		files:        map[string][]byte{},
		tarballs:     map[string][]byte{},
		turnDone:     map[int]agentapi.TurnComplete{},
		toolchainPut: map[string][]byte{},
	}
	a.srv = httptest.NewServer(http.HandlerFunc(a.handle))
	t.Cleanup(a.srv.Close)
	return a
}

// enqueue adds a queued turn and returns it.
func (a *threadAPI) enqueue(text string) *apiTurn {
	a.mu.Lock()
	defer a.mu.Unlock()
	turn := &apiTurn{ID: len(a.turns) + 1, Text: text, state: "queued"}
	a.turns = append(a.turns, turn)
	return turn
}

func (a *threadAPI) turnState(id int) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.turns[id-1].state
}

func (a *threadAPI) driver(paths Paths) *Driver {
	c := NewClient(a.srv.URL, "", Target{Kind: KindSessions, Namespace: sessionNS, Name: sessionID})
	c.Token = testToken
	c.RetryDelay = time.Millisecond
	d := New(c, paths, nil)
	d.Log = discardLogger()
	d.Stderr = io.Discard
	d.OmpBin = fakeBinary
	d.PushAddr = ""
	d.EventInterval = 20 * time.Millisecond
	d.ControlInterval = 20 * time.Millisecond
	d.PromptGrace = 200 * time.Millisecond
	d.ReadyTimeout = 20 * time.Second
	return d
}

func (a *threadAPI) nextLocked() *apiTurn {
	for _, want := range []string{"running", "queued"} { // a delivered turn comes back before a new one
		for _, turn := range a.turns {
			if turn.state == want {
				if want == "queued" {
					turn.state, turn.started = "running", time.Now()
				}
				return turn
			}
		}
	}
	return nil
}

func (a *threadAPI) handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, a.base)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if rest == "/thread/next" {
		a.next(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	defer a.mu.Unlock()
	route := r.Method + " " + rest
	switch {
	case route == "GET ":
		a.json(w, a.cfg)
	case route == "GET /plan":
		a.json(w, a.plan)
	case route == "POST /prepared":
		var p agentapi.Prepared
		_ = json.Unmarshal(body, &p)
		a.prepared = append(a.prepared, p)
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "/thread/") && rest != "/thread/control":
		content, found := a.files[strings.TrimPrefix(rest, "/thread/")]
		if !found {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(content)
	case route == "PUT /thread/result":
		a.results = append(a.results, body)
		a.files["result"] = body
	case route == "GET /thread/control":
		stop := false
		for _, turn := range a.turns {
			stop = stop || (turn.state == "running" && turn.StopAfter > 0 && time.Since(turn.started) >= turn.StopAfter)
		}
		a.json(w, map[string]bool{"stop": stop})
	case r.Method == http.MethodPost && strings.HasPrefix(rest, "/thread/turns/"):
		a.completeTurn(w, rest, body)
	case r.Method == http.MethodGet && (rest == "/continuity" || strings.HasPrefix(rest, "/toolchains/")):
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
	case r.Method == http.MethodPut && strings.HasPrefix(rest, "/toolchains/"):
		a.toolchainPut[strings.TrimPrefix(rest, "/toolchains/")] = body
	case route == "POST /events":
		var ev agentapi.Events
		_ = json.Unmarshal(body, &ev)
		a.events = append(a.events, ev.Events...)
	case route == "POST /complete":
		var c agentapi.Complete
		_ = json.Unmarshal(body, &c)
		a.completes = append(a.completes, c)
	default:
		http.NotFound(w, r)
	}
}

func (a *threadAPI) json(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// next is the capped long poll of GET thread/next.
func (a *threadAPI) next(w http.ResponseWriter, r *http.Request) {
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	deadline := time.Now().Add(min(time.Duration(wait)*time.Second, a.pollCap))
	for {
		a.mu.Lock()
		turn := a.nextLocked()
		a.mu.Unlock()
		switch {
		case turn != nil:
			a.json(w, agentapi.TurnAssignment{ID: turn.ID, Text: turn.Text})
			return
		case time.Now().After(deadline), r.Context().Err() != nil:
			w.WriteHeader(http.StatusNoContent)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// completeTurn handles POST thread/turns/{id}/complete with the lock held.
func (a *threadAPI) completeTurn(w http.ResponseWriter, rest string, body []byte) {
	id, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rest, "/thread/turns/"), "/complete"))
	if err != nil || id < 1 || id > len(a.turns) || a.turns[id-1].state != "running" {
		http.Error(w, "turn is not running", http.StatusConflict)
		return
	}
	var done agentapi.TurnComplete
	_ = json.Unmarshal(body, &done)
	a.turns[id-1].state = "done"
	a.turnDone[id] = done
}

// threadConfig is the session config of a sitting of the fixture's thread.
func threadConfig(f *threadFixture, mutate func(*agentapi.SessionConfig)) agentapi.SessionConfig {
	cfg := agentapi.SessionConfig{
		SystemPrompt:   "You are a cloud agent.",
		Model:          agentapi.ModelConfig{Default: fakeModel, Thinking: "medium"},
		TimeoutSeconds: 30,
		Continuity:     true,
		Thread: &agentapi.ThreadConfig{
			Name: threadName, Branch: threadBranch, BaseCommit: f.base, RepoName: "app",
			TurnTimeoutSeconds: 30, IdleTimeoutSeconds: 1, MaxSittingSeconds: 120,
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return cfg
}

// threadEnv is a prepared sitting: the fixture's source bundle served, prepare already run.
type threadEnv struct {
	f     *threadFixture
	api   *threadAPI
	paths Paths
	d     *Driver
}

func newThreadEnv(t *testing.T, mutateCfg func(*agentapi.SessionConfig)) *threadEnv {
	t.Helper()
	f := newThreadFixture(t)
	api := newThreadAPI(t)
	api.plan = agentapi.Plan{Thread: f.plan()}
	api.files["source"] = readBytes(t, f.deltaPath)
	api.cfg = threadConfig(f, mutateCfg)
	paths := testPaths(t)
	paths.ThreadSecrets = filepath.Join(filepath.Dir(paths.Token), "thread-secrets")
	env := &threadEnv{f: f, api: api, paths: paths, d: api.driver(paths)}
	if err := env.d.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return env
}

func (e *threadEnv) wsGit(t *testing.T, args ...string) string {
	t.Helper()
	return runGit(t, e.paths.Workspace, args...)
}

func TestPrepareThreadDelta(t *testing.T) {
	f := newThreadFixture(t)
	api := newThreadAPI(t)
	plan := f.plan()
	plan.HasSetup = true
	plan.SecretFiles = []agentapi.ThreadSecretFile{{Key: "file-0", Path: "config/.env"}}
	plan.Toolchain = &agentapi.ToolchainPlan{}
	api.plan = agentapi.Plan{Thread: plan}
	api.files["source"] = readBytes(t, f.deltaPath)
	api.files["setup"] = buildTarGz(t,
		tarEntry{Name: "AGENTS.md", Body: "# OMP User Preferences"},
		tarEntry{Name: "skills/mine/SKILL.md", Body: "---\nname: mine\n---\nuser skill"},
		tarEntry{Name: "skills/shared/SKILL.md", Body: "user version"},
	)
	paths := testPaths(t)
	paths.ThreadSecrets = filepath.Join(filepath.Dir(paths.Token), "thread-secrets")
	writeFile(t, filepath.Join(paths.ThreadSecrets, "file-0"), "SECRET_KEY=hunter2-hunter2\n")
	d := api.driver(paths)

	if err := d.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	ws := paths.Workspace

	// The branch holds the handoff, with the user's committed and uncommitted work.
	if got := runGit(t, ws, "rev-parse", "HEAD"); got != f.handoff {
		t.Fatalf("HEAD = %s, want the handoff commit %s", got, f.handoff)
	}
	if got := runGit(t, ws, "branch", "--show-current"); got != threadBranch {
		t.Fatalf("branch = %q, want %q", got, threadBranch)
	}
	for _, name := range []string{"README.md", "feature.txt", "wip.txt"} {
		if _, err := os.Stat(filepath.Join(ws, name)); err != nil {
			t.Errorf("workspace misses %s: %v", name, err)
		}
	}
	if got := runGit(t, ws, "rev-parse", refBase); got != f.base {
		t.Fatalf("base ref = %s, want %s", got, f.base)
	}

	// Identity and signing are the thread's; the remote has no credentials.
	if got := runGit(t, ws, "config", "user.email"); got != "ada@example.com" {
		t.Errorf("user.email = %q", got)
	}
	if got := runGit(t, ws, "config", "commit.gpgsign"); got != "false" {
		t.Errorf("commit.gpgsign = %q", got)
	}
	if got := runGit(t, ws, "remote", "get-url", "origin"); got != "https://example.com/app.git" {
		t.Errorf("origin = %q, want the URL without credentials", got)
	}
	if strings.Contains(string(readBytes(t, filepath.Join(ws, ".git", "config"))), "secret-token") {
		t.Error("the repository configuration holds the remote's credentials")
	}

	// The secret file is copied privately and kept out of commits.
	secret := filepath.Join(ws, "config", ".env")
	if got := string(readBytes(t, secret)); got != "SECRET_KEY=hunter2-hunter2\n" {
		t.Fatalf("secret file = %q", got)
	}
	if st, err := os.Stat(secret); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("secret file mode = %v, %v", st.Mode(), err)
	}
	if status := runGit(t, ws, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("the secret file shows up in git status:\n%s", status)
	}

	// The shipped setup is in the agent directory.
	if got := string(readBytes(t, filepath.Join(paths.OmpAgentDir, "AGENTS.md"))); got != "# OMP User Preferences" {
		t.Errorf("AGENTS.md = %q", got)
	}
	if _, err := os.Stat(filepath.Join(paths.SkillsDir, "mine", "SKILL.md")); err != nil {
		t.Errorf("user skill missing: %v", err)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.prepared) != 1 || api.prepared[0] != (agentapi.Prepared{WorkspaceRevision: f.handoff}) {
		t.Fatalf("prepared = %+v, want revision %s and no toolchain", api.prepared, f.handoff)
	}
}

func TestPrepareThreadFullHistory(t *testing.T) {
	f := newThreadFixture(t)
	api := newThreadAPI(t)
	plan := f.plan()
	plan.BaseCommit, plan.BaseRef, plan.FetchURL, plan.RemoteURL = "", "", "", ""
	api.plan = agentapi.Plan{Thread: plan}
	api.files["source"] = readBytes(t, f.fullPath)
	paths := testPaths(t)
	d := api.driver(paths)

	if err := d.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := runGit(t, paths.Workspace, "rev-parse", "HEAD"); got != f.handoff {
		t.Fatalf("HEAD = %s, want %s", got, f.handoff)
	}
	if got := runGit(t, paths.Workspace, "rev-list", "--count", "HEAD"); got != "3" {
		t.Fatalf("history has %s commits, want the full 3", got)
	}
	if got := runGit(t, paths.Workspace, "remote"); got != "" {
		t.Fatalf("remotes = %q, want none without a remote URL", got)
	}
}

func TestPrepareThreadWithoutSourceStartsAtBase(t *testing.T) {
	f := newThreadFixture(t)
	api := newThreadAPI(t)
	plan := f.plan()
	plan.HasSource, plan.HandoffCommit = false, f.base
	api.plan = agentapi.Plan{Thread: plan}
	paths := testPaths(t)

	if err := api.driver(paths).Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := runGit(t, paths.Workspace, "rev-parse", "HEAD"); got != f.base {
		t.Fatalf("HEAD = %s, want the base %s", got, f.base)
	}
}

func TestPrepareThreadBaseMissing(t *testing.T) {
	missing := strings.Repeat("a", 40)
	tests := map[string]func(*agentapi.ThreadPlan, *threadFixture){
		"unknown commit": func(p *agentapi.ThreadPlan, _ *threadFixture) { p.BaseCommit = missing },
		"unknown branch": func(p *agentapi.ThreadPlan, f *threadFixture) { p.BaseRef = "gone"; p.BaseCommit = missing },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			f := newThreadFixture(t)
			api := newThreadAPI(t)
			plan := f.plan()
			mutate(plan, f)
			api.plan = agentapi.Plan{Thread: plan}
			api.files["source"] = readBytes(t, f.deltaPath)

			err := api.driver(testPaths(t)).Prepare(context.Background())
			want := "base commit " + missing + " is not on " + threadRemote + " " + plan.BaseRef +
				"; fetch and push, then hand off again"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want it to contain %q", err, want)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if len(api.prepared) != 0 {
				t.Fatal("a failed prepare must not report prepared")
			}
		})
	}
}

func TestPrepareThreadResultBeatsSource(t *testing.T) {
	f := newThreadFixture(t)
	// A previous sitting continued the handoff by one commit and stored the branch.
	runGit(t, f.local, "checkout", "-q", "-b", threadBranch, f.handoff)
	writeFile(t, filepath.Join(f.local, "done.txt"), "from the cloud\n")
	runGit(t, f.local, "add", "-A")
	runGit(t, f.local, "commit", "-q", "-m", "cloud work")
	result := runGit(t, f.local, "rev-parse", "HEAD")
	resultBundle := filepath.Join(f.root, "result.bundle")
	runGit(t, f.local, "bundle", "create", resultBundle, "refs/heads/"+threadBranch, "^"+f.base)

	api := newThreadAPI(t)
	plan := f.plan()
	plan.HasResult = true
	api.plan = agentapi.Plan{Thread: plan}
	api.files["source"] = readBytes(t, f.deltaPath)
	api.files["result"] = readBytes(t, resultBundle)
	paths := testPaths(t)

	if err := api.driver(paths).Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := runGit(t, paths.Workspace, "rev-parse", "HEAD"); got != result {
		t.Fatalf("HEAD = %s, want the stored result %s", got, result)
	}
	if _, err := os.Stat(filepath.Join(paths.Workspace, "done.txt")); err != nil {
		t.Fatalf("the result's file is missing: %v", err)
	}
}

func TestPrepareThreadRejectsBadSecretPaths(t *testing.T) {
	for _, bad := range []string{"../outside", "/abs", ".git/config", ".git"} {
		t.Run(bad, func(t *testing.T) {
			f := newThreadFixture(t)
			api := newThreadAPI(t)
			plan := f.plan()
			plan.SecretFiles = []agentapi.ThreadSecretFile{{Key: "file-0", Path: bad}}
			api.plan = agentapi.Plan{Thread: plan}
			api.files["source"] = readBytes(t, f.deltaPath)
			paths := testPaths(t)
			paths.ThreadSecrets = filepath.Join(filepath.Dir(paths.Token), "thread-secrets")
			writeFile(t, filepath.Join(paths.ThreadSecrets, "file-0"), "x")

			if err := api.driver(paths).Prepare(context.Background()); err == nil {
				t.Fatalf("a secret file path %q must be refused", bad)
			}
		})
	}
}

func TestPrepareThreadSecretFileCannotEscapeThroughSymlink(t *testing.T) {
	f := newThreadFixture(t)
	outside := t.TempDir()
	// The user's repository holds a symlink out of the workspace where a secret file is to be written.
	if err := os.Symlink(outside, filepath.Join(f.local, "link")); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.local, "add", "-A")
	runGit(t, f.local, "commit", "-q", "-m", "planted symlink")
	handoff := runGit(t, f.local, "rev-parse", "HEAD")
	runGit(t, f.local, "update-ref", refHandoff, handoff)
	runGit(t, f.local, "bundle", "create", f.deltaPath, refHandoff, "^"+f.base)

	api := newThreadAPI(t)
	plan := f.plan()
	plan.HandoffCommit = handoff
	plan.SecretFiles = []agentapi.ThreadSecretFile{{Key: "file-0", Path: "link/stolen.env"}}
	api.plan = agentapi.Plan{Thread: plan}
	api.files["source"] = readBytes(t, f.deltaPath)
	paths := testPaths(t)
	paths.ThreadSecrets = filepath.Join(filepath.Dir(paths.Token), "thread-secrets")
	writeFile(t, filepath.Join(paths.ThreadSecrets, "file-0"), "SECRET=abcdefghij")

	if err := api.driver(paths).Prepare(context.Background()); err == nil {
		t.Fatal("writing through a symlink that leaves the workspace must fail")
	}
	if _, err := os.Stat(filepath.Join(outside, "stolen.env")); err == nil {
		t.Fatal("the secret file was written outside the workspace")
	}
}

func TestPrepareThreadToolchainCache(t *testing.T) {
	const miseToml = "[tools]\ngo = \"1.27\"\n"
	disable := []string{"node"}
	// setup is a thread whose handoff carries a mise.toml, with the given fake mise. It also
	// returns the toolchain hash prepare has to compute for that workspace.
	setup := func(t *testing.T, mise string) (*threadAPI, *Driver, Paths, string) {
		t.Helper()
		f := newThreadFixture(t)
		writeFile(t, filepath.Join(f.local, miseTOML), miseToml)
		runGit(t, f.local, "add", "-A")
		runGit(t, f.local, "commit", "-q", "-m", "mise")
		handoff := runGit(t, f.local, "rev-parse", "HEAD")
		runGit(t, f.local, "update-ref", refHandoff, handoff)
		runGit(t, f.local, "bundle", "create", f.deltaPath, refHandoff, "^"+f.base)

		api := newThreadAPI(t)
		plan := f.plan()
		plan.HandoffCommit = handoff
		plan.Toolchain = &agentapi.ToolchainPlan{Disable: disable}
		api.plan = agentapi.Plan{Thread: plan}
		api.files["source"] = readBytes(t, f.deltaPath)
		paths := testPaths(t)
		d := api.driver(paths)
		d.MiseBin = writeScript(t, mise)

		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, miseTOML), miseToml)
		hash, err := ToolchainHash(dir, disable)
		if err != nil || hash == "" {
			t.Fatalf("expected hash = %q, %v", hash, err)
		}
		return api, d, paths, hash
	}

	t.Run("miss installs and caches", func(t *testing.T) {
		api, d, _, hash := setup(t, shebang+"mkdir -p \"$MISE_DATA_DIR/installs\" && echo tool > \"$MISE_DATA_DIR/installs/go\"\n")
		if err := d.Prepare(context.Background()); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		api.mu.Lock()
		defer api.mu.Unlock()
		put, ok := api.toolchainPut[hash]
		if !ok {
			t.Fatalf("toolchain was not cached under %s; puts: %v", hash, api.toolchainPut)
		}
		if members := tarMembers(t, put); members["data/installs/go"] != "tool\n" {
			t.Fatalf("cached toolchain members = %v", members)
		}
		if len(api.prepared) != 1 || api.prepared[0].ToolchainHash != hash {
			t.Fatalf("prepared = %+v, want toolchain hash %s", api.prepared, hash)
		}
	})

	t.Run("hit extracts without installing", func(t *testing.T) {
		api, d, paths, hash := setup(t, shebang+"exit 42\n") // mise must not run
		api.tarballs["/toolchains/"+hash] = buildTarGz(t, tarEntry{Name: "data/installs/go", Body: "cached\n"})
		if err := d.Prepare(context.Background()); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if got := string(readBytes(t, filepath.Join(paths.MiseDir, "data", "installs", "go"))); got != "cached\n" {
			t.Fatalf("restored toolchain = %q", got)
		}
		api.mu.Lock()
		defer api.mu.Unlock()
		if len(api.toolchainPut) != 0 {
			t.Fatal("a cache hit must not upload")
		}
	})

	t.Run("failed install is an event, not an error", func(t *testing.T) {
		api, d, _, _ := setup(t, shebang+"echo broken >&2\nexit 3\n")
		if err := d.Prepare(context.Background()); err != nil {
			t.Fatalf("Prepare must survive a failed toolchain install: %v", err)
		}
		api.mu.Lock()
		defer api.mu.Unlock()
		var found bool
		for _, e := range api.events {
			found = found || (e.Kind == eventToolchain && strings.HasPrefix(e.Text, "toolchain install failed: "))
		}
		if !found || len(api.toolchainPut) != 0 || len(api.prepared) != 1 {
			t.Fatalf("events = %+v, puts = %v, prepared = %+v", api.events, api.toolchainPut, api.prepared)
		}
	})
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
