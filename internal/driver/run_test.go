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
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	omprpc "github.com/can1357/oh-my-pi/sdk/go/omp-rpc"
	"sigs.k8s.io/yaml"

	"github.com/TechDufus/omp-operator/internal/agentapi"
)

// wrapOmp installs a shell wrapper around fakeomp that records the arguments and every command
// the driver writes to omp's stdin. It returns the wrapper and the stdin log.
func wrapOmp(t *testing.T) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin, log = filepath.Join(dir, "omp"), filepath.Join(dir, "stdin.log")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$WRAP_LOG.args"
fifo="$WRAP_LOG.fifo"
rm -f "$fifo"
mkfifo "$fifo"
exec 3<&0
tee -a "$WRAP_LOG" "$fifo" >/dev/null <&3 3<&- &
exec "$WRAP_BIN" "$@" <"$fifo" 3<&-
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WRAP_LOG", log)
	t.Setenv("WRAP_BIN", fakeBinary)
	return bin, log
}

func baseConfig() agentapi.SessionConfig {
	object := json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string"}}}`)
	return agentapi.SessionConfig{
		Prompt:         "triage the alert",
		SystemPrompt:   "You are an agent.",
		Model:          agentapi.ModelConfig{Default: fakeModel, Thinking: "medium"},
		Tools:          []string{"read", "bash"},
		TimeoutSeconds: 30,
		Actions: []agentapi.ToolDef{
			{Name: "echo", Description: "echo it", Parameters: object},
			{Name: finishTool, Description: "finish it", Parameters: json.RawMessage(`{"type":"object"}`)},
		},
	}
}

type runResult struct {
	api      *fakeAPI
	paths    Paths
	stdinLog string
	args     []string
	done     agentapi.Complete
}

// runFake runs the driver's run command against fakeomp with the given FAKEOMP_SCRIPT.
func runFake(t *testing.T, script string, mutate func(*agentapi.SessionConfig, *fakeAPI, *Driver, Paths)) runResult {
	t.Helper()
	bin, log := wrapOmp(t)
	t.Setenv("FAKEOMP_SCRIPT", script)
	api := newFakeAPI(t, KindSessions)
	api.cfg = baseConfig()
	paths := testPaths(t)
	d := api.driver(paths)
	d.OmpBin = bin
	if mutate != nil {
		mutate(&api.cfg, api, d, paths)
	}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	unlock := api.lock()
	defer unlock()
	if len(api.completes) != 1 {
		t.Fatalf("complete reports = %v", api.completes)
	}
	res := runResult{api: api, paths: paths, stdinLog: readFile(t, log), done: api.completes[0]}
	res.args = strings.Split(strings.TrimSpace(readFile(t, log+".args")), "\n")
	return res
}

func (r runResult) actionBodies(tool string) []string {
	unlock := r.api.lock()
	defer unlock()
	var out []string
	for _, a := range r.api.actions {
		if a.Tool == tool {
			out = append(out, a.Body)
		}
	}
	return out
}

func (r runResult) eventKinds() map[string][]string {
	unlock := r.api.lock()
	defer unlock()
	out := map[string][]string{}
	for _, e := range r.api.events {
		out[e.Kind] = append(out[e.Kind], e.Text)
	}
	return out
}

func (r runResult) commands(typ string) int {
	return strings.Count(r.stdinLog, `"type":"`+typ+`"`)
}

