//go:build e2e

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

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	omprpc "github.com/can1357/oh-my-pi/sdk/go/omp-rpc"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/TechDufus/omp-operator/test/fakeprovider"
)

const (
	cloudAgent     = "cloud-fake"
	gitServerHost  = "gitserver.e2e.svc.cluster.local"
	gitServerToken = "gitserver-token"
	secretEnvName  = "E2E_SECRET_ENV"
	secretEnvValue = "e2e-secret-env-value"
	dotenvValue    = "super-secret-value-123"
	wipSubject     = "omp-cloud: local changes at handoff"
	localSubject   = "local unpushed commit"
	dirtyReadme    = "# app\nline one\ndirty edit\n"
	// cloudMaxSessions is limits.maxConcurrentSessions of the cloud agent below.
	cloudMaxSessions = 4
)

// The cloud agent: the e2e image (driver + fakeomp + git), every built-in tool, cloudMaxSessions
// concurrent sittings (several threads run at once and each sitting idles a minute), a sitting
// that ends after the shortest idle time the API accepts, a question timeout of the shortest
// the API accepts, and the git server below as the one git host, with push on. FAKEOMP_ECHO_ENV
// makes fakeomp leak the secret env value into every reply so redaction can be checked.
const cloudAgentYAML = `apiVersion: omp.techdufus.com/v1alpha1
kind: OmpAgent
metadata:
  name: ` + cloudAgent + `
  namespace: e2e
spec:
  model:
    default: fake/fake-model
  tools:
    allow: ["*"]
  toolchain: {}
  limits:
    maxConcurrentSessions: 4
    retryDelay: 30s
  cloud:
    idleTimeout: 1m
    questionTimeout: 1m
    git:
      - host: ` + gitServerHost + `
        auth:
          tokenSecretRef: {name: ` + gitServerToken + `, key: token}
        push: true
  pod:
    env:
      - name: FAKEOMP_ECHO_ENV
        value: ` + secretEnvName + `
`

func gitServerYAML(image string) string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: gitserver
  namespace: e2e
spec:
  replicas: 1
  selector:
    matchLabels: {app: gitserver}
  template:
    metadata:
      labels: {app: gitserver}
    spec:
      containers:
        - name: gitserver
          image: ` + image + `
          imagePullPolicy: IfNotPresent
          command: ["/usr/local/bin/omp-e2e-gitserver"]
          env:
            - {name: GITSERVER_REPOS, value: app}
            - name: GITSERVER_TOKEN
              valueFrom:
                secretKeyRef: {name: ` + gitServerToken + `, key: token}
          ports:
            - containerPort: 8080
          readinessProbe:
            tcpSocket: {port: 8080}
          volumeMounts:
            - {name: repos, mountPath: /repos}
      volumes:
        - name: repos
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: gitserver
  namespace: e2e
spec:
  selector: {app: gitserver}
  ports:
    - port: 8080
      targetPort: 8080
`
}

// ---- the cloud API, as the owner's client sees it ----

type apiQuestion struct {
	ID         int      `json:"id"`
	Text       string   `json:"text"`
	Selected   []string `json:"selected"`
	Answer     string   `json:"answer"`
	AnsweredBy string   `json:"answeredBy"`
	AnsweredAt string   `json:"answeredAt"`
	Expired    bool     `json:"expired"`
}

type apiTurn struct {
	ID           int           `json:"id"`
	State        string        `json:"state"`
	Text         string        `json:"text"`
	By           string        `json:"by"`
	Reply        string        `json:"reply"`
	Title        string        `json:"title"`
	Error        string        `json:"error"`
	Head         string        `json:"head"`
	FilesChanged int           `json:"filesChanged"`
	Pushed       bool          `json:"pushed"`
	PushError    string        `json:"pushError"`
	Questions    []apiQuestion `json:"questions"`
}

// apiPendingQuestion is the thread summary's question: the oldest unanswered one.
type apiPendingQuestion struct {
	Turn    int    `json:"turn"`
	ID      int    `json:"id"`
	Text    string `json:"text"`
	Options []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"options"`
	Remaining int `json:"remaining"`
}

type apiThread struct {
	Name          string              `json:"name"`
	Agent         string              `json:"agent"`
	Owner         string              `json:"owner"`
	Phase         string              `json:"phase"`
	ActiveSession string              `json:"activeSession"`
	Question      *apiPendingQuestion `json:"question"`
	Turns         []apiTurn           `json:"turns"`
	Repo          struct {
		BaseCommit    string `json:"baseCommit"`
		HandoffCommit string `json:"handoffCommit"`
		RemoteURL     string `json:"remoteURL"`
	} `json:"repo"`
	Paths struct {
		Home      string `json:"home"`
		Workspace string `json:"workspace"`
	} `json:"paths"`
}

type apiEvent struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// whoAmIAgent is an agent of GET /api/v1/whoami with the capacity and thread counts.
type whoAmIAgent struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Ready     bool   `json:"ready"`
	Capacity  struct {
		MaxConcurrentSessions int `json:"maxConcurrentSessions"`
		Running               int `json:"running"`
	} `json:"capacity"`
	Threads struct {
		Working int `json:"working"`
		Waiting int `json:"waiting"`
		Idle    int `json:"idle"`
	} `json:"threads"`
}

type whoAmI struct {
	User   string        `json:"user"`
	Agents []whoAmIAgent `json:"agents"`
}

