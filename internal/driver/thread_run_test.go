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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/TechDufus/omp-operator/internal/agentapi"
)

const hangScript = `FAKEOMP:{"mode":"hang"}`

func runSitting(t *testing.T, env *threadEnv) {
	t.Helper()
	if err := env.d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func (e *threadEnv) onlyComplete(t *testing.T) agentapi.Complete {
	t.Helper()
	e.api.mu.Lock()
	defer e.api.mu.Unlock()
	if len(e.api.completes) != 1 {
		t.Fatalf("complete reports = %+v, want exactly one", e.api.completes)
	}
	return e.api.completes[0]
}

func (e *threadEnv) eventTexts(kind string) []string {
	e.api.mu.Lock()
	defer e.api.mu.Unlock()
	var out []string
	for _, ev := range e.api.events {
		if ev.Kind == kind {
			out = append(out, ev.Text)
		}
	}
	return out
}

func TestThreadSittingServesTurnsThenIdles(t *testing.T) {
	bin, log := wrapOmp(t)
	env := newThreadEnv(t, nil)
	env.d.OmpBin = bin
	env.api.enqueue(`FAKEOMP:{"files":{"a.txt":"1\n"},"reply":"# First reply\nwith details"}`)
	env.api.enqueue(`FAKEOMP:{"files":{"b.txt":"2\n"},"reply":"second"}`)

	runSitting(t, env)

	done := env.onlyComplete(t)
	if done.Outcome != agentapi.OutcomeCompleted || !done.FinishCalled || done.ContinuityRestored || done.Error != "" {
		t.Fatalf("sitting report = %+v", done)
	}
	env.api.mu.Lock()
	first, second := env.api.turnDone[1], env.api.turnDone[2]
	results, transcript, continuity := env.api.results, env.api.transcript, env.api.continuity
	env.api.mu.Unlock()

	if first.Outcome != agentapi.OutcomeCompleted || first.Reply != "# First reply\nwith details" || first.Error != "" {
		t.Fatalf("turn 1 = %+v", first)
	}
	if second.Outcome != agentapi.OutcomeCompleted || second.Reply != "second" {
		t.Fatalf("turn 2 = %+v", second)
	}
	// The diffstat is against the base: the user's two files plus one per turn.
	if first.FilesChanged != 3 || first.Insertions != 3 || second.FilesChanged != 4 || second.Insertions != 4 || second.Deletions != 0 {
		t.Fatalf("diffstats: turn 1 %+v, turn 2 %+v", first, second)
	}
	if first.Head == "" || first.Head == env.f.handoff || second.Head == first.Head {
		t.Fatalf("heads: handoff %s, turn 1 %s, turn 2 %s", env.f.handoff, first.Head, second.Head)
	}
	if got := env.wsGit(t, "rev-parse", "refs/heads/"+threadBranch); got != second.Head {
		t.Fatalf("branch tip = %s, want the last turn's head %s", got, second.Head)
	}
	if got := env.wsGit(t, "log", "--format=%s|%an <%ae>", "-2"); got != "second|Ada <ada@example.com>\nFirst reply|Ada <ada@example.com>" {
		t.Fatalf("turn commits:\n%s", got)
	}
	if string(transcript) != "<html>fake</html>" || len(continuity) == 0 {
		t.Fatalf("transcript = %q, continuity = %d bytes", transcript, len(continuity))
	}

	// Every turn stores the branch; the newest bundle carries the head and can be applied on the user's side.
	if len(results) != 2 {
		t.Fatalf("result uploads = %d, want one per turn", len(results))
	}
	verifyResultBundle(t, env, results[1], second.Head)

	turns := env.eventTexts(eventTurn)
	if len(turns) != 2 || !strings.HasPrefix(turns[0], "turn 1: FAKEOMP:") || !strings.HasPrefix(turns[1], "turn 2: ") {
		t.Fatalf("turn events = %v", turns)
	}
	if assistant := env.eventTexts(eventAssistant); len(assistant) != 2 || assistant[0] != "# First reply\nwith details" {
		t.Fatalf("assistant events = %v", assistant)
	}
	checkOneOmpServedBothTurns(t, env, log)
	if env.api.turnState(1) != "done" || env.api.turnState(2) != "done" {
		t.Fatalf("turn states: %s, %s", env.api.turnState(1), env.api.turnState(2))
	}
}

// verifyResultBundle checks that bundle applies on the user's repository and carries head on the thread branch.
func verifyResultBundle(t *testing.T, env *threadEnv, bundle []byte, head string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "result.bundle")
	if err := os.WriteFile(file, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, env.f.local, "bundle", "verify", file)
	if heads := runGit(t, env.f.local, "bundle", "list-heads", file); !strings.Contains(heads, head+" refs/heads/"+threadBranch) {
		t.Fatalf("bundle heads:\n%s", heads)
	}
}