func TestRunNormalWithCall(t *testing.T) {
	res := runFake(t, `{"calls":[{"tool":"echo","args":{"msg":"hi"}}]}`, nil)

	if res.done.Outcome != agentapi.OutcomeCompleted || !res.done.FinishCalled || res.done.Error != "" {
		t.Fatalf("complete = %+v", res.done)
	}
	if got := res.actionBodies("echo"); len(got) != 1 || got[0] != `{"msg":"hi"}` {
		t.Fatalf("echo calls = %v", got)
	}
	finish := res.actionBodies(finishTool)
	if len(finish) != 1 || !strings.Contains(finish[0], `"verdict":"ok"`) {
		t.Fatalf("finish calls = %v", finish)
	}
	if want := (agentapi.Usage{InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 300, CacheWriteTokens: 40, Cost: "0.0125"}); res.done.Usage != want {
		t.Fatalf("usage = %+v, want %+v", res.done.Usage, want)
	}
	unlock := res.api.lock()
	if string(res.api.transcript) != "<html>fake</html>" {
		t.Fatalf("transcript = %q", res.api.transcript)
	}
	if res.api.continuity != nil {
		t.Fatal("continuity must not be uploaded without a continuityKey")
	}
	unlock()
	if tools := res.eventKinds()[eventTool]; len(tools) < 2 || !strings.HasPrefix(tools[0], `echo {"msg":"hi"`) {
		t.Fatalf("tool events = %v", tools)
	}
	if notices := res.eventKinds()[eventNotice]; len(notices) != 1 || notices[0] != "fakeomp working" {
		t.Fatalf("notice events = %v", notices)
	}

	// The command line is the plan's, and the host tools are essential (top-level) tools.
	wantArgs := []string{"--mode", "rpc", "--no-ui", "--approval-mode", "yolo", "--tools", "read,bash",
		"--max-time", "30s", "--session-dir", res.paths.SessionDir, "--append-system-prompt", res.paths.SystemPrompt}
	if strings.Join(res.args, " ") != strings.Join(wantArgs, " ") {
		t.Fatalf("omp args = %v, want %v", res.args, wantArgs)
	}
	if res.commands("set_host_tools") != 1 || strings.Count(res.stdinLog, `"loadMode":"essential"`) != 2 {
		t.Fatalf("host tools must be registered once, both essential:\n%s", res.stdinLog)
	}
	if res.commands("open_session") != 0 || res.commands("abort") != 0 || res.commands("prompt") != 1 {
		t.Fatalf("unexpected commands:\n%s", res.stdinLog)
	}
	if got := readFile(t, res.paths.SystemPrompt); got != "You are an agent." {
		t.Fatalf("system prompt = %q", got)
	}
}

func TestRunWritesOmpConfig(t *testing.T) {
	res := runFake(t, "", func(cfg *agentapi.SessionConfig, _ *fakeAPI, _ *Driver, _ Paths) {
		cfg.Model = agentapi.ModelConfig{
			Default: "anthropic/claude-sonnet-5-5", Small: "anthropic/claude-haiku-5", Thinking: "high",
			Gateway: &agentapi.GatewayConfig{URL: "http://omp-auth.omp-system.svc:4000", Providers: []string{"anthropic"}},
		}
		cfg.MCP = json.RawMessage(`{"mcpServers":{"vm":{"type":"http","url":"http://vm","headers":{"X":"${OMP_MCP_0_0}"}}}}`)
	})
	var settings struct {
		ModelRoles           map[string]string `json:"modelRoles"`
		DefaultThinkingLevel string            `json:"defaultThinkingLevel"`
		Tools                struct {
			ApprovalMode string `json:"approvalMode"`
		} `json:"tools"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(res.paths.OmpAgentDir, "config.yml"))), &settings); err != nil {
		t.Fatal(err)
	}
	roles := settings.ModelRoles
	if roles[roleDefault] != "anthropic/claude-sonnet-5-5" || roles["task"] != roles[roleDefault] ||
		roles["smol"] != "anthropic/claude-haiku-5" || roles["tiny"] != roles["smol"] ||
		settings.DefaultThinkingLevel != "high" || settings.Tools.ApprovalMode != "yolo" {
		t.Fatalf("config.yml = %+v", settings)
	}
	var models struct {
		Providers map[string]struct {
			BaseURL   string `json:"baseUrl"`
			APIKey    string `json:"apiKey"`
			Transport string `json:"transport"`
		} `json:"providers"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(res.paths.OmpAgentDir, "models.yml"))), &models); err != nil {
		t.Fatal(err)
	}
	p := models.Providers["anthropic"]
	if len(models.Providers) != 1 || p.BaseURL != "http://omp-auth.omp-system.svc:4000" || p.APIKey != "OMP_GATEWAY_TOKEN" || p.Transport != "pi-native" {
		t.Fatalf("models.yml = %+v", models)
	}
	if got := readFile(t, filepath.Join(res.paths.OmpAgentDir, "mcp.json")); !strings.Contains(got, "${OMP_MCP_0_0}") {
		t.Fatalf("mcp.json = %q", got)
	}
}

