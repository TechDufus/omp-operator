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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	omprpc "github.com/can1357/oh-my-pi/sdk/go/omp-rpc"

	"github.com/TechDufus/omp-operator/test/fakeprovider"
)

// cardType is the customType of the transcript cards the extension writes.
const cardType = "omp-cloud"

func (f *fakeCloudAPI) uploadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.uploads)
}

func (f *fakeCloudAPI) upload(i int) upload {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.uploads[i]
}

// waitUploads waits until the fake cloud API has received n handoffs.
func (f *fakeCloudAPI) waitUploads(t *testing.T, c *ompClient, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for f.uploadCount() < n {
		if time.Now().After(deadline) {
			c.dumpFrames(t)
			t.Fatalf("fake cloud API got %d handoffs within %s, want %d; notifications %q",
				f.uploadCount(), timeout, n, c.notifications())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// jsonl decodes the JSON lines of a session file; lines that are not JSON objects (the reserved
// title slot) are skipped.
func jsonl(data []byte) []map[string]any {
	var out []map[string]any
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(line, &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func isCard(e map[string]any) bool {
	return e["type"] == "custom_message" && e["customType"] == cardType
}

func cards(entries []map[string]any) []map[string]any {
	var out []map[string]any
	for _, e := range entries {
		if isCard(e) {
			out = append(out, e)
		}
	}
	return out
}

// waitCards waits until the session file holds at least n cards and returns its entries.
func waitCards(t *testing.T, c *ompClient, file string, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		data, err := os.ReadFile(file)
		if err == nil {
			entries := jsonl(data)
			if len(cards(entries)) >= n {
				return entries
			}
		}
		if time.Now().After(deadline) {
			c.dumpFrames(t)
			t.Fatalf("session file %s did not get %d %q cards within a minute (read error %v)", file, n, cardType, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// assertTree fails unless every entry has a unique id and a parent that exists earlier in the file.
func assertTree(t *testing.T, entries []map[string]any) {
	t.Helper()
	seen := map[string]bool{}
	for _, e := range entries {
		id, _ := e["id"].(string)
		if e["type"] == "session" || id == "" {
			continue
		}
		if seen[id] {
			t.Errorf("entry id %q appears twice", id)
		}
		if parent, ok := e["parentId"].(string); ok && !seen[parent] {
			t.Errorf("entry %q (%v) has parent %q, which is not an earlier entry: the tree is broken", id, e["type"], parent)
		}
		seen[id] = true
	}
}

// startCloudOmp starts the real omp with the extension under test and the fake provider.
func startCloudOmp(
	t *testing.T, model *fakeprovider.Server, api *fakeCloudAPI, repo string, extra ...string,
) *ompClient {
	t.Helper()
	home := cloudHome(t, model)
	args := make([]string, 0, 16+len(extra))
	args = append(args,
		"--mode", "rpc", "--approval-mode", "yolo", "--model", fakeprovider.Model,
		"--no-skills", "--no-rules", "--no-title", "--no-lsp", "--no-pty",
		"--session-dir", filepath.Join(t.TempDir(), "sessions"), "-e", cloudExtensionDir(t))
	return startRecordingOmp(t, repo, cloudEnv(home, api), append(args, extra...)...)
}

// resumeUpload opens an uploaded conversation in a fresh omp, as the cloud agent's driver does,
// and proves the session resumes and a prompt reaches the model with the history. repo is the
// checkout the session came from: the cloud agent mirrors that path, and omp cancels the
// resume of a session whose recorded working directory differs from the current one.
func resumeUpload(t *testing.T, repo string, up upload, wantInHistory ...string) {
	t.Helper()
	var req struct {
		ConversationFile string `json:"conversationFile"`
	}
	if err := json.Unmarshal(up.request, &req); err != nil || req.ConversationFile == "" {
		t.Fatalf("upload request carries no conversationFile (%v): %s", err, up.request)
	}
	model := fakeprovider.New(t, fakeprovider.WithReply("cloud reply"))
	home := cloudHome(t, model)
	sessionDir := filepath.Join(t.TempDir(), "sessions")
	writeFile(t, filepath.Join(sessionDir, filepath.Base(req.ConversationFile)), string(up.conversation))

	c := startRecordingOmp(t, repo, []string{"HOME=" + home},
		"--mode", "rpc", "--no-ui", "--approval-mode", "yolo", "--no-skills", "--no-rules",
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
	if _, err := c.PromptAndWait(ctx, omprpc.PromptCommand{Message: "continue in the cloud"}, 2*time.Minute); err != nil {
		t.Fatalf("prompt after resume: %v", err)
	}
	reqs := model.RawRequests()
	if len(reqs) != 1 {
		t.Fatalf("model saw %d requests, want 1", len(reqs))
	}
	for _, want := range append(wantInHistory, "continue in the cloud") {
		if !strings.Contains(reqs[0], want) {
			t.Errorf("model request lacks %q; the resumed history did not reach it:\n%.2000s", want, reqs[0])
		}
	}
	if strings.Contains(reqs[0], cardType) {
		t.Errorf("the resumed history still mentions %q:\n%.2000s", cardType, reqs[0])
	}
}

// TestCloudExtensionRegistersCommandToolAndRenderer loads the extension into the real omp and
// checks what omp exposes of its registrations: the /cloud command, the cloud tool offered to the
// model, a card persisted by /cloud (no arguments) with the message type the renderer is bound
// to, and that /cloud help answers from the extension without a model call.
func TestCloudExtensionRegistersCommandToolAndRenderer(t *testing.T) {
	model := fakeprovider.New(t)
	api := newFakeCloudAPI(t)
	c := startCloudOmp(t, model, api, gitRepoWithCommit(t))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmds, err := c.GetAvailableCommands(ctx)
	if err != nil {
		t.Fatalf("get_available_commands: %v", err)
	}
	if !slices.ContainsFunc(cmds, func(cmd omprpc.AvailableSlashCommand) bool { return cmd.Name == "cloud" }) {
		t.Fatalf("the extension's /cloud command is not registered: %+v", cmds)
	}

	// /cloud help is answered by the extension: a widget, no model request.
	if _, err := c.Prompt(ctx, omprpc.PromptCommand{Message: "/cloud help"}); err != nil {
		t.Fatalf("prompt /cloud help: %v", err)
	}
	deadline := time.Now().Add(time.Minute)
	var widget map[string]any
	for widget == nil {
		for _, f := range c.uiRequests("setWidget") {
			if f["widgetKey"] == "omp-cloud" {
				widget = f
			}
		}
		if time.Now().After(deadline) {
			c.dumpFrames(t)
			t.Fatal("/cloud help did not set the omp-cloud widget")
		}
		time.Sleep(50 * time.Millisecond)
	}
	lines, _ := json.Marshal(widget["widgetLines"])
	for _, want := range []string{"/cloud", "send", "answer", "pull", "resume"} {
		if !strings.Contains(string(lines), want) {
			t.Errorf("help widget lacks %q: %s", want, lines)
		}
	}
	if got := model.Count(); got != 0 {
		t.Errorf("/cloud help caused %d model requests, want 0", got)
	}

	// A real turn shows the model the cloud tool.
	if _, err := c.PromptAndWait(ctx, omprpc.PromptCommand{Message: localPrompt}, 2*time.Minute); err != nil {
		t.Fatalf("local turn: %v", err)
	}
	reqs := model.Requests()
	if len(reqs) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(reqs))
	}
	if names := reqs[0].ToolNames(); !slices.Contains(names, "cloud") {
		t.Errorf("the model was not offered the cloud tool; tools: %v", names)
	}

	// /cloud with no arguments in RPC mode (no overlay) persists an inventory card.
	state, err := c.GetState(ctx)
	if err != nil || state.SessionFile == nil {
		t.Fatalf("get_state: %+v, %v", state, err)
	}
	if _, err := c.Prompt(ctx, omprpc.PromptCommand{Message: "/cloud"}); err != nil {
		t.Fatalf("prompt /cloud: %v", err)
	}
	entries := waitCards(t, c, *state.SessionFile, 1)
	card := cards(entries)[0]
	if card["display"] != true {
		t.Errorf("card display = %v, want true", card["display"])
	}
	if content, _ := card["content"].(string); !strings.HasPrefix(content, "[cloud]") {
		t.Errorf("card content for the model = %q, want a [cloud] prefix", content)
	}
	details, _ := card["details"].(map[string]any)
	if details["kind"] != "inventory" {
		t.Errorf("card details = %v, want an inventory card", details)
	}
	if got := model.Count(); got != 1 {
		t.Errorf("/cloud caused model requests: count %d, want 1 (the local turn)", got)
	}
	for _, f := range c.framesOfType("extension_error") {
		t.Errorf("omp reported an extension error: %v", f)
	}
}

func (c *ompClient) framesOfType(typ string) []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, f := range c.frames {
		if f["type"] == typ {
			out = append(out, f)
		}
	}
	return out
}

// TestHandoffStripsCloudCards hands the same session off twice, with cards of the extension in
// between the local turns. The second upload must carry no omp-cloud card, the tree must be
// re-linked around them, and the upload must resume in a fresh omp.
func TestHandoffStripsCloudCards(t *testing.T) {
	model := fakeprovider.New(t)
	api := newFakeCloudAPI(t)
	repo := gitRepoWithCommit(t)
	c := startCloudOmp(t, model, api, repo, "--no-tools")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	turn1 := omprpc.PromptCommand{Message: "alpha-marker local turn"}
	if _, err := c.PromptAndWait(ctx, turn1, 2*time.Minute); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	state, err := c.GetState(ctx)
	if err != nil || state.SessionFile == nil {
		t.Fatalf("get_state: %+v, %v", state, err)
	}
	file := *state.SessionFile

	// First handoff: leaves a launch card in the transcript. /cloud adds an inventory card.
	if _, err := c.Prompt(ctx, omprpc.PromptCommand{Message: "/cloud first handoff"}); err != nil {
		t.Fatalf("prompt first handoff: %v", err)
	}
	api.waitUploads(t, c, 1, time.Minute)
	waitCards(t, c, file, 1)
	if _, err := c.Prompt(ctx, omprpc.PromptCommand{Message: "/cloud"}); err != nil {
		t.Fatalf("prompt /cloud: %v", err)
	}
	waitCards(t, c, file, 2)

	// A local turn after the cards: its user message is a child of a card.
	if _, err := c.PromptAndWait(ctx, omprpc.PromptCommand{Message: "beta-marker local turn"}, 2*time.Minute); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	local, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	localEntries := jsonl(local)
	cardIDs := map[string]bool{}
	for _, card := range cards(localEntries) {
		cardIDs[card["id"].(string)] = true
	}
	childOfCard := false
	for _, e := range localEntries {
		if p, ok := e["parentId"].(string); ok && cardIDs[p] && !isCard(e) {
			childOfCard = true
		}
	}
	if !childOfCard {
		t.Fatalf("precondition: no entry is a child of a card, so re-linking is not exercised:\n%s", local)
	}

	// Second handoff.
	if _, err := c.Prompt(ctx, omprpc.PromptCommand{Message: "/cloud second handoff"}); err != nil {
		t.Fatalf("prompt second handoff: %v", err)
	}
	api.waitUploads(t, c, 2, time.Minute)
	up := api.upload(1)

	entries := jsonl(up.conversation)
	if got := cards(entries); len(got) != 0 {
		t.Errorf("the upload still carries %d %q cards: %v", len(got), cardType, got)
	}
	if strings.Contains(string(up.conversation), cardType) {
		t.Errorf("the upload mentions %q:\n%s", cardType, up.conversation)
	}
	assertTree(t, entries)
	for _, want := range []string{"alpha-marker local turn", "beta-marker local turn"} {
		if !strings.Contains(string(up.conversation), want) {
			t.Errorf("upload lacks the local turn %q", want)
		}
	}
	resumeUpload(t, repo, up, "alpha-marker local turn", "beta-marker local turn")
}

// TestModelDelegationReachesCloudWithoutPendingToolCall has the fake model call the cloud tool
// with op delegate. The handoff must reach the fake cloud API with a conversation whose tool
// calls all have results (the pending cloud call is cut), the launch card must be tagged as the
// model's, and the conversation must resume in a fresh omp.
func TestModelDelegationReachesCloudWithoutPendingToolCall(t *testing.T) {
	const task = "refactor the parser in the cloud"
	model := fakeprovider.New(t,
		fakeprovider.WithToolCall("cloud", `{"op":"delegate","task":"`+task+`"}`),
		fakeprovider.WithReply("handed it off"))
	api := newFakeCloudAPI(t)
	repo := gitRepoWithCommit(t)
	c := startCloudOmp(t, model, api, repo)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	turn, err := c.PromptAndWait(ctx, omprpc.PromptCommand{Message: "please delegate the parser refactor"}, 3*time.Minute)
	if err != nil {
		c.dumpFrames(t)
		t.Fatalf("turn: %v", err)
	}
	_ = turn
	if got := model.Count(); got != 2 {
		c.dumpFrames(t)
		t.Fatalf("provider requests = %d, want 2 (the tool call, then its result)", got)
	}
	if results := toolResultTexts(model); len(results) != 1 || !strings.Contains(results[0], cloudThread) {
		t.Fatalf("the cloud tool's result did not name the thread %q: %q", cloudThread, results)
	}
	if api.uploadCount() != 1 {
		t.Fatalf("fake cloud API handoffs = %d, want 1", api.uploadCount())
	}
	up := api.upload(0)

	var req map[string]any
	if err := json.Unmarshal(up.request, &req); err != nil {
		t.Fatal(err)
	}
	if req["prompt"] != task {
		t.Errorf("request.prompt = %v, want %q", req["prompt"], task)
	}

	entries := jsonl(up.conversation)
	assertTree(t, entries)
	callIDs, resultIDs := map[string]bool{}, map[string]bool{}
	for _, e := range entries {
		if e["type"] != "message" {
			continue
		}
		msg, _ := e["message"].(map[string]any)
		switch msg["role"] {
		case "assistant":
			blocks, _ := msg["content"].([]any)
			for _, b := range blocks {
				if block, _ := b.(map[string]any); block["type"] == "toolCall" {
					callIDs[block["id"].(string)] = true
				}
			}
		case "toolResult":
			if id, ok := msg["toolCallId"].(string); ok {
				resultIDs[id] = true
			}
		}
	}
	for id := range callIDs {
		if !resultIDs[id] {
			t.Errorf("the uploaded conversation has tool call %q without a result", id)
		}
	}
	if len(callIDs) != 0 {
		t.Errorf("the uploaded conversation carries tool calls %v; the pending cloud call must be cut", callIDs)
	}
	if !strings.Contains(string(up.conversation), "please delegate the parser refactor") {
		t.Errorf("upload lacks the user's message:\n%s", up.conversation)
	}

	// The launch card records that the model started the thread.
	state, err := c.GetState(ctx)
	if err != nil || state.SessionFile == nil {
		t.Fatalf("get_state: %+v, %v", state, err)
	}
	local := waitCards(t, c, *state.SessionFile, 1)
	details, _ := cards(local)[0]["details"].(map[string]any)
	if details["kind"] != "launch" || details["origin"] != "tool" || details["thread"] != cloudThread {
		t.Errorf("launch card details = %v, want a tool-origin launch of %s", details, cloudThread)
	}

	resumeUpload(t, repo, up, "please delegate the parser refactor")
}
