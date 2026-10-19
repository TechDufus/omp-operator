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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	omprpc "github.com/can1357/oh-my-pi/sdk/go/omp-rpc"
)

var fakeBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakeomp-test")
	if err != nil {
		panic(err)
	}
	fakeBinary = filepath.Join(dir, "fakeomp")
	if out, err := exec.Command("go", "build", "-o", fakeBinary, ".").CombinedOutput(); err != nil {
		_ = os.RemoveAll(dir)
		panic("go build: " + err.Error() + "\n" + string(out))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// recorder collects the arguments of host tool calls.
type recorder struct {
	mu    sync.Mutex
	calls map[string][]string
}

func (r *recorder) args(tool string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls[tool])
}

func (r *recorder) tool(name string) omprpc.HostTool {
	return omprpc.HostTool{
		Definition: omprpc.HostToolDefinition{
			Name:        name,
			Description: "test tool " + name,
			Parameters:  map[string]json.RawMessage{"type": json.RawMessage(`"object"`)},
			LoadMode:    omprpc.Ptr(omprpc.ToolLoadModeEssential),
		},
		Handler: func(_ context.Context, _ *omprpc.HostToolCall, args json.RawMessage) (omprpc.HostToolResultPayload, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.calls[name] = append(r.calls[name], string(args))
			return omprpc.TextResult("result of " + name), nil
		},
	}
}

type harness struct {
	t          *testing.T
	client     *omprpc.Client
	rec        *recorder
	sessionDir string

	mu       sync.Mutex
	frames   []omprpc.RpcServerFrame
	framesWG sync.WaitGroup
}

// start launches fakeomp with the flags the operator's driver passes and the
// given FAKEOMP_SCRIPT, registering host tools echo and finish.
func start(t *testing.T, scriptJSON string, env ...string) *harness {
	t.Helper()
	root := t.TempDir()
	return startIn(t, root, filepath.Join(root, "session"), scriptJSON, env...)
}

func startIn(t *testing.T, root, sessionDir, scriptJSON string, env ...string) *harness {
	t.Helper()
	promptFile := filepath.Join(root, "system-prompt.md")
	if err := os.WriteFile(promptFile, []byte("system"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(fakeBinary, "--mode", "rpc", "--no-ui", "--approval-mode", "yolo",
		"--tools", "read,bash", "--max-time", "60s", "--session-dir", sessionDir,
		"--append-system-prompt", promptFile)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)
	if scriptJSON != "" {
		cmd.Env = append(cmd.Env, "FAKEOMP_SCRIPT="+scriptJSON)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	h := &harness{t: t, rec: &recorder{calls: map[string][]string{}}, sessionDir: sessionDir}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := omprpc.Start(ctx, cmd, omprpc.WithHostTools(h.rec.tool("echo"), h.rec.tool("finish")))
	if err != nil {
		t.Fatalf("start: %v\n%s", err, stderr.String())
	}
	h.client = client
	h.framesWG.Go(func() {
		for frame := range client.Frames() {
			h.mu.Lock()
			h.frames = append(h.frames, frame)
			h.mu.Unlock()
		}
	})
	t.Cleanup(func() {
		_ = client.Close()
		h.framesWG.Wait()
		if t.Failed() {
			t.Logf("fakeomp stderr:\n%s", stderr.String())
		}
	})
	return h
}

// stop closes the client and returns every frame the SDK delivered.
func (h *harness) stop() []omprpc.RpcServerFrame {
	h.t.Helper()
	if err := h.client.Close(); err != nil {
		h.t.Fatalf("close: %v", err)
	}
	h.framesWG.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.frames)
}

func unknowns(frames []omprpc.RpcServerFrame) []omprpc.UnknownNotification {
	var out []omprpc.UnknownNotification
	for _, f := range frames {
		if u, ok := f.Value.(omprpc.UnknownNotification); ok {
			out = append(out, u)
		}
	}
	return out
}

func prompt(t *testing.T, h *harness) omprpc.PromptTurn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	turn, err := h.client.PromptAndWait(ctx, omprpc.PromptCommand{Message: "go"}, 10*time.Second)
	if err != nil {
		t.Fatalf("PromptAndWait: %v", err)
	}
	return turn
}

const normalScript = `{"calls":[{"tool":"echo","args":{"message":"hi"}}],` +
	`"finish":{"verdict":"fixed","title":"t","summary":"s","report":"r"}}`