// checkOneOmpServedBothTurns: one omp opened the conversation once, took two prompts, and got no tool flags.
func checkOneOmpServedBothTurns(t *testing.T, env *threadEnv, log string) {
	t.Helper()
	stdin := readFile(t, log)
	if strings.Count(stdin, `"type":"open_session"`) != 1 || strings.Count(stdin, `"type":"prompt"`) != 2 ||
		strings.Count(stdin, `"type":"set_ask_dialog","enabled":true`) != 1 {
		t.Fatalf("omp commands:\n%s", stdin)
	}
	args := strings.Split(strings.TrimSpace(readFile(t, log+".args")), "\n")
	want := []string{"--mode", "rpc-ui", "--no-ui", "--approval-mode", "yolo", "--max-time", "120s",
		"--session-dir", env.paths.SessionDir, "--append-system-prompt", env.paths.SystemPrompt}
	if !slices.Equal(args, want) {
		t.Fatalf("omp args = %v, want %v", args, want)
	}
}

func TestThreadSittingStopAbortsTheRunningTurn(t *testing.T) {
	env := newThreadEnv(t, nil)
	hung := env.api.enqueue(hangScript)
	hung.StopAfter = 300 * time.Millisecond
	env.api.enqueue(`FAKEOMP:{"reply":"after the stop"}`)

	runSitting(t, env)

	env.api.mu.Lock()
	defer env.api.mu.Unlock()
	if got := env.api.turnDone[1]; got.Outcome != agentapi.OutcomeError || got.Error != "prompt aborted" || got.Reply != "" {
		t.Fatalf("stopped turn = %+v", got)
	}
	if got := env.api.turnDone[2]; got.Outcome != agentapi.OutcomeCompleted || got.Reply != "after the stop" {
		t.Fatalf("the sitting must go on after a stop, turn 2 = %+v", got)
	}
	if len(env.api.completes) != 1 || env.api.completes[0].Outcome != agentapi.OutcomeCompleted {
		t.Fatalf("complete = %+v", env.api.completes)
	}
}

func TestThreadSittingRetryableExitLeavesTheTurnRunning(t *testing.T) {
	env := newThreadEnv(t, nil)
	env.api.enqueue(`FAKEOMP:{"mode":"exit-early"}`)
	env.api.enqueue(`FAKEOMP:{"reply":"never served"}`)

	runSitting(t, env)

	done := env.onlyComplete(t)
	if done.Outcome != agentapi.OutcomeRetryable || !strings.Contains(done.Error, "exited before prompt_result") {
		t.Fatalf("sitting report = %+v", done)
	}
	if env.api.turnState(1) != "running" || env.api.turnState(2) != "queued" {
		t.Fatalf("turn states = %s, %s; the failed turn must stay running for the next sitting",
			env.api.turnState(1), env.api.turnState(2))
	}
	env.api.mu.Lock()
	defer env.api.mu.Unlock()
	if len(env.api.turnDone) != 0 {
		t.Fatalf("no turn may be completed: %+v", env.api.turnDone)
	}
	// What the sitting had is saved for the next one.
	if len(env.api.results) != 1 || len(env.api.continuity) == 0 {
		t.Fatalf("results = %d, continuity = %d bytes", len(env.api.results), len(env.api.continuity))
	}
}

func TestThreadSittingInterruptSavesWorkAndKeepsTheTurn(t *testing.T) {
	env := newThreadEnv(t, nil)
	env.api.enqueue(hangScript)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for env.api.turnState(1) != "running" {
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(300 * time.Millisecond)
		cancel() // SIGTERM
	}()
	// The agent had done something before the pod was told to stop.
	writeFile(t, filepath.Join(env.paths.Workspace, "unsaved.txt"), "work in progress\n")

	if err := env.d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	done := env.onlyComplete(t)
	if done.Outcome != agentapi.OutcomeRetryable || done.Error != errInterrupted {
		t.Fatalf("sitting report = %+v", done)
	}
	if env.api.turnState(1) != "running" {
		t.Fatalf("turn state = %s, want running", env.api.turnState(1))
	}
	if got := env.wsGit(t, "show", "--name-only", "--format=%s", "HEAD"); !strings.Contains(got, "omp-cloud: turn 1") || !strings.Contains(got, "unsaved.txt") {
		t.Fatalf("the interrupted work was not committed:\n%s", got)
	}
	env.api.mu.Lock()
	defer env.api.mu.Unlock()
	if len(env.api.results) != 1 || len(env.api.continuity) == 0 || len(env.api.turnDone) != 0 {
		t.Fatalf("results = %d, continuity = %d bytes, completed turns = %v",
			len(env.api.results), len(env.api.continuity), env.api.turnDone)
	}
}