func TestRunWithoutGatewayWritesNoModelsFile(t *testing.T) {
	res := runFake(t, "", nil)
	if _, err := os.Stat(filepath.Join(res.paths.OmpAgentDir, "models.yml")); err == nil {
		t.Fatal("models.yml must only exist with a gateway")
	}
	if _, err := os.Stat(filepath.Join(res.paths.OmpAgentDir, "mcp.json")); err == nil {
		t.Fatal("mcp.json must only exist when the session has MCP servers")
	}
	var settings struct {
		ModelRoles map[string]string `json:"modelRoles"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(res.paths.OmpAgentDir, "config.yml"))), &settings); err != nil {
		t.Fatal(err)
	}
	if settings.ModelRoles["smol"] != fakeModel || settings.ModelRoles["tiny"] != fakeModel {
		t.Fatalf("smol/tiny must fall back to the default model: %v", settings.ModelRoles)
	}
}

func TestRunExitEarlyIsRetryable(t *testing.T) {
	res := runFake(t, `{"mode":"exit-early"}`, nil)
	if res.done.Outcome != agentapi.OutcomeRetryable || res.done.FinishCalled {
		t.Fatalf("complete = %+v", res.done)
	}
	if !strings.Contains(res.done.Error, "exited before prompt_result") {
		t.Fatalf("error = %q", res.done.Error)
	}
	unlock := res.api.lock()
	defer unlock()
	if res.api.transcript != nil {
		t.Fatal("no transcript can be exported from a dead omp")
	}
}

func TestRunNoFinishNudgesOnceThenCompletes(t *testing.T) {
	res := runFake(t, `{"mode":"no-finish","calls":[{"tool":"echo","args":{"msg":"x"}}]}`, nil)
	if res.done.Outcome != agentapi.OutcomeCompleted || res.done.FinishCalled {
		t.Fatalf("complete = %+v", res.done)
	}
	if res.commands("prompt") != 2 {
		t.Fatalf("want the prompt and exactly one nudge, got %d prompts:\n%s", res.commands("prompt"), res.stdinLog)
	}
	if !strings.Contains(res.stdinLog, "You ended without calling finish. Call finish now with your report.") {
		t.Fatalf("nudge text missing:\n%s", res.stdinLog)
	}
	if nudges := res.eventKinds()[eventNudge]; len(nudges) != 1 {
		t.Fatalf("nudge events = %v", nudges)
	}
	if len(res.actionBodies(finishTool)) != 0 {
		t.Fatal("finish must not have been called")
	}
}

func TestRunNoNudgeAfterFinish(t *testing.T) {
	res := runFake(t, "", nil)
	if res.commands("prompt") != 1 || len(res.eventKinds()[eventNudge]) != 0 {
		t.Fatalf("finish was called; no nudge expected:\n%s", res.stdinLog)
	}
}

func TestRunFinishRejectedByOperatorIsNotRecorded(t *testing.T) {
	res := runFake(t, "", func(_ *agentapi.SessionConfig, api *fakeAPI, _ *Driver, _ Paths) {
		api.action = func(tool string, _ []byte) (int, agentapi.ActionResult) {
			if tool == finishTool {
				return http.StatusOK, agentapi.ActionResult{Text: "title too long", IsError: true}
			}
			return http.StatusOK, agentapi.ActionResult{Text: "ok"}
		}
	})
	if res.done.FinishCalled {
		t.Fatal("a rejected finish is not a finish")
	}
	if res.commands("prompt") != 2 {
		t.Fatalf("a rejected finish must trigger the nudge:\n%s", res.stdinLog)
	}
}

func TestRunActionHTTPFailureBecomesToolError(t *testing.T) {
	res := runFake(t, `{"calls":[{"tool":"echo","args":{}}]}`, func(_ *agentapi.SessionConfig, api *fakeAPI, _ *Driver, _ Paths) {
		api.action = func(tool string, _ []byte) (int, agentapi.ActionResult) {
			if tool == "echo" {
				return http.StatusInternalServerError, agentapi.ActionResult{}
			}
			return http.StatusOK, agentapi.ActionResult{Text: "ok"}
		}
	})
	if res.done.Outcome != agentapi.OutcomeCompleted || !res.done.FinishCalled {
		t.Fatalf("an action transport failure must not fail the session: %+v", res.done)
	}
	if len(res.actionBodies("echo")) != 1 {
		t.Fatalf("actions must never be retried: %v", res.actionBodies("echo"))
	}
}

func TestRunGarbageLineIsSkipped(t *testing.T) {
	res := runFake(t, `{"mode":"garbage"}`, nil)
	if res.done.Outcome != agentapi.OutcomeCompleted || !res.done.FinishCalled {
		t.Fatalf("complete = %+v", res.done)
	}
}

func TestRunHangAbortsAndReportsTimeout(t *testing.T) {
	start := time.Now()
	res := runFake(t, `{"mode":"hang"}`, func(cfg *agentapi.SessionConfig, _ *fakeAPI, d *Driver, _ Paths) {
		cfg.TimeoutSeconds = 2
		d.PromptGrace = 100 * time.Millisecond
	})
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("took %v", elapsed)
	}
	if res.done.Outcome != agentapi.OutcomeError || res.done.Error != "session timed out" || res.done.FinishCalled {
		t.Fatalf("complete = %+v", res.done)
	}
	if res.commands("abort") != 1 {
		t.Fatalf("abort must be sent exactly once:\n%s", res.stdinLog)
	}
	// The transcript and usage are still collected after the abort.
	if res.done.Usage.InputTokens != 1000 {
		t.Fatalf("usage = %+v", res.done.Usage)
	}
	unlock := res.api.lock()
	defer unlock()
	if string(res.api.transcript) != "<html>fake</html>" {
		t.Fatalf("transcript = %q", res.api.transcript)
	}
}

func TestRunContinuity(t *testing.T) {
	continuity := func(cfg *agentapi.SessionConfig, _ *fakeAPI, _ *Driver, _ Paths) { cfg.Continuity = true }

	t.Run("fresh", func(t *testing.T) {
		res := runFake(t, "", continuity)
		if res.done.ContinuityRestored || res.done.Outcome != agentapi.OutcomeCompleted {
			t.Fatalf("complete = %+v", res.done)
		}
		if res.commands("open_session") != 1 ||
			!strings.Contains(res.stdinLog, `"sessionDir":"`+res.paths.SessionDir+`"`) ||
			!strings.Contains(res.stdinLog, `"provider":"fake"`) || !strings.Contains(res.stdinLog, `"modelId":"fake-model"`) {
			t.Fatalf("open_session must come first with the session dir and model:\n%s", res.stdinLog)
		}
		if first := strings.Index(res.stdinLog, `"type":"open_session"`); first > strings.Index(res.stdinLog, `"type":"prompt"`) {
			t.Fatal("open_session must precede the prompt")
		}
		unlock := res.api.lock()
		defer unlock()
		if members := tarMembers(t, res.api.continuity); members["fake.jsonl"] == "" {
			t.Fatalf("continuity tarball members = %v", members)
		}
	})

	t.Run("resumed", func(t *testing.T) {
		res := runFake(t, "", func(cfg *agentapi.SessionConfig, api *fakeAPI, d *Driver, paths Paths) {
			continuity(cfg, api, d, paths)
			writeFile(t, filepath.Join(paths.SessionDir, "old.jsonl"), historyText)
		})
		if !res.done.ContinuityRestored {
			t.Fatalf("complete = %+v", res.done)
		}
		unlock := res.api.lock()
		defer unlock()
		members := tarMembers(t, res.api.continuity)
		if members["old.jsonl"] != historyText || members["fake.jsonl"] == "" {
			t.Fatalf("continuity tarball members = %v", members)
		}
	})

	t.Run("too large", func(t *testing.T) {
		res := runFake(t, "", func(cfg *agentapi.SessionConfig, api *fakeAPI, d *Driver, paths Paths) {
			continuity(cfg, api, d, paths)
			d.ContinuityMax = 10
		})
		unlock := res.api.lock()
		defer unlock()
		if res.api.continuity != nil {
			t.Fatal("an oversized tarball must not be uploaded")
		}
		var noted bool
		for _, e := range res.api.events {
			noted = noted || (e.Kind == eventContinuity && strings.Contains(e.Text, "exceeds"))
		}
		if !noted {
			t.Fatalf("a skipped upload must be reported as an event: %v", res.api.events)
		}
	})
}

func TestRunRedaction(t *testing.T) {
	const secret = "supersecret-value-123"
	t.Setenv(demoEnv, secret)
	t.Setenv("FAKEOMP_ECHO_ENV", demoEnv)
	res := runFake(t, `{"calls":[{"tool":"echo","args":{"msg":"hi"}}]}`, func(cfg *agentapi.SessionConfig, _ *fakeAPI, _ *Driver, _ Paths) {
		cfg.Redact = []agentapi.RedactSource{{Name: demoName, Env: demoEnv}}
	})
	if res.done.Outcome != agentapi.OutcomeCompleted || !res.done.FinishCalled {
		t.Fatalf("complete = %+v", res.done)
	}
	unlock := res.api.lock()
	defer unlock()
	var seen bool
	for _, e := range res.api.events {
		if strings.Contains(e.Text, secret) {
			t.Fatalf("event leaks the secret: %+v", e)
		}
		seen = seen || strings.Contains(e.Text, "[redacted:demo]")
	}
	if !seen {
		t.Fatalf("expected [redacted:demo] in events: %v", res.api.events)
	}
	var finish string
	for _, a := range res.api.actions {
		if strings.Contains(a.Body, secret) {
			t.Fatalf("action %s leaks the secret: %s", a.Tool, a.Body)
		}
		if a.Tool == finishTool {
			finish = a.Body
		}
	}
	if !strings.Contains(finish, "[redacted:demo]") {
		t.Fatalf("finish = %s", finish)
	}
	if strings.Contains(string(res.api.transcript), secret) {
		t.Fatal("transcript leaks the secret")
	}
}

func TestPutTranscriptRedacts(t *testing.T) {
	const secret = "supersecret-value-123"
	t.Setenv(demoEnv, secret)
	file := filepath.Join(t.TempDir(), "t.html")
	writeFile(t, file, "<html>token "+secret+"</html>")
	api := newFakeAPI(t, KindSessions)
	d := api.driver(testPaths(t))
	red := OSRedactor([]agentapi.RedactSource{{Name: demoName, Env: demoEnv}})
	if err := d.putTranscriptFile(context.Background(), file, red); err != nil {
		t.Fatal(err)
	}
	unlock := api.lock()
	defer unlock()
	if got := string(api.transcript); got != "<html>token [redacted:demo]</html>" {
		t.Fatalf("transcript = %q", got)
	}
}

func TestRunCompleteErrorIsRedactedAndTruncated(t *testing.T) {
	const secret = "supersecret-value-123"
	t.Setenv(demoEnv, secret)
	api := newFakeAPI(t, KindSessions)
	api.cfg = baseConfig()
	api.cfg.Redact = []agentapi.RedactSource{{Name: demoName, Env: demoEnv}}
	d := api.driver(testPaths(t))
	d.OmpBin = "/definitely/not/omp-" + secret
	if err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	unlock := api.lock()
	defer unlock()
	done := api.completes[0]
	if done.Outcome != agentapi.OutcomeError || strings.Contains(done.Error, secret) {
		t.Fatalf("complete = %+v", done)
	}
	if len(done.Error) > 1000 {
		t.Fatalf("error too long: %d", len(done.Error))
	}
}

func TestRunFailsWhenCompletionCannotBeReported(t *testing.T) {
	api := newFakeAPI(t, KindSessions)
	api.cfg = baseConfig()
	d := api.driver(testPaths(t))
	d.OmpBin = "/definitely/not/omp"
	d.API.Attempts = 1
	d.API.Token = "wrong"
	if err := d.Run(context.Background()); err == nil {
		t.Fatal("an unreachable API must fail the driver so the pod ends without complete")
	}
}

func TestRunReadyTimeoutIsRetryable(t *testing.T) {
	dir := t.TempDir()
	silent := filepath.Join(dir, "omp")
	if err := os.WriteFile(silent, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	api := newFakeAPI(t, KindSessions)
	api.cfg = baseConfig()
	d := api.driver(testPaths(t))
	d.OmpBin = silent
	d.ReadyTimeout = 300 * time.Millisecond
	if err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	unlock := api.lock()
	defer unlock()
	if api.completes[0].Outcome != agentapi.OutcomeRetryable || !strings.Contains(api.completes[0].Error, "ready") {
		t.Fatalf("complete = %+v", api.completes[0])
	}
}

func TestOmpArgs(t *testing.T) {
	p := Paths{SessionDir: "/s", SystemPrompt: "/p"}
	cfg := agentapi.SessionConfig{SystemPrompt: "x"}
	got := strings.Join(ompArgs(cfg, 90*time.Second, p), " ")
	want := "--mode rpc --no-ui --approval-mode yolo --no-tools --max-time 90s --session-dir /s --append-system-prompt /p"
	if got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
	cfg.SystemPrompt = ""
	cfg.Tools = []string{"read"}
	if got := strings.Join(ompArgs(cfg, time.Minute, p), " "); strings.Contains(got, "append-system-prompt") || !strings.Contains(got, "--tools read") {
		t.Fatalf("args = %q", got)
	}
}

func TestClassify(t *testing.T) {
	past, future := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	result := func(status omprpc.PromptStatus, perr *omprpc.PromptError) omprpc.PromptTurn {
		return omprpc.PromptTurn{Result: &omprpc.PromptResultEvent{Status: status, Error: perr}}
	}
	perr := func(status int64, retryable bool) *omprpc.PromptError {
		e := &omprpc.PromptError{Message: boomMsg, Retryable: retryable}
		if status != 0 {
			e.HTTPStatus = new(status)
		}
		return e
	}
	tests := []struct {
		name        string
		turn        omprpc.PromptTurn
		err         error
		deadline    time.Time
		interrupted bool
		outcome     string
		msg         string
	}{
		{"completed", result(omprpc.PromptStatusCompleted, nil), nil, future, false, agentapi.OutcomeCompleted, ""},
		{"handled locally", omprpc.PromptTurn{}, nil, future, false, agentapi.OutcomeCompleted, ""},
		{"401", result(omprpc.PromptStatusError, perr(401, false)), nil, future, false, agentapi.OutcomeAuth, boomMsg},
		{"403", result(omprpc.PromptStatusError, perr(403, true)), nil, future, false, agentapi.OutcomeAuth, boomMsg},
		{"429", result(omprpc.PromptStatusError, perr(429, false)), nil, future, false, agentapi.OutcomeRetryable, boomMsg},
		{"503", result(omprpc.PromptStatusError, perr(503, false)), nil, future, false, agentapi.OutcomeRetryable, boomMsg},
		{"retryable flag", result(omprpc.PromptStatusError, perr(0, true)), nil, future, false, agentapi.OutcomeRetryable, boomMsg},
		{"400", result(omprpc.PromptStatusError, perr(400, false)), nil, future, false, agentapi.OutcomeError, boomMsg},
		{"error without detail", result(omprpc.PromptStatusError, nil), nil, future, false, agentapi.OutcomeError, "prompt failed"},
		{"aborted in time", result(omprpc.PromptStatusAborted, nil), nil, future, false, agentapi.OutcomeError, "prompt aborted"},
		{"aborted by omp's own max-time", result(omprpc.PromptStatusAborted, nil), nil, past, false, agentapi.OutcomeError, errTimedOut},
		{"deadline", omprpc.PromptTurn{}, context.DeadlineExceeded, future, false, agentapi.OutcomeError, errTimedOut},
		{"closed", omprpc.PromptTurn{}, omprpc.ErrClosed, future, false, agentapi.OutcomeRetryable, "omp exited before prompt_result"},
		{"rejected", omprpc.PromptTurn{}, &omprpc.CommandError{Command: "prompt", Message: "Unknown model"}, future, false, agentapi.OutcomeError, "prompt rejected: "},
		{"other error", omprpc.PromptTurn{}, errors.New("weird"), future, false, agentapi.OutcomeError, "weird"},
		{"interrupted", omprpc.PromptTurn{}, context.Canceled, future, true, agentapi.OutcomeRetryable, "driver interrupted"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outcome, msg := classify(tc.turn, tc.err, tc.deadline, tc.interrupted)
			if outcome != tc.outcome || !strings.HasPrefix(msg, tc.msg) {
				t.Fatalf("classify = (%q, %q), want (%q, %q...)", outcome, msg, tc.outcome, tc.msg)
			}
		})
	}
}