// agent returns the named agent of the e2e namespace, or nil.
func (w whoAmI) agent(name string) *whoAmIAgent {
	for i := range w.Agents {
		if w.Agents[i].Name == name && w.Agents[i].Namespace == agentNS {
			return &w.Agents[i]
		}
	}
	return nil
}

type cloudAPI struct{ base string }

func (a cloudAPI) call(method, path, token string, body io.Reader, contentType string) (int, []byte) {
	GinkgoHelper()
	req, err := http.NewRequest(method, a.base+path, body)
	Expect(err).NotTo(HaveOccurred())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	return resp.StatusCode, b
}

func (a cloudAPI) get(path string, out any) {
	GinkgoHelper()
	code, body := a.call(http.MethodGet, path, cloudToken, nil, "")
	Expect(code).To(Equal(http.StatusOK), string(body))
	if out != nil {
		Expect(json.Unmarshal(body, out)).To(Succeed(), string(body))
	}
}

func threadPath(name string) string { return "/api/v1/namespaces/" + agentNS + "/threads/" + name }

func (a cloudAPI) thread(name string) apiThread {
	GinkgoHelper()
	var t apiThread
	a.get(threadPath(name), &t)
	return t
}

func (a cloudAPI) events(name string) []apiEvent {
	GinkgoHelper()
	var r struct {
		Events []apiEvent `json:"events"`
	}
	a.get(threadPath(name)+"/events", &r)
	return r.Events
}

func (t apiThread) turn(id int) *apiTurn {
	for i := range t.Turns {
		if t.Turns[i].ID == id {
			return &t.Turns[i]
		}
	}
	return nil
}

// waitTurn waits for turn id of a thread to reach state; a failed turn fails the wait unless
// failed is the state asked for.
func (a cloudAPI) waitTurn(name string, id int, state string, timeout time.Duration) apiTurn {
	GinkgoHelper()
	var got apiTurn
	Eventually(func() string {
		turn := a.thread(name).turn(id)
		if turn == nil {
			return "missing"
		}
		got = *turn
		if turn.State == "failed" && state != "failed" {
			StopTrying(fmt.Sprintf("turn %d of %s failed: %s", id, name, turn.Error)).Now()
		}
		return turn.State
	}, timeout, time.Second).Should(Equal(state), "turn %d of %s", id, name)
	return got
}

// waitPhase waits for a thread to reach a phase; a failed turn fails the wait.
func (a cloudAPI) waitPhase(name, phase string, timeout time.Duration) apiThread {
	GinkgoHelper()
	var got apiThread
	Eventually(func() string {
		got = a.thread(name)
		for _, t := range got.Turns {
			if t.State == "failed" {
				StopTrying(fmt.Sprintf("turn %d of %s failed: %s", t.ID, name, t.Error)).Now()
			}
		}
		return got.Phase
	}, timeout, time.Second).Should(Equal(phase), "thread %s", name)
	return got
}

// modelSaw reports whether some request to the local model ended with a message containing substr.
func modelSaw(fp *fakeprovider.Server, substr string) bool {
	for _, req := range fp.Requests() {
		if len(req.Messages) > 0 && strings.Contains(req.Messages[len(req.Messages)-1].Text(), substr) {
			return true
		}
	}
	return false
}

// ---- the owner's omp ----

// localOmp is a real `omp --mode rpc -e clients/omp-cloud`: the owner's omp, in a throw-away
// HOME and repository, which hands work to the cloud with /cloud.
type localOmp struct {
	client *omprpc.Client
	home   string
	repo   string
	gitEnv []string

	mu    sync.Mutex
	notes []string // every extension notify message, in order, "error: " prefixed for errors
}

func (l *localOmp) record(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.notes = append(l.notes, msg)
}

func (l *localOmp) noteCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.notes)
}

func (l *localOmp) notesSince(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.notes[n:])
}

const errorNote = "error: "

// waitNote waits for a notification after position since that contains substr. An error
// notification that does not contain it fails the wait, unless errors are expected.
func (l *localOmp) waitNote(since int, substr string, errorsExpected bool, timeout time.Duration) string {
	GinkgoHelper()
	var found string
	Eventually(func() bool {
		for _, n := range l.notesSince(since) {
			if strings.Contains(n, substr) {
				found = n
				return true
			}
			if !errorsExpected && strings.HasPrefix(n, errorNote) {
				StopTrying("unexpected error notification: " + n).Now()
			}
		}
		return false
	}, timeout, 500*time.Millisecond).Should(BeTrue(),
		"no notification containing %q; got %q", substr, l.notesSince(since))
	return found
}

// cloud sends `/cloud <args>` as a prompt and returns the notification position before it.
func (l *localOmp) cloud(args string) int {
	GinkgoHelper()
	since := l.noteCount()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ack, err := l.client.Prompt(ctx, omprpc.PromptCommand{Message: "/cloud " + args})
	Expect(err).NotTo(HaveOccurred())
	Expect(ack.AgentInvoked == nil || !*ack.AgentInvoked).To(BeTrue(),
		"/cloud must be handled by the extension, not the model")
	return since
}

func (l *localOmp) git(args ...string) string {
	GinkgoHelper()
	out, err := gitIn(l.repo, l.gitEnv, args...)
	Expect(err).NotTo(HaveOccurred())
	return out
}

func gitIn(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	_, _ = fmt.Fprintf(GinkgoWriter, "running in %s: git %s\n", dir, strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w\n%s%s", strings.Join(args, " "), err, out.String(), errOut.String())
	}
	return out.String(), nil
}