func TestThreadSittingStopsTakingTurnsNearItsLifetime(t *testing.T) {
	env := newThreadEnv(t, func(cfg *agentapi.SessionConfig) {
		cfg.Thread.MaxSittingSeconds = 2 // less than one turn timeout: no second turn fits
	})
	env.api.enqueue(`FAKEOMP:{"reply":"one"}`)
	env.api.enqueue(`FAKEOMP:{"reply":"two"}`)

	runSitting(t, env)

	if done := env.onlyComplete(t); done.Outcome != agentapi.OutcomeCompleted {
		t.Fatalf("sitting report = %+v", done)
	}
	// The first turn is always taken; the second waits for the next sitting.
	if env.api.turnState(1) != "done" || env.api.turnState(2) != "queued" {
		t.Fatalf("turn states = %s, %s", env.api.turnState(1), env.api.turnState(2))
	}
}

func TestThreadSittingScrubsSecretsFromWhatItSends(t *testing.T) {
	const secret = "hunter2-hunter2-value"
	t.Setenv("FAKEOMP_ECHO_ENV", "LEAKY")
	t.Setenv("LEAKY", secret)
	env := newThreadEnv(t, func(cfg *agentapi.SessionConfig) {
		cfg.Redact = []agentapi.RedactSource{{Name: "LEAKY", Env: "LEAKY"}}
	})
	env.api.enqueue(`FAKEOMP:{"reply":"# token ` + secret + `"}`)

	runSitting(t, env)

	env.api.mu.Lock()
	defer env.api.mu.Unlock()
	done := env.api.turnDone[1]
	if strings.Contains(done.Reply, secret) || !strings.Contains(done.Reply, "[redacted:LEAKY]") {
		t.Fatalf("reply = %q", done.Reply)
	}
	for _, e := range env.api.events {
		if strings.Contains(e.Text, secret) {
			t.Fatalf("event %s leaks the secret: %q", e.Kind, e.Text)
		}
	}
	// The commit message is derived from the reply; it is pushed to a remote, so it is scrubbed too.
	if got := runGit(t, env.paths.Workspace, "log", "-1", "--format=%s"); strings.Contains(got, secret) {
		t.Fatalf("commit message leaks the secret: %q", got)
	}
}