func TestNormalTurnWithCalls(t *testing.T) {
	h := start(t, normalScript)

	if v := h.client.ProtocolVersion(); v != 2 {
		t.Fatalf("protocol version = %d, want 2", v)
	}
	state, err := h.client.GetState(context.Background())
	if err != nil {
		t.Fatalf("get_state: %v", err)
	}
	names := make([]string, 0, len(state.DumpTools))
	for _, tool := range state.DumpTools {
		names = append(names, tool.Name)
	}
	for _, want := range []string{"read", "bash", "echo", "finish"} {
		if !slices.Contains(names, want) {
			t.Errorf("get_state dumpTools %v lacks %q", names, want)
		}
	}

	turn := prompt(t, h)
	if turn.Result == nil || turn.Result.Status != omprpc.PromptStatusCompleted || !turn.Result.AgentInvoked {
		t.Fatalf("prompt_result = %+v", turn.Result)
	}
	if turn.Result.ID == nil || *turn.Result.ID == "" {
		t.Errorf("prompt_result carries no request id")
	}
	checkToolArgs(t, h)
	checkSessionCommands(t, h)
	checkFrames(t, h.stop())
}

func checkToolArgs(t *testing.T, h *harness) {
	t.Helper()
	if got := h.rec.args("echo"); len(got) != 1 || got[0] != `{"message":"hi"}` {
		t.Errorf("echo args = %v", got)
	}
	finishArgs := h.rec.args("finish")
	if len(finishArgs) != 1 {
		t.Fatalf("finish called %d times", len(finishArgs))
	}
	var fin map[string]string
	if err := json.Unmarshal([]byte(finishArgs[0]), &fin); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"verdict": "fixed", "title": "t", "summary": "s", "report": "r"}
	if !maps.Equal(fin, want) {
		t.Errorf("finish args = %v, want %v", fin, want)
	}
}

// checkSessionCommands covers the session file, get_session_stats and export_html.
func checkSessionCommands(t *testing.T, h *harness) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(h.sessionDir, "fake.jsonl")); err != nil {
		t.Errorf("fake.jsonl in --session-dir: %v", err)
	}

	stats, err := h.client.GetSessionStats(context.Background())
	if err != nil {
		t.Fatalf("get_session_stats: %v", err)
	}
	tokens := stats.Tokens
	if tokens.Input != 1000 || tokens.Output != 200 || tokens.CacheRead != 300 || tokens.CacheWrite != 40 {
		t.Errorf("stats tokens = %+v", tokens)
	}
	if stats.Cost != 0.0125 {
		t.Errorf("stats cost = %v", stats.Cost)
	}

	htmlPath := filepath.Join(t.TempDir(), "out.html")
	path, err := h.client.ExportHTML(context.Background(), omprpc.ExportHTMLCommand{OutputPath: &htmlPath})
	if err != nil {
		t.Fatalf("export_html: %v", err)
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "<html>fake</html>" {
		t.Errorf("export_html wrote %q (%v) to %q", body, err, path)
	}
}

// checkFrames asserts that the SDK decoded every frame and saw the expected events.
func checkFrames(t *testing.T, frames []omprpc.RpcServerFrame) {
	t.Helper()
	if u := unknowns(frames); len(u) > 0 {
		t.Fatalf("SDK could not decode %d frames: %+v", len(u), u[0])
	}
	var startTools []string
	var ends, notices, agentStarts, agentEnds int
	for _, f := range frames {
		switch v := f.Value.(type) {
		case omprpc.ToolExecutionStartEvent:
			startTools = append(startTools, v.ToolName)
		case omprpc.ToolExecutionEndEvent:
			ends++
		case omprpc.NoticeEvent:
			notices++
		case omprpc.AgentStartEvent:
			agentStarts++
		case omprpc.AgentEndEvent:
			agentEnds++
		}
	}
	if !slices.Equal(startTools, []string{"echo", "finish"}) {
		t.Errorf("tool_execution_start tools = %v", startTools)
	}
	if ends != 2 || notices != 1 || agentStarts != 1 || agentEnds != 1 {
		t.Errorf("tool ends %d, notices %d, agent_start %d, agent_end %d", ends, notices, agentStarts, agentEnds)
	}
}