// scrubbedEnv is the process environment without anything that could point the owner's omp
// or git at the real home directory.
func scrubbedEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "HOME="), strings.HasPrefix(kv, "PI_"), strings.HasPrefix(kv, "OMP_CLOUD_"),
			strings.HasPrefix(kv, "GIT_"), strings.HasPrefix(kv, "XDG_"):
		default:
			env = append(env, kv)
		}
	}
	return env
}

func mustWrite(path, content string) {
	GinkgoHelper()
	Expect(os.MkdirAll(filepath.Dir(path), 0o755)).To(Succeed())
	Expect(os.WriteFile(path, []byte(content), 0o644)).To(Succeed())
}

// startLocalOmp builds the owner's HOME and repository and starts omp with the extension.
// gitBase is the port-forwarded git server; origin keeps its in-cluster URL, as it would for a
// user who clones from the cluster's git host.
func startLocalOmp(ompBin, apiBase, gitBase, gitToken string, fp *fakeprovider.Server) *localOmp {
	GinkgoHelper()
	root, err := filepath.EvalSymlinks(projectDir)
	Expect(err).NotTo(HaveOccurred())
	root = filepath.Join(root, "bin", "e2e-client")
	Expect(os.RemoveAll(root)).To(Succeed())
	l := &localOmp{home: filepath.Join(root, "home"), repo: filepath.Join(root, "repo")}
	Expect(os.MkdirAll(l.repo, 0o755)).To(Succeed())

	agentDir := filepath.Join(l.home, ".omp", "agent")
	mustWrite(filepath.Join(agentDir, "config.yml"),
		"modelRoles:\n  default: "+fakeprovider.Model+"\n  smol: openai-codex/x\n")
	mustWrite(filepath.Join(agentDir, "models.yml"), fp.ModelsYAML())
	mustWrite(filepath.Join(agentDir, "AGENTS.md"), "# OMP User Preferences\n\ne2e marker\n")
	mustWrite(filepath.Join(agentDir, "skills", "demo", "SKILL.md"),
		"---\nname: demo\ndescription: A demo skill. Use when asked about the demo.\n---\nThe demo says hello.\n")
	mustWrite(filepath.Join(agentDir, "extensions", "marker.ts"), "export default function marker() {}\n")

	l.gitEnv = append(scrubbedEnv(), "HOME="+l.home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0")
	origin := "http://" + gitServerHost + ":8080/app.git"
	forwarded := strings.Replace(gitBase, "http://", "http://x-access-token:"+gitToken+"@", 1) + "/"
	l.git("init", "-q", "-b", "main")
	l.git("config", "user.name", "E2E User")
	l.git("config", "user.email", "e2e@example.com")
	l.git("remote", "add", "origin", origin)
	// The raw remote URL is the in-cluster one; this machine reaches it through the port-forward.
	l.git("config", "url."+forwarded+".insteadOf", "http://"+gitServerHost+":8080/")
	mustWrite(filepath.Join(l.repo, "README.md"), "# app\nline one\n")
	mustWrite(filepath.Join(l.repo, ".gitignore"), ".env\n")
	l.git("add", "-A")
	l.git("commit", "-q", "-m", "initial")
	l.git("push", "-q", "-u", "origin", "main")
	// What a handoff has to carry: an unpushed commit, a dirty tracked file, an untracked file, a
	// gitignored secret file and the repo's cloud settings.
	mustWrite(filepath.Join(l.repo, "local.txt"), "local\n")
	l.git("add", "local.txt")
	l.git("commit", "-q", "-m", localSubject)
	mustWrite(filepath.Join(l.repo, "README.md"), dirtyReadme)
	mustWrite(filepath.Join(l.repo, "untracked.txt"), "untracked\n")
	mustWrite(filepath.Join(l.repo, ".env"), "SECRET_VALUE="+dotenvValue+"\n")
	mustWrite(filepath.Join(l.repo, ".omp", "cloud.json"),
		`{"secretFiles":[".env"],"secretEnv":["`+secretEnvName+`"]}`+"\n")

	cmd := exec.Command(ompBin, "--mode", "rpc", "--approval-mode", "yolo",
		"-e", filepath.Join(projectDir, "clients", "omp-cloud"))
	cmd.Dir = l.repo
	cmd.Env = append(scrubbedEnv(), "HOME="+l.home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0", "OMP_CLOUD_URL="+apiBase, "OMP_CLOUD_TOKEN="+cloudToken,
		"OMP_CLOUD_NAMESPACE="+agentNS, "OMP_CLOUD_AGENT="+cloudAgent, secretEnvName+"="+secretEnvValue)
	cmd.Stderr = GinkgoWriter
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	l.client, err = omprpc.Start(ctx, cmd)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { _ = l.client.Close() })
	go l.drain()
	return l
}

// card is an omp-cloud card in the owner's transcript.
type card struct {
	Kind, Origin, Thread, Content string
}

// cards reads the cards from the owner's current session file.
func (l *localOmp) cards() []card {
	GinkgoHelper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	state, err := l.client.GetState(ctx)
	Expect(err).NotTo(HaveOccurred())
	if state.SessionFile == nil {
		return nil
	}
	data, err := os.ReadFile(*state.SessionFile)
	if err != nil {
		return nil
	}
	var out []card
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		var e struct {
			Type       string `json:"type"`
			CustomType string `json:"customType"`
			Content    string `json:"content"`
			Details    struct {
				Kind   string `json:"kind"`
				Origin string `json:"origin"`
				Thread string `json:"thread"`
			} `json:"details"`
		}
		if json.Unmarshal(line, &e) != nil || e.Type != "custom_message" || e.CustomType != "omp-cloud" {
			continue
		}
		out = append(out, card{Kind: e.Details.Kind, Origin: e.Details.Origin, Thread: e.Details.Thread, Content: e.Content})
	}
	return out
}