func TestMergeThreadSettings(t *testing.T) {
	gateway := &agentapi.GatewayConfig{URL: "http://gw", Providers: []string{"anthropic"}}
	cfg := func(thinking string, gw *agentapi.GatewayConfig) agentapi.SessionConfig {
		return agentapi.SessionConfig{
			Model:  agentapi.ModelConfig{Default: "anthropic/claude-sonnet-5-5", Thinking: "medium", Gateway: gw},
			Thread: &agentapi.ThreadConfig{Thinking: thinking},
		}
	}
	tests := []struct {
		name     string
		shipped  string
		cfg      agentapi.SessionConfig
		wantRole map[string]string
		wantKeys map[string]any
	}{
		{
			name: "roles of providers the gateway does not serve are remapped",
			shipped: `modelRoles:
  default: openai-codex/gpt-6
  smol: anthropic/claude-haiku-5
  slow: ollama/llama
  plan: sonnet
theme: dark
`,
			cfg: cfg("high", gateway),
			wantRole: map[string]string{
				"default": "anthropic/claude-sonnet-5-5", "smol": "anthropic/claude-haiku-5",
				"slow": "anthropic/claude-sonnet-5-5", "plan": "anthropic/claude-sonnet-5-5",
			},
			wantKeys: map[string]any{"theme": "dark", "defaultThinkingLevel": "high"},
		},
		{
			name:     "shipped thinking level stays when the thread names none",
			shipped:  "defaultThinkingLevel: low\n",
			cfg:      cfg("", gateway),
			wantRole: map[string]string{"default": "anthropic/claude-sonnet-5-5"},
			wantKeys: map[string]any{"defaultThinkingLevel": "low"},
		},
		{
			name:     "no gateway: every role is the thread's model",
			shipped:  "modelRoles:\n  smol: anthropic/claude-haiku-5\n",
			cfg:      cfg("", nil),
			wantRole: map[string]string{"default": "anthropic/claude-sonnet-5-5", "smol": "anthropic/claude-sonnet-5-5"},
			wantKeys: map[string]any{"defaultThinkingLevel": "medium"},
		},
		{
			name:     "no shipped settings: a plain session's roles",
			shipped:  "",
			cfg:      cfg("", gateway),
			wantRole: map[string]string{"default": "anthropic/claude-sonnet-5-5", "task": "anthropic/claude-sonnet-5-5", "tiny": "anthropic/claude-sonnet-5-5"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			paths := testPaths(t)
			d := New(NewClient("http://unused", "", Target{}), paths, discardLogger())
			if tt.shipped != "" {
				writeFile(t, filepath.Join(paths.OmpAgentDir, "config.yml"), tt.shipped)
			}
			if err := d.writeThreadOmpConfig(tt.cfg); err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(paths.OmpAgentDir, "config.yml"))), &got); err != nil {
				t.Fatal(err)
			}
			roles, _ := got["modelRoles"].(map[string]any)
			for role, want := range tt.wantRole {
				if roles[role] != want {
					t.Errorf("modelRoles.%s = %v, want %s (all: %v)", role, roles[role], want, roles)
				}
			}
			for key, want := range tt.wantKeys {
				if got[key] != want {
					t.Errorf("%s = %v, want %v", key, got[key], want)
				}
			}
			// Whatever the user shipped, the pod needs no approvals, desktop, collab room or plugin updates.
			for group, want := range map[string]map[string]any{
				"computer": {"enabled": false}, "collab": {"autoStart": "off"},
				"marketplace": {"autoUpdate": "off"}, "tools": {"approvalMode": "yolo"},
			} {
				m, _ := got[group].(map[string]any)
				for k, v := range want {
					if m[k] != v {
						t.Errorf("%s.%s = %v, want %v", group, k, m[k], v)
					}
				}
			}
		})
	}
}

func TestWriteThreadOmpConfigKeepsOtherFiles(t *testing.T) {
	paths := testPaths(t)
	d := New(NewClient("http://unused", "", Target{}), paths, discardLogger())
	writeFile(t, filepath.Join(paths.OmpAgentDir, "config.yml"), "{not yaml: [")
	cfg := agentapi.SessionConfig{
		SystemPrompt: "cloud prompt",
		MCP:          json.RawMessage(`{"mcpServers":{}}`),
		Model: agentapi.ModelConfig{
			Default: "anthropic/claude-sonnet-5-5",
			Gateway: &agentapi.GatewayConfig{URL: "http://gw", Providers: []string{"anthropic", "openai-codex"}},
		},
		Thread: &agentapi.ThreadConfig{},
	}
	if err := d.writeThreadOmpConfig(cfg); err != nil {
		t.Fatalf("an unreadable shipped config must not fail the sitting: %v", err)
	}
	if got := readFile(t, paths.SystemPrompt); got != "cloud prompt" {
		t.Errorf("system prompt = %q", got)
	}
	if got := readFile(t, filepath.Join(paths.OmpAgentDir, "mcp.json")); got != `{"mcpServers":{}}` {
		t.Errorf("mcp.json = %q", got)
	}
	models := readFile(t, filepath.Join(paths.OmpAgentDir, "models.yml"))
	if !strings.Contains(models, "anthropic:") || !strings.Contains(models, "openai-codex:") {
		t.Errorf("models.yml must route every gateway provider:\n%s", models)
	}
}