func TestDefaultScriptFinishesOK(t *testing.T) {
	h := start(t, "")
	turn := prompt(t, h)
	if turn.Result == nil || turn.Result.Status != omprpc.PromptStatusCompleted {
		t.Fatalf("prompt_result = %+v", turn.Result)
	}
	if h.rec.args("echo") != nil {
		t.Errorf("echo called without calls")
	}
	fin := h.rec.args("finish")
	if len(fin) != 1 || !strings.Contains(fin[0], `"verdict":"ok"`) {
		t.Errorf("finish args = %v", fin)
	}
}

func TestOpenSessionResumes(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "session")

	first := startIn(t, root, dir, "")
	res, err := first.client.OpenSession(context.Background(), omprpc.OpenSessionCommand{SessionDir: dir})
	if err != nil {
		t.Fatalf("open_session: %v", err)
	}
	if res.Resumed || res.Cancelled || res.SessionID == "" {
		t.Errorf("fresh open_session = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "fake.jsonl")); err != nil {
		t.Errorf("open_session did not write fake.jsonl: %v", err)
	}
	if u := unknowns(first.stop()); len(u) > 0 {
		t.Errorf("undecodable frames: %+v", u[0])
	}

	second := startIn(t, root, dir, "")
	res, err = second.client.OpenSession(context.Background(), omprpc.OpenSessionCommand{
		SessionDir: dir, Provider: new("anthropic"), ModelID: new("claude-sonnet-5-5"),
	})
	if err != nil {
		t.Fatalf("open_session: %v", err)
	}
	if !res.Resumed {
		t.Errorf("open_session over an existing .jsonl: resumed = false")
	}
}

// A session directory that holds a real omp session (a restored handoff conversation) is
// continued in place: no fake.jsonl, and what fakeomp appends keeps every line a valid entry.
func TestOpenSessionContinuesRealSession(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "session")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dir, "2026-10-19T04-17-08-220Z_real-id.jsonl")
	history := `{"type":"title","v":1,"title":"","pad":""}` + "\n" +
		`{"type":"session","version":3,"id":"real-id","cwd":"/x"}` + "\n" +
		`{"type":"message","id":"aaaa1111","parentId":null,"message":{"role":"user","content":"hi"}}` + "\n"
	if err := os.WriteFile(real, []byte(history), 0o644); err != nil {
		t.Fatal(err)
	}

	h := startIn(t, root, dir, "")
	res, err := h.client.OpenSession(context.Background(), omprpc.OpenSessionCommand{SessionDir: dir})
	if err != nil {
		t.Fatalf("open_session: %v", err)
	}
	if !res.Resumed || res.SessionID != "real-id" || res.SessionFile == nil || *res.SessionFile != real {
		t.Fatalf("open_session = %+v, want the real session resumed", res)
	}
	prompt(t, h)

	if _, err := os.Stat(filepath.Join(dir, "fake.jsonl")); err == nil {
		t.Error("fake.jsonl was created next to a real session")
	}
	raw, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 4 || !strings.HasPrefix(string(raw), history) {
		t.Fatalf("session file = %q, want the history plus one appended entry", raw)
	}
	var entry struct{ Type, ID, ParentID string }
	err = json.Unmarshal([]byte(lines[3]), &entry)
	if err != nil || entry.Type != "custom" || entry.ID == "" || entry.ParentID != "aaaa1111" {
		t.Fatalf("appended entry = %q (%+v, %v), want a custom entry chained to the last message", lines[3], entry, err)
	}
}

