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
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	omprpc "github.com/can1357/oh-my-pi/sdk/go/omp-rpc"

	"github.com/TechDufus/omp-operator/test/fakeprovider"
)

// uiRequests returns every extension_ui_request frame of the given method seen so far.
func (c *ompClient) uiRequests(method string) []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, f := range c.frames {
		if f["type"] == "extension_ui_request" && f["method"] == method {
			out = append(out, f)
		}
	}
	return out
}

// waitAskRequest waits for the n-th (1-based) `ask` request and decodes it with the SDK's own type.
func (c *ompClient) waitAskRequest(t *testing.T, n int, timeout time.Duration) omprpc.AskUiRequest {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if reqs := c.uiRequests("ask"); len(reqs) >= n {
			raw, err := json.Marshal(reqs[n-1])
			if err != nil {
				t.Fatal(err)
			}
			var req omprpc.AskUiRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				t.Fatalf("ask request does not decode as omprpc.AskUiRequest: %v\n%s", err, raw)
			}
			return req
		}
		if time.Now().After(deadline) {
			c.dumpFrames(t)
			t.Fatalf("no ask request #%d within %s", n, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// toolResultTexts returns the content of every tool message in the provider's latest request.
func toolResultTexts(model *fakeprovider.Server) []string {
	reqs := model.Requests()
	if len(reqs) == 0 {
		return nil
	}
	var out []string
	for _, m := range reqs[len(reqs)-1].Messages {
		if m.Role == "tool" {
			out = append(out, m.Text())
		}
	}
	return out
}

// TestAskArrivesAsOneUiRequest checks the driver's assumption about rpc-ui: with set_ask_dialog
// enabled, a model's ask tool call arrives as exactly one extension_ui_request of method "ask"
// carrying every question, and an AnswersUiResponse reaches the tool as its result.
func TestAskArrivesAsOneUiRequest(t *testing.T) {
	tests := []struct {
		name      string
		args      string
		check     func(t *testing.T, req omprpc.AskUiRequest)
		answers   []omprpc.AskAnswer
		wantInRes []string
	}{
		{
			name: "single select with one option",
			args: `{"questions":[{"id":"which","question":"Which database?",` +
				`"options":[{"label":"Postgres","description":"the default"}]}]}`,
			check: func(t *testing.T, req omprpc.AskUiRequest) {
				if len(req.Questions) != 1 {
					t.Fatalf("questions = %d, want 1", len(req.Questions))
				}
				q := req.Questions[0]
				if q.ID != "which" || q.Question != "Which database?" || q.Multi {
					t.Errorf("question = %+v", q)
				}
				if len(q.Options) != 1 || q.Options[0].Label != "Postgres" ||
					q.Options[0].Description == nil || *q.Options[0].Description != "the default" {
					t.Errorf("options = %+v", q.Options)
				}
			},
			answers:   []omprpc.AskAnswer{{ID: "which", SelectedOptions: []string{"Postgres"}}},
			wantInRes: []string{"Postgres"},
		},
		{
			name: "multi select with options and custom input",
			args: `{"questions":[{"id":"features","question":"Which features?","multi":true,` +
				`"options":[{"label":"Auth"},{"label":"Billing"},{"label":"Search"}]}]}`,
			check: func(t *testing.T, req omprpc.AskUiRequest) {
				if len(req.Questions) != 1 || !req.Questions[0].Multi || len(req.Questions[0].Options) != 3 {
					t.Errorf("questions = %+v", req.Questions)
				}
			},
			answers: []omprpc.AskAnswer{{
				ID: "features", SelectedOptions: []string{"Auth", "Search"}, CustomInput: new("plus a changelog"),
			}},
			wantInRes: []string{"Auth", "Search", "plus a changelog"},
		},
		{
			name: "two questions in one request",
			args: `{"questions":[{"id":"a","question":"First?","options":[{"label":"Yes"},{"label":"No"}]},` +
				`{"id":"b","question":"Second?","options":[{"label":"Red"},{"label":"Blue"}]}]}`,
			check: func(t *testing.T, req omprpc.AskUiRequest) {
				if len(req.Questions) != 2 || req.Questions[0].ID != "a" || req.Questions[1].ID != "b" {
					t.Errorf("questions = %+v", req.Questions)
				}
			},
			answers: []omprpc.AskAnswer{
				{ID: "a", SelectedOptions: []string{"No"}},
				{ID: "b", SelectedOptions: []string{"Blue"}},
			},
			wantInRes: []string{"No", "Blue"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := fakeprovider.New(t, fakeprovider.WithToolCall("ask", tt.args), fakeprovider.WithReply("thanks"))
			home := cloudHome(t, model)
			repo := gitRepoWithCommit(t)
			c := startRecordingOmp(t, repo, []string{"HOME=" + home},
				"--mode", "rpc-ui", "--no-ui", "--approval-mode", "yolo", "--model", fakeprovider.Model,
				"--no-skills", "--no-rules", "--no-title", "--no-lsp", "--no-pty",
				"--session-dir", t.TempDir())

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			on, err := c.SetAskDialog(ctx, omprpc.SetAskDialogCommand{Enabled: true})
			if err != nil || !on {
				t.Fatalf("set_ask_dialog = %v, %v; want enabled", on, err)
			}

			done := make(chan error, 1)
			go func() {
				_, err := c.PromptAndWait(ctx, omprpc.PromptCommand{Message: "ask me something"}, 2*time.Minute)
				done <- err
			}()

			req := c.waitAskRequest(t, 1, time.Minute)
			tt.check(t, req)
			if err := c.Send(omprpc.AnswersUiResponse{ID: req.ID, Answers: tt.answers}); err != nil {
				t.Fatalf("send answers: %v", err)
			}
			if err := <-done; err != nil {
				c.dumpFrames(t)
				t.Fatalf("turn: %v", err)
			}

			if got := len(c.uiRequests("ask")); got != 1 {
				t.Errorf("ask requests = %d, want exactly 1 per tool call", got)
			}
			for _, other := range []string{"select", "confirm", "input", "editor"} {
				if got := len(c.uiRequests(other)); got != 0 {
					t.Errorf("%d %q dialogs arrived besides the ask request", got, other)
				}
			}
			if got := model.Count(); got != 2 {
				t.Fatalf("provider requests = %d, want 2 (tool call, then the answer)", got)
			}
			results := strings.Join(toolResultTexts(model), "\n")
			if results == "" {
				t.Fatalf("the answer never reached the model as a tool result: %q", model.RawRequests()[1])
			}
			for _, want := range tt.wantInRes {
				if !strings.Contains(results, want) {
					t.Errorf("tool result %q lacks %q", results, want)
				}
			}
		})
	}
}