func TestThreadOmpArgs(t *testing.T) {
	paths := testPaths(t)
	tests := []struct {
		name   string
		tools  []string
		prompt string
		want   []string
	}{
		{"every built-in tool", nil, "p", []string{"--mode", "rpc-ui", "--no-ui", "--approval-mode", "yolo",
			"--max-time", "600s", "--session-dir", paths.SessionDir, "--append-system-prompt", paths.SystemPrompt}},
		{"listed tools", []string{"read", "bash"}, "", []string{"--mode", "rpc-ui", "--no-ui", "--approval-mode", "yolo",
			"--tools", "read,bash", "--max-time", "600s", "--session-dir", paths.SessionDir}},
		{"no tools", []string{}, "", []string{"--mode", "rpc-ui", "--no-ui", "--approval-mode", "yolo",
			"--no-tools", "--max-time", "600s", "--session-dir", paths.SessionDir}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := agentapi.SessionConfig{Tools: tt.tools, SystemPrompt: tt.prompt, Thread: &agentapi.ThreadConfig{MaxSittingSeconds: 600}}
			if got := threadOmpArgs(cfg, paths); !slices.Equal(got, tt.want) {
				t.Fatalf("args = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSubUsage(t *testing.T) {
	after := agentapi.Usage{InputTokens: 1500, OutputTokens: 300, CacheReadTokens: 50, CacheWriteTokens: 10, Cost: "0.0375"}
	before := agentapi.Usage{InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 50, Cost: "0.0125"}
	if got, want := subUsage(after, before), (agentapi.Usage{InputTokens: 500, OutputTokens: 100, CacheWriteTokens: 10, Cost: "0.025"}); got != want {
		t.Fatalf("subUsage = %+v, want %+v", got, want)
	}
	// A failed statistics call reads as zero; the difference never goes negative.
	if got := subUsage(agentapi.Usage{}, before); got != (agentapi.Usage{Cost: "0"}) {
		t.Fatalf("subUsage(zero, before) = %+v", got)
	}
}

// finalizeEnv is a prepared thread with an event sink to read back, for calling finalizeThread directly.
func finalizeEnv(t *testing.T) (*threadEnv, *threadRun) {
	t.Helper()
	env := newThreadEnv(t, nil)
	sink := newEventSink(env.d.API, &Redactor{}, time.Hour, discardLogger())
	return env, &threadRun{cfg: *env.api.cfg.Thread, red: &Redactor{}, events: sink}
}

func sinkTexts(tr *threadRun) []string {
	tr.events.mu.Lock()
	defer tr.events.mu.Unlock()
	out := make([]string, 0, len(tr.events.buf))
	for _, e := range tr.events.buf {
		out = append(out, e.Kind+": "+e.Text)
	}
	return out
}

func TestFinalizeThreadCommitsBundlesAndMeasures(t *testing.T) {
	env, tr := finalizeEnv(t)
	ctx := context.Background()
	ws := env.paths.Workspace

	// Uncommitted work becomes a commit titled by the reply, and the branch follows it.
	writeFile(t, filepath.Join(ws, "new.txt"), "a\nb\n")
	first := env.d.finalizeThread(ctx, tr, 1, "## Added new.txt\nand explained")
	if got := env.wsGit(t, "log", "-1", "--format=%s"); got != "Added new.txt" {
		t.Fatalf("commit message = %q", got)
	}
	if first.Head != env.wsGit(t, "rev-parse", "refs/heads/"+threadBranch) || first.Head == env.f.handoff {
		t.Fatalf("head = %s", first.Head)
	}
	if first.Files != 3 || first.Insertions != 4 || first.Deletions != 0 || first.Pushed || first.PushError != "" {
		t.Fatalf("finalized = %+v", first)
	}
	if env.api.resultCount() != 1 {
		t.Fatalf("result uploads = %d", env.api.resultCount())
	}

	// Nothing changed: no commit, no second upload, the same head.
	again := env.d.finalizeThread(ctx, tr, 2, "reply")
	if again.Head != first.Head || env.api.resultCount() != 1 {
		t.Fatalf("an unchanged branch must not be committed or uploaded again: %+v, uploads %d", again, env.api.resultCount())
	}

	// An empty reply falls back to the turn number.
	writeFile(t, filepath.Join(ws, "more.txt"), "x\n")
	env.d.finalizeThread(ctx, tr, 3, "")
	if got := env.wsGit(t, "log", "-1", "--format=%s"); got != "omp-cloud: turn 3" {
		t.Fatalf("commit message = %q", got)
	}
	if env.api.resultCount() != 2 {
		t.Fatalf("result uploads = %d, want 2", env.api.resultCount())
	}
}

func TestFinalizeThreadKeepsBranchWhenHeadWanders(t *testing.T) {
	env, tr := finalizeEnv(t)
	ws := env.paths.Workspace
	want := env.wsGit(t, "rev-parse", "refs/heads/"+threadBranch)

	// The agent switched to an unrelated history and committed there.
	env.wsGit(t, "checkout", "-q", "--orphan", "elsewhere")
	writeFile(t, filepath.Join(ws, "orphan.txt"), "x\n")
	env.wsGit(t, "add", "orphan.txt")
	got := env.d.finalizeThread(context.Background(), tr, 1, "reply")

	if got.Head != want {
		t.Fatalf("head = %s, want the untouched branch tip %s", got.Head, want)
	}
	events := strings.Join(sinkTexts(tr), "\n")
	if !strings.Contains(events, "HEAD is not on "+threadBranch+"; the result stays at "+threadBranch) {
		t.Fatalf("events:\n%s", events)
	}
}

func TestFinalizeThreadPushesThroughTheSidecar(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantPushed bool
		wantErr    string
	}{
		{"pushed", http.StatusOK, `{"pushed":true}`, true, ""},
		{"push failed", http.StatusBadGateway, `{"error":"remote rejected"}`, false, "remote rejected"},
		{"sidecar without a message", http.StatusInternalServerError, `oops`, false, "HTTP 500"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requests := make(chan string, 4)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]string
				_ = json.NewDecoder(r.Body).Decode(&body)
				requests <- r.Method + " " + r.URL.Path + " " + body["head"]
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			env, tr := finalizeEnv(t)
			env.d.PushAddr = strings.TrimPrefix(srv.URL, "http://")

			got := env.d.finalizeThread(context.Background(), tr, 1, "reply")

			if got.Pushed != tt.wantPushed || !strings.Contains(got.PushError, tt.wantErr) || (tt.wantErr == "") != (got.PushError == "") {
				t.Fatalf("pushed = %v, error = %q", got.Pushed, got.PushError)
			}
			if want := "POST /push " + got.Head; len(requests) != 1 || <-requests != want {
				t.Fatalf("push requests, want exactly [%s]", want)
			}
		})
	}
}

func TestFinalizeThreadRedactsTheCommitMessage(t *testing.T) {
	env, tr := finalizeEnv(t)
	tr.red = NewRedactor([]agentapi.RedactSource{{Name: "KEY", Env: "KEY"}},
		func(string) string { return "super-secret-value" }, nil)
	writeFile(t, filepath.Join(env.paths.Workspace, "f.txt"), "x\n")

	env.d.finalizeThread(context.Background(), tr, 1, "used super-secret-value to log in")

	if got := env.wsGit(t, "log", "-1", "--format=%s"); got != "used [redacted:KEY] to log in" {
		t.Fatalf("commit message = %q", got)
	}
}

func TestParseShortstat(t *testing.T) {
	tests := map[string][3]int{
		"":                                {0, 0, 0},
		" 1 file changed, 1 insertion(+)": {1, 1, 0},
		" 3 files changed, 12 insertions(+), 4 deletions(-)":         {3, 12, 4},
		" 2 files changed, 7 deletions(-)":                           {2, 0, 7},
		" 1 file changed, 1 insertion(+), 1 deletion(-)":             {1, 1, 1},
		"warning: something else entirely":                           {0, 0, 0},
		" 10 files changed, 100 insertions(+), 200 deletions(-)\n  ": {10, 100, 200},
	}
	for in, want := range tests {
		files, ins, del := parseShortstat(in)
		if got := [3]int{files, ins, del}; got != want {
			t.Errorf("parseShortstat(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSanitizeRemote(t *testing.T) {
	tests := map[string]string{
		"https://user:tok@github.com/o/r.git":    "https://github.com/o/r.git",
		"https://ghp_token@github.com/o/r.git":   "https://github.com/o/r.git",
		"http://host:8080/app.git":               "http://host:8080/app.git",
		"ssh://git:pw@host:2222/o/r.git":         "ssh://git@host:2222/o/r.git",
		"git@github.com:o/r.git":                 "git@github.com:o/r.git",
		"/srv/git/repo.git":                      "/srv/git/repo.git",
		"ssh://git@github.com/o/r.git":           "ssh://git@github.com/o/r.git",
		"https://github.com/o/r.git":             "https://github.com/o/r.git",
		"file:///srv/git/repo.git":               "file:///srv/git/repo.git",
		"https://github.com/o/r.git?x=1#section": "https://github.com/o/r.git?x=1#section",
	}
	for in, want := range tests {
		if got := sanitizeRemote(in); got != want {
			t.Errorf("sanitizeRemote(%q) = %q, want %q", in, got, want)
		}
	}
}
