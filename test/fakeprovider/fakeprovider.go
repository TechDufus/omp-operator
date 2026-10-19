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

// Package fakeprovider is an httptest stand-in for an OpenAI chat-completions provider, shared
// by the ompcompat and e2e suites. It streams a scripted answer to every request and records
// what omp sent. Point omp at it with the models.yml from [Server.ModelsYAML] and select
// [Model] as the model.
package fakeprovider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

const (
	// Provider is the provider name in the models.yml from ModelsYAML.
	Provider = "fake"
	// ModelID is the model id within Provider.
	ModelID = "fake-model"
	// Model is the provider/id form omp and the operator use to select the model.
	Model = Provider + "/" + ModelID
	// DefaultReply is the text the server answers with unless WithReply says otherwise.
	DefaultReply = "done"
)

// Message is one chat message of a recorded request.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Text returns the message's text: Content is either a JSON string or an array of parts.
func (m Message) Text() string {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &parts) != nil {
		return string(m.Content)
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// Request is the part of a chat-completions request the fake model looks at.
type Request struct {
	Messages []Message `json:"messages"`
	Tools    []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

// ToolNames lists the tools the request offered the model.
func (r Request) ToolNames() []string {
	names := make([]string, 0, len(r.Tools))
	for _, tool := range r.Tools {
		names = append(names, tool.Function.Name)
	}
	return names
}

// HasToolResult reports whether the conversation holds a tool result.
func (r Request) HasToolResult() bool {
	return slices.ContainsFunc(r.Messages, func(m Message) bool { return m.Role == "tool" })
}

// EndsWithToolResult reports whether the last message is a tool result, i.e. the request is the
// model's next step after a tool call it made.
func (r Request) EndsWithToolResult() bool {
	return len(r.Messages) > 0 && r.Messages[len(r.Messages)-1].Role == "tool"
}

// Option configures New.
type Option func(*Server)

// WithReply sets the text the model answers with (default DefaultReply).
func WithReply(text string) Option {
	return func(s *Server) { s.reply = text }
}

// WithToolCall makes the model call tool with the JSON arguments args until the conversation
// holds a tool result, then answer with the reply text.
func WithToolCall(tool, args string) Option {
	return func(s *Server) { s.tool, s.toolArgs = tool, args }
}

// ArmToolCall makes the next request that does not follow a tool result answer with a call of
// tool with the JSON arguments args; the request after that answers with the reply text. It
// lets a test choose which turn of a long conversation calls a tool.
func (s *Server) ArmToolCall(tool, args string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armedTool, s.armedArgs = tool, args
}

// Server is the running fake provider.
type Server struct {
	// URL is the server's base URL (no trailing slash, no /v1).
	URL string

	srv      *httptest.Server
	reply    string
	tool     string
	toolArgs string

	armedTool, armedArgs string

	mu   sync.Mutex
	reqs []Request
	raw  []string
}

// New starts a fake provider that is closed when the test ends. Without options it answers every
// request with the text DefaultReply.
func New(t testing.TB, opts ...Option) *Server {
	t.Helper()
	s := &Server{reply: DefaultReply}
	for _, opt := range opts {
		opt(s)
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	s.URL = s.srv.URL
	t.Cleanup(s.srv.Close)
	return s
}

// ModelsYAML is the models.yml that registers the server as provider Provider with model ModelID.
func (s *Server) ModelsYAML() string {
	return fmt.Sprintf(`providers:
  %s:
    baseUrl: %s/v1
    auth: none
    api: openai-completions
    models:
      - id: %s
        contextWindow: 128000
        maxTokens: 4096
`, Provider, s.URL, ModelID)
}

// Count is the number of chat-completions requests received so far.
func (s *Server) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

// Requests returns the parsed requests received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs)
}

// RawRequests returns the request bodies received so far.
func (s *Server) RawRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.raw)
}

func sseChunk(w io.Writer, v any) {
	raw, _ := json.Marshal(v)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.raw = append(s.raw, string(body))
	tool, toolArgs := s.tool, s.toolArgs
	callID := fmt.Sprintf("call_compat_%d", len(s.reqs))
	call := tool != "" && !req.HasToolResult()
	if s.armedTool != "" && !req.EndsWithToolResult() {
		tool, toolArgs, call = s.armedTool, s.armedArgs, true
		s.armedTool, s.armedArgs = "", ""
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"id": "chatcmpl-compat", "object": "chat.completion.chunk", "created": 1, "model": ModelID,
			"choices": []any{map[string]any{keyIndex: 0, "delta": delta, "finish_reason": finish}},
		}
	}
	usage := map[string]any{
		"id": "chatcmpl-compat", "object": "chat.completion.chunk", "created": 1, "model": ModelID,
		"choices": []any{}, "usage": map[string]any{"prompt_tokens": 120, "completion_tokens": 12, "total_tokens": 132},
	}
	if !call {
		sseChunk(w, chunk(map[string]any{"role": "assistant", "content": s.reply}, nil))
		sseChunk(w, chunk(map[string]any{}, "stop"))
	} else {
		sseChunk(w, chunk(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
			keyIndex: 0, "id": callID, "type": keyFunction,
			keyFunction: map[string]any{"name": tool, "arguments": ""},
		}}}, nil))
		sseChunk(w, chunk(map[string]any{"tool_calls": []any{map[string]any{
			keyIndex: 0, keyFunction: map[string]any{"arguments": toolArgs},
		}}}, nil))
		sseChunk(w, chunk(map[string]any{}, "tool_calls"))
	}
	sseChunk(w, usage)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

// JSON keys the streamed chunks repeat.
const (
	keyIndex    = "index"
	keyFunction = "function"
)