// waitCard waits for a card of kind about thread in the owner's transcript.
func (l *localOmp) waitCard(kind, thread string, timeout time.Duration) card {
	GinkgoHelper()
	var found card
	Eventually(func() bool {
		for _, c := range l.cards() {
			if c.Kind == kind && c.Thread == thread {
				found = c
				return true
			}
		}
		return false
	}, timeout, time.Second).Should(BeTrue(), "no %s card for %s in the owner's transcript", kind, thread)
	return found
}

// drain records the extension's notifications; it must run for omp to make progress.
func (l *localOmp) drain() {
	for frame := range l.client.Frames() {
		req, ok := frame.Value.(omprpc.ExtensionUiRequest)
		if !ok {
			continue
		}
		if n, ok := req.Value.(omprpc.NotifyUiRequest); ok {
			msg := n.Message
			if n.NotifyType != nil && *n.NotifyType == omprpc.NotifyTypeError {
				msg = errorNote + msg
			}
			_, _ = fmt.Fprintf(GinkgoWriter, "notify: %s\n", msg)
			l.record(msg)
		}
	}
}

// tarNames lists the file names of a (gzip-compressed or plain) tar archive.
func tarNames(archive []byte) []string {
	GinkgoHelper()
	var r io.Reader = bytes.NewReader(archive)
	if len(archive) > 2 && archive[0] == 0x1f && archive[1] == 0x8b {
		zr, err := gzip.NewReader(r)
		Expect(err).NotTo(HaveOccurred())
		r = zr
	}
	var names []string
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		Expect(err).NotTo(HaveOccurred())
		if h.Typeflag == tar.TypeReg {
			names = append(names, filepath.Base(h.Name))
		}
	}
	return names
}

// script is a fakeomp turn script as the prompt `FAKEOMP:<json>`; encoding/json gives the
// compact, space-free form the slash command passes through untouched.
func script(v map[string]any) string {
	GinkgoHelper()
	raw, err := json.Marshal(v)
	Expect(err).NotTo(HaveOccurred())
	return "FAKEOMP:" + string(raw)
}