func TestExitEarly(t *testing.T) {
	h := start(t, `{"mode":"exit-early"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := h.client.PromptAndWait(ctx, omprpc.PromptCommand{Message: "go"}, 10*time.Second)
	if !errors.Is(err, omprpc.ErrClosed) {
		t.Fatalf("PromptAndWait error = %v, want ErrClosed", err)
	}
	if h.rec.args("finish") != nil {
		t.Errorf("finish called in exit-early mode")
	}
}

func TestNoFinish(t *testing.T) {
	h := start(t, `{"mode":"no-finish","calls":[{"tool":"echo","args":{}}]}`)
	for range 2 { // the driver's nudge is a second prompt
		turn := prompt(t, h)
		if turn.Result == nil || turn.Result.Status != omprpc.PromptStatusCompleted {
			t.Fatalf("prompt_result = %+v", turn.Result)
		}
	}
	if got := h.rec.args("echo"); len(got) != 2 {
		t.Errorf("echo called %d times, want 2", len(got))
	}
	if got := h.rec.args("finish"); len(got) != 0 {
		t.Errorf("finish called in no-finish mode: %v", got)
	}
}

func TestGarbageLineIsSkipped(t *testing.T) {
	h := start(t, `{"mode":"garbage"}`)
	turn := prompt(t, h)
	if turn.Result == nil || turn.Result.Status != omprpc.PromptStatusCompleted {
		t.Fatalf("prompt_result = %+v", turn.Result)
	}
	u := unknowns(h.stop())
	if len(u) != 1 || u[0].Err == nil || !strings.Contains(string(u[0].Raw), "not json") {
		t.Fatalf("want exactly the garbage line as an undecodable frame, got %+v", u)
	}
}

func TestHangUntilAbort(t *testing.T) {
	h := start(t, `{"mode":"hang","calls":[{"tool":"echo","args":{}}]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	type outcome struct {
		turn omprpc.PromptTurn
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		turn, err := h.client.PromptAndWait(ctx, omprpc.PromptCommand{Message: "go"}, 12*time.Second)
		done <- outcome{turn, err}
	}()

	// Abort until the prompt is in flight; aborting an idle server is harmless.
	for {
		select {
		case o := <-done:
			if o.err != nil {
				t.Fatalf("PromptAndWait: %v", o.err)
			}
			if o.turn.Result == nil || o.turn.Result.Status != omprpc.PromptStatusAborted {
				t.Fatalf("prompt_result = %+v, want aborted", o.turn.Result)
			}
			if len(h.rec.args("echo")) != 0 || len(h.rec.args("finish")) != 0 {
				t.Errorf("hang mode ran tools")
			}
			if u := unknowns(h.stop()); len(u) > 0 {
				t.Errorf("undecodable frames: %+v", u[0])
			}
			return
		case <-time.After(50 * time.Millisecond):
			if err := h.client.Abort(ctx); err != nil {
				t.Fatalf("abort: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("hang prompt never aborted")
		}
	}
}

func TestEchoEnvLeaksValue(t *testing.T) {
	const secret = "s3cret-token-value"
	h := start(t, `{"calls":[{"tool":"echo","args":{"message":"hi"}}]}`, "FAKEOMP_ECHO_ENV=TEST_LEAK", "TEST_LEAK="+secret)
	prompt(t, h)

	if fin := h.rec.args("finish"); len(fin) != 1 || !strings.Contains(fin[0], secret) {
		t.Errorf("finish args %v do not carry the env value", fin)
	}
	var startArgs, notices []string
	for _, f := range h.stop() {
		switch v := f.Value.(type) {
		case omprpc.ToolExecutionStartEvent:
			startArgs = append(startArgs, string(v.Args))
		case omprpc.NoticeEvent:
			notices = append(notices, v.Message)
		}
	}
	if len(startArgs) != 2 || !strings.Contains(startArgs[0], secret) || !strings.Contains(startArgs[1], secret) {
		t.Errorf("tool_execution_start args = %v", startArgs)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], secret) {
		t.Errorf("notices = %v", notices)
	}
	// The host tool itself still receives the script's args untouched.
	if got := h.rec.args("echo"); len(got) != 1 || got[0] != `{"message":"hi"}` {
		t.Errorf("echo args = %v", got)
	}
}

func TestInvalidScriptExits2(t *testing.T) {
	for _, script := range []string{`{not json`, `{"mode":"bogus"}`} {
		cmd := exec.Command(fakeBinary, "--mode", "rpc")
		cmd.Env = append(os.Environ(), "FAKEOMP_SCRIPT="+script)
		err := cmd.Run()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Errorf("script %q: err = %v, want exit status 2", script, err)
		}
	}
}

// promptText plays one prompt with the given message.
func promptText(t *testing.T, h *harness, message string) (omprpc.PromptTurn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return h.client.PromptAndWait(ctx, omprpc.PromptCommand{Message: message}, 10*time.Second)
}

func TestTurnScriptWritesFilesChecksAndReplies(t *testing.T) {
	const secret = "echoed-secret-value"
	h := start(t, "", "FAKEOMP_ECHO_ENV=ECHO_VAR", "ECHO_VAR="+secret, "PRESENT_VAR=x")
	root := filepath.Dir(h.sessionDir)
	abs := filepath.Join(root, "dir", "a.txt")

	script := `FAKEOMP:{"files":{"dir/a.txt":"hello\n"},"checkFiles":["dir/a.txt","missing.txt","` + abs + `"],` +
		`"checkEnv":["PRESENT_VAR","ABSENT_VAR"],"reply":"scripted reply"}`
	turn, err := promptText(t, h, script)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Result == nil || turn.Result.Status != omprpc.PromptStatusCompleted {
		t.Fatalf("result = %+v", turn.Result)
	}
	if got := string(mustRead(t, filepath.Join(root, "dir", "a.txt"))); got != "hello\n" {
		t.Fatalf("a.txt = %q", got)
	}
	check := string(mustRead(t, filepath.Join(root, checkFile)))
	want := "cwd: " + root + "\ndir/a.txt: present\nmissing.txt: absent\n" + abs + ": present\n" +
		"PRESENT_VAR: set\nABSENT_VAR: unset\n"
	if check != want {
		t.Fatalf("%s = %q, want %q", checkFile, check, want)
	}
	text, err := h.client.GetLastAssistantText(context.Background())
	if err != nil || text == nil || *text != "scripted reply "+secret {
		t.Fatalf("last assistant text = %v, %v", text, err)
	}
	if got := deref(turn.AssistantText); got != "scripted reply "+secret {
		t.Fatalf("assistant message text = %q", got)
	}
	if got := h.rec.args("finish"); len(got) != 0 {
		t.Fatalf("a scripted turn must not call finish, got %v", got)
	}

	// The next turn has its own script; the default reply applies.
	if _, err := promptText(t, h, `FAKEOMP:{"files":{"b.txt":"2"}}`); err != nil {
		t.Fatal(err)
	}
	if text, _ := h.client.GetLastAssistantText(context.Background()); text == nil || *text != defaultReply+" "+secret {
		t.Fatalf("second reply = %v", text)
	}
	if got := string(mustRead(t, filepath.Join(root, "b.txt"))); got != "2" {
		t.Fatalf("b.txt = %q", got)
	}
}

func TestTurnScriptHangsUntilAbort(t *testing.T) {
	h := start(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = h.client.Abort(ctx)
	}()
	turn, err := promptText(t, h, `FAKEOMP:{"mode":"hang"}`)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Result == nil || turn.Result.Status != omprpc.PromptStatusAborted {
		t.Fatalf("result = %+v", turn.Result)
	}
}

func TestTurnScriptErrors(t *testing.T) {
	h := start(t, "")
	if _, err := promptText(t, h, `FAKEOMP:{not json`); err == nil {
		t.Fatal("a malformed script must be rejected")
	}
	turn, err := promptText(t, h, `FAKEOMP:{"files":{"../escape.txt":"x"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Result == nil || turn.Result.Status != omprpc.PromptStatusError ||
		turn.Result.Error == nil || !strings.Contains(turn.Result.Error.Message, "escape.txt") {
		t.Fatalf("result = %+v", turn.Result)
	}
}

func TestFinishSkippedWhenNotRegistered(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command(fakeBinary, "--mode", "rpc", "--session-dir", filepath.Join(root, "s"))
	cmd.Dir = root
	rec := &recorder{calls: map[string][]string{}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := omprpc.Start(ctx, cmd, omprpc.WithHostTools(rec.tool("echo")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if _, err := client.PromptAndWait(ctx, omprpc.PromptCommand{Message: "go"}, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := rec.args("finish"); len(got) != 0 {
		t.Fatalf("finish was called although it is not registered: %v", got)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want options
	}{
		{
			"driver flags",
			[]string{
				"--mode", "rpc", "--no-ui", "--approval-mode", "yolo", "--tools", "read, bash",
				"--max-time", "30s", "--session-dir", "/s", "--append-system-prompt", "/p",
			},
			options{sessionDir: "/s", tools: []string{"read", "bash"}},
		},
		{"equals form", []string{"--session-dir=/x", "--tools=grep"}, options{sessionDir: "/x", tools: []string{"grep"}}},
		{"nothing", nil, options{}},
		{"unknown flags ignored", []string{"--whatever", "--no-ui"}, options{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseArgs(tt.args)
			if got.sessionDir != tt.want.sessionDir || !slices.Equal(got.tools, tt.want.tools) {
				t.Errorf("parseArgs(%v) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}