var _ = Describe("cloud threads", Ordered, func() {
	var (
		api      cloudAPI
		web      *portForward
		metrics  *portForward
		local    *localOmp
		fp       *fakeprovider.Server
		thread   string // the thread of the handoff
		hang     string // the thread that is stopped
		turn1    apiTurn
		turn2    apiTurn
		turn3    apiTurn
		branch   string
		sittings = func(name string) []string {
			names := make([]string, 0, 2)
			for _, s := range list("ompsessions", agentNS, apiGroup+"/thread="+name) {
				names = append(names, s.GetName())
			}
			slices.Sort(names)
			return names
		}
		waitSitting = func(name string, timeout time.Duration) *unstructured.Unstructured {
			var s *unstructured.Unstructured
			Eventually(func() string {
				s = get("ompsessions", name)
				if str(s, "status", "phase") == "Failed" {
					StopTrying(fmt.Sprintf("sitting %s Failed: %s", name, str(s, "status", "error"))).Now()
				}
				return str(s, "status", "phase")
			}, timeout, 2*time.Second).Should(Equal("Succeeded"), "sitting %s", name)
			return s
		}
	)

	BeforeAll(func() {
		ompBin := os.Getenv("OMP_BIN")
		Expect(ompBin).NotTo(BeEmpty(), "OMP_BIN must name the pinned omp (mise run test-e2e sets it)")

		gitToken := randomToken()
		mustApply(secretYAML(agentNS, gitServerToken, map[string]string{"token": gitToken}))
		mustApply(gitServerYAML(e2eImage))
		mustRun("kubectl", "-n", agentNS, "rollout", "status", "deployment/gitserver", "--timeout=3m")
		mustApply(cloudAgentYAML)
		expectReady("ompagents", cloudAgent)

		web = startPortForward(8080)
		metrics = startPortForward(9090)
		gitPF := startPortForwardIn(agentNS, "gitserver", 8080)
		api = cloudAPI{base: web.Base}

		By("starting the owner's omp with the omp-cloud extension and taking one local turn")
		fp = fakeprovider.New(suiteT)
		local = startLocalOmp(ompBin, web.Base, gitPF.Base, gitToken, fp)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		res, err := local.client.PromptAndWait(ctx, omprpc.PromptCommand{Message: "say hello locally"}, 2*time.Minute)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Result).NotTo(BeNil())
		Expect(fp.Count()).To(BeNumerically(">=", 1))
	})

	It("hands the repository and conversation off with /cloud", func() {
		since := local.cloud(script(map[string]any{
			"files": map[string]string{"hello.txt": "hello\n"},
			"checkFiles": []string{
				".env",
				filepath.Join(local.home, ".omp/agent/AGENTS.md"),
				filepath.Join(local.home, ".omp/agent/skills/demo/SKILL.md"),
				filepath.Join(local.home, ".omp/agent/extensions/marker.ts"),
			},
			"checkEnv": []string{secretEnvName},
		}))
		started := local.waitNote(since, " started on e2e/"+cloudAgent, false, 3*time.Minute)
		m := regexp.MustCompile(`^☁ (\S+) started on e2e/` + cloudAgent).FindStringSubmatch(started)
		Expect(m).NotTo(BeNil(), started)
		thread = m[1]
		branch = "omp-cloud/" + thread
		Expect(thread).To(HavePrefix(cloudAgent + "-"))

		detail := api.thread(thread)
		Expect(detail.Owner).To(Equal("token:e2e"))
		Expect(detail.Paths.Workspace).To(Equal(local.repo), "the cloud agent works at the same repo path")
		Expect(detail.Paths.Home).To(Equal(local.home), "the cloud agent works at the same home path")
		Expect(detail.Repo.RemoteURL).To(Equal("http://" + gitServerHost + ":8080/app.git"))
		Expect(detail.Repo.BaseCommit).To(Equal(strings.TrimSpace(local.git("rev-parse", "origin/main"))))
		Expect(detail.Repo.HandoffCommit).NotTo(Equal(detail.Repo.BaseCommit))
		Expect(detail.Turns[0].State).To(BeElementOf("queued", "running", "done"))
	})

	It("runs turn 1, then turn 2 in the same sitting", func() {
		turn1 = api.waitTurn(thread, 1, "done", 5*time.Minute)
		// The sitting is warm for one idle timeout; send the follow-up straight away.
		since := local.cloud("send " + thread + " " + script(map[string]any{
			"files": map[string]string{"second.txt": "2\n"},
		}))
		local.waitNote(since, "queued turn 2 on "+thread, false, time.Minute)
		turn2 = api.waitTurn(thread, 2, "done", 3*time.Minute)

		Expect(sittings(thread)).To(Equal([]string{thread + "-1"}), "a warm follow-up must reuse the sitting")
		Expect(turn1.Pushed).To(BeTrue(), "push error: %q", turn1.PushError)
		Expect(turn2.Pushed).To(BeTrue(), "push error: %q", turn2.PushError)
		Expect(turn1.Head).To(MatchRegexp(`^[0-9a-f]{40}$`))
		Expect(turn2.Head).NotTo(Equal(turn1.Head))
	})

	It("pushed the WIP commit, the new files and the check results to origin, without .env", func() {
		ref := "refs/remotes/" + "origin/" + branch
		local.git("fetch", "-q", "origin", "+refs/heads/"+branch+":"+ref)
		Expect(strings.TrimSpace(local.git("rev-parse", ref))).To(Equal(turn2.Head))

		show := func(rev, file string) string { return local.git("show", rev+":"+file) }
		Expect(show(turn1.Head, "hello.txt")).To(Equal("hello\n"))
		Expect(show(turn2.Head, "second.txt")).To(Equal("2\n"))

		check := strings.Split(strings.TrimSpace(show(turn1.Head, "fakeomp-check.txt")), "\n")
		Expect(check[0]).To(Equal("cwd: " + local.repo))
		Expect(check).To(ContainElements(
			".env: present",
			filepath.Join(local.home, ".omp/agent/AGENTS.md")+": present",
			filepath.Join(local.home, ".omp/agent/skills/demo/SKILL.md")+": present",
			filepath.Join(local.home, ".omp/agent/extensions/marker.ts")+": present",
			secretEnvName+": set",
		))

		// The WIP commit is the dirty edit plus the untracked file, on top of the unpushed commit.
		var wipCommits []string
		for line := range strings.SplitSeq(strings.TrimSpace(local.git("log", "--format=%H %s", ref)), "\n") {
			if sha, subject, _ := strings.Cut(line, " "); subject == wipSubject {
				wipCommits = append(wipCommits, sha)
			}
		}
		Expect(wipCommits).To(HaveLen(1), "exactly one WIP commit")
		wip := wipCommits[0]
		Expect(show(wip, "README.md")).To(Equal(dirtyReadme))
		Expect(show(wip, "untracked.txt")).To(Equal("untracked\n"))
		Expect(strings.TrimSpace(local.git("log", "-1", "--format=%s", wip+"^"))).To(Equal(localSubject))
		originMain := strings.TrimSpace(local.git("rev-parse", "origin/main"))
		Expect(strings.TrimSpace(local.git("merge-base", ref, "origin/main"))).To(Equal(originMain))

		// The gitignored secret file never reaches the branch, in any commit or message.
		Expect(strings.TrimSpace(local.git("log", "--format=%H", ref, "--", ".env"))).To(BeEmpty())
		_, err := gitIn(local.repo, local.gitEnv, "grep", "-q", "-F", dotenvValue, ref)
		Expect(err).To(HaveOccurred(), "the .env value is in the pushed tree")
		Expect(local.git("log", "--format=%B", ref)).NotTo(ContainSubstring(secretEnvValue))
	})

	It("redacts shipped secrets from replies and events", func() {
		Expect(turn1.Reply).To(And(ContainSubstring("[redacted:"), Not(ContainSubstring(secretEnvValue))))
		Eventually(func() string {
			var b strings.Builder
			for _, e := range api.events(thread) {
				b.WriteString(e.Kind + ": " + e.Text + "\n")
			}
			return b.String()
		}, time.Minute, 2*time.Second).Should(And(ContainSubstring("[redacted:"),
			Not(ContainSubstring(secretEnvValue)), Not(ContainSubstring(dotenvValue))))
	})

	It("restores a cold thread in a second sitting after the idle timeout", func() {
		first := waitSitting(thread+"-1", 4*time.Minute)
		Expect(str(first, "status", "verdict")).To(BeEmpty(), "a thread sitting ends without a verdict")
		Eventually(func() string { return str(get("ompthreads", thread), "status", "phase") }, time.Minute, 2*time.Second).
			Should(Equal("Idle"))

		since := local.cloud("send " + thread + " " + script(map[string]any{
			"files": map[string]string{"third.txt": "3\n"},
		}))
		local.waitNote(since, "queued turn 3 on "+thread, false, time.Minute)
		turn3 = api.waitTurn(thread, 3, "done", 5*time.Minute)
		Expect(sittings(thread)).To(Equal([]string{thread + "-1", thread + "-2"}))
		Expect(turn3.Pushed).To(BeTrue(), "push error: %q", turn3.PushError)

		second := waitSitting(thread+"-2", 4*time.Minute)
		restored, _, _ := unstructured.NestedBool(second.Object, "status", "continuity", "restored")
		Expect(restored).To(BeTrue(), "the second sitting must restore the conversation")
	})

	It("pulls the result branch back with /cloud pull", func() {
		since := local.cloud("pull " + thread)
		local.waitNote(since, branch+" → "+turn3.Head[:8], false, time.Minute)
		Expect(strings.TrimSpace(local.git("rev-parse", "refs/heads/"+branch))).To(Equal(turn3.Head))
		for file, content := range map[string]string{"hello.txt": "hello\n", "second.txt": "2\n", "third.txt": "3\n"} {
			Expect(local.git("show", "refs/heads/"+branch+":"+file)).To(Equal(content))
		}
		_, err := gitIn(local.repo, local.gitEnv, "merge-base", "--is-ancestor", turn2.Head, turn3.Head)
		Expect(err).NotTo(HaveOccurred(), "turn 3 must build on turn 2 after the cold restore")
	})

	It("resumes the conversation: refuses a dirty tree, then switches branch and session", func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		before, err := local.client.GetState(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(before.SessionFile).NotTo(BeNil())

		since := local.cloud("resume " + thread)
		local.waitNote(since, "commit or stash", true, time.Minute)

		local.git("stash", "push", "-u", "-q")
		since = local.cloud("resume " + thread)
		local.waitNote(since, "resumed "+thread+" on branch "+branch, false, 2*time.Minute)

		Expect(strings.TrimSpace(local.git("branch", "--show-current"))).To(Equal(branch))
		after, err := local.client.GetState(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(after.SessionFile).NotTo(BeNil())
		file := *after.SessionFile
		Expect(file).NotTo(Equal(*before.SessionFile))
		Expect(after.SessionID).NotTo(Equal(before.SessionID))
		Expect(file).To(HavePrefix(filepath.Join(local.home, ".omp", "agent", "sessions")+string(filepath.Separator)),
			"the resumed conversation lives in the HOME session bucket")

		// It is the conversation the handoff shipped (new id, parentSession) and the thread kept.
		raw, err := os.ReadFile(file)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(And(ContainSubstring(`"parentSession"`), ContainSubstring(after.SessionID)))
		code, body := api.call(http.MethodGet, threadPath(thread)+"/conversation", cloudToken, nil, "")
		Expect(code).To(Equal(http.StatusOK))
		Expect(tarNames(body)).To(ContainElement(filepath.Base(file)))
	})

	It("stops a running turn with /cloud stop and explains an unknown thread", func() {
		// Back on main with a clean tree: a handoff at the pushed base needs no source bundle.
		local.git("switch", "-q", "main")
		since := local.cloud(script(map[string]any{"mode": "hang"}))
		started := local.waitNote(since, " started on e2e/"+cloudAgent, false, 3*time.Minute)
		m := regexp.MustCompile(`^☁ (\S+) started on`).FindStringSubmatch(started)
		Expect(m).NotTo(BeNil(), started)
		hang = m[1]

		Eventually(func() string {
			if t := api.thread(hang).turn(1); t != nil {
				return t.State
			}
			return ""
		}, 3*time.Minute, time.Second).Should(Equal("running"))

		since = local.cloud("stop " + hang)
		local.waitNote(since, "stopped "+hang+": running yes", false, time.Minute)
		stopped := api.waitTurn(hang, 1, "failed", 3*time.Minute)
		Expect(stopped.Error).To(ContainSubstring("stopped by token:e2e"))
		// The live watcher reports the failed turn within one poll. It is an error notification and
		// would otherwise land in the notification window of whatever spec comes next.
		local.waitNote(since, hang+" turn 1 failed: stopped by token:e2e", true, time.Minute)

		since = local.cloud("stop the flaky test")
		hint := local.waitNote(since, "/cloud new", true, time.Minute)
		Expect(hint).To(ContainSubstring(`no thread matches "the"`))
	})

	It("authenticates the cloud API and reports capacity and thread counts", func() {
		var who whoAmI
		api.get("/api/v1/whoami", &who)
		Expect(who.User).To(Equal("token:e2e"))
		agent := who.agent(cloudAgent)
		Expect(agent).NotTo(BeNil())
		Expect(agent.Ready).To(BeTrue())
		Expect(agent.Capacity.MaxConcurrentSessions).To(Equal(cloudMaxSessions))
		Expect(agent.Capacity.Running).To(And(BeNumerically(">=", 0), BeNumerically("<=", cloudMaxSessions)))
		var listed struct {
			Threads []struct {
				Name  string `json:"name"`
				Agent string `json:"agent"`
			} `json:"threads"`
		}
		api.get("/api/v1/namespaces/"+agentNS+"/threads", &listed)
		counts := agent.Threads
		mine := 0
		for _, t := range listed.Threads {
			if t.Agent == cloudAgent {
				mine++
			}
		}
		Expect(counts.Working+counts.Waiting+counts.Idle).To(Equal(mine),
			"whoami counts %+v, the list has %d threads of %s", counts, mine, cloudAgent)
		Expect(counts.Idle).To(BeNumerically(">=", 1), "the handoff thread is idle")

		code, body := api.call(http.MethodPost, "/api/v1/namespaces/"+agentNS+"/threads", viewerToken,
			strings.NewReader("{}"), "application/json")
		Expect(code).To(Equal(http.StatusForbidden), string(body))
		Expect(string(body)).To(ContainSubstring("operator role required"))
		code, body = api.call(http.MethodGet, threadPath(thread), viewerToken, nil, "")
		Expect(code).To(Equal(http.StatusOK), string(body))

		code, _ = api.call(http.MethodPost, "/api/v1/namespaces/"+agentNS+"/threads", randomToken(),
			strings.NewReader("{}"), "application/json")
		Expect(code).To(Equal(http.StatusUnauthorized))
		code, _ = api.call(http.MethodGet, "/api/v1/whoami", "", nil, "")
		Expect(code).To(Equal(http.StatusUnauthorized))
	})

	It("shows the thread on the dashboard", func() {
		creds := base64.StdEncoding.EncodeToString([]byte(dashUser + ":" + dashPass))
		code, body, err := httpDo(http.MethodGet, web.Base+"/threads/"+agentNS+"/"+thread, nil,
			map[string]string{"Authorization": "Basic " + creds})
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusOK), body)
		Expect(body).To(And(ContainSubstring("<h2>Turns</h2>"), ContainSubstring(thread),
			ContainSubstring("fake reply"), ContainSubstring(turn3.Head), ContainSubstring("/cloud pull")))
		Expect(body).NotTo(ContainSubstring(secretEnvValue))

		code, _, err = httpDo(http.MethodGet, web.Base+"/threads/"+agentNS+"/"+thread, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusUnauthorized))
	})

	It("exposes thread metrics", func() {
		Eventually(func() string {
			_, body, err := httpDo(http.MethodGet, metrics.Base+"/metrics", nil, nil)
			if err != nil {
				return err.Error()
			}
			var hits []string
			for line := range strings.SplitSeq(body, "\n") {
				if strings.HasPrefix(line, "omp_operator_thread_turns_total{") && strings.Contains(line, `state="done"`) {
					hits = append(hits, line)
				}
			}
			return strings.Join(hits, "\n")
		}, time.Minute, 2*time.Second).ShouldNot(BeEmpty())
	})

	// ---- questions and model delegation ----

	askScript := func(extra map[string]any) string {
		s := map[string]any{
			"ask": map[string]any{"questions": []map[string]any{{
				"id":       "db",
				"question": "Which-database-should-the-service-use?",
				"options":  []map[string]any{{"label": "Postgres", "description": "relational"}, {"label": "SQLite"}},
			}}},
			"reply": "asked",
		}
		maps.Copy(s, extra)
		return script(s)
	}
	startedThread := func(since int) string {
		GinkgoHelper()
		started := local.waitNote(since, " started on e2e/"+cloudAgent, false, 3*time.Minute)
		m := regexp.MustCompile(`^☁ (\S+) started on`).FindStringSubmatch(started)
		Expect(m).NotTo(BeNil(), started)
		return m[1]
	}
	var modelCallsBeforeQuestions int

	It("round-trips a question from the cloud agent: Waiting, a card, /cloud answer, Idle", func() {
		modelCallsBeforeQuestions = fp.Count()
		since := local.cloud(askScript(nil))
		asked := startedThread(since)

		By("the thread goes Waiting with the question on the API and on the OmpThread status")
		detail := api.waitPhase(asked, "Waiting", 4*time.Minute)
		Expect(detail.Question).NotTo(BeNil())
		Expect(detail.Question.Text).To(Equal("Which-database-should-the-service-use?"))
		Expect(detail.Question.Turn).To(Equal(1))
		Expect(detail.Question.Remaining).To(Equal(1))
		Expect(detail.Question.Options).To(HaveLen(2))
		Expect(detail.Question.Options[0].Label).To(Equal("Postgres"))
		Expect(detail.Question.Options[0].Description).To(Equal("relational"))
		Eventually(func() string { return str(get("ompthreads", asked), "status", "phase") }, time.Minute, time.Second).
			Should(Equal("Waiting"))
		Expect(str(get("ompthreads", asked), "status", "question", "text")).To(Equal(detail.Question.Text))

		By("whoami counts the waiting thread and the running sitting")
		var who whoAmI
		api.get("/api/v1/whoami", &who)
		agent := who.agent(cloudAgent)
		Expect(agent).NotTo(BeNil())
		Expect(agent.Capacity.MaxConcurrentSessions).To(Equal(cloudMaxSessions))
		Expect(agent.Capacity.Running).To(And(BeNumerically(">=", 1), BeNumerically("<=", cloudMaxSessions)))
		Expect(agent.Threads.Waiting).To(BeNumerically(">=", 1))

		By("the local omp is told: a notification and a question card")
		local.waitNote(since, "☁ "+asked+" asks: Which-database-should-the-service-use?", false, time.Minute)
		qcard := local.waitCard("question", asked, time.Minute)
		Expect(qcard.Origin).To(Equal("user"))
		Expect(qcard.Content).To(And(HavePrefix("[cloud]"), ContainSubstring("Postgres")))

		By("/cloud answer picks the option")
		answerSince := local.cloud("answer " + asked + " Postgres")
		local.waitNote(answerSince, "answered", false, time.Minute)

		By("the turn finishes with the answer in fakeomp's reply and the thread returns to Idle")
		done := api.waitTurn(asked, 1, "done", 3*time.Minute)
		Expect(done.Reply).To(ContainSubstring("answers: db=Postgres"))
		Expect(done.Questions).To(HaveLen(1))
		Expect(done.Questions[0].Selected).To(Equal([]string{"Postgres"}))
		Expect(done.Questions[0].AnsweredBy).To(Equal("token:e2e"))
		Expect(done.Questions[0].Expired).To(BeFalse())
		api.waitPhase(asked, "Idle", time.Minute)
		Expect(api.thread(asked).Question).To(BeNil())
		Eventually(func() string { return str(get("ompthreads", asked), "status", "phase") }, time.Minute, time.Second).
			Should(Equal("Idle"))
		Eventually(func() []string {
			events := api.events(asked)
			kinds := make([]string, 0, len(events))
			for _, e := range events {
				kinds = append(kinds, e.Kind)
			}
			return kinds
		}, time.Minute, 2*time.Second).Should(ContainElements("question", "answer"))
		Expect(api.thread(asked).turn(1).Questions[0].AnsweredAt).NotTo(BeEmpty())
	})

	It("tells the agent to continue when a question goes unanswered for the question timeout", func() {
		since := local.cloud(askScript(nil))
		lapsed := startedThread(since)
		api.waitPhase(lapsed, "Waiting", 4*time.Minute)

		// Nobody answers. The agent's questionTimeout is one minute.
		done := api.waitTurn(lapsed, 1, "done", 5*time.Minute)
		Expect(done.Reply).To(And(ContainSubstring("answers: db="),
			ContainSubstring("No answer within 1m"), ContainSubstring("Continue with your best judgment")))
		Expect(done.Questions).To(HaveLen(1))
		Expect(done.Questions[0].Expired).To(BeTrue())
		Expect(done.Questions[0].AnsweredBy).To(Equal("timeout"))
		Expect(done.Questions[0].Selected).To(BeEmpty())
		api.waitPhase(lapsed, "Idle", time.Minute)

		// Cards of threads started with /cloud only display: they never woke the model.
		Expect(fp.Count()).To(Equal(modelCallsBeforeQuestions),
			"user-origin cards must not start a model turn; requests: %d", fp.Count())
	})

	It("lets the model delegate with the cloud tool, answer the question and hear back", func() {
		task := askScript(map[string]any{
			"files": map[string]string{"delegated.txt": "by the model\n"}, "reply": "delegated-done",
		})
		args, err := json.Marshal(map[string]any{"op": "delegate", "task": task})
		Expect(err).NotTo(HaveOccurred())
		fp.ArmToolCall("cloud", string(args))

		By("the model calls the cloud tool and the thread is created")
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		before := fp.Count()
		_, err = local.client.PromptAndWait(ctx,
			omprpc.PromptCommand{Message: "please delegate the database work to the cloud"}, 3*time.Minute)
		Expect(err).NotTo(HaveOccurred())
		Expect(fp.Count()).To(Equal(before+2), "the model's tool call, then its answer after the tool result")
		reqs := fp.Requests()
		last := reqs[len(reqs)-1]
		Expect(last.EndsWithToolResult()).To(BeTrue())
		result := last.Messages[len(last.Messages)-1].Text()
		m := regexp.MustCompile(`Started cloud thread (\S+) on e2e/` + cloudAgent).FindStringSubmatch(result)
		Expect(m).NotTo(BeNil(), "the tool result did not name the thread: %q", result)
		delegated := m[1]
		Expect(api.thread(delegated).Owner).To(Equal("token:e2e"))
		Expect(api.thread(delegated).Turns).NotTo(BeEmpty())
		launch := local.waitCard("launch", delegated, time.Minute)
		Expect(launch.Origin).To(Equal("tool"), "the launch card records who started the thread")

		By("the question card wakes the model, which answers through the cloud tool")
		answerArgs, err := json.Marshal(map[string]any{"op": "answer", "thread": delegated, "selected": []string{"Postgres"}})
		Expect(err).NotTo(HaveOccurred())
		fp.ArmToolCall("cloud", string(answerArgs))
		qcard := local.waitCard("question", delegated, 4*time.Minute)
		Expect(qcard.Origin).To(Equal("tool"))
		Eventually(func() bool { return modelSaw(fp, "[cloud] "+delegated+" is waiting for an answer") },
			time.Minute, time.Second).Should(BeTrue(), "the question card did not start a model turn")

		By("the cloud turn completes with the model's answer")
		done := api.waitTurn(delegated, 1, "done", 4*time.Minute)
		Expect(done.Reply).To(ContainSubstring("answers: db=Postgres"))
		Expect(done.Questions).To(HaveLen(1))
		Expect(done.Questions[0].AnsweredBy).To(Equal("token:e2e"))
		Expect(done.Questions[0].Selected).To(Equal([]string{"Postgres"}))

		By("the finished turn arrives as a [cloud] card and starts another model turn")
		tcard := local.waitCard("turn", delegated, time.Minute)
		Expect(tcard.Origin).To(Equal("tool"))
		Expect(tcard.Content).To(HavePrefix("[cloud] " + delegated + " turn 1 done"))
		Eventually(func() bool { return modelSaw(fp, "[cloud] "+delegated+" turn 1 done") },
			time.Minute, time.Second).Should(BeTrue(), "the turn card did not start a model turn")
		api.waitPhase(delegated, "Idle", time.Minute)
	})
})
